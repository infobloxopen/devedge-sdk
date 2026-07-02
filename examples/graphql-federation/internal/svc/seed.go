package svc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// SeededRegion / SeededAsset are the demo rows the sample seeds.
type SeededRegion struct{ ID, Name string }
type SeededAsset struct{ ID, Name, RegionID string }

// DemoRegions / DemoAssets are the sample dataset: 5 assets sharing 2 regions,
// so the anti-N+1 guarantee (2 distinct region ids -> ONE BatchGet) is visible.
var DemoRegions = []SeededRegion{
	{ID: "r1", Name: "us-east"},
	{ID: "r2", Name: "eu-west"},
}

var DemoAssets = []SeededAsset{
	{ID: "a1", Name: "web-01", RegionID: "r1"},
	{ID: "a2", Name: "web-02", RegionID: "r2"},
	{ID: "a3", Name: "db-01", RegionID: "r1"},
	{ID: "a4", Name: "cache-01", RegionID: "r2"},
	{ID: "a5", Name: "lb-01", RegionID: "r1"},
}

// tenantMD stamps account-id: acme so the seeding writes pass the fail-closed
// authz on each service (the same tenant the DevAuthorizer grants).
func tenantMD(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "account-id", "acme")
}

// Seed populates both services over the wire with the demo dataset.
func Seed(ctx context.Context, regionAddr, assetAddr string) error {
	ctx = tenantMD(ctx)

	regionConn, err := grpc.NewClient(regionAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial region: %w", err)
	}
	defer regionConn.Close()
	assetConn, err := grpc.NewClient(assetAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial asset: %w", err)
	}
	defer assetConn.Close()

	regionClient := federationv1.NewRegionServiceClient(regionConn)
	for _, r := range DemoRegions {
		if _, err := regionClient.CreateRegion(ctx, &federationv1.CreateRegionRequest{
			Region: &federationv1.Region{Id: r.ID, DisplayName: r.Name},
		}); err != nil {
			return fmt.Errorf("create region %s: %w", r.ID, err)
		}
	}

	assetClient := federationv1.NewAssetServiceClient(assetConn)
	for _, a := range DemoAssets {
		if _, err := assetClient.CreateAsset(ctx, &federationv1.CreateAssetRequest{
			Asset: &federationv1.Asset{Id: a.ID, DisplayName: a.Name, RegionId: a.RegionID},
		}); err != nil {
			return fmt.Errorf("create asset %s: %w", a.ID, err)
		}
	}
	return nil
}
