// Package server provides a batteries-included gRPC server builder for
// Infoblox services. It assembles the framework interceptor chain (request-ID,
// error mapping, tenant-ID, fail-closed authz, field-mask validation, ETag
// preconditions, read-mask response shaping) and, optionally, an HTTP/JSON
// gateway in front of the gRPC endpoint.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/lro"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
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
}

// Server is the assembled gRPC server (plus optional HTTP gateway).
type Server struct {
	cfg        Config
	grpcSrv    *grpc.Server
	gwMux      *runtime.ServeMux // nil when HTTPAddr == ""
	gatewayFns []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error
	grpcLis    net.Listener // set by Serve
	httpLis    net.Listener // set by Serve when HTTPAddr != ""

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
	chain := []grpc.UnaryServerInterceptor{
		middleware.RequestIDUnary(),
		middleware.ErrorMapperUnary(),
		middleware.TenantIDUnary(),
		grpcauthz.UnaryServerInterceptor("sdk", authzOpts...),
		middleware.FieldMaskUnarySource(func() map[string]string { return s.verbMap() }),
		etag.PreconditionUnary(),
		middleware.ReadMaskUnary(),
		middleware.ValidateOnlyUnary(),
		middleware.DeduplicateUnary(cfg.DeduplicationStore),
	}
	chain = append(chain, cfg.Interceptors...)

	s.grpcSrv = grpc.NewServer(grpc.ChainUnaryInterceptor(chain...))

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

	lis, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("server: listen %q: %w", s.cfg.GRPCAddr, err)
	}
	s.grpcLis = lis

	errCh := make(chan error, 2)
	go func() {
		if err := s.grpcSrv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			errCh <- fmt.Errorf("server: grpc serve: %w", err)
		}
	}()

	var httpSrv *http.Server
	if s.gwMux != nil {
		conn, err := grpc.NewClient(
			lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
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
		s.httpLis = httpLis
		httpSrv = &http.Server{Handler: s.gwMux}
		go func() {
			if err := httpSrv.Serve(httpLis); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("server: http serve: %w", err)
			}
		}()
	}

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

// GRPCAddr returns the actual bound gRPC address once Serve has started (useful
// when GRPCAddr was ":0"); before that it returns the configured address.
func (s *Server) GRPCAddr() string {
	if s.grpcLis != nil {
		return s.grpcLis.Addr().String()
	}
	return s.cfg.GRPCAddr
}

// HTTPAddr returns the actual bound HTTP gateway address once Serve has started
// (useful when HTTPAddr was ":0"); before that it returns the configured address.
// Returns "" when no HTTP gateway is configured.
func (s *Server) HTTPAddr() string {
	if s.httpLis != nil {
		return s.httpLis.Addr().String()
	}
	return s.cfg.HTTPAddr
}
