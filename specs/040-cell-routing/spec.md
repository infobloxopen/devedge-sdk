# F040 — Cell-based routing (synchronous plane: L1 + L2 + fail-safe default)

**Workstream:** WS-008 (cell-based development). **Phase:** P1 routing plane.
**Initiative proposal:** `development-hub/specs/cell-based-development-proposal.md`
(design basis: the external defense-in-depth dossier — epochs not clocks; never one barrier;
fail closed under uncertainty).

## 1. Scope

Ship the **runtime routing seams** in `devedge-sdk` that let a service route each tenant to a
cell with a **fail-safe default cell**, reject calls for a tenant mid-move, and enforce the
current route epoch cell-side. New package: **`github.com/infobloxopen/devedge-sdk/cells`**.

Deliver four pieces from the proposal §5:

- **RoutingTable** — the tenant→cell directory contract (`Get` / `CompareAndSet` / `Watch`) plus
  an **in-memory default impl** (the dev/test backend; an etcd/Raft adapter is a later module).
- **CellRouter (Router)** — a watch-fed local cache that resolves a tenant to a routing
  **Decision**, returning the **default cell** for unknown tenants and when the table is
  unreachable (fail-safe for reads), and flagging uncertainty so writes can fail closed.
- **TenantGate (L2)** — the per-instance admission barrier (`TryEnter` / `Leave` /
  `CloseForBarrier`) keyed by tenant, with monotonic `route_epoch`, per-request admission tokens,
  in-flight tracking, and deadline-bounded drain. Usable by handlers **and** background workers.
- **L1 middleware** — gRPC unary + stream interceptors and an HTTP middleware that resolve the
  route, stamp internal route metadata, reject mid-move (gRPC `UNAVAILABLE`/`ABORTED`; HTTP `503`
  + `Retry-After`), and (gRPC) acquire/release the L2 admission token.

### Out of scope (later phases — do NOT build here; scope-gated)
The move **controller** + 7-phase protocol orchestration (preflight/rollback/recovery); the
etcd/Raft RoutingTable backend; **L3** storage fencing; **L4** event/outbox plane; data-owning
cells; the `devedge cell` CLI; budget-metered campaigns; per-cell rollout. The TenantGate exposes
`CloseForBarrier` (so the future controller can drive a barrier) but this slice does not implement
the controller itself.

## 2. Acceptance criteria

- **AC-1 Directory + CAS.** `RoutingTable.Get` returns a tenant's `TenantRoute` or `ErrNoRoute`.
  `CompareAndSet` applies a new route only if the stored `(RouteEpoch, State)` matches the expected
  pair (create when absent via the zero/`StateUnknown` expectation); it rejects a regression with
  `ErrEpochRegression` (**invariant 7: epochs never decrease**) and a stale precondition with
  `ErrCASConflict`. The in-memory impl is concurrency-safe.
- **AC-2 Watch.** `Watch` streams route changes (add/update/delete) and the channel closes on ctx
  cancel. A `Router` started against the table converges its cache from watch events.
- **AC-3 Fail-safe default.** A tenant with no route resolves to the configured **default cell**
  with `AdmitNew=true`. With the table unreachable and no cached entry, an unknown tenant still
  resolves to the default cell (`Stale=true`) — reads stay available.
- **AC-4 Reject mid-move (L1).** When the resolved state is moving
  (`QUIESCING`/`DRAINING`/`COPYING`/`COMMITTING`), a new call is rejected: gRPC `UNAVAILABLE` with
  `RetryInfo`, HTTP `503` with `Retry-After`. `ACTIVE`/`ACTIVE_NEW` admit; `ABORTED` routes to the
  active cell (move abandoned, tenant stays).
- **AC-5 Wrong-cell defense (L1, gRPC).** If a request reaches an instance whose `localCell` is not
  the tenant's resolved cell (stale upstream router), it is rejected `UNAVAILABLE` — the gateway is
  not trusted for correctness.
- **AC-6 Admission epoch enforcement (L2).** `TryEnter(tenant, epoch)` issues an `AdmissionToken`
  with the next per-tenant `admission_seq` only when the gate is `OPEN`, `acceptingNew`, and
  `epoch == gate.routeEpoch`; otherwise `ErrTenantDraining` or `ErrStaleRouteEpoch`. `Leave`
  releases it. (**Invariant 5**: a request admitted after a barrier begins cannot be admitted in
  the source cell.)
- **AC-7 Barrier drain.** `CloseForBarrier(ctx, tenant, barrierEpoch)` flips the gate to
  `DRAINING`/`acceptingNew=false`, records the `cutoff_seq`, and blocks until in-flight reaches
  zero **or** ctx deadline; on deadline it returns the cutoff marked **forced** (never waits
  forever). After commit/reconcile to a higher epoch the gate reopens at the new epoch; reconciling
  to a cell ≠ `localCell` closes the gate (tenant no longer served here).
- **AC-8 Fail closed for uncertain writes.** When the route is uncertain (table unreachable, no
  fresh cached `ACTIVE`), a **mutating** gRPC call is rejected `UNAVAILABLE` (fail closed) while a
  read is served from the default/last-known cell (fail safe). Mutation classification is a
  configurable predicate defaulting to AIP method-name prefixes.
- **AC-9 Backwards-compatible no-op.** A service that wires the interceptor with no routes
  populated and `localCell == cells.DefaultCellID` behaves exactly as today: every tenant resolves
  to the default cell and is admitted. Adopting the seam costs nothing until routes exist.
- **AC-10 Observability.** Rejections, stale-epoch rejections, and gate in-flight are emitted via
  the OTel **API** (no SDK import in core); the `Router` exposes a `health.Check` reporting whether
  its watch has synced. Structured fields (`tenant_id`, `route_epoch`, `cell_id`, `admission_seq`)
  are attached to context for the logging middleware.

## 3. Failure modes (each → its defense)

| Failure | Defense (where) |
|---|---|
| Stale gateway routes to old cell after move | L2 gate `ErrStaleRouteEpoch`/`ErrTenantDraining` (AC-5/6) |
| Table unreachable, tenant unknown | fail-safe default for reads (AC-3); fail-closed for writes (AC-8) |
| Epoch replay / regression | `CompareAndSet` rejects `ErrEpochRegression` (AC-1) |
| Drain never completes (hung in-flight) | deadline-bounded `CloseForBarrier`, forced cutoff (AC-7) |
| Background worker bypasses the edge | worker calls `TryEnter`/`Leave` directly — same barrier (AC-6) |
| Retry storm against a moving tenant | `RetryInfo` / `Retry-After` hint (AC-4) |
| Malicious tenant header | tenant derived from trusted identity upstream (`middleware.TenantIDFromContext`), never a client route header |

## 4. Tasks (model-routing tags)

- **T1 [C]** `cells` core types: `State` enum + helpers, `TenantRoute`, errors, `RoutingTable`
  interface, `RouteEvent`. (correctness contract)
- **T2 [C]** In-memory `RoutingTable` (`MemTable`): CAS + monotonic-epoch guard + watch fan-out.
- **T3 [C]** `Router`: watch-fed cache, `Resolve→Decision`, default-cell fallback, staleness,
  `health.Check`.
- **T4 [C]** `TenantGate` + `GateRegistry`: `TryEnter`/`Leave`/`CloseForBarrier`, admission tokens,
  deadline drain, `Reconcile(route)` for watch-driven epoch maintenance.
- **T5 [C]** L1 middleware: gRPC unary + stream interceptors, HTTP middleware, gRPC/HTTP status
  mapping, header stamping, mutation predicate, metrics.
- **T6 [S]** Tests: table CAS/watch, router fallback/staleness, gate epochs/drain, middleware
  route/reject, and an in-memory end-to-end `ACTIVE→QUIESCING→DRAINING→commit` + stale-epoch
  rejection. (mechanical given the behaviors above)
- **T7 [S]** Package doc (`doc.go`) + README/CLAUDE note + COMPAT note (new package, additive).

## 5. Verification gate
`make build` + `make vet` + `make test` green (root module + go.work). Scope-gate the diff: every
file traces to a task above; no move controller, no L3/L4, no etcd backend, no CLI. The package
adds **no new third-party dependency** to core beyond what's already required (grpc, otel API).
