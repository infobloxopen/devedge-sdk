package cells

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// Move-controller errors.
var (
	// ErrNoRouteToMove means a move was requested for a tenant with no existing
	// route. First placement goes through [MoveController.Assign], not Move.
	ErrNoRouteToMove = errors.New("cells: cannot move a tenant with no route (use Assign for first placement)")
	// ErrMoveDeadline means a move's liveness deadline elapsed before the source
	// drained or the event plane quiesced; the controller rolls the tenant back.
	ErrMoveDeadline = errors.New("cells: move deadline elapsed before the cut")
	// ErrSameCell means From and To name the same cell — nothing to move.
	ErrSameCell = errors.New("cells: move source and target are the same cell")
)

// Drainer closes a tenant's source gate for a move barrier and blocks until the
// in-flight set drains or the barrier deadline forces the cut. It is the move
// controller's view of the L2 admission gate (Phase 2). A nil Drainer means the
// controller does not coordinate a local drain (e.g. drains happen out of band).
type Drainer interface {
	// CloseAndDrain stops new admissions for tenantID and waits for in-flight work
	// to clear, returning the [Cutoff] (Forced when the deadline fired first). The
	// barrier epoch fences late admissions.
	CloseAndDrain(ctx context.Context, tenantID string, barrierEpoch uint64) (Cutoff, error)
}

// GateDrainer adapts a [GateRegistry] to the [Drainer] interface so the move
// controller can drive the local L2 barrier directly.
type GateDrainer struct{ reg *GateRegistry }

// NewGateDrainer wraps reg as a [Drainer].
func NewGateDrainer(reg *GateRegistry) *GateDrainer { return &GateDrainer{reg: reg} }

// CloseAndDrain implements [Drainer] by driving [GateRegistry.CloseForBarrier].
// It returns ctx.Err() alongside the Cutoff so a forced (deadline) cut surfaces
// as an error the controller can act on, while a clean drain returns nil.
func (d *GateDrainer) CloseAndDrain(ctx context.Context, tenantID string, barrierEpoch uint64) (Cutoff, error) {
	cut := d.reg.CloseForBarrier(ctx, tenantID, barrierEpoch)
	if cut.Forced {
		return cut, ctx.Err()
	}
	return cut, nil
}

// MovePlan describes one tenant's move from FromCell to ToCell. CohortID is set
// when the move is part of a cohort. Force bypasses the budget check.
type MovePlan struct {
	TenantID string
	FromCell string
	ToCell   string
	Operator string
	CohortID string // optional: the logical cohort this move belongs to
	Force    bool   // bypass the budget gate
}

// ControllerOption configures a [MoveController].
type ControllerOption func(*MoveController)

// WithFencer sets the L3 storage [Fencer]. nil ⇒ fencing is a no-op.
func WithFencer(f Fencer) ControllerOption {
	return func(c *MoveController) { c.fencer = f }
}

// WithEventBarrier sets the L4 [EventBarrier]. nil ⇒ event phases are no-ops.
func WithEventBarrier(b EventBarrier) ControllerOption {
	return func(c *MoveController) { c.events = b }
}

// WithDrainer sets the L2 [Drainer]. nil ⇒ no local drain coordination.
func WithDrainer(d Drainer) ControllerOption {
	return func(c *MoveController) { c.drainer = d }
}

// WithClock injects the controller's clock (default [time.Now]). Used for move
// deadlines and budget accounting; tests pass a controllable clock.
func WithClock(now func() time.Time) ControllerOption {
	return func(c *MoveController) {
		if now != nil {
			c.now = now
		}
	}
}

// WithIDGenerator injects the barrier-ID generator (default: a per-controller
// monotonic counter). Tests pass a deterministic generator.
func WithIDGenerator(gen func() string) ControllerOption {
	return func(c *MoveController) {
		if gen != nil {
			c.newID = gen
		}
	}
}

// WithPreflight installs a hook run at Phase 0 of every move; a non-nil error
// aborts the move before any state changes (default: always allow).
func WithPreflight(fn func(ctx context.Context, plan MovePlan) error) ControllerOption {
	return func(c *MoveController) {
		if fn != nil {
			c.preflight = fn
		}
	}
}

// WithDrainDeadline sets how long a move may spend draining/quiescing before the
// controller forces a rollback (default 30s). It is liveness, never safety.
func WithDrainDeadline(d time.Duration) ControllerOption {
	return func(c *MoveController) {
		if d > 0 {
			c.drainDeadline = d
		}
	}
}

// WithBudgetMeter attaches a [BudgetMeter]. When set, a non-forced move whose
// estimated cost would breach the tenant's remaining monthly budget is refused
// with [ErrBudgetExceeded], and a completed move's cost is recorded.
func WithBudgetMeter(m *BudgetMeter) ControllerOption {
	return func(c *MoveController) { c.budget = m }
}

const defaultDrainDeadline = 30 * time.Second

// MoveController drives the move protocol that relocates a tenant from one cell to
// another in depth: a forward-only route epoch advances N→N+2 across a quiesce →
// drain → fence → event-pause → commit sequence, every routing-table mutation an
// idempotent compare-and-swap (never a blind set). It is crash-safe: the routing
// table is the recovery state, so [MoveController.Resume] re-derives the phase and
// finishes forward (or rolls back past the deadline).
//
// A nil Fencer/EventBarrier/Drainer turns the corresponding phase into a no-op,
// so the same controller serves compute-only (shared-DB) cells and data-owning
// cells alike.
type MoveController struct {
	table   RoutingTable
	fencer  Fencer
	events  EventBarrier
	drainer Drainer

	now           func() time.Time
	newID         func() string
	preflight     func(ctx context.Context, plan MovePlan) error
	drainDeadline time.Duration
	budget        *BudgetMeter

	idSeq uint64 // backs the default monotonic barrier-ID generator
}

// NewMoveController builds a controller over table. Without options it is
// compute-only (no fencer/events/drainer), uses [time.Now], a monotonic barrier-ID
// counter, an allow-all preflight, and a 30s drain deadline.
func NewMoveController(table RoutingTable, opts ...ControllerOption) *MoveController {
	c := &MoveController{
		table:         table,
		now:           time.Now,
		preflight:     func(context.Context, MovePlan) error { return nil },
		drainDeadline: defaultDrainDeadline,
	}
	c.newID = func() string {
		n := atomic.AddUint64(&c.idSeq, 1)
		return "barrier-" + strconv.FormatUint(n, 10)
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// estimateCost is the unavailability a move is assumed to cost for the budget
// gate: the worst-case reject window, bounded by the drain deadline.
func (c *MoveController) estimateCost() time.Duration { return c.drainDeadline }

// Move drives the full move protocol for one tenant from its current cell to
// plan.ToCell, advancing the route epoch N→N+2. It is idempotent: a Move whose
// effect already landed (e.g. a retry) converges without regressing the epoch or
// double-advancing. On any error before the Phase-7 commit CAS, the tenant is
// rolled back to its source cell at a fresh, higher epoch.
func (c *MoveController) Move(ctx context.Context, plan MovePlan) error {
	if plan.FromCell == plan.ToCell {
		return ErrSameCell
	}

	// Phase 0 — Preflight + budget gate.
	if err := c.preflight(ctx, plan); err != nil {
		return fmt.Errorf("cells: preflight: %w", err)
	}
	if c.budget != nil && !plan.Force {
		if !c.budget.Allowed(plan.TenantID, c.estimateCost()) {
			return fmt.Errorf("%w: tenant %q has %s remaining", ErrBudgetExceeded, plan.TenantID, c.budget.Remaining(plan.TenantID))
		}
	}

	cur, err := c.table.Get(ctx, plan.TenantID)
	if errors.Is(err, ErrNoRoute) {
		return ErrNoRouteToMove
	}
	if err != nil {
		return err
	}
	// Already resting on the target — nothing to do (idempotent).
	if cur.State.AdmitsNew() && cur.ActiveCell == plan.ToCell {
		return nil
	}

	start := c.now()
	if err := c.drive(ctx, plan, cur); err != nil {
		// Roll back to the source cell at a fresh, higher epoch (forward-only).
		if rbErr := c.Rollback(ctx, plan.TenantID); rbErr != nil {
			return errors.Join(err, fmt.Errorf("cells: rollback: %w", rbErr))
		}
		return err
	}
	if c.budget != nil {
		c.budget.Record(plan.TenantID, c.now().Sub(start))
	}
	return nil
}

// drive executes Phases 1–7. cur is the last-observed route (ACTIVE at epoch N).
// Each CAS expects the last-observed route; an ErrCASConflict re-reads and the
// phase is re-evaluated so a concurrent driver / retry converges idempotently.
func (c *MoveController) drive(ctx context.Context, plan MovePlan, cur TenantRoute) error {
	if !cur.State.AdmitsNew() {
		// A move is already mid-flight (crash recovery / concurrent driver): defer
		// to Resume so we continue forward from the persisted phase rather than
		// restarting the barrier.
		return c.resumeFrom(ctx, cur)
	}

	barrierEpoch := cur.RouteEpoch + 1
	barrierID := c.newID()
	deadline := c.now().Add(c.drainDeadline)

	// Phase 1 — Begin barrier: ACTIVE(N) → QUIESCING.
	begun, err := c.cas(ctx, cur, func(r TenantRoute) TenantRoute {
		r.SourceCell = plan.FromCell
		r.TargetCell = plan.ToCell
		r.State = StateQuiescing
		r.BarrierEpoch = barrierEpoch
		r.BarrierID = barrierID
		r.Deadline = deadline
		r.LastOperator = plan.Operator
		return r
	}, StateQuiescing)
	if err != nil {
		return err
	}
	return c.continueMove(ctx, begun)
}

// continueMove drives Phases 2–7 from a route already in (or past) QUIESCING. It
// is the shared body of an initial Move and a Resume, so both converge through
// the same forward-only CAS chain.
func (c *MoveController) continueMove(ctx context.Context, r TenantRoute) error {
	barrierEpoch := r.BarrierEpoch
	commitEpoch := barrierEpoch + 1
	target := r.TargetCell

	// Bound the remaining work by the move deadline so a stuck drain/event plane
	// rolls back rather than hangs (liveness only).
	dctx := ctx
	if !r.Deadline.IsZero() {
		var cancel context.CancelFunc
		dctx, cancel = context.WithDeadline(ctx, r.Deadline)
		defer cancel()
	}

	// Phase 2 — Close source + drain, then QUIESCING → DRAINING.
	if r.State == StateQuiescing {
		if c.drainer != nil {
			if _, err := c.drainer.CloseAndDrain(dctx, r.TenantID, barrierEpoch); err != nil {
				return fmt.Errorf("%w: drain: %v", ErrMoveDeadline, err)
			}
		}
		var err error
		if r, err = c.cas(ctx, r, func(x TenantRoute) TenantRoute {
			x.State = StateDraining
			return x
		}, StateDraining); err != nil {
			return err
		}
	}

	// Phase 3 — Storage fence (seal all writers for the barrier).
	if r.State == StateDraining {
		if c.fencer != nil {
			if err := c.fencer.Seal(ctx, r.TenantID, barrierEpoch); err != nil {
				return fmt.Errorf("cells: seal: %w", err)
			}
		}
		// Phase 5 — Event barrier: pause the publisher and wait until drained.
		if c.events != nil {
			if err := c.events.SetPolicy(ctx, r.TenantID, PolicyPause, barrierEpoch); err != nil {
				return fmt.Errorf("cells: event pause: %w", err)
			}
			if err := c.waitEventsDrained(dctx, r.TenantID, barrierEpoch); err != nil {
				return err
			}
		}
		var err error
		if r, err = c.cas(ctx, r, func(x TenantRoute) TenantRoute {
			x.State = StateCopying
			x.EventPolicy = PolicyPause
			x.EventEpoch = barrierEpoch
			return x
		}, StateCopying); err != nil {
			return err
		}
	}

	// Phase 6 — Data catch-up. Compute-only ⇒ no-op; COPYING → COMMITTING.
	if r.State == StateCopying {
		var err error
		if r, err = c.cas(ctx, r, func(x TenantRoute) TenantRoute {
			x.State = StateCommitting
			return x
		}, StateCommitting); err != nil {
			return err
		}
	}

	// Phase 7 — Commit the cut: hand ownership to the target, resume NORMAL events,
	// flip the route to ACTIVE on the target at the post-move epoch.
	if r.State == StateCommitting {
		if c.fencer != nil {
			if err := c.fencer.SetOwner(ctx, r.TenantID, target, commitEpoch); err != nil {
				return fmt.Errorf("cells: commit owner: %w", err)
			}
		}
		if c.events != nil {
			if err := c.events.SetPolicy(ctx, r.TenantID, PolicyNormal, commitEpoch); err != nil {
				return fmt.Errorf("cells: event resume: %w", err)
			}
		}
		if _, err := c.cas(ctx, r, func(x TenantRoute) TenantRoute {
			x.ActiveCell = target
			x.SourceCell = ""
			x.TargetCell = ""
			x.State = StateActive
			x.RouteEpoch = commitEpoch
			x.BarrierEpoch = 0
			x.BarrierID = ""
			x.EventPolicy = PolicyNormal
			x.EventEpoch = commitEpoch
			x.Deadline = time.Time{}
			return x
		}, StateActive); err != nil {
			return err
		}
	}
	return nil
}

// waitEventsDrained polls the event barrier until the source publisher has
// flushed/paused everything up to barrierEpoch, or ctx (the deadline-bounded
// context) is done.
func (c *MoveController) waitEventsDrained(ctx context.Context, tenantID string, barrierEpoch uint64) error {
	for {
		ok, err := c.events.Drained(ctx, tenantID, barrierEpoch)
		if err != nil {
			return fmt.Errorf("cells: event drain: %w", err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: event drain: %v", ErrMoveDeadline, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// cas applies mutate to a copy of expect and CompareAndSets it. On ErrCASConflict
// it re-Gets and, if the stored route already satisfies the phase (its State is
// wantState — i.e. the step already landed), returns it as success (idempotent);
// otherwise it returns the conflict so the caller can re-evaluate from the new
// state. It returns the route now stored (the next phase's expect).
func (c *MoveController) cas(ctx context.Context, expect TenantRoute, mutate func(TenantRoute) TenantRoute, wantState State) (TenantRoute, error) {
	next := mutate(expect)
	err := c.table.CompareAndSet(ctx, expect, next)
	switch {
	case err == nil:
		return next, nil
	case errors.Is(err, ErrCASConflict):
		// Someone moved the route under us. Re-read; if the step already landed,
		// adopt it (idempotent), else surface the new state to the caller.
		cur, gErr := c.table.Get(ctx, expect.TenantID)
		if gErr != nil {
			return TenantRoute{}, gErr
		}
		if cur.State == wantState && cur.RouteEpoch >= expect.RouteEpoch {
			return cur, nil
		}
		return cur, ErrCASConflict
	default:
		return TenantRoute{}, err
	}
}

// Rollback abandons an in-progress move and returns the tenant to its SOURCE cell
// at a fresh, higher epoch (barrierEpoch+1) — forward-only: the epoch never
// decreases, so a fenced stale writer stays fenced. Fencing and the event plane
// are reset to the source at the new epoch before the route flips back to ACTIVE.
// Idempotent: a tenant already resting (ACTIVE/ACTIVE_NEW) is left untouched.
func (c *MoveController) Rollback(ctx context.Context, tenantID string) error {
	for {
		cur, err := c.table.Get(ctx, tenantID)
		if errors.Is(err, ErrNoRoute) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.State.AdmitsNew() {
			return nil // nothing in flight
		}
		source := cur.SourceCell
		if source == "" {
			source = cur.ActiveCell
		}
		newEpoch := cur.BarrierEpoch + 1
		if newEpoch <= cur.RouteEpoch {
			newEpoch = cur.RouteEpoch + 1
		}

		if c.fencer != nil {
			if err := c.fencer.SetOwner(ctx, tenantID, source, newEpoch); err != nil {
				return fmt.Errorf("cells: rollback owner: %w", err)
			}
		}
		if c.events != nil {
			if err := c.events.SetPolicy(ctx, tenantID, PolicyNormal, newEpoch); err != nil {
				return fmt.Errorf("cells: rollback events: %w", err)
			}
		}

		next := cur
		next.ActiveCell = source
		next.SourceCell = ""
		next.TargetCell = ""
		next.State = StateActive
		next.RouteEpoch = newEpoch
		next.BarrierEpoch = 0
		next.BarrierID = ""
		next.EventPolicy = PolicyNormal
		next.EventEpoch = newEpoch
		next.Deadline = time.Time{}

		err = c.table.CompareAndSet(ctx, cur, next)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrCASConflict) {
			continue // re-read and retry
		}
		return err
	}
}

// Resume is crash recovery: the routing table is the recovery state, so Resume
// re-reads the route and drives it forward to a consistent ACTIVE — finishing a
// commit, continuing a quiesce/drain/copy, or rolling back if the move deadline
// has elapsed. A resting route (ACTIVE/ACTIVE_NEW/ABORTED) is a no-op. Idempotent
// and safe to call repeatedly.
func (c *MoveController) Resume(ctx context.Context, tenantID string) error {
	cur, err := c.table.Get(ctx, tenantID)
	if errors.Is(err, ErrNoRoute) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.resumeFrom(ctx, cur)
}

// resumeFrom drives the route cur forward from whatever phase it is in. A route
// past its deadline (and not yet committing) is rolled back; otherwise it
// continues forward. COMMITTING always finishes forward (the cut is decided).
func (c *MoveController) resumeFrom(ctx context.Context, cur TenantRoute) error {
	if cur.State.AdmitsNew() {
		return nil
	}
	// Past the deadline and the cut is not yet decided ⇒ roll back. COMMITTING is
	// past the point of no return: finish it forward.
	if cur.State != StateCommitting && !cur.Deadline.IsZero() && c.now().After(cur.Deadline) {
		return c.Rollback(ctx, cur.TenantID)
	}
	if err := c.continueMove(ctx, cur); err != nil {
		if rbErr := c.Rollback(ctx, cur.TenantID); rbErr != nil {
			return errors.Join(err, fmt.Errorf("cells: rollback: %w", rbErr))
		}
		return err
	}
	return nil
}

// Assign places tenantID on cell (sticky first placement). With no route it
// creates ACTIVE at epoch 1; if the tenant already rests on cell it is a no-op;
// otherwise it moves the tenant from its current cell to cell.
func (c *MoveController) Assign(ctx context.Context, tenantID, cell, operator string) error {
	cur, err := c.table.Get(ctx, tenantID)
	switch {
	case errors.Is(err, ErrNoRoute):
		next := TenantRoute{
			TenantID:     tenantID,
			RouteEpoch:   1,
			ActiveCell:   cell,
			State:        StateActive,
			EventPolicy:  PolicyNormal,
			EventEpoch:   1,
			LastOperator: operator,
		}
		if c.fencer != nil {
			if err := c.fencer.SetOwner(ctx, tenantID, cell, 1); err != nil {
				return fmt.Errorf("cells: assign owner: %w", err)
			}
		}
		err := c.table.CompareAndSet(ctx, TenantRoute{}, next)
		if errors.Is(err, ErrCASConflict) {
			// Lost the create race — re-evaluate as an existing route.
			return c.Assign(ctx, tenantID, cell, operator)
		}
		return err
	case err != nil:
		return err
	}

	if cur.State.AdmitsNew() && cur.ActiveCell == cell {
		return nil // already placed
	}
	return c.Move(ctx, MovePlan{
		TenantID: tenantID,
		FromCell: cur.ActiveCell,
		ToCell:   cell,
		Operator: operator,
	})
}

// CohortMoveError aggregates the per-member outcome of a [MoveController.MoveCohort].
type CohortMoveError struct {
	CohortID   string
	FailedAt   string           // the tenant whose move failed
	Cause      error            // the underlying failure
	RolledBack []string         // members rolled back as a result (best-effort)
	Errors     map[string]error // per-tenant errors encountered during rollback
}

// Error implements error.
func (e *CohortMoveError) Error() string {
	return fmt.Sprintf("cells: cohort %q move failed at tenant %q: %v (rolled back %d member(s))",
		e.CohortID, e.FailedAt, e.Cause, len(e.RolledBack))
}

// Unwrap exposes the underlying cause.
func (e *CohortMoveError) Unwrap() error { return e.Cause }

// MoveCohort moves a set of tenants under one logical cohort barrier with
// all-or-nothing intent: members are moved in order, and if any member fails the
// already-committed members are reversed (moved back to their origin cell,
// best-effort) so the cohort never lands half-placed. v1 sequences the members.
//
// Reversing a committed member is itself a forward move (back to its source
// cell), so the route epoch only ever advances — undoing a cohort never regresses
// an epoch.
func (c *MoveController) MoveCohort(ctx context.Context, cohortID string, members []MovePlan) error {
	committed := make([]MovePlan, 0, len(members))
	for _, m := range members {
		m.CohortID = cohortID
		if err := c.Move(ctx, m); err != nil {
			cohortErr := &CohortMoveError{
				CohortID: cohortID,
				FailedAt: m.TenantID,
				Cause:    err,
				Errors:   make(map[string]error),
			}
			// Move already rolled back the failed member in place; reverse the ones
			// that committed by moving them back to where they started. Force so the
			// reversal is never itself blocked by the budget gate.
			for _, done := range committed {
				reverse := MovePlan{
					TenantID: done.TenantID,
					FromCell: done.ToCell,
					ToCell:   done.FromCell,
					Operator: done.Operator,
					CohortID: cohortID,
					Force:    true,
				}
				if rbErr := c.Move(ctx, reverse); rbErr != nil {
					cohortErr.Errors[done.TenantID] = rbErr
				} else {
					cohortErr.RolledBack = append(cohortErr.RolledBack, done.TenantID)
				}
			}
			return cohortErr
		}
		committed = append(committed, m)
	}
	return nil
}
