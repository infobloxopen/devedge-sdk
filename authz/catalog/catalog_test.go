package catalog

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
)

func TestBuild(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/dns.v1.ZoneService/GetZone", Verb: authz.Get, Resource: "zone"},
		{Method: "/dns.v1.ZoneService/ListZones", Verb: authz.List, Resource: "zone"},
		{Method: "/dns.v1.ZoneService/CreateZone", Verb: authz.Create, Resource: "zone"},
		{Method: "/dns.v1.ZoneService/ExportZones", Verb: "download", Resource: "zone"},
		{Method: "/grpc.health.v1.Health/Check", Public: true}, // contributes nothing
	}

	cat := Build("dns", rules)

	if cat.Application != "dns" {
		t.Fatalf("application = %q", cat.Application)
	}
	if len(cat.Resources) != 1 {
		t.Fatalf("resources = %d, want 1 (zone)", len(cat.Resources))
	}
	zone := cat.Resources["zone"]

	wantVerbs := []string{"create", "download", "get", "list"}
	if !reflect.DeepEqual(zone.Verbs, wantVerbs) {
		t.Fatalf("verbs = %v, want %v", zone.Verbs, wantVerbs)
	}

	// View = {get,list,watch} ∩ supported = {get,list}; Manage adds create.
	if !reflect.DeepEqual(zone.Groups["View"], []string{"get", "list"}) {
		t.Fatalf("View = %v, want [get list]", zone.Groups["View"])
	}
	if !reflect.DeepEqual(zone.Groups["Manage"], []string{"create", "get", "list"}) {
		t.Fatalf("Manage = %v, want [create get list]", zone.Groups["Manage"])
	}
	if got := zone.Endpoints["get"]; !reflect.DeepEqual(got, []string{"/dns.v1.ZoneService/GetZone"}) {
		t.Fatalf("get endpoints = %v", got)
	}

	// JSON round-trips.
	b, err := cat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Catalog
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, cat) {
		t.Fatalf("JSON round-trip mismatch")
	}
}
