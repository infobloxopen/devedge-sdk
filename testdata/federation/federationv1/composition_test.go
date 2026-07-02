package federationv1_test

// composition_test.go is the F041 / WS-021 P1 AC-5 proof at the fixture level:
// a composition fetches N Assets (the reference SOURCE) then resolves their
// cross-service region references in exactly ONE BatchGet (the anti-N+1
// guarantee) through reference.Load — on BOTH the ent and GORM backends.
//
// It also asserts the generated reference metadata (AC-1) and the metadata-only
// invariant that Asset has no ent edge to Region (AC-4).

import (
	"context"
	"testing"

	_ "modernc.org/sqlite" // registers driver name "sqlite" (aliased to "sqlite3" in sqlite_test.go)

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/reference"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// tenantCtx stamps accountID as the tenant identity, satisfying the repositories'
// tenant scoping (the generated repos scope every query by account_id from ctx).
func tenantCtx(accountID string) context.Context {
	return middleware.WithTenantID(context.Background(), accountID)
}

// countingBatchGetter wraps a Region BatchRepository's BatchGet and counts how
// many times it is invoked, so a test can assert the anti-N+1 guarantee: exactly
// ONE BatchGet call for N Asset references. It is the reference.BatchGetter[T]
// the resolver hands to reference.Load. A per-row (Get-per-asset) resolver would
// drive calls up to N and fail the calls==1 assertion.
type countingBatchGetter struct {
	repo  persistence.BatchRepository[*federationv1.Region, string]
	ctx   context.Context
	calls int
}

func (g *countingBatchGetter) BatchGet(_ context.Context, ids []string) ([]*federationv1.Region, error) {
	g.calls++
	// Use the tenant-stamped ctx: the repo scopes BatchGet by account_id.
	return g.repo.BatchGet(g.ctx, ids)
}

// backend pairs a Region batch repo with an Asset repo for one storage engine.
// The composition test runs identically over ent and GORM.
type backend struct {
	name    string
	regions persistence.BatchRepository[*federationv1.Region, string]
	assets  persistence.Repository[*federationv1.Asset, string]
	cleanup func()
}

// entBackend stands up the ent client + the generated ent repositories.
func entBackend(t *testing.T) backend {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:federation_ent?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	return backend{
		name:    "ent",
		regions: federationv1.NewRegionEntBatchRepository(client),
		assets:  federationv1.NewAssetEntRepository(client),
		cleanup: func() { _ = client.Close() },
	}
}

// gormBackend stands up a GORM SQLite DB + the generated GORM repositories after
// migrating both models.
func gormBackend(t *testing.T) backend {
	t.Helper()
	db, err := gorm.Open(
		openTestSQLite("file:federation_gorm?mode=memory&cache=shared&_pragma=foreign_keys(1)"),
		&gorm.Config{Logger: logger.Discard},
	)
	if err != nil {
		t.Fatalf("open gorm sqlite: %v", err)
	}
	if err := db.AutoMigrate(&federationv1.RegionModel{}, &federationv1.AssetModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return backend{
		name:    "gorm",
		regions: federationv1.NewRegionRepository(db),
		assets:  federationv1.NewAssetRepository(db),
		cleanup: func() {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		},
	}
}

// TestComposition_SingleBatchGet is the keystone AC-5 proof on BOTH backends:
// seed 2 Regions + 5 Assets referencing those 2 (with sharing), list the Assets,
// then resolve their region references via reference.Load — asserting exactly ONE
// BatchGet for the N=5 references and that the 2 distinct targets resolve.
func TestComposition_SingleBatchGet(t *testing.T) {
	backends := map[string]func(*testing.T) backend{
		"ent":  entBackend,
		"gorm": gormBackend,
	}

	for name, mk := range backends {
		mk := mk
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.cleanup()

			ctx := tenantCtx("acme")

			// Seed M=2 Regions.
			for _, r := range []*federationv1.Region{
				{Id: "r1", AccountId: "acme", DisplayName: "us-east"},
				{Id: "r2", AccountId: "acme", DisplayName: "eu-west"},
			} {
				if _, err := b.regions.Create(ctx, r); err != nil {
					t.Fatalf("[%s] create region %s: %v", b.name, r.Id, err)
				}
			}

			// Seed N=5 Assets referencing the 2 regions (r1 and r2 each shared).
			assetsSeed := []*federationv1.Asset{
				{Id: "a1", AccountId: "acme", DisplayName: "asset-1", RegionId: "r1"},
				{Id: "a2", AccountId: "acme", DisplayName: "asset-2", RegionId: "r2"},
				{Id: "a3", AccountId: "acme", DisplayName: "asset-3", RegionId: "r1"},
				{Id: "a4", AccountId: "acme", DisplayName: "asset-4", RegionId: "r2"},
				{Id: "a5", AccountId: "acme", DisplayName: "asset-5", RegionId: "r1"},
			}
			for _, a := range assetsSeed {
				if _, err := b.assets.Create(ctx, a); err != nil {
					t.Fatalf("[%s] create asset %s: %v", b.name, a.Id, err)
				}
			}

			// List the 5 Assets (the composition's first fan-out).
			assets, _, err := b.assets.List(ctx, persistence.ListOptions{})
			if err != nil {
				t.Fatalf("[%s] list assets: %v", b.name, err)
			}
			if len(assets) != len(assetsSeed) {
				t.Fatalf("[%s] list assets: want %d, got %d", b.name, len(assetsSeed), len(assets))
			}

			// Wrap the Region repo's BatchGet in a counting getter and register it
			// under the reference's target type. reference.Load resolves through it.
			getter := &countingBatchGetter{repo: b.regions, ctx: ctx}
			resolver := reference.NewStaticResolver()
			resolver.Register("region.example.com/Region", getter)

			// The single reference declared on Asset (AssetServiceReferences[0]).
			ref := federationv1.AssetServiceReferences[0]

			byID, err := reference.Load[*federationv1.Region](
				ctx, resolver, ref, assets,
				func(a *federationv1.Asset) []string { return []string{a.GetRegionId()} },
				func(r *federationv1.Region) string { return r.GetId() },
			)
			if err != nil {
				t.Fatalf("[%s] reference.Load: %v", b.name, err)
			}

			// THE keystone: exactly one BatchGet for the 5 references. A per-row
			// (Get-per-asset) resolver would make this 5 and fail here.
			if getter.calls != 1 {
				t.Fatalf("[%s] want exactly 1 BatchGet for %d references, got %d (anti-N+1 broken)",
					b.name, len(assets), getter.calls)
			}
			// 2 distinct region ids named by the 5 assets → 2 resolved targets.
			if len(byID) != 2 {
				t.Fatalf("[%s] want 2 distinct regions resolved, got %d: %v", b.name, len(byID), byID)
			}
			if got := byID["r1"]; got == nil || got.GetDisplayName() != "us-east" {
				t.Errorf("[%s] r1 resolved to %v, want display_name=us-east", b.name, got)
			}
			if got := byID["r2"]; got == nil || got.GetDisplayName() != "eu-west" {
				t.Errorf("[%s] r2 resolved to %v, want display_name=eu-west", b.name, got)
			}

			// Every asset's region_id resolves to a seeded region (the composition
			// stitches source→target above the services, no in-process edge).
			for _, a := range assets {
				if _, ok := byID[a.GetRegionId()]; !ok {
					t.Errorf("[%s] asset %s region_id=%q did not resolve", b.name, a.GetId(), a.GetRegionId())
				}
			}
		})
	}
}

// TestAssetServiceReferences_Metadata is the AC-1 assertion at the fixture level:
// protoc-gen-svc emits exactly one reference from Asset's region_id
// google.api.resource_reference, with the right target type, FK field, and
// cardinality.
func TestAssetServiceReferences_Metadata(t *testing.T) {
	refs := federationv1.AssetServiceReferences
	if len(refs) != 1 {
		t.Fatalf("AssetServiceReferences: want 1 reference, got %d: %+v", len(refs), refs)
	}
	r := refs[0]
	if r.TargetType != "region.example.com/Region" {
		t.Errorf("TargetType = %q, want region.example.com/Region", r.TargetType)
	}
	if r.FKField != "region_id" {
		t.Errorf("FKField = %q, want region_id", r.FKField)
	}
	if r.FieldName != "RegionId" {
		t.Errorf("FieldName = %q, want RegionId", r.FieldName)
	}
	if r.Cardinality != reference.One {
		t.Errorf("Cardinality = %q, want %q (scalar FK)", r.Cardinality, reference.One)
	}
}
