---
title: Events
weight: 6
---

Some rules span **more than one aggregate**: "when a user is suspended, revoke that user's
API keys"; "when an account is closed, suspend its users". The suspended user and its API
keys are [different aggregates](../aggregates/) — each its own consistency boundary — so the
reaction **cannot** be one transaction (that would be a forbidden two-aggregate write), and
doing it inline as a second call is a **dual write**: update aggregate A, then call B, and a
crash in between loses the second step forever.

The SDK's answer is a **transactional outbox** with **domain events**. You record an event in
the *same commit* as the aggregate change, and a dispatcher delivers it later to a handler
that updates the other aggregate in *its own* transaction. The two changes are connected by
**eventual consistency**, not a shared transaction.

> **Status.** Ships today on top of the [transaction seam](../transactions/) and the
> [aggregate machinery](../aggregates/): an `events.Publisher` that enlists in the current
> `Atomically`, a pluggable **write-only** `persistence.OutboxStore` (in-memory dev default
> plus an ent/SQL store and a reusable `gormtx.GormOutboxStore`), and an in-process
> `events.Dispatcher` that consumes the outbox as a **forward cursor** for same-DB
> cross-aggregate reactions, with handler registration and idempotency. Backends: ent, GORM,
> and in-memory — `gormtx` also ships a `GormIdempotencyStore`, the SQL-backed exactly-once
> marker that records inside the handler's own transaction. The IAM fixture runs the worked
> example on GORM as well as ent. The seam is broker-neutral — no Kafka/NATS/CDC dependency in
> the core. **Cross-server delivery (publishing to message queues) is out of scope for the
> SDK:** a separate project tails the write-only outbox via the documented
> `persistence.OutboxCDCConsumer` seam (WAL/Debezium → queues). The in-process dispatcher here
> is for same-DB reactions.

## The dual-write problem

Without an outbox, a cross-aggregate reaction looks like this:

```go
// WRONG: a dual write. If the process crashes after the user commit but before the
// keys are revoked, the keys are never revoked — the system is permanently inconsistent.
suspend(user)          // commit aggregate A
revokeKeys(user.ID)    // separate write to aggregate B — may never happen
```

You cannot fix it by wrapping both in one `Atomically`: that is a **two-aggregate
transaction**, which the aggregate boundary gate forbids (and which couples two consistency
boundaries that should fail independently).

## The transactional outbox

The fix is to make the *intent to react* durable **atomically with the change that triggers
it**. `Publish` writes an outbox row **through the current transaction**, so the row commits
if and only if the aggregate change commits:

```go
// RIGHT: the event commits in the SAME transaction as the user change.
err := tx.Atomically(ctx, func(ctx context.Context) error {
    if _, err := users.Update(ctx, id, suspended, "status"); err != nil {
        return err
    }
    // Appends an outbox row THROUGH the ctx transaction (events.Publisher).
    return publisher.Publish(ctx, events.Event{
        Type:          "iam.v1.UserSuspended",
        AggregateType: "User",
        AggregateID:   id,
        Payload:       []byte(id), // events reference aggregates by ID only
    })
})
```

If `fn` returns an error, the transaction rolls back and **both** the user change *and* the
outbox row vanish — there is no orphan event. If it commits, the event is durably recorded,
guaranteed, with the change that justifies it. That is the whole point: the event can never be
lost relative to the change, and the change can never happen without the event.

`Publish` **must** be called inside `Atomically`. Called outside a transaction it returns
`persistence.ErrNoTransaction` — the safe choice: refuse rather than write an event that is
not atomic with any aggregate change (the same `RequireTx` guard the
[transaction seam](../transactions/) uses for "tx not propagated").

## Dispatch: a forward cursor over a write-only outbox

A **dispatcher** delivers committed events for **same-DB cross-aggregate reactions**. The
outbox table is **write-only** — the only writes to it are the producer's `Append` (above) and
retention's `DROP PARTITION` — so the dispatcher **never claims, leases, marks, or deletes an
outbox row**. Instead it consumes the outbox as a **forward cursor**:

1. **Loads its position** from a **sidecar** (`persistence.OutboxCursorStore`) — the
   `(created_time, id)` of the last event it consumed. The cursor lives outside the outbox so
   the outbox stays write-only.
2. **Reads forward** with `ReadAfter(cursor, limit)` — `WHERE (created_time, id) > cursor ORDER
   BY created_time, id LIMIT n`, a non-mutating scan that returns events in commit/created order.
3. **Runs every registered handler** for each event's type, **each in its own `Atomically`** —
   so a handler is a normal, safe **single-aggregate write**.
4. **Advances the cursor** in the sidecar past each delivered event. The outbox row is never
   touched.

```go
cursors := gormtx.NewGormOutboxCursorStore(db) // the dispatcher's sidecar
d := events.NewDispatcher(store, cursors, tx, gormtx.NewGormIdempotencyStore(db))
d.Subscribe("iam.v1.UserSuspended", "revoke-api-keys", func(ctx context.Context, evt events.Event) error {
    // Runs in its OWN transaction; revokes keys in a SEPARATE aggregate.
    return revokeKeysForUser(ctx, string(evt.Payload))
})
go d.Poll(ctx, time.Second, 100, logErr) // or call d.RunOnce(ctx, n) on a tick
```

Run **one dispatcher instance per service** for the cursor (the external queue project is the
scale-out path — see below). Delivery is **at-least-once**: a handler error (or a crash between
deliver and cursor-advance) leaves the cursor un-advanced, so a later pass **re-delivers** the
event — a handler can run **more than once**. Handlers must therefore be **idempotent**. The
dispatcher records each `(event id, handler)` pair inside the handler's own transaction and
**skips an already-applied pair on redelivery**, so a duplicate delivery is a no-op:
**at-least-once delivery + the idempotency marker = an exactly-once effect**, robust even if
cursor concurrency is imperfect. The event `id` is the idempotency key (the same AIP-155 dedup
idea the [request dedup interceptor](../../) uses). Design handlers so re-applying them is
harmless regardless (e.g. "ensure these keys are revoked", not "decrement a counter").

## The consistency tradeoff

This is **eventual** consistency, and that is a deliberate tradeoff:

- Between the suspend commit and the dispatch, the user is suspended but the keys are **still
  live**. There is a window. Size it with the poll interval and design around it (e.g. the
  auth path can also check the user's status, not only the key).
- Ordering is **commit/created order** — the forward cursor reads `ORDER BY created_time, id`,
  so per-aggregate ordering is preserved; there is no stronger global cross-aggregate order. Do
  not write a handler that assumes event X for aggregate A arrives before event Y for aggregate B.
- A **poison event** (a handler that always fails) is the cursor's *head* (the oldest
  un-consumed event). While it fails, the cursor cannot advance past it without skipping the gap,
  so the batch stops there — **bounded head-of-line blocking**. After `maxAttempts` consecutive
  failures on that same head event (default 5, set with `events.WithMaxAttempts`), the dispatcher
  records it to a **sidecar dead-letter** and **advances the cursor past it**, so one
  permanently-failing event does not wedge the stream forever. The head-failure count and the
  dead-letter live in the sidecar — **not** in an outbox `attempts` column (the outbox is
  write-only).

The alternative — a synchronous two-aggregate transaction — buys strong consistency at the
cost of coupling two boundaries (and is disallowed by the aggregate gate). For cross-aggregate
rules, eventual consistency via events is the correct shape; reach for it deliberately.

## Write-only outbox, drop-partition retention, and the CDC seam

The outbox is a **write-only** log. The **only** writes to the table are the producer's
transactional `INSERT` (`Append`, atomic with the aggregate change) and retention's `DROP
PARTITION` DDL. The dispatch path **never `UPDATE`s or `DELETE`s an outbox row** — there is no
`delivered_time`, `attempts`, or `leased_until` column and no claim/lease/mark machinery. The
in-process dispatcher tracks its progress entirely in its **sidecar** cursor store
(`outbox_dispatch_cursor` for the forward position and head-of-line failure count, plus a
dead-letter table); the cursor advance is the *only* record of delivery. This is deliberate: a
write-only table never accrues per-poll re-lease/mark churn or write+delete vacuum bloat, and it
is the clean substrate a change-data-capture consumer wants (only `INSERT`s ever appear).

Exactly-once stays guarded by the per-`(event, handler)` idempotency marker, which commits
inside the handler's own transaction: a crash between deliver and cursor-advance re-delivers the
event, and the marker makes that re-delivery a no-op — **at-least-once + idempotency =
exactly-once effect**.

**Retention is `DROP PARTITION`, not row delete.** The outbox table is RANGE-partitioned on
`created_time` (monthly partitions; PostgreSQL declarative partitions, MySQL `PARTITION BY
RANGE (TO_DAYS(created_time))`). `OutboxRetention.DropPartitionsBefore(t)` removes aged data by
**dropping whole partitions** — an O(1) DDL operation that never scans or deletes individual
rows; a partition that overlaps `t` is kept so an in-window row is never lost. The SDK does not
own a scheduler: wire `gormtx.RunRetention` (which also rolls the partition window forward so
this month and next always have a partition to append into) into your own cron / `CronJob` /
ticker. **Drop only partitions that are both older than the retention window AND fully behind the
dispatch cursor**, so the in-process dispatcher never loses an event it has not yet consumed;
size the retention window comfortably longer than the dispatcher could ever fall behind.
Validated on **PostgreSQL and MySQL** via testcontainers (the dev/in-memory and SQLite backends,
which have no declarative partitioning, model the same contract as "forget rows older than `t`").

**Cross-server delivery is an external project, via a documented CDC seam.** Publishing events
to message queues (so *other* services react) is **out of scope for this SDK**. A full in-process
logical-replication / binlog (Debezium-like) engine is heavy and would pull a CDC dependency into
the clean core, so the SDK ships `persistence.OutboxCDCConsumer` as an **interface only** (no
implementation). A separate project tails the write-only outbox — PostgreSQL logical replication
(WAL), MySQL binlog, or Debezium — and publishes to queues. Because the outbox is write-only,
that consumer only ever sees `INSERT`s and partition drops, never row `UPDATE`s/`DELETE`s.

### Notes for whoever builds the external WAL/CDC consumer

Key learnings from the spike, for the separate queue-publishing project:

- **A partitioned outbox needs `CREATE PUBLICATION … WITH (publish_via_partition_root = true)`**
  so PostgreSQL logical replication publishes inserts as the *parent* `outbox` table rather than
  per-partition child tables.
- **A logical-replication slot pins WAL while the consumer is down.** If the consumer stalls,
  WAL accumulates and can fill the disk — a real outage hazard. Monitor slot lag
  (`pg_replication_slots.confirmed_flush_lsn` vs the current LSN) and alert.
- **MySQL binlog flips to event-loss on expiry.** Once binlogs purge
  (`binlog_expire_logs_seconds`), a consumer that was down longer than the retention loses
  events. Size binlog retention to the worst-case consumer downtime and alert on it.
- **The stream is at-least-once.** Downstream consumers must be **idempotent** (dedup on the
  event id), exactly as the in-process dispatcher relies on the idempotency marker.

## Events reference aggregates by ID

An `events.Event` carries the emitting aggregate's **type and id** plus an opaque `Payload` —
never the aggregate object itself. This mirrors `ddd.v1.references` ([aggregates](../aggregates/)):
a handler re-loads whatever it needs from the IDs, in its own transaction, against the *current*
state — it never acts on a stale snapshot smuggled inside the event.

## Worked example: IAM `UserSuspended → revoke keys`

`testdata/iam` proves the shape end to end (`iam_events_test.go`):

- **Suspend** updates the `User` (a real mutation) and `Publish`es a `UserSuspended` event in
  one `Atomically`. The event row commits with the user change (and is discarded on rollback —
  no orphan row, verified directly against the ent `outbox` table).
- A registered handler reacts to `UserSuspended` by **revoking the user's API keys** — a write
  to the *separate* `ApiKey` aggregate, in the handler's own transaction.
- The test asserts the keys are **still present immediately after suspend** and **revoked only
  after dispatch** — eventual consistency, demonstrated, not a cross-aggregate transaction.

The `OutboxStore` is modelled as a **write-only** ent `outbox` table (account-scoped, with only
the durable event: `id, account_id, aggregate_type, aggregate_id, event_type, payload,
created_time`) so `Publish` writes it through the same `*ent.Tx` as the aggregate change. The
dispatcher's progress lives in separate sidecar tables (`outbox_cursors`,
`outbox_dead_letters`), never in the outbox.

The same worked example runs on **GORM** (`events_gorm_test.go`), wired with the reusable
`gormtx.GormOutboxStore`, `gormtx.GormOutboxCursorStore`, and `gormtx.GormIdempotencyStore`. The
GORM idempotency store is the SQL-backed exactly-once marker: `Record` inserts a primary-key row
**inside the handler's own GORM transaction**, so the marker commits atomically with the reaction
and a concurrent (or re-delivered) double-delivery loses the primary-key race and rolls its
duplicate effect back. The GORM fixture exercises exactly-once under both a sequential
cursor-rewind re-delivery and a genuine two-dispatcher concurrent race.

## Failure modes

- **`Publish` that does not enlist** — would reintroduce the dual write. The store's `Append`
  and `Publisher.Publish` both fail closed outside a transaction (`ErrNoTransaction`).
- **Stuck / poison events** — the cursor's head event blocks the stream (bounded head-of-line)
  until it succeeds or, after `maxAttempts` consecutive failures, is dead-lettered in the sidecar
  and the cursor advances past it. The sidecar dead-letter is your audit / alert / replay source.
- **Double-fire** — at-least-once delivery (a crash between deliver and cursor-advance
  re-delivers) means handlers must be idempotent (keyed on the event id).
- **Throughput / scale-out** — one dispatcher per service; throughput is bounded by the poll
  interval and batch size. The scale-out path for cross-server delivery is the **external**
  WAL/CDC → queue project (above), not multiple in-process dispatchers over one cursor.

## See also

- [Transactions](../transactions/) — the `Atomically` seam `Publish` enlists in.
- [Aggregates](../aggregates/) — why cross-aggregate reactions are events, not transactions.
