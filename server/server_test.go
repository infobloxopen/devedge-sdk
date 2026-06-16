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
	"github.com/infobloxopen/devedge-sdk/server"
)

func TestNew_EmptyGRPCAddr_ReturnsError(t *testing.T) {
	_, err := server.New(server.Config{
		GRPCAddr: "",
	})
	if err == nil {
		t.Fatal("expected error when GRPCAddr is empty, got nil")
	}
}

func TestNew_ValidConfig_Succeeds(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":9090",
	})
	if err != nil {
		t.Fatalf("unexpected error with valid config: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Server, got nil")
	}
}

func TestServe_CancelledContext_ReturnsQuickly(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":0", // ephemeral port
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Serve(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned non-nil error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after context cancellation")
	}
}

// TestAssertMethodsDeclared_MissingRule tests that AssertMethodsDeclared from
// grpcauthz errors when a method has no rule. This is the boot-time gate the
// server should call at startup.
func TestAssertMethodsDeclared_MissingRule_ReturnsError(t *testing.T) {
	methods := []string{
		"/widget.v1.WidgetService/GetWidget",
		"/widget.v1.WidgetService/CreateWidget",
	}
	// Only declare one of the two methods — the other is missing.
	opts := []grpcauthz.Option{
		grpcauthz.WithMethodRule("/widget.v1.WidgetService/GetWidget", authz.Get, "widget"),
	}
	err := grpcauthz.AssertMethodsDeclared(methods, opts...)
	if err == nil {
		t.Fatal("expected error when a method has no declared rule, got nil")
	}
}

// TestServer_Rules_ReturnsConfiguredRules verifies that the server exposes the
// rules it was configured with (needed for the boot-time AssertMethodsDeclared call).
func TestServer_Rules_ReturnsConfiguredRules(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc.v1.Svc/GetFoo", Verb: authz.Get, Resource: "foo"},
		{Method: "/svc.v1.Svc/CreateFoo", Verb: authz.Create, Resource: "foo", Public: false},
	}
	s, err := server.New(server.Config{
		GRPCAddr: ":9092",
		Rules:    rules,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := s.Rules()
	if len(got) != len(rules) {
		t.Fatalf("expected %d rules, got %d", len(rules), len(got))
	}
	for i, r := range rules {
		if got[i].Method != r.Method || got[i].Verb != r.Verb || got[i].Resource != r.Resource {
			t.Fatalf("rule[%d] mismatch: want %+v, got %+v", i, r, got[i])
		}
	}
}

const probeMethod = "/test.v1.Svc/Do"

// probeServiceDesc is a one-method gRPC service whose handler is a no-op, used
// to drive a real request through the interceptor chain server.New builds.
var probeServiceDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Svc",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Do",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, req any) (any, error) { return &emptypb.Empty{}, nil }
			if interceptor == nil {
				return h(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: probeMethod}, h)
		},
	}},
}

// TestNew_PrincipalFunc_AuthorizesDocumentedGrant is the regression guard for
// IMPL-002: before the PrincipalFunc seam existed, server.New hard-wired an
// empty principal, so the documented DevAuthorizer grant could never match and
// every non-public call was denied. With Config.PrincipalFunc set to
// grpcauthz.DevPrincipalFunc(), the documented "group:admin in t1" grant must
// authorize a real request — while an unauthenticated caller still fails closed.
func TestNew_PrincipalFunc_AuthorizesDocumentedGrant(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Rules:    []authz.MethodRule{{Method: probeMethod, Verb: authz.Get, Resource: "thing"}},
		Authorizer: authz.NewDevAuthorizer(authz.Grant{
			Tenant: "t1", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
		}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.GRPCServer().RegisterService(&probeServiceDesc, struct{}{})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.GRPCServer().Serve(lis) }()
	defer s.GRPCServer().Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Allowed: caller presents the documented identity (account-id -> tenant,
	// groups -> group:admin).
	okCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	okCtx = metadata.NewOutgoingContext(okCtx, metadata.Pairs("account-id", "t1", "groups", "admin"))
	if err := conn.Invoke(okCtx, probeMethod, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatalf("authorized call denied (IMPL-002 regression): %v", err)
	}

	// Denied: no identity -> empty principal -> default deny (fail closed).
	denyCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := conn.Invoke(denyCtx, probeMethod, &emptypb.Empty{}, &emptypb.Empty{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for unauthenticated caller, got %v", err)
	}
}
