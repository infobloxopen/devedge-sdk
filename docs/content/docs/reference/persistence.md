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
}
```

The neutral seam. Its method set matches the API verb vocabulary (get/list/create/update/delete),
so service code can depend on it and swap the underlying shape (GORM, ent, sqlc, hand-written)
without changes. The generated GORM and ent repositories both satisfy it for their resource type
(`Repository[*APIKey, string]`).

## ListOptions

```go
type ListOptions struct {
    Filter    string
    PageSize  int
    PageToken string
    // (order/filter parameters for resource-oriented listing)
}
```

Resource-oriented list parameters: filter, page size, and an opaque page token. Generated
repositories default `PageSize` to 50 and encode the next offset as a base64 page token.

## Errors

```go
var ErrNotFound = errors.New("persistence: not found")
```

Returned by `Get` (and the generated `LookupBy<Field>Hash`) when no record matches. Map it to
`codes.NotFound` at the gRPC boundary — that mapping is what makes cross-tenant reads look like
"does not exist", which is exactly what [tenant isolation](../../concepts/tenant-isolation/)
requires.

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
