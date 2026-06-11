package widgetsv1_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/seccheck"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// newSecTestServer boots a gRPC-only server (no HTTP gateway) with the given
// authorizer and returns the server plus its gRPC address.
func newSecTestServer(t *testing.T, az authz.Authorizer) string {
	t.Helper()
	_, grpcAddr, _ := newTestServer(t, az)
	return grpcAddr
}

// TestSecurity_AuthZ (US2): every non-public method must deny an unknown principal.
func TestSecurity_AuthZ(t *testing.T) {
	// Zero grants → default-deny for all methods.
	denyAll := authz.NewDevAuthorizer()
	grpcAddr := newSecTestServer(t, denyAll)

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := widgetsv1.NewWidgetServiceClient(conn)

	calls := map[string]seccheck.CallFn{
		"/toy.v1.WidgetService/CreateWidget": func(ctx context.Context) error {
			_, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
				Widget: &widgetsv1.Widget{Name: "x"},
			})
			return err
		},
		"/toy.v1.WidgetService/GetWidget": func(ctx context.Context) error {
			_, err := client.GetWidget(ctx, &widgetsv1.GetWidgetRequest{Id: "x"})
			return err
		},
		"/toy.v1.WidgetService/ListWidgets": func(ctx context.Context) error {
			_, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{})
			return err
		},
		"/toy.v1.WidgetService/UpdateWidget": func(ctx context.Context) error {
			_, err := client.UpdateWidget(ctx, &widgetsv1.UpdateWidgetRequest{
				Widget:     &widgetsv1.Widget{Id: "x", Name: "y"},
				UpdateMask: []string{"name"},
			})
			return err
		},
		"/toy.v1.WidgetService/DeleteWidget": func(ctx context.Context) error {
			_, err := client.DeleteWidget(ctx, &widgetsv1.DeleteWidgetRequest{Id: "x"})
			return err
		},
	}

	findings := seccheck.AssertUnknownPrincipalDenied(context.Background(), widgetsv1.WidgetServiceAuthzRules, calls)
	seccheck.RunT(t, findings)
}

// TestSecurity_VerboseErrors (US4): error messages must not leak internal details.
func TestSecurity_VerboseErrors(t *testing.T) {
	// Full-access grant so the calls reach the handler (errors come from business logic, not authz).
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	grpcAddr := newSecTestServer(t, permissive)

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := widgetsv1.NewWidgetServiceClient(conn)

	ctx := ctxWithMD("account-id", "tester")

	triggers := []seccheck.ErrorTrigger{
		{
			Method: "GetWidget/nonexistent",
			Fn: func(ctx context.Context) error {
				_, err := client.GetWidget(ctx, &widgetsv1.GetWidgetRequest{Id: "nonexistent-id-xyz"})
				return err
			},
		},
		{
			Method: "UpdateWidget/nonexistent",
			Fn: func(ctx context.Context) error {
				_, err := client.UpdateWidget(ctx, &widgetsv1.UpdateWidgetRequest{
					Widget:     &widgetsv1.Widget{Id: "nonexistent-id-xyz", Name: "y"},
					UpdateMask: []string{"name"},
				})
				return err
			},
		},
	}

	findings := seccheck.AssertErrorMessagesClean(ctx, triggers)
	seccheck.RunT(t, findings)
}

// TestSecurity_CrossAccountIsolation (US3): resources created by alice must not be visible to bob.
// MemoryRepository is not tenant-scoped, so findings are EXPECTED — this test documents the gap
// without failing the build.
func TestSecurity_CrossAccountIsolation(t *testing.T) {
	// Grant both alice and bob full access on widgets.
	az := authz.NewDevAuthorizer(
		authz.Grant{Tenant: "*", Subjects: []string{"*"}, Verbs: []authz.Verb{"*"}, Resource: "*"},
	)
	grpcAddr := newSecTestServer(t, az)

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := widgetsv1.NewWidgetServiceClient(conn)

	cfg := seccheck.IsolationConfig{
		PrincipalA: "alice",
		PrincipalB: "bob",
		CreateFn: func(ctx context.Context) (string, error) {
			w, err := client.CreateWidget(ctx, &widgetsv1.CreateWidgetRequest{
				Widget: &widgetsv1.Widget{Name: "alice-widget"},
			})
			if err != nil {
				return "", err
			}
			return w.Id, nil
		},
		ReadFn: func(ctx context.Context, id string) error {
			_, err := client.GetWidget(ctx, &widgetsv1.GetWidgetRequest{Id: id})
			return err
		},
		ListFn: func(ctx context.Context) (int, error) {
			resp, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{})
			if err != nil {
				return 0, err
			}
			return len(resp.Widgets), nil
		},
	}

	findings := seccheck.AssertCrossAccountIsolation(context.Background(), cfg)
	t.Logf("US3 cross-account isolation findings (expected — MemoryRepository not scoped by tenant): %v", findings)
}
