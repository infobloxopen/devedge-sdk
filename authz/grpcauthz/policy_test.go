package grpcauthz_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
)

const zoneGet = "/zone.v1.ZoneService/GetZone"

func principalFn() grpcauthz.Option {
	return grpcauthz.WithPrincipalFunc(func(context.Context) (authz.Principal, error) {
		return authz.Principal{Subject: "u1", Tenant: "acme"}, nil
	})
}

func call(ic grpc.UnaryServerInterceptor, method string) (any, error) {
	return ic(context.Background(), struct{}{}, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return "ok", nil })
}

func TestEnforceModeDenies(t *testing.T) {
	ic := grpcauthz.UnaryServerInterceptor("sdk",
		grpcauthz.WithAuthorizer(authz.DenyAll),
		principalFn(),
		grpcauthz.WithRules(authz.MethodRule{Method: zoneGet, Verb: authz.Get, Resource: "zone"}),
	)
	if _, err := call(ic, zoneGet); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("enforce mode must deny, got %v", err)
	}
}

func TestAlertModeAllowsAndEmits(t *testing.T) {
	var emitted *authz.Alert
	sink := authz.AlertSinkFunc(func(_ context.Context, a authz.Alert) { emitted = &a })

	ic := grpcauthz.UnaryServerInterceptor("sdk",
		grpcauthz.WithAuthorizer(authz.DenyAll), // would deny
		principalFn(),
		grpcauthz.WithAlertSink(sink),
		grpcauthz.WithRules(authz.MethodRule{
			Method: zoneGet, Verb: authz.Get, Resource: "zone", Mode: authz.ModeAlert,
		}),
	)
	resp, err := call(ic, zoneGet)
	if err != nil {
		t.Fatalf("alert mode must allow through, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("handler should have run, got %v", resp)
	}
	if emitted == nil {
		t.Fatalf("alert mode must emit an alert")
	}
	if emitted.Method != zoneGet || emitted.Principal.Subject != "u1" {
		t.Fatalf("alert carries the wrong context: %+v", *emitted)
	}
}

func TestFeaturesCarriedIntoAccessRequest(t *testing.T) {
	var seen []string
	rec := authz.AuthorizerFunc(func(_ context.Context, req authz.AccessRequest) (authz.Decision, error) {
		seen = req.Features
		return authz.Decision{Allow: true}, nil
	})
	ic := grpcauthz.UnaryServerInterceptor("sdk",
		grpcauthz.WithAuthorizer(rec),
		principalFn(),
		grpcauthz.WithRules(authz.MethodRule{
			Method: zoneGet, Verb: authz.Get, Resource: "zone", Features: []string{"dossier.query"},
		}),
	)
	if _, err := call(ic, zoneGet); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(seen) != 1 || seen[0] != "dossier.query" {
		t.Fatalf("declared features must reach the AccessRequest, got %v", seen)
	}
}

func TestAssertMethodsDeclaredValidatesPolicyFields(t *testing.T) {
	src := func(rules ...authz.MethodRule) grpcauthz.Option {
		return grpcauthz.WithRuleSource(func() []authz.MethodRule { return rules })
	}

	t.Run("invalid mode fails closed", func(t *testing.T) {
		err := grpcauthz.AssertMethodsDeclared([]string{zoneGet},
			src(authz.MethodRule{Method: zoneGet, Verb: authz.Get, Resource: "zone", Mode: "audit"}))
		if err == nil {
			t.Fatalf("an unknown mode must be rejected at boot")
		}
	})

	t.Run("quota without metric fails closed", func(t *testing.T) {
		err := grpcauthz.AssertMethodsDeclared([]string{zoneGet},
			src(authz.MethodRule{Method: zoneGet, Verb: authz.Get, Resource: "zone", Quota: &authz.QuotaRule{}}))
		if err == nil {
			t.Fatalf("a quota without a metric must be rejected at boot")
		}
	})

	t.Run("well-formed policy rule passes", func(t *testing.T) {
		err := grpcauthz.AssertMethodsDeclared([]string{zoneGet},
			src(authz.MethodRule{
				Method: zoneGet, Verb: authz.Get, Resource: "zone",
				Mode: authz.ModeAlert, Features: []string{"f"}, Quota: &authz.QuotaRule{Metric: "calls", Window: "month"},
			}))
		if err != nil {
			t.Fatalf("well-formed rule must pass: %v", err)
		}
	})
}
