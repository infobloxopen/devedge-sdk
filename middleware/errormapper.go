package middleware

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// ErrorMapperUnary returns a gRPC unary interceptor that maps well-known
// persistence errors to canonical gRPC status codes.
func ErrorMapperUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		switch {
		case errors.Is(err, persistence.ErrNotFound):
			return nil, status.Error(codes.NotFound, "not found")
		case errors.Is(err, persistence.ErrConflict):
			return nil, status.Error(codes.AlreadyExists, "already exists")
		case errors.Is(err, persistence.ErrPreconditionFailed):
			return nil, status.Error(codes.FailedPrecondition, "precondition failed")
		default:
			return nil, err
		}
	}
}
