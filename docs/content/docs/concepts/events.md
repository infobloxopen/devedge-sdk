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
> `Atomically`, a pluggable `persistence.OutboxStore` (in-memory dev default + an ent/SQL
> store), and an in-process `events.Dispatcher` with handler registration and idempotency.
> Backends: ent and in-memory. The seam is broker-neutral — no Kafka/NATS dependency in the
> core; a broker adapter would implement the same `OutboxStore`/`Dispatcher` seam outside it.

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

## Dispatch: at-least-once + idempotent

A **dispatcher** delivers committed events. The dev default is an in-process poller that:

1. **Claims** a batch of undelivered rows from the `OutboxStore`. The claim takes a short
   *lease* on each row so a second dispatcher does not also claim it. (The default uses a
   lease rather than `SELECT … FOR UPDATE SKIP LOCKED`, which needs ent's `sql/lock` — off in
   this repo.)
2. **Runs every registered handler** for the event's type, **each in its own `Atomically`**.
   A handler is therefore a normal, safe **single-aggregate write** — the cross-aggregate
   reaction is itself a well-formed aggregate change.
3. **Marks the event delivered** only once every handler has succeeded.

```go
d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
d.Subscribe("iam.v1.UserSuspended", "revoke-api-keys", func(ctx context.Context, evt events.Event) error {
    // Runs in its OWN transaction; revokes keys in a SEPARATE aggregate.
    return revokeKeysForUser(ctx, string(evt.Payload))
})
go d.Poll(ctx, time.Second, 100, logErr) // or call d.RunOnce(ctx, n) on a tick
```

Delivery is **at-least-once**. If a handler errors (or the process crashes), the event is left
undelivered and re-claimed later — so a handler can run **more than once**. Handlers must
therefore be **idempotent**. The dispatcher helps: it records each `(event id, handler)` pair
once the handler's transaction commits, and **skips an already-applied pair on redelivery** —
so a duplicate delivery is a no-op. The event `id` is the idempotency key (the same AIP-155
dedup idea the [request dedup interceptor](../../) uses). Design handlers so that re-applying
them is harmless regardless (e.g. "ensure these keys are revoked", not "decrement a counter").

## The consistency tradeoff

This is **eventual** consistency, and that is a deliberate tradeoff:

- Between the suspend commit and the dispatch, the user is suspended but the keys are **still
  live**. There is a window. Size it with the poll interval and design around it (e.g. the
  auth path can also check the user's status, not only the key).
- Ordering is **per-aggregate at best** — there is no global event order. Do not write a
  handler that assumes event X for aggregate A arrives before event Y for aggregate B.
- A **poison event** (a handler that always fails) is retried indefinitely; the `attempts`
  counter on the row lets you build a dead-letter / alert threshold.

The alternative — a synchronous two-aggregate transaction — buys strong consistency at the
cost of coupling two boundaries (and is disallowed by the aggregate gate). For cross-aggregate
rules, eventual consistency via events is the correct shape; reach for it deliberately.

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

The `OutboxStore` is modelled as an ent `outbox` table (account-scoped, with the F032 fields:
`id, account_id, aggregate_type, aggregate_id, event_type, payload, created_time,
delivered_time, attempts`) so `Publish` writes it through the same `*ent.Tx` as the aggregate
change.

## Failure modes

- **`Publish` that does not enlist** — would reintroduce the dual write. The store's `Append`
  and `Publisher.Publish` both fail closed outside a transaction (`ErrNoTransaction`).
- **Stuck / poison events** — the `attempts` counter supports a dead-letter / alert threshold.
- **Double-fire** — at-least-once delivery means handlers must be idempotent (keyed on the
  event id).
- **Lease contention on claim** — the default's throughput is bounded by the poll interval and
  batch size; a high-volume system would move to a broker adapter behind the same seam.

## See also

- [Transactions](../transactions/) — the `Atomically` seam `Publish` enlists in.
- [Aggregates](../aggregates/) — why cross-aggregate reactions are events, not transactions.
