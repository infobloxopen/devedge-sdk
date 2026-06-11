package persistence

import (
	"context"
	"encoding/base64"
	"strconv"
	"sync"

	"github.com/google/uuid"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
)

// SetIfMatchExpectation injects an expected ETag into ctx for testing the
// precondition check in Update.
func SetIfMatchExpectation(ctx context.Context, expectedETag string) context.Context {
	return etag.SetIfMatch(ctx, expectedETag)
}

// MemoryRepository is an in-memory, concurrency-safe [Repository] for
// development and tests. Not for production: nothing is persisted. List
// supports cursor-based pagination; filter/order are ignored.
type MemoryRepository[T any, K comparable] struct {
	mu    sync.RWMutex
	items map[K]T
	etags map[K]string
	keys  []K
	keyFn func(T) K
}

// NewMemoryRepository returns an in-memory repository. keyFn extracts the key
// from an entity (used by Create to detect conflicts).
func NewMemoryRepository[T any, K comparable](keyFn func(T) K) *MemoryRepository[T, K] {
	return &MemoryRepository[T, K]{
		items: map[K]T{},
		etags: map[K]string{},
		keys:  []K{},
		keyFn: keyFn,
	}
}

// Get implements [Repository]. If an ETag is stored for the key it is written
// into ctx via [etag.SetNewETag] so callers (and interceptors) can read it.
func (r *MemoryRepository[T, K]) Get(ctx context.Context, key K) (T, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[key]
	if !ok {
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

// List implements [Repository] with cursor-based pagination.
// PageToken is a base64-encoded decimal offset. PageSize defaults to 50.
// Filter and OrderBy are ignored by the in-memory implementation.
func (r *MemoryRepository[T, K]) List(_ context.Context, opts ListOptions) ([]T, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}

	offset := 0
	if opts.PageToken != "" {
		if decoded, err := base64.StdEncoding.DecodeString(opts.PageToken); err == nil {
			if n, err := strconv.Atoi(string(decoded)); err == nil {
				offset = n
			}
		}
	}

	total := len(r.keys)
	if offset > total {
		offset = total
	}

	end := offset + pageSize
	if end > total {
		end = total
	}

	page := r.keys[offset:end]
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
// entity.
func (r *MemoryRepository[T, K]) Create(_ context.Context, entity T) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.keyFn(entity)
	if _, ok := r.items[key]; ok {
		var zero T
		return zero, ErrConflict
	}
	r.items[key] = entity
	r.etags[key] = uuid.New().String()
	r.keys = append(r.keys, key)
	return entity, nil
}

// Update implements [Repository]. The fieldMask is accepted for interface
// compatibility but ignored (the entity is replaced in full). If an ETag
// expectation is present in ctx (via [etag.IfMatchFromContext]) and it does not
// match the stored ETag, Update returns [ErrPreconditionFailed]. On success the
// new ETag is written into ctx via [etag.SetNewETag].
func (r *MemoryRepository[T, K]) Update(ctx context.Context, key K, entity T, _ ...string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Delete implements [Repository].
func (r *MemoryRepository[T, K]) Delete(_ context.Context, key K) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[key]; !ok {
		return ErrNotFound
	}
	delete(r.items, key)
	delete(r.etags, key)
	// Remove from ordered keys slice.
	for i, k := range r.keys {
		if k == key {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			break
		}
	}
	return nil
}
