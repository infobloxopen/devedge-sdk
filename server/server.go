// Package server provides a batteries-included gRPC server builder for
// Infoblox services. It assembles the framework interceptor chain (request-ID,
// error mapping, tenant-ID, fail-closed authz, field-mask validation, ETag
// preconditions, read-mask response shaping) and, optionally, an HTTP/JSON
// gateway in front of the gRPC endpoint.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	sdkhealth "github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/lro"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
	"github.com/infobloxopen/devedge-sdk/resilience"
)

// DefaultGRPCAddr is the default listen address for the gRPC endpoint.
const DefaultGRPCAddr = ":9090"

// shutdownTimeout bounds graceful shutdown of both the gRPC and HTTP servers.
const shutdownTimeout = 5 * time.Second

// Config carries the options for constructing a Server.
type Config struct {
	// GRPCAddr is the TCP address to listen on (e.g. ":9090" or ":0"). Required.
	GRPCAddr string
	// HTTPAddr is the optional gateway address (e.g. ":8080"). Empty disables
	// the HTTP gateway.
	HTTPAddr string
	// Rules are the declared authz rules; they feed both grpcauthz (enforcement)
	// and the field-mask interceptor (verb lookup).
	Rules []authz.MethodRule
	// Authorizer is the pluggable decision point. Defaults to
	// authz.NewDevAuthorizer(nil) if nil.
	Authorizer authz.Authorizer
	// PrincipalFunc derives the authenticated authz.Principal from each request's
	// context. When nil the principal is empty, so — with a default-deny
	// Authorizer — every non-public method is denied (fail closed). For local
	// development set grpcauthz.DevPrincipalFunc() to derive the principal from
	// request metadata; in production supply one backed by a verified token.
	PrincipalFunc grpcauthz.PrincipalFunc
	// Interceptors are additional unary interceptors appended after the
	// framework chain.
	Interceptors []grpc.UnaryServerInterceptor
	// DeduplicationStore is the idempotency store for DeduplicateUnary. Defaults to MemoryDeduplicationStore (10-minute TTL) when nil.
	DeduplicationStore middleware.DeduplicationStore
	// LROStore is the operation store for long-running operations (AIP-151).
	// Defaults to lro.NewMemoryStore(1h) when nil.
	LROStore lro.Store
	// Logger is the structured logger the default chain's middleware.LoggingUnary
	// writes one record per RPC to (trace-correlated, secret-redacted payloads at
	// Debug). Defaults to slog.Default() when nil.
	Logger *slog.Logger
	// ReadinessChecks is the list of readiness checks the server runs on every
	// /readyz probe and whenever it drives the gRPC health status. An empty slice
	// means "always ready" (the default). Each check is bounded by a 2s timeout;
	// a single failure flips /readyz to 503 and the gRPC overall status to
	// NOT_SERVING. Liveness (/healthz, gRPC health Check on "") is always
	// process-up only — deps never go in the liveness check.
	ReadinessChecks []sdkhealth.Check
	// Resilience configures optional resilience policy interceptors inserted into
	// the default chain. server.New applies a 30-second request timeout when the
	// zero value is supplied; rate limiting and circuit breaking are opt-in (nil).
	Resilience ResilienceConfig
}

// ResilienceConfig holds the resilience policy settings for the server's
// default interceptor chain.
type ResilienceConfig struct {
	// RequestTimeout bounds every unary handler invocation. server.New defaults
	// to 30s when this field is zero; set to resilience.NoTimeout to explicitly
	// disable the timeout. Per-method overrides (PerMethodTimeout) take
	// precedence; a per-method value of resilience.NoTimeout disables that
	// method's timeout regardless of RequestTimeout.
	//
	// A handler that exceeds the deadline receives codes.DeadlineExceeded.
	// Handlers should honour ctx.Done() for clean early exit.
	RequestTimeout time.Duration
	// PerMethodTimeout overrides RequestTimeout for specific gRPC full-method
	// names (e.g. "/mypackage.MyService/LongOp": 5*time.Minute). Set a method's
	// value to resilience.NoTimeout to disable the timeout for that method only.
	PerMethodTimeout map[string]time.Duration
	// RateLimiter, when non-nil, is inserted right after TenantIDUnary (before
	// authz) to shed excess load early with codes.ResourceExhausted. Default
	// nil = off. Use resilience.NewTokenBucket or supply your own implementation.
	RateLimiter resilience.RateLimiter
	// CircuitBreaker, when non-nil, wraps handler invocations just inside the
	// framework chain. Default nil = off. Plug in sony/gobreaker,
	// afex/hystrix-go, or any resilience.CircuitBreaker implementation.
	CircuitBreaker resilience.CircuitBreaker
}

// Server is the assembled gRPC server (plus optional HTTP gateway).
type Server struct {
	cfg        Config
	grpcSrv    *grpc.Server
	healthSrv  *health.Server    // gRPC health service; always registered
	gwMux      *runtime.ServeMux // nil when HTTPAddr == ""
	gatewayFns []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error

	// lisMu guards grpcLis/httpLis: Serve writes them from its own goroutine while
	// the GRPCAddr()/HTTPAddr() accessors (documented for use "once Serve has
	// started", e.g. to read a kernel-assigned ":0" port) read them concurrently
	// from another goroutine. Without the mutex that is a data race.
	lisMu   sync.Mutex
	grpcLis net.Listener // set by Serve
	httpLis net.Listener // set by Serve when HTTPAddr != ""

	// rules is the accumulated authz rule set: Config.Rules seeds it and each
	// AddRules call (from a generated Register<Svc>) appends to it. The authz and
	// field-mask interceptors read this live set (via a rule-source closure), and
	// the boot-time completeness gate runs over it at Serve — so a service's rules
	// are enforced even though they are contributed after New.
	rules []authz.MethodRule
	// methods is every gRPC FullMethod registered with the server (recorded by the
	// generated Register<Svc>). The completeness gate at Serve fails closed if any
	// of these lacks a rule or a public exemption.
	methods []string
	// memberBindings is the accumulated set of DDD aggregate member→root bindings
	// (recorded by the generated Register<Svc> of a member service). The boundary
	// gate at Serve fails closed if a member resource registers a write method.
	memberBindings []MemberBinding
}

// New validates cfg and constructs a Server. It builds the framework
// interceptor chain and wires the authz rules into both the authorizer and the
// field-mask validator. Returns an error if any required field is missing.
func New(cfg Config) (*Server, error) {
	if cfg.GRPCAddr == "" {
		return nil, fmt.Errorf("server: GRPCAddr is required")
	}
	if cfg.Authorizer == nil {
		// Default to a default-deny dev authorizer (no grants).
		cfg.Authorizer = authz.NewDevAuthorizer()
	}
	if cfg.DeduplicationStore == nil {
		cfg.DeduplicationStore = middleware.NewMemoryDeduplicationStore(10 * time.Minute)
	}
	if cfg.LROStore == nil {
		cfg.LROStore = lro.NewMemoryStore(time.Hour)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Apply the 30s request-timeout default when the field is zero-valued (not
	// explicitly set). Use resilience.NoTimeout in ResilienceConfig to opt out.
	if cfg.Resilience.RequestTimeout == 0 {
		cfg.Resilience.RequestTimeout = 30 * time.Second
	}

	// Seed the accumulated rule set from Config.Rules (now an optional additive
	// override; the generated Register<Svc> contributes the rest via AddRules).
	s := &Server{cfg: cfg, rules: append([]authz.MethodRule(nil), cfg.Rules...)}

	authzOpts := []grpcauthz.Option{
		// The interceptor reads the LIVE accumulated set so rules contributed by
		// Register<Svc> (AddRules) after New are enforced.
		grpcauthz.WithRuleSource(func() []authz.MethodRule { return s.rules }),
		grpcauthz.WithAuthorizer(cfg.Authorizer),
	}
	if cfg.PrincipalFunc != nil {
		authzOpts = append(authzOpts, grpcauthz.WithPrincipalFunc(cfg.PrincipalFunc))
	}

	// Interceptor chain — outermost first. FieldMaskUnary reads the live verb map
	// (FullMethod -> verb) so update-method mask validation covers AddRules rules.
	//
	// Resilience placement:
	//   - RateLimitUnary: right after TenantIDUnary, before LoggingUnary/authz —
	//     sheds load before any authz work; still has tenant context.
	//   - BreakerUnary: framework-chain innermost (after DeduplicateUnary) — just
	//     outside the actual handler, per spec.
	//   - TimeoutUnary: truly innermost (after cfg.Interceptors and BreakerUnary)
	//     — bounds the handler call itself.
	chain := []grpc.UnaryServerInterceptor{
		middleware.RequestIDUnary(),
		middleware.ErrorMapperUnary(),
		middleware.TenantIDUnary(),
	}
	// Rate-limit: shed load early, before logging and authz.
	if cfg.Resilience.RateLimiter != nil {
		chain = append(chain, resilience.RateLimitUnary(cfg.Resilience.RateLimiter))
	}
	chain = append(chain,
		// LoggingUnary sits after request-ID/tenant (so the record carries both)
		// and before authz (so it captures the final code, e.g. PermissionDenied).
		// It is trace-correlated and redacts secret-annotated payload fields.
		middleware.LoggingUnary(cfg.Logger),
		grpcauthz.UnaryServerInterceptor("sdk", authzOpts...),
		middleware.FieldMaskUnarySource(func() map[string]string { return s.verbMap() }),
		etag.PreconditionUnary(),
		middleware.ReadMaskUnary(),
		middleware.ValidateOnlyUnary(),
		middleware.DeduplicateUnary(cfg.DeduplicationStore),
	)
	chain = append(chain, cfg.Interceptors...)
	// Breaker: just outside the handler (framework-chain innermost position,
	// after any caller-supplied interceptors).
	if cfg.Resilience.CircuitBreaker != nil {
		chain = append(chain, resilience.BreakerUnary(cfg.Resilience.CircuitBreaker))
	}
	// Timeout: truly innermost — wraps the actual handler invocation.
	// Applied when RequestTimeout > 0 (or when per-method overrides exist).
	if cfg.Resilience.RequestTimeout > 0 || len(cfg.Resilience.PerMethodTimeout) > 0 {
		chain = append(chain, resilience.TimeoutUnary(cfg.Resilience.RequestTimeout, cfg.Resilience.PerMethodTimeout))
	}

	// StatsHandler installs the OTel gRPC server instrumentation: per-RPC server
	// spans + RED metrics (rpc.server.duration, request/response sizes) emitted to
	// the GLOBAL TracerProvider/MeterProvider. Those globals are a no-op until an
	// adapter (observability/otel) installs an SDK — so this is free and
	// side-effect-free by default. otelgrpc depends on the OTel API only, never the
	// SDK; spans/metrics come from the stats handler and logging from the chain, so
	// they never double-instrument.
	s.grpcSrv = grpc.NewServer(
		grpc.ChainUnaryInterceptor(chain...),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)

	// Register the gRPC health service (AC-1). The health.Server is always
	// present so a gRPC-native probe can be used even when the HTTP gateway is
	// disabled. It starts SERVING; the readiness aggregator drives it to
	// NOT_SERVING while any readiness check fails (AC-3).
	//
	// Health methods are declared PUBLIC so they bypass the authz interceptor
	// (AC-4: probes must be reachable unauthenticated, e.g. by the kubelet).
	// We seed them into the accumulated rule set and also record them as registered
	// methods so the completeness gate at Serve does not fail-close on them.
	s.healthSrv = health.NewServer()
	s.healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s.grpcSrv, s.healthSrv)
	healthMethods := []string{
		grpc_health_v1.Health_Check_FullMethodName,
		grpc_health_v1.Health_List_FullMethodName,
		grpc_health_v1.Health_Watch_FullMethodName,
	}
	for _, m := range healthMethods {
		s.rules = append(s.rules, authz.MethodRule{Method: m, Public: true})
	}
	s.methods = append(s.methods, healthMethods...)

	if cfg.HTTPAddr != "" {
		s.gwMux = runtime.NewServeMux(
			runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
			runtime.WithErrorHandler(httpErrorHandler),
		)
	}

	return s, nil
}

// verbMap returns the live FullMethod -> verb map derived from the accumulated
// rule set, for the field-mask interceptor.
func (s *Server) verbMap() map[string]string {
	m := make(map[string]string, len(s.rules))
	for _, r := range s.rules {
		m[r.Method] = string(r.Verb)
	}
	return m
}

// incomingHeaderMatcher forwards the headers the framework chain reads — from the
// HTTP gateway into gRPC metadata — so the documented REST examples work with
// plain headers:
//   - account-id (tenant scoping, via middleware.TenantIDUnary) plus subject/groups
//     (consumed by grpcauthz.DevPrincipalFunc), e.g. `-H 'account-id: t1'`;
//   - if-match / if-none-match (AIP-154 conditional requests) so etag.PreconditionUnary
//     can enforce the 412 precondition over the gateway, not just over direct gRPC.
//
// All other headers keep grpc-gateway's default behavior, including the standard
// `Grpc-Metadata-` prefix passthrough.
func incomingHeaderMatcher(key string) (string, bool) {
	switch strings.ToLower(key) {
	case "account-id", "subject", "groups", "if-match", "if-none-match":
		return strings.ToLower(key), true
	default:
		return runtime.DefaultHeaderMatcher(key)
	}
}

// httpErrorHandler is the gateway's HTTP error handler. It defers to
// grpc-gateway's default mapping/body for every error except a failed AIP-154
// ETag precondition: middleware.ErrorMapperUnary surfaces that as
// codes.FailedPrecondition, which grpc-gateway would otherwise render as 400.
// AIP-154 specifies 412 Precondition Failed, and the documented client recipe
// ("echo the ETag as If-Match for a 412-guarded conditional update") expects it,
// so we keep the default JSON body but override the status line to 412.
func httpErrorHandler(ctx context.Context, mux *runtime.ServeMux, m runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	if status.Code(err) == codes.FailedPrecondition {
		w = &statusOverrideWriter{ResponseWriter: w, code: http.StatusPreconditionFailed}
	}
	runtime.DefaultHTTPErrorHandler(ctx, mux, m, w, r, err)
}

// statusOverrideWriter forces the HTTP status code written by the wrapped
// handler to a fixed value, leaving headers and body untouched. Used to surface
// a failed ETag precondition as 412 (see httpErrorHandler).
type statusOverrideWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusOverrideWriter) WriteHeader(int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(w.code)
}

// Serve starts the gRPC server (and the HTTP gateway when configured) and
// blocks until ctx is cancelled, after which it shuts both down gracefully.
// It returns the first fatal error from either server, or nil on clean
// shutdown.
func (s *Server) Serve(ctx context.Context) error {
	// Boot-time completeness gate (fail-closed), now run over the ACCUMULATED rule
	// set + every registered method: a registered RPC with neither a rule nor a
	// public exemption must not serve. Rules contributed via AddRules (by the
	// generated Register<Svc>) are visible here because the gate runs at Serve,
	// after all registration, rather than per-Register.
	if err := grpcauthz.AssertMethodsDeclared(
		s.methods,
		grpcauthz.WithRuleSource(func() []authz.MethodRule { return s.rules }),
	); err != nil {
		return err
	}
	// F031 DDD boundary gate (fail-closed), beside the authz completeness gate: a
	// declared aggregate member resource must not register a write-capable standard
	// method on the transport surface — writes route through the aggregate root.
	if err := AssertAggregateBoundaries(s.methods, s.memberBindings); err != nil {
		return err
	}

	// Derive a cancellable context so Serve OWNS the lifecycle of every background
	// goroutine it starts (the readiness loop): it stops when ctx is cancelled OR
	// when Serve returns for any reason — including the error path below, where the
	// caller's ctx may never be cancelled. Without this the readiness loop would
	// outlive a Serve that returned early, leaking a goroutine + ticker that keeps
	// driving the (already-stopped) gRPC health server.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lis, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("server: listen %q: %w", s.cfg.GRPCAddr, err)
	}
	s.setGRPCLis(lis)

	errCh := make(chan error, 2)
	go func() {
		if err := s.grpcSrv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			errCh <- fmt.Errorf("server: grpc serve: %w", err)
		}
	}()

	var httpSrv *http.Server
	if s.gwMux != nil {
		// The in-process gateway->gRPC dial carries the OTel gRPC client stats
		// handler so the gateway's HTTP server span continues into a gRPC client
		// span and W3C trace context is injected into the gRPC metadata — yielding
		// ONE trace across the REST hop. No-op until an adapter installs an SDK.
		conn, err := grpc.NewClient(
			lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		)
		if err != nil {
			s.grpcSrv.Stop()
			return fmt.Errorf("server: dial gateway upstream: %w", err)
		}
		for _, fn := range s.gatewayFns {
			if err := fn(ctx, s.gwMux, conn); err != nil {
				s.grpcSrv.Stop()
				return fmt.Errorf("server: register gateway: %w", err)
			}
		}
		httpLis, err := net.Listen("tcp", s.cfg.HTTPAddr)
		if err != nil {
			s.grpcSrv.Stop()
			return fmt.Errorf("server: listen http %q: %w", s.cfg.HTTPAddr, err)
		}
		s.setHTTPLis(httpLis)

		// Compose an outer ServeMux so that /healthz and /readyz are registered
		// BEFORE the gateway mux and are NOT subject to the authz interceptor
		// (they're HTTP-only, never traverse the gRPC chain — AC-4). The gateway
		// mux is mounted at "/" so all other traffic still routes normally.
		//
		// otelhttp wraps the gateway mux (not the probe routes) so gateway spans
		// are traced; probes intentionally stay off the trace path (kubelet noise).
		outerMux := http.NewServeMux()
		outerMux.HandleFunc("/healthz", s.handleLiveness)
		outerMux.HandleFunc("/readyz", s.handleReadiness)
		outerMux.Handle("/", otelhttp.NewHandler(s.gwMux, "gateway"))

		httpSrv = &http.Server{Handler: outerMux}
		go func() {
			if err := httpSrv.Serve(httpLis); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("server: http serve: %w", err)
			}
		}()
	}

	// Start the readiness-check background loop that keeps the gRPC health status
	// in sync with the aggregated readiness result (AC-3) on a 5-second poll. It
	// runs regardless of whether the HTTP gateway is enabled (the gRPC health
	// service is always registered). It stops when the derived ctx is cancelled —
	// which the deferred cancel above guarantees on EVERY Serve return path, so the
	// loop never outlives the server.
	go s.runReadinessLoop(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		// A server failed to start/run; tear down and report it.
		s.shutdown(httpSrv)
		return err
	}

	s.shutdown(httpSrv)

	// Surface any error captured during shutdown without blocking.
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// shutdown gracefully stops the HTTP gateway (if any) and the gRPC server,
// bounded by shutdownTimeout.
func (s *Server) shutdown(httpSrv *http.Server) {
	grpcDone := make(chan struct{})
	go func() {
		s.grpcSrv.GracefulStop()
		close(grpcDone)
	}()

	if httpSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = httpSrv.Shutdown(shutCtx)
		cancel()
	}

	select {
	case <-grpcDone:
	case <-time.After(shutdownTimeout):
		s.grpcSrv.Stop()
	}
}

// handleLiveness serves GET /healthz: returns 200 as long as the process is up.
// No dependency checks — liveness is process-only (AC-2, AC-4).
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleReadiness serves GET /readyz: runs the readiness aggregator and returns
// 200 if all checks pass, 503 + JSON listing failures otherwise (AC-2).
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	failures := sdkhealth.Aggregate(r.Context(), s.cfg.ReadinessChecks)
	if len(failures) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	checkErrs := make(map[string]string, len(failures))
	for _, f := range failures {
		checkErrs[f.Name] = f.Err.Error()
	}
	body, _ := json.Marshal(map[string]any{
		"status": "unready",
		"checks": checkErrs,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

// runReadinessLoop polls the readiness aggregator on a 5-second ticker and
// drives the gRPC overall health status (AC-3). It runs until ctx is cancelled.
func (s *Server) runReadinessLoop(ctx context.Context) {
	// Run immediately at startup, then on each tick.
	s.syncHealthStatus(ctx)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncHealthStatus(ctx)
		}
	}
}

// syncHealthStatus runs one readiness-aggregation pass and flips the gRPC
// overall health status accordingly (SERVING ↔ NOT_SERVING).
func (s *Server) syncHealthStatus(ctx context.Context) {
	failures := sdkhealth.Aggregate(ctx, s.cfg.ReadinessChecks)
	if len(failures) == 0 {
		s.healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	} else {
		s.healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}
}

// GRPCServer returns the underlying *grpc.Server so callers can register their
// service implementations on it.
func (s *Server) GRPCServer() *grpc.Server { return s.grpcSrv }

// Rules returns the accumulated authz rule set: Config.Rules plus everything
// contributed via AddRules (e.g. by the generated Register<Svc>).
func (s *Server) Rules() []authz.MethodRule { return s.rules }

// AddRules appends authz rules to the server's accumulated set. The generated
// Register<Svc>/Register<Svc>WithRepository call it with the service's
// <Svc>AuthzRules so the developer never hand-assembles Config.Rules. The authz
// and field-mask interceptors read the live set, and the boot-time completeness
// gate runs over it at Serve. Call before Serve.
func (s *Server) AddRules(rules ...authz.MethodRule) {
	s.rules = append(s.rules, rules...)
}

// RecordMethods records gRPC FullMethods registered with the server so the
// completeness gate at Serve can verify each has a rule or a public exemption.
// The generated Register<Svc> calls it with the service's method names.
func (s *Server) RecordMethods(methods ...string) {
	s.methods = append(s.methods, methods...)
}

// RecordMemberBinding records that a service's resource is a DDD aggregate MEMBER
// owned by a root (with the write methods it registers). The generated
// Register<Svc> of a member service calls it; the boundary gate
// [AssertAggregateBoundaries] runs over the accumulated set at Serve (fail-closed:
// a member that registers a write method does not serve). Call before Serve.
func (s *Server) RecordMemberBinding(b MemberBinding) {
	s.memberBindings = append(s.memberBindings, b)
}

// MemberBindings returns the accumulated DDD aggregate member→root bindings.
func (s *Server) MemberBindings() []MemberBinding { return s.memberBindings }

// LROStore returns the long-running operation store this server was configured with.
func (s *Server) LROStore() lro.Store { return s.cfg.LROStore }

// GatewayMux returns the HTTP gateway mux, or nil when no HTTP gateway is
// configured.
func (s *Server) GatewayMux() *runtime.ServeMux { return s.gwMux }

// RegisterGateway records a gateway registration function to be invoked against
// the gateway mux and the in-process gRPC connection when Serve starts. It is a
// no-op at runtime unless an HTTP gateway is configured.
func (s *Server) RegisterGateway(fn func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error) {
	s.gatewayFns = append(s.gatewayFns, fn)
}

func (s *Server) setGRPCLis(lis net.Listener) {
	s.lisMu.Lock()
	s.grpcLis = lis
	s.lisMu.Unlock()
}

func (s *Server) setHTTPLis(lis net.Listener) {
	s.lisMu.Lock()
	s.httpLis = lis
	s.lisMu.Unlock()
}

// GRPCAddr returns the actual bound gRPC address once Serve has started (useful
// when GRPCAddr was ":0"); before that it returns the configured address.
func (s *Server) GRPCAddr() string {
	s.lisMu.Lock()
	lis := s.grpcLis
	s.lisMu.Unlock()
	if lis != nil {
		return lis.Addr().String()
	}
	return s.cfg.GRPCAddr
}

// HTTPAddr returns the actual bound HTTP gateway address once Serve has started
// (useful when HTTPAddr was ":0"); before that it returns the configured address.
// Returns "" when no HTTP gateway is configured.
func (s *Server) HTTPAddr() string {
	s.lisMu.Lock()
	lis := s.httpLis
	s.lisMu.Unlock()
	if lis != nil {
		return lis.Addr().String()
	}
	return s.cfg.HTTPAddr
}
