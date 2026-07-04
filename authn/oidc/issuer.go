// Package oidc provides the JOSE/JWKS-backed concrete implementations of the
// devedge-sdk authn seams (WS-026): an [Issuer] that mints + signs the app
// bearer and serves its JWKS (Role 2), and an [Authenticator] that verifies a
// bearer against a JWKS (Role 3). Keeping the go-jose dependency in this nested
// module keeps the SDK root dependency-light.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// defaultKID derives a stable key id from the public key (RFC 7638 JWK
// thumbprint), so minted tokens and the served JWKS agree on the kid.
func defaultKID(pub *rsa.PublicKey) string {
	jwk := jose.JSONWebKey{Key: pub}
	tp, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "default"
	}
	return base64.RawURLEncoding.EncodeToString(tp)
}

// claimTenant/claimGroups/claimScopes are the private claim names the app bearer
// carries the authored principal in. They are symmetric across mint (Issuer) and
// verify (Authenticator) so a round-trip reconstructs the same principal.
const (
	claimTenant = "tenant"
	claimGroups = "groups"
	claimScopes = "scopes"
)

// Issuer mints + signs app bearer tokens for authored principals and serves its
// public verification keys as a JWKS. It implements authn.Issuer. The signing
// key is RSA; the key id (kid) ties minted tokens to the served JWKS so an
// [Authenticator] selects the right key.
type Issuer struct {
	issuer   string        // the `iss` claim: the app's identity
	audience []string      // the `aud` claim: the app's microservices
	ttl      time.Duration // token lifetime

	mu     sync.RWMutex
	signer jose.Signer
	kid    string
	pub    *rsa.PublicKey
	priv   *rsa.PrivateKey
}

// IssuerOption configures an [Issuer].
type IssuerOption func(*issuerConfig)

type issuerConfig struct {
	ttl     time.Duration
	key     *rsa.PrivateKey
	kid     string
	keyBits int
}

// WithTTL sets the minted-token lifetime (default 1h).
func WithTTL(d time.Duration) IssuerOption {
	return func(c *issuerConfig) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithSigningKey supplies the RSA signing key + its key id, instead of
// generating an ephemeral one. Use this to persist/rotate keys across restarts.
func WithSigningKey(key *rsa.PrivateKey, kid string) IssuerOption {
	return func(c *issuerConfig) {
		c.key = key
		c.kid = kid
	}
}

// WithKeyBits sets the generated RSA key size when no key is supplied
// (default 2048).
func WithKeyBits(bits int) IssuerOption {
	return func(c *issuerConfig) {
		if bits > 0 {
			c.keyBits = bits
		}
	}
}

// NewIssuer returns an Issuer that mints tokens with `iss`=issuer and
// `aud`=audience. With no [WithSigningKey], it generates an ephemeral RSA key at
// construction. It errors if key generation or signer construction fails.
func NewIssuer(issuer string, audience []string, opts ...IssuerOption) (*Issuer, error) {
	if issuer == "" {
		return nil, fmt.Errorf("oidc: issuer is required")
	}
	cfg := &issuerConfig{ttl: time.Hour, keyBits: 2048}
	for _, o := range opts {
		o(cfg)
	}

	key := cfg.key
	if key == nil {
		var err error
		key, err = rsa.GenerateKey(rand.Reader, cfg.keyBits)
		if err != nil {
			return nil, fmt.Errorf("oidc: generate signing key: %w", err)
		}
	}
	kid := cfg.kid
	if kid == "" {
		kid = defaultKID(&key.PublicKey)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("oidc: new signer: %w", err)
	}

	return &Issuer{
		issuer:   issuer,
		audience: audience,
		ttl:      cfg.ttl,
		signer:   signer,
		kid:      kid,
		pub:      &key.PublicKey,
		priv:     key,
	}, nil
}

// Mint implements authn.Issuer: it signs an app bearer encoding p's identity and
// authored claims (iss=the app, aud=the app's microservices, short-lived).
func (s *Issuer) Mint(_ context.Context, p authz.Principal) (string, error) {
	now := time.Now()
	s.mu.RLock()
	signer := s.signer
	iss, aud, ttl := s.issuer, s.audience, s.ttl
	s.mu.RUnlock()

	std := jwt.Claims{
		Issuer:   iss,
		Subject:  p.Subject,
		Audience: jwt.Audience(aud),
		Expiry:   jwt.NewNumericDate(now.Add(ttl)),
		IssuedAt: jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
	}
	private := map[string]any{}
	if p.Tenant != "" {
		private[claimTenant] = p.Tenant
	}
	if len(p.Groups) > 0 {
		private[claimGroups] = p.Groups
	}
	if len(p.Scopes) > 0 {
		private[claimScopes] = p.Scopes
	}
	for k, v := range p.Claims {
		// Never let extra claims shadow the standard/authored claim names.
		switch k {
		case claimTenant, claimGroups, claimScopes, "iss", "sub", "aud", "exp", "iat", "nbf":
			continue
		}
		private[k] = v
	}

	token, err := jwt.Signed(signer).Claims(std).Claims(private).Serialize()
	if err != nil {
		return "", fmt.Errorf("oidc: sign token: %w", err)
	}
	return token, nil
}

// JWKS returns the issuer's public verification keys as a JSON Web Key Set.
func (s *Issuer) JWKS() jose.JSONWebKeySet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       s.pub,
		KeyID:     s.kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}
}

// JWKSHandler serves the issuer's JWKS as application/json — mount it at the
// app's JWKS endpoint (e.g. via server.Config.HTTPHandlers) so verifiers can
// fetch the app's public keys.
func (s *Issuer) JWKSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(s.JWKS())
	})
}

// KeySet returns the issuer's public keys for an in-process [Authenticator]
// (no HTTP round-trip needed when the minter and verifier share a process/test).
func (s *Issuer) KeySet() jose.JSONWebKeySet { return s.JWKS() }
