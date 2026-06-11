package middleware_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

func TestTenantID_PropagatesFromMetadata(t *testing.T) {
	intc := mw.TenantIDUnary()
	var gotTenant string
	handler := func(ctx context.Context, req any) (any, error) {
		gotTenant = mw.TenantIDFromContext(ctx)
		return nil, nil
	}
	md := metadata.Pairs("account-id", "tenant-abc")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := intc(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/List"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTenant != "tenant-abc" {
		t.Fatalf("expected TenantIDFromContext to return 'tenant-abc', got %q", gotTenant)
	}
}

func TestTenantID_EmptyWhenNoMetadata(t *testing.T) {
	intc := mw.TenantIDUnary()
	var gotTenant string
	handler := func(ctx context.Context, req any) (any, error) {
		gotTenant = mw.TenantIDFromContext(ctx)
		return nil, nil
	}
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/List"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Stub returns "" even with no metadata, so this test passes — but it should
	// return "" only when truly absent, which matches expected behavior for the
	// no-metadata case. This test asserts the absence case stays "".
	if gotTenant != "" {
		t.Fatalf("expected empty tenant-ID when no account-id in metadata, got %q", gotTenant)
	}
}
