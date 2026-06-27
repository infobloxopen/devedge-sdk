# F032 — Tasks (cross-aggregate outbox + events)

Prereq: **F030 merged** (`TxRunner`) and **F031 merged** (`references`, aggregate boundaries, `testdata/iam`). Tags **[S]**/**[C]**.

## Phase 1 — outbox store
- **T1 [S]** `persistence.OutboxStore` interface (append-in-tx; claim; mark-delivered) modeled on `lro.Store`/`DeduplicationStore`.
- **T2 [S]** In-DB dev default: `outbox` table (`id, account_id, aggregate_type, aggregate_id, event_type, payload, created_time, delivered_time, attempts`); migration via infobloxopen/migrate; account-scoped.

## Phase 2 — publish
- **T3 [S]** `events.Publisher.Publish(ctx, event)` enlists in the F030 ctx tx (shares the commit). Outside-tx behavior per D-1 (★ error vs immediate). Tests: AC-1, AC-4.

## Phase 3 — dispatch
- **T4 [S]** `Dispatcher`: claim undelivered (SKIP LOCKED if ent `sql/lock` enabled, else claimed-flag/lease), invoke handlers, mark delivered, attempts++.
- **T5 [S]** Handler registration by event type; each handler runs in its own `Atomically` tx; idempotency via event id (reuse dedup semantics). Tests: AC-2.

## Phase 4 — example + docs
- **T6 [S]** Extend `testdata/iam`: emit `UserSuspended` on suspend; register a handler that revokes the user's API keys in a separate aggregate tx. Tests: AC-3 (eventual consistency).
- **T7 [C]** `concepts/events.md` (outbox, at-least-once + idempotency, cross-aggregate tradeoff); cross-link `aggregates.md`. AC-5.
- **T8 [S]** Verify gate: build/test/vet (`go vet` for lint); `/verify-change`; AC-6 (no broker dep in clean core; suites green).

## Exit criteria
AC-1..AC-6 met; IAM `UserSuspended→revoke-keys` green; docs complete; verify-change passes. Then final hardening + release; roadmap shipped.
