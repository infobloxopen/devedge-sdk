package middleware_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

type fakeValidateOnlyReq struct {
	ValidateOnly bool
}

func (r *fakeValidateOnlyReq) GetValidateOnly() bool { return r.ValidateOnly }

type fakeNoValidateOnlyReq struct{}

func TestValidateOnlyUnary_True_StoresInContext(t *testing.T) {
	intc := mw.ValidateOnlyUnary()
	var got bool
	handler := func(ctx context.Context, req any) (any, error) {
		got = mw.ValidateOnlyFromContext(ctx)
		return nil, nil
	}
	req := &fakeValidateOnlyReq{ValidateOnly: true}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/CreateFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("expected ValidateOnlyFromContext to return true when validate_only=true, got false")
	}
}

func TestValidateOnlyUnary_False_ContextAbsent(t *testing.T) {
	intc := mw.ValidateOnlyUnary()
	var got bool
	handler := func(ctx context.Context, req any) (any, error) {
		got = mw.ValidateOnlyFromContext(ctx)
		return nil, nil
	}
	req := &fakeValidateOnlyReq{ValidateOnly: false}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/CreateFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("expected ValidateOnlyFromContext to return false when validate_only=false, got true")
	}
}

func TestValidateOnlyUnary_NoInterface_ContextAbsent(t *testing.T) {
	intc := mw.ValidateOnlyUnary()
	var got bool
	handler := func(ctx context.Context, req any) (any, error) {
		got = mw.ValidateOnlyFromContext(ctx)
		return nil, nil
	}
	req := &fakeNoValidateOnlyReq{}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/CreateFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("expected ValidateOnlyFromContext to return false when request doesn't implement the interface, got true")
	}
}

func TestValidateOnlyUnary_AlwaysCallsHandler(t *testing.T) {
	intc := mw.ValidateOnlyUnary()
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	req := &fakeValidateOnlyReq{ValidateOnly: true}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be called, but it was not")
	}
	if resp != "ok" {
		t.Fatalf("expected handler response 'ok', got %v", resp)
	}
}

func TestValidateOnlyFromContext_PlainContext_ReturnsFalse(t *testing.T) {
	if mw.ValidateOnlyFromContext(context.Background()) {
		t.Fatal("expected ValidateOnlyFromContext to return false on plain context.Background(), got true")
	}
}
