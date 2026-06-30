---
title: Documentation
next: getting-started
weight: 1
---

**devedge-sdk** is the runtime library that production Infoblox services import. It gives you a
gRPC/HTTP service with authorization, tenant isolation, persistence, and observability
already wired — so you write business logic, not plumbing.

devedge-sdk is the companion to [devedge](https://github.com/infobloxopen/devedge), the
local dev and deployment tooling. devedge handles dev- and deploy-time concerns;
devedge-sdk is the runtime library your service binary imports.

## What the SDK provides

- **A service from one proto.** `server.New` assembles a gRPC server and an optional
  HTTP/JSON gateway, AIP-correct by default. It wires the full interceptor chain
  (request-ID → error mapper → tenant-ID → fail-closed authz → field-mask → ETag/412 →
  read-mask → validate-only → dedup) and the Google-AIP semantics that go with it:
  field-mask `PATCH`, ETag/412 concurrency, pagination, filtering, soft-delete, batch,
  request de-duplication, and long-running operations.
- **Fail-closed authorization.** The SDK is secure by default, and the security
  invariants are provable in CI. `(infoblox.authz.v1.rule)` declares each method's
  requirement. The service refuses to start if any served method is undeclared. Every
  query is scoped to the caller's `account-id`. Fields annotated with
  `(infoblox.field.v1.opts).secret` are encrypted at rest and never returned in
  responses. The `seccheck` package verifies authz completeness, unknown-principal
  denial, cross-account isolation, clean error messages, and no secret leak — all in CI.
- **Swappable backends.** Each integration point ships a usable dev default, not just a
  swap point, and accepts a production backend without requiring changes to service code.
  The integration points are: persistence (a `Repository[T,K]` seam, with an in-memory
  store and generated GORM/ent adapters), transactions and DDD aggregates, domain events
  (a transactional outbox backed by an in-memory bus or Kafka), the authz decision point,
  and the secret encryptor.
- **Operational foundation.** The operational foundation is included: the scaffold wires
  observability (OTel traces, metrics, and logs), health and readiness probes, resilience
  interceptors (timeouts, rate limiting, and circuit breaker), configuration, and
  deployment artifacts (Kubernetes and Docker Compose), with no hand-authoring required.

## Documentation sections

{{< cards >}}
  {{< card link="getting-started/" title="Getting Started" icon="play" subtitle="Install the SDK and stand up a running, fail-closed service in five minutes." >}}
  {{< card link="explanation/" title="Why / Evaluate" icon="light-bulb" subtitle="What devedge-sdk is, its security posture, and its pluggability model — start here if you're evaluating." >}}
  {{< card link="how-to/" title="How-to Guides" icon="book-open" subtitle="Task-focused guides grouped by concern: Model & Persist, Secure, Operate, Configure." >}}
  {{< card link="reference/" title="Reference" icon="document-text" subtitle="API reference for each package and codegen plugin." >}}
  {{< card link="tutorial/" title="Tutorial" icon="academic-cap" subtitle="Build the API Key Manager service end to end." >}}
  {{< card link="concepts/" title="Concepts" icon="template" subtitle="Runtime internals: the interceptor chain, the annotation contract, tenant isolation, aggregates, and events." >}}
{{< /cards >}}
