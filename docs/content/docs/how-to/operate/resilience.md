---
title: Resilience — Timeouts, Rate Limiting & Circuit Breakers
weight: 3
aliases:
  - /docs/guides/resilience/
---

The SDK ships three resilience policy interceptors — request timeouts, rate limiting, and a
circuit-breaker seam — all inserted into the default unary chain. Concrete algorithms sit behind
interfaces so you can swap implementations (or turn them off) without changing core.

## Defaults

| Policy | Default | Opt-out |
|--------|---------|---------|
| **Request timeout** | 30 s | Set `Resilience.RequestTimeout` to `resilience.NoTimeout` |
| **Rate limiting** | Off | Set `Resilience.RateLimiter` to a `resilience.RateLimiter` |
| **Circuit breaker** | Off | Set `Resilience.CircuitBreaker` to a `resilience.CircuitBreaker` |

## Request timeout

Every unary handler is bounded by a context deadline. The default is 30 seconds. A handler that
exceeds the deadline receives `codes.DeadlineExceeded`; the client sees it immediately.

```go
import (
    "time"
    "github.com/infobloxopen/devedge-sdk/resilience"
    "github.com/infobloxopen/devedge-sdk/server"
)

srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    Resilience: server.ResilienceConfig{
        // Override the 30s default for the whole server.
        RequestTimeout: 10 * time.Second,
        // Disable the timeout for a known long-running method.
        PerMethodTimeout: map[string]time.Duration{
            "/mypackage.MyService/ExportReport": 5 * time.Minute,
            "/mypackage.MyService/LROPoll":      resilience.NoTimeout,
        },
    },
})
```

To disable the global timeout entirely, pass `resilience.NoTimeout`:

```go
Resilience: server.ResilienceConfig{
    RequestTimeout: resilience.NoTimeout, // no global timeout
},
```

> **Note:** LRO operations return quickly (they hand off to a background goroutine and return an
> Operation immediately), so the default 30 s timeout is safe for them.

Handlers that run long should respect `ctx.Done()` for clean early exit — which they must already
do for tracing, deduplication, and LRO cancellation.

## Rate limiting

Rate limiting is **opt-in**. Set `RateLimiter` to any `resilience.RateLimiter` implementation.
The interceptor is inserted **before authz** to shed load as early as possible.

### Built-in token bucket

The SDK ships a stdlib token bucket (no extra dependencies):

```go
import "github.com/infobloxopen/devedge-sdk/resilience"

srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    Resilience: server.ResilienceConfig{
        // Allow 100 requests per second with a burst of 200.
        RateLimiter: resilience.NewTokenBucket(100, 200),
    },
})
```

### Custom / distributed limiter

Implement the `RateLimiter` interface to use Redis, a service mesh policy, or any other backend:

```go
// RedisRateLimiter is a sketch — adapt to your Redis client.
type RedisRateLimiter struct {
    client *redis.Client
    limit  rate.Limit
}

func (r *RedisRateLimiter) Allow(ctx context.Context, fullMethod string) bool {
    key := "ratelimit:" + fullMethod
    count, _ := r.client.Incr(ctx, key).Result()
    if count == 1 {
        r.client.Expire(ctx, key, time.Second)
    }
    return count <= int64(r.limit)
}

// Wire it in:
srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    Resilience: server.ResilienceConfig{
        RateLimiter: &RedisRateLimiter{client: redisClient, limit: 100},
    },
})
```

Swapping the limiter requires no change to core — only a config field assignment.

## Circuit breaker

The SDK ships the `CircuitBreaker` interface and `BreakerUnary` interceptor; **no concrete breaker
library is baked in**. Choose any library and wrap it behind the interface.

### Interface

```go
type CircuitBreaker interface {
    Execute(ctx context.Context, fn func() (any, error)) (any, error)
}
```

### Example: sony/gobreaker

`sony/gobreaker` is a popular, well-maintained circuit-breaker library. The SDK does not depend on
it; add it to your service:

```sh
go get github.com/sony/gobreaker
```

```go
import (
    "context"
    "github.com/sony/gobreaker"
    "github.com/infobloxopen/devedge-sdk/resilience"
    "github.com/infobloxopen/devedge-sdk/server"
)

// gobreakerAdapter wraps sony/gobreaker to implement resilience.CircuitBreaker.
type gobreakerAdapter struct {
    cb *gobreaker.CircuitBreaker
}

func (a *gobreakerAdapter) Execute(_ context.Context, fn func() (any, error)) (any, error) {
    return a.cb.Execute(func() (any, error) { return fn() })
}

func NewGobreakerAdapter(settings gobreaker.Settings) resilience.CircuitBreaker {
    return &gobreakerAdapter{cb: gobreaker.NewCircuitBreaker(settings)}
}

// Wire it in:
srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    Resilience: server.ResilienceConfig{
        CircuitBreaker: NewGobreakerAdapter(gobreaker.Settings{
            Name:        "my-service",
            MaxRequests: 5,
            Interval:    10 * time.Second,
            Timeout:     30 * time.Second,
            ReadyToTrip: func(counts gobreaker.Counts) bool {
                return counts.ConsecutiveFailures > 3
            },
        }),
    },
})
```

The breaker is inserted **just outside the handler** (innermost framework position, after all other
framework interceptors). When the circuit opens, it returns an error without calling the handler.

### Mapping open-circuit errors to gRPC codes

When the circuit opens, `Execute` returns an error. Wrap it in the appropriate gRPC status before
returning from your adapter so clients receive a meaningful code:

```go
func (a *gobreakerAdapter) Execute(ctx context.Context, fn func() (any, error)) (any, error) {
    out, err := a.cb.Execute(func() (any, error) { return fn() })
    if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
        return nil, status.Error(codes.Unavailable, "circuit open")
    }
    return out, err
}
```

## Chain placement summary

The resilience interceptors are inserted at fixed positions in the default chain:

```
RequestIDUnary
ErrorMapperUnary
TenantIDUnary
→ RateLimitUnary          (if configured — sheds load before authz)
LoggingUnary
grpcauthz
FieldMaskUnary
ETagPrecondition
ReadMaskUnary
ValidateOnlyUnary
DeduplicateUnary
[cfg.Interceptors...]
→ BreakerUnary            (if configured — just outside the handler)
→ TimeoutUnary            (always — innermost, bounds the handler itself)
handler
```
