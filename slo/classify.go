package slo

import (
	"fmt"
	"strings"
)

// Category is the declaration layer an indicator belongs to (WS-025 three
// layers). A saturation/resource metric is a Layer-0 signal, never an objective.
type Category string

const (
	// CategorySignal is a Layer-0 monitoring signal (RED/golden/USE). No target;
	// declaring it as an SLI is a category error.
	CategorySignal Category = "signal"
	// CategoryServiceSLI is a Layer-1 service indicator (availability/latency).
	CategoryServiceSLI Category = "service-SLI"
	// CategoryJourneySLI is a Layer-2 business/journey indicator across services.
	CategoryJourneySLI Category = "journey-SLI"
)

// Severity of a classifier finding. An error fails `slogen lint`; a warn does not.
type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
)

// Finding is one classifier result.
type Finding struct {
	Severity Severity `json:"severity" yaml:"severity"`
	Object   string   `json:"object" yaml:"object"`
	Kind     string   `json:"kind" yaml:"kind"`
	Message  string   `json:"message" yaml:"message"`
}

// Findings is the classifier's result set.
type Findings []Finding

// HasError reports whether any finding is error-severity (a lint failure).
func (fs Findings) HasError() bool {
	for _, f := range fs {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// saturationTokens name Layer-0 resource/saturation signals. A metric whose name
// contains one of these tokens is a signal, not an objective (WS-025 G2/FM-1).
var saturationTokens = map[string]bool{
	"cpu": true, "memory": true, "mem": true, "ram": true,
	"queue": true, "depth": true, "backlog": true, "inflight": true,
	"pool": true, "disk": true, "storage": true,
	"goroutine": true, "goroutines": true, "threads": true, "thread": true,
	"heap": true, "gc": true, "fd": true, "fds": true,
	"descriptor": true, "descriptors": true,
	"connections": true, "connection": true, "conns": true,
	"utilization": true, "saturation": true, "usage": true, "used": true,
	"load": true, "capacity": true, "limit": true,
}

// causeTokens name cause-based (non-symptom) indicators. Prefer a symptom users
// feel (availability/latency/error rate) — these are flagged, not rejected.
var causeTokens = map[string]bool{
	"restart": true, "restarts": true, "oom": true, "oomkill": true,
	"retry": true, "retries": true, "evictions": true, "eviction": true,
	"panic": true, "panics": true, "throttled": true, "throttle": true,
}

func tokenize(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/' || r == ' ' || r == ':'
	})
}

// IsSaturationSignal reports whether a metric name denotes a Layer-0
// resource/saturation signal (utilization, cpu, memory, queue depth, pool, ...).
func IsSaturationSignal(name string) bool {
	for _, t := range tokenize(name) {
		if saturationTokens[t] {
			return true
		}
	}
	return false
}

// isCauseBased reports whether a metric name denotes a cause rather than a
// user-visible symptom.
func isCauseBased(name string) bool {
	for _, t := range tokenize(name) {
		if causeTokens[t] {
			return true
		}
	}
	return false
}

// IndicatorCandidate is a proposed indicator to classify (the authoring-skill
// entry point). Metric is the metric name or query it measures; Layer is "",
// "service", or "journey"; Symptom is nil when unknown.
type IndicatorCandidate struct {
	Name                 string
	Metric               string
	Layer                string
	HasErrorBudgetPolicy bool
	Symptom              *bool
}

// Classify categorizes a candidate indicator and returns findings. A
// saturation/resource metric is rejected as a signal; a missing error-budget
// policy is rejected; a cause-based indicator is flagged.
func Classify(c IndicatorCandidate) (Category, Findings) {
	var fs Findings
	probe := c.Metric
	if probe == "" {
		probe = c.Name
	}
	if IsSaturationSignal(probe) {
		fs = append(fs, Finding{
			Severity: SeverityError, Object: c.Name, Kind: "SLI",
			Message: fmt.Sprintf("%q measures %q, which is a Layer-0 signal (resource/saturation), not an objective. Signals are always-on and have no target; page nobody on them alone. Turn the user-visible symptom into the SLI instead.", c.Name, probe),
		})
		return CategorySignal, fs
	}
	cat := CategoryServiceSLI
	if strings.EqualFold(c.Layer, "journey") {
		cat = CategoryJourneySLI
	}
	if !c.HasErrorBudgetPolicy {
		fs = append(fs, Finding{
			Severity: SeverityError, Object: c.Name, Kind: "SLO",
			Message: fmt.Sprintf("SLO %q has no error-budget policy. An SLO without a policy is decoration — declare what happens when the budget is spent.", c.Name),
		})
	}
	if c.Symptom != nil && !*c.Symptom || isCauseBased(probe) {
		fs = append(fs, Finding{
			Severity: SeverityWarn, Object: c.Name, Kind: "SLI",
			Message: fmt.Sprintf("%q looks cause-based, not symptom-based. Prefer an indicator users feel (availability/latency/error rate); measure causes as Layer-0 signals.", c.Name),
		})
	}
	return cat, fs
}

// Lint validates an OpenSLO Document and runs the classifier over every SLI/SLO
// using the default gRPC metric naming (its default latency bucket boundaries).
// It returns structured findings; an error-severity finding fails `slogen lint`.
func Lint(doc *Document) Findings {
	return LintWithConfig(doc, DefaultGRPCNaming())
}

// LintWithConfig is Lint with an explicit MetricNaming, so a service that
// customizes its histogram buckets can validate its latency thresholds against
// its own bucket boundaries (MetricNaming.LatencyBucketBoundaries).
func LintWithConfig(doc *Document, naming MetricNaming) Findings {
	var fs Findings

	// Every SLI (standalone or inline) is checked for a signal-as-SLI category
	// error, cause-based indicators, and a latency threshold that is not a
	// histogram bucket boundary (a silently non-firing alert).
	checkSLI := func(sli *SLI) {
		// Prefer the metric identifiers over the SLI name; report at most one
		// saturation error and one cause-based warning per SLI.
		ids := sliMetricIdentifiers(sli)
		saturation := false
		for _, id := range ids {
			if IsSaturationSignal(id) {
				fs = append(fs, Finding{
					Severity: SeverityError, Object: sli.Metadata.Name, Kind: "SLI",
					Message: fmt.Sprintf("SLI %q measures %q, which is a Layer-0 signal (resource/saturation), not an objective. Objectives are user-visible symptoms (availability/latency); measure resource pressure as a signal with no target.", sli.Metadata.Name, id),
				})
				saturation = true
				break
			}
		}
		if !saturation {
			for _, id := range ids {
				if isCauseBased(id) {
					fs = append(fs, Finding{
						Severity: SeverityWarn, Object: sli.Metadata.Name, Kind: "SLI",
						Message: fmt.Sprintf("SLI %q measures %q, which looks cause-based. Prefer a symptom users feel; measure causes as Layer-0 signals.", sli.Metadata.Name, id),
					})
					break
				}
			}
		}
		// Latency threshold must be an actual histogram bucket boundary, or the
		// emitter's le="<threshold>" matcher selects no series and the burn-rate
		// alert silently never fires. This applies ONLY to a typed otel-rpc source
		// (a raw-query journey SLI builds its own expression, so there is no le
		// matcher to validate).
		if rm := sli.Spec.RatioMetric; rm != nil && strings.TrimSpace(rm.Good.Query) == "" {
			g := rm.Good.Spec
			if g.LatencyThresholdSeconds > 0 && !naming.isBucketBoundary(g.LatencyThresholdSeconds) {
				fs = append(fs, Finding{
					Severity: SeverityError, Object: sli.Metadata.Name, Kind: "SLI",
					Message: fmt.Sprintf("latency SLI %q threshold %ss is not a histogram bucket boundary, so the le=%q matcher selects no series and the burn-rate alert silently never fires. Use the nearest boundary %ss (valid boundaries: %s).",
						sli.Metadata.Name, formatFloat(g.LatencyThresholdSeconds), formatFloat(g.LatencyThresholdSeconds), formatFloat(naming.nearestBoundary(g.LatencyThresholdSeconds)), boundaryList(naming.boundaries())),
				})
			}
		}
	}
	for i := range doc.SLIs {
		checkSLI(&doc.SLIs[i])
	}

	for i := range doc.SLOs {
		s := &doc.SLOs[i]
		if s.Spec.Indicator != nil {
			checkSLI(s.Spec.Indicator)
		}
		hasPolicy := len(s.Spec.AlertPolicies) > 0 || strings.TrimSpace(s.Metadata.Annotations["devedge.io/error-budget-policy"]) != ""
		if !hasPolicy {
			fs = append(fs, Finding{
				Severity: SeverityError, Object: s.Metadata.Name, Kind: "SLO",
				Message: fmt.Sprintf("SLO %q has no error-budget policy (no alertPolicies and no devedge.io/error-budget-policy annotation). An SLO without a policy is decoration — declare the consequence when the budget is spent.", s.Metadata.Name),
			})
		}
		if len(s.Spec.Objectives) == 0 {
			fs = append(fs, Finding{
				Severity: SeverityError, Object: s.Metadata.Name, Kind: "SLO",
				Message: fmt.Sprintf("SLO %q has no objective target.", s.Metadata.Name),
			})
		}
		if strings.EqualFold(s.Metadata.Annotations["devedge.io/uncalibrated"], "true") {
			fs = append(fs, Finding{
				Severity: SeverityWarn, Object: s.Metadata.Name, Kind: "SLO",
				Message: fmt.Sprintf("SLO %q target is an un-calibrated default. Set it from a measured baseline (de slo check / query Cortex) before it pages anyone.", s.Metadata.Name),
			})
		}
		// A *present* error-budget policy is not a *real* one: the scaffold ships a
		// "TODO:" placeholder the human must replace. Without this check the linter
		// reports "no findings" on a policy nobody wrote — blessing undone work.
		if pol := strings.TrimSpace(s.Metadata.Annotations["devedge.io/error-budget-policy"]); pol != "" && isPlaceholderPolicy(pol) {
			fs = append(fs, Finding{
				Severity: SeverityWarn, Object: s.Metadata.Name, Kind: "SLO",
				Message: fmt.Sprintf("SLO %q error-budget policy is still a placeholder (%q). Declare the real consequence when the budget is spent (freeze/escalate) — a placeholder policy is decoration.", s.Metadata.Name, truncPolicy(pol)),
			})
		}
		// The un-calibrated marker is self-attested and can simply be deleted to
		// silence the warning above. Independently flag a target left at exactly a
		// generated default: dropping the marker does not calibrate it.
		if !strings.EqualFold(s.Metadata.Annotations["devedge.io/uncalibrated"], "true") && hasDefaultTarget(s) {
			fs = append(fs, Finding{
				Severity: SeverityWarn, Object: s.Metadata.Name, Kind: "SLO",
				Message: fmt.Sprintf("SLO %q target is exactly a generated default (0.999/0.99) but is not marked un-calibrated. Dropping the marker does not calibrate it — set the target from a measured baseline (de slo check) or restore the devedge.io/uncalibrated marker.", s.Metadata.Name),
			})
		}
	}
	return fs
}

// isPlaceholderPolicy reports whether an error-budget-policy annotation is still
// unfilled boilerplate (the scaffold ships "TODO: define the consequence…").
func isPlaceholderPolicy(s string) bool {
	u := strings.ToUpper(s)
	for _, tok := range []string{"TODO", "TBD", "FIXME", "XXX", "<REPLACE", "PLACEHOLDER"} {
		if strings.Contains(u, tok) {
			return true
		}
	}
	return false
}

// hasDefaultTarget reports whether any objective is left at a bare generated
// default target (see derive.go DefaultDeriveOptions: 0.999 availability, 0.99
// latency). Compared with a small epsilon for YAML float round-tripping.
func hasDefaultTarget(s *SLO) bool {
	for _, o := range s.Spec.Objectives {
		for _, def := range []float64{0.999, 0.99} {
			if d := o.Target - def; d < 1e-9 && d > -1e-9 {
				return true
			}
		}
	}
	return false
}

// truncPolicy shortens a policy string for a finding message.
func truncPolicy(s string) string {
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

// sliMetricIdentifiers gathers the metric names/queries an SLI measures, for the
// classifier to inspect.
func sliMetricIdentifiers(sli *SLI) []string {
	var ids []string
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			ids = append(ids, s)
		}
	}
	if rm := sli.Spec.RatioMetric; rm != nil {
		add(rm.Good.Spec.Signal)
		add(rm.Good.Query)
		add(rm.Total.Spec.Signal)
		add(rm.Total.Query)
	}
	if tm := sli.Spec.ThresholdMetric; tm != nil {
		add(tm.MetricSource.Spec.Signal)
		add(tm.MetricSource.Query)
	}
	// The SLI name itself is a weak signal (e.g. "cpu-utilization").
	add(sli.Metadata.Name)
	return ids
}
