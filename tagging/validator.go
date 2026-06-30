package tagging

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sync"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/types"
)

// Validator applies per-tenant tag definitions locally against a fail-safe
// [Store]. It is the write-guard (implementing types.TagValidator) and the
// read-filter (FilterTags) for tag governance. It is safe for concurrent use
// (the Store is, and the compiled-pattern cache is a sync.Map). Construct with
// [New].
type Validator struct {
	store   Store
	reCache sync.Map // pattern string -> *regexp.Regexp or error (compile result, memoised)
}

// New returns a validator reading from store. The usual wiring is a static or
// file-backed rules.Source feeding a fail-safe rules.Cache:
//
//	src := rules.NewFileSource[tagging.DefinitionSet]("/etc/tags/defs.json", 0)
//	go src.Run(ctx)                          // FileSource has no Run; see rules docs
//	cache := rules.NewCache("tag-definitions", src)
//	go cache.Run(ctx)                        // also register cache as a health.Check
//	v := tagging.New(cache)                  // v is a types.TagValidator
func New(store Store) *Validator { return &Validator{store: store} }

// compile-time: the real fail-safe cache satisfies the Store seam, and the
// validator satisfies the SDK's existing tag-validation extension point.
var _ types.TagValidator = (*Validator)(nil)

// ValidateTags is the write-guard. It enforces the tenant's tag definitions on
// the tags being written, returning a descriptive error on the first violation.
//
// Posture (see the package doc):
//   - no tags                  → nil (nothing to govern)
//   - snapshot not ready       → ErrDefinitionsUnavailable (fail-closed)
//   - ready, no set for tenant → nil (permissive: no governance configured)
//   - per tag: unknown key     → allowed (permissive overlay)
//     revoked key      → rejected
//     restricted value → rejected unless in Values
//     regexp value     → rejected unless it matches Pattern (un-compilable Pattern rejects)
//     freeform/unset   → allowed
func (v *Validator) ValidateTags(ctx context.Context, t types.Tags) error {
	if len(t) == 0 {
		return nil
	}
	if !v.store.Ready() {
		return ErrDefinitionsUnavailable
	}
	ds, ok := v.defsFor(tenantFromContext(ctx))
	if !ok {
		return nil // ready, but no definitions configured → permissive overlay
	}
	for key, val := range t {
		def, ok := ds.Definitions[key]
		if !ok {
			continue // unknown key → allowed (definitions are an overlay, not an allowlist)
		}
		if def.Status == StatusRevoked {
			return fmt.Errorf("tagging: tag key %q is revoked", key)
		}
		switch def.Type {
		case TypeRestricted:
			if !slices.Contains(def.Values, val) {
				return fmt.Errorf("tagging: value %q is not permitted for restricted tag %q", val, key)
			}
		case TypeRegexp:
			re, err := v.compile(def.Pattern)
			if err != nil {
				return fmt.Errorf("tagging: tag %q has an invalid pattern %q: %w", key, def.Pattern, err)
			}
			if !re.MatchString(val) {
				if def.PatternDescription != "" {
					return fmt.Errorf("tagging: value %q does not match the pattern for tag %q (%s)", val, key, def.PatternDescription)
				}
				return fmt.Errorf("tagging: value %q does not match the pattern for tag %q", val, key)
			}
		default: // TypeFreeform or unset → any value allowed
		}
	}
	return nil
}

// FilterTags is the read-filter: it returns a copy of t with revoked tags
// stripped, so a tag whose definition has been retired becomes invisible. It is
// best-effort: when the snapshot is not ready, or the
// tenant has no definition set, t is returned unchanged — reads stay available
// rather than hiding everything during a tag-service outage. Tags with no
// definition are kept (permissive overlay).
func (v *Validator) FilterTags(ctx context.Context, t types.Tags) types.Tags {
	if len(t) == 0 {
		return t
	}
	if !v.store.Ready() {
		return t // best-effort: cannot determine which keys are revoked
	}
	ds, ok := v.defsFor(tenantFromContext(ctx))
	if !ok {
		return t
	}
	return t.Filter(func(key, _ string) bool {
		def, ok := ds.Definitions[key]
		return !ok || def.Status != StatusRevoked
	})
}

// defsFor returns the definition set for tenant, falling back to the global
// default set (the "" tenant) when the tenant has none — the same whole-set
// fallback featureflags uses (evaluator.go lookupFlagSet).
func (v *Validator) defsFor(tenant string) (DefinitionSet, bool) {
	if tenant != "" {
		if ds, ok := v.store.Get(tenant); ok {
			return ds, true
		}
	}
	return v.store.Get("")
}

// tenantFromContext reads the tenant from the request principal (P2). With no
// principal (an unauthenticated/public path) the tenant is "", so the global
// default definition set applies.
func tenantFromContext(ctx context.Context) string {
	if p, ok := middleware.PrincipalFromContext(ctx); ok {
		return p.Tenant
	}
	return ""
}

// compile returns the memoised compiled form of pattern. Both the compiled
// regexp and a compile error are cached, so a malformed pattern is reported
// without recompiling on every write.
func (v *Validator) compile(pattern string) (*regexp.Regexp, error) {
	if cached, ok := v.reCache.Load(pattern); ok {
		switch c := cached.(type) {
		case *regexp.Regexp:
			return c, nil
		case error:
			return nil, c
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		v.reCache.Store(pattern, err)
		return nil, err
	}
	v.reCache.Store(pattern, re)
	return re, nil
}
