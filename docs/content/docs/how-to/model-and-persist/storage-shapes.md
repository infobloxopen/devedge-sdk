---
title: Storage Shapes
weight: 3
aliases:
  - /docs/guides/storage-shapes/
---

A *storage shape* is the combination of a data-modeling tool and its generated code that backs a
service's `Repository` implementation. The SDK offers four shapes — two with code generators
(`protoc-gen-storage` for GORM and `protoc-gen-ent` for ent), and two convention-based options
(sqlc and hand-written). Pick the shape that best fits your data model; you can mix shapes across
resources in the same service.

The SDK is resource-oriented and AIP-aligned: the API contract — resources, standard methods,
field masks, filtering, and pagination — is primary, and how a service stores those resources is
a secondary, swappable concern. The SDK does not impose an ORM and has no ORM dependency, so no
shape is mandated.

Use this page when you are starting a new resource and want to choose between the available
shapes, or when you need to wire a shape to the SDK's persistence seam.

## Available shapes

| Shape | Source of truth | What you get | Best for |
|---|---|---|---|
| **proto → GORM** (`protoc-gen-storage`) | the `.proto` resource | generated ORM model + CRUDL `Repository` | lowest-friction when the proto already defines the resource |
| **ent** ([entgo.io](https://entgo.io)) | a Go ent schema | a type-safe client; graph edges and traversal, hooks, and a privacy layer | rich domain graphs, relationship-heavy data; privacy layer pairs well with the authz seam |
| **sqlc** | hand-written SQL | compile-time-safe query code, no reflection | hand-tuned or performance-critical paths; the escape hatch |
| **hand-written** | your code | implement `Repository[T,K]` directly | small or simple stores, or wrapping any shape above |

The SDK ships generators for GORM and ent. sqlc and hand-written are conventions, not generators.

## Choosing between GORM and ent

| Question | Lean GORM | Lean ent |
|---|---|---|
| Is the proto the natural source of truth? | yes — generate straight from it | maybe — ent schema is separate Go |
| Plain per-resource CRUD? | yes | overkill |
| Relationship-heavy graph (edges, traversals)? | awkward | **yes** — ent's strength |
| Want a query-level privacy layer alongside authz? | no | **yes** |
| Want hooks on mutations? | callbacks | **first-class** |
| Minimize new concepts for the team? | **yes** | learning curve |

Both enforce **tenant isolation** the same way — every query is scoped by `account_id` from
`middleware.TenantIDFromContext(ctx)` (see [Tenant isolation](../../../concepts/tenant-isolation/)).

## Constructor signatures

The generated constructors differ only by whether the message has secret fields. When secret fields
are present, the constructor accepts an `Encryptor`:

```go
// GORM — protoc-gen-storage
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
// (without secret fields it would be: func NewWidgetRepository(db *gorm.DB) *WidgetRepository)

// ent — protoc-gen-ent generates the schema; you wire it to the seam with a thin
// entrepo.EntRepository adapter (see the apikey example + the codegen reference):
func NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*APIKey, string]
```

Both satisfy the same neutral `Repository` seam — it is backend-agnostic, so it favors neither
GORM nor ent:

```go
type Repository[T any, K comparable] interface {
    Get(ctx context.Context, key K) (T, error)
    List(ctx context.Context, opts ListOptions) (items []T, nextPageToken string, err error)
    Create(ctx context.Context, entity T) (T, error)
    Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error)
    Delete(ctx context.Context, key K) error
    Undelete(ctx context.Context, key K) (T, error) // AIP-149; ErrNotFound on hard-delete stores
}
```

## Database logger configuration

You own the `gorm.Open` call; the generated repository uses whatever `*gorm.DB` you pass to its
constructor, including that DB's logger. GORM's default logger writes every query — and every
"record not found" — to stderr, including the SQL, the bound values (tenant IDs included), and
the `file.go:line` of the call site.

{{< callout type="info" >}}
**That output never crosses the API boundary** — `seccheck.AssertErrorMessagesClean` still passes.
However, it is noisy in development and leaks internals into server logs in production.
{{< /callout >}}

Set the logger explicitly when you open the connection:

```go
import (
    "gorm.io/gorm"
    "gorm.io/gorm/logger"
)

db, err := gorm.Open(dialector, &gorm.Config{
    // Silence the SQL/notfound spam; or use logger.Default.LogMode(logger.Warn)
    // to keep slow-query + error logs without the per-query firehose.
    Logger: logger.Default.LogMode(logger.Silent),
})
```

To route GORM through your service's `slog`/structured logger instead, implement
`logger.Interface` (or adapt one of the community bridges) and pass it as `Logger`. The repository
inherits it for every query.

## Wiring a shape to the persistence seam

There are two ways to connect a shape to the rest of your service:

1. **Behind the `Repository` seam** — wrap the generated client in a type that implements
   `Repository[T,K]`. Service code depends only on the seam, so the shape can change locally
   without touching callers. Both generated repositories already do this, and it is the right
   approach for plain CRUD.
2. **Directly** — use the shape's generated client where its capabilities matter (ent's graph
   queries or privacy rules). The SDK does not force a lowest-common-denominator: it provides the
   connection and migration conventions and gets out of the way.

## What the SDK provides for every shape

Regardless of which shape you choose, the SDK provides:

- **Connection convention** — the `persistence.DSN` abstraction, including devedge's indirect
  *hotload* form (`fsnotify://<driver>/<abs-path>` + a real DSN file), so rotated credentials
  reload without a restart.
- **A neutral seam and dev store** — `Repository[T,K]` plus an in-memory `MemoryRepository` for
  the common CRUD case and for tests.
- **Migrations** — schema migrations use `infobloxopen/migrate` (the org-standard fork)
  regardless of shape.

See the [persistence reference](../../../reference/persistence/) for the full API.
