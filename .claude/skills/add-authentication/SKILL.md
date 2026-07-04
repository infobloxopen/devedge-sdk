---
name: add-authentication
description: Secure a devedge service with verified-token authentication — wire the authn verify seam (authn.Authenticator) so the authorizer sees a token-verified authz.Principal instead of raw headers, run it locally against the dev IdP (devedge-idp) and the dev authz service, and understand the two-tier token model. Use whenever a service must authenticate callers, you are replacing grpcauthz.DevPrincipalFunc for a trusted path, wiring server.Config.Authenticator, minting/verifying app bearers, or standing up local auth for a dogfood. Produces a fail-closed verify→decide pipeline with dev-manipulable identities and grants; production swaps the upstream IdP + authz backend by config, no service-code change.
---

# Add authentication to a devedge service (WS-026 two-tier token model)

## When this fires

A service needs to know *who* is calling from a **verified token**, not from client-supplied
headers. Today a scaffolded service uses `grpcauthz.DevPrincipalFunc()` — which trusts raw
`account-id`/`groups`/`subject` metadata (fine for the earliest local loop, unsafe for anything
real). This skill wires the **verify seam** so the authorizer sees a **token-verified**
`authz.Principal`, and shows how to run the whole thing locally against the shipped **dev security
suite** (dev IdP + dev authz service).

Authentication (this skill) answers *who is the verified caller?* → `authz.Principal`.
Authorization (`authz.Authorizer`, see `add-annotation`) then decides *what may they do?* The two
are separate stages and both run; **authn never authorizes.**

## The model in three sentences

- **The IdP** (`devedge-idp`) authenticates the human and issues a **coarse** identity assertion
  (`id_token`: identity + which apps they may enter — nothing more).
- **The app identity** (Role 2, a confidential relying party — typically the uFE-shell BFF, or a
  small per-app token service) completes the OIDC dance, **authors the rich claims** for this app
  (roles/tenant/scopes via a `ClaimsMapper`), and **mints + signs its own app bearer**.
- **Your microservice** (Role 3) **verifies the app's bearer** against the **app's** JWKS and maps
  the claims to `authz.Principal`. It trusts the **app**, never the IdP directly — so swapping the
  upstream IdP (dev → Okta/Auth0/Keycloak) is a config change at the app identity only.

`TokenTopology` selects `two-tier` (default) or `single-issuer` (the IdP mints the audience-scoped
bearer and the microservice trusts the IdP directly). The verify seam is topology-agnostic — it
verifies a bearer against *whichever* issuer it is told to trust — so single-issuer is just "point
the verifier at the IdP, don't mint."

## Do this — the verify seam (Role 3), the part every service needs

The JOSE/JWKS backend lives in the nested module `github.com/infobloxopen/devedge-sdk/authn/oidc`
(so the SDK root stays dependency-light — you pull go-jose only when you import it):

```go
import (
    "github.com/infobloxopen/devedge-sdk/authn/oidc"
    "github.com/infobloxopen/devedge-sdk/server"
)

authr, err := oidc.NewAuthenticator(oidc.Config{
    // Two-tier: trust the APP's issuer/JWKS. Single-issuer: point these at the IdP.
    Keys:             &oidc.RemoteJWKS{URL: appJWKSURL},   // or oidc.StaticKeySet{Keys: issuer.KeySet()}
    ExpectedIssuer:   appIssuer,                            // the app (two-tier) or the IdP (single-issuer)
    ExpectedAudience: "my-service",                          // this service's audience
})
// then:
srv, _ := server.New(server.Config{
    GRPCAddr:      ":9090",
    Rules:         rules,          // your authz.MethodRule set (see add-annotation)
    Authorizer:    authorizer,     // authz.DevAuthorizer, devsvc.Client, or opaauthz
    Authenticator: authr,          // <-- inserts the verify interceptor BEFORE authz
})
```

What this buys you, automatically:

- The **authentication interceptor runs before authz**. It reads the `authorization: Bearer <jwt>`
  metadata, verifies signature + `iss`/`aud`/`exp`, and stashes the verified principal.
- `PrincipalFunc` **defaults to `authn.VerifiedPrincipal`** when an `Authenticator` is set, so the
  authorizer reads the verified principal — **replacing the unverified `DevPrincipalFunc`** on the
  trusted path. (Don't set both unless you mean to.)
- **Fail-closed:** an invalid/expired/wrong-issuer bearer → `codes.Unauthenticated`, handler never
  runs. No bearer → empty principal → your default-deny authorizer denies any non-public method.
  Spoofed `account-id`/`groups` headers are **ignored** — identity comes only from the verified token.
- `Authenticator: nil` preserves today's behavior (no verify stage).

`servicekit.HostConfig.Authenticator` is the same field for a composed host.

## Run it locally against the dev security suite

The dev IdP and a callable dev authz service ship in `github.com/infobloxopen/devedge-idp`
(passwordless, dummy secrets — **dev only**).

1. **Discover your app as a launchpad tile + client:** add optional `tile` metadata to your route in
   `devedge.yaml`, then `de idp clients sync` writes `idp-clients.json` (client_id = app name, dummy
   secret, redirect URI, tile). `de idp up` routes the IdP at `idp.dev.test`.
2. **Run the IdP:** `IDP_CLIENTS=./idp-clients.json go run ./cmd/idp` (in devedge-idp). Its launchpad
   (`/`) shows a tile per registered app; the picker logs you in passwordlessly (alice/bob/carol).
   Edit `idp-clients.json` and the tiles hot-reload — no restart.
3. **App identity (Role 2)** — the confidential RP that mints. Use `oidc.NewRelyingParty` to do the
   auth-code+PKCE dance with the IdP → a coarse `authn.Identity`, then a `ClaimsMapper` to author the
   app claims, then `oidc.NewIssuer(appIssuer, []string{"my-service"})` to `Mint` the app bearer:
   ```go
   mapper := authn.NewStaticClaimsMapper("my-app", map[string]authz.Principal{
       "alice": {Tenant: "tenant-a", Groups: []string{"admin"}},
   }, authn.WithRequireEntitlement())          // dev claims — edit freely, hot-reloadable
   p, _ := mapper.MapClaims(ctx, identity)      // identity from RelyingParty.Exchange
   bearer, _ := issuer.Mint(ctx, p)             // iss=my-app, aud=my-service
   ```
   Serve the issuer's JWKS with `issuer.JWKSHandler()` (mount it via `server.Config.HTTPHandlers`),
   and point your microservice's `RemoteJWKS.URL` at it.
4. **Dev authz (decide):** run `cmd/devauthz` and set `Authorizer: &devsvc.Client{BaseURL: devauthzURL}`.
   Flip a grant live — edit the grants file (poll-reload) or `PUT /v1/grants` — and the decision
   changes with no rebuild. Swap to production is `Authorizer: opaauthz.New(...)`, same seam.

## Guardrails / non-goals

- The dev IdP is **not** production: passwordless, dummy secrets, built-in identities. Real
  Okta/Auth0/PDS is the private `-internal` overlay (swap the app identity's upstream, no service
  change). The dev authz service is **not** a new policy engine — production authz stays OPA/PARGS.
- **The IdP authors only coarse claims** (identity + app-access). Roles/tenant/scopes are authored at
  the app identity (the `ClaimsMapper`), never on the IdP token. Don't ask the IdP for app roles.
- Keep it **fail-closed**: never set `WithFailOpen`, never trust request metadata for identity once a
  real `Authenticator` is wired.
- The mint/verify claim schema is symmetric (`tenant`/`groups`/`scopes` + passthrough); if you add
  custom claims at mint they arrive in `Principal.Claims` at verify.

## Verify your change

Prove it end to end, not just that it compiles: mint a bearer for a granted identity → call a
non-public method → **OK**; call with no bearer → **PermissionDenied**; call with a garbage/expired
bearer → **Unauthenticated**. The `devedge-idp` repo's `e2e/` tests (`twotier`, `verifydecide`,
`cli_devicegrant`) are the reference shape.
