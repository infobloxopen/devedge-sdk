package slo

import (
	"strings"
	"testing"
)

func TestDeriveFromOpenAPI_Golden(t *testing.T) {
	doc := deriveToyDoc(t)
	got, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	goldenCompare(t, "toy.slo.golden.yaml", got)
}

func TestDeriveShapeAndGoodConstruction(t *testing.T) {
	doc := deriveToyDoc(t)

	// AC-1: exactly four grouped SLOs (read/write × availability/latency).
	if len(doc.SLOs) != 4 {
		t.Fatalf("want 4 SLOs, got %d", len(doc.SLOs))
	}
	for _, s := range doc.SLOs {
		// D8: every SLO carries an error-budget policy stub + an AlertPolicy ref.
		if strings.TrimSpace(s.Metadata.Annotations["devedge.io/error-budget-policy"]) == "" {
			t.Errorf("SLO %q missing error-budget-policy annotation", s.Metadata.Name)
		}
		if len(s.Spec.AlertPolicies) == 0 {
			t.Errorf("SLO %q missing alertPolicies", s.Metadata.Name)
		}
		// D7: objectives marked un-calibrated; 28d rolling window.
		if s.Metadata.Annotations["devedge.io/uncalibrated"] != "true" {
			t.Errorf("SLO %q not marked uncalibrated", s.Metadata.Name)
		}
		if len(s.Spec.TimeWindow) != 1 || s.Spec.TimeWindow[0].Duration != "28d" || !s.Spec.TimeWindow[0].IsRolling {
			t.Errorf("SLO %q window not 28d rolling: %+v", s.Metadata.Name, s.Spec.TimeWindow)
		}
	}

	// AC-2/FM-4: the read-availability SLI's good excludes BOTH client + server
	// faults; total (valid) excludes ONLY client faults; a client-fault code is
	// never counted as bad (it is in total's exclude set, i.e. absent from valid).
	sli := doc.sliByName("widget-service-read-availability")
	if sli == nil {
		t.Fatalf("read-availability SLI not found; names: %v", sliNames(doc))
	}
	good := sli.Spec.RatioMetric.Good.Spec.ExcludeStatuses
	total := sli.Spec.RatioMetric.Total.Spec.ExcludeStatuses
	if !containsAll(good, GRPCServerFaultStatuses) || !containsAll(good, GRPCClientFaultStatuses) {
		t.Errorf("good must exclude client+server faults, got %v", good)
	}
	if !equalSet(total, GRPCClientFaultStatuses) {
		t.Errorf("valid (total) must exclude ONLY client faults, got %v", total)
	}
	// FM-4: NOT_FOUND (client fault) must not be in the server-fault set.
	if contains(GRPCServerFaultStatuses, "NOT_FOUND") {
		t.Errorf("NOT_FOUND must not be a server fault")
	}

	// The derived doc must lint clean (only uncalibrated warnings, no errors).
	fs := Lint(doc)
	if fs.HasError() {
		t.Errorf("derived doc has lint errors: %+v", fs)
	}
}

func sliNames(doc *Document) []string {
	var out []string
	for _, s := range doc.SLIs {
		out = append(out, s.Metadata.Name)
	}
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func containsAll(s, want []string) bool {
	for _, w := range want {
		if !contains(s, w) {
			return false
		}
	}
	return true
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return containsAll(a, b) && containsAll(b, a)
}
