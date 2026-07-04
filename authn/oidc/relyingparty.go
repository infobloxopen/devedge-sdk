package oidc

import (
	"context"
	"fmt"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/infobloxopen/devedge-sdk/authn"
)

// appsClaim is the coarse app-access entitlement claim the IdP asserts (which
// apps this identity may enter). It is the ONLY authorization-shaped claim the
// IdP is allowed to author (WS-026 §2.1 / D11); roles/tenant/scopes are authored
// downstream by the app identity's ClaimsMapper.
const appsClaim = "apps"

// RelyingParty is the confidential OIDC relying-party client that the app
// identity (Role 2) uses to complete the auth-code + PKCE dance with the upstream
// IdP and validate the returned identity assertion. It is the pluggable upstream
// seam (D2): point it at the dev IdP, Okta, Auth0, or Keycloak — microservices
// are unaffected because they trust the app's issuer, not this upstream.
//
// The typical flow: AuthCodeURL redirects the browser to the IdP; the IdP
// authenticates (passwordlessly, in dev) and redirects back with a code;
// Exchange verifies the id_token and returns the coarse [authn.Identity], which
// the app identity then maps to a principal (authn.ClaimsMapper) and mints an app
// bearer for (authn.Issuer).
type RelyingParty struct {
	provider *coreoidc.Provider
	verifier *coreoidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// RelyingPartyConfig configures a [RelyingParty].
type RelyingPartyConfig struct {
	// IssuerURL is the upstream IdP's issuer (its discovery is at
	// IssuerURL/.well-known/openid-configuration). Required.
	IssuerURL string
	// ClientID / ClientSecret are the app identity's confidential-client
	// credentials as registered at the IdP. Required.
	ClientID     string
	ClientSecret string
	// RedirectURL is the app identity's callback (where the IdP returns the code).
	RedirectURL string
	// Scopes are the requested scopes; "openid" is always included.
	Scopes []string
}

// NewRelyingParty performs OIDC discovery against the IdP and returns a
// configured relying party. It errors if discovery fails or required fields are
// missing (fail-closed configuration).
func NewRelyingParty(ctx context.Context, cfg RelyingPartyConfig) (*RelyingParty, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oidc: IssuerURL is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oidc: ClientID is required")
	}
	provider, err := coreoidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover issuer %q: %w", cfg.IssuerURL, err)
	}
	scopes := append([]string{coreoidc.ScopeOpenID}, cfg.Scopes...)
	return &RelyingParty{
		provider: provider,
		verifier: provider.Verifier(&coreoidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
	}, nil
}

// AuthCodeURL builds the authorization-endpoint redirect URL for a login,
// carrying the PKCE S256 challenge derived from verifier (use
// [oauth2.GenerateVerifier] to make one and keep it for Exchange). state should
// be a random anti-CSRF value the caller round-trips.
func (rp *RelyingParty) AuthCodeURL(state, verifier string) string {
	return rp.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Exchange trades the authorization code for tokens (sending the PKCE verifier),
// verifies the returned id_token against the IdP's JWKS + this client's audience,
// and returns the COARSE identity assertion (subject + name/email + app-access).
// It fails closed if the id_token is missing or fails verification.
func (rp *RelyingParty) Exchange(ctx context.Context, code, verifier string) (authn.Identity, error) {
	tok, err := rp.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return authn.Identity{}, fmt.Errorf("oidc: code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return authn.Identity{}, fmt.Errorf("oidc: token response has no id_token")
	}
	return rp.verifyIDToken(ctx, rawID)
}

// verifyIDToken validates the id_token and extracts the coarse identity.
func (rp *RelyingParty) verifyIDToken(ctx context.Context, rawID string) (authn.Identity, error) {
	idTok, err := rp.verifier.Verify(ctx, rawID)
	if err != nil {
		return authn.Identity{}, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	var claims struct {
		Name  string   `json:"name"`
		Email string   `json:"email"`
		Apps  []string `json:"apps"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return authn.Identity{}, fmt.Errorf("oidc: parse id_token claims: %w", err)
	}
	// Capture the full raw claim set for any additional coarse fields.
	raw := map[string]any{}
	_ = idTok.Claims(&raw)
	delete(raw, appsClaim)
	return authn.Identity{
		Subject: idTok.Subject,
		Name:    claims.Name,
		Email:   claims.Email,
		Apps:    claims.Apps,
		Raw:     raw,
	}, nil
}
