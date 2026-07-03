package slo

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PrometheusRule (monitoring.coreos.com/v1) shapes. Cortex's ruler loads the same
// group/rule format, so this CR is Cortex-ruler-compatible.
type prometheusRule struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   promMeta     `yaml:"metadata"`
	Spec       promRuleSpec `yaml:"spec"`
}

type promMeta struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type promRuleSpec struct {
	Groups []promGroup `yaml:"groups"`
}

type promGroup struct {
	Name  string          `yaml:"name"`
	Rules []promRuleEntry `yaml:"rules"`
}

type promRuleEntry struct {
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// windowRank orders rate windows deterministically (short → long).
var windowRank = map[string]int{"5m": 0, "30m": 1, "1h": 2, "6h": 3, "1d": 4, "3d": 5}

func rankWindow(w string) int {
	if r, ok := windowRank[w]; ok {
		return r
	}
	return 99
}

// emitPrometheus projects the Document to a Cortex-ruler PrometheusRule: SLI
// error-ratio recording rules (one series per rate window) + multi-window
// multi-burn-rate alerting rules. Query strings reference the ground-truth
// normalized metric names via naming (WS-025 D5), pinned by a golden test.
func emitPrometheus(doc *Document, naming MetricNaming) ([]Rendered, error) {
	recGroup := promGroup{Name: "devedge-slo-sli-recordings"}
	alertGroup := promGroup{Name: "devedge-slo-burn-rate-alerts"}

	for i := range doc.SLOs {
		s := &doc.SLOs[i]
		sli := doc.sliByName(s.Spec.IndicatorRef)
		if sli == nil && s.Spec.Indicator != nil {
			sli = s.Spec.Indicator
		}
		if sli == nil || sli.Spec.RatioMetric == nil {
			return nil, fmt.Errorf("slo: emit prometheus: SLO %q has no resolvable ratio indicator", s.Metadata.Name)
		}
		if len(s.Spec.Objectives) == 0 {
			return nil, fmt.Errorf("slo: emit prometheus: SLO %q has no objective", s.Metadata.Name)
		}
		target := s.Spec.Objectives[0].Target
		conds := resolveConditions(doc, s)

		// Recording rules: one error-ratio series per distinct window across the
		// SLO's burn-rate conditions.
		for _, w := range conditionWindows(conds) {
			recGroup.Rules = append(recGroup.Rules, promRuleEntry{
				Record: recordName(w),
				Expr:   errorRatioExpr(naming, sli.Spec.RatioMetric, w),
				Labels: recordLabels(s),
			})
		}

		// Alerting rules: one per MWMBR tier (fast/medium/slow).
		for _, c := range conds {
			alertGroup.Rules = append(alertGroup.Rules, burnAlert(s, c, target))
		}
	}

	rule := prometheusRule{
		APIVersion: "monitoring.coreos.com/v1",
		Kind:       "PrometheusRule",
		Metadata: promMeta{
			Name:   docName(doc) + "-slo-rules",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "devedge", "app.kubernetes.io/part-of": "devedge-slo"},
		},
		Spec: promRuleSpec{Groups: []promGroup{recGroup, alertGroup}},
	}
	b, err := yaml.Marshal(&rule)
	if err != nil {
		return nil, err
	}
	return []Rendered{{Filename: docName(doc) + "-slo.prometheusrule.yaml", Content: b}}, nil
}

// recordName is the recording-rule metric name for a window.
func recordName(window string) string {
	return "slo:sli_error:ratio_rate" + window
}

func recordLabels(s *SLO) map[string]string {
	return map[string]string{
		"slo":          s.Metadata.Name,
		"service":      s.Spec.Service,
		"sli_type":     s.Metadata.Labels["devedge.io/sli-type"],
		"method_class": s.Metadata.Labels["devedge.io/method-class"],
	}
}

// errorRatioExpr builds `1 - (good / total)` at a window — the error fraction the
// burn-rate alert compares to the budget. Uniform across availability and
// latency (for latency, good = under-threshold, so 1-good/total = the fraction
// over the threshold).
func errorRatioExpr(naming MetricNaming, rm *RatioMetric, window string) string {
	good := rateExpr(naming, rm.Good.Spec, window)
	total := rateExpr(naming, rm.Total.Spec, window)
	return fmt.Sprintf("1 - (%s / %s)", good, total)
}

// rateExpr builds a windowed sum(rate(...)) over the normalized series for one
// side of the ratio.
func rateExpr(naming MetricNaming, src OTelRatioSource, window string) string {
	var series, le string
	if src.SLIType == SLITypeLatency && src.LatencyThresholdSeconds > 0 {
		series = naming.series(src.Signal, "_bucket")
		le = `le="` + formatFloat(src.LatencyThresholdSeconds) + `"`
	} else {
		series = naming.series(src.Signal, "_count")
	}
	sel := selector(
		eqMatcher(naming.ServiceLabel, src.Service),
		reMatcher(naming.MethodLabel, src.Methods),
		notReMatcher(naming.StatusLabel, src.ExcludeStatuses),
		le,
	)
	return fmt.Sprintf("sum(rate(%s%s[%s]))", series, sel, window)
}

// burnAlert builds one MWMBR alerting rule: fire when the error ratio exceeds
// burn·budget over BOTH the long and short windows.
func burnAlert(s *SLO, c AlertCondition, target float64) promRuleEntry {
	long, short := c.Spec.Condition.LookbackWindow, c.Spec.Condition.ShortWindow
	burn := formatFloat(c.Spec.Condition.Threshold)
	budget := "(1 - " + formatFloat(target) + ")"
	sel := `{slo="` + s.Metadata.Name + `"}`
	threshold := "(" + burn + " * " + budget + ")"
	expr := fmt.Sprintf("%s%s > %s and %s%s > %s",
		recordName(long), sel, threshold,
		recordName(short), sel, threshold)
	tier := tierName(c.Metadata.Name)
	return promRuleEntry{
		Alert: alertName(s.Metadata.Name, tier),
		Expr:  expr,
		For:   alertFor(c.Spec.Severity),
		Labels: map[string]string{
			"severity": c.Spec.Severity,
			"slo":      s.Metadata.Name,
			"service":  s.Spec.Service,
		},
		Annotations: map[string]string{
			"summary":     fmt.Sprintf("%s burning error budget (%s burn over %s & %s)", s.Metadata.Name, burn, long, short),
			"description": fmt.Sprintf("SLO %q is burning its error budget at >%s× over %s and %s. Objective %s.", s.Metadata.Name, burn, long, short, formatFloat(target)),
			"runbook":     "TODO: link the error-budget-policy runbook (define with `de slo` / define-slo skill).",
		},
	}
}

func alertFor(sev string) string {
	if sev == "page" {
		return "2m"
	}
	return "15m"
}

func tierName(condName string) string {
	switch {
	case strings.HasSuffix(condName, "-fast"):
		return "Fast"
	case strings.HasSuffix(condName, "-medium"):
		return "Medium"
	case strings.HasSuffix(condName, "-slow"):
		return "Slow"
	default:
		return "Burn"
	}
}

func alertName(sloName, tier string) string {
	parts := strings.FieldsFunc(sloName, func(r rune) bool { return r == '-' || r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	b.WriteString("ErrorBudgetBurn" + tier)
	return b.String()
}

// resolveConditions returns the burn-rate conditions an SLO's alert policies
// reference, in a deterministic burn order (fast → slow).
func resolveConditions(doc *Document, s *SLO) []AlertCondition {
	var out []AlertCondition
	seen := map[string]bool{}
	for _, pn := range s.Spec.AlertPolicies {
		for i := range doc.AlertPolicies {
			p := &doc.AlertPolicies[i]
			if p.Metadata.Name != pn {
				continue
			}
			for _, cr := range p.Spec.Conditions {
				for j := range doc.AlertConditions {
					c := doc.AlertConditions[j]
					if c.Metadata.Name == cr.ConditionRef && !seen[c.Metadata.Name] {
						seen[c.Metadata.Name] = true
						out = append(out, c)
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Spec.Condition.Threshold > out[j].Spec.Condition.Threshold
	})
	return out
}

// conditionWindows returns the distinct rate windows across conditions, ordered
// short → long.
func conditionWindows(conds []AlertCondition) []string {
	set := map[string]bool{}
	for _, c := range conds {
		if c.Spec.Condition.LookbackWindow != "" {
			set[c.Spec.Condition.LookbackWindow] = true
		}
		if c.Spec.Condition.ShortWindow != "" {
			set[c.Spec.Condition.ShortWindow] = true
		}
	}
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return rankWindow(out[i]) < rankWindow(out[j]) })
	return out
}

// docName returns a stable name for the whole document (first service, else "slo").
func docName(doc *Document) string {
	if len(doc.Services) > 0 {
		return doc.Services[0].Metadata.Name
	}
	if len(doc.SLOs) > 0 && doc.SLOs[0].Spec.Service != "" {
		return doc.SLOs[0].Spec.Service
	}
	return "slo"
}
