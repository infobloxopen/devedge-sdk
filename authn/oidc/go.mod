// authn/oidc is a NESTED Go module (WS-011 / F039 isolation idiom; WS-026): the
// JOSE/JWKS signing + verification machinery for the two-tier token model lives
// here, not in the root module's dependency graph. The module path equals the
// import path, so a consumer pulls go-jose only when it `require`s THIS module.
//
// It provides the concrete backends for the root authn seams: an Issuer (Role 2
// mint + JWKS) and an Authenticator (Role 3 verify). It requires the root
// devedge-sdk module for the authn.Issuer/authn.Authenticator seams and the
// authz.Principal type. The local go.work at the repo root resolves that require
// to the working tree during dev/CI; the require below is the version a
// published consumer resolves, bumped by the synchronized release script.
module github.com/infobloxopen/devedge-sdk/authn/oidc

go 1.25.5

require (
	github.com/go-jose/go-jose/v4 v4.0.5
	github.com/infobloxopen/devedge-sdk v0.52.0
)

require golang.org/x/crypto v0.32.0 // indirect
