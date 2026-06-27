package persistence

import (
	"context"
	"encoding/base64"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
)

// memRepoSeq assigns each MemoryRepository a process-unique, monotonically
// increasing id at construction. The id is the stable lock-ordering key used by
// MemoryTxRunner to acquire participant locks in a consistent order (deadlock
// avoidance). Unlike a pointer-derived key it is GC-move-safe and deterministic.
var memRepoSeq atomic.Uint64

// SetIfMatchExpectation injects an expected ETag into ctx for testing the
// precondition check in Update.
func SetIfMatchExpectation(ctx context.Context, expectedETag string) context.Context {
	return etag.SetIfMatch(ctx, expectedETag)
}

// MemoryRepository is an in-memory, concurrency-safe [Repository] for
// development and tests. Not for production: nothing is persisted. List
// supports cursor-based pagination; filter/order are ignored.
//
// Soft-delete is uniform: Delete marks an entity deleted without removing it;
// Undelete clears the mark. ShowDeleted in ListOptions includes deleted entities.
type MemoryRepository[T any, K comparable] struct {
	mu      sync.RWMutex
	items   map[K]T
	etags   map[K]string
	keys    []K
	deleted map[K]bool
	keyFn   func(T) K
	id      uint64 // stable lock-ordering key (see memRepoSeq)
}

// NewMemoryRepository returns an in-memory repository. keyFn extracts the key
// from an entity (used by Create to detect conflicts).
func NewMemoryRepository[T any, K comparable](keyFn func(T) K) *MemoryRepository[T, K] {
	return &MemoryRepository[T, K]{
		items:   map[K]T{},
		etags:   map[K]string{},
		keys:    []K{},
		deleted: map[K]bool{},
		keyFn:   keyFn,
		id:      memRepoSeq.Add(1),
	}
}

// Get implements [Repository]. If an ETag is stored for the key it is written
// into ctx via [etag.SetNewETag] so callers (and interceptors) can read it.
// Returns ErrNotFound for soft-deleted entities.
func (r *MemoryRepository[T, K]) Get(ctx context.Context, key K) (T, error) {
	if !r.inThisTx(ctx) {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}
	v, ok := r.items[key]
	if !ok || r.deleted[key] {
		var zero T
		return zero, ErrNotFound
	}
	if stored := r.etags[key]; stored != "" {
		etag.SetNewETag(ctx, stored)
	}
	return v, nil
}

// GetETagForKey returns the stored ETag for a key, or empty string if not
// found. Intended for tests to read ETags directly.
func (r *MemoryRepository[T, K]) GetETagForKey(key K) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.etags[key]
}

// GetETagForKeyTx is the transaction-aware sibling of GetETagForKey: it reads the
// stored ETag while RESPECTING an active transaction this repository is enrolled
// in. Inside such a transaction the [MemoryTxRunner] already holds the write lock
// (which is non-reentrant), so taking r.mu would self-deadlock — callers that read
// the etag from inside an Atomically (e.g. an AggregateSpec.LoadEtag re-validating
// the optimistic-concurrency precondition inside the serialized critical section)
// MUST use this method, not GetETagForKey.
func (r *MemoryRepository[T, K]) GetETagForKeyTx(ctx context.Context, key K) string {
	if !r.inThisTx(ctx) {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}
	return r.etags[key]
}

// List implements [Repository] with cursor-based pagination.
// PageToken is a base64-encoded decimal offset. PageSize defaults to 50.
// Filter and OrderBy are ignored by the in-memory implementation.
// Soft-deleted entities are excluded unless opts.ShowDeleted is true.
func (r *MemoryRepository[T, K]) List(ctx context.Context, opts ListOptions) ([]T, string, error) {
	if !r.inThisTx(ctx) {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}

	// Build a slice of visible keys respecting ShowDeleted.
	visible := make([]K, 0, len(r.keys))
	for _, k := range r.keys {
		if r.deleted[k] && !opts.ShowDeleted {
			continue
		}
		visible = append(visible, k)
	}

	offset := 0
	if opts.PageToken != "" {
		if decoded, err := base64.StdEncoding.DecodeString(opts.PageToken); err == nil {
			if n, err := strconv.Atoi(string(decoded)); err == nil {
				offset = n
			}
		}
	}

	total := len(visible)
	offset = min(offset, total)

	end := min(offset+pageSize, total)

	page := visible[offset:end]
	items := make([]T, 0, len(page))
	for _, k := range page {
		items = append(items, r.items[k])
	}

	var nextPageToken string
	if offset+pageSize < total {
		nextPageToken = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(offset + pageSize)))
	}

	return items, nextPageToken, nil
}

// Create implements [Repository]. It generates and stores an ETag for the new
// entity and surfaces it via [etag.SetNewETag] (mirroring Update), so a handler
// that simply delegates to the repository — including the generated default CRUD
// handler — gets the AIP-154 ETag trailer with no extra code.
func (r *MemoryRepository[T, K]) Create(ctx context.Context, entity T) (T, error) {
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	key := r.keyFn(entity)
	if _, ok := r.items[key]; ok {
		var zero T
		return zero, ErrConflict
	}
	newETag := uuid.New().String()
	r.items[key] = entity
	r.etags[key] = newETag
	r.keys = append(r.keys, key)
	etag.SetNewETag(ctx, newETag)
	return entity, nil
}

// Update implements [Repository]. The fieldMask is accepted for interface
// compatibility but ignored (the entity is replaced in full). If an ETag
// expectation is present in ctx (via [etag.IfMatchFromContext]) and it does not
// match the stored ETag, Update returns [ErrPreconditionFailed]. On success the
// new ETag is written into ctx via [etag.SetNewETag].
func (r *MemoryRepository[T, K]) Update(ctx context.Context, key K, entity T, _ ...string) (T, error) {
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	if _, ok := r.items[key]; !ok {
		var zero T
		return zero, ErrNotFound
	}

	// ETag precondition check.
	if stored := r.etags[key]; stored != "" {
		if expected := etag.IfMatchFromContext(ctx); expected != "" && expected != stored {
			var zero T
			return zero, ErrPreconditionFailed
		}
	}

	r.items[key] = entity
	newETag := uuid.New().String()
	r.etags[key] = newETag
	etag.SetNewETag(ctx, newETag)
	return entity, nil
}

// Delete implements [Repository] with soft-delete semantics: the entity is
// marked deleted but not removed. Get returns ErrNotFound; List excludes it
// unless opts.ShowDeleted is set. Returns ErrNotFound when the key is absent
// or already soft-deleted.
func (r *MemoryRepository[T, K]) Delete(ctx context.Context, key K) error {
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	if _, ok := r.items[key]; !ok {
		return ErrNotFound
	}
	if r.deleted[key] {
		return ErrNotFound
	}
	r.deleted[key] = true
	return nil
}

// BatchGet implements [BatchRepository]. Returns items in the same order as keys.
// Returns ErrNotFound if any key does not exist or is soft-deleted. An empty
// keys slice returns an empty slice with no error.
func (r *MemoryRepository[T, K]) BatchGet(ctx context.Context, keys []K) ([]T, error) {
	if len(keys) == 0 {
		return []T{}, nil
	}
	if !r.inThisTx(ctx) {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}
	items := make([]T, 0, len(keys))
	for _, k := range keys {
		if _, ok := r.items[k]; !ok || r.deleted[k] {
			return nil, ErrNotFound
		}
		items = append(items, r.items[k])
	}
	return items, nil
}

// BatchUpdate implements [BatchRepository]. Updates all items atomically: it
// pre-checks every key before mutating, so if any key is missing or soft-deleted
// it returns ErrNotFound without modifying anything. Each entity is replaced in
// full (the per-item field mask is accepted for interface compatibility but
// ignored, matching Update) and gets a fresh ETag. ETag preconditions are not
// applied to batch updates. Returns updated entities in the same order as items;
// an empty items slice returns an empty slice with no error.
func (r *MemoryRepository[T, K]) BatchUpdate(ctx context.Context, items []BatchUpdateItem[T, K]) ([]T, error) {
	if len(items) == 0 {
		return []T{}, nil
	}
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	for _, it := range items {
		if _, ok := r.items[it.Key]; !ok || r.deleted[it.Key] {
			return nil, ErrNotFound
		}
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		r.items[it.Key] = it.Entity
		r.etags[it.Key] = uuid.New().String()
		out = append(out, it.Entity)
	}
	return out, nil
}

// BatchDelete implements [BatchRepository]. Soft-deletes all keys atomically.
// Pre-checks all keys before mutating: if any key is missing or already
// soft-deleted, returns ErrNotFound without deleting anything.
func (r *MemoryRepository[T, K]) BatchDelete(ctx context.Context, keys []K) error {
	if len(keys) == 0 {
		return nil
	}
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	for _, k := range keys {
		if _, ok := r.items[k]; !ok || r.deleted[k] {
			return ErrNotFound
		}
	}
	for _, k := range keys {
		r.deleted[k] = true
	}
	return nil
}

// Undelete implements [Repository]: clears the soft-delete mark so the entity
// reappears in Get and List. Returns ErrNotFound when the key is absent or not
// currently soft-deleted.
func (r *MemoryRepository[T, K]) Undelete(ctx context.Context, key K) (T, error) {
	if !r.inThisTx(ctx) {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	if _, ok := r.items[key]; !ok {
		var zero T
		return zero, ErrNotFound
	}
	if !r.deleted[key] {
		var zero T
		return zero, ErrNotFound
	}
	delete(r.deleted, key)
	return r.items[key], nil
}
