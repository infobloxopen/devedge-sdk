package cells

import (
	"context"
	"errors"
	"sync"
)

// Gate-admission errors.
var (
	// ErrTenantDraining means the tenant's gate is not accepting new work — it is
	// draining for a move barrier or closed because the tenant moved away.
	ErrTenantDraining = errors.New("cells: tenant gate not accepting new work")
	// ErrStaleRouteEpoch means the request's route epoch does not match the gate's
	// current epoch — a fencing conflict (the caller raced a move).
	ErrStaleRouteEpoch = errors.New("cells: stale route epoch")
)

// gateState is the admission state of a single tenant's gate on this instance.
type gateState uint8

const (
	gateOpen     gateState = iota // accepting new admissions at routeEpoch
	gateDraining                  // barrier in progress: no new admissions; waiting for in-flight
	gateClosed                    // tenant not served by this cell (moved away / turned off)
)

// AdmissionToken is issued by [GateRegistry.TryEnter] for one admitted unit of
// tenant work. It carries the fencing identity a later storage/event layer (L3/L4)
// uses; in this routing slice it bounds the in-flight set and the barrier cutoff.
type AdmissionToken struct {
	TenantID     string
	CellID       string
	InstanceID   string
	RouteEpoch   uint64
	AdmissionSeq uint64 // strictly increasing per tenant on this instance
	BarrierID    string
}

// Cutoff is the result of closing a gate for a barrier: the max admission_seq
// admitted before the barrier on this instance, and whether the gate fully
// drained before the deadline.
type Cutoff struct {
	TenantID     string
	InstanceID   string
	BarrierEpoch uint64
	MaxSeq       uint64 // admissions with seq <= MaxSeq were let in before the barrier
	Drained      bool   // in-flight reached zero before the deadline
	Forced       bool   // the deadline fired with work still in flight (caller must fence stragglers)
}

type gate struct {
	mu           sync.Mutex
	cond         *sync.Cond
	routeEpoch   uint64
	state        gateState
	acceptingNew bool
	nextSeq      uint64
	cutoffSeq    uint64
	barrierEpoch uint64
	inflight     map[uint64]struct{}
}

func newGate(epoch uint64) *gate {
	g := &gate{
		routeEpoch:   epoch,
		state:        gateOpen,
		acceptingNew: true,
		inflight:     make(map[uint64]struct{}),
	}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// GateRegistry holds the per-tenant admission gates for one service instance in
// one cell. It is the L2 correctness barrier: every path into tenant work — edge
// handlers, internal calls, background workers, relays — must acquire admission
// through it. The L1 middleware does so automatically for gRPC; workers call
// [GateRegistry.TryEnter]/[GateRegistry.Leave] directly.
type GateRegistry struct {
	cellID     string
	instanceID string

	mu    sync.Mutex
	gates map[string]*gate
}

// NewGateRegistry returns a registry for the given cell and instance identity.
func NewGateRegistry(cellID, instanceID string) *GateRegistry {
	if cellID == "" {
		cellID = DefaultCellID
	}
	return &GateRegistry{
		cellID:     cellID,
		instanceID: instanceID,
		gates:      make(map[string]*gate),
	}
}

// CellID returns the cell this registry's instance belongs to.
func (gr *GateRegistry) CellID() string { return gr.cellID }

func (gr *GateRegistry) getOrCreate(tenantID string, epoch uint64) *gate {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	g, ok := gr.gates[tenantID]
	if !ok {
		g = newGate(epoch)
		gr.gates[tenantID] = g
	}
	return g
}

func (gr *GateRegistry) get(tenantID string) *gate {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	return gr.gates[tenantID]
}

// TryEnter admits one unit of work for tenantID at routeEpoch, returning an
// AdmissionToken to be released with [GateRegistry.Leave]. It fails with
// ErrTenantDraining when the gate is not accepting new work and ErrStaleRouteEpoch
// when routeEpoch does not match the gate's current epoch. An absent gate is
// lazily opened at routeEpoch, so a service with no routes populated admits at
// epoch 0 with no setup (backwards-compatible).
func (gr *GateRegistry) TryEnter(tenantID string, routeEpoch uint64) (AdmissionToken, error) {
	g := gr.get(tenantID)
	if g == nil {
		g = gr.getOrCreate(tenantID, routeEpoch)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != gateOpen || !g.acceptingNew {
		return AdmissionToken{}, ErrTenantDraining
	}
	if routeEpoch != g.routeEpoch {
		return AdmissionToken{}, ErrStaleRouteEpoch
	}
	g.nextSeq++
	seq := g.nextSeq
	g.inflight[seq] = struct{}{}
	return AdmissionToken{
		TenantID:     tenantID,
		CellID:       gr.cellID,
		InstanceID:   gr.instanceID,
		RouteEpoch:   routeEpoch,
		AdmissionSeq: seq,
	}, nil
}

// Leave releases an admission. When the gate is draining and the last in-flight
// admission leaves, a pending [GateRegistry.CloseForBarrier] is signalled.
func (gr *GateRegistry) Leave(tok AdmissionToken) {
	g := gr.get(tok.TenantID)
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.inflight, tok.AdmissionSeq)
	if g.state == gateDraining && len(g.inflight) == 0 {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// Inflight returns the number of in-flight admissions for tenantID (0 if no gate).
func (gr *GateRegistry) Inflight(tenantID string) int {
	g := gr.get(tenantID)
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

// Open (re)opens tenantID's gate at routeEpoch, accepting new work. The epoch is
// monotonic: a lower epoch than the gate's current epoch is ignored. Called when
// a route is observed ACTIVE on this cell (see [GateRegistry.Reconcile]) and after
// a move commits/reopens at a higher epoch.
func (gr *GateRegistry) Open(tenantID string, routeEpoch uint64) {
	g := gr.getOrCreate(tenantID, routeEpoch)
	g.mu.Lock()
	if routeEpoch >= g.routeEpoch {
		g.routeEpoch = routeEpoch
		g.state = gateOpen
		g.acceptingNew = true
	}
	g.mu.Unlock()
}

// beginDrain stops new admissions for a barrier without blocking. The blocking
// wait for in-flight to clear is [GateRegistry.CloseForBarrier].
func (gr *GateRegistry) beginDrain(tenantID string, barrierEpoch uint64) {
	g := gr.getOrCreate(tenantID, 0)
	g.mu.Lock()
	if g.state != gateClosed {
		g.state = gateDraining
		g.acceptingNew = false
		g.barrierEpoch = barrierEpoch
		g.cutoffSeq = g.nextSeq
	}
	g.mu.Unlock()
}

// Reset forgets a tenant's gate so the next admission lazily re-opens it at the
// requested epoch. Used when a tenant's route is removed (a cell turned off) and
// the tenant reverts to the default cell: there is no longer a route epoch, so
// the default cell must admit at epoch 0 even though its gate may have advanced
// to a higher epoch (Open is monotonic and would otherwise ignore epoch 0).
//
// Note: because a removed route carries no epoch, a later re-assignment must use
// a fresh, higher epoch to preserve forward-only ordering across the gap — the
// table tombstone that enforces this belongs to the move-controller phase.
func (gr *GateRegistry) Reset(tenantID string) {
	gr.mu.Lock()
	delete(gr.gates, tenantID)
	gr.mu.Unlock()
}

// closeGate marks tenantID as no longer served by this cell (moved away / turned
// off): no new admissions, and any drainers are released.
func (gr *GateRegistry) closeGate(tenantID string) {
	g := gr.getOrCreate(tenantID, 0)
	g.mu.Lock()
	g.state = gateClosed
	g.acceptingNew = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// CloseForBarrier flips tenantID's gate to draining (no new admissions), records
// the cutoff sequence, and blocks until in-flight reaches zero or ctx is done.
// On ctx deadline it returns a Cutoff marked Forced (it never waits forever — the
// caller fences stragglers at L3/L4 in a later phase). This is the barrier
// primitive the future move controller drives in Phase 2 of a move.
func (gr *GateRegistry) CloseForBarrier(ctx context.Context, tenantID string, barrierEpoch uint64) Cutoff {
	g := gr.getOrCreate(tenantID, 0)

	g.mu.Lock()
	if g.state != gateClosed {
		g.state = gateDraining
		g.acceptingNew = false
		g.barrierEpoch = barrierEpoch
		g.cutoffSeq = g.nextSeq
	}
	cut := Cutoff{
		TenantID:     tenantID,
		InstanceID:   gr.instanceID,
		BarrierEpoch: barrierEpoch,
		MaxSeq:       g.cutoffSeq,
	}

	// Wake the wait loop if ctx fires while we're blocked in cond.Wait.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			g.cond.Broadcast()
		case <-stop:
		}
	}()

	for len(g.inflight) > 0 && ctx.Err() == nil {
		g.cond.Wait()
	}
	close(stop)

	cut.Drained = len(g.inflight) == 0
	cut.Forced = !cut.Drained
	g.mu.Unlock()
	return cut
}

// Reconcile aligns the local gates with the authoritative route for one tenant
// (driven by the routing-table watch — see [GateController]). It opens the gate
// when the tenant is ACTIVE on this cell, begins draining when a move with this
// cell as source starts, and closes the gate when the tenant is served elsewhere.
func (gr *GateRegistry) Reconcile(route TenantRoute) {
	switch {
	case route.State.IsMoving() && route.SourceCell == gr.cellID:
		gr.beginDrain(route.TenantID, route.BarrierEpoch)
	case route.ActiveCell == gr.cellID && route.State.AdmitsNew():
		gr.Open(route.TenantID, route.RouteEpoch)
	case route.ActiveCell != gr.cellID && route.ActiveCell != "":
		gr.closeGate(route.TenantID)
	}
}
