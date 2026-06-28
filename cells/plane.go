package cells

import (
	"context"
	"errors"
	"sync"
)

// ErrFenceRegression is returned when a Fencer or EventBarrier is asked to move
// a tenant's epoch backwards — forbidden (invariant 7: epochs never decrease).
var ErrFenceRegression = errors.New("cells: fence epoch must not decrease")

// Fencer is L3 storage fencing as the move controller sees it: it sets the
// authoritative (owner cell, route epoch) that the data layer's write-guard
// enforces on every tenant-scoped mutation. The write-guard itself lives in the
// persistence adapter (ent mixin / gorm callback) and rejects any write whose
// admission token (cell + route epoch, from [AdmissionTokenFromContext]) does
// not match the current fence — so a stale or zombie writer from the old cell is
// rejected at the row, even if it slipped past L1/L2.
//
// A nil Fencer means compute-only with no adversarial fencing required (the route
// epoch + the L2 gate are the barrier); the controller treats fencing as a no-op.
type Fencer interface {
	// Seal blocks ALL tenant-scoped writes for tenantID for the duration of the
	// move barrier (Phase 3): no cell may mutate the tenant until SetOwner lifts
	// it. Idempotent; barrierEpoch is forward-only ([ErrFenceRegression] if it
	// regresses).
	Seal(ctx context.Context, tenantID string, barrierEpoch uint64) error
	// SetOwner installs (ownerCell, routeEpoch) as the only writer allowed and
	// lifts any seal (Phase 7 commit, or rollback). Idempotent; routeEpoch is
	// forward-only.
	SetOwner(ctx context.Context, tenantID, ownerCell string, routeEpoch uint64) error
}

// EventBarrier is the L4 event/outbox plane as the move controller sees it. The
// controller pauses or drains a tenant's publisher at the barrier (Phase 5) and
// resumes NORMAL publishing at the new epoch after the cut (Phase 7). The
// concrete implementation is backed by the transactional outbox + relay.
//
// A nil EventBarrier means the service emits no events; the controller treats the
// event phases as no-ops.
type EventBarrier interface {
	// SetPolicy applies the publisher mode for a tenant at eventEpoch:
	//   Phase 5 → PolicyPause or PolicyDrainQueue at the barrier epoch;
	//   Phase 7 → PolicyNormal at the new (post-move) epoch.
	// Idempotent; eventEpoch is forward-only.
	SetPolicy(ctx context.Context, tenantID string, policy EventPolicy, eventEpoch uint64) error
	// Drained reports whether the source publisher has flushed or paused every
	// event up to the barrier — the liveness check the controller waits on before
	// committing. A compute-only / event-free tenant is always drained.
	Drained(ctx context.Context, tenantID string, eventEpoch uint64) (bool, error)
}

// --- in-memory defaults (dev / tests; the canonical reference behavior) -------

type fenceState struct {
	ownerCell string
	epoch     uint64
	sealed    bool
}

// MemFencer is an in-memory [Fencer]: it records the authoritative owner/epoch
// per tenant and can be consulted by an in-memory write-guard via [MemFencer.Allow].
// The persistence-backed fencer enforces the identical contract against a DB row.
type MemFencer struct {
	mu sync.RWMutex
	m  map[string]fenceState
}

// NewMemFencer returns an empty in-memory fencer.
func NewMemFencer() *MemFencer { return &MemFencer{m: make(map[string]fenceState)} }

// Seal implements [Fencer].
func (f *MemFencer) Seal(_ context.Context, tenantID string, barrierEpoch uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.m[tenantID]
	if barrierEpoch < cur.epoch {
		return ErrFenceRegression
	}
	f.m[tenantID] = fenceState{ownerCell: cur.ownerCell, epoch: barrierEpoch, sealed: true}
	return nil
}

// SetOwner implements [Fencer].
func (f *MemFencer) SetOwner(_ context.Context, tenantID, ownerCell string, routeEpoch uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.m[tenantID]
	if routeEpoch < cur.epoch {
		return ErrFenceRegression
	}
	f.m[tenantID] = fenceState{ownerCell: ownerCell, epoch: routeEpoch, sealed: false}
	return nil
}

// Allow reports whether a write for tenantID from cell at routeEpoch is permitted
// under the current fence. A tenant with no fence row is allowed (fail-open only
// for never-fenced tenants — once a move seals a tenant, stale writers are
// rejected). This is what an in-memory write-guard calls; the DB write-guard runs
// the equivalent predicate in the writing transaction.
func (f *MemFencer) Allow(tenantID, cell string, routeEpoch uint64) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	cur, ok := f.m[tenantID]
	if !ok {
		return true
	}
	if cur.sealed {
		return false
	}
	return cell == cur.ownerCell && routeEpoch == cur.epoch
}

// MemEventBarrier is an in-memory [EventBarrier]: it records the current policy
// and epoch per tenant and always reports drained (an in-memory bus has no
// in-flight broker sends to flush).
type MemEventBarrier struct {
	mu sync.RWMutex
	m  map[string]struct {
		policy EventPolicy
		epoch  uint64
	}
}

// NewMemEventBarrier returns an empty in-memory event barrier.
func NewMemEventBarrier() *MemEventBarrier {
	return &MemEventBarrier{m: make(map[string]struct {
		policy EventPolicy
		epoch  uint64
	})}
}

// SetPolicy implements [EventBarrier].
func (b *MemEventBarrier) SetPolicy(_ context.Context, tenantID string, policy EventPolicy, eventEpoch uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.m[tenantID]; ok && eventEpoch < cur.epoch {
		return ErrFenceRegression
	}
	b.m[tenantID] = struct {
		policy EventPolicy
		epoch  uint64
	}{policy: policy, epoch: eventEpoch}
	return nil
}

// Drained implements [EventBarrier]; an in-memory barrier is always drained.
func (b *MemEventBarrier) Drained(_ context.Context, _ string, _ uint64) (bool, error) {
	return true, nil
}

// Policy returns the current publisher policy and epoch recorded for tenantID.
func (b *MemEventBarrier) Policy(tenantID string) (EventPolicy, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cur := b.m[tenantID]
	return cur.policy, cur.epoch
}
