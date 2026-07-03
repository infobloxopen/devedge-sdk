package slo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadJourneyDoc(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "checkout.journey.slo.yaml"))
	if err != nil {
		t.Fatalf("read journey doc: %v", err)
	}
	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("parse journey doc: %v", err)
	}
	return doc
}

// TestJourney_LintsCleanAndClassifiesJourney proves a hand-authored Layer-2 doc
// passes the classifier and is categorized as a journey SLI.
func TestJourney_LintsCleanAndClassifiesJourney(t *testing.T) {
	doc := loadJourneyDoc(t)

	fs := Lint(doc)
	if fs.HasError() {
		t.Fatalf("journey doc must lint clean, got: %+v", fs)
	}

	s := doc.SLOs[0]
	if s.Metadata.Annotations["devedge.io/layer"] != "journey" {
		t.Errorf("journey SLO must carry devedge.io/layer: journey, got %q", s.Metadata.Annotations["devedge.io/layer"])
	}

	// Classify a candidate mirroring the journey SLI.
	cat, cfs := Classify(IndicatorCandidate{
		Name:                 "checkout-availability",
		Layer:                "journey",
		HasErrorBudgetPolicy: true,
	})
	if cat != CategoryJourneySLI {
		t.Errorf("want journey-SLI category, got %s", cat)
	}
	if cfs.HasError() {
		t.Errorf("valid journey candidate should not error: %+v", cfs)
	}
}

// TestJourney_RawQueryRendersPrometheus proves the raw-query source renders
// end-to-end: the composed query appears verbatim (with $window substituted) in
// the recording rules, and the burn-rate alerts reference them.
func TestJourney_RawQueryRendersPrometheus(t *testing.T) {
	doc := loadJourneyDoc(t)
	rs, err := Render(TargetPrometheus, doc, RenderOptions{})
	if err != nil {
		t.Fatalf("render prometheus: %v", err)
	}
	out := string(rs[0].Content)
	// $window is substituted per recording-rule window, not left literal.
	if strings.Contains(out, windowToken) {
		t.Errorf("window token not substituted:\n%s", out)
	}
	// The composed cross-service query appears at multiple windows.
	mustContain(t, out, `slo:sli_error:ratio_rate5m{slo="cartd-read-availability", service="cartd"}`)
	mustContain(t, out, `slo:sli_error:ratio_rate1h{slo="orderd-write-availability", service="orderd"}`)
	// The typed otel-rpc metric names must NOT appear (this is a pure raw-query SLO).
	if strings.Contains(out, "rpc_server_call_duration_seconds") {
		t.Errorf("journey raw-query SLO must not emit typed otel-rpc series:\n%s", out)
	}
	// Burn-rate alerts reference the journey recording rules.
	mustContain(t, out, `CheckoutAvailabilityErrorBudgetBurnFast`)
	mustContain(t, out, "(14.4 * (1 - 0.995))")
	goldenCompare(t, "checkout.prometheusrule.golden.yaml", []byte(out))
}

func TestJourney_RawQueryRendersGrafana(t *testing.T) {
	doc := loadJourneyDoc(t)
	rs, err := Render(TargetGrafana, doc, RenderOptions{})
	if err != nil {
		t.Fatalf("render grafana: %v", err)
	}
	goldenCompare(t, "checkout.dashboard.golden.json", rs[0].Content)
}

// TestJourney_FailsLoudOnEmptySource proves a ratio side with neither a query
// nor a typed signal fails loud, naming the SLO.
func TestJourney_FailsLoudOnEmptySource(t *testing.T) {
	doc := &Document{
		SLIs: []SLI{{
			APIVersion: APIVersion, Kind: KindSLI,
			Metadata: Metadata{Name: "empty"},
			Spec:     SLISpec{RatioMetric: &RatioMetric{Good: MetricSource{}, Total: MetricSource{}}},
		}},
		SLOs: []SLO{{
			APIVersion: APIVersion, Kind: KindSLO,
			Metadata: Metadata{Name: "empty", Annotations: map[string]string{"devedge.io/error-budget-policy": "x"}},
			Spec: SLOSpec{
				Service: "empty", IndicatorRef: "empty", Objectives: []Objective{{Target: 0.99}},
				AlertPolicies: []string{"p"},
			},
		}},
		AlertPolicies:   []AlertPolicy{{Metadata: Metadata{Name: "p"}, Spec: AlertPolicySpec{Conditions: []AlertPolicyCondition{{ConditionRef: "c"}}}}},
		AlertConditions: []AlertCondition{{Metadata: Metadata{Name: "c"}, Spec: AlertConditionSpec{Condition: AlertConditionDetail{LookbackWindow: "1h", ShortWindow: "5m"}}}},
	}
	_, err := Render(TargetPrometheus, doc, RenderOptions{})
	if err == nil {
		t.Fatal("a source with neither query nor typed signal must fail loud")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should name the SLO: %v", err)
	}
}
