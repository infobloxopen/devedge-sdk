package lro

import (
	"context"
	"sync"
	"time"
)

// Store persists and retrieves [Operation] resources.
type Store interface {
	Create(ctx context.Context, op *Operation) error
	Get(ctx context.Context, name string) (*Operation, error)
	Update(ctx context.Context, op *Operation) error
	// Cancel atomically marks the named operation as done with [ErrCancelled].
	// Returns [ErrNotFound] if name is unknown, [ErrAlreadyDone] if Done=true.
	Cancel(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]*Operation, error)
}

type memoryEntry struct {
	op        *Operation
	expiresAt time.Time // zero means never expires
}

// MemoryStore is a goroutine-safe in-memory [Store].
// Completed operations expire and are purged after ttl; pending operations never expire.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]*memoryEntry
	ttl time.Duration
}

// NewMemoryStore returns a MemoryStore that expires completed operations after ttl.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	return &MemoryStore{m: make(map[string]*memoryEntry), ttl: ttl}
}

func (s *MemoryStore) Create(ctx context.Context, op *Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *op
	s.m[op.Name] = &memoryEntry{op: &cp}
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, name string) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[name]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(s.m, name)
		return nil, ErrNotFound
	}
	cp := *e.op
	return &cp, nil
}

// Update replaces the stored operation. If the operation is already done,
// the call is a no-op (idempotent double-complete protection).
func (s *MemoryStore) Update(ctx context.Context, op *Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[op.Name]
	if !ok {
		return ErrNotFound
	}
	if e.op.Done {
		return nil // already completed — idempotent
	}
	cp := *op
	e.op = &cp
	if op.Done {
		e.expiresAt = time.Now().Add(s.ttl)
	}
	return nil
}

// Cancel atomically marks the operation as done with [ErrCancelled].
// The check-and-set is under the same lock so it is race-free.
func (s *MemoryStore) Cancel(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[name]
	if !ok {
		return ErrNotFound
	}
	if e.op.Done {
		return ErrAlreadyDone
	}
	now := time.Now()
	cancelled := *e.op
	cancelled.Done = true
	cancelled.Err = ErrCancelled
	cancelled.UpdateTime = now
	e.op = &cancelled
	e.expiresAt = now.Add(s.ttl)
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return ErrNotFound
	}
	delete(s.m, name)
	return nil
}

// List returns all non-expired operations, purging expired entries in the process.
func (s *MemoryStore) List(ctx context.Context) ([]*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]*Operation, 0, len(s.m))
	for name, e := range s.m {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(s.m, name)
			continue
		}
		cp := *e.op
		out = append(out, &cp)
	}
	return out, nil
}
