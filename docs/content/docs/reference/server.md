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
    // Interceptors are additional unary interceptors appended after the framework chain.
    Interceptors []grpc.UnaryServerInterceptor
}
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `GRPCAddr` | **yes** | — | `:0` binds an ephemeral port; read it back with `GRPCAddr()` after `Serve` |
| `HTTPAddr` | no | `""` (disabled) | enables the grpc-gateway HTTP/JSON proxy |
| `Rules` | no* | `nil` | feeds **both** authz enforcement and field-mask verb lookup; *required in practice or every non-public call is denied |
| `Authorizer` | no | `authz.NewDevAuthorizer()` (no grants → deny all) | swap for OPA/Cedar/remote PDP |
| `Interceptors` | no | `nil` | appended **after** the framework chain |

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
    grpcauthz.UnaryServerInterceptor("sdk", authzOpts...), // fail-closed
    middleware.FieldMaskUnary(verbMap),                    // verbMap built from cfg.Rules
    etag.PreconditionUnary(),
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
gRPC connection when `Serve` starts. No-op unless an HTTP gateway is configured.

```go
func (s *Server) GatewayMux() *runtime.ServeMux // nil when no HTTP gateway
func (s *Server) Rules() []authz.MethodRule
func (s *Server) GRPCAddr() string // actual bound addr after Serve (useful when GRPCAddr was ":0")
func (s *Server) HTTPAddr() string // actual bound gateway addr after Serve; "" when no gateway
```

## Complete `main.go`

```go {filename="main.go"}
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/infobloxopen/devedge-sdk/authz"
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
    })
    if err != nil {
        log.Fatal(err)
    }

    // Register the gRPC service implementation.
    widgetv1.RegisterWidgetServiceServer(srv.GRPCServer(), newWidgetServer())

    // Register the HTTP/JSON gateway (only runs if HTTPAddr is set).
    srv.RegisterGateway(widgetv1.RegisterWidgetServiceHandler)

    log.Printf("gRPC %s  HTTP %s", srv.GRPCAddr(), srv.HTTPAddr())
    if err := srv.Serve(ctx); err != nil {
        log.Fatal(err)
    }
    log.Println("shut down cleanly")
}
```
