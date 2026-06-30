---
title: middleware
weight: 2
---

```go
import "github.com/infobloxopen/devedge-sdk/middleware"
```

Package `middleware` provides the unary gRPC interceptors that `server.New` assembles into a chain. Use this reference when you need to understand what each interceptor does, configure one individually, or add an interceptor to a custom `grpc.Server`.

`server.New` wires the interceptors in this order (outermost first):

```
RequestID → ErrorMapper → TenantID → grpcauthz → FieldMask → ETag/412 → ReadMask → ValidateOnly → Deduplicate
```

Each interceptor is also usable standalone if you build your own `grpc.Server`.

## RequestID

```go
func RequestIDUnary() grpc.UnaryServerInterceptor
```

Attaches or propagates a request ID so every log line and downstream call can be correlated. This is the outermost interceptor, so the ID covers the whole request lifecycle.

## Error mapper

```go
func ErrorMapperUnary() grpc.UnaryServerInterceptor
```

Converts internal errors returned by handlers into safe gRPC status codes, stripping internal detail before it reaches the client. This interceptor is the runtime partner to `seccheck.AssertErrorMessagesClean`, which verifies that error messages are clean at test time.

## Tenant ID

```go
func TenantIDUnary() grpc.UnaryServerInterceptor
func TenantIDFromContext(ctx context.Context) string         // "" if absent
func WithTenantID(ctx context.Context, tenantID string) context.Context // tests / non-gRPC paths
const DefaultCellID = "default"
```

Reads the `account-id` key from incoming gRPC metadata onto `ctx` and sets a `cell-id: default` outgoing header. Generated repositories call `TenantIDFromContext` to scope every query — this is the root of [tenant isolation](../../concepts/tenant-isolation/). Use `WithTenantID` in tests and non-gRPC call paths that cannot go through the interceptor.

## grpcauthz

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

The authorization decision point. A method with no matching rule, or a principal with no grant, receives `codes.PermissionDenied`. Call `AssertMethodsDeclared` at boot to refuse startup when any served method is undeclared — this provides a fail-closed completeness gate. The constructor and options are rough-compatible with `infobloxopen/atlas-authz-middleware/grpc_opa` (see the repo's `COMPAT.md`).

Through `server.New`, set the principal with `Config.PrincipalFunc` (it threads to `WithPrincipalFunc`).

{{< callout type="warning" >}}
**`grpcauthz.DevPrincipalFunc()` is for development only.** It reads `account-id`, `subject`, and `groups` from raw request metadata. In production, supply a `PrincipalFunc` backed by a verified token. With no `PrincipalFunc`, the principal is empty and every non-public call is denied.
{{< /callout >}}

## Field mask

```go
func FieldMaskUnary(verbMap map[string]string) grpc.UnaryServerInterceptor
```

Validates a request's field mask against the method's verb. `server.New` builds `verbMap` (`FullMethod → verb`) from `Config.Rules`, so the same rule set that drives authz also drives field-mask validation.

### Gateway encoding of update_mask

A generated Update RPC uses `repeated string update_mask` (not `google.protobuf.FieldMask`). Over the grpc-gateway these have different wire forms. Supply `update_mask` as separate repeated query params using proto snake_case names:

| Form | Result |
|---|---|
| `?update_mask=name&update_mask=value` | **correct** |
| `?update_mask=name,value` | silent no-op (single unknown path) |
| `?update_mask=displayName` | silent no-op (camelCase rejected) |

{{< callout type="warning" >}}
**The two common alternatives fail silently (HTTP 200, zero fields updated).** Always use separate repeated query params with snake_case field names.
{{< /callout >}}

`read_mask` (`google.protobuf.FieldMask`) is different: it accepts a comma-joined single param — `?read_mask=name,create_time`. See [persistence → Update and the field mask](../persistence/#update_mask-encoding-over-the-gateway) for full examples.

## ETag and preconditions

```go
import "github.com/infobloxopen/devedge-sdk/middleware/etag"

func PreconditionUnary() grpc.UnaryServerInterceptor
func IfMatchFromContext(ctx context.Context) string
func SetNewETag(ctx context.Context, val string) context.Context
func NewETagFromContext(ctx context.Context) string
func SetIfMatch(ctx context.Context, val string) context.Context // testing
```

Implements HTTP ETag and conditional-request semantics over gRPC. The interceptor reads the `if-match` precondition from incoming metadata into `ctx` via `IfMatchFromContext`, and writes the response ETag as a gRPC `etag` trailer when the handler signals one via `SetNewETag`.

The ETag value comes from the resource itself. Declare a `string etag = N [(google.api.field_behavior) = OUTPUT_ONLY];` field on the resource. The `protoc-gen-storage` plugin stamps a fresh token on every Create and Update and surfaces it on read, so `w.GetEtag()` returns a real, changing value a client can echo as `If-Match`. A stale token yields a 412.

```go
func (s *server) GetWidget(ctx context.Context, req *pb.GetWidgetRequest) (*pb.Widget, error) {
    w := s.repo.Get(ctx, req.Id) // w.GetEtag() is populated by the storage layer

    // 412 precondition: reject if the client's If-Match doesn't match current state.
    if im := etag.IfMatchFromContext(ctx); im != "" && im != w.GetEtag() {
        return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
    }

    etag.SetNewETag(ctx, w.GetEtag()) // written as the response 'etag' trailer
    return w, nil
}
```

The token is opaque (AIP-154) — clients must not parse it. On the **ent** backend, the generated `entrepo.EtagMixin` supplies the `etag` column and a mutation hook that stamps `etag.New()` on every Create/Update. Surface it on reads with `p.Etag = e.Etag` in your `fromEnt<Resource>` mapping; the handler pattern above is then identical on both backends.

## Read mask

```go
func ReadMaskUnary() grpc.UnaryServerInterceptor
```

Applies partial-response filtering to a response (AIP-157). When the request message has a `google.protobuf.FieldMask read_mask = N;` field with non-empty paths, the interceptor clears every field not named in the mask after the handler returns. Requests with no `read_mask` field, a nil mask, or an empty path list pass through unchanged.

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

## Validate only

```go
func ValidateOnlyUnary() grpc.UnaryServerInterceptor
func ValidateOnlyFromContext(ctx context.Context) bool
```

Supports dry-run validation (AIP-163). When the request message has a `bool validate_only = N;` field set to `true`, the interceptor records the flag on `ctx`. The handler still runs — read the flag with `ValidateOnlyFromContext` and return without persisting:

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

### Gateway delivery of validate_only

{{< callout type="warning" >}}
**With a `body: "<resource>"` mapping, `validate_only` in the JSON body is silently ignored and the resource is persisted.** Pass `validate_only` as a query parameter instead.
{{< /callout >}}

```bash
# body:"widget": validate_only must be a query param; a body field is ignored.
curl -X POST 'localhost:8080/v1/widgets?validate_only=true' -d '{"name":"dry-run"}'
```

To accept `validate_only` in the JSON body, map the method with `body: "*"`. All top-level request fields then bind from the body. Over direct gRPC the request-message field works as-is. The same rule applies to `request_id` and any other top-level control field.

## Deduplication

```go
func DeduplicateUnary(store DeduplicationStore) grpc.UnaryServerInterceptor

type DeduplicationStore interface {
    Load(requestID string) (any, bool)
    Store(requestID string, response any)
}
func NewMemoryDeduplicationStore(ttl time.Duration) *MemoryDeduplicationStore // server.New default: 10m
```

Supports idempotent retries (AIP-155). When the request message has a `string request_id = N;` field (the idempotency key), the first successful call for that `request_id` is cached. A retry with the same `request_id` replays the cached response without re-running the handler.

The cache is bypassed when `request_id` is empty or when `validate_only` is `true`. Handler errors are not cached, so a failed call can be retried safely. Override the store with `Config.DeduplicationStore`.

### Gateway delivery of request_id

`request_id` is a top-level field like `validate_only`. With a `body: "<resource>"` mapping it must be sent as a query parameter (`?request_id=<uuid>`, not in the JSON body). With `body: "*"` it binds from the body. Custom methods typically use `body: "*"` and therefore accept `request_id` in the body directly.

## Custom methods

A custom verb (AIP-136, for example `:publish`) is an ordinary RPC with a custom-verb HTTP mapping and its own authz rule — no special interceptor is required:

```proto
rpc PublishWidget(PublishWidgetRequest) returns (Widget) {
  option (google.api.http)        = {post: "/v1/widgets/{id}:publish", body: "*"};
  option (infoblox.authz.v1.rule) = {verb: "publish", resource: "widgets"};
}
```

The `verb` you choose flows into the authz rule table and the boot-time completeness gate like any standard verb. The gateway maps the `:publish` suffix; the storage plugin generates no extra method — custom business logic lives in your handler.

## Log redaction

```go
import "github.com/infobloxopen/devedge-sdk/middleware/redact"

func Message(m proto.Message) proto.Message       // clone with secret fields → "[REDACTED]"
func UnaryServerInterceptor() grpc.UnaryServerInterceptor // logs redacted req/resp at Debug
```

`Message` returns a clone of `m` with every `(infoblox.field.v1.opts) = {secret: true}` field replaced by `[REDACTED]` (string) or its zero value (other kinds) — the original is untouched. `UnaryServerInterceptor` logs redacted copies of the request and response via `slog.Debug`; the real request and response passed to the handler are unchanged.

{{< callout type="info" >}}
**This interceptor is not part of the default `server.New` chain.** Add it via `Config.Interceptors` when you want request/response debug logging.
{{< /callout >}}
