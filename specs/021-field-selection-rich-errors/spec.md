# F021 — Field Selection + Rich Error Details

**AIPs**: AIP-157 (field selection via `read_mask`), AIP-193 (rich gRPC error details)  
**Status**: spec  
**Branch**: `021-field-selection-rich-errors`

---

## Problem statement

Two DX gaps remain after F020:

1. **No response-side field masking** (AIP-157). Callers cannot project the fields they need from
   Get/List responses; the server always returns the full resource. This forces clients to do
   client-side filtering and wastes bandwidth on large resources.

2. **Thin error messages** (AIP-193). The `ErrorMapperUnary` produces bare gRPC status codes
   (`codes.NotFound`, `codes.InvalidArgument`) with only a human-readable string. Callers — and
   framework-level tooling — cannot programmatically identify *which* field failed validation,
   *what* resource was not found, or *why* a precondition failed. The AIP-193 standard provides
   structured `google.rpc.Status` detail messages for exactly this purpose.

---

## Goals

- **G-001** Let callers pass a `read_mask` on Get/List requests and receive only the requested
  fields, with all unselected fields zeroed in the response proto.
- **G-002** Provide a `FieldViolationError` type in the `persistence` package so any layer
  (validation, storage, middleware) can signal "field X is invalid because Y" in a structured way.
- **G-003** Upgrade `ErrorMapperUnary` to attach `google.rpc.BadRequest.FieldViolation`,
  `google.rpc.ResourceInfo`, and `google.rpc.ErrorInfo` details to status errors so callers can
  inspect them programmatically.
- **G-004** Wire `ReadMaskUnary` into the framework's server interceptor chain.

### Non-goals

- Column-level DB projection to reduce `SELECT` bandwidth (optimization; can layer on top later).
- Recursive/nested field-mask paths for sub-messages (flat resources only for Phase 1).
- `google.rpc.RequestInfo` or `google.rpc.Help` detail types — not needed for the target DX.
- AIP-157 field masks on Update (already handled by `FieldMaskUnary` via `update_mask`).

---

## Design

### AIP-157: read_mask

AIP-157 specifies that Get and List requests **may** carry a `google.protobuf.FieldMask read_mask`
field. When present and non-empty, the server zeroes all fields *not* listed in the mask before
returning the response. An empty `read_mask` (or absent field) means "return all fields."

Implementation strategy:

```
Request (has read_mask?)
        │ yes
        ▼
   handler runs (full resource returned from storage)
        │
        ▼
   ReadMaskUnary intercepts response
        │
        ▼
   fieldmask.Apply(resp proto.Message, paths []string)
        │  • iterates proto reflection fields
        │  • clears any field whose proto name (snake_case)
        │    AND json name (camelCase) are not in the paths set
        │  • unknown paths are silently ignored (not an error)
        │  • empty paths → no-op
        ▼
   masked response returned to caller
```

The storage layer is untouched — it always returns full rows. Masking is a response-layer concern.

### AIP-193: rich error details

Three detail types cover the common cases:

| Situation | gRPC code | Detail attached |
|-----------|-----------|-----------------|
| `persistence.ErrNotFound` | `codes.NotFound` | `google.rpc.ResourceInfo{description: "resource not found"}` |
| `persistence.ErrConflict` | `codes.AlreadyExists` | `google.rpc.ErrorInfo{reason: "ALREADY_EXISTS", domain: "devedge-sdk/persistence"}` |
| `persistence.ErrPreconditionFailed` | `codes.FailedPrecondition` | `google.rpc.ErrorInfo{reason: "PRECONDITION_FAILED", domain: "devedge-sdk/persistence"}` |
| `persistence.FieldViolationError` (via `errors.As`) | `codes.InvalidArgument` | `google.rpc.BadRequest{field_violations: [{field, description}]}` |

`FieldViolationError` is a new type in `persistence/`:
```go
type FieldViolationError struct {
    Field       string // proto field name (snake_case)
    Description string // human-readable reason
}
func (e *FieldViolationError) Error() string { ... }
```

If `status.WithDetails` fails (should not happen with valid proto-any types), the mapper falls back
to returning the plain status code without details — it must not panic or lose the code.

### Interceptor chain order

Existing chain (outermost → innermost):
```
RequestID → ErrorMapper → TenantID → AuthZ → FieldMaskUnary → ETag
```

After F021:
```
RequestID → ErrorMapper → TenantID → AuthZ → FieldMaskUnary → ETag → ReadMaskUnary
```

`ReadMaskUnary` goes last in the framework chain so:
- Authz gate has already passed (unauthorized requests never receive masked/full responses).
- ETag comparison already happened on the full resource.
- Field masking is applied as the final response shaping step before the response is serialized.

---

## Feature requirements

### `persistence` package

| ID | Requirement |
|----|-------------|
| FR-001 | Add `FieldViolationError` struct to `persistence/` with exported `Field string` and `Description string` fields. It must implement `error`. |
| FR-002 | Provide a `NewFieldViolation(field, description string) *FieldViolationError` constructor. |

### `middleware` package — field apply + interceptor

| ID | Requirement |
|----|-------------|
| FR-010 | Add `Apply(msg proto.Message, paths []string)` to `middleware/fieldmask.go`. Empty `paths` → no-op. Matches field by proto name (snake_case) and JSON name (camelCase). Clears non-matching fields via proto reflection. Unknown path names are silently ignored. |
| FR-011 | `Apply` must NOT recurse into nested message fields (flat resources only; nested sub-messages are either retained as-is or cleared as a unit if their top-level path is not in the mask). |
| FR-012 | Add `ReadMaskUnary() grpc.UnaryServerInterceptor` to `middleware/fieldmask.go`. The interceptor: (a) type-asserts request to `interface { GetReadMask() *fieldmaskpb.FieldMask }`; (b) if mask is non-nil and has non-empty paths, calls the handler then calls `Apply` on the response if the response is a `proto.Message`; (c) if no `GetReadMask()` or mask is nil/empty → passes through unchanged; (d) on handler error → passes through the error unchanged. |

### `middleware` package — error mapper upgrade

| ID | Requirement |
|----|-------------|
| FR-020 | Upgrade `ErrorMapperUnary` to attach `google.rpc.ResourceInfo` detail on `ErrNotFound`. `ResourceInfo.Description` must be `"resource not found"`. `ResourceType` and `ResourceName` are left empty (the interceptor has no resource-type context). |
| FR-021 | Attach `google.rpc.ErrorInfo{reason:"ALREADY_EXISTS", domain:"devedge-sdk/persistence"}` on `ErrConflict`. |
| FR-022 | Attach `google.rpc.ErrorInfo{reason:"PRECONDITION_FAILED", domain:"devedge-sdk/persistence"}` on `ErrPreconditionFailed`. |
| FR-023 | Detect `persistence.FieldViolationError` via `errors.As`. Map to `codes.InvalidArgument` + `google.rpc.BadRequest` with one `FieldViolation{Field, Description}` entry. |
| FR-024 | If `status.WithDetails` returns an error, fall back to the plain status (code + message, no details). Do not panic. |
| FR-025 | Unmapped errors (not matching any `persistence` sentinel) continue to pass through unchanged. |

### `server` package

| ID | Requirement |
|----|-------------|
| FR-030 | Add `middleware.ReadMaskUnary()` to the framework interceptor chain in `server/server.go`, appended after `etag.PreconditionUnary()` and before `cfg.Interceptors`. |

### Fixture protos

| ID | Requirement |
|----|-------------|
| FR-040 | Add `google.protobuf.FieldMask read_mask = 8;` to `GetWidgetRequest` in `testdata/toy/widgets.proto`. |
| FR-041 | Add `google.protobuf.FieldMask read_mask = 4;` to `ListWidgetsRequest` in `testdata/toy/widgets.proto`. |
| FR-042 | Add `google.protobuf.FieldMask read_mask = 3;` to `GetAPIKeyRequest` in `testdata/apikey/apikey.proto`. Field 3 is free (account_id is on the resource, not the request). |
| FR-043 | Add `google.protobuf.FieldMask read_mask = 4;` to `ListAPIKeysRequest` in `testdata/apikey/apikey.proto` (field 3 is `show_deleted`). |
| FR-044 | Re-run `buf generate` (or `make generate`) for both fixture modules to regenerate `*.pb.go`. Commit regenerated files. |

### Dependencies

| ID | Requirement |
|----|-------------|
| FR-050 | Promote `google.golang.org/genproto/googleapis/rpc` from indirect to direct in the root `go.mod` (needed for `errdetails`). Run `go mod tidy`. |

---

## Failure modes

| ID | Mode | Mitigation |
|----|------|------------|
| FM-001 | Caller sends empty `read_mask` (zero-value FieldMask with no Paths) expecting all fields — `Apply` must NOT clear anything. | Guard: `if len(paths) == 0 { return }`. |
| FM-002 | Caller sends an unknown field name in `read_mask` (typo or deprecated field) — must not error; fields are just not retained. | Unknown paths are silently skipped in `Apply`. |
| FM-003 | `status.WithDetails` returns a marshalling error (should not happen with valid proto types) — must not panic or drop the gRPC code. | Fallback: return the plain `status.Error(code, msg)` if `WithDetails` errors. |
| FM-004 | `ErrorMapper` encounters a `FieldViolationError` wrapped inside another error — must unwrap via `errors.As`. | Use `errors.As(err, &fv)` before the switch, not a direct type assertion. |
| FM-005 | The response from a handler is `nil` (e.g., empty DeleteWidgetResponse) — `ReadMaskUnary` must not panic. | Guard: `if resp == nil { return resp, nil }` before type-asserting to `proto.Message`. |
| FM-006 | The response is a non-proto `any` (rare, defensive) — `Apply` must not be called. | Type assert `resp.(proto.Message)` is guarded with `ok` check. |

---

## Acceptance criteria

### AIP-157 — field selection

| ID | Criterion |
|----|-----------|
| AC-001 | `Apply` with `paths=["display_name"]` on a populated `*Widget` proto clears `id`, `color`, `weight`, `etag`, `name`; retains `display_name`. |
| AC-002 | `Apply` with `paths=[]` on a populated `*Widget` proto leaves all fields intact (no-op). |
| AC-003 | `Apply` with `paths=["displayName"]` (JSON camelCase) on a `*Widget` has the same effect as `paths=["display_name"]`. |
| AC-004 | `Apply` with `paths=["unknown_field", "display_name"]` retains `display_name` and clears all others; no error is returned for `unknown_field`. |
| AC-005 | `ReadMaskUnary` on a Get request with `read_mask.paths=["display_name"]` calls `Apply` on the proto response. |
| AC-006 | `ReadMaskUnary` on a Get request with no `GetReadMask()` interface does not call `Apply` and passes the response through unchanged. |
| AC-007 | `ReadMaskUnary` on a nil response (e.g., DeleteWidgetResponse) does not panic. |
| AC-008 | Integration test (toy server): `GetWidget` with `read_mask{paths:["display_name"]}` returns a `Widget` with `display_name` populated and `color`, `weight`, `id` zero. |

### AIP-193 — rich error details

| ID | Criterion |
|----|-----------|
| AC-010 | `ErrorMapperUnary` on `ErrNotFound` returns `codes.NotFound` with a `ResourceInfo` detail attached; `detail.Description == "resource not found"`. |
| AC-011 | `ErrorMapperUnary` on `ErrConflict` returns `codes.AlreadyExists` with an `ErrorInfo` detail; `detail.Reason == "ALREADY_EXISTS"`. |
| AC-012 | `ErrorMapperUnary` on `ErrPreconditionFailed` returns `codes.FailedPrecondition` with `ErrorInfo{Reason: "PRECONDITION_FAILED"}`. |
| AC-013 | `ErrorMapperUnary` on `&persistence.FieldViolationError{Field:"color", Description:"must be a hex code"}` returns `codes.InvalidArgument` with a `BadRequest` detail containing one `FieldViolation{field:"color", description:"must be a hex code"}`. |
| AC-014 | `ErrorMapperUnary` on a wrapped `FieldViolationError` (via `fmt.Errorf("outer: %w", fv)`) still produces the `BadRequest` detail (unwrap via `errors.As`). |
| AC-015 | Existing test scenarios from `errormapper_test.go` (`Conflict`, `PreconditionFailed`, `NotFound`, `UnmappedError`, `StatusMessageDoesNotContainPersistencePrefix`) all continue to pass. |

---

## Out of scope for F021

- DB column-level projection to avoid `SELECT *` (a storage-layer optimization).
- Recursive nested field masking (e.g., `widget.metadata.key`).
- `ResourceInfo.ResourceType` / `ResourceName` population (no type context in the interceptor; a
  future resource-context middleware can layer on top).
- `google.rpc.RequestInfo`, `google.rpc.DebugInfo`, `google.rpc.Help` detail types.
