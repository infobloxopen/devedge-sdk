package persistence

import (
	"context"
	"sync"
)

// MemoryRepository is an in-memory, concurrency-safe [Repository] for
// development and tests. Not for production: nothing is persisted, and
// ListOptions filter/order/paging are ignored.
type MemoryRepository[T any, K comparable] struct {
	mu    sync.RWMutex
	items map[K]T
	keyFn func(T) K
}

// NewMemoryRepository returns an in-memory repository. keyFn extracts the key
// from an entity (used by Create to detect conflicts).
func NewMemoryRepository[T any, K comparable](keyFn func(T) K) *MemoryRepository[T, K] {
	return &MemoryRepository[T, K]{items: map[K]T{}, keyFn: keyFn}
}

// Get implements [Repository].
func (r *MemoryRepository[T, K]) Get(_ context.Context, key K) (T, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[key]
	if !ok {
		var zero T
		return zero, ErrNotFound
	}
	return v, nil
}

// List implements [Repository].
func (r *MemoryRepository[T, K]) List(_ context.Context, _ ListOptions) ([]T, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]T, 0, len(r.items))
	for _, v := range r.items {
		out = append(out, v)
	}
	return out, "", nil
}

// Create implements [Repository].
func (r *MemoryRepository[T, K]) Create(_ context.Context, entity T) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.keyFn(entity)
	if _, ok := r.items[key]; ok {
		var zero T
		return zero, ErrConflict
	}
	r.items[key] = entity
	return entity, nil
}

// Update implements [Repository]. The fieldMask is accepted for interface
// compatibility but ignored by the in-memory store (it replaces the entity).
func (r *MemoryRepository[T, K]) Update(_ context.Context, key K, entity T, _ ...string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[key]; !ok {
		var zero T
		return zero, ErrNotFound
	}
	r.items[key] = entity
	return entity, nil
}

// Delete implements [Repository].
func (r *MemoryRepository[T, K]) Delete(_ context.Context, key K) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[key]; !ok {
		return ErrNotFound
	}
	delete(r.items, key)
	return nil
}
