package entrepo

import (
	"context"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"

	"github.com/infobloxopen/devedge-sdk/middleware"
)

// TenantFilterer is implemented by generated ent query types that support
// tenant-scoped filtering. protoc-gen-ent emits WhereAccountID on each query type.
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
