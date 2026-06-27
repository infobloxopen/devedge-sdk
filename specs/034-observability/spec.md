# F034 — Built-in observability: OpenTelemetry tracing + RED metrics + structured slog, behind a dependency-light seam

**Status**: design locked. **Issue**: #90 (DX 054, P0). **Initiative**: WS-007 (reposition + operational *ilities).
**Depends on**: `server.New`/`Serve` (server/server.go), the existing `middleware/redact` helper, the
default unary interceptor chain.

**Origin / user directive (verbatim intent)**: build every operational capability "behind a neutral seam,
with the concrete backend — and its transitive dependencies — isolated in a separate adapter sub-package.
The core must stay dependency-light." Observability specifically: "Prefer the OTel **API** (designed for
pluggable exporters) over hard-wiring a vendor SDK into core."

## Problem
At v0.23.0 the SDK has **zero** observability: no tracing, no metrics, no structured request logging in the
default chain (grep confirms no `go.opentelemetry.io/*`, no Prometheus). A scaffolded service is a black box
in production — no RED metrics, no distributed traces across the gateway→gRPC hop, no trace-correlated logs.
This is table stakes for the "microservices foundation" framing WS-007 is honestly closing toward.

## Decision (locked)
**The OTel API is the seam.** OpenTelemetry's Go API (`go.opentelemetry.io/otel`, `.../trace`,
`.../metric`, `.../propagation`) is itself a vendor-neutral, pluggable seam: instrumentation calls a
**global no-op** provider until an SDK is installed, so instrumenting the core is free and side-effect-free.
We exploit that directly rather than inventing a parallel interface.

- **Core (dependency-light) — OTel API + contrib instrumentation only.** `server/` installs:
  - `grpc.StatsHandler(otelgrpc.NewServerHandler())` on the gRPC server → per-RPC server spans + RED
    metrics (`rpc.server.duration`, request/response sizes) via the **global** providers.
  - `grpc.WithStatsHandler(otelgrpc.NewClientHandler())` on the in-process gateway→gRPC dial, and
    `otelhttp.NewHandler(mux, …)` wrapping the gateway mux → an incoming HTTP request starts a server span
    and the client handler injects W3C context into gRPC metadata. End-to-end one trace.
  - `otelgrpc`/`otelhttp` are **contrib instrumentation** that depend on the OTel **API** (+ metric/trace),
    **not** the SDK or any exporter. They are acceptable in core (this is what the API layer is *for*).
- **Adapter (`observability/otel/`) — the ONLY package that imports the OTel SDK + exporters.** Exposes:
  ```go
  package otel // github.com/infobloxopen/devedge-sdk/observability/otel
  type Config struct {
      ServiceName, ServiceVersion string
      Exporter     string // "otlp" (default), "stdout" (dev), "none" (no-op)
      OTLPEndpoint string // optional override of OTEL_EXPORTER_OTLP_ENDPOINT
  }
  // Setup installs the global TracerProvider, MeterProvider, and W3C TraceContext+Baggage
  // propagator, then returns a shutdown that flushes exporters (bounded timeout).
  func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)
  ```
  Imports confined here: `otel/sdk/{trace,metric,resource}`, `exporters/otlp/otlptrace/otlptracegrpc`,
  `exporters/otlp/otlpmetric/otlpmetricgrpc`, `exporters/stdout/{stdouttrace,stdoutmetric}`, `semconv`.
  Honors the standard `OTEL_*` env vars so a deployment configures it with no code change.
- **Structured logging on by default.** New `middleware.LoggingUnary(logger *slog.Logger)` is added to the
  default chain. It logs one structured record per RPC at Info — `method`, `grpc.code`, `duration_ms`,
  `request_id`, tenant (`account-id`), and `trace_id`/`span_id` pulled from the span context (OTel API,
  already in core). At Debug it logs request/response **run through the existing `redact.Message`** so
  secret-annotated fields are never emitted in cleartext — redaction is on by default wherever payloads
  are logged. `server.Config` gains `Logger *slog.Logger` (defaults to `slog.Default()`).
- **Core sets no globals.** Good library hygiene: only the adapter calls `otel.SetTracerProvider/…`. Core
  only *reads* the globals via the contrib handlers. With no adapter installed everything is inert
  (no-op provider, no propagator) → zero overhead, no behavioral change for consumers who don't opt in.

## Locked defaults
- Default `Exporter` in the scaffold = `"otlp"`, driven by standard `OTEL_EXPORTER_OTLP_ENDPOINT`; if that
  env is unset the OTLP exporter is created lazily/no-ops without crashing the service. `"stdout"` for local
  dev, `"none"` to fully disable.
- Logging defaults to Info summaries (no payloads); payload logging (redacted) only at Debug.
- One adapter (`observability/otel`, OTLP for traces+metrics) ships now. A Prometheus-pull exporter is an
  **additional** adapter that slots in behind the same seam later — NOT built now (scope gate).

## Design — files
- `server/server.go`: add stats handlers (server + in-process client), wrap gateway mux with `otelhttp`,
  insert `LoggingUnary` into the default chain (after RequestID/ErrorMapper/Tenant, before authz so the log
  captures request_id+tenant+final code), add `Config.Logger`.
- `middleware/logging.go` (new): `LoggingUnary(*slog.Logger) grpc.UnaryServerInterceptor`, trace-correlated,
  redaction-on-by-default for payloads.
- `observability/otel/otel.go` (new adapter): `Setup`/`Config`/shutdown as above.
- `observability/doc.go` or guard test: `cleancore_test.go` asserting **no** core package imports the OTel
  SDK/exporters (mirror of the kafka clean-core discipline).
- Scaffold: `cmd/devedge-sdk/internal/scaffold/templates/main.go.tmpl` + `main.ent.go.tmpl` call
  `otel.Setup(ctx, otel.Config{ServiceName: …, ServiceVersion: …})` with deferred shutdown and pass
  `Logger` into `server.Config`; `go.mod.tmpl` + `go.mod.ent.tmpl` add the adapter + API requires;
  `README.md.tmpl`/`Makefile.tmpl` note the `OTEL_*` env.
- Docs: `docs/content/docs/guides/observability.md` (how to wire an exporter; the seam; the env vars).

## Acceptance criteria
- **AC-1 (instrumented by default, free by default).** A scaffolded service run with
  `OTEL_EXPORTER_OTLP_ENDPOINT` set emits per-RPC spans + RED metrics to the collector; with no
  endpoint/adapter it runs unchanged with zero observability overhead (no-op providers).
- **AC-2 (one trace across the hop).** A request through the REST gateway produces a single trace:
  HTTP server span → gRPC client span → gRPC server span → handler, linked by W3C context.
- **AC-3 (trace-correlated, redacted logs).** The default chain logs each RPC as a structured slog record
  carrying `trace_id`/`span_id`; secret-annotated fields never appear in cleartext in any log path.
- **AC-4 (THE dependency-light gate).** An import-guard test proves no core package (`server`, `middleware`,
  `authz`, `grpcauthz`, `persistence`, `events` excluding `kafkabus`, `lro`, `secret`) imports
  `go.opentelemetry.io/otel/sdk` or any `…/exporters/…`; only `observability/otel` does. The OTel SDK +
  exporters appear in the root `go.mod` solely because the adapter pulls them (indirect, exactly like
  `franz-go` for `kafkabus`).
- **AC-5 (gates).** `go build ./...`, `go vet ./...`, `go test ./...`, and the repo Security Check pass;
  scaffold E2E (apx + buf) stays green.

## Failure modes
- **Core leaks a global side effect** (e.g. `otel.SetTracerProvider` from `server.New`) → breaks the seam,
  surprises consumers. Mitigation: globals set ONLY in the adapter; guard test forbids SDK import in core.
- **StatsHandler double-instruments** alongside the interceptor chain → inflated metrics. Mitigation: use
  the stats handler for spans/metrics; interceptors only for logging — no overlap; verify counts in a test.
- **Exporter hangs on shutdown** → process won't exit. Mitigation: `Setup` returns a shutdown with a bounded
  context timeout; scaffold defers it with a timeout.
- **OTLP endpoint unset crashes startup** → bad dev UX. Mitigation: lazy/no-op exporter when endpoint absent.

## Tasks
- **T1 [C]** core seam — `server/server.go` stats handlers (server + in-process client) + `otelhttp` gateway
  wrap + `Config.Logger`; `middleware/logging.go` `LoggingUnary` (trace-correlated, redact-by-default).
- **T2 [C]** `observability/otel` adapter — `Setup`/`Config`/shutdown; OTLP+stdout+none; resource from
  ServiceName/Version; W3C propagator. The sole SDK/exporter importer.
- **T3 [S]** tests — `cleancore_test.go` import guard (AC-4); `LoggingUnary` redaction + trace-id unit test;
  a gateway→gRPC propagation test (single trace, AC-2) using a stdout/in-memory exporter.
- **T4 [S]** scaffold — `main.go.tmpl` + `main.ent.go.tmpl` wire `otel.Setup` + `Logger`; `go.mod*.tmpl`
  add requires; `README.md.tmpl`/`Makefile.tmpl` note `OTEL_*` env.
- **T5 [S]** docs — `guides/observability.md`.

## Exit
All ACs green; PR merged to main; tag cut. The DX cadence re-run must later show observability as **present**.
