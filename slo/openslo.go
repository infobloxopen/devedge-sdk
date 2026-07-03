// Package slo is the reliability seam for devedge services (WS-025): an OpenSLO
// v1 intermediate representation, contract-derived default SLOs, a fail-loud
// three-layer classifier, and pure-text emitters that project the IR to
// Prometheus/Cortex rules, Grafana dashboards, and Loki LogQL rules.
//
// The package is deliberately dependency-light: it imports only the standard
// library and gopkg.in/yaml.v3 (already in the root module). It pulls in no
// Prometheus/Grafana/Datadog client library — emitters produce YAML/JSON as
// text — so a service that imports it stays isolated (check-graph-isolation
// stays green). The heavier CLI that orchestrates it lives in cmd/slogen.
//
// The three declaration layers WS-025 enforces:
//
//   - Layer 0 — signals / API KPIs (RED, golden signals, USE). Always-on, no
//     target. See KPIReference. Declaring one as an SLI is a category error the
//     classifier rejects.
//   - Layer 1 — service SLIs/SLOs, derived from the AIP contract (Derive*).
//   - Layer 2 — business/journey SLOs (classified here; resolved across services
//     via the apx catalog in a later phase).
package slo

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// APIVersion is the OpenSLO version this package emits and reads.
const APIVersion = "openslo/v1"

// OpenSLO object kinds.
const (
	KindSLI            = "SLI"
	KindSLO            = "SLO"
	KindService        = "Service"
	KindAlertPolicy    = "AlertPolicy"
	KindAlertCondition = "AlertCondition"
)

// MetricSourceTypeOTel is the devedge-neutral metric-source type. It names the
// SDK's OTel RED instrument as the source of an indicator; emitters translate it
// to a backend query. It is not backend-specific, so the OpenSLO IR never
// hardcodes PromQL/LogQL — that is the emitter's job.
const MetricSourceTypeOTel = "devedge/otel-rpc"

// SLI-type discriminators carried in an OTelRatioSource.
const (
	SLITypeAvailability = "availability"
	SLITypeLatency      = "latency"
)

// Metadata is the common OpenSLO object metadata.
type Metadata struct {
	Name        string            `yaml:"name"`
	DisplayName string            `yaml:"displayName,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// Service is an OpenSLO Service — the thing a set of SLOs is measured against.
type Service struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   Metadata    `yaml:"metadata"`
	Spec       ServiceSpec `yaml:"spec"`
}

// ServiceSpec is the Service body.
type ServiceSpec struct {
	Description string `yaml:"description,omitempty"`
}

// SLI is an OpenSLO service-level indicator: how "good" is measured.
type SLI struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       SLISpec  `yaml:"spec"`
}

// SLISpec is the indicator body. A ratioMetric expresses good/valid; a
// thresholdMetric expresses a single series compared to a threshold (supported
// for reading hand-authored docs so the classifier can judge them).
type SLISpec struct {
	Description     string           `yaml:"description,omitempty"`
	RatioMetric     *RatioMetric     `yaml:"ratioMetric,omitempty"`
	ThresholdMetric *ThresholdMetric `yaml:"thresholdMetric,omitempty"`
}

// RatioMetric is good events over valid (total) events.
type RatioMetric struct {
	Counter bool         `yaml:"counter"`
	Good    MetricSource `yaml:"good"`
	Total   MetricSource `yaml:"total"`
}

// ThresholdMetric is a single metric compared to a threshold. Only read here (a
// devedge-generated SLI is always a ratioMetric); it lets the classifier inspect
// hand-authored SLIs (e.g. someone pointing an SLI at a saturation gauge).
type ThresholdMetric struct {
	MetricSource MetricSource `yaml:"metricSource"`
}

// MetricSource names the source of a metric. For devedge-generated indicators,
// Type is MetricSourceTypeOTel and Spec is an OTelRatioSource; Query captures a
// raw backend query when a hand-authored source uses one.
type MetricSource struct {
	Type  string          `yaml:"type"`
	Spec  OTelRatioSource `yaml:"spec"`
	Query string          `yaml:"query,omitempty"`
}

// OTelRatioSource captures the tech-agnostic derivation intent for one side
// (good or total) of a ratio metric, read from the SDK's OTel RED instrument.
// Emitters translate it to a backend query using a MetricNaming.
type OTelRatioSource struct {
	// SLIType is availability or latency.
	SLIType string `yaml:"sliType"`
	// Signal is the OTel metric base name (pre-Prometheus-normalization), e.g.
	// rpc.server.call.duration.
	Signal string `yaml:"signal"`
	// Transport is grpc or http (selects the label conventions).
	Transport string `yaml:"transport"`
	// Service is the value of the service label (e.g. the proto FQN
	// toy.v1.WidgetService), matched by the query. Optional.
	Service string `yaml:"service,omitempty"`
	// Methods is the set of method-label values in this SLO's group.
	Methods []string `yaml:"methods,omitempty"`
	// MethodClass is read or write (documentation only).
	MethodClass string `yaml:"methodClass,omitempty"`
	// ExcludeStatuses are status-label values removed from this count. For an
	// availability "total" it is the client-fault set (valid = all − client
	// fault); for "good" it is client-fault ∪ server-fault (good = valid − bad).
	ExcludeStatuses []string `yaml:"excludeStatuses,omitempty"`
	// LatencyThresholdSeconds, when > 0 on the "good" source of a latency SLI,
	// counts only requests whose duration is at or under this bucket boundary.
	LatencyThresholdSeconds float64 `yaml:"latencyThresholdSeconds,omitempty"`
}

// SLO is an OpenSLO service-level objective: an SLI plus a target over a window,
// a budgeting method, and the alert (error-budget) policy.
type SLO struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       SLOSpec  `yaml:"spec"`
}

// SLOSpec is the objective body.
type SLOSpec struct {
	Description     string       `yaml:"description,omitempty"`
	Service         string       `yaml:"service"`
	IndicatorRef    string       `yaml:"indicatorRef,omitempty"`
	Indicator       *SLI         `yaml:"indicator,omitempty"`
	BudgetingMethod string       `yaml:"budgetingMethod"`
	TimeWindow      []TimeWindow `yaml:"timeWindow"`
	Objectives      []Objective  `yaml:"objectives"`
	AlertPolicies   []string     `yaml:"alertPolicies,omitempty"`
}

// TimeWindow is the SLO measurement window (28d rolling default).
type TimeWindow struct {
	Duration  string `yaml:"duration"`
	IsRolling bool   `yaml:"isRolling"`
}

// Objective is a single target ratio for the SLO.
type Objective struct {
	DisplayName string  `yaml:"displayName,omitempty"`
	Target      float64 `yaml:"target"`
}

// AlertPolicy is the error-budget consequence: which conditions fire and how.
type AlertPolicy struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   Metadata        `yaml:"metadata"`
	Spec       AlertPolicySpec `yaml:"spec"`
}

// AlertPolicySpec is the alert-policy body.
type AlertPolicySpec struct {
	Description        string                 `yaml:"description,omitempty"`
	AlertWhenBreaching bool                   `yaml:"alertWhenBreaching,omitempty"`
	AlertWhenNoData    bool                   `yaml:"alertWhenNoData,omitempty"`
	Conditions         []AlertPolicyCondition `yaml:"conditions,omitempty"`
}

// AlertPolicyCondition references an AlertCondition by name.
type AlertPolicyCondition struct {
	ConditionRef string `yaml:"conditionRef"`
}

// AlertCondition is a single burn-rate condition (SRE Workbook MWMBR tier).
type AlertCondition struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   Metadata           `yaml:"metadata"`
	Spec       AlertConditionSpec `yaml:"spec"`
}

// AlertConditionSpec is the condition body.
type AlertConditionSpec struct {
	Description string               `yaml:"description,omitempty"`
	Severity    string               `yaml:"severity"`
	Condition   AlertConditionDetail `yaml:"condition"`
}

// AlertConditionDetail is a burn-rate condition. Threshold is the burn-rate
// multiplier (14.4/6/1); the emitter multiplies it by the error budget.
type AlertConditionDetail struct {
	Kind           string  `yaml:"kind"` // "burnrate"
	Op             string  `yaml:"op"`   // "gt"
	Threshold      float64 `yaml:"threshold"`
	LookbackWindow string  `yaml:"lookbackWindow"`
	ShortWindow    string  `yaml:"shortWindow,omitempty"`
	AlertAfter     string  `yaml:"alertAfter,omitempty"`
}

// Document is an ordered set of OpenSLO objects that marshals to (and parses
// from) a multi-document YAML stream.
type Document struct {
	Services        []Service
	SLIs            []SLI
	SLOs            []SLO
	AlertPolicies   []AlertPolicy
	AlertConditions []AlertCondition
}

// Marshal renders the Document as a deterministic multi-document YAML stream,
// in a fixed kind order (Service, SLI, SLO, AlertPolicy, AlertCondition).
func (d *Document) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	emit := func(v any) error {
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		if buf.Len() > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(b)
		return nil
	}
	for i := range d.Services {
		if err := emit(&d.Services[i]); err != nil {
			return nil, err
		}
	}
	for i := range d.SLIs {
		if err := emit(&d.SLIs[i]); err != nil {
			return nil, err
		}
	}
	for i := range d.SLOs {
		if err := emit(&d.SLOs[i]); err != nil {
			return nil, err
		}
	}
	for i := range d.AlertPolicies {
		if err := emit(&d.AlertPolicies[i]); err != nil {
			return nil, err
		}
	}
	for i := range d.AlertConditions {
		if err := emit(&d.AlertConditions[i]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Parse reads a multi-document OpenSLO YAML stream into a Document, dispatching
// each document by its kind. Unknown kinds are an error (fail-loud).
func Parse(data []byte) (*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	doc := &Document{}
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("slo: parse: %w", err)
		}
		// An empty document (e.g. a trailing `---`) decodes to a zero node.
		if node.Kind == 0 {
			continue
		}
		var tm struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := node.Decode(&tm); err != nil {
			return nil, fmt.Errorf("slo: parse: read kind: %w", err)
		}
		switch tm.Kind {
		case KindService:
			var v Service
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("slo: parse Service: %w", err)
			}
			doc.Services = append(doc.Services, v)
		case KindSLI:
			var v SLI
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("slo: parse SLI: %w", err)
			}
			doc.SLIs = append(doc.SLIs, v)
		case KindSLO:
			var v SLO
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("slo: parse SLO: %w", err)
			}
			doc.SLOs = append(doc.SLOs, v)
		case KindAlertPolicy:
			var v AlertPolicy
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("slo: parse AlertPolicy: %w", err)
			}
			doc.AlertPolicies = append(doc.AlertPolicies, v)
		case KindAlertCondition:
			var v AlertCondition
			if err := node.Decode(&v); err != nil {
				return nil, fmt.Errorf("slo: parse AlertCondition: %w", err)
			}
			doc.AlertConditions = append(doc.AlertConditions, v)
		case "":
			return nil, fmt.Errorf("slo: parse: document missing kind")
		default:
			return nil, fmt.Errorf("slo: parse: unknown kind %q", tm.Kind)
		}
	}
	return doc, nil
}

// sliByName returns the SLI with the given name, or nil.
func (d *Document) sliByName(name string) *SLI {
	for i := range d.SLIs {
		if d.SLIs[i].Metadata.Name == name {
			return &d.SLIs[i]
		}
	}
	return nil
}
