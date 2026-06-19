package entrepo

import (
	"context"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
)

// softDeleteContextKey is the unexported key for the show_deleted flag in context.
type softDeleteContextKey struct{}

// TenantFilterer is implemented by generated ent query types that support
// tenant-scoped filtering. protoc-gen-ent emits WhereAccountID on each tenant
// resource's query type (in ent/<resource>_filter.ent.go); the TenantMixin
// interceptor below calls it. If a query type does not implement this interface
// the filter is silently skipped — which is why the method is generated, not
// left to the consumer (GH #39).
type TenantFilterer interface {
	WhereAccountID(id string)
}

// SetTenantFilter applies the account_id filter to q if q implements TenantFilterer
// and the tenantID is non-empty.
func SetTenantFilter(q ent.Query, tenantID string) {
	if tenantID == "" {
		return
	}
	if f, ok := q.(TenantFilterer); ok {
		f.WhereAccountID(tenantID)
	}
}

// TenantMixin is an ent schema mixin that adds an immutable account_id field
// and a query interceptor that automatically scopes queries to the tenant
// from context (via middleware.TenantIDFromContext).
//
// Generated schemas embed this mixin when the proto message has an account_id field.
type TenantMixin struct {
	mixin.Schema
}

func (TenantMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("account_id").
			NotEmpty().
			Immutable().
			Comment("Tenant discriminator — all queries are automatically scoped to this value."),
	}
}

func (TenantMixin) Interceptors() []ent.Interceptor {
	return []ent.Interceptor{
		ent.InterceptFunc(func(next ent.Querier) ent.Querier {
			return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
				tenantID := middleware.TenantIDFromContext(ctx)
				SetTenantFilter(q, tenantID)
				return next.Query(ctx, q)
			})
		}),
	}
}

// WithShowDeleted returns a context that instructs soft-delete interceptors to
// include soft-deleted entities in query results (AIP-148 show_deleted).
func WithShowDeleted(ctx context.Context) context.Context {
	return context.WithValue(ctx, softDeleteContextKey{}, true)
}

// showDeletedFromContext returns true when the context was tagged by WithShowDeleted.
func showDeletedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(softDeleteContextKey{}).(bool)
	return v
}

// SoftDeleteFilterer is implemented by generated ent query types that support
// soft-delete filtering. protoc-gen-ent emits WhereDeleteTimeIsNil on each query type.
type SoftDeleteFilterer interface {
	WhereDeleteTimeIsNil()
}

// SetSoftDeleteFilter applies the soft-delete predicate to q when it implements
// SoftDeleteFilterer. Silently skipped when q does not implement the interface.
func SetSoftDeleteFilter(q ent.Query) {
	if f, ok := q.(SoftDeleteFilterer); ok {
		f.WhereDeleteTimeIsNil()
	}
}

// SoftDeleteMixin is an ent schema mixin that adds a nullable delete_time field
// and a query interceptor that automatically excludes soft-deleted entities
// unless the context was tagged with WithShowDeleted (AIP-148).
//
// Generated schemas embed this mixin when the proto message has a delete_time
// OUTPUT_ONLY Timestamp field.
type SoftDeleteMixin struct {
	mixin.Schema
}

func (SoftDeleteMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Time("delete_time").
			Optional().
			Nillable().
			Comment("AIP-148 soft-delete timestamp; nil for live entities."),
	}
}

func (SoftDeleteMixin) Interceptors() []ent.Interceptor {
	return []ent.Interceptor{
		ent.InterceptFunc(func(next ent.Querier) ent.Querier {
			return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
				if !showDeletedFromContext(ctx) {
					SetSoftDeleteFilter(q)
				}
				return next.Query(ctx, q)
			})
		}),
	}
}

// etagSetter is implemented by every generated mutation type for a resource that
// embeds EtagMixin (the mixin's etag field generates a SetEtag method). The hook
// below uses it to stamp a fresh token without depending on a concrete type.
type etagSetter interface {
	SetEtag(string)
}

// EtagMixin is an ent schema mixin that adds an opaque AIP-154 `etag` field and a
// mutation hook that stamps a fresh token (etag.New) on every Create and Update —
// so a resource's ETag changes whenever the resource changes. This mirrors the
// GORM storage layer, which stamps etag.New() on Create/Update, giving the two
// backends the same optimistic-concurrency behavior out of the box.
//
// Generated schemas embed this mixin when the proto message has a string `etag`
// field. The write path (persist + re-stamp) is fully automatic — there is no
// consumer code to write for it. A consumer's fromEnt mapping surfaces e.Etag on
// the proto so a Get returns the stable token a client echoes as If-Match; the
// If-Match precondition comparison itself stays the documented handler pattern,
// identical on both backends.
type EtagMixin struct {
	mixin.Schema
}

func (EtagMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("etag").
			Optional().
			Comment("AIP-154 opaque concurrency token; re-stamped on every write."),
	}
}

func (EtagMixin) Hooks() []ent.Hook {
	return []ent.Hook{
		func(next ent.Mutator) ent.Mutator {
			return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
				// Stamp a fresh token on create and every update. ent query
				// interceptors do not run for mutations, so this lives in a hook
				// (which does) — the write-path analogue of the Tenant/SoftDelete
				// query interceptors above.
				if m.Op().Is(ent.OpCreate | ent.OpUpdate | ent.OpUpdateOne) {
					if s, ok := m.(etagSetter); ok {
						s.SetEtag(etag.New())
					}
				}
				return next.Mutate(ctx, m)
			})
		},
	}
}
