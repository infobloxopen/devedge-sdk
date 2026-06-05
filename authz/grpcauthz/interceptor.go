// Package grpcauthz wires the SDK's pluggable [authz.Authorizer] into gRPC as a
// fail-closed server interceptor. Its constructor and functional-option shape
// are rough-compatible with infobloxopen/atlas-authz-middleware/grpc_opa so that
// services can adopt the SDK with minimal change; see COMPAT.md. This package
// deliberately does NOT import that middleware or any policy engine — it depends
// only on the clean authz core + gRPC.
package grpcauthz

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
)

type obligationsKey struct{}

// Obligations returns any obligations attached to ctx by the interceptor
// (e.g. row-level filters a downstream handler should apply).
func Obligations(ctx context.Context) (map[string]any, bool) {
	o, ok := ctx.Value(obligationsKey{}).(map[string]any)
	return o, ok
}

// UnaryServerInterceptor returns a fail-closed unary interceptor. application is
// accepted for compatibility/labeling. Every method must be declared via
// WithMethodRule or WithPublicMethod; an undeclared method is denied.
func UnaryServerInterceptor(application string, opts ...Option) grpc.UnaryServerInterceptor {
	c := newConfig(opts...)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := c.authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// StreamServerInterceptor returns a fail-closed stream interceptor.
func StreamServerInterceptor(application string, opts ...Option) grpc.StreamServerInterceptor {
	c := newConfig(opts...)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := c.authorize(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: newCtx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func (c *config) authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	r, declared := c.rules[fullMethod]
	if !declared {
		if c.failOpen {
			return ctx, nil
		}
		return ctx, status.Errorf(codes.PermissionDenied, "authz: no rule declared for method %q", fullMethod)
	}
	if r.public {
		return ctx, nil
	}
	princ, err := c.principalFn(ctx)
	if err != nil {
		if c.failOpen {
			return ctx, nil
		}
		return ctx, status.Error(codes.Unauthenticated, "authz: no principal")
	}
	dec, err := c.authorizer.Authorize(ctx, authz.AccessRequest{
		Principal: princ,
		Verb:      r.verb,
		Resource:  authz.Resource{Type: r.resource},
		Method:    fullMethod,
	})
	if err != nil {
		if c.failOpen {
			return ctx, nil
		}
		return ctx, status.Error(codes.Internal, "authz: decision error")
	}
	if !dec.Allow {
		// AIP-211 existence-hiding message.
		return ctx, status.Errorf(codes.PermissionDenied,
			"Permission %q denied on resource %q (or it might not exist)", r.verb, r.resource)
	}
	if len(dec.Obligations) > 0 {
		ctx = context.WithValue(ctx, obligationsKey{}, dec.Obligations)
	}
	return ctx, nil
}

// AssertMethodsDeclared returns an error if any method has neither a rule nor a
// public declaration under the given options. Call it at startup (with the same
// options passed to the interceptor) to fail closed before the server serves —
// the boot-time completeness gate.
func AssertMethodsDeclared(methods []string, opts ...Option) error {
	c := newConfig(opts...)
	var missing []string
	for _, m := range methods {
		if _, ok := c.rules[m]; !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("authz: %d method(s) undeclared (add WithMethodRule or WithPublicMethod): %v",
			len(missing), missing)
	}
	return nil
}
