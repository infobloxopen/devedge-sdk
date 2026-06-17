# F026 — Batch Method Codegen (GORM + ent) + BatchUpdate

**AIPs**: AIP-137 (batch methods), AIP-134 (field-mask update)
**Status**: done
**Branch**: `026-batch-codegen`
**Extends**: F022 (which added `BatchRepository` + `MemoryRepository.BatchGet/BatchDelete` only)

---

## Problem statement

F022 introduced `BatchRepository[T,K]` and implemented `BatchGet`/`BatchDelete` on
`MemoryRepository` plus a hand-wired toy fixture. Two gaps remain:

1. **No `BatchUpdate`.** The batch surface can read and delete sets but cannot update a set of
   resources in one atomic call.
2. **No codegen for SQL backends.** `protoc-gen-storage` (GORM) and `protoc-gen-ent` (ent) do not
   emit batch methods, so generated repositories satisfy `Repository[T,K]` but **not**
   `BatchRepository[T,K]`. Today a service that wants batch on a real database must hand-write the
   atomic, transaction-wrapped, tenant- and soft-delete-aware methods — exactly the error-prone
   work a framework should own. The `persistence.md` docs currently say so as a known limitation.

This feature closes both: adds `BatchUpdate`, and generates all three batch methods for both SQL
shapes so generated repositories satisfy `BatchRepository[T,K]` out of the box.

---

## Goals

- **G-001** Add `BatchUpdate` to `BatchRepository[T,K]` (AIP-137 + AIP-134 field-mask semantics),
  with a `BatchUpdateItem[T,K]` carrier `{Key, Entity, FieldMask}`. Keep `BatchGet` and
  `BatchDelete` unchanged (purely additive — no breaking removal).
- **G-002** Implement `BatchUpdate` on `MemoryRepository` with atomic (all-or-nothing) semantics.
- **G-003** Extend `protoc-gen-storage` (GORM) to emit `BatchGet`, `BatchUpdate`, `BatchDelete` on
  every generated repository, transaction-wrapped and reusing the existing tenant/soft-delete
  patterns; flip the compile-time check to `persistence.BatchRepository[T,K]`.
- **G-004** Extend `protoc-gen-ent` (ent) to emit a per-resource `<Resource>EntRepository` wrapper
  that embeds the hand-written adapter and adds the three batch methods using `client.Tx`, with
  **explicit** tenant + soft-delete predicates (ent query interceptors do not cover mutations).
- **G-005** Add `BatchUpdateWidgets` to the toy fixture (FieldMask shape) and keep
  `BatchGetWidgets`/`BatchDeleteWidgets`; wire handlers + integration tests.
- **G-006** A cross-backend conformance suite asserting identical batch semantics on
  MemoryRepository + GORM-sqlite + ent-sqlite.
- **G-007** Update `persistence.md` to reflect that batch codegen now exists for GORM **and** ent.

### Non-goals

- `BatchCreate` — future work.
- Partial-success batch semantics — atomic all-or-nothing is the contract (callers needing partial
  results layer on top).
- Per-item ETag/If-Match preconditions inside batch — single-resource `Update` keeps the ETag gate;
  batch update does not carry per-item `If-Match` in this iteration.
- JSON Patch (RFC 6902) / JSON Merge Patch (RFC 7386) — settled on proto `FieldMask`.
- Generating the *full* ent repository wiring — `protoc-gen-ent` gains only the batch wrapper;
  Create/Get/List/Update/Delete wiring stays hand-written.

---

## Design

### Interface (`persistence/repository.go`)

```go
type BatchRepository[T any, K comparable] interface {
    Repository[T, K]
    BatchGet(ctx context.Context, keys []K) ([]T, error)                       // unchanged (F022)
    BatchUpdate(ctx context.Context, items []BatchUpdateItem[T, K]) ([]T, error) // NEW
    BatchDelete(ctx context.Context, keys []K) error                           // unchanged (F022)
}

// BatchUpdateItem is one update within a BatchUpdate: target key, new entity,
// and an optional field mask (empty = full update, matching Repository.Update).
type BatchUpdateItem[T any, K comparable] struct {
    Key       K
    Entity    T
    FieldMask []string
}
```

### MemoryRepository.BatchUpdate

Two-pass under the write lock (mirrors `BatchDelete`): validate every key exists and is not
soft-deleted; then replace each entity in full and regenerate its ETag. Empty items → `([]T{}, nil)`.
Field mask accepted but ignored (Memory replaces in full, exactly like single `Update`). Returns
updated entities in input order. No per-item ETag precondition.

### GORM codegen (`protoc-gen-storage`)

Emit per resource, reusing the existing tenant predicate emit (`if tenantID != "" { q = q.Where("account_id = ?", tenantID) }`) and soft-delete handling:

- **`BatchGet(keys)`** — dedup; `WHERE id IN (?)` (+ tenant); reassemble into key order via a
  `map[id]model`; if any key absent from the result → `ErrNotFound`.
- **`BatchDelete(keys)`** — dedup; `db.Transaction` → bulk `WHERE id IN (?)` (+ tenant) `.Delete()`
  (soft via `gorm.DeletedAt`); `RowsAffected != len(keys)` → `ErrNotFound` (rollback).
- **`BatchUpdate(items)`** — `db.Transaction`; construct `txRepo := &<R>{db: tx}`; loop calling the
  generated single `Update(ctx, it.Key, it.Entity, it.FieldMask...)`; any error rolls back; collect
  results in input order.
- Flip compile-check: `var _ persistence.BatchRepository[<pb>, string] = (*<R>)(nil)`.

### ent codegen (`protoc-gen-ent`, path b)

New output: `<Resource>EntRepository` wrapper (a repo file, distinct from the schema files the
plugin emits today). It embeds `*entrepo.EntRepository[*<Pb>, string]` for the standard methods and
adds the three batch methods over a `client.Tx(ctx)`:

- All three carry **explicit** `account_id = ?` (when the resource has a tenant field) and
  `delete_time IS NULL` predicates, because `TenantMixin`/`SoftDeleteMixin` interceptors are
  query-only and do not apply to bulk mutations.
- **`BatchGet`** — `client.<R>.Query().Where(<R>.IDIn(keys...))` + predicates; reorder; count check.
- **`BatchDelete`** — in a Tx, bulk `Update().Where(IDIn + predicates).SetDeleteTime(now)`; affected
  count check.
- **`BatchUpdate`** — in a Tx, per item: `UpdateOneID(key)` with per-field `.SetX()` driven by the
  field mask (secret fields re-encrypted via the `Encryptor`, mirroring the hand-written `Update_`).
- Compile-check: `var _ persistence.BatchRepository[*<Pb>, string] = (*<R>EntRepository)(nil)`.

### Toy fixture

Keep `BatchGetWidgets`/`BatchDeleteWidgets`. Add:
```proto
message BatchUpdateWidgetsRequest  { repeated UpdateWidgetRequest requests = 1; }
message BatchUpdateWidgetsResponse { repeated Widget widgets = 1; }
rpc BatchUpdateWidgets(BatchUpdateWidgetsRequest) returns (BatchUpdateWidgetsResponse) {
  option (google.api.http) = { post: "/v1/widgets:batchUpdate", body: "*" };
  option (infoblox.authz.v1.rule) = { verb: "update", resource: "widgets" };
}
```
Handler maps `req.Requests` → `[]persistence.BatchUpdateItem` → `repo.BatchUpdate` → response in order.

---

## Feature requirements

| ID | Requirement |
|----|-------------|
| FR-001 | Add `BatchUpdateItem[T,K]` and `BatchUpdate` to `BatchRepository` in `persistence/repository.go`. `BatchGet`/`BatchDelete` unchanged. |
| FR-002 | `MemoryRepository.BatchUpdate`: empty → `([]T{}, nil)`; else two-pass under write-lock (validate all keys live → replace all + new ETags); any missing/soft-deleted key → `ErrNotFound`, nothing modified; return in input order. |
| FR-010 | `protoc-gen-storage` emits `BatchGet`/`BatchUpdate`/`BatchDelete` per repository with the semantics in Design; compile-check is `persistence.BatchRepository`. |
| FR-011 | Generated GORM batch methods reuse the existing tenant predicate and soft-delete handling; `BatchDelete`/`BatchUpdate` run inside `db.Transaction`. |
| FR-020 | `protoc-gen-ent` emits `<Resource>EntRepository` with the three batch methods over `client.Tx`, explicit tenant + soft-delete predicates, per-field mask setters for `BatchUpdate`, secret re-encryption; compile-check `persistence.BatchRepository`. |
| FR-030 | Toy proto adds `BatchUpdateWidgets` (FieldMask shape) and keeps `BatchGet`/`BatchDelete`; regenerate `*.pb.go`/`*.pb.gw.go`/`*.svc.go`; boot-gate lists the method. |
| FR-031 | Toy handler implements `BatchUpdateWidgets` via `repo.BatchUpdate`. |
| FR-040 | Cross-backend conformance tests run identical batch behavior against Memory + GORM-sqlite + ent-sqlite. |
| FR-050 | `docs/.../persistence.md`: `BatchRepository` documents all three methods; remove the "codegen not implemented" warning; note GORM **and** ent generate batch. |

---

## Failure modes

| ID | Mode | Mitigation |
|----|------|------------|
| FM-001 | `BatchUpdate` empty items — caller expects empty list, not error. | Guard `if len(items) == 0 { return []T{}, nil }`. |
| FM-002 | `BatchUpdate` with any missing/soft-deleted key — must not partially update. | Two-pass (Memory) / transaction rollback (SQL): validate all before mutating. |
| FM-003 | GORM `BatchDelete`/`BatchGet` with duplicate keys — naive `len` check misfires. | Dedup keys before count check. |
| FM-004 | ent bulk mutation silently crosses tenants / re-deletes — interceptors are query-only. | Generated batch methods inject `account_id` + `delete_time IS NULL` predicates explicitly. |
| FM-005 | GORM `BatchUpdate` partial failure mid-loop. | Wrap loop in `db.Transaction`; any item error rolls back the whole batch. |
| FM-006 | ent `BatchUpdate` drops secret fields or zero values. | Reuse the single-`Update` per-field setter + Encryptor logic inside the Tx. |
| FM-007 | Regenerated `widgets.svc.go` must declare `BatchUpdateWidgets` in the boot-gate. | `AssertMethodsDeclared` at startup; tests won't start otherwise. |

---

## Acceptance criteria

### Interface + MemoryRepository
| ID | Criterion |
|----|-----------|
| AC-001 | `BatchUpdate` with two existing keys returns both updated entities in input order; subsequent `Get` reflects the new values. |
| AC-002 | `BatchUpdate` with empty items returns `([]T{}, nil)`. |
| AC-003 | `BatchUpdate` where one key is missing returns `ErrNotFound`; the other key is unchanged (atomic). |
| AC-004 | `BatchUpdate` where one key is soft-deleted returns `ErrNotFound`; the other key is unchanged. |

### GORM + ent codegen
| ID | Criterion |
|----|-----------|
| AC-010 | Generated GORM repo satisfies `persistence.BatchRepository[T,string]` (compile-time). |
| AC-011 | Generated ent `<R>EntRepository` satisfies `persistence.BatchRepository[T,string]` (compile-time). |
| AC-012 | GORM `BatchDelete` of two ids is atomic; a missing id leaves the other live (sqlite test). |
| AC-013 | GORM `BatchUpdate` of two ids applies both; a missing id rolls back the other (sqlite test). |
| AC-014 | ent batch methods are tenant-scoped: a key in another tenant is treated as not found (sqlite isolation test). |
| AC-015 | ent `BatchUpdate`/`BatchDelete` honor soft-delete (already-deleted key → `ErrNotFound`; delete stamps `delete_time`). |

### Conformance + fixture
| ID | Criterion |
|----|-----------|
| AC-020 | The conformance suite passes identically against Memory, GORM-sqlite, and ent-sqlite. |
| AC-021 | `BatchUpdateWidgets` over HTTP `POST /v1/widgets:batchUpdate` updates two widgets; a missing id → `codes.NotFound` with neither updated. |
| AC-022 | Toy server boots with `BatchUpdateWidgets` declared in authz rules (boot-gate passes). |

---

## Out of scope for F026
- `BatchCreate`; partial-success semantics; per-item ETag preconditions in batch.
- Full ent repository wiring generation (only the batch wrapper is generated).
- LRO wrapping of large batches; AIP-152 cancellation of batch.
