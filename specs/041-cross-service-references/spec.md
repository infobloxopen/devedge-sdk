# F041 — Cross-service resource references + guaranteed BatchGet (federatability primitives, WS-021 P1)

**AIPs**: AIP-122 (resource names), AIP-124 (resource references), AIP-137 (batch / `BatchGet`), AIP-157 (read_mask)
**Status**: CLARIFIED — all forks locked 2026-07-01 (ready for Plan) · WS-021 **P1 / WP-B**

> **Guiding principle (locked, per SDK convention):** clean implementation, **no backward
> compatibility** — the SDK is pre-1.0 with no real users. Reference metadata is additive; do not
> preserve superseded patterns.

**Extends**: F010 (`protoc-gen-svc`), F027 (generated repository adapter / `BatchRepository`), F029 (default CRUD handlers + auto-wired rules), WS-005 (canonical schema via apx), WS-019 (`apilayout`)
**Depends on (WP-A, apx — NOT this repo):** an apx catalog **`type → module → endpoint`** resolution (the "canonical via apx" piece of ratified D3, refined by D-1). **No new annotation** — cross-service references reuse the standard **`google.api.resource_reference`** (AIP-124), already available via the `google/api` imports. Consequently, **metadata emission here does NOT block on WP-A** (the annotation already exists); only endpoint *resolution* (P2 expansion) consumes the catalog index. Tracked in hub WS-021; specify WP-A in `apx` (`.specify/`).
**Origin**: WS-021 (hub `specs/cross-service-federated-querying-proposal.md`) — user question: devedge exposes REST APIs from microservices; UIs need to query across moats (domains) of objects. This feature is the **substrate**: declare a cross-service reference once + guarantee it can be batch-fetched, so cross-moat composition (P2 REST `?expand=`, and a deferred GraphQL gateway) is possible with **one round trip per collection** and **no N+1**.

---

## Problem statement

A uFE view spans moats: *asset* (assetd) + its *policy* (policyd) + owner *identity* (iam). Today a
resource can reference another **within its own aggregate** via `infoblox.field.v1` containment, and
can name **another local aggregate** via `infoblox.ddd.v1.references` (`{aggregate, foreign_key}`,
ext 50012) — but that annotation is deliberately **edge-less** (a scalar FK, no traversable Go edge,
no cascade — so code cannot walk or mutate across roots) and names a **local message**, not a
catalog-resolvable cross-**service** resource type. There is therefore **no way to declare that a
field points at a resource served by another microservice**, and no metadata a composition layer
could read to resolve it.

Second, `BatchGet` (AIP-137, `persistence.BatchRepository`, `GET /…:batchGet`) **exists but is
optional** — a resource may not expose it. Any cross-moat fetch that resolves references one-at-a-time
is an N+1 waterfall. Efficient composition requires a **guarantee**: if a type is referenced, it can
be batch-fetched.

This feature adds exactly those two primitives on the devedge-sdk side (WP-B). It does **not** build a
consumer (REST `?expand=` is P2/F042; the GraphQL gateway is deferred by WS-021 D1) and it does
**not** define the annotation (WP-A, apx/apis).

---

## Goals

- **G-1 (recognize + emit metadata).** `protoc-gen-svc` recognizes the canonical cross-service
  reference annotation on a proto field and emits machine-consumable **reference metadata** — source
  field → `{ target resource type, target module, foreign-key field, cardinality }` — into the
  generated Go package (e.g. a `<Svc>References` registry), readable at runtime by a composition layer.
- **G-2 (guarantee BatchGet on referenced targets).** Any resource that is the **target** of a
  cross-service reference exposes `BatchGet(names[])` over gRPC **and** REST (`:batchGet`), backed by
  `persistence.BatchRepository` (the ent/GORM path from F027/WS-005) and the F029 default handler —
  "referenced ⇒ batch-fetchable" holds by construction.
- **G-3 (fail-loud, never a silent N+1).** A cross-service reference whose resolved target type does
  **not** expose `BatchGet` is a **build/registration-time error** with a clear message — never a
  runtime per-row fetch.
- **G-4 (preserve the moat/write boundary).** Reference metadata is **metadata only**: no Go graph
  edge, no persistence change — the field stays a scalar FK (restrict/SetNull, never cascade). Code
  still cannot walk or mutate across aggregate roots; only a composition layer *above* the services
  reads the metadata to fetch (reads compose; writes route to the owning root).
- **G-5 (authz + read_mask on the batch path).** `BatchGet` flows through the same fail-closed authz
  interceptor (verb `read`) and `read_mask` middleware (AIP-157) as `Get`/`List`; row-level
  `Obligations` apply. The batch read path is not a privilege or projection escape hatch.

## Non-goals

- **REST `?expand=` link-expansion** — the committed WS-021 consumer, but a **separate feature (P2 /
  F042)**. This feature only produces the metadata + BatchGet guarantee it will consume.
- **The GraphQL federation gateway** — deferred by WS-021 D1 (enabled, not built).
- **Cross-domain filtering / joins** (e.g. "assets whose owner's region = X") — routed to the Search
  seam (WS-014 P5), not this substrate.
- **Owning the catalog `type → module → endpoint` resolution** — that is WP-A (`apx`); this feature
  **consumes** it. There is **no new annotation to define** (D-1: reuse `google.api.resource_reference`).
- **Cross-aggregate mutations or traversable Go edges** — explicitly preserved-against (G-4).
- **Changing the `persistence.Repository`/`BatchRepository` contract or the proto/API shape** beyond
  guaranteeing `BatchGet` generation on referenced targets.

---

## Design decisions (★ = confirm in Clarify)

- **D-1 (annotation shape — LOCKED, Clarify 2026-07-01: reuse Google).** Cross-service references
  reuse the standard **`google.api.resource_reference { type }`** (AIP-124) on the FK field — **no new
  `infoblox.ref.v1` annotation**. `type` is the AIP-122 resource type (matches `google.api.resource.
  type`); the target **module/endpoint** is resolved from the apx catalog **`type → module` index**
  (WP-A), since AIP-122 types are globally unique — so `module` is *not* annotated. This keeps the
  surface idiomatic (devedge already uses `google.api.resource`) and reduces "canonical via apx" (D3)
  to *canonicalizing the catalog index*, not releasing a new schema. The generator reads an internal
  `{ targetType, fkField }` view (module catalog-resolved at use).
- **D-2 (metadata emission).** `protoc-gen-svc` emits a generated `<Svc>References` value (or a package
  registry) mapping each annotated field's path → `{ targetType, module, fkField, cardinality }`,
  `DO NOT EDIT`, in the proto's Go package — the same emission style as `<Svc>AuthzRules` (F029 D-3).
- **D-3 (BatchGet guarantee scope — LOCKED, Clarify 2026-07-01: targets-only + opt-in).** Guarantee
  generated `BatchGet` on **reference-target resources** (the minimum needed), with an **opt-in to
  force-all**. Generation extends F027/WS-005 repository codegen + the F029 default handler
  (`BatchGet<R>` method + `:batchGet` route + `read` rule).
- **D-4 (fail-loud gate — LOCKED, Clarify 2026-07-01: both).** A referenced target lacking `BatchGet`
  fails at **codegen** (earliest, before a binary exists — the **primary** gate) **and** at
  **registration/`Serve`** (the backstop that catches cross-repo/version skew local codegen can't see).
- **D-5 (no edge / persistence unchanged — locked).** Metadata only. The referenced field stays a
  scalar FK; no `ent`/GORM edge, no cascade; no traversable Go accessor across roots. Mirrors the
  `ddd.v1.references` invariant (G-4).
- **D-6 (authz/read_mask on batch path — locked).** `BatchGet<R>` carries an `(infoblox.authz.v1.rule)
  = {verb:"read", resource:"…"}` and passes through the existing `ReadMaskUnary` + authz interceptors;
  `Obligations` (row filters) apply. No new authz surface (G-5).
- **D-7 (resolver seam — LOCKED, Clarify 2026-07-01: define now).** Define a small `ReferenceResolver`
  seam in this feature (`targetType → a BatchGet-capable client`) with a **static/in-process**
  implementation for the two-service fixture; the **catalog-backed** implementation lands with WP-A/P2.
  Defining the interface now lets AC-5 be proven without the catalog.

---

## Acceptance criteria

- **AC-1 (metadata).** A proto field carrying a cross-service reference generates reference metadata
  naming the **target resource type + module** (+ fkField, cardinality); a unit test asserts the
  emitted `<Svc>References` registry.
- **AC-2 (BatchGet guaranteed).** A resource that is a reference target serves `BatchGet` over gRPC
  **and** REST (`:batchGet`), backed by `BatchRepository`, honoring `read_mask` and the `read` authz
  rule; existing fixtures stay green.
- **AC-3 (fail-loud).** A cross-service reference whose target type lacks `BatchGet` fails
  codegen/registration with a clear, actionable error — proven by a fixture. Never a silent runtime
  per-row fetch.
- **AC-4 (write boundary preserved).** The referenced field remains a scalar FK with **no** traversable
  Go edge and **no** cascade; a test asserts no cross-aggregate mutation/traversal path is generated
  (parity with `ddd.v1.references`).
- **AC-5 (one BatchGet per collection — the anti-N+1 proof).** A two-service fixture: service **A**'s
  resource references service **B**'s resource. A composition fetches **N** A's, then resolves their B
  references in **exactly one** B `BatchGet` call (DataLoader-style batch), honoring `read_mask` +
  per-service authz. The test asserts **one** BatchGet invocation for N references — a per-row fetch
  fails it. Runs on **ent and GORM**.
- **AC-6 (docs).** `reference/codegen.md` documents the cross-service reference annotation, the emitted
  metadata, the BatchGet guarantee, the fail-loud rule, and the write-boundary invariant.

## Failure modes to cover

- **Reference to an unpublished/renamed target type** → resolution fails **loud at build/registration**,
  not at request time (D-4).
- **Target lacks BatchGet** → fail-loud (AC-3), never a silent N+1.
- **N+1 regression** → AC-5 asserts a single BatchGet for N references; a per-row resolver fails the
  test.
- **Accidental Go edge / cascade** introduced by the reference → AC-4 guards (metadata-only, D-5).
- **Authz/projection bypass on the batch path** → `BatchGet` must carry a `read` rule (F029
  completeness gate covers it) and honor `read_mask`/`Obligations` (D-6).
- **Annotation coverage** → recognize `google.api.resource_reference` only on scalar FK fields of
  resource messages; a reference on a non-resource message or a message-typed field is a codegen error
  (not silently ignored).

---

## Phasing (to be detailed in tasks.md during Plan)

1. **[unblocked — the annotation already exists]** Recognize `google.api.resource_reference` on FK
   fields of resource messages in `protoc-gen-svc`; emit `<Svc>References` metadata
   (`{targetType, fkField, cardinality}`; `module` catalog-resolved at use) (pure, unit-tested) — AC-1.
2. Guarantee `BatchGet` on reference-target resources: extend F027/WS-005 repository codegen + the F029
   default handler (`BatchGet<R>` + `:batchGet` + `read` rule); fail-loud check (D-3/D-4) — AC-2/AC-3.
3. `ReferenceResolver` seam + static impl (D-7); the two-service composition fixture on **ent + GORM**
   — AC-4/AC-5.
4. Docs (AC-6) + a dogfood two-service composition.

**Upstream coordination:** WP-A is now *only* the apx catalog `type → module → endpoint` resolution
(D-1 locks the annotation as standard `google.api.resource_reference`, so **no schema release**). It
proceeds in parallel; WP-B steps 1–3 do **not** block on it — the P1 fixture uses the static resolver
(D-7), and catalog-backed endpoint resolution arrives with P2.

**Resolved (Clarify 2026-07-01):** D-1 reuse `google.api.resource_reference` (no new annotation;
`module` catalog-resolved); D-3 BatchGet on reference-targets + opt-in force-all; D-4 fail-loud at
codegen (primary) + `Serve` (backstop); D-7 define the `ReferenceResolver` seam now with a static
impl. **Next gate:** Plan (`tasks.md`, tasks tagged `[S]`/`[C]`).
