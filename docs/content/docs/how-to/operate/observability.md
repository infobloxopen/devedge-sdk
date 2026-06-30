---
title: Observability
weight: 1
aliases:
  - /docs/guides/observability/
---

A service built with the SDK is observable by default: per-RPC distributed traces, RED metrics (rate, errors, duration), and trace-correlated structured logs are emitted with zero configuration. A single end-to-end trace spans the REST gateway and the gRPC hop behind it. Observability is wired behind a dependency-light seam, so the core stays free of any vendor SDK and you choose the backend at the edge. Use this page when you want to enable telemetry export, configure an exporter, or understand what signals the SDK emits and where they come from.

## OpenTelemetry API seam

The SDK instruments itself against the [OpenTelemetry](https://opentelemetry.io/) Go API (`go.opentelemetry.io/otel`, `.../trace`, `.../metric`, `.../propagation`). The API is vendor-neutral: instrumentation calls a global no-op provider until a real SDK is installed. This means the SDK core can emit telemetry at no cost until you explicitly opt in.

Two packages share the responsibility:

- **Core packages** (`server`, `middleware`, and similar) import the OTel API and contrib instrumentation only. The server installs `otelgrpc` stats handlers on both the gRPC server and the in-process gateway client, and wraps the gateway mux with `otelhttp`. These contrib packages depend on the API, not the SDK or any exporter.
- **`observability/otel` adapter** is the only package that imports the OTel SDK and exporters. It installs the global `TracerProvider` and `MeterProvider` and registers a W3C propagator. This mirrors the pattern used by `events/kafkabus`, which is the only package that imports the Kafka client — the heavy dependency stays in one adapter and out of the core's dependency graph. A CI guard test enforces this boundary.

{{< callout type="info" >}}
**Without a call to `otel.Setup`, all instrumentation is inert.** Providers remain no-op, no propagator is registered, and there is zero overhead and no behavioral change. Nothing in core sets a global provider.
{{< /callout >}}

## Enabling observability

Call `otel.Setup` once at startup and defer its bounded shutdown. A scaffolded service already includes this call:

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

`otel.Config` fields:

| Field            | Meaning                                                                 |
|------------------|-------------------------------------------------------------------------|
| `ServiceName`    | `service.name` on every span and metric (or `OTEL_SERVICE_NAME`).      |
| `ServiceVersion` | `service.version` on every span and metric.                             |
| `Exporter`       | `"otlp"` (default), `"stdout"` (dev console), or `"none"` (no-op).     |
| `OTLPEndpoint`   | Optional override of `OTEL_EXPORTER_OTLP_ENDPOINT` (`host:port`).      |

## Configuring an exporter

The default `"otlp"` exporter sends traces and metrics to an OTLP/gRPC collector. You control the endpoint entirely through standard `OTEL_*` environment variables, with no code change required to point at a different backend:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
export OTEL_SERVICE_NAME=orders
./orders
```

{{< callout type="info" >}}
**The OTLP exporter connects lazily.** An unset or unreachable endpoint does not crash startup — the service runs as if uninstrumented until a collector becomes reachable.
{{< /callout >}}

For local development, set `Exporter: "stdout"` to print spans and metrics to the console. Set `Exporter: "none"` to disable export while keeping the W3C propagator active.

A Prometheus-pull exporter or any other backend can be added as an additional adapter without changing core packages.

## Common `OTEL_*` environment variables

| Variable                       | Purpose                                                      |
|--------------------------------|--------------------------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT`  | Collector endpoint for both traces and metrics.              |
| `OTEL_SERVICE_NAME`            | Service name when `Config.ServiceName` is empty.             |
| `OTEL_EXPORTER_OTLP_HEADERS`   | Headers (e.g. auth) sent to the collector.                   |
| `OTEL_EXPORTER_OTLP_PROTOCOL`  | `grpc` (default here) / `http/protobuf`.                     |
| `OTEL_TRACES_SAMPLER`          | Sampling strategy (e.g. `parentbased_traceidratio`).         |
| `OTEL_RESOURCE_ATTRIBUTES`     | Extra resource attributes merged onto every signal.          |

## Logs

`server.New` places `middleware.LoggingUnary` in the default interceptor chain, after the request-ID and tenant interceptors and before authz. It emits one structured `slog` record per RPC at Info level with the fields `method`, `grpc.code`, `duration_ms`, `request_id`, `account-id`, and — when a span is active — `trace_id` and `span_id`. The trace and span IDs are read from the API span context, so log records correlate with traces automatically.

At Debug level, the interceptor logs the request and response after passing them through `redact.Message`. Fields marked `(infoblox.field.v1.opts).secret = true` never appear in cleartext in any log path.

To use a different logger, set `server.Config.Logger`. It defaults to `slog.Default()`.

## Emitted signals

| Signal  | Source                                                                                 |
|---------|----------------------------------------------------------------------------------------|
| Traces  | `otelgrpc` server span per RPC; `otelhttp` server span at the gateway; `otelgrpc` client span on the in-process dial — one linked trace. |
| Metrics | `otelgrpc` RED metrics (`rpc.server.duration`, request/response sizes) via the global meter. |
| Logs    | `middleware.LoggingUnary` — one trace-correlated, secret-redacted record per RPC.      |
