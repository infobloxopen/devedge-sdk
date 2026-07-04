package authn_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authn"
	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
)

func incoming(kv ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
}

// passHandler records the ctx it saw and returns ok.
func passHandler(seen *context.Context) grpc.UnaryHandler {
	return func(ctx context.Context, _ any) (any, error) {
		*seen = ctx
		return "ok", nil
	}
}

func TestStaticClaimsMapper_MapsAndCopies(t *testing.T) {
	m := authn.NewStaticClaimsMapper("orders", map[string]authz.Principal{
		"alice": {Tenant: "tenant-a", Groups: []string{"admin"}, Claims: map[string]any{"k": "v"}},
	})
	p, err := m.MapClaims(context.Background(), authn.Identity{Subject: "alice", Apps: []string{"orders"}})
	if err != nil {
		t.Fatalf("MapClaims: %v", err)
	}
	if p.Subject != "alice" || p.Tenant != "tenant-a" || len(p.Groups) != 1 || p.Groups[0] != "admin" {
		t.Fatalf("mapped principal = %+v", p)
	}
	// Mutating the returned principal must not affect the stored mapping.
	p.Groups[0] = "hacked"
	p.Claims["k"] = "mutated"
	p2, _ := m.MapClaims(context.Background(), authn.Identity{Subject: "alice", Apps: []string{"orders"}})
	if p2.Groups[0] != "admin" || p2.Claims["k"] != "v" {
		t.Fatalf("stored mapping was mutated through returned principal: %+v", p2)
	}
}

func TestStaticClaimsMapper_Entitlement(t *testing.T) {
	m := authn.NewStaticClaimsMapper("orders", map[string]authz.Principal{
		"bob": {Groups: []string{"viewer"}},
	}, authn.WithRequireEntitlement())

	// Not entitled: app-access does not include "orders".
	if _, err := m.MapClaims(context.Background(), authn.Identity{Subject: "bob", Apps: []string{"other"}}); !errors.Is(err, authn.ErrNotEntitled) {
		t.Fatalf("want ErrNotEntitled, got %v", err)
	}
	// Entitled.
	if _, err := m.MapClaims(context.Background(), authn.Identity{Subject: "bob", Apps: []string{"orders"}}); err != nil {
		t.Fatalf("entitled MapClaims: %v", err)
	}
}

func TestStaticClaimsMapper_HotReload(t *testing.T) {
	m := authn.NewStaticClaimsMapper("orders", nil)
	// Unmapped, entitlement not required -> bare authenticated principal.
	p, _ := m.MapClaims(context.Background(), authn.Identity{Subject: "carol"})
	if p.Subject != "carol" || p.Tenant != "" || len(p.Groups) != 0 {
		t.Fatalf("unmapped principal should be bare, got %+v", p)
	}
	// Live edit.
	m.Set("carol", authz.Principal{Tenant: "tenant-b", Groups: []string{"admin"}})
	p, _ = m.MapClaims(context.Background(), authn.Identity{Subject: "carol"})
	if p.Tenant != "tenant-b" || len(p.Groups) != 1 {
		t.Fatalf("after Set, principal = %+v", p)
	}
}

func TestInterceptor_ValidBearer_StashesPrincipal(t *testing.T) {
	a := authn.AuthenticatorFunc(func(_ context.Context, bearer string) (authz.Principal, error) {
		if bearer != "good" {
			return authz.Principal{}, errors.New("bad")
		}
		return authz.Principal{Subject: "alice", Tenant: "tenant-a"}, nil
	})
	var seen context.Context
	_, err := authn.UnaryServerInterceptor(a)(incoming("authorization", "Bearer good"), nil, &grpc.UnaryServerInfo{}, passHandler(&seen))
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	p, ok := middleware.PrincipalFromContext(seen)
	if !ok || p.Subject != "alice" || p.Tenant != "tenant-a" {
		t.Fatalf("principal not stashed: %+v ok=%v", p, ok)
	}
	// VerifiedPrincipal adapter reads the same.
	vp, _ := authn.VerifiedPrincipal(seen)
	if vp.Subject != "alice" {
		t.Fatalf("VerifiedPrincipal = %+v", vp)
	}
}

func TestInterceptor_InvalidBearer_FailsClosed(t *testing.T) {
	a := authn.AuthenticatorFunc(func(context.Context, string) (authz.Principal, error) {
		return authz.Principal{}, errors.New("invalid signature")
	})
	var seen context.Context
	_, err := authn.UnaryServerInterceptor(a)(incoming("authorization", "Bearer nope"), nil, &grpc.UnaryServerInfo{}, passHandler(&seen))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if seen != nil {
		t.Fatal("handler ran despite invalid bearer (not fail-closed)")
	}
}

func TestInterceptor_NoBearer_OptionalPassesThrough(t *testing.T) {
	a := authn.AuthenticatorFunc(func(context.Context, string) (authz.Principal, error) {
		t.Fatal("Authenticate should not be called without a bearer")
		return authz.Principal{}, nil
	})
	var seen context.Context
	_, err := authn.UnaryServerInterceptor(a)(incoming(), nil, &grpc.UnaryServerInfo{}, passHandler(&seen))
	if err != nil {
		t.Fatalf("optional passthrough: %v", err)
	}
	if seen == nil {
		t.Fatal("handler did not run on optional passthrough")
	}
	if _, ok := middleware.PrincipalFromContext(seen); ok {
		t.Fatal("principal should not be stashed without a bearer")
	}
}

func TestInterceptor_NoBearer_RequiredRejects(t *testing.T) {
	a := authn.AuthenticatorFunc(func(context.Context, string) (authz.Principal, error) { return authz.Principal{}, nil })
	var seen context.Context
	_, err := authn.UnaryServerInterceptor(a, authn.WithRequired())(incoming(), nil, &grpc.UnaryServerInfo{}, passHandler(&seen))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestInterceptor_NilAuthenticator_Inert(t *testing.T) {
	var seen context.Context
	_, err := authn.UnaryServerInterceptor(nil)(incoming("authorization", "Bearer whatever"), nil, &grpc.UnaryServerInfo{}, passHandler(&seen))
	if err != nil || seen == nil {
		t.Fatalf("nil authenticator should be inert: err=%v seen=%v", err, seen)
	}
}
