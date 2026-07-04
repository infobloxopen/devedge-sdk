---
title: Call another service
weight: 4
---

When one service calls another, it must present a token the callee will accept. This page shows how
to attach that token automatically — passing the caller's own bearer through within a trust domain,
and exchanging it for a scoped token when crossing into another. Use it when a service makes
outbound gRPC or REST calls on behalf of an authenticated caller.

## The trust boundary is the audience

A verified bearer carries an `aud` (audience) claim naming who may accept it. That audience is the
trust boundary:

- **Same audience — pass the bearer through.** All of an app's own services share one audience, so
  the caller's inbound bearer is already a valid token for any of them. Forwarding it is correct,
  not a leak: the callee verifies the same audience the caller was issued for.
- **Different audience — exchange the bearer.** Sending your inbound bearer to a service in another
  trust domain (another app, or a shared platform service) is a confused-deputy risk: that service
  was never the intended audience. Instead, obtain a token scoped to the target's audience via
  [RFC 8693](https://www.rfc-editor.org/rfc/rfc8693) token exchange. The exchanged token carries an
  `act` (actor) claim naming your service, so the delegation is auditable.

The [`authn.TokenSource`](../../../reference/) seam makes that decision for you: given a target
audience, it returns the inbound bearer unchanged when the target is already one of the caller's
audiences, or an exchanged token otherwise. It is cached, so a cross-domain call costs one exchange,
not one per request. And it is fail-closed: it never sends the raw inbound token to a different
audience.

## Accept a shared audience

A single audience can cover a whole app's services. Configure each service's verifier to accept the
set of audiences it belongs to — a token validates when any of its `aud` values matches any accepted
audience:

```go
authr, err := oidc.NewAuthenticator(oidc.Config{
    Keys:              keys,
    ExpectedIssuer:    appIssuer,
    ExpectedAudiences: []string{"orders-app"}, // one audience for all of orders-app's services
})
```

Use `ExpectedAudiences` for a service that spans trust domains (it accepts any audience in the set).
`ExpectedAudience` remains a convenience alias for the single-audience case; it is appended to the
set. At least one audience is required unless you set `AllowAnyAudience` — see
[Add authentication](../add-authentication/).

## Attach a token to outbound gRPC calls

Wire the outbound interceptor with a `TokenSource` and an `AudienceResolver`. The resolver maps a
target service to the audience its tokens must carry:

```go
import (
    "github.com/infobloxopen/devedge-sdk/authn"
    "github.com/infobloxopen/devedge-sdk/authn/oidc"
    "google.golang.org/grpc"
)

// The delegation TokenSource: it exchanges the inbound bearer when the target
// audience differs from the caller's. Its deps (go-jose, HTTP) live in the nested
// authn/oidc module, so the SDK root stays dependency-light.
ts, err := oidc.NewExchanger(oidc.ExchangeConfig{
    TokenEndpoint: stsTokenURL,  // the STS / IdP token endpoint (RFC 8693)
    ClientID:      "orders-svc", // this service's own client identity
    ClientSecret:  clientSecret,
})

// The audience map: gRPC service name -> the audience its tokens must carry.
audiences := authn.StaticAudiences{
    "orders.v1.Orders":   "orders-app",   // same audience -> passthrough
    "billing.v1.Billing": "billing-app",  // different audience -> exchange
}

conn, err := grpc.NewClient(
    target,
    grpc.WithChainUnaryInterceptor(authn.UnaryClientInterceptor(ts, audiences)),
    // ... transport credentials
)
```

The interceptor resolves the target audience from the invoked method's service (the
`/pkg.Service/Method` prefix), obtains a token for that audience from the `TokenSource`, and sets
`authorization: Bearer <token>` on the outgoing metadata. A method whose service has no mapped
audience is rejected with `codes.FailedPrecondition` before the call is made — so the raw inbound
token is never sent cross-domain by accident.

## Attach a token to outbound REST calls

For a REST client that talks to one target, wrap its transport. The round tripper obtains a token
for the target's audience per request (caching happens in the `TokenSource`):

```go
client := &http.Client{
    Transport: authn.NewRoundTripper(http.DefaultTransport, ts, "billing-app"),
}
resp, err := client.Get("https://billing.dev.test/v1/invoices/123")
```

A transport error obtaining the token fails the request; the round tripper never sends it without
the intended bearer, and it does not mutate the caller's original request.

## How the exchange works

`Exchanger.TokenFor` is a delegation (on-behalf-of) source. It reads the caller's raw inbound bearer
from the context — stashed by the authentication interceptor via `middleware.WithInboundBearer` — and
uses it as the exchange `subject_token`. For a target audience the caller does not already hold, it
POSTs the RFC 8693 grant to the token endpoint:

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the caller's inbound bearer>
subject_token_type=urn:ietf:params:oauth:token-type:jwt
requested_token_type=urn:ietf:params:oauth:token-type:access_token
audience=<the target audience>
```

It caches the result keyed by `(subject, target-audience)` with a TTL of the token's lifetime minus
a safety skew (default 30s), so subsequent calls for the same caller and target reuse it. It fails
closed everywhere: no inbound bearer, or any error from the token endpoint, returns an error — never
a fallback to sending the raw inbound token.

The example above authenticates your service to the STS with `client_secret_basic` (`ClientID` +
`ClientSecret`). Production deployments typically use a signed client assertion
(`client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer`) instead; the
`Exchanger` carries whatever `act` claim the STS returns and does not fabricate one client-side.

{{< callout type="info" >}}
Maintaining the audience map by hand does not scale. `AudienceResolver` is a seam: today
`StaticAudiences` reads it from config, and a future catalog-backed resolver will derive the target
audience from the service registry, so call sites need no per-target maintenance.
{{< /callout >}}
