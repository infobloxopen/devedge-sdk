package featureflags

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/infobloxopen/devedge-sdk/middleware"
)

// Evaluator resolves flags locally against a fail-safe [Store] of per-tenant
// flag sets. It is safe for concurrent use (the Store is). Construct with [New].
type Evaluator struct {
	store Store
}

// New returns an evaluator reading from store. The usual wiring is a static or
// file-backed rules.Source feeding a fail-safe rules.Cache:
//
//	src := rules.NewFileSource[featureflags.FlagSet]("/etc/flags/flags.json", 0)
//	go src.Run(ctx)
//	cache := rules.NewCache("feature-flags", src)
//	go cache.Run(ctx)                       // also register cache as a health.Check
//	ff := featureflags.New(cache)
func New(store Store) *Evaluator { return &Evaluator{store: store} }

// evalContext is the targeting input, auto-derived from the request principal.
type evalContext struct {
	targetingKey string         // stable id for percentage rollout
	attributes   map[string]any // claims + synthetic subject/tenant/groups/scopes
}

// resolution is the engine's internal result. found=false means "serve the
// caller's code default" (reason explains why: DEFAULT, DISABLED, or ERROR).
type resolution struct {
	value   any
	variant string
	reason  Reason
	found   bool
}

// Bool returns the flag's boolean value, or def when the flag is absent,
// disabled, or not a boolean.
func (e *Evaluator) Bool(ctx context.Context, key string, def bool) bool {
	return e.BoolDetails(ctx, key, def).Value
}

// BoolDetails is Bool with the chosen variant and reason.
func (e *Evaluator) BoolDetails(ctx context.Context, key string, def bool) BoolDetails {
	r := e.evaluate(ctx, key)
	if !r.found {
		return BoolDetails{Value: def, Reason: r.reason}
	}
	v, ok := r.value.(bool)
	if !ok {
		return BoolDetails{Value: def, Reason: ReasonError}
	}
	return BoolDetails{Value: v, Variant: r.variant, Reason: r.reason}
}

// String returns the flag's string value, or def when the flag is absent,
// disabled, or not a string.
func (e *Evaluator) String(ctx context.Context, key, def string) string {
	return e.StringDetails(ctx, key, def).Value
}

// StringDetails is String with the chosen variant and reason.
func (e *Evaluator) StringDetails(ctx context.Context, key, def string) StringDetails {
	r := e.evaluate(ctx, key)
	if !r.found {
		return StringDetails{Value: def, Reason: r.reason}
	}
	v, ok := r.value.(string)
	if !ok {
		return StringDetails{Value: def, Reason: ReasonError}
	}
	return StringDetails{Value: v, Variant: r.variant, Reason: r.reason}
}

// Int returns the flag's integer value, or def when the flag is absent,
// disabled, or not an integer. JSON numbers (float64) that are integral are
// accepted.
func (e *Evaluator) Int(ctx context.Context, key string, def int64) int64 {
	return e.IntDetails(ctx, key, def).Value
}

// IntDetails is Int with the chosen variant and reason.
func (e *Evaluator) IntDetails(ctx context.Context, key string, def int64) IntDetails {
	r := e.evaluate(ctx, key)
	if !r.found {
		return IntDetails{Value: def, Reason: r.reason}
	}
	v, ok := toInt64(r.value)
	if !ok {
		return IntDetails{Value: def, Reason: ReasonError}
	}
	return IntDetails{Value: v, Variant: r.variant, Reason: r.reason}
}

// Float returns the flag's float value, or def when the flag is absent,
// disabled, or not numeric.
func (e *Evaluator) Float(ctx context.Context, key string, def float64) float64 {
	return e.FloatDetails(ctx, key, def).Value
}

// FloatDetails is Float with the chosen variant and reason.
func (e *Evaluator) FloatDetails(ctx context.Context, key string, def float64) FloatDetails {
	r := e.evaluate(ctx, key)
	if !r.found {
		return FloatDetails{Value: def, Reason: r.reason}
	}
	v, ok := toFloat64(r.value)
	if !ok {
		return FloatDetails{Value: def, Reason: ReasonError}
	}
	return FloatDetails{Value: v, Variant: r.variant, Reason: r.reason}
}

// Object returns the flag's structured value as-is (e.g. a map decoded from
// JSON), or def when the flag is absent or disabled.
func (e *Evaluator) Object(ctx context.Context, key string, def any) any {
	return e.ObjectDetails(ctx, key, def).Value
}

// ObjectDetails is Object with the chosen variant and reason.
func (e *Evaluator) ObjectDetails(ctx context.Context, key string, def any) ObjectDetails {
	r := e.evaluate(ctx, key)
	if !r.found {
		return ObjectDetails{Value: def, Reason: r.reason}
	}
	return ObjectDetails{Value: r.value, Variant: r.variant, Reason: r.reason}
}

// evaluate is the core engine: resolve the tenant's flag set, find the flag,
// apply its rules against the auto-derived evaluation context, and select a
// value. It never returns an error — failure paths set found=false so the typed
// accessor serves the caller's code default.
func (e *Evaluator) evaluate(ctx context.Context, key string) resolution {
	ec, tenant := e.contextFromCtx(ctx)

	fs, ok := e.lookupFlagSet(tenant)
	if !ok {
		return resolution{reason: ReasonDefault}
	}
	flag, ok := fs.Flags[key]
	if !ok {
		return resolution{reason: ReasonDefault}
	}
	if flag.Disabled {
		return resolution{reason: ReasonDisabled}
	}

	for _, rule := range flag.Rules {
		if !matchAll(ec, rule.Match) || !weightGate(key, ec.targetingKey, rule.Weight) {
			continue
		}
		v, ok := flag.Variants[rule.Variant]
		if !ok {
			continue // misconfigured rule: variant not defined — skip, don't fail
		}
		reason := ReasonTargetingMatch
		if effectiveWeight(rule.Weight) < 100 {
			reason = ReasonSplit
		}
		return resolution{value: v, variant: rule.Variant, reason: reason, found: true}
	}

	// No rule matched: serve the flag's default variant/value.
	if flag.DefaultVariant != "" {
		if v, ok := flag.Variants[flag.DefaultVariant]; ok {
			return resolution{value: v, variant: flag.DefaultVariant, reason: ReasonStatic, found: true}
		}
	}
	if flag.Default != nil {
		return resolution{value: flag.Default, reason: ReasonStatic, found: true}
	}
	return resolution{reason: ReasonDefault}
}

// lookupFlagSet returns the tenant's flag set, falling back to the global
// default set (the "" tenant) when the tenant has none.
func (e *Evaluator) lookupFlagSet(tenant string) (FlagSet, bool) {
	if tenant != "" {
		if fs, ok := e.store.Get(tenant); ok {
			return fs, true
		}
	}
	return e.store.Get("")
}

// contextFromCtx builds the evaluation context from the request principal (P2).
// With no principal (an unauthenticated/public path) the context is empty: only
// flags with a default or an empty-match rule resolve.
func (e *Evaluator) contextFromCtx(ctx context.Context) (evalContext, string) {
	ec := evalContext{attributes: map[string]any{}}
	p, ok := middleware.PrincipalFromContext(ctx)
	if !ok {
		return ec, ""
	}
	ec.targetingKey = p.Subject
	if ec.targetingKey == "" {
		ec.targetingKey = p.Tenant
	}
	ec.attributes["subject"] = p.Subject
	ec.attributes["tenant"] = p.Tenant
	ec.attributes["groups"] = p.Groups
	ec.attributes["scopes"] = p.Scopes
	for k, v := range p.Claims {
		ec.attributes[k] = v
	}
	return ec, p.Tenant
}

// matchAll reports whether every expression passes (logical AND). An empty set
// matches everyone.
func matchAll(ec evalContext, exprs []MatchExpression) bool {
	for _, ex := range exprs {
		if !matchOne(ec, ex) {
			return false
		}
	}
	return true
}

func matchOne(ec evalContext, ex MatchExpression) bool {
	raw, present := ec.attributes[ex.Key]
	switch ex.Op {
	case OpExists:
		return present && len(attrStrings(raw)) > 0
	case OpIn:
		return anyIn(attrStrings(raw), ex.Values)
	case OpNotIn:
		return !anyIn(attrStrings(raw), ex.Values)
	default:
		return false
	}
}

// attrStrings normalises an attribute value to a set of strings for comparison.
func attrStrings(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	default:
		return []string{fmt.Sprint(t)}
	}
}

func anyIn(have, want []string) bool {
	if len(have) == 0 || len(want) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(want))
	for _, w := range want {
		set[w] = struct{}{}
	}
	for _, h := range have {
		if _, ok := set[h]; ok {
			return true
		}
	}
	return false
}

// effectiveWeight maps an unset/non-positive weight to 100 ("all who match").
func effectiveWeight(weight int) int {
	if weight <= 0 {
		return 100
	}
	return weight
}

// weightGate admits the caller into a percentage rollout deterministically: the
// same (flag, targeting key) is consistently in or out, so a 25% rollout always
// includes the same quarter of callers and a re-evaluation never flips.
func weightGate(flagKey, targetingKey string, weight int) bool {
	w := effectiveWeight(weight)
	if w >= 100 {
		return true
	}
	return bucket(flagKey, targetingKey) < w
}

func bucket(flagKey, targetingKey string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(flagKey))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(targetingKey))
	return int(h.Sum32() % 100)
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64: // JSON numbers decode to float64
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return int64(t), true
		}
	case float32:
		f := float64(t)
		if f == math.Trunc(f) {
			return int64(f), true
		}
	}
	return 0, false
}

func toFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	}
	return 0, false
}
