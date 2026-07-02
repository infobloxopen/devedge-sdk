package graphqlfederation_test

// e2e_test.go is the F042 end-to-end proof over REAL listeners: it starts the
// region + asset services (each on its own gRPC listener, ent+sqlite,
// fail-closed authz) and the GraphQL gateway, then drives federated queries
// through the gateway's HTTP handler and asserts:
//
//	AC-2  a cross-service query returns each asset with its composed region.
//	AC-3  (keystone) N assets -> M distinct regions costs the region service
//	      EXACTLY ONE BatchGet — a per-row regression fails the calls==1 assert.
//	AC-4  a { region { name } } selection narrows the region fetch's read_mask.
//	AC-5  a request with NO principal is denied per-service (null + error), and a
//	      request with a valid principal succeeds — the gateway never bypasses authz.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/graphql-go/graphql"
	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/examples/graphql-federation/internal/svc"
	"github.com/infobloxopen/devedge-sdk/federationgql"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// regionSpy is the interceptor on the REGION service that proves the anti-N+1
// guarantee end to end (AC-3): it counts BatchGetRegions invocations and, for
// AC-4, captures the read_mask the gateway pushed down as metadata.
type regionSpy struct {
	mu           sync.Mutex
	batchGets    int
	lastBatchIDs []string
	lastReadMask string
}

func (s *regionSpy) interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == federationv1.RegionService_BatchGetRegions_FullMethodName {
		s.mu.Lock()
		s.batchGets++
		if r, ok := req.(*federationv1.BatchGetRegionsRequest); ok {
			s.lastBatchIDs = append([]string(nil), r.GetIds()...)
		}
		s.lastReadMask = svc.ReadMaskFromIncoming(ctx)
		s.mu.Unlock()
	}
	return handler(ctx, req)
}

func (s *regionSpy) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchGets
}

func (s *regionSpy) readMask() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReadMask
}

// harness starts region (with the spy) + asset + gateway over real listeners and
// seeds the demo dataset.
type harness struct {
	spy  *regionSpy
	gw   *svc.Gateway
	stop func()
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	spy := &regionSpy{}
	region, err := svc.StartRegion(ctx, ":0", "e2e_region", spy.interceptor)
	if err != nil {
		cancel()
		t.Fatalf("start region: %v", err)
	}
	asset, err := svc.StartAsset(ctx, ":0", "e2e_asset")
	if err != nil {
		region.Stop()
		cancel()
		t.Fatalf("start asset: %v", err)
	}
	if err := svc.Seed(ctx, region.Addr, asset.Addr); err != nil {
		asset.Stop()
		region.Stop()
		cancel()
		t.Fatalf("seed: %v", err)
	}
	gw, err := svc.NewGateway(region.Addr, asset.Addr)
	if err != nil {
		asset.Stop()
		region.Stop()
		cancel()
		t.Fatalf("build gateway: %v", err)
	}
	return &harness{
		spy: spy,
		gw:  gw,
		stop: func() {
			gw.Close()
			asset.Stop()
			region.Stop()
			cancel()
		},
	}
}

// query executes a GraphQL query through the gateway schema with the given
// account id (empty = no principal). It uses federationgql.Execute so the same
// preload/authz path the HTTP handler uses is exercised.
func (h *harness) query(ctx context.Context, accountID, q string) *graphql.Result {
	ctx = svc.WithAccountID(ctx, accountID)
	return federationgql.Execute(ctx, h.gw.Schema, q, nil)
}

func TestE2E_CrossServiceQuery(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	// AC-2: composed data across the two services.
	res := h.query(context.Background(), "acme", `{ assets { id name region { id name } } }`)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	data := decodeData(t, res)
	assets := data["assets"].([]any)
	if len(assets) != len(svc.DemoAssets) {
		t.Fatalf("want %d assets, got %d", len(svc.DemoAssets), len(assets))
	}
	// Each asset carries its composed region.
	regionsByAsset := map[string]string{}
	for _, a := range assets {
		m := a.(map[string]any)
		reg, ok := m["region"].(map[string]any)
		if !ok || reg == nil {
			t.Fatalf("asset %v has no composed region", m["id"])
		}
		regionsByAsset[m["id"].(string)] = reg["name"].(string)
	}
	if regionsByAsset["a1"] != "us-east" {
		t.Errorf("a1.region.name = %q, want us-east", regionsByAsset["a1"])
	}
	if regionsByAsset["a2"] != "eu-west" {
		t.Errorf("a2.region.name = %q, want eu-west", regionsByAsset["a2"])
	}

	// AC-3 (keystone): 5 assets -> 2 distinct regions -> exactly ONE BatchGet.
	if got := h.spy.calls(); got != 1 {
		t.Fatalf("region service saw %d BatchGet calls, want exactly 1 (anti-N+1 broken over the wire)", got)
	}
	t.Logf("AC-3: region service received exactly %d BatchGet for %d assets / 2 distinct regions", h.spy.calls(), len(assets))
}

func TestE2E_ReadMaskPushdown(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	// AC-4: selecting only region { name } narrows the pushed-down read_mask to
	// display_name (the region service observes the narrowed mask via metadata).
	res := h.query(context.Background(), "acme", `{ assets { region { name } } }`)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	if got := h.spy.readMask(); got != "display_name" {
		t.Fatalf("region read_mask = %q, want display_name", got)
	}
	t.Logf("AC-4: region BatchGet observed read_mask=%q for { region { name } }", h.spy.readMask())

	// Widening the selection widens the mask.
	res = h.query(context.Background(), "acme", `{ assets { region { id name } } }`)
	if len(res.Errors) != 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	got := strings.Split(h.spy.readMask(), ",")
	if !contains(got, "display_name") || !contains(got, "id") {
		t.Errorf("region read_mask = %q, want it to include id and display_name", h.spy.readMask())
	}
}

func TestE2E_AuthzNotBypassed(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	// AC-5a: NO principal — the asset service's fail-closed interceptor denies
	// the root list, so the query returns an error and no composed data.
	res := h.query(context.Background(), "", `{ assets { id region { id name } } }`)
	if len(res.Errors) == 0 {
		t.Fatal("expected a GraphQL error for a request with no principal, got none")
	}
	data := decodeData(t, res)
	if a, ok := data["assets"]; ok && a != nil {
		if rows, ok := a.([]any); ok && len(rows) > 0 {
			t.Fatalf("no-principal request returned composed data (authz bypassed): %v", rows)
		}
	}
	t.Logf("AC-5a: no-principal request denied — errors=%v, data.assets=%v", res.Errors, data["assets"])

	// AC-5b: a valid principal succeeds.
	res = h.query(context.Background(), "acme", `{ assets { id region { id name } } }`)
	if len(res.Errors) != 0 {
		t.Fatalf("valid-principal query errored: %v", res.Errors)
	}
	data = decodeData(t, res)
	if len(data["assets"].([]any)) != len(svc.DemoAssets) {
		t.Fatalf("valid-principal query returned %d assets, want %d", len(data["assets"].([]any)), len(svc.DemoAssets))
	}
	t.Logf("AC-5b: valid principal returned %d composed assets", len(data["assets"].([]any)))
}

// --- helpers ----------------------------------------------------------------

func decodeData(t *testing.T, res *graphql.Result) map[string]any {
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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}
