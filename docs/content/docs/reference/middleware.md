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
RequestID → ErrorMapper → TenantID → grpcauthz → FieldMask → ETag/412 → ReadMask → ValidateOnly → Deduplicate
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
func WithPrincipalFunc(fn PrincipalFunc) Option
func WithMethodRule(method string, verb authz.Verb, resource string) Option
func WithPublicMethod(method string) Option

// Principal extraction:
type PrincipalFunc func(ctx context.Context) (authz.Principal, error)
func DevPrincipalFunc() PrincipalFunc   // dev-only: account-id/subject/groups metadata → Principal

// Boot-time gate:
func AssertMethodsDeclared(served []string, opts ...Option) error
```
The decision point. **Denies by default**: a method with no matching rule, or a principal with no
grant, gets `codes.PermissionDenied`. `AssertMethodsDeclared` refuses to start if any served
method is undeclared — call it at boot for a fail-closed completeness gate. The constructor and
options are rough-compatible with `infobloxopen/atlas-authz-middleware/grpc_opa` (see the repo's
`COMPAT.md`).

Through `server.New`, set the principal with `Config.PrincipalFunc` (it threads to
`WithPrincipalFunc`). `grpcauthz.DevPrincipalFunc()` is a **dev-only** extractor that reads
`account-id`/`subject`/`groups` from request metadata; in production supply a `PrincipalFunc`
backed by a **verified** token, never raw client headers. With no `PrincipalFunc` the principal is
empty, so no grant can match and every non-public call is denied.

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

## ReadMaskUnary — partial responses (AIP-157)

```go
func ReadMaskUnary() grpc.UnaryServerInterceptor
```
**Trigger:** a `google.protobuf.FieldMask read_mask = N;` field on the **request** message. When the
mask has non-empty paths, the interceptor applies it to the response after the handler returns,
clearing every field not named in the mask. Requests with no `read_mask` field, a nil mask, or an
empty path list pass through unchanged.

```proto
message GetWidgetRequest {
  string id = 1;
  google.protobuf.FieldMask read_mask = 2; // client asks for a subset of fields
}
```
```bash
# Over the gateway, read_mask is a repeated query param of field paths:
curl 'localhost:8080/v1/widgets/w1?read_mask=name,create_time'
```

## ValidateOnlyUnary — dry-run (AIP-163)

```go
func ValidateOnlyUnary() grpc.UnaryServerInterceptor
func ValidateOnlyFromContext(ctx context.Context) bool
```
**Trigger:** a `bool validate_only = N;` field on the **request** message. When it is true the
interceptor records the flag on `ctx`; **the handler still runs**, so the handler is responsible for
validating and then returning *without persisting*. Read the flag with `ValidateOnlyFromContext`:

```go
func (s *server) CreateWidget(ctx context.Context, req *pb.CreateWidgetRequest) (*pb.Widget, error) {
    if err := validate(req.Widget); err != nil {
        return nil, err
    }
    if middleware.ValidateOnlyFromContext(ctx) {
        return req.Widget, nil // dry run: validated, not written
    }
    return s.repo.Create(ctx, req.Widget)
}
```

## DeduplicateUnary — idempotent retries (AIP-155)

```go
func DeduplicateUnary(store DeduplicationStore) grpc.UnaryServerInterceptor

type DeduplicationStore interface {
    Load(requestID string) (any, bool)
    Store(requestID string, response any)
}
func NewMemoryDeduplicationStore(ttl time.Duration) *MemoryDeduplicationStore // server.New default: 10m
```
**Trigger:** a `string request_id = N;` field on the **request** message (the idempotency key). The
first successful call for a given `request_id` is cached; a retry with the same `request_id` replays
the cached response without re-running the handler. Requests with an empty `request_id`, or with
`validate_only=true`, bypass the cache; **handler errors are not cached** (so a failed call can be
retried). Override the store with `Config.DeduplicationStore`.

## Custom methods

A custom verb (AIP-136, e.g. `:publish`) is an ordinary RPC with a custom-verb HTTP mapping and its
own authz rule — no special interceptor:

```proto
rpc PublishWidget(PublishWidgetRequest) returns (Widget) {
  option (google.api.http)        = {post: "/v1/widgets/{id}:publish", body: "*"};
  option (infoblox.authz.v1.rule) = {verb: "publish", resource: "widgets"};
}
```
The `verb` you choose flows into the authz rule table and the boot-time completeness gate like any
standard verb. The gateway maps the `:publish` suffix; the storage plugin generates no extra method
(custom business logic lives in your handler).

## redact — log redaction for secret fields

```go
import "github.com/infobloxopen/devedge-sdk/middleware/redact"

func Message(m proto.Message) proto.Message       // clone with secret fields → "[REDACTED]"
func UnaryServerInterceptor() grpc.UnaryServerInterceptor // logs redacted req/resp at Debug
```
`Message` returns a **clone** of `m` with every `(infoblox.field.v1.opts) = {secret: true}` field
replaced by `[REDACTED]` (string) or its zero value (other kinds) — the original is untouched.
`UnaryServerInterceptor` logs redacted copies of the request and response via `slog.Debug`; the
real request/response passed to the handler are unchanged. This is **not** part of the default
`server.New` chain — add it via `Config.Interceptors` if you want request/response debug logging.
