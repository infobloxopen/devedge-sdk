package middleware

import (
	"context"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// This file is the P2 identity-aware extension SPI: it puts the authorized
// caller and the resource/verb an operation targets onto the request context
// behind public accessors, so a NON-authz extension (an audit middleware, a
// tag-enforcement middleware, the change feed's Actor) can observe "who did what
// to which resource" uniformly — without re-deriving identity or reaching into
// the authz engine. The authz interceptor (the one stage that already computes
// these) populates them after a successful decision; everyone else just reads.
//
// It reuses authz.Principal rather than introducing a parallel identity type, so
// there is one notion of "the caller" across the SDK.

type principalKey struct{}
type resourceKey struct{}

// ResourceRef identifies the resource an operation targets: its type, its
// concrete name/id, and the verb being performed.
//
// Name is BEST-EFFORT: the authz stage resolves the resource TYPE and the VERB
// from the method's rule, but it does not (today) parse the concrete id out of
// the request, so Name is typically empty on the value the interceptor stashes.
// The authoritative resource identity for a change record comes from the entity
// itself (see events.ChangeEmitting), not from this ref.
type ResourceRef struct {
	Type string
	Name string
	Verb string
}

// WithPrincipal returns a copy of ctx carrying the authorized principal. Called
// by the authz interceptor after a successful decision.
func WithPrincipal(ctx context.Context, p authz.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authorized principal stashed on ctx and true,
// or the zero principal and false when none is present (an unauthenticated or
// public path).
func PrincipalFromContext(ctx context.Context) (authz.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(authz.Principal)
	return p, ok
}

// WithResource returns a copy of ctx carrying the resource/verb the current
// operation targets. Called by the authz interceptor.
func WithResource(ctx context.Context, r ResourceRef) context.Context {
	return context.WithValue(ctx, resourceKey{}, r)
}

// ResourceFromContext returns the resource/verb the current operation targets
// and true, or the zero ref and false when none is present.
func ResourceFromContext(ctx context.Context) (ResourceRef, bool) {
	r, ok := ctx.Value(resourceKey{}).(ResourceRef)
	return r, ok
}
