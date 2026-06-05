package grpcauthz

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
)

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

func TestUndeclaredMethodDeniedByDefault(t *testing.T) {
	intc := UnaryServerInterceptor("test") // no rules declared
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}, okHandler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for undeclared method, got %v", err)
	}
}

func TestDeniedWhenNoGrant(t *testing.T) {
	intc := UnaryServerInterceptor("test",
		WithMethodRule("/dns.v1.ZoneService/GetZone", authz.Get, "zone"),
		WithAuthorizer(authz.NewDevAuthorizer()), // empty → default deny
		WithPrincipalFunc(func(context.Context) (authz.Principal, error) {
			return authz.Principal{Subject: "u1", Tenant: "t1"}, nil
		}),
	)
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/dns.v1.ZoneService/GetZone"}, okHandler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied with no grant, got %v", err)
	}
}

func TestAllowedByDevGrant(t *testing.T) {
	intc := UnaryServerInterceptor("test",
		WithMethodRule("/dns.v1.ZoneService/GetZone", authz.Get, "zone"),
		WithAuthorizer(authz.NewDevAuthorizer(
			authz.Grant{Tenant: "t1", Subjects: []string{"u1"}, Verbs: []authz.Verb{authz.Get}, Resource: "zone"},
		)),
		WithPrincipalFunc(func(context.Context) (authz.Principal, error) {
			return authz.Principal{Subject: "u1", Tenant: "t1"}, nil
		}),
	)
	out, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/dns.v1.ZoneService/GetZone"}, okHandler)
	if err != nil || out != "ok" {
		t.Fatalf("want allow, got out=%v err=%v", out, err)
	}
}

func TestPublicMethodAllowed(t *testing.T) {
	intc := UnaryServerInterceptor("test", WithPublicMethod("/health/Check"))
	out, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/health/Check"}, okHandler)
	if err != nil || out != "ok" {
		t.Fatalf("want allow for public method, got out=%v err=%v", out, err)
	}
}

func TestAssertMethodsDeclared(t *testing.T) {
	err := AssertMethodsDeclared(
		[]string{"/a/B", "/c/D"},
		WithMethodRule("/a/B", authz.Get, "x"),
	)
	if err == nil {
		t.Fatal("want error for undeclared /c/D")
	}
	if err := AssertMethodsDeclared([]string{"/a/B"}, WithMethodRule("/a/B", authz.Get, "x")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
