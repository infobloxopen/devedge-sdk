package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/quota"
	"github.com/infobloxopen/devedge-sdk/server"
)

// serveProbe registers probeServiceDesc on s, serves it on a loopback listener,
// and returns a dialed client connection. It uses GRPCServer().Serve directly
// (like TestNew_PrincipalFunc) so the chain server.New built — including the P12
// entitlement wrap and the P13 quota interceptor — is exercised over a real RPC.
func serveProbe(t *testing.T, s *server.Server) *grpc.ClientConn {
	t.Helper()
	s.GRPCServer().RegisterService(&probeServiceDesc, struct{}{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.GRPCServer().Serve(lis) }()
	t.Cleanup(func() { s.GRPCServer().Stop() })

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func invokeAs(conn *grpc.ClientConn, account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("account-id", account, "groups", "admin"))
	return conn.Invoke(ctx, probeMethod, &emptypb.Empty{}, &emptypb.Empty{})
}

// TestServer_EntitlementAndQuotaGate proves the unified gate (P12) and the usage
// meter (P13) compose end-to-end through server.New: rbac ∧ entitlement in one
// decision, then quota metered after authz.
func TestServer_EntitlementAndQuotaGate(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Rules: []authz.MethodRule{{
			Method: probeMethod, Verb: authz.Get, Resource: "thing",
			Features: []string{"sandbox"},
			Quota:    &authz.QuotaRule{Metric: "probes"},
		}},
		// rbac grants any tenant in group:admin; entitlement narrows to t1.
		Authorizer: authz.NewDevAuthorizer(authz.Grant{
			Tenant: "*", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
		}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
		FeatureSource: authz.NewStaticFeatures(map[string][]string{"t1": {"sandbox"}}),
		UsageMeter:    quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{"t1": {"probes": 1}})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	conn := serveProbe(t, s)

	// t2: rbac passes (group:admin, tenant "*") but lacks the "sandbox" feature —
	// the SAME decision denies (entitlement). No quota is consumed (authz first).
	if err := invokeAs(conn, "t2"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing entitlement must deny, got %v", err)
	}
	// t1: rbac ∧ entitlement both pass; first call within quota succeeds.
	if err := invokeAs(conn, "t1"); err != nil {
		t.Fatalf("t1 first call should pass: %v", err)
	}
	// t1: second call exceeds the per-account limit of 1 → ResourceExhausted.
	if err := invokeAs(conn, "t1"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("quota over-limit must be ResourceExhausted, got %v", err)
	}
}

// TestServer_AlertModeAllowsAndEmits proves ModeAlert lets a would-be-denied
// request through while routing an alert to the configured AlertSink.
func TestServer_AlertModeAllowsAndEmits(t *testing.T) {
	alerts := make(chan authz.Alert, 1)
	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Rules: []authz.MethodRule{{
			Method: probeMethod, Verb: authz.Get, Resource: "thing", Mode: authz.ModeAlert,
		}},
		// Grants only t1; t2 would be denied — but alert mode allows + emits.
		Authorizer: authz.NewDevAuthorizer(authz.Grant{
			Tenant: "t1", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
		}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
		AlertSink:     authz.AlertSinkFunc(func(_ context.Context, a authz.Alert) { alerts <- a }),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	conn := serveProbe(t, s)

	if err := invokeAs(conn, "t2"); err != nil {
		t.Fatalf("alert mode must allow the call through, got %v", err)
	}
	select {
	case a := <-alerts:
		if a.Method != probeMethod {
			t.Fatalf("alert carries wrong method: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alert mode must emit an alert")
	}
}
