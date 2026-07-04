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
	iss := newIssuer(t, oidc.WithTTL(500*time.Millisecond))
	a, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{Keys: iss.KeySet()},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: appAud,
		Leeway:           5 * time.Millisecond, // tight skew so the test need not wait 30s
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	bearer, _ := iss.Mint(context.Background(), authz.Principal{Subject: "alice"})
	// Valid immediately (well inside the 500ms TTL).
	if _, err := a.Authenticate(context.Background(), bearer); err != nil {
		t.Fatalf("fresh token should validate: %v", err)
	}
	// Expired beyond leeway.
	time.Sleep(700 * time.Millisecond)
	if _, err := a.Authenticate(context.Background(), bearer); err == nil {
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
		// ExpectedAudience intentionally empty, AllowAnyAudience not set.
	}); err == nil {
		t.Error("empty ExpectedAudience without AllowAnyAudience must error (fail closed)")
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{},
		ExpectedIssuer:   appIssuer,
		AllowAnyAudience: true,
	}); err != nil {
		t.Errorf("empty ExpectedAudience WITH AllowAnyAudience must succeed, got %v", err)
	}
	if _, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{},
		ExpectedIssuer:   appIssuer,
		ExpectedAudience: "svc-a",
	}); err != nil {
		t.Errorf("non-empty ExpectedAudience must succeed, got %v", err)
	}
}
