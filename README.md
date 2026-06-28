# devedge-sdk

[![CI](https://github.com/infobloxopen/devedge-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/infobloxopen/devedge-sdk/actions/workflows/ci.yml)
[![Docs](https://github.com/infobloxopen/devedge-sdk/actions/workflows/docs.yml/badge.svg)](https://infobloxopen.github.io/devedge-sdk/)
[![Go Reference](https://pkg.go.dev/badge/github.com/infobloxopen/devedge-sdk.svg)](https://pkg.go.dev/github.com/infobloxopen/devedge-sdk)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

**A complete, secure-by-default Infoblox microservice from a single proto.** Define a resource and its
methods in proto; devedge-sdk gives you a running gRPC + REST service with Google-AIP semantics,
fail-closed authorization, multi-tenant isolation, and a persistence/eventing layer wired in — every
backend a swappable seam, every security invariant provable in CI.

It is the runtime companion to [devedge](https://github.com/infobloxopen/devedge): devedge is the
**dev- and deploy-time** edge; `devedge-sdk` is the **runtime library** that production services import.

> **Status: early.** APIs will change. Pin a version and read the [changelog/releases](https://github.com/infobloxopen/devedge-sdk/releases) before upgrading.
> The operational baseline is still filling in — built-in **observability** and **health/readiness
> probes** are tracked on the [foundation roadmap (#97)](https://github.com/infobloxopen/devedge-sdk/issues/97).

## 📚 Documentation

**Full docs live at [infobloxopen.github.io/devedge-sdk](https://infobloxopen.github.io/devedge-sdk/).**
Start here:

| Section | What's there |
|---|---|
| 🚀 [Getting Started](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/) | [Install](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/installation/) the SDK and stand up a running, fail-closed service in five minutes ([Quickstart](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/quickstart/)). |
| 💡 [Concepts](https://infobloxopen.github.io/devedge-sdk/docs/concepts/) | The [architecture](https://infobloxopen.github.io/devedge-sdk/docs/concepts/architecture/) and seam model, the [annotation contract](https://infobloxopen.github.io/devedge-sdk/docs/concepts/annotations/), [tenant isolation](https://infobloxopen.github.io/devedge-sdk/docs/concepts/tenant-isolation/), [aggregates](https://infobloxopen.github.io/devedge-sdk/docs/concepts/aggregates/), and [events](https://infobloxopen.github.io/devedge-sdk/docs/concepts/events/). |
| 📖 [Guides](https://infobloxopen.github.io/devedge-sdk/docs/guides/) | Task how-tos: [define a service](https://infobloxopen.github.io/devedge-sdk/docs/guides/define-a-service/), [model a resource](https://infobloxopen.github.io/devedge-sdk/docs/guides/model-a-resource/), [pick a storage shape](https://infobloxopen.github.io/devedge-sdk/docs/guides/storage-shapes/), [run seccheck](https://infobloxopen.github.io/devedge-sdk/docs/guides/security-check/). |
| 📑 [Reference](https://infobloxopen.github.io/devedge-sdk/docs/reference/) | Per-package API reference and codegen-plugin docs. |
| 🎓 [Tutorial](https://infobloxopen.github.io/devedge-sdk/docs/tutorial/api-key-manager/) | Build the **API Key Manager** service end to end. |

## Why devedge-sdk

- **From one proto to a running, AIP-correct service.** Scaffold a service, declare your resource and
  methods, and `server.New` stands up gRPC **plus** an optional HTTP/JSON gateway with the standard
  methods and Google-AIP semantics already wired: field-mask `PATCH` (AIP-134), ETag/`If-Match`→412
  optimistic concurrency (AIP-154), pagination, filtering (AIP-160), soft-delete + undelete
  (AIP-148/149), batch methods (AIP-137), request de-duplication (AIP-155), and long-running operations
  (AIP-151/152). Correct API semantics, for free.
- **Secure by default — and provable.** Authorization is **fail-closed**: annotate each RPC with
  `(infoblox.authz.v1.rule)` and the service *refuses to boot* if any served method is undeclared.
  Every query is scoped by `account-id` at the storage layer (GORM **and** ent), so one principal can
  never see another's resources; secret-annotated fields are encrypted at rest and never returned; and
  errors never leak SQL or stack traces. The `seccheck` package proves all of it in CI.
- **Pluggable seams, dev defaults, zero service-code change.** Persistence (in-memory → GORM/ent),
  transactions, **domain events** (in-memory → Kafka, via a transactional outbox), the authz decision
  point, and the secret encryptor each ship a dev-suitable default and swap for a production backend
  **without touching service code**. DDD **aggregates** and multi-surface projections are first-class.
- **Codegen from your proto; dependency-light core.** `protoc-gen-svc` scaffolds the service,
  `protoc-gen-storage` emits a GORM repository, `protoc-gen-ent` emits an ent schema, and
  `protoc-gen-devedge-authz` emits the authz-rules table — the proto is the single source of truth.
  Core packages depend only on the standard library: **no ORM, no policy-engine dependency**.

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

In about five minutes you go from a `.proto` to a running, fail-closed gRPC + REST service.

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

You now have a running, fail-closed service speaking gRPC and REST. The chain `server.New` builds,
outermost first:

```
RequestID → ErrorMapper → TenantID → grpcauthz (fail-closed) → FieldMask → ETag/412 → ReadMask → ValidateOnly → Deduplicate
```

→ Full walkthrough with tests: [Quickstart](https://infobloxopen.github.io/devedge-sdk/docs/getting-started/quickstart/).

## Packages

| Package | What it provides |
|---|---|
| [`server`](./server) | `Server` lifecycle: gRPC + optional HTTP/JSON gateway, the interceptor chain auto-wired, graceful shutdown. |
| [`middleware`](./middleware) | The interceptors: `RequestID`, `TenantID`, `FieldMask`, `ErrorMapper`, `ValidateOnly`, `Dedup`, and [`etag`](./middleware/etag). |
| [`authz`](./authz) | Engine-neutral model: `Principal`, `Resource`, `Verb`, `AccessRequest`, `Decision`, the pluggable `Authorizer`, and `DevAuthorizer` (in-process, default-deny). |
| [`authz/grpcauthz`](./authz/grpcauthz) | Fail-closed gRPC interceptor + boot gate. Rough-compatible with `atlas-authz-middleware/grpc_opa` ([COMPAT.md](./COMPAT.md)). |
| [`authz/catalog`](./authz/catalog) | Builds the **permission catalog** (per resource: verbs, endpoints, `View`/`Manage` groups) from declared rules. |
| [`authz/authzpb`](./authz/authzpb) | Reflection-based rule extractor — reads `(infoblox.authz.v1.rule)` off linked descriptors, no generated file. |
| [`persistence`](./persistence) | ORM-free `Repository[T,K]` seam, in-memory dev store, transactions, batch, DSN hotload, filtering, and **aggregate** roots. Storage *shape* is per-service ([SHAPES.md](./persistence/SHAPES.md)). |
| [`events`](./events) | Domain events via a **transactional outbox** — `Publisher`/`Bus`/idempotency, an in-memory bus, and a [Kafka](./events/kafkabus) bus, for safe cross-aggregate reactions. |
| [`secret`](./secret) | Secret-at-rest `Encryptor`: AES-256-GCM + HMAC for dev, HashiCorp Vault Transit for prod. |
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

Security is a **foundation property here, not a bolt-on**: it is enforced by construction and verified
in CI, so a correctly-built service is secure by default.

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

**Secret fields.** Mark a field `secret` in proto; generated code hashes it for lookup and encrypts the
ciphertext — AES-256-GCM in dev, HashiCorp Vault Transit in production — so plaintext is never persisted
and never returned. See [model a resource → secret fields](https://infobloxopen.github.io/devedge-sdk/docs/guides/model-a-resource/#secret-fields)
and the [Vault Transit guide](https://infobloxopen.github.io/devedge-sdk/docs/guides/vault-transit/).

### Swapping the decision point

`WithAuthorizer` / `Config.Authorizer` takes any `authz.Authorizer`. To target a production engine,
implement the one-method interface — an OPA-backed authorizer calling a sidecar, a Cedar/OpenFGA
client, a remote PDP — and pass it in. Nothing else in the service changes. The SDK core stays
engine-neutral: **no OPA, no ORM, no policy-model types** — those belong in adapters built *on* the SDK.

## Building from source

This repo is a **multi-module workspace** (WS-011 / F039): the dep-light root library plus six
nested modules (`cmd`, `config/koanf`, `events/kafkabus`, `observability/otel`, `persistence/gormtx`,
`persistence/entrepo`). A committed `go.work` resolves the cross-module references locally; the
build/vet/test targets loop over every module (the `MODULES` list in the Makefile).

```bash
make build                 # build every module (via go.work)
make test                  # unit tests across every module
make vet                   # go vet across every module
make lint                  # golangci-lint if installed, else go vet
make build-gowork-off      # build each module with the workspace OFF (real requires only)
make check-graph-isolation # prove a server-only consumer's graph is free of the heavy adapter deps
make generate              # rebuild plugins + regenerate after any .proto change
make security-check        # run the seccheck assertions
make release VERSION=vX.Y.Z   # synchronized multi-module release (dry run; PUSH=1 to publish)
```

`testdata/toy`, `testdata/apikey`, `testdata/fleet`, and `testdata/iam` are separate consumer modules
with their own integration tests, deliberately NOT in `go.work` — run them with
`cd testdata/<name> && GOWORK=off go test ./...`. To carve a future heavy component into its own
isolated module, follow the [Adding an Isolated Module](docs/content/docs/explanation/adding-an-isolated-module.md)
checklist. See [AGENTS.md](./AGENTS.md) for the full contributor guide.

## License

Apache-2.0. See [LICENSE](./LICENSE).
