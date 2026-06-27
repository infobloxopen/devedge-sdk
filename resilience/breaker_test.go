package resilience_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/resilience"
)

// openBreaker simulates an open circuit breaker that rejects all requests.
type openBreaker struct{}

func (openBreaker) Execute(_ context.Context, _ func() (any, error)) (any, error) {
	return nil, status.Error(codes.Unavailable, "circuit open")
}

// closedBreaker is a pass-through circuit breaker (closed state = normal operation).
type closedBreaker struct{}

func (closedBreaker) Execute(_ context.Context, fn func() (any, error)) (any, error) {
	return fn()
}

// TestBreakerUnary_Passthrough verifies that a closed (pass-through) breaker
// does not interfere with normal handler execution.
func TestBreakerUnary_Passthrough(t *testing.T) {
	intercept := resilience.BreakerUnary(closedBreaker{})
	resp, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// TestBreakerUnary_OpenRejects verifies that an open breaker returns its error
// without calling the handler.
func TestBreakerUnary_OpenRejects(t *testing.T) {
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}
	intercept := resilience.BreakerUnary(openBreaker{})
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), handler)
	if err == nil {
		t.Fatal("expected error from open breaker, got nil")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable from open breaker, got %v", status.Code(err))
	}
	if handlerCalled {
		t.Fatal("handler must not be called when circuit is open")
	}
}

// TestBreakerUnary_NilIsNoop verifies that a nil CircuitBreaker is a safe
// no-op (does not panic).
func TestBreakerUnary_NilIsNoop(t *testing.T) {
	intercept := resilience.BreakerUnary(nil)
	resp, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), fastHandler)
	if err != nil {
		t.Fatalf("unexpected error with nil breaker: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

// TestBreakerUnary_PropagatesHandlerError verifies that a handler error is
// surfaced through a closed breaker unchanged.
func TestBreakerUnary_PropagatesHandlerError(t *testing.T) {
	sentinel := errors.New("handler error")
	errHandler := func(ctx context.Context, req any) (any, error) {
		return nil, sentinel
	}
	intercept := resilience.BreakerUnary(closedBreaker{})
	_, err := intercept(context.Background(), nil, makeInfo("/svc/Method"), errHandler)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}
