package lro

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Manager creates and tracks long-running operations.
type Manager struct {
	store Store
}

// NewManager returns a Manager backed by store.
func NewManager(store Store) *Manager {
	return &Manager{store: store}
}

// Store returns the backing store (useful for GetOperation/ListOperations handlers).
func (m *Manager) Store() Store { return m.store }

// Submit starts fn asynchronously, records a pending [Operation], and returns it.
// The caller receives the operation immediately with Done=false.
//
// fn always runs with a fresh background context (not bound to the request
// lifecycle) so that the work continues even after the original request
// completes or is cancelled by the gRPC framework. This is the correct
// semantics for AIP-151: the operation outlives the RPC that created it.
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

	go func() {
		resp, fnErr := fn(context.Background())
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
		_ = m.store.Update(context.Background(), updated)
	}()

	return op, nil
}
