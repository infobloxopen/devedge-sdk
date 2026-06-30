---
title: Cell-Based Development
weight: 7
---

A **cell** is an independently deployable instance of a service that serves a subset of tenants.
A thin tenant-to-cell directory routes each request to the right instance, with a fail-safe default
cell for tenants not yet explicitly assigned.

Cell-based development gives you blast-radius isolation: a bad deploy, a hot tenant, or a failed
upgrade affects only the cell that owns those tenants, not every tenant in the system. Each tenant
is pinned to exactly one cell at a time; the router is a directory, not a load balancer.

Use cell-based development when you need to limit the impact of deployments, contain noisy tenants,
or roll out changes progressively across a subset of your tenant fleet.

## Two primitives

The `cells` package reduces cell operations to two building blocks:

1. **Provision a version-pinned cell and assign a tenant subset to it.**
2. **Move a tenant from one cell to another without corrupting state.**

Strategies such as hot-tenant isolation, blast-radius spreading, even-distribution rebalancing, and
progressive rollout are policies that schedule these two primitives.

## Enforcement layers

Correctness derives from a monotonic per-tenant **route epoch** that never decreases (even on
rollback), per-request **admission tokens**, and per-tenant **event sequence numbers** — never from
timestamps. Timeouts are used only for liveness (drain deadlines), never for safety.

Enforcement is layered so that no single failure allows a stale writer — an instance still acting
for a tenant after that tenant has moved to a newer cell or epoch — to corrupt tenant state:

| Layer | Component | Role |
|-------|-----------|------|
| **L1 — routing** | [`Router`]({{< relref "/docs/reference/middleware" >}}) + middleware | Fast, cached. Rejects calls for a moving tenant (gRPC `UNAVAILABLE`/`ABORTED`, HTTP `503`+`Retry-After`). Not the correctness barrier. |
| **L2 — admission** | `GateRegistry` | The primary correctness barrier. Every handler and background worker must pass a per-instance, per-tenant gate. |
| **L3 — storage fence** | `Fencer` + the persistence write-guard | Rejects any tenant-scoped write whose admission token (cell + epoch) does not match the current fence, so a zombie writer — an instance still running for a tenant that has already moved to a newer cell or epoch — is rejected at the row. |
| **L4 — event fence** | `EventBarrier` + the transactional outbox | Per-tenant `event_seq`/`event_epoch` prevents an old publisher from publishing once a newer epoch owns the tenant. |

## Move protocol

`MoveController` drives a drain-and-cutover that advances the route epoch `N → N+2` through an
idempotent, recoverable sequence of compare-and-swap transitions:

```
ACTIVE(A,N) → QUIESCING → DRAINING → COPYING → COMMITTING → ACTIVE(B,N+2)
   begin barrier   close+drain   fence+pause   data catch-up   commit the cut
```

Each transition has the following properties:

- **Forward-only.** Epochs never decrease. A rollback reopens the source at a higher epoch, so a
  fenced stale writer remains fenced.
- **Recoverable.** The routing table is the recovery state. `Resume` re-reads the route and drives
  it forward, or rolls back past the deadline. A `COMMITTING` route always finishes forward because
  the cut is already decided.
- **Compute-only by default.** When cells share a database, the data-catch-up phase is a no-op. The
  route epoch and the L2 gate act as the barrier. Data-owning cells add snapshot and CDC
  (change-data capture) catch-up; the high-watermark fields on `TenantRoute` carry the proof. This
  is a pluggable later phase.

## Availability budget

A target of **99.995%/month (approximately 130 seconds of downtime per tenant per month)** is small
enough that drain-and-cutover stays within budget: a move makes only the moving tenant briefly
unavailable, and the protocol is built around that budget.

`BudgetMeter` tracks per-tenant unavailability and refuses or defers a move that would exceed a
tenant's remaining budget, unless the move is forced.

{{< callout type="warning" >}}
**The system fails closed under uncertainty.** When the outcome of a move is unclear, the SDK
rejects the operation and charges the budget rather than risk a split-brain state, in which two
cells both believe they own the same tenant. Correctness takes precedence over availability.
{{< /callout >}}

## Stateless and stateful profiles

The storage fence (L3), event fence (L4), and the full move protocol are opt-in based on whether
your service owns state.

- **Stateless services** need only L1 + L2 and a route-flip move.
- **Stateful services** that own data or emit events get L3 + L4 and the full protocol.

Both profiles use the same `TenantRoute` contract. For the stateless profile, `Fencer` and
`EventBarrier` are `nil`, which turns those phases into no-ops.

## A minimal example

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

## Formal verification

The move protocol's safety invariants — at most one active cell per committed epoch, no stale write
commits, monotonic epochs, strictly increasing `event_seq`, no stale publish — are encoded as a TLA+
model in [`cells/spec/CellMove.tla`](https://github.com/infobloxopen/devedge-sdk/blob/main/cells/spec/CellMove.tla)
and exercised as executable checks in the package tests.

## Extension points

Compute-only shared-database cells are the default. The following are documented extension points
you can plug in as your requirements grow:

- Alternative routing-table backends (etcd, CR-GitOps). The in-memory and file backends ship today
  behind the same interface.
- Data-owning cells' CDC catch-up.
- Per-tenant pause and drain enforcement inside the event relay.
