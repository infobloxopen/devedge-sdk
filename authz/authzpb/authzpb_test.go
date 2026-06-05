package authzpb_test

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/authzpb"
	"github.com/infobloxopen/devedge-sdk/authz/authzpb/internal/testpb" // registers descriptors + provides codegen table
	"github.com/infobloxopen/devedge-sdk/authz/catalog"
)

func TestRulesFromGlobal(t *testing.T) {
	got := map[string]authz.MethodRule{}
	for _, r := range authzpb.RulesFromGlobal() {
		got[r.Method] = r
	}

	want := []authz.MethodRule{
		{Method: "/testpb.ZoneService/GetZone", Verb: authz.Get, Resource: "zone"},
		{Method: "/testpb.ZoneService/CreateZone", Verb: authz.Create, Resource: "zone"},
		{Method: "/testpb.ZoneService/Healthz", Public: true},
	}
	for _, w := range want {
		g, ok := got[w.Method]
		if !ok {
			t.Fatalf("missing rule for %s", w.Method)
		}
		if g != w {
			t.Fatalf("%s = %+v, want %+v", w.Method, g, w)
		}
	}
	if _, ok := got["/testpb.ZoneService/Undeclared"]; ok {
		t.Fatalf("Undeclared method must be omitted (no annotation)")
	}

	// End-to-end: the extracted rules build the permission catalog.
	cat := catalog.Build("test", authzpb.RulesFromGlobal())
	zone, ok := cat.Resources["zone"]
	if !ok {
		t.Fatalf("catalog missing 'zone' resource")
	}
	if len(zone.Groups["View"]) == 0 || len(zone.Groups["Manage"]) == 0 {
		t.Fatalf("expected View/Manage groups, got %+v", zone.Groups)
	}
}

// TestCodegenMatchesReflection proves the two generator variants agree: the
// compile-time table emitted by protoc-gen-devedge-authz (testpb.ZoneServiceAuthzRules)
// is identical to what authzpb extracts from descriptors at runtime.
func TestCodegenMatchesReflection(t *testing.T) {
	reflected := map[string]authz.MethodRule{}
	for _, r := range authzpb.RulesFromGlobal() {
		reflected[r.Method] = r
	}
	for _, r := range testpb.ZoneServiceAuthzRules {
		got, ok := reflected[r.Method]
		if !ok {
			t.Fatalf("codegen rule %s not found via reflection", r.Method)
		}
		if got != r {
			t.Fatalf("mismatch for %s: reflection=%+v codegen=%+v", r.Method, got, r)
		}
	}
	if len(testpb.ZoneServiceAuthzRules) != len(reflected) {
		t.Fatalf("codegen has %d rules, reflection found %d", len(testpb.ZoneServiceAuthzRules), len(reflected))
	}
}
