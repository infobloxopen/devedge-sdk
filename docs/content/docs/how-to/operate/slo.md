---
title: Define SLOs
weight: 6
---

A service-level objective (SLO) states a reliability target for your service — "99.9% of read requests
succeed over 28 days" — as data the SDK turns into monitoring rules and dashboards. Defining one gives
you an error budget and burn-rate alerts without hand-writing Prometheus rules, and a single place to
state a target distinct from the raw telemetry. Use this page when you add or change a service's
reliability targets, or review its `slo.yaml`. For the model behind it — the three layers, good/valid
ratios, error budgets — see [Reliability](../../../concepts/reliability/).

## What you get by default

A scaffolded service ships a starter `slo.yaml` derived from its API contract: four grouped
objectives — read availability, read latency, write availability, write latency. Each is a correct
good/valid ratio with the client-fault codes excluded from the denominator, a 28-day rolling window, a
mandatory error-budget policy stub, and a target marked un-calibrated. The service has a good default
objective before its first deploy; you calibrate and extend it.

The objectives are written in [OpenSLO](https://openslo.com/), a vendor-neutral format. The `slogen`
tool derives, lints, and projects them; `de slo` orchestrates `slogen` with the generator pinned to
your SDK version.

## Generate objectives from the contract

The scaffold already wrote `slo.yaml`. Regenerate it after you change the service's resource methods:

```bash
de slo generate
```

This reads the enriched OpenAPI your build produces and rewrites `slo.yaml`. Under the hood it runs:

```bash
slogen generate --openapi openapi/<service>.openapi.yaml \
  --service <proto-fqn> --out slo.yaml
```

The `--service` value is the proto fully-qualified service name (for example
`orders.v1.OrderService`), which is the `rpc.service` label the generated queries filter on.

{{< callout type="info" >}}
`de slo generate` groups only the standard AIP methods — `Get`/`List`/`BatchGet` as read,
`Create`/`Update`/`Delete`/`Undelete` as write. It does **not** derive an objective for a custom
(AIP-136) method such as `Resolve`, and it lists the ones it skipped. A custom method is often the
service's hot path, so author its indicator by hand — see [Add a non-default
indicator](#add-a-non-default-indicator).
{{< /callout >}}

## Calibrate the target

The generated target is a placeholder. Set it from a measured baseline:

1. Measure the service's achieved ratio over the window (`de slo check`, or query the error ratio in
   Cortex).
2. Set the objective just below the sustained baseline. Never set a target above what the service
   sustains.
3. Remove the `devedge.io/uncalibrated` annotation once the target is measured.
4. Replace the `devedge.io/error-budget-policy` TODO with the real consequence — for example,
   freezing feature releases until the budget recovers over a full window.

{{< callout type="warning" >}}
A calibrated **latency threshold must equal a histogram bucket boundary**. The rendered rule compares
the duration histogram at `le="<threshold>"`; a value that is not a real bucket boundary matches no
series, so the error ratio is empty and the latency burn-rate alert **silently never fires**. The
default boundaries (seconds) are `0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5,
7.5, 10`. Setting a measured target such as `0.18` is a common calibration mistake — round it to the
nearest boundary (`0.25`). `de slo lint` enforces this and reports the nearest valid boundary. If your
service customizes its histogram buckets, override the boundary set in the metric naming config.
{{< /callout >}}

## Lint

Validate the objectives and run the classifier before you commit:

```bash
de slo lint slo.yaml       # or: slogen lint slo.yaml [--format json]
```

The classifier fails the command on an error and reports a warning otherwise:

- A saturation or resource metric declared as an SLI (CPU, memory, queue depth, pool utilization) —
  **error**. Measure the user-visible symptom instead, not the resource.
- An SLO with no error-budget policy — **error**.
- A cause-based indicator (retries, restarts) — **flagged**.
- An un-calibrated or aspirational target — **warning**.

## Render and deploy

Project the objectives to your monitoring backend:

```bash
de slo render --target prometheus --in slo.yaml --out deploy/prometheus
de slo render --target grafana    --in slo.yaml --out deploy/grafana
```

The `prometheus` target emits a Cortex-ruler `PrometheusRule` with SLI recording rules and burn-rate
alerting rules. The `grafana` target emits an importable dashboard per service with an SLI trend, an
error-budget burndown, and a burn-rate panel. A `loki` target emits log-derived SLI rules for cases
that are cleaner to measure from logs.

The queries reference the metric names the SDK actually emits after the OpenTelemetry-to-Prometheus
normalization that Grafana Alloy applies — `rpc_server_call_duration_seconds_*` with the
`rpc_response_status_code` label. You write no query strings.

To ship the rules with the service, enable monitoring in your Helm values overlay and paste the
rendered rule groups:

```yaml
monitoring:
  enabled: true
  prometheusRule:
    groups:
      # paste the .spec.groups from deploy/prometheus/<service>-slo.prometheusrule.yaml
```

The chart's `PrometheusRule` and `ServiceMonitor` templates are off by default and render only when
you enable them.

## Burn-rate alerting

The rendered alerts are multi-window multi-burn-rate, so you page on real budget burn rather than a
single failed request:

| Tier | Burn rate | Windows | Action |
|------|-----------|---------|--------|
| Fast | 14.4× | 1h and 5m | Page |
| Medium | 6× | 6h and 30m | Page |
| Slow | 1× | 3d and 6h | Ticket |

The alert threshold is the burn rate times the error budget (`burn × (1 − target)`), evaluated over
both windows so a transient spike does not page.

## Add a non-default indicator

Availability and latency are derived from the contract. For freshness, correctness, coverage,
throughput, or durability, add the indicator by hand and pick the metric it measures. The
[`define-slo`](../../../concepts/reliability/) skill walks through choosing the SLI type and writing a
good ratio. Keep the client-fault exclusion when you override the availability denominator.

## Author a business or journey SLO

The derived objectives cover a single service. A **journey SLO** (Layer 2) states the reliability of a
critical user journey that spans several services — checkout, sign-up, a data-import flow — and it is
product-owned. It is a distinct layer from the per-service objectives, with a distinct owner and a
distinct consequence.

Author a journey SLO in a **separate file** from the derived service `slo.yaml`. The separate file is
the boundary between a business objective and the raw per-service telemetry: a service team refines
`slo.yaml`; product owns `journeys/*.slo.yaml`.

A journey SLI uses a **raw-query metric source** instead of the derived otel-rpc source. Set the
`query` field on the `good` and `total` sides to compose across service moats — typically the product
of each participating service's availability, read from that service's error-ratio recording rule:

```yaml
kind: SLI
metadata:
  name: checkout-availability
spec:
  ratioMetric:
    counter: false
    good:
      type: devedge/journey
      query: (1 - slo:sli_error:ratio_rate$window{slo="cartd-read-availability", service="cartd"}) * (1 - slo:sli_error:ratio_rate$window{slo="orderd-write-availability", service="orderd"})
    total:
      type: devedge/journey
      query: "1"
```

The `$window` token is replaced with each burn-rate window (`5m`, `1h`, ...) when the rules render, so
the composed objective gets the same multi-window multi-burn-rate alerting as a service SLO. A raw
query with no `$window` token is used verbatim at every window. The `query` wins over any typed
source; a ratio side with neither a query nor a typed signal fails the render, naming the SLO.

A journey SLO carries the same requirements as any objective: mark it `devedge.io/layer: journey`, give
it a 28-day window, reference a burn-rate `AlertPolicy`, and **write the error-budget policy**. `de slo
lint` validates it and `de slo render` projects it exactly like a service SLO. The latency-bucket-
boundary check does not apply to a raw-query SLI. A complete example is in the repository at
`slo/testdata/checkout.journey.slo.yaml`.

The three layers stay separate: a **signal** (Layer 0) diagnoses and has no target; a **service SLO**
(Layer 1) is derived from one service's contract; a **journey SLO** (Layer 2) composes several
services' indicators into a product objective. Keeping them in distinct declaration kinds — and, for
journeys, a distinct file — is what stops a monitoring signal from being mistaken for a business
objective.

## The metric names, and a semconv bump

The generated queries track the instruments this SDK emits by default: the new OpenTelemetry semantic
convention (`rpc.server.call.duration` in seconds, `rpc.response.status_code`). The metric prefix,
unit suffix, and label names are configurable, so an OpenTelemetry semantic-convention change is a
configuration change, not a rewrite. A service that opts into the legacy convention with
`OTEL_SEMCONV_STABILITY_OPT_IN` renders with the legacy naming
(`rpc_server_duration_milliseconds_*`, numeric `rpc_grpc_status_code`).
