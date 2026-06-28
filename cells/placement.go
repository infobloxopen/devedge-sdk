package cells

import (
	"context"
	"errors"
	"sync"
)

// ErrNoCells is returned by a [PlacementPolicy] asked to place a tenant when no
// candidate cells were offered.
var ErrNoCells = errors.New("cells: no candidate cells for placement")

// PlacementPolicy decides which cell a tenant should be placed on, given the
// candidate cells. It is consulted for first placement ([MoveController.Assign])
// and to compute rebalance targets (see [Campaign]). Correctness never depends on
// the policy — a route is pinned to exactly one cell regardless of how the cell
// was chosen.
type PlacementPolicy interface {
	// Place returns the chosen cell for tenantID from cells, or [ErrNoCells] when
	// cells is empty. Implementations must be deterministic for a given input so a
	// rebalance plan is reproducible.
	Place(ctx context.Context, tenantID string, cells []string) (string, error)
}

// PlacementFunc adapts a function to a [PlacementPolicy].
type PlacementFunc func(ctx context.Context, tenantID string, cells []string) (string, error)

// Place implements [PlacementPolicy].
func (f PlacementFunc) Place(ctx context.Context, tenantID string, cells []string) (string, error) {
	return f(ctx, tenantID, cells)
}

// StickyDefaultPolicy always places a tenant on defaultCell, ignoring the
// candidate set — the v1 default: every tenant lands on one well-known cell and
// only moves on an explicit operator action.
func StickyDefaultPolicy(defaultCell string) PlacementPolicy {
	if defaultCell == "" {
		defaultCell = DefaultCellID
	}
	return PlacementFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		return defaultCell, nil
	})
}

// roundRobin hands out cells in deterministic rotation order. It is thread-safe.
type roundRobin struct {
	mu   sync.Mutex
	next int
}

// RoundRobinPolicy distributes tenants across the candidate cells in rotation,
// advancing deterministically per call. Thread-safe. With a fixed candidate set
// the assignment sequence is reproducible, so a rebalance plan built from it is
// stable.
func RoundRobinPolicy() PlacementPolicy {
	rr := &roundRobin{}
	return PlacementFunc(func(_ context.Context, _ string, cells []string) (string, error) {
		if len(cells) == 0 {
			return "", ErrNoCells
		}
		rr.mu.Lock()
		i := rr.next % len(cells)
		rr.next++
		rr.mu.Unlock()
		return cells[i], nil
	})
}

// LeastLoadedPolicy places a tenant on the candidate cell with the smallest load,
// as reported by load (e.g. tenant count). Ties break toward the earliest cell in
// the candidate slice, keeping placement deterministic for a fixed load snapshot.
func LeastLoadedPolicy(load func(cell string) int) PlacementPolicy {
	return PlacementFunc(func(_ context.Context, _ string, cells []string) (string, error) {
		if len(cells) == 0 {
			return "", ErrNoCells
		}
		best := cells[0]
		bestLoad := load(best)
		for _, c := range cells[1:] {
			if l := load(c); l < bestLoad {
				best, bestLoad = c, l
			}
		}
		return best, nil
	})
}
