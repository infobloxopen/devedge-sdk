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
	ErrNotFound          = errors.New("persistence: not found")
	ErrConflict          = errors.New("persistence: conflict")
	ErrPreconditionFailed = errors.New("persistence: precondition failed")
)

// MaxPageSize is the AIP-158 upper bound the generated List paths clamp a
// caller-requested page_size to. A request for more than this many resources is
// silently reduced to MaxPageSize (the response still paginates via next_page_token),
// so a single unbounded List cannot exhaust server memory.
const MaxPageSize = 1000

// ListOptions carries resource-oriented list parameters (filter/order/paging),
// aligned with standard API list semantics.
type ListOptions struct {
	Filter      string
	OrderBy     string
	PageSize    int
	PageToken   string
	// ShowDeleted includes soft-deleted resources in List results (AIP-148).
	// When false (the zero value), soft-deleted resources are excluded.
	ShowDeleted bool
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
	// Undelete restores a soft-deleted entity (AIP-149). Returns ErrNotFound when the
	// entity does not exist, was never soft-deleted, or has been permanently purged.
	// Implementations backed by hard-delete storage always return ErrNotFound.
	Undelete(ctx context.Context, key K) (T, error)
}

// BatchRepository extends Repository with multi-resource batch operations (AIP-137).
// All batch operations are atomic: if any key is invalid, the entire call fails without
// modifying any resource.
type BatchRepository[T any, K comparable] interface {
	Repository[T, K]
	// BatchGet retrieves multiple resources by key. Returns items in the same order as
	// keys. Returns ErrNotFound if any key does not exist or is soft-deleted. An empty
	// keys slice returns an empty slice with no error.
	BatchGet(ctx context.Context, keys []K) ([]T, error)
	// BatchUpdate updates multiple resources atomically. Each item carries its key, the
	// new entity value, and an optional field mask (empty mask = full update, matching
	// Update). Returns the updated entities in the same order as items. Returns
	// ErrNotFound if any key does not exist or is soft-deleted; on error no items are
	// modified. An empty items slice returns an empty slice with no error.
	BatchUpdate(ctx context.Context, items []BatchUpdateItem[T, K]) ([]T, error)
	// BatchDelete soft-deletes multiple resources. Returns ErrNotFound if any key does
	// not exist or is already soft-deleted; on error no items are deleted. An empty
	// keys slice is a no-op.
	BatchDelete(ctx context.Context, keys []K) error
}

// BatchUpdateItem is a single update within a BatchUpdate call: the target key, the
// new entity value, and an optional field mask selecting which fields to write. An
// empty FieldMask is a full update, matching Repository.Update.
type BatchUpdateItem[T any, K comparable] struct {
	Key       K
	Entity    T
	FieldMask []string
}
