package cells

import (
	"context"
	"sync"
)

// memWatchBuffer is the per-watcher channel buffer. CAS does a non-blocking send
// under the table lock; if a slow consumer fills the buffer the event is dropped
// (the Router lazily re-Gets on a cache miss, and L2/L3 re-check the epoch — the
// in-memory table is a best-effort propagation plane, not the correctness plane).
const memWatchBuffer = 64

// MemTable is the in-memory [RoutingTable] — the dev/test backend. It is
// concurrency-safe. A Raft/etcd-backed table implements the same interface for
// production (later module); correctness never depends on this fan-out being
// lossless because every cell re-checks the epoch at L2/L3.
type MemTable struct {
	mu       sync.Mutex
	routes   map[string]TenantRoute
	revision uint64
	watchers map[int]chan RouteEvent
	nextW    int
}

// NewMemTable returns an empty in-memory routing table.
func NewMemTable() *MemTable {
	return &MemTable{
		routes:   make(map[string]TenantRoute),
		watchers: make(map[int]chan RouteEvent),
	}
}

// Get implements [RoutingTable].
func (t *MemTable) Get(_ context.Context, tenantID string) (TenantRoute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.routes[tenantID]
	if !ok {
		return TenantRoute{}, ErrNoRoute
	}
	return r, nil
}

// CompareAndSet implements [RoutingTable]. The precondition is checked against
// the stored route's (RouteEpoch, State); creation requires expect to be the
// zero TenantRoute and the tenant to be absent. A lower next.RouteEpoch than the
// stored epoch is rejected with ErrEpochRegression.
func (t *MemTable) CompareAndSet(_ context.Context, expect, next TenantRoute) error {
	if next.TenantID == "" {
		return ErrCASConflict
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	cur, ok := t.routes[next.TenantID]
	if !ok {
		// Creating the first route: expect must be the zero (absent) route.
		if !expect.IsZero() {
			return ErrCASConflict
		}
	} else {
		if cur.RouteEpoch != expect.RouteEpoch || cur.State != expect.State {
			return ErrCASConflict
		}
		if next.RouteEpoch < cur.RouteEpoch {
			return ErrEpochRegression
		}
	}

	t.routes[next.TenantID] = next
	t.revision++
	t.broadcast(RouteEvent{Route: next, TenantID: next.TenantID, Revision: t.revision})
	return nil
}

// Delete removes a tenant's route (e.g. turning off a cell reverts its tenants to
// the default cell). It is idempotent and broadcasts a delete event. Not part of
// [RoutingTable] — admin/test affordance on the in-memory backend.
func (t *MemTable) Delete(_ context.Context, tenantID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.routes[tenantID]; !ok {
		return nil
	}
	delete(t.routes, tenantID)
	t.revision++
	t.broadcast(RouteEvent{TenantID: tenantID, Deleted: true, Revision: t.revision})
	return nil
}

// Watch implements [RoutingTable]. The returned channel is closed when ctx is
// cancelled.
func (t *MemTable) Watch(ctx context.Context) (<-chan RouteEvent, error) {
	t.mu.Lock()
	id := t.nextW
	t.nextW++
	ch := make(chan RouteEvent, memWatchBuffer)
	t.watchers[id] = ch
	t.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Remove under the lock so no in-flight CAS send (also under the lock) can
		// target ch after we close it, then close so consumers observe end-of-stream.
		t.mu.Lock()
		delete(t.watchers, id)
		close(ch)
		t.mu.Unlock()
	}()
	return ch, nil
}

// broadcast sends ev to every live watcher. Caller must hold t.mu. The send is
// non-blocking: a full buffer drops the event (see memWatchBuffer).
func (t *MemTable) broadcast(ev RouteEvent) {
	for _, ch := range t.watchers {
		select {
		case ch <- ev:
		default:
		}
	}
}
