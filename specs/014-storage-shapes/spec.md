# Feature Specification: Dual storage shapes — GORM + ent, both with tenant isolation

**Feature Branch**: `014-storage-shapes`
**Created**: 2026-06-11
**Status**: Draft

## Context

`protoc-gen-storage` generates GORM-backed repositories but has two gaps that
make it unsafe for production multi-tenant services:

1. **No tenant isolation** — `List`, `Get`, `Update`, `Delete` have no
   `WHERE account_id = ?` clause. Any developer forgetting to add a scope leaks
   data across accounts. The security test `TestSecurity_CrossAccountIsolation`
   currently logs expected findings against `MemoryRepository`; a real GORM
   repo has the same gap.
2. **No hash-lookup method** — secret fields store a `_hash` column for equality
   lookup (e.g. `ValidateAPIKey` needs to find a key by its hash), but no
   generated query covers this case.

This feature closes both gaps in GORM and delivers a second storage shape — ent
(entgo.io) — that enforces tenant isolation **structurally** through its privacy
layer: every query is automatically scoped to the tenant from context, making
cross-account leaks impossible to introduce by omission.

Both shapes implement `persistence.Repository[T,K]` so they are interchangeable
behind the neutral seam.

## Clarifications

- **Tenant field convention**: a proto message field named `account_id` (string)
  is the tenant discriminator. Both shapes detect this by field name; no new
  annotation needed. If absent, tenant scoping is skipped (the service is not
  multi-tenant).
- **GORM tenant scoping**: `middleware.TenantIDFromContext(ctx)` is read at
  query time. An empty tenant ID is a no-op (dev/test path). A non-empty ID is
  applied as `db.Where("account_id = ?", tenantID)` on every List, Get, Update,
  Delete.
- **`LookupByHash`**: for each `secret:true` field, the generated GORM
  repository gains `LookupBy<FieldName>Hash(ctx, hash string) (T, error)` — a
  direct indexed lookup on the `_hash` column. This is the `ValidateAPIKey`
  pattern.
- **ent two-step codegen**: `protoc-gen-ent` emits ent schema files
  (`ent/schema/<Resource>.go`). The developer runs `go generate ./ent/...`
  (entc) to produce the typed client. This mirrors ent's standard workflow and
  keeps `protoc-gen-ent` free of the entc binary dependency.
- **ent privacy layer**: the generated schema includes an `Interceptor` that
  reads `middleware.TenantIDFromContext(ctx)` and appends a `WHERE account_id = ?`
  predicate to every query. This fires before any query reaches Postgres —
  the developer cannot bypass it without modifying the generated schema.
- **ent secret fields**: same pattern as GORM — `_hash` + `_cipher` columns,
  no plaintext column. The repository constructor takes `secret.Encryptor`.
- **`EntRepository[T,K]`**: adapter in `persistence/entrepo/` that wraps the
  ent-generated client and implements `persistence.Repository[T,K]`. Services
  that need ent's graph or privacy features directly can bypass the seam.
- **`testdata/apikey` updated**: both shapes demonstrated — GORM with tenant
  isolation + LookupByHash; ent with privacy layer. Two separate test files,
  same proto.
- **No ent dep in root go.mod until needed**: `entgo.io/ent` is added to the
  root `go.mod` because `persistence/entrepo` imports it. The testdata/apikey
  go.mod also gains it for the ent client.
- **sqlc remains the escape hatch**: not implemented in this feature. sqlc fits
  a per-service hand-tuned query pattern, not framework-level codegen.

## User Scenarios & Testing

### User Story 1 — GORM: List is scoped to tenant, cross-account leak impossible (P1) 🎯

A developer uses the generated GORM repository. Any `List` call from a request
with `account_id: alice` in gRPC metadata only returns alice's records; bob's
records are invisible even if the developer forgets to add a filter.

**Acceptance Scenarios**:

1. **Given** alice and bob each have one APIKey, **When** `List` is called with
   alice's context, **Then** only alice's key is returned.
2. **Given** bob calls `Get(ctx, aliceKeyID)`, **When** the GORM repo executes,
   **Then** `ErrNotFound` is returned (the tenant filter scopes the lookup).
3. **Given** no `account_id` in context (dev/test path), **When** `List` is
   called, **Then** all records are returned (no filter applied — dev mode).

**Independent Test**: unit test with SQLite in-memory via gorm's test helper
(or mock rows), verifying the generated WHERE clause.

---

### User Story 2 — GORM: LookupByHash for secret field (P1)

`ValidateAPIKey` looks up a key by submitting the raw value; the service hashes
it and calls `LookupByKeyValueHash(ctx, hash)`.

**Acceptance Scenarios**:

1. **Given** an APIKey stored with `KeyValueHash = h`, **When**
   `LookupByKeyValueHash(ctx, h)` is called, **Then** the APIKey is returned.
2. **Given** a hash that matches no stored key, **When** the lookup runs,
   **Then** `ErrNotFound` is returned.

**Independent Test**: unit test of generated method (template test + compile
check in testdata/apikey).

---

### User Story 3 — ent: privacy layer enforces tenant isolation structurally (P1)

A developer uses the generated ent repository. Cross-account access is
impossible at the query level — the privacy interceptor rejects or filters
any query that lacks a matching tenant in context.

**Acceptance Scenarios**:

1. **Given** alice and bob each have one APIKey, **When** `List` is called with
   alice's context, **Then** only alice's key is returned — ent's interceptor
   added `WHERE account_id = 'alice'` automatically.
2. **Given** a context with no `account_id`, **When** any query runs, **Then**
   ent's privacy layer allows it with no filter (dev mode — same as GORM).
3. **Given** the generated schema, **When** the schema is inspected, **Then**
   `account_id` is an indexed, immutable field and the interceptor is wired.

**Independent Test**: ent integration test using SQLite driver (in-memory, no
Postgres needed).

---

### User Story 4 — ent: secret field stored as hash+cipher, never plaintext (P2)

The ent schema for APIKey has `KeyValueHash` + `KeyValueCipher` columns; the
`EntRepository.Create` encrypts before writing.

**Acceptance Scenarios**:

1. **Given** an APIKey with `key_value = "sk_live_abc"`, **When** `Create` is
   called via `EntRepository`, **Then** the stored row has non-empty
   `key_value_hash` and `key_value_cipher`; the returned entity has
   `key_value = ""`.
2. **Given** the schema, **When** its fields are listed, **Then** no `key_value`
   field exists.

---

### User Story 5 — Both shapes pass SecurityCheck isolation (P1)

`TestSecurity_CrossAccountIsolation` (currently logging a known gap) passes when
run against either the GORM or ent repository.

**Independent Test**: update the toy security test to use a real GORM/ent repo
rather than MemoryRepository for the isolation assertion.

---

### Edge Cases

- ent privacy interceptor with empty tenant → allow all (dev mode, same as GORM).
- `LookupByHash` with empty hash → `ErrNotFound` immediately (no DB round-trip).
- ent schema with no `account_id` field → no interceptor wired (non-tenant service).
- GORM `Update` with tenant in context → WHERE clause added; attempting to update
  another tenant's record returns `ErrNotFound`.

## Requirements

### Functional Requirements

- **FR-001**: `protoc-gen-storage` MUST detect an `account_id` string field and,
  when present, inject `db.Where("account_id = ?", middleware.TenantIDFromContext(ctx))`
  (skipped when tenant is empty) into `List`, `Get`, `Update`, and `Delete`.
- **FR-002**: For each `secret:true` field, `protoc-gen-storage` MUST generate
  `LookupBy<GoFieldName>Hash(ctx context.Context, hash string) (T, error)` on
  the repository — a direct lookup on the `_hash` indexed column.
- **FR-003**: A new `cmd/protoc-gen-ent` plugin MUST generate
  `ent/schema/<Resource>.go` containing: field definitions mirroring the proto
  message (secret fields → `_hash`+`_cipher` only), indexes, an `account_id`
  field via `TenantMixin` (when present in proto), and a query interceptor that
  appends `WHERE account_id = ?` from context when the tenant is non-empty.
- **FR-004**: `persistence/entrepo/` MUST provide `EntRepository[T,K]` wrapping
  an ent-generated client and implementing `persistence.Repository[T,K]`. The
  constructor takes an ent client + `secret.Encryptor`. `Create` and `Update`
  hash+encrypt secret fields; `fromEnt` never returns secret field plaintext.
- **FR-005**: `entgo.io/ent` MUST be added to `go.mod`.
- **FR-006**: `testdata/apikey` MUST demonstrate both shapes: (a) GORM with
  tenant isolation and `LookupByKeyValueHash`; (b) ent with privacy layer and
  secret fields. Both compile and their respective tests pass.
- **FR-007**: `TestSecurity_CrossAccountIsolation` in `testdata/apikey` MUST
  pass (not just log) when using the GORM or ent repository.

### Key Entities

- **`TenantMixin`** (`ent/schema/mixin.go`, framework-provided): ent mixin that
  adds `account_id` field + query interceptor. Generated schemas embed it.
- **`EntRepository[T,K]`** (`persistence/entrepo/`): adapter implementing
  `persistence.Repository[T,K]` over an ent-generated client.
- Updated **`protoc-gen-storage`**: tenant scoping + LookupByHash.
- New **`protoc-gen-ent`**: ent schema generation from proto.

## Success Criteria

- **SC-001**: GORM `List` with alice's context returns only alice's records;
  with bob's context only bob's (unit test with injected tenant context).
- **SC-002**: `LookupByKeyValueHash` finds the correct record; returns
  `ErrNotFound` for unknown hash (unit test, template output verified).
- **SC-003**: ent `List` with alice's context returns only alice's records;
  `account_id` filter is applied via interceptor (ent SQLite integration test).
- **SC-004**: ent `Create` stores `key_value_hash` + `key_value_cipher`; `key_value`
  column absent from schema; returned entity has `key_value = ""` (ent test).
- **SC-005**: `TestSecurity_CrossAccountIsolation` passes (zero findings) for
  both GORM and ent repositories in testdata/apikey (SC-005 closes the known gap).
- **SC-006**: `go build ./... && make test` clean; `testdata/apikey go build ./...`
  clean; `testdata/apikey go test ./...` passes.
