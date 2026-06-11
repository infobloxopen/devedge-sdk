// Package etag provides gRPC middleware for HTTP ETag / conditional-request
// semantics: it reads the If-Match precondition from incoming metadata and
// writes the ETag for the response to the outgoing trailer.
package etag

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type ifMatchKey struct{}
type newETagKey struct{}

// etagHolder is a mutable cell used to communicate a new ETag value from a
// handler back to the enclosing PreconditionUnary interceptor.
type etagHolder struct{ val string }

// IfMatchFromContext returns the If-Match value stored in ctx by the
// PreconditionUnary interceptor, or "" if absent.
func IfMatchFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ifMatchKey{}).(string); ok {
		return v
	}
	return ""
}

// SetIfMatch injects an expected ETag into ctx for testing precondition checks.
func SetIfMatch(ctx context.Context, val string) context.Context {
	return context.WithValue(ctx, ifMatchKey{}, val)
}

// SetNewETag stores val so it can be read back via NewETagFromContext and
// written as a gRPC trailer by PreconditionUnary. If a *etagHolder is already
// present in ctx (injected by the interceptor), it is mutated in-place so the
// interceptor sees the value without a new context being threaded back. A new
// context carrying the value is returned regardless, enabling standalone use
// (e.g. unit tests that call SetNewETag outside of an interceptor chain).
func SetNewETag(ctx context.Context, val string) context.Context {
	if h, ok := ctx.Value(newETagKey{}).(*etagHolder); ok {
		h.val = val
		return ctx
	}
	// No holder in context (standalone / test usage) — store the value directly.
	h := &etagHolder{val: val}
	return context.WithValue(ctx, newETagKey{}, h)
}

// NewETagFromContext returns the ETag value stored in ctx via SetNewETag, or
// "" if absent.
func NewETagFromContext(ctx context.Context) string {
	if h, ok := ctx.Value(newETagKey{}).(*etagHolder); ok {
		return h.val
	}
	return ""
}

// PreconditionUnary returns a gRPC unary interceptor that reads If-Match from
// incoming metadata into context and writes the new ETag as a trailer.
// Handlers signal their generated ETag by calling SetNewETag(ctx, val).
func PreconditionUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// Extract If-Match precondition from incoming metadata.
		ifMatch := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("if-match"); len(vals) > 0 {
				ifMatch = vals[0]
			}
		}
		ctx = context.WithValue(ctx, ifMatchKey{}, ifMatch)

		// Inject a mutable holder so handlers can communicate a new ETag back
		// to the interceptor without needing to return a modified context.
		holder := &etagHolder{}
		ctx = context.WithValue(ctx, newETagKey{}, holder)

		resp, err := handler(ctx, req)

		// Write the ETag trailer if the handler set one.
		if holder.val != "" {
			_ = grpc.SetTrailer(ctx, metadata.Pairs("etag", holder.val))
		}

		return resp, err
	}
}
