package server

import (
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// TestAssertReferenceTargets is F041 AC-3 (unit half): a cross-service reference
// whose target type serves BatchGet passes; one whose target does not FAILS loud.
func TestAssertReferenceTargets(t *testing.T) {
	regionRef := reference.Reference{
		FieldName:   "RegionId",
		FKField:     "region_id",
		TargetType:  "region.example.com/Region",
		Cardinality: reference.One,
	}

	t.Run("target serves BatchGet -> ok", func(t *testing.T) {
		targets := map[string]struct{}{"region.example.com/Region": {}}
		if err := AssertReferenceTargets(targets, []reference.Reference{regionRef}); err != nil {
			t.Fatalf("want no error, got %v", err)
		}
	})

	t.Run("target lacks BatchGet -> fail loud", func(t *testing.T) {
		// No batch targets registered.
		err := AssertReferenceTargets(map[string]struct{}{}, []reference.Reference{regionRef})
		if err == nil {
			t.Fatal("want a fail-loud error, got nil")
		}
		if !strings.Contains(err.Error(), "region.example.com/Region") ||
			!strings.Contains(err.Error(), "BatchGet") {
			t.Fatalf("error should name the target type and BatchGet: %v", err)
		}
	})

	t.Run("no references -> ok", func(t *testing.T) {
		if err := AssertReferenceTargets(nil, nil); err != nil {
			t.Fatalf("want no error, got %v", err)
		}
	})
}

// TestServer_ReferenceGateAtServe proves the gate runs over the server's
// accumulated records: a recorded reference without a matching batch target makes
// the (real) gate over the server state fail closed.
func TestServer_ReferenceGateAtServe(t *testing.T) {
	s := &Server{}
	s.RecordReferences(reference.Reference{
		FieldName:  "RegionId",
		FKField:    "region_id",
		TargetType: "region.example.com/Region",
	})
	// No RecordBatchTarget call -> the target is not batch-fetchable.
	if err := AssertReferenceTargets(s.batchTargets, s.references); err == nil {
		t.Fatal("want the reference gate over server state to fail, got nil")
	}

	// Now declare the target batch-fetchable -> the gate passes.
	s.RecordBatchTarget("region.example.com/Region")
	if err := AssertReferenceTargets(s.batchTargets, s.references); err != nil {
		t.Fatalf("want the gate to pass once the target serves BatchGet, got %v", err)
	}
}

// TestServer_ExternalReferenceTarget proves the split-microservice opt-out
// (finding 067): a reference whose target is served in ANOTHER process boots once
// declared external, without falsely advertising a local BatchGet.
func TestServer_ExternalReferenceTarget(t *testing.T) {
	s := &Server{}
	s.RecordReferences(reference.Reference{
		FieldName:  "VendorId",
		FKField:    "vendor_id",
		TargetType: "vendord.v1/Vendor",
	})
	// Before declaring the target, the gate fails closed (it has no local BatchGet).
	if err := AssertReferenceTargets(s.satisfiableTargets(), s.references); err == nil {
		t.Fatal("want the gate to fail before the external target is declared")
	}

	s.RecordExternalReferenceTarget("vendord.v1/Vendor")
	if err := AssertReferenceTargets(s.satisfiableTargets(), s.references); err != nil {
		t.Fatalf("want the gate to pass once the target is declared external, got %v", err)
	}
	// Honesty: an external target must NOT be recorded as a local BatchGet.
	if _, isLocalBatch := s.batchTargets["vendord.v1/Vendor"]; isLocalBatch {
		t.Fatal("external target must not be advertised as a local BatchGet")
	}
}
