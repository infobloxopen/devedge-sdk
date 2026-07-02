# F041 — Tasks (Plan)

WS-021 P1 / WP-B. Each task tagged `[S]` (mechanical) or `[C]` (complex). Implement
top-to-bottom; each phase's gate is green before the next. No back-compat — clean
implementation (the SDK is pre-1.0). Locked decisions D-1/D-3/D-4/D-7 in `spec.md`.

Ground truth confirmed in code: `google.api.resource_reference` (AIP-124) is a
`FieldOptions` extension (`apiannotations.E_ResourceReference` → `*ResourceReference`
with `GetType()`), already importable via the `google/api/resource.proto` import the
fixtures use. The generated ent/GORM/Memory repositories are already full
`persistence.BatchRepository` (they always emit `BatchGet`); the gap G-2 addresses is
the **handler/RPC surface** — the F029 CRUD handler classifies only the 6 standard
methods, leaving `BatchGet` hand-written (see `testdata/toy/server_test.go`).

## Phase 1 — recognize the annotation + emit `<Svc>References` metadata (AC-1)

- T-101 `[C]` In `cmd/protoc-gen-svc/main.go`, read `apiannotations.E_ResourceReference`
  on each **scalar string FK field** of a **resource message** (a message carrying
  `google.api.resource`). Build a per-service `[]referenceInfo{ FieldGoName, FieldProto,
  TargetType, FKField, Cardinality }` (cardinality = list vs single per `field.Desc.IsList()`).
  Coverage guard (spec failure mode): a `resource_reference` on a **non-resource message**
  or on a **message-typed field** is a `gen.Error` (fail-loud codegen), never silently
  ignored. Resolve which message a service's references live on via the already-detected
  `svc.Resource`.
- T-102 `[C]` In `cmd/protoc-gen-svc/render.go`, emit a generated `var <Svc>References =
  []reference.Reference{ ... }` table (DO NOT EDIT), mirroring the `<Svc>AuthzRules`
  emission style. `reference.Reference` is the ROOT-module seam type (T-301). Emit nothing
  when a service has no references. Wire it into the servicekit `Descriptor` (additive
  field) so a host can introspect references — mirrors `AuthzRules: <Svc>AuthzRules`.
- T-103 `[S]` `render_test.go`: a service with a `resource_reference` FK field →
  `<Svc>References` present, valid Go (`go/format.Source`), correct TargetType/FKField/
  cardinality; a service with no references → no table. `main_test.go` (or render): the
  coverage guard errors on a reference on a non-resource / message-typed field.

## Phase 2 — the `ReferenceResolver` seam + `reference.Reference` type (D-7, ROOT module, stdlib-only) (AC-5 substrate)

- T-201 `[C]` New package `reference/` in the ROOT module, **stdlib-only** (keeps
  `check-graph-isolation` green): `Reference{ FieldName, TargetType, FKField, Cardinality }`
  (the metadata type the generator emits) + `Cardinality` enum; `BatchGetter[T]` interface
  (`BatchGet(ctx, ids []string) ([]T, error)` — the batch-fetch capability a target client
  must expose); `ReferenceResolver` interface (`ResolverFor(targetType string) (any, bool)`
  → a BatchGet-capable client); `StaticResolver` in-process impl (`map[string]any`,
  register a client per target type). A generic `Load[T]` helper that DataLoader-style
  dedups the FK values of N parents and issues **exactly one** `BatchGet` for the distinct
  set (the anti-N+1 primitive AC-5 asserts).
- T-202 `[S]` `reference/reference_test.go`: `StaticResolver` round-trip; `Load` issues one
  BatchGet for N parents (a counting `BatchGetter` asserts call-count == 1); missing target
  type → clear error.

## Phase 3 — guarantee `BatchGet` on reference-target resources (G-2/G-3, D-3/D-4) (AC-2/AC-3)

- T-301 `[C]` `cmd/protoc-gen-svc`: classify a `BatchGet<R>` RPC (new `stdBatchGet`:
  name-prefix `BatchGet`, request has `repeated string ids`/`names`, response has repeated
  resource) and **auto-generate the handler method** delegating to the repo's `BatchGet`
  (AIP-137). The CRUD handler's `Repo` field becomes `persistence.BatchRepository[*<R>,
  string]` **iff** the service has a `stdBatchGet` method (so passing a non-batch repo is a
  compile error — the codegen-time half of D-4); `New<Svc>Handler` /
  `Register<Svc>WithRepository` take `BatchRepository` in that case. Emit the response over
  the detected repeated-resource field + honor read_mask via the existing interceptor (no
  handler change needed — `ReadMaskUnary` already applies).
- T-302 `[C]` **Fail-loud gate (D-4 backstop) at `Serve`**: record each service's references
  (`server.RecordReferences`, mirroring `RecordMemberBinding`) + which resource types serve
  `BatchGet` (from the registered method set / a `BatchTargets` record). New
  `server/reference_gate.go`: `AssertReferenceTargets(targets, references)` errors when a
  referenced target type has no registered `BatchGet` method — clear, actionable message,
  fail-closed, pure function (unit-testable), run in `Serve` beside
  `AssertAggregateBoundaries`. The generated `Register<Svc>` contributes both records.
- T-303 `[C]` `render_test.go` + `server/reference_gate_test.go`: `stdBatchGet` handler
  emitted + `Repo` is `BatchRepository` (valid Go); the gate passes when a referenced target
  registers BatchGet and **errors** when it does not (AC-3 unit half).

## Phase 4 — the two-service cross-service fixture, ent AND GORM (AC-4/AC-5)

- T-401 `[C]` New fixture module `testdata/federation/`: two proto packages / services —
  **service B** `region.v1` (`Region` resource, target; serves `GetRegion` + `BatchGetRegions`
  + List; `read` rule) and **service A** `asset.v1` (`Asset` resource with a scalar
  `region_id` FK carrying `(google.api.resource_reference) = {type: "region.example.com/Region"}`;
  serves Get/List/Create). Both generate the FULL backend set (go/grpc/authz/svc/storage/ent)
  + `buf.gen.federation.yaml` + entry in `buf.yaml` + `Makefile generate`. ent client via
  `go generate ./ent`. This is a real cross-service reference on a scalar FK (the CROSS-SERVICE
  ref, distinct from iam's local `ddd.v1.references`).
- T-402 `[C]` `testdata/federation/.../composition_test.go` (AC-5 keystone, table over
  {ent, GORM}): boot BOTH services on one server (or two), Create N Assets referencing M
  distinct Regions; a composition (a) Lists N Assets, (b) resolves their `region_id`
  references via `reference.Load` + a `StaticResolver` wired to the Region `BatchGet` client,
  asserting **exactly ONE** BatchGetRegions call for the N refs (a counting wrapper), honoring
  read_mask + per-service `read` authz. A per-row resolver variant must fail the count assert.
- T-403 `[S]` AC-4 assertion: a test/inspection proving the `region_id` reference stays a
  **scalar FK** — no traversable Go edge on `Asset` into `Region`, no cascade (parity with
  `ddd.v1.references`); the generated ent schema for `Asset` has no `Region` edge.

## Phase 5 — docs + full gate (AC-6)

- T-501 `[S]` `docs/content/docs/reference/codegen.md`: the cross-service reference annotation
  (`google.api.resource_reference` on a scalar FK), the emitted `<Svc>References` metadata, the
  BatchGet guarantee on referenced targets, the fail-loud rule (codegen + Serve), and the
  write-boundary invariant (metadata only, scalar FK, no edge/cascade). Reference the
  `reference` package seam.
- T-502 `[S]` Full gates green: root `go build ./...`, `go vet ./...`, `gofmt`, `go test ./...`;
  `make generate` (regenerate all fixtures, no unexpected drift); `testdata/{toy,apikey,fleet,
  iam,federation}` `go test ./...`; `make security-check`; `make check-graph-isolation`; and
  `GOWORK=off go build ./...` (root) matching release-verify discipline.

## Verify

- Functional: build/vet/gofmt/test green; AC-5 composition asserts a single BatchGet on ent
  AND GORM with real behavior; `check-graph-isolation` green (new `reference` pkg is
  stdlib-only); GOWORK=off root build green.
- Scope: every change traces to an F041 goal/AC/task. OUT (do not build): REST `?expand=`
  (P2), GraphQL, the catalog-backed resolver (separate WP), any Go graph edge / cross-aggregate
  mutation.
</content>
