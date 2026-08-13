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
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/infobloxopen/devedge-sdk v0.64.0
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.5 // indirect
	github.com/infobloxopen/apis/proto/infoblox/storage v1.0.0-alpha.2 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
