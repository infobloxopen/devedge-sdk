# F042 — Cross-service GraphQL federation gateway + runnable sample app (WS-021 P3)

**AIPs**: AIP-122 (resource names), AIP-124 (`google.api.resource_reference`), AIP-137 (BatchGet), AIP-157 (read_mask)
**Status**: CLARIFIED — D-3 locked (eager per-collection preload) · WS-021 **P3** (reactivates the previously-DEFERRED Approach A — the user's `/goal` now requires the GraphQL seam) · 2026-07-02

> **Guiding principle (locked, SDK convention):** clean implementation, no backward compatibility (pre-1.0). Dependency-light ROOT: the GraphQL library is a transitive dep, so the gateway lives in its **own module** (`federationgql/`), never the root — `check-graph-isolation` must stay green.

**Extends**: F041 (`reference` seam — `Reference`, `ReferenceResolver`, `BatchGetter`, `Load`; `<Svc>References` metadata; guaranteed `BatchGet`), F029 (default CRUD handlers), WS-011 (multi-module isolation), apx `pkg/typeresolver` (WP-A catalog-backed resolution).
**Origin**: WS-021 `/goal` (2026-07-02) — "ship the rest of this with an end-to-end verification with a sample app that tests the cross-microservice GraphQL seam; ensure there are skills in our repo that help agents set that seam up if not automatic." P1 (primitives) shipped in devedge-sdk v0.45.0 + apx v0.14.0; this is the consumer that turns the primitives into a working GraphQL seam.

---

## Problem statement

F041 made services **federatable** — a field declares a cross-service reference (`google.api.resource_reference`), `protoc-gen-svc` emits `<Svc>References` metadata, referenced targets guarantee `BatchGet`, and `reference.Load` resolves N parents' references in exactly ONE BatchGet. But there is **no consumer** that turns those primitives into a queryable graph: a UI still cannot issue one query that spans microservice moats. This feature builds the **catalog/metadata-driven GraphQL federation gateway** (Approach A from the WS-021 proposal): each resource becomes a GraphQL type, each cross-service reference becomes a GraphQL **edge** resolved via `reference.Load` (one BatchGet per collection), authz-transparent, and proves it with a **runnable sample app** + an **end-to-end test** that queries across two real service processes.

## Goals

- **G-1 (gateway seam, isolated module).** A new `federationgql/` module builds a `graphql.Schema` from a set of resource descriptors + a `reference.ReferenceResolver`. Each resource → a GraphQL object type; each `reference.Reference` → an object-typed GraphQL field resolved through `reference.Load`. Root query exposes per-resource `get(id)` + `list`. The GraphQL library dep is confined to this module (root graph unchanged; `check-graph-isolation` green).
- **G-2 (one BatchGet per collection — the anti-N+1 guarantee, end to end).** Resolving a reference edge across N parents in a single GraphQL query issues **exactly one** `BatchGet` to the target service (DataLoader-style batching or eager per-collection preload over `reference.Load`). This is F041 AC-5, now proven **across a real service boundary through GraphQL**.
- **G-3 (authz-transparent).** The gateway makes **zero** authz decisions. It propagates the caller's identity/context unchanged into every downstream service call; a per-service `PermissionDenied` surfaces as a per-field GraphQL error (`null` + `errors[]`), never a bypass. `read_mask` (AIP-157) is pushed down from the GraphQL selection set where the descriptor supports it.
- **G-4 (catalog-backed resolver).** The gateway resolves a `Reference.TargetType` to a downstream `BatchGetter` via a `reference.ReferenceResolver`. The sample wires a resolver whose clients dial the real services; the apx `pkg/typeresolver` (WP-A) is the catalog-backed `type → module` lookup behind it (replacing F041's static-only resolver).
- **G-5 (runnable sample app + e2e).** A self-contained example (`examples/graphql-federation/`) with **two microservices** (`region`, `asset`; asset references region) + a **gateway binary**, runnable by a human (start services + gateway → curl a GraphQL query that traverses `asset → region`). An **automated e2e test** starts both services + the gateway over real listeners and asserts: (a) a cross-service query returns composed data; (b) exactly one `BatchGet` hits the region service for N assets; (c) a `read_mask`-narrowed selection fetches only requested fields; (d) a request with no principal is denied per-service (authz not bypassed).
- **G-6 (a SKILL to set the seam up).** Because wiring the gateway is **not fully automatic** (register services, map references to edges, wire the resolver), add a repo skill (`.claude/skills/setup-graphql-federation/`) that walks an agent through standing up the seam for a new pair of services, plus a reference doc. This is the `/goal`'s "skills that help agents set that seam up" requirement.

## Non-goals

- **Auto-generating the entire schema from the apx catalog with zero wiring** — the descriptor/registration step stays explicit (hence the skill, G-6). Full codegen of the gateway from the catalog is a later increment.
- **GraphQL mutations across aggregates** — reads compose; the DDD write boundary holds (mutations route to the owning service). Mutations, subscriptions, and cross-domain filtering (→ Search seam, WS-014 P5) are out of scope.
- **Apollo-federation-native subgraphs (A1)** — this is the mesh/gateway shape (A2); A1 stays deferred.
- **A new datastore / response cache** — the gateway is stateless composition.

## Design decisions (★ = confirm in Clarify)

- **D-1 (module + library — LOCKED).** New module `github.com/infobloxopen/devedge-sdk/federationgql`, added to `go.work` `use` + `scripts/release.sh` module set + a CI `GOWORK=off` test step (WS-011 pattern). GraphQL library: **`github.com/graphql-go/graphql`** (programmatic runtime schema — fits a metadata-driven gateway; pure Go, single dep). Confined to this module.
- **D-2 (descriptor API).** The gateway is built from explicit descriptors:
  ```go
  type Resource struct {
      Type       string            // AIP-122 type, e.g. "asset.example.com/Asset"
      Name       string            // GraphQL type name, e.g. "Asset"
      Scalars    []ScalarField     // id + scalar fields (+ Resolve from source)
      References []reference.Reference // from the generated <Svc>References
      Get        func(ctx, id string) (any, error)
      List       func(ctx, ListArgs) ([]any, error)
      IDOf       func(any) string
      RefIDs     func(reference.Reference, source any) []string
  }
  func NewSchema(resources []Resource, resolver reference.ReferenceResolver) (graphql.Schema, error)
  func Handler(schema graphql.Schema) http.Handler // propagates request context (principal)
  ```
- **D-3 (batching mechanism — LOCKED, Clarify 2026-07-02: eager per-collection preload).** The `list`/`get` resolver runs `reference.Load` for each declared reference and stashes the resolved targets in a request-scoped cache (keyed by target type + id) that the edge field resolver reads. Deterministically **one BatchGet per referenced collection** for the list→edge shape. A per-request DataLoader (thunk-based, for arbitrary query depth) is a later generalization; not this cut. The e2e (G-5b / AC-3) asserts the single BatchGet with a spy.
- **D-4 (authz propagation — LOCKED).** Resolvers receive the GraphQL execution `context.Context` carrying the caller's principal/metadata; downstream clients forward it (gRPC metadata / HTTP headers). The gateway never constructs or elevates a principal. A downstream denial → that field resolves to `null` with a GraphQL error entry.
- **D-5 (read_mask pushdown).** When a descriptor's `Get`/`List`/`BatchGet` accept a field mask, the gateway derives it from the GraphQL selection set (the requested scalar fields) and passes it, so the downstream service honors AIP-157. Where a descriptor doesn't support it, the gateway fetches full and projects in-memory (documented limitation).
- **D-6 (sample app shape — LOCKED).** `examples/graphql-federation/`: `region/` service (Region resource), `asset/` service (Asset with `google.api.resource_reference` → Region), `gateway/` main wiring both + the resolver, and an `e2e_test.go`. Services generated via the SDK toolchain (proving annotation → metadata → BatchGet → gateway end to end), sqlite-backed, real listeners.

## Acceptance criteria

- **AC-1 (schema builds).** `NewSchema` produces a valid GraphQL schema with `Asset`/`Region` types, `Asset.region: Region` edge, and root `assets`/`asset(id)`/`regions`/`region(id)` fields. Unit-tested with in-process fakes.
- **AC-2 (cross-service query, e2e).** With region + asset services + gateway running over real listeners, `{ assets { id, region { id, name } } }` returns each asset with its composed region. e2e test.
- **AC-3 (one BatchGet — the keystone).** For N assets referencing M distinct regions, the region service receives **exactly one** `BatchGet` of the M ids (spy/interceptor on the region service asserts call count == 1; a per-row regression fails it). e2e.
- **AC-4 (read_mask).** `{ assets { region { name } } }` causes the region fetch to request only `name` (+id) — asserted via the region service seeing the narrowed mask. e2e.
- **AC-5 (authz not bypassed).** A GraphQL request carrying no principal has its `region` edge (and/or `assets`) denied by the region/asset service's fail-closed interceptor → GraphQL returns `null` + an error, never composed data. A request with a valid principal succeeds. e2e.
- **AC-6 (isolation).** `check-graph-isolation` stays green: the ROOT module graph does not gain the GraphQL library; it is confined to `federationgql/`. The new module builds `GOWORK=off`.
- **AC-7 (runnable sample).** `examples/graphql-federation/` runs via a documented command (a `Makefile`/README): start services + gateway, curl the GraphQL endpoint, get composed data. A human can reproduce the seam.
- **AC-8 (skill).** `.claude/skills/setup-graphql-federation/SKILL.md` exists and walks an agent through wiring the seam for a new service pair (declare reference → ensure BatchGet → describe resources → register + resolver → serve), referencing the sample. A reference doc (`docs/.../graphql-federation.md`) documents the seam.

## Failure modes to cover

- **N+1 regression** → AC-3 spy asserts exactly one BatchGet; a per-row resolver fails the test.
- **Missing resolver for a target type** → `reference.Load` already fails loud; the gateway surfaces it as a GraphQL error, not a silent null.
- **Authz bypass** → AC-5 proves per-service denial propagates; the gateway holds no allow path.
- **Root dep leak** → AC-6 `check-graph-isolation`; the GraphQL lib must not appear in the root/module-graph of a non-federationgql consumer.
- **Partial failure** → one downstream error yields a per-field GraphQL error + partial data, not a whole-query failure (GraphQL null-propagation semantics).

## Phasing (tasks.md during Plan)

1. `federationgql` module scaffold (go.mod, go.work, release.sh, CI step) + `NewSchema`/`Handler` over descriptors + edge resolution via `reference.Load` (D-3b) — AC-1/AC-6.
2. `examples/graphql-federation/` two services (region, asset) + gateway main + resolver wiring (dialing clients) — AC-7.
3. `e2e_test.go`: real listeners, cross-service query, single-BatchGet spy, read_mask, authz — AC-2/3/4/5.
4. Skill `setup-graphql-federation` + reference doc — AC-8.

**Next gate**: Clarify (D-3 batching), then Plan → implement. Ship: merge + synchronized release (new module joins at the next version).
