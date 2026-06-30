---
title: lro
weight: 7
---

```go
import "github.com/infobloxopen/devedge-sdk/lro"
```

Package `lro` implements the [AIP-151](https://google.aip.dev/151) long-running operation (LRO) pattern. It gives you a server-side store and manager for work that outlives a single request — exports, imports, migrations, and similar jobs. A handler starts the work and immediately returns an `Operation` with `Done=false`; the client polls until the operation completes.

Use `lro` when an RPC handler needs to start asynchronous work and let the client track progress separately. The server holds the LRO store for you; wiring the operation-facing RPCs is your responsibility.

`lro` is a runtime helper, not a codegen surface: there is no proto annotation that turns an RPC into an LRO. You start async work from inside an ordinary handler with a `Manager`, and you expose the operation lifecycle (get, list, cancel) through RPCs you declare yourself.

{{< callout type="info" >}}
**The SDK provides the operation engine, not a generated API.** `lro` ships the `Manager`, `Store`, `Operation`, and cancellation primitives. To expose operations over gRPC and the HTTP gateway, declare `GetOperation`, `ListOperations`, and `CancelOperation` RPCs that map to `Manager.Store()` and `Manager.Cancel()`, and a proto `Operation` message that you map to and from `lro.Operation`. [AIP-151's `google.longrunning.Operations`](https://google.aip.dev/151) is the canonical shape to model them on.
{{< /callout >}}

## Types

```go
// An operation resource. Name is always "operations/{uuid}".
type Operation struct {
    Name       string
    Done       bool
    Metadata   any       // your progress/metadata value
    Response   any       // set when Done and successful
    Err        error     // set when Done and failed (or ErrCancelled)
    CreateTime time.Time
    UpdateTime time.Time
}

func OperationName(id string) string // "operations/<id>"

// Persistence seam for operations.
type Store interface {
    Create(ctx context.Context, op *Operation) error
    Get(ctx context.Context, name string) (*Operation, error)
    Update(ctx context.Context, op *Operation) error
    Cancel(ctx context.Context, name string) error // atomically marks done w/ ErrCancelled
    Delete(ctx context.Context, name string) error
    List(ctx context.Context) ([]*Operation, error)
}

// In-memory Store: completed operations expire after ttl; pending ones never expire.
func NewMemoryStore(ttl time.Duration) *MemoryStore

// Creates and tracks operations, including cancellation.
type Manager struct{ /* ... */ }
func NewManager(store Store) *Manager
func (m *Manager) Store() Store
func (m *Manager) Submit(ctx context.Context, metadata any, fn func(context.Context) (any, error)) (*Operation, error)
func (m *Manager) Cancel(ctx context.Context, name string) error

// Client/poller helper.
func WaitOperation(ctx context.Context, store Store, name string, poll time.Duration) (*Operation, error)

// Errors.
var ErrNotFound    // operation name unknown
var ErrAlreadyDone // Cancel on a completed operation
var ErrCancelled   // set as Operation.Err when cancelled before completion
```

## Starting async work

`Manager.Submit` records a pending operation and runs `fn` on a background-derived context, so the work outlives the gRPC call as AIP-151 requires. The background context remains cancellable via `Manager.Cancel`. `Submit` returns the pending `Operation` immediately:

```go
type exportServer struct {
    mgr  *lro.Manager
    repo *exportsv1.ExportRepository
}

func (s *exportServer) CreateExport(ctx context.Context, req *pb.CreateExportRequest) (*pb.Operation, error) {
    op, err := s.mgr.Submit(ctx, &pb.ExportMetadata{Target: req.Export.DestinationId},
        func(workCtx context.Context) (any, error) {
            // Long work runs here; honor workCtx cancellation.
            return s.runExport(workCtx, req.Export)
        })
    if err != nil {
        return nil, err
    }
    return toProtoOperation(op), nil // your mapping from lro.Operation → your proto Operation
}
```

When `fn` returns a value, it lands in `Operation.Response`. When `fn` returns an error, it lands in `Operation.Err`. `MemoryStore.Update` is idempotent, so a cancelled operation is not overwritten by a late completion.

## Polling and cancelling

Three operations cover an operation's lifecycle: `WaitOperation` blocks until the operation is done, `store.Get` reads its current status once, and `mgr.Cancel` stops it.

```go
// Block until done (or ctx deadline). poll defaults to 100ms when <= 0.
final, err := lro.WaitOperation(ctx, store, op.Name, 200*time.Millisecond)

// One-shot status for a GetOperation handler.
op, err := store.Get(ctx, name) // lro.ErrNotFound if unknown/expired

// Cancel: marks the operation done with ErrCancelled and signals the goroutine.
err := mgr.Cancel(ctx, name)    // lro.ErrAlreadyDone if it already completed
```

## Store configuration

`server.New` provides a default store so you do not need to construct one for the common case:

```go
srv, _ := server.New(server.Config{
    // ...
    LROStore: lro.NewMemoryStore(time.Hour), // optional; this is also the default when nil
})

store := srv.LROStore()       // the configured store
mgr := lro.NewManager(store)  // build a Manager over it for your handlers
```

The default `LROStore` is `lro.NewMemoryStore(1h)`: completed operations are purged one hour after they finish. Swap in a persistent `Store` implementation for operations that must survive a process restart.
