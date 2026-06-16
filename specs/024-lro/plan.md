# F024 Plan — AIP-151 LRO

## Approach

Single new `lro` package. No new proto types or module dependencies: `*anypb.Any` for
metadata/response, `*spb.Status` for error. The toy fixture demonstrates the pattern with
its own proto `OperationStatus` message (avoids adding google.longrunning dependency).

## Task breakdown

See `tasks.md`.

## Sequence

1. `lro` package: Operation type + Store interface + MemoryStore + Manager + WaitOperation.
2. Wire `LROStore` into `server.Config` / `server.New`.
3. Toy fixture: proto additions (`ProcessWidgetRequest`, `OperationStatus`, `GetOperationStatus`),
   regenerate, implement handlers.
4. Integration test (AC-007/AC-008).
5. Build + vet + tests green; mark tracker done.
