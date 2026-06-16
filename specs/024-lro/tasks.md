# F024 Tasks — AIP-151 LRO

- [S] T01: Create `lro/operation.go` — `Operation` struct, `ErrNotFound` sentinel, name helper `OperationName(uuid)`.
- [S] T02: Create `lro/store.go` — `Store` interface + `MemoryStore` (mutex-protected map, TTL on completed ops, `NewMemoryStore(ttl time.Duration)`).
- [S] T03: Create `lro/manager.go` — `Manager` struct wrapping a `Store`; `Submit(ctx, metadata proto.Message, fn)` creates pending op, launches goroutine, updates on completion; goroutine selects on ctx.Done() to mark cancelled.
- [S] T04: Create `lro/wait.go` — `WaitOperation(ctx, store, name, poll)` polls until done or ctx expires.
- [S] T05: Create `lro/lro_test.go` — unit tests: MemoryStore CRUD + TTL expiry; Manager submit+complete; Manager ctx-cancel marks error; WaitOperation success + timeout.
- [S] T06: Wire `server.Config.LROStore lro.Store` into `server/server.go`; auto-init `lro.NewMemoryStore(1h)` when nil in `New`; no behaviour change for existing interceptors.
- [S] T07: Add `ProcessWidgetRequest` (field `id string`), `OperationStatus` (`name`, `done`, `result`), `GetOperationStatusRequest` (`name`), and `ProcessWidget`/`GetOperationStatus` RPC stubs to `testdata/toy/widgets.proto`; add `google.api.http` annotations; run `buf generate` (or `make generate`) to regenerate.
- [S] T08: Implement `ProcessWidget` handler in toy server fixture: submit work to `Manager` (50ms fake delay, sets result to `"processed:<id>"`), return pending `OperationStatus`; implement `GetOperationStatus` handler (reads from store, maps to `OperationStatus`); register both in the toy `Register` func.
- [S] T09: Add integration test `TestLRO_ProcessWidget` in `testdata/toy/server_test.go`: call `ProcessWidget`, assert `done=false`; poll with `WaitOperation`; assert `done=true` and result is `"processed:<id>"`.
- [S] T10: `go build ./... && go vet ./...` clean; `go test ./...` all pass; update AIP gap tracker and WS-002 brief.
