# Formal model of the tenant-move protocol

`CellMove.tla` is a TLA+ model of the cell-based-development **safe tenant-move
protocol** (drain-and-cutover). It exists so the protocol's safety invariants can
be checked on the design — exhaustively, against an adversary — independently of
the Go implementation.

## Why a model

The move protocol's correctness rests on **epochs, not clocks**: a monotonic
per-tenant route epoch, an L2 admission gate, an L3 storage fence, and an L4 event
fence, layered so no single failure lets a stale writer corrupt tenant state. That
is exactly the kind of property that is easy to assert and hard to get right. The
model lets a checker explore every interleaving of concurrent (including
stale/zombie) writers against the controller's CAS transitions.

## The eight invariants (proposal §10.1)

| # | Invariant | In the model |
|---|-----------|--------------|
| 1 | At most one cell is ACTIVE for any committed route epoch | `Inv1_SingleActivePerEpoch` |
| 2 | No write from an old epoch can commit once a newer epoch owns the tenant | `Inv2_NoStaleWriteCommits` |
| 3 | Every successful mutation has exactly one of: normal event / drain event / durable pending row | enforced by the transactional outbox (single local tx); not a state invariant here |
| 4 | `event_seq` is strictly increasing per tenant | `Inv4_EventSeqStrictlyIncreasing` |
| 5 | A request admitted after the barrier cannot commit in the source cell | `Inv5_NoCommitAfterBarrier` |
| 6 | A pre-barrier success is included in the source high-watermark before target activation | data-owning (P4) concern; modeled structurally by the drain-before-commit ordering |
| 7 | Route epochs never decrease, including rollback | `Inv7_MonotonicEpoch` |
| 8 | An old publisher cannot publish once a newer event epoch owns the tenant | `Inv8_NoStalePublish` |

## Mapping to the `cells` package

| Model variable / action | Go counterpart |
|---|---|
| `epoch`, `state`, `active`, `source`, `target`, `barrierEpoch` | `cells.TenantRoute` fields; `cells.State` |
| `Write` guard (cell + epoch + open gate) | `cells.GateRegistry` (L2) + the persistence write-guard / `cells.Fencer` (L3) |
| `Publish` guard (event epoch) | `cells.EventBarrier` (L4) + the outbox `event_epoch` |
| `BeginBarrier`/`CloseGates`/`EventBarrier`/`Commit`/`Rollback` | the phases of `cells.MoveController` (CAS via `RoutingTable.CompareAndSet`) |
| `committed` epochs strictly increasing | `ErrEpochRegression` (`MemTable` and the fencer reject backward epochs) |

The model is single-tenant on purpose: every invariant is per-tenant, so one
tenant with several cells and adversarial writers covers the safety surface while
keeping the state space small enough to check exhaustively.

## Running the checker

The spec is authored for TLC; it has **not** been run in this environment (no Java
runtime present here). To check it locally:

```sh
# with the TLA+ tools (tla2tools.jar) on the classpath:
tlc -config CellMove.cfg CellMove.tla
# or from the TLA+ Toolbox, load CellMove.tla and run the `Safety` invariant.
```

The bounds in `CellMove.cfg` (`Cells = {a, b}`, `MaxEpoch = 4`, `MaxEventSeq = 3`)
keep the model finite and fast while still forcing rollbacks, re-moves, and stale
writers.

## Relationship to the tests

The same invariants are exercised as **executable checks** in the `cells` package
tests (epoch monotonicity across moves and rollback, single active cell per
committed epoch, stale-writer rejection at the fence, no commit after the
barrier). The TLA+ model is the formal companion: the tests show the
implementation holds the invariants on the paths they cover; the model shows the
*design* holds them on every interleaving.
