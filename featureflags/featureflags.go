// Package featureflags is the local, in-process feature-flag evaluator (seam
// P3b) — the first consumer of the per-tenant rules substrate (rules, seam
// P3a). Services evaluate flags against a synced local snapshot; the flag
// service being down never slows or breaks a consumer, exactly as the change
// feed decouples from the outbox.
//
// The public surface is shaped after the OpenFeature client — typed accessors
// (Bool/String/Int/Float/Object) plus *Details forms carrying the chosen
// variant and a reason — because that shape is portable, typed-multivariate,
// and familiar. The SDK copies the ergonomics; it deliberately does NOT import
// the OpenFeature Go SDK in core, to stay dependency-light. A bridge adapter
// (a separate module) can let real OpenFeature providers plug in later.
//
// Targeting reuses the P2 identity SPI: the evaluation context is auto-filled
// from the request principal (middleware.PrincipalFromContext) — subject,
// tenant, groups, scopes, and JWT claims — so match expressions and percentage
// rollouts target real callers with no per-call wiring. Everything here is
// stdlib + the SDK's middleware/authz seams; flag data arrives through a
// pluggable rules.Source and is held in a fail-safe rules.Cache.
//
// Mechanism, not policy: this package evaluates flags; it does not author them.
// Flags are authored in an existing control plane (e.g. Kubernetes CRDs /
// GitOps); a Source adapter feeds them in.
package featureflags

// FlagSet is the per-tenant ruleset distributed via rules.Source[FlagSet]: the
// flags configured for one tenant. A tenant with no FlagSet falls back to the
// global default set (the "" tenant), then to the code default the caller
// passes each accessor.
type FlagSet struct {
	Flags map[string]Flag `json:"flags"`
}

// Flag is a single flag definition: a default value (or default variant) plus
// ordered targeting rules. Values are untyped (JSON bool/string/number/object);
// the typed accessor coerces and falls back to its code default on a mismatch.
type Flag struct {
	// Key is the flag's stable identifier.
	Key string `json:"key"`
	// Disabled turns the flag off: every lookup serves the caller's code default
	// with reason DISABLED, ignoring rules.
	Disabled bool `json:"disabled,omitempty"`
	// Default is the value served when no rule targets the caller. Ignored when
	// DefaultVariant is set.
	Default any `json:"default,omitempty"`
	// DefaultVariant, when set, names the entry of Variants used as the default.
	DefaultVariant string `json:"defaultVariant,omitempty"`
	// Variants are named values a rule (or DefaultVariant) can select.
	Variants map[string]any `json:"variants,omitempty"`
	// Rules are evaluated in order; the first whose match expressions all pass
	// and whose weight gate admits the caller selects its variant.
	Rules []Rule `json:"rules,omitempty"`
}

// Rule selects a variant for callers it targets. All Match expressions must
// pass (logical AND); an empty Match targets everyone. Weight is an optional
// percentage rollout.
type Rule struct {
	// Variant is the variant key this rule selects (must exist in Flag.Variants).
	Variant string `json:"variant"`
	// Match is the set of expressions that must all pass for the rule to apply.
	Match []MatchExpression `json:"match,omitempty"`
	// Weight is a percentage rollout in 1..99: the caller is admitted iff a
	// deterministic hash of (flag key, targeting key) buckets below it, so the
	// same caller is consistently in or out. 0 or >=100 means "all who match".
	Weight int `json:"weight,omitempty"`
}

// Operator is the comparison a [MatchExpression] applies.
type Operator string

const (
	// OpIn passes when any value of the attribute is in Values.
	OpIn Operator = "IN"
	// OpNotIn passes when no value of the attribute is in Values (including when
	// the attribute is absent).
	OpNotIn Operator = "NOT_IN"
	// OpExists passes when the attribute is present and non-empty (Values ignored).
	OpExists Operator = "EXISTS"
)

// MatchExpression tests one evaluation-context attribute (a claim name, or one
// of the synthetic keys "subject", "tenant", "groups", "scopes") against Values.
type MatchExpression struct {
	Key    string   `json:"key"`
	Op     Operator `json:"op"`
	Values []string `json:"values,omitempty"`
}

// Reason explains how a value was chosen, mirroring OpenFeature reason codes.
type Reason string

const (
	// ReasonStatic — served from the flag's default value/variant; no rule matched.
	ReasonStatic Reason = "STATIC"
	// ReasonTargetingMatch — a rule's match expressions selected the variant.
	ReasonTargetingMatch Reason = "TARGETING_MATCH"
	// ReasonSplit — selected by percentage rollout (weight).
	ReasonSplit Reason = "SPLIT"
	// ReasonDisabled — the flag is disabled; the caller's code default is served.
	ReasonDisabled Reason = "DISABLED"
	// ReasonDefault — no flag set or no such flag; the caller's code default is served.
	ReasonDefault Reason = "DEFAULT"
	// ReasonError — the flag value could not be coerced to the requested type;
	// the caller's code default is served.
	ReasonError Reason = "ERROR"
)

// Store is the read side the [Evaluator] depends on: a fail-safe, per-tenant
// snapshot of flag sets. *rules.Cache[FlagSet] implements it, so the package
// need not import rules; tests can supply a trivial in-memory implementation.
type Store interface {
	// Get returns the flag set for tenant and true, or the zero set and false
	// when the tenant has none.
	Get(tenant string) (FlagSet, bool)
}

// Details carries an evaluated value plus the chosen variant and the reason.
type Details[V any] struct {
	Value   V
	Variant string
	Reason  Reason
}

// Typed Details aliases for the four scalar accessors.
type (
	BoolDetails   = Details[bool]
	StringDetails = Details[string]
	IntDetails    = Details[int64]
	FloatDetails  = Details[float64]
	ObjectDetails = Details[any]
)
