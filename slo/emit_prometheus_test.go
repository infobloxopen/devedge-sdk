package slo

import (
	"strings"
	"testing"
)

func renderToyPrometheus(t *testing.T, naming MetricNaming) string {
	t.Helper()
	doc := deriveToyDoc(t)
	rs, err := Render(TargetPrometheus, doc, RenderOptions{Naming: naming})
	if err != nil {
		t.Fatalf("render prometheus: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 rendered file, got %d", len(rs))
	}
	return string(rs[0].Content)
}

func TestEmitPrometheus_Golden(t *testing.T) {
	got := renderToyPrometheus(t, DefaultGRPCNaming())
	goldenCompare(t, "toy.prometheusrule.golden.yaml", []byte(got))
}

func TestEmitPrometheus_GroundTruthMetricNames(t *testing.T) {
	out := renderToyPrometheus(t, DefaultGRPCNaming())

	// AC-2: the availability numerator/denominator reference the NEW-semconv
	// series + status label this SDK actually emits (WS-025 D5).
	mustContain(t, out, "rpc_server_call_duration_seconds_count")
	mustContain(t, out, "rpc_response_status_code!~")
	mustContain(t, out, "rpc_method=~")
	mustContain(t, out, `rpc_service="toy.v1.WidgetService"`)
	// server-fault set (bad) and client-fault set (excluded from valid) present.
	for _, s := range GRPCServerFaultStatuses {
		mustContain(t, out, s)
	}
	for _, s := range GRPCClientFaultStatuses {
		mustContain(t, out, s)
	}
	// Latency uses the histogram bucket at a boundary threshold.
	mustContain(t, out, "rpc_server_call_duration_seconds_bucket")
	mustContain(t, out, `le="0.25"`)

	// AC-3: the three MWMBR burn-rate thresholds.
	mustContain(t, out, "(14.4 * (1 - 0.999))")
	mustContain(t, out, "(6 * (1 - 0.999))")
	mustContain(t, out, "(1 * (1 - 0.999))")
	// fast/medium page, slow ticket.
	mustContain(t, out, "severity: page")
	mustContain(t, out, "severity: ticket")
	// recording-rule windows.
	for _, w := range []string{"ratio_rate5m", "ratio_rate1h", "ratio_rate6h", "ratio_rate3d", "ratio_rate30m"} {
		mustContain(t, out, w)
	}
	mustContain(t, out, "monitoring.coreos.com/v1")
	mustContain(t, out, "kind: PrometheusRule")

	// AC-2 negative: a client-fault code must not be counted as bad — it must
	// appear only inside a `!~` (excluded) matcher, never a `=~` (matched) one.
	if strings.Contains(out, `rpc_response_status_code=~"`) {
		// If a positive status matcher exists, it must not contain client faults.
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `rpc_response_status_code=~"`) && strings.Contains(line, "NOT_FOUND") {
				t.Errorf("client-fault NOT_FOUND appears in a positive status matcher (would count as bad): %s", line)
			}
		}
	}
}

func TestEmitPrometheus_LegacyNaming(t *testing.T) {
	// A service that opts into the OLD RPC semconv (OTEL_SEMCONV_STABILITY_OPT_IN)
	// emits rpc.server.duration (ms) with the numeric rpc.grpc.status_code label.
	// Modeling that is a CONFIG change, not a code edit: derive with the legacy
	// signal + numeric status sets, render with LegacyGRPCNaming.
	opts := DefaultDeriveOptions()
	opts.SignalOverride = "rpc.server.duration"
	opts.ServerFaultStatuses = GRPCServerFaultCodes
	opts.ClientFaultStatuses = GRPCClientFaultCodes
	doc, err := DefaultsForResource(ResourceDefaults{
		ServiceShort: "orders", ServiceLabel: "orders.v1.OrderService", Resource: "Order", ResourcePlural: "Orders",
	}, opts)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	rs, err := Render(TargetPrometheus, doc, RenderOptions{Naming: LegacyGRPCNaming()})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(rs[0].Content)
	mustContain(t, out, "rpc_server_duration_milliseconds_count")
	mustContain(t, out, `rpc_grpc_status_code!~"3|5|6|7|16"`)
	mustContain(t, out, `rpc_grpc_status_code!~"3|5|6|7|16|2|4|13|14|15"`)
}

func mustContain(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("output missing %q", needle)
	}
}
