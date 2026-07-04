// Package authn defines the transport-neutral, pluggable AUTHENTICATION seams
// for devedge services — the "verify" and "mint" halves of the two-tier token
// model (WS-026). It is deliberately free of any JOSE/JWKS/OIDC-library types;
// the concrete signing/verifying backend lives in the nested authn/oidc module
// so the SDK root stays dependency-light.
//
// # The two-tier model
//
// Authentication answers "who is the verified caller?" and produces an
// [authz.Principal]; authorization ([authz.Authorizer]) then decides what that
// principal may do. authn never authorizes — it only verifies identity and
// authors the claims that become the principal.
//
// Three cooperating roles:
//
//   - Role 1 — the upstream IdP (a separate service, e.g. devedge-idp) issues an
//     IDENTITY ASSERTION (an OIDC id_token) with COARSE claims only: identity +
//     app-access entitlement. It does not mint API bearers.
//   - Role 2 — the app identity is a confidential relying party that completes
//     the OIDC dance with the IdP, then MINTS + signs its own app bearer via an
//     [Issuer], authoring the rich app-specific claims through a [ClaimsMapper].
//   - Role 3 — the microservice VERIFIES the app bearer via an [Authenticator]
//     (signature + iss/aud/exp) and maps its claims back to an [authz.Principal].
//
// # Topology
//
// [TokenTopology] selects who mints and whom the verifier trusts. The verify
// seam is topology-agnostic — it verifies a bearer against whichever issuer it
// is configured to trust — so single-issuer is just Role 1 + Role 3 (minting
// off) and two-tier adds the Role 2 minter. The default is [TwoTier].
package authn

import (
	"context"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// TokenTopology selects the token minting/verification topology.
type TokenTopology string

const (
	// TwoTier is the default: the app identity mints the app bearer (Role 2) and
	// microservices verify against the APP's issuer/JWKS (Role 3). Swapping the
	// upstream IdP is a config change at Role 2 only — no microservice change.
	TwoTier TokenTopology = "two-tier"
	// SingleIssuer is the alternative: the IdP mints the audience-scoped bearer
	// directly and microservices verify against the IDP's issuer/JWKS. Role 2 does
	// not mint. Falls out of Role 1 + Role 3 because verification is
	// topology-agnostic.
	SingleIssuer TokenTopology = "single-issuer"
)

// Identity is the COARSE upstream identity assertion, as produced by the IdP
// (Role 1): who the caller is and which apps they may enter. It intentionally
// carries NO in-app roles/tenant/scopes — those are authored downstream by the
// app identity's [ClaimsMapper] (Role 2), which is why the app-identity tier
// exists (WS-026 §2.1 / D11).
type Identity struct {
	// Subject is the stable user identifier (the id_token `sub`).
	Subject string
	// Name is the human-readable display name, if asserted.
	Name string
	// Email is the caller's email, if asserted.
	Email string
	// Apps is the app-access entitlement: the app/client names this identity may
	// enter (the same set that drives the IdP launchpad tiles). A [ClaimsMapper]
	// uses it to confirm entitlement to a specific app.
	Apps []string
	// Raw holds any additional coarse claims from the upstream assertion.
	Raw map[string]any
}

// ClaimsMapper authors the app-specific [authz.Principal] for an [Identity] —
// the Role 2 claim-authoring seam (WS-026 §2.1). It confirms the identity is
// entitled to THIS app (via [Identity.Apps]) and enriches the principal with the
// roles/tenant/scopes that drive authz. The dev default is [StaticClaimsMapper]
// (a manipulable, hot-reloadable static mapping); production binds a real source
// (directory / entitlements service).
type ClaimsMapper interface {
	// MapClaims returns the authored principal for id within this app, or an error
	// (e.g. [ErrNotEntitled]) when the identity may not enter the app.
	MapClaims(ctx context.Context, id Identity) (authz.Principal, error)
}

// Issuer mints + signs the app bearer for an authored principal — the Role 2
// mint seam. iss is the app; aud is the app's microservices. The concrete
// JOSE-backed implementation lives in authn/oidc (dependency-light root).
type Issuer interface {
	// Mint returns a signed app bearer encoding p's identity and authored claims.
	Mint(ctx context.Context, p authz.Principal) (bearer string, err error)
}

// Authenticator verifies a bearer and returns the caller's [authz.Principal] —
// the Role 3 verify seam. The default backend (authn/oidc) verifies the app
// bearer against the app's issuer/JWKS (signature + iss/aud/exp) and maps the
// verified claims to a principal. It MUST fail closed: an invalid, expired, or
// wrong-issuer token returns an error, never a partial principal.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (authz.Principal, error)
}

// AuthenticatorFunc adapts a function to an [Authenticator].
type AuthenticatorFunc func(ctx context.Context, bearer string) (authz.Principal, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(ctx context.Context, bearer string) (authz.Principal, error) {
	return f(ctx, bearer)
}
