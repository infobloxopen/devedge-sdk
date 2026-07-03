package slo

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// lokiRuleFile is a Loki ruler file (Prometheus-compatible group format; the
// exprs are LogQL). Deliberately thinner than the Prometheus emitter — log-
// derived SLIs are for correctness/coverage cases cleaner from logs than metrics.
type lokiRuleFile struct {
	Groups []promGroup `yaml:"groups"`
}

const lokiHeader = `# Loki ruler recording rules for log-derived SLIs (WS-025, minimal starter).
#
# These compute an availability error ratio from LOG lines rather than metrics —
# useful when correctness/coverage is only observable in logs. This is a
# STARTING TEMPLATE: adapt the stream selector ({service_name=...}) and the
# parser/field extraction (| logfmt | status_code=...) to your service's log
# schema. The metric-based Prometheus rules remain the primary SLI source.
`

// emitLoki projects availability SLOs to a minimal Loki ruler recording-rule
// group. Latency-from-logs is skipped (measure latency from the histogram).
func emitLoki(doc *Document, _ MetricNaming) ([]Rendered, error) {
	group := promGroup{Name: "devedge-slo-log-sli"}
	for i := range doc.SLOs {
		s := &doc.SLOs[i]
		if s.Metadata.Labels["devedge.io/sli-type"] != SLITypeAvailability {
			continue
		}
		sli := doc.sliByName(s.Spec.IndicatorRef)
		if sli == nil {
			sli = s.Spec.Indicator
		}
		if sli == nil || sli.Spec.RatioMetric == nil {
			continue
		}
		rm := sli.Spec.RatioMetric

		// Raw-query source (Layer-2 journey path): use the author's LogQL good/total
		// directly, substituting the window token. Takes precedence over the typed
		// server-fault derivation.
		if strings.TrimSpace(rm.Good.Query) != "" && strings.TrimSpace(rm.Total.Query) != "" {
			good := strings.ReplaceAll(rm.Good.Query, windowToken, "5m")
			total := strings.ReplaceAll(rm.Total.Query, windowToken, "5m")
			group.Rules = append(group.Rules, promRuleEntry{
				Record: "slo:log_sli_error:ratio_rate5m",
				Expr:   fmt.Sprintf("1 - ((%s) / (%s))", good, total),
				Labels: map[string]string{"slo": s.Metadata.Name, "service": s.Spec.Service},
			})
			continue
		}

		serverFault := setDiff(rm.Good.Spec.ExcludeStatuses, rm.Total.Spec.ExcludeStatuses)
		if len(serverFault) == 0 {
			continue
		}
		stream := `{service_name="` + s.Spec.Service + `"}`
		bad := fmt.Sprintf(`sum(rate(%s | logfmt | status_code=~"%s" [5m]))`, stream, strings.Join(serverFault, "|"))
		total := fmt.Sprintf(`sum(rate(%s | logfmt [5m]))`, stream)
		group.Rules = append(group.Rules, promRuleEntry{
			Record: "slo:log_sli_error:ratio_rate5m",
			Expr:   bad + " / " + total,
			Labels: map[string]string{"slo": s.Metadata.Name, "service": s.Spec.Service},
		})
	}
	if len(group.Rules) == 0 {
		return nil, fmt.Errorf("slo: emit loki: no availability SLOs to derive log SLIs from")
	}
	b, err := yaml.Marshal(&lokiRuleFile{Groups: []promGroup{group}})
	if err != nil {
		return nil, err
	}
	return []Rendered{{Filename: docName(doc) + "-slo.loki-rules.yaml", Content: append([]byte(lokiHeader), b...)}}, nil
}

// setDiff returns elements of a not in b, preserving a's order.
func setDiff(a, b []string) []string {
	inB := map[string]bool{}
	for _, x := range b {
		inB[x] = true
	}
	var out []string
	for _, x := range a {
		if !inB[x] {
			out = append(out, x)
		}
	}
	return out
}
