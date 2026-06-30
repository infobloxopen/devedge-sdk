---
title: Architecture
weight: 1
---

devedge-sdk is the runtime layer of the contract-first developer experience. It sits between the proto
contract your team authors and the engine and policy backends your platform team operates. It translates what you declare in .proto
annotations into enforced behavior at runtime — authorization, secret handling, tenant scoping,
and persistence — without requiring you to wire those concerns by hand.

Use this page to understand how the SDK's layers relate to each other, how an inbound request
moves through the interceptor chain, and where generated code fits into that picture.

## Layers

![devedge-sdk architecture](../../../images/architecture.svg)

| Layer | What lives there | Who owns it |
|---|---|---|
| **1 — Contract** | the `.proto` files: resources, RPCs, `(infoblox.authz.v1.rule)` and `(infoblox.field.v1.opts).secret` annotations | the service team |
| **2 — SDK runtime** (this repo) | the interceptor chain, the authz seam, secret handling, the persistence seam + generated shapes, seccheck | shared, imported by every service |
| **3 — Backends** | the decision point (OPA/Cedar/remote PDP), the secret store (Vault), the database (Postgres) | platform / infra |

What you declare in layer 1 drives what runs in layer 2. Layer 3 backends are pluggable: every
seam ships a dev-suitable default and accepts a production backend without requiring any change to
service code.

## Dependencies and backends

- **Dependencies.** Core packages depend only on the standard library plus gRPC/protobuf.
  Transport and engine integrations live in clearly separated subpackages.
- **Engine neutrality.** The SDK has no internal coupling to any engine: it carries no
  policy-engine dependency (such as OPA), no ORM dependency, and no internal policy-model types.
  Policy-model and ORM types live in adapters built on the SDK, not in the SDK itself.
- **Fail-closed authorization.** Authorization denies by default: an undeclared method is denied,
  and the server can refuse to boot if any served method has no rule. See
  [Security posture](../../explanation/security-posture/#fail-closed-authorization) for how the
  boot-time gate and the runtime interceptor work.
- **In-process defaults.** Every seam is pluggable: it ships a dev-suitable default, not just an
  interface. The dev authorizer, the AES-256-GCM dev encryptor, and the in-memory repository all
  run in-process with zero external services, and each accepts a production backend without
  changing service code.

## Request path

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
ReadMask       apply the response read-mask (AIP-157 field selection)
   │
   ▼
ValidateOnly   short-circuit when validate_only is set (no mutation)
   │
   ▼
Deduplicate    idempotency replay for retried mutations
   │
   ▼
your handler   → Repository (tenant-scoped) → Encryptor (secret fields)
```

The same `[]authz.MethodRule` set drives both the authz interceptor (enforcement) and the
field-mask interceptor (verb lookup). It is also the source the permission catalog is built from,
so you declare the rules once and they are consumed in several places.

## Codegen

`buf` compiles the proto contract, and the SDK's plugins generate running code from it:

- **`protoc-gen-devedge-authz`** → the `<Service>AuthzRules` `[]MethodRule` table.
- **`protoc-gen-svc`** → a `Register<Service>` helper (boot-gate + gRPC/gateway registration); you write the handlers.
- **`protoc-gen-storage`** → a GORM-backed `Repository` with tenant scoping and secret columns.
- **`protoc-gen-ent`** → an ent schema with the same tenant/secret behavior via ent's privacy
  and hook layers.

See [Codegen reference](../../reference/codegen/).
