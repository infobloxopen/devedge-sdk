# Compatibility with `atlas-authz-middleware`

The established Infoblox gRPC authz middleware is the public
`github.com/infobloxopen/atlas-authz-middleware/grpc_opa`. `devedge-sdk`'s
`grpcauthz` is **rough-compatible** with it: the same constructor shape and
functional-option pattern, so adopting the SDK is a small change, not a rewrite.
The SDK deliberately **does not import** that middleware or any policy engine — it
stays clean and engine-neutral.

## What matches

| Concern | atlas-authz-middleware | devedge-sdk `grpcauthz` |
|---|---|---|
| Unary constructor | `UnaryServerInterceptor(application string, opts ...Option) grpc.UnaryServerInterceptor` | identical signature |
| Stream constructor | `StreamServerInterceptor(application string, opts ...Option)` | identical signature |
| Options pattern | functional `Option`s (`WithAuthorizer`, `WithAddress`, …) | functional `Option`s (`WithAuthorizer`, `WithMethodRule`, `WithPublicMethod`, …) |
| Pluggable decision | `WithAuthorizer(...Authorizer)` | `WithAuthorizer(authz.Authorizer)` |
| Fail closed | interceptor errors when not allowed | interceptor returns `codes.PermissionDenied` when not allowed / undeclared |

## What's cleaner (the intentional differences)

- **Decision interface.** atlas's `Authorizer` is OPA-shaped:
  `Evaluate(ctx, fullMethod string, grpcReq any, opaEvaluator OpaEvaluator) (bool, context.Context, error)`
  plus `OpaQuery(...)`. The SDK's is engine-neutral and one method:
  `Authorize(ctx, authz.AccessRequest) (authz.Decision, error)`.
- **Declared requirements.** atlas leaves the required permission implicit in the
  policy (keyed off the runtime method string). The SDK makes the requirement
  **explicit per method** (`WithMethodRule` / proto annotations) and adds a
  **boot-time completeness gate** (`AssertMethodsDeclared`) — an undeclared
  method is denied, and the service can refuse to start.
- **No engine in the core.** Engine specifics (the OPA evaluator, its decision
  input, obligations wire format, entitlements) are *not* in the SDK. They live in
  an adapter on the consuming side.

## Adapting an OPA-backed authorizer

Wrap an OPA-backed decision behind the SDK's `Authorizer` in your service (or in a
separate internal adapter package — not in this clean SDK):

```go
// opaAuthorizer adapts an OPA-backed decision to authz.Authorizer.
type opaAuthorizer struct{ /* OPA client */ }

func (o opaAuthorizer) Authorize(ctx context.Context, req authz.AccessRequest) (authz.Decision, error) {
    // Map req.Verb / req.Resource / req.Principal into the engine's decision input,
    // call it, and translate the result to {allow, obligations}.
    allow, obligations, err := callOPA(ctx, req)
    return authz.Decision{Allow: allow, Obligations: obligations}, err
}

intc := grpcauthz.UnaryServerInterceptor("dns", grpcauthz.WithAuthorizer(opaAuthorizer{...}))
```

This keeps the policy engine and any internal policy data where they belong (the
internal side) while services consume one clean, swappable interface.
