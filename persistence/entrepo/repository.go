package entrepo

import (
	"context"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
)

// CreateFn creates entity and returns the stored result.
type CreateFn[T any] func(ctx context.Context, entity T) (T, error)

// GetFn retrieves entity by key.
type GetFn[T any, K comparable] func(ctx context.Context, key K) (T, error)

// ListFn lists entities with options.
type ListFn[T any] func(ctx context.Context, opts persistence.ListOptions) ([]T, string, error)

// UpdateFn updates entity by key with optional field mask.
type UpdateFn[T any, K comparable] func(ctx context.Context, key K, entity T, fieldMask ...string) (T, error)

// DeleteFn deletes entity by key.
type DeleteFn[K comparable] func(ctx context.Context, key K) error

// EntRepository adapts ent-generated client functions to persistence.Repository[T,K].
// Construct via New; each function field wraps the corresponding ent client method.
type EntRepository[T any, K comparable] struct {
	Enc     secret.Encryptor // may be nil if no secret fields
	Create_ CreateFn[T]
	Get_    GetFn[T, K]
	List_   ListFn[T]
	Update_ UpdateFn[T, K]
	Delete_ DeleteFn[K]
}

func (r *EntRepository[T, K]) Create(ctx context.Context, entity T) (T, error) {
	return r.Create_(ctx, entity)
}

func (r *EntRepository[T, K]) Get(ctx context.Context, key K) (T, error) {
	return r.Get_(ctx, key)
}

func (r *EntRepository[T, K]) List(ctx context.Context, opts persistence.ListOptions) ([]T, string, error) {
	return r.List_(ctx, opts)
}

func (r *EntRepository[T, K]) Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error) {
	return r.Update_(ctx, key, entity, fieldMask...)
}

func (r *EntRepository[T, K]) Delete(ctx context.Context, key K) error {
	return r.Delete_(ctx, key)
}

// compile-time check
var _ persistence.Repository[any, string] = (*EntRepository[any, string])(nil)
