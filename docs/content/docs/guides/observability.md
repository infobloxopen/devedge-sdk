---
title: Observability
weight: 6
---

A service built with the SDK is observable by default: per-RPC **traces**, **RED metrics**, and
trace-correlated structured **logs**, with a single end-to-end trace across the REST gateway → gRPC
hop. It is wired behind a dependency-light seam, so the core stays free of any vendor SDK and you
choose the backend at the edge.

## The seam: the OpenTelemetry API

OpenTelemetry's Go **API** (`go.opentelemetry.io/otel`, `.../trace`, `.../metric`,
`.../propagation`) is itself a vendor-neutral, pluggable seam. Instrumentation calls a **global
no-op** provider until an SDK is installed — so the SDK core can instrument freely and it costs
nothing until you opt in.

- **Core (`server`, `middleware`, …) imports the OTel API + contrib instrumentation only.** The
  server installs `otelgrpc` stats handlers (server + the in-process gateway client) and wraps the
  gateway mux with `otelhttp`. These contrib packages depend on the API, **not** the SDK or any
  exporter.
- **The `observability/otel` adapter is the ONLY package that imports the OTel SDK + exporters.** It
  installs the global `TracerProvider`/`MeterProvider` and a W3C propagator. This mirrors how
  `events/kafkabus` is the only package that imports the Kafka client — the heavy dependency is
  confined to one adapter, kept out of the core's dependency closure (enforced by a CI guard test).

With **no** call to `otel.Setup`, everything is inert: no-op providers, no propagator, zero overhead
and no behavioral change. Nothing in core sets a global.

## Turning it on

Call `otel.Setup` once at startup and defer its bounded shutdown. A scaffolded service already does
this:

```go
import "github.com/infobloxopen/devedge-sdk/observability/otel"

shutdown, err := otel.Setup(ctx, otel.Config{
    ServiceName:    "orders",
    ServiceVersion: version,
})
if err != nil {
    log.Fatalf("observability: %v", err)
}
defer func() {
    sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(sctx)
}()
```

`otel.Config`:

| Field            | Meaning                                                                 |
|------------------|-------------------------------------------------------------------------|
| `ServiceName`    | `service.name` on every span/metric (or `OTEL_SERVICE_NAME`).           |
| `ServiceVersion` | `service.version` on every span/metric.                                 |
| `Exporter`       | `"otlp"` (default), `"stdout"` (dev console), or `"none"` (no-op).       |
| `OTLPEndpoint`   | Optional override of `OTEL_EXPORTER_OTLP_ENDPOINT` (`host:port`).        |

## Installing an exporter

The default `"otlp"` exporter ships traces **and** metrics to an OTLP/gRPC collector and is driven
entirely by the standard `OTEL_*` environment — no code change to point it at a backend:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
export OTEL_SERVICE_NAME=orders
./orders
```

The OTLP exporter connects **lazily**, so an unset or unreachable endpoint never crashes startup —
the service runs as if uninstrumented until a collector is reachable. For local development set
`Exporter: "stdout"` to print spans/metrics to the console; set `"none"` to fully disable export
while keeping the W3C propagator active.

A Prometheus-pull exporter (or any other backend) slots in behind the same seam as an **additional**
adapter — the core does not change.

## Common `OTEL_*` environment variables

| Variable                       | Purpose                                                      |
|--------------------------------|--------------------------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT`  | Collector endpoint for both traces and metrics.              |
| `OTEL_SERVICE_NAME`            | Service name when `Config.ServiceName` is empty.             |
| `OTEL_EXPORTER_OTLP_HEADERS`   | Headers (e.g. auth) sent to the collector.                   |
| `OTEL_EXPORTER_OTLP_PROTOCOL`  | `grpc` (default here) / `http/protobuf`.                     |
| `OTEL_TRACES_SAMPLER`          | Sampling strategy (e.g. `parentbased_traceidratio`).         |
| `OTEL_RESOURCE_ATTRIBUTES`     | Extra resource attributes merged onto every signal.          |

## Logs: trace-correlated and redacted by default

`server.New` puts `middleware.LoggingUnary` in the default chain (after request-ID/tenant, before
authz). It logs **one structured slog record per RPC** at Info — `method`, `grpc.code`,
`duration_ms`, `request_id`, `account-id`, and, when a span is active, `trace_id`/`span_id` (read
from the API span context) so logs correlate with traces. At Debug it logs the request/response
**run through `redact.Message`** first, so `(infoblox.field.v1.opts).secret = true` fields never
appear in cleartext in any log path. Provide your own logger via `server.Config.Logger`; it defaults
to `slog.Default()`.

## What you get

| Signal  | Source                                                                                 |
|---------|----------------------------------------------------------------------------------------|
| Traces  | `otelgrpc` server span per RPC; `otelhttp` server span at the gateway; `otelgrpc` client span on the in-process dial — one linked trace. |
| Metrics | `otelgrpc` RED metrics (`rpc.server.duration`, request/response sizes) via the global meter. |
| Logs    | `middleware.LoggingUnary` — one trace-correlated, secret-redacted record per RPC.      |
