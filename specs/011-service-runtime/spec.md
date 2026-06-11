# Feature Specification: Service Runtime — middleware chain, lifecycle, ETag/412, pagination

**Feature Branch**: `011-service-runtime`
**Created**: 2026-06-11
**Status**: Draft

## Context

W3-4 delivered `protoc-gen-svc` + `protoc-gen-storage`: a service author can generate a handler
interface, a storage model, and an authz rules table from their proto. But the generated
`RegisterWidgetService` is a no-op stub (its body is `_ = adapter; _ = s`) and nothing wires
the authz interceptor, request-ID, error mapping, or tenant extraction. A service built with the
W3-4 output cannot boot, serve requests, or enforce authorization.

W5-6 closes that gap: it delivers the **service runtime** — the layer that wires all the
framework concerns (authz, request-ID, error mapping, tenant-ID, ETag/412, field-mask, pagination)
into a bootable gRPC+HTTP server, driven from generated annotations.

**Checkpoint (vision.md W5-6):** a generated service boots with real interceptors, serves gRPC
and REST, enforces authz with the DevAuthorizer, validates ETags, paginates list responses, and
extracts tenant_id — all from the proto annotation contract, with no per-handler boilerplate.

## Clarifications

- **`RegisterWidgetService` signature change**: the W3-4 stub has a TODO anticipating this.
  The new signature is `RegisterWidgetService(s *server.Server, srv WidgetServiceServer) error`.
  It asserts all methods declared, registers gRPC, and (when the server has an HTTP gateway
  configured) registers the grpc-gateway HTTP handler. The generated code imports
  `github.com/infobloxopen/devedge-sdk/server`; the toy's separate go.mod already uses a
  `replace` directive for local development, so this is a non-issue.
- **ETag transport**: ETag values travel as gRPC response trailer metadata (`etag` key), not as
  proto fields. The `if-match` value travels as gRPC request metadata (`if-match` key). An
  `etag.PreconditionUnary` interceptor extracts `if-match`, puts it in context; the Repository
  reads from context. This is RFC 9110-aligned and keeps proto messages clean.
- **ETag generation**: `MemoryRepository.Create` and `.Update` compute a new random ETag (UUID
  v4) and store it alongside the entity. The handler must set the response trailer after each
  mutating call. `MemoryRepository.Get` also returns the current ETag in context so the handler
  can set it on read responses.
- **TenantID**: extracted from gRPC incoming metadata key `account-id` (the Infoblox standard
  claim name in header form). Full JWT parsing and signature verification are Portunus-gated (out
  of scope); the interceptor trusts the header value in dev mode. Put into context via a typed
  key exported from `middleware`.
- **Field-mask**: `UpdateWidgetRequest.update_mask` is `repeated string`. The
  `FieldMaskUnary` interceptor checks that update-verb requests carry a non-empty mask; returns
  `InvalidArgument` if the mask is empty. `MemoryRepository.Update` respects the mask by merging
  only the named fields.
- **grpc-gateway**: added to `go.mod`. The `server.Config.HTTPAddr` field is optional; when set,
  the server starts a grpc-gateway HTTP mux on that address, connected back to the same gRPC
  server. The generated `RegisterWidgetService` registers the HTTP handler when a gateway mux is
  present. `widgets.proto` gains `google.api.http` annotations and an `etag` field on `Widget`
  (for REST response bodies; gRPC transport still uses metadata).
- **GORM stays out of devedge-sdk go.mod**: grpc-gateway is added to the main module; GORM
  stays only in `testdata/toy/go.mod`. The generated `.storage.go` file imports `gorm.io/gorm`
  which lives in the toy's separate module — unchanged.
- **`cell_id="default"` constant**: exported as `server.DefaultCellID = "default"`. Middleware
  adds it as a constant gRPC metadata entry on all outgoing calls; handlers can read it from
  context for telemetry labeling. No dynamic cell routing in this feature.
- **Middleware chain order** (outermost first): RequestID → ErrorMapper → TenantID → AuthZ →
  FieldMask → [handler]. ETag checking is inside the Repository, not a separate interceptor layer.

## User Scenarios & Testing

### User Story 1 — A generated service boots and serves with real authz (P1) 🎯 MVP

A developer creates a `server.Server` with the generated `WidgetServiceAuthzRules` and a
`DevAuthorizer` with grants, calls `RegisterWidgetService(s, handler)`, and starts the server.
gRPC requests to `CreateWidget` and `GetWidget` succeed when the caller has the right grant;
requests without a matching grant receive `PermissionDenied`. The server shuts down cleanly on
SIGTERM.

**Acceptance Scenarios**:

1. **Given** a `server.Server` with DevAuthorizer granting `create/widgets`, **When**
   `CreateWidget` is called via gRPC, **Then** the handler is invoked and returns the widget.
2. **Given** the same server, **When** `CreateWidget` is called with an empty principal (no
   `account-id` metadata), **Then** `PermissionDenied` is returned (DevAuthorizer has no grant
   for the empty principal).
3. **Given** a server where a method has no rule declared, **When** `RegisterWidgetService` is
   called, **Then** it returns a non-nil error naming the undeclared method (boot gate).
4. **Given** the server is running, **When** its context is cancelled, **Then** it shuts down
   within 5 s (graceful shutdown drains in-flight RPCs).

**Independent Test**: `testdata/toy/server_test.go` with a live gRPC server on a random port.

---

### User Story 2 — ETag/412 optimistic concurrency (P1)

A developer creates a widget, reads its ETag from the response trailer, updates it with the
correct ETag (success), then attempts a second update with the stale ETag (412).

**Acceptance Scenarios**:

1. **Given** a `CreateWidget` response, **When** the response trailer is inspected, **Then** it
   contains an `etag` key with a non-empty value.
2. **Given** a correct `if-match` metadata value on `UpdateWidget`, **When** the call is made,
   **Then** it succeeds and returns a new ETag in the response trailer.
3. **Given** a stale `if-match` value (the ETag from before the previous update), **When**
   `UpdateWidget` is called, **Then** it returns `FailedPrecondition` (gRPC 412 equivalent).
4. **Given** no `if-match` metadata on `UpdateWidget`, **When** the call is made, **Then** it
   succeeds (ETag is optional in dev mode; 428 requiring is a per-service policy, not enforced
   by default).

**Independent Test**: integration test that creates, reads ETag, updates twice; asserts 412 on
second update with stale ETag.

---

### User Story 3 — AIP pagination on List (P1)

A developer lists widgets with `page_size=2`; receives two items and a `next_page_token`; pages
through until all items are returned with an empty `next_page_token`.

**Acceptance Scenarios**:

1. **Given** 5 widgets in the store and `page_size=2`, **When** `ListWidgets` is called, **Then**
   the response contains 2 widgets and a non-empty `next_page_token`.
2. **Given** the token from scenario 1, **When** `ListWidgets` is called again, **Then** the
   response contains 2 more (different) widgets.
3. **Given** the last page, **When** `ListWidgets` is called, **Then** `next_page_token` is empty.
4. **Given** `page_size=0` or unset, **When** `ListWidgets` is called, **Then** a default page
   size (50) is used and all ≤50 widgets are returned.

**Independent Test**: `MemoryRepository` unit test — insert 5, list page 1, list page 2, list
page 3; assert correct items per page and empty final token.

---

### User Story 4 — Request-ID propagation and tenant-ID extraction (P2)

A developer's handler reads the request-ID and tenant-ID from context without any setup code.

**Acceptance Scenarios**:

1. **Given** a gRPC call with `account-id: tenant-abc` metadata, **When** the handler reads
   `middleware.TenantIDFromContext(ctx)`, **Then** it returns `"tenant-abc"`.
2. **Given** a gRPC call with no `x-request-id` metadata, **When** the handler reads
   `middleware.RequestIDFromContext(ctx)`, **Then** it returns a non-empty UUID string (generated
   by the interceptor).
3. **Given** a gRPC call with `x-request-id: existing-id` metadata, **When** the handler reads
   `middleware.RequestIDFromContext(ctx)`, **Then** it returns `"existing-id"` (propagated).

**Independent Test**: unit tests for each interceptor with a mock handler that reads from ctx.

---

### User Story 5 — HTTP gateway (REST) serves the same service (P2)

A developer configures `server.Config.HTTPAddr` and makes REST calls that map to gRPC handlers
via grpc-gateway.

**Acceptance Scenarios**:

1. **Given** a server with `HTTPAddr: ":0"` and `GRPCAddr: ":0"`, **When**
   `POST /v1/widgets` is called with a JSON body, **Then** it routes to `CreateWidget` and
   returns a JSON Widget.
2. **Given** a server, **When** `GET /v1/widgets/{id}` is called, **Then** it routes to
   `GetWidget` and returns the JSON widget.
3. **Given** a server, **When** `GET /v1/widgets` is called with `?page_size=2`, **Then** it
   routes to `ListWidgets` and returns a paginated JSON response.

**Independent Test**: `testdata/toy/server_test.go` — HTTP subtest using `net/http` client
against the gateway address.

---

### Edge Cases

- What if `RegisterWidgetService` is called with a nil server? → returns non-nil error.
- What if two services are registered with conflicting authz rules? → `AssertMethodsDeclared`
  checks each service's rules independently; no conflict possible (disjoint method spaces).
- What if `page_size` is negative? → treated as 0 (use default).
- What if the gRPC endpoint is not reachable by the gateway at startup? → `Serve()` returns an
  error; no partial-start state.
- What if `UpdateWidget` is called with an `update_mask` containing an unknown field? →
  `FieldMaskUnary` returns `InvalidArgument` naming the unknown field.
- What if context is cancelled during `Serve()`? → gRPC server graceful-stops; HTTP server
  shuts down with a 5 s drain deadline.

## Requirements

### Functional Requirements

- **FR-001**: A new `server/` package MUST export a `Server` type constructed via
  `server.New(cfg Config) (*Server, error)`. `Config` fields: `GRPCAddr string` (required),
  `HTTPAddr string` (optional; omit to disable HTTP gateway), `Rules []authz.MethodRule`,
  `Authorizer authz.Authorizer`, `Interceptors []grpc.UnaryServerInterceptor` (extra, appended
  after the framework chain).
- **FR-002**: `Server.Serve(ctx context.Context) error` MUST start the gRPC listener, optionally
  start the HTTP gateway listener, and block until the context is cancelled or a fatal error
  occurs. On context cancellation, the gRPC server MUST graceful-stop (drain in-flight RPCs up
  to 5 s then force-stop); the HTTP gateway MUST shut down with a 5 s deadline.
- **FR-003**: The framework interceptor chain wired by `server.New` MUST be, in order:
  RequestID → ErrorMapper → TenantID → AuthZ (grpcauthz.UnaryServerInterceptor) → FieldMask.
- **FR-004**: `RegisterWidgetService(s *server.Server, srv WidgetServiceServer) error` MUST (1)
  call `grpcauthz.AssertMethodsDeclared` against the service's rules and return the error if any
  method is undeclared; (2) register the gRPC service handler; (3) register the grpc-gateway HTTP
  handler if the server has an HTTP gateway configured.
- **FR-005**: A new `middleware/` package MUST export `RequestIDUnary() grpc.UnaryServerInterceptor`
  which reads `x-request-id` from incoming metadata (using it if present) or generates a UUID v4,
  stores it in context via `middleware.RequestIDFromContext`, and sets it on the outgoing header.
- **FR-006**: `middleware.ErrorMapperUnary() grpc.UnaryServerInterceptor` MUST map
  `persistence.ErrNotFound` → `codes.NotFound`, `persistence.ErrConflict` →
  `codes.AlreadyExists`, `persistence.ErrPreconditionFailed` → `codes.FailedPrecondition`; for
  all mapped errors the gRPC status message MUST NOT contain SQL text, stack traces, or hostnames.
  Unmapped errors pass through unchanged.
- **FR-007**: `middleware.TenantIDUnary() grpc.UnaryServerInterceptor` MUST read
  `account-id` from incoming gRPC metadata and store it in context via
  `middleware.TenantIDFromContext`. Missing `account-id` is not an error (empty string stored).
- **FR-008**: `middleware.FieldMaskUnary() grpc.UnaryServerInterceptor` MUST inspect requests
  that implement `interface{ GetUpdateMask() []string }`: if the slice is empty on a method with
  authz verb `"update"`, return `codes.InvalidArgument`. Pass through all other requests.
- **FR-009**: A new `middleware/etag/` package MUST export `PreconditionUnary()` which reads
  `if-match` from incoming gRPC metadata for requests where the method has authz verb `"update"`,
  stores it in context via `etag.IfMatchFromContext`; then, after the handler returns, reads the
  new ETag from `etag.NewETagFromContext` and appends it to the gRPC response trailer as `etag`.
- **FR-010**: `persistence` MUST add `ErrPreconditionFailed = errors.New("persistence: precondition failed")`.
  `MemoryRepository.Update` MUST: (1) read the expected ETag from context via
  `etag.IfMatchFromContext`; (2) if non-empty and mismatching the stored ETag, return
  `ErrPreconditionFailed`; (3) on success, generate a new UUID ETag, store it with the entity,
  and publish it to context via `etag.SetNewETag`.
- **FR-011**: `MemoryRepository.List` MUST implement cursor-based pagination using
  `ListOptions.PageSize` (default 50 when ≤0) and `ListOptions.PageToken` (base64-encoded
  integer offset). An empty `PageToken` starts from offset 0. The returned `nextPageToken` MUST
  be empty when there are no more items.
- **FR-012**: `widgets.proto` MUST gain `google.api.http` HTTP annotations on all five methods
  mapping to standard AIP REST paths (`POST /v1/widgets`, `GET /v1/widgets/{id}`,
  `GET /v1/widgets`, `PATCH /v1/widgets/{widget.id}`, `DELETE /v1/widgets/{id}`). `Widget`
  MUST gain a `string etag = 5` field for REST response bodies.
- **FR-013**: `go.mod` MUST add `github.com/grpc-ecosystem/grpc-gateway/v2` and
  `github.com/google/uuid`. GORM MUST NOT be added (stays in `testdata/toy/go.mod` only).
- **FR-014**: `server.DefaultCellID = "default"` MUST be exported as a package-level constant.
  The `TenantIDUnary` interceptor MUST also set `cell-id: "default"` in the outgoing gRPC header
  (constant; no dynamic routing in this feature).

### Key Entities

- **`server.Server`**: wraps `*grpc.Server` + optional HTTP gateway mux; wires the framework
  interceptor chain; starts/stops the listeners.
- **`server.Config`**: declares all server options; `GRPCAddr` required; `HTTPAddr` optional.
- **`middleware.RequestIDKey`, `middleware.TenantIDKey`**: typed context keys; accessed via
  `RequestIDFromContext` / `TenantIDFromContext`.
- **`etag.IfMatchKey`, `etag.NewETagKey`**: typed context keys for passing ETag values through
  the interceptor→repository→interceptor round-trip.
- **`ErrPreconditionFailed`**: new error var in `persistence/`.

## Success Criteria

- **SC-001**: `go build ./...` and `go vet ./...` pass from the devedge-sdk root; `make test`
  passes (all existing unit tests green).
- **SC-002**: The integration test `testdata/toy/server_test.go` exercises all five W5-6
  scenarios (authz, ETag/412, pagination, request-ID, HTTP gateway) and passes.
- **SC-003**: GORM is NOT added to devedge-sdk's root `go.mod` (stays in toy's go.mod only);
  verified by `grep gorm go.mod` returning no match.
- **SC-004**: `RegisterWidgetService` returns a non-nil error when any method in the service
  lacks an authz rule (boot gate; unit test).
- **SC-005**: `MemoryRepository.List` returns correct items per page with a stable cursor;
  final page returns empty `next_page_token` (unit test with 5 items, page_size=2).
- **SC-006**: `MemoryRepository.Update` with a stale ETag returns `ErrPreconditionFailed`
  (unit test).
- **SC-007**: `ErrorMapperUnary` maps all three persistence errors to the correct gRPC codes;
  the mapped status messages contain no SQL text (unit test).
- **SC-008**: `buf generate` (root + toy) runs without errors after `widgets.proto` is updated;
  regenerated files compile (verified by `go build ./testdata/toy/...`).
