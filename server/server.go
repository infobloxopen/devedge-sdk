// Package server provides a batteries-included gRPC server builder for
// Infoblox services. It assembles the framework interceptor chain (request-ID,
// error mapping, tenant-ID, fail-closed authz, field-mask validation, ETag
// preconditions) and, optionally, an HTTP/JSON gateway in front of the gRPC
// endpoint.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
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
	// Interceptors are additional unary interceptors appended after the
	// framework chain.
	Interceptors []grpc.UnaryServerInterceptor
}

// Server is the assembled gRPC server (plus optional HTTP gateway).
type Server struct {
	cfg        Config
	grpcSrv    *grpc.Server
	gwMux      *runtime.ServeMux // nil when HTTPAddr == ""
	gatewayFns []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error
	grpcLis    net.Listener // set by Serve
	httpLis    net.Listener // set by Serve when HTTPAddr != ""
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

	// verbMap feeds FieldMaskUnary: FullMethod -> verb string.
	verbMap := make(map[string]string, len(cfg.Rules))
	for _, r := range cfg.Rules {
		verbMap[r.Method] = string(r.Verb)
	}

	authzOpts := []grpcauthz.Option{
		grpcauthz.WithRules(cfg.Rules...),
		grpcauthz.WithAuthorizer(cfg.Authorizer),
	}

	// Interceptor chain — outermost first.
	chain := []grpc.UnaryServerInterceptor{
		middleware.RequestIDUnary(),
		middleware.ErrorMapperUnary(),
		middleware.TenantIDUnary(),
		grpcauthz.UnaryServerInterceptor("sdk", authzOpts...),
		middleware.FieldMaskUnary(verbMap),
		etag.PreconditionUnary(),
	}
	chain = append(chain, cfg.Interceptors...)

	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(chain...))

	var gwMux *runtime.ServeMux
	if cfg.HTTPAddr != "" {
		gwMux = runtime.NewServeMux()
	}

	return &Server{cfg: cfg, grpcSrv: grpcSrv, gwMux: gwMux}, nil
}

// Serve starts the gRPC server (and the HTTP gateway when configured) and
// blocks until ctx is cancelled, after which it shuts both down gracefully.
// It returns the first fatal error from either server, or nil on clean
// shutdown.
func (s *Server) Serve(ctx context.Context) error {
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

// Rules returns the declared MethodRules this server was configured with.
func (s *Server) Rules() []authz.MethodRule { return s.cfg.Rules }

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
