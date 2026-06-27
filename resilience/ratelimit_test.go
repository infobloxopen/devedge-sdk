package resilience_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/resilience"
)

// TestRateLimitUnary_ResourceExhausted verifies that a request denied by the
// limiter returns codes.ResourceExhausted and never calls the handler.
func TestRateLimitUnary_ResourceExhausted(t *testing.T) {
	// Zero-rps zero-burst bucket denies everything immediately.
	limiter := resilience.NewTokenBucket(0, 0)
	intercept := resilience.RateLimitUnary(limiter)
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), handler)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", err)
	}
	if handlerCalled {
		t.Fatal("handler must not be called when rate limit is exceeded")
	}
}

// TestRateLimitUnary_Allows verifies that a request allowed by the limiter
// reaches the handler.
func TestRateLimitUnary_Allows(t *testing.T) {
	// Very high rps, burst=1 — first request always passes.
	limiter := resilience.NewTokenBucket(1000, 10)
	intercept := resilience.RateLimitUnary(limiter)
	resp, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// alwaysDenyLimiter is a test RateLimiter that denies every request.
type alwaysDenyLimiter struct{}

func (alwaysDenyLimiter) Allow(_ context.Context, _ string) bool { return false }

// TestRateLimitUnary_SwappedLimiter verifies that swapping the RateLimiter
// implementation changes behavior without any core change.
func TestRateLimitUnary_SwappedLimiter(t *testing.T) {
	intercept := resilience.RateLimitUnary(alwaysDenyLimiter{})
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted from swapped limiter, got %v", err)
	}
}

// TestTokenBucket_EnforcesBurst verifies that the token bucket admits exactly
// burst requests immediately and denies the next one.
func TestTokenBucket_EnforcesBurst(t *testing.T) {
	const burst = 3
	// Very low rps so no tokens refill during the test.
	limiter := resilience.NewTokenBucket(0.001, burst)
	for i := 0; i < burst; i++ {
		if !limiter.Allow(context.Background(), "/svc/M") {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	if limiter.Allow(context.Background(), "/svc/M") {
		t.Fatal("request after burst exhausted should be denied")
	}
}

// TestTokenBucket_Concurrency verifies the token bucket is safe under
// concurrent callers (no data races; run with -race).
func TestTokenBucket_Concurrency(t *testing.T) {
	limiter := resilience.NewTokenBucket(10000, 1000)
	intercept := resilience.RateLimitUnary(limiter)
	var (
		wg      sync.WaitGroup
		allowed atomic.Int64
		denied  atomic.Int64
	)
	const goroutines = 50
	const callsEach = 20
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsEach; j++ {
				_, err := intercept(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/M"}, fastHandler)
				if err == nil {
					allowed.Add(1)
				} else {
					denied.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("allowed=%d denied=%d", allowed.Load(), denied.Load())
	// At 10000 rps and burst 1000, the vast majority should be allowed.
	// We just assert no panic/race (enforced by -race flag) and some passed.
	if allowed.Load() == 0 {
		t.Fatal("expected some requests to be allowed under high rps")
	}
}

// TestTokenBucket_Refill verifies that tokens refill over time.
func TestTokenBucket_Refill(t *testing.T) {
	// 100 rps, burst=1: one token initially; drain it, then wait for refill.
	limiter := resilience.NewTokenBucket(1000, 1)
	if !limiter.Allow(context.Background(), "/svc/M") {
		t.Fatal("first request should be allowed")
	}
	if limiter.Allow(context.Background(), "/svc/M") {
		t.Fatal("second immediate request should be denied (bucket empty)")
	}
	// Wait a bit longer than 1ms to get at least 1 new token at 1000 rps.
	time.Sleep(5 * time.Millisecond)
	if !limiter.Allow(context.Background(), "/svc/M") {
		t.Fatal("request after refill window should be allowed")
	}
}
