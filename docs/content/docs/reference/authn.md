---
title: authn
weight: 5
---

```go
import "github.com/infobloxopen/devedge-sdk/authn"
```

Package `authn` defines the authentication seams — the verify and mint halves of the two-tier token
model. It carries no JOSE, JWKS, or OIDC library types; the signing and verifying backend lives in
the nested module `authn/oidc` (documented below), so a service that only wires the seams stays
dependency-light. Authentication produces an `authz.Principal`; the authorizer then decides what
that principal may do.

Three parties cooperate: an identity provider (IdP) asserts a coarse identity, an app identity mints
an app bearer that authors this app's claims, and your microservice verifies that bearer. To wire
these seams into a service, see [Add authentication](../../how-to/secure/add-authentication/).

## TokenTopology

Selects who mints the bearer and whom the verifier trusts. The verify seam is topology-agnostic;
single-issuer omits the app-identity minter.

```go
type TokenTopology string

const (
    TwoTier      TokenTopology = "two-tier"      // the app identity mints; microservices verify the app's issuer/JWKS
    SingleIssuer TokenTopology = "single-issuer" // the IdP mints the audience-scoped bearer; microservices verify the IdP
)
```

`TwoTier` is the default.

## Identity

The coarse upstream identity the IdP asserts: who the caller is and which apps they may enter. It
carries no in-app roles, tenant, or scopes — a [ClaimsMapper](#claimsmapper) authors those.

```go
type Identity struct {
    Subject string
    Name    string
    Email   string
    Apps    []string
    Raw     map[string]any
}
```

| Field | What it is |
|---|---|
| `Subject` | the stable user identifier (the id_token `sub`) |
| `Name` | the human-readable display name, if asserted |
| `Email` | the caller's email, if asserted |
| `Apps` | the app-access entitlement: the app or client names this identity may enter |
| `Raw` | any additional coarse claims from the upstream assertion |

## ClaimsMapper

Authors the app-specific `authz.Principal` for an [Identity](#identity): confirms the identity may
enter this app and enriches it with the roles, tenant, and scopes that drive authorization. The
development implementation is [StaticClaimsMapper](#staticclaimsmapper).

```go
type ClaimsMapper interface {
    // MapClaims returns the authored principal for id within this app, or an
    // error (e.g. ErrNotEntitled) when the identity may not enter the app.
    MapClaims(ctx context.Context, id Identity) (authz.Principal, error)
}
```

## Issuer

Mints and signs the app bearer for an authored principal. The concrete implementation is
`oidc.Issuer`, below.

```go
type Issuer interface {
    // Mint returns a signed app bearer encoding p's identity and authored claims.
    Mint(ctx context.Context, p authz.Principal) (bearer string, err error)
}
```

## Authenticator

Verifies a bearer and returns the caller's principal — the seam `server.Config.Authenticator`
accepts. It fails closed: an invalid, expired, or wrong-issuer token returns an error, never a
partial principal. The concrete implementation is `oidc.Authenticator`, below.

```go
type Authenticator interface {
    Authenticate(ctx context.Context, bearer string) (authz.Principal, error)
}

type AuthenticatorFunc func(ctx context.Context, bearer string) (authz.Principal, error)
```

`AuthenticatorFunc` adapts a plain function to an `Authenticator`.

## VerifiedPrincipal

```go
func VerifiedPrincipal(ctx context.Context) (authz.Principal, error)
```

The `grpcauthz.PrincipalFunc` adapter that returns the principal the
[authentication interceptor](#unaryserverinterceptor) stashed on the context, or the zero principal
when none is present. Setting `server.Config.Authenticator` wires this as the `PrincipalFunc` by
default, so the authorizer reads only verified identities.

## UnaryServerInterceptor

```go
func UnaryServerInterceptor(a Authenticator, opts ...InterceptorOption) grpc.UnaryServerInterceptor

const DefaultMetadataKey = "authorization"
```

Returns the authentication interceptor. It reads the bearer from request metadata, verifies it with
`a`, and stashes the verified principal for [VerifiedPrincipal](#verifiedprincipal) to read. It runs
before the authz interceptor and fails closed: a presented-but-invalid bearer is rejected with
`codes.Unauthenticated`. A nil `Authenticator` returns a no-op interceptor. `server.New` installs
this for you when `Config.Authenticator` is set.

| Option | Effect |
|---|---|
| `WithMetadataKey(key string)` | read the bearer from `key` instead of `authorization` |
| `WithRequired()` | reject a request carrying no bearer with `codes.Unauthenticated`; the default passes a bearer-less request through with no principal, so public methods work and non-public methods fail closed at the authorizer |

## StaticClaimsMapper

The development [ClaimsMapper](#claimsmapper): a concurrency-safe, hot-reloadable map from identity
subject to the principal that identity gets in this app. Edit it at runtime (`Set` / `Replace`) to
change an identity's roles without a rebuild. It is not a production claims source.

```go
type StaticClaimsMapper struct {
    AppName string // the app or client name entitlement is checked against
    // unexported fields
}

func NewStaticClaimsMapper(appName string, bySubject map[string]authz.Principal, opts ...StaticClaimsOption) *StaticClaimsMapper

func (m *StaticClaimsMapper) Set(subject string, p authz.Principal)
func (m *StaticClaimsMapper) Replace(bySubject map[string]authz.Principal)
func (m *StaticClaimsMapper) MapClaims(ctx context.Context, id Identity) (authz.Principal, error)
```

`NewStaticClaimsMapper` copies the seed map.

| Method | What it does |
|---|---|
| `Set` | install or replace the authored principal for one subject (hot-reload one identity) |
| `Replace` | swap the entire subject-to-principal map atomically (hot-reload a whole claims file) |
| `MapClaims` | return the authored principal for `id.Subject`, with `Subject` forced to the verified identity; returned slices and maps are copies |

| Option | Effect |
|---|---|
| `WithRequireEntitlement()` | fail with `ErrNotEntitled` when the identity's app-access set does not include `AppName` |
| `WithRequireClaims()` | fail with `ErrNoClaims` when an entitled identity has no mapping entry, instead of returning a subject-only principal |

`ErrNotEntitled` and `ErrNoClaims` are the sentinel errors `MapClaims` returns.

## Package `authn/oidc`

```go
import "github.com/infobloxopen/devedge-sdk/authn/oidc"
```

The JOSE/JWKS-backed implementations of the seams above: an `Issuer` that mints and signs the app
bearer and serves its JWKS, and an `Authenticator` that verifies a bearer against a JWKS. It signs
and verifies with RS256. Keeping the go-jose dependency in this nested module keeps the SDK root
dependency-light.

### Issuer

Mints and signs app bearers for authored principals and serves its public verification keys as a
JWKS. Implements the `authn.Issuer` interface.

```go
func NewIssuer(issuer string, audience []string, opts ...IssuerOption) (*Issuer, error)

func (s *Issuer) Mint(ctx context.Context, p authz.Principal) (string, error)
func (s *Issuer) JWKS() jose.JSONWebKeySet
func (s *Issuer) JWKSHandler() http.Handler
func (s *Issuer) KeySet() jose.JSONWebKeySet
```

`issuer` is the app; `audience` is the app's microservices. With no signing key supplied,
`NewIssuer` generates an ephemeral RSA key at construction.

| Method | What it returns |
|---|---|
| `Mint` | a signed, short-lived app bearer encoding `p` (`iss` = the app, `aud` = the app's microservices) |
| `JWKS` | the public verification keys as a JSON Web Key Set |
| `JWKSHandler` | an `http.Handler` serving the JWKS as `application/json`; mount it at the app's JWKS endpoint via `server.Config.HTTPHandlers` |
| `KeySet` | the public keys for an in-process `Authenticator`, skipping the HTTP round-trip when minter and verifier share a process |

| Option | Default | Effect |
|---|---|---|
| `WithTTL(d)` | 1h | the minted-token lifetime |
| `WithSigningKey(key, kid)` | ephemeral | supply the RSA signing key and its key id, to persist or rotate keys across restarts |
| `WithKeyBits(bits)` | 2048 | the generated RSA key size when no key is supplied |

### Authenticator

Verifies app bearers against a JWKS and maps their claims to a principal. Implements the
`authn.Authenticator` interface and fails closed: a bad signature, wrong issuer or audience, or
expired token returns an error.

```go
type Config struct {
    Keys             KeySource     // where verification keys come from; required
    ExpectedIssuer   string        // the required `iss`; required
    ExpectedAudience string        // the required `aud` member; empty skips the audience check
    Leeway           time.Duration // clock-skew tolerance for exp/nbf; <=0 defaults to 30s
}

func NewAuthenticator(cfg Config) (*Authenticator, error)
func (a *Authenticator) Authenticate(ctx context.Context, bearer string) (authz.Principal, error)
```

`NewAuthenticator` errors when `Keys` or `ExpectedIssuer` is missing (fail-closed configuration).

| Field | Required | Default | Notes |
|---|---|---|---|
| `Keys` | yes | — | a [key source](#key-sources): `StaticKeySet` or `RemoteJWKS` |
| `ExpectedIssuer` | yes | — | the app's identity in two-tier, the IdP's in single-issuer |
| `ExpectedAudience` | no | `""` (skip) | this microservice's audience; skipping is discouraged outside single-issuer bootstrap |
| `Leeway` | no | 30s | clock-skew tolerance when validating `exp` / `nbf` |

### Key sources

Supply the verification keys to an [Authenticator](#authenticator-1). `KeySource` is a sealed
interface; use one of the two provided implementations.

```go
type KeySource interface {
    // unexported method — implemented only by StaticKeySet and RemoteJWKS
}

type StaticKeySet struct{ Keys jose.JSONWebKeySet }

type RemoteJWKS struct {
    URL    string
    Client *http.Client  // nil -> http.DefaultClient
    TTL    time.Duration // <=0 -> 5m
}
```

| Type | Use it for |
|---|---|
| `StaticKeySet` | in-process or test verification against fixed keys (for example an `Issuer`'s `KeySet()`); no network I/O |
| `RemoteJWKS` | fetch and cache a JWKS from a URL (the app's endpoint in two-tier, the IdP's in single-issuer); refreshes after `TTL` and on a key-id miss, and serves the cached set rather than failing on a transient fetch error |

### RelyingParty

The confidential OIDC relying-party client the app identity uses to complete the auth-code + PKCE
exchange with the upstream IdP and validate the returned identity assertion. Point it at the dev
IdP, Okta, Auth0, or Keycloak — microservices are unaffected, since they trust the app's issuer, not
this upstream.

```go
type RelyingPartyConfig struct {
    IssuerURL    string   // the upstream IdP's issuer; required
    ClientID     string   // the app identity's client id; required
    ClientSecret string   // the app identity's client secret
    RedirectURL  string   // the app identity's callback
    Scopes       []string // requested scopes; "openid" is always included
}

func NewRelyingParty(ctx context.Context, cfg RelyingPartyConfig) (*RelyingParty, error)

func (rp *RelyingParty) AuthCodeURL(state, verifier string) string
func (rp *RelyingParty) Exchange(ctx context.Context, code, verifier string) (authn.Identity, error)
```

`NewRelyingParty` performs OIDC discovery against the IdP and errors if discovery fails or a required
field is missing.

| Method | What it does |
|---|---|
| `AuthCodeURL` | build the authorization-endpoint redirect URL, carrying the PKCE S256 challenge derived from `verifier` (make one with `oauth2.GenerateVerifier`); `state` is a random anti-CSRF value the caller round-trips |
| `Exchange` | trade the authorization code for tokens, verify the returned `id_token`, and return the coarse [Identity](#identity); fails closed if the `id_token` is missing or fails verification |

## See also

- [Add authentication](../../how-to/secure/add-authentication/) — the task recipe that wires these seams.
- [server → Config](../server/#config) — the `Authenticator` and `HTTPHandlers` fields.
- [seccheck](../seccheck/) — prove authorization coverage and isolation in CI.
- [Security posture](../../explanation/security-posture/) — the model behind the security surface.
