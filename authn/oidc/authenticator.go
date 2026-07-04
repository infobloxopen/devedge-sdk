package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// defaultLeeway tolerates small clock skew when validating exp/nbf.
const defaultLeeway = 30 * time.Second

// KeySource supplies the verification JWKS to an [Authenticator]. StaticKeySet
// (in-process/test) and RemoteJWKS (an app's JWKS endpoint) are provided.
type KeySource interface {
	// keySet returns the current verification key set.
	keySet(ctx context.Context) (jose.JSONWebKeySet, error)
}

// StaticKeySet is a fixed [KeySource] — e.g. an in-process Issuer's KeySet(), or
// keys loaded from config. Verification does no network I/O.
type StaticKeySet struct{ Keys jose.JSONWebKeySet }

func (s StaticKeySet) keySet(context.Context) (jose.JSONWebKeySet, error) { return s.Keys, nil }

// RemoteJWKS fetches (and caches) a JWKS from a URL — the app's JWKS endpoint in
// two-tier, or the IdP's in single-issuer. It refreshes after TTL and on a kid
// miss (key rotation). Safe for concurrent use.
type RemoteJWKS struct {
	URL    string
	Client *http.Client  // nil -> http.DefaultClient
	TTL    time.Duration // <=0 -> 5m

	mu       sync.Mutex
	cached   jose.JSONWebKeySet
	fetched  time.Time
	haveOnce bool
}

func (r *RemoteJWKS) keySet(ctx context.Context) (jose.JSONWebKeySet, error) {
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.haveOnce && time.Since(r.fetched) < ttl {
		return r.cached, nil
	}
	ks, err := r.fetch(ctx)
	if err != nil {
		if r.haveOnce {
			return r.cached, nil // serve stale rather than fail on a transient fetch error
		}
		return jose.JSONWebKeySet{}, err
	}
	r.cached, r.fetched, r.haveOnce = ks, time.Now(), true
	return ks, nil
}

func (r *RemoteJWKS) fetch(ctx context.Context) (jose.JSONWebKeySet, error) {
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("oidc: fetch jwks %q: %w", r.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jose.JSONWebKeySet{}, fmt.Errorf("oidc: fetch jwks %q: status %d", r.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	var ks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &ks); err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("oidc: parse jwks: %w", err)
	}
	return ks, nil
}

// Config configures an [Authenticator].
type Config struct {
	// Keys is where verification keys come from (StaticKeySet or RemoteJWKS).
	// Required.
	Keys KeySource
	// ExpectedIssuer is the required `iss` — the app's identity in two-tier, the
	// IdP's in single-issuer. Required (fail closed).
	ExpectedIssuer string
	// ExpectedAudience is the required `aud` member — this microservice's audience.
	// Empty skips the audience check (discouraged outside single-issuer bootstrap).
	ExpectedAudience string
	// Leeway tolerates clock skew when validating exp/nbf. <=0 defaults to 30s.
	Leeway time.Duration
}

// Authenticator verifies app bearer tokens against a JWKS and maps their claims
// to an [authz.Principal]. It implements authn.Authenticator and is fail-closed:
// a bad signature, wrong issuer/audience, or expired token returns an error.
type Authenticator struct {
	keys     KeySource
	issuer   string
	audience string
	leeway   time.Duration
}

// NewAuthenticator constructs a verifier from cfg. It errors if the key source
// or expected issuer is missing (fail-closed configuration).
func NewAuthenticator(cfg Config) (*Authenticator, error) {
	if cfg.Keys == nil {
		return nil, fmt.Errorf("oidc: Keys (KeySource) is required")
	}
	if cfg.ExpectedIssuer == "" {
		return nil, fmt.Errorf("oidc: ExpectedIssuer is required (fail closed)")
	}
	lw := cfg.Leeway
	if lw <= 0 {
		lw = defaultLeeway
	}
	return &Authenticator{keys: cfg.Keys, issuer: cfg.ExpectedIssuer, audience: cfg.ExpectedAudience, leeway: lw}, nil
}

// Authenticate implements authn.Authenticator.
func (a *Authenticator) Authenticate(ctx context.Context, bearer string) (authz.Principal, error) {
	tok, err := jwt.ParseSigned(bearer, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return authz.Principal{}, fmt.Errorf("oidc: parse token: %w", err)
	}
	ks, err := a.keys.keySet(ctx)
	if err != nil {
		return authz.Principal{}, fmt.Errorf("oidc: key set: %w", err)
	}
	key, err := selectKey(ks, tok)
	if err != nil {
		return authz.Principal{}, err
	}

	var std jwt.Claims
	private := map[string]any{}
	if err := tok.Claims(key, &std, &private); err != nil {
		return authz.Principal{}, fmt.Errorf("oidc: verify signature: %w", err)
	}

	expected := jwt.Expected{Issuer: a.issuer, Time: time.Now()}
	if a.audience != "" {
		expected.AnyAudience = jwt.Audience{a.audience}
	}
	if err := std.ValidateWithLeeway(expected, a.leeway); err != nil {
		return authz.Principal{}, fmt.Errorf("oidc: claim validation: %w", err)
	}

	return principalFromClaims(std, private), nil
}

// selectKey picks the verification key matching the token's kid, or the sole key
// when the token omits a kid and exactly one key is available.
func selectKey(ks jose.JSONWebKeySet, tok *jwt.JSONWebToken) (any, error) {
	var kid string
	if len(tok.Headers) > 0 {
		kid = tok.Headers[0].KeyID
	}
	if kid != "" {
		if matches := ks.Key(kid); len(matches) > 0 {
			return matches[0].Key, nil
		}
		return nil, fmt.Errorf("oidc: no verification key for kid %q", kid)
	}
	if len(ks.Keys) == 1 {
		return ks.Keys[0].Key, nil
	}
	return nil, fmt.Errorf("oidc: token has no kid and key set is ambiguous (%d keys)", len(ks.Keys))
}

// principalFromClaims reconstructs the authored principal minted by Issuer.Mint.
func principalFromClaims(std jwt.Claims, private map[string]any) authz.Principal {
	p := authz.Principal{Subject: std.Subject}
	if v, ok := private[claimTenant].(string); ok {
		p.Tenant = v
	}
	p.Groups = stringSlice(private[claimGroups])
	p.Scopes = stringSlice(private[claimScopes])
	// Surface remaining private claims (never the ones already mapped).
	for k, v := range private {
		switch k {
		case claimTenant, claimGroups, claimScopes:
			continue
		}
		if p.Claims == nil {
			p.Claims = map[string]any{}
		}
		p.Claims[k] = v
	}
	return p
}

// stringSlice coerces a JSON claim ([]any of strings, or a single string) to
// []string.
func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}
