package middleware

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// DefaultCellID is the cell identifier returned in outgoing headers when no
// specific cell is resolved from the tenant.
const DefaultCellID = "default"

type tenantIDKey struct{}
type systemContextKey struct{}

// TenantIDFromContext returns the tenant that scopes the current request. The
// VERIFIED PRINCIPAL is the authority: when an authenticated principal is on the
// context (put there by the authn/authz stage via WithPrincipal), its Tenant is
// returned. The "account-id" header (see TenantIDUnary) is a routing hint only
// and is used solely as a fallback on paths that never establish a principal
// (e.g. the event consumer, which injects the tenant explicitly via
// WithTenantID). Returns "" when no tenant is established — callers on a
// tenant-scoped resource must fail closed.
func TenantIDFromContext(ctx context.Context) string {
	if p, ok := PrincipalFromContext(ctx); ok {
		return p.Tenant
	}
	if v, ok := ctx.Value(tenantIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTenantID injects tenantID directly into ctx. Intended for tests and
// non-gRPC call paths that cannot go through TenantIDUnary (e.g. the event
// consumer). It sets the header-fallback value only; a verified principal on
// the context still takes precedence in TenantIDFromContext.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// WithSystemContext marks ctx as a trusted cross-tenant/system operation that
// BYPASSES the tenant fence (admin, migration, background jobs). It is the ONLY
// sanctioned way to run a tenant-scoped query/mutation without an established
// tenant — an absent tenant otherwise fails closed. NEVER derive it from client
// input.
func WithSystemContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemContextKey{}, true)
}

// IsSystemContext reports whether ctx was marked by WithSystemContext.
func IsSystemContext(ctx context.Context) bool {
	v, _ := ctx.Value(systemContextKey{}).(bool)
	return v
}

// TenantIDUnary returns a gRPC unary interceptor that extracts the "account-id"
// key from incoming metadata and stores it in context as a ROUTING/CELLS hint.
// It also sets a "cell-id: default" outgoing header. The stashed value does NOT
// override the verified principal: TenantIDFromContext returns the principal's
// Tenant when one is present and only falls back to this header otherwise, so a
// spoofed account-id cannot widen a request's tenant scope.
func TenantIDUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		tenantID := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("account-id"); len(vals) > 0 {
				tenantID = vals[0]
			}
		}
		ctx = context.WithValue(ctx, tenantIDKey{}, tenantID)
		_ = grpc.SetHeader(ctx, metadata.Pairs("cell-id", DefaultCellID))
		return handler(ctx, req)
	}
}
