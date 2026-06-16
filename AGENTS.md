# devedge-sdk — Agent guide

## Commands

```
make build             # compile all root packages
make test              # unit tests (root module only)
make vet               # go vet ./...
make lint              # golangci-lint or go vet
make generate          # rebuild generated files after any .proto change
make security-check    # security assertions against toy fixture
```

`testdata/toy` and `testdata/apikey` are separate Go modules — `make test` does **not** cover them.
Run `cd testdata/toy && go test ./...` after any proto or middleware change.

## Layout

| Path | What lives here |
|------|----------------|
| `authz/` | Engine-neutral model: `Principal`, `Verb`, `Resource`, `Authorizer`, `DevAuthorizer` |
| `authz/grpcauthz/` | Fail-closed gRPC interceptor (boot-gate + per-call deny) |
| `authz/catalog/` | Permission catalog builder (feeds PARG generators and UI) |
| `authz/authzpb/` | Reflection-based rule extractor (no generated file needed) |
| `lro/` | AIP-151/152 long-running operations: `Store`, `Manager`, cancellation |
| `middleware/` | `RequestID`, `TenantID`, `FieldMask`, `ErrorMapper`, `ValidateOnly`, `Dedup` |
| `middleware/etag/` | ETag + `If-Match` → 412/428 |
| `persistence/` | `Repository[T,K]` seam, `MemoryRepository`, DSN hotload, filter, resourcename |
| `seccheck/` | Static + dynamic §3.5 security assertions |
| `secret/` | AES-256-GCM + HMAC-SHA256 encryptor; Vault Transit adapter |
| `server/` | `Server` lifecycle (gRPC + HTTP gateway, interceptor chain auto-wired) |
| `cmd/protoc-gen-svc` | Generates `<Service>Server` handler interface + `Register<Svc>` |
| `cmd/protoc-gen-storage` | Generates GORM model + `Repository[*T, string]` from a proto resource |
| `cmd/protoc-gen-ent` | Generates ent schema from a proto resource |
| `cmd/protoc-gen-devedge-authz` | Generates `<Service>AuthzRules` table from proto annotations |
| `testdata/toy/` | Toy `WidgetService` — the integration test and security-gate target (own `go.mod`) |
| `testdata/apikey/` | APIKey fixture with GORM + ent shapes (own `go.mod`) |

## Always / Ask first / Never

**Always:**
- Edit `.proto` first; run `make generate` before touching generated Go
- Add `(infoblox.authz.v1.rule)` to every new RPC before writing its handler
- Run `cd testdata/toy && go test ./...` after any middleware or server change
- Run `make security-check` before committing changes to `seccheck/`, `middleware/`, or `server/`

**Ask first:**
- Adding a direct dependency to root `go.mod` — the core must stay lean
- Changing `authz.MethodRule`, `authz.Verb`, any `Store` interface, or any `Repository` method signature (wire-compat break; update callers before merging)
- Removing or renaming an exported type in `authz/`, `lro/`, `middleware/`, `persistence/`, or `server/`
- Adding a new proto annotation to the canonical schema (`infobloxopen/apis` via apx) — requires an extension-number reservation and a release through both canonical repos

**Never:**
- Edit `*.pb.go`, `*.svc.go`, `*.storage.go`, `*.pb.gw.go`, `*.authz.go` — fix the proto or the plugin, then re-run `make generate`
- Import OPA, GORM, or any ORM/policy-engine in `authz/`, `authz/grpcauthz/`, or `persistence/` — those packages are the engine-neutral core; adapters live outside the module
- Return a `[secret]`-annotated field value from a List or Get response
- Add a per-method interceptor by hand — use proto annotations to drive middleware wiring (that is the architecture's reason for being)
- Grant a `public: true` authz exemption without a code review

## Annotation contract

Two annotations ship with the SDK, registered in `github.com/infobloxopen/apis`:

| Annotation | Path | Purpose |
|-----------|------|---------|
| `(infoblox.authz.v1.rule)` | `proto/infoblox/authz/v1/authz.proto` | `{verb, resource, public}` on each service method — feeds enforcement, the permission catalog, and PARG generators |
| `(infoblox.field.v1.rule)` | `proto/infoblox/field/v1/field.proto` | `{secret}` on message fields — drives log redaction and seccheck |

Extension numbers: `50001` (authz rule), `50002` (field rule) — both placeholder; reserve before broad adoption.

Canonical Go import: `github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.2`

The local `proto/` copy is a **buf codegen input only** — the canonical module is the single protoregistry registration. Never import both (two copies panic at `init`).

## Generated-file policy

Files with `// Code generated ... DO NOT EDIT.` and a `.pb.`, `.svc.`, `.storage.`, `.authz.`, or `.pb.gw.` infix are generated. If a generated file is wrong: fix the `.proto` or the plugin under `cmd/`, then re-run `make generate`.

## Core-cleanliness invariant

`authz/`, `authz/grpcauthz/`, and `persistence/` must have zero imports of OPA, GORM, or any policy/ORM engine. Engine-specific adapters live outside this module (e.g. `Infoblox-CTO/devedge-sdk-internal`). Enforce via `go mod graph` if unsure.

## Security invariants (enforced by `seccheck` and `make security-check`)

| Invariant | Enforced by |
|-----------|-------------|
| Every RPC has `(infoblox.authz.v1.rule)` or `public: true` | `seccheck.AssertRulesComplete` |
| No secret-annotated field value in a read/list response | `seccheck.AssertNoSecretFieldsLeaked` |
| Unknown principal → `PermissionDenied` on all RPCs | `seccheck.AssertUnknownPrincipalDenied` |
| Account A cannot read Account B's resources | `seccheck.AssertCrossAccountIsolation` |
| Errors never leak SQL, stack traces, or hostnames | `seccheck.AssertErrorMessagesClean` |
