---
title: persistence
weight: 4
---

```go
import "github.com/infobloxopen/devedge-sdk/persistence"
```

Package `persistence` provides connection + storage helpers that **do not impose an ORM**: an
optional engine-neutral `Repository[T,K]`, an in-memory dev implementation, a `DSN` abstraction,
and the storage *shape* convention. The SDK has **no ORM dependency**.

## Repository[T,K]

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

The neutral seam. Its method set matches the API verb vocabulary (get/list/create/update/delete,
plus AIP-149 `Undelete`), so service code can depend on it and swap the underlying shape (GORM,
ent, sqlc, hand-written) without changes. The generated GORM and ent repositories both satisfy it
for their resource type (`Repository[*APIKey, string]`). A resource that does not opt into
soft-delete still gets an `Undelete` that returns `ErrNotFound`, so the seam stays uniform.

### Update and the field mask

`Update(ctx, key, entity, fieldMask...)` follows AIP-134:

- **With a field mask** — only the named proto fields are written. Unknown names return
  `codes.InvalidArgument`. Names are validated against the `<Msg>Columns` whitelist (below).
- **Without a field mask** — every writable column is updated to the entity's value, **including
  zero values** (`false`, `0`, `""`). The generated GORM repository writes via a column map so a
  "disable this" (`active=false`) or "clear that" (`label=""`) update is persisted rather than
  silently dropped. Secret columns are only rewritten when the entity carries a new secret value,
  so a non-secret update never wipes the stored secret.

## ListOptions

```go
type ListOptions struct {
    Filter      string // AIP-160 filter expression (see "Filtering & ordering")
    OrderBy     string // AIP-132 order_by, e.g. "created_at desc, name"
    PageSize    int    // generated repos default to 50 when <= 0
    PageToken   string // opaque; generated repos encode the next offset as base64
    ShowDeleted bool   // AIP-148: include soft-deleted resources in List (default false)
}
```

Resource-oriented list parameters. Generated repositories default `PageSize` to 50, encode the
next offset as a base64 page token, and — for soft-delete resources — exclude deleted rows unless
`ShowDeleted` is set.

## Filtering & ordering (`persistence/filter`)

```go
import "github.com/infobloxopen/devedge-sdk/persistence/filter"
```

`ListOptions.Filter` and `ListOptions.OrderBy` are parsed by the `persistence/filter` subpackage,
which the generated GORM repository calls for you. Both are **safe by construction**: literal
values become bind arguments (never string-interpolated, so the grammar is SQL-injection-proof),
and every field name is validated against a per-message whitelist.

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

**Order_by grammar (AIP-132):** a comma-separated list of `field [asc|desc]` terms (direction
case-insensitive; `asc` is the default), e.g. `created_at desc, name`.

```go
cond, err := filter.Parse(opts.Filter, APIKeyColumns)        // → parameterized WHERE
clauses, err := filter.ParseOrderBy(opts.OrderBy, APIKeyColumns) // → validated ORDER BY terms
```

### The `<Msg>Columns` whitelist

`protoc-gen-storage` emits a `var <Msg>Columns = map[string]string{…}` mapping each filterable
proto field name → its DB column. `filter.Parse` / `filter.ParseOrderBy` reject any field not in
this map with `codes.InvalidArgument`. This is what you point API clients at: only the fields in
`<Msg>Columns` are valid in `filter` / `order_by` strings. Secret and output-only fields are
deliberately excluded.

## Generated repository helpers

Beyond the `Repository` interface, `protoc-gen-storage` emits per-resource helpers when the proto
opts into the relevant feature:

- **`LookupBy<Field>Hash(ctx, hash) (T, error)`** — for each secret field, a constant-time-ish
  lookup by stored hash (the secret is never compared in plaintext). Returns `ErrNotFound` when no
  row matches.
- **`PurgeExpired(ctx, before time.Time) (int64, error)`** — for resources with an `expire_time`
  field, hard-deletes soft-deleted rows whose `expire_time` is before the cutoff; returns the count
  removed.

## BatchRepository[T,K]

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

The AIP-137 extension for multi-resource operations. All three methods are atomic: if any key is
invalid (missing or soft-deleted) the whole call fails without modifying any resource. Results are
returned in the same order as the input. `MemoryRepository` implements it, and **both code
generators emit all three methods**, so generated SQL repositories satisfy `BatchRepository[T,K]`
out of the box:

- **`protoc-gen-storage` (GORM)** generates `BatchGet`/`BatchUpdate`/`BatchDelete` on every
  repository. `BatchUpdate`/`BatchDelete` run in a `db.Transaction`; `BatchUpdate` reuses the
  single `Update` per item (field mask + tenant scope inherited).
- **`protoc-gen-ent` (ent)** generates a per-resource `<Resource>EntRepository` wrapper
  (`New<Resource>EntBatchRepository`) that embeds the hand-written adapter and adds the three batch
  methods over an ent transaction. Reads ride the tenant/soft-delete query interceptors;
  mutations carry **explicit** `account_id` + `delete_time IS NULL` predicates, because ent
  interceptors do not cover mutations.

{{< callout type="info" >}}
`BatchUpdate` carries a field mask **per item** (`requests[].update_mask` in AIP-137 wire form),
not at the top level, so the `FieldMaskUnary` interceptor correctly steps aside for batch methods.
Batch updates do not apply per-item ETag/`If-Match` preconditions. `BatchCreate` is not yet
generated.
{{< /callout >}}

## Errors

```go
var (
    ErrNotFound           = errors.New("persistence: not found")
    ErrConflict           = errors.New("persistence: conflict")
    ErrPreconditionFailed = errors.New("persistence: precondition failed")
)
```

`ErrNotFound` is returned by `Get`, `Undelete`, and the generated `LookupBy<Field>Hash` when no
record matches. Map it to `codes.NotFound` at the gRPC boundary — that mapping is what makes
cross-tenant reads look like "does not exist", which is exactly what
[tenant isolation](../../concepts/tenant-isolation/) requires. `ErrConflict` and
`ErrPreconditionFailed` map to `codes.AlreadyExists` and `codes.FailedPrecondition` (e.g. an ETag
mismatch).

The generated GORM `Create`/`Update` produce these sentinels from raw driver errors: a
**unique-constraint violation** (e.g. a duplicate `unique` field within a tenant) becomes
`ErrConflict`, and a **foreign-key / not-null violation** becomes `ErrPreconditionFailed`. They do
this via `persistence.ConstraintError`, which classifies a driver error and returns the matching
clean sentinel — never the raw SQL — so the client sees `AlreadyExists`/`FailedPrecondition` rather
than a 500 leaking the table and column names. The [`ErrorMapper`](../middleware/) interceptor then
turns the sentinel into the gRPC status; you do not write that mapping by hand.

```go
// ConstraintError returns ErrConflict / ErrPreconditionFailed for a recognized
// driver constraint violation (SQLite, PostgreSQL, MySQL), or nil otherwise.
func ConstraintError(err error) error
```

{{< callout type="info" >}}
Defense in depth: `seccheck.AssertErrorMessagesClean` also fails on raw constraint text
(`constraint failed`, `duplicate key`, `SQLSTATE`, a generated `<resource>_models.` table name),
so an *unmapped* constraint leak is caught by the security gate rather than shipping silently.
{{< /callout >}}

## MemoryRepository

```go
func NewMemoryRepository[T any, K comparable](keyFn func(T) K) *MemoryRepository[T, K]
```

An in-memory `Repository[T,K]` for the common CRUD case and for tests — no database, no external
services. Use it to develop and test handlers before wiring a real shape.

## DSN — connection convention

```go
type DSN struct { /* ... */ }
```

The connection abstraction, including devedge's indirect **hotload** form:

```
fsnotify://<driver>/<abs-path>
```

paired with a real DSN file. When the file changes (e.g. rotated credentials), the connection
reloads **without a restart**. This is the uniform indirect-DSN + real-DSN-file pattern used
across devedge engines.

## Migrations

Schema migrations use `infobloxopen/migrate` (the org-standard fork) **regardless of shape** —
the same engine whether you chose GORM, ent, or sqlc.

## Shapes

A "shape" is how entities/queries are modeled and generated. The SDK ships generators for two
(GORM via `protoc-gen-storage`, ent via `protoc-gen-ent`) and treats sqlc and hand-written as
conventions. See [Storage shapes](../../guides/storage-shapes/) for the comparison table and how
to plug a shape in — behind the neutral seam, or directly when you need the shape's full power.
