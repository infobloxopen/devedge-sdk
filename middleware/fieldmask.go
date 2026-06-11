package middleware

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FieldMaskUnary returns a gRPC unary interceptor that validates UpdateMask on
// update-verb methods. verbMap maps FullMethod → verb string (e.g. "update").
// For any method whose verb is "update", the request must implement
// GetUpdateMask() []string and the mask must be non-empty; otherwise
// codes.InvalidArgument is returned.
func FieldMaskUnary(verbMap map[string]string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if verb, ok := verbMap[info.FullMethod]; ok && verb == "update" {
			type maskGetter interface {
				GetUpdateMask() []string
			}
			if mg, ok := req.(maskGetter); ok {
				if len(mg.GetUpdateMask()) == 0 {
					return nil, status.Error(codes.InvalidArgument, "update_mask is required for update operations")
				}
			}
		}
		return handler(ctx, req)
	}
}
