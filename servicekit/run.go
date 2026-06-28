package servicekit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/config"
	"github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/server"
)

// HostConfig is the process-level configuration the HOST owns (never a module):
// listen addresses, the authorizer/principal wiring, the logger, and the set of
// modules to compose. The same HostConfig shape drives both a standalone binary
// (one module) and a composed "suite" binary (N modules).
type HostConfig struct {
	// Modules are the modules to compose into this host. One module is the
	// standalone case; N modules is the composed-suite case. Required.
	Modules []Module

	// GRPCAddr is the shared gRPC listen address (e.g. ":9090"). Defaults to
	// server.DefaultGRPCAddr when empty.
	GRPCAddr string
	// HTTPAddr is the shared HTTP gateway address (e.g. ":8080"). Empty disables
	// the gateway.
	HTTPAddr string

	// Authorizer is the shared decision point handed to the one server. Defaults
	// to a default-deny dev authorizer (server.New's default) when nil.
	Authorizer authz.Authorizer
	// PrincipalFunc derives the authz.Principal from each request. When nil the
	// principal is empty, so every non-public method is denied (fail closed).
	PrincipalFunc grpcauthz.PrincipalFunc

	// Logger is the host's structured logger; each module gets a child scoped to
	// its ID. Defaults to slog.Default() when nil.
	Logger *slog.Logger

	// ConfigSources are the configuration sources (env/flags/file) the host loads
	// module config from. Empty is valid (modules fall back to struct defaults).
	// P3: per-module prefix layering is applied over these sources.
	ConfigSources []config.Source

	// Context is the host's root context; Serve runs until it is cancelled. When
	// nil, Run installs a context cancelled on SIGTERM/Interrupt (the daemon
	// default) — the host, not any module, owns signal handling.
	Context context.Context
}

// Run is the composed-host entrypoint (proposal §5.3). It builds the ONE shared
// server, registers every module on it, and serves — the same path whether one
// module (standalone) or N (a suite). P1 implements the minimal-but-correct
// standalone-and-trivially-N-module lifecycle:
//
//  1. resolve host defaults + the root context (host owns signals);
//  2. validate the descriptors (unique IDs; no duplicate service names / route
//     prefixes / permission names — see ValidateModules);
//  3. server.New(cfg) ONCE — the single shared server + interceptor chain;
//  4. for each module, module.Register(ctx, app) — wiring it onto the shared
//     server (the generated Register<Svc>WithRepository) plus its health checks;
//  5. server.Serve(ctx) — the EXISTING fail-closed union completeness gate
//     (server.go:337) validates the combined surface.
//
// P1 intentionally does NOT do DB module-namespacing (P2), host-owned relay/
// consumer-per-module, config prefix layering, bulkheads, or failurePolicy (P3).
// Those enrich steps 3-5 later; the extension points are marked `P3:` below. The
// registries handed to each module are minimal P1 implementations (config.go).
func Run(hc HostConfig) error {
	ctx := hc.Context
	var stop context.CancelFunc
	if ctx == nil {
		// The HOST owns signal handling and process lifetime — a module never
		// installs a signal handler or calls os.Exit.
		ctx, stop = signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
	}

	logger := hc.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Step 2: validate the composition before building anything.
	if err := ValidateModules(hc.Modules); err != nil {
		return err
	}

	grpcAddr := hc.GRPCAddr
	if grpcAddr == "" {
		grpcAddr = server.DefaultGRPCAddr
	}

	// Step 3: the ONE shared server. Modules contribute their rules/methods/
	// gateway onto it; the union completeness gate runs at Serve.
	//
	// P2: resolve shared backends (DB pool[s], events.Bus, metrics) here and
	//     allocate each module's DatabaseNamespace before Register.
	srv, err := server.New(server.Config{
		GRPCAddr:      grpcAddr,
		HTTPAddr:      hc.HTTPAddr,
		Authorizer:    hc.Authorizer,
		PrincipalFunc: hc.PrincipalFunc,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	app := &App{
		Server:  srv,
		Config:  newConfigProvider(hc.ConfigSources),
		DB:      inertDatabaseRegistry{},
		Events:  inertEventRegistry{},
		Health:  &serverHealthRegistry{srv: srv},
		Logger:  logger,
		Metrics: inertMetricsRegistry{},
	}

	// Step 4: register every module onto the shared server, in slice order.
	//
	// P2: per module, allocate its DatabaseNamespace + run its migrations under a
	//     per-module advisory lock BEFORE Register, then hand Register a repo
	//     bound to the namespaced handle.
	// P3: start one relay + consumer per module outbox here; start the module's
	//     background jobs under supervision; wrap Register in a per-module
	//     bulkhead/panic boundary keyed by module ID.
	for _, m := range hc.Modules {
		d := m.Descriptor()
		if err := m.Register(ctx, app); err != nil {
			return fmt.Errorf("servicekit: register module %q: %w", d.ID, err)
		}
	}

	// Step 5: serve. server.Serve runs the EXISTING fail-closed union gate over
	// the combined methods/rules (server.go:337) plus the DDD boundary gate, then
	// blocks until ctx is cancelled.
	if err := srv.Serve(ctx); err != nil {
		return err
	}
	return nil
}

// serverHealthRegistry is the P1 HealthRegistry: it appends a module's readiness
// check to the shared server's readiness set. server.Config.ReadinessChecks is
// the seam the server already aggregates over (/readyz + gRPC health), so this is
// fully functional in P1.
type serverHealthRegistry struct {
	srv *server.Server
}

func (h *serverHealthRegistry) Register(name string, check health.Check) error {
	if check == nil {
		return fmt.Errorf("servicekit: health check %q is nil", name)
	}
	// AddReadinessCheck appends to the live readiness set (see server.go); the
	// aggregator picks it up on the next probe/poll.
	h.srv.AddReadinessCheck(check)
	return nil
}
