package slo

import (
	"encoding/json"
	"testing"
)

func TestEmitGrafana_Golden(t *testing.T) {
	doc := deriveToyDoc(t)
	rs, err := Render(TargetGrafana, doc, RenderOptions{})
	if err != nil {
		t.Fatalf("render grafana: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 dashboard, got %d", len(rs))
	}
	// Must be valid JSON.
	var probe map[string]any
	if err := json.Unmarshal(rs[0].Content, &probe); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	if probe["uid"] != "widget-service-slo" {
		t.Errorf("unexpected dashboard uid: %v", probe["uid"])
	}
	goldenCompare(t, "toy.dashboard.golden.json", rs[0].Content)
}

func TestEmitLoki_Minimal(t *testing.T) {
	doc := deriveToyDoc(t)
	rs, err := Render(TargetLoki, doc, RenderOptions{})
	if err != nil {
		t.Fatalf("render loki: %v", err)
	}
	out := string(rs[0].Content)
	mustContain(t, out, "slo:log_sli_error:ratio_rate5m")
	mustContain(t, out, "logfmt")
}

func TestRender_UnknownTargetFailsLoud(t *testing.T) {
	_, err := Render("datadog", &Document{}, RenderOptions{})
	if err == nil {
		t.Fatal("unknown target must be an error")
	}
}
