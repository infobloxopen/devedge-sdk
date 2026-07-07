// Command openapiv2to3 converts a grpc-gateway-emitted OpenAPI v2 (.swagger.json)
// document into an OpenAPI 3.0 YAML document, then runs a lossless ENRICHMENT
// pass over it from a proto FileDescriptorSet so the published spec carries the
// full AIP contract (field_behavior, resource identity, method classification,
// references, pagination) — the single interchange every downstream generator
// reads (WS-024 D1). The enrichment shares the internal/aip resolver/classifier
// with the codegen plugins so compiled behavior and published OpenAPI cannot
// drift (D-new-1).
//
// Usage:
//
//	openapiv2to3 -descriptor <fds.binpb> [-json-names=auto|snake|camel] [-strict] <input.swagger.json> [output-dir]
//
// The output lands at <output-dir>/<name>.openapi.yaml (default: openapi/ beside
// the input).
//
// The enrichment matches swagger operations to proto methods by (verb,
// path-template) from google.api.http rules (prefix-tolerant, so a patched
// basePath is fine), rewrites each matched operationId to the canonical
// `Service_Method` form (the original kept as x-legacy-operation-id when it
// differs), auto-detects snake_case vs camelCase property names (-json-names
// overrides), and resolves definition names back to proto messages through a
// tiered resolver. This tolerant matching is the DEFAULT for every input —
// gateway-v2 (protoc-gen-openapiv2) and the OLD grpc-gateway v1 / atlas
// (protoc-gen-swagger) toolchains alike; nothing about the input is assumed
// (WS-035 R1). Any unmatched/ambiguous item, plus proto methods with no swagger
// operation (a legitimate partial view), is reported — on stderr always, and to
// <out>.coverage.json when there is anything to review — rather than failing.
//
// -strict opts back into hard failure (non-zero exit, no output) when an
// operation, schema, or field is unmatched or ambiguous. Proto methods with no
// swagger operation stay a report section even under -strict: a swagger that
// covers only part of a service is a partial view, not a defect.
//
// -compat=gateway-v1 is a DEPRECATED no-op, retained one release for grace: the
// tolerant matching it used to gate is now the default, so the flag is accepted
// and ignored.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entry point: it parses args, converts + enriches, writes
// the YAML, and returns a process exit code (0 on success, non-zero on failure).
func run(args []string) int {
	fs := flag.NewFlagSet("openapiv2to3", flag.ContinueOnError)
	descriptorPath := fs.String("descriptor", "", "path to a binary proto FileDescriptorSet for the enrichment pass (required)")
	compatMode := fs.String("compat", "", `DEPRECATED no-op (accepted for one release): the tolerant matching "gateway-v1" used to gate is now the default for every input`)
	jsonNames := fs.String("json-names", "auto", `how schema properties are keyed against proto fields — "auto" (probe the document), "snake" (fd.Name()), or "camel" (fd.JSONName())`)
	strict := fs.Bool("strict", false, "fail (non-zero exit, no output) if any operation, schema, or field is unmatched or ambiguous, instead of reporting (proto methods with no swagger operation stay a report section)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: %v\n", err)
		return 1
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintf(os.Stderr, "usage: openapiv2to3 -descriptor <fds.binpb> [-json-names=auto|snake|camel] [-strict] <input.swagger.json> [output-dir]\n")
		return 1
	}
	inputPath := rest[0]
	outDir := filepath.Join(filepath.Dir(inputPath), "openapi")
	if len(rest) >= 2 {
		outDir = rest[1]
	}

	// -compat is a deprecated no-op (WS-035 R1): the tolerant matching it used to
	// gate is now the default, so accept "gateway-v1" and ignore it (one release
	// of grace), and still reject any other value so a typo is caught.
	switch *compatMode {
	case "":
	case "gateway-v1":
		fmt.Fprintln(os.Stderr, "openapiv2to3: -compat=gateway-v1 is deprecated and now a no-op; its tolerant matching is the default")
	default:
		fmt.Fprintf(os.Stderr, "openapiv2to3: unknown -compat mode %q (\"gateway-v1\" is the only accepted, deprecated value)\n", *compatMode)
		return 1
	}
	switch *jsonNames {
	case "auto", "snake", "camel":
	default:
		fmt.Fprintf(os.Stderr, "openapiv2to3: invalid -json-names %q (want auto, snake, or camel)\n", *jsonNames)
		return 1
	}

	// FM-2: a missing/unspecified FDS is a hard error — we never emit a spec that
	// silently lacks the x-aip-* enrichment.
	if *descriptorPath == "" {
		fmt.Fprintln(os.Stderr, "openapiv2to3: -descriptor is required (losslessness cannot be verified without the FDS)")
		return 1
	}
	files, err := loadDescriptors(*descriptorPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: %v\n", err)
		return 1
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: read %s: %v\n", inputPath, err)
		return 1
	}

	var doc2 openapi2.T
	if err := json.Unmarshal(data, &doc2); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: parse v2 %s: %v\n", inputPath, err)
		return 1
	}

	v3doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: convert %s: %v\n", inputPath, err)
		return 1
	}

	// Servers must survive conversion: kin-openapi derives servers only when the
	// swagger carries a `host`; a basePath-only document (typical for gw-v1 files
	// patched to serve under a prefix like /api/<domain>/v1) would otherwise
	// silently lose its base URL, and every path would resolve against the wrong
	// root downstream.
	if len(v3doc.Servers) == 0 && doc2.BasePath != "" {
		v3doc.Servers = openapi3.Servers{&openapi3.Server{URL: doc2.BasePath}}
	}

	// Lossless, proto-authoritative enrichment pass (WS-024 Part B). The tolerant
	// (verb, path-template) matching + coverage report is the default for every
	// input (WS-035 R1); -strict opts back into fail-loud.
	rep, err := enrichCompat(v3doc, files, compatOptions{jsonNames: *jsonNames, strict: *strict})
	if rep != nil {
		rep.Input = inputPath
		rep.print(os.Stderr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: enrich %s: %v\n", inputPath, err)
		return 1
	}

	// Serialise to JSON then round-trip through yaml.v3 for clean YAML output.
	jsonBytes, err := json.Marshal(v3doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: marshal v3: %v\n", err)
		return 1
	}
	var raw any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: unmarshal for yaml: %v\n", err)
		return 1
	}
	// Normalise literal tabs in string values to spaces. Brownfield swagger
	// descriptions (atlas collection-operator boilerplate, leaked proto-comment
	// indentation) frequently carry raw tab characters; yaml.v3 emits those as
	// block scalars that other YAML parsers (spectral's JS-YAML, PyYAML) refuse
	// to parse, which would break `apx lint`/`finalize` on the emitted spec.
	raw = sanitizeTabs(raw)
	yamlBytes, err := yaml.Marshal(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: yaml marshal: %v\n", err)
		return 1
	}

	base := filepath.Base(inputPath)
	name := strings.TrimSuffix(base, ".swagger.json")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: mkdir %s: %v\n", outDir, err)
		return 1
	}
	outPath := filepath.Join(outDir, name+".openapi.yaml")
	if err := os.WriteFile(outPath, yamlBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: write %s: %v\n", outPath, err)
		return 1
	}
	fmt.Printf("openapiv2to3: wrote %s\n", outPath)

	// The machine-readable coverage report lands next to the spec ONLY when there
	// is something to review (an unmatched/ambiguous item, a skipped field, a
	// sanitized format, a repaired path, or an uncovered proto method). A clean
	// conversion writes just the spec, so existing consumers (`de api publish`,
	// apx) see no new file in openapi/ (WS-035 R1); the human summary still prints
	// to stderr every run.
	if rep != nil && rep.hasReviewable() {
		covPath := outPath + ".coverage.json"
		covBytes, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "openapiv2to3: marshal coverage: %v\n", err)
			return 1
		}
		if err := os.WriteFile(covPath, append(covBytes, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "openapiv2to3: write %s: %v\n", covPath, err)
			return 1
		}
		fmt.Printf("openapiv2to3: wrote %s\n", covPath)
	}
	return 0
}

// sanitizeTabs recursively replaces literal tab characters in every string
// value with a single space. Tabs are legal inside YAML scalar content per the
// spec, but yaml.v3 renders a multi-line string that contains them as a block
// scalar whose tab-bearing lines other YAML parsers (spectral's JS-YAML,
// PyYAML) reject — so an emitted spec would fail `apx lint`/`finalize`. Tabs in
// OpenAPI descriptions are always cosmetic (proto-comment indentation, boilerplate
// trailing whitespace), so normalising them to spaces is loss-free for the API.
func sanitizeTabs(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\t", " ")
	case map[string]any:
		for k, val := range t {
			t[k] = sanitizeTabs(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = sanitizeTabs(val)
		}
		return t
	default:
		return v
	}
}
