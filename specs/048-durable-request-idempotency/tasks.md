# F048 tasks — durable request idempotency (WS-043)

Tags: `[S]` mechanical, `[C]` complex/design-bearing. Each task cites the AC/FM it satisfies.

- **T1 [S]** Scope the in-memory path's key: `DeduplicateUnary` composes `(tenant, method, request_id)`
  via `VerifiedTenantID` + `info.FullMethod` instead of the bare `request_id`. (AC-05, AC-09, D2)
- **T2 [S]** Regression test: tenant A's `request_id` must not replay to tenant B on the memory path;
  same `request_id` on two methods must not collide. (AC-05)
- **T3 [C]** `middleware/idempotency.go`: `IdempotencyKey`, `IdempotencyRecord`, status consts,
  `DurableIdempotencyStore` interface (`Lookup`/`Claim`/`Complete`/`GC`), sentinel errors + gRPC
  codes, options (TTL, fingerprint). (D4, D7, FM-01)
- **T4 [C]** `middleware.DurableDeduplicateUnary(store, tx, opts)`: pass-through gates (no id /
  validate_only), non-tx `Lookup` fast path (completed→replay, in_progress→409), `Atomically`
  slow path (claim→handler→complete, replay-on-conflict), proto marshal/replay via registry,
  fingerprint check. (AC-01..07, FM-01..05, D3, D5)
- **T5 [C]** `persistence/gormtx/dedupstore.go`: `IdempotencyKeyRow` model (PK account_id/method/
  request_id, status, response_type, response, fingerprint, created_at, expires_at) + namespace
  option; `GormDurableDedupStore` implementing `Lookup`/`Claim`/`Complete`/`GC` with in-tx
  `TxFromContext` binding, `ON CONFLICT DO NOTHING` claim, expired-row reclaim. (AC-01..04,08, D6, D8, FM-06)
- **T6 [S]** Register `IdempotencyKeyRow` in `gormtx.MigrationModelsFor` (+ `frameworkBaseTable`); add
  the `idempotency_keys` DDL to the baseline `.up.sql`/`.down.sql` (kept in lockstep with schemagen).
- **T7 [C]** `server/server.go`: `Config.DurableDedup *middleware.DurableDedup`; when set, the chain
  uses `DurableDeduplicateUnary`, else the legacy memory path. Memory default preserved. (D1)
- **T8 [C]** gormtx integration tests (SQLite in-memory): exactly-once under double-apply, atomic
  rollback on handler error, verbatim replay across a fresh store instance (restart proxy),
  in-progress 409, fingerprint reject, TTL expiry + GC. (AC-01..08, FM-03,04,06)
- **T9 [S]** Interceptor unit tests with a fake store: fast-path replay, validate_only/empty
  pass-through, tenant scoping, non-proto fail-loud. (AC-05,06, FM-01)
- **T10 [S]** Docs: `concepts/` note + CHANGELOG; wire a servicekit/scaffolder touchpoint or document
  the `server.Config.DurableDedup` seam as the enablement point.
- **T11 [S]** Build + vet + full `go test ./...`; version bump + release notes.

Hardening loops (post-implement): (1) security — cross-tenant replay, in-flight abuse, fingerprint
bypass, injection via request_id/method; (2) correctness/DX — race semantics, RLS interaction,
API ergonomics.

## Increment 2 (v0.63.0) — deferred follow-ups

See spec.md "Increment 2" for full ACs/FMs. Tasks T12–T18 (Deliverable A = servicekit auto-wiring +
host-scheduled GC; Deliverable B = reserve→remote→complete saga). All `[C]` except T16/T18 `[S]`.
Hardening loops: (1) independent security review, (2) correctness review of the diff; every defect
fixed gets a regression test.

## Increment 3 (v0.64.0) — ent parity + hot-table performance

See spec.md "Increment 3" for full ACs/FMs. Framework layer unchanged (interface-only; no shared
store; no `*sql.Tx` seam). Deliverable C = ent-backed `EntDurableDedupStore` (generic function-field
store in `entrepo`, closures bind the ent tx via `TxFromContext`; encoded-`id` PK because ent has no
composite PK / no raw-SQL access). Deliverable D = PG table tuning (off the drift-gated baseline),
batched partition-safe GC, opt-in hash partitioning by the full PK (exactly-once preserved; never
time-partitioned; create-time fail-loud-if-plain; SQLite/default byte-for-byte unaffected).

- **T19 [C]** entrepo `EntDurableDedupStore` + logic. (DC-1, DC-4)
- **T20 [C]** iam fixture ent schema + closure wiring + `ent generate`. (DC-2, DC-3)
- **T21 [C]** gormtx `TuneIdempotencyKeys` + `EnsureIdempotencyKeysPartitioned` + batched `GC`. (DD-1..3)
- **T22 [C]** gormtx `MigrateOptions.IdempotencyPartitions` + `MigrateModule` wiring. (DD-3, DD-4)
- **T23 [S]** servicekit `DurableIdempotencyConfig.PartitionCount`. (DD-4)
- **T24 [C]** gorm+ent SQLite parity + PG-gated partition/tuning tests. (AC-C*, AC-D*)
- **T25 [S]** vet/test/build-gowork-off/check-migration-baseline; CHANGELOG; v0.64.0 release.

Hardening loops: (1) security review (injection via identifiers, exactly-once under partitioning,
cross-tenant), (2) correctness review of the diff (gorm↔ent parity, batched-GC bounds, partition
routing). Every defect fixed gets a regression test.
