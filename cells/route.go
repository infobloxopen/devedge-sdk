package cells

import (
	"context"
	"errors"
	"time"
)

// DefaultCellID is the fail-safe cell that serves any tenant lacking an explicit
// route (and any tenant whose route cannot be resolved). It matches
// middleware.DefaultCellID so a service that adopts cell routing with no routes
// populated behaves exactly as before: every tenant lands on "default".
const DefaultCellID = "default"

// State is the lifecycle of a tenant's route. The resting states (ACTIVE,
// ACTIVE_NEW) admit new work; the transitional move states reject new work at
// L1 while the source cell drains. ABORTED is a move that was abandoned — the
// tenant stays on its current active cell. StateUnknown is the zero value and
// doubles as the "expect absent" precondition in [RoutingTable.CompareAndSet].
type State uint8

const (
	StateUnknown    State = iota // zero value; also "expect absent" in a CAS
	StateActive                  // serving normally on ActiveCell at RouteEpoch
	StateQuiescing               // move begun; gateways observing the new epoch reject new calls
	StateDraining                // source gates closed; waiting for in-flight to finish
	StateCopying                 // data/event catch-up (data-owning cells; P4)
	StateCommitting              // about to flip the route to the target cell
	StateActiveNew               // serving on the new cell at the post-move epoch
	StateAborted                 // move abandoned; tenant remains on ActiveCell
)

// AdmitsNew reports whether a tenant in this state accepts new calls. Only the
// resting states do; every transitional move state rejects new calls so the
// source cell can drain to a clean cut.
func (s State) AdmitsNew() bool {
	switch s {
	case StateActive, StateActiveNew, StateAborted, StateUnknown:
		return true
	default:
		return false
	}
}

// IsMoving reports whether this state is a transitional move state (new calls
// rejected at L1).
func (s State) IsMoving() bool {
	switch s {
	case StateQuiescing, StateDraining, StateCopying, StateCommitting:
		return true
	default:
		return false
	}
}

// String returns the canonical name of the state.
func (s State) String() string {
	switch s {
	case StateActive:
		return "ACTIVE"
	case StateQuiescing:
		return "QUIESCING"
	case StateDraining:
		return "DRAINING"
	case StateCopying:
		return "COPYING"
	case StateCommitting:
		return "COMMITTING"
	case StateActiveNew:
		return "ACTIVE_NEW"
	case StateAborted:
		return "ABORTED"
	default:
		return "UNKNOWN"
	}
}

// TenantRoute is the per-tenant record in the routing table — the single source
// of truth for where a tenant is served and whether it is moving. Safety derives
// from RouteEpoch (monotonic, never decreases), not from clocks.
//
// Event/data-plane fields (event_epoch, high-watermarks) from the full proposal
// are intentionally absent from this routing-plane slice (L4/P4) and will be
// added without breaking this contract.
type TenantRoute struct {
	TenantID   string
	RouteEpoch uint64 // monotonic per tenant; never decreases, even on rollback
	ActiveCell string // the cell currently serving the tenant
	SourceCell string // during a move: the cell being drained
	TargetCell string // during a move: the cell being moved to
	State      State

	BarrierID    string // identifies the in-progress move barrier
	BarrierEpoch uint64 // the epoch new work is rejected at while draining

	Deadline     time.Time // liveness deadline for the current move phase (never a safety mechanism)
	LastOperator string    // audit: who drove the last transition
}

// IsZero reports whether r is the zero TenantRoute (no tenant set). Used to mean
// "absent" in CAS preconditions.
func (r TenantRoute) IsZero() bool {
	return r.TenantID == "" && r.RouteEpoch == 0 && r.State == StateUnknown
}

// RouteEvent is a single change observed on a [RoutingTable.Watch] stream.
type RouteEvent struct {
	Route    TenantRoute // the new state of the tenant's route (zero TenantID if Deleted)
	TenantID string      // the affected tenant (always set, even when Deleted)
	Deleted  bool        // the route was removed; the tenant reverts to the default cell
	Revision uint64      // table-global monotonic revision at which this change applied
}

// Routing-table errors.
var (
	// ErrNoRoute means the tenant has no explicit route; the caller falls back to
	// the default cell.
	ErrNoRoute = errors.New("cells: no route for tenant")
	// ErrCASConflict means the CompareAndSet precondition (expected epoch+state)
	// did not match the stored route.
	ErrCASConflict = errors.New("cells: compare-and-set conflict")
	// ErrEpochRegression means a CompareAndSet attempted to lower a tenant's
	// RouteEpoch — forbidden (invariant 7: epochs never decrease).
	ErrEpochRegression = errors.New("cells: route epoch must not decrease")
)

// RoutingTable is the tenant→cell directory: the single source of truth for
// route state. It is consumed read-mostly by routers/cells via Watch; mutations
// go only through CompareAndSet (every transition an idempotent compare-and-swap,
// never a blind set). The in-memory [MemTable] is the dev/test backend; a
// Raft/etcd-backed adapter implements the same interface in a later module.
type RoutingTable interface {
	// Get returns the current route for tenantID, or ErrNoRoute if the tenant has
	// no explicit assignment.
	Get(ctx context.Context, tenantID string) (TenantRoute, error)

	// CompareAndSet atomically stores next for next.TenantID, but only if the
	// currently stored route matches expect on (RouteEpoch, State). To create the
	// first route for a tenant, pass the zero TenantRoute as expect (StateUnknown,
	// epoch 0) and the tenant must currently be absent. It returns ErrCASConflict
	// when the precondition fails and ErrEpochRegression when next.RouteEpoch is
	// below the stored epoch.
	CompareAndSet(ctx context.Context, expect, next TenantRoute) error

	// Watch returns a channel of route changes. The channel is closed when ctx is
	// cancelled. Implementations may coalesce rapid updates but must always
	// deliver the latest state for any changed tenant.
	Watch(ctx context.Context) (<-chan RouteEvent, error)
}
