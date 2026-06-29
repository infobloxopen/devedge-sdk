package featureflags_test

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/featureflags"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/rules"
)

// mapStore is a trivial in-memory Store for tests.
type mapStore map[string]featureflags.FlagSet

func (m mapStore) Get(tenant string) (featureflags.FlagSet, bool) {
	fs, ok := m[tenant]
	return fs, ok
}

// compile-time: the real fail-safe cache satisfies the evaluator's Store seam.
var _ featureflags.Store = (*rules.Cache[featureflags.FlagSet])(nil)

func ctxWith(p authz.Principal) context.Context {
	return middleware.WithPrincipal(context.Background(), p)
}

func TestBool_DefaultWhenNoFlagSet(t *testing.T) {
	e := featureflags.New(mapStore{})
	d := e.BoolDetails(context.Background(), "x", true)
	if d.Value != true || d.Reason != featureflags.ReasonDefault {
		t.Fatalf("got %+v, want value=true reason=DEFAULT", d)
	}
}

func TestBool_DefaultWhenFlagMissing(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{}}})
	if got := e.Bool(context.Background(), "missing", false); got != false {
		t.Fatalf("got %v, want false (code default)", got)
	}
}

func TestBool_Disabled(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"f": {Key: "f", Disabled: true, Default: true},
	}}})
	d := e.BoolDetails(context.Background(), "f", false)
	if d.Value != false || d.Reason != featureflags.ReasonDisabled {
		t.Fatalf("got %+v, want value=false (code default) reason=DISABLED", d)
	}
}

func TestStaticDefault(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"f": {Key: "f", Default: "on"},
	}}})
	d := e.StringDetails(context.Background(), "f", "fallback")
	if d.Value != "on" || d.Reason != featureflags.ReasonStatic {
		t.Fatalf("got %+v, want value=on reason=STATIC", d)
	}
}

func TestDefaultVariant(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"color": {Key: "color", DefaultVariant: "blue", Variants: map[string]any{"blue": "#00f", "red": "#f00"}},
	}}})
	d := e.StringDetails(context.Background(), "color", "")
	if d.Value != "#00f" || d.Variant != "blue" || d.Reason != featureflags.ReasonStatic {
		t.Fatalf("got %+v, want value=#00f variant=blue reason=STATIC", d)
	}
}

func TestTargetingMatch_Groups(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"beta": {
			Key:      "beta",
			Default:  false,
			Variants: map[string]any{"on": true},
			Rules: []featureflags.Rule{{
				Variant: "on",
				Match:   []featureflags.MatchExpression{{Key: "groups", Op: featureflags.OpIn, Values: []string{"beta-testers"}}},
			}},
		},
	}}})

	// In the targeted group → on, TARGETING_MATCH.
	d := e.BoolDetails(ctxWith(authz.Principal{Subject: "u1", Groups: []string{"beta-testers"}}), "beta", false)
	if d.Value != true || d.Reason != featureflags.ReasonTargetingMatch {
		t.Fatalf("targeted: got %+v, want value=true reason=TARGETING_MATCH", d)
	}
	// Not in the group → falls to static default.
	d = e.BoolDetails(ctxWith(authz.Principal{Subject: "u2", Groups: []string{"others"}}), "beta", false)
	if d.Value != false || d.Reason != featureflags.ReasonStatic {
		t.Fatalf("untargeted: got %+v, want value=false reason=STATIC", d)
	}
}

func TestMatch_NotInAndExists(t *testing.T) {
	store := mapStore{"": {Flags: map[string]featureflags.Flag{
		"f": {Key: "f", Default: "base", Variants: map[string]any{"v": "matched"}, Rules: []featureflags.Rule{{
			Variant: "v",
			Match: []featureflags.MatchExpression{
				{Key: "region", Op: featureflags.OpExists},
				{Key: "tenant", Op: featureflags.OpNotIn, Values: []string{"blocked"}},
			},
		}}},
	}}}
	e := featureflags.New(store)

	// region present + tenant not blocked → matched.
	got := e.String(ctxWith(authz.Principal{Tenant: "t1", Claims: map[string]any{"region": "us"}}), "f", "")
	if got != "matched" {
		t.Fatalf("got %q, want matched", got)
	}
	// region present but tenant blocked → NOT_IN fails → default.
	got = e.String(ctxWith(authz.Principal{Tenant: "blocked", Claims: map[string]any{"region": "us"}}), "f", "")
	if got != "base" {
		t.Fatalf("got %q, want base (blocked tenant)", got)
	}
	// region absent → EXISTS fails → default.
	got = e.String(ctxWith(authz.Principal{Tenant: "t1"}), "f", "")
	if got != "base" {
		t.Fatalf("got %q, want base (no region)", got)
	}
}

func TestWeightRollout_DeterministicAndProportional(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"roll": {Key: "roll", Default: false, Variants: map[string]any{"on": true}, Rules: []featureflags.Rule{{
			Variant: "on", Weight: 30, // 30% rollout, no match exprs
		}}},
	}}})

	// Determinism: same subject, repeated calls, same answer.
	c := ctxWith(authz.Principal{Subject: "stable-user"})
	first := e.Bool(c, "roll", false)
	for i := 0; i < 50; i++ {
		if e.Bool(c, "roll", false) != first {
			t.Fatal("weight rollout not deterministic for a fixed subject")
		}
	}

	// Proportionality + SPLIT reason on the included cohort.
	in := 0
	const n = 5000
	for i := 0; i < n; i++ {
		sub := "user-" + itoa(i)
		d := e.BoolDetails(ctxWith(authz.Principal{Subject: sub}), "roll", false)
		if d.Value {
			in++
			if d.Reason != featureflags.ReasonSplit {
				t.Fatalf("included caller %s reason=%s, want SPLIT", sub, d.Reason)
			}
		}
	}
	frac := float64(in) / float64(n)
	if frac < 0.27 || frac > 0.33 {
		t.Fatalf("rollout fraction %.3f, want ~0.30 (±0.03)", frac)
	}
}

func TestTypeMismatch_IsError(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"s": {Key: "s", Default: "not-a-bool"},
	}}})
	d := e.BoolDetails(context.Background(), "s", true)
	if d.Value != true || d.Reason != featureflags.ReasonError {
		t.Fatalf("got %+v, want value=true (code default) reason=ERROR", d)
	}
}

func TestInt_FromJSONFloat(t *testing.T) {
	// JSON numbers decode to float64; an integral one coerces to int64.
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"limit": {Key: "limit", Default: float64(42)},
	}}})
	if got := e.Int(context.Background(), "limit", 0); got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
	// A non-integral float is not a valid Int → ERROR → code default.
	e2 := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"ratio": {Key: "ratio", Default: float64(1.5)},
	}}})
	if d := e2.IntDetails(context.Background(), "ratio", 7); d.Value != 7 || d.Reason != featureflags.ReasonError {
		t.Fatalf("got %+v, want value=7 reason=ERROR", d)
	}
}

func TestPerTenantOverrideAndGlobalFallback(t *testing.T) {
	e := featureflags.New(mapStore{
		"": {Flags: map[string]featureflags.Flag{ // global default
			"f": {Key: "f", Default: "global"},
		}},
		"t1": {Flags: map[string]featureflags.Flag{ // tenant override
			"f": {Key: "f", Default: "tenant"},
		}},
	})
	// t1 sees its own value.
	if got := e.String(ctxWith(authz.Principal{Tenant: "t1"}), "f", ""); got != "tenant" {
		t.Fatalf("t1 got %q, want tenant", got)
	}
	// t2 has no flag set → falls back to the global "" set.
	if got := e.String(ctxWith(authz.Principal{Tenant: "t2"}), "f", ""); got != "global" {
		t.Fatalf("t2 got %q, want global", got)
	}
}

func TestNoPrincipal_OnlyDefaultsResolve(t *testing.T) {
	e := featureflags.New(mapStore{"": {Flags: map[string]featureflags.Flag{
		"targeted": {Key: "targeted", Default: false, Variants: map[string]any{"on": true}, Rules: []featureflags.Rule{{
			Variant: "on", Match: []featureflags.MatchExpression{{Key: "groups", Op: featureflags.OpIn, Values: []string{"beta"}}},
		}}},
	}}})
	// No principal on ctx: targeting can't match, default is served.
	d := e.BoolDetails(context.Background(), "targeted", false)
	if d.Value != false || d.Reason != featureflags.ReasonStatic {
		t.Fatalf("got %+v, want value=false reason=STATIC", d)
	}
}

// itoa avoids importing strconv just for the rollout test loop.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
