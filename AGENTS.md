# devedge-sdk — Agent guide

## Commands

```
make build                 # build every module (root + 6 nested) via go.work
make test                  # unit tests across every module
make vet                   # go vet across every module
make lint                  # golangci-lint or go vet
make build-gowork-off      # build each module with the workspace OFF (real requires only)
make check-graph-isolation # prove a server-only consumer's graph is free of the heavy adapter deps
make generate              # rebuild generated files after any .proto change
make security-check        # security assertions against toy fixture
make release VERSION=vX.Y.Z   # synchronized multi-module release (dry run; PUSH=1 to publish)
```

This is a **multi-module repo** (WS-011 / F039): the dep-light root library plus six nested modules
(`cmd`, `config/koanf`, `events/kafkabus`, `observability/otel`, `persistence/gormtx`,
`persistence/entrepo`). A committed `go.work` resolves cross-module refs locally; the build/vet/test
targets loop over the `MODULES` list. The `testdata/*` fixtures are separate consumer modules NOT in
`go.work` — `make test` does **not** cover them. Run `cd testdata/toy && GOWORK=off go test ./...`
after any proto or middleware change. To carve a future heavy component into its own module, see
`docs/content/docs/explanation/adding-an-isolated-module.md`.

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
| `cmd/` | The CLI + `protoc-gen-*` plugins — its OWN module (may import ent/gorm freely; kept out of the root library graph) |
| `cmd/protoc-gen-svc` | Generates `<Service>Server` handler interface + `Register<Svc>` |
| `cmd/protoc-gen-storage` | Generates GORM model + `Repository[*T, string]` from a proto resource |
| `cmd/protoc-gen-ent` | Generates ent schema from a proto resource |
| `cmd/protoc-gen-devedge-authz` | Generates `<Service>AuthzRules` table from proto annotations |
| `observability/otel/` | Nested module: OTel SDK + exporters (the only package that imports them) |
| `config/koanf/` | Nested module: the koanf-backed config adapter |
| `events/kafkabus/` | Nested module: the franz-go Kafka adapter |
| `persistence/gormtx/` | Nested module: the gorm transaction runner + outbox (gorm lives here, not in core) |
| `persistence/entrepo/` | Nested module: the ent-backed `Repository` adapter (ent lives here, not in core) |
| `testdata/{toy,apikey,fleet,iam}/` | Consumer fixtures — separate modules with own `go.mod` + `replace`, NOT in `go.work` |

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
- Import OPA, GORM, ent, or any ORM/policy-engine in `authz/`, `authz/grpcauthz/`, or `persistence/` — those packages are the engine-neutral core. The gorm/ent adapters live in their own nested modules (`persistence/gormtx`, `persistence/entrepo`), so a core import of them is a cross-module **compile error**; other engine adapters live outside the repo (e.g. `Infoblox-CTO/devedge-sdk-internal`).
- Return a `[secret]`-annotated field value from a List or Get response
- Add a per-method interceptor by hand — use proto annotations to drive middleware wiring (that is the architecture's reason for being)
- Grant a `public: true` authz exemption without a code review

## Annotation contract

Two annotations ship with the SDK, registered in `github.com/infobloxopen/apis`:

| Annotation | Path | Purpose |
|-----------|------|---------|
| `(infoblox.authz.v1.rule)` | `proto/infoblox/authz/v1/authz.proto` | `{verb, resource, public}` on each service method — feeds enforcement, the permission catalog, and PARG generators |
| `(infoblox.field.v1.opts)` | `proto/infoblox/field/v1/field.proto` | `{secret: true}` on message fields — drives log redaction and seccheck |

Extension numbers: `50001` (authz rule), `50003` (field opts) — both placeholder; reserve before broad adoption.

Canonical Go imports (versions per `go.mod`): `github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.4`
and `github.com/infobloxopen/apis/proto/infoblox/field@v1.0.0-alpha.1`

The local `proto/` copy is a **buf codegen input only** — the canonical module is the single protoregistry registration. Never import both (two copies panic at `init`).

## Generated-file policy

Files with `// Code generated ... DO NOT EDIT.` and a `.pb.`, `.svc.`, `.storage.`, `.authz.`, or `.pb.gw.` infix are generated. If a generated file is wrong: fix the `.proto` or the plugin under `cmd/`, then re-run `make generate`.

## Core-cleanliness invariant

`authz/`, `authz/grpcauthz/`, and `persistence/` must have zero imports of OPA, GORM, ent, or any policy/ORM engine. The gorm/ent adapters are now their OWN nested modules (`persistence/gormtx`, `persistence/entrepo`), so a core import of them fails to compile across the module boundary — `make build-gowork-off` and the `cleancore_test.go` guards enforce it; other engine adapters live outside the repo (e.g. `Infoblox-CTO/devedge-sdk-internal`). Enforce via `go mod graph` if unsure.

## Security invariants (enforced by `seccheck` and `make security-check`)

| Invariant | Enforced by |
|-----------|-------------|
| Every RPC has `(infoblox.authz.v1.rule)` or `public: true` | `seccheck.AssertRulesComplete` |
| No secret-annotated field value in a read/list response | `seccheck.AssertNoSecretFieldsLeaked` |
| Unknown principal → `PermissionDenied` on all RPCs | `seccheck.AssertUnknownPrincipalDenied` |
| Account A cannot read Account B's resources | `seccheck.AssertCrossAccountIsolation` |
| Errors never leak SQL, stack traces, or hostnames | `seccheck.AssertErrorMessagesClean` |
