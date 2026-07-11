package servicekit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/infobloxopen/devedge-sdk/authn"
	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/config"
	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/middleware"
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

	// HTTPHandlers mount custom net/http handlers on the shared HTTP server at
	// path patterns, for endpoints that are NOT gRPC-gateway routes — an OIDC
	// provider's authorization/token/JWKS/discovery endpoints, webhooks, a login
	// UI, static assets. They compose with the module gateways on the one HTTP
	// server (see server.Config.HTTPHandlers). Requires HTTPAddr to be set.
	HTTPHandlers []server.HTTPHandler

	// Authorizer is the shared decision point handed to the one server. Defaults
	// to a default-deny dev authorizer (server.New's default) when nil.
	Authorizer authz.Authorizer
	// PrincipalFunc derives the authz.Principal from each request. When nil the
	// principal is empty, so every non-public method is denied (fail closed).
	PrincipalFunc grpcauthz.PrincipalFunc

	// Authenticator, when set, inserts the WS-026 authentication interceptor
	// before authz on the shared server: it verifies the request bearer and
	// stashes the verified principal for the authorizer to read (see
	// server.Config.Authenticator). Nil preserves today's behavior.
	Authenticator authn.Authenticator

	// Logger is the host's structured logger; each module gets a child scoped to
	// its ID. Defaults to slog.Default() when nil.
	Logger *slog.Logger

	// ConfigSources are the configuration sources (env/flags/file) the host loads
	// module config from. P3 layers them: a platform-global layer + per-module
	// prefix-scoped layers, so each module reads only its own slice.
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

	// Bus is the shared event bus the host owns one relay + one consumer per module
	// over (WS-012 P3). nil defaults to an in-process membus — the same-binary,
	// one-DB default (durable outbox → relay → in-process bus → consumer; NOT a
	// direct handler call). A composed host spanning DBs/daemons passes a Kafka bus
	// behind the same events.Bus seam.
	Bus events.Bus

	// FailurePolicies overrides a module's self-declared FailurePolicy (Descriptor)
	// per the composition (WS-012 P3, §5.9), keyed by module ID. A composition marks
	// core modules fail-host and optional modules degraded here without changing the
	// module's code. An entry wins over the module's descriptor default.
	FailurePolicies map[string]FailurePolicy

	// DefaultFailurePolicy is the host-wide default for any module that declares
	// none and has no FailurePolicies entry. Empty defaults to FailHost (a module
	// failure fails the host fast — the conservative, standalone-friendly default).
	DefaultFailurePolicy FailurePolicy

	// DurableIdempotency opts the host into the durable, exactly-once request-
	// idempotency path (WS-043 / F048). nil leaves the best-effort in-memory
	// DeduplicateUnary default unchanged. When set, Run installs a late-bound host
	// holder as server.Config.DurableDedup, each module supplies its namespaced store +
	// tx via App.EnableDurableIdempotency, the host runs a periodic GC sweep, and boot
	// fails loudly if a registered module's idempotency_keys table is not migrated. A
	// module with no DB is fine — the host falls back to a correct in-process store.
	DurableIdempotency *DurableIdempotencyConfig
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

// hostState is the host's shared, cross-module runtime the per-module App helpers
// reference (App.RegisterOutboxRelay / Subscribe / RegisterBackgroundJob). It holds
// the one shared event registry (host-owned dispatchers), the supervisor (bulkheads +
// failure policy), and the collected background jobs. Run builds it once.
type hostState struct {
	events *hostEventRegistry
	sup    *supervisor

	mu   sync.Mutex
	jobs []backgroundJob

	// idemEnabled reports whether the host opted into durable idempotency
	// (HostConfig.DurableIdempotency != nil); idemRegs collects the per-module
	// registrations from App.EnableDurableIdempotency during Register.
	idemEnabled bool
	idemRegs    []durableIdemRegistration
}

// registerDurableIdempotency records a module's durable idempotency store + tx so the
// host wires them into the shared holder. Called from App.EnableDurableIdempotency.
// It fails loud when the host did not opt in, when a field is nil, or on a second
// call for the same module.
func (h *hostState) registerDurableIdempotency(moduleID string, reg DurableIdempotencyRegistration) error {
	if !h.idemEnabled {
		return fmt.Errorf("servicekit: module %q called EnableDurableIdempotency but HostConfig.DurableIdempotency is not set (opt in at the host)", moduleID)
	}
	if reg.Store == nil || reg.Tx == nil {
		return fmt.Errorf("servicekit: module %q EnableDurableIdempotency: Store and Tx are both required", moduleID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.idemRegs {
		if r.moduleID == moduleID {
			return fmt.Errorf("servicekit: module %q already enabled durable idempotency (once per module)", moduleID)
		}
	}
	h.idemRegs = append(h.idemRegs, durableIdemRegistration{moduleID: moduleID, store: reg.Store, tx: reg.Tx})
	return nil
}

// backgroundJob is a supervised job the host runs for a module.
type backgroundJob struct {
	moduleID string
	name     string
	fn       func(ctx context.Context) error
}

// registerJob records a module's background job so Run starts it under the module's
// bulkhead after the boot gate passes.
func (h *hostState) registerJob(moduleID, name string, fn func(ctx context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("servicekit: module %q background job %q has a nil function", moduleID, name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobs = append(h.jobs, backgroundJob{moduleID: moduleID, name: name, fn: fn})
	return nil
}

// Run is the composed-host entrypoint (proposal §5.3). It builds the ONE shared
// server, resolves the shared backends once, registers every module on it under a
// per-module bulkhead, starts exactly one relay + one consumer per module outbox plus
// every supervised background job, and serves — the same path whether one module
// (standalone) or N (a suite). The full §5.3 lifecycle:
//
//  1. resolve host defaults + the root context (host owns signals);
//  2. validate the descriptors (unique IDs; no duplicate service names / route
//     prefixes / permission names; a coherent, globally-unique event graph);
//  3. resolve the SHARED backends once: the one server (interceptor chain), the one
//     events.Bus, the host config store, the per-module DB registry, the supervisor;
//  4. for each module, in slice order: allocate its DatabaseNamespace (§5.4), run its
//     migrations under a per-module advisory lock (host-run), then Register it onto the
//     shared server inside the module's panic boundary — the module wires its rules/
//     gateway, its health checks, its outbox relay + handlers, and its background jobs;
//  5. start ONE relay + ONE consumer per module outbox (host-owned, no double-start)
//     and every supervised background job, each in its module's bulkhead;
//  6. server.Serve(ctx) — the EXISTING fail-closed union completeness gate
//     (server.go:337) validates the combined surface, then blocks until ctx is done;
//  7. on shutdown: close the shared bus + wait for the supervised goroutines.
func Run(hc HostConfig) error {
	// Step 1: host defaults + root context. The HOST owns signal handling and process
	// lifetime — a module never installs a signal handler or calls os.Exit. The
	// context is cancellable with a CAUSE so a FailHost module failure can fail-fast.
	parent := hc.Context
	var stopSignals context.CancelFunc
	if parent == nil {
		parent, stopSignals = signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stopSignals()
	}
	ctx, failHost := context.WithCancelCause(parent)
	defer failHost(nil)

	logger := hc.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Step 2: validate the composition (IDs, surfaces, event graph) before building.
	if err := ValidateModules(hc.Modules); err != nil {
		return err
	}
	// Reject a module config prefix that collides with a host-owned platform-global
	// namespace (runtime/database/observability/authz/events) — a module must not
	// shadow the host's config layer.
	for _, m := range hc.Modules {
		d := m.Descriptor()
		if p := normalizePrefix(configPrefixFor(d)); p != "" && isReservedConfigPrefix(p) {
			return fmt.Errorf("servicekit: module %q config prefix %q collides with a host-owned platform-global namespace", d.ID, p)
		}
	}

	grpcAddr := hc.GRPCAddr
	if grpcAddr == "" {
		grpcAddr = server.DefaultGRPCAddr
	}

	// Step 3: the ONE shared server. Modules contribute their rules/methods/gateway
	// onto it; the union completeness gate runs at Serve.
	serverCfg := server.Config{
		GRPCAddr:      grpcAddr,
		HTTPAddr:      hc.HTTPAddr,
		HTTPHandlers:  hc.HTTPHandlers,
		Authorizer:    hc.Authorizer,
		PrincipalFunc: hc.PrincipalFunc,
		Authenticator: hc.Authenticator,
		Logger:        logger,
	}
	// Durable idempotency (DA-3): install a late-bound host holder as DurableDedup so
	// the interceptor chain is built now but the per-module stores/txs are supplied
	// during Register below. The holder is a valid (non-nil) Store + Tx at New; it is
	// only CALLED once requests arrive (post-Serve), by which time it is populated.
	var idemHolder *hostDurableDedup
	if hc.DurableIdempotency != nil {
		idemHolder = newHostDurableDedup()
		serverCfg.DurableDedup = &middleware.DurableDedup{
			Store:              idemHolder,
			Tx:                 idemHolder,
			TTL:                hc.DurableIdempotency.TTL,
			DisableFingerprint: hc.DurableIdempotency.DisableFingerprint,
			MaxResponseBytes:   hc.DurableIdempotency.MaxResponseBytes,
			Mode:               hc.DurableIdempotency.Mode,
		}
	}
	srv, err := server.New(serverCfg)
	if err != nil {
		return err
	}

	// The per-module DB registry (P2): resolves a per-module DatabaseNamespace from
	// the shared engine + each module's isolation policy. With no HostConfig.Database
	// the engine is empty and the registry yields a zero-qualification namespace
	// (single-module / unshared-DB default — unchanged behavior).
	dbReg := hostDatabaseRegistry{}
	if hc.Database != nil {
		dbReg.engine = hc.Database.Engine
		dbReg.defaultPolicy = hc.Database.DefaultIsolation
	}

	// The host config store (P3): one source set, scoped per module by prefix.
	cfgStore := newConfigStore(hc.ConfigSources)

	// The shared event registry (P3): one bus, host-owned relays + consumers.
	eventReg := newHostEventRegistry(hc.Bus)

	// The supervisor (P3): per-module bulkheads + failure policy. policyOf resolves a
	// module's effective FailurePolicy (composition override > descriptor > host default).
	policies := resolveFailurePolicies(hc)
	sup := newSupervisor(logger, failHost, func(moduleID string) FailurePolicy {
		return policies[moduleID]
	})

	host := &hostState{events: eventReg, sup: sup, idemEnabled: hc.DurableIdempotency != nil}

	// Step 4: per module — allocate namespace, migrate (host-run, advisory-locked),
	// then Register inside the module's panic boundary, in slice order.
	for _, m := range hc.Modules {
		d := m.Descriptor()

		// (4a) allocate the module's DB namespace from its descriptor policy.
		ns, nerr := dbReg.Namespace(d.ID, d.Database)
		if nerr != nil {
			return fmt.Errorf("servicekit: allocate DB namespace for module %q: %w", d.ID, nerr)
		}

		// (4b) host-run, advisory-locked migration before the module registers.
		if hc.Migrate != nil {
			if merr := hc.Migrate(ctx, ns, d.Database); merr != nil {
				return fmt.Errorf("servicekit: migrate module %q: %w", d.ID, merr)
			}
		}

		// Register the module's readiness gate on the shared server so a later degraded
		// failure attributes to exactly this module on /readyz (P3 health attribution).
		srv.AddReadinessCheck(sup.readinessFor(d.ID))

		// (4c) register the module onto the shared server, inside its panic boundary so
		// a panic during wiring is attributed to the module rather than crashing the
		// host. The module gets a per-module App (config scoped to its prefix, the
		// event/job helpers keyed to its ID).
		app := &App{
			Server:   srv,
			Config:   cfgStore.providerFor(configPrefixFor(d)),
			DB:       dbReg,
			Events:   eventReg,
			Health:   &serverHealthRegistry{srv: srv},
			Logger:   logger.With("module", d.ID),
			Metrics:  hostMetricsRegistry{},
			moduleID: d.ID,
			host:     host,
		}
		if rerr := guardCall(d.ID, "Register", func() error { return m.Register(ctx, app) }); rerr != nil {
			return fmt.Errorf("servicekit: register module %q: %w", d.ID, rerr)
		}
	}

	// Step 4d: durable idempotency — resolve the holder's routing from the descriptors
	// + the module registrations, fail loud if a registered store is not migrated, and
	// start the host-scheduled GC sweep tied to the host lifecycle (DA-3/DA-5/DA-6).
	var idemGCDone chan struct{}
	if idemHolder != nil {
		idemHolder.build(hc.Modules, host.idemRegs)
		// A mixed host — some modules opted in, others did not — silently downgrades the
		// unregistered modules to non-durable, per-pod idempotency. Surface it loudly.
		if missing := idemHolder.unregisteredModules(hc.Modules); len(missing) > 0 {
			logger.Warn("servicekit: durable idempotency is enabled but some modules did not call EnableDurableIdempotency; their methods are NOT durably idempotent (they use the in-process fallback)", "modules", missing)
		}
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
		verr := idemHolder.verifyMigrated(probeCtx)
		cancelProbe()
		if verr != nil {
			return verr
		}
		if !hc.DurableIdempotency.DisableGC {
			interval := hc.DurableIdempotency.GCInterval
			if interval <= 0 {
				interval = DefaultIdempotencyGCInterval
			}
			idemGCDone = make(chan struct{})
			go runIdempotencyGC(ctx, idemHolder, interval, logger, idemGCDone)
		}
	}

	// Step 5: start the host-owned dispatchers (exactly one relay + one consumer per
	// module outbox — no double-start) and the supervised background jobs, each in its
	// module's bulkhead. These run until ctx is cancelled (host shutdown or a FailHost
	// module taking the host down).
	eventReg.startDispatchers(ctx, sup)
	host.startJobs(ctx, sup)

	// Step 7 (deferred): on return, close the shared bus + wait for supervised
	// goroutines (and the idempotency GC sweep) so shutdown is clean (no leaked
	// relay/consumer/job/GC goroutines).
	//
	// Cancel the host context FIRST: the GC sweep (and supervised goroutines) only exit
	// on ctx.Done(), so if Serve returns an error while the parent ctx is still live
	// (e.g. a port-bind failure or a fail-closed boot gate — neither cancels ctx), the
	// subsequent waits would block forever. failHost(nil) is idempotent with the
	// early-registered `defer failHost(nil)`, and the context.Cause check below has
	// already run, so this cannot alter the returned error.
	defer func() {
		failHost(nil)
		eventReg.closeBus()
		sup.wait()
		if idemGCDone != nil {
			<-idemGCDone
		}
	}()

	// Step 6: serve. server.Serve runs the EXISTING fail-closed union gate over the
	// combined methods/rules (server.go:337) plus the DDD boundary gate, then blocks
	// until ctx is cancelled.
	if serr := srv.Serve(ctx); serr != nil {
		return serr
	}
	// If a FailHost module took the host down, surface its cause rather than nil.
	if cause := context.Cause(ctx); cause != nil && cause != context.Canceled && cause != ctx.Err() {
		return cause
	}
	return nil
}

// runIdempotencyGC is the host-scheduled durable-idempotency sweep (DA-5): it calls
// store.GC on interval until ctx is cancelled, then closes done so Run's shutdown
// waits for it. A panic is contained + logged (a GC bug never crashes the host); a
// sweep error is logged and retried on the next tick. Retention only grows if this
// stops, so it logs loudly on exit-by-panic.
func runIdempotencyGC(ctx context.Context, store middleware.DurableIdempotencyStore, interval time.Duration, logger *slog.Logger, done chan<- struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			logger.Error("servicekit: idempotency GC panicked; sweep stopped (retention will grow)", "panic", r)
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n, err := store.GC(ctx, now)
			switch {
			case err != nil && ctx.Err() == nil:
				logger.Warn("servicekit: idempotency GC sweep failed", "err", err)
			case n > 0:
				logger.Debug("servicekit: idempotency GC swept expired records", "count", n)
			}
		}
	}
}

// startJobs starts every registered background job under its module's bulkhead.
func (h *hostState) startJobs(ctx context.Context, sup *supervisor) {
	h.mu.Lock()
	jobs := make([]backgroundJob, len(h.jobs))
	copy(jobs, h.jobs)
	h.mu.Unlock()
	for _, j := range jobs {
		j := j
		sup.Go(j.moduleID, "job:"+j.name, func() {
			if err := j.fn(ctx); err != nil && ctx.Err() == nil {
				sup.reportError(j.moduleID, fmt.Errorf("job %q: %w", j.name, err))
			}
		})
	}
}

// resolveFailurePolicies computes the effective FailurePolicy for each module:
// HostConfig.FailurePolicies override > the module's Descriptor.FailurePolicy > the
// host default (HostConfig.DefaultFailurePolicy, or FailHost when unset).
func resolveFailurePolicies(hc HostConfig) map[string]FailurePolicy {
	def := hc.DefaultFailurePolicy
	if def == FailurePolicyUnset {
		def = FailHost
	}
	out := make(map[string]FailurePolicy, len(hc.Modules))
	for _, m := range hc.Modules {
		d := m.Descriptor()
		policy := def
		if d.FailurePolicy != FailurePolicyUnset {
			policy = d.FailurePolicy
		}
		if override, ok := hc.FailurePolicies[d.ID]; ok && override != FailurePolicyUnset {
			policy = override
		}
		out[d.ID] = policy
	}
	return out
}

// configPrefixFor returns the config prefix for a module: its declared
// ConfigDescriptor.Prefix, or its ID when the prefix is empty (the default).
func configPrefixFor(d Descriptor) string {
	if d.Config.Prefix != "" {
		return d.Config.Prefix
	}
	return d.ID
}

// serverHealthRegistry is the HealthRegistry: it appends a module's readiness check to
// the shared server's readiness set. server.Config.ReadinessChecks is the seam the
// server already aggregates over (/readyz + gRPC health), so this is fully functional.
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
