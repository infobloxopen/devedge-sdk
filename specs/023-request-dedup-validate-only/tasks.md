# F023 Tasks — Request Deduplication + Validate-Only

## Middleware: validate-only (AIP-163)

- [ ] T-001 [S] Add `validateOnlyKey` context key and `ValidateOnlyFromContext(ctx) bool` helper to `middleware/validateonly.go`
- [ ] T-002 [S] Add `ValidateOnlyUnary()` interceptor to `middleware/validateonly.go`: interface-assert `GetValidateOnly() bool`, store in context, call handler
- [ ] T-003 [S] Unit tests in `middleware/validateonly_test.go`: AC-001, AC-002

## Middleware: deduplication (AIP-155)

- [ ] T-004 [S] Add `DeduplicationStore` interface and `MemoryDeduplicationStore` (mutex + map + TTL, `NewMemoryDeduplicationStore(ttl)`) to `middleware/dedup.go`
- [ ] T-005 [S] Add `DeduplicateUnary(store)` interceptor to `middleware/dedup.go`: interface-assert `GetRequestId() string`, skip on validate-only, load/store logic; never cache errors
- [ ] T-006 [S] Unit tests in `middleware/dedup_test.go`: AC-004, AC-005, AC-006, AC-007, AC-008, AC-009, AC-010

## Server wiring

- [ ] T-007 [S] Add `DeduplicationStore middleware.DeduplicationStore` field to `server.Config`; auto-initialize to `MemoryDeduplicationStore` (default TTL) in `server.New` when nil; append `ValidateOnlyUnary` and `DeduplicateUnary(store)` to the interceptor chain (after `ReadMaskUnary`)

## Toy fixture

- [ ] T-008 [S] Add `string request_id = 2` and `bool validate_only = 3` to `CreateWidgetRequest` in `testdata/toy/widgets.proto`; regenerate `testdata/toy/widgetsv1/` with `buf generate --template buf.gen.toy.yaml`
- [ ] T-009 [S] Update `CreateWidget` handler in `testdata/toy/widgetsv1/widgets.svc.go` to skip `repo.Create` when `middleware.ValidateOnlyFromContext(ctx)` is true; return the constructed widget directly

## Integration tests

- [ ] T-010 [S] Add integration tests to `testdata/toy/server_test.go` covering AC-003, AC-006, AC-007, AC-008, AC-009, AC-011

## Verification

- [ ] T-011 [S] Run `go test ./...` from repo root and from `testdata/toy/`; confirm all tests pass; run `/verify-change`
