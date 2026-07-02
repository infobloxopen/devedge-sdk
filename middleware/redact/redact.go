// Package redact provides proto-reflection-based helpers that replace
// write-only field values with "[REDACTED]" (strings) or the zero value (other
// kinds) before they are logged or returned.
//
// A field is write-only when its EFFECTIVE AIP field_behavior (resolved by the
// shared internal/aip package) includes INPUT_ONLY. That is a superset of the
// (infoblox.field.v1.opts).secret case: a secret field derives INPUT_ONLY, so
// it is still redacted, and an explicitly INPUT_ONLY field is redacted too.
// secret ALSO drives storage encryption/hashing (unchanged, elsewhere);
// redaction here only strips values from wire/log output.
//
// It ships as a standalone function ([Message]), a log-only interceptor
// ([UnaryServerInterceptor]), and an opt-in response-stripping interceptor
// ([ResponseUnary]).
package redact

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/infobloxopen/devedge-sdk/internal/aip"
)

// Message returns a clone of m with every write-only field (effective
// field_behavior includes INPUT_ONLY — the secret case and explicit INPUT_ONLY)
// MASKED: string fields become "[REDACTED]", other kinds become the zero value.
// This is the LOGGING form — a record shows the value was present but hidden.
// Safe to call on nil.
func Message(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	clone := proto.Clone(m)
	walkAndRedact(clone.ProtoReflect(), true)
	return clone
}

// Strip returns a clone of m with every write-only field (INPUT_ONLY — the
// secret case and explicit INPUT_ONLY) CLEARED to its zero value (empty string,
// not "[REDACTED]"). This is the RESPONSE form — a write-only field is never
// returned, so it is absent/empty on the wire. Safe to call on nil.
func Strip(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	clone := proto.Clone(m)
	walkAndRedact(clone.ProtoReflect(), false)
	return clone
}

// walkAndRedact strips write-only fields. When mask is true, string fields are
// replaced with "[REDACTED]" (logging); otherwise every write-only field is
// cleared to its zero value (responses).
func walkAndRedact(msg protoreflect.Message, mask bool) {
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
			walkAndRedact(v.Message(), mask)
			return true
		}
		// A write-only field (INPUT_ONLY, resolved via the shared classifier so a
		// secret field — which derives INPUT_ONLY — is still covered) is stripped.
		bs, _ := aip.ResolveFieldBehavior(fd)
		if !aip.HasBehavior(bs, aip.InputOnly) {
			return true
		}
		if mask && fd.Kind() == protoreflect.StringKind {
			msg.Set(fd, protoreflect.ValueOfString("[REDACTED]"))
		} else {
			msg.Clear(fd)
		}
		return true
	})
}

// UnaryServerInterceptor returns a gRPC unary server interceptor that logs
// redacted copies of the request and response (write-only fields replaced with
// "[REDACTED]"). The real request/response passed to the handler and returned to
// the client are unchanged.
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

// ResponseUnary returns a gRPC unary server interceptor that STRIPS write-only
// fields (INPUT_ONLY — the secret case and explicit INPUT_ONLY) from the
// RESPONSE returned to the client, enforcing the field_behavior contract at the
// wire boundary: a write-only field is accepted on input but never returned.
//
// It is OPT-IN — wire it via server.Config.Interceptors for a service that must
// guarantee write-only fields never leave the process. It is intentionally NOT
// in the default server chain: some services return a secret exactly once (e.g.
// on Create), so response stripping is a per-service policy, not a framework
// default.
func ResponseUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			return resp, err
		}
		if m, ok := resp.(proto.Message); ok {
			return Strip(m), nil
		}
		return resp, err
	}
}
