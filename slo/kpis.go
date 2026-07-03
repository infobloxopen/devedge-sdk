package slo

import (
	"fmt"
	"strings"
)

// KPI is one Layer-0 signal (a monitoring KPI) in OTel semantic-convention
// terms. Layer-0 signals are always-on and have NO target — they diagnose, they
// do not page alone. Turning one into an objective is a category error the
// classifier rejects.
type KPI struct {
	Family string // RED | Golden Signal | USE
	Name   string
	Signal string // OTel instrument / attribute
	Note   string
}

// KPIReference returns the Layer-0 API KPI catalog: the four golden signals,
// RED per request/RPC, and USE for resources, in OTel semconv terms.
func KPIReference() []KPI {
	return []KPI{
		{"RED", "Rate", "count of rpc.server.call.duration / http.server.request.duration", "request throughput; sum(rate(..._count[5m]))"},
		{"RED", "Errors", "the server-fault fraction of the same histogram", "grouped by rpc.response.status_code / http.response.status_code; exclude client faults"},
		{"RED", "Duration", "rpc.server.call.duration / http.server.request.duration (histogram)", "p50/p95/p99 via histogram_quantile over _bucket"},
		{"Golden Signal", "Latency", "duration histogram (as RED Duration)", "distinguish latency of successful vs failed requests"},
		{"Golden Signal", "Traffic", "request rate (as RED Rate)", "how much demand the service is under"},
		{"Golden Signal", "Errors", "server-fault rate (as RED Errors)", "the rate of requests that fail"},
		{"Golden Signal", "Saturation", "resource utilization gauges (USE)", "how full the service is; the leading indicator of trouble"},
		{"USE", "Utilization", "cpu / memory / pool / queue-depth gauges", "resource busy fraction — a SIGNAL, never an SLI"},
		{"USE", "Saturation", "queue depth / backlog / waiters", "work that cannot be serviced yet — a SIGNAL, never an SLI"},
		{"USE", "Errors", "resource error counters (e.g. connection failures)", "resource-level faults feeding request Errors"},
	}
}

// KPIReferenceText renders the KPI catalog as a human-readable table for
// `slogen kpis`.
func KPIReferenceText() string {
	var b strings.Builder
	b.WriteString("Layer-0 API KPIs (signals) — OTel semantic conventions.\n")
	b.WriteString("Always-on, no target, page nobody on them ALONE. They diagnose; SLOs (Layer 1)\n")
	b.WriteString("turn the user-visible symptoms into objectives.\n\n")
	family := ""
	for _, k := range KPIReference() {
		if k.Family != family {
			family = k.Family
			fmt.Fprintf(&b, "== %s ==\n", family)
		}
		fmt.Fprintf(&b, "  %-12s %s\n", k.Name, k.Signal)
		if k.Note != "" {
			fmt.Fprintf(&b, "  %-12s   ↳ %s\n", "", k.Note)
		}
	}
	b.WriteString("\nThe SDK emits RED per request automatically (server.go otelgrpc/otelhttp).\n")
	b.WriteString("Through Alloy→Cortex these normalize to rpc_server_call_duration_seconds_* /\n")
	b.WriteString("http_server_request_duration_seconds_* (dots→underscores, unit suffix, classic\n")
	b.WriteString("histogram _bucket/_count/_sum). See `slogen generate` to derive SLOs from them.\n")
	return b.String()
}
