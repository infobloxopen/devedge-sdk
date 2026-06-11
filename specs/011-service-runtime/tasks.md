# Tasks: service runtime

**Branch**: `011-service-runtime`
**Spec**: `specs/011-service-runtime/spec.md`
**Plan**: `specs/011-service-runtime/plan.md`

---

## Phase 1: Dependencies + persistence additions

- [X] T001 [S] Add `github.com/grpc-ecosystem/grpc-gateway/v2` and `github.com/google/uuid` to
  `go.mod`: run `go get github.com/grpc-ecosystem/grpc-gateway/v2 github.com/google/uuid` then
  `go mod tidy`. Verify `go build ./...` still passes and `grep gorm go.mod` returns nothing.

- [X] T002 [S] Add `ErrPreconditionFailed` to `persistence/repository.go` (alongside the two
  existing error vars). Update `MemoryRepository.List` in `persistence/memory.go` to implement
  cursor-based pagination: encode offset as `base64(strconv.Itoa(offset))`; default page size 50
  when `ListOptions.PageSize <= 0`; empty `next_page_token` on last page. Keep insertion order
  via a `[]K` keys slice (protected by the existing mutex). Add `MemoryRepository.Create` to
  generate and store a UUID ETag alongside each entity (store in a parallel `map[K]string` etag
  store). Run `go test ./persistence/... -count=1`; existing tests must pass.

---

## Phase 2: Tests (write before implementation — must be red first)

- [X] T003 [S] [US3] Write `persistence/memory_pagination_test.go`: insert 5 widgets; call
  `List(ctx, {PageSize: 2, PageToken: ""})` → assert 2 items, non-empty token; call again with
  that token → assert 2 different items; call again → assert 1 item, empty token. Also test
  `PageSize: 0` → default 50. These must be green after T002 (pagination is in Phase 1).

- [X] T004 [S] [US2] Write `persistence/memory_etag_test.go`: create entity → get ETag from
  repo's ETag store; update with correct ETag → succeeds, returns new ETag; update with old
  (stale) ETag → returns `ErrPreconditionFailed`. Must be red until T002 adds ETag to Update.

- [X] T005 [S] [US4] Write `middleware/requestid_test.go`, `middleware/tenantid_test.go`,
  `middleware/fieldmask_test.go`, and `middleware/etag/etag_test.go` (all in new packages):
  - requestid: interceptor injects UUID when `x-request-id` absent; propagates when present.
  - tenantid: interceptor stores `account-id` metadata in context; empty when absent.
  - fieldmask: interceptor rejects update-verb request with empty `update_mask`; passes through
    non-update verbs; passes through requests without `GetUpdateMask()`.
  - etag: interceptor reads `if-match` metadata → sets context key; after handler sets new ETag
    in context → interceptor appends `etag` to response trailer.
  All four packages don't exist yet; tests must fail to compile until T006–T008 are done.

- [X] T006 [S] [US1] Write `middleware/errormapper_test.go`:
  - handler returning `persistence.ErrNotFound` → interceptor maps to `codes.NotFound`.
  - handler returning `persistence.ErrConflict` → `codes.AlreadyExists`.
  - handler returning `persistence.ErrPreconditionFailed` → `codes.FailedPrecondition`.
  - status messages must NOT contain "persistence:" prefix (SC-007).
  - handler returning an unmapped error → passes through unchanged.
  Must be red until T009 is done.

- [X] T007 [S] [US1] Write `server/server_test.go`:
  - `New` with nil `Authorizer` doesn't panic (uses DevAuthorizer default).
  - `RegisterWidgetService` with a service where one method has no rule → returns non-nil error
    (SC-004).
  - Server shuts down within 5 s when context is cancelled (start server, cancel ctx, assert
    `Serve` returns within 5 s).
  Must be red until T011 + T013 are done.

---

## Phase 3: Middleware implementation

- [X] T008 [S] [US4] Implement `middleware/requestid.go` + `middleware/tenantid.go` +
  `middleware/fieldmask.go` + `middleware/etag/etag.go` (FR-005/007/008/009).

  **requestid.go**: typed key `requestIDKey`; `RequestIDFromContext(ctx) string`;
  `RequestIDUnary()` reads `x-request-id` from `metadata.FromIncomingContext`, generates
  `uuid.New().String()` if absent, sets in ctx + outgoing header via `grpc.SetHeader`.

  **tenantid.go**: typed key `tenantIDKey`; `TenantIDFromContext(ctx) string`;
  `TenantIDUnary()` reads `account-id` from incoming metadata, stores in ctx, sets
  `cell-id: "default"` in outgoing header. Export `const DefaultCellID = "default"`.

  **fieldmask.go**: `FieldMaskUnary()` — if method has authz verb "update" (check via
  `grpcauthz.VerbFromContext` or passed-in rules map), type-assert request to
  `interface{ GetUpdateMask() []string }`, return `InvalidArgument` if slice is empty.
  Keep implementation simple: accept a `map[string]string` of `{fullMethod → verb}` built from
  the rules slice; check only when verb == "update".

  **etag/etag.go**: typed keys `ifMatchKey`, `newETagKey`; `IfMatchFromContext`,
  `SetNewETag`, `NewETagFromContext` accessors; `PreconditionUnary()` — reads `if-match`
  from metadata before handler, sets ifMatchKey in ctx; after handler (defer), reads newETagKey
  and sets gRPC trailer `etag` via `grpc.SetTrailer`.

  Run T005 tests — all must be green.

- [X] T009 [S] [US1] Implement `middleware/errormapper.go` (FR-006):
  `ErrorMapperUnary()` — wraps handler; inspects returned error:
  - `errors.Is(err, persistence.ErrNotFound)` → `status.Error(codes.NotFound, "not found")`
  - `errors.Is(err, persistence.ErrConflict)` → `status.Error(codes.AlreadyExists, "already exists")`
  - `errors.Is(err, persistence.ErrPreconditionFailed)` → `status.Error(codes.FailedPrecondition, "precondition failed")`
  - else: pass through.
  Run T006 tests — all must be green.

---

## Phase 4: Persistence ETag completion

- [X] T010 [S] [US2] Update `MemoryRepository.Update` in `persistence/memory.go` to check
  `etag.IfMatchFromContext(ctx)`: if non-empty and differs from stored ETag → return
  `ErrPreconditionFailed`. On success: generate `uuid.New().String()` as the new ETag, update the
  etag store, call `etag.SetNewETag(ctx, newETag)`. Also update `MemoryRepository.Get` to call
  `etag.SetNewETag(ctx, storedETag)` so read responses can return the current ETag.
  Run T004 tests — all must be green.

---

## Phase 5: Server package

- [X] T011 [C] Implement `server/server.go` (FR-001/002/003/014):
  - `New(cfg Config) (*Server, error)`: validate `GRPCAddr` non-empty; default `Authorizer` to
    `authz.NewDevAuthorizer(nil)` if nil; build interceptor chain:
    `grpc.ChainUnaryInterceptor(middleware.RequestIDUnary(), middleware.ErrorMapperUnary(),
    middleware.TenantIDUnary(), grpcauthz.UnaryServerInterceptor("sdk", authzOpts...),
    middleware.FieldMaskUnary(verbMap), etag.PreconditionUnary(), cfg.Interceptors...)`;
    create `grpc.NewServer(grpc.ChainUnaryInterceptor(...))`.
  - `Serve(ctx context.Context) error`: listen on `GRPCAddr` → start gRPC in goroutine; if
    `HTTPAddr` non-empty: dial loopback gRPC with `grpc.NewClient`, run registered gateway fns on
    `runtime.NewServeMux()`, start HTTP server in goroutine; block on ctx.Done() → graceful stop
    gRPC (5 s timeout) + HTTP shutdown (5 s deadline); return first error.
  - `RegisterGateway(fn func(ctx, *runtime.ServeMux, *grpc.ClientConn) error)`: store fn for
    Serve to invoke.
  - `GRPCServer() *grpc.Server`, `Rules() []authz.MethodRule`.
  Run T007 tests — all must be green.

---

## Phase 6: Proto + codegen

- [X] T012 [S] Update `widgets.proto`:
  - Add `import "google/api/annotations.proto"`.
  - Add `option (google.api.http)` annotations on all 5 methods:
    - `CreateWidget`: `post: "/v1/widgets"` body: `"widget"`
    - `GetWidget`: `get: "/v1/widgets/{id}"`
    - `ListWidgets`: `get: "/v1/widgets"`
    - `UpdateWidget`: `patch: "/v1/widgets/{widget.id}"` body: `"widget"`
    - `DeleteWidget`: `delete: "/v1/widgets/{id}"`
  - Add `string etag = 5` to `Widget` message.
  Update `buf.gen.toy.yaml` to add the `protoc-gen-grpc-gateway` plugin targeting `testdata/toy`.
  Run `buf generate --template buf.gen.toy.yaml` → must produce `widgetsv1/widgets.pb.gw.go`.

- [X] T013 [S] Update `cmd/protoc-gen-svc/render.go` to generate the new `RegisterWidgetService`
  body (plan §2). The template change:
  - Function signature: `(s *server.Server, srv <Service>Server) error`
  - Body: `AssertMethodsDeclared`, `pb.Register<Service>Server`, `s.RegisterGateway(...)`,
    `return nil`.
  - Add imports: `github.com/infobloxopen/devedge-sdk/server`, `grpcauthz`, gateway pb package.
  Run `go test ./cmd/protoc-gen-svc/... -count=1` — render tests must pass.
  Then run `buf generate --template buf.gen.toy.yaml` → regenerate `widgetsv1/widgets.svc.go`
  with the new signature.

---

## Phase 7: Integration test

- [X] T014 [S] [US1–5] Write `testdata/toy/server_test.go` (separate go.mod — add
  `google.golang.org/grpc`, `grpc-gateway/v2` to toy go.mod if not present). The test:
  1. Creates `server.New(Config{GRPCAddr: ":0", HTTPAddr: ":0", Rules: WidgetServiceAuthzRules,
     Authorizer: authz.NewDevAuthorizer(authz.Grant{Principal:"alice", Verb:"*", Resource:"*"})})`
  2. Calls `RegisterWidgetService(s, &toyHandler{})` — must return nil.
  3. Starts server in a goroutine; waits for it to be ready (dial check).
  4. **AuthZ** sub-test: `CreateWidget` as "alice" → success; as "" → PermissionDenied (SC-004).
  5. **ETag** sub-test: create → read ETag from trailer; update with correct ETag → new ETag in
     trailer; update with stale ETag → `FailedPrecondition` (SC-006).
  6. **Pagination** sub-test: create 5 widgets; list page_size=2 → 2 items + token; list page 2
     → 2 items; list page 3 → 1 item, empty token (SC-005).
  7. **Request-ID** sub-test: call without `x-request-id` → trailer/header has a UUID.
  8. **HTTP gateway** sub-test: `POST /v1/widgets` → JSON widget; `GET /v1/widgets/{id}` →
     JSON widget; `GET /v1/widgets?page_size=2` → paginated JSON (US5).
  9. Cancel context; assert `Serve` returns within 5 s.
  Run `cd testdata/toy && go test -v -count=1 ./... -timeout 30s` — all must pass.

---

## Phase 8: Verify + commit

- [X] T015 [S] `go build ./... && go vet ./...` from the repo root — clean (SC-001).
  `grep gorm go.mod` → no match (SC-003).
  `make test` → all root-module tests green.

- [X] T016 [S] `cd testdata/toy && go build ./... && go test -v -count=1 ./... -timeout 30s`
  → all integration tests green (SC-002).

- [X] T017 [S] Commit all: spec + plan + tasks + implementation.
  Message: `011: service runtime — server lifecycle, middleware chain, ETag/412, pagination`.

---

## Dependencies & Execution Order

- T001 → T002 → T003, T004 (tests use new persistence API)
- T003, T004 green immediately after T002
- T005, T006, T007 (red) → T008 (green T005), T009 (green T006), T011 (green T007)
- T008 → T010 (etag interceptor used in MemoryRepository Update)
- T010 → green T004
- T011 needs T008, T009 (middleware deps)
- T012 → T013 (proto must be updated before codegen template runs)
- T013 → T014 (integration test uses new generated code + server package)
- T014 → T015 → T016 → T017

## Complexity Tags

| Task | Tag | Reason |
|------|-----|--------|
| T001 | [S] | Mechanical: go get + go mod tidy |
| T002 | [S] | Pagination math + ETag store: straightforward but a few moving parts |
| T003 | [S] | Table-driven list pagination tests; pure logic |
| T004 | [S] | ETag unit tests; pure persistence logic |
| T005 | [S] | Four small interceptor test files; standard gRPC interceptor testing |
| T006 | [S] | Error-mapping unit tests; straightforward |
| T007 | [S] | Server unit tests; boot gate + shutdown |
| T008 | [S] | Four small interceptors; each is ~30 LOC of standard gRPC middleware |
| T009 | [S] | Error mapper; ~20 LOC |
| T010 | [S] | ETag check in Update; ~15 LOC addition |
| T011 | [C] | Server lifecycle: two listeners, gateway dial, goroutine coordination, graceful shutdown races |
| T012 | [S] | Proto annotation + buf.gen update; mechanical |
| T013 | [S] | Template update in protoc-gen-svc; mechanical string change |
| T014 | [S] | Integration test; mechanical gRPC test client setup + assertions |
| T015 | [S] | Run commands, check output |
| T016 | [S] | Run integration test |
| T017 | [S] | Git commit |
