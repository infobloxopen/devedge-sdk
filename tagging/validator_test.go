package tagging_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/rules"
	"github.com/infobloxopen/devedge-sdk/tagging"
	"github.com/infobloxopen/devedge-sdk/types"
)

// store is a trivial in-memory Store for tests, with a Ready toggle so both the
// fail-closed (not ready) and governed (ready) paths can be exercised.
type store struct {
	sets  map[string]tagging.DefinitionSet
	ready bool
}

func (s store) Get(tenant string) (tagging.DefinitionSet, bool) {
	ds, ok := s.sets[tenant]
	return ds, ok
}
func (s store) Ready() bool { return s.ready }

// compile-time: the real fail-safe cache satisfies the validator's Store seam,
// so a service wires rules.Cache[DefinitionSet] straight into tagging.New.
var _ tagging.Store = (*rules.Cache[tagging.DefinitionSet])(nil)

func defs(d ...tagging.Definition) tagging.DefinitionSet {
	m := make(map[string]tagging.Definition, len(d))
	for _, def := range d {
		m[def.Key] = def
	}
	return tagging.DefinitionSet{Definitions: m}
}

func ctxWith(p authz.Principal) context.Context {
	return middleware.WithPrincipal(context.Background(), p)
}

func TestValidate_EmptyTagsAllowedEvenWhenNotReady(t *testing.T) {
	v := tagging.New(store{ready: false})
	if err := v.ValidateTags(context.Background(), nil); err != nil {
		t.Fatalf("nil tags: %v", err)
	}
	if err := v.ValidateTags(context.Background(), types.Tags{}); err != nil {
		t.Fatalf("empty tags: %v", err)
	}
}

func TestValidate_FailClosedWhenNotReady(t *testing.T) {
	v := tagging.New(store{ready: false})
	err := v.ValidateTags(context.Background(), types.Tags{"env": "prod"})
	if !errors.Is(err, tagging.ErrDefinitionsUnavailable) {
		t.Fatalf("got %v, want ErrDefinitionsUnavailable", err)
	}
}

func TestValidate_PermissiveWhenReadyButNoSet(t *testing.T) {
	// Ready, but no set for this tenant and no global "" set → allow.
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{}})
	if err := v.ValidateTags(ctxWith(authz.Principal{Tenant: "t1"}), types.Tags{"anything": "goes"}); err != nil {
		t.Fatalf("permissive-when-no-set: %v", err)
	}
}

func TestValidate_UnknownKeyAllowed(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(tagging.Definition{Key: "env", Type: tagging.TypeRestricted, Values: []string{"prod"}}),
	}})
	// "team" has no definition → permissive overlay allows it.
	if err := v.ValidateTags(context.Background(), types.Tags{"team": "platform"}); err != nil {
		t.Fatalf("unknown key should be allowed: %v", err)
	}
}

func TestValidate_Restricted(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(tagging.Definition{Key: "env", Type: tagging.TypeRestricted, Values: []string{"prod", "dev"}}),
	}})
	if err := v.ValidateTags(context.Background(), types.Tags{"env": "prod"}); err != nil {
		t.Fatalf("allowed value rejected: %v", err)
	}
	if err := v.ValidateTags(context.Background(), types.Tags{"env": "staging"}); err == nil {
		t.Fatal("disallowed restricted value should be rejected")
	}
}

func TestValidate_Regexp(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(
			tagging.Definition{Key: "cost-center", Type: tagging.TypeRegexp, Pattern: `^CC-\d{4}$`},
			tagging.Definition{Key: "bad", Type: tagging.TypeRegexp, Pattern: `^[a-z`}, // un-compilable
		),
	}})
	if err := v.ValidateTags(context.Background(), types.Tags{"cost-center": "CC-1234"}); err != nil {
		t.Fatalf("matching value rejected: %v", err)
	}
	if err := v.ValidateTags(context.Background(), types.Tags{"cost-center": "1234"}); err == nil {
		t.Fatal("non-matching regexp value should be rejected")
	}
	// Un-compilable pattern is un-evaluable → fail-closed reject of that key.
	if err := v.ValidateTags(context.Background(), types.Tags{"bad": "x"}); err == nil {
		t.Fatal("value under an invalid pattern should be rejected")
	}
}

func TestValidate_FreeformAllowsAnyValue(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(
			tagging.Definition{Key: "note", Type: tagging.TypeFreeform},
			tagging.Definition{Key: "label"}, // unset type ⇒ treated as freeform
		),
	}})
	if err := v.ValidateTags(context.Background(), types.Tags{"note": "whatever", "label": "anything"}); err != nil {
		t.Fatalf("freeform/unset-type should allow any value: %v", err)
	}
}

func TestValidate_RevokedKeyRejected(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(tagging.Definition{Key: "legacy", Type: tagging.TypeFreeform, Status: tagging.StatusRevoked}),
	}})
	if err := v.ValidateTags(context.Background(), types.Tags{"legacy": "x"}); err == nil {
		t.Fatal("revoked key should be rejected on write")
	}
}

func TestValidate_PerTenantAndGlobalFallback(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"":   defs(tagging.Definition{Key: "env", Type: tagging.TypeFreeform}),                              // global: lax
		"t1": defs(tagging.Definition{Key: "env", Type: tagging.TypeRestricted, Values: []string{"prod"}}), // tenant: strict
	}})
	// t1 uses its own strict definition.
	if err := v.ValidateTags(ctxWith(authz.Principal{Tenant: "t1"}), types.Tags{"env": "dev"}); err == nil {
		t.Fatal("t1 strict definition should reject dev")
	}
	// t2 has no set → falls back to the lax global "" set.
	if err := v.ValidateTags(ctxWith(authz.Principal{Tenant: "t2"}), types.Tags{"env": "dev"}); err != nil {
		t.Fatalf("t2 should inherit lax global set: %v", err)
	}
}

func TestFilter_StripsRevoked(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{
		"": defs(
			tagging.Definition{Key: "legacy", Type: tagging.TypeFreeform, Status: tagging.StatusRevoked},
			tagging.Definition{Key: "env", Type: tagging.TypeFreeform, Status: tagging.StatusActive},
		),
	}})
	out := v.FilterTags(context.Background(), types.Tags{"legacy": "old", "env": "prod", "team": "x"})
	if _, ok := out["legacy"]; ok {
		t.Fatal("revoked tag should be stripped on read")
	}
	if out["env"] != "prod" {
		t.Fatal("active tag should be kept")
	}
	if out["team"] != "x" {
		t.Fatal("undefined tag should be kept (permissive overlay)")
	}
}

func TestFilter_BestEffortWhenNotReady(t *testing.T) {
	// Not ready: cannot know which keys are revoked → return unchanged.
	v := tagging.New(store{ready: false})
	in := types.Tags{"legacy": "old", "env": "prod"}
	out := v.FilterTags(context.Background(), in)
	if len(out) != 2 {
		t.Fatalf("not-ready filter should pass tags through unchanged, got %v", out)
	}
}

func TestFilter_EmptyAndNoSet(t *testing.T) {
	v := tagging.New(store{ready: true, sets: map[string]tagging.DefinitionSet{}})
	if out := v.FilterTags(context.Background(), nil); out != nil {
		t.Fatalf("nil in → nil out, got %v", out)
	}
	in := types.Tags{"a": "b"}
	if out := v.FilterTags(ctxWith(authz.Principal{Tenant: "t1"}), in); out["a"] != "b" {
		t.Fatalf("no set → unchanged, got %v", out)
	}
}
