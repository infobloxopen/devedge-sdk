package rules

import (
	"context"
	"maps"
	"sync"
)

// staticWatchBuffer is the per-watcher channel buffer. A broadcast does a
// non-blocking send; if a slow consumer fills the buffer the event is dropped.
// A [Cache] tolerates a dropped event because it re-snapshots on Run and the
// next change re-delivers the latest state — the source is a best-effort
// propagation plane, not the correctness plane (same contract as
// cells.MemTable).
const staticWatchBuffer = 64

// StaticSource is the in-memory [Source] — the dev/test default. It is
// concurrency-safe and supports live updates via Set, Delete, and Replace, each
// of which broadcasts to watchers so a [Cache] over it stays current.
// Production transports (FileSource, a ConfigMap bridge, OPA) implement Source
// in adapters; tests and OSS dev defaults use this.
//
// The zero value is not usable; construct with [NewStaticSource].
type StaticSource[T any] struct {
	mu       sync.Mutex
	rules    map[string]T
	revision uint64
	watchers map[int]chan Event[T]
	nextW    int
}

// NewStaticSource returns an empty in-memory source.
func NewStaticSource[T any]() *StaticSource[T] {
	return &StaticSource[T]{
		rules:    make(map[string]T),
		watchers: make(map[int]chan Event[T]),
	}
}

// Get implements [Source].
func (s *StaticSource[T]) Get(_ context.Context, tenant string) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rules[tenant]
	if !ok {
		var zero T
		return zero, ErrNotFound
	}
	return v, nil
}

// Set upserts the ruleset for tenant and broadcasts the change.
func (s *StaticSource[T]) Set(tenant string, v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[tenant] = v
	s.revision++
	s.broadcast(Event[T]{Tenant: tenant, Value: v, Revision: s.revision})
}

// Delete removes the ruleset for tenant (a no-op if absent) and broadcasts a
// delete event so consumers revert the tenant to the default ruleset.
func (s *StaticSource[T]) Delete(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[tenant]; !ok {
		return
	}
	delete(s.rules, tenant)
	s.revision++
	s.broadcast(Event[T]{Tenant: tenant, Deleted: true, Revision: s.revision})
}

// Replace atomically swaps the entire tenant→ruleset map: tenants in next are
// upserted and tenants present before but absent from next are deleted. Each
// resulting change is broadcast. Useful for a source that reloads a whole
// document (e.g. a file or ConfigMap) and reconciles to it.
func (s *StaticSource[T]) Replace(next map[string]T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tenant := range s.rules {
		if _, ok := next[tenant]; !ok {
			delete(s.rules, tenant)
			s.revision++
			s.broadcast(Event[T]{Tenant: tenant, Deleted: true, Revision: s.revision})
		}
	}
	for tenant, v := range next {
		s.rules[tenant] = v
		s.revision++
		s.broadcast(Event[T]{Tenant: tenant, Value: v, Revision: s.revision})
	}
}

// Snapshot implements [Snapshotter].
func (s *StaticSource[T]) Snapshot(_ context.Context) (map[string]T, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.rules), s.revision, nil
}

// Watch implements [Source]. The returned channel is closed when ctx is
// cancelled.
func (s *StaticSource[T]) Watch(ctx context.Context) (<-chan Event[T], error) {
	s.mu.Lock()
	id := s.nextW
	s.nextW++
	ch := make(chan Event[T], staticWatchBuffer)
	s.watchers[id] = ch
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Remove under the lock so no in-flight broadcast (also under the lock)
		// can target ch after we close it, then close so consumers observe
		// end-of-stream.
		s.mu.Lock()
		delete(s.watchers, id)
		close(ch)
		s.mu.Unlock()
	}()
	return ch, nil
}

// broadcast sends ev to every live watcher. The caller must hold s.mu. The send
// is non-blocking: a full buffer drops the event (see staticWatchBuffer).
func (s *StaticSource[T]) broadcast(ev Event[T]) {
	for _, ch := range s.watchers {
		select {
		case ch <- ev:
		default:
		}
	}
}
