---
title: Resilience — Timeouts, Rate Limiting & Circuit Breakers
weight: 3
aliases:
  - /docs/guides/resilience/
---

The SDK provides three resilience interceptors — request timeouts, rate limiting, and circuit breaking — inserted at fixed positions in the default gRPC unary interceptor chain. Each policy sits behind an interface so you can supply your own implementation or turn the policy off without touching other parts of the server configuration.

Use this page when you need to tune how your service handles slow handlers, excessive inbound load, or cascading failures from downstream dependencies.

## Defaults

| Policy | Default | Opt-out |
|--------|---------|---------|
| **Request timeout** | 30 s | Set `Resilience.RequestTimeout` to `resilience.NoTimeout` |
| **Rate limiting** | Off | Set `Resilience.RateLimiter` to a `resilience.RateLimiter` |
| **Circuit breaker** | Off | Set `Resilience.CircuitBreaker` to a `resilience.CircuitBreaker` |

## Request timeout

Every unary handler runs under a context deadline. The default is 30 seconds. A handler that exceeds the deadline receives `codes.DeadlineExceeded`; the client sees the error immediately.

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

{{< callout type="info" >}}
**LRO (long-running operation) handlers are safe under the default 30-second timeout.** LRO handlers return quickly — they hand off to a background goroutine and return an `Operation` immediately — so the 30-second deadline is not a concern for them.
{{< /callout >}}

Handlers that run for a long time should respect `ctx.Done()` for clean early exit, which is also required for tracing, deduplication, and LRO cancellation.

## Rate limiting

Rate limiting is **opt-in**. Set `RateLimiter` to any `resilience.RateLimiter` implementation. The interceptor runs **before authz**, so it sheds load as early as possible in the chain.

### Built-in token bucket

The SDK includes a token-bucket implementation that uses only the standard library:

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

### Custom or distributed limiter

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

Swapping in a different limiter requires no change to the core — you assign a different value to the `RateLimiter` config field and nothing else.

## Circuit breaker

The SDK defines the `CircuitBreaker` interface and the `BreakerUnary` interceptor. No concrete circuit-breaker library is included. You choose a library and wrap it behind the interface.

### Interface

```go
type CircuitBreaker interface {
    Execute(ctx context.Context, fn func() (any, error)) (any, error)
}
```

### Example: sony/gobreaker

`sony/gobreaker` is a widely used circuit-breaker library. Add it to your service:

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

The breaker interceptor runs at the innermost framework position, just outside the handler and after all other framework interceptors. When the circuit is open, the interceptor returns an error without calling the handler.

### Mapping open-circuit errors to gRPC codes

When the circuit is open, `Execute` returns an error. Wrap it in the appropriate gRPC status before returning from your adapter so clients receive a meaningful code:

```go
func (a *gobreakerAdapter) Execute(ctx context.Context, fn func() (any, error)) (any, error) {
    out, err := a.cb.Execute(func() (any, error) { return fn() })
    if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
        return nil, status.Error(codes.Unavailable, "circuit open")
    }
    return out, err
}
```

## Chain placement

The resilience interceptors occupy fixed positions in the default unary chain:

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
