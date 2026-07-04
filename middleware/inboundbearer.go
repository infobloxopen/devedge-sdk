package middleware

import "context"

// This file extends the P2 identity SPI (identity.go) with the caller's RAW
// inbound bearer, stashed alongside the principal by the authentication stage.
// It exists for on-behalf-of / delegation: an outbound call into ANOTHER trust
// domain (a different audience) must present a token scoped to that domain, and
// RFC 8693 token exchange uses the inbound bearer as the `subject_token`. Within
// one audience (an app's own services) the raw bearer is passed through
// unchanged; across audiences it is exchanged. See the root authn.TokenSource
// seam (WS-028). Purely additive: nothing reads this unless a TokenSource does.

type inboundBearerKey struct{}

// WithInboundBearer returns a copy of ctx carrying the caller's raw inbound
// bearer token. The authentication interceptor calls it next to
// [WithPrincipal] after a successful verify, so a downstream TokenSource can act
// on behalf of the caller when calling another service.
func WithInboundBearer(ctx context.Context, raw string) context.Context {
	return context.WithValue(ctx, inboundBearerKey{}, raw)
}

// InboundBearerFromContext returns the caller's raw inbound bearer and true, or
// "" and false when none is present (an unauthenticated/public path, or a stage
// that did not stash it). A delegation TokenSource fails closed on false: there
// is no verified identity to act on behalf of.
func InboundBearerFromContext(ctx context.Context) (string, bool) {
	raw, ok := ctx.Value(inboundBearerKey{}).(string)
	return raw, ok
}
