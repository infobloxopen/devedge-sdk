package middleware_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	intc := mw.RequestIDUnary()
	var gotID string
	handler := func(ctx context.Context, req any) (any, error) {
		gotID = mw.RequestIDFromContext(ctx)
		return nil, nil
	}
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Get"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID == "" {
		t.Fatal("expected interceptor to generate and store a request-ID in context, got empty string")
	}
}

func TestRequestID_PropagatesExisting(t *testing.T) {
	intc := mw.RequestIDUnary()
	var gotID string
	handler := func(ctx context.Context, req any) (any, error) {
		gotID = mw.RequestIDFromContext(ctx)
		return nil, nil
	}
	md := metadata.Pairs("x-request-id", "existing-id")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := intc(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Get"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID != "existing-id" {
		t.Fatalf("expected interceptor to propagate 'existing-id' from metadata, got %q", gotID)
	}
}
