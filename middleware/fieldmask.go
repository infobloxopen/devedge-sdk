package middleware

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// FieldMaskUnary returns a gRPC unary interceptor that validates UpdateMask on
// update-verb methods. verbMap maps FullMethod → verb string (e.g. "update").
// For any method whose verb is "update", the request must implement
// GetUpdateMask() []string and the mask must be non-empty; otherwise
// codes.InvalidArgument is returned.
func FieldMaskUnary(verbMap map[string]string) grpc.UnaryServerInterceptor {
	return FieldMaskUnarySource(func() map[string]string { return verbMap })
}

// FieldMaskUnarySource is FieldMaskUnary over a lazily-resolved verb map, so the
// interceptor sees verbs for rules contributed after construction (e.g. the
// server's accumulated AddRules set). src is consulted on each request.
func FieldMaskUnarySource(src func() map[string]string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if verb, ok := src()[info.FullMethod]; ok && verb == "update" {
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

// Apply zeroes out fields of msg that are not listed in paths (AIP-157).
// An empty paths slice is a no-op — all fields are retained.
// Paths may use either proto field names (snake_case) or JSON names (camelCase).
// Unknown paths are silently ignored. Only top-level fields are considered;
// the function does not recurse into nested message fields.
func Apply(msg proto.Message, paths []string) {
	if len(paths) == 0 {
		return
	}
	keep := make(map[string]bool, len(paths))
	for _, p := range paths {
		keep[p] = true
	}
	m := msg.ProtoReflect()
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if !keep[string(fd.Name())] && !keep[fd.JSONName()] {
			m.Clear(fd)
		}
		return true
	})
}

// ReadMaskUnary returns a gRPC unary interceptor that applies a read_mask to
// proto responses (AIP-157). If the request implements
// GetReadMask() *fieldmaskpb.FieldMask and the mask has non-empty paths, the
// interceptor calls Apply on the response after the handler returns.
// Requests without GetReadMask(), nil masks, or empty path lists pass through
// unchanged. Handler errors are passed through without modification.
func ReadMaskUnary() grpc.UnaryServerInterceptor {
	type readMaskGetter interface {
		GetReadMask() *fieldmaskpb.FieldMask
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		rmg, hasReadMask := req.(readMaskGetter)
		if !hasReadMask {
			return handler(ctx, req)
		}
		mask := rmg.GetReadMask()
		if mask == nil || len(mask.GetPaths()) == 0 {
			return handler(ctx, req)
		}

		resp, err := handler(ctx, req)
		if err != nil || resp == nil {
			return resp, err
		}
		if pm, ok := resp.(proto.Message); ok {
			Apply(pm, mask.GetPaths())
		}
		return resp, nil
	}
}
