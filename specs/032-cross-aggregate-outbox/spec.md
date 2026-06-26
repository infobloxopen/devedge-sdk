# F032 — Cross-aggregate eventual consistency (transactional outbox + domain events)

**AIPs**: AIP-154 (etag), AIP-155 (idempotency — dedup precedent).
**Status**: DRAFT — depends on **F030** (`TxRunner`) and **F031** (`references`, aggregate boundaries). In-repo.
**Guiding principle**: opt-in + additive; clean core (the event seam is an interface + dev default, no broker dependency in core); at-least-once delivery; pre-1.0, no back-compat.
**Extends**: F030 (`Atomically`), F031 (aggregate boundaries + `references`). Store-seam precedent: `lro/store.go:10`, `middleware/dedup.go:12`.
**Origin**: the IAM thread — rules that span aggregates ("UserSuspended → revoke that user's API keys", "AccountClosed → suspend users") must **not** be one transaction. The safe mechanism is a transactional outbox: emit an event in the same commit as the aggregate change, deliver it asynchronously, react in another aggregate. This prevents dual-write loss (update A, call B, crash between).

---

## Problem statement

F031 makes single-aggregate writes safe and forces cross-aggregate links to be ID-only (`references`). But cross-aggregate *reactions* still have no safe home: doing them inline is either a forbidden two-aggregate transaction or a dual-write that loses the second step on crash. F032 adds a **transactional outbox**: `events.Publish(evt)` writes an outbox row inside the current `Atomically` tx (so the event commits atomically with the aggregate change), and a dispatcher delivers it at-least-once to handlers that update other aggregates.

## Goals
- **G-1 (event seam).** `events.Publisher.Publish(ctx, event)` that enlists in the ctx `Atomically` tx (same commit as the aggregate write). Outside a tx → explicit error or immediate-dispatch (★).
- **G-2 (outbox store).** A pluggable `OutboxStore` (append within tx; claim/mark-delivered) with a dev default (the same DB, an `outbox` table) — mirroring `lro.Store`/`DeduplicationStore`.
- **G-3 (dispatcher).** A `Dispatcher` that polls/claims undelivered events and invokes registered handlers; at-least-once; idempotent handlers (reuse the dedup/AIP-155 precedent).
- **G-4 (handler registration).** Subscribe a handler to an event type; handler runs in its own aggregate tx (`Atomically`), so a reaction is itself a safe single-aggregate write.
- **G-5 (worked example + docs).** Extend `testdata/iam`: `UserSuspended` → a handler revokes the user's API keys; `concepts/events.md` (and an `aggregates.md` cross-link) covering the "cross-aggregate = events, not transactions" rule and the consistency tradeoff.

## Non-goals
- A message broker / Kafka integration (the seam allows it; core ships the in-DB default only).
- Exactly-once delivery (at-least-once + idempotent handlers).
- Event sourcing / event-as-source-of-truth (events are notifications, not the store of record).
- Ordering guarantees beyond per-aggregate (★ confirm).

## Design decisions (★ = confirm in Clarify)
- **D-1 (enlist in tx).** `Publish` reads the ctx tx handle (from F030) and appends to the outbox via the tx-bound store, so the event and the aggregate change share one commit. ★ Behavior when called outside `Atomically`: error (safest) vs best-effort immediate dispatch.
- **D-2 (outbox schema).** `outbox(id, aggregate_type, aggregate_id, event_type, payload, created_time, delivered_time, attempts)`; migration via infobloxopen/migrate. Account-scoped (`account_id`) for tenant isolation.
- **D-3 (dispatcher).** Dev default = in-process poller claiming rows (`SELECT … FOR UPDATE SKIP LOCKED` where supported; otherwise a claimed-flag + lease). ★ Note: ent `sql/lock` is not enabled today — either enable it (one-line generator change) or use a claimed-flag/lease for the default.
- **D-4 (idempotency).** Handlers must be idempotent; provide a dedup key (event id) and reuse `DeduplicationStore` semantics.
- **D-5 (event shape).** Events are small proto messages (type URL + payload) referencing aggregates by ID only (consistent with F031 `references`).

## Acceptance criteria
- **AC-1.** `Publish` inside `Atomically` commits the event atomically with the aggregate change; a rollback discards the event too (no orphan outbox row).
- **AC-2.** The dispatcher delivers an undelivered event at-least-once; a handler crash re-delivers; a duplicate delivery is a no-op via the idempotency key.
- **AC-3.** `UserSuspended` → the registered handler revokes the user's API keys in a separate aggregate tx; eventual consistency demonstrated (keys revoked after dispatch, not in the suspend tx).
- **AC-4.** `Publish` outside a tx behaves per D-1 (error or immediate dispatch) — covered by a test.
- **AC-5 (docs).** `concepts/events.md` covers the outbox pattern, at-least-once + idempotency, and the cross-aggregate consistency tradeoff; cross-linked from `aggregates.md`.
- **AC-6 (no regression + clean core).** No broker dependency added to `persistence`/`authz`/`grpcauthz`; existing suites green.

## Failure modes
- **Dual-write if `Publish` doesn't enlist** — the whole point; assert the event shares the tx (AC-1).
- **Stuck/poison events** — attempts counter + a dead-letter/alert threshold.
- **Dispatcher at-least-once double-fire** — idempotent handlers (AC-2).
- **Lock contention on claim** — SKIP LOCKED or lease; document the default's throughput limits.
- **Ordering assumptions** — document per-aggregate only.

## Phasing
1. **[S]** `OutboxStore` interface + in-DB dev default + migration (G-2, D-2).
2. **[S]** `events.Publisher.Publish` enlisting in the F030 tx (G-1, D-1).
3. **[S]** `Dispatcher` + handler registration + idempotency (G-3, G-4, D-3, D-4).
4. **[S]** Extend `testdata/iam`: `UserSuspended` → revoke keys (G-5, AC-3).
5. **[C]** `concepts/events.md` (G-5, AC-5).
6. **[S]** Verify gate (build/test/vet; `/verify-change`; AC-6).

## Open questions
1. Publish-outside-tx behavior (D-1). 2. Enable ent `sql/lock` for SKIP LOCKED vs claimed-flag/lease (D-3). 3. Ordering guarantees. 4. Dead-letter policy/threshold.
