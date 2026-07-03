package slo

import (
	"os"
	"path/filepath"
	"testing"
)

// goldenCompare compares got against the golden file at testdata/<name>. Set
// UPDATE_GOLDEN=1 to (re)write the golden.
func goldenCompare(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// deriveToyDoc derives the OpenSLO doc from the toy enriched OpenAPI fixture.
func deriveToyDoc(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "toy.openapi.yaml"))
	if err != nil {
		t.Fatalf("read toy openapi: %v", err)
	}
	doc, err := DefaultsFromOpenAPI(data, "toy.v1.WidgetService", DefaultDeriveOptions())
	if err != nil {
		t.Fatalf("DefaultsFromOpenAPI: %v", err)
	}
	return doc
}
