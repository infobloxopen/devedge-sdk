---
title: Transactions
weight: 4
---

A transaction lets you make several writes commit or roll back together, as a unit. devedge-sdk
exposes this through one backend-neutral seam, `persistence.TxRunner`, and its `Atomically`
method. The repositories you call inside `Atomically` automatically join the transaction, so you
write your domain logic without any `Begin`/`Commit` plumbing.

Use a transaction when a write must span more than one operation atomically — for example, load
a parent, check its state, then write a child, all or nothing. You do not need a transaction for
the common case: the generated CRUD handler does a single `Create`, `Update`, or `Delete`, which
is already atomic at the row level. For the "read, modify, write one resource" case, use an
[etag](#optimistic-concurrency-with-etag) instead.

## The seam

```go
// persistence/tx.go
type TxRunner interface {
    Atomically(ctx context.Context, fn func(ctx context.Context) error) error
}
```

`Atomically` runs `fn` inside a single backend transaction. The work commits when `fn` returns
`nil` and rolls back when `fn` returns an error or panics. There is no `Begin`/`Commit` to manage
and no per-call-site `WithTx(...)` plumbing: the transaction travels on `ctx`.

Propagation is context-based, which keeps the core engine-neutral. `Atomically` stashes an opaque
backend handle on `ctx` (`persistence.WithTx`). A transaction-aware repository reads that handle
(`persistence.TxFromContext`) and binds its writes to the transaction for the duration of `fn`.
Package `persistence` never imports an ORM or driver — the handle is an `any` that only the
backend's `TxRunner` and its generated repositories understand.

## Check-then-write recipe

The canonical shape is "load a parent, validate it, then write a child" — for example, "an order
item cannot be added once the order is SHIPPED".

```go
err := txRunner.Atomically(ctx, func(ctx context.Context) error {
    parent, err := parents.Get(ctx, parentID)   // tx-bound read
    if err != nil {
        return err
    }
    if parent.State == "SHIPPED" {
        return status.Error(codes.FailedPrecondition, "fleet is shipped")
    }
    _, err = children.Create(ctx, child)         // tx-bound write
    return err                                   // nil → commit, non-nil → roll back
})
```

Two rules make this work:

- **Pass the inner `ctx` to every repository call inside `fn`.** A call that uses the inner `ctx`
  joins the transaction; a call that uses the outer `ctx` (or a repository that ignores `ctx`)
  does not. See [Failure modes](#failure-modes).
- **Nested `Atomically` calls join the outer transaction.** A nested call is a no-op begin, not a
  second transaction, so a helper that wraps its own work in `Atomically` composes safely when
  called from within an outer `Atomically`.

## Backends

Three `TxRunner` implementations ship as development defaults: ent, in-memory, and GORM.

### ent

`protoc-gen-ent` generates a per-package `EntTxRunner` next to the repositories:

```go
txRunner := apikeyv1.NewEntTxRunner(client) // same *ent.Client the repos use
```

`Atomically` opens `client.Tx(ctx)`, stashes the `*ent.Tx` on `ctx`, and the generated
repositories resolve `tx.<Type>` instead of the constructor client. Every `Create`, `Update`,
`Delete`, and `Undelete` inside `fn` runs on the transaction. The work commits on success and
rolls back on error or panic.

### in-memory

`persistence.NewMemoryTxRunner` coordinates one or more `MemoryRepository` instances so a single
`Atomically` can span them — for example, a parent and a child repository:

```go
parents := persistence.NewMemoryRepository(func(p *Parent) string { return p.Id })
children := persistence.NewMemoryRepository(func(c *Child) string { return c.Id })
txRunner := persistence.NewMemoryTxRunner(parents, children)
```

It takes each participant's write lock for the duration of `fn` and snapshots the maps on entry.
On error or panic it restores the snapshots (rollback); on success it keeps them (commit). Because
the write lock is held across `fn`, a concurrent reader blocks until the transaction completes and
never sees partial state.

{{< callout type="info" >}}
**Pass every repository that one `Atomically` may write to `NewMemoryTxRunner`.** The in-memory
backend has no relational graph — it is flat maps for development and tests — so it can only span
the repositories you register with it.
{{< /callout >}}

### GORM

`persistence/gormtx` ships `GormTxRunner`, the sibling of `EntTxRunner` for the
`protoc-gen-storage` (GORM) backend:

```go
txRunner := gormtx.NewGormTxRunner(db) // same *gorm.DB the generated repos use
```

`Atomically` opens a GORM transaction (`db.WithContext(ctx).Begin()`), stashes the
transaction-scoped `*gorm.DB` on `ctx`, and the generated repositories resolve that handle (via
their `conn(ctx)` helper) instead of the constructor `*gorm.DB`. Every `Create`, `Update`,
`Delete`, and `Undelete` inside `fn` runs on the transaction; the work commits on success and
rolls back on error or panic. A nested call joins a GORM transaction already on `ctx`.

`persistence/gormtx` also ships `GormOutboxStore` and `GormIdempotencyStore` — the GORM-backed
transactional outbox and exactly-once idempotency stores — wired through the same `OutboxStore`
and `IdempotencyStore` seams. See [Events](../events/).

## Optimistic concurrency with etag

For a single-resource `Update`, the concurrency control is the resource `etag` (AIP-154), not a
transaction. The framework stamps a fresh `etag` on every `Create` and `Update` (`EtagMixin` on
the ent side, the in-memory repository on the dev side), and the `middleware/etag` interceptor
enforces the client's `If-Match`:

1. A client reads a resource and gets its `etag`.
2. On `Update`, the client echoes that value as `If-Match`.
3. If the stored `etag` has moved on — someone else updated the resource in between — the update
   is rejected with `persistence.ErrPreconditionFailed`, surfaced as a `412` or `428`.

This prevents a lost update without holding a transaction open across the read and the write. Use
`etag`/`If-Match` for the common "read, modify, write one resource" case; use `Atomically` when a
write must span more than one operation atomically.

## Failure modes

{{< callout type="warning" >}}
**A write on the outer `ctx` runs outside the transaction even though it sits inside `fn`.** This
is the worst case, because the code looks atomic but is not. A write issued with the outer `ctx`,
or through a repository that ignores `ctx`, does not join the transaction.

Mitigation: the generated repositories are transaction-aware by default and bind from `ctx`, so
always pass the inner `ctx`. A write path that must be transactional can call
`persistence.RequireTx(ctx)` first — it returns `persistence.ErrNoTransaction` when `ctx` is not
enrolled — so a caller who forgot to wrap the work fails loudly rather than writing silently.
This guards only callers that opt in; it cannot prove that a non-transaction-aware adapter
participated.
{{< /callout >}}

- **Two aggregates in one `Atomically`.** This is not prevented by the type system. Keep one
  consistency boundary per transaction. Cross-aggregate consistency is eventual — use the
  [transactional outbox and domain events](../events/) seam, not a two-aggregate transaction.

## Aggregates

Transactions are the foundation for aggregates — a consistency boundary that spans several
resources with invariants enforced on write. The aggregate machinery (an `AggregateRepository`, a
fail-closed boundary gate, and member write-redirection) builds on this seam. See
[Aggregates](../aggregates/).
