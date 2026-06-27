# F037 — Resilience: request timeouts + rate limiting as policy interceptors, algorithms behind interfaces

**Status**: design locked. **Issue**: #94 (DX 058, P1). **Initiative**: WS-007.
**Depends on**: the default unary interceptor chain in `server/server.go`; coexists with #90 (logging/otel)
and #91 (health). **Pre-GA**: clean implementation over back-compat.

**Origin / user directive (load-bearing principle)**: "policy interceptors with the **algorithm behind an
interface** (so a circuit-breaker lib is swappable, not baked in)."

## Problem
At v0.23.0 there is no resilience middleware: no request-timeout enforcement, no rate limiting, no
circuit-breaker seam. A handler can run unbounded; a traffic spike has no shedding; there is no place to plug
a breaker. These are table-stakes for the foundation framing — but the concrete algorithm/lib must stay
swappable, never baked into core.

## Decision (locked)
A new `resilience` package providing policy interceptors whose algorithms sit behind interfaces:
- **Timeout** (stdlib). `resilience.TimeoutUnary(def time.Duration, perMethod map[string]time.Duration)
  grpc.UnaryServerInterceptor` — wraps the handler context with `context.WithTimeout` (per-method override >
  default). A handler exceeding the deadline yields `codes.DeadlineExceeded`. Pure `context`/`time`.
- **Rate limiting** (algorithm behind an interface).
  ```go
  type RateLimiter interface { Allow(ctx context.Context, fullMethod string) bool }
  func RateLimitUnary(l RateLimiter) grpc.UnaryServerInterceptor // denies → codes.ResourceExhausted
  func NewTokenBucket(rps float64, burst int) RateLimiter        // default in-process limiter
  ```
  The default token bucket uses `golang.org/x/time/rate` IF already in the module graph; otherwise a tiny
  stdlib token bucket (mutex + time). A distributed (Redis) limiter is a swap behind `RateLimiter` — not built.
- **Circuit breaker = seam + docs, no lib baked in** (honors "document circuit-breaker"):
  ```go
  type CircuitBreaker interface { Execute(ctx context.Context, fn func() (any, error)) (any, error) }
  func BreakerUnary(b CircuitBreaker) grpc.UnaryServerInterceptor // optional; wraps the handler
  ```
  Core ships the interface + interceptor only; the guide shows plugging `sony/gobreaker` (or any lib) — the
  SDK depends on NO breaker library.

## Wiring & defaults
- `server.Config` gains a minimal `Resilience` sub-config:
  - `RequestTimeout time.Duration` (default **30s**; `0` disables), `PerMethodTimeout map[string]time.Duration`.
  - `RateLimiter resilience.RateLimiter` (default `nil` = **off**; opt-in — global RPS ceilings are
    deployment-specific).
  - `CircuitBreaker resilience.CircuitBreaker` (default `nil` = off; opt-in).
- Chain placement: rate-limit is inserted **early** (shed load before authz/work), timeout wraps the handler
  **innermost** (so it bounds the actual handler), breaker (if set) just outside the handler. Only unary
  today (no stream chain exists).
- Defaults chosen to be safe: timeout on with a generous 30s (bounds runaway handlers; LRO returns quickly so
  it's unaffected); rate-limit + breaker off until the consumer opts in.

## Design — files
- `resilience/timeout.go`, `resilience/ratelimit.go` (interface + token bucket), `resilience/breaker.go`
  (interface + interceptor) — new package.
- `server/server.go`: add the `Resilience` config; insert the interceptors into the default chain at the
  documented positions.
- `resilience/*_test.go`: timeout→DeadlineExceeded; ratelimit→ResourceExhausted + swap; breaker passthrough.
- Extend the dep-light guard: no breaker lib, no heavy limiter lib in core.
- Docs: `docs/content/docs/guides/resilience.md` (incl. a gobreaker swap example + a Redis-limiter sketch).

## Acceptance criteria
- **AC-1.** A handler that runs past `RequestTimeout` (or its per-method override) returns `DeadlineExceeded`;
  `0` disables; a fast handler is unaffected.
- **AC-2.** With a `RateLimiter` set, requests beyond the limit get `ResourceExhausted`; the default token
  bucket enforces rps/burst; swapping the `RateLimiter` interface changes behavior with no core change.
- **AC-3.** `CircuitBreaker` interface + `BreakerUnary` exist and are documented with a real lib swap; core
  imports NO circuit-breaker library (dep-light guard).
- **AC-4 (dependency-light gate).** Core gains no heavy resilience dep: at most `golang.org/x/time/rate`
  (only if already in-graph); no `sony/gobreaker`/`afex/hystrix`/etc. Guard test + `go list -deps` proof.
- **AC-5 (gates).** build/vet/test/security green; scaffold E2E green.

## Failure modes
- **Default timeout breaks a legitimately long unary call** → 30s is generous + per-method override +
  `0`-disables; documented. (Streaming unaffected — no stream chain.)
- **Rate limiter contention / lock under load** → token bucket must be lock-light/concurrent-safe; test under
  parallelism.
- **Timeout interceptor doesn't actually cancel the handler** → it bounds the ctx; document that handlers must
  honor ctx cancellation (as they already must for tracing/dedup). Test with a ctx-respecting handler.
- **A baked-in breaker lib sneaks in** → guard test forbids; ship interface only.

## Tasks
- **T1 [C]** `resilience` package — timeout interceptor, `RateLimiter` interface + token bucket,
  `CircuitBreaker` interface + `BreakerUnary` (concurrency-sensitive → [C]).
- **T2 [S]** wire `Resilience` config into `server.go` default chain at documented positions.
- **T3 [S]** tests (timeout, rate-limit + swap, breaker passthrough, concurrency) + dep-light guard extension.
- **T4 [S]** docs `guides/resilience.md` with gobreaker + Redis-limiter swap examples.

## Exit
All ACs green; PR merged; tag cut. DX cadence shows resilience present (timeouts/rate-limit; breaker as a
documented seam).
