# Implementation Plan: F020 — Soft delete + Undelete (AIP-148, AIP-149)

**Feature Branch**: `020-soft-delete`
**Spec**: `specs/020-soft-delete/spec.md`
**Created**: 2026-06-15
**Status**: Draft

## Section 1 — Approach

F020 converts the framework's incidental, always-on GORM soft-delete into a
deliberate, proto-driven policy and adds AIP-149 Undelete. The work flows
**inside-out**: first teach the generator to *recognize* the contract, then
widen the seam the generated code must satisfy, then make every implementation
honor it. Concretely: (1) extend `protoc-gen-storage` so its `messageInfo`
carries `SoftDelete` / `HasExpireTime`, detected in `main.go` by the
`delete_time`/`expire_time` field name + `google.protobuf.Timestamp` message
kind + `OUTPUT_ONLY` behavior, intercepting those two fields *before* the
generic `IsMessage` skip path that would otherwise emit a "nested message
skipped" TODO; (2) make `render.go` emit the model column, `fromModel`
population, column-map entries, `Delete` (soft vs `Unscoped()` hard), `List`
(`if opts.ShowDeleted { q = q.Unscoped() }` *before* tenant/filter `WHERE`s),
`Undelete`, and `PurgeExpired` conditionally on those flags. The render unit
tests (`render_test.go`) are updated in lockstep — including the deliberate
`TestRenderStorageFile_basic` assertion flip from `mustContain(gorm.DeletedAt)`
to `mustNotContain` per FR-010/OQ-2.

Once the generator is correct, widen the **seam**: add `Undelete` to
`persistence.Repository` and `ShowDeleted` to `ListOptions`, which forces every
implementation to follow — the in-memory `MemoryRepository` (uniform
soft-delete via a `deleted` set), and the ent adapter `EntRepository` (a new
`Undelete_` function field + `SoftDeleteMixin` in `entrepo`). Then extend
`seccheck.IsolationConfig` with the optional soft-deleted-isolation probe.
Finally, opt the two fixtures into the contract (`widgets.proto` gets
`delete_time`; `apikey.proto` gets `delete_time` + `expire_time`), rebuild the
plugin, regenerate, and add the integration/isolation tests that exercise the
generated GORM repo and the ent repo end-to-end on inline SQLite. The
`make build && make test` gate runs last across the root module and both
`testdata/*` sub-modules, with a `git diff` check confirming no unintended
churn in any message that did not opt in (AC-012).

## Section 2 — File changes

### Generator (`cmd/protoc-gen-storage`)

| File | Change | Why |
|------|--------|-----|
| `cmd/protoc-gen-storage/render.go` | Add `SoftDelete bool` and `HasExpireTime bool` to `messageInfo`; gate the `DeletedAt` model field on `SoftDelete`; add conditional `ExpireTime sql.NullTime` model field; add `database/sql` + `timestamppb` imports when any message needs them; populate `p.DeleteTime`/`p.ExpireTime` in `fromModel`; add `delete_time`/`expire_time` to the column map; make `Delete` soft vs `Unscoped()`-hard and return `ErrNotFound` on `RowsAffected==0`; add `if opts.ShowDeleted { q = q.Unscoped() }` to `List`; emit `Undelete` and `PurgeExpired` conditionally. | FR-003/004/005/006/007/008/009/014/020 — all generated output. |
| `cmd/protoc-gen-storage/main.go` | Detect the soft-delete marker (`field.Desc.Name()=="delete_time"`, kind is `google.protobuf.Timestamp` message, `IsOutputOnly`) → `msg.SoftDelete=true`; same test for `expire_time` → `msg.HasExpireTime=true`; intercept both fields so they are **not** emitted as ordinary scalar/message columns. | FR-001/002/003 — feed the flags from the proto. |
| `cmd/protoc-gen-storage/render_test.go` | Flip `TestRenderStorageFile_basic` `gorm.DeletedAt` assertion to `mustNotContain` + assert no `Undelete`; add tests for: soft-delete model field present, `Undelete` body shape, `Delete` soft vs hard `Unscoped()`, `List` `ShowDeleted` branch, `fromModel` timestamp population, column-map entries, `expire_time`/`PurgeExpired`. | AC-001..AC-005, AC-007 — codegen unit coverage. |

### Repository seam (`persistence`)

| File | Change | Why |
|------|--------|-----|
| `persistence/repository.go` | Add `Undelete(ctx, key K) (T, error)` to the `Repository[T,K]` interface; add `ShowDeleted bool` to `ListOptions`. | FR-011/012 — the seam every implementation satisfies. |
| `persistence/memory.go` | Add a `deleted map[K]bool`; `Delete` marks deleted (returns `ErrNotFound` if absent or already deleted); `Get`/`List` skip deleted keys (`List` includes them when `opts.ShowDeleted`); add `Undelete` (clears the mark, `ErrNotFound` if not currently deleted). Initialize the map in `NewMemoryRepository`. | FR-013 — in-memory soft-delete semantics. |
| `persistence/memory_test.go` | Add a soft-delete round-trip test (Create→Delete→Get=NotFound; List excludes; `ShowDeleted` includes; Undelete restores; Undelete-live=NotFound; second Delete=NotFound). Existing `TestMemoryRepository` second-delete assertion still holds and stays. | AC-006. |

### ent shape (`persistence/entrepo` + apikey fixture)

| File | Change | Why |
|------|--------|-----|
| `persistence/entrepo/repository.go` | Add `UndeleteFn[T,K]` type, an `Undelete_` function field on `EntRepository`, and an `Undelete` method delegating to it. (The compile-time `var _ persistence.Repository[...]` then re-verifies the extended interface.) | FR-016 — ent adapter satisfies the widened seam. |
| `persistence/entrepo/mixin.go` | Add `SoftDeleteMixin` mirroring `TenantMixin`: a nullable `delete_time` ent field + a query interceptor that filters out non-null `delete_time` rows unless a context flag (set from `opts.ShowDeleted`) is present; add a `WithShowDeleted(ctx) ctx` / context-key helper and a `SoftDeleteFilterer`-style query hook. | FR-016 — ent soft-delete primitive. |
| `persistence/entrepo/mixin_test.go` | Add `SoftDeleteMixin` field + interceptor presence tests, mirroring the `TenantMixin` tests. | FR-016 verification. |
| `testdata/apikey/ent/schema/api_key.go` | Add `entrepo.SoftDeleteMixin{}` to the regenerated schema's `Mixin()` (driven by the `delete_time` field on `apikey.proto` via `protoc-gen-ent`). | FR-016/018 — fixture ent schema gains soft-delete. |
| `testdata/apikey/apikeyv1/ent_wiring.go` | Add the `Undelete_` closure (clear `delete_time` for the key, tenant-scoped, `ErrNotFound` when not currently deleted) and route `opts.ShowDeleted` into the list query context flag; copy `DeleteTime`/`ExpireTime` in `fromEntAPIKey`. | FR-016 — ent APIKey repo round-trips Undelete. |
| `cmd/protoc-gen-ent/*.go` | Minimal: when a message has a `delete_time` `OUTPUT_ONLY` Timestamp field, emit `entrepo.SoftDeleteMixin{}` in the generated schema's `Mixin()` (and skip emitting `delete_time`/`expire_time` as ordinary ent fields). Scope limited to making the apikey fixture compile + pass isolation (FR-016). | FR-016 — generator wiring for the ent shape. |

### Tenant isolation (`seccheck`)

| File | Change | Why |
|------|--------|-----|
| `seccheck/dynamic.go` | Add optional `DeleteFn func(ctx, id string) error` and `ListDeletedFn func(ctx) (int, error)` to `IsolationConfig`; when both are set, `AssertCrossAccountIsolation` additionally deletes as A, then asserts B's `ListDeletedFn` (show_deleted) returns 0 and B's `ReadFn` returns NotFound. Nil fields → unchanged behavior. | FR-015 — soft-deleted cross-tenant probe. |
| `seccheck/dynamic_test.go` | Add a test covering the new branch (both fields set → finding when B sees A's soft-deleted row; nil → no new findings). | FR-015 verification. |

### Fixtures + regen

| File | Change | Why |
|------|--------|-----|
| `testdata/toy/widgets.proto` | `import "google/protobuf/timestamp.proto"`; add `google.protobuf.Timestamp delete_time = 7 [OUTPUT_ONLY]` to `Widget`; add `bool show_deleted = 3` to `ListWidgetsRequest`. | FR-017. |
| `testdata/apikey/apikey.proto` | `import "google/protobuf/timestamp.proto"`; add `delete_time = 7 [OUTPUT_ONLY]` and `expire_time = 8 [OUTPUT_ONLY]` to `APIKey`; add `bool show_deleted = 3` to `ListAPIKeysRequest`. | FR-018. |
| `testdata/toy/widgetsv1/*.storage.go`, `*.pb.go` (regenerated) | Regenerated output: gated `DeletedAt`, `Undelete`, `ShowDeleted` branch, `delete_time` column-map entry. | FR-019 — committed regen. |
| `testdata/apikey/apikeyv1/*.storage.go`, ent tree, `*.pb.go` (regenerated) | Regenerated output: soft-delete + expire_time + `PurgeExpired` + ent `delete_time` field. | FR-019 — committed regen. |

### Integration tests (regenerated APIKey repo)

| File | Change | Why |
|------|--------|-----|
| `testdata/apikey/apikeyv1/sqlite_test.go` *(or a new `softdelete_sqlite_test.go` in the same package)* | GORM integration: Create→Delete→Get=NotFound; List default excludes; `ShowDeleted:true` includes with non-nil `DeleteTime`; Undelete→Get with nil `DeleteTime`; Undelete-live=NotFound; `PurgeExpired` removes an expired row (count==1) then Undelete=NotFound. | AC-008, AC-011. |
| `testdata/apikey/apikeyv1/security_isolation_test.go` | Extend both GORM and ent isolation tests with `DeleteFn`/`ListDeletedFn`; assert zero findings under `show_deleted` (FM-004). Add an ent Undelete round-trip assertion (AC-010). | AC-009, AC-010. |

> Files deliberately **not** changed: `cmd/protoc-gen-svc` (OQ-4 deferred),
> `middleware/etag`, `persistence/filter`, `persistence/resourcename`, the toy
> service `.svc.go` wiring. `testdata/toy` has no GORM integration test today;
> F020 does not add one there (toy stays codegen-shape-only), so its regen is
> verified by `make build && make test` + the `git diff` scope check.

## Section 3 — Dependency order

**Strict sequence (each blocks the next):**

1. **Generator (`render.go` + `main.go`) + its unit tests** — pure functions,
   no DB, no proto compile. Must be correct before anything downstream consumes
   it. `render_test.go` is the fast feedback loop here.
2. **Repository seam (`repository.go`)** — adding `Undelete` to the interface
   and `ShowDeleted` to `ListOptions` **breaks compilation** of every
   implementation until they are updated, so it must land paired with:
3. **`MemoryRepository` + `EntRepository`** — both must implement `Undelete`
   immediately after step 2 or the module will not build. The generated GORM
   repo also gains `Undelete` (from step 1), so all three implementations
   satisfy the seam together.
4. **`SoftDeleteMixin` (`entrepo/mixin.go`)** — needed before the ent fixture
   schema/wiring can reference it.
5. **Fixtures (`*.proto`) + rebuild plugin + regen** — must come *after* the
   generator is final (step 1) so regenerated `.storage.go` is correct; must
   come after step 4 so the regenerated ent schema can embed `SoftDeleteMixin`.
   Rebuild `bin/protoc-gen-storage` (and `protoc-gen-ent`) **before** `buf
   generate`, or the regen uses a stale plugin.
6. **Integration + isolation tests** — depend on the regenerated APIKey repo
   (step 5) and the extended seccheck (`dynamic.go`, which can land in parallel
   with steps 1–4).
7. **`make build && make test` gate** — last; `git diff` scope check (AC-012).

**Parallelizable:**

- `seccheck/dynamic.go` + its test (step depends only on the spec, not on the
  generator) can be done any time before step 6.
- The `render_test.go` assertions can be written alongside the `render.go`
  changes (TDD), not strictly after.
- `MemoryRepository` and `EntRepository`/`SoftDeleteMixin` are independent of
  each other once the interface (step 2) is in place.

## Section 4 — Risks / watch-outs

- **`Unscoped()` ordering vs tenant predicate (FR-014/FM-004).** In the
  generated `List`, `Unscoped()` must be chained **before** `.Where("account_id
  = ?", tenantID)` and the filter clauses. `Unscoped()` lifts only GORM's
  automatic `deleted_at IS NULL` scope; if tenant/filter `WHERE`s are dropped or
  applied to a fresh query, `show_deleted` would leak across tenants. The
  isolation test (AC-009) is the guard — write it to actually delete-as-A and
  list-with-show-deleted-as-B.
- **`RowsAffected == 0` detection on `Delete` (FR-007/FM-002).** Today `Delete`
  returns `nil` even when nothing matched. The new code must capture the
  `*gorm.DB` result (`res := q.Delete(...)`), check `res.Error`, then
  `if res.RowsAffected == 0 { return persistence.ErrNotFound }`. Easy to miss
  that GORM's soft-delete of an already-soft-deleted row matches **zero** rows
  (default scope excludes it) — that is the desired second-delete=NotFound
  behavior, not a bug.
- **`Undelete` must use `Unscoped()` + `WHERE deleted_at IS NOT NULL`.** Without
  `Unscoped()` the update can't even see the soft-deleted row; without the
  `deleted_at IS NOT NULL` predicate, Undelete on a live row would match and
  silently "succeed" instead of returning NotFound (FM-001/OQ-3 decision is
  NotFound). The `Update("deleted_at", nil)` must run on the `Unscoped()` query.
- **`render_test.go` assertion flip is load-bearing (FR-010/OQ-2).** The basic
  test currently *requires* `gorm.DeletedAt`. Flipping it to `mustNotContain`
  while a stale `bin/protoc-gen-storage` is still on PATH during regen will
  produce inconsistent output. Rebuild the plugin before regenerating, and run
  the unit tests (which use the in-process `renderStorageFile`, no binary)
  first.
- **`delete_time`/`expire_time` are `MessageKind` + `OUTPUT_ONLY`.** In
  `main.go` they will otherwise be caught by the existing `IsMessage` branch
  (emitting a "nested message skipped" TODO) and by the `IsOutputOnly` skip.
  Detection must run early and the two fields must be excluded from the ordinary
  field loop so they neither produce a TODO nor a scalar column — they are
  handled specially per FR-004/005/006.
- **`database/sql` / `timestamppb` imports are conditional.** Only add
  `database/sql` when some message in the file has `expire_time` (the
  `sql.NullTime` column) and `timestamppb` when some message needs the
  `fromModel` timestamp population. Unconditional imports break the
  `TestRenderStorageFile_basic` / no-import tests and produce unused-import
  errors on non-opted-in files. Mirror the existing `withSecrets` /
  `withMiddleware` import-gating pattern.
- **ent compile-time interface check (FR-016/AC-010).** `var _
  persistence.Repository[any, string] = (*EntRepository[any, string])(nil)` in
  `entrepo/repository.go` will fail to compile the moment `Undelete` is added to
  the interface until `EntRepository.Undelete` exists. Add the method in the
  same change as the interface widening. The apikey `ent_wiring.go` must set
  `Undelete_` or it will nil-panic at call time (compiles fine, fails the test).
- **The apikey ent schema is generated (`DO NOT EDIT`).** `schema/api_key.go`
  is emitted by `protoc-gen-ent`. The `SoftDeleteMixin` must be added via the
  generator (minimal `protoc-gen-ent` change, FR-016) so a regen does not
  clobber a hand-edit. Keep the `protoc-gen-ent` change scoped strictly to
  emitting the mixin + skipping the timestamp fields — full ent purge/TTL is
  OQ-1 (out of scope).
- **Existing `MemoryRepository` `TestMemoryRepository` invariant.** Its
  second-delete=`ErrNotFound` assertion must still pass under the new
  soft-delete `Delete`: deleting an already-soft-deleted key returns
  `ErrNotFound`. But the first `Delete` no longer removes the key from `items`,
  so any test that asserts `len(items)==0` after one delete via internal state
  must go through `List` (which now skips deleted) rather than the map.
- **Two sub-modules + scope diff (AC-012).** `make test` must be run in the root
  module, `testdata/toy`, and `testdata/apikey` (separate `go.mod`s). After
  regen, `git diff` the generated tree: any message without `delete_time`/
  `expire_time` must show **no** change beyond the intended FR-010 flip
  (DeletedAt removed). Unexpected churn means the conditional gating leaked.
