package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
)

func allowAll() authz.Authorizer {
	return authz.AuthorizerFunc(func(context.Context, authz.AccessRequest) (authz.Decision, error) {
		return authz.Decision{Allow: true, Reason: "ok", Obligations: map[string]any{"k": "v"}}, nil
	})
}

func TestEntitlementAuthorizer(t *testing.T) {
	features := authz.NewStaticFeatures(map[string][]string{
		"acme": {"dossier.query", "sandbox"},
	})
	gate := authz.WithEntitlement(allowAll(), features)

	req := func(tenant string, feats ...string) authz.AccessRequest {
		return authz.AccessRequest{Principal: authz.Principal{Tenant: tenant}, Features: feats}
	}

	t.Run("allow when permission allows and feature granted", func(t *testing.T) {
		dec, err := gate.Authorize(context.Background(), req("acme", "dossier.query"))
		if err != nil || !dec.Allow {
			t.Fatalf("want allow, got %+v err=%v", dec, err)
		}
		if dec.Obligations["k"] != "v" {
			t.Fatalf("inner obligations must survive entitlement check: %+v", dec.Obligations)
		}
	})

	t.Run("deny when a required feature is not granted", func(t *testing.T) {
		dec, err := gate.Authorize(context.Background(), req("acme", "sandbox", "premium"))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if dec.Allow {
			t.Fatalf("want deny on missing feature, got allow")
		}
	})

	t.Run("no features declared is a passthrough", func(t *testing.T) {
		dec, err := gate.Authorize(context.Background(), req("acme"))
		if err != nil || !dec.Allow {
			t.Fatalf("want allow, got %+v err=%v", dec, err)
		}
	})

	t.Run("entitlement is not checked when permission denies", func(t *testing.T) {
		denyGate := authz.WithEntitlement(authz.DenyAll, features)
		dec, _ := denyGate.Authorize(context.Background(), req("acme", "premium"))
		if dec.Allow {
			t.Fatalf("permission deny must win regardless of features")
		}
	})

	t.Run("fail closed on source error", func(t *testing.T) {
		errSrc := authz.FeatureSourceFunc(func(context.Context, string) (map[string]bool, error) {
			return nil, errors.New("boom")
		})
		g := authz.WithEntitlement(allowAll(), errSrc)
		dec, err := g.Authorize(context.Background(), req("acme", "dossier.query"))
		if err == nil {
			t.Fatalf("want error surfaced")
		}
		if dec.Allow {
			t.Fatalf("must fail closed on source error")
		}
	})

	t.Run("nil feature source returns inner unchanged", func(t *testing.T) {
		inner := allowAll()
		if got := authz.WithEntitlement(inner, nil); got == nil {
			t.Fatalf("expected inner returned, got nil")
		}
	})
}

func TestLogAlertSinkAndFunc(t *testing.T) {
	// LogAlertSink must not panic with a nil logger (defaults to slog.Default).
	authz.NewLogAlertSink(nil).Emit(context.Background(), authz.Alert{Method: "/m", Reason: "r"})

	var got authz.Alert
	var sink authz.AlertSink = authz.AlertSinkFunc(func(_ context.Context, a authz.Alert) { got = a })
	sink.Emit(context.Background(), authz.Alert{Method: "/svc/M", Reason: "denied"})
	if got.Method != "/svc/M" || got.Reason != "denied" {
		t.Fatalf("AlertSinkFunc did not forward: %+v", got)
	}
}
