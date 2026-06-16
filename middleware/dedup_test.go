package middleware_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

// dedupReq implements GetRequestId() only.
type dedupReq struct {
	requestID string
}

func (r *dedupReq) GetRequestId() string { return r.requestID }

// dedupValidateReq implements both GetRequestId() and GetValidateOnly().
type dedupValidateReq struct {
	requestID    string
	validateOnly bool
}

func (r *dedupValidateReq) GetRequestId() string    { return r.requestID }
func (r *dedupValidateReq) GetValidateOnly() bool   { return r.validateOnly }

// noIDReq has no GetRequestId method.
type noIDReq struct{}

func TestDedupStore_MissAndHit(t *testing.T) {
	s := mw.NewMemoryDeduplicationStore(time.Minute)

	if _, ok := s.Load("x"); ok {
		t.Fatal("expected miss on empty store")
	}

	s.Store("x", "hello")
	v, ok := s.Load("x")
	if !ok {
		t.Fatal("expected hit after Store")
	}
	if v != "hello" {
		t.Fatalf("expected 'hello', got %v", v)
	}
}

func TestDedupStore_TTLExpiry(t *testing.T) {
	s := mw.NewMemoryDeduplicationStore(10 * time.Millisecond)
	s.Store("y", "val")

	time.Sleep(20 * time.Millisecond)

	if _, ok := s.Load("y"); ok {
		t.Fatal("expected miss after TTL expiry")
	}
}

func TestDeduplicateUnary_SameRequestIDDeduplicates(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	intc := mw.DeduplicateUnary(store)

	calls := 0
	handler := func(ctx context.Context, req any) (any, error) {
		calls++
		return "response", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}

	resp1, err := intc(context.Background(), &dedupReq{requestID: "id-1"}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp2, err := intc(context.Background(), &dedupReq{requestID: "id-1"}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected handler called once, got %d", calls)
	}
	if resp1 != resp2 {
		t.Fatalf("expected same response, got %v and %v", resp1, resp2)
	}
}

func TestDeduplicateUnary_DifferentRequestIDsBothInvoke(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	intc := mw.DeduplicateUnary(store)

	calls := 0
	handler := func(ctx context.Context, req any) (any, error) {
		calls++
		return "response", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}

	intc(context.Background(), &dedupReq{requestID: "id-a"}, info, handler)
	intc(context.Background(), &dedupReq{requestID: "id-b"}, info, handler)

	if calls != 2 {
		t.Fatalf("expected handler called twice for distinct IDs, got %d", calls)
	}
}

func TestDeduplicateUnary_EmptyRequestIDAlwaysInvokes(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	intc := mw.DeduplicateUnary(store)

	calls := 0
	handler := func(ctx context.Context, req any) (any, error) {
		calls++
		return "response", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}

	intc(context.Background(), &dedupReq{requestID: ""}, info, handler)
	intc(context.Background(), &dedupReq{requestID: ""}, info, handler)
	intc(context.Background(), &dedupReq{requestID: ""}, info, handler)

	if calls != 3 {
		t.Fatalf("expected handler called 3 times for empty request_id, got %d", calls)
	}
}

func TestDeduplicateUnary_ValidateOnlySkipsCache(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	validateOnly := mw.ValidateOnlyUnary()
	dedup := mw.DeduplicateUnary(store)

	// chain: ValidateOnlyUnary → DeduplicateUnary → handler
	calls := 0
	innerHandler := func(ctx context.Context, req any) (any, error) {
		calls++
		return "response", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}

	// wrap dedup+handler into a single grpc.UnaryHandler for validateOnly
	dedupHandler := func(ctx context.Context, req any) (any, error) {
		return dedup(ctx, req, info, innerHandler)
	}

	// first call: validate_only=true — should invoke handler but not cache
	validateOnly(context.Background(), &dedupValidateReq{requestID: "id-2", validateOnly: true}, info, dedupHandler)
	if calls != 1 {
		t.Fatalf("expected handler called once for validate_only, got %d", calls)
	}

	// second call: validate_only=false, same request_id — cache was not populated,
	// so handler must be called again
	validateOnly(context.Background(), &dedupValidateReq{requestID: "id-2", validateOnly: false}, info, dedupHandler)
	if calls != 2 {
		t.Fatalf("expected handler called again for real call after validate_only, got %d", calls)
	}
}

func TestDeduplicateUnary_ErrorNotCached(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	intc := mw.DeduplicateUnary(store)

	calls := 0
	handlerErr := errors.New("boom")
	handler := func(ctx context.Context, req any) (any, error) {
		calls++
		return nil, handlerErr
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}

	_, err := intc(context.Background(), &dedupReq{requestID: "id-3"}, info, handler)
	if err != handlerErr {
		t.Fatalf("expected handlerErr, got %v", err)
	}

	// second call with same request_id must re-execute handler (error not cached)
	_, err = intc(context.Background(), &dedupReq{requestID: "id-3"}, info, handler)
	if err != handlerErr {
		t.Fatalf("expected handlerErr on second call, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected handler called twice (error not cached), got %d", calls)
	}
}

func TestDeduplicateUnary_NoRequestIDInterface_PassesThrough(t *testing.T) {
	store := mw.NewMemoryDeduplicationStore(time.Minute)
	intc := mw.DeduplicateUnary(store)

	calls := 0
	handler := func(ctx context.Context, req any) (any, error) {
		calls++
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Get"}

	intc(context.Background(), &noIDReq{}, info, handler)
	intc(context.Background(), &noIDReq{}, info, handler)

	if calls != 2 {
		t.Fatalf("expected handler called twice for req without GetRequestId, got %d", calls)
	}
}
