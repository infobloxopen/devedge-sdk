package etag_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/middleware/etag"
)

func TestPrecondition_ReadsIfMatchFromMetadata(t *testing.T) {
	intc := etag.PreconditionUnary()
	var gotIfMatch string
	handler := func(ctx context.Context, req any) (any, error) {
		gotIfMatch = etag.IfMatchFromContext(ctx)
		return nil, nil
	}
	md := metadata.Pairs("if-match", "abc")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := intc(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Update"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIfMatch != "abc" {
		t.Fatalf("expected IfMatchFromContext to return 'abc' (from if-match metadata), got %q", gotIfMatch)
	}
}

func TestSetNewETag_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx2 := etag.SetNewETag(ctx, "etag-val-42")
	got := etag.NewETagFromContext(ctx2)
	if got != "etag-val-42" {
		t.Fatalf("SetNewETag / NewETagFromContext roundtrip: expected 'etag-val-42', got %q", got)
	}
}

// TestPrecondition_HandlerCanSetETag verifies that the interceptor injects an
// etagHolder into the context so the handler can call SetNewETag, and that the
// value is immediately visible via NewETagFromContext on the same context.
// Actual trailer-writing (grpc.SetTrailer) requires a live server context and is
// verified end-to-end in testdata/toy/server_test.go.
func TestPrecondition_HandlerCanSetETag(t *testing.T) {
	intc := etag.PreconditionUnary()
	const wantETag = "v-generated-etag"

	handler := func(ctx context.Context, req any) (any, error) {
		etag.SetNewETag(ctx, wantETag)
		if got := etag.NewETagFromContext(ctx); got != wantETag {
			t.Errorf("NewETagFromContext inside handler: expected %q, got %q", wantETag, got)
		}
		return nil, nil
	}
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Create"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
