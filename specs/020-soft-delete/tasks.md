# F020 Tasks

Each task produces a verifiable diff. Tags: `[S]` = mechanical (Sonnet),
`[C]` = judgment/ripple/template logic (Opus). Sequencing is governed by
`plan.md` Section 3.

## Phase 1 — Generator changes (`cmd/protoc-gen-storage`)

- [X] [S] T001: In `render.go`, add `SoftDelete bool` and `HasExpireTime bool`
  fields to the `messageInfo` struct (with doc comments). No behavior change yet.
  (FR-003)

- [X] [C] T002: In `main.go` `generateFile`, detect the soft-delete and TTL
  markers: a field named `delete_time` whose kind is `google.protobuf.Timestamp`
  message and `IsOutputOnly` → `msg.SoftDelete = true`; same test for
  `expire_time` → `msg.HasExpireTime = true`. Exclude both fields from the
  ordinary `msg.Fields` append (so they are not emitted as columns or as a
  "nested message skipped" TODO). Verify by inspecting kind + name + behavior.
  (FR-001, FR-002, FR-003)

- [X] [C] T003: In `render.go` `renderMessage`, gate the trailing model fields:
  emit `DeletedAt gorm.DeletedAt \`gorm:"index"\`` only when `msg.SoftDelete`;
  emit `ExpireTime sql.NullTime \`gorm:"column:expire_time;index"\`` when
  `msg.HasExpireTime`. Keep `ETag`/`CreatedAt`/`UpdatedAt` unconditional. (FR-004)

- [X] [C] T004: In `render.go` `renderStorageFile`, add conditional imports —
  `database/sql` when any message has `HasExpireTime`, and
  `google.golang.org/protobuf/types/known/timestamppb` when any message has
  `SoftDelete` or `HasExpireTime`. Mirror the existing `withSecrets`/
  `withMiddleware` gating and add `_ = sql.NullTime{}` / `_ = timestamppb.New`
  to the unused-suppression var block as needed. (FR-004/005)

- [X] [C] T005: In `render.go` `fromModel_<Msg>`, populate
  `p.DeleteTime = timestamppb.New(m.DeletedAt.Time)` guarded by
  `m.DeletedAt.Valid` when `msg.SoftDelete`, and
  `p.ExpireTime = timestamppb.New(m.ExpireTime.Time)` guarded by
  `m.ExpireTime.Valid` when `msg.HasExpireTime`. `toModel` is unchanged (both
  are already skipped as `IsOutputOnly`). (FR-005)

- [X] [S] T006: In `render.go`, add column-map entries: `"delete_time":
  "deleted_at"` when `msg.SoftDelete`, `"expire_time": "expire_time"` when
  `msg.HasExpireTime`, appended to the `<Msg>Columns` map literal. (FR-006)

- [X] [C] T007: In `render.go`, rewrite generated `Delete`: capture
  `res := q.Delete(&<Msg>Model{})` (soft, when `msg.SoftDelete`) or
  `q.Unscoped().Delete(...)` (hard, when `!msg.SoftDelete`), check `res.Error`,
  and return `persistence.ErrNotFound` when `res.RowsAffected == 0`. Preserve
  tenant scoping exactly. (FR-007/FM-002)

- [X] [C] T008: In `render.go` generated `List`, when `msg.SoftDelete` emit
  `if opts.ShowDeleted { q = q.Unscoped() }` **before** the tenant `WHERE` and
  filter clauses; emit nothing when `!msg.SoftDelete`. (FR-008, FR-014)

- [X] [C] T009: In `render.go`, emit a generated `Undelete(ctx, key string)`
  method **only** when `msg.SoftDelete`: `Unscoped().Model(...).Where("id = ?",
  key)` (+ tenant `account_id` when present) + `Where("deleted_at IS NOT NULL")`
  + `Update("deleted_at", nil)`; `ErrNotFound` on `RowsAffected == 0`; return
  `r.Get(ctx, key)`. (FR-009/FM-001/FM-005)

- [X] [C] T010: In `render.go`, emit a generated `PurgeExpired(ctx, before
  time.Time) (int64, error)` method **only** when `msg.HasExpireTime`:
  `Unscoped().Where("expire_time IS NOT NULL AND expire_time <= ?", before)`
  (+ tenant scope) `.Delete(&<Msg>Model{})`, returning `res.RowsAffected`.
  (FR-020)

- [X] [C] T011: In `render_test.go`, flip `TestRenderStorageFile_basic`:
  replace `mustContain(t, out, "gorm.DeletedAt")` with `mustNotContain` and add
  `mustNotContain(t, out, "func (r *WidgetRepository) Undelete(")`. Add new
  table-driven/standalone tests covering AC-001..AC-005 + AC-007: soft-delete
  model field present; `Undelete` body (`Unscoped()`, `"deleted_at IS NOT
  NULL"`, `Update("deleted_at", nil)`, `RowsAffected == 0`); `Delete` soft has
  no `Unscoped()` / non-soft `Delete` has `Unscoped()`; `List` `if
  opts.ShowDeleted`; `fromModel` `timestamppb.New(m.DeletedAt.Time)`;
  `expire_time` column + `PurgeExpired`; column-map entries.
  (AC-001, AC-002, AC-003, AC-004, AC-005, AC-007)

## Phase 2 — Repository seam (`persistence`)

- [X] [C] T012: In `persistence/repository.go`, add
  `Undelete(ctx context.Context, key K) (T, error)` to the `Repository[T,K]`
  interface and `ShowDeleted bool` to `ListOptions` (with doc comments per
  FR-011, FR-012). This breaks compilation of implementations until T013 land.

- [X] [C] T013: In `persistence/memory.go`, implement soft-delete: add
  `deleted map[K]bool` (init in `NewMemoryRepository`); `Delete` marks deleted
  (`ErrNotFound` if absent or already deleted); `Get` returns `ErrNotFound` for
  a deleted key; `List` skips deleted keys unless `opts.ShowDeleted`; add
  `Undelete` (clears the mark; `ErrNotFound` if absent or not currently
  deleted; returns the entity). (FR-013)

- [X] [S] T014: In `persistence/memory_test.go`, add `TestMemoryRepository_SoftDelete`
  covering AC-006 (Create→Delete→Get=NotFound; List excludes; `ShowDeleted:true`
  includes; Undelete restores; Undelete-live=NotFound; second Delete=NotFound).
  Confirm existing `TestMemoryRepository` still passes. (AC-006)

## Phase 3 — ent shape (`persistence/entrepo` + apikey fixture)

- [X] [C] T015: In `persistence/entrepo/repository.go`, add
  `UndeleteFn[T,K] func(ctx, key K) (T, error)`, an `Undelete_ UndeleteFn[T,K]`
  field on `EntRepository`, and an `Undelete` method delegating to it. The
  compile-time `var _ persistence.Repository[...]` now re-verifies the widened
  interface. (FR-016)

- [X] [C] T016: In `persistence/entrepo/mixin.go`, add `SoftDeleteMixin`
  (mirroring `TenantMixin`): a nullable `delete_time` ent field + an
  interceptor that filters out non-null `delete_time` rows unless a
  show-deleted context flag is set; add `WithShowDeleted(ctx) ctx` +
  context-key + a query-filter helper. (FR-016)

- [X] [S] T017: In `persistence/entrepo/mixin_test.go`, add `SoftDeleteMixin`
  field-presence and interceptor-presence tests mirroring the `TenantMixin`
  tests, plus a `var _ ent.Mixin = entrepo.SoftDeleteMixin{}` compile check.
  (FR-016)

- [X] [C] T018: In `cmd/protoc-gen-ent`, when a message declares a `delete_time`
  `OUTPUT_ONLY` Timestamp field, emit `entrepo.SoftDeleteMixin{}` in the
  generated schema's `Mixin()` and skip emitting `delete_time`/`expire_time` as
  ordinary ent fields. Scope strictly to the apikey fixture compiling +
  passing isolation (no purge/TTL ent logic). (FR-016)

## Phase 4 — Tenant isolation (`seccheck`)

- [X] [C] T019: In `seccheck/dynamic.go`, add optional
  `DeleteFn func(ctx, id string) error` and
  `ListDeletedFn func(ctx) (int, error)` to `IsolationConfig`; when both are
  set, `AssertCrossAccountIsolation` deletes as A, then asserts B's
  `ListDeletedFn` returns 0 and B's `ReadFn` returns NotFound (FM-004). Nil
  fields → unchanged. Add a matching test in `seccheck/dynamic_test.go`. (FR-015)

## Phase 5 — Fixtures + regen

- [X] [S] T020: Update `testdata/toy/widgets.proto`: import
  `google/protobuf/timestamp.proto`; add `google.protobuf.Timestamp delete_time
  = 7 [(google.api.field_behavior) = OUTPUT_ONLY]` to `Widget`; add `bool
  show_deleted = 3` to `ListWidgetsRequest`. (FR-017)

- [X] [S] T021: Update `testdata/apikey/apikey.proto`: import
  `google/protobuf/timestamp.proto`; add `delete_time = 7 [OUTPUT_ONLY]` and
  `expire_time = 8 [OUTPUT_ONLY]` to `APIKey`; add `bool show_deleted = 3` to
  `ListAPIKeysRequest`. (FR-018)

- [X] [C] T022: Rebuild the plugins (`make generate` builds
  `bin/protoc-gen-storage` + `protoc-gen-ent` first) and regenerate all
  testdata. Inspect `git diff` of the regenerated tree: confirm soft-delete /
  expire_time / Undelete / PurgeExpired / ent `delete_time` appear as intended
  and that no non-opted-in message changed beyond the FR-010 `DeletedAt`
  removal. (FR-019/AC-012)

- [X] [C] T023: In `testdata/apikey/apikeyv1/ent_wiring.go`, add the `Undelete_`
  closure (tenant-scoped clear of `delete_time`, `ErrNotFound` when not
  currently deleted), route `opts.ShowDeleted` into the list query via the
  `SoftDeleteMixin` context flag, and copy `DeleteTime`/`ExpireTime` in
  `fromEntAPIKey`. (FR-016/AC-010)

## Phase 6 — Integration + isolation tests

- [X] [C] T024: Add a GORM integration test (new
  `testdata/apikey/apikeyv1/softdelete_sqlite_test.go`, reusing the inline
  SQLite dialector): Create→Delete→Get=NotFound; List default excludes;
  `ShowDeleted:true` includes with non-nil `DeleteTime`; Undelete→Get with nil
  `DeleteTime`; Undelete-live=NotFound; `PurgeExpired` removes an expired row
  (count==1) then Undelete=NotFound. (AC-008, AC-011)

- [X] [C] T025: Extend `testdata/apikey/apikeyv1/security_isolation_test.go`:
  add `DeleteFn`/`ListDeletedFn` to both the GORM and ent `IsolationConfig`s
  and assert zero findings under `show_deleted` (FM-004); add an ent Undelete
  round-trip assertion (Delete→Get=NotFound→Undelete→Get=found).
  (AC-009, AC-010)

## Phase 7 — Verification gate

- [X] [C] T026: Run `make build && make test` in the root module,
  `testdata/toy`, and `testdata/apikey`; run `make vet`/`make lint` if present;
  re-run the `git diff` scope check (AC-012). All green + no unintended generated
  churn before the feature is done.
