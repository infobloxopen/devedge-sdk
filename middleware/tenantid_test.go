package middleware_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/authz"
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

// SEC-002 regression: the VERIFIED PRINCIPAL is the tenant authority. When a
// principal scoped to tenant "a" is on the context, a spoofed account-id header
// ("b", the routing hint) must NOT widen scope — TenantIDFromContext returns "a".
// This fails on the old code, which returned the header value "b".
func TestTenantID_PrincipalIsAuthorityOverHeader(t *testing.T) {
	ctx := mw.WithTenantID(context.Background(), "b") // spoofed/routing header
	ctx = mw.WithPrincipal(ctx, authz.Principal{Subject: "u", Tenant: "a"})
	if got := mw.TenantIDFromContext(ctx); got != "a" {
		t.Fatalf("verified principal must be the tenant authority: want %q, got %q", "a", got)
	}
}

// The header is used only as a fallback on paths that never establish a principal.
func TestTenantID_HeaderFallbackWithoutPrincipal(t *testing.T) {
	ctx := mw.WithTenantID(context.Background(), "b")
	if got := mw.TenantIDFromContext(ctx); got != "b" {
		t.Fatalf("header fallback: want %q, got %q", "b", got)
	}
}

// A principal carrying no tenant yields "" even when a header is present: the
// principal is authoritative, so the fence fails closed rather than falling back
// to the untrusted header.
func TestTenantID_PrincipalWithoutTenantDoesNotFallBackToHeader(t *testing.T) {
	ctx := mw.WithTenantID(context.Background(), "b")
	ctx = mw.WithPrincipal(ctx, authz.Principal{Subject: "u"}) // no Tenant
	if got := mw.TenantIDFromContext(ctx); got != "" {
		t.Fatalf("principal (authority) has no tenant: want %q, got %q", "", got)
	}
}

func TestSystemContext(t *testing.T) {
	if mw.IsSystemContext(context.Background()) {
		t.Fatal("bare context must not be a system context")
	}
	if !mw.IsSystemContext(mw.WithSystemContext(context.Background())) {
		t.Fatal("WithSystemContext must mark the context")
	}
}
