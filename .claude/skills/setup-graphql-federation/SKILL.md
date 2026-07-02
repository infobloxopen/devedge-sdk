---
name: setup-graphql-federation
description: Stand up the cross-service GraphQL federation seam — a graph that composes resources across microservice moats with one BatchGet per collection and per-service authz preserved. Use when a UI needs one resource plus its references in other services.
---

# Set up the cross-service GraphQL federation seam

## What this seam is

devedge microservices are "moats" (DDD aggregate roots). A UI often needs one resource plus its
references in *other* services. The `federationgql` module builds a single GraphQL schema where each
resource is a type and each **cross-service reference** is a GraphQL edge, resolved through the P1
`reference` seam so N parents cost **one** `BatchGet` (not N). It is **authz-transparent** — the
gateway makes zero authz decisions; it propagates the caller's principal and each service still
enforces (a denial becomes a per-field `null` + GraphQL error, never a bypass).

Setting it up is **not fully automatic**: you register the resources, map references to edges, and
wire the resolver. This skill is that procedure. The worked example is
`examples/graphql-federation/` (region + asset services + a gateway + an e2e test) — read it alongside
these steps; it is the canonical copy-from source.

## Prerequisites

- The source resource declares the reference on its foreign-key field:
  `[(google.api.resource_reference) = { type: "<target-service>.example.com/<Kind>" }]` (AIP-124).
  `make generate` then emits a `<Svc>References` table (`[]reference.Reference`) and **guarantees
  `BatchGet`** on the target resource (F041). A referenced target that lacks `BatchGet` fails codegen —
  that is the fail-loud gate, not a bug to work around.
- The gateway lives in its **own module** (`federationgql`), and the GraphQL library
  (`github.com/graphql-go/graphql`) must stay there — never import `federationgql` from a service, or
  `check-graph-isolation` breaks. Build the gateway as a separate process.

## Steps

**1. Confirm the primitives.** After `make generate`, the source service exposes `<Svc>References` and
the target exposes `BatchGet` / `:batchGet` (backed by a `persistence.BatchRepository`).

**2. Build a `reference.BatchGetter[T]` per target type.** Wrap each target's BatchGet client so it
satisfies `BatchGet(ctx, ids []string) ([]T, error)` in ONE backend call (never a per-id loop). To
honor `read_mask`, have the getter read `federationgql.ReadMaskFromContext(ctx, targetType)` and push
the mask downstream (a `read_mask` request field, or gRPC metadata `read-mask` — the sample uses
metadata because the fixture's request has no mask field).

**3. Register the getters in a `reference.ReferenceResolver`.** Wrap each typed getter with
`federationgql.AnyGetter(g)` (adapts `BatchGetter[T]` → `BatchGetter[any]`) and register it:
```go
r := reference.NewStaticResolver()
r.Register("region.example.com/Region", federationgql.AnyGetter(regionGetter))
```
For catalog-driven production, back the resolver with apx `pkg/typeresolver.Resolve` (the
`type → module` index, WP-A) so the target endpoint is *discovered* from the type, not hard-coded.

**4. Describe each resource** as a `federationgql.Resource`:
- `Type` — the AIP-122 resource type (matches `google.api.resource.type`).
- `Name` — the GraphQL type name.
- `Scalars` — `[]federationgql.ScalarField{ Name, Type (*graphql.Scalar), Resolve func(any) any, MaskPath }`.
  `MaskPath` maps a GraphQL field to its proto field so the selection set derives the right `read_mask`
  (e.g. GraphQL `name` → proto `display_name`).
- `References` — from the generated `<Svc>References` (each becomes an edge to the target's type).
- `Get func(ctx, GetArgs) (any, error)` / `List func(ctx, ListArgs) ([]any, error)` — call the owning
  service (they receive `ReadMask` derived from the selection).
- `IDOf func(any) string`, `RefIDs func(reference.Reference, any) []string` (extract a reference's FK
  id(s) from a parent), and optional `EdgeFieldName func(reference.Reference) string`.

**5. Build + serve.** `schema, err := federationgql.NewSchema(resources, resolver)` →
`http.Handle("/graphql", federationgql.Handler(schema))`. The handler runs resolvers with the
**request `context.Context`** — make sure the caller's principal/auth metadata is on it (an
interceptor/middleware), because the gateway forwards it downstream unchanged and never elevates it.

**6. Verify the seam** (mirror `examples/graphql-federation/e2e_test.go`):
- `{ <parents> { <scalar>, <edge> { <target scalar> } } }` returns composed data across services;
- the target service receives **exactly one** `BatchGet` for the whole collection (assert with a spy —
  a per-row regression must fail the test);
- a `{ <edge> { <one field> } }` selection narrows the downstream `read_mask`;
- a request with **no principal** is denied per-service (`null` + error), a valid one succeeds.

## Known gotchas

- **One BatchGet per collection comes from `reference.Load`** (eager per-collection preload keyed by
  `(target type, id)`). Resolve edges through the gateway's preload; a per-row `Get` in an edge
  resolver reintroduces the N+1 the seam exists to kill. Two references sharing a target type still
  fetch each collection's ids — don't dedup by target type alone.
- **Authz is never the gateway's job** — it holds no allow path. A downstream `PermissionDenied` →
  per-field `null` + GraphQL error. Do not construct or elevate a principal in a resolver.
- **Keep `graphql-go` out of service modules** — only the gateway module imports `federationgql`;
  `check-graph-isolation` guards the root graph.
- **A cross-service reference resolves at the gateway, not via the same-server reference gate.** In a
  genuine service split the source service does not co-locate the target's `BatchGet`, so register it
  without `RecordReferences` (the gate is a co-located-deployment backstop) — see the sample's
  `internal/svc`.
- **No cross-aggregate mutations through the graph** — reads compose; writes route to the owning
  service's root (the DDD write boundary holds). Mutations/subscriptions are out of scope.
- **Cross-domain *filtering*** ("parents whose target's field = X") is a join across moats — that is
  the Search seam (WS-014 P5), not this gateway.

## Reference

- Worked example: `examples/graphql-federation/` (region + asset + gateway + `e2e_test.go`; run with
  `cd examples/graphql-federation && go run ./gateway`, then curl `/graphql`).
- Package doc: `docs/content/docs/reference/graphql-federation.md`.
- Foundations: the `reference` package (F041, `reference.Load`), apx `pkg/typeresolver` (WP-A), the
  WS-021 proposal.
