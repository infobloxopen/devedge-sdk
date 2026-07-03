---
title: API KPIs
weight: 7
---

This page is the reference for the Layer-0 API key performance indicators (KPIs) a devedge-sdk service
emits — the RED metrics, the four golden signals, and the USE resource metrics, in OpenTelemetry
semantic-convention terms. Use it when you need the exact signal name behind a metric, or to choose
the symptom an SLO should measure. These signals are always-on and have no target; the objective layer
is covered in [Define SLOs](../../how-to/operate/slo/). Print this catalog at any time with `de slo
kpis` (or `slogen kpis`).

Layer-0 signals are diagnostic. They page nobody on their own. A service-level objective (Layer 1)
turns a user-visible symptom drawn from these signals into a target; see
[Reliability](../../concepts/reliability/) for the layer model.

## RED — per request

Every request the SDK serves produces RED metrics from the OpenTelemetry instrumentation on the gRPC
server and the REST gateway.

| KPI | Source | Notes |
|-----|--------|-------|
| Rate | count of `rpc.server.call.duration` / `http.server.request.duration` | request throughput; `sum(rate(..._count[5m]))` |
| Errors | the server-fault fraction of the same histogram | grouped by `rpc.response.status_code` / `http.response.status_code`; exclude client faults |
| Duration | `rpc.server.call.duration` / `http.server.request.duration` (histogram) | p50/p95/p99 via `histogram_quantile` over `_bucket` |

## Golden signals

The four golden signals map onto the same telemetry, with saturation added from resource metrics.

| KPI | Source | Notes |
|-----|--------|-------|
| Latency | duration histogram | distinguish the latency of successful and failed requests |
| Traffic | request rate | how much demand the service is under |
| Errors | server-fault rate | the rate of requests that fail |
| Saturation | resource utilization gauges | how full the service is; the leading indicator of trouble |

## USE — per resource

For a constrained resource — a connection pool, a work queue, CPU, memory — the USE method tracks
utilization, saturation, and errors.

| KPI | Source | Notes |
|-----|--------|-------|
| Utilization | CPU / memory / pool / queue-depth gauges | the busy fraction of a resource — a signal, never an SLI |
| Saturation | queue depth / backlog / waiters | work that cannot be serviced yet — a signal, never an SLI |
| Errors | resource error counters | resource-level faults that feed request errors |

A utilization or saturation metric is a signal, not an objective. If you care about a resource filling
up, alert on the user-visible symptom it causes (latency or availability); the classifier rejects a
saturation metric declared as an SLI.

## From OpenTelemetry to Prometheus

The SDK emits these over OTLP with no scrape endpoint. Grafana Alloy receives the OTLP stream and
`remote_write`s to Cortex, normalizing the names on the way: dots become underscores, a unit suffix is
appended, and a classic histogram becomes `_bucket` / `_count` / `_sum` series. So
`rpc.server.call.duration` arrives as `rpc_server_call_duration_seconds_bucket` (and `_count`,
`_sum`), labeled `rpc_response_status_code`, `rpc_method`, and `rpc_service`. The
[SLO generator](../../how-to/operate/slo/) references these normalized names, so you do not write query
strings by hand.

## The `devedge/otel-rpc` SLI source

Each derived Layer-1 indicator is a good/valid ratio whose `good` and `total` sides carry a typed
metric source, `type: devedge/otel-rpc`, with the fields below under `spec`. The `de slo render`
emitters translate this source into a backend query, so you set these fields rather than write PromQL.
Use this reference when you hand-author an indicator for a method the generator does not cover (see
[Add a non-default indicator](../../how-to/operate/slo/#add-a-non-default-indicator)); the fastest
start is to copy a generated block from `slo.yaml` and edit the `methods` list.

| Field | Type | Values | Meaning |
|---|---|---|---|
| `sliType` | string | `availability`, `latency` | Which indicator this side measures. |
| `signal` | string | e.g. `rpc.server.call.duration` | OTel metric base name, before Prometheus normalization. |
| `transport` | string | `grpc`, `http` | Selects the status- and duration-label conventions. |
| `service` | string | proto FQN, e.g. `linkd.v1.LinkService` | The `rpc.service` label value the query filters on. Optional; omit to filter by method only. |
| `methods` | list of string | method names, e.g. `[Resolve]` | The `rpc.method` label values this indicator groups. |
| `methodClass` | string | `read`, `write` | Documentation only; does not affect the query. |
| `excludeStatuses` | list of string | status-label values | Outcomes removed from this count. For an availability `total` this is the client-fault set (so `valid = all − client-fault`); for its `good` it is client-fault plus server-fault (so `good = valid − bad`). |
| `latencyThresholdSeconds` | float | a histogram bucket boundary | On the `good` side of a latency SLI, counts only requests at or under this duration. Must equal a real bucket boundary — see the calibration callout in [Define SLOs](../../how-to/operate/slo/#calibrate-the-target). |

A journey (Layer-2) SLI uses a raw `query` instead of this typed source; see
[Author a business or journey SLO](../../how-to/operate/slo/#author-a-business-or-journey-slo).
