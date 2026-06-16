# F022 Tasks

Each task produces a verifiable diff. Tags: `[S]` = mechanical (Sonnet),
`[C]` = judgment/ripple/template logic (Opus). Sequencing is governed by
`spec.md` Design section.

## Phase 1 — BatchRepository interface + MemoryRepository batch ops

- [ ] [S] T001: In `persistence/repository.go`, add `BatchRepository[T any, K comparable]`
  interface below the existing `Repository` interface. It embeds `Repository[T, K]` and adds:
  - `BatchGet(ctx context.Context, keys []K) ([]T, error)` — returns items in key order; `ErrNotFound` if any key missing/deleted.
  - `BatchDelete(ctx context.Context, keys []K) error` — atomic soft-delete; `ErrNotFound` if any key missing/already-deleted.
  Add a doc comment explaining atomic semantics. (FR-001)

- [ ] [S] T002: In `persistence/memory.go`, implement `BatchGet` on `MemoryRepository`:
  - Empty `keys` → return `([]T{}, nil)` (FM-001).
  - Acquire `r.mu.RLock()`.
  - For each key: if `!r.items[key]` present OR `r.deleted[key]` → release lock, return `ErrNotFound` (FM-002).
  - Collect items in key order into `[]T`.
  - Release lock, return items.
  (FR-002, AC-001, AC-002, AC-003)

- [ ] [S] T003: In `persistence/memory.go`, implement `BatchDelete` on `MemoryRepository`:
  - Empty `keys` → return `nil` (FM-003).
  - Acquire `r.mu.Lock()`.
  - Pre-check pass: for each key, if not in `r.items` OR `r.deleted[key]` → release lock, return `ErrNotFound` (FM-004, FM-005).
  - Mutation pass: for each key, `r.deleted[key] = true`.
  - Release lock, return `nil`.
  (FR-003, AC-004, AC-005, AC-006, AC-007)

- [ ] [S] T004: Add unit tests to `persistence/memory_test.go`:
  - `TestMemoryRepository_BatchGet_Success`: create two widgets A and B; `BatchGet([A,B])` returns both in order.
  - `TestMemoryRepository_BatchGet_EmptyKeys`: `BatchGet([])` returns empty slice, no error.
  - `TestMemoryRepository_BatchGet_MissingKey`: create A; `BatchGet([A, "missing"])` returns `ErrNotFound`.
  - `TestMemoryRepository_BatchGet_SoftDeletedKey`: create A, Delete A; `BatchGet([A])` returns `ErrNotFound`.
  - `TestMemoryRepository_BatchDelete_Success`: create A and B; `BatchDelete([A,B])` returns nil; `Get(A)` and `Get(B)` return `ErrNotFound`.
  - `TestMemoryRepository_BatchDelete_EmptyKeys`: `BatchDelete([])` returns nil, no state change.
  - `TestMemoryRepository_BatchDelete_MissingKey`: create A; `BatchDelete([A,"missing"])` returns `ErrNotFound`; A is NOT deleted.
  - `TestMemoryRepository_BatchDelete_AlreadyDeleted`: create A, Delete A; `BatchDelete([A])` returns `ErrNotFound`.
  (FR-030, AC-001–AC-007)

## Phase 2 — Toy fixture proto additions

- [ ] [C] T005: Update `testdata/toy/widgets.proto`:
  - Add `import "google/protobuf/empty.proto";`.
  - Add `google.protobuf.Timestamp archived_time = 8 [(google.api.field_behavior) = OUTPUT_ONLY];` to `Widget`.
  - Add messages: `ArchiveWidgetRequest { string id = 1; }`, `ArchiveWidgetResponse { Widget widget = 1; }`.
  - Add messages: `BatchGetWidgetsRequest { repeated string ids = 1; }`, `BatchGetWidgetsResponse { repeated Widget widgets = 1; }`.
  - Add message: `BatchDeleteWidgetsRequest { repeated string ids = 1; }`.
  - Add `rpc ArchiveWidget(ArchiveWidgetRequest) returns (ArchiveWidgetResponse)` with:
    - `(google.api.http) = {post: "/v1/widgets/{id}:archive", body: "*"}`.
    - `(infoblox.authz.v1.rule) = {verb: "archive", resource: "widgets"}`.
  - Add `rpc BatchGetWidgets(BatchGetWidgetsRequest) returns (BatchGetWidgetsResponse)` with:
    - `(google.api.http) = {get: "/v1/widgets:batchGet"}`.
    - `(infoblox.authz.v1.rule) = {verb: "read", resource: "widgets"}`.
  - Add `rpc BatchDeleteWidgets(BatchDeleteWidgetsRequest) returns (google.protobuf.Empty)` with:
    - `(google.api.http) = {post: "/v1/widgets:batchDelete", body: "*"}`.
    - `(infoblox.authz.v1.rule) = {verb: "delete", resource: "widgets"}`.
  (FR-010–FR-017)

- [ ] [C] T006: Re-run `buf generate` from the toy fixture:
  - Run from `testdata/toy` (or root if the buf config covers it): `buf generate`.
  - Inspect `git diff` on regenerated files:
    - `widgetsv1/widgets.pb.go`: Widget gains `ArchivedTime`; new request/response types present.
    - `widgetsv1/widgets_grpc.pb.go`: new `ArchiveWidget`, `BatchGetWidgets`, `BatchDeleteWidgets` method stubs.
    - `widgetsv1/widgets.pb.gw.go`: new HTTP route handlers for `:archive`, `:batchGet`, `:batchDelete`.
    - `widgetsv1/widgets.svc.go`: regenerated with 8 methods in boot-gate (5 original + 3 new). Verify manually.
  - Commit regenerated files.
  (FR-018, AC-016)

## Phase 3 — Handler implementation + integration tests

- [ ] [S] T007: In `testdata/toy/widgetsv1/server_test.go`, add the three handler method
  implementations to `toyHandler`:

  ```go
  func (h *toyHandler) ArchiveWidget(ctx context.Context, req *widgetsv1.ArchiveWidgetRequest) (*widgetsv1.ArchiveWidgetResponse, error) {
      w, err := h.repo.Get(ctx, req.Id)
      if err != nil { return nil, err }
      w.ArchivedTime = timestamppb.Now()
      w, err = h.repo.Update(ctx, req.Id, w)
      if err != nil { return nil, err }
      return &widgetsv1.ArchiveWidgetResponse{Widget: w}, nil
  }

  func (h *toyHandler) BatchGetWidgets(ctx context.Context, req *widgetsv1.BatchGetWidgetsRequest) (*widgetsv1.BatchGetWidgetsResponse, error) {
      widgets, err := h.repo.BatchGet(ctx, req.Ids)
      if err != nil { return nil, err }
      return &widgetsv1.BatchGetWidgetsResponse{Widgets: widgets}, nil
  }

  func (h *toyHandler) BatchDeleteWidgets(ctx context.Context, req *widgetsv1.BatchDeleteWidgetsRequest) (*emptypb.Empty, error) {
      if err := h.repo.BatchDelete(ctx, req.Ids); err != nil { return nil, err }
      return &emptypb.Empty{}, nil
  }
  ```

  Add import `"google.golang.org/protobuf/types/known/timestamppb"` and
  `emptypb "google.golang.org/protobuf/types/known/emptypb"` as needed.
  (FR-020, FR-021, FR-022)

- [ ] [S] T008: Add integration tests to `testdata/toy/widgetsv1/server_test.go`:

  `TestArchiveWidget`:
  - Create widget W.
  - Call `ArchiveWidget({id: W.Id})`.
  - Assert `resp.Widget.ArchivedTime` is non-nil.
  - Assert `resp.Widget.Id == W.Id`.
  (AC-010)

  `TestArchiveWidget_NotFound`:
  - Call `ArchiveWidget({id: "nonexistent"})`.
  - Assert `status.Code(err) == codes.NotFound`.
  (AC-011)

  `TestBatchGetWidgets`:
  - Create widgets A and B.
  - Call `BatchGetWidgets({ids: [A.Id, B.Id]})`.
  - Assert `resp.Widgets` has length 2; both IDs present.
  (AC-012)

  `TestBatchGetWidgets_MissingId`:
  - Create widget A.
  - Call `BatchGetWidgets({ids: [A.Id, "missing"]})`.
  - Assert `status.Code(err) == codes.NotFound`.
  (AC-013)

  `TestBatchDeleteWidgets`:
  - Create widgets A and B.
  - Call `BatchDeleteWidgets({ids: [A.Id, B.Id]})`.
  - Assert no error.
  - Call `GetWidget({id: A.Id})` → assert `codes.NotFound`.
  - Call `GetWidget({id: B.Id})` → assert `codes.NotFound`.
  (AC-014)

  `TestBatchDeleteWidgets_MissingId_IsAtomic`:
  - Create widget A.
  - Call `BatchDeleteWidgets({ids: [A.Id, "missing"]})`.
  - Assert `codes.NotFound`.
  - Call `GetWidget({id: A.Id})` → assert success (A was NOT deleted).
  (AC-015)

  (FR-031, AC-010–AC-015)

## Phase 4 — Verification gate

- [ ] [C] T009: Run the full verification gate:
  - `go build ./...` from root — must compile clean.
  - `go vet ./...` from root — zero findings.
  - `go test ./persistence/...` — all new batch tests pass.
  - `go test ./...` from `testdata/toy` — all new integration tests pass, existing tests unchanged.
  - `go test ./...` from `testdata/apikey` — existing tests unaffected.
  - `git diff --stat` scope check: only expected files changed; no unintended generated churn.
