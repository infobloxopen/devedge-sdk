package slo

import (
	"strings"
	"testing"
)

func TestClassify_SaturationIsSignal(t *testing.T) {
	cat, fs := Classify(IndicatorCandidate{
		Name: "widget-memory", Metric: "container_memory_utilization", HasErrorBudgetPolicy: true,
	})
	if cat != CategorySignal {
		t.Errorf("want signal, got %s", cat)
	}
	if !fs.HasError() {
		t.Fatalf("want error finding, got %+v", fs)
	}
	if !strings.Contains(fs[0].Message, "Layer-0 signal") {
		t.Errorf("message should name the Layer-0 signal error: %s", fs[0].Message)
	}
}

func TestClassify_MissingPolicyIsError(t *testing.T) {
	_, fs := Classify(IndicatorCandidate{Name: "checkout-availability", Metric: "rpc.server.call.duration"})
	if !fs.HasError() {
		t.Fatalf("want error for missing policy, got %+v", fs)
	}
}

func TestClassify_Journey(t *testing.T) {
	cat, fs := Classify(IndicatorCandidate{
		Name: "checkout-journey", Metric: "rpc.server.call.duration", Layer: "journey", HasErrorBudgetPolicy: true,
	})
	if cat != CategoryJourneySLI {
		t.Errorf("want journey-SLI, got %s", cat)
	}
	if fs.HasError() {
		t.Errorf("valid journey SLI should not error: %+v", fs)
	}
}

func TestClassify_CauseBasedWarns(t *testing.T) {
	_, fs := Classify(IndicatorCandidate{Name: "pod-restarts", Metric: "pod_restart_total", HasErrorBudgetPolicy: true})
	// pod_restart is not saturation; it is cause-based → warn (not error).
	if fs.HasError() {
		t.Errorf("cause-based should warn, not error: %+v", fs)
	}
	if len(fs) == 0 || fs[0].Severity != SeverityWarn {
		t.Errorf("want a warn finding, got %+v", fs)
	}
}

func TestLint_SaturationSLIRejected(t *testing.T) {
	doc := &Document{
		SLIs: []SLI{{
			APIVersion: APIVersion, Kind: KindSLI,
			Metadata: Metadata{Name: "mem-pressure"},
			Spec: SLISpec{RatioMetric: &RatioMetric{
				Good:  MetricSource{Type: MetricSourceTypeOTel, Spec: OTelRatioSource{Signal: "container_memory_utilization"}},
				Total: MetricSource{Type: MetricSourceTypeOTel, Spec: OTelRatioSource{Signal: "container_memory_utilization"}},
			}},
		}},
		SLOs: []SLO{{
			APIVersion: APIVersion, Kind: KindSLO,
			Metadata: Metadata{Name: "mem-pressure", Annotations: map[string]string{"devedge.io/error-budget-policy": "x"}},
			Spec:     SLOSpec{IndicatorRef: "mem-pressure", Objectives: []Objective{{Target: 0.99}}, AlertPolicies: []string{"p"}},
		}},
	}
	fs := Lint(doc)
	if !fs.HasError() {
		t.Fatalf("saturation-as-SLI must be a lint error, got %+v", fs)
	}
	found := false
	for _, f := range fs {
		if f.Severity == SeverityError && strings.Contains(f.Message, "Layer-0 signal") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a Layer-0-signal error finding, got %+v", fs)
	}
}

func TestLint_MissingErrorBudgetPolicyRejected(t *testing.T) {
	doc := deriveToyDoc(t)
	// Strip one SLO's policy entirely.
	doc.SLOs[0].Spec.AlertPolicies = nil
	delete(doc.SLOs[0].Metadata.Annotations, "devedge.io/error-budget-policy")
	fs := Lint(doc)
	if !fs.HasError() {
		t.Fatalf("SLO without error-budget policy must be a lint error, got %+v", fs)
	}
}

func TestIsSaturationSignal(t *testing.T) {
	sat := []string{"container_memory_utilization", "cpu_usage_seconds", "go_goroutines", "queue_depth", "db_pool_saturation", "disk_used_bytes"}
	for _, s := range sat {
		if !IsSaturationSignal(s) {
			t.Errorf("%q should be a saturation signal", s)
		}
	}
	notSat := []string{"rpc.server.call.duration", "http.server.request.duration", "checkout_availability"}
	for _, s := range notSat {
		if IsSaturationSignal(s) {
			t.Errorf("%q should NOT be a saturation signal", s)
		}
	}
}
