# devedge-sdk

[![CI](https://github.com/infobloxopen/devedge-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/infobloxopen/devedge-sdk/actions/workflows/ci.yml)
[![Docs](https://github.com/infobloxopen/devedge-sdk/actions/workflows/docs.yml/badge.svg)](https://infobloxopen.github.io/devedge-sdk/)
[![Go Reference](https://pkg.go.dev/badge/github.com/infobloxopen/devedge-sdk.svg)](https://pkg.go.dev/github.com/infobloxopen/devedge-sdk)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

A clean, pluggable Go framework for building Infoblox services. **Declare authorization
and secrets once in your proto** — the framework enforces them everywhere, refuses to boot
if any served method is undeclared, and never lets a secret field leak.

It is the runtime companion to [devedge](https://github.com/infobloxopen/devedge): devedge is
the **dev- and deploy-time** edge; `devedge-sdk` is the **runtime library** that production
services import.

> **Status: early.** APIs will change. Pin a version and read the [changelog/releases](https://github.com/infobloxopen/devedge-sdk/releases) before upgrading.

## 📚 Documentation

**Full docs live at [infobloxopen.github.io/devedge-sdk](https://infobloxopen.github.io/devedge-sdk/).**
Start here:

| Section | What's there |
|---|---|
| 🚀 [Getting Started](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/) | [Install](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/installation/) the SDK and stand up a service in five minutes ([Quickstart](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/quickstart/)). |
| 💡 [Concepts](https://infobloxopen.github.io/devedge-sdk/docs/concepts/) | The [architecture](https://infobloxopen.github.io/devedge-sdk/docs/concepts/architecture/), the [annotation contract](https://infobloxopen.github.io/devedge-sdk/docs/concepts/annotations/), and the [tenant-isolation model](https://infobloxopen.github.io/devedge-sdk/docs/concepts/tenant-isolation/). |
| 📖 [Guides](https://infobloxopen.github.io/devedge-sdk/docs/guides/) | Task how-tos: [define a service](https://infobloxopen.github.io/devedge-sdk/docs/guides/define-a-service/), [pick a storage shape](https://infobloxopen.github.io/devedge-sdk/docs/guides/storage-shapes/), [handle secrets](https://infobloxopen.github.io/devedge-sdk/docs/guides/secret-fields/), [run seccheck](https://infobloxopen.github.io/devedge-sdk/docs/guides/security-check/), [set up Vault](https://infobloxopen.github.io/devedge-sdk/docs/guides/vault-transit/). |
| 📑 [Reference](https://infobloxopen.github.io/devedge-sdk/docs/reference/) | Per-package API reference and codegen-plugin docs. |
| 🎓 [Tutorial](https://infobloxopen.github.io/devedge-sdk/docs/tutorial/api-key-manager/) | Build the **API Key Manager** service end to end. |

## Why devedge-sdk

- **Declare authz once, enforced everywhere.** Annotate an RPC with `(infoblox.authz.v1.rule)`.
  The framework builds the per-method rule table, enforces it **fail-closed**, and refuses to
  boot if any served method is undeclared.
- **Secret fields encrypted at rest.** Mark a field `secret` in proto; generated code hashes it
  for lookup and encrypts the ciphertext — AES-256-GCM in dev, HashiCorp Vault Transit in prod.
  Plaintext is never persisted and never returned.
- **Cross-account tenant isolation.** Every query is scoped by `account-id` at the storage layer
  (GORM *and* ent). One principal can never see another's resources — and `seccheck` proves it in CI.
- **Batteries-included gRPC server.** `server.New` assembles the interceptor chain — request-ID,
  error mapping, tenant-ID, fail-closed authz, field-mask validation, ETag/412 preconditions —
  plus an optional HTTP/JSON gateway.
- **Codegen from your proto.** `protoc-gen-svc` scaffolds the service, `protoc-gen-storage` emits a
  GORM repository, `protoc-gen-ent` emits an ent schema. The proto is the single source of truth.
- **Pluggable, dependency-light core.** Core packages depend only on the standard library — no ORM,
  no policy-engine dependency. Every seam ships a dev default and swaps for a production backend
  **without touching service code**.

## Install

```bash
go get github.com/infobloxopen/devedge-sdk@latest
```

Install the codegen plugins onto your `PATH` so `buf generate` can invoke them:

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-storage@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-ent@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
```

Requires **Go 1.25+** and **[buf](https://buf.build)**. Postgres and Vault are only needed for the
production storage and secret backends — the in-memory store and dev encryptor run in-process. Full
prerequisites: [Installation](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/installation/).

## Quickstart

**1. Declare each RPC's authz requirement in proto** — verb + resource is all a method needs:

```proto
service WidgetService {
  rpc GetWidget(GetWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = {verb: "get", resource: "widget:{id}"};
  }
  rpc CreateWidget(CreateWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "widget"};
  }
}
```

**2. Generate** (`buf generate`) — `protoc-gen-devedge-authz` emits a `WidgetServiceAuthzRules`
table next to the `.pb.go`.

**3. Wire the server.** The `Authorizer` defaults to **default-deny**, so every call is denied
until you grant something — fail-closed by construction:

```go
srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    HTTPAddr: ":8080", // optional HTTP/JSON gateway; omit to run gRPC-only
    Rules:    widgetv1.WidgetServiceAuthzRules, // generated in step 2

    // Dev decision point — grant group:admin everything. Swap for an
    // OPA/Cedar/remote Authorizer in production; nothing else changes.
    Authorizer: authz.NewDevAuthorizer(authz.Grant{
        Tenant: "t1", Subjects: []string{"group:admin"},
        Verbs: []authz.Verb{"*"}, Resource: "*",
    }),
    // Derive the principal from request metadata (account-id → tenant, groups →
    // group:<name>). Use a verified-token func in production.
    PrincipalFunc: grpcauthz.DevPrincipalFunc(),
})
if err != nil {
    log.Fatal(err)
}

widgetv1.RegisterWidgetServiceServer(srv.GRPCServer(), &widgetServer{})
log.Fatal(srv.Serve(ctx)) // blocks until ctx is cancelled
```

The chain `server.New` builds, outermost first:

```
RequestID → ErrorMapper → TenantID → grpcauthz (fail-closed) → FieldMask → ETag/412 → ReadMask → ValidateOnly → Deduplicate
```

→ Full walkthrough with tests: [Quickstart](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/quickstart/).

## Packages

| Package | What it provides |
|---|---|
| [`authz`](./authz) | Engine-neutral model: `Principal`, `Resource`, `Verb`, `AccessRequest`, `Decision`, the pluggable `Authorizer`, and `DevAuthorizer` (in-process, default-deny). |
| [`authz/grpcauthz`](./authz/grpcauthz) | Fail-closed gRPC interceptor + boot gate. Rough-compatible with `atlas-authz-middleware/grpc_opa` ([COMPAT.md](./COMPAT.md)). |
| [`authz/catalog`](./authz/catalog) | Builds the **permission catalog** (per resource: verbs, endpoints, `View`/`Manage` groups) from declared rules. |
| [`authz/authzpb`](./authz/authzpb) | Reflection-based rule extractor — reads `(infoblox.authz.v1.rule)` off linked descriptors, no generated file. |
| [`server`](./server) | `Server` lifecycle: gRPC + optional HTTP/JSON gateway, interceptor chain auto-wired. |
| [`middleware`](./middleware) | The interceptors: `RequestID`, `TenantID`, `FieldMask`, `ErrorMapper`, `ValidateOnly`, `Dedup`, and [`etag`](./middleware/etag). |
| [`secret`](./secret) | Secret-at-rest `Encryptor`: AES-256-GCM + HMAC for dev, HashiCorp Vault Transit for prod. |
| [`persistence`](./persistence) | ORM-free `Repository[T,K]` seam, in-memory dev store, DSN hotload, filtering. Storage *shape* is per-service ([SHAPES.md](./persistence/SHAPES.md)). |
| [`lro`](./lro) | AIP-151/152 long-running operations: `Store`, `Manager`, `Operation`, cancellation. |
| [`seccheck`](./seccheck) | Static + dynamic security assertions you run in CI (see [Security model](#security-model)). |

Full API reference for every package: **[Reference docs ↗](https://infobloxopen.github.io/devedge-sdk/docs/reference/)**.

### Codegen plugins

The proto is the single source of truth; `make generate` (or `buf generate`) drives these:

| Plugin (`cmd/…`) | Output |
|---|---|
| [`protoc-gen-svc`](./cmd/protoc-gen-svc) | service scaffold (`*.svc.go`) |
| [`protoc-gen-storage`](./cmd/protoc-gen-storage) | GORM-backed `Repository` (`*.storage.go`) |
| [`protoc-gen-ent`](./cmd/protoc-gen-ent) | ent schema (`ent/schema/*.go`) |
| [`protoc-gen-devedge-authz`](./cmd/protoc-gen-devedge-authz) | the `<Service>AuthzRules` `[]MethodRule` table (`*.authz.go`) |

## Security model

Authorization is **fail-closed**: an undeclared or ungranted method is denied with no code required.
The `seccheck` package turns the model's invariants into assertions you run in CI (`make security-check`):

| Invariant | Asserted by |
|---|---|
| Every RPC has `(infoblox.authz.v1.rule)` or `public: true` | `AssertRulesComplete` |
| No secret-annotated field value in a read/list response | `AssertNoSecretFieldsLeaked` |
| Unknown principal → `PermissionDenied` on all RPCs | `AssertUnknownPrincipalDenied` |
| Account A cannot read Account B's resources | `AssertCrossAccountIsolation` |
| Errors never leak SQL, stack traces, or hostnames | `AssertErrorMessagesClean` |

→ [Security Check guide](https://infobloxopen.github.io/devedge-sdk/docs/guides/security-check/).

### Swapping the decision point

`WithAuthorizer` / `Config.Authorizer` takes any `authz.Authorizer`. To target a production engine,
implement the one-method interface — an OPA-backed authorizer calling a sidecar, a Cedar/OpenFGA
client, a remote PDP — and pass it in. Nothing else in the service changes. The SDK core stays
engine-neutral: **no OPA, no ORM, no policy-model types** — those belong in adapters built *on* the SDK.

## Building from source

```bash
make build            # go build ./...
make test             # unit tests (root module)
make vet              # go vet ./...
make lint             # golangci-lint if installed, else go vet
make generate         # rebuild plugins + regenerate after any .proto change
make security-check   # run the seccheck assertions
```

`testdata/toy`, `testdata/apikey`, and `testdata/fleet` are separate modules with their own
integration tests — run them with `cd testdata/<name> && go test ./...`. See [AGENTS.md](./AGENTS.md)
for the full contributor guide.

## License

Apache-2.0. See [LICENSE](./LICENSE).
