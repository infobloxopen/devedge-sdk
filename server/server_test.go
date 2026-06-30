package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	sdkhealth "github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/persistence"
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
// Note: New automatically adds public rules for the gRPC health service methods
// (Check, List, Watch), so Rules() returns those plus the configured rules.
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
	// Rules() includes the 3 automatically-added health service public rules
	// (grpc.health.v1.Health/{Check,List,Watch}), so the total is len(rules)+3.
	wantTotal := len(rules) + 3
	if len(got) != wantTotal {
		t.Fatalf("expected %d rules (configured + 3 health), got %d", wantTotal, len(got))
	}
	// Verify the configured rules are present by method name.
	byMethod := make(map[string]authz.MethodRule, len(got))
	for _, r := range got {
		byMethod[r.Method] = r
	}
	for _, r := range rules {
		gr, ok := byMethod[r.Method]
		if !ok {
			t.Fatalf("configured rule %s missing from Rules()", r.Method)
		}
		if gr.Verb != r.Verb || gr.Resource != r.Resource {
			t.Fatalf("rule %s mismatch: want %+v, got %+v", r.Method, r, gr)
		}
	}
}

// TestAddRules_AccumulatesIntoRules verifies AddRules appends to the server's
// rule set on top of Config.Rules (F029 D-3): Config.Rules seeds it, AddRules
// (called by the generated Register<Svc>) contributes the rest.
// Note: New also seeds 3 health service public rules, so the total includes those.
func TestAddRules_AccumulatesIntoRules(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Rules:    []authz.MethodRule{{Method: "/svc.v1.Svc/A", Verb: authz.Get, Resource: "a"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.AddRules(
		authz.MethodRule{Method: "/svc.v1.Svc/B", Verb: authz.Create, Resource: "b"},
		authz.MethodRule{Method: "/svc.v1.Svc/C", Public: true},
	)
	got := s.Rules()
	// 1 seed + 2 added + 3 health = 6
	wantTotal := 6
	if len(got) != wantTotal {
		t.Fatalf("Rules() = %d, want %d (1 seed + 2 added + 3 health)", len(got), wantTotal)
	}
	byMethod := map[string]authz.MethodRule{}
	for _, r := range got {
		byMethod[r.Method] = r
	}
	for _, m := range []string{"/svc.v1.Svc/A", "/svc.v1.Svc/B", "/svc.v1.Svc/C"} {
		if _, ok := byMethod[m]; !ok {
			t.Errorf("Rules() missing %s", m)
		}
	}
}

// TestServe_UndeclaredMethod_FailsClosed verifies the boot-time completeness gate
// now runs at Serve over the accumulated rule set + recorded methods (F029 D-3 /
// AC-4): a registered RPC with neither a rule nor a public exemption makes Serve
// fail closed.
func TestServe_UndeclaredMethod_FailsClosed(t *testing.T) {
	s, err := server.New(server.Config{GRPCAddr: ":0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Record a method but contribute NO rule for it.
	s.RecordMethods("/svc.v1.Svc/Orphan")
	s.AddRules(authz.MethodRule{Method: "/svc.v1.Svc/Other", Verb: authz.Get, Resource: "x"})

	err = s.Serve(context.Background())
	if err == nil {
		t.Fatal("Serve: want fail-closed error for undeclared method, got nil")
	}
	if !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("Serve error = %q, want it to mention 'undeclared'", err.Error())
	}
}

// TestServe_AllMethodsDeclared_PassesGate verifies the gate passes (and Serve
// proceeds to listen) when every recorded method has a rule. The cancelled
// context returns Serve cleanly, proving the gate did not block a valid config.
func TestServe_AllMethodsDeclared_PassesGate(t *testing.T) {
	s, err := server.New(server.Config{GRPCAddr: ":0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.RecordMethods("/svc.v1.Svc/Get", "/svc.v1.Svc/Health")
	s.AddRules(
		authz.MethodRule{Method: "/svc.v1.Svc/Get", Verb: authz.Get, Resource: "x"},
		authz.MethodRule{Method: "/svc.v1.Svc/Health", Public: true},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve with all methods declared returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after cancel")
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

// notFoundMethod is a public method whose handler returns a raw persistence
// sentinel, used by the BC-04 access-log regression test.
const notFoundMethod = "/test.v1.Svc/NotFound"

// notFoundServiceDesc is a one-method gRPC service whose handler returns
// persistence.ErrNotFound — a raw sentinel that ErrorMapperUnary maps to
// codes.NotFound. It drives the BC-04 regression: the access log must record
// the mapped client-visible code, not the raw sentinel (which status.Code()
// reports as Unknown).
var notFoundServiceDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Svc",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "NotFound",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, req any) (any, error) { return nil, persistence.ErrNotFound }
			if interceptor == nil {
				return h(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: notFoundMethod}, h)
		},
	}},
}

// TestAccessLog_RecordsMappedCode_NotRawSentinel is the BC-04 regression guard
// (devedge-sdk#134). When a handler returns a raw persistence sentinel, the
// ErrorMapper interceptor remaps it to the canonical gRPC code the client sees
// (NotFound). Before the fix, ErrorMapper was OUTER to LoggingUnary, so on the
// error-return path the logging interceptor read status.Code(raw-sentinel) =
// Unknown before the remap — the access log disagreed with the client and with
// the RED metrics. With ErrorMapper now inner to Logging, the logged grpc.code
// must equal the client-observed code.
func TestAccessLog_RecordsMappedCode_NotRawSentinel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Logger:   logger,
		// Public so the request reaches the handler (we are testing the handler
		// error path, not authz).
		Rules: []authz.MethodRule{{Method: notFoundMethod, Public: true}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.GRPCServer().RegisterService(&notFoundServiceDesc, struct{}{})

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invokeErr := conn.Invoke(ctx, notFoundMethod, &emptypb.Empty{}, &emptypb.Empty{})

	// The client must see the mapped code (proves ErrorMapper still maps).
	if got := status.Code(invokeErr); got != codes.NotFound {
		t.Fatalf("client code = %v, want NotFound (ErrorMapper must still map the sentinel)", got)
	}

	// The access log must record the SAME code the client saw, not Unknown.
	var summary map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal slog record %q: %v", line, err)
		}
		if m["msg"] == "grpc request handled" {
			summary = m
		}
	}
	if summary == nil {
		t.Fatalf("no access-log summary record found in:\n%s", buf.String())
	}
	if got := summary["grpc.code"]; got != codes.NotFound.String() {
		t.Errorf("access-log grpc.code = %v, want %q (BC-04: must equal the client-visible code, not the raw-sentinel Unknown)", got, codes.NotFound.String())
	}
}

// -- health tests (T3) -------------------------------------------------------

// toggleCheck is a readiness check whose readiness can be toggled in tests. The
// fail flag is an atomic because the test goroutine flips it while the server's
// readiness-loop goroutine reads it via Check (otherwise a data race under -race).
type toggleCheck struct {
	name string
	fail atomic.Bool
}

func (c *toggleCheck) Name() string { return c.name }
func (c *toggleCheck) Check(_ context.Context) error {
	if c.fail.Load() {
		return errors.New("not ready")
	}
	return nil
}

// startHealthServer spins up a server with an HTTP gateway and returns the gRPC
// address, HTTP address, a gRPC client connection, and a cancel func.
func startHealthServer(t *testing.T, checks []sdkhealth.Check) (grpcAddr, httpAddr string, conn *grpc.ClientConn, cancel func()) {
	t.Helper()
	s, err := server.New(server.Config{
		GRPCAddr:        ":0",
		HTTPAddr:        ":0",
		ReadinessChecks: checks,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, ctxCancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()

	// Poll until the server has bound both listeners (GRPCAddr/HTTPAddr change
	// from the configured ":0" to the actual bound address once Serve starts).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ga := s.GRPCAddr()
		ha := s.HTTPAddr()
		if ga != ":0" && ha != ":0" && ga != "" && ha != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.GRPCAddr() == ":0" || s.HTTPAddr() == ":0" {
		ctxCancel()
		t.Fatal("server did not start within 5s")
	}

	conn, err = grpc.NewClient(s.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		ctxCancel()
		t.Fatalf("dial: %v", err)
	}
	return s.GRPCAddr(), s.HTTPAddr(), conn, func() {
		_ = conn.Close()
		ctxCancel()
	}
}

// TestHealth_GRPCHealthCheck_ReturnsSERVING verifies AC-1: the gRPC health
// service returns SERVING for a server with no readiness checks.
func TestHealth_GRPCHealthCheck_ReturnsSERVING(t *testing.T) {
	_, _, conn, cancel := startHealthServer(t, nil)
	defer cancel()

	hc := grpc_health_v1.NewHealthClient(conn)
	ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	resp, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("Health/Check: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.Status)
	}
}

// TestHealth_Healthz_Returns200 verifies AC-2: GET /healthz → 200 when the
// process is up.
func TestHealth_Healthz_Returns200(t *testing.T) {
	_, httpAddr, _, cancel := startHealthServer(t, nil)
	defer cancel()

	resp, err := http.Get("http://" + httpAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHTTPMiddleware_WrapsGatewayNotProbes verifies the P2 HTTP middleware seam:
// a configured Config.HTTPMiddleware wraps the REST gateway routes (so it runs
// even for an unmatched gateway path) but NOT the /healthz and /readyz probes,
// which must stay off the extension path.
func TestHTTPMiddleware_WrapsGatewayNotProbes(t *testing.T) {
	mark := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Audit-MW", "1")
			next.ServeHTTP(w, r)
		})
	}
	s, err := server.New(server.Config{
		GRPCAddr:       ":0",
		HTTPAddr:       ":0",
		HTTPMiddleware: []func(http.Handler) http.Handler{mark},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (s.HTTPAddr() == ":0" || s.HTTPAddr() == "") {
		time.Sleep(20 * time.Millisecond)
	}
	if s.HTTPAddr() == ":0" || s.HTTPAddr() == "" {
		t.Fatal("server did not start within 5s")
	}
	base := "http://" + s.HTTPAddr()

	// A gateway route (unmatched → 404) is still wrapped by the middleware.
	gwResp, err := http.Get(base + "/v1/anything")
	if err != nil {
		t.Fatalf("GET gateway path: %v", err)
	}
	gwResp.Body.Close()
	if gwResp.Header.Get("X-Audit-MW") != "1" {
		t.Errorf("HTTP middleware must wrap gateway routes (missing X-Audit-MW header), status=%d", gwResp.StatusCode)
	}

	// The liveness probe must NOT be wrapped by the middleware.
	hzResp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	hzResp.Body.Close()
	if hzResp.Header.Get("X-Audit-MW") != "" {
		t.Errorf("HTTP middleware must NOT wrap the /healthz probe, but X-Audit-MW was set")
	}
}

// TestHealth_Readyz_AllPassReturns200_FailureReturns503 verifies AC-2 and AC-3:
// /readyz flips between 200 and 503 as the readiness check toggles.
func TestHealth_Readyz_AllPassReturns200_FailureReturns503(t *testing.T) {
	check := &toggleCheck{name: "dep"}
	_, httpAddr, _, cancel := startHealthServer(t, []sdkhealth.Check{check})
	defer cancel()

	// Initially ready (check.fail == false).
	resp, err := http.Get("http://" + httpAddr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 when ready, got %d", resp.StatusCode)
	}

	// Toggle to failing.
	check.fail.Store(true)
	resp, err = http.Get("http://" + httpAddr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz (failing): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("want 503 when unready, got %d (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "unready") {
		t.Errorf("body missing 'unready': %s", body)
	}

	// Toggle back to ready.
	check.fail.Store(false)
	resp, err = http.Get("http://" + httpAddr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz (recovered): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 on recovery, got %d", resp.StatusCode)
	}
}

// TestHealth_GRPCStatus_FlipsWithReadiness verifies AC-3: the gRPC overall
// health status flips to NOT_SERVING when any readiness check fails and back
// to SERVING on recovery. Because the background loop polls every 5s we drive
// syncHealthStatus indirectly by waiting for the first sync (startup) then
// toggling and waiting one more poll cycle.
func TestHealth_GRPCStatus_FlipsWithReadiness(t *testing.T) {
	check := &toggleCheck{name: "dep"}
	_, _, conn, cancel := startHealthServer(t, []sdkhealth.Check{check})
	defer cancel()

	hc := grpc_health_v1.NewHealthClient(conn)
	waitStatus := func(want grpc_health_v1.HealthCheckResponse_ServingStatus, deadline time.Duration) {
		t.Helper()
		ctx, c := context.WithTimeout(context.Background(), deadline)
		defer c()
		for {
			resp, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
			if err != nil {
				if ctx.Err() != nil {
					t.Fatalf("timed out waiting for gRPC health status %v", want)
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if resp.Status == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Initially SERVING (startup sync).
	waitStatus(grpc_health_v1.HealthCheckResponse_SERVING, 3*time.Second)

	// Toggle check to failing; the background loop should flip to NOT_SERVING
	// within ~5s (one poll tick) + a little slack.
	check.fail.Store(true)
	waitStatus(grpc_health_v1.HealthCheckResponse_NOT_SERVING, 8*time.Second)

	// Recover: flip back to SERVING.
	check.fail.Store(false)
	waitStatus(grpc_health_v1.HealthCheckResponse_SERVING, 8*time.Second)
}

// TestHealth_Readyz_Unauthenticated verifies AC-4: the /readyz probe is
// reachable without any auth headers (the authz interceptor only applies to
// gRPC requests routed through the gateway, not to the outer-mux probe routes).
func TestHealth_Readyz_Unauthenticated(t *testing.T) {
	_, httpAddr, _, cancel := startHealthServer(t, nil)
	defer cancel()

	// Plain request with no auth headers — must not get 401/403.
	req, err := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/readyz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz (no auth): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Errorf("probe behind authz: got %d, want 200", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
