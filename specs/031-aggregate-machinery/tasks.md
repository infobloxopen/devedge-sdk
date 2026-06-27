# F031 — Tasks (aggregate machinery)

Prereq: **F030 merged** (`TxRunner`/`Atomically` + tx-aware ent adapter). Tags **[S]**/**[C]**.

## Phase 1 — annotations (SDK-owned, in-repo)
- **T1 [S]** `proto/infoblox/ddd/v1/ddd.proto`: `aggregate{root}` + `member{root}` (extend `MessageOptions`); `references{aggregate,foreign_key}` (extend `FieldOptions`, in `ddd.v1` — NOT shared `field.v1`). SDK-owned `go_package`.
- **T2 [S]** Add a buf gen target producing local `…/devedge-sdk/proto/infoblox/ddd/v1` bindings (first locally-generated annotation `.pb.go` — safe: SDK-private namespace).
- **T3 [S]** Mirror into `cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/ddd/v1`; extend `mirror_drift_test.go`.

## Phase 2 — codegen consumes annotations
- **T4 [S]** `protoc-gen-ent` + `protoc-gen-svc` import the local `dddv1` bindings; detect root/member/references.
- **T5 [C]** `references` codegen: scalar FK + ID, NO edge (G-5); negative render test (no cross-root edge method).
- **T6 [C]** Member write-redirection in `protoc-gen-svc`: skip `stdCreate/Update/Delete/Undelete` cases for members, keep `Get`/`List` (G-4); render tests.
- **T7 [C]** Cascade: containment-edge FKs emit `OnDelete: Cascade` (G-6); migration for the change; `protoc-gen-storage` emits `references` as a plain scalar FK so GORM fixtures don't break.

## Phase 3 — AggregateRepository + etag-as-version
- **T8 [S]** `persistence.AggregateRepository[Root,ID]` interface; ent impl `Load` (graph-load primitive, D-2) + `Save` (member-mutation tracking, D-3) on `TxRunner`; memory impl (bespoke assembly).
- **T9 [S]** etag root-bump in `Save` (explicit root touch, guard double-bump, D-5); add `EtagMixin`+etag field to the fleet fixture; re-render.
- **T10 [C]** Tests: AC-3 (round-trip + stale-root-etag→`ErrPreconditionFailed`), AC-5 (cascade on root delete).

## Phase 4 — boundary gate + domain behavior
- **T11 [S]** `server`: member→root accumulator via `Register<Svc>`; `AssertAggregateBoundaries` at `Serve` (G-3, D-4). Tests: AC-1.
- **T12 [S]** `Save` calls `Root.Validate(ctx) error` by convention (G-8, D-7). Tests: AC-6.

## Phase 5 — fixture + docs
- **T13 [S]** `testdata/iam` fixture (accounts/users/groups/memberships/api-keys): account=partition, api-key own aggregate via `references`, membership owned by rule-holder, auth via projection. Proves AC-1..AC-7.
- **T14 [C]** Complete `concepts/aggregates.md` (G-9, AC-8) incl. multi-surface clarification.
- **T15 [S]** Verify gate: build/test/vet green (`go vet` for lint); `/verify-change`; AC-9 no-regression on apikey/fleet/toy.

## Exit criteria
AC-1..AC-9 met on ent + memory; IAM fixture green; docs complete; verify-change passes. Then hardening + release.
