---
title: persistence
weight: 4
---

```go
import "github.com/infobloxopen/devedge-sdk/persistence"
```

Package `persistence` provides connection and storage helpers for service data access. It gives you a typed, engine-neutral `Repository[T,K]` interface, an in-memory implementation for development and testing, a `DSN` abstraction with live credential reload, and the storage shape convention. The package has no ORM dependency: the `Repository[T,K]` interface is optional, and you choose whether to back it with an ORM. Use this package when wiring a service to a database backend.

## `Repository[T,K]`

```go
type Repository[T any, K comparable] interface {
    Get(ctx context.Context, key K) (T, error)
    List(ctx context.Context, opts ListOptions) (items []T, nextPageToken string, err error)
    Create(ctx context.Context, entity T) (T, error)
    Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error)
    Delete(ctx context.Context, key K) error
    // Undelete restores a soft-deleted entity (AIP-149). Returns ErrNotFound when the
    // entity does not exist, was never soft-deleted, or has been permanently purged.
    // Implementations backed by hard-delete storage always return ErrNotFound.
    Undelete(ctx context.Context, key K) (T, error)
}
```

`Repository[T,K]` is the engine-neutral storage interface. Its method set matches the API verb vocabulary (get/list/create/update/delete, plus AIP-149 `Undelete`), so service code depends on this interface and can swap the underlying backend (GORM, ent, sqlc, or hand-written) without changes. The generated GORM and ent repositories both satisfy it for their resource type, for example `Repository[*APIKey, string]`.

A resource opts into soft-delete by declaring a server-managed `delete_time` timestamp field in its proto definition:

```proto
google.protobuf.Timestamp delete_time = N [(google.api.field_behavior) = OUTPUT_ONLY]
```

Without that field, `Delete` performs a hard delete, `ShowDeleted` has no effect, and `Undelete` returns `ErrNotFound`. A resource without soft-delete still gets an `Undelete` method that returns `ErrNotFound`, so the seam stays uniform: every repository satisfies the same `Repository[T,K]` interface, and you do not branch on whether a backend happens to support restore. See the *Soft-delete (opt-in)* section in the codegen reference for the full generated shape.

### Update and the field mask

`Update(ctx, key, entity, fieldMask...)` follows AIP-134:

- **With a field mask** — only the named proto fields are written. Unknown names return `codes.InvalidArgument`. Names are validated against the `<Msg>Columns` whitelist (see [The `<Msg>Columns` whitelist](#the-msgcolumns-whitelist) below).
- **Without a field mask** — every writable column is updated to the entity's value, including zero values (`false`, `0`, `""`). The generated GORM repository writes via a column map so a "disable this" (`active=false`) or "clear that" (`label=""`) update is persisted rather than silently dropped.
- **Secret columns** — a secret column is only rewritten when the entity carries a new secret value, so a non-secret update does not overwrite the stored secret.
- **The tenant key (`account_id`) is never writable** — it is assigned at create and sourced from the request context. It is omitted from the no-mask column map and rejected with `codes.InvalidArgument` if named in the field mask, so an update that omits `account_id` cannot blank the scoping key and orphan the row from its tenant.

### `update_mask` encoding over the gateway

A generated Update RPC uses `repeated string update_mask` (not `google.protobuf.FieldMask`), while a Get/List RPC uses `google.protobuf.FieldMask read_mask`. The two fields have different wire encodings over the grpc-gateway, and the wrong form fails silently (HTTP 200, fresh etag, zero fields updated):

| Encoding | Works? | Notes |
|---|---|---|
| `?update_mask=name&update_mask=value` | **yes** | repeated query params, proto snake_case name |
| `?update_mask=name,value` | **no** | treated as a single unknown path; silently no-ops |
| `?update_mask=displayName` | **no** | JSON camelCase rejected; no error returned |

Supply `update_mask` as separate repeated query params using the proto snake_case field name:

```bash
# Correct — two separate params:
curl -X PATCH 'localhost:8080/v1/widgets/w1?update_mask=name&update_mask=label' \
  -d '{"name":"new-name","label":"new-label"}'

# Wrong — comma-joined: silently updates zero fields (HTTP 200, new etag):
curl -X PATCH 'localhost:8080/v1/widgets/w1?update_mask=name,label' ...

# Wrong — camelCase: silently updates zero fields:
curl -X PATCH 'localhost:8080/v1/widgets/w1?update_mask=displayName' ...
```

{{< callout type="info" >}}
`read_mask` (`google.protobuf.FieldMask`) uses a different gateway encoding: a comma-joined value in a single query param — `?read_mask=name,create_time`. The difference comes from the proto type: `FieldMask` has a built-in gateway codec that accepts comma-joining; `repeated string` does not.
{{< /callout >}}

See [middleware → Field mask](../middleware/#field-mask) for the interceptor that validates update masks server-side.

## `ListOptions`

```go
type ListOptions struct {
    Filter      string // AIP-160 filter expression (see "Filtering & ordering")
    OrderBy     string // AIP-132 order_by, e.g. "created_at desc, name"
    PageSize    int    // generated repos default to 50 when <= 0
    PageToken   string // opaque; generated repos encode the next offset as base64
    ShowDeleted bool   // AIP-148: include soft-deleted resources in List (default false)
}
```

`ListOptions` carries the resource-oriented list parameters for a `List` call. Generated repositories default `PageSize` to 50, encode the next offset as a base64 page token, and — for soft-delete resources — exclude deleted rows unless `ShowDeleted` is set.

## Filtering and ordering

```go
import "github.com/infobloxopen/devedge-sdk/persistence/filter"
```

`ListOptions.Filter` and `ListOptions.OrderBy` are parsed by the `persistence/filter` subpackage, which the generated GORM repository calls for you. Both are safe by construction: literal values become bind arguments and are never string-interpolated, so the grammar is SQL-injection-proof. Every field name is validated against a per-message whitelist before the query runs.

**Filter grammar (AIP-160 subset):**

| Element | Supported |
|---|---|
| Comparison | `=`, `!=`, `<`, `<=`, `>`, `>=` |
| Boolean | `AND`, `OR` (case-insensitive), parentheses for grouping |
| Values | double-quoted strings (`"alice"`) and numbers (`5`) |

```
name = "alice" AND weight > 5
(status = "active" OR status = "pending") AND weight >= 0
```

**Order_by grammar (AIP-132):** a comma-separated list of `field [asc|desc]` terms (direction case-insensitive; `asc` is the default), e.g. `created_at desc, name`.

```go
cond, err := filter.Parse(opts.Filter, APIKeyColumns)        // → parameterized WHERE
clauses, err := filter.ParseOrderBy(opts.OrderBy, APIKeyColumns) // → validated ORDER BY terms
```

### The `<Msg>Columns` whitelist

`protoc-gen-storage` emits a `var <Msg>Columns = map[string]string{…}` that maps each filterable proto field name to its DB column. `filter.Parse` and `filter.ParseOrderBy` reject any field not in this map with `codes.InvalidArgument`. Only the fields in `<Msg>Columns` are valid in `filter` or `order_by` strings. Secret and output-only fields are excluded.

On the **ent backend**, `protoc-gen-ent` emits the equivalent `var <Msg>EntColumns` (and, for a `tags` field, `var <Msg>EntJSONColumns`) into the proto's Go package. The ent column names differ from GORM's — for example, ent stores soft-delete as `delete_time`, GORM as `deleted_at` — and the ent-suffixed names let both coexist when a service generates both backends. An ent-only service wires filtering with these maps:

```go
entrepo.FilterPredicate(opts.Filter, <Msg>EntColumns, <Msg>EntJSONColumns)
```

## Generated repository helpers

Beyond the `Repository` interface, `protoc-gen-storage` emits per-resource helpers when the proto opts into the relevant feature:

- **`LookupBy<Field>Hash(ctx, hash) (T, error)`** — for each secret field, a constant-time-ish lookup by stored hash (the secret is never compared in plaintext). Returns `ErrNotFound` when no row matches.
- **`PurgeExpired(ctx, before time.Time) (int64, error)`** — for resources with an `expire_time` field, hard-deletes any row whose `expire_time` is at or before the cutoff (regardless of soft-delete state), tenant-scoped; returns the count removed. `expire_time` is `OUTPUT_ONLY` and stamped by your Create handler (see *codegen → TTL*). The generated `toModel` carries it to the column, so a row created through the interface has a real expiry for `PurgeExpired` to reap. The cutoff is normalized to UTC internally (stored values are UTC), so `PurgeExpired(time.Now())` reaps correctly on SQLite regardless of the caller's local time zone.

## `BatchRepository[T,K]`

```go
type BatchRepository[T any, K comparable] interface {
    Repository[T, K]
    BatchGet(ctx context.Context, keys []K) ([]T, error)                        // AIP-137, atomic
    BatchUpdate(ctx context.Context, items []BatchUpdateItem[T, K]) ([]T, error) // AIP-137, atomic
    BatchDelete(ctx context.Context, keys []K) error                            // AIP-137, atomic
}

// BatchUpdateItem carries one update: target key, new entity, optional field
// mask (empty = full update, matching Update).
type BatchUpdateItem[T any, K comparable] struct {
    Key       K
    Entity    T
    FieldMask []string
}
```

`BatchRepository[T,K]` extends `Repository[T,K]` with multi-resource operations following AIP-137. All three methods are atomic: if any key is invalid (missing or soft-deleted), the whole call fails without modifying any resource. Results are returned in the same order as the input.

`MemoryRepository` implements `BatchRepository[T,K]`. Both code generators also emit all three methods, so generated SQL repositories satisfy `BatchRepository[T,K]` without additional wiring:

- **`protoc-gen-storage` (GORM)** generates `BatchGet`, `BatchUpdate`, and `BatchDelete` on every repository. `BatchUpdate` and `BatchDelete` run in a `db.Transaction`. `BatchUpdate` reuses the single `Update` call per item, inheriting the field mask and tenant scope.
- **`protoc-gen-ent` (ent)** generates a per-resource `<Resource>EntRepository` wrapper (`New<Resource>EntBatchRepository`) that embeds the hand-written adapter and adds the three batch methods over an ent transaction. Reads ride the tenant/soft-delete query interceptors; mutations carry explicit `account_id` and `delete_time IS NULL` predicates, because ent interceptors do not cover mutations.

{{< callout type="info" >}}
`BatchUpdate` carries a field mask per item (`requests[].update_mask` in AIP-137 wire form), not at the top level, so the `FieldMaskUnary` interceptor correctly steps aside for batch methods. Batch updates do not apply per-item ETag/`If-Match` preconditions. `BatchCreate` is not yet generated.
{{< /callout >}}

## Errors

```go
var (
    ErrNotFound           = errors.New("persistence: not found")
    ErrConflict           = errors.New("persistence: conflict")
    ErrPreconditionFailed = errors.New("persistence: precondition failed")
)
```

`ErrNotFound` is returned by `Get`, `Undelete`, and the generated `LookupBy<Field>Hash` when no record matches. Map it to `codes.NotFound` at the gRPC boundary. That mapping makes cross-tenant reads appear as "does not exist", which is what [tenant isolation](../../concepts/tenant-isolation/) requires.

`ErrConflict` maps to `codes.AlreadyExists` and `ErrPreconditionFailed` maps to `codes.FailedPrecondition` (for example, an ETag mismatch).

The generated GORM `Create` and `Update` produce these sentinels from raw driver errors via `persistence.ConstraintError`, which classifies a driver error and returns the matching sentinel — never the raw SQL. A unique-constraint violation (for example, a duplicate `unique` field within a tenant) becomes `ErrConflict`. A foreign-key or not-null violation becomes `ErrPreconditionFailed`. The client sees `AlreadyExists` or `FailedPrecondition` rather than a 500 leaking the table and column names. The [`ErrorMapper`](../middleware/) interceptor then turns the sentinel into the gRPC status without additional hand-written mapping.

On the **ent backend**, the generated adapter (`<resource>_repo.ent.go`, from `protoc-gen-ent`) calls `persistence.ConstraintError` on every `Save` or mutation error to produce the same sentinels. As a safety net, `ErrorMapperUnary` also runs `persistence.ConstraintError` on any otherwise-unclassified error, so even a custom adapter that omits the call still returns a clean `AlreadyExists` or `FailedPrecondition` with no raw SQL.

```go
// ConstraintError returns ErrConflict / ErrPreconditionFailed for a recognized
// driver constraint violation (SQLite, PostgreSQL, MySQL), or nil otherwise.
func ConstraintError(err error) error
```

{{< callout type="info" >}}
Defense in depth: `seccheck.AssertErrorMessagesClean` also fails on raw constraint text (`constraint failed`, `duplicate key`, `SQLSTATE`, a generated `<resource>_models.` table name), so an unmapped constraint leak is caught by the security gate rather than shipping silently.
{{< /callout >}}

## `MemoryRepository`

```go
func NewMemoryRepository[T any, K comparable](keyFn func(T) K) *MemoryRepository[T, K]
```

`MemoryRepository` is an in-memory `Repository[T,K]` for the common CRUD case and for tests — no database, no external services. Use it to develop and test handlers before wiring a real backend.

## DSN

```go
type DSN struct { /* ... */ }
```

`DSN` is the connection abstraction for devedge services. It supports an indirect **hotload** form:

```
fsnotify://<driver>/<abs-path>
```

paired with a real DSN file. When the file changes (for example, after a credential rotation), the connection reloads without a restart. This is the uniform indirect-DSN + real-DSN-file pattern used across devedge engines.

## Migrations

On PostgreSQL (and MySQL) the schema-of-record evolves through **versioned, sequentially-numbered SQL migrations** — `NNNN_<description>.up.sql` / `.down.sql`, zero-padded and gap-free (`0001`, `0002`, …). The `persistence/migrate` engine applies them through the org-standard `infobloxopen/migrate` fork (which adds a persisted down-store and dirty-state recovery), regardless of which storage shape you chose — GORM, ent, or sqlc all share the same engine.

**Framework baseline.** The SDK owns a generated `0001_framework_init.{up,down}.sql` — the framework tables (the transactional outbox, including the cell-development `event_seq`/`event_epoch` columns; idempotency markers; the dispatch cursor + dead-letter sidecars; and the `tenant_fence`/`tenant_event_seq`/`tenant_event_policy` cell tables). It is composed **ahead of** your module's own migrations, so every service builds its schema forward from `0001`; your first migration is `0002`. The baseline is generated from the canonical framework models with [Atlas](https://atlasgo.io) at build time (never in the service binary) and drift-checked in CI (`make check-migration-baseline`).

**Safe by default.** The migration connection sets `lock_timeout=2s` and `statement_timeout=60s` (overridable per module) so a contended migration **fails fast** instead of queuing behind live queries; it never touches the app pool. Migrations run within the module's schema (`search_path`), so `schema_migrations` is per-module, and a **single advisory lock** serializes concurrent hosts/replicas — one runs, the rest wait and find the schema already current. A failed migration leaves a recoverable dirty state that the next corrected run **auto-recovers**, and the persisted down-store lets a rollback run even when the running image no longer ships the down file.

**`CREATE INDEX CONCURRENTLY`** must be the **only** statement in its migration file (Postgres cannot run it inside a transaction block); a multi-statement file mixing it fails loud.

**Dev fast-path.** SQLite (dev/test only) keeps `AutoMigrate`, which also creates the domain tables so a freshly scaffolded service runs out of the box; the versioned-SQL path is verified against real PostgreSQL in CI. To adopt the versioned path, switch the dialector to Postgres, set a `postgres://` DSN, and add your domain tables as `module/migrations/0002_*.up.sql`.

To author and apply a migration step by step, see [Change the database schema](../../how-to/model-and-persist/change-the-database-schema/).

## Storage shapes

A _shape_ is how entities and queries are modeled and generated. The SDK ships generators for two shapes (GORM via `protoc-gen-storage`, ent via `protoc-gen-ent`) and treats sqlc and hand-written as additional conventions. See [Storage shapes](../../how-to/model-and-persist/storage-shapes/) for the comparison table and how to plug a shape in — behind the `Repository` interface, or directly when you need the backend's full surface.
