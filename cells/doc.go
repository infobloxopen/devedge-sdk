// Package cells implements the synchronous routing plane for cell-based
// development: routing each tenant to an isolated cell with a fail-safe
// default, rejecting calls for a tenant that is moving, and enforcing the current
// route epoch cell-side. It is isolation, not load balancing — the router is a
// tenant→cell directory, and a tenant is pinned to exactly one cell at a time.
//
// Safety derives from a monotonic per-tenant route epoch and per-instance
// admission tokens, never from clocks, and is enforced in depth (no single
// barrier is trusted):
//
//   - [RoutingTable] is the single source of truth (Get/CompareAndSet/Watch).
//     [MemTable] is the dev/test backend; a Raft/etcd adapter implements the same
//     interface in a later module. Every mutation is an idempotent compare-and-swap
//     and route epochs never decrease.
//   - [Router] (L1) is a watch-fed cache that resolves a tenant to a [Decision],
//     falling back to the default cell for unknown tenants and when the table is
//     unreachable (reads stay available; the Stale flag lets writes fail closed).
//   - [GateRegistry] (L2) is the real correctness barrier: a per-instance,
//     per-tenant admission gate (TryEnter/Leave/CloseForBarrier) that handlers and
//     background workers alike must pass. [GateController] keeps gates aligned with
//     the table via its watch.
//   - [UnaryServerInterceptor]/[StreamServerInterceptor]/[HTTPMiddleware] wire L1+L2
//     into the request path: route, stamp metadata, reject mid-move (gRPC
//     UNAVAILABLE/ABORTED, HTTP 503 + Retry-After), and admit through the gate.
//
//   - [MoveController] drives the safe tenant-move protocol: a forward-only route
//     epoch advances across quiesce → drain → fence → event-pause → commit, every
//     transition an idempotent CAS, with rollback, crash recovery (the table is the
//     recovery state), a drain deadline (liveness only), and a [BudgetMeter].
//     [Fencer] (L3) and [EventBarrier] (L4) are the storage- and event-plane
//     barriers it drives; the persistence adapters supply the concrete backends,
//     and [MemFencer]/[MemEventBarrier] are the in-memory references. [Campaign]
//     schedules moves for rebalances; [PlacementPolicy] decides placement.
//   - [MemTable] and [FileTable] are the in-memory and file-backed routing tables
//     (the latter lets a CLI and a running service share routes); a Raft/etcd or
//     CR/GitOps adapter implements the same interface for production.
//
// Deferred and documented as the pluggable layer: data-owning cells' data
// catch-up (Phase 6 is a no-op for compute-only shared-DB cells), per-tenant
// PAUSE/DRAIN_QUEUE enforcement inside the event relay, and the etcd/CR
// routing-table backends. The model in spec/CellMove.tla checks the safety
// invariants of the move protocol.
package cells
