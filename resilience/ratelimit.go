package resilience

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RateLimiter is the seam for rate-limiting policies. Allow returns true when
// the request may proceed, false when it should be shed (ResourceExhausted).
type RateLimiter interface {
	Allow(ctx context.Context, fullMethod string) bool
}

// RateLimitUnary returns a unary server interceptor that delegates to l.Allow.
// When Allow returns false the interceptor returns codes.ResourceExhausted
// immediately; the handler is never invoked.
func RateLimitUnary(l RateLimiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !l.Allow(ctx, info.FullMethod) {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

// tokenBucket is a minimal in-process token-bucket rate limiter implemented
// with stdlib only (no golang.org/x/time/rate dependency). Safe for concurrent use.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	rps      float64
	burst    float64
	lastTick time.Time
}

// NewTokenBucket returns a RateLimiter that allows rps requests per second with
// a burst capacity of burst. Implemented with stdlib time+sync; golang.org/x/time
// is not in the module graph so this avoids a new dependency.
func NewTokenBucket(rps float64, burst int) RateLimiter {
	return &tokenBucket{
		tokens:   float64(burst),
		rps:      rps,
		burst:    float64(burst),
		lastTick: time.Now(),
	}
}

// Allow implements RateLimiter. It refills tokens proportional to elapsed time
// then consumes one token, returning false when the bucket is empty.
func (tb *tokenBucket) Allow(_ context.Context, _ string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastTick).Seconds()
	tb.lastTick = now
	tb.tokens += elapsed * tb.rps
	if tb.tokens > tb.burst {
		tb.tokens = tb.burst
	}
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}
