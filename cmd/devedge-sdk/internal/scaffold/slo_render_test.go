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

	// F070: the slug derives from the gRPC service short name (OrderService ->
	// order-service), matching `de slo generate`, NOT the binary name "orders".
	for _, s := range doc.SLOs {
		if s.Spec.Service != "order-service" {
			t.Errorf("SLO %q service slug = %q, want order-service (the gRPC short name)", s.Metadata.Name, s.Spec.Service)
		}
	}
	// F070: no phantom methods — the scaffold proto has no BatchGet or Undelete RPC.
	for _, sli := range doc.SLIs {
		if sli.Spec.RatioMetric == nil {
			continue
		}
		for _, meth := range sli.Spec.RatioMetric.Good.Spec.Methods {
			if meth == "BatchGetOrders" || meth == "UndeleteOrder" {
				t.Errorf("phantom method %q in SLI %q (scaffold proto has no such RPC)", meth, sli.Metadata.Name)
			}
		}
	}
}
