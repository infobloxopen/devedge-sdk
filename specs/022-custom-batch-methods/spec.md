# F022 — Custom Method Scaffold + Batch Methods

**AIPs**: AIP-136 (custom methods), AIP-137 (batch methods)  
**Status**: spec  
**Branch**: `022-custom-batch-methods`

---

## Problem statement

Two DX gaps remain after F021:

1. **No custom method pattern** (AIP-136). The SDK has no example of a non-CRUD method, and the
   developer story for adding one (HTTP routing with `:verb` suffix, authz rule convention, handler
   wiring) is undocumented. Without a concrete fixture, teams must guess the pattern, often getting
   the HTTP annotation or authz verb wrong.

2. **No batch operation support** (AIP-137). Callers that need to retrieve or delete multiple
   resources today must issue N serial gRPC calls. The `Repository` interface and
   `MemoryRepository` have no multi-key primitives, forcing each service to reinvent its own
   loop — with inconsistent locking, partial-failure semantics, and no framework guidance.

---

## Goals

- **G-001** Define and document the AIP-136 custom method pattern in the SDK via a concrete
  fixture: an `ArchiveWidget` custom method that demonstrates `:verb` HTTP routing and a
  non-standard authz verb.
- **G-002** Add a `BatchRepository[T,K]` interface to the `persistence` package that extends
  `Repository[T,K]` with `BatchGet` and `BatchDelete` operations.
- **G-003** Implement `BatchGet` and `BatchDelete` on `MemoryRepository` with atomic (all-or-nothing)
  failure semantics and correct soft-delete awareness.
- **G-004** Add `BatchGetWidgets` and `BatchDeleteWidgets` to the toy fixture service, demonstrating
  the AIP-137 HTTP routing pattern (`:batchGet` / `:batchDelete`).
- **G-005** Provide integration tests for all three new fixture methods.

### Non-goals

- `BatchCreate` and `BatchUpdate` — covered by future work; not in this spec.
- Changes to `protoc-gen-svc` or `protoc-gen-storage` — the existing plugins handle custom and
  batch methods without modification (they emit all methods into the boot-gate automatically).
- Long-running operations (LRO) for batch methods — F023+ territory.
- Partial-success semantics for batch ops — atomic failure keeps the persistence contract simple;
  partial results can layer on top later.
- AIP-122 resource-name style for batch IDs — the toy fixture uses bare `id` strings for
  consistency with existing CRUD methods; full `name` paths are a separate concern.

---

## Design

### AIP-136: custom methods

AIP-136 defines custom methods as any RPC that does not map to standard Create/Read/Update/Delete.
The key conventions are:

1. **HTTP routing**: custom methods use a colon-separated verb suffix on the resource URL:
   ```
   POST /v1/widgets/{id}:archive
   ```
2. **gRPC method naming**: `rpc <Verb><Resource>(<Verb><Resource>Request) returns (<Verb><Resource>Response)`.
3. **Authz**: the method carries `(infoblox.authz.v1.rule)` with a free-form verb (e.g.
   `verb: "archive"`). The boot-gate includes it in the declared-methods check; `DevAuthorizer`
   grants it in dev/test environments.
4. **Interceptor chain**: custom methods flow through the same chain as standard methods. The
   `FieldMaskUnary` and `ReadMaskUnary` interceptors are no-ops for requests without
   `update_mask`/`read_mask` fields.

**SDK contract**: the plugin, interceptor chain, and authz boot-gate all handle custom methods
transparently. The only developer action is adding the RPC with proper annotations.

**Fixture design** — `ArchiveWidget`:
- Adds `google.protobuf.Timestamp archived_time = 8` (OUTPUT_ONLY) to `Widget`.
- `ArchiveWidget(ArchiveWidgetRequest) returns (ArchiveWidgetResponse)`:
  - HTTP: `POST /v1/widgets/{id}:archive`, body `"*"`.
  - Authz: `verb: "archive", resource: "widgets"`.
  - Handler: Get → stamp `archived_time = now` → Update → return in response.

```
Client
  │  POST /v1/widgets/abc:archive  (HTTP gateway)
  ▼
gRPC: ArchiveWidget({id: "abc"})
  │
  ▼
Interceptor chain (same as standard methods):
  RequestID → ErrorMapper → TenantID → AuthZ → FieldMaskUnary → ETag → ReadMaskUnary
  │  AuthZ gate: "archive" on "widgets" declared → passes
  ▼
ArchiveWidget handler:
  w, _ := repo.Get(ctx, "abc")
  w.ArchivedTime = timestamppb.Now()
  w, _ = repo.Update(ctx, "abc", w)
  return &ArchiveWidgetResponse{Widget: w}
```

### AIP-137: batch methods

Batch methods operate on multiple resources in one call. AIP-137 conventions:

1. **BatchGet**: HTTP GET with query parameters (or POST body for complex cases):
   ```
   GET /v1/widgets:batchGet?ids=a&ids=b
   ```
2. **BatchDelete**: HTTP POST with `:batchDelete` verb:
   ```
   POST /v1/widgets:batchDelete  { "ids": ["a", "b"] }
   ```
3. **Authz**: inherits the underlying CRUD verb (`read` for BatchGet, `delete` for BatchDelete).
4. **Failure semantics**: atomic — if any key is missing or already deleted, the entire operation
   returns `ErrNotFound`. No partial commits. Services that need partial-success can layer their
   own logic on top of the `BatchRepository` interface.

**`BatchRepository` interface** (in `persistence/repository.go`):

```go
// BatchRepository extends Repository with multi-resource batch operations.
// All batch operations are atomic: if any key is invalid the call fails
// without modifying any resource.
type BatchRepository[T any, K comparable] interface {
    Repository[T, K]
    // BatchGet retrieves multiple resources by key. Returns ErrNotFound if any
    // key does not exist or is soft-deleted. Items are returned in the same
    // order as keys.
    BatchGet(ctx context.Context, keys []K) ([]T, error)
    // BatchDelete soft-deletes multiple resources. Returns ErrNotFound if any
    // key does not exist or is already soft-deleted; on error no items are
    // deleted.
    BatchDelete(ctx context.Context, keys []K) error
}
```

**`MemoryRepository` batch implementation**:

```
BatchGet:
  empty keys → return []T{}, nil
  RLock
  for each key:
    if not found or deleted → RUnlock, return ErrNotFound
    collect item
  RUnlock
  return items (same order as keys)

BatchDelete:
  empty keys → return nil
  Lock
  for each key (pre-check pass):
    if not found or already deleted → Unlock, return ErrNotFound
  for each key (mutation pass):
    deleted[key] = true
  Unlock
  return nil
```

Two-pass approach ensures atomicity under a single lock: validate all keys first, then mutate. No
partial state is possible.

**Fixture additions** — BatchGetWidgets and BatchDeleteWidgets:

```proto
message BatchGetWidgetsRequest   { repeated string ids = 1; }
message BatchGetWidgetsResponse  { repeated Widget widgets = 1; }
message BatchDeleteWidgetsRequest { repeated string ids = 1; }

rpc BatchGetWidgets(BatchGetWidgetsRequest) returns (BatchGetWidgetsResponse) {
  option (google.api.http) = {get: "/v1/widgets:batchGet"};
  option (infoblox.authz.v1.rule) = {verb: "read", resource: "widgets"};
}
rpc BatchDeleteWidgets(BatchDeleteWidgetsRequest) returns (google.protobuf.Empty) {
  option (google.api.http) = {post: "/v1/widgets:batchDelete", body: "*"};
  option (infoblox.authz.v1.rule) = {verb: "delete", resource: "widgets"};
}
```

### What does NOT change

- `protoc-gen-svc`: already emits all service methods into the boot-gate; no changes needed.
- `protoc-gen-storage`: the batch methods are service-layer operations, not storage-model
  generation; no changes needed.
- `server/server.go` interceptor chain: custom/batch methods flow through unchanged.
- `middleware/errormapper.go`: `ErrNotFound` from a batch call maps to `codes.NotFound` the same
  as a single-resource call.

---

## Feature requirements

### `persistence` package

| ID | Requirement |
|----|-------------|
| FR-001 | Add `BatchRepository[T any, K comparable]` interface to `persistence/repository.go`, embedding `Repository[T, K]` and adding `BatchGet(ctx context.Context, keys []K) ([]T, error)` and `BatchDelete(ctx context.Context, keys []K) error`. |
| FR-002 | `BatchGet` on `MemoryRepository`: empty `keys` → return `([]T{}, nil)`. Non-empty: acquire read-lock, check every key (not found or soft-deleted → return `ErrNotFound`), collect items in key order, release lock, return items. |
| FR-003 | `BatchDelete` on `MemoryRepository`: empty `keys` → return `nil`. Non-empty: acquire write-lock, pre-check all keys (not found or already soft-deleted → return `ErrNotFound`), then mark all `deleted[key] = true`, release lock. The two-pass approach ensures no partial deletes. |

### `testdata/toy/widgets.proto`

| ID | Requirement |
|----|-------------|
| FR-010 | Add `google.protobuf.Timestamp archived_time = 8 [(google.api.field_behavior) = OUTPUT_ONLY];` to the `Widget` message. |
| FR-011 | Add `ArchiveWidgetRequest { string id = 1; }` and `ArchiveWidgetResponse { Widget widget = 1; }` messages. |
| FR-012 | Add `BatchGetWidgetsRequest { repeated string ids = 1; }` and `BatchGetWidgetsResponse { repeated Widget widgets = 1; }` messages. |
| FR-013 | Add `BatchDeleteWidgetsRequest { repeated string ids = 1; }` message. |
| FR-014 | Add `rpc ArchiveWidget` to `WidgetService` with HTTP annotation `{post: "/v1/widgets/{id}:archive", body: "*"}` and authz rule `{verb: "archive", resource: "widgets"}`. |
| FR-015 | Add `rpc BatchGetWidgets` to `WidgetService` with HTTP annotation `{get: "/v1/widgets:batchGet"}` and authz rule `{verb: "read", resource: "widgets"}`. |
| FR-016 | Add `rpc BatchDeleteWidgets` to `WidgetService` returning `google.protobuf.Empty`, with HTTP annotation `{post: "/v1/widgets:batchDelete", body: "*"}` and authz rule `{verb: "delete", resource: "widgets"}`. |
| FR-017 | Add `import "google/protobuf/empty.proto";` to the toy proto for the `google.protobuf.Empty` return type. |
| FR-018 | Re-run `buf generate` from the toy fixture directory; commit regenerated `*.pb.go` and `*.pb.gw.go`. The regenerated `widgets.svc.go` must include `ArchiveWidget`, `BatchGetWidgets`, `BatchDeleteWidgets` in the boot-gate method list. |

### `testdata/toy/` handler

| ID | Requirement |
|----|-------------|
| FR-020 | Implement `ArchiveWidget(ctx, req)` in the toy handler: call `repo.Get(ctx, req.Id)` → set `w.ArchivedTime = timestamppb.Now()` → call `repo.Update(ctx, req.Id, w)` → return `&ArchiveWidgetResponse{Widget: w}`. Return errors from Get/Update unchanged (the ErrorMapper will translate). |
| FR-021 | Implement `BatchGetWidgets(ctx, req)` in the toy handler: call `repo.BatchGet(ctx, req.Ids)` → return `&BatchGetWidgetsResponse{Widgets: widgets}`. |
| FR-022 | Implement `BatchDeleteWidgets(ctx, req)` in the toy handler: call `repo.BatchDelete(ctx, req.Ids)` → on success return `&emptypb.Empty{}`. |

### Tests

| ID | Requirement |
|----|-------------|
| FR-030 | `persistence/memory_test.go`: add `TestMemoryRepository_BatchGet_Success`, `TestMemoryRepository_BatchGet_EmptyKeys`, `TestMemoryRepository_BatchGet_MissingKey`, `TestMemoryRepository_BatchDelete_Success`, `TestMemoryRepository_BatchDelete_EmptyKeys`, `TestMemoryRepository_BatchDelete_MissingKey`, `TestMemoryRepository_BatchDelete_AlreadyDeleted`. |
| FR-031 | `testdata/toy/widgetsv1/server_test.go`: add `TestArchiveWidget`, `TestBatchGetWidgets`, `TestBatchDeleteWidgets` integration tests using the existing `startTestServer` pattern. |

---

## Failure modes

| ID | Mode | Mitigation |
|----|------|------------|
| FM-001 | `BatchGet` with empty `keys` — caller expects an empty list, not an error. | Guard: `if len(keys) == 0 { return []T{}, nil }`. |
| FM-002 | `BatchGet` with one or more missing keys — must not return partial results. | Pre-check all keys under read-lock; return `ErrNotFound` on first miss before collecting any results. |
| FM-003 | `BatchDelete` with empty `keys` — no-op, return `nil`. | Guard: `if len(keys) == 0 { return nil }`. |
| FM-004 | `BatchDelete` with some keys missing — must not partially delete the others. | Two-pass under write-lock: validate all, then mutate all; any failure in validate pass aborts without mutation. |
| FM-005 | `BatchDelete` on already-soft-deleted keys — treated as not found (consistent with single Delete behavior). | Pre-check pass: `if r.deleted[key]` → return `ErrNotFound`. |
| FM-006 | `ArchiveWidget` on a soft-deleted widget — Get returns `ErrNotFound`; handler propagates; ErrorMapper emits `codes.NotFound`. | No special handling; falls through existing error path. |
| FM-007 | `ArchiveWidget` called twice on the same widget — idempotent: second call overwrites `archived_time` with a newer timestamp. | No uniqueness guard needed; Update replaces the entity. |
| FM-008 | Regenerated `widgets.svc.go` must declare all eight methods (5 original + 3 new) in the boot-gate; missing declaration causes a boot failure. | Gate is enforced at startup via `AssertMethodsDeclared`; tests won't start if this fails. |

---

## Acceptance criteria

### `BatchRepository` + `MemoryRepository`

| ID | Criterion |
|----|-----------|
| AC-001 | `BatchGet` with two existing IDs returns both items in the same order as the input keys. |
| AC-002 | `BatchGet` with an empty slice returns an empty slice and no error. |
| AC-003 | `BatchGet` where the second key does not exist returns `ErrNotFound` and no items. |
| AC-004 | `BatchDelete` on two existing non-deleted items: both are subsequently unreachable via `Get` (`ErrNotFound`). |
| AC-005 | `BatchDelete` with an empty slice returns `nil` without modifying any state. |
| AC-006 | `BatchDelete` where one key does not exist returns `ErrNotFound`; the other item is NOT deleted (atomic). |
| AC-007 | `BatchDelete` where one key is already soft-deleted returns `ErrNotFound`; the other item is NOT deleted. |

### Integration — toy fixture

| ID | Criterion |
|----|-----------|
| AC-010 | `ArchiveWidget` on an existing widget returns `ArchiveWidgetResponse` with `Widget.ArchivedTime` non-nil. |
| AC-011 | `ArchiveWidget` on a non-existent widget returns `codes.NotFound`. |
| AC-012 | `BatchGetWidgets` with two widget IDs returns `BatchGetWidgetsResponse` with both widgets. |
| AC-013 | `BatchGetWidgets` with one missing ID returns `codes.NotFound`. |
| AC-014 | `BatchDeleteWidgets` with two widget IDs returns `google.protobuf.Empty`; subsequent `GetWidget` on either ID returns `codes.NotFound`. |
| AC-015 | `BatchDeleteWidgets` with one non-existent ID returns `codes.NotFound`; the other widget is NOT deleted (atomic). |
| AC-016 | The toy server starts successfully with all eight methods declared in authz rules (boot-gate passes). |

---

## Out of scope for F022

- `BatchCreate` and `BatchUpdate` methods.
- Partial-success batch semantics (returning what succeeded alongside what failed).
- DB-level batch queries (the SDK's batch ops are a service-layer abstraction over single-row ops
  in `MemoryRepository`; production implementations using GORM/ent/sqlc can push the batch to SQL).
- Long-running operations wrapper for large batches.
- AIP-152 cancellation support for batch methods.
