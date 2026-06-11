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

// TenantIDFromContext returns the tenant-ID stored in ctx, or "" if absent.
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey{}).(string); ok {
		return v
	}
	return ""
}

// TenantIDUnary returns a gRPC unary interceptor that extracts the tenant-ID
// from the "account-id" key in incoming metadata and stores it in context. It
// also sets a "cell-id: default" outgoing header.
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
