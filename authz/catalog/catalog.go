// Package catalog turns declared method rules into the permission catalog — the
// code-backed source of truth that the API enforces, that a portal can render as
// a role-creation UI, and that a downstream engine/policy generator can consume.
//
// It is engine-neutral (no policy-engine coupling). It derives, per resource, the verbs the
// resource supports and the endpoints implementing each, plus the View/Manage
// intent groups (the canonical groups from the permission-standardization epic),
// each intersected with the verbs the resource actually supports.
package catalog

import (
	"encoding/json"
	"sort"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// Canonical intent groups (PTCI-3285): View = read-equivalent; Manage = full
// control. Each is intersected with the verbs a resource actually supports.
var (
	viewVerbs   = []authz.Verb{authz.Get, authz.List, authz.Watch}
	manageVerbs = []authz.Verb{authz.Create, authz.Update, authz.Delete, authz.Get, authz.List, authz.Watch}
)

// Catalog is the generated permission catalog for one application.
type Catalog struct {
	Application string              `json:"application"`
	Resources   map[string]Resource `json:"resources"`
}

// Resource is the catalog entry for one resource type.
type Resource struct {
	Verbs     []string            `json:"verbs"`     // supported verbs, sorted
	Endpoints map[string][]string `json:"endpoints"` // verb -> method ids, sorted
	Groups    map[string][]string `json:"groups"`    // intent group -> its supported verbs
}

// Build derives the catalog from declared rules. Public rules and rules missing a
// verb or resource contribute no permission.
func Build(application string, rules []authz.MethodRule) Catalog {
	type acc struct {
		verbs map[authz.Verb]bool
		eps   map[authz.Verb]map[string]bool
	}
	tmp := map[string]*acc{}

	for _, r := range rules {
		if r.Public || r.Verb == "" || r.Resource == "" {
			continue
		}
		a := tmp[r.Resource]
		if a == nil {
			a = &acc{verbs: map[authz.Verb]bool{}, eps: map[authz.Verb]map[string]bool{}}
			tmp[r.Resource] = a
		}
		a.verbs[r.Verb] = true
		if a.eps[r.Verb] == nil {
			a.eps[r.Verb] = map[string]bool{}
		}
		if r.Method != "" {
			a.eps[r.Verb][r.Method] = true
		}
	}

	cat := Catalog{Application: application, Resources: make(map[string]Resource, len(tmp))}
	for res, a := range tmp {
		endpoints := make(map[string][]string, len(a.eps))
		for v, set := range a.eps {
			endpoints[string(v)] = sortedKeys(set)
		}
		groups := map[string][]string{}
		if g := intersect(viewVerbs, a.verbs); len(g) > 0 {
			groups["View"] = g
		}
		if g := intersect(manageVerbs, a.verbs); len(g) > 0 {
			groups["Manage"] = g
		}
		cat.Resources[res] = Resource{
			Verbs:     verbStrings(a.verbs),
			Endpoints: endpoints,
			Groups:    groups,
		}
	}
	return cat
}

// JSON renders the catalog as indented JSON (the code-backed source of truth;
// trivially convertible to YAML for human review).
func (c Catalog) JSON() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }

// verbStrings returns the set's verbs as a sorted []string.
func verbStrings(set map[authz.Verb]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intersect returns the members of ordered that are present in set, preserving
// ordered's order.
func intersect(ordered []authz.Verb, set map[authz.Verb]bool) []string {
	var out []string
	for _, v := range ordered {
		if set[v] {
			out = append(out, string(v))
		}
	}
	return out
}
