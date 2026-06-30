---
title: Pluggability Model
weight: 2
---

The pluggability model is the way devedge-sdk separates a lightweight core from
optional, heavy-dependency adapters. The core packages — `server`, `middleware`,
`persistence`, `config`, `health`, `resilience` — depend only on the Go standard
library plus gRPC/protobuf. Every heavy dependency (ORM, policy engine, secret
manager, Kafka client, OTel SDK) lives in a clearly separated adapter sub-package.
Use this page to understand how the core and adapter layers fit together and what
that means when you add a new backend to your service.

## Core packages

The core packages import only the **Go standard library plus gRPC/protobuf**. No ORM, no
policy engine, no Vault SDK, no Kafka client, and no OTel SDK enters the core's dependency
closure. CI guard tests enforce this boundary — for example, `go list -deps ./server | grep gorm`
must be empty.

This means that when you import `server` or `middleware`, you do not pull in any of those heavy
transitive dependencies. Your service's `go.sum` stays lean, compile times stay fast, and no
version conflicts arise from indirect ORM or policy-engine imports.

## Seams and adapters

Every pluggable concern follows the same three-part shape:

1. **A narrow interface in core** — for example, `persistence.Repository[T,K]`,
   `secret.Encryptor`, `authz.Authorizer`, `health.Check`, `resilience.CircuitBreaker`.
2. **A development-suitable default in core (or a nearby package)** — for example,
   `persistence.MemoryRepository`, `secret.NewDev`, `authz.NewDevAuthorizer`. These run
   in-process with no external services required.
3. **A production adapter in a sub-package** — for example, `secret/vault` (Vault Transit),
   `events/kafkabus` (Kafka), `observability/otel` (OTel SDK and exporters). These are the only
   packages that import the heavy dependency.

A service starts with the development default. When ready for production, you import the adapter
sub-package, pass the adapter to the same constructor, and ship. Nothing else in the service
changes.

## Observability

The OTel seam illustrates the pattern:

- **Core imports the OTel API** (`go.opentelemetry.io/otel`, `…/trace`, `…/metric`,
  `…/propagation`). The API is itself a vendor-neutral, pluggable no-op by default — instrumentation
  in core costs nothing until an SDK is installed.
- **`observability/otel` is the only package that imports the OTel SDK and exporters.** It installs
  the global `TracerProvider`/`MeterProvider` and the W3C propagator. This is the same pattern as
  `events/kafkabus` being the only package that imports the Kafka client.
- **Calling `otel.Setup` is the only opt-in step.** Without it, everything is inert: no-op
  providers, no propagator, zero overhead. With it, every span, metric, and log correlation that
  core already emits flows to the configured backend.

A future Prometheus-pull exporter or a different tracing backend slots in as an additional adapter
with no change to core.

## Persistence

The persistence seam exposes `Repository[T,K]`:

```
persistence.MemoryRepository   — in-process dev/test default (no DB)
protoc-gen-storage             — generated GORM adapter
protoc-gen-ent                 — generated ent adapter
hand-written Repository        — any other backend (sqlc, DynamoDB, …)
```

Service code only depends on `persistence.Repository[T,K]`. The backend is a constructor argument.
`server.New` does not know or care which adapter was passed.

## Authorization

Authorization (authz) follows the same shape:

```
authz.NewDevAuthorizer(grants)  — in-process, grants-based dev default
opa.NewAuthorizer(…)            — OPA sidecar adapter (in the private internal SDK)
cedar.NewAuthorizer(…)          — Cedar adapter (future)
```

`server.Config.Authorizer` is typed `authz.Authorizer`. The fail-closed property lives in the
interceptor, not in the adapter — both the development and OPA adapters deny undeclared methods by
the same boot-gate check.

## Dependency boundary reference

The table below summarizes what each package imports and what it keeps out.

| Package | What it imports | What it does not import |
|---|---|---|
| `server`, `middleware` | stdlib, gRPC, OTel API | ORM, policy engine, Vault, Kafka |
| `config` | stdlib only | koanf (opt-in via `config/koanf`) |
| `health` | stdlib only | ORM, gRPC (only `google.golang.org/grpc/health` which is already in the graph) |
| `observability/otel` | OTel SDK + OTLP exporter | nothing extra; this IS the heavy package |
| `events/kafkabus` | Kafka client | nothing extra; this IS the heavy package |

{{< callout type="warning" >}}
**Any PR that adds a transitive ORM import to `server` or `middleware` fails the CI guard test.**
Check the guard output before merging if you add a new dependency to either package.
{{< /callout >}}

## Working with adapters

- **Scaffold with the development defaults.** Everything works in-process; no external services are
  needed to run or test your service locally.
- **Swap adapters at the service boundary, not inside service code.** When you want Vault for secret
  fields, import `secret` and pass `secret.NewVaultTransit(…)` to the repository constructor. The
  handler and the repository interface stay unchanged.
- **Add a new backend without touching core.** A third-party team can ship a new authz adapter or a
  new storage adapter as a separate module. The core never changes; the seam is stable.

See [Architecture](../../concepts/architecture/) for the full interceptor chain, and the
[How-to Guides](../../how-to/) for the practical steps in each area.
