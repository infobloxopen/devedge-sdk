---
title: Pluggability Model
weight: 2
---

devedge-sdk is structured around a single architectural principle: **the core is dep-light; every
heavy dependency lives in a clearly separated adapter.** This is the pluggability model. It is what
allows the SDK to instrument freely, ship dev defaults for every concern, and let services swap
production backends without changing service code.

## The dep-light core

The core packages (`server`, `middleware`, `persistence`, `config`, `health`, `resilience`, …) depend
only on the **Go standard library plus gRPC/protobuf**. No ORM, no policy engine, no Vault SDK,
no Kafka client, no OTel SDK enters the core's dependency closure. This is enforced by CI guard
tests (`go list -deps ./server | grep gorm` must be empty, and so on).

The benefit: when you import `server` or `middleware`, you do not pull in any of those heavy
transitive dependencies. The service's `go.sum` stays lean; compile times stay fast; no version
conflicts arise from indirect ORM or policy-engine imports.

## Seams and adapters

Every pluggable concern follows the same shape:

1. **A narrow interface in core.** For example, `persistence.Repository[T,K]`, `secret.Encryptor`,
   `authz.Authorizer`, `health.Check`, `resilience.CircuitBreaker`.
2. **A dev-suitable default in core (or a nearby package).** `persistence.MemoryRepository`,
   `secret.NewDev`, `authz.NewDevAuthorizer` — all run in-process, zero external services.
3. **A production adapter in a sub-package.** `secret/vault` (Vault Transit), `events/kafkabus`
   (Kafka), `observability/otel` (OTel SDK + exporters). These are the **only** packages that
   import the heavy dependency.

A service starts with the dev default. When ready, it imports the adapter sub-package, passes the
adapter to the same constructor, and ships. Nothing else changes.

## Observability as a worked example

The OTel seam illustrates this most clearly:

- **Core imports the OTel API** (`go.opentelemetry.io/otel`, `…/trace`, `…/metric`,
  `…/propagation`). The API is itself a vendor-neutral, pluggable no-op by default — instrumentation
  in core costs nothing until an SDK is installed.
- **`observability/otel` is the only package that imports the OTel SDK + exporters.** It installs
  the global `TracerProvider`/`MeterProvider` and the W3C propagator. This is the same pattern as
  `events/kafkabus` being the only package that imports the Kafka client.
- **Calling `otel.Setup` is the only opt-in step.** Without it, everything is inert: no-op
  providers, no propagator, zero overhead. With it, every span, metric, and log correlation that
  core already emits flows to the configured backend.

This means a future Prometheus-pull exporter, or a different tracing backend, slots in as an
**additional adapter** with no change to core.

## Persistence as a worked example

The persistence seam exposes `Repository[T,K]`:

```
persistence.MemoryRepository   — in-process dev/test default (no DB)
protoc-gen-storage             — generated GORM adapter
protoc-gen-ent                 — generated ent adapter
hand-written Repository        — any other backend (sqlc, DynamoDB, …)
```

Service code only depends on `persistence.Repository[T,K]`. The backend is a constructor argument.
`server.New` does not know or care which adapter was passed.

## Authz as a worked example

```
authz.NewDevAuthorizer(grants)  — in-process, grants-based dev default
opa.NewAuthorizer(…)            — OPA sidecar adapter (in the private internal SDK)
cedar.NewAuthorizer(…)          — Cedar adapter (future)
```

`server.Config.Authorizer` is typed `authz.Authorizer`. The fail-closed property is in the
interceptor, not in the adapter — both dev and OPA adapters deny undeclared methods by the same
boot-gate check.

## The dep-light guarantee in practice

| Package | What it imports | What it does NOT import |
|---|---|---|
| `server`, `middleware` | stdlib, gRPC, OTel API | ORM, policy engine, Vault, Kafka |
| `config` | stdlib only | koanf (opt-in via `config/koanf`) |
| `health` | stdlib only | ORM, gRPC (only `google.golang.org/grpc/health` which is already in the graph) |
| `observability/otel` | OTel SDK + OTLP exporter | nothing extra; this IS the heavy package |
| `events/kafkabus` | Kafka client | nothing extra; this IS the heavy package |

CI enforces this: any PR that adds a transitive ORM import to `server` or `middleware` fails the
guard test.

## What this means for you

- **Scaffold a service with the dev defaults.** Everything works in-process; no external services
  needed to run or test the service locally.
- **Swap adapters at the edge, not in the service.** When you want Vault for secret fields, import
  `secret` and pass `secret.NewVaultTransit(…)` to the repository constructor. The handler and
  the repository interface are unchanged.
- **Add a new backend without touching core.** A third-party team can ship a new authz adapter or
  a new storage adapter as a separate module. The core never changes; the seam is stable.

See [Architecture](../../concepts/architecture/) for the full interceptor chain, and the
[How-to Guides](../../how-to/) for the practical steps in each area.
