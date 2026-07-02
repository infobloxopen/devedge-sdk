package federationgql_test

// federationgql_test exercises the schema builder + edge resolution + read_mask
// pushdown with IN-PROCESS fakes (F042 AC-1): no service processes, no gRPC. The
// keystone single-BatchGet guarantee (D-3) is proven here with a counting
// BatchGetter, and again across real listeners in the sample app's e2e
// (examples/graphql-federation, AC-2/3/4/5).

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/graphql-go/graphql"

	"github.com/infobloxopen/devedge-sdk/federationgql"
	"github.com/infobloxopen/devedge-sdk/reference"
)

// --- in-process fakes -------------------------------------------------------

type region struct {
	ID   string
	Name string
}

type asset struct {
	ID       string
	Name     string
	RegionID string
}

// countingRegionGetter is the reference.BatchGetter[*region] the resolver hands
// to reference.Load; it counts BatchGet calls (the anti-N+1 assertion) and
// records the last mask it observed (read_mask pushdown).
type countingRegionGetter struct {
	regions  map[string]*region
	calls    int
	lastMask []string
}

func (g *countingRegionGetter) BatchGet(ctx context.Context, ids []string) ([]*region, error) {
	g.calls++
	g.lastMask = federationgql.ReadMaskFromContext(ctx, "region.example.com/Region")
	out := make([]*region, 0, len(ids))
	for _, id := range ids {
		if r, ok := g.regions[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func regionResource() federationgql.Resource {
	return federationgql.Resource{
		Type: "region.example.com/Region",
		Name: "Region",
		Scalars: []federationgql.ScalarField{
			{Name: "id", MaskPath: "id", Resolve: func(s any) any { return s.(*region).ID }},
			{Name: "name", MaskPath: "display_name", Resolve: func(s any) any { return s.(*region).Name }},
		},
		IDOf: func(s any) string { return s.(*region).ID },
	}
}

func assetResource(regions map[string]*region, assets []*asset) federationgql.Resource {
	return federationgql.Resource{
		Type: "asset.example.com/Asset",
		Name: "Asset",
		Scalars: []federationgql.ScalarField{
			{Name: "id", MaskPath: "id", Resolve: func(s any) any { return s.(*asset).ID }},
			{Name: "name", MaskPath: "display_name", Resolve: func(s any) any { return s.(*asset).Name }},
		},
		References: []reference.Reference{{
			FieldName:   "RegionId",
			FKField:     "region_id",
			TargetType:  "region.example.com/Region",
			Cardinality: reference.One,
		}},
		Get: func(_ context.Context, args federationgql.GetArgs) (any, error) {
			for _, a := range assets {
				if a.ID == args.ID {
					return a, nil
				}
			}
			return nil, nil
		},
		List: func(_ context.Context, _ federationgql.ListArgs) ([]any, error) {
			out := make([]any, len(assets))
			for i, a := range assets {
				out[i] = a
			}
			return out, nil
		},
		IDOf:   func(s any) string { return s.(*asset).ID },
		RefIDs: func(_ reference.Reference, s any) []string { return []string{s.(*asset).RegionID} },
	}
}

// site is a SECOND source type that also references Region — used to prove the
// preload dedup is keyed by (target type, id), not a bare "target loaded" flag,
// so a second collection's distinct regions are not silently dropped.
type site struct {
	ID       string
	RegionID string
}

func siteResource(sites []*site) federationgql.Resource {
	return federationgql.Resource{
		Type: "site.example.com/Site",
		Name: "Site",
		Scalars: []federationgql.ScalarField{
			{Name: "id", MaskPath: "id", Resolve: func(s any) any { return s.(*site).ID }},
		},
		References: []reference.Reference{{
			FieldName:   "RegionId",
			FKField:     "region_id",
			TargetType:  "region.example.com/Region",
			Cardinality: reference.One,
		}},
		List: func(_ context.Context, _ federationgql.ListArgs) ([]any, error) {
			out := make([]any, len(sites))
			for i, s := range sites {
				out[i] = s
			}
			return out, nil
		},
		Get:    func(_ context.Context, _ federationgql.GetArgs) (any, error) { return nil, nil },
		IDOf:   func(s any) string { return s.(*site).ID },
		RefIDs: func(_ reference.Reference, s any) []string { return []string{s.(*site).RegionID} },
	}
}

// TestTwoSourcesShareTargetType is the regression for the preload dedup bug: two
// distinct source collections (assets + sites) reference the SAME target type in
// one query. Each must resolve its OWN regions — a dedup keyed only by target
// type would fetch assets' regions, mark Region "loaded", and drop sites'
// distinct regions to null.
func TestTwoSourcesShareTargetType(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{
		"r1": {ID: "r1", Name: "us-east"},
		"r2": {ID: "r2", Name: "eu-west"},
		"r3": {ID: "r3", Name: "ap-south"}, // only sites reference r3
	}}
	assets := []*asset{{ID: "a1", RegionID: "r1"}, {ID: "a2", RegionID: "r2"}}
	sites := []*site{{ID: "s1", RegionID: "r2"}, {ID: "s2", RegionID: "r3"}}

	resolver := reference.NewStaticResolver()
	resolver.Register("region.example.com/Region", federationgql.AnyGetter[*region](getter))
	schema, err := federationgql.NewSchema(
		[]federationgql.Resource{assetResource(getter.regions, assets), siteResource(sites), regionResource()},
		resolver,
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}

	res := federationgql.Execute(context.Background(), schema,
		`{ assets { id region { id name } } sites { id region { id name } } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	data := decode(t, res)

	// Sites' region r3 (never referenced by assets) MUST resolve — the bug dropped it.
	siteRows := data["sites"].([]any)
	s2 := siteRows[1].(map[string]any)
	reg := s2["region"]
	if reg == nil {
		t.Fatal("site s2's region (r3) resolved to null — second-source target was dropped (dedup bug)")
	}
	if reg.(map[string]any)["name"] != "ap-south" {
		t.Errorf("s2.region.name = %v, want ap-south", reg.(map[string]any)["name"])
	}

	// Assets still resolve their regions.
	assetRows := data["assets"].([]any)
	a1reg := assetRows[0].(map[string]any)["region"].(map[string]any)
	if a1reg["name"] != "us-east" {
		t.Errorf("a1.region.name = %v, want us-east", a1reg["name"])
	}

	// Batching intact: assets need {r1,r2} (1 call), sites add only {r3} not
	// already fetched (1 more call) — 2 total, NOT one-per-row (4).
	if getter.calls != 2 {
		t.Fatalf("want 2 BatchGet (one per collection's missing ids), got %d", getter.calls)
	}
}

func buildTestSchema(t *testing.T, getter *countingRegionGetter, assets []*asset) graphql.Schema {
	t.Helper()
	resolver := reference.NewStaticResolver()
	resolver.Register("region.example.com/Region", federationgql.AnyGetter[*region](getter))
	schema, err := federationgql.NewSchema(
		[]federationgql.Resource{assetResource(getter.regions, assets), regionResource()},
		resolver,
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return schema
}

// --- tests ------------------------------------------------------------------

// TestNewSchema_BuildsTypesAndRoots is AC-1: the schema has Asset/Region types,
// an Asset.region edge, and root assets/asset(id)/regions/region(id) fields.
func TestNewSchema_BuildsTypesAndRoots(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{}}
	schema := buildTestSchema(t, getter, nil)

	if schema.Type("Asset") == nil {
		t.Error("schema missing Asset type")
	}
	if schema.Type("Region") == nil {
		t.Error("schema missing Region type")
	}
	assetObj, ok := schema.Type("Asset").(*graphql.Object)
	if !ok {
		t.Fatalf("Asset is not an object type")
	}
	if _, ok := assetObj.Fields()["region"]; !ok {
		t.Errorf("Asset type missing region edge; fields=%v", fieldNames(assetObj))
	}
	q := schema.QueryType()
	for _, want := range []string{"assets", "asset", "regions", "region"} {
		if _, ok := q.Fields()[want]; !ok {
			t.Errorf("root Query missing field %q; fields=%v", want, fieldNames(q))
		}
	}
}

// TestNewSchema_Errors covers the descriptor validation failures.
func TestNewSchema_Errors(t *testing.T) {
	resolver := reference.NewStaticResolver()
	// nil resolver
	if _, err := federationgql.NewSchema(nil, nil); err == nil {
		t.Error("expected error for nil resolver")
	}
	// reference target with no registered resource
	_, err := federationgql.NewSchema([]federationgql.Resource{{
		Type: "asset.example.com/Asset", Name: "Asset",
		References: []reference.Reference{{TargetType: "region.example.com/Region", Cardinality: reference.One}},
		IDOf:       func(any) string { return "" },
	}}, resolver)
	if err == nil {
		t.Error("expected error for unregistered reference target type")
	}
	// duplicate type
	_, err = federationgql.NewSchema([]federationgql.Resource{
		{Type: "x/Y", Name: "Y1", IDOf: func(any) string { return "" }},
		{Type: "x/Y", Name: "Y2", IDOf: func(any) string { return "" }},
	}, resolver)
	if err == nil {
		t.Error("expected error for duplicate resource Type")
	}
}

// TestCrossServiceQuery_SingleBatchGet is the keystone at unit level: N assets
// referencing M distinct regions resolve their region edges in exactly ONE
// BatchGet (D-3). A per-row resolver would drive calls to N and fail here.
func TestCrossServiceQuery_SingleBatchGet(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{
		"r1": {ID: "r1", Name: "us-east"},
		"r2": {ID: "r2", Name: "eu-west"},
	}}
	assets := []*asset{
		{ID: "a1", Name: "asset-1", RegionID: "r1"},
		{ID: "a2", Name: "asset-2", RegionID: "r2"},
		{ID: "a3", Name: "asset-3", RegionID: "r1"},
		{ID: "a4", Name: "asset-4", RegionID: "r2"},
		{ID: "a5", Name: "asset-5", RegionID: "r1"},
	}
	schema := buildTestSchema(t, getter, assets)

	res := federationgql.Execute(context.Background(), schema,
		`{ assets { id name region { id name } } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	if getter.calls != 1 {
		t.Fatalf("want exactly 1 BatchGet for %d assets, got %d (anti-N+1 broken)", len(assets), getter.calls)
	}

	data := decode(t, res)
	rows := data["assets"].([]any)
	if len(rows) != 5 {
		t.Fatalf("want 5 assets, got %d", len(rows))
	}
	a1 := rows[0].(map[string]any)
	reg := a1["region"].(map[string]any)
	if reg["name"] != "us-east" {
		t.Errorf("a1.region.name = %v, want us-east", reg["name"])
	}
}

// TestReadMaskPushdown is AC-4 at unit level: { assets { region { name } } }
// narrows the region fetch's read_mask to display_name (+ nothing else — id was
// not selected).
func TestReadMaskPushdown(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{
		"r1": {ID: "r1", Name: "us-east"},
	}}
	assets := []*asset{{ID: "a1", Name: "asset-1", RegionID: "r1"}}
	schema := buildTestSchema(t, getter, assets)

	res := federationgql.Execute(context.Background(), schema, `{ assets { region { name } } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	got := append([]string(nil), getter.lastMask...)
	sort.Strings(got)
	if want := []string{"display_name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("region read_mask = %v, want %v", got, want)
	}

	// Selecting both id and name widens the mask.
	getter.lastMask = nil
	res = federationgql.Execute(context.Background(), schema, `{ assets { region { id name } } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	got = append([]string(nil), getter.lastMask...)
	sort.Strings(got)
	if want := []string{"display_name", "id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("region read_mask = %v, want %v", got, want)
	}
}

// TestReadMask_FragmentMergedEdge is the regression for the mask-derivation gap:
// when the region edge is selected in TWO places (inline + fragment) with
// different scalars, the pushed-down mask must UNION them — the resolver is asked
// for the merged selection, so a mask covering only the first occurrence would
// under-fetch.
func TestReadMask_FragmentMergedEdge(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{"r1": {ID: "r1", Name: "us-east"}}}
	assets := []*asset{{ID: "a1", Name: "asset-1", RegionID: "r1"}}
	schema := buildTestSchema(t, getter, assets)

	res := federationgql.Execute(context.Background(), schema,
		`{ assets { region { name } ...F } } fragment F on Asset { region { id } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	got := append([]string(nil), getter.lastMask...)
	sort.Strings(got)
	if want := []string{"display_name", "id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged region read_mask = %v, want %v (mask must union both edge occurrences)", got, want)
	}
}

// TestReadMask_AliasedEdge proves an aliased edge (r: region) still derives the
// mask from its sub-selection (the edge is identified by field name, not alias).
func TestReadMask_AliasedEdge(t *testing.T) {
	getter := &countingRegionGetter{regions: map[string]*region{"r1": {ID: "r1", Name: "us-east"}}}
	assets := []*asset{{ID: "a1", Name: "asset-1", RegionID: "r1"}}
	schema := buildTestSchema(t, getter, assets)

	res := federationgql.Execute(context.Background(), schema, `{ assets { r: region { name } } }`, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	if got := getter.lastMask; len(got) != 1 || got[0] != "display_name" {
		t.Errorf("aliased-edge region read_mask = %v, want [display_name]", got)
	}
}

// TestMissingResolver_FailsLoud proves a reference to a target type with no
// registered client surfaces as a GraphQL error on the edge, not a silent null.
func TestMissingResolver_FailsLoud(t *testing.T) {
	assets := []*asset{{ID: "a1", Name: "asset-1", RegionID: "r1"}}
	// Resolver registers NO region client.
	resolver := reference.NewStaticResolver()
	schema, err := federationgql.NewSchema(
		[]federationgql.Resource{assetResource(map[string]*region{}, assets), regionResource()},
		resolver,
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	res := federationgql.Execute(context.Background(), schema, `{ assets { region { id } } }`, nil)
	if len(res.Errors) == 0 {
		t.Fatal("expected a GraphQL error for the missing region resolver, got none")
	}
}

// --- helpers ----------------------------------------------------------------

func fieldNames(o *graphql.Object) []string {
	var out []string
	for n := range o.Fields() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func decode(t *testing.T, res *graphql.Result) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return m
}
