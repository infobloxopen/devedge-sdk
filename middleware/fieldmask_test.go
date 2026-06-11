package middleware_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

// fakeUpdateReq implements the UpdateMask accessor expected by FieldMaskUnary.
type fakeUpdateReq struct {
	UpdateMask []string
}

func (r *fakeUpdateReq) GetUpdateMask() []string { return r.UpdateMask }

var testVerbMap = map[string]string{
	"/test.v1.Svc/UpdateFoo": "update",
}

func TestFieldMask_UpdateVerbEmptyMask_ReturnsInvalidArgument(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, nil
	}
	req := &fakeUpdateReq{UpdateMask: nil}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/UpdateFoo"}, handler)
	if err == nil {
		t.Fatal("expected InvalidArgument error when update-verb method has empty UpdateMask, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument, got %v", st.Code())
	}
}

func TestFieldMask_UpdateVerbNonEmptyMask_PassesThrough(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	req := &fakeUpdateReq{UpdateMask: []string{"name", "description"}}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/UpdateFoo"}, handler)
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

func TestFieldMask_NonUpdateVerb_PassesThrough(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	// /test.v1.Svc/GetFoo is not in the verb map, so it's not an update
	req := &fakeUpdateReq{UpdateMask: nil}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/GetFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error for non-update method: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be called for non-update method")
	}
}
