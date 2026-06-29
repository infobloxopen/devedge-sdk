package rules

import (
	"context"
	"fmt"
	"sync"
)

// Cache is the consumer-side fail-safe snapshot over any [Source]. It does the
// initial load (via [Snapshotter] when the source supports it), then subscribes
// to [Source.Watch] and keeps a last-known-good copy of every tenant's ruleset
// in memory. Consumers read from that copy, so evaluation is local and a source
// outage degrades to stale-but-available data rather than a failed request —
// the same decoupling the change feed gets from the outbox.
//
// A Cache is NOT ready until its initial load succeeds (or, for a source that
// is not a Snapshotter, until the Watch subscription is established). It
// implements [health.Check] so readiness wires into the server's /readyz and
// gRPC health endpoints: a service does not report ready until its rules are
// loaded.
//
// The zero value is not usable; construct with [NewCache].
type Cache[T any] struct {
	name string
	src  Source[T]

	mu    sync.RWMutex
	data  map[string]T
	ready bool
	rev   uint64 // highest source revision applied; guards against stale events
}

// NewCache returns a cache over src. name identifies it in readiness output
// (e.g. "feature-flags"). Call [Cache.Run] once to start syncing.
func NewCache[T any](name string, src Source[T]) *Cache[T] {
	return &Cache[T]{
		name: name,
		src:  src,
		data: make(map[string]T),
	}
}

// Run subscribes to the source, loads the initial snapshot, then applies Watch
// events until ctx is cancelled. It blocks; run it in a goroutine. Run returns
// the context error on cancellation, or a non-nil error if the initial Watch
// subscription fails.
//
// Watch is opened BEFORE the snapshot so no change is missed in the gap; events
// that predate the snapshot are buffered and skipped by revision, so the cache
// converges correctly. A failed initial Snapshot is non-fatal: the cache stays
// not-ready and becomes ready on the first event that arrives, so a rules
// source that is down at startup degrades to "not ready yet" rather than
// crashing the consumer.
func (c *Cache[T]) Run(ctx context.Context) error {
	ch, err := c.src.Watch(ctx)
	if err != nil {
		return fmt.Errorf("rules: cache %q: watch: %w", c.name, err)
	}

	// Initial bulk load when the source supports it, so the cache is ready with
	// complete data. A snapshot failure is tolerated (see above). An empty but
	// successful snapshot is a valid ready state — no configured rules means
	// every lookup falls back to the consumer's default.
	if snap, ok := c.src.(Snapshotter[T]); ok {
		if data, rev, err := snap.Snapshot(ctx); err == nil {
			c.mu.Lock()
			c.data = data
			c.rev = rev
			c.ready = true
			c.mu.Unlock()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, open := <-ch:
			if !open {
				return ctx.Err()
			}
			c.apply(ev)
		}
	}
}

// apply folds one event into the cached state, ignoring events at or below the
// highest revision already applied (stale buffered events from before the
// initial snapshot). Applying any event marks the cache ready — it confirms a
// live view of the source even if the initial snapshot failed.
func (c *Cache[T]) apply(ev Event[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = true
	if ev.Revision != 0 && ev.Revision <= c.rev {
		return
	}
	if ev.Revision > c.rev {
		c.rev = ev.Revision
	}
	if ev.Deleted {
		delete(c.data, ev.Tenant)
		return
	}
	c.data[ev.Tenant] = ev.Value
}

// Get returns the cached ruleset for tenant and true, or the zero value and
// false when the tenant has no ruleset (the caller falls back to a default). It
// reads from memory and never blocks on the source.
func (c *Cache[T]) Get(tenant string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[tenant]
	return v, ok
}

// Ready reports whether the initial load has completed. Before the first
// successful load it is false; consumers may still call Get (it returns
// not-found and they use defaults), but readiness gates traffic.
func (c *Cache[T]) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Name implements health.Check.
func (c *Cache[T]) Name() string { return c.name }

// Check implements health.Check: nil once the cache has loaded, otherwise an
// error so /readyz reports the service as not ready.
func (c *Cache[T]) Check(_ context.Context) error {
	if !c.Ready() {
		return fmt.Errorf("rules: cache %q not yet loaded", c.name)
	}
	return nil
}
