package apikeyv1_test

// generated_handler_test.go — F029 acceptance (AC-1, AC-6): a single-resource
// service serves full CRUD over gRPC with NO hand-written handler and NO
// hand-listed authz rules. The server is wired by the one-call CRUD path:
//
//	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)
//	apikeyv1.RegisterAPIKeyServiceWithRepository(s, repo)
//	s.Serve(ctx)
//
// The generated apikeyv1.APIKeyServiceCRUDHandler supplies every method by
// delegating to the repository; the tenant is stamped by the repository (not the
// handler), and the authz rules are auto-contributed via server.AddRules with the
// completeness gate at Serve.

import (
	"context"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// startGeneratedHandlerServer boots a real server whose APIKeyService is wired
// ENTIRELY by the generated default handler + RegisterAPIKeyServiceWithRepository
// (no hand-written handler, no Config.Rules), and returns a connected client.
func startGeneratedHandlerServer(t *testing.T) apikeyv1.APIKeyServiceClient {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:gen_handler?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	t.Cleanup(func() { _ = client.Close() })

	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	s, err := server.New(server.Config{
		GRPCAddr:      ":0",
		Authorizer:    authz.NewDevAuthorizer(authz.Grant{Tenant: "*", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*"}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := apikeyv1.RegisterAPIKeyServiceWithRepository(s, repo); err != nil {
		t.Fatalf("RegisterAPIKeyServiceWithRepository: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.GRPCAddr(); a != "" && a != ":0" {
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
		t.Fatalf("dial %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return apikeyv1.NewAPIKeyServiceClient(conn)
}

// TestGeneratedHandler_CRUDRoundTrip runs Create -> Get -> List -> Delete ->
// Get(NotFound) through the generated default handler (AC-1). APIKeyService is a
// Create/Get/List/Delete service (no Update RPC), so the generated handler omits
// Update — exactly the conservative shape-detection F029 promises.
func TestGeneratedHandler_CRUDRoundTrip(t *testing.T) {
	client := startGeneratedHandlerServer(t)
	// account-id satisfies the tenant middleware (and is stamped by the repo on
	// create); groups:admin satisfies the grant via DevPrincipalFunc.
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "tenant1", "groups", "admin"))

	created, err := client.CreateAPIKey(ctx, &apikeyv1.CreateAPIKeyRequest{
		ApiKey: &apikeyv1.APIKey{Id: "k1", Label: "primary", KeyValue: "sk_secret"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if created.Id != "k1" || created.Label != "primary" {
		t.Fatalf("CreateAPIKey: got %+v", created)
	}
	// The repository (not the handler) stamped the tenant from context.
	if created.AccountId != "tenant1" {
		t.Errorf("CreateAPIKey: account_id = %q, want tenant1 (repo-stamped)", created.AccountId)
	}

	got, err := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: "k1"})
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.Label != "primary" {
		t.Errorf("GetAPIKey: label = %q, want primary", got.Label)
	}

	list, err := client.ListAPIKeys(ctx, &apikeyv1.ListAPIKeysRequest{PageSize: 10})
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(list.ApiKeys) != 1 {
		t.Fatalf("ListAPIKeys: want 1, got %d", len(list.ApiKeys))
	}

	if _, err := client.DeleteAPIKey(ctx, &apikeyv1.DeleteAPIKeyRequest{Id: "k1"}); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	_, err = client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: "k1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetAPIKey after delete: want NotFound, got %v (err=%v)", status.Code(err), err)
	}
}

// TestGeneratedHandler_TenantIsolation proves the generated handler + repo scope
// reads to the caller's tenant: tenant2 cannot see tenant1's key (AC-6).
func TestGeneratedHandler_TenantIsolation(t *testing.T) {
	client := startGeneratedHandlerServer(t)
	t1 := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "tenant1", "groups", "admin"))
	t2 := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "tenant2", "groups", "admin"))

	if _, err := client.CreateAPIKey(t1, &apikeyv1.CreateAPIKeyRequest{ApiKey: &apikeyv1.APIKey{Id: "iso-1"}}); err != nil {
		t.Fatalf("CreateAPIKey t1: %v", err)
	}
	if _, err := client.GetAPIKey(t2, &apikeyv1.GetAPIKeyRequest{Id: "iso-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant GetAPIKey: want NotFound, got %v", status.Code(err))
	}
}
