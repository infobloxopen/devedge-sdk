---
name: add-feature
description: Add a new feature package or interceptor to devedge-sdk — new AIP semantics, new middleware, or a new SDK abstraction. Use this when implementing a new Fx (e.g. F026).
---

# Add a feature to devedge-sdk

## 1. Locate it

Check the layout in `AGENTS.md`. Middleware interceptors belong in `middleware/`; storage
helpers in `persistence/`; async operation patterns in `lro/`; new AIP support with its own
state may warrant a new top-level package (e.g. `lro/` for AIP-151/152). New plugins go
under `cmd/`.

## 2. Interface before implementation

Define public interface types and sentinel errors first. The interface should be injected
(via `server.Config`), not called directly inside other packages.

## 3. Core-cleanliness check

If the feature touches `authz/`, `authz/grpcauthz/`, or `persistence/`: zero ORM or
policy-engine imports. If it needs one, it goes in an adapter outside the module.

## 4. Wire into the server

Add to `server/server.go`:
- A `Config` field (nil-safe; auto-initialize with `New*` default if nil)
- An accessor method (e.g. `s.LROStore()`)
- Chain the interceptor in the right slot (check `server.go` for the existing order)

## 5. Exercise in the toy fixture

Extend `testdata/toy/widgets.proto` with a representative RPC (if the feature is
proto-driven), run `make generate`, and add handler + integration tests in
`testdata/toy/server_test.go`. The toy is the integration gate — if it is not
exercised there, the feature is not verifiable.

## 6. Unit tests in the feature package

For Store/Manager/interceptor: cover the happy path, the error paths, and any
concurrency invariants (idempotent ops, cancellation).

## 7. Gate

```
make build && make vet && make test
cd testdata/toy && go test ./...
make security-check
```

All must be green before marking done.

## 8. Spec bookkeeping

Mark the feature done in `specs/aip-gap-tracker.md` (hub repo) and update
`work/WS-002-brief.md` with a done note.
