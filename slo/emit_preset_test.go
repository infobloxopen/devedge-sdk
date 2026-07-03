package slo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRender_PresetDir proves the --preset-dir seam: when the dir holds a
// "<target>.tmpl", Render executes it (with the Document + the built-in emitter
// helpers) instead of the built-in emitter. This is what the internal
// Grafana-Operator overlay uses to wrap the same artifacts in CRs.
func TestRender_PresetDir(t *testing.T) {
	dir := t.TempDir()
	tmpl := `apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: {{ (index .Services 0).Metadata.Name }}-slo
spec:
  json: |
    {{ range (grafanaDashboards .) }}{{ printf "%s" .Content }}{{ end }}
`
	if err := os.WriteFile(filepath.Join(dir, "grafana.tmpl"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := deriveToyDoc(t)
	rs, err := Render(TargetGrafana, doc, RenderOptions{PresetDir: dir})
	if err != nil {
		t.Fatalf("render preset: %v", err)
	}
	out := string(rs[0].Content)
	if !strings.Contains(out, "kind: GrafanaDashboard") {
		t.Errorf("preset output should be the CR wrapper, got:\n%s", out)
	}
	if !strings.Contains(out, "widget-service-slo") {
		t.Errorf("preset should name the service: %s", out)
	}
	// Absent preset falls back to the built-in emitter.
	rs2, err := Render(TargetGrafana, doc, RenderOptions{PresetDir: t.TempDir()})
	if err != nil {
		t.Fatalf("render fallback: %v", err)
	}
	if strings.Contains(string(rs2[0].Content), "GrafanaDashboard") {
		t.Errorf("missing preset should fall back to built-in dashboard JSON")
	}
}
