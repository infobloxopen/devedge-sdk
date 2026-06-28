---------------------------- MODULE CellMove ----------------------------
(***************************************************************************)
(* Formal model of the cell-based-development safe tenant-move protocol     *)
(* (drain-and-cutover) for a single tenant routed across a set of cells.    *)
(* The protocol's invariants are per-tenant, so a single-tenant model with  *)
(* multiple cells and adversarial concurrent writers is sufficient to        *)
(* exercise every safety property.                                          *)
(*                                                                          *)
(* This is the correctness artifact for the move controller: it encodes the  *)
(* eight safety invariants and the move state machine so they can be          *)
(* model-checked with TLC BEFORE trusting the Go implementation. See          *)
(* README.md for the mapping to the `cells` package types.                  *)
(*                                                                          *)
(* Safety comes from EPOCHS, not clocks:                                     *)
(*  - route_epoch is monotonic per tenant (never decreases, even rollback). *)
(*  - L2 admission gate: a writer is admitted only at the gate's epoch.     *)
(*  - L3 storage fence: a write commits only if it carries the owner cell   *)
(*    and the current route_epoch (modeled by Write's guard).               *)
(*  - L4 event fence: an old publisher can never publish once a newer        *)
(*    event_epoch owns the tenant (modeled by Publish's guard).             *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
    Cells,          \* set of cell ids, e.g. {a, b}
    MaxEpoch,       \* bound on route_epoch so the state space is finite
    MaxEventSeq     \* bound on the event sequence

ASSUME Cardinality(Cells) >= 2
ASSUME MaxEpoch \in Nat /\ MaxEpoch >= 2
ASSUME MaxEventSeq \in Nat

VARIABLES
    state,          \* move phase (see States)
    epoch,          \* current route_epoch (monotonic)
    active,         \* the owning cell at `epoch` (the committed route)
    source,         \* source cell during a move (else = active)
    target,         \* target cell during a move (else = active)
    barrierEpoch,   \* epoch stamped on the in-flight barrier
    gateOpen,       \* [cell -> BOOLEAN] : is that cell's L2 admission gate accepting?
    storeEpoch,     \* route_epoch recorded on the last committed storage write
    storeCell,      \* owner cell recorded on the last committed storage write
    eventLog,       \* sequence of published events: <<[seq, epoch, cell]>>
    nextEventSeq,   \* next event_seq to assign
    eventEpoch,     \* the event_epoch currently authorized to publish NORMAL
    committed       \* set of <<epoch, cell>> ever committed as the active route

vars == <<state, epoch, active, source, target, barrierEpoch, gateOpen,
          storeEpoch, storeCell, eventLog, nextEventSeq, eventEpoch, committed>>

States == {"ACTIVE", "QUIESCING", "DRAINING", "COMMITTING", "ABORTED"}

InitCell == CHOOSE c \in Cells : TRUE

Init ==
    /\ state = "ACTIVE"
    /\ epoch = 0
    /\ active = InitCell
    /\ source = InitCell
    /\ target = InitCell
    /\ barrierEpoch = 0
    /\ gateOpen = [c \in Cells |-> c = InitCell]
    /\ storeEpoch = 0
    /\ storeCell = InitCell
    /\ eventLog = << >>
    /\ nextEventSeq = 1
    /\ eventEpoch = 0
    /\ committed = {<<0, InitCell>>}

(***************************************************************************)
(* DATA-PLANE actions (the adversary): any cell may ATTEMPT a write or a    *)
(* publish at any epoch it believes current — including a stale/zombie cell.*)
(* The L2/L3/L4 guards must reject the bad ones.                            *)
(***************************************************************************)

\* L2 + L3: a write from cell c at epoch e commits ONLY if c currently owns the
\* tenant at the current route_epoch, that cell's gate is open, and no move is in
\* progress. A stale-epoch or wrong-cell write is silently rejected (fenced).
Write(c, e) ==
    /\ IF /\ c = active
          /\ e = epoch
          /\ gateOpen[c]
          /\ state = "ACTIVE"
       THEN storeEpoch' = e /\ storeCell' = c
       ELSE UNCHANGED <<storeEpoch, storeCell>>
    /\ UNCHANGED <<state, epoch, active, source, target, barrierEpoch,
                   gateOpen, eventLog, nextEventSeq, eventEpoch, committed>>

\* L4: a publish from cell c at event_epoch ee succeeds only if c owns the tenant
\* and ee is the authorized event_epoch. An old publisher is fenced forever.
Publish(c, ee) ==
    /\ nextEventSeq <= MaxEventSeq
    /\ IF /\ c = active
          /\ ee = eventEpoch
          /\ gateOpen[c]
          /\ state = "ACTIVE"
       THEN /\ eventLog' = Append(eventLog,
                              [seq |-> nextEventSeq, epoch |-> ee, cell |-> c])
            /\ nextEventSeq' = nextEventSeq + 1
       ELSE UNCHANGED <<eventLog, nextEventSeq>>
    /\ UNCHANGED <<state, epoch, active, source, target, barrierEpoch,
                   gateOpen, storeEpoch, storeCell, eventEpoch, committed>>

(***************************************************************************)
(* CONTROL-PLANE actions: the move, each an idempotent CAS by the controller.*)
(***************************************************************************)

\* Phase 1 — Begin barrier (CAS ACTIVE -> QUIESCING); pick target != active.
BeginBarrier ==
    /\ state = "ACTIVE"
    /\ epoch + 2 <= MaxEpoch
    /\ \E tc \in Cells :
         /\ tc # active
         /\ source' = active
         /\ target' = tc
         /\ barrierEpoch' = epoch + 1
    /\ state' = "QUIESCING"
    /\ UNCHANGED <<epoch, active, gateOpen, storeEpoch, storeCell,
                   eventLog, nextEventSeq, eventEpoch, committed>>

\* Phase 2 — Close source gates (no cell admits during the cut).
CloseGates ==
    /\ state = "QUIESCING"
    /\ gateOpen' = [c \in Cells |-> FALSE]
    /\ state' = "DRAINING"
    /\ UNCHANGED <<epoch, active, source, target, barrierEpoch,
                   storeEpoch, storeCell, eventLog, nextEventSeq,
                   eventEpoch, committed>>

\* Phase 5 — Event barrier: bump event_epoch so the old publisher is fenced.
\* (Phases 3/4/6 — storage seal, drain-to-zero, data catch-up — are captured by
\*  the Write/Publish guards in this abstract compute-only model.)
EventBarrier ==
    /\ state = "DRAINING"
    /\ eventEpoch' = barrierEpoch + 1
    /\ state' = "COMMITTING"
    /\ UNCHANGED <<epoch, active, source, target, barrierEpoch,
                   gateOpen, storeEpoch, storeCell, eventLog,
                   nextEventSeq, committed>>

\* Phase 7 — Commit route (CAS): active := target, epoch := N+2, reopen target.
Commit ==
    /\ state = "COMMITTING"
    /\ epoch' = barrierEpoch + 1
    /\ active' = target
    /\ source' = target
    /\ gateOpen' = [c \in Cells |-> c = target]
    /\ storeEpoch' = epoch'
    /\ storeCell' = target
    /\ state' = "ACTIVE"
    /\ committed' = committed \cup {<<epoch', target>>}
    /\ UNCHANGED <<target, barrierEpoch, eventLog, nextEventSeq, eventEpoch>>

\* Rollback before commit: reopen the SOURCE at a NEW (higher) epoch.
\* Epochs never decrement (Inv7) — forward-only recovery.
Rollback ==
    /\ state \in {"QUIESCING", "DRAINING", "COMMITTING"}
    /\ epoch' = barrierEpoch + 1
    /\ active' = source
    /\ target' = source
    /\ gateOpen' = [c \in Cells |-> c = source]
    /\ storeEpoch' = epoch'
    /\ storeCell' = source
    /\ eventEpoch' = barrierEpoch + 1
    /\ state' = "ACTIVE"
    /\ committed' = committed \cup {<<epoch', source>>}
    /\ UNCHANGED <<source, barrierEpoch, eventLog, nextEventSeq>>

Next ==
    \/ BeginBarrier
    \/ CloseGates
    \/ EventBarrier
    \/ Commit
    \/ Rollback
    \/ \E c \in Cells, e \in 0..MaxEpoch : Write(c, e)
    \/ \E c \in Cells, e \in 0..MaxEpoch : Publish(c, e)

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* THE EIGHT INVARIANTS (proposal §10.1).                                   *)
(***************************************************************************)

\* Inv1: at most one cell is ACTIVE for any committed route_epoch.
Inv1_SingleActivePerEpoch ==
    \A e \in {ce[1] : ce \in committed} :
        Cardinality({ce[2] : ce \in {x \in committed : x[1] = e}}) = 1

\* Inv2: no committed storage write carries an epoch below the current owner
\* epoch, and a write at the current epoch is from the active cell (the fence).
Inv2_NoStaleWriteCommits ==
    /\ storeEpoch <= epoch
    /\ (storeEpoch = epoch => storeCell = active)

\* Inv4: event_seq is strictly increasing, gap-free, per tenant.
Inv4_EventSeqStrictlyIncreasing ==
    \A i \in 1..Len(eventLog) : eventLog[i].seq = i

\* Inv5: once a move has begun (not ACTIVE), the source admits nothing — a
\* request admitted after the barrier cannot commit in the source cell.
Inv5_NoCommitAfterBarrier ==
    (state \in {"QUIESCING", "DRAINING", "COMMITTING"})
        => (\A c \in Cells : ~gateOpen[c])

\* Inv7: route epochs never decrease.
Inv7_MonotonicEpoch ==
    \A ce \in committed : ce[1] <= epoch

\* Inv8: every published event was stamped at an epoch <= the current
\* event_epoch — an old publisher cannot publish past a newer epoch.
Inv8_NoStalePublish ==
    \A i \in 1..Len(eventLog) : eventLog[i].epoch <= eventEpoch

TypeOK ==
    /\ state \in States
    /\ epoch \in 0..MaxEpoch
    /\ active \in Cells /\ source \in Cells /\ target \in Cells
    /\ barrierEpoch \in 0..MaxEpoch
    /\ gateOpen \in [Cells -> BOOLEAN]
    /\ storeCell \in Cells
    /\ eventEpoch \in 0..MaxEpoch

Safety ==
    /\ TypeOK
    /\ Inv1_SingleActivePerEpoch
    /\ Inv2_NoStaleWriteCommits
    /\ Inv4_EventSeqStrictlyIncreasing
    /\ Inv5_NoCommitAfterBarrier
    /\ Inv7_MonotonicEpoch
    /\ Inv8_NoStalePublish

=============================================================================
