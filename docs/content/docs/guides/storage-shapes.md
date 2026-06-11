---
title: Storage Shapes
weight: 2
---

The SDK is **resource-oriented and AIP-aligned**: the API contract (resources, standard methods,
field masks, filtering, pagination) is primary. *How* a service stores those resources is a
secondary, swappable concern — so the SDK **does not impose an ORM** and has **no ORM
dependency**.

A "shape" is how entities and queries are modeled and generated. A service picks the one that
fits; none is mandated.

## The shapes

| Shape | Source of truth | What you get | When |
|---|---|---|---|
| **proto → GORM** (`protoc-gen-storage`) | the `.proto` resource | generated ORM model + CRUDL `Repository` | lowest-friction when the proto already defines the resource |
| **ent** ([entgo.io](https://entgo.io)) | a Go ent schema | a type-safe client; **graph edges/traversal**, hooks, and a **privacy layer** | rich domain graphs, relationship-heavy data; privacy layer pairs well with the authz seam |
| **sqlc** | hand-written SQL | compile-time-safe query code, no reflection | hand-tuned / performance-critical paths (the escape hatch) |
| **hand-written** | your code | implement `Repository[T,K]` directly | small/simple stores, or wrapping any of the above |

The SDK ships generators for the first two (`protoc-gen-storage`, `protoc-gen-ent`). sqlc and
hand-written are conventions, not generators.

## GORM vs ent — how to choose

| Question | Lean GORM | Lean ent |
|---|---|---|
| Is the proto the natural source of truth? | yes — generate straight from it | maybe — ent schema is separate Go |
| Plain per-resource CRUD? | yes | overkill |
| Relationship-heavy graph (edges, traversals)? | awkward | **yes** — ent's strength |
| Want a query-level **privacy layer** alongside authz? | no | **yes** |
| Want hooks on mutations? | callbacks | **first-class** |
| Minimize new concepts for the team? | **yes** | learning curve |

Both enforce **tenant isolation** the same way — every query is scoped by `account_id` from
`middleware.TenantIDFromContext(ctx)` (see [Tenant Isolation](../../concepts/tenant-isolation/)).

## Constructor signatures

The generated constructors differ only by whether the message has secret fields (which adds an
`Encryptor`):

```go
// GORM — protoc-gen-storage
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
// (without secret fields it would be: func NewWidgetRepository(db *gorm.DB) *WidgetRepository)

// ent — protoc-gen-ent generates the schema; the SDK wiring exposes:
func NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*APIKey, string]
```

Both satisfy the same neutral seam:

```go
type Repository[T any, K comparable] interface {
    Get(ctx context.Context, key K) (T, error)
    List(ctx context.Context, opts ListOptions) (items []T, nextPageToken string, err error)
    Create(ctx context.Context, entity T) (T, error)
    Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error)
    Delete(ctx context.Context, key K) error
}
```

## Two ways to plug a shape in

1. **Behind the neutral seam** — wrap the generated client in a type that implements
   `Repository[T,K]`. Service code depends only on the seam, so the shape can change locally
   without touching callers. Good for plain CRUD. (Both generated repositories already do this.)
2. **Directly** — use the shape's generated client where its capabilities matter (ent's graph
   queries or privacy rules). The SDK does **not** force a lowest-common-denominator; it gives
   you the connection + migration conventions and gets out of the way.

## What the SDK provides regardless of shape

- **Connection convention** — the `persistence.DSN` abstraction, including devedge's indirect
  *hotload* form (`fsnotify://<driver>/<abs-path>` + a real DSN file), so rotated credentials
  reload without a restart.
- **A neutral seam + dev store** — `Repository[T,K]` plus an in-memory `MemoryRepository` for the
  common CRUD case and for tests.
- **Migrations** — schema migrations use `infobloxopen/migrate` (the org-standard fork)
  regardless of shape.

See the [persistence reference](../../reference/persistence/) for the full API.
