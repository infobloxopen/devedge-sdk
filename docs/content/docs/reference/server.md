---
title: server
weight: 1
---

```go
import "github.com/infobloxopen/devedge-sdk/server"
```

Package `server` provides a gRPC server builder with the framework interceptor chain wired in.
It assembles the chain — request ID, error mapping, tenant ID, fail-closed authz, field-mask
validation, ETag preconditions — and optionally runs an HTTP/JSON gateway in front of the gRPC
endpoint.

## Config

```go
type Config struct {
    // GRPCAddr is the TCP address to listen on (e.g. ":9090" or ":0"). Required.
    GRPCAddr string
    // HTTPAddr is the optional gateway address (e.g. ":8080"). Empty disables the HTTP gateway.
    HTTPAddr string
    // Rules is an OPTIONAL additive authz rule set. The generated Register<Service>
    // contributes each service's rules automatically (via AddRules), so the
    // generated path never needs this; set it only to merge in extra rules. The
    // rules feed both grpcauthz (enforcement) and the field-mask interceptor.
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
    // Logger is the structured logger the default chain's middleware.LoggingUnary
    // writes one record per RPC to (trace-correlated, secret-redacted payloads at
    // Debug). Defaults to slog.Default() when nil.
    Logger *slog.Logger
    // ReadinessChecks is the list of readiness checks the server runs on every
    // /readyz probe and whenever it drives the gRPC health status. An empty slice
    // means "always ready". Each check is bounded by a 2s timeout.
    ReadinessChecks []health.Check
    // Resilience configures optional resilience policy interceptors inserted into
    // the default chain. server.New applies a 30-second request timeout when the
    // zero value is supplied; rate limiting and circuit breaking are opt-in (nil).
    Resilience ResilienceConfig
}
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `GRPCAddr` | **yes** | — | `:0` binds an ephemeral port; read it back with `GRPCAddr()` after `Serve` |
| `HTTPAddr` | no | `""` (disabled) | enables the grpc-gateway HTTP/JSON proxy |
| `Rules` | no | `nil` | **optional** additive override; the generated `Register<Service>` contributes each service's rules via `AddRules`, so you rarely set this. The accumulated set must cover every registered method or `Serve` fails closed. |
| `Authorizer` | no | `authz.NewDevAuthorizer()` (no grants → deny all) | swap for OPA/Cedar/remote PDP |
| `PrincipalFunc` | no* | `nil` → empty principal | how the caller's identity is derived; *without it **no grant can match**, so every non-public call is denied. Use `grpcauthz.DevPrincipalFunc()` in dev, a verified-token function in prod |
| `Interceptors` | no | `nil` | appended **after** the framework chain |
| `DeduplicationStore` | no | in-memory (10m TTL) | idempotency replay store for `DeduplicateUnary` |
| `LROStore` | no | in-memory (1h TTL) | long-running operation store (AIP-151) |
| `Logger` | no | `slog.Default()` | structured logger for `middleware.LoggingUnary`; one record per RPC, trace-correlated with secret-redacted payloads at Debug |
| `ReadinessChecks` | no | `nil` (always ready) | list of readiness checks run on every `/readyz` probe and gRPC health status; each check is bounded by a 2s timeout; a single failure flips `/readyz` to 503 |
| `Resilience` | no | 30s request timeout; rate limiting and circuit breaking off | see `ResilienceConfig` below |

### ResilienceConfig

```go
type ResilienceConfig struct {
    RequestTimeout   time.Duration
    PerMethodTimeout map[string]time.Duration
    RateLimiter      resilience.RateLimiter
    CircuitBreaker   resilience.CircuitBreaker
}
```

| Field | Default | Notes |
|---|---|---|
| `RequestTimeout` | 30s | bounds every unary handler; set to `resilience.NoTimeout` to disable; zero value → 30s applied by `server.New` |
| `PerMethodTimeout` | `nil` | per gRPC full-method timeout override (e.g. `"/pkg.Svc/LongOp": 5*time.Minute`); takes precedence over `RequestTimeout`; `resilience.NoTimeout` disables that method's timeout |
| `RateLimiter` | `nil` (off) | inserted after `TenantIDUnary` to shed excess load with `codes.ResourceExhausted`; use `resilience.NewTokenBucket` or any `resilience.RateLimiter` implementation |
| `CircuitBreaker` | `nil` (off) | wraps handler invocations just inside the framework chain; plug in `sony/gobreaker`, `afex/hystrix-go`, or any `resilience.CircuitBreaker` implementation |

`DefaultGRPCAddr` is `":9090"`.

## New

```go
func New(cfg Config) (*Server, error)
```

`New` validates `cfg` and constructs a `*Server`. It returns an error if `GRPCAddr` is empty.
When `Authorizer` is nil it defaults to a default-deny dev authorizer (no grants), so the server
is fail-closed out of the box.

`New` builds this unary interceptor chain (outermost first):

```go
chain := []grpc.UnaryServerInterceptor{
    middleware.RequestIDUnary(),
    middleware.ErrorMapperUnary(),
    middleware.TenantIDUnary(),
    grpcauthz.UnaryServerInterceptor("sdk", authzOpts...), // fail-closed; reads the LIVE accumulated rule set (Config.Rules + AddRules)
    middleware.FieldMaskUnarySource(...),                  // verb map derived from the live accumulated rule set
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

`Serve` first runs the boot-time authz completeness gate (fail-closed) over the accumulated rule
set and every registered method. A registered RPC with neither a rule nor a `public: true`
exemption makes `Serve` return an `undeclared` error and never bind. `Serve` then starts the gRPC
server (and the HTTP gateway when configured) and blocks until `ctx` is cancelled, then shuts both
down gracefully (bounded by a 5s timeout). It returns the first fatal error from either server, or
nil on clean shutdown.

{{< callout type="info" >}}
**The completeness gate runs at `Serve`, not at `Register`.** This lets it see rules contributed
by every `Register<Service>` / `…WithRepository` call, which call `AddRules` after `New`.
Registering a service but never serving means the gate never runs — you must call `Serve`.
{{< /callout >}}

```go
func (s *Server) GRPCServer() *grpc.Server
```

The underlying `*grpc.Server`, so you can register your service implementations on it.

```go
func (s *Server) RegisterGateway(fn func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error)
```

Records a grpc-gateway registration function, invoked against the gateway mux and the in-process
gRPC connection when `Serve` starts. This is a no-op unless an HTTP gateway is configured. It is
the lower-level alternative to the generated `Register<Service>` helper (see *codegen*).

{{< callout type="warning" >}}
**Use `RegisterGateway` or the generated `Register<Service>` helper, not both.** The generated
helper already calls `RegisterGateway` for you, so combining them registers the gateway twice.
{{< /callout >}}

```go
func (s *Server) AddRules(rules ...authz.MethodRule)   // contribute authz rules (the generated Register<Service> calls this)
func (s *Server) RecordMethods(methods ...string)      // record registered FullMethods for the Serve-time gate
func (s *Server) GatewayMux() *runtime.ServeMux         // nil when no HTTP gateway
func (s *Server) Rules() []authz.MethodRule             // the accumulated set (Config.Rules + AddRules)
func (s *Server) GRPCAddr() string // actual bound addr after Serve (useful when GRPCAddr was ":0")
func (s *Server) HTTPAddr() string // actual bound gateway addr after Serve; "" when no gateway
```

### Multi-service protos: rules are auto-wired

`protoc-gen-devedge-authz` emits one `<Service>AuthzRules` per `service` (and an `AllAuthzRules`
aggregate). You do not hand-merge them. Registering each service — via
`Register<Service>WithRepository(s, repo)` or `Register<Service>(s, impl)` — calls
`s.AddRules(<Service>AuthzRules...)` for you, so the accumulated set covers every registered
service by the time `Serve` runs the completeness gate.

A service you never register contributes no rules and cannot silently serve unprotected: its
methods are not registered, and any that were would fail the gate. See
[codegen → protoc-gen-devedge-authz](../codegen/#protoc-gen-devedge-authz).

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
        // No Rules: the generated registration contributes WidgetServiceAuthzRules
        // for you (Config.Rules is now only an optional additive override).
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

    // One-call CRUD wiring (generated by protoc-gen-svc): construct the generated
    // default handler over the repository, register it on gRPC + the HTTP gateway,
    // and contribute the service's authz rules. The boot-time completeness gate runs
    // at Serve. (For custom logic, embed WidgetServiceCRUDHandler, override the
    // methods you change, and use RegisterWidgetService(srv, h) instead.)
    if err := widgetv1.RegisterWidgetServiceWithRepository(srv, newWidgetRepository()); err != nil {
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

In production, set `PrincipalFunc` to one that reads a verified token (never raw client headers)
and swap `Authorizer` for your PDP. Nothing else in the service changes.
