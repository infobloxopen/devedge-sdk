---
title: Architecture
weight: 1
---

devedge-sdk is the **runtime layer** of the Infoblox developer experience. It sits between the
proto contract a team authors and the engine/policy backends a platform team operates.

## Three layers

![devedge-sdk architecture](../../../images/architecture.svg)

| Layer | What lives there | Who owns it |
|---|---|---|
| **1 — Contract** | the `.proto` files: resources, RPCs, `(infoblox.authz.v1.rule)` and `(infoblox.authz.v1.field).secret` annotations | the service team |
| **2 — SDK runtime** (this repo) | the interceptor chain, the authz seam, secret handling, the persistence seam + generated shapes, seccheck | shared, imported by every service |
| **3 — Backends** | the decision point (OPA/Cedar/remote PDP), the secret store (Vault), the database (Postgres) | platform / infra |

The SDK's job is to make layer 1 (what a team declares) drive layer 2 (what runs), while
keeping layer 3 **pluggable** — every seam ships a dev-suitable default and swaps for a
production backend **without changing service code**.

## Design principles

- **Clean and dependency-light.** Core packages depend only on the standard library (plus
  gRPC/protobuf); transport and engine integrations live in clearly separated subpackages.
- **No internal coupling.** The SDK is engine-neutral: **no policy-engine dependency** (e.g.
  OPA), **no ORM dependency**, and no internal policy-model types. Those belong in adapters
  built *on* the SDK, not in it.
- **Fail closed.** Authorization denies by default; an undeclared method is denied, and the
  server can refuse to boot if any served method has no rule.
- **Pluggable, with a dev-suitable default.** The dev authorizer, the AES-256-GCM dev
  encryptor, and the in-memory repository all run in-process with zero external services.

## The request path

When a request reaches a `server.New`-built server, it flows through the interceptor chain
(outermost first):

```
incoming gRPC call
   │
   ▼
RequestID      attach/propagate a request id
   │
   ▼
ErrorMapper    convert internal errors to safe gRPC status codes
   │
   ▼
TenantID       read "account-id" from metadata onto ctx; set "cell-id: default"
   │
   ▼
grpcauthz      fail-closed authorization — deny unless a rule + grant allows
   │
   ▼
FieldMask      validate the request field mask against the method's verb
   │
   ▼
ETag/412       read If-Match precondition; write the response ETag trailer
   │
   ▼
your handler   → Repository (tenant-scoped) → Encryptor (secret fields)
```

The same `[]authz.MethodRule` set drives **both** the authz interceptor (enforcement) and the
field-mask interceptor (verb lookup), and is also what the permission catalog is built from —
declare once, consume in several places.

## Where codegen fits

The proto contract is compiled by `buf`, and the SDK's plugins turn it into running code:

- **`protoc-gen-devedge-authz`** → the `<Service>AuthzRules` `[]MethodRule` table.
- **`protoc-gen-svc`** → the service scaffold (handlers wired to a repository).
- **`protoc-gen-storage`** → a GORM-backed `Repository` with tenant scoping and secret columns.
- **`protoc-gen-ent`** → an ent schema with the same tenant/secret behavior via ent's privacy
  and hook layers.

See [Codegen reference](../../reference/codegen/).
