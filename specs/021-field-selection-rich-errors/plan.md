# F021 — Implementation Plan

## 1. Objective

Deliver AIP-157 response-side field selection (`read_mask`) and AIP-193 rich gRPC error details
(`BadRequest.FieldViolation`, `ResourceInfo`, `ErrorInfo`) as framework middleware, without
modifying the storage generators or the persistence seam beyond adding one new error type.

## 2. Architecture

### Field selection (AIP-157)

```
caller --read_mask{paths}→ gRPC request
        └─ ReadMaskUnary (new, last in chain)
                │   calls handler with full request
                │   ← handler returns full proto response
                └─ fieldmask.Apply(resp, paths)
                        │   proto.Message.ProtoReflect().Range(...)
                        │   Clear() non-listed fields
                        └─ masked response to caller
```

`Apply` lives in `middleware/fieldmask.go` (same file as `FieldMaskUnary`). It is a pure function
(no side effects, no state) that operates on any `proto.Message` via reflection — no generated-code
changes required.

`ReadMaskUnary` reads `GetReadMask() *fieldmaskpb.FieldMask` from the request via interface type
assertion. This works with any proto request that declares `google.protobuf.FieldMask read_mask`.

### Rich errors (AIP-193)

```
handler returns err
    └─ ErrorMapperUnary
            ├─ errors.As(err, &fv) → FieldViolationError
            │       → codes.InvalidArgument + BadRequest{FieldViolation{Field, Description}}
            ├─ errors.Is(err, ErrNotFound)
            │       → codes.NotFound + ResourceInfo{Description: "resource not found"}
            ├─ errors.Is(err, ErrConflict)
            │       → codes.AlreadyExists + ErrorInfo{reason:"ALREADY_EXISTS", ...}
            ├─ errors.Is(err, ErrPreconditionFailed)
            │       → codes.FailedPrecondition + ErrorInfo{reason:"PRECONDITION_FAILED", ...}
            └─ other → pass through
```

`FieldViolationError` lives in `persistence/` alongside existing sentinels. It is the only
persistence-package change.

## 3. Task sequence and dependencies

```
T001 ─ persistence/: FieldViolationError type
T002 ─ middleware/errormapper.go: upgrade with rich details (depends on T001)
T003 ─ middleware/errormapper_test.go: detail assertions (depends on T002)
T004 ─ middleware/fieldmask.go: Apply function
T005 ─ middleware/fieldmask.go: ReadMaskUnary interceptor (depends on T004)
T006 ─ server/server.go: wire ReadMaskUnary (depends on T005)
T007 ─ testdata/toy/widgets.proto: add read_mask
T008 ─ testdata/apikey/apikey.proto: add read_mask
T009 ─ buf generate for both fixtures (depends on T007, T008)
T010 ─ go.mod: promote errdetails to direct (depends on T002)
T011 ─ middleware/fieldmask_test.go: Apply + ReadMaskUnary tests (depends on T004, T005)
T012 ─ integration test in testdata/toy (depends on T006, T009)
T013 ─ verification gate: build + test all (depends on all above)
```

T001–T003 (AIP-193) and T004–T006 (AIP-157 apply+interceptor) are independent tracks that can
be done in sequence within each track; T007–T009 (proto regen) is likewise independent until T012.

## 4. File map

| File | Change |
|------|--------|
| `persistence/errors.go` (new) | `FieldViolationError` + `NewFieldViolation` |
| `middleware/errormapper.go` | Add `errdetails` import; upgrade `ErrorMapperUnary` |
| `middleware/errormapper_test.go` | Add AC-010..AC-015 assertions |
| `middleware/fieldmask.go` | Add `Apply`, `ReadMaskUnary` |
| `middleware/fieldmask_test.go` | Add AC-001..AC-007 tests |
| `server/server.go` | Append `ReadMaskUnary()` to chain |
| `testdata/toy/widgets.proto` | Add `read_mask` fields to Get/List requests |
| `testdata/apikey/apikey.proto` | Add `read_mask` fields to Get/List requests |
| `testdata/toy/widgetsv1/widgets.pb.go` | Regenerated |
| `testdata/apikey/apikeyv1/apikey.pb.go` | Regenerated |
| `testdata/toy/widgetsv1/server_test.go` | AC-008 GetWidget read_mask integration test |
| `go.mod` / `go.sum` | Promote `genproto/googleapis/rpc` to direct |

## 5. Dependencies and risks

**Dependencies already satisfied**:
- `google.golang.org/protobuf` — direct dep; provides `proto.Message`, `protoreflect`, `fieldmaskpb`
- `google.golang.org/genproto/googleapis/rpc` — already indirect; `errdetails` lives here

**Risks**:
- `buf generate` requires `buf` CLI and network access to BSR for proto deps. If unavailable, the
  fixture `.pb.go` files can be hand-edited (they only gain a `ReadMask` field + getter).
- `status.WithDetails` requires that `errdetails` types are registered proto types — they are
  (all `google.rpc.*` types are in the global registry). No runtime risk.

## 6. Verification

After all tasks complete:

1. `go build ./...` from root — must compile clean.
2. `go vet ./...` — zero findings.
3. `go test ./middleware/...` — all new + existing tests pass.
4. `go test ./...` from `testdata/toy` — server integration tests pass.
5. `go test ./...` from `testdata/apikey` — all existing tests still pass.
6. `git diff --stat` — no generated files changed unexpectedly beyond fixtures.
