# GraphQL federation gateway — runnable sample (F042 / WS-021 P3)

This sample stands up **two independent microservices** and a **GraphQL gateway**
that composes them into one queryable graph, so a single query traverses a
cross-service reference — `asset → region` — without an N+1 waterfall.

- **`region/`** — the `RegionService` (the reference **target**). It serves the
  guaranteed AIP-137 `BatchGetRegions`, which is what lets the gateway resolve
  many asset references in **one** call.
- **`asset/`** — the `AssetService` (the reference **source**). Each `Asset` has
  a `region_id` annotated with `google.api.resource_reference` → `Region`, so the
  generated `AssetServiceReferences` metadata declares the cross-service edge.
- **`gateway/`** — wires both services + a `reference.ReferenceResolver` into a
  `federationgql` schema and serves it over HTTP.

Both services are backed by ent + in-memory sqlite (no Docker), run on real
listeners, and enforce fail-closed authz — so a request with no principal is
denied per service, and the gateway never bypasses it.

> The two services reuse the F041 fixture's generated code
> (`testdata/federation`), so the sample exercises the real
> annotation → metadata → BatchGet → gateway path without re-running codegen.

## Run it (one command)

```bash
cd examples/graphql-federation
make run          # or: go run ./gateway
```

That starts the region + asset services in-process on ephemeral ports, seeds a
demo dataset (5 assets sharing 2 regions), and serves the GraphQL endpoint on
`:8080`. Then, in another shell:

```bash
make query        # curls the federated query below
```

or by hand:

```bash
curl -s localhost:8080/graphql \
  -H 'X-Account-Id: acme' \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ assets { id name region { id name } } }"}'
```

Response — each asset carries its composed region, resolved across the service
boundary in **one** `BatchGetRegions`:

```json
{"data":{"assets":[
  {"id":"a1","name":"web-01","region":{"id":"r1","name":"us-east"}},
  {"id":"a2","name":"web-02","region":{"id":"r2","name":"eu-west"}},
  {"id":"a3","name":"db-01","region":{"id":"r1","name":"us-east"}},
  {"id":"a4","name":"cache-01","region":{"id":"r2","name":"eu-west"}},
  {"id":"a5","name":"lb-01","region":{"id":"r1","name":"us-east"}}
]}}
```

Drop the `X-Account-Id` header and the request is denied per service — the
gateway holds no allow path:

```json
{"data":{"assets":null},"errors":[{"message":"... PermissionDenied ... assets ...","path":["assets"]}]}
```

## Run the services separately

The `region` and `asset` binaries run standalone too:

```bash
go run ./region -addr :9101   # prints its bound gRPC address
go run ./asset  -addr :9102
go run ./gateway -all-in-one=false -region :9101 -asset :9102
```

(In this mode you seed via the services' REST/gRPC APIs yourself; the all-in-one
mode above seeds for you.)

## What the end-to-end test proves

`go test ./...` (`make e2e`) starts region + asset + gateway over real listeners
and asserts, with a spy interceptor on the region service:

| AC | Assertion |
|----|-----------|
| AC-2 | `{ assets { id, region { id name } } }` returns composed data |
| AC-3 | 5 assets → 2 distinct regions costs the region service **exactly one** `BatchGet` (a per-row regression fails the test) |
| AC-4 | `{ region { name } }` narrows the pushed-down `read_mask` to `display_name` |
| AC-5 | a request with no principal is denied per service (null + error); a valid principal succeeds |

## Wiring the seam yourself

See `docs/content/docs/reference/graphql-federation.md` in the SDK for the
`federationgql` API, and the `internal/svc/gateway.go` file here for a worked
resolver + descriptor wiring.
