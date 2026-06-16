# Feature Specification: F020 — Soft delete + Undelete (AIP-148, AIP-149)

**Feature Branch**: `020-soft-delete`
**Created**: 2026-06-15
**Status**: Draft

## Summary

F020 turns the framework's *incidental* GORM soft-delete into a deliberate,
proto-driven **policy** and adds the AIP-149 Undelete operation. Today every
generated `<Msg>Model` carries an unconditional `DeletedAt gorm.DeletedAt`
column, so `Delete` already soft-deletes and `List`/`Get` already hide
soft-deleted rows — but none of this is signaled by the proto, none of it is
reflected in the resource's API surface (`delete_time` / `expire_time`), there
is no way to *include* soft-deleted rows (`show_deleted`), and there is no way
to bring a resource back (`Undelete`). This feature: (1) makes soft-delete an
opt-in driven by a `google.protobuf.Timestamp delete_time` field on the
resource message; (2) maps `delete_time`/`expire_time` between the proto and
the GORM model; (3) adds `show_deleted` to `ListOptions`; (4) adds an
`Undelete(ctx, key)` method to the `Repository` seam and its implementations;
and (5) extends cross-account isolation checks to cover soft-deleted rows.

## Goals

- Make soft-delete an explicit, opt-in contract signaled by a
  `google.protobuf.Timestamp delete_time` field on a resource message.
- Map `delete_time` (and optional `expire_time`) between the proto resource and
  the GORM model so the API surface reflects AIP-148 semantics.
- Keep AIP-148 default behavior: `Delete` sets `delete_time`; `Get` on a
  soft-deleted resource returns NOT_FOUND; `List` excludes soft-deleted rows by
  default and includes them when `show_deleted` is set.
- Add `Undelete(ctx, key)` (AIP-149) to the `Repository` interface, the
  in-memory implementation, and the generated GORM repository; clearing
  `delete_time` makes the resource reappear in `List`/`Get`.
- Extend `seccheck.AssertCrossAccountIsolation` so a tenant cannot see another
  tenant's soft-deleted resources, including via `show_deleted`.
- Preserve backward compatibility: a resource message **without** a
  `delete_time` field generates byte-for-byte the same code it does today.

## Non-goals

- **No generated gRPC `Undelete` RPC.** Per AIP-149 the framework exposes
  Undelete as a `Repository` method; a service that wants an `Undelete<Msg>`
  RPC wires it by hand. `protoc-gen-svc` is **not** modified.
- **No background purge daemon / cron.** `expire_time` is *recorded and
  exposed*; the framework provides a `PurgeExpired(ctx, before)` repository
  helper and a documented hook, but does not run a sweeper itself (see Open
  Question OQ-1).
- **No migration of `Delete`/`Get`/`Update` to resource-name arguments.** Keys
  remain raw `string` (unchanged from F019's scope boundary).
- **No change to the ETag / `If-Match` precondition machinery.**
- **No new proto annotation.** Soft-delete is signaled by the presence of the
  standard `delete_time` field, not by an `infoblox.field.v1` option.

## Background (AIP-148 + AIP-149 semantics)

**AIP-148 (soft delete).** A soft-deletable resource carries an `OUTPUT_ONLY`
`google.protobuf.Timestamp delete_time` field. `Delete` does not remove the
row; it stamps `delete_time`. A soft-deleted resource:

- is excluded from standard `List` responses by default;
- returns NOT_FOUND from standard `Get`;
- becomes visible in `List` only when the request sets `bool show_deleted = N`.

A resource may also carry an `OUTPUT_ONLY` `google.protobuf.Timestamp
expire_time` indicating when the system may permanently purge it. AIP-148 leaves
the purge mechanism to the service; it only requires that `expire_time` is
surfaced so clients know the TTL.

**AIP-149 (undelete).** A soft-deleted resource can be restored by an
`Undelete` operation that clears `delete_time`. After undelete the resource
reappears in `List` and `Get`. Undelete on a resource that is not soft-deleted
is a no-op success or `ALREADY_EXISTS`/`FAILED_PRECONDITION` per service choice;
undelete on a resource that has been *purged* (hard-deleted) is NOT_FOUND.

**How this maps onto the current code.** The generated `<Msg>Model` already has
`DeletedAt gorm.DeletedAt` and `Delete` already calls GORM's soft-delete. F020
keeps that GORM mechanism as the storage primitive but ties it to the *proto
contract*: GORM's `DeletedAt` is the column behind the proto's `delete_time`.
The unconditional emission becomes conditional on the proto opting in.

## Clarifications

- **Opt-in signal.** A resource is soft-deletable iff its message has a field
  named `delete_time` of type `google.protobuf.Timestamp` annotated
  `(google.api.field_behavior) = OUTPUT_ONLY`. The generator detects this and
  sets `messageInfo.SoftDelete = true`. (Reusing the existing `IsOutputOnly`
  detection added in F019; no new annotation.)
- **`expire_time` is optional and independent.** A message may declare
  `google.protobuf.Timestamp expire_time` (`OUTPUT_ONLY`) with or without
  `delete_time`. `expire_time` alone does **not** make a resource soft-deletable
  (it is just a TTL column); soft-delete requires `delete_time`.
- **Behavior change is gated.** When `SoftDelete = false`, the generator emits a
  **hard delete** (`Unscoped().Delete(...)`) and does **not** emit
  `DeletedAt`, `Undelete`, or `show_deleted` handling — matching the
  longstanding spec-010 intent but flipping today's *always-soft* default to
  *soft only when opted in*. This is the one intentional change to existing
  generated output (see FR-010 / OQ-2).
- **Timestamp mapping.** GORM owns `DeletedAt gorm.DeletedAt` (a
  `sql.NullTime`). The proto `delete_time` is `*timestamppb.Timestamp`.
  `fromModel_<Msg>` converts `m.DeletedAt` → `p.DeleteTime` when valid;
  `toModel_<Msg>` does **not** copy `delete_time` (it is OUTPUT_ONLY and managed
  by Delete/Undelete, never by the caller). `expire_time` maps to a plain
  `ExpireTime sql.NullTime` / `*time.Time` column.
- **Key argument.** `Undelete` takes the same raw `string` key as `Delete`
  (consistent with F019's boundary; callers `Parse<Msg>Name` first if they hold
  a resource name).
- **Test fixtures.** `testdata/toy/widgets.proto` opts **in** to soft-delete
  (adds `delete_time`); `testdata/apikey/apikey.proto` adds both `delete_time`
  and `expire_time` (so the TTL path is exercised on the tenant+secret fixture).
  A non-opted-in path is covered by a unit-test-only `messageInfo` with no
  `delete_time` field, asserting the hard-delete shape is emitted.

## Functional Requirements

### Proto contract

- **FR-001**: A resource message is *soft-deletable* when it declares
  `google.protobuf.Timestamp delete_time = N [(google.api.field_behavior) =
  OUTPUT_ONLY]`. No other annotation is required. The generator MUST detect this
  field by name + kind + OUTPUT_ONLY and set `messageInfo.SoftDelete = true`.

- **FR-002**: A resource message MAY declare `google.protobuf.Timestamp
  expire_time = M [(google.api.field_behavior) = OUTPUT_ONLY]`. The generator
  MUST detect it and set `messageInfo.HasExpireTime = true`. `expire_time` is
  independent of `delete_time` (FR-001) and does not by itself enable
  soft-delete.

### Code generator (`protoc-gen-storage`)

- **FR-003**: Extend `cmd/protoc-gen-storage/render.go` `messageInfo` with
  `SoftDelete bool` and `HasExpireTime bool`. Extend `cmd/protoc-gen-storage/main.go`
  to set them: a field is the soft-delete marker when
  `field.Desc.Name() == "delete_time"`, its message kind is
  `google.protobuf.Timestamp`, and `IsOutputOnly` is true; the TTL marker is the
  same test for `expire_time`. Both `delete_time` and `expire_time` MUST be
  treated as non-persisted-as-scalar (they are not emitted as ordinary string
  columns; the generator handles them specially per FR-004/FR-005).

- **FR-004**: In `renderMessage`, the trailing model fields are emitted
  conditionally:
  - When `msg.SoftDelete == true`: emit `DeletedAt gorm.DeletedAt
    \`gorm:"index"\`` (unchanged column, now gated).
  - When `msg.SoftDelete == false`: do **not** emit `DeletedAt`.
  - When `msg.HasExpireTime == true`: emit `ExpireTime sql.NullTime
    \`gorm:"column:expire_time;index"\``. (Adds a `database/sql` import when any
    message in the file has `expire_time`.)
  - `ETag`, `CreatedAt`, `UpdatedAt` are emitted as today regardless.

- **FR-005**: `fromModel_<Msg>` MUST populate the proto timestamps:
  - When `msg.SoftDelete`: if `m.DeletedAt.Valid`, set
    `p.DeleteTime = timestamppb.New(m.DeletedAt.Time)`. (Adds a
    `google.golang.org/protobuf/types/known/timestamppb` import when any message
    needs it.) Note: because `Get`/`List` exclude soft-deleted rows by default,
    `p.DeleteTime` is non-nil only on `show_deleted` reads and on the
    `Undelete`/`Delete` return paths.
  - When `msg.HasExpireTime`: if `m.ExpireTime.Valid`, set
    `p.ExpireTime = timestamppb.New(m.ExpireTime.Time)`.
  - `toModel_<Msg>` MUST NOT copy `delete_time` or `expire_time` from the proto
    (OUTPUT_ONLY, framework-managed). They are added to the skip set alongside
    `IsID`/`IsRepeated`/`IsMessage`/`IsSecret`/`IsOutputOnly` — note both fields
    are already `IsOutputOnly`, so they are skipped by the existing toModel/Columns
    filter; FR-005 only adds the *fromModel* population and the model-struct
    fields (FR-004).

- **FR-006**: The `<Msg>Columns` map MUST include `"delete_time": "deleted_at"`
  when `msg.SoftDelete`, and `"expire_time": "expire_time"` when
  `msg.HasExpireTime`, so AIP-160 filters and AIP-132 order_by may reference
  them (e.g. `filter: "delete_time != null"`, `order_by: "expire_time"`). They
  are the only OUTPUT_ONLY fields admitted into the column map; ordinary
  OUTPUT_ONLY fields (e.g. `name`) remain excluded.

- **FR-007**: Generated `Delete` MUST be soft when `msg.SoftDelete`:
  - Soft (default GORM): `q...Delete(&<Msg>Model{})` — GORM stamps `DeletedAt`.
    Tenant scoping (`account_id = ?`) is preserved exactly as today.
  - When `!msg.SoftDelete`: `q...Unscoped().Delete(&<Msg>Model{})` (hard delete),
    so a non-opted-in resource is permanently removed.
  - In both cases, `Delete` MUST return `persistence.ErrNotFound` when no row
    matched (GORM `RowsAffected == 0`), so the gRPC layer maps it to NOT_FOUND.
    (Today `Delete` returns nil even when nothing matched; FR-007 tightens this
    so Undelete/Delete races and missing-key deletes are observable — see FM-002.)

- **FR-008**: Generated `Get` and `List` MUST keep AIP-148 default behavior:
  - `Get` returns `persistence.ErrNotFound` for a soft-deleted row (GORM's
    default scope already excludes `DeletedAt IS NOT NULL`; no code change beyond
    confirming the soft-deleted row is not returned).
  - `List` excludes soft-deleted rows by default and includes them when
    `opts.ShowDeleted` is true (FR-009). When `msg.SoftDelete`, the `List` body
    emits `if opts.ShowDeleted { q = q.Unscoped() }` **before** the tenant/filter
    clauses, so `Unscoped` lifts only the soft-delete scope while tenant and
    filter `WHERE`s still apply (FR-014).
  - When `!msg.SoftDelete`, no `ShowDeleted` branch is emitted (the option is
    ignored — there are no soft-deleted rows to show).

- **FR-009**: Generated `Undelete` MUST be emitted **only** when
  `msg.SoftDelete`:
  ```go
  func (r *<Msg>Repository) Undelete(ctx context.Context, key string) (<pbType>, error) {
      // scope to tenant exactly as Get/Delete do
      q := r.db.WithContext(ctx).Unscoped().Model(&<Msg>Model{}).Where("id = ?", key)
      // (+ account_id = ? when hasTenant)
      q = q.Where("deleted_at IS NOT NULL") // only act on currently-deleted rows
      res := q.Update("deleted_at", nil)
      if res.Error != nil { return nil, fmt.Errorf("undelete <Msg>: %w", res.Error) }
      if res.RowsAffected == 0 { return nil, persistence.ErrNotFound }
      return r.Get(ctx, key)
  }
  ```
  The `deleted_at IS NOT NULL` predicate makes Undelete a NOT_FOUND on a row
  that was never deleted or already purged (FM-001, FM-003), and the tenant
  predicate makes it NOT_FOUND across tenants (FM-004).

- **FR-010**: **Backward compatibility.** A message with no `delete_time` field
  MUST generate code with hard delete, no `DeletedAt` column, no `Undelete`, and
  no `ShowDeleted` branch. The existing `render_test.go` basic test
  (`TestRenderStorageFile_basic`) currently asserts `gorm.DeletedAt` is present
  for a message with no `delete_time`; that assertion MUST be moved to a new
  soft-delete test and replaced with `mustNotContain(..., "gorm.DeletedAt")` for
  the non-opted-in basic case. This is the one deliberate output change (OQ-2).

### Repository seam

- **FR-011**: Add to `persistence.Repository[T, K]`:
  ```go
  Undelete(ctx context.Context, key K) (T, error)
  ```
  Undelete clears the soft-delete marker and returns the restored entity.
  Implementations that do not support soft-delete (none, after this change)
  would return `ErrNotFound`; all framework implementations support it.

- **FR-012**: Add `ShowDeleted bool` to `persistence.ListOptions`. When false
  (zero value), `List` excludes soft-deleted resources; when true, soft-deleted
  resources are included. Existing callers that do not set it keep the
  AIP-148-default (exclude) behavior, so this is a source-compatible addition.

- **FR-013**: `MemoryRepository` MUST implement soft-delete semantics to satisfy
  the extended interface and to back unit tests without a DB:
  - Add a `deleted map[K]bool` (or a `deletedAt map[K]time.Time`).
  - `Delete(key)`: mark `deleted[key] = true` instead of removing from `items`;
    return `ErrNotFound` if the key is absent OR already soft-deleted.
  - `Get(key)`: return `ErrNotFound` for a soft-deleted key.
  - `List(opts)`: skip soft-deleted keys unless `opts.ShowDeleted`.
  - `Undelete(key)`: clear `deleted[key]`; return `ErrNotFound` if the key is
    absent or not currently soft-deleted; return the restored entity on success.
  - The in-memory implementation is *uniformly* soft-delete (it has no proto to
    opt in/out); this is documented as a deliberate simplification.

### Tenant isolation

- **FR-014**: When `opts.ShowDeleted` lifts the soft-delete scope via
  `Unscoped()`, the generated `List` MUST still apply the `account_id = ?` tenant
  `WHERE` (and any filter clauses). `Unscoped()` removes only GORM's automatic
  `deleted_at IS NULL` clause; it MUST NOT remove tenant scoping. The generated
  code MUST call `Unscoped()` first and then chain `.Where("account_id = ?",
  tenantID)` so the tenant predicate survives.

- **FR-015**: Extend `seccheck.IsolationConfig` / `AssertCrossAccountIsolation`
  to optionally verify soft-deleted isolation:
  - Add optional `DeleteFn func(ctx context.Context, id string) error` and
    `ListDeletedFn func(ctx context.Context) (count int, err error)` to
    `IsolationConfig`.
  - When both are set, the assertion: creates as A (existing), deletes as A,
    then lists *with show_deleted* as B and asserts count 0; and reads (Get) as B
    and asserts NOT_FOUND. This proves B cannot see A's soft-deleted rows even
    via `show_deleted`.
  - When the new fields are nil, behavior is unchanged (purely additive; existing
    callers unaffected).

### ent shape

- **FR-016**: The ent shape MUST gain a `SoftDeleteMixin` in
  `persistence/entrepo` mirroring `TenantMixin`: it adds a nullable
  `delete_time` field and an interceptor that filters out rows with non-null
  `delete_time` unless a context flag (set from `opts.ShowDeleted`) is present.
  `EntRepository[T, K]` gains an `Undelete_ UndeleteFn[T, K]` function field and
  an `Undelete` method delegating to it, satisfying the extended `Repository`
  interface. Wiring the ent `delete_time` field through `protoc-gen-ent` output
  for the apikey fixture is **in scope** only to the extent needed for the
  fixture to compile and pass the isolation test; full ent purge/TTL is OQ-1.
  (Rationale: the repo ships two storage shapes — GORM and ent — and the
  `Repository` interface is shared, so ent MUST satisfy the new method or the
  compile-time `var _ persistence.Repository[...]` check breaks.)

### Fixtures + regen

- **FR-017**: Update `testdata/toy/widgets.proto`: add `import
  "google/protobuf/timestamp.proto"`; add `google.protobuf.Timestamp
  delete_time = 7 [(google.api.field_behavior) = OUTPUT_ONLY]` to `Widget`. Add
  `bool show_deleted = 3` to `ListWidgetsRequest` (so the toy service can route
  it into `ListOptions.ShowDeleted`; the generated `.svc.go`/server wiring is
  out of scope, but the field documents the contract).

- **FR-018**: Update `testdata/apikey/apikey.proto`: add `import
  "google/protobuf/timestamp.proto"`; add `google.protobuf.Timestamp
  delete_time = 7 [(google.api.field_behavior) = OUTPUT_ONLY]` and
  `google.protobuf.Timestamp expire_time = 8 [(google.api.field_behavior) =
  OUTPUT_ONLY]` to `APIKey`; add `bool show_deleted = 3` to
  `ListAPIKeysRequest`.

- **FR-019**: Regenerate all testdata. `make build && make test` clean in the
  root module and in both `testdata/toy` and `testdata/apikey` sub-modules.

- **FR-020**: Provide a `PurgeExpired(ctx context.Context, before time.Time)
  (int64, error)` helper on the generated GORM repository **only** when
  `msg.HasExpireTime`: hard-deletes (`Unscoped().Delete`) rows whose
  `expire_time` is non-null and `<= before`, scoped to tenant, returning the
  count purged. This is the documented mechanism for FR-002's TTL; the framework
  does not call it on a schedule (OQ-1).

## Failure Modes

- **FM-001 — Undelete on a never-deleted resource.** `Undelete(key)` for a row
  that exists and has `deleted_at IS NULL`: the `deleted_at IS NOT NULL`
  predicate matches no rows → `RowsAffected == 0` → return
  `persistence.ErrNotFound` (gRPC NOT_FOUND). (See OQ-3 on whether this should be
  a no-op success instead.)

- **FM-002 — Delete on a missing or already-soft-deleted key.** `Delete(key)`
  for an absent key, or for a key already soft-deleted (GORM's default scope
  won't match it), yields `RowsAffected == 0` → `persistence.ErrNotFound`. A
  second `Delete` of the same resource is therefore NOT_FOUND, not a silent
  success.

- **FM-003 — Undelete on a hard-deleted (purged) resource.** After
  `PurgeExpired` (or a non-soft-delete hard delete), the row is gone. `Undelete`
  finds no row → `RowsAffected == 0` → NOT_FOUND. Restoration of purged data is
  impossible by design.

- **FM-004 — `show_deleted` must not leak across tenants.** A `List` with
  `show_deleted=true` issued by tenant B MUST NOT return tenant A's soft-deleted
  rows. `Unscoped()` lifts only the soft-delete scope; the `account_id = ?`
  predicate still applies (FR-014). Covered by FR-015's extended isolation check.

- **FM-005 — Concurrent Delete + Undelete race.** Two concurrent operations on
  the same key resolve at the row level: `Delete` sets `deleted_at`, `Undelete`
  conditionally clears it (`WHERE deleted_at IS NOT NULL`). Whichever commits
  last wins; the loser observes `RowsAffected == 0` and returns NOT_FOUND rather
  than corrupting state. No `delete_time` value is ever partially written. (No
  new locking is introduced; the conditional `WHERE` is the concurrency guard.)

- **FM-006 — `delete_time`/`expire_time` supplied by the caller on
  Create/Update.** Because both are OUTPUT_ONLY and skipped by `toModel`, a
  client-supplied value is silently ignored (never persisted). This matches
  AIP-148: the server owns these timestamps.

- **FM-007 — Filtering on `delete_time` without `show_deleted`.** A `List` whose
  filter references `delete_time` but does not set `show_deleted` still has
  GORM's default `deleted_at IS NULL` scope applied, so `filter:
  "delete_time != null"` returns nothing. This is consistent (you must opt into
  the deleted set to query it); documented, not an error.

## Acceptance Criteria

- **AC-001** (FR-003, FR-004, FR-010): Codegen unit test — a `messageInfo` with
  a soft-delete `delete_time` field emits `gorm.DeletedAt`; a `messageInfo`
  without it emits neither `gorm.DeletedAt` nor an `Undelete` method
  (`mustNotContain`). `render_test.go` `TestRenderStorageFile_basic` updated per
  FR-010.

- **AC-002** (FR-009): Codegen unit test — a soft-deletable `messageInfo`
  produces `func (r *<Msg>Repository) Undelete(ctx context.Context, key string)`
  containing `Unscoped()`, `"deleted_at IS NOT NULL"`, `Update("deleted_at",
  nil)`, and `RowsAffected == 0` → `persistence.ErrNotFound`.

- **AC-003** (FR-007): Codegen unit test — soft-deletable message's `Delete`
  does **not** contain `Unscoped()`; a non-soft message's `Delete` **does**
  contain `Unscoped()`. Both return `persistence.ErrNotFound` when
  `RowsAffected == 0`.

- **AC-004** (FR-008): Codegen unit test — soft-deletable message's `List`
  contains `if opts.ShowDeleted` and `Unscoped()`; non-soft message's `List`
  contains neither.

- **AC-005** (FR-005): Codegen unit test — soft-deletable message's
  `fromModel_<Msg>` contains `timestamppb.New(m.DeletedAt.Time)` guarded by
  `m.DeletedAt.Valid`; `toModel_<Msg>` does **not** assign `delete_time`.

- **AC-006** (FR-013): `MemoryRepository` unit test — Create→Delete→Get returns
  `ErrNotFound`; List excludes the deleted item; List with `ShowDeleted:true`
  includes it; Undelete restores it (Get succeeds, List default includes it);
  Undelete on a never-deleted key returns `ErrNotFound`; second Delete returns
  `ErrNotFound`.

- **AC-007** (FR-006, FR-017, FR-018): Generated `WidgetColumns` contains
  `"delete_time": "deleted_at"`; generated `APIKeyColumns` contains both
  `"delete_time": "deleted_at"` and `"expire_time": "expire_time"`.

- **AC-008** (FR-007, FR-008, FR-009): GORM integration test (inline SQLite, as
  in `sqlite_test.go`) on the regenerated APIKey repo: Create→Delete→Get =
  NOT_FOUND; List default excludes; List `ShowDeleted:true` includes with a
  non-nil `DeleteTime`; Undelete→Get succeeds with nil `DeleteTime`; Undelete on
  a live row = NOT_FOUND.

- **AC-009** (FR-014, FR-015, FM-004): Cross-account isolation test (extending
  `security_isolation_test.go`) — A creates and soft-deletes a resource; B's
  `List` with `show_deleted=true` returns 0; B's `Get` returns NOT_FOUND;
  `seccheck.AssertCrossAccountIsolation` reports zero findings. Covered for both
  the GORM and ent repos.

- **AC-010** (FR-016): The ent-backed APIKey repository compiles against the
  extended `Repository` interface (`var _ persistence.Repository[*APIKey,
  string]` holds) and its `Undelete` round-trips: Delete→Get=NotFound→Undelete→
  Get=found.

- **AC-011** (FR-020): GORM integration test — Create with an `expire_time` in
  the past, then `PurgeExpired(ctx, now)` removes it (count == 1) and a
  subsequent `Undelete` returns NOT_FOUND (FM-003).

- **AC-012** (FR-019): `make build && make test` clean in the root module,
  `testdata/toy`, and `testdata/apikey`. No diff in generated `.storage.go` for
  any message that does not declare `delete_time`/`expire_time` beyond the
  intended FR-010 change — verified by `git diff` on the regenerated tree.

## Open Questions

- **OQ-1 — `expire_time` purge mechanism.** *Recommended default:* the framework
  records/exposes `expire_time` and ships `PurgeExpired(ctx, before)` (FR-020) as
  the supported hook, but does **not** run a background sweeper. A built-in cron
  couples the SDK to a scheduler and a clock policy that belong to the service
  (and conflicts with the shared-cluster co-existence model). Decision needed:
  confirm "helper + docs, no daemon" for F020, leaving an optional
  `server`-level sweeper to a later feature.

- **OQ-2 — Flipping the default from always-soft to opt-in.** Today every model
  is soft-delete (unconditional `DeletedAt`). FR-010 makes it opt-in, which
  *changes generated output* for any existing resource that lacks `delete_time`.
  *Recommended default:* flip it — soft-delete should be an explicit API
  contract, and both shipping fixtures opt in, so no real consumer regresses.
  Alternative (lower blast radius): keep emitting `DeletedAt` unconditionally and
  only gate `Undelete`/`show_deleted`/`delete_time`-mapping on the proto field.
  Decision affects whether AC-001/AC-003/AC-004's `mustNotContain` assertions
  apply.

- **OQ-3 — Undelete on a live resource: NOT_FOUND vs no-op success.** AIP-149
  allows either. *Recommended default:* NOT_FOUND (FM-001) because the
  conditional `WHERE deleted_at IS NOT NULL` makes it free and unambiguous, and
  the in-memory + GORM implementations behave identically. If the platform
  prefers idempotent undelete, switch both implementations to return the live
  entity with no error — decide before implementation since it changes AC-006 and
  AC-008.

- **OQ-4 — `ShowDeleted` plumbing from gRPC to `ListOptions`.** F020 adds the
  proto `bool show_deleted` field and the `ListOptions.ShowDeleted` field, but
  the generated `.svc.go` / server glue that copies request→`ListOptions` is
  owned by `protoc-gen-svc` and the hand-written service. *Recommended default:*
  document the mapping and exercise it directly in tests by setting
  `ListOptions.ShowDeleted`; defer any `protoc-gen-svc` change to a follow-on, so
  F020 stays scoped to persistence + `protoc-gen-storage`.
