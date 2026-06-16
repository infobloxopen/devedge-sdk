# F021 Tasks

Each task produces a verifiable diff. Tags: `[S]` = mechanical (Sonnet),
`[C]` = judgment/ripple/template logic (Opus). Sequencing is governed by
`plan.md` Section 3.

## Phase 1 — Rich error types + upgraded ErrorMapper (AIP-193)

- [X] [S] T001: Create `persistence/errors.go`. Define `FieldViolationError` struct with
  `Field string` and `Description string`, implementing `error` (message: `"field violation: <Field>: <Description>"`).
  Add `NewFieldViolation(field, description string) *FieldViolationError` constructor.
  Do NOT move the existing sentinels (`ErrNotFound`, `ErrConflict`, `ErrPreconditionFailed`) —
  those stay in `persistence/repository.go`. (FR-001, FR-002)

- [X] [C] T002: Upgrade `middleware/errormapper.go`:
  - Add import for `google.golang.org/genproto/googleapis/rpc/errdetails`.
  - Before the existing `switch`, use `errors.As(err, &fv)` to detect `*persistence.FieldViolationError`;
    if matched, build `codes.InvalidArgument` status with `BadRequest.FieldViolation` detail and return.
  - In the `ErrNotFound` case, attach `ResourceInfo{Description: "resource not found"}` via
    `status.New(...).WithDetails(...)`.
  - In the `ErrConflict` case, attach `ErrorInfo{Reason: "ALREADY_EXISTS", Domain: "devedge-sdk/persistence"}`.
  - In the `ErrPreconditionFailed` case, attach `ErrorInfo{Reason: "PRECONDITION_FAILED", Domain: "devedge-sdk/persistence"}`.
  - If `WithDetails` returns an error, fall back to `status.Error(code, msg)` — no panic.
  (FR-020, FR-021, FR-022, FR-023, FR-024, FM-003, FM-004)

- [X] [S] T003: Update `middleware/errormapper_test.go`:
  - For `ErrNotFound`: assert `st.Details()` contains a `*errdetails.ResourceInfo` with `Description == "resource not found"` (AC-010).
  - For `ErrConflict`: assert `*errdetails.ErrorInfo` with `Reason == "ALREADY_EXISTS"` (AC-011).
  - For `ErrPreconditionFailed`: assert `*errdetails.ErrorInfo` with `Reason == "PRECONDITION_FAILED"` (AC-012).
  - Add `TestErrorMapper_FieldViolation_ReturnsBadRequestDetail`: direct `FieldViolationError`,
    assert `codes.InvalidArgument` + `*errdetails.BadRequest` with one `FieldViolation{field:"color", description:"must be a hex code"}` (AC-013).
  - Add `TestErrorMapper_WrappedFieldViolation_Unwraps`: `fmt.Errorf("outer: %w", fv)`,
    same assertions (AC-014).
  - All existing test cases must still compile and pass (AC-015).
  (AC-010, AC-011, AC-012, AC-013, AC-014, AC-015)

## Phase 2 — Field mask apply + interceptor (AIP-157)

- [X] [C] T004: In `middleware/fieldmask.go`, add `Apply(msg proto.Message, paths []string)`:
  - `if len(paths) == 0 { return }` — empty paths is a no-op (FM-001).
  - Build a `map[string]bool` from `paths` (add each path as-is — callers may pass either
    snake_case proto name or camelCase JSON name).
  - `msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool { ... })`:
    if neither `string(fd.Name())` nor `fd.JSONName()` is in the set, call `msg.ProtoReflect().Clear(fd)`.
  - Unknown paths → silently ignored (FM-002).
  - Add required imports: `google.golang.org/protobuf/proto`, `google.golang.org/protobuf/reflect/protoreflect`.
  (FR-010, FR-011, AC-001, AC-002, AC-003, AC-004)

- [X] [C] T005: In `middleware/fieldmask.go`, add `ReadMaskUnary() grpc.UnaryServerInterceptor`:
  - Type-assert request: `type readMaskGetter interface { GetReadMask() *fieldmaskpb.FieldMask }`.
  - If request implements `readMaskGetter` and `mask := req.GetReadMask(); mask != nil && len(mask.GetPaths()) > 0`:
    call handler, then on nil error + non-nil response, type-assert response to `proto.Message`
    (with `ok` guard) and call `Apply(pm, mask.GetPaths())`.
  - If request has no `GetReadMask()` or mask is nil/empty → pass through unchanged.
  - On handler error → return `nil, err` unchanged (FM-005, FM-006).
  - Add import: `google.golang.org/protobuf/types/known/fieldmaskpb`.
  (FR-012, AC-005, AC-006, AC-007)

- [X] [S] T006: In `server/server.go`, append `middleware.ReadMaskUnary()` to the framework chain
  after `etag.PreconditionUnary()` and before `cfg.Interceptors`. Update the package-level doc
  comment to list `ReadMaskUnary` in the chain description. (FR-030)

## Phase 3 — Fixture proto updates + regen

- [X] [S] T007: In `testdata/toy/widgets.proto`:
  - Add `import "google/protobuf/field_mask.proto";`.
  - Add `google.protobuf.FieldMask read_mask = 8;` to `GetWidgetRequest`.
  - Add `google.protobuf.FieldMask read_mask = 4;` to `ListWidgetsRequest`.
  (FR-040, FR-041)

- [X] [S] T008: In `testdata/apikey/apikey.proto`:
  - Add `import "google/protobuf/field_mask.proto";`.
  - Add `google.protobuf.FieldMask read_mask = 3;` to `GetAPIKeyRequest`.
  - Add `google.protobuf.FieldMask read_mask = 4;` to `ListAPIKeysRequest`.
  (FR-042, FR-043)

- [X] [C] T009: Rebuild generated files for both fixture modules:
  - Run `buf generate` from `testdata/toy` (using `buf.gen.toy.yaml` or root `buf.gen.yaml`).
  - Run `buf generate` from `testdata/apikey`.
  - Inspect `git diff` of regenerated `*.pb.go`: confirm `GetWidgetRequest`/`GetAPIKeyRequest` gain
    `ReadMask *fieldmaskpb.FieldMask` + `GetReadMask()` accessor; no other resource messages changed.
  (FR-044)

## Phase 4 — go.mod cleanup

- [X] [S] T010: In root `go.mod`, promote `google.golang.org/genproto/googleapis/rpc` from
  `// indirect` to a direct `require` entry. Run `go mod tidy` and commit updated `go.mod` + `go.sum`.
  Verify `go build ./...` still clean. (FR-050)

## Phase 5 — Tests

- [X] [S] T011: Update `middleware/fieldmask_test.go` — add tests for `Apply` and `ReadMaskUnary`:
  - `TestApply_SubsetMask_ClearsOtherFields`: populate a `*widgetsv1.Widget` (import from testdata
    or use a hand-typed minimal proto); call `Apply` with `["display_name"]`; assert `DisplayName`
    is non-empty, `Color` / `Weight` / `Id` are zero (AC-001).
  - `TestApply_EmptyMask_NoOp`: call `Apply` with `[]string{}`; assert all fields unchanged (AC-002).
  - `TestApply_JSONNamePath`: call with `["displayName"]`; same effect as `["display_name"]` (AC-003).
  - `TestApply_UnknownPath_Ignored`: call with `["unknown_xyz", "display_name"]`; no error, `display_name` retained (AC-004).
  - `TestReadMaskUnary_WithMask_CallsApply`: fake request + fake proto response; assert masked field is zero (AC-005).
  - `TestReadMaskUnary_NoMaskInterface_PassesThrough`: request without `GetReadMask()`; response unchanged (AC-006).
  - `TestReadMaskUnary_NilResponse_NoPanic`: handler returns `nil, nil`; interceptor must not panic (AC-007).
  All existing fieldmask tests must still pass.

- [X] [C] T012: Add `TestGetWidget_ReadMask` integration scenario to
  `testdata/toy/widgetsv1/server_test.go`:
  - Create a Widget, then call `GetWidget` with `read_mask{paths:["display_name"]}`.
  - Assert response has `DisplayName` non-empty, `Id` == `""`, `Color` == `""`, `Weight` == `0` (AC-008).
  Use the existing test server setup pattern (`startTestServer`, `grpc.NewClient`, etc.).

## Phase 6 — Verification gate

- [X] [C] T013: Run the full verification gate:
  - `go build ./...` from root — must compile clean.
  - `go vet ./...` from root — zero findings.
  - `go test ./middleware/...` — all new and existing tests pass.
  - `go test ./...` from `testdata/toy` — all scenarios green (including new AC-008).
  - `go test ./...` from `testdata/apikey` — existing tests unaffected.
  - `git diff --stat` scope check: only expected files changed; no unintended generated churn.
