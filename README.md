# devedge-sdk

A clean, pluggable SDK for building Infoblox services. It is the runtime
companion to [devedge](https://github.com/infobloxopen/devedge) (the local dev
edge / deployment substrate): devedge is **dev- and deploy-time** tooling;
`devedge-sdk` is the **runtime library** that production services import.

> **Status: early.** APIs will change. The SDK currently provides the
> authorization seam and a minimal persistence seam, each with a
> development-suitable implementation and a clean extension point.

## Principles

- **Clean and dependency-light.** The core packages depend only on the standard
  library; transport/engine integrations live in clearly separated subpackages.
- **Pluggable, with a dev-suitable default.** Every seam ships an implementation
  good enough for local development, and is swappable for a production backend
  **without changing service code**.
- **No internal coupling.** The SDK is engine-neutral: **no policy-engine
  dependency** (e.g. OPA), no ORM, and no internal policy-model types. Those belong
  in adapters built *on* the SDK, not in it.
- **Fail closed.** Authorization denies by default; an undeclared method is denied.
- **Multi-language ready.** Go lives at the module root today. Other languages
  will arrive either as sibling directories here or as language-specific repos
  (`devedge-sdk-<lang>`); the *contracts* (the authz model, the verb vocabulary)
  are language-neutral by design.

## Packages

| Package | What it provides |
|---|---|
| [`authz`](./authz) | The engine-neutral authorization model: `Principal`, `Resource`, `Verb`, `AccessRequest`, `Decision`, and the pluggable `Authorizer` (PDP). Ships `DevAuthorizer` (in-process, default-deny, grant-driven) for development. |
| [`authz/grpcauthz`](./authz/grpcauthz) | A fail-closed gRPC server interceptor that enforces an `Authorizer`. Constructor + options are **rough-compatible** with `infobloxopen/atlas-authz-middleware/grpc_opa` (see [COMPAT.md](./COMPAT.md)). |
| [`authz/catalog`](./authz/catalog) | Turns declared `authz.MethodRule`s into the **permission catalog** (per resource: supported verbs, the endpoints implementing each, and the `View`/`Manage` intent groups) — the code-backed source of truth the API enforces, a portal renders, and an engine/policy generator consumes. |
| [`authz/authzpb`](./authz/authzpb) + [`cmd/protoc-gen-devedge-authz`](./cmd/protoc-gen-devedge-authz) | Two ways to turn the proto `(infoblox.authz.v1.rule)` annotation into `[]authz.MethodRule`: **reflection** over descriptors at runtime (`authzpb`, no generated file) or a **codegen plugin** that emits a `<Service>AuthzRules` table (`protoc-gen-devedge-authz`). Both produce identical rules. |
| [`persistence`](./persistence) | Connection + storage helpers that **do not impose an ORM**: an optional engine-neutral `Repository[T,K]`, an in-memory dev implementation, and a `DSN` abstraction supporting devedge's indirect hotload convention. The persistence *shape* (proto→GORM, [ent](https://entgo.io), sqlc, …) is a pluggable per-service choice — see [persistence/SHAPES.md](./persistence/SHAPES.md). |

## Quickstart (gRPC authz)

```go
import (
    "github.com/infobloxopen/devedge-sdk/authz"
    "github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
)

intc := grpcauthz.UnaryServerInterceptor("dns",
    // Dev decision point — swap for an OPA/Cedar/remote Authorizer in prod.
    grpcauthz.WithAuthorizer(authz.NewDevAuthorizer(
        authz.Grant{Tenant: "t1", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*"},
    )),
    grpcauthz.WithPrincipalFunc(principalFromJWT),       // your auth layer puts it on ctx
    grpcauthz.WithMethodRule("/dns.v1.ZoneService/GetZone", authz.Get, "zone"),
    grpcauthz.WithPublicMethod("/grpc.health.v1.Health/Check"),
)

// Fail closed at boot: refuse to start if any served method is undeclared.
if err := grpcauthz.AssertMethodsDeclared(allServedMethods, opts...); err != nil {
    log.Fatal(err)
}

srv := grpc.NewServer(grpc.UnaryInterceptor(intc))
```

## Declare once, generate the rest

A method's authz requirement is a single `authz.MethodRule` (`{Method, Verb,
Resource}`), and the same set feeds **both** enforcement and the catalog:

```go
rules := []authz.MethodRule{
    {Method: "/dns.v1.ZoneService/GetZone",    Verb: authz.Get,    Resource: "zone"},
    {Method: "/dns.v1.ZoneService/CreateZone", Verb: authz.Create, Resource: "zone"},
}

intc := grpcauthz.UnaryServerInterceptor("dns", grpcauthz.WithRules(rules...), /* + authorizer, principal */)
cat  := catalog.Build("dns", rules)   // permission catalog: verbs, endpoints, View/Manage groups
out, _ := cat.JSON()
```

Or **declare them as proto annotations** and let the SDK produce the `[]MethodRule`
for you ([`proto/infoblox/authz/v1/authz.proto`](./proto/infoblox/authz/v1/authz.proto)):

```proto
rpc GetZone(GetZoneRequest) returns (Zone) {
  option (infoblox.authz.v1.rule) = { verb: "get", resource: "zone:{zone_id}" };
}
```

Two equivalent ways to consume that annotation — pick per service:

- **Reflection** — `authzpb.RulesFromGlobal()` reads it off the linked descriptors
  at startup. No generated file.
- **Codegen** — `protoc-gen-devedge-authz` (run by `make generate`) emits a
  `<Service>AuthzRules` table next to the `.pb.go`; pass it to `WithRules(...)`.

What lives **outside** this clean SDK: an internal policy-CRD generator and the
OPA-backed `Authorizer` that consume the catalog/rules — those are engine/policy
-specific and belong on the internal side.

## Swapping the decision point

`WithAuthorizer` takes any `authz.Authorizer`. To target a production engine,
implement the one-method interface — e.g. an OPA-backed authorizer that calls a
sidecar, a Cedar/OpenFGA client, or a remote PDP — and pass it in. Nothing else
in the service changes.

## License

Apache-2.0. See [LICENSE](./LICENSE).
