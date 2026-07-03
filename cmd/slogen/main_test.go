package main

import (
	"os"
	"path/filepath"
	"testing"
)

// run executes the slogen CLI with args, returning the error (nil on success).
func run(args ...string) error {
	c := newRootCmd()
	c.SetArgs(args)
	return c.Execute()
}

// TestCLIRoundTrip exercises generate → lint → render against the toy fixture,
// proving the CLI contract `de` orchestrates.
func TestCLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	openapi := filepath.Join("..", "..", "slo", "testdata", "toy.openapi.yaml")
	sloPath := filepath.Join(dir, "slo.yaml")

	if err := run("generate", "--openapi", openapi, "--service", "toy.v1.WidgetService", "--out", sloPath); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := os.Stat(sloPath); err != nil {
		t.Fatalf("slo.yaml not written: %v", err)
	}

	// The generated doc lints clean (warnings only).
	if err := run("lint", sloPath); err != nil {
		t.Fatalf("lint of generated doc should pass: %v", err)
	}

	// Render each target.
	outDir := filepath.Join(dir, "out")
	for _, target := range []string{"prometheus", "grafana", "loki"} {
		if err := run("render", "--target", target, "--in", sloPath, "--out", outDir); err != nil {
			t.Fatalf("render %s: %v", target, err)
		}
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) < 3 {
		t.Fatalf("want >=3 rendered files, got %d", len(entries))
	}
}

// TestCLILintFailsLoud proves lint exits non-zero on a saturation-metric SLI.
func TestCLILintFailsLoud(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	content := `apiVersion: openslo/v1
kind: SLI
metadata:
    name: mem
spec:
    ratioMetric:
        counter: true
        good:
            type: devedge/otel-rpc
            spec:
                signal: container_memory_utilization
        total:
            type: devedge/otel-rpc
            spec:
                signal: container_memory_utilization
`
	if err := os.WriteFile(bad, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("lint", bad); err == nil {
		t.Fatal("lint must fail on a saturation-metric SLI")
	}
}
