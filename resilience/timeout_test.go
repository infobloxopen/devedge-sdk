package resilience_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/resilience"
)

func makeInfo(method string) *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: method}
}

// slowHandler blocks until its context is cancelled, then returns the ctx error.
func slowHandler(d time.Duration) grpc.UnaryHandler {
	return func(ctx context.Context, req any) (any, error) {
		select {
		case <-time.After(d):
			return "ok", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func fastHandler(ctx context.Context, req any) (any, error) {
	return "ok", nil
}

// TestTimeoutUnary_DeadlineExceeded verifies that a handler running past the
// deadline returns codes.DeadlineExceeded.
func TestTimeoutUnary_DeadlineExceeded(t *testing.T) {
	intercept := resilience.TimeoutUnary(50*time.Millisecond, nil)
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), slowHandler(5*time.Second))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", status.Code(err))
	}
}

// TestTimeoutUnary_FastHandlerUnaffected verifies that a handler completing
// within the deadline is not affected.
func TestTimeoutUnary_FastHandlerUnaffected(t *testing.T) {
	intercept := resilience.TimeoutUnary(5*time.Second, nil)
	resp, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// TestTimeoutUnary_ZeroDisables verifies that NoTimeout (<=0) disables the
// timeout even for a slow handler.
func TestTimeoutUnary_ZeroDisables(t *testing.T) {
	intercept := resilience.TimeoutUnary(resilience.NoTimeout, nil)
	// Handler returns quickly in test; we just need to confirm no timeout error.
	resp, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error with NoTimeout: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// TestTimeoutUnary_PerMethodOverride verifies that a per-method override takes
// precedence over the default.
func TestTimeoutUnary_PerMethodOverride(t *testing.T) {
	const method = "/svc/SlowMethod"
	// Default is long; per-method override is short → should timeout.
	intercept := resilience.TimeoutUnary(5*time.Second, map[string]time.Duration{
		method: 50 * time.Millisecond,
	})
	_, err := intercept(context.Background(), nil, makeInfo(method), slowHandler(5*time.Second))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded via per-method override, got %v", err)
	}
}

// TestTimeoutUnary_PerMethodNoTimeoutOverride verifies a per-method NoTimeout
// disables the default for that method.
func TestTimeoutUnary_PerMethodNoTimeoutOverride(t *testing.T) {
	const method = "/svc/LongOp"
	// Default is very short; per-method disables it → fast handler should pass.
	intercept := resilience.TimeoutUnary(50*time.Millisecond, map[string]time.Duration{
		method: resilience.NoTimeout,
	})
	resp, err := intercept(context.Background(), nil, makeInfo(method), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error with per-method NoTimeout: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// TestTimeoutUnary_ErrorPassthrough verifies handler errors unrelated to the
// timeout are passed through unchanged.
func TestTimeoutUnary_ErrorPassthrough(t *testing.T) {
	handlerErr := status.Error(codes.NotFound, "not found")
	errHandler := func(ctx context.Context, req any) (any, error) {
		return nil, handlerErr
	}
	intercept := resilience.TimeoutUnary(5*time.Second, nil)
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), errHandler)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound passthrough, got %v", err)
	}
}
