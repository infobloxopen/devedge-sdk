// Package redact provides proto-reflection-based helpers that replace
// (infoblox.field.v1.opts).secret = true field values with "[REDACTED]"
// before logging. It ships as both a standalone function and a gRPC unary
// interceptor wrapper.
package redact

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// Message returns a clone of m with all fields annotated
// (infoblox.field.v1.opts).secret = true replaced with "[REDACTED]"
// (string fields) or zero value (other kinds). Safe to call on nil.
func Message(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	clone := proto.Clone(m)
	walkAndRedact(clone.ProtoReflect())
	return clone
}

func walkAndRedact(msg protoreflect.Message) {
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
			walkAndRedact(v.Message())
			return true
		}
		opts := fd.Options()
		if opts == nil {
			return true
		}
		if !proto.HasExtension(opts, fieldv1.E_Opts) {
			return true
		}
		fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions)
		if !ok || fopts == nil || !fopts.GetSecret() {
			return true
		}
		switch fd.Kind() {
		case protoreflect.StringKind:
			msg.Set(fd, protoreflect.ValueOfString("[REDACTED]"))
		default:
			msg.Clear(fd)
		}
		return true
	})
}

// UnaryServerInterceptor returns a gRPC unary server interceptor that logs
// redacted copies of the request and response (secret fields replaced with
// "[REDACTED]"). The real request/response passed to the handler are unchanged.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if m, ok := req.(proto.Message); ok {
			slog.Debug("grpc request", "method", info.FullMethod, "req", Message(m))
		}
		resp, err := handler(ctx, req)
		if err == nil {
			if m, ok := resp.(proto.Message); ok {
				slog.Debug("grpc response", "method", info.FullMethod, "resp", Message(m))
			}
		}
		return resp, err
	}
}
