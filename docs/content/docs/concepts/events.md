---
title: Events
weight: 6
---

Events let one part of your service react to a change in another part without sharing a database
transaction. You record a domain event in the same commit as the change that caused it, and a
dispatcher delivers that event to a handler that runs its own follow-up work in a separate
transaction. The two changes are linked by eventual consistency rather than a shared
transaction.

Use events when a rule spans more than one [aggregate](../aggregates/) — for example, "when a
user is suspended, revoke that user's API keys", or "when an account is closed, suspend its
users". A user and its API keys are different aggregates, each its own consistency boundary, so
the reaction cannot be a single transaction. The SDK gives you a transactional outbox and an
in-process dispatcher to connect them safely.

The pieces are:

- An `events.Publisher` that records an event in the current transaction.
- A pluggable `persistence.OutboxStore` that holds the events (in-memory for development, plus
  ent/SQL and GORM stores for production).
- An in-process `events.Dispatcher` that reads the outbox and runs your handlers.

This page covers same-database reactions inside one service. Publishing events to other services
over a message queue is a separate concern; see [Cross-service delivery](#cross-service-delivery)
at the end.

## A minimal example

You publish an event inside the same `Atomically` block as the change that triggers it. The
event is recorded only if the change commits.

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

A dispatcher then delivers the committed event to a handler that does the follow-up work:

```go
cursors := gormtx.NewGormOutboxCursorStore(db) // the dispatcher's sidecar
d := events.NewDispatcher(store, cursors, tx, gormtx.NewGormIdempotencyStore(db))
d.Subscribe("iam.v1.UserSuspended", "revoke-api-keys", func(ctx context.Context, evt events.Event) error {
    // Runs in its OWN transaction; revokes keys in a SEPARATE aggregate.
    return revokeKeysForUser(ctx, string(evt.Payload))
})
go d.Poll(ctx, time.Second, 100, logErr) // or call d.RunOnce(ctx, n) on a tick
```

The rest of this page explains what each step guarantees, how the outbox is stored, and what can
go wrong.

## Why an outbox

A cross-aggregate reaction without an outbox is a *dual write*: you commit the first change,
then make a separate call to do the second.

```go
// WRONG: a dual write. If the process crashes after the user commit but before the
// keys are revoked, the keys are never revoked — the system is permanently inconsistent.
suspend(user)          // commit aggregate A
revokeKeys(user.ID)    // separate write to aggregate B — may never happen
```

If the process crashes between the two writes, the second one is lost forever and the system is
permanently inconsistent. You cannot fix this by wrapping both writes in one `Atomically`,
because that is a two-aggregate transaction, which the [aggregate boundary gate](../aggregates/)
rejects.

The outbox solves the problem by making the *intent to react* durable in the same transaction as
the change that triggers it. `Publish` writes an outbox row through the current transaction, so
the row commits if and only if the change commits.

```go
err := tx.Atomically(ctx, func(ctx context.Context) error {
    if _, err := users.Update(ctx, id, suspended, "status"); err != nil {
        return err
    }
    return publisher.Publish(ctx, events.Event{
        Type:          "iam.v1.UserSuspended",
        AggregateType: "User",
        AggregateID:   id,
        Payload:       []byte(id),
    })
})
```

If `fn` returns an error, the transaction rolls back and both the user change and the outbox row
are discarded — the event cannot exist without the change that justifies it, and the change
cannot commit without recording the event.

{{< callout type="warning" >}}
**Call `Publish` only inside `Atomically`.** Called outside a transaction it returns
`persistence.ErrNoTransaction` and writes nothing, rather than recording an event that is not
atomic with any change. This is the same `RequireTx` guard the
[transaction seam](../transactions/) uses.
{{< /callout >}}

## Dispatching events

A dispatcher delivers committed events to handlers for same-database cross-aggregate reactions.

The outbox table is *write-only*: the only writes to it are the producer's insert (above) and
retention's partition drop. The dispatcher never claims, leases, marks, or deletes an outbox
row. Instead it reads the outbox as a *forward cursor* — it remembers the position of the last
event it processed and reads forward from there.

A single pass does four things:

1. **Load the position.** The dispatcher reads its cursor — the `(created_time, id)` of the last
   event it consumed — from a separate store (`persistence.OutboxCursorStore`). The cursor lives
   outside the outbox so the outbox stays write-only. This separate store is called the
   *sidecar*.
2. **Read forward.** `ReadAfter(cursor, limit)` runs `WHERE (created_time, id) > cursor ORDER BY
   created_time, id LIMIT n` — a read-only scan that returns events in commit order.
3. **Run the handlers.** For each event, the dispatcher runs every handler registered for that
   event's type, each inside its own `Atomically`. A handler is therefore a normal,
   single-aggregate write.
4. **Advance the cursor.** The dispatcher moves the cursor in the sidecar past each delivered
   event. The outbox row itself is never touched.

Run one dispatcher instance per service. (To scale delivery across servers, see
[Cross-service delivery](#cross-service-delivery).)

## Delivery guarantees

Delivery is *at-least-once*. If a handler returns an error, or the process crashes between
delivering an event and advancing the cursor, the cursor stays where it was, so a later pass
re-delivers the event. A handler can therefore run more than once for the same event.

Because of that, **handlers must be idempotent.** The dispatcher helps: it records each
`(event id, handler)` pair inside the handler's own transaction, and on re-delivery it skips a
pair it has already applied, so a duplicate delivery does no work. The combination of
at-least-once delivery and this idempotency marker produces an *exactly-once effect*, even if
cursor updates race. The event `id` is the idempotency key (the same AIP-155 deduplication idea
the [request-deduplication interceptor](../../) uses).

{{< callout type="info" >}}
**Design handlers to be safe to re-apply, regardless of the marker.** Prefer "ensure these keys
are revoked" over "decrement a counter". Re-running the first is harmless; re-running the second
corrupts state.
{{< /callout >}}

## Consistency tradeoffs

Events give you eventual consistency between two aggregates, not strong consistency. That trade
has consequences you design around:

- **There is a delivery window.** Between the suspend commit and the dispatch, the user is
  suspended but the keys are still live. Size the window with the poll interval, and design for
  it — for example, have the auth path also check the user's status, not only the key.
- **Ordering is per-aggregate, not global.** The forward cursor reads `ORDER BY created_time,
  id`, so events for a single aggregate arrive in commit order. There is no stronger order across
  aggregates. Do not write a handler that assumes event X for aggregate A arrives before event Y
  for aggregate B.

The alternative — a synchronous two-aggregate transaction — gives you strong consistency but
couples two consistency boundaries that should be able to fail independently, and the
[aggregate gate](../aggregates/) rejects it. For cross-aggregate rules, use events.

## How events are stored

An `events.Event` carries the emitting aggregate's **type and id** plus an opaque `Payload` —
never the aggregate object itself. This mirrors `ddd.v1.references`
([aggregates](../aggregates/)): a handler reloads whatever it needs from the IDs, in its own
transaction, against the *current* state. It never acts on a stale snapshot carried inside the
event.

The outbox table holds only the durable event and is write-only. Its columns are `id`,
`account_id`, `aggregate_type`, `aggregate_id`, `event_type`, `payload`, and `created_time`. The
dispatcher's progress lives in separate sidecar tables (`outbox_cursors`,
`outbox_dead_letters`), never in the outbox itself. Keeping the outbox write-only has two
benefits:

- The table never accumulates per-poll lease churn or vacuum bloat from repeated updates and
  deletes.
- A change-data-capture consumer sees only inserts, which is the simplest stream to tail (see
  [Cross-service delivery](#cross-service-delivery)).

### Backends

The outbox and its sidecar ship on three backends:

| Backend | OutboxStore | Cursor store | Idempotency store |
|---|---|---|---|
| in-memory | built-in (dev default) | built-in | built-in |
| ent / SQL | ent `outbox` table | sidecar tables | sidecar marker |
| GORM | `gormtx.GormOutboxStore` | `gormtx.GormOutboxCursorStore` | `gormtx.GormIdempotencyStore` |

The seam is broker-neutral: there is no Kafka, NATS, or CDC dependency in the core. The GORM
idempotency store is the SQL-backed exactly-once marker — `Record` inserts a primary-key row
inside the handler's own GORM transaction, so the marker commits atomically with the reaction. A
concurrent or re-delivered double delivery loses the primary-key race, and its duplicate effect
rolls back.

### Retention

Retention removes aged events by **dropping whole partitions**, not by deleting rows. The outbox
table is range-partitioned on `created_time` (monthly partitions; PostgreSQL declarative
partitions, MySQL `PARTITION BY RANGE (TO_DAYS(created_time))`).
`OutboxRetention.DropPartitionsBefore(t)` drops partitions older than `t` — an O(1) DDL
operation that never scans or deletes individual rows. A partition that overlaps `t` is kept, so
an in-window row is never lost.

The SDK does not run a scheduler for you. Wire `gormtx.RunRetention` into your own cron job,
Kubernetes `CronJob`, or ticker. `RunRetention` also rolls the partition window forward, so the
current month and the next always have a partition to append into.

{{< callout type="warning" >}}
**Drop only partitions that are both older than the retention window and fully behind the
dispatch cursor.** Otherwise the in-process dispatcher can lose an event it has not yet consumed.
Size the retention window comfortably longer than the dispatcher could ever fall behind.
{{< /callout >}}

Retention is validated on PostgreSQL and MySQL with testcontainers. The in-memory and SQLite
backends have no declarative partitioning, so they model the same contract as "forget rows older
than `t`".

## Failure modes

- **`Publish` outside a transaction.** This would reintroduce the dual write. Both
  `OutboxStore.Append` and `Publisher.Publish` fail closed with `persistence.ErrNoTransaction`
  when called outside a transaction, so the event is never written.
- **A poison event.** A handler that always fails blocks the cursor at that event, because the
  cursor cannot advance past an unprocessed event without skipping a gap. This is *bounded
  head-of-line blocking*: the batch stops at the failing event. After `maxAttempts` consecutive
  failures on that same event (default 5, configurable with `events.WithMaxAttempts`), the
  dispatcher records the event to a *dead-letter* table in the sidecar and advances the cursor
  past it, so one permanently-failing event does not wedge the stream forever. Both the failure
  count and the dead-letter live in the sidecar, never in an outbox column. The dead-letter table
  is your source for auditing, alerting, and replay.
- **Duplicate delivery.** At-least-once delivery means a crash between deliver and cursor-advance
  re-delivers the event. Handlers must be idempotent, keyed on the event `id`.
- **Throughput.** One dispatcher per service; throughput is bounded by the poll interval and
  batch size. To deliver across servers, use the external consumer described below, not multiple
  in-process dispatchers over one cursor.

## Worked example: IAM `UserSuspended` revokes keys

`testdata/iam` proves the shape end to end (`iam_events_test.go`):

- **Suspend** updates the `User` and publishes a `UserSuspended` event in one `Atomically`. The
  event row commits with the user change, and is discarded on rollback — verified directly
  against the ent `outbox` table.
- A registered handler reacts to `UserSuspended` by **revoking the user's API keys**, a write to
  the separate `ApiKey` aggregate in the handler's own transaction.
- The test asserts that the keys are still present immediately after suspend, and revoked only
  after dispatch — eventual consistency, demonstrated, not a cross-aggregate transaction.

The same example runs on GORM (`events_gorm_test.go`), wired with `gormtx.GormOutboxStore`,
`gormtx.GormOutboxCursorStore`, and `gormtx.GormIdempotencyStore`. The GORM fixture exercises the
exactly-once effect under both a sequential cursor-rewind re-delivery and a genuine
two-dispatcher concurrent race.

## Cross-service delivery

Publishing events to a message queue so that *other* services react is out of the SDK's scope.
A full in-process logical-replication or binlog engine (Debezium-style) is heavy and would pull
a CDC dependency into the core, so the SDK ships `persistence.OutboxCDCConsumer` as an interface
only, with no implementation. A separate project tails the write-only outbox — PostgreSQL logical
replication (WAL), MySQL binlog, or Debezium — and publishes to queues. Because the outbox is
write-only, that consumer only ever sees inserts and partition drops.

{{< callout type="info" >}}
**Notes for building the external CDC consumer.** These came out of the spike and are worth
knowing before you build the queue-publishing project:

- A partitioned outbox needs `CREATE PUBLICATION … WITH (publish_via_partition_root = true)` so
  PostgreSQL logical replication publishes inserts as the parent `outbox` table rather than as
  per-partition child tables.
- A logical-replication slot pins WAL while the consumer is down. If the consumer stalls, WAL
  accumulates and can fill the disk — a real outage hazard. Monitor slot lag
  (`pg_replication_slots.confirmed_flush_lsn` against the current LSN) and alert on it.
- MySQL binlog drops to event loss on expiry. Once binlogs purge
  (`binlog_expire_logs_seconds`), a consumer that was down longer than the retention loses
  events. Size binlog retention to the worst-case consumer downtime and alert on it.
- The stream is at-least-once. Downstream consumers must be idempotent (deduplicate on the event
  `id`), exactly as the in-process dispatcher relies on its idempotency marker.
{{< /callout >}}

## See also

- [Transactions](../transactions/) — the `Atomically` seam that `Publish` enlists in.
- [Aggregates](../aggregates/) — why cross-aggregate reactions are events, not transactions.
