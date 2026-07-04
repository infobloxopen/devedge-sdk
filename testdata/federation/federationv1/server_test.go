package federationv1_test

// server_test.go is the bonus end-to-end variant of the F041 fixture: it boots a
// real server.New with BOTH services registered (ent-backed), which exercises
// the boot-time fail-loud reference gate (AssertReferenceTargets) — AssetService
// declares a reference to region.example.com/Region and RegionService serves the
// generated BatchGetRegions, so Serve must succeed. It then drives BatchGetRegions
// over a real gRPC client, confirming the guaranteed batch path works over the
// wire and honors read_mask (AIP-157) exactly like Get/List.

import (
	"context"
	"testing"
	"time"

	_ "modernc.org/sqlite" // "sqlite" driver, aliased to "sqlite3" in sqlite_test.go

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/ent/enttest"
	_ "github.com/infobloxopen/devedge-sdk/testdata/federation/ent/runtime" // installs mixin validators + tenant interceptors
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// TestServer_BatchGetRegions_OverWire boots both services on a real server and
// resolves regions through the guaranteed AIP-137 BatchGetRegions RPC.
func TestServer_BatchGetRegions_OverWire(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:federation_srv?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant: "*", Subjects: []string{"*"}, Verbs: []authz.Verb{"*"}, Resource: "*",
	})
	s, err := server.New(server.Config{
		GRPCAddr:   ":0",
		Authorizer: permissive,
		// The verified principal is the tenant authority (SEC-002); in dev the
		// account-id header is promoted to Principal.Tenant at the identity stage.
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// Register both services. AssetService records a reference to Region;
	// RegionService (serving BatchGetRegions) records Region as a batch target —
	// so the boot-time AssertReferenceTargets gate passes at Serve.
	if err := federationv1.RegisterRegionServiceWithRepository(s, federationv1.NewRegionEntBatchRepository(client)); err != nil {
		t.Fatalf("RegisterRegionService: %v", err)
	}
	if err := federationv1.RegisterAssetServiceWithRepository(s, federationv1.NewAssetEntRepository(client)); err != nil {
		t.Fatalf("RegisterAssetService: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serveErr := make(chan error, 1)
	go func() {
		if err := s.Serve(ctx); err != nil {
			serveErr <- err
		}
	}()

	// Wait for the gRPC listener to bind (or a fail-loud gate error from Serve).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			t.Fatalf("Serve failed (reference gate?): %v", err)
		default:
		}
		if addr := s.GRPCAddr(); addr != "" && addr != ":0" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	addr := s.GRPCAddr()
	if addr == "" || addr == ":0" {
		t.Fatal("server did not bind gRPC address within 2s")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reqCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "acme"))
	regionClient := federationv1.NewRegionServiceClient(conn)

	// Seed 2 regions over the wire.
	for _, r := range []*federationv1.Region{
		{Id: "r1", DisplayName: "us-east"},
		{Id: "r2", DisplayName: "eu-west"},
	} {
		if _, err := regionClient.CreateRegion(reqCtx, &federationv1.CreateRegionRequest{Region: r}); err != nil {
			t.Fatalf("CreateRegion %s: %v", r.Id, err)
		}
	}

	// BatchGetRegions: the guaranteed AIP-137 path resolves both in one call.
	resp, err := regionClient.BatchGetRegions(reqCtx, &federationv1.BatchGetRegionsRequest{Ids: []string{"r1", "r2"}})
	if err != nil {
		t.Fatalf("BatchGetRegions: %v", err)
	}
	if len(resp.Regions) != 2 {
		t.Fatalf("BatchGetRegions: want 2 regions, got %d", len(resp.Regions))
	}
	if resp.Regions[0].Id != "r1" || resp.Regions[1].Id != "r2" {
		t.Errorf("BatchGetRegions: wrong order/ids: %q, %q", resp.Regions[0].Id, resp.Regions[1].Id)
	}
	if resp.Regions[0].DisplayName != "us-east" {
		t.Errorf("BatchGetRegions: r1 display_name = %q, want us-east", resp.Regions[0].DisplayName)
	}
}

// TestServer_GetRegion_ReadMask confirms BatchGet's read path shares the same
// read_mask (AIP-157) middleware as Get: a masked GetRegion returns only the
// requested field, cleared elsewhere.
func TestServer_GetRegion_ReadMask(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:federation_srv_mask?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant: "*", Subjects: []string{"*"}, Verbs: []authz.Verb{"*"}, Resource: "*",
	})
	s, err := server.New(server.Config{GRPCAddr: ":0", Authorizer: permissive, PrincipalFunc: grpcauthz.DevPrincipalFunc()})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := federationv1.RegisterRegionServiceWithRepository(s, federationv1.NewRegionEntBatchRepository(client)); err != nil {
		t.Fatalf("RegisterRegionService: %v", err)
	}
	// Register AssetService too so the reference gate is exercised at Serve.
	if err := federationv1.RegisterAssetServiceWithRepository(s, federationv1.NewAssetEntRepository(client)); err != nil {
		t.Fatalf("RegisterAssetService: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serveErr := make(chan error, 1)
	go func() {
		if err := s.Serve(ctx); err != nil {
			serveErr <- err
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			t.Fatalf("Serve failed: %v", err)
		default:
		}
		if addr := s.GRPCAddr(); addr != "" && addr != ":0" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	addr := s.GRPCAddr()
	if addr == "" || addr == ":0" {
		t.Fatal("server did not bind gRPC address within 2s")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reqCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "acme"))
	regionClient := federationv1.NewRegionServiceClient(conn)

	if _, err := regionClient.CreateRegion(reqCtx, &federationv1.CreateRegionRequest{
		Region: &federationv1.Region{Id: "rm-1", DisplayName: "masked"},
	}); err != nil {
		t.Fatalf("CreateRegion: %v", err)
	}

	got, err := regionClient.GetRegion(reqCtx, &federationv1.GetRegionRequest{
		Id:       "rm-1",
		ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	})
	if err != nil {
		t.Fatalf("GetRegion with read_mask: %v", err)
	}
	if got.DisplayName != "masked" {
		t.Errorf("read_mask display_name = %q, want masked", got.DisplayName)
	}
	if got.Id != "" {
		t.Errorf("read_mask should have cleared id, got %q", got.Id)
	}
}
