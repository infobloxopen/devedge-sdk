package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/infobloxopen/devedge-sdk/middleware"
)

// RFC 8693 token-exchange grant + token-type URNs.
const (
	grantTokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
)

// defaultExchangeSkew is the cache-TTL safety margin: a cached exchanged token is
// treated as stale this long before its real expiry, so a call never presents a
// token that expires in flight.
const defaultExchangeSkew = 30 * time.Second

// exchangeSigAlgs are the signature algorithms we accept when DECODING (not
// verifying) the inbound bearer's claims. The authn interceptor already verified
// the bearer's signature; here we only read `aud`/`sub`/`exp`, so this list just
// keeps go-jose's parser from rejecting a legitimately-signed token by algorithm.
var exchangeSigAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// ExchangeConfig configures an [Exchanger].
type ExchangeConfig struct {
	// TokenEndpoint is the STS / IdP token endpoint implementing the RFC 8693
	// token-exchange grant. Required.
	TokenEndpoint string
	// ClientID is the CALLING service's own client identity at the STS. Required.
	ClientID string
	// ClientSecret authenticates the calling service via client_secret_basic.
	// Supported here; the alternative — a signed client assertion
	// (client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer)
	// — is the production path and is documented in the how-to but not implemented
	// in this helper.
	ClientSecret string
	// Skew is the cache-TTL safety margin (default 30s): a cached token is retired
	// this long before its true expiry.
	Skew time.Duration
	// HTTPClient makes the exchange request. nil defaults to a 10s-timeout client.
	HTTPClient *http.Client
}

// Exchanger obtains a token scoped to a target audience on behalf of the inbound
// caller, via RFC 8693 token exchange, and caches the result. It implements
// authn.TokenSource. It is a DELEGATION (on-behalf-of) source: it requires the
// caller's inbound bearer on the context (stashed by the authn interceptor via
// middleware.WithInboundBearer) as the exchange `subject_token`; a pure
// service-identity call (client-credentials, no inbound caller) is a separate,
// future path. It fails closed everywhere — any error yields an error, never the
// raw inbound token sent to a different audience.
type Exchanger struct {
	endpoint     string
	clientID     string
	clientSecret string
	skew         time.Duration
	http         *http.Client

	mu       sync.Mutex
	cache    map[cacheKey]cacheEntry
	inflight map[cacheKey]*exchangeCall
}

type cacheKey struct{ subject, audience string }

type cacheEntry struct {
	token     string
	expiresAt time.Time // real token expiry MINUS skew: valid while now < expiresAt
}

// exchangeCall dedupes concurrent exchanges for the same key (single-flight), so
// a burst of callers on a cache miss makes ONE HTTP exchange, not N.
type exchangeCall struct {
	done  chan struct{}
	token string
	err   error
}

// NewExchanger constructs an [Exchanger] from cfg. It errors when the token
// endpoint or client id is missing (fail-closed configuration).
func NewExchanger(cfg ExchangeConfig) (*Exchanger, error) {
	if cfg.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc: ExchangeConfig.TokenEndpoint is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oidc: ExchangeConfig.ClientID is required (the calling service's client identity)")
	}
	skew := cfg.Skew
	if skew <= 0 {
		skew = defaultExchangeSkew
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Exchanger{
		endpoint:     cfg.TokenEndpoint,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		skew:         skew,
		http:         hc,
		cache:        map[cacheKey]cacheEntry{},
		inflight:     map[cacheKey]*exchangeCall{},
	}, nil
}

// TokenFor implements authn.TokenSource. It returns the inbound bearer unchanged
// when targetAudience is already one of the caller's audiences (passthrough
// within a trust domain), or a cached/freshly-exchanged token scoped to
// targetAudience otherwise. It fails closed: no inbound bearer, an undecodable
// bearer, or any exchange error yields an error.
func (e *Exchanger) TokenFor(ctx context.Context, targetAudience string) (string, error) {
	raw, ok := middleware.InboundBearerFromContext(ctx)
	if !ok || raw == "" {
		return "", fmt.Errorf("oidc: token exchange: no inbound bearer on context (nothing to act on behalf of; this is a delegation source)")
	}
	sub, aud, err := decodeSubAud(raw)
	if err != nil {
		return "", fmt.Errorf("oidc: token exchange: decode inbound bearer: %w", err)
	}
	// Passthrough: the target is already one of the caller's audiences (same trust
	// domain), so the inbound bearer is the correct token to present.
	if aud.Contains(targetAudience) {
		return raw, nil
	}

	key := cacheKey{subject: sub, audience: targetAudience}

	e.mu.Lock()
	if ent, ok := e.cache[key]; ok && time.Now().Before(ent.expiresAt) {
		e.mu.Unlock()
		return ent.token, nil
	}
	if call, ok := e.inflight[key]; ok {
		// Another goroutine is exchanging this exact key; wait for it.
		e.mu.Unlock()
		<-call.done
		return call.token, call.err
	}
	call := &exchangeCall{done: make(chan struct{})}
	e.inflight[key] = call
	e.mu.Unlock()

	token, expiresAt, err := e.exchange(ctx, raw, targetAudience)

	e.mu.Lock()
	delete(e.inflight, key)
	if err == nil {
		if fresh := expiresAt.Add(-e.skew); fresh.After(time.Now()) {
			e.cache[key] = cacheEntry{token: token, expiresAt: fresh}
		}
	}
	e.mu.Unlock()

	call.token, call.err = token, err
	close(call.done)
	return token, err
}

// exchange performs the RFC 8693 form-POST and returns the exchanged access token
// and its absolute expiry (the earlier of `expires_in` and the token's own `exp`).
func (e *Exchanger) exchange(ctx context.Context, subjectToken, targetAudience string) (string, time.Time, error) {
	form := url.Values{}
	form.Set("grant_type", grantTokenExchange)
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", tokenTypeJWT)
	form.Set("requested_token_type", tokenTypeAccessToken)
	form.Set("audience", targetAudience)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oidc: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Authenticate the CALLING service via client_secret_basic. (A signed client
	// assertion is the production alternative; see the how-to.)
	req.SetBasicAuth(e.clientID, e.clientSecret)

	resp, err := e.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oidc: exchange request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("oidc: exchange failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("oidc: parse exchange response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("oidc: exchange response has no access_token")
	}

	now := time.Now()
	var expiresAt time.Time
	if tr.ExpiresIn > 0 {
		expiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	// The STS emits an `act` (actor) claim naming the calling service; we do not
	// fabricate it — we only read the token's `exp` to bound the cache TTL.
	if _, _, exp, err := decodeClaims(tr.AccessToken); err == nil && !exp.IsZero() {
		if expiresAt.IsZero() || exp.Before(expiresAt) {
			expiresAt = exp
		}
	}
	return tr.AccessToken, expiresAt, nil
}

// decodeSubAud decodes (WITHOUT verifying) the `sub` and `aud` of a bearer. The
// signature was already verified by the authn interceptor.
func decodeSubAud(raw string) (string, jwt.Audience, error) {
	sub, aud, _, err := decodeClaims(raw)
	return sub, aud, err
}

// decodeClaims parses a signed JWT and returns its `sub`, `aud`, and `exp`
// WITHOUT verifying the signature (the inbound bearer was already verified
// upstream; an exchanged token is the STS's to vouch for).
func decodeClaims(raw string) (string, jwt.Audience, time.Time, error) {
	tok, err := jwt.ParseSigned(raw, exchangeSigAlgs)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("parse token: %w", err)
	}
	var std jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&std); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("read claims: %w", err)
	}
	var exp time.Time
	if std.Expiry != nil {
		exp = std.Expiry.Time()
	}
	return std.Subject, std.Audience, exp, nil
}
