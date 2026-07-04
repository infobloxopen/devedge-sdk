package oidc_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authn"
	"github.com/infobloxopen/devedge-sdk/authn/oidc"
	"github.com/infobloxopen/devedge-sdk/authz"
)

const (
	appIssuer = "https://orders.dev.test"
	appAud    = "orders-api"
)

// compile-time interface conformance.
var (
	_ authn.Issuer        = (*oidc.Issuer)(nil)
	_ authn.Authenticator = (*oidc.Authenticator)(nil)
	_ authn.TokenSource   = (*oidc.Exchanger)(nil)
)

func newIssuer(t *testing.T, opts ...oidc.IssuerOption) *oidc.Issuer {
	t.Helper()
	iss, err := oidc.NewIssuer(appIssuer, []string{appAud}, opts...)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func newAuth(t *testing.T, iss *oidc.Issuer, wantIss, wantAud string) *oidc.Authenticator {
	t.Helper()
	a, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{Keys: iss.KeySet()},
		ExpectedIssuer:   wantIss,
		ExpectedAudience: wantAud,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a
}

func TestMintVerify_RoundTrip(t *testing.T) {
	iss := newIssuer(t)
	a := newAuth(t, iss, appIssuer, appAud)

	want := authz.Principal{
		Subject: "alice",
		Tenant:  "tenant-a",
		Groups:  []string{"admin", "ops"},
		Scopes:  []string{"orders:read", "orders:write"},
		Claims:  map[string]any{"email": "alice@dev.test"},
	}
	bearer, err := iss.Mint(context.Background(), want)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := a.Authenticate(context.Background(), bearer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Subject != want.Subject || got.Tenant != want.Tenant {
		t.Fatalf("principal = %+v, want subject/tenant %q/%q", got, want.Subject, want.Tenant)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "admin" {
		t.Fatalf("groups = %v", got.Groups)
	}
	if len(got.Scopes) != 2 || got.Scopes[1] != "orders:write" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
	if got.Claims["email"] != "alice@dev.test" {
		t.Fatalf("claims = %v", got.Claims)
	}
}

func TestVerify_WrongIssuer_FailsClosed(t *testing.T) {
	iss := newIssuer(t)
	a := newAuth(t, iss, "https://evil.example", appAud)
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "alice"})
	if _, err := a.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("wrong issuer accepted (not fail-closed)")
	}
}

func TestVerify_WrongAudience_FailsClosed(t *testing.T) {
	iss := newIssuer(t)
	a := newAuth(t, iss, appIssuer, "some-other-api")
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "alice"})
	if _, err := a.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("wrong audience accepted (not fail-closed)")
	}
}

func TestVerify_Expired_FailsClosed(t *testing.T) {
	// A fresh token validates. Use a normal (1h) TTL so the freshness assertion is
	// not a race with a tiny lifetime: a GC/scheduler pause between mint and verify
	// cannot make a 1h token look expired.
	fresh := newIssuer(t)
	af := newAuth(t, fresh, appIssuer, appAud)
	freshTok, _ := fresh.Mint(context.Background(), authz.Principal{Subject: "alice"})
	if _, err := af.Authenticate(context.Background(), freshTok); err != nil {
		t.Fatalf("fresh token should validate: %v", err)
	}

	// A short-lived token, once its lifetime has clearly elapsed, fails closed.
	// This direction is robust: a pause can only make the token MORE expired.
	short := newIssuer(t, oidc.WithTTL(100*time.Millisecond))
	as, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{Keys: short.KeySet()},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: appAud,
		Leeway:           5 * time.Millisecond, // tight skew so the test need not wait 30s
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	shortTok, _ := short.Mint(context.Background(), authz.Principal{Subject: "alice"})
	time.Sleep(300 * time.Millisecond) // well past the 100ms TTL + 5ms leeway
	if _, err := as.Authenticate(context.Background(), shortTok); err == nil {
		t.Fatal("expired token accepted (not fail-closed)")
	}
}

func TestVerify_Tampered_FailsClosed(t *testing.T) {
	iss := newIssuer(t)
	a := newAuth(t, iss, appIssuer, appAud)
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "alice"})
	// Flip a byte in the payload segment.
	b := []byte(bearer)
	for i := len(b) / 2; i < len(b); i++ {
		if b[i] != '.' {
			if b[i] == 'a' {
				b[i] = 'b'
			} else {
				b[i] = 'a'
			}
			break
		}
	}
	if _, err := a.Authenticate(context.Background(), string(b)); err == nil {
		t.Fatal("tampered token accepted (not fail-closed)")
	}
}

func TestVerify_WrongKey_FailsClosed(t *testing.T) {
	minter := newIssuer(t)
	other := newIssuer(t) // different signing key
	a := newAuth(t, other, appIssuer, appAud)
	bearer, _ := minter.Mint(context.Background(), authz.Principal{Subject: "alice"})
	if _, err := a.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("token signed by a different key accepted (not fail-closed)")
	}
}

func TestVerify_RemoteJWKS(t *testing.T) {
	iss := newIssuer(t)
	srv := httptest.NewServer(iss.JWKSHandler())
	defer srv.Close()

	a, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             &oidc.RemoteJWKS{URL: srv.URL},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: appAud,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "carol", Tenant: "tenant-b"})
	got, err := a.Authenticate(context.Background(), bearer)
	if err != nil {
		t.Fatalf("Authenticate via remote JWKS: %v", err)
	}
	if got.Subject != "carol" || got.Tenant != "tenant-b" {
		t.Fatalf("principal = %+v", got)
	}
}

func TestNewAuthenticator_FailClosedConfig(t *testing.T) {
	if _, err := oidc.NewAuthenticator(oidc.Config{ExpectedIssuer: appIssuer}); err == nil {
		t.Error("missing KeySource should error")
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{Keys: oidc.StaticKeySet{}}); err == nil {
		t.Error("missing ExpectedIssuer should error")
	}
}

// SEC-004 regression: an empty ExpectedAudience must FAIL CLOSED (a verifier that
// accepts any audience would honor a token minted for a different service), unless
// the caller explicitly opts out with AllowAnyAudience. Fails on the old code,
// which silently skipped the audience check on an empty ExpectedAudience.
func TestNewAuthenticator_RequiresAudience(t *testing.T) {
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:           oidc.StaticKeySet{},
		ExpectedIssuer: appIssuer,
		// ExpectedAudience/ExpectedAudiences intentionally empty, AllowAnyAudience not set.
	}); err == nil {
		t.Error("empty audience set without AllowAnyAudience must error (fail closed)")
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{},
		ExpectedIssuer:   appIssuer,
		AllowAnyAudience: true,
	}); err != nil {
		t.Errorf("empty audience set WITH AllowAnyAudience must succeed, got %v", err)
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: "svc-a",
	}); err != nil {
		t.Errorf("non-empty ExpectedAudience must succeed, got %v", err)
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:              oidc.StaticKeySet{},
		ExpectedIssuer:    appIssuer,
		ExpectedAudiences: []string{"svc-a", "svc-b"},
	}); err != nil {
		t.Errorf("non-empty ExpectedAudiences must succeed, got %v", err)
	}
}

// SEC-004 / WS-028 multi-audience: a token validates when ANY of its `aud` values
// matches ANY configured audience, so one audience can cover a whole app's
// services and a domain-spanning service can accept a set — while a token whose
// audiences intersect NONE of the configured set still fails closed.
func TestVerify_MultiAudience(t *testing.T) {
	// Issuer mints tokens with aud = {"orders-api", "billing-api"}.
	iss, err := oidc.NewIssuer(appIssuer, []string{"orders-api", "billing-api"})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "alice"})

	// A verifier configured with a SET that intersects the token's aud accepts it,
	// even though neither of its configured audiences is the only one on the token.
	a, err := oidc.NewAuthenticator(oidc.Config{
		Keys:              oidc.StaticKeySet{Keys: iss.KeySet()},
		ExpectedIssuer:    appIssuer,
		ExpectedAudiences: []string{"reports-api", "billing-api"}, // billing-api intersects
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), bearer); err != nil {
		t.Fatalf("multi-audience intersection should validate: %v", err)
	}

	// The ExpectedAudience convenience alias is appended to the set: a single
	// matching audience via the alias also validates.
	aAlias, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{Keys: iss.KeySet()},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: "orders-api",
	})
	if err != nil {
		t.Fatalf("NewAuthenticator (alias): %v", err)
	}
	if _, err := aAlias.Authenticate(context.Background(), bearer); err != nil {
		t.Fatalf("alias audience should validate: %v", err)
	}

	// No intersection -> fail closed.
	aNone, err := oidc.NewAuthenticator(oidc.Config{
		Keys:              oidc.StaticKeySet{Keys: iss.KeySet()},
		ExpectedIssuer:    appIssuer,
		ExpectedAudiences: []string{"reports-api", "audit-api"},
	})
	if err != nil {
		t.Fatalf("NewAuthenticator (none): %v", err)
	}
	if _, err := aNone.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("token whose audiences intersect none of the configured set was accepted (not fail-closed)")
	}
}
