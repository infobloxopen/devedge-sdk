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

	// Database declares the SHARED database the composed host's modules namespace
	// themselves within (WS-012 P2). When set (Engine non-empty), Run allocates a
	// DatabaseNamespace per module from this engine + each module's
	// DatabaseDescriptor policy, hands it to the module via App.DB, and — if
	// Migrate is set — runs the module's migrations under a per-module advisory lock
	// BEFORE the module registers. When unset, the host has no shared DB axis (the
	// single-module / unshared-DB default) and behavior is unchanged.
	Database *DatabaseConfig

	// Migrate is the host's per-module migration runner (WS-012 P2). servicekit is
	// ORM-free, so the host supplies the runner that actually executes a module's
	// migration against the concrete backend (the gormtx/entrepo adapter), under the
	// advisory lock the adapter owns. Run calls it once per module, after the
	// module's namespace is allocated and before the module registers — host-run,
	// never module-run-from-init. nil disables migration (the module/host migrated
	// elsewhere, or there is nothing to migrate).
	Migrate MigrationRunner
}

// DatabaseConfig is the shared-database declaration a composed host owns (WS-012 P2):
// the engine every module namespaces itself within and the composition's default
// isolation policy. The per-module schema/prefix is derived from this plus the
// module's DatabaseDescriptor.
type DatabaseConfig struct {
	// Engine is the shared DB engine (e.g. "postgres"). It selects the schema-vs-
	// prefix branch of each module's isolation policy. Empty means no shared DB
	// (the single-module default), in which case no namespacing is applied.
	Engine string
	// DefaultIsolation is the composition's default isolation policy, used for any
	// module that leaves its DatabaseDescriptor.Isolation unset. Empty defaults to
	// schema-preferred (Postgres schema, prefix fallback).
	DefaultIsolation IsolationPolicy
}

// MigrationRunner runs ONE module's migrations against the concrete backend, under
// the host's discipline (WS-012 §5.4): the host calls it per module, after the
// module's DatabaseNamespace is allocated and before the module registers. The
// implementation (supplied by the host, which imports the gormtx/entrepo adapter)
// acquires the per-module advisory lock, ensures the schema/prefix, runs the
// migrations, and stamps the module's own migration table. It MUST be idempotent.
//
// servicekit defines only this seam (it stays ORM-free); the adapter provides the
// concrete runner (e.g. one that calls gormtx.MigrateModule).
type MigrationRunner func(ctx context.Context, ns DatabaseNamespace, d DatabaseDescriptor) error

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
	// P3: resolve the remaining shared backends (events.Bus, metrics) here too.
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

	// P2: the host's database registry resolves a per-module DatabaseNamespace from
	// the shared engine + each module's isolation policy. With no HostConfig.Database
	// the engine is empty and the registry yields a zero-qualification namespace
	// (single-module / unshared-DB default — unchanged behavior).
	dbReg := hostDatabaseRegistry{}
	if hc.Database != nil {
		dbReg.engine = hc.Database.Engine
		dbReg.defaultPolicy = hc.Database.DefaultIsolation
	}

	app := &App{
		Server:  srv,
		Config:  newConfigProvider(hc.ConfigSources),
		DB:      dbReg,
		Events:  inertEventRegistry{},
		Health:  &serverHealthRegistry{srv: srv},
		Logger:  logger,
		Metrics: inertMetricsRegistry{},
	}

	// Step 4: per module — allocate its DatabaseNamespace, run its migrations under a
	// per-module advisory lock (host-run, NEVER module-run), then register it onto
	// the shared server, in slice order.
	//
	// P3: start one relay + consumer per module outbox here; start the module's
	//     background jobs under supervision; wrap Register in a per-module
	//     bulkhead/panic boundary keyed by module ID.
	for _, m := range hc.Modules {
		d := m.Descriptor()

		// (4a) allocate the module's DB namespace from its descriptor policy.
		ns, nerr := dbReg.Namespace(d.ID, d.Database)
		if nerr != nil {
			return fmt.Errorf("servicekit: allocate DB namespace for module %q: %w", d.ID, nerr)
		}

		// (4b) host-run, advisory-locked migration before the module registers. The
		// runner (adapter-supplied) owns the lock + schema/prefix creation + stamping
		// the module's own migration table. It runs whenever a runner is supplied; the
		// namespace it receives is zero-qualification for a single-module / unshared-DB
		// host (bare tables, unchanged) and per-module schema/prefix when a shared DB
		// is declared. Migration is HOST-run here, never module-run-from-init.
		if hc.Migrate != nil {
			if merr := hc.Migrate(ctx, ns, d.Database); merr != nil {
				return fmt.Errorf("servicekit: migrate module %q: %w", d.ID, merr)
			}
		}

		// (4c) register the module onto the shared server. The module reads its
		// namespace from app.DB.Namespace(d.ID, d.Database) and binds its repo/stores
		// to it.
		if rerr := m.Register(ctx, app); rerr != nil {
			return fmt.Errorf("servicekit: register module %q: %w", d.ID, rerr)
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
