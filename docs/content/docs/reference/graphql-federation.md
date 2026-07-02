---
title: graphql-federation
weight: 8
---

```go
import "github.com/infobloxopen/devedge-sdk/federationgql"
```

Package `federationgql` is the cross-service GraphQL federation gateway. It turns the
[`reference`](../persistence/) cross-service primitives — the generated `<Svc>References`
metadata and the guaranteed AIP-137 `BatchGet` on referenced targets — into one queryable
graph: each resource becomes a GraphQL object type, each declared
`google.api.resource_reference` becomes an object-typed **edge**, and a client issues a single
GraphQL query that spans microservice boundaries.

{{< callout type="info" >}}
`federationgql` lives in its **own Go module** so the GraphQL runtime library
(`github.com/graphql-go/graphql`) stays a transitive dependency — a server-only consumer's
module graph never gains it (`make check-graph-isolation`). Import it only in the gateway
binary, not in your services.
{{< /callout >}}

## What it does (and does not)

- **Composes reads across services.** A resource's cross-service reference edge is resolved
  through [`reference.Load`](#anti-n1), so N parents' references cost **one** `BatchGet` per
  referenced collection — the anti-N+1 guarantee, now proven across a real service boundary.
- **Makes zero authz decisions.** The gateway propagates the caller's execution context
  (principal / metadata) unchanged into every downstream call. A per-service `PermissionDenied`
  surfaces as a per-field GraphQL error (`null` + `errors[]`) — never a bypass.
- **Pushes `read_mask` down.** Where a descriptor supports a field mask, the gateway derives it
  from the GraphQL selection set (AIP-157) and passes it down so the service returns only the
  requested fields.
- **No mutations, no subscriptions, no cross-domain filtering.** Reads compose; the DDD write
  boundary holds (writes route to the owning service).

## The descriptor API

The gateway is built from explicit `Resource` descriptors — the wiring is deliberately not
auto-derived from a catalog (that registration step is what the
[setup skill](#setting-up-the-seam) walks you through).

```go
type Resource struct {
    Type       string                // AIP-122 type, e.g. "asset.example.com/Asset"
    Name       string                // GraphQL type name, e.g. "Asset"
    Scalars    []ScalarField         // id + scalar fields (with Resolve + optional MaskPath)
    References []reference.Reference  // from the generated <Svc>References table
    Get        func(ctx context.Context, args GetArgs) (any, error)
    List       func(ctx context.Context, args ListArgs) ([]any, error)
    IDOf       func(source any) string
    RefIDs     func(ref reference.Reference, source any) []string
    EdgeFieldName func(ref reference.Reference) string // optional edge-name override
}

type ScalarField struct {
    Name     string          // GraphQL field name (e.g. "id", "name")
    Type     *graphql.Scalar // defaults to graphql.String
    Resolve  func(source any) any
    MaskPath string          // downstream read_mask path (e.g. "display_name"); empty = no pushdown
}

type ListArgs struct {
    PageSize  int
    PageToken string
    ReadMask  []string // selection-set-derived field paths for pushdown
}

type GetArgs struct {
    ID       string
    ReadMask []string
}
```

## Building the schema and handler

```go
func NewSchema(resources []Resource, resolver reference.ReferenceResolver) (graphql.Schema, error)
func Handler(schema graphql.Schema) http.Handler
func Execute(ctx context.Context, schema graphql.Schema, query string, variables map[string]any) *graphql.Result
```

`NewSchema` builds a GraphQL object type per resource, one object-typed edge field per declared
reference (named after the target type, e.g. `Asset.region`), and a root `Query` with per-resource
`list` and `get(id)` fields (`assets` / `asset(id)` / `regions` / `region(id)`). It fails on an
empty/duplicate `Name`/`Type` or a reference whose `TargetType` names no registered resource.

`Handler` serves the schema over HTTP (POST `{query, operationName, variables}` or GET `?query=`).
It uses the incoming `*http.Request`'s context verbatim as the GraphQL execution context — so
whatever principal/metadata an upstream auth middleware placed on it flows into the resolvers and
their downstream clients. `Execute` is the programmatic entry the handler wraps.

## Wiring downstream clients {#anti-n1}

The gateway resolves reference edges through `reference.Load[any, any]`, which type-asserts the
resolver's registered client to a `reference.BatchGetter[any]`. Your service's generated BatchGet
client is a typed `reference.BatchGetter[*T]`; adapt it with `AnyGetter`:

```go
func AnyGetter[T any](g reference.BatchGetter[T]) reference.BatchGetter[any]
```

```go
resolver := reference.NewStaticResolver()
resolver.Register("region.example.com/Region",
    federationgql.AnyGetter[*regionv1.Region](regionBatchClient))

schema, err := federationgql.NewSchema(
    []federationgql.Resource{assetDescriptor, regionDescriptor},
    resolver,
)
```

### One BatchGet per collection (eager preload)

The `list`/`get` root resolver runs `reference.Load` for each declared reference **up front** and
stashes the resolved targets in a request-scoped cache; the edge field resolver reads from that
cache and performs no fetch of its own. So a list-of-N → edge query costs deterministically **one**
`BatchGet` per referenced collection.

### read_mask pushdown

```go
func ReadMaskFromContext(ctx context.Context, targetType string) []string
```

For the target of an edge, the gateway derives the `read_mask` from the edge's sub-selection and
stashes it on the context. A downstream client that honors AIP-157 reads it inside its `BatchGet`
via `ReadMaskFromContext` and pushes it down (as a proto `read_mask` field, gRPC metadata, or
however the service expects). A client that does not implement masking fetches full and the GraphQL
runtime projects the response.

## Authz transparency

Resolvers receive the GraphQL execution `context.Context` carrying the caller's principal/metadata;
the downstream clients forward it (gRPC metadata / HTTP headers). The gateway never constructs or
elevates a principal. A downstream denial resolves that field to `null` with a GraphQL error entry —
partial data plus `errors[]`, per GraphQL null-propagation semantics.

## Setting up the seam {#setting-up-the-seam}

Standing up the gateway for a new service pair is not fully automatic (register services, map
references to edges, wire the resolver). See the runnable sample at
[`examples/graphql-federation`](https://github.com/infobloxopen/devedge-sdk/tree/main/examples/graphql-federation)
— a `region` service, an `asset` service (whose `region_id` references `Region`), and a `gateway`
that wires both into one GraphQL endpoint — and its `e2e_test.go`, which asserts the cross-service
query, the single `BatchGet`, `read_mask` pushdown, and fail-closed authz over real listeners.
