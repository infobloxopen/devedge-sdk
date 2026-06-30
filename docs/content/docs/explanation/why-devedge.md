---
title: Why devedge-sdk?
weight: 1
---

devedge-sdk is a Go runtime library that wires cross-cutting concerns — authorization,
multi-tenancy, secret handling, observability, resilience, and health probes — into every service
that imports it. It gives you a secure, AIP-correct foundation so you can focus on business logic
rather than rebuilding the same infrastructure on every service.

Use devedge-sdk when you are building a multi-tenant platform service that carries
production traffic. See [When to use it](#when-to-use-it) for the full criteria.

## What the SDK provides

devedge-sdk is the runtime layer that closes cross-cutting gaps structurally:

- **One proto file drives the entire service.** The authz rules, the storage model, the HTTP/JSON
  gateway, and the scaffold are all generated from the proto. The contract is the single source of
  truth; hand-written boilerplate disappears.
- **Security is the default, not an option.** Authorization is fail-closed — an undeclared method
  is denied, and the server refuses to boot with any served method undeclared. Tenant isolation is
  enforced at the storage layer, not left to handler logic. Secret-annotated fields are never stored
  as plaintext and never returned in responses.
- **Provable in CI.** The `seccheck` package turns these invariants into ordinary Go test
  assertions: authz completeness, unknown-principal denial, cross-account isolation, clean error
  messages, and no-secret-leak. The properties are proven on every PR, not assumed.
- **Pluggable seams with dev defaults.** Every production concern — the decision point (OPA/Cedar),
  the secret store (Vault), the database — ships behind an interface with a dev-suitable default
  that runs in-process with zero external services. Swapping the backend requires no service code
  change.
- **Observability from day one.** Traces, RED metrics, and trace-correlated logs are wired by
  `server.New`. The backend is pluggable (OTel API + adapter pattern); calling `otel.Setup` once is
  enough to point the service at any OTLP collector.

## Gaps in hand-written services

Without a shared runtime, each team re-solves the same set of problems. Small deviations in those
solutions produce security gaps and operational inconsistencies that are expensive to find after the
fact:

- **Authorization** is opt-in and often incomplete. Middleware is added per route; a forgotten
  route is an open door. The "fail-closed by default" property has to be hand-maintained.
- **Multi-tenancy** is applied inconsistently. A missed filter predicate lets one tenant read
  another's data. There is no mechanical way to prove isolation in CI.
- **Secret fields** are stored as plaintext unless the developer remembers to hash and encrypt
  them — and remembers to redact them from logs and responses.
- **Observability** is bolted on service-by-service with different libraries and different signal
  shapes, making cross-service correlation difficult.
- **Resilience** (timeouts, rate limiting, circuit breakers) is absent from most services until
  an incident reveals the gap.

## When to use it

devedge-sdk is the right foundation when:

- You are building a **multi-tenant platform service** that carries production traffic.
- You need **AIP-correct** (resource-oriented, standard methods, field masks, filtering,
  pagination, soft-delete, ETag/412 concurrency, LRO) semantics without implementing them.
- You want **authz and secret handling provably correct in CI**, not assumed.
- You want to adopt a shared operational foundation (health probes, observability, resilience)
  without each service team re-inventing it.

It is not the right choice for:

- Pure batch / offline workloads with no gRPC or HTTP surface.
- Services that are not multi-tenant and have no need for the authz/secret-handling machinery.

## Where it fits

devedge-sdk is the **runtime library** — the module your service imports. Its companion,
[devedge](https://github.com/infobloxopen/devedge), is the **dev- and deploy-time CLI**: it
scaffolds services, manages the local cluster, and renders deployment artifacts. The two are
designed to be used together but are independent — the SDK is a Go module, the CLI is a tool.

See [Architecture](../../concepts/architecture/) for the three-layer model and the interceptor
chain, and [Pluggability](../pluggability/) for how the seam model keeps your service code
vendor-neutral as backends evolve.
