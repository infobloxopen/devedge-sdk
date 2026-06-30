// Package tagging is the local, in-process tag-governance validator (seam P3b
// for a richer rule type) — a second consumer of the per-tenant rules substrate
// (rules, seam P3a), after feature flags. A service validates resource tags
// against a synced local snapshot of tag definitions; the tag-definition service
// being down never blocks the data plane (subject to the configured fail
// posture), exactly as the change feed decouples from the outbox.
//
// It implements the SDK's existing extension seam types.TagValidator (the
// write-guard) and adds FilterTags (the read-filter), so an external
// tag-governance service can enforce per-tenant tag policy on every domain
// operation across every service WITHOUT forking the SDK or hand-editing each handler: the
// service authors definitions and feeds them through a rules.Source, and binds
// to this validator exactly as an external authz engine binds to
// authz.Authorizer. This is the structural twin of featureflags, with the rule
// type T = DefinitionSet instead of a flag set, plus a write/read guard.
//
// Definitions follow a common tag-governance model: per-key definitions with a
// type (freeform / restricted / regexp) and a status (active / revoked). The
// validator is mechanism, not policy — it distributes and applies whatever
// definitions a Source delivers; it does NOT author them.
//
// Governance posture (mechanism choices, fixed; the policy in the definitions
// is the variable part):
//
//   - Permissive overlay: a tag whose key has NO definition is allowed.
//     Definitions constrain only the keys they cover; they are a governance
//     overlay, not an allowlist.
//   - Fail-closed write: when the local snapshot is not ready (the rules source
//     is down or the cache has not warmed), a write that CARRIES tags is
//     rejected with ErrDefinitionsUnavailable. A write with no tags always
//     passes — there is nothing to govern.
//   - Best-effort read: FilterTags strips revoked tags when the snapshot is
//     ready; when it is not, it returns tags unchanged. Reads stay available; a
//     briefly-visible stale revoked tag is preferable to hiding every tag during
//     a tag-service outage.
//
// The package depends only on the standard library plus the SDK's types,
// middleware, and authz seams — no ORM, no transport, no new module — so the
// root module stays dependency-light. A real definition source (e.g. a
// tag-definition sync or a ConfigMap bridge) is a separate adapter built
// just-in-time, never in core.
package tagging

import "errors"

// DefinitionType is how a TagDefinition constrains the values of its key.
type DefinitionType string

const (
	// TypeFreeform allows any value for the key. The zero value is treated as
	// freeform: a definition that exists but specifies no value constraint
	// governs only the key's existence and status, not its values.
	TypeFreeform DefinitionType = "freeform"
	// TypeRestricted allows only the values listed in Definition.Values.
	TypeRestricted DefinitionType = "restricted"
	// TypeRegexp allows only values matching Definition.Pattern.
	TypeRegexp DefinitionType = "regexp"
)

// Status is the lifecycle state of a TagDefinition.
type Status string

const (
	// StatusActive is a live definition. The zero value is treated as active.
	StatusActive Status = "active"
	// StatusRevoked is a retired definition: its key is rejected on write and
	// stripped on read (revoked definitions are hidden, the common convention;
	// this validator also rejects them on write, which is stricter — a revoked
	// key cannot be set).
	StatusRevoked Status = "revoked"
)

// Definition is the governance rule for one tag key. It is plain JSON data
// distributed via a rules.Source[DefinitionSet]; this package interprets it but
// does not author it.
type Definition struct {
	// Key is the tag key this definition governs.
	Key string `json:"key"`
	// Type is the value constraint (freeform / restricted / regexp). Empty is
	// treated as freeform.
	Type DefinitionType `json:"type,omitempty"`
	// Values is the permitted set when Type is restricted.
	Values []string `json:"values,omitempty"`
	// Pattern is the RE2 regular expression a value must fully or partially
	// match when Type is regexp. A pattern that fails to compile makes the key
	// un-evaluable, so a write carrying that key is rejected (fail-closed).
	Pattern string `json:"pattern,omitempty"`
	// PatternDescription is human-readable guidance for the regexp, surfaced in
	// errors and tooling. It carries no semantics.
	PatternDescription string `json:"patternDescription,omitempty"`
	// Status is the lifecycle state. Empty is treated as active.
	Status Status `json:"status,omitempty"`
}

// DefinitionSet is the per-tenant ruleset distributed via
// rules.Source[DefinitionSet]: the tag definitions configured for one tenant,
// keyed by tag key. A tenant with no set falls back to the global default set
// (the "" tenant), then — when the snapshot is ready but no definition covers a
// key — to "allowed" (the permissive overlay).
type DefinitionSet struct {
	Definitions map[string]Definition `json:"definitions"`
}

// Store is the read side the [Validator] depends on: a fail-safe, per-tenant
// snapshot of definition sets plus a readiness signal. *rules.Cache[DefinitionSet]
// implements it, so the package need not import rules; tests can supply a
// trivial in-memory implementation. Ready drives the fail-closed write posture:
// a not-ready store means the validator cannot make a governance decision.
type Store interface {
	// Get returns the definition set for tenant and true, or the zero set and
	// false when the tenant has none.
	Get(tenant string) (DefinitionSet, bool)
	// Ready reports whether the snapshot has loaded. While false, the validator
	// fails closed on tag-carrying writes and degrades to a no-op on reads.
	Ready() bool
}

// ErrDefinitionsUnavailable is returned by [Validator.ValidateTags] when a write
// carries tags but the definition snapshot has not loaded (fail-closed). Map it
// to an unavailable/aborted status at the transport boundary so the caller can
// retry once definitions are loaded.
var ErrDefinitionsUnavailable = errors.New("tagging: definitions not loaded")
