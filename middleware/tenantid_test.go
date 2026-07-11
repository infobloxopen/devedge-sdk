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

// SEC-042-01 regression: the verified accessor is header-blind. With only the
// client-settable "account-id" header set (via WithTenantID / TenantIDUnary) and
// NO verified principal, VerifiedTenantID must return "", false — a fence built
// on it fails closed rather than trusting the spoofable header. (TenantIDFromContext
// still returns the header value "b" as a routing convenience — asserted separately.)
func TestVerifiedTenantID_HeaderOnlyReturnsFalse(t *testing.T) {
	ctx := mw.WithTenantID(context.Background(), "b") // spoofable routing header, no principal
	if got, ok := mw.VerifiedTenantID(ctx); ok || got != "" {
		t.Fatalf("header-only must not be a verified tenant: got (%q, %v), want (%q, %v)", got, ok, "", false)
	}
	// The routing accessor still surfaces the header (unchanged behaviour).
	if got := mw.TenantIDFromContext(ctx); got != "b" {
		t.Fatalf("TenantIDFromContext routing fallback: want %q, got %q", "b", got)
	}
}

// A verified principal yields its tenant from VerifiedTenantID, and the header is
// irrelevant to the verified answer.
func TestVerifiedTenantID_PrincipalTenant(t *testing.T) {
	ctx := mw.WithTenantID(context.Background(), "b") // spoofed header must not matter
	ctx = mw.WithPrincipal(ctx, authz.Principal{Subject: "u", Tenant: "a"})
	if got, ok := mw.VerifiedTenantID(ctx); !ok || got != "a" {
		t.Fatalf("verified principal tenant: got (%q, %v), want (%q, %v)", got, ok, "a", true)
	}
}

// A verified principal with no tenant returns ("", true): identity is verified but
// the tenant is genuinely unset (fail closed), distinct from the "no principal" case.
func TestVerifiedTenantID_PrincipalWithoutTenant(t *testing.T) {
	ctx := mw.WithPrincipal(context.Background(), authz.Principal{Subject: "u"}) // no Tenant
	if got, ok := mw.VerifiedTenantID(ctx); !ok || got != "" {
		t.Fatalf("verified principal without tenant: got (%q, %v), want (%q, %v)", got, ok, "", true)
	}
}

func TestVerifiedTenantID_NoPrincipalNoHeader(t *testing.T) {
	if got, ok := mw.VerifiedTenantID(context.Background()); ok || got != "" {
		t.Fatalf("bare context: got (%q, %v), want (%q, %v)", got, ok, "", false)
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
