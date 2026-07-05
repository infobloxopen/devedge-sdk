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

func TestLint_PlaceholderPolicyAndUnmarkedDefaultTargetWarned(t *testing.T) {
	// DX run 24 / finding 110: the two "human did the work" gates were gameable —
	// a "TODO" placeholder policy and a default target with the uncalibrated marker
	// simply deleted both passed as "OK: no findings". They must now warn.
	doc := &Document{
		SLOs: []SLO{
			{ // scaffold's "TODO:" placeholder policy, real (non-default) target
				APIVersion: APIVersion, Kind: KindSLO,
				Metadata: Metadata{Name: "placeholder-policy", Annotations: map[string]string{
					"devedge.io/error-budget-policy": "TODO: define the consequence when this budget is spent.",
				}},
				Spec: SLOSpec{Objectives: []Objective{{Target: 0.9973}}, AlertPolicies: []string{"p"}},
			},
			{ // default target, uncalibrated marker dropped, real policy (the gamed case)
				APIVersion: APIVersion, Kind: KindSLO,
				Metadata: Metadata{Name: "unmarked-default", Annotations: map[string]string{
					"devedge.io/error-budget-policy": "Freeze releases until the 28d budget recovers over a full window.",
				}},
				Spec: SLOSpec{Objectives: []Objective{{Target: 0.999}}, AlertPolicies: []string{"p"}},
			},
		},
	}
	fs := Lint(doc)
	if fs.HasError() {
		t.Fatalf("neither gamed case should be a hard error (warn-only), got %+v", fs)
	}
	var gotPlaceholder, gotDefault bool
	for _, f := range fs {
		if f.Object == "placeholder-policy" && strings.Contains(f.Message, "placeholder") {
			gotPlaceholder = true
		}
		if f.Object == "unmarked-default" && strings.Contains(f.Message, "generated default") {
			gotDefault = true
		}
	}
	if !gotPlaceholder {
		t.Errorf("want a placeholder-policy warning, got %+v", fs)
	}
	if !gotDefault {
		t.Errorf("want an unmarked-default-target warning, got %+v", fs)
	}
}

func TestLint_LatencyThresholdMustBeBucketBoundary(t *testing.T) {
	// A calibrated threshold that is not a histogram bucket boundary makes the
	// le="..." matcher select no series → the latency burn-rate alert silently
	// never fires. Lint must reject it.
	base := deriveToyDoc(t)

	// Baseline: the derived defaults (0.25 read, 0.5 write) ARE boundaries →
	// no latency-boundary error.
	for _, f := range Lint(base) {
		if f.Severity == SeverityError {
			t.Fatalf("derived doc (0.25/0.5 thresholds) must have no error findings, got: %+v", f)
		}
	}

	// Mutate a latency SLI to 0.18 (between the 0.1 and 0.25 buckets) → error.
	doc := deriveToyDoc(t)
	mutated := false
	for i := range doc.SLIs {
		if rm := doc.SLIs[i].Spec.RatioMetric; rm != nil && rm.Good.Spec.SLIType == SLITypeLatency {
			doc.SLIs[i].Spec.RatioMetric.Good.Spec.LatencyThresholdSeconds = 0.18
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("no latency SLI found to mutate")
	}
	fs := Lint(doc)
	if !fs.HasError() {
		t.Fatalf("threshold 0.18 must be a lint error, got: %+v", fs)
	}
	found := false
	for _, f := range fs {
		if f.Severity == SeverityError && strings.Contains(f.Message, "not a histogram bucket boundary") && strings.Contains(f.Message, "0.25") {
			found = true
		}
	}
	if !found {
		t.Errorf("want an error naming 0.18 and the nearest boundary 0.25, got: %+v", fs)
	}

	// A custom boundary set makes 0.18 valid (service customized its buckets).
	custom := DefaultGRPCNaming()
	custom.LatencyBucketBoundaries = []float64{0.05, 0.18, 0.5, 1}
	for _, f := range LintWithConfig(doc, custom) {
		if f.Severity == SeverityError {
			t.Fatalf("0.18 must be valid under a custom boundary set, got: %+v", f)
		}
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
