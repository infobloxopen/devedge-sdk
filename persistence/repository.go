// Package persistence provides connection and storage helpers for Infoblox
// services. It intentionally does NOT impose an ORM or a single persistence
// model: the API/app shape (resource-oriented, AIP-aligned) is what the vision
// drives, and persistence is a pluggable concern that serves it underneath.
//
// What the SDK provides:
//   - a connection convention — the [DSN] abstraction (including devedge's
//     indirect hotload form) for resolving a driver + data source name;
//   - an optional, engine-neutral [Repository] seam plus an in-memory
//     implementation ([MemoryRepository]) for the common CRUD case and tests.
//
// What the SDK does NOT dictate is the "persistence shape" — how entities and
// queries are modeled and generated. That is a per-service, pluggable choice.
// Candidate shapes include proto->GORM (the current atlas approach; one option,
// not the default), ent (entgo.io), and sqlc. A service may code against the
// neutral [Repository] seam for portability, or use a shape's generated client
// directly when it needs that shape's power. Schema migrations use
// infobloxopen/migrate regardless of shape. See SHAPES.md.
package persistence

import (
	"context"
	"errors"
)

// Common errors.
var (
	ErrNotFound = errors.New("persistence: not found")
	ErrConflict = errors.New("persistence: conflict")
)

// ListOptions carries resource-oriented list parameters (filter/order/paging),
// aligned with standard API list semantics.
type ListOptions struct {
	Filter    string
	OrderBy   string
	PageSize  int
	PageToken string
}

// Repository is a generic CRUD seam for an entity T keyed by K. The methods
// mirror the standard API operations (get/list/create/update/delete), matching
// the authz verb vocabulary.
type Repository[T any, K comparable] interface {
	Get(ctx context.Context, key K) (T, error)
	List(ctx context.Context, opts ListOptions) (items []T, nextPageToken string, err error)
	Create(ctx context.Context, entity T) (T, error)
	Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error)
	Delete(ctx context.Context, key K) error
}
