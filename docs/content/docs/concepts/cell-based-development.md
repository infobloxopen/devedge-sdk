---
title: Cell-Based Development
weight: 7
---

A **cell** is an independently deployable instance of a service that serves a **subset of tenants**,
fronted by a thin tenant→cell directory with a fail-safe **default cell** for everyone else. The
point is **isolation, not load balancing**: a bad deploy, a hot tenant, or a failed upgrade hits
**one cell, not all of them**. A tenant is pinned to exactly one cell at a time; the router is a
directory, not a traffic spreader.

The `cells` package provides the runtime: a routing plane (which cell serves a tenant) and a
move plane (relocating a tenant from one cell to another **safely**, without corrupting state).

## The two primitives

Everything reduces to two operations:

1. **Provision a version-pinned cell and assign a tenant subset to it.**
2. **Move a tenant from one cell to another, safely.**

Hot-tenant isolation, blast-radius spreading, even-distribution rebalancing, and progressive
rollout are all just *policies that schedule these two primitives*.

## Safety from epochs, not clocks

Correctness derives from a monotonic per-tenant **route epoch** that never decreases (even on
rollback), per-request **admission tokens**, and per-tenant **event sequence numbers** — never from
timestamps. Timeouts are used only for liveness (drain deadlines), never for safety.

Enforcement is **layered**, so no single failure lets a stale writer corrupt tenant state:

| Layer | Component | Role |
|-------|-----------|------|
| **L1 — routing** | [`Router`]({{< relref "/docs/reference/middleware" >}}) + middleware | Fast, cached, *not trusted for correctness*. Rejects calls for a moving tenant (gRPC `UNAVAILABLE`/`ABORTED`, HTTP `503`+`Retry-After`). |
| **L2 — admission** | `GateRegistry` | The real correctness barrier: a per-instance, per-tenant gate every handler **and background worker** must pass. |
| **L3 — storage fence** | `Fencer` + the persistence write-guard | The data layer rejects any tenant-scoped write whose admission token (cell + epoch) does not match the current fence — so a zombie writer is rejected at the row. |
| **L4 — event fence** | `EventBarrier` + the transactional outbox | Per-tenant `event_seq`/`event_epoch` so an old publisher cannot publish once a newer epoch owns the tenant. |

## The move protocol

`MoveController` drives a **drain-and-cutover** that advances the route epoch `N → N+2` through an
idempotent, recoverable sequence of compare-and-swap transitions:

```
ACTIVE(A,N) → QUIESCING → DRAINING → COPYING → COMMITTING → ACTIVE(B,N+2)
   begin barrier   close+drain   fence+pause   data catch-up   commit the cut
```

- **Forward-only.** Epochs never decrease; rollback reopens the source at a *higher* epoch, so a
  fenced stale writer stays fenced.
- **Recoverable.** The routing table *is* the recovery state — `Resume` re-reads the route and
  drives it forward (or rolls back past the deadline). A `COMMITTING` route always finishes forward
  (the cut is decided).
- **Compute-only by default.** When cells share a database the data-catch-up phase is a no-op; the
  route epoch + the L2 gate are the barrier. Data-owning cells add snapshot/CDC catch-up (the
  high-watermark fields on `TenantRoute` carry the proof) — a pluggable later phase.

## The availability budget is the design driver

A target of **99.995%/month ≈ 130 s of downtime per tenant per month** makes drain-and-cutover the
right default: a move makes only the *moving* tenant briefly unavailable, and that is what the budget
is for. `BudgetMeter` tracks per-tenant unavailability and **refuses or defers** a move that would
breach a tenant's remaining budget (unless forced). Correctness wins over availability: under
uncertainty the system **fails closed** (rejects, spends budget) rather than risk split-brain.

## Two profiles of one contract

The heavy machinery (storage fence, event plane, the full protocol) is opt-in by **statefulness**.
A **stateless** service needs only L1 + L2 and a route-flip move; a **stateful** service that owns
data or emits events gets L3 + L4 and the full protocol. Same `TenantRoute` contract either way —
`Fencer` and `EventBarrier` are nil for the stateless profile, turning those phases into no-ops.

## Using the API

```go
table := cells.NewFileTable("/var/lib/app/cell-routes.json") // or NewMemTable for tests
router := cells.NewRouter(table, cells.WithDefaultCell("default"))
_ = router.Start(ctx)

// L1+L2 on the request path:
grpcServer := grpc.NewServer(grpc.UnaryInterceptor(
    cells.UnaryServerInterceptor(router, gates)))

// Operate moves (or use the `de cell` CLI, which wraps these):
ctrl := cells.NewMoveController(table,
    cells.WithBudgetMeter(cells.NewBudgetMeter()),
    cells.WithDrainDeadline(10*time.Second))
_ = ctrl.Assign(ctx, "tenant-7", "cell-a", "alice")        // sticky first placement
_ = ctrl.Move(ctx, cells.MovePlan{TenantID: "tenant-7", FromCell: "cell-a", ToCell: "cell-b"})

// Rebalance many tenants under a placement policy:
camp := cells.NewCampaign(ctrl)
plan, _ := cells.PlanFromPolicy(ctx, tenants, cells.RoundRobinPolicy(cellIDs), nil)
_, _ = camp.Run(ctx, plan)
```

For stateful services, wire the persistence-backed `Fencer` and `EventBarrier` (the `gormtx`
adapter's `GormFencer` / `OutboxEventBarrier`) into the controller and install the tenant
write-guard.

## Correctness is proven, not asserted

The move protocol's safety invariants — at most one active cell per committed epoch, no stale write
commits, monotonic epochs, strictly increasing `event_seq`, no stale publish — are encoded as a TLA+
model in [`cells/spec/CellMove.tla`](https://github.com/infobloxopen/devedge-sdk/blob/main/cells/spec/CellMove.tla)
and exercised as executable checks in the package tests.

## Deferred (the pluggable layer)

Compute-only shared-DB cells are the default. The etcd / CR-GitOps routing-table backends (the
in-memory and file backends ship today behind the same interface), data-owning cells' CDC catch-up,
and per-tenant pause/drain enforcement inside the event relay are the documented extension points.
