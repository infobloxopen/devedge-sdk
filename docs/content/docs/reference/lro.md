---
title: lro
weight: 7
---

```go
import "github.com/infobloxopen/devedge-sdk/lro"
```

Package `lro` implements the **AIP-151 long-running operation** pattern: a server-side primitive for
work that outlives a single request (exports, imports, migrations). A handler starts the work, gets
back an `Operation` immediately with `Done=false`, and the client polls until it completes.

`lro` is a **runtime helper, not a codegen surface** — there is no proto annotation that turns an RPC
into an LRO. You start async work from inside an ordinary handler with a `Manager`, and you expose
the operation lifecycle (get / list / cancel) through RPCs you declare yourself (a
`google.longrunning.Operations`-style service, or your own). The server holds the store for you; the
wiring of operation-facing RPCs is the consumer's.

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

## Starting async work from a handler

`Manager.Submit` records a pending operation and runs `fn` on a **background-derived context** (so
the work outlives the gRPC call per AIP-151) that `Manager.Cancel` can still cancel. It returns the
pending `Operation` immediately:

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

If `fn` returns a value it lands in `Operation.Response`; if it returns an error, in `Operation.Err`.
`MemoryStore.Update` is idempotent, so a cancelled operation is not overwritten by a late completion.

## Polling, waiting, and cancelling

```go
// Block until done (or ctx deadline). poll defaults to 100ms when <= 0.
final, err := lro.WaitOperation(ctx, store, op.Name, 200*time.Millisecond)

// One-shot status for a GetOperation handler.
op, err := store.Get(ctx, name) // lro.ErrNotFound if unknown/expired

// Cancel: marks the operation done with ErrCancelled and signals the goroutine.
err := mgr.Cancel(ctx, name)    // lro.ErrAlreadyDone if it already completed
```

## Wiring the store on the server

`server.New` provides a store so you don't have to construct one for the common case:

```go
srv, _ := server.New(server.Config{
    // ...
    LROStore: lro.NewMemoryStore(time.Hour), // optional; this is also the default when nil
})

store := srv.LROStore()       // the configured store
mgr := lro.NewManager(store)  // build a Manager over it for your handlers
```

The default `LROStore` is `lro.NewMemoryStore(1h)` — completed operations are purged an hour after
they finish. Swap in a persistent `Store` implementation for operations that must survive a restart.

{{< callout type="info" >}}
**Scope.** The SDK ships the operation *engine* (Manager / Store / Operation / cancellation), not a
generated Operations API. To expose operations over gRPC + the HTTP gateway, declare `GetOperation`,
`ListOperations`, and `CancelOperation` RPCs (mapping to `Manager.Store()` / `Manager.Cancel`) and a
proto `Operation` message you map to/from `lro.Operation`. AIP-151's `google.longrunning.Operations`
is the canonical shape to model them on.
{{< /callout >}}
