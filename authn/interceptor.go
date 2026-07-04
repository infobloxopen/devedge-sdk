package authn

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
)

// DefaultMetadataKey is the incoming metadata key the bearer is read from.
const DefaultMetadataKey = "authorization"

type interceptorConfig struct {
	metadataKey string
	required    bool
}

// InterceptorOption configures the authentication interceptor.
type InterceptorOption func(*interceptorConfig)

// WithMetadataKey overrides the metadata key the bearer is read from
// (default "authorization").
func WithMetadataKey(key string) InterceptorOption {
	return func(c *interceptorConfig) {
		if key != "" {
			c.metadataKey = key
		}
	}
}

// WithRequired makes the interceptor reject a request that carries NO bearer
// with codes.Unauthenticated. The default is optional: a request with no bearer
// passes through with no principal stashed, so PUBLIC methods still work and
// non-public methods fail closed downstream at the (default-deny) authorizer.
// A malformed or INVALID bearer is always rejected regardless of this option.
func WithRequired() InterceptorOption {
	return func(c *interceptorConfig) { c.required = true }
}

// UnaryServerInterceptor returns the authentication interceptor (Role 3). It
// runs BEFORE the authz interceptor: it pulls the bearer from request metadata,
// verifies it with the [Authenticator], and stashes the verified
// [authz.Principal] on the context via [middleware.WithPrincipal]. The authz
// interceptor then reads it through [VerifiedPrincipal] (wired as its
// grpcauthz.PrincipalFunc). It is fail-closed: an invalid bearer is rejected
// with codes.Unauthenticated and the handler never runs.
//
// A nil Authenticator returns a no-op interceptor (identity unchanged) so the
// stage is inert until a backend is configured.
func UnaryServerInterceptor(a Authenticator, opts ...InterceptorOption) grpc.UnaryServerInterceptor {
	cfg := &interceptorConfig{metadataKey: DefaultMetadataKey}
	for _, o := range opts {
		o(cfg)
	}
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if a == nil {
			return handler(ctx, req)
		}
		bearer, ok := bearerFromContext(ctx, cfg.metadataKey)
		if !ok {
			if cfg.required {
				return nil, status.Error(codes.Unauthenticated, "authn: missing bearer token")
			}
			return handler(ctx, req)
		}
		p, err := a.Authenticate(ctx, bearer)
		if err != nil {
			// Fail closed: a presented-but-invalid token is rejected, never
			// silently downgraded to an empty principal.
			return nil, status.Error(codes.Unauthenticated, "authn: invalid bearer token")
		}
		return handler(middleware.WithPrincipal(ctx, p), req)
	}
}

// VerifiedPrincipal is the grpcauthz.PrincipalFunc adapter: it returns the
// principal the authentication interceptor stashed on ctx, or the zero principal
// when none is present (an unauthenticated/public path — authz default-denies
// any non-public method for it). Wire it with
// grpcauthz.WithPrincipalFunc(authn.VerifiedPrincipal) so the authorizer sees
// only VERIFIED identities, replacing the unverified DevPrincipalFunc.
func VerifiedPrincipal(ctx context.Context) (authz.Principal, error) {
	p, ok := middleware.PrincipalFromContext(ctx)
	if !ok {
		return authz.Principal{}, nil
	}
	return p, nil
}

// bearerFromContext extracts the bearer token from the "<key>: Bearer <token>"
// incoming metadata header (scheme match is case-insensitive).
func bearerFromContext(ctx context.Context, key string) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(key)
	if len(vals) == 0 {
		return "", false
	}
	const scheme = "bearer "
	v := vals[0]
	if len(v) < len(scheme) || !strings.EqualFold(v[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(v[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
