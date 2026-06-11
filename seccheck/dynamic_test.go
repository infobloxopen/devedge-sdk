package seccheck

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// --- AssertUnknownPrincipalDenied tests ---

func TestAssertUnknownPrincipalDenied_AllDenied(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/MethodA", Verb: "read", Resource: "widget"},
		{Method: "/svc/MethodB", Verb: "write", Resource: "widget"},
	}
	calls := map[string]CallFn{
		"/svc/MethodA": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
		"/svc/MethodB": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

func TestAssertUnknownPrincipalDenied_OnePasses(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/MethodA", Verb: "read", Resource: "widget"},
		{Method: "/svc/MethodB", Verb: "write", Resource: "widget"},
	}
	calls := map[string]CallFn{
		"/svc/MethodA": func(ctx context.Context) error {
			return nil // should have been denied
		},
		"/svc/MethodB": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
	if findings[0].Method != "/svc/MethodA" {
		t.Errorf("expected finding for /svc/MethodA, got %q", findings[0].Method)
	}
}

func TestAssertUnknownPrincipalDenied_PublicSkipped(t *testing.T) {
	called := false
	rules := []authz.MethodRule{
		{Method: "/svc/PublicMethod", Public: true},
	}
	calls := map[string]CallFn{
		"/svc/PublicMethod": func(ctx context.Context) error {
			called = true
			return nil
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if called {
		t.Error("CallFn for a Public method should never be invoked")
	}
}

// --- AssertErrorMessagesClean tests ---

func TestAssertErrorMessagesClean_Clean(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.NotFound, "not found")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

func TestAssertErrorMessagesClean_LeaksPersistencePrefix(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.NotFound, "persistence: not found")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
}

func TestAssertErrorMessagesClean_LeaksSQLKeyword(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.Internal, "WHERE id = 'foo'")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
}

func TestAssertErrorMessagesClean_UnexpectedSuccess(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return nil // expected an error but got none
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Warning {
		t.Errorf("expected Warning severity, got %v", findings[0].Severity)
	}
}
