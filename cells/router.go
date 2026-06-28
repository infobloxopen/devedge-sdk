package cells

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Decision is the outcome of resolving a tenant to a cell. It is consumed by the
// L1 middleware to route, stamp metadata, admit, or reject.
type Decision struct {
	TenantID   string
	Cell       string // the cell to route to (the default cell when Known is false)
	RouteEpoch uint64 // the epoch to admit at; 0 for default-cell tenants
	State      State
	AdmitNew   bool // the state admits new calls (false while the tenant is moving)
	IsDefault  bool // resolved to the fail-safe default cell
	Known      bool // an explicit route exists (false = default fallback)
	Stale      bool // served under uncertainty (table unreachable on a miss, or aged-out cache)
}

// Router is a read-mostly, watch-fed cache over a [RoutingTable]. It resolves a
// tenant to a [Decision], falling back to the fail-safe default cell for unknown
// tenants and when the table is unreachable (reads stay available; the Stale flag
// lets the write path fail closed). It implements [health.Check]: not ready until
// its watch has been established.
type Router struct {
	table       RoutingTable
	defaultCell string
	freshness   time.Duration // >0 marks cache entries older than this as Stale

	mu     sync.RWMutex
	cache  map[string]cacheEntry
	synced atomic.Bool
}

type cacheEntry struct {
	route   TenantRoute
	at      time.Time
	present bool // false = negative cache: tenant has no explicit route
}

// RouterOption configures a [Router].
type RouterOption func(*Router)

// WithDefaultCell overrides the fail-safe default cell (default: [DefaultCellID]).
func WithDefaultCell(cell string) RouterOption {
	return func(r *Router) {
		if cell != "" {
			r.defaultCell = cell
		}
	}
}

// WithFreshness marks a cached route older than d as Stale, so the write path
// fails closed rather than trust a route the watch may have failed to refresh.
// Zero (default) disables age-based staleness and relies on the watch stream.
func WithFreshness(d time.Duration) RouterOption {
	return func(r *Router) { r.freshness = d }
}

// NewRouter builds a Router over table. Call [Router.Start] to begin watching.
func NewRouter(table RoutingTable, opts ...RouterOption) *Router {
	r := &Router{
		table:       table,
		defaultCell: DefaultCellID,
		cache:       make(map[string]cacheEntry),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// DefaultCell returns the cell served to tenants without an explicit route.
func (r *Router) DefaultCell() string { return r.defaultCell }

// Start establishes the watch and keeps the cache converged until ctx is
// cancelled. It returns once the watch is established (the Router is then ready);
// the background loop applies events thereafter. Safe to call once.
func (r *Router) Start(ctx context.Context) error {
	ch, err := r.table.Watch(ctx)
	if err != nil {
		return err
	}
	r.synced.Store(true)
	go r.loop(ctx, ch)
	return nil
}

func (r *Router) loop(ctx context.Context, ch <-chan RouteEvent) {
	for {
		select {
		case <-ctx.Done():
			r.synced.Store(false)
			return
		case ev, ok := <-ch:
			if !ok {
				r.synced.Store(false)
				return
			}
			r.apply(ev)
		}
	}
}

func (r *Router) apply(ev RouteEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Deleted {
		// Route removed → negative-cache so the tenant resolves to the default cell.
		r.cache[ev.TenantID] = cacheEntry{at: time.Now(), present: false}
		return
	}
	r.cache[ev.Route.TenantID] = cacheEntry{route: ev.Route, at: time.Now(), present: true}
}

// Resolve returns the routing Decision for tenantID. A cache hit is served
// locally; a miss lazily reads the table (negative-caching unknown tenants). When
// the table is unreachable on a miss, it returns a Stale default-cell decision so
// reads stay available while writes can fail closed.
func (r *Router) Resolve(ctx context.Context, tenantID string) Decision {
	if tenantID == "" {
		// No tenant scope (e.g. an unauthenticated/global method); treat as default.
		return r.defaultDecision("", false)
	}

	r.mu.RLock()
	e, ok := r.cache[tenantID]
	r.mu.RUnlock()
	if ok {
		if !e.present {
			return r.defaultDecision(tenantID, false)
		}
		return r.decisionFromRoute(e.route, r.aged(e.at))
	}

	// Cache miss — lazily read the table.
	route, err := r.table.Get(ctx, tenantID)
	switch {
	case errors.Is(err, ErrNoRoute):
		r.cacheNegative(tenantID)
		return r.defaultDecision(tenantID, false)
	case err != nil:
		// Table unreachable and tenant unknown: fail safe to default for reads,
		// flag Stale so a mutating call fails closed at L1.
		return r.defaultDecision(tenantID, true)
	default:
		r.cachePositive(route)
		return r.decisionFromRoute(route, false)
	}
}

func (r *Router) aged(at time.Time) bool {
	return r.freshness > 0 && time.Since(at) > r.freshness
}

func (r *Router) cachePositive(route TenantRoute) {
	r.mu.Lock()
	r.cache[route.TenantID] = cacheEntry{route: route, at: time.Now(), present: true}
	r.mu.Unlock()
}

func (r *Router) cacheNegative(tenantID string) {
	r.mu.Lock()
	r.cache[tenantID] = cacheEntry{at: time.Now(), present: false}
	r.mu.Unlock()
}

func (r *Router) decisionFromRoute(route TenantRoute, stale bool) Decision {
	cell := route.ActiveCell
	if cell == "" {
		cell = r.defaultCell
	}
	return Decision{
		TenantID:   route.TenantID,
		Cell:       cell,
		RouteEpoch: route.RouteEpoch,
		State:      route.State,
		AdmitNew:   route.State.AdmitsNew(),
		IsDefault:  false,
		Known:      true,
		Stale:      stale,
	}
}

func (r *Router) defaultDecision(tenantID string, stale bool) Decision {
	return Decision{
		TenantID:   tenantID,
		Cell:       r.defaultCell,
		RouteEpoch: 0,
		State:      StateActive,
		AdmitNew:   true,
		IsDefault:  true,
		Known:      false,
		Stale:      stale,
	}
}

// Name implements [health.Check].
func (r *Router) Name() string { return "cell-router" }

// Check implements [health.Check]: ready once the watch is established.
func (r *Router) Check(context.Context) error {
	if !r.synced.Load() {
		return errors.New("cell-router: routing-table watch not established")
	}
	return nil
}
