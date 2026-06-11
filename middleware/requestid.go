package middleware

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type requestIDKey struct{}

// RequestIDFromContext returns the request-ID stored in ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// RequestIDUnary returns a gRPC unary interceptor that propagates or generates
// a request-ID. It reads the "x-request-id" key from incoming metadata; if
// present and non-empty that value is used, otherwise a new UUID is generated.
// The ID is stored in context and echoed as an outgoing header.
func RequestIDUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("x-request-id"); len(vals) > 0 && vals[0] != "" {
				id = vals[0]
			}
		}
		if id == "" {
			id = uuid.New().String()
		}
		ctx = context.WithValue(ctx, requestIDKey{}, id)
		_ = grpc.SetHeader(ctx, metadata.Pairs("x-request-id", id))
		return handler(ctx, req)
	}
}
