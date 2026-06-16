package lro

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Manager creates and tracks long-running operations.
type Manager struct {
	store   Store
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewManager returns a Manager backed by store.
func NewManager(store Store) *Manager {
	return &Manager{store: store, cancels: make(map[string]context.CancelFunc)}
}

// Store returns the backing store (useful for GetOperation/ListOperations handlers).
func (m *Manager) Store() Store { return m.store }

// Submit starts fn asynchronously, records a pending [Operation], and returns it.
// The caller receives the operation immediately with Done=false.
//
// fn runs with a fresh background-derived context that is independent of the
// original request lifecycle (so the work outlives the gRPC call per AIP-151)
// but can be cancelled via [Manager.Cancel].
func (m *Manager) Submit(ctx context.Context, metadata any, fn func(context.Context) (any, error)) (*Operation, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	id := uuid.New().String()
	now := time.Now()
	op := &Operation{
		Name:       OperationName(id),
		Done:       false,
		Metadata:   metadata,
		CreateTime: now,
		UpdateTime: now,
	}
	if err := m.store.Create(ctx, op); err != nil {
		return nil, err
	}

	opName := op.Name
	createTime := op.CreateTime

	opCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[opName] = cancel
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.cancels, opName)
			m.mu.Unlock()
			cancel() // idempotent cleanup
		}()

		resp, fnErr := fn(opCtx)
		updated := &Operation{
			Name:       opName,
			Done:       true,
			Metadata:   metadata,
			CreateTime: createTime,
			UpdateTime: time.Now(),
		}
		if fnErr != nil {
			updated.Err = fnErr
		} else {
			updated.Response = resp
		}
		// No-op if the store already marked the op cancelled.
		_ = m.store.Update(context.Background(), updated)
	}()

	return op, nil
}

// Cancel signals cancellation of the named operation.
// It atomically marks the operation done in the store and signals the goroutine's
// context. Returns [ErrNotFound] if unknown, [ErrAlreadyDone] if already complete.
func (m *Manager) Cancel(ctx context.Context, name string) error {
	if err := m.store.Cancel(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	cancel, ok := m.cancels[name]
	m.mu.Unlock()
	if ok {
		cancel()
	}
	return nil
}
