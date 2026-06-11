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

- A **proto annotation contract** — `(infoblox.authz.v1.rule)` declares a method's authz
  requirement; `(infoblox.authz.v1.field).secret` marks a field as sensitive.
- **Codegen plugins** — `protoc-gen-svc`, `protoc-gen-storage`, `protoc-gen-ent` turn the
  proto into a service scaffold, a GORM repository, and an ent schema.
- A **batteries-included server** — `server.New` assembles the framework interceptor chain
  (request-ID → error mapper → tenant-ID → fail-closed authz → field-mask → ETag/412) and an
  optional HTTP/JSON gateway.
- **Secret-at-rest** — the `secret` package encrypts and hashes secret fields; AES-256-GCM
  for dev, Vault Transit for production.
- **Pluggable persistence** — a neutral `Repository[T,K]` seam, an in-memory dev store, and
  two generated shapes (GORM, ent) with tenant isolation built in.
- **Security checks** — the `seccheck` package proves, in CI, that authz is complete, unknown
  principals are denied, cross-account isolation holds, error messages are clean, and no
  secret field ever leaks in a response.

## Sections

{{< cards >}}
  {{< card link="getting-started/" title="Getting Started" icon="play" subtitle="Install the SDK and stand up a service in five minutes." >}}
  {{< card link="concepts/" title="Concepts" icon="light-bulb" subtitle="Architecture, the annotation contract, and the tenant-isolation model." >}}
  {{< card link="guides/" title="Guides" icon="book-open" subtitle="Task-focused how-tos: define a service, pick a storage shape, handle secrets, run seccheck, set up Vault." >}}
  {{< card link="reference/" title="Reference" icon="document-text" subtitle="API reference for each package and codegen plugin." >}}
  {{< card link="tutorial/" title="Tutorial" icon="academic-cap" subtitle="Build the API Key Manager service end to end." >}}
{{< /cards >}}
