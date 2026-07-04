package authn

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// ErrNotEntitled is returned by a [ClaimsMapper] when an identity is not
// entitled to enter the app (its app-access set does not include the app).
var ErrNotEntitled = errors.New("authn: identity not entitled to this app")

// ErrNoClaims is returned by a [ClaimsMapper] when an identity is entitled but
// has no authored claims for the app and the mapper is configured to require
// them.
var ErrNoClaims = errors.New("authn: no authored claims for identity in this app")

// StaticClaimsMapper is the dev-default [ClaimsMapper]: a manipulable,
// hot-reloadable static mapping from identity Subject to the [authz.Principal]
// that identity gets in THIS app. It is easy to edit at runtime (Set / Replace)
// so a developer can "give bob group X in tenant-b for this app" in seconds
// without a rebuild — the WS-026 dev-manipulability requirement (D10). It is NOT
// a production claims source.
//
// Entitlement: when RequireEntitlement is set (via [WithRequireEntitlement]),
// MapClaims first confirms the identity's app-access ([Identity.Apps]) includes
// AppName and returns [ErrNotEntitled] otherwise — enforcing the IdP's coarse
// app-access before authoring any app claims.
type StaticClaimsMapper struct {
	// AppName is the app/client name entitlement is checked against.
	AppName string

	mu                 sync.RWMutex
	bySubject          map[string]authz.Principal
	requireEntitlement bool
	requireClaims      bool
}

// StaticClaimsOption configures a [StaticClaimsMapper].
type StaticClaimsOption func(*StaticClaimsMapper)

// WithRequireEntitlement makes MapClaims fail closed with [ErrNotEntitled] when
// the identity's app-access set does not include AppName.
func WithRequireEntitlement() StaticClaimsOption {
	return func(m *StaticClaimsMapper) { m.requireEntitlement = true }
}

// WithRequireClaims makes MapClaims fail with [ErrNoClaims] when an entitled
// identity has no mapping entry (instead of returning a bare Subject-only
// principal).
func WithRequireClaims() StaticClaimsOption {
	return func(m *StaticClaimsMapper) { m.requireClaims = true }
}

// NewStaticClaimsMapper returns a mapper for appName seeded with bySubject
// (subject -> authored principal for this app). The seed map is copied.
func NewStaticClaimsMapper(appName string, bySubject map[string]authz.Principal, opts ...StaticClaimsOption) *StaticClaimsMapper {
	m := &StaticClaimsMapper{AppName: appName, bySubject: maps.Clone(bySubject)}
	if m.bySubject == nil {
		m.bySubject = map[string]authz.Principal{}
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Set installs (or replaces) the authored principal for one subject. Safe for
// concurrent use — this is the hot-reload path for editing a single identity's
// app claims live.
func (m *StaticClaimsMapper) Set(subject string, p authz.Principal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bySubject[subject] = p
}

// Replace swaps the entire subject->principal mapping atomically (hot-reload of
// a whole claims file). The provided map is copied.
func (m *StaticClaimsMapper) Replace(bySubject map[string]authz.Principal) {
	next := maps.Clone(bySubject)
	if next == nil {
		next = map[string]authz.Principal{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bySubject = next
}

// MapClaims implements [ClaimsMapper]. It confirms entitlement (when required),
// then returns the authored principal for id.Subject with Subject forced to the
// verified identity. The returned Groups/Scopes/Claims slices/maps are copies so
// callers cannot mutate the stored mapping.
func (m *StaticClaimsMapper) MapClaims(_ context.Context, id Identity) (authz.Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.requireEntitlement && m.AppName != "" && !slices.Contains(id.Apps, m.AppName) {
		return authz.Principal{}, ErrNotEntitled
	}
	p, ok := m.bySubject[id.Subject]
	if !ok {
		if m.requireClaims {
			return authz.Principal{}, ErrNoClaims
		}
		// Entitled but unmapped: a bare, authenticated-but-unprivileged principal.
		// authz still default-denies anything not granted.
		return authz.Principal{Subject: id.Subject}, nil
	}
	// Copy so the caller cannot mutate stored state; force Subject to the verified id.
	out := authz.Principal{
		Subject: id.Subject,
		Tenant:  p.Tenant,
		Groups:  slices.Clone(p.Groups),
		Scopes:  slices.Clone(p.Scopes),
	}
	out.Claims = maps.Clone(p.Claims)
	return out, nil
}
