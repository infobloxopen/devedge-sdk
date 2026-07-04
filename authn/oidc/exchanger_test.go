package oidc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authn/oidc"
	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
)

const (
	stsIssuer   = "https://sts.dev.test"
	inboundAud  = "orders-api"  // the caller's own audience (passthrough target)
	crossAud    = "billing-api" // a DIFFERENT audience (exchange target)
	callerSub   = "alice"
	stsClientID = "orders-svc"
	stsSecret   = "s3cr3t"
)

const grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"

// mockSTS is an httptest RFC 8693 token-exchange endpoint. It mints an exchanged
// token whose aud = the requested audience (via stsIss), counts requests, and can
// be told to fail.
type mockSTS struct {
	stsIss    *oidc.Issuer
	calls     atomic.Int64
	expiresIn int64 // seconds put in the JSON response; 0 omits it
	fail      bool  // when true, respond 500
}

func (m *mockSTS) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if m.fail {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.FormValue("grant_type") != grantTokenExchange ||
			r.FormValue("subject_token") == "" ||
			r.FormValue("audience") == "" {
			http.Error(w, "bad exchange request", http.StatusBadRequest)
			return
		}
		// The STS would normally decode the subject_token's subject; the mint below
		// carries a fixed subject that matches the inbound caller for the test.
		tok, err := m.stsIss.Mint(r.Context(), authz.Principal{Subject: callerSub})
		if err != nil {
			http.Error(w, "mint", http.StatusInternalServerError)
			return
		}
		resp := map[string]any{
			"access_token":      tok,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "N_A",
		}
		if m.expiresIn > 0 {
			resp["expires_in"] = m.expiresIn
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// newSTS builds a mock STS that mints exchanged tokens for crossAud.
func newSTS(t *testing.T, ttl time.Duration, expiresIn int64) *mockSTS {
	t.Helper()
	iss, err := oidc.NewIssuer(stsIssuer, []string{crossAud}, oidc.WithTTL(ttl))
	if err != nil {
		t.Fatalf("NewIssuer (sts): %v", err)
	}
	return &mockSTS{stsIss: iss, expiresIn: expiresIn}
}

// inboundCtx mints an app bearer (aud=inboundAud, sub=callerSub) and stashes it
// as the inbound bearer, exactly as authn.UnaryServerInterceptor would.
func inboundCtx(t *testing.T) context.Context {
	t.Helper()
	appIss, err := oidc.NewIssuer("https://orders.dev.test", []string{inboundAud})
	if err != nil {
		t.Fatalf("NewIssuer (app): %v", err)
	}
	bearer, err := appIss.Mint(context.Background(), authz.Principal{Subject: callerSub})
	if err != nil {
		t.Fatalf("Mint inbound: %v", err)
	}
	return middleware.WithInboundBearer(context.Background(), bearer)
}

func newExchanger(t *testing.T, endpoint string, skew time.Duration) *oidc.Exchanger {
	t.Helper()
	ex, err := oidc.NewExchanger(oidc.ExchangeConfig{
		TokenEndpoint: endpoint,
		ClientID:      stsClientID,
		ClientSecret:  stsSecret,
		Skew:          skew,
	})
	if err != nil {
		t.Fatalf("NewExchanger: %v", err)
	}
	return ex
}

// TestExchange_Passthrough: when the target is already one of the caller's
// audiences, TokenFor returns the inbound bearer unchanged and makes NO HTTP call.
func TestExchange_Passthrough(t *testing.T) {
	sts := newSTS(t, time.Hour, 3600)
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0)
	ctx := inboundCtx(t)

	got, err := ex.TokenFor(ctx, inboundAud)
	if err != nil {
		t.Fatalf("TokenFor passthrough: %v", err)
	}
	raw, _ := middleware.InboundBearerFromContext(ctx)
	if got != raw {
		t.Fatalf("passthrough should return the inbound bearer unchanged")
	}
	if n := sts.calls.Load(); n != 0 {
		t.Fatalf("passthrough must make no STS call, got %d", n)
	}
}

// TestExchange_CrossAudience: a different target triggers an exchange whose result
// carries the target audience.
func TestExchange_CrossAudience(t *testing.T) {
	sts := newSTS(t, time.Hour, 3600)
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0)
	ctx := inboundCtx(t)

	got, err := ex.TokenFor(ctx, crossAud)
	if err != nil {
		t.Fatalf("TokenFor exchange: %v", err)
	}
	if n := sts.calls.Load(); n != 1 {
		t.Fatalf("cross-audience must make one STS call, got %d", n)
	}
	// The exchanged token must validate against the STS issuer for the TARGET aud.
	verifier, err := oidc.NewAuthenticator(oidc.Config{
		Keys:             oidc.StaticKeySet{Keys: sts.stsIss.KeySet()},
		ExpectedIssuer:   stsIssuer,
		ExpectedAudience: crossAud,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, err := verifier.Authenticate(context.Background(), got); err != nil {
		t.Fatalf("exchanged token does not carry target audience %q: %v", crossAud, err)
	}
}

// TestExchange_Caching: a second TokenFor for the same (sub, target) is served
// from cache — NO second HTTP call.
func TestExchange_Caching(t *testing.T) {
	sts := newSTS(t, time.Hour, 3600)
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0)
	ctx := inboundCtx(t)

	first, err := ex.TokenFor(ctx, crossAud)
	if err != nil {
		t.Fatalf("first TokenFor: %v", err)
	}
	second, err := ex.TokenFor(ctx, crossAud)
	if err != nil {
		t.Fatalf("second TokenFor: %v", err)
	}
	if first != second {
		t.Fatalf("cached call returned a different token")
	}
	if n := sts.calls.Load(); n != 1 {
		t.Fatalf("second TokenFor must be served from cache (want 1 STS call), got %d", n)
	}
}

// TestExchange_TTLReExchange: once the cached token's freshness window has
// clearly elapsed, the next TokenFor re-exchanges. (The cache-HIT direction is
// covered by TestExchange_Caching; this test only asserts the expiry direction,
// where an early wakeup or scheduler pause can only make the token MORE expired —
// so it is not timing-fragile.)
func TestExchange_TTLReExchange(t *testing.T) {
	// Short-lived exchanged token, no expires_in -> cache TTL follows the token exp.
	sts := newSTS(t, 100*time.Millisecond, 0)
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0) // skew 0 so freshness == token exp
	ctx := inboundCtx(t)

	if _, err := ex.TokenFor(ctx, crossAud); err != nil {
		t.Fatalf("first TokenFor: %v", err)
	}
	if n := sts.calls.Load(); n != 1 {
		t.Fatalf("want 1 STS call after first, got %d", n)
	}
	// Let the freshness window (100ms) clearly elapse, then it must re-exchange.
	time.Sleep(400 * time.Millisecond)
	if _, err := ex.TokenFor(ctx, crossAud); err != nil {
		t.Fatalf("re-exchange TokenFor: %v", err)
	}
	if n := sts.calls.Load(); n != 2 {
		t.Fatalf("expired cache must re-exchange (want 2 STS calls), got %d", n)
	}
}

// TestExchange_FailClosed_NoInboundBearer: without an inbound bearer there is
// nothing to act on behalf of — TokenFor errors and makes no HTTP call.
func TestExchange_FailClosed_NoInboundBearer(t *testing.T) {
	sts := newSTS(t, time.Hour, 3600)
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0)

	if _, err := ex.TokenFor(context.Background(), crossAud); err == nil {
		t.Fatal("TokenFor without an inbound bearer must fail closed")
	}
	if n := sts.calls.Load(); n != 0 {
		t.Fatalf("no-inbound-bearer must make no STS call, got %d", n)
	}
}

// TestExchange_FailClosed_STSError: an STS error surfaces as an error — never a
// fallback to the raw inbound token.
func TestExchange_FailClosed_STSError(t *testing.T) {
	sts := newSTS(t, time.Hour, 3600)
	sts.fail = true
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()
	ex := newExchanger(t, srv.URL, 0)
	ctx := inboundCtx(t)

	if _, err := ex.TokenFor(ctx, crossAud); err == nil {
		t.Fatal("an STS error must fail closed (no fallback to the raw inbound token)")
	}
}
