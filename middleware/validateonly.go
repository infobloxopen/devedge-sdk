package middleware

import (
	"context"

	"google.golang.org/grpc"
)

type validateOnlyKey struct{}

// ValidateOnlyFromContext returns the validate-only flag stored in ctx by
// ValidateOnlyUnary, or false if absent.
func ValidateOnlyFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(validateOnlyKey{}).(bool)
	return v
}

// ValidateOnlyUnary returns a gRPC unary interceptor that stores true in
// context when the request implements GetValidateOnly() bool and returns true.
// The handler is always called.
func ValidateOnlyUnary() grpc.UnaryServerInterceptor {
	type validateOnlyGetter interface {
		GetValidateOnly() bool
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if vog, ok := req.(validateOnlyGetter); ok && vog.GetValidateOnly() {
			ctx = context.WithValue(ctx, validateOnlyKey{}, true)
		}
		return handler(ctx, req)
	}
}
