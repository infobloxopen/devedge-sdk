---
title: Add authentication
weight: 3
---

Authentication makes a service identify a caller from a verified token instead of trusting
client-supplied headers. Wiring it gives you a `authz.Principal` the authorizer can trust — the
token's signature, issuer, audience, and expiry are all checked before any handler runs — so a
spoofed `account-id` or `groups` header can no longer stand in for a real identity. Use this page
when a service must know who is calling on a trusted path, or when you are replacing the
development-only `grpcauthz.DevPrincipalFunc()` (which trusts raw request metadata) with verified
tokens.

Authentication answers "who is the verified caller?" and produces a principal. Authorization —
the [`authz.Authorizer`](../security-check/) — then decides what that principal may do. The two are
separate stages, and both run: authentication never authorizes.

## The two-tier model

Three parties cooperate to turn a login into a verified principal:

- **The identity provider (IdP)** authenticates the person and issues a coarse identity assertion
  (an `id_token`): who they are and which apps they may enter — nothing more.
- **The app identity** is a confidential OIDC relying party — typically a micro-frontend shell's
  backend or a small per-app token service. It completes the OIDC exchange with the IdP, authors
  this app's claims (roles, tenant, scopes) through a `ClaimsMapper`, and mints and signs its own
  app bearer.
- **Your microservice** verifies the app's bearer against the app's JWKS and maps the claims to a
  principal. It trusts the app, not the IdP directly, so swapping the upstream IdP (development to
  Okta, Auth0, or Keycloak) is a configuration change at the app identity alone — no microservice
  change.

The `TokenTopology` selects `two-tier` (the default, above) or `single-issuer` (the IdP mints the
audience-scoped bearer and the microservice trusts the IdP directly). The verify seam is the same
either way — it verifies a bearer against whichever issuer it is told to trust — so single-issuer
is "point the verifier at the IdP and skip minting." See [Single-issuer](#single-issuer).

## Wire the verify seam

The verify seam is the part every service needs. Its JOSE/JWKS backend lives in the nested module
`github.com/infobloxopen/devedge-sdk/authn/oidc`, so the go-jose dependency enters your build only
when you import it — the SDK root stays dependency-light.

Construct an authenticator and pass it as `server.Config.Authenticator`:

```go
import (
    "github.com/infobloxopen/devedge-sdk/authn/oidc"
    "github.com/infobloxopen/devedge-sdk/server"
)

authr, err := oidc.NewAuthenticator(oidc.Config{
    Keys:             &oidc.RemoteJWKS{URL: appJWKSURL}, // the app's JWKS endpoint
    ExpectedIssuer:   appIssuer,                          // the app identity (two-tier)
    ExpectedAudience: "my-service",                       // this service's audience
})
if err != nil {
    log.Fatal(err)
}

srv, err := server.New(server.Config{
    GRPCAddr:      ":9090",
    Authorizer:    authorizer, // authz.NewDevAuthorizer, devsvc.Client, or opaauthz
    Authenticator: authr,      // inserts the verify interceptor before authz
})
```

For a composed host, set the same field on
[`servicekit.HostConfig.Authenticator`](../../../reference/servicekit/#hostconfig).

Setting `Authenticator` wires three behaviors:

- **Verify before authorize.** The authentication interceptor runs before authz: it reads the
  `authorization: Bearer <jwt>` metadata, verifies the signature and the `iss`, `aud`, and `exp`
  claims, and stashes the verified principal on the request context.
- **Verified principal by default.** `PrincipalFunc` defaults to `authn.VerifiedPrincipal`, so the
  authorizer reads the verified principal, replacing the unverified `grpcauthz.DevPrincipalFunc()`
  on the trusted path.
- **Fail closed.** An invalid, expired, or wrong-issuer bearer returns `codes.Unauthenticated` and
  the handler never runs. A request with no bearer yields an empty principal, which a default-deny
  authorizer denies for any non-public method.

`Authenticator: nil` keeps the prior behavior — no verification stage, identity from `PrincipalFunc`
alone.

{{< callout type="warning" >}}
**Once you wire `Authenticator`, leave `PrincipalFunc` unset.** It defaults to
`authn.VerifiedPrincipal`, which reads the token-verified principal. Setting it back to
`grpcauthz.DevPrincipalFunc()` reintroduces header-trusting identity and undoes the verification —
a caller could then spoof `account-id` and `groups`.
{{< /callout >}}

## Mint the app bearer

In two-tier, the app identity produces the bearer your service verifies. It completes the OIDC
exchange, authors the claims, then mints:

```go
import (
    "github.com/infobloxopen/devedge-sdk/authn"
    "github.com/infobloxopen/devedge-sdk/authn/oidc"
    "github.com/infobloxopen/devedge-sdk/authz"
    "golang.org/x/oauth2"
)

// 1. Complete the auth-code + PKCE exchange with the IdP.
rp, err := oidc.NewRelyingParty(ctx, oidc.RelyingPartyConfig{
    IssuerURL:    idpIssuer, // the dev IdP now; Okta/Auth0/Keycloak in production
    ClientID:     "my-app",
    ClientSecret: clientSecret,
    RedirectURL:  "https://my-app.dev.test/callback",
    Scopes:       []string{"profile", "email"},
})
verifier := oauth2.GenerateVerifier()
// Redirect the caller to rp.AuthCodeURL(state, verifier); on the callback:
identity, err := rp.Exchange(ctx, code, verifier) // coarse authn.Identity

// 2. Author this app's claims for the identity.
mapper := authn.NewStaticClaimsMapper("my-app", map[string]authz.Principal{
    "alice": {Tenant: "tenant-a", Groups: []string{"admin"}},
}, authn.WithRequireEntitlement())
principal, err := mapper.MapClaims(ctx, identity)

// 3. Mint + sign the app bearer for the authored principal.
issuer, err := oidc.NewIssuer("my-app", []string{"my-service"})
bearer, err := issuer.Mint(ctx, principal) // iss=my-app, aud=my-service
```

`NewStaticClaimsMapper` is the development claims source: edit the subject-to-principal map at
runtime (`Set` / `Replace`) to change an identity's roles without a rebuild. `WithRequireEntitlement`
rejects an identity whose app-access set does not include the app name. Production binds a real
claims source (a directory or entitlements service) behind the same `ClaimsMapper` seam.

Serve the app identity's JWKS so the microservice can fetch its verification keys, and point the
microservice's `RemoteJWKS.URL` at it:

```go
srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    HTTPAddr: ":8080",
    HTTPHandlers: []server.HTTPHandler{
        {Pattern: "/keys", Handler: issuer.JWKSHandler()},
    },
    // ...
})
```

When the minter and verifier share a process (a test, or a single-binary app identity), skip the
HTTP round-trip and verify against the issuer's in-process keys with
`oidc.StaticKeySet{Keys: issuer.KeySet()}`.

## Adapt the generated tests

`devedge-sdk new service` generates `TestSmoke` and `TestSecurity` that authenticate by sending
`account-id`, `subject`, and `groups` metadata headers, which `grpcauthz.DevPrincipalFunc()` turns
into the principal. Once you set `Authenticator`, `PrincipalFunc` defaults to
`authn.VerifiedPrincipal`, so those headers no longer confer identity — the principal comes only
from a verified bearer. The authorized calls in those tests then fail: their caller carries no
groups, so the `group:admin` grant no longer matches.

Give the tests a bearer to present. Stand up an in-process issuer, point the host's `Authenticator`
at its keys with `oidc.StaticKeySet`, and mint a bearer per call:

```go
import (
    "github.com/infobloxopen/devedge-sdk/authn"
    "github.com/infobloxopen/devedge-sdk/authn/oidc"
    "github.com/infobloxopen/devedge-sdk/authz"
    "google.golang.org/grpc/metadata"
)

const (
    testIssuer   = "https://test.local"
    testAudience = "my-service"
)

// testAuth returns an issuer the test mints with and an authenticator that trusts
// it. Boot the host under test with authr as its Authenticator.
func testAuth(t *testing.T) (*oidc.Issuer, authn.Authenticator) {
    t.Helper()
    iss, err := oidc.NewIssuer(testIssuer, []string{testAudience})
    if err != nil {
        t.Fatalf("NewIssuer: %v", err)
    }
    authr, err := oidc.NewAuthenticator(oidc.Config{
        Keys:             oidc.StaticKeySet{Keys: iss.KeySet()},
        ExpectedIssuer:   testIssuer,
        ExpectedAudience: testAudience,
    })
    if err != nil {
        t.Fatalf("NewAuthenticator: %v", err)
    }
    return iss, authr
}

// bearerCtx mints an app bearer for p and attaches it as `authorization: Bearer`,
// the only identity the wired Authenticator now trusts.
func bearerCtx(t *testing.T, ctx context.Context, iss *oidc.Issuer, p authz.Principal) context.Context {
    t.Helper()
    bearer, err := iss.Mint(ctx, p)
    if err != nil {
        t.Fatalf("Mint: %v", err)
    }
    return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
}
```

Then replace the header-based identity in the generated tests. The **tenant now rides on the
verified principal**, not the `account-id` header — the header is a routing/cells hint, never the
isolation authority (the storage fence scopes off the principal and fails closed when it carries no
tenant). Put the tenant in the principal the bearer authors:

```go
// before: identity AND tenant from headers
mdCtx := metadata.NewOutgoingContext(context.Background(),
    metadata.Pairs("account-id", "tenant1", "groups", "admin"))

// after: the bearer carries the verified principal, INCLUDING its tenant
mdCtx := bearerCtx(t, context.Background(), iss,
    authz.Principal{Subject: "alice", Tenant: "tenant1", Groups: []string{"admin"}})
```

In `TestSecurity`, change the `asAdmin` helper the same way — wrap the seccheck-supplied context
with a bearer for an admin principal whose `Tenant` is set, instead of relying on the `account-id`
header. Your `ClaimsMapper` must populate `Principal.Tenant` (as in the mapper above) so every
verified principal carries the tenant the fence enforces.

{{< callout type="info" >}}
**The deny cases need no change.** `TestSmoke_DeniedForUnknownPrincipal` and the
`AssertUnknownPrincipalDenied` helper present no bearer, so the principal is empty and the
default-deny authorizer still denies every non-public method. The fail-closed path does not depend
on headers.
{{< /callout >}}

## Run it locally against the dev security suite

The `github.com/infobloxopen/devedge-idp` project ships a development-only identity provider and a
callable dev authz service (passwordless, dummy secrets — development only). Together they let you
exercise the full verify-then-decide pipeline without standing up a real IdP or policy engine.

**Prerequisites:** a service wired with `Authenticator` (above) and a checkout of `devedge-idp`.

1. Register your app as an IdP client. The IdP reads its clients from an `idp-clients.yaml` file
   (the `IDP_CLIENTS` path in step 2). Produce that file one of two ways:

   - **With the daemon running** — run `de start` so the daemon discovers your registered app, then
     `de idp clients sync` to write `idp-clients.yaml` (the client ID is your app name, with a dummy
     secret and redirect URI). Add optional `tile` metadata to your route in `devedge.yaml` first to
     set the launchpad tile. `de idp up` routes the IdP at `idp.dev.test`.
   - **By hand** — for a local-only service with no daemon or `devedge.yaml`, write the file
     yourself. `de idp clients sync` fails without a running daemon, so a hand-written file is the
     fallback. Each entry needs a `client_id`, `client_secret`, `redirect_uris`, and a `tile`:

     ```yaml
     - client_id: my-app
       client_secret: dev-secret-my-app
       redirect_uris: [https://my-app.dev.test/callback]
       tile:
         name: My App
         description: ""
         icon_url: ""
         launch_url: https://my-app.dev.test/
     ```

   The file augments the seeded client and hot-reloads on edit. A `.json` file with the same keys is
   also accepted (YAML is a superset of JSON); its exact shape is in the
   [devedge-idp README](https://github.com/infobloxopen/devedge-idp#hot-reloadable-clients-file).
2. Run the IdP: `IDP_CLIENTS=./idp-clients.yaml go run ./cmd/idp` from the `devedge-idp` checkout.
   Its launchpad at `/` shows a tile per registered app; the identity picker logs you in
   passwordlessly as `alice`, `bob`, or `carol`. Editing `idp-clients.yaml` hot-reloads the tiles —
   no restart.
3. Run the dev authz service: `go run ./cmd/devauthz`. Set your service's authorizer to
   `Authorizer: &devsvc.Client{BaseURL: devauthzURL}` (the `github.com/infobloxopen/devedge-sdk/authz/devsvc`
   client). Its grants live in a `grants.yaml` file — a YAML list of grants
   (`tenant`/`subjects`/`verbs`/`resource`, lowercase keys). Change a decision live by editing that
   file (polled and reloaded) or with `PUT /v1/grants` — no rebuild:

   ```yaml
   - tenant: tenant-a
     subjects: [group:admin]
     verbs: ["*"]
     resource: "*"
   ```

Moving to production swaps the upstream IdP at the app identity and swaps the authorizer to
`opaauthz.New(...)`, both behind the seams above — the service code is unchanged.

## Single-issuer

When you do not run a separate app identity, point the verifier at the IdP directly and trust the
IdP's `id_token`. Only the authenticator config changes:

```go
authr, err := oidc.NewAuthenticator(oidc.Config{
    Keys:             &oidc.RemoteJWKS{URL: idpJWKSURL},
    ExpectedIssuer:   idpIssuer,
    ExpectedAudience: clientID, // an id_token carries aud = client_id
})
```

Single-issuer tokens carry only coarse claims, so the authorizer grants the subject directly rather
than mapped roles. Everything else — the interceptor, the fail-closed behavior, the authorizer —
is identical to two-tier.

## Verify

Prove the pipeline end to end, not just that it compiles. Mint a bearer for a granted identity, then
drive three requests against a non-public method:

| Request | Expected result |
|---|---|
| Valid bearer for a granted identity | the call succeeds |
| No bearer | `codes.PermissionDenied` — empty principal, default-deny |
| A garbage or expired bearer | `codes.Unauthenticated` — rejected at verify, before authz |

Confirm too that a spoofed `account-id` header cannot change the tenant a request is scoped to: the
storage fence anchors on the **verified principal's** `Tenant`, and the header is only a routing
hint. With a valid bearer for `tenant1`, appending `account-id: tenant2` still confines every
query to `tenant1`; presenting no verified tenant fails closed rather than falling back to the
header. The runnable reference for the whole chain is the `devedge-idp` project's `e2e/` tests —
`twotier_test.go`, `cli_devicegrant_test.go`, and `verifydecide_test.go`.

## See also

- [Security Check](../security-check/) — prove authz completeness, unknown-principal denial, and
  tenant isolation in CI.
- [server → Config](../../../reference/server/#config) — the `Authenticator` and `HTTPHandlers`
  fields.
- [servicekit → HostConfig](../../../reference/servicekit/#hostconfig) — the same fields on a
  composed host.
- [authn / authn/oidc](../../../reference/authn/) — the reference for every seam and type wired here.
- [Security posture](../../../explanation/security-posture/) — the model behind the security surface.
- The `add-authentication` skill walks an agent through wiring the seam step by step.
