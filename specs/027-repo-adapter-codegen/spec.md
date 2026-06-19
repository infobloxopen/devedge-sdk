# F027 — Repository Adapter Codegen (clean cross-backend persistence wiring)

**AIPs**: AIP-132/133/134/135 (CRUD), AIP-148 (soft-delete/TTL), AIP-154 (etag)
**Status**: shipped (core) — phases 1–5 + 7 done; **deferred:** phase 5b (multi-surface codegen) + phase 6 (GORM parity), to build when a real two-surface or GORM-only consumer exists. See tasks.md for the resume plan.
**Branch**: `feat/027-repo-adapter-codegen`
**Extends**: F026 (batch codegen — the generated batch wrapper currently *depends on* the
hand-written adapter this feature generates), F013 (secret fields), F020 (soft-delete)

---

## Problem statement

The `ent` backend is the only storage shape that still requires a **hand-written adapter**. A
service author must write `New<R>EntRepository` (the six `entrepo.EntRepository` closures —
Create/Get/List/Update/Delete/Undelete, with `persistence.ConstraintError` classification,
`ent.IsNotFound → persistence.ErrNotFound` mapping, tenant + soft-delete mutation guards, the
secret hash/cipher block, and AIP-160 filter/order/paging) plus the `fromEnt<R>` projection — by
hand, ~300 lines per resource, copied and adapted from a fixture. By contrast `protoc-gen-storage`
(GORM) already generates a complete `persistence.Repository`.

The Run 9 developer-experience assessment (service `coupond`, SDK v0.10.0) found this is the single
largest, most consistent token/churn cost of building a new service — **~85% of build effort was
transcribing this adapter + its test harness** (filed as devedge-sdk **#53**). It is also where
correctness bugs creep in (a forgotten `ConstraintError`, a dropped field) — the SDK already added
`ErrorMapperUnary` as a safety net (#45) precisely because hand-written adapters forget.

Three deeper gaps exist on **both** backends, not just ent:

1. **No developer-owned override seam.** GORM bakes `toModel`/`fromModel` into the generated file;
   ent has no generated adapter at all. There is no first-class place to customize a projection
   (computed/derived fields) that survives regeneration.
2. **No multi-surface support.** Both generators assume one proto message ↔ one storage model. A
   single model backing several API surfaces (e.g. `v1.Coupon` + `v2.Coupon` + `admin.Coupon`) is
   not expressible.
3. **No fail-closed contract.** A proto field the generator can't map is silently dropped (Run 9
   reproduced this: a docs-following agent never noticed an unmapped field), rather than failing
   generation with an actionable message.

This feature is being done **clean — there is no backward-compatibility constraint** (the SDK is
pre-1.0, still in development). We delete the hand-written `ent_wiring.go` fixtures rather than
preserve them, and refactor GORM output to the same contract rather than gate it behind an opt-in.

---

## Goals

- **G-001** Generate the ent repository adapter — `New<R>EntRepository` (the six closures) and the
  deterministic `fromEnt<R>` / `toEnt<R>` mapping — so an ent-backed service needs **no hand-written
  wiring**. Output equivalence with today's hand-written fixtures (apikey, fleet) is the bar.
- **G-002 (fail-closed)** A non-framework proto field with no deterministic mapping (no matching
  storage column, no recognized framework kind) **fails generation** with a precise message naming
  the field and the exact remedy. No silent drops.
- **G-003 (owned override seam)** A scaffold-once, developer-owned hook (`fromEnt<R>Custom` /
  `toEnt<R>Custom`) the generated projection calls — where genuinely custom or divergent mapping
  lives. The generator **never overwrites** it; regeneration of the deterministic layer is safe.
- **G-004 (multi-surface)** A **backend-neutral** message option `(infoblox.storage.v1.model)` binds
  a proto message to its backing storage model (absent → the message name). N messages naming the
  same model → **one repository + N projections**.
- **G-005 (cross-backend parity)** The auto-wire-vs-fail rules live in **one shared package**
  imported by both `protoc-gen-ent` and `protoc-gen-storage`, so a service gets byte-identical field
  coverage and identical fail-closed errors regardless of engine. GORM output is refactored to the
  same projection-split contract.

## Non-goals

- A higher-level ORM/DSL or runtime-reflection adapter (keeps the protobuf-first model the source of
  truth). The escape hatch for anything non-mappable is the owned hook (G-003), not abstraction.
- Preserving any existing generated/hand-written output shape (no back-compat).

---

## Design decisions (locked with the requester)

- **D-1 Split files (refined).** Generated `<snake>_repo.ent.go` (`DO NOT EDIT`) holds the six
  closures **and** the deterministic `fromEnt<R>`/`toEnt<R>` for all auto-wirable fields — it
  regenerates freely, so **adding an auto-wirable field is a no-op for the developer** (the
  "tooling makes it easy" requirement). The **owned** layer (G-003) is a separate scaffold-once
  hook file for the non-deterministic remainder, which the generated projection calls and which is
  never regenerated. *(Refinement of the literal "scaffold-once whole projection" choice: splitting
  deterministic-generated from owned-custom preserves auto-wire-on-regen while keeping custom +
  multi-surface code unmanaged and yours.)*
- **D-2 Neutral multi-surface annotation.** `option (infoblox.storage.v1.model) = "Coupon";` — a new
  `proto/infoblox/storage/v1` MessageOptions extension (mirrors how `infoblox.field.v1` /
  `infoblox.authz.v1` define options). Engine-agnostic by name.
- **D-3 Fail-closed by default.** Unmapped non-framework field → generation error. (A `--check`-only
  mode may follow; default is hard-fail.)

---

## Acceptance criteria

- **AC-001** For `apikey` and `fleet`, the generated `<snake>_repo.ent.go` makes the existing
  hand-written `ent_wiring.go` deletable: `go build` + `go test ./...` + `make security-check` stay
  green with the hand-written files removed.
- **AC-002** Generated adapter behaves identically over SQLite: tenant scoping, soft-delete +
  undelete, `ConstraintError` → clean 409/412 (no raw SQL), secret hash/cipher + never-returned,
  AIP-160 filter/order/paging, AIP-154 etag surfaced on read. (ent sqlite test, mirroring F026.)
- **AC-003** A proto field with no storage column and no framework kind → `protoc-gen-ent` exits
  non-zero with `…<Message>.<field>: no mapping; add a storage field, mark it OUTPUT_ONLY/secret,
  or implement fromEnt<R>Custom`. Generation succeeds once remedied.
- **AC-004** Two messages with `option (infoblox.storage.v1.model)="Coupon"` produce one
  `coupon_repo.ent.go` repo and two projections; both round-trip over the gateway.
- **AC-005** The shared field-coverage checker is the sole source of the auto/fail decision; a unit
  test proves `protoc-gen-ent` and `protoc-gen-storage` classify the same field set identically.
- **AC-006** GORM output is refactored to the split-file + owned-hook contract; toy/apikey/fleet
  GORM paths stay green.
- **AC-007** Re-running the Run 9 `coupond` build docs-only on **both** backends needs **zero**
  hand-written adapter (the #53 churn is gone); recorded as a before/after in docs.

## Failure modes to cover

- Unmapped field (AC-003) — hard fail, actionable.
- Name collision: a generated `New<R>EntRepository` vs a leftover hand-written one — the migration
  (delete hand-written) is part of the change; a duplicate must be a compile error, never silent.
- Type mismatch (proto enum ↔ stored string, nested message ↔ JSON) — classified as non-mappable →
  owned hook, with a hint (not a silent best-effort coercion).
- Multi-surface where two messages naming one model disagree on a field's type → fail-closed.

---

## Phasing (see tasks.md)

1. ent adapter generator + render tests (no wiring/fixtures yet — pure, green unit increment).
2. Wire into `main.go`; migrate fixtures (delete hand-written `ent_wiring.go`); ent sqlite tests.
3. Shared fail-closed field-coverage checker package; both plugins consume it.
4. Owned override seam (`fromEnt<R>Custom`) + one-shot scaffolder.
5. Neutral `infoblox.storage.v1.model` option + multi-surface grouping.
6. GORM parity: `protoc-gen-storage` adopts the shared checker + split-file + neutral annotation.
7. Docs + re-run Run 9 `coupond` on both backends; full SDK gates + `security-check`.
