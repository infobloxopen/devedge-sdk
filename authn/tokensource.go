package authn

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// This file is the OUTBOUND half of the token model (WS-028): the ergonomic,
// cached service-to-service token seam. The trust boundary is the audience —
// within one audience (an app's own services) the caller passes its inbound
// bearer through; across audiences it must present a token scoped to the target,
// obtained via RFC 8693 token exchange. TokenSource hides that decision behind
// one call; the concrete exchanging implementation lives in the nested authn/oidc
// module (go-jose + HTTP), so this seam stays stdlib+grpc and the SDK root stays
// dependency-light.

// TokenSource yields the bearer to present when calling another service. TokenFor
// returns the inbound token unchanged when targetAudience is already one of the
// caller's audiences (passthrough within a trust domain), or a token scoped to
// targetAudience obtained via RFC 8693 token exchange otherwise. It fails closed:
// if it cannot produce a token scoped to targetAudience it returns an error, never
// the raw inbound token destined for a different audience.
type TokenSource interface {
	TokenFor(ctx context.Context, targetAudience string) (string, error)
}

// AudienceResolver maps a logical outbound target (service name / host) to the
// audience its tokens must carry. Sourced from config now; apx-catalog-backed
// later — the resolver seam is what lets the catalog become the ergonomic path
// without changing call sites.
type AudienceResolver interface {
	// AudienceFor returns the audience configured for target and true, or "" and
	// false when target is unmapped (the caller must fail closed on false).
	AudienceFor(target string) (audience string, ok bool)
}

// StaticAudiences is a map-backed [AudienceResolver] for config-driven wiring:
// target (a gRPC service name like "orders.v1.Orders", or a host) -> audience.
type StaticAudiences map[string]string

// AudienceFor implements [AudienceResolver].
func (s StaticAudiences) AudienceFor(target string) (string, bool) {
	aud, ok := s[target]
	return aud, ok
}

// UnaryClientInterceptor returns a gRPC client interceptor that attaches an
// outbound bearer for the target service. It resolves the target audience from
// the invoked method's service (the "/pkg.Service/Method" prefix) via r, obtains
// a token for that audience from ts, and sets "authorization: Bearer <token>" on
// the outgoing metadata. It is FAIL-CLOSED: a method whose service has no mapped
// audience returns codes.FailedPrecondition without making the call, so the raw
// inbound token is never sent cross-domain by accident.
func UnaryClientInterceptor(ts TokenSource, r AudienceResolver) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if ts == nil || r == nil {
			return status.Error(codes.FailedPrecondition, "authn: outbound interceptor missing TokenSource or AudienceResolver")
		}
		svc := serviceFromMethod(method)
		aud, ok := r.AudienceFor(svc)
		if !ok {
			return status.Errorf(codes.FailedPrecondition, "authn: no audience mapped for outbound target %q (fail closed; add it to the AudienceResolver)", svc)
		}
		token, err := ts.TokenFor(ctx, aud)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "authn: token for audience %q: %v", aud, err)
		}
		ctx = metadata.AppendToOutgoingContext(ctx, DefaultMetadataKey, "Bearer "+token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// serviceFromMethod extracts the service name from a gRPC full method
// ("/pkg.Service/Method" -> "pkg.Service"). It returns method unchanged if it is
// not in that shape.
func serviceFromMethod(method string) string {
	m := strings.TrimPrefix(method, "/")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[:i]
	}
	return m
}

// NewRoundTripper wraps base with one that attaches "Authorization: Bearer
// <token>" for a single known audience — for REST clients that each talk to one
// target. It obtains the token from ts per request (so caching/exchange happen in
// ts). A transport error obtaining the token fails the request; it never sends
// the request without the intended token. base nil defaults to
// http.DefaultTransport.
func NewRoundTripper(base http.RoundTripper, ts TokenSource, audience string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerRoundTripper{base: base, ts: ts, audience: audience}
}

type bearerRoundTripper struct {
	base     http.RoundTripper
	ts       TokenSource
	audience string
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.ts == nil {
		return nil, fmt.Errorf("authn: RoundTripper has no TokenSource")
	}
	token, err := rt.ts.TokenFor(req.Context(), rt.audience)
	if err != nil {
		return nil, fmt.Errorf("authn: token for audience %q: %w", rt.audience, err)
	}
	// A RoundTripper must not mutate the caller's request; clone it (shallow copy
	// with a cloned header) before setting Authorization.
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+token)
	return rt.base.RoundTrip(r2)
}
