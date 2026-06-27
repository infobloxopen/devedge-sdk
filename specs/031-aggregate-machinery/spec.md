# F031 — DDD-safe aggregate machinery (SDK-owned annotations, AggregateRepository, fail-closed boundaries)

**AIPs**: AIP-121/122 (resource-oriented design, resource names), AIP-131–135 (standard methods), AIP-136 (custom methods), AIP-148/149 (soft-delete/Undelete), AIP-154 (etag).
**Status**: DRAFT — depends on **F030** (transaction seam) being merged. In-repo; no external dependency.
**Guiding principle**: opt-in + additive; fail-closed boundaries (mirror the authz completeness gate); clean core (no domain types in `persistence`/`authz`/`grpcauthz`); pre-1.0, no back-compat.
**Extends**: F030 (`TxRunner`/`Atomically`), F010 (`protoc-gen-svc`), F011 (`server` boot gate), F027 (repo adapter), F029 (default handlers + override).
**Origin**: the design thread on cross-entity invariants. F030 delivered atomicity; F031 delivers the *aggregate* abstraction and makes the bypass path fail closed.

---

## Problem statement

With F030, a developer can write atomically through the seam, but the SDK still has no notion of a **consistency boundary**: repositories are per-table, member resources are independently writable, and nothing detects a write that bypasses the aggregate root. F031 adds (1) a way to *declare* aggregate boundaries, (2) an `AggregateRepository` that loads/saves a cluster in one transaction guarded by the root's etag, and (3) a fail-closed gate that a declared member resource cannot register write handlers. Reads stay independently addressable (addressability ≠ write authority).

The annotations are **SDK-owned and generated in-repo** — see F030 *Annotation ownership*. Only modifying the shared published `field.v1` would need the external pipeline, and we avoid that by putting `references` in `ddd.v1`.

---

## Goals
- **G-1 (annotations).** SDK-owned `infoblox.ddd.v1`: message options `aggregate { root: bool }`, `member { root: string }`; field option `references { aggregate, foreign_key }`. Local `.pb.go` generation; scaffold mirror (drift-tested).
- **G-2 (AggregateRepository).** `AggregateRepository[Root, ID]` with `Load` (eager-load the cluster) and `Save` (persist the cluster in one `Atomically` tx; If-Match on the root etag). ent-shape implementation + a memory implementation (bespoke graph assembly).
- **G-3 (fail-closed boundary gate).** At `Serve`, `AssertAggregateBoundaries` denies a member resource that registers a write-capable standard method (Create/Update/Delete/Undelete/Batch*). Reads allowed. Member→root map accumulated via `Register<Svc>` like authz rules.
- **G-4 (member write-redirection).** `protoc-gen-svc` emits a member resource's write methods as `Unimplemented` (route-through-root doc), keeps `Get`/`List`.
- **G-5 (references vs containment).** `ddd.v1.references` generates a scalar FK + ID and **no** traversable edge; `belongs_to`/`has_many` keep generating edges (containment).
- **G-6 (cascade on owned members).** Owned-member FKs generate `OnDelete: Cascade` (not the current `SetNull`); `references` stay restrict/SetNull. Migration story for the FK change.
- **G-7 (etag = aggregate version).** `Save` bumps the root's etag on any member change (explicit root touch in the tx, guarded against double-bump); add `EtagMixin` to the aggregate fixture.
- **G-8 (domain-behavior file).** A regen-safe owned file where root invariant methods live; `Save` calls a `Validate(ctx) error`-by-convention hook before persist.
- **G-9 (IAM worked example + docs).** `testdata/iam` (accounts/users/groups/memberships/api-keys) proving account-as-partition, api-key-as-own-aggregate, membership ownership; complete `concepts/aggregates.md`.

## Non-goals
- GORM/storage-backend aggregate support (ent + memory only initially; `protoc-gen-storage` must at least not break on `references` — treat as a scalar FK).
- Event sourcing / cross-aggregate transactions / outbox (→ F032).
- Owning `main.go`.

## Design decisions (★ = confirm in Clarify) — see F030 D-2..D-7 for the validated detail
- **D-1 (annotation package).** New `proto/infoblox/ddd/v1/ddd.proto` + **local** buf gen target producing `…/devedge-sdk/proto/infoblox/ddd/v1` bindings (the one place the repo generates an annotation `.pb.go` locally — safe because the namespace is SDK-private, no register-once collision). Mirror byte-identically into `cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/ddd/v1` (extend `mirror_drift_test.go`).
- **D-2 (Load).** ★ Choose: (a) a new graph-load seam primitive on the generated ent repo (`LoadAggregate`) that eager-loads declared containment edges, vs (b) `AggregateRepository.Load` calls the ent client `.With<Edge>()` directly behind the seam. Recommend (a) so service code never touches the ent client.
- **D-3 (Save semantics).** ★ Full-graph replace vs diff vs explicit member-mutation tracking. Recommend **member-mutation tracking** (the root records added/removed/changed members) to avoid destructive full-replace and to drive cascade correctly.
- **D-4 (gate).** `AssertAggregateBoundaries(methods, memberRoots)` beside `grpcauthz.AssertMethodsDeclared` at `Serve`. `Undelete`/`Batch*` = writes.
- **D-5 (etag root bump).** In `Save`, after member writes, issue one explicit update on the root that triggers `EtagMixin`; guard so a direct root change doesn't double-bump.
- **D-6 (cascade).** Generator sets `OnDelete: Cascade` for containment edges of a member→root; emit the migration.
- **D-7 (domain file).** `Save` looks for `Root.Validate(ctx) error` (convention) and calls it pre-persist.

## Acceptance criteria
- **AC-1.** Declaring `Item member_of Order` + registering a write-capable `CreateItem` handler **fails at `Serve`** with a clear error; removing/redirecting it serves; `Get`/`List` for `Item` unaffected.
- **AC-2.** A generated member handler returns `Unimplemented` for writes, serves `Get`/`List`; overriding the write to delegate to the root op compiles and behaves; regen doesn't disturb the override.
- **AC-3.** `Order` aggregate round-trips: `Load` returns root+members; a domain method mutates; `Save` persists the cluster in one tx; a stale root etag → `ErrPreconditionFailed`.
- **AC-4.** `ddd.v1.references` generates scalar FK + ID and **no** edge (negative test: no cross-root edge method); `belongs_to`/`has_many` still generate edges.
- **AC-5.** Deleting an aggregate root cascades to owned members (FK `OnDelete: Cascade`), verified on sqlite; `references` targets are not cascaded.
- **AC-6.** A root `Validate(ctx)` invariant ("≥1 admin", "no item once SHIPPED") rejects the offending `Save` with a mapped gRPC code; passing case persists.
- **AC-7.** `testdata/iam` builds and serves: account=partition (existing `account_id`/`TenantMixin`), api-key own aggregate referencing user via `references`, membership owned by the rule-holder, auth lookup via projection (not aggregate load).
- **AC-8.** `concepts/aggregates.md` complete with the decision test, orders/items + IAM examples, "addressable reads, aggregate-controlled writes", and the multi-surface clarification (read-only projection of a member is **not** a member write for the gate).
- **AC-9 (no regression).** Existing apikey/fleet/toy suites stay green; non-aggregate services unaffected.

## Failure modes (see F030 list + )
- Member soft-delete asymmetry (Vehicle hard-delete vs Fleet soft-delete) — constrain member delete semantics or make members soft-delete under a soft-delete root.
- Giant aggregate eager-load — advisory lint; high-cardinality children should be `references`, not containment.
- Gate guards only the transport surface — direct ent-client writes remain developer responsibility (document).
- Two-aggregate `Save` — API shape one-root-per-call + docs.

## Phasing
1. **[S]** `ddd.v1` proto + local bindings + mirror + drift test (D-1, G-1).
2. **[S]** `protoc-gen-{ent,svc}` read the annotations; `references` codegen (G-5), member write-redirection (G-4), cascade (G-6).
3. **[S]** `AggregateRepository` Load/Save (ent + memory) on `TxRunner`; etag root-bump (G-2, G-7, D-2/D-3/D-5).
4. **[S]** `server` boundary gate (G-3, D-4).
5. **[S]** domain-behavior file (G-8); `aggregates.md` (G-9 docs).
6. **[S]** `testdata/iam` fixture (G-9) proving AC-1..AC-8.

## Open questions
1. Load primitive (D-2 a vs b). 2. Save semantics (D-3). 3. Member soft-delete policy under a soft-delete root. 4. Does `protoc-gen-storage` need any `references` handling beyond emitting a scalar FK (to not break GORM fixtures)? 5. Lint vs hard-gate for giant-aggregate eager-load.
