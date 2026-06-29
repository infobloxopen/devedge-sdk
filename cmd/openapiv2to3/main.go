// Command openapiv2to3 converts a grpc-gateway-emitted OpenAPI v2 (.swagger.json)
// document into an OpenAPI 3.0 YAML document, writing it to openapi/<name>.openapi.yaml.
// Usage: openapiv2to3 <input.swagger.json> [output-dir]
// If output-dir is omitted it defaults to "openapi/" relative to the input file's directory.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: openapiv2to3 <input.swagger.json> [output-dir]\n")
		os.Exit(1)
	}
	inputPath := os.Args[1]
	outDir := filepath.Join(filepath.Dir(inputPath), "openapi")
	if len(os.Args) >= 3 {
		outDir = os.Args[2]
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: read %s: %v\n", inputPath, err)
		os.Exit(1)
	}

	var doc2 openapi2.T
	if err := json.Unmarshal(data, &doc2); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: parse v2 %s: %v\n", inputPath, err)
		os.Exit(1)
	}

	v3doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: convert %s: %v\n", inputPath, err)
		os.Exit(1)
	}

	// Serialise to JSON then round-trip through yaml.v3 for clean YAML output.
	jsonBytes, err := json.Marshal(v3doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: marshal v3: %v\n", err)
		os.Exit(1)
	}
	var raw any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: unmarshal for yaml: %v\n", err)
		os.Exit(1)
	}
	yamlBytes, err := yaml.Marshal(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: yaml marshal: %v\n", err)
		os.Exit(1)
	}

	base := filepath.Base(inputPath)
	name := strings.TrimSuffix(base, ".swagger.json")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: mkdir %s: %v\n", outDir, err)
		os.Exit(1)
	}
	outPath := filepath.Join(outDir, name+".openapi.yaml")
	if err := os.WriteFile(outPath, yamlBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "openapiv2to3: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
	fmt.Printf("openapiv2to3: wrote %s\n", outPath)
}
