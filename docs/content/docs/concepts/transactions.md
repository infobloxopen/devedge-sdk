---
title: Transactions
weight: 4
---

Most write handlers touch one resource and need no transaction — the generated CRUD
handler does a single `Create`/`Update`/`Delete` and is already atomic at the row level.
But some writes must span more than one operation **atomically**: load a parent, check
its state, then write a child — all or nothing. devedge-sdk expresses that through one
backend-neutral seam: **`persistence.TxRunner`**.

## The seam

```go
// persistence/tx.go
type TxRunner interface {
    Atomically(ctx context.Context, fn func(ctx context.Context) error) error
}
```

`Atomically` runs `fn` inside a single backend transaction. The repositories used
**inside** `fn` are transaction-bound automatically: the work commits when `fn` returns
`nil` and rolls back when `fn` returns an error (or panics). There is no `Begin`/`Commit`
to manage and no per-call-site `WithTx(...)` plumbing — the transaction travels on
`ctx`.

Propagation is context-based and clean-core safe. `Atomically` stashes an opaque backend
handle on `ctx` (`persistence.WithTx`); a tx-aware repository reads it
(`persistence.TxFromContext`) and binds its writes to that transaction for the duration
of `fn`. Package `persistence` never imports an ORM or driver — the handle is an `any`
that only the backend's `TxRunner` and its generated repositories understand.

## Atomic check-then-write recipe

The canonical shape — "an order item cannot be added once the order is SHIPPED",
"a fleet's vehicle is written only after the fleet is loaded and checked":

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

Pass the **inner** `ctx` to every repository call inside `fn`. A call that uses the inner
`ctx` participates in the transaction; a call that uses the outer `ctx` (or a repository
that ignores `ctx`) does not — see *Failure modes* below.

Nested `Atomically` calls **join** the outer transaction (a no-op begin); they do not open
a second transaction. So a helper that wraps its own work in `Atomically` composes safely
when called from within an outer `Atomically`.

## Backends

Three `TxRunner` implementations ship as development defaults.

### ent

`protoc-gen-ent` generates a per-package `EntTxRunner` next to the repositories:

```go
txRunner := apikeyv1.NewEntTxRunner(client) // same *ent.Client the repos use
```

`Atomically` opens `client.Tx(ctx)`, stashes the `*ent.Tx` on `ctx`, and the generated
repositories resolve `tx.<Type>` instead of the constructor client — so every
`Create`/`Update`/`Delete`/`Undelete` inside `fn` runs on the transaction. Commit on
success; rollback on error or panic.

### in-memory

`persistence.NewMemoryTxRunner` coordinates one or more `MemoryRepository` instances so a
single `Atomically` can span them (e.g. a parent and a child repository):

```go
parents := persistence.NewMemoryRepository(func(p *Parent) string { return p.Id })
children := persistence.NewMemoryRepository(func(c *Child) string { return c.Id })
txRunner := persistence.NewMemoryTxRunner(parents, children)
```

It takes each participant's write lock for the duration of `fn` and snapshots the maps at
entry; on error or panic it restores the snapshots (rollback), on success it keeps them
(commit). Because the write lock is held across `fn`, a concurrent reader blocks until the
transaction completes and therefore never sees partial state.

> The in-memory backend has no relational graph — it is flat maps for development and
> tests. Pass every repository that may be written in one `Atomically` to
> `NewMemoryTxRunner`.

### GORM

`persistence/gormtx` ships a `GormTxRunner`, the sibling of `EntTxRunner` for the
`protoc-gen-storage` (GORM) backend:

```go
txRunner := gormtx.NewGormTxRunner(db) // same *gorm.DB the generated repos use
```

`Atomically` opens a GORM transaction (`db.WithContext(ctx).Begin()`), stashes the
transaction-scoped `*gorm.DB` on `ctx`, and the generated repositories resolve that handle
(via their `conn(ctx)` helper) instead of the constructor `*gorm.DB` — so every
`Create`/`Update`/`Delete`/`Undelete` inside `fn` runs on the transaction. Commit on
success; rollback on error or panic. A nested call joins a GORM transaction already on
`ctx`. `persistence/gormtx` also ships `GormOutboxStore` and `GormIdempotencyStore` (the
GORM-backed transactional outbox and exactly-once idempotency stores) wired through the
same `OutboxStore` / `IdempotencyStore` seams.

## etag is the optimistic-concurrency token

For a single-resource `Update`, the concurrency control is the resource **`etag`** (AIP-154),
not a transaction. The framework stamps a fresh `etag` on every `Create`/`Update`
(`EtagMixin` on the ent side, the in-memory repository on the dev side) and the
`middleware/etag` interceptor enforces the client's `If-Match`:

- A client reads a resource and gets its `etag`.
- On `Update` the client echoes it as `If-Match`.
- If the stored `etag` has moved on (someone else updated the resource in between), the
  update is rejected — `persistence.ErrPreconditionFailed`, surfaced as a `412`/`428` — so a
  lost update is prevented without holding a transaction open across the read and the write.

Use `etag`/`If-Match` for the common "read-modify-write one resource" case; reach for
`Atomically` when a write must span more than one operation atomically.

## Failure modes

- **Transaction not propagated (worst case — looks atomic, isn't).** A write issued with
  the outer `ctx`, or through a repository that ignores `ctx`, runs **outside** the
  transaction even though it sits lexically inside `fn`. Mitigation: the tx-aware
  repositories are the generated default, and they bind from `ctx`; always pass the inner
  `ctx`. A write path that must be transactional can call `persistence.RequireTx(ctx)`
  first — it returns `persistence.ErrNoTransaction` when `ctx` is not enrolled — so a
  caller who forgot to wrap the work fails loudly rather than writing silently. (This
  guards only callers that opt in; it cannot prove a non-tx-aware adapter participated.)
- **Two aggregates in one `Atomically`.** Not type-prevented. Keep one consistency
  boundary per transaction; cross-aggregate consistency is eventual — via the
  [transactional outbox + domain events](../events/) seam — not a two-aggregate transaction.

## Aggregates

Transactions are the foundation for **aggregates** — a consistency boundary spanning
several resources with invariants enforced on write. The aggregate machinery (an
`AggregateRepository`, a fail-closed boundary gate, member write-redirection) builds on
this seam. See [Aggregates](../aggregates/).
