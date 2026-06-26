# F030 — Aggregate transaction seam (Tier 0), with an in-repo roadmap for DDD-safe boundaries

**AIPs**: AIP-121 (resource-oriented design), AIP-122 (resource names), AIP-131–135 (standard methods), AIP-136 (custom methods), AIP-154 (etag/optimistic concurrency)
**Status**: DRAFT v3 — re-scoped after a codebase realism review, then **corrected** (the annotation layer is SDK-owned in-repo work, not an external-governance dependency). Ready for Clarify.
**Guiding principle**: clean implementation, NO backward compatibility (pre-1.0, no real users). Aggregate support is **opt-in** and **additive**. The SDK provides *seams, defaults, and fail-closed gates* — it does **not** impose a domain framework or put service domain types in the clean core (`authz`, `persistence`, `grpcauthz`).
**Extends**: F010 (`protoc-gen-svc`), F011 (service runtime / `server` + boot gate), F027 (generated repository adapter), F029 (default CRUD handlers + override seam). Introduces **WS-006 (domain-model safety)**.
**Origin**: a design thread on enforcing cross-entity invariants ("an order item cannot be added once the order is SHIPPED"; "a group must keep ≥1 admin"). The investigation found the SDK has (a) **no transaction primitive** on `persistence.Repository`; (b) **per-table** repository granularity (the shape DDD warns against); and (c) a write path that lets a subordinate resource be mutated directly, **bypassing** any centralized invariant — with nothing detecting it.

> **Re-scope summary (v3).** A realism review concluded the original draft was *3–4 features wearing one number*; it is split for **size and risk**, not because any part is externally blocked. v2 wrongly framed the DDD annotations as gated on the canonical `infobloxopen/apis` release pipeline. **Correction:** that gate applies only to *shared, externally-published* annotation packages; a **new `infoblox.ddd.v1` namespace owned by this SDK is ordinary in-repo work** (see *Annotation ownership*). So: F030 ships the transaction foundation now; **F031** is a normal in-repo follow-up for the aggregate machinery; **F032** is the cross-aggregate outbox. The diagnosis and the fail-closed thesis are unchanged and validated.

---

## Problem statement

`protoc-gen-{ent,storage}` generate a `persistence.Repository[*R, K]` per resource and `protoc-gen-svc` (F029) generates a CRUD handler per service. For Tier-1 CRUD this is ideal. For a resource with a **cross-entity invariant**, three gaps appear (all confirmed in code):

1. **No atomicity.** `persistence.Repository` is `Get/List/Create/Update/Delete/Undelete` with no `Begin`/`WithTx`/unit-of-work (`persistence/repository.go:48-58`). Single-op `Create`/`Update` in the generated ent repo call `b.Save(ctx)` with **no surrounding transaction** (`testdata/apikey/apikeyv1/api_key_repo.ent.go:52`); only the batch methods open `client.Tx` (`api_key.batch.ent.go:70,133`). So "load the parent, check its state, write the child — atomically" cannot be expressed through the seam; the developer must drop to the raw ent client, losing portability.
2. **Per-table repositories.** Each message yields its own repository and (via F029) its own write handlers. There is no *consistency boundary* spanning several messages. The neutral seam also **cannot eager-load a graph**: `fromEntFleet` populates scalars only and never fills the `Vehicles []*Vehicle` field the proto declares (`testdata/fleet/fleetv1/fleet_repo.ent.go:160-176` vs `fleet.pb.go:44`).
3. **Silent bypass.** F029's escape hatch (embed `<Svc>CRUDHandler`, override one method) plus per-resource generated writes mean a subordinate entity (e.g. `Item`) stays independently writable; nothing detects the bypass.

Thesis (unchanged): the SDK already makes correctness *mandatory* where it matters — the fail-closed authz completeness gate at `Serve` (`grpcauthz.AssertMethodsDeclared`, `grpcauthz/interceptor.go:107-120`, run at `server/server.go:213`). Apply the same instinct to aggregates. The safety machinery (boundary gate, write redirection) needs a `ddd.v1` annotation, which is **SDK-owned and built in-repo** (F031) — so the split below is about delivering the atomicity foundation first, not about an external blocker.

---

## Scope of THIS feature (F030)

**In scope — Tier 0 only:**
- **A transaction seam** (`persistence.TxRunner` / `Atomically`) with **tx-aware generated ent + in-memory repositories**, so atomic "load → check → write" works through the seam on both backends.
- **etag as the optimistic-concurrency token — documented**, using the existing `middleware/etag` machinery; no automatic parent/root bump yet.

**Sequenced follow-ups (separate features, all in-repo):**
- **F031 — DDD-safe aggregate machinery:** the SDK-owned `infoblox.ddd.v1` annotations (aggregate/member) + an SDK-owned `references` annotation; `AggregateRepository[Root,ID]`; the fail-closed boundary gate; member write-redirection; cascade-on-delete for owned members; etag-as-aggregate-version; the domain-behavior file; the IAM worked-example fixture + `aggregates.md`.
- **F032 — cross-aggregate eventual consistency:** the outbox / domain-event seam (store-seam precedent: `lro/store.go:10`, `middleware/dedup.go:12`).

---

## Goals

**F030 (this feature):**
- **G-1 (transaction seam).** A backend-neutral `persistence.TxRunner.Atomically(ctx, fn)`: repositories used inside `fn` are transaction-bound; commit on `nil`, rollback on error. Dev defaults for ent and in-memory. Delivering this **requires making the generated repositories tx-aware** (see D-1) — it is *not* a wrapper over `client.Tx`.
- **G-2 (etag concurrency token — documented).** Document the resource `etag` as the optimistic-concurrency token for `Update` (already enforced via `middleware/etag` + the per-entity `EtagMixin`, `persistence/entrepo/mixin.go:146-175`). No aggregate/root semantics yet.
- **G-3 (docs).** A `concepts/transactions.md` and a stub `concepts/aggregates.md` decision guide ("smallest set consistent in one transaction" test; atomic check-then-write recipe; "addressable for reads, aggregate-controlled for writes"), with the forward pointer to F031.

**Deferred goals (roadmap; specified in F032/F031):** `AggregateRepository`; the SDK-owned `infoblox.ddd.v1` annotations + `references`; the boundary gate; member write-redirection; etag-as-aggregate-version; the IAM fixture; the outbox/event seam.

## Non-goals

- **Event sourcing / CQRS read-model generation.** Out of scope. The auth/read hot path keeps using existing projections (`LookupBy<Field>Hash`, multi-surface read surfaces).
- **A cross-aggregate distributed transaction / 2PC.** The SDK *discourages* two-aggregate transactions; cross-aggregate consistency is eventual (F032).
- **GORM/storage-backend aggregate support, initially.** `protoc-gen-storage` is a full parallel generator reading the same field annotations and emitting its own associations + transactions (`cmd/protoc-gen-storage/render.go:8,174-177,423-449`), and every fixture builds **both** backends (`buf.gen.fleet.yaml`). F030's `TxRunner` MUST ship an in-memory + ent default; a GORM `TxRunner` and any GORM handling of the future `references` are **explicitly deferred** (called out so the GORM codegen path is not silently broken).
- **Domain types in the clean core.** Aggregates and their behavior live in the *service's* generated/owned packages.
- **Owning `main.go`** (consistent with F029): DB open, migrations, repository construction stay developer-owned.

---

## Annotation ownership (in-repo — SDK-owned, NOT externally gated)

This corrects the v2 "external blocker" framing.

- The **existing** modeling annotations (`field.v1`, `storage.v1`, `authz.v1`) are consumed as Go bindings **from the published module** — generators import `fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"` and read `proto.GetExtension(opts, fieldv1.E_Opts)` (`cmd/protoc-gen-ent/main.go:23-24,120`); go.mod pins them with **no `replace`**; there is **no local `.pb.go`** (the local `proto/infoblox/field/v1/field.proto:11` `go_package` points at the canonical module). The proto header states why: it deliberately skips local generation so each **shared** extension registers exactly once per process (a second copy panics protobuf).
- **That register-once rationale applies only to shared packages.** A new `infoblox.ddd.v1` namespace that **only this SDK defines** has no other consumer, so F031 can:
  1. add `proto/infoblox/ddd/v1/ddd.proto` with a **devedge-sdk-owned `go_package`**,
  2. **generate its `.pb.go` locally** (a new buf gen target — the one thing the repo intentionally doesn't do for the shared packages),
  3. import that local binding in `protoc-gen-{ent,svc}` and read `dddv1.E_Aggregate` / `E_Member` / `E_References`.
  No `apx`, no two-repo release, no governance-reserved extension number.
- **The only genuine external gate is mutating the shared, published `field.v1.FieldOptions`** (e.g. adding `references` *to it*). **We avoid that** by putting the cross-aggregate `references` annotation in the SDK-owned `ddd.v1`, not in `field.v1`.
- **Distribution to services:** ship `ddd.proto` via the SDK's own buf module + the existing scaffold-mirror (`cmd/devedge-sdk/internal/scaffold/mirrors/...`, byte-identical, drift-tested by `mirror_drift_test.go`) — SDK-controlled, not canonical-apis governance.

**Conclusion:** F031's annotation layer is ordinary in-repo work. It is sequenced after F030 for size/risk, not because of any external dependency.

---

## Design decisions (★ = confirm in Clarify)

- **D-1 (TxRunner seam — requires adapter regeneration, NOT a wrapper).**
  ```go
  // persistence/tx.go
  type TxRunner interface {
      Atomically(ctx context.Context, fn func(ctx context.Context) error) error
  }
  ```
  Propagation is **ctx-based**: `Atomically` stashes a backend tx handle on `ctx`; tx-aware repositories read it and bind to the tx for the duration of `fn`. The obstacle is real: ent tx writes must go through `tx.<Type>` (a separate `*Tx` holding a tx-driver-bound client — `testdata/apikey/ent/client.go:118-132`, `tx.go:13-24`), and the generated repo **captures the non-tx `*ent.Client` in closures at construction** (`api_key_repo.ent.go:23-26,31`; `EntRepository.Create_` at `persistence/entrepo/repository.go:30-39`). So tx-awareness **requires regenerating the F027 ent adapter** to resolve the client/tx from `ctx`. ★ **Decide now:** **(a) regenerate the adapter** so each operation resolves `tx-or-client` from `ctx` (recommended) vs (b) a `WithTx(ctx) Repository` factory (leaks tx-awareness into call sites; breaks the "repos inside `fn` are tx-bound" ergonomic). The in-memory default is cheap: copy the maps under the existing `RWMutex` for snapshot/rollback (`persistence/memory.go`).
- **D-2 (AggregateRepository) — DEFERRED to F031; corrected framing.**
  ```go
  type AggregateRepository[Root any, ID comparable] interface {
      Load(ctx context.Context, id ID) (Root, error)
      Save(ctx context.Context, root Root) (Root, error)
  }
  ```
  Not "natural on ent via `.With<Edge>()`": the neutral seam discards edges (`fromEntFleet` never fills `Vehicles`, `fleet_repo.ent.go:160-176`), so `Load` must either (i) grow a graph-load seam primitive or (ii) use the raw ent client. The **memory backend has no graph** (flat `map[K]T`), so its `Load`/`Save` is bespoke per-aggregate. ★ **F031 chooses `Save` semantics** — full-graph replace vs diff vs member-mutation tracking — which drives child cascade/orphan (D-5b).
- **D-3 (annotations — SDK-owned `infoblox.ddd.v1`, generated locally) — DEFERRED to F031; in-repo.** Define, in-repo, message options `AggregateOptions{ bool root }` and `MemberOptions{ string root }` (extend `google.protobuf.MessageOptions`) and a field-level `References{ string aggregate; string foreign_key }` — **all in the SDK-owned `ddd.v1`**, with local `.pb.go` generation (see *Annotation ownership*). `belongs_to`/`has_many` (existing `field.v1`) stay = within-aggregate containment (ent edges); `ddd.v1.references` = across-boundary link → scalar FK + ID only, **no traversable edge**, so code cannot walk/mutate across roots. Mirror into the scaffold (drift-tested).
- **D-4 (fail-closed boundary gate) — DEFERRED to F031; structurally feasible.** `AssertMethodsDeclared` is a pure fn over `(methods, rules)` run at `Serve` (`grpcauthz/interceptor.go:107-120`; `server/server.go:213`); a parallel `AssertAggregateBoundaries(methods, memberRoots)` slots in beside it, with a member→root accumulator mirroring `AddRules`/`RecordMethods` (`server/server.go:78-83,317-326`). Needs the `ddd.v1` annotation flowing into `Register<Svc>` (in-repo, F031). `Undelete` counts as a write (and is only generated for soft-delete resources — `protoc-gen-svc/main.go:274`).
- **D-5 (member write-redirection in `protoc-gen-svc`) — DEFERRED to F031; cheapest piece.** The handler embeds `Unimplemented<Svc>Server` (`render.go:146`); `classifyMethod` already buckets each RPC (`main.go:201-287`). Suppressing member writes = skip emitting the `stdCreate/stdUpdate/stdDelete/stdUndelete` cases for a member resource (`render.go:150-202`), keep `stdGet/stdList` (~30 lines). Stub is a plain `Unimplemented` ("route through the root" is a doc-comment).
- **D-5b (cascade/orphan — must be resolved in F031).** The current Fleet→Vehicle FK is `OnDelete: SetNull` (`testdata/fleet/ent/migrate/schema.go:52`) — deleting a Fleet **orphans** its Vehicles, the opposite of aggregate ownership. F031 must add a codegen path setting `OnDelete: Cascade` for **owned** members (containment edges), leaving `references` as `SetNull`/restrict, plus a migration story (infobloxopen/migrate per `SHAPES.md`).
- **D-6 (etag-as-aggregate-version) — DEFERRED to F031; no home today.** The per-entity `EtagMixin` re-stamps the *mutated entity's own* etag (`entrepo/mixin.go:146-175`) with no parent awareness, and the only aggregate-shaped fixture has **no etag at all** — Fleet/Vehicle embed neither `EtagMixin` nor an etag field (`testdata/fleet/ent/schema/fleet.go:20-24`, `vehicle.go:18-23`); only apikey does (`api_key.go:22`). So F031 must (i) add `EtagMixin` to the aggregate fixture and (ii) have `Save` issue an explicit root touch inside the tx (guarded against double-bump). F030 only documents etag as the single-row concurrency token (G-2).
- **D-7 (domain-behavior file) — DEFERRED to F031; pattern is real.** Precedent: the owned `FromEnt<R>Custom` / `ToEnt<R>On{Create,Update}` package vars (`api_key_repo.ent.go:185-190`). A `Validate(ctx) error`-by-convention hook called from `Save` is plausible, with teeth only if `Save` is the sole write path (→ the D-4 gate guards the transport surface; direct ent-client writes remain the developer's responsibility, same caveat class as the authz gate).
- **D-8 (cross-aggregate consistency) — DEFERRED to F032.** Outbox/event seam for "UserSuspended → revoke keys". F030/F031 only ensure cross-aggregate links are ID-only (`ddd.v1.references`). Store-seam precedent: `lro/store.go:10-19`, `middleware/dedup.go:12-15`.

---

## Acceptance criteria

**F030 (this feature):**
- **AC-1 (atomic check-then-write).** Using only `TxRunner` + the neutral seam (no raw ent client in handler code), a handler loads a parent, checks state, writes a child **atomically**; a forced failure mid-`fn` rolls back with no partial write. Proven on **ent and in-memory** backends.
- **AC-2 (tx-aware repos).** A write issued through a generated repo *inside* `Atomically` participates in the transaction (a concurrent reader sees nothing until commit; a rollback discards it). A write through a non-enrolled repo is detectable/documented (★ failure-mode hardening).
- **AC-3 (no regression).** A service that uses no `TxRunner` generates byte-identical output to today *for the parts unrelated to the adapter change*; the apikey/fleet/toy suites stay green. (AC-1/AC-2 require the D-1 adapter regeneration — re-render the fixtures.)
- **AC-4 (docs).** `concepts/transactions.md` documents `Atomically`, the atomic check-then-write recipe, and etag-as-concurrency-token; `aggregates.md` stub has the decision test + forward pointer to F031.

**Deferred ACs (F031 unless noted):** aggregate round-trip + stale-root-etag → `ErrPreconditionFailed` (requires adding `EtagMixin` to the fleet fixture, D-6); fail-closed member-write boundary at `Serve` (D-4); member write-redirection default (D-5); `ddd.v1.references` generates scalar FK + ID and **no** edge, negative test; cascade-on-root-delete for owned members (D-5b); the IAM fixture validating account-as-partition + api-key-as-own-aggregate + the auth projection.

## Failure modes to cover

- **Tx not propagated (worst case — looks atomic, isn't).** A non-tx-aware repo silently writes outside the transaction. Mitigation: tx-aware repos are the generated default (D-1 option a); ★ can `Atomically` assert every write saw the ctx tx?
- **Two aggregates in one `Atomically`.** Not type-preventable; mitigate by API shape (one root's `Save` per call) + docs + advisory lint — not a hard gate.
- **Giant aggregate.** Eager-loading an unbounded `has_many` on `Load`. Mitigation: small-aggregate guidance + advisory lint; high-cardinality children should be `references` (own aggregate). (F031.)
- **Cascade vs orphan mismatch.** FK default is `OnDelete: SetNull` (`migrate/schema.go:52`); F031 must flip owned-member FKs to `Cascade` + migrate. Until then "delete root" leaves dangling members.
- **Member soft-delete asymmetry.** A member may be hard-delete (Vehicle: `vehicle_repo.ent.go:113`, no `Undelete`) while the root is soft-delete (`fleet_repo.ent.go:118` + `Undelete_`); root `Undelete` cannot restore it. Aggregates must constrain member delete semantics.
- **Memory backend has no graph.** Cluster `Load`/`Save` on the flat-map backend (`persistence/memory.go`) is bespoke per-aggregate; F030's AC-1 is single parent+child (not a full cluster) to keep this bounded.
- **etag double-bump / lost update.** Member change must bump the root exactly once; an independent member etag must not be mistaken for the concurrency token. (F031, D-6.)
- **Boundary gate false safety.** The gate guards only the registered transport surface; a handler reaching into the ent client directly bypasses it (same caveat class as the authz gate). Document explicitly. (F031.)

---

## Phasing (corrected)

1. **F030 (this feature) — Tier 0.** `TxRunner`/`Atomically` + **regenerated tx-aware ent adapter** + in-memory default + etag-as-concurrency-token docs. AC-1..AC-4. Self-contained; unblocks everything.
2. **F031 — DDD-safe aggregate machinery (in-repo).** SDK-owned `infoblox.ddd.v1` (aggregate/member + `references`) with local bindings; `AggregateRepository` (Load/Save + chosen graph-load primitive + `Save` semantics); fail-closed boundary gate (D-4); member write-redirection (D-5); cascade-on-delete for owned members (D-5b); etag-as-aggregate-version (D-6, incl. adding `EtagMixin` to the fleet fixture); domain-behavior file (D-7); IAM fixture + `aggregates.md`. **Sequenced after F030; no external dependency.**
3. **F032 — cross-aggregate eventual consistency.** Outbox / domain-event seam (D-8).

---

## Open questions for Clarify (★ consolidated)

1. **D-1 mechanism** — confirm **(a) regenerate the F027 adapter** to resolve tx-or-client from `ctx` (recommended) vs (b) a `WithTx` factory. This is the crux of F030; it is a codegen change, not a wrapper.
2. **F030 atomicity scope** — confirm single parent+child atomic write (not a full cluster `Load`/`Save`) is the right Tier-0 boundary, deferring graph load/save to F031 (recommended, to avoid the memory-graph cost).
3. **Un-enrolled-write detectability** inside `Atomically` (failure-mode hardening).
4. **GORM `TxRunner`** — confirm out-of-scope for F030 (in-memory + ent only), with a tracking note so the storage path isn't assumed.
5. **(F031) Multi-surface interaction** — does a read-only WS-005 projection of a member count as a "member write" for the gate? Expected **no** (read-only, no Encryptor — `multisurface_test.go:3-10`, `api_key_summary_repo.ent.go:17`); confirm and document.
6. **(F031) `Save` semantics** — full-graph replace vs diff vs member-mutation tracking (drives D-5b cascade).
7. **(F031) Migrations** — story for the `references` FK and the `OnDelete: Cascade` change (infobloxopen/migrate per `SHAPES.md`).

---

## Realism review (folded in)

A senior DDD/Go-data-modeling critic compared the v1 draft against the code. Verdict: the **diagnosis and fail-closed thesis are correct and evidenced**; the packaging was wrong (and v2 then over-corrected the annotation cost). Confirmed by code:

- **"Account = partition, not aggregate" holds** — `account_id` is the tenant discriminator, auto-scoped by `TenantMixin` (`entrepo/mixin.go:53,57-67`) and re-applied on every mutation (`fleet_repo.ent.go:94`). The IAM guidance is sound.
- **No tx seam / single-op non-tx / batch-only tx** — confirmed (`repository.go:48`, `api_key_repo.ent.go:52`, `api_key.batch.ent.go:70`).
- **Neutral seam can't eager-load** — `fromEntFleet` discards edges (`fleet_repo.ent.go:160-176`).
- **etag-as-version has no home** — per-entity `EtagMixin` (`mixin.go:146-175`); fleet/vehicle carry no etag (`fleet.go:20-24`, `vehicle.go:18-23`).
- **Cascade contradicts ownership** — FK is `OnDelete: SetNull` (`migrate/schema.go:52`).
- **GORM parity gap** — `protoc-gen-storage` reads the same annotations and runs in every fixture build.

**Annotation-cost correction (v3).** The critic's "cross-repo, governance-gated" claim is true only for the *existing shared* packages (generators import external bindings: `protoc-gen-ent/main.go:23-24`; go.mod pins; no local `.pb.go`; register-once per the proto header). It does **not** apply to a **new SDK-owned `ddd.v1`**, which can be generated and consumed in-repo. The only real external gate — modifying the published `field.v1.FieldOptions` — is avoided by keeping `references` in `ddd.v1`. Net effect: F031 is a normal in-repo feature, sequenced (not blocked) after F030.

Changes applied across v2→v3: re-scoped F030 to the transaction seam; corrected D-1 (regeneration), D-2 (not "natural on ent"), D-6 (no etag home); added GORM non-goal, cascade (D-5b) + soft-delete-asymmetry + memory-graph failure modes; **reframed the annotation work as SDK-owned in-repo (D-3 + Annotation ownership) rather than an external blocker**; split the aggregate machinery into F031 (in-repo) and the outbox into F032; added the multi-surface and migration open questions.
