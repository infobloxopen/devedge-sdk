package slo

import (
	"math"
	"strings"
)

// DefaultLatencyBucketBoundaries are the OpenTelemetry duration advisory
// histogram boundaries (seconds) that otelgrpc/otelhttp use by default. A
// latency SLI threshold MUST equal one of these: the emitter renders it as a
// le="<threshold>" matcher on the _bucket series, and if the value is not an
// actual bucket boundary the matcher selects NO series — the error-ratio
// recording rule yields an empty vector and the latency burn-rate alert silently
// never fires. A service that customizes its histogram buckets overrides the set
// via MetricNaming.LatencyBucketBoundaries.
var DefaultLatencyBucketBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// MetricNaming maps an OTel instrument to the Prometheus metric name and label
// names it becomes AFTER the OTel→Prometheus normalization that Grafana Alloy
// applies before remote_write to Cortex (dots→underscores, unit suffix,
// classic-histogram _bucket/_count/_sum, and the semconv label names).
//
// The defaults track what THIS SDK actually emits. server.go installs
// otelgrpc.NewServerHandler() / otelhttp.NewHandler() with no options and does
// not set OTEL_SEMCONV_STABILITY_OPT_IN, so on otelgrpc/otelhttp v0.69.0 (OTel
// v1.44.0) the DEFAULT instrument is the NEW semantic convention (semconv
// v1.41.0):
//
//   - gRPC server: rpc.server.call.duration, unit seconds, status label
//     rpc.response.status_code with UPPER_SNAKE canonical string values
//     (OK, UNAVAILABLE, ...), method label rpc.method, service label rpc.service.
//   - HTTP gateway: http.server.request.duration, unit seconds, status label
//     http.response.status_code (numeric), method label http.request.method.
//
// A semconv version bump is therefore a MetricNaming change, not a code edit.
// DefaultGRPCNaming is the SDK default; LegacyGRPCNaming covers the
// OTEL_SEMCONV_STABILITY_OPT_IN=grpc/old|dup opt-in (rpc.server.duration in ms,
// numeric rpc.grpc.status_code).
type MetricNaming struct {
	// UnitSuffix is the Prometheus unit suffix, e.g. "seconds" (new) or
	// "milliseconds" (legacy). Empty means no unit suffix.
	UnitSuffix string
	// StatusLabel is the normalized status label, e.g. rpc_response_status_code.
	StatusLabel string
	// MethodLabel is the normalized method label, e.g. rpc_method.
	MethodLabel string
	// ServiceLabel is the normalized service label, e.g. rpc_service. Empty
	// disables service filtering (e.g. the HTTP gateway variant filters by route).
	ServiceLabel string
	// LatencyBucketBoundaries are the histogram bucket boundaries (seconds) a
	// latency SLI threshold must equal. Empty means DefaultLatencyBucketBoundaries.
	// Override it when a service customizes its histogram buckets.
	LatencyBucketBoundaries []float64
}

// DefaultGRPCNaming is the naming this SDK emits by default (new semconv).
func DefaultGRPCNaming() MetricNaming {
	return MetricNaming{
		UnitSuffix:              "seconds",
		StatusLabel:             "rpc_response_status_code",
		MethodLabel:             "rpc_method",
		ServiceLabel:            "rpc_service",
		LatencyBucketBoundaries: DefaultLatencyBucketBoundaries,
	}
}

// HTTPGatewayNaming measures reliability at the REST gateway (http.server
// .request.duration). It is an alternative "closer to the edge" measurement,
// documented but not the default. Status values are numeric HTTP codes, so the
// excluded/bad sets are regex fragments (4.., 5..) rather than gRPC names.
func HTTPGatewayNaming() MetricNaming {
	return MetricNaming{
		UnitSuffix:              "seconds",
		StatusLabel:             "http_response_status_code",
		MethodLabel:             "http_request_method",
		ServiceLabel:            "",
		LatencyBucketBoundaries: DefaultLatencyBucketBoundaries,
	}
}

// LegacyGRPCNaming covers a service that opts into the OLD RPC semconv via
// OTEL_SEMCONV_STABILITY_OPT_IN (rpc.server.duration in milliseconds, numeric
// rpc.grpc.status_code). Pair it with the numeric status sets below.
func LegacyGRPCNaming() MetricNaming {
	return MetricNaming{
		UnitSuffix:              "milliseconds",
		StatusLabel:             "rpc_grpc_status_code",
		MethodLabel:             "rpc_method",
		ServiceLabel:            "rpc_service",
		LatencyBucketBoundaries: DefaultLatencyBucketBoundaries,
	}
}

// Status-code sets for the availability good/valid split (WS-025 D4).
//
// New-semconv gRPC values are UPPER_SNAKE canonical strings (otelgrpc's
// canonicalString on grpc/codes). The numeric variants are for the legacy
// opt-in. HTTP variants are anchored regex fragments.
var (
	// GRPCServerFaultStatuses count against availability (bad):
	// UNKNOWN(2), DEADLINE_EXCEEDED(4), INTERNAL(13), UNAVAILABLE(14), DATA_LOSS(15).
	GRPCServerFaultStatuses = []string{"UNKNOWN", "DEADLINE_EXCEEDED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS"}
	// GRPCClientFaultStatuses are excluded from the valid denominator:
	// INVALID_ARGUMENT(3), NOT_FOUND(5), ALREADY_EXISTS(6), PERMISSION_DENIED(7), UNAUTHENTICATED(16).
	GRPCClientFaultStatuses = []string{"INVALID_ARGUMENT", "NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "UNAUTHENTICATED"}

	// Numeric variants for the legacy opt-in (rpc_grpc_status_code).
	GRPCServerFaultCodes = []string{"2", "4", "13", "14", "15"}
	GRPCClientFaultCodes = []string{"3", "5", "6", "7", "16"}

	// HTTPClientFaultStatuses (4xx) are excluded from valid; HTTPServerFaultStatuses
	// (5xx) are bad. Anchored regex fragments for http_response_status_code.
	HTTPClientFaultStatuses = []string{"4.."}
	HTTPServerFaultStatuses = []string{"5.."}
)

// series returns the normalized Prometheus series name for the OTel signal and a
// classic-histogram suffix ("_bucket", "_count", "_sum", or "" for the base).
func (n MetricNaming) series(signal, suffix string) string {
	base := strings.ReplaceAll(signal, ".", "_")
	if n.UnitSuffix != "" {
		base += "_" + n.UnitSuffix
	}
	return base + suffix
}

// selector builds a PromQL label selector { ... } from the parts. Each part is a
// raw matcher like `rpc_method=~"a|b"`. Empty parts are dropped.
func selector(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return "{" + strings.Join(kept, ", ") + "}"
}

// eqMatcher builds `label="value"`, or "" if either is empty.
func eqMatcher(label, value string) string {
	if label == "" || value == "" {
		return ""
	}
	return label + `="` + value + `"`
}

// reMatcher builds `label=~"a|b"`, or "" if empty.
func reMatcher(label string, values []string) string {
	if label == "" || len(values) == 0 {
		return ""
	}
	return label + `=~"` + strings.Join(values, "|") + `"`
}

// notReMatcher builds `label!~"a|b"`, or "" if empty.
func notReMatcher(label string, values []string) string {
	if label == "" || len(values) == 0 {
		return ""
	}
	return label + `!~"` + strings.Join(values, "|") + `"`
}

// boundaries returns the effective latency bucket boundaries (the default set
// when unset).
func (n MetricNaming) boundaries() []float64 {
	if len(n.LatencyBucketBoundaries) > 0 {
		return n.LatencyBucketBoundaries
	}
	return DefaultLatencyBucketBoundaries
}

// isBucketBoundary reports whether v equals one of the configured latency bucket
// boundaries (within a small tolerance for float representation).
func (n MetricNaming) isBucketBoundary(v float64) bool {
	for _, b := range n.boundaries() {
		if math.Abs(v-b) <= 1e-9 {
			return true
		}
	}
	return false
}

// nearestBoundary returns the configured bucket boundary closest to v.
func (n MetricNaming) nearestBoundary(v float64) float64 {
	bs := n.boundaries()
	best := bs[0]
	for _, b := range bs[1:] {
		if math.Abs(v-b) < math.Abs(v-best) {
			best = b
		}
	}
	return best
}

// boundaryList renders the boundaries as a comma-separated list for messages.
func boundaryList(bs []float64) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = formatFloat(b)
	}
	return strings.Join(parts, ", ")
}
