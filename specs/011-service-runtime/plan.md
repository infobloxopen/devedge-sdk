# Implementation Plan: Service Runtime

**Branch**: `011-service-runtime` | **Date**: 2026-06-11 | **Spec**: `spec.md`

## Summary

Wire the W3-4 codegen output into a bootable gRPC+HTTP service. Five new packages
(`server/`, `middleware/`, `middleware/etag/`) + two modified packages (`persistence/`,
`cmd/protoc-gen-svc`) + two new module deps (grpc-gateway, uuid). The checkpoint artifact is
`testdata/toy/server_test.go` — a live integration test that boots the full stack.

## Technical Context

**Language/Version**: Go 1.25.5
**Primary Dependencies added**: `github.com/grpc-ecosystem/grpc-gateway/v2`,
`github.com/google/uuid`; GORM stays out of root go.mod (SC-003)
**Testing**: stdlib `net` (random port listener), existing `go test ./...`
**Target Platform**: macOS + Linux (portable stdlib + grpc deps)
**Constraints**: GORM must not appear in root go.mod; no Portunus/OPA dependency

## Constitution Check (CLAUDE.md core principles)

| Principle | Status |
|-----------|--------|
| **Clean core** — no ORM/policy-engine dep in core packages | ✅ grpc-gateway ≠ ORM/policy; GORM stays in toy only |
| **Pluggable with dev-suitable defaults** | ✅ DevAuthorizer is the default; `Config.Authorizer` is swappable |
| **Fail closed** in authz | ✅ `AssertMethodsDeclared` boot gate retained; grpcauthz default is deny |

## Project Structure

### Documentation
```
specs/011-service-runtime/
├── spec.md     ✅ done
├── plan.md     ✅ this file
└── tasks.md    (next)
```

### Source Code
```
server/
├── server.go        NEW — Server, Config, New(), Serve()
└── server_test.go   NEW — unit tests (graceful shutdown, boot gate error)

middleware/
├── requestid.go     NEW — RequestIDUnary, RequestIDFromContext
├── requestid_test.go
├── errormapper.go   NEW — ErrorMapperUnary
├── errormapper_test.go
├── tenantid.go      NEW — TenantIDUnary, TenantIDFromContext, DefaultCellID constant
├── tenantid_test.go
├── fieldmask.go     NEW — FieldMaskUnary
└── fieldmask_test.go

middleware/etag/
├── etag.go          NEW — PreconditionUnary, IfMatchFromContext, SetNewETag, NewETagFromContext
└── etag_test.go     NEW — unit tests

persistence/
├── repository.go    MODIFY — add ErrPreconditionFailed
├── memory.go        MODIFY — pagination in List, ETag check in Update, ETag set on Create/Update

cmd/protoc-gen-svc/
├── render.go        MODIFY — new Register template: (*server.Server, Handler) error

testdata/toy/
├── widgets.proto    MODIFY — add google.api.http annotations, etag field on Widget
├── buf.gen.toy.yaml MODIFY — add protoc-gen-grpc-gateway plugin
├── widgetsv1/
│   ├── widgets.svc.go    REGENERATED (new Register signature + body)
│   ├── widgets.pb.go     REGENERATED (Widget gains etag field)
│   └── widgets.pb.gw.go  NEW (grpc-gateway handler, generated)
└── server_test.go   NEW — full-stack integration test

go.mod / go.sum      MODIFY — add grpc-gateway/v2, google/uuid
```

## Architecture Decisions

### 1. `server.Server` — wraps grpc.Server + optional HTTP gateway

```go
type Config struct {
    GRPCAddr     string
    HTTPAddr     string             // optional; empty disables HTTP gateway
    Rules        []authz.MethodRule // fed to grpcauthz interceptor
    Authorizer   authz.Authorizer   // defaults to DevAuthorizer(nil) if nil
    Interceptors []grpc.UnaryServerInterceptor // appended after framework chain
}

type Server struct {
    grpcSrv  *grpc.Server
    gwMux    *runtime.ServeMux   // nil when HTTPAddr == ""
    grpcAddr string
    httpAddr string
    // ... context, registered gateway fns
}

func New(cfg Config) (*Server, error)
func (s *Server) Serve(ctx context.Context) error
func (s *Server) GRPCServer() *grpc.Server
func (s *Server) RegisterGateway(fn func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error)
```

`New` wires the chain: `grpc.ChainUnaryInterceptor(requestID, errorMapper, tenantID, authz, fieldMask, ...extra)`.
`Serve` starts the gRPC listener, dials itself for the gateway (`grpc.NewClient`), runs gateway
`fns`, starts the HTTP server, then blocks until context cancelled.

### 2. Generated `RegisterWidgetService` (updated protoc-gen-svc template)

```go
func RegisterWidgetService(s *server.Server, srv WidgetServiceServer) error {
    if err := grpcauthz.AssertMethodsDeclared(
        WidgetServiceAuthzRules, s.Rules()...); err != nil {
        return err
    }
    pb.RegisterWidgetServiceServer(s.GRPCServer(), &widgetServiceAdapter{srv: srv})
    s.RegisterGateway(func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
        return pb.RegisterWidgetServiceHandlerClient(ctx, mux, NewWidgetServiceClient(conn))
    })
    return nil
}
```

The template is parameterised; `protoc-gen-svc` emits this for any service.

### 3. ETag round-trip via context (FR-009/010)

```
PreconditionUnary (before handler):
  reads metadata "if-match" → etag.setIfMatch(ctx, val)

MemoryRepository.Update:
  reads etag.ifMatchFromContext(ctx)
  if non-empty && != storedETag → return ErrPreconditionFailed
  generate newETag = uuid.New().String()
  store entity with newETag
  etag.setNewETag(ctx, newETag)

PreconditionUnary (after handler, via defer):
  reads etag.newETagFromContext(ctx)
  if non-empty → grpc.SetTrailer(ctx, metadata.Pairs("etag", val))
```

The two context keys (`ifMatchKey`, `newETagKey`) are unexported types in `middleware/etag/`
so only the framework writes them; handlers read via exported accessor functions.

### 4. MemoryRepository.List pagination

Cursor = base64(strconv.Itoa(offset)). Items stored in insertion order via a `[]K` slice.
`List` sorts by insertion order, slices `[offset : offset+pageSize]`, encodes next offset.
Thread-safe because the existing `sync.RWMutex` covers the keys slice.

### 5. Middleware field-mask gate

The interceptor uses a type assertion: `req.(interface{ GetUpdateMask() []string })`. If the
assertion holds and the slice is empty, return `InvalidArgument`. The generated
`UpdateWidgetRequest` already has `repeated string update_mask` → Go field `UpdateMask []string`
with a getter. No reflection required.

### 6. Tradeoffs

| Decision | Chosen | Rejected | Reason |
|----------|--------|----------|--------|
| ETag in context vs proto field | context | proto field on request | RFC 9110: ETag is a transport header, not a message field; clean separation |
| AssertMethodsDeclared location | inside generated Register | in server.New | Boot gate tied to each service's rule set, not the server |
| HTTP gateway dial | loopback (grpc.NewClient to GRPCAddr) | in-process | Simpler; grpc-gateway's standard pattern; dev latency is irrelevant |
| field-mask gate: empty mask = error vs all-fields | error | all-fields update | AIP-134: partial update requires explicit mask; default-deny discipline |
| cell_id | constant "default" in middleware | dynamic lookup | Cells are Phase-1.5+; reserve the wire slot, don't build the router |
