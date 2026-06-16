# F024 — AIP-151 Long-Running Operations (LRO)

**Status:** in progress
**AIP:** [AIP-151](https://google.aip.dev/151)
**Branch:** `024-lro`

---

## Problem

Some API methods (e.g., bulk-process, async resource creation, long-running jobs) cannot
complete within a single request/response cycle. AIP-151 defines the `Operation` resource
pattern: a method starts the work and immediately returns a *pending* operation resource; the
caller polls `GetOperation` until `done=true` carries either a response or an error.

The framework has no support for this pattern today. Callers must roll their own async tracking.

---

## Goal

Add an `lro` package to devedge-sdk that:
1. Provides the `Operation` type (AIP-151 shape, `operations/{uuid}` names).
2. Provides a `Store` interface + `MemoryStore` (goroutine-safe, TTL on completed ops).
3. Provides a `Manager` that accepts a handler func, runs it async, and tracks the operation.
4. Adds a `WaitOperation` polling helper (context-aware, exponential back-off).
5. Wires into `server.Config` (`LROStore`) and exposes `GetOperation`/`ListOperations` as a
   gRPC service on the toy server fixture.

No new module dependencies: metadata/response use `*anypb.Any` (already in go.mod via
`google.golang.org/protobuf`); errors use `*statuspb.Status` (already in go.mod via
`google.golang.org/grpc`).

---

## Acceptance criteria

| ID | Criterion |
|----|-----------|
| AC-001 | `lro.Operation` has `Name string`, `Done bool`, `Metadata *anypb.Any`, `Response *anypb.Any`, `Error *spb.Status`, `CreateTime time.Time`, `UpdateTime time.Time`. |
| AC-002 | `lro.Store` interface: `Create(ctx, *Operation) error`, `Get(ctx, name string) (*Operation, error)`, `Update(ctx, *Operation) error`, `Delete(ctx, name string) error`, `List(ctx) ([]*Operation, error)`. |
| AC-003 | `lro.MemoryStore` is goroutine-safe; completed (done) operations expire and are purged after a configurable TTL (default 1h). Pending operations do not expire. |
| AC-004 | `lro.Manager.Submit(ctx, metadata any, fn func(context.Context) (any, error)) (*Operation, error)` creates a pending operation (name = `operations/{uuid}`), runs `fn` in a goroutine with a fresh `context.Background()` (not tied to the gRPC request — AIP-151 work outlives the RPC), updates the operation to done on completion (sets Response or Error). If `ctx` is already cancelled when Submit is called, returns an error without creating the operation. |
| AC-005 | `lro.WaitOperation(ctx, store Store, name string, poll time.Duration) (*Operation, error)` blocks until `done=true` (capped by ctx), returns the final operation. |
| AC-006 | `server.Config` gains `LROStore lro.Store`; when nil, `New` auto-initialises `lro.NewMemoryStore(1*time.Hour)`. |
| AC-007 | `testdata/toy`: add `ProcessWidget(ProcessWidgetRequest) returns (OperationStatus)` custom method with `verb:"write"` authz rule; `OperationStatus` is a toy-proto message `{name string, done bool, result string}`; handler submits work to `Manager`, returns pending status immediately; after ~50ms background work updates the operation. |
| AC-008 | Integration test exercises the full lifecycle: `ProcessWidget` → `GetOperationStatus` → operation is eventually done; test polls up to 500ms using `WaitOperation`. |
| AC-009 | `go build ./...` and `go vet ./...` are clean; all existing tests still pass. |

---

## Failure modes

- **Race in MemoryStore:** all mutations hold the mutex; no read without lock.
- **Request context cancel kills work:** fn must NOT run with the gRPC request context; it runs
  with `context.Background()` so the work outlives the RPC. If `ctx` is already done at Submit
  time, return an error immediately (don't create a ghost operation).
- **WaitOperation infinite loop:** the loop respects `ctx` deadline/cancel and returns immediately
  when ctx expires.
- **Double-complete:** `Update` on an already-done operation is a no-op (idempotent).

---

## Out of scope

- `CancelOperation` / `DeleteOperation` gRPC endpoints (deferred; needs AIP-152).
- `ListOperations` pagination (deferred; MemoryStore.List returns all).
- Persistent operation store (MemoryStore is dev/test; production uses the Store interface).
- Using `google.longrunning.Operation` proto type (deferred; avoids new module dep for now).
