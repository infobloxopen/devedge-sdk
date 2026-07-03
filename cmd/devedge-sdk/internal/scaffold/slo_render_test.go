package scaffold

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/infobloxopen/devedge-sdk/slo"
)

// TestRenderSLO_EmitsGoodDefault proves a scaffolded service gets a starter
// slo.yaml on disk that the WS-025 classifier passes (only un-calibrated
// warnings, no errors) — reliability-by-default (AC-8).
func TestRenderSLO_EmitsGoodDefault(t *testing.T) {
	m, err := Options{
		Service:  "orders",
		Resource: "Order",
		Backend:  BackendGORM,
		Dir:      t.TempDir(),
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	dir := t.TempDir()
	if err := renderSLO(dir, m); err != nil {
		t.Fatalf("renderSLO: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "slo.yaml"))
	if err != nil {
		t.Fatalf("slo.yaml not written: %v", err)
	}
	doc, err := slo.Parse(data)
	if err != nil {
		t.Fatalf("parse slo.yaml: %v", err)
	}
	if len(doc.SLOs) != 4 {
		t.Errorf("want 4 grouped SLOs, got %d", len(doc.SLOs))
	}
	fs := slo.Lint(doc)
	if fs.HasError() {
		t.Errorf("scaffold slo.yaml must lint clean (no errors), got: %+v", fs)
	}
	// The rpc.service label filter is the proto FQN.
	found := false
	for _, sli := range doc.SLIs {
		if sli.Spec.RatioMetric != nil && sli.Spec.RatioMetric.Good.Spec.Service == "orders.v1.OrderService" {
			found = true
		}
	}
	if !found {
		t.Errorf("SLIs should filter by the proto FQN rpc.service label orders.v1.OrderService")
	}
}
