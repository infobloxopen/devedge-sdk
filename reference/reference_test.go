package reference_test

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// region is a stand-in target resource type for the unit tests.
type region struct {
	id   string
	name string
}

// countingBatchGetter records how many times BatchGet is called so a test can
// assert the anti-N+1 guarantee (exactly one call for N references).
type countingBatchGetter struct {
	byID  map[string]region
	calls int
}

func (g *countingBatchGetter) BatchGet(_ context.Context, ids []string) ([]region, error) {
	g.calls++
	out := make([]region, 0, len(ids))
	for _, id := range ids {
		if r, ok := g.byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestStaticResolver_RoundTrip(t *testing.T) {
	r := reference.NewStaticResolver()
	g := &countingBatchGetter{byID: map[string]region{"r1": {id: "r1"}}}
	r.Register("region.example.com/Region", g)

	got, ok := r.ResolverFor("region.example.com/Region")
	if !ok {
		t.Fatal("ResolverFor: want registered client, got none")
	}
	if got != g {
		t.Fatalf("ResolverFor: wrong client %v", got)
	}
	if _, ok := r.ResolverFor("unknown/Type"); ok {
		t.Fatal("ResolverFor(unknown): want not-ok")
	}
}

// TestLoad_SingleBatchGet is the anti-N+1 proof at the unit level: N parents
// naming M distinct targets resolve in exactly ONE BatchGet of the distinct set.
func TestLoad_SingleBatchGet(t *testing.T) {
	const targetType = "region.example.com/Region"
	g := &countingBatchGetter{byID: map[string]region{
		"r1": {id: "r1", name: "us"},
		"r2": {id: "r2", name: "eu"},
	}}
	res := reference.NewStaticResolver()
	res.Register(targetType, g)

	ref := reference.Reference{FieldName: "RegionId", FKField: "region_id", TargetType: targetType, Cardinality: reference.One}

	type asset struct{ regionID string }
	// 5 parents, only 2 distinct region ids (with an empty one that must be dropped).
	parents := []asset{{"r1"}, {"r2"}, {"r1"}, {"r2"}, {""}}

	got, err := reference.Load[region](
		context.Background(), res, ref, parents,
		func(a asset) []string { return []string{a.regionID} },
		func(r region) string { return r.id },
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.calls != 1 {
		t.Fatalf("Load: want exactly 1 BatchGet call for %d parents, got %d", len(parents), g.calls)
	}
	if len(got) != 2 {
		t.Fatalf("Load: want 2 distinct targets resolved, got %d", len(got))
	}
	if got["r1"].name != "us" || got["r2"].name != "eu" {
		t.Fatalf("Load: wrong targets %#v", got)
	}
}

func TestLoad_MissingResolver(t *testing.T) {
	res := reference.NewStaticResolver() // nothing registered
	ref := reference.Reference{FieldName: "RegionId", FKField: "region_id", TargetType: "region.example.com/Region", Cardinality: reference.One}
	type asset struct{ regionID string }
	_, err := reference.Load[region](
		context.Background(), res, ref, []asset{{"r1"}},
		func(a asset) []string { return []string{a.regionID} },
		func(r region) string { return r.id },
	)
	if err == nil {
		t.Fatal("Load: want fail-loud error for a missing resolver, got nil")
	}
}

func TestLoad_NoRefs(t *testing.T) {
	res := reference.NewStaticResolver()
	ref := reference.Reference{FieldName: "RegionId", FKField: "region_id", TargetType: "region.example.com/Region", Cardinality: reference.One}
	type asset struct{ regionID string }
	// All empty FKs → no BatchGet needed, empty map, no resolver required.
	got, err := reference.Load[region](
		context.Background(), res, ref, []asset{{""}, {""}},
		func(a asset) []string { return []string{a.regionID} },
		func(r region) string { return r.id },
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load: want empty result, got %#v", got)
	}
}
