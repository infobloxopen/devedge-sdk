---
title: Documentation
next: getting-started
weight: 1
---

Welcome to the **devedge-sdk** documentation.

devedge-sdk is the runtime library that production Infoblox services import. It is the
companion to [devedge](https://github.com/infobloxopen/devedge) (the local dev edge /
deployment substrate): devedge is **dev- and deploy-time** tooling; devedge-sdk is the
**runtime library**.

## What it gives you

- **A running, AIP-correct service from one proto.** `server.New` assembles a gRPC server plus an
  optional HTTP/JSON gateway with the framework interceptor chain wired (request-ID → error mapper →
  tenant-ID → fail-closed authz → field-mask → ETag/412 → read-mask → validate-only → dedup) and the
  Google-AIP semantics that go with it: field-mask `PATCH`, ETag/412 concurrency, pagination,
  filtering, soft-delete, batch, request de-duplication, and long-running operations.
- **Secure by default, provable in CI.** Authorization is fail-closed — `(infoblox.authz.v1.rule)`
  declares each method's requirement and the service refuses to boot if any served method is
  undeclared; every query is tenant-scoped by `account-id`; secret-annotated fields
  (`(infoblox.field.v1.opts).secret`) are encrypted at rest and never returned. The `seccheck`
  package proves authz completeness, unknown-principal denial, cross-account isolation, clean error
  messages, and no-secret-leak — all in CI.
- **Pluggable seams with dev defaults.** Persistence (a neutral `Repository[T,K]` seam: in-memory dev
  store → generated GORM/ent shapes), transactions and DDD **aggregates**, domain **events** (a
  transactional outbox with an in-memory bus → Kafka), the authz decision point, and the secret
  encryptor each ship a dev-suitable default and swap for a production backend **without changing
  service code**.
- **Codegen from your proto.** `protoc-gen-devedge-authz` (the authz-rules table), `protoc-gen-svc`
  (service scaffold), `protoc-gen-storage` (GORM repository), and `protoc-gen-ent` (ent schema) — plus
  the third-party `protoc-gen-grpc-gateway` — turn the proto into a complete service. The proto is the
  single source of truth; core packages depend only on the standard library (no ORM, no policy engine).

## Sections

{{< cards >}}
  {{< card link="getting-started/" title="Getting Started" icon="play" subtitle="Install the SDK and stand up a running, fail-closed service in five minutes." >}}
  {{< card link="concepts/" title="Concepts" icon="light-bulb" subtitle="Architecture and the seam model, the annotation contract, tenant isolation, aggregates, and events." >}}
  {{< card link="guides/" title="Guides" icon="book-open" subtitle="Task-focused how-tos: define a service, model a resource, pick a storage shape, run seccheck." >}}
  {{< card link="reference/" title="Reference" icon="document-text" subtitle="API reference for each package and codegen plugin." >}}
  {{< card link="tutorial/" title="Tutorial" icon="academic-cap" subtitle="Build the API Key Manager service end to end." >}}
{{< /cards >}}
