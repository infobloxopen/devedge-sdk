// Package cells implements the synchronous routing plane for cell-based
// development (WS-008): routing each tenant to an isolated cell with a fail-safe
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
// Out of scope here (later WS-008 phases): the move controller and its 7-phase
// protocol, storage fencing (L3), the event/outbox plane (L4), data-owning cells,
// and the devedge cell CLI. CloseForBarrier is provided so the future controller
// can drive a barrier, but this package does not orchestrate moves.
package cells
