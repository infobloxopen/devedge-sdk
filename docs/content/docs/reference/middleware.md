---
title: middleware
weight: 2
---

```go
import "github.com/infobloxopen/devedge-sdk/middleware"
```

Package `middleware` provides the framework's unary gRPC interceptors. `server.New` assembles them
into a chain in this order (outermost first):

```
RequestID → ErrorMapper → TenantID → grpcauthz → FieldMask → ETag/412
```

Each is also usable standalone if you build your own `grpc.Server`.

## RequestIDUnary

```go
func RequestIDUnary() grpc.UnaryServerInterceptor
```
Attaches/propagates a request id so every log line and downstream call can be correlated. It is
the outermost interceptor, so the id covers the whole request lifecycle.

## ErrorMapperUnary

```go
func ErrorMapperUnary() grpc.UnaryServerInterceptor
```
Converts internal errors returned by handlers into safe gRPC status codes, stripping internal
detail before it reaches the client. This is the runtime partner to
`seccheck.AssertErrorMessagesClean`, which verifies messages stay clean.

## TenantIDUnary

```go
func TenantIDUnary() grpc.UnaryServerInterceptor
func TenantIDFromContext(ctx context.Context) string         // "" if absent
func WithTenantID(ctx context.Context, tenantID string) context.Context // tests / non-gRPC paths
const DefaultCellID = "default"
```
Reads the `account-id` key from incoming metadata onto `ctx` and sets a `cell-id: default`
outgoing header. Generated repositories call `TenantIDFromContext` to scope every query — this is
the root of [tenant isolation](../../concepts/tenant-isolation/). Use `WithTenantID` in tests and
non-gRPC call paths that cannot go through the interceptor.

## grpcauthz — fail-closed authorization

```go
import "github.com/infobloxopen/devedge-sdk/authz/grpcauthz"

func UnaryServerInterceptor(app string, opts ...Option) grpc.UnaryServerInterceptor

// Options:
func WithRules(rules ...authz.MethodRule) Option
func WithAuthorizer(a authz.Authorizer) Option
func WithPrincipalFunc(fn func(ctx context.Context) authz.Principal) Option
func WithMethodRule(method string, verb authz.Verb, resource string) Option
func WithPublicMethod(method string) Option

// Boot-time gate:
func AssertMethodsDeclared(served []string, opts ...Option) error
```
The decision point. **Denies by default**: a method with no matching rule, or a principal with no
grant, gets `codes.PermissionDenied`. `AssertMethodsDeclared` refuses to start if any served
method is undeclared — call it at boot for a fail-closed completeness gate. The constructor and
options are rough-compatible with `infobloxopen/atlas-authz-middleware/grpc_opa` (see the repo's
`COMPAT.md`).

## FieldMaskUnary

```go
func FieldMaskUnary(verbMap map[string]string) grpc.UnaryServerInterceptor
```
Validates a request's field mask against the method's verb. `server.New` builds `verbMap`
(`FullMethod → verb`) from `Config.Rules`, so the same rule set that drives authz also drives
field-mask validation.

## etag — ETag / 412 preconditions

```go
import "github.com/infobloxopen/devedge-sdk/middleware/etag"

func PreconditionUnary() grpc.UnaryServerInterceptor
func IfMatchFromContext(ctx context.Context) string
func SetNewETag(ctx context.Context, val string) context.Context
func NewETagFromContext(ctx context.Context) string
func SetIfMatch(ctx context.Context, val string) context.Context // testing
```
Implements HTTP ETag / conditional-request semantics over gRPC. It reads the `if-match`
precondition from incoming metadata into `ctx` (`IfMatchFromContext`), and writes the response
ETag as a gRPC `etag` trailer when the handler signals one via `SetNewETag`:

```go
func (s *server) GetWidget(ctx context.Context, req *pb.GetWidgetRequest) (*pb.Widget, error) {
    w := s.repo.Get(ctx, req.Id)

    // 412 precondition: reject if the client's If-Match doesn't match current state.
    if im := etag.IfMatchFromContext(ctx); im != "" && im != w.ETag {
        return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
    }

    etag.SetNewETag(ctx, w.ETag) // written as the response 'etag' trailer
    return w, nil
}
```

## redact — log redaction for secret fields

```go
import "github.com/infobloxopen/devedge-sdk/middleware/redact"

func Message(m proto.Message) proto.Message       // clone with secret fields → "[REDACTED]"
func UnaryServerInterceptor() grpc.UnaryServerInterceptor // logs redacted req/resp at Debug
```
`Message` returns a **clone** of `m` with every `(infoblox.authz.v1.field).secret = true` field
replaced by `[REDACTED]` (string) or its zero value (other kinds) — the original is untouched.
`UnaryServerInterceptor` logs redacted copies of the request and response via `slog.Debug`; the
real request/response passed to the handler are unchanged. This is **not** part of the default
`server.New` chain — add it via `Config.Interceptors` if you want request/response debug logging.
