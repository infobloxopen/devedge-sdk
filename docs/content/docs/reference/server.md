---
title: server
weight: 1
---

```go
import "github.com/infobloxopen/devedge-sdk/server"
```

Package `server` provides a batteries-included gRPC server builder. It assembles the framework
interceptor chain (request-ID, error mapping, tenant-ID, fail-closed authz, field-mask
validation, ETag preconditions) and, optionally, an HTTP/JSON gateway in front of the gRPC
endpoint.

## Config

```go
type Config struct {
    // GRPCAddr is the TCP address to listen on (e.g. ":9090" or ":0"). Required.
    GRPCAddr string
    // HTTPAddr is the optional gateway address (e.g. ":8080"). Empty disables the HTTP gateway.
    HTTPAddr string
    // Rules are the declared authz rules; they feed both grpcauthz (enforcement)
    // and the field-mask interceptor (verb lookup).
    Rules []authz.MethodRule
    // Authorizer is the pluggable decision point.
    // Defaults to authz.NewDevAuthorizer() (default-deny) if nil.
    Authorizer authz.Authorizer
    // PrincipalFunc derives the authz.Principal from each request (threaded to
    // grpcauthz.WithPrincipalFunc). nil → empty principal → every non-public
    // call is denied. Use grpcauthz.DevPrincipalFunc() in dev; a verified-token
    // function in production.
    PrincipalFunc grpcauthz.PrincipalFunc
    // Interceptors are additional unary interceptors appended after the framework chain.
    Interceptors []grpc.UnaryServerInterceptor
    // DeduplicationStore backs the idempotency interceptor. Defaults to an
    // in-memory store (10-minute TTL) when nil.
    DeduplicationStore middleware.DeduplicationStore
    // LROStore backs long-running operations (AIP-151). Defaults to an in-memory
    // store (1-hour TTL) when nil.
    LROStore lro.Store
}
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `GRPCAddr` | **yes** | — | `:0` binds an ephemeral port; read it back with `GRPCAddr()` after `Serve` |
| `HTTPAddr` | no | `""` (disabled) | enables the grpc-gateway HTTP/JSON proxy |
| `Rules` | no* | `nil` | feeds **both** authz enforcement and field-mask verb lookup; *required in practice or every non-public call is denied. For a proto with multiple services combine all `<Service>AuthzRules` — see below. |
| `Authorizer` | no | `authz.NewDevAuthorizer()` (no grants → deny all) | swap for OPA/Cedar/remote PDP |
| `PrincipalFunc` | no* | `nil` → empty principal | how the caller's identity is derived; *without it **no grant can match**, so every non-public call is denied. Use `grpcauthz.DevPrincipalFunc()` in dev, a verified-token function in prod |
| `Interceptors` | no | `nil` | appended **after** the framework chain |
| `DeduplicationStore` | no | in-memory (10m TTL) | idempotency replay store for `DeduplicateUnary` |
| `LROStore` | no | in-memory (1h TTL) | long-running operation store (AIP-151) |

`DefaultGRPCAddr` is `":9090"`.

## New

```go
func New(cfg Config) (*Server, error)
```

Validates `cfg` and constructs a `*Server`. Returns an error if `GRPCAddr` is empty. When
`Authorizer` is nil it defaults to a **default-deny** dev authorizer (no grants), so the server is
fail-closed out of the box.

`New` builds this unary interceptor chain (outermost first):

```go
chain := []grpc.UnaryServerInterceptor{
    middleware.RequestIDUnary(),
    middleware.ErrorMapperUnary(),
    middleware.TenantIDUnary(),
    grpcauthz.UnaryServerInterceptor("sdk", authzOpts...), // fail-closed; authzOpts adds WithPrincipalFunc when Config.PrincipalFunc is set
    middleware.FieldMaskUnary(verbMap),                    // verbMap built from cfg.Rules
    etag.PreconditionUnary(),
    middleware.ReadMaskUnary(),                            // AIP-157 response field selection
    middleware.ValidateOnlyUnary(),                        // short-circuit validate_only requests
    middleware.DeduplicateUnary(cfg.DeduplicationStore),   // idempotency replay
}
chain = append(chain, cfg.Interceptors...)
```

## Server methods

```go
func (s *Server) Serve(ctx context.Context) error
```
Starts the gRPC server (and the HTTP gateway when configured) and **blocks until `ctx` is
cancelled**, then shuts both down gracefully (bounded by a 5s timeout). Returns the first fatal
error from either server, or nil on clean shutdown.

```go
func (s *Server) GRPCServer() *grpc.Server
```
The underlying `*grpc.Server`, so you can register your service implementations on it.

```go
func (s *Server) RegisterGateway(fn func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error)
```
Records a grpc-gateway registration function, invoked against the gateway mux and the in-process
gRPC connection when `Serve` starts. No-op unless an HTTP gateway is configured. This is the
lower-level alternative to the generated `Register<Service>` helper (see *codegen*); use one or the
other, not both — the generated helper already calls `RegisterGateway` for you, so combining them
registers the gateway twice.

```go
func (s *Server) GatewayMux() *runtime.ServeMux // nil when no HTTP gateway
func (s *Server) Rules() []authz.MethodRule
func (s *Server) GRPCAddr() string // actual bound addr after Serve (useful when GRPCAddr was ":0")
func (s *Server) HTTPAddr() string // actual bound gateway addr after Serve; "" when no gateway
```

### Combining rules for a multi-service proto

`protoc-gen-devedge-authz` emits one `<Service>AuthzRules []authz.MethodRule` per `service`
declaration. A proto that exposes multiple services — for example an owner `FooService` and a
read-projection `FooSummaryService` — requires **all** tables merged into a single slice:

```go
import "slices" // Go 1.22+

srv, err := server.New(server.Config{
    Rules: slices.Concat(
        myv1.FooServiceAuthzRules,
        myv1.FooSummaryServiceAuthzRules,
    ),
    // ...
})
```

Without combining, any service whose rules are omitted fails the boot-time completeness gate
(`AssertMethodsDeclared`), which the generated `Register<Service>` helper runs at startup.
See [codegen → protoc-gen-devedge-authz](../codegen/#protoc-gen-devedge-authz) for the full
explanation and a stdlib `append` alternative.

## Complete `main.go`

```go {filename="main.go"}
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/infobloxopen/devedge-sdk/authz"
    "github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
    "github.com/infobloxopen/devedge-sdk/server"

    "github.com/example/widget/widgetv1"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    srv, err := server.New(server.Config{
        GRPCAddr: ":9090",
        HTTPAddr: ":8080",
        Rules:    widgetv1.WidgetServiceAuthzRules,
        Authorizer: authz.NewDevAuthorizer(authz.Grant{
            Tenant:   "t1",
            Subjects: []string{"group:admin"},
            Verbs:    []authz.Verb{"*"},
            Resource: "*",
        }),
        // Dev: derive the principal from request metadata so the grant above can
        // match (account-id → Tenant, groups → group:<name>). In production use a
        // PrincipalFunc backed by a verified token, never raw client headers.
        PrincipalFunc: grpcauthz.DevPrincipalFunc(),
    })
    if err != nil {
        log.Fatal(err)
    }

    // Canonical wiring (generated by protoc-gen-svc): one call runs the boot-time
    // authz completeness gate, registers the gRPC implementation, AND registers the
    // HTTP/JSON gateway handler. Prefer this over calling RegisterWidgetServiceServer
    // + RegisterGateway by hand — doing both double-registers the gateway.
    if err := widgetv1.RegisterWidgetService(srv, newWidgetServer()); err != nil {
        log.Fatal(err)
    }

    log.Printf("gRPC %s  HTTP %s", srv.GRPCAddr(), srv.HTTPAddr())
    if err := srv.Serve(ctx); err != nil {
        log.Fatal(err)
    }
    log.Println("shut down cleanly")
}
```

## Make an authorized request

`server.New` installs a gateway header matcher that forwards the identity headers `account-id`,
`subject`, and `groups` into gRPC metadata, and `grpcauthz.DevPrincipalFunc()` (wired above) turns
them into the `authz.Principal`. So the grant `{Tenant: "t1", Subjects: ["group:admin"]}`
authorizes a caller who presents `account-id: t1` and `groups: admin`:

```bash
# Allowed — identity matches the dev grant:
curl -s -X POST localhost:8080/v1/widgets \
  -H 'account-id: t1' -H 'groups: admin' \
  -d '{"name": "w1"}'

# Denied — no identity → empty principal → fail closed:
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/v1/widgets/w1   # 403
```

In production, set `PrincipalFunc` to one that reads a **verified** token (never raw client
headers) and swap `Authorizer` for your PDP — nothing else in the service changes.
