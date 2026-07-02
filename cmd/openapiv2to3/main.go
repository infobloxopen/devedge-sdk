// Command openapiv2to3 converts a grpc-gateway-emitted OpenAPI v2 (.swagger.json)
// document into an OpenAPI 3.0 YAML document, then runs a lossless ENRICHMENT
// pass over it from a proto FileDescriptorSet so the published spec carries the
// full AIP contract (field_behavior, resource identity, method classification,
// references, pagination) — the single interchange every downstream generator
// reads (WS-024 D1). The enrichment shares the internal/aip resolver/classifier
// with the codegen plugins so compiled behavior and published OpenAPI cannot
// drift (D-new-1).
//
// Usage: openapiv2to3 -descriptor <fds.binpb> <input.swagger.json> [output-dir]
// The output lands at <output-dir>/<name>.openapi.yaml (default: openapi/ beside
// the input).
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
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: %v\n", err)
		return 1
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintf(os.Stderr, "usage: openapiv2to3 -descriptor <fds.binpb> <input.swagger.json> [output-dir]\n")
		return 1
	}
	inputPath := rest[0]
	outDir := filepath.Join(filepath.Dir(inputPath), "openapi")
	if len(rest) >= 2 {
		outDir = rest[1]
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

	// Lossless, proto-authoritative enrichment pass (WS-024 Part B).
	if err := enrich(v3doc, files); err != nil {
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
	return 0
}
