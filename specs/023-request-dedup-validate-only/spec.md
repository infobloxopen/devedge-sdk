# F023 — Request Deduplication (AIP-155) + Validate-Only (AIP-163)

**AIPs**: AIP-155 (request deduplication), AIP-163 (validate-only)
**Status**: done
**Branch**: `023-request-dedup-validate-only`

---

## Problem statement

Two API ergonomics gaps remain after F022:

1. **No validate-only support (AIP-163).** Callers have no way to dry-run a mutation request
   without committing side effects. This is critical during client development and for operations
   workflows that want to preflight a batch of changes. Without it, every mistake in a Create or
   Update leaves behind real state that must be cleaned up manually.

2. **No idempotent mutation support (AIP-155).** Callers that retry a mutation on network failure
   risk duplicating resources. AIP-155's `request_id` field gives the SDK a safe retry primitive:
   the server deduplicates identical requests within a time window and returns the original
   response, making retries transparent to callers. This is distinct from the existing
   `x-request-id` tracing header, which is for observability only and is not idempotency-safe.

---

## Goals

- **G-001** Add `ValidateOnlyUnary()` interceptor: extract `validate_only: bool` from the request
  (via Go interface assertion) and store it in context. Handlers check
  `middleware.ValidateOnlyFromContext(ctx)` to decide whether to skip persistence.
- **G-002** Add `ValidateOnlyFromContext(ctx)` context helper so any handler or downstream
  component can read the flag without coupling to the proto type.
- **G-003** Define `DeduplicationStore` interface (`Load(id) (any, bool)` /
  `Store(id, response any)`) and a `MemoryDeduplicationStore` implementation backed by a
  `sync.Mutex`-protected map with per-entry TTL (default 10 min, configurable).
- **G-004** Add `DeduplicateUnary(store)` interceptor: check `request_id: string` in the request
  (via interface assertion), serve cached response on hit, execute and cache on miss.
  Skip entirely when `validate_only` is set (dry-run responses must not populate the cache).
- **G-005** Wire `ValidateOnlyUnary` and `DeduplicateUnary` into the framework chain in
  `server/server.go` (both at the end, after `ReadMaskUnary`, in that order).
- **G-006** Extend the toy fixture `CreateWidgetRequest` with `request_id` and `validate_only`
  fields; update `CreateWidget` handler to respect the validate-only flag.
- **G-007** Add integration tests covering all acceptance criteria (see below).

### Non-goals

- Persistent (DB-backed) dedup store — `MemoryDeduplicationStore` is the dev/test default;
  production implementations swap in via the `DeduplicationStore` interface.
- Dedup for streaming RPCs — unary only.
- Caching error responses — only successful (nil-error) handler results are cached.
- `validate_only` on read methods — reads have no side effects; the flag is irrelevant.
- Propagating `validate_only` through `DeduplicationStore` (the interceptor skips the store
  entirely, so the store never sees dry-run responses).
- Adding `request_id` / `validate_only` to every fixture method — demonstrating the pattern on
  `CreateWidget` is sufficient; teams add to their own methods following the same convention.

---

## Design

### AIP-163: validate-only

AIP-163 defines a `validate_only: bool` field that callers set on mutation requests to request a
dry run. The server must validate inputs and return either the would-be response or a validation
error — but must not commit any side effects.

**Interceptor pattern** — handler-cooperative:

The `ValidateOnlyUnary` interceptor uses the same interface-assertion style as `ReadMaskUnary`:

```go
type validateOnlyGetter interface{ GetValidateOnly() bool }
```

If the request implements the interface and returns `true`, the flag is stored in context. The
interceptor then calls the handler normally; the handler itself checks the flag before persisting.
This keeps the interceptor thin (context carrier only) and gives handlers full control over what
"no side effects" means for their operation.

Handlers call `middleware.ValidateOnlyFromContext(ctx)` — same pattern as
`etag.IfMatchFromContext` and `middleware.TenantIDFromContext`.

**Fixture: `CreateWidget` validate-only path**

When `validate_only=true`, the handler:
1. Validates the request (required fields, etc.) — same checks as normal.
2. Constructs the `Widget` response exactly as it would for a real create (fills `name`, `id`,
   `etag`, etc.).
3. Returns the response without calling `repo.Create(...)`.

A subsequent `GetWidget` on the would-be ID returns `NotFound`, proving no state was written.

### AIP-155: request deduplication

AIP-155 defines `request_id: string` on mutation requests. The server deduplicates requests
within a window: if it receives a request with a `request_id` it has seen recently, it returns
the cached response instead of re-executing.

**`DeduplicationStore` interface:**

```go
type DeduplicationStore interface {
    Load(requestID string) (any, bool)
    Store(requestID string, response any)
}
```

`MemoryDeduplicationStore` wraps `sync.Mutex` + `map[string]entry` where `entry` holds the
response and an expiry timestamp. `Load` evicts expired entries lazily. `Store` always overwrites.
Default TTL is 10 minutes; `NewMemoryDeduplicationStore(ttl time.Duration)` allows override.

**`DeduplicateUnary` interceptor logic:**

```
1. Cast req to getRequestIDer (GetRequestId() string).
   If the interface is missing or the string is empty → pass through (no dedup).
2. Check ValidateOnlyFromContext(ctx). If true → pass through (skip cache entirely).
3. Call store.Load(requestID).
   If found → return cached response directly (skip handler).
4. Call handler.
   If err != nil → return err (do not cache failures).
5. store.Store(requestID, resp).
6. Return resp, nil.
```

**Interface assertion, not proto reflection.** Both interceptors use Go interface assertions
(`GetRequestId() string`, `GetValidateOnly() bool`) rather than proto reflection. This is
consistent with how `ReadMaskUnary` checks `GetReadMask()` and `FieldMaskUnary` checks
`GetUpdateMask()`. Generated proto code already has these getters for free.

### Chain position

Updated chain in `server/server.go` (outermost → innermost):

```
RequestIDUnary
ErrorMapperUnary
TenantIDUnary
grpcauthz.UnaryServerInterceptor
FieldMaskUnary
etag.PreconditionUnary
ReadMaskUnary
ValidateOnlyUnary        ← new, F023
DeduplicateUnary(store)  ← new, F023
```

`ValidateOnlyUnary` runs before `DeduplicateUnary` so the dedup interceptor can read the flag
from context rather than re-inspecting the request.

`DeduplicateUnary` requires a `DeduplicationStore` to be injected via `server.Config`. A
`MemoryDeduplicationStore` with the default TTL is created automatically when `Config.DeduplicationStore`
is nil (zero-config default, matching the existing pattern for `Authorizer`).

### `server.Config` additions

```go
// DeduplicationStore is the idempotency store used by DeduplicateUnary.
// Defaults to a MemoryDeduplicationStore with a 10-minute TTL when nil.
DeduplicationStore middleware.DeduplicationStore
```

### Toy fixture proto additions

`CreateWidgetRequest` gains two new fields:

```proto
message CreateWidgetRequest {
  Widget widget       = 1;
  string request_id   = 2; // AIP-155: idempotency key; empty = no dedup.
  bool   validate_only = 3; // AIP-163: dry-run; validate but do not persist.
}
```

Field numbers 2 and 3 are unused in the existing message, so this is backward-compatible.

Regenerate `widgetsv1/` with `buf generate`.

### Fixture handler changes (`testdata/toy/widgetsv1/widgets.svc.go`)

`CreateWidget` (hand-maintained handler file — not generated):

```go
func (s *WidgetServer) CreateWidget(ctx context.Context, req *widgetsv1.CreateWidgetRequest) (*widgetsv1.Widget, error) {
    w := buildWidget(req.Widget) // existing construction logic

    if middleware.ValidateOnlyFromContext(ctx) {
        return w, nil // validate path: return without persisting
    }

    if err := s.repo.Create(ctx, w); err != nil {
        return nil, err
    }
    return w, nil
}
```

---

## Acceptance criteria

| ID | Criterion |
|----|-----------|
| AC-001 | `ValidateOnlyUnary` stores `true` in context when request has `validate_only=true`; context value is `false`/absent otherwise. |
| AC-002 | `ValidateOnlyFromContext` returns `false` on a plain context (no prior interceptor). |
| AC-003 | `CreateWidget` with `validate_only=true` returns a non-nil `Widget`; a subsequent `GetWidget` for the same ID returns `NotFound`. |
| AC-004 | `MemoryDeduplicationStore.Load` returns `(nil, false)` on a miss; `(response, true)` after a `Store`. |
| AC-005 | `MemoryDeduplicationStore` evicts entries after the TTL; a `Load` after expiry returns a miss. |
| AC-006 | `DeduplicateUnary`: two `CreateWidget` calls with the same non-empty `request_id` return identical responses; the second call does not create a second widget (store has exactly one entry). |
| AC-007 | `DeduplicateUnary`: two `CreateWidget` calls with different `request_id` values each execute independently (two distinct widgets are created). |
| AC-008 | `DeduplicateUnary`: `CreateWidget` with empty `request_id` always executes (no dedup); N calls create N widgets. |
| AC-009 | `DeduplicateUnary`: `CreateWidget` with `validate_only=true` and a non-empty `request_id` does not populate the cache; a subsequent real call with the same `request_id` executes normally. |
| AC-010 | `DeduplicateUnary`: a handler error (e.g., bad request) is never cached; the next call with the same `request_id` re-executes the handler. |
| AC-011 | Both interceptors are present in the default chain returned by `server.New`; `server.Config.DeduplicationStore` is auto-initialized when nil. |

---

## Failure modes

| Scenario | Expected behaviour |
|----------|--------------------|
| `request_id` set but `validate_only=true` | Dedup cache is skipped entirely (AC-009). |
| Handler panics on duplicate `request_id` call | Panic propagates as usual; nothing is cached for panicked calls (cache is written only after a nil-error return). |
| `DeduplicationStore` is nil | `server.New` auto-creates `MemoryDeduplicationStore` — `nil` store is never passed to `DeduplicateUnary`. |
| TTL elapsed between two retries | Miss on second call → handler re-executes → new cache entry; this is the expected idempotency-window expiry. |
| Two goroutines race on the same `request_id` | `MemoryDeduplicationStore` is mutex-protected; only one will get a miss and execute; the other may also get a miss (no in-flight coalescing in Phase 1 — both execute independently, last writer wins in the store). Document this limitation. |

---

## Task list

See `tasks.md`.
