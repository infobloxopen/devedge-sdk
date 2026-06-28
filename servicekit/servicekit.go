// Package servicekit defines the composable-service contract for devedge-sdk:
// a service is split into an importable [Module] (owns DOMAIN behavior) and one
// or more executable HOSTS (own PROCESS behavior). The SAME module runs
// standalone (one module per host) or composed into a "suite" binary (N modules,
// one host) by changing the host config, not the module — the k3s-style property.
//
// The design principle, enforced throughout: a module owns domain behavior; a
// host owns process behavior. A module MAY define resources/handlers/repos/
// migrations/events/config-schema/health/jobs/authz-rules/routes; a module MUST
// NOT own process lifetime, ports, global flags/env/logging, DB creation outside
// its namespace, listener startup, signal handling, os.Exit, or cross-module
// table ownership — those belong to the host (see [Run]).
//
// servicekit lives in the ROOT module and depends only on the SDK's root
// interfaces ([server.Server], [persistence.Repository], [events.Bus],
// [health.Check], [authz.MethodRule], log/slog). Concrete backends (gorm, ent,
// otel, kafka) stay in the optional adapter sub-modules, so the core stays
// dependency-light.
//
// P1 shipped the contract, descriptor types, registry INTERFACES, and a minimal
// [Run]. P2 made the DB axis real: [DatabaseNamespace] + [IsolationPolicy] (aliasing
// the persistence types), a real [DatabaseRegistry] that allocates a per-module
// schema/prefix, and host-run, advisory-locked, per-module-namespaced migration wired
// into [Run] via [HostConfig.Migrate]. P3 (this surface) made the host OWN process
// behavior across modules: it resolves shared backends once, starts exactly one event
// relay + one consumer per module outbox over one shared [events.Bus] (so a composed
// binary does not double-start dispatchers — see [App.RegisterOutboxRelay] /
// [App.Subscribe]), layers config per module prefix (so two modules read isolated
// slices from one source set), and contains per-module failures with in-process
// bulkheads + a [FailurePolicy] (a module panic is recovered + attributed; a degraded
// module marks itself unready while the host stays up; a core module fails the host
// fast). Boot validates a globally-unique, coherent event graph. P4 (de compose), P5
// (test harness), and P6 (deploy rendering) remain. See specs/030-composable-services.
package servicekit

import (
	"context"
	"log/slog"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/server"
)

// Module is the unit of composition: an importable, self-describing service.
// The generator (protoc-gen-svc) emits a Module() constructor whose Descriptor()
// is populated from proto facts and whose Register wraps the existing generated
// Register<Svc>WithRepository over the shared server.
type Module interface {
	// Descriptor returns the module's static, introspectable facts. It must be
	// safe to call before Register (the host validates descriptors first) and
	// must return the same value on every call.
	Descriptor() Descriptor

	// Register wires the module into the shared host. It is given the running
	// host's shared services (the one [server.Server] plus the per-module
	// registries) via app. Implementations register their gRPC service + gateway
	// + authz rules on app.Server (typically by calling the generated
	// Register<Svc>WithRepository) and register any health checks, event
	// handlers, and background jobs. Register MUST NOT start listeners, parse
	// flags, call os.Exit, or otherwise take over process lifetime.
	Register(ctx context.Context, app *App) error
}

// Descriptor is a module's static, introspectable facts — known before boot so
// the host can validate the composition (unique IDs, no duplicate gRPC service
// names / route prefixes / permission names) and so tooling can describe a suite
// without running it. The generator populates it from proto facts it already
// knows; hand-written extras flow through the module's Options callback.
type Descriptor struct {
	// ID is the stable, unique module identifier (e.g. "orders"). It is the key
	// the host uses for uniqueness, the config prefix default (P3), and the DB
	// namespace key (P2). Required.
	ID string
	// DisplayName is a human-friendly label (e.g. "Orders Service"). Optional.
	DisplayName string
	// Version is the module's semantic version (e.g. "v0.4.1"). Optional in P1.
	Version string

	// Methods are the gRPC FullMethods the module registers (e.g.
	// "/orders.v1.OrderService/CreateOrder"). The generator fills these from the
	// proto service; the host uses them to detect duplicate service names across
	// modules (it reuses the server's union completeness gate for rule coverage).
	Methods []string
	// AuthzRules are the module's declared authz rules — the same
	// <Svc>AuthzRules the generated Register<Svc> contributes. Carried on the
	// descriptor so the host can detect duplicate permission names before boot.
	AuthzRules []authz.MethodRule
	// Routes are the HTTP gateway route prefixes / hostnames the module serves
	// (for duplicate-prefix detection and, later, gateway composition).
	Routes []RouteDescriptor
	// Resources are the API resources the module owns (module-qualified, e.g.
	// "orders.order"), for catalog/UI and duplicate detection.
	Resources []ResourceDescriptor

	// Config is the module's typed config schema + prefix (P3 layering).
	Config ConfigDescriptor
	// Database is the module's namespace policy + migrations FS (P2 namespacing).
	Database DatabaseDescriptor
	// Events declares the module's publishes/subscribes + outbox (P3 host-owned
	// relay/consumer per module).
	Events EventDescriptor
	// HealthChecks are the readiness checks the module contributes.
	HealthChecks []HealthDescriptor
	// BackgroundJobs are the supervised background jobs the module runs (P3).
	BackgroundJobs []JobDescriptor

	// Requires declares the SDK/Go/Postgres version ranges the module needs, for
	// composition-time compatibility gating (validated by `de compose tidy` /
	// the test harness in later phases).
	Requires Compatibility

	// FailurePolicy is the module's self-declared failure posture in a composed host
	// (P3, proposal §5.9): FailHost (core — a failure fails the host fast) or Degraded
	// (optional — a failure isolates the module + marks it unready, host stays up).
	// Empty defers to the host default. A composition can override it per module via
	// HostConfig.FailurePolicies.
	FailurePolicy FailurePolicy
}

// RouteDescriptor describes one HTTP gateway route the module serves: a path
// prefix and/or a hostname. The host uses these to detect cross-module route
// collisions and, in P6, to render per-module ingress.
type RouteDescriptor struct {
	// Prefix is the HTTP path prefix (e.g. "/api/orders/v1"). The host detects
	// duplicate prefixes across modules at validation time.
	Prefix string
	// Host is an optional hostname the module answers on (e.g.
	// "orders.dev.test"). Empty means "any host".
	Host string
}

// ResourceDescriptor names one API resource the module owns. Names are
// module-qualified (e.g. "orders.order", not bare "order") so two co-resident
// modules cannot collide on a resource name.
type ResourceDescriptor struct {
	// Name is the module-qualified resource name (e.g. "orders.order").
	Name string
	// Plural is the resource collection id used in authz rules / routes (e.g.
	// "orders"). Optional.
	Plural string
}

// ConfigDescriptor declares a module's typed config schema and its namespace
// prefix. P1 carries the shape; the host's per-module config layering (a
// ConfigProvider scoped to Prefix) lands in P3.
type ConfigDescriptor struct {
	// Prefix is the per-module config namespace (e.g. "orders"); host-global
	// keys (runtime.*, database.*, observability.*) live outside any prefix.
	Prefix string
	// Schema is a pointer to the module's typed config struct (config.Load
	// target). Optional; nil means the module has no config of its own.
	Schema any
	// Defaults are programmatic defaults overlaid beneath the struct `default:`
	// tags. Optional.
	Defaults map[string]any
}

// DatabaseDescriptor declares a module's database isolation policy and the
// filesystem holding its migrations. P1 carries the shape only; the host-run,
// advisory-locked, per-module-namespaced migration runner is P2 (the load-bearing
// new work). A module never runs its own migrations from init().
type DatabaseDescriptor struct {
	// Isolation is the namespacing policy (see [IsolationPolicy]). Empty defers
	// to the host/composition default (schema-preferred, set in P2).
	Isolation IsolationPolicy
	// Schema is the preferred Postgres schema name for the module (defaults to
	// the module ID in P2). Optional in P1.
	Schema string
	// TablePrefix is the table-name prefix for prefix-isolation engines
	// (defaults to a short form of the module ID in P2). Optional in P1.
	TablePrefix string
	// Migrations is the module's embedded migrations filesystem (the host runs
	// them under a per-module advisory lock in P2). nil means no migrations.
	Migrations MigrationsFS
}

// IsolationPolicy is the database module-namespacing policy (P2). It is an ALIAS of
// persistence.IsolationPolicy — the single source of truth shared with the gormtx/
// entrepo adapters that honor it — so the contract type and the enforcement type are
// the same. The shared-DB default is schema-preferred (Postgres schema, prefix
// fallback).
type IsolationPolicy = persistence.IsolationPolicy

const (
	// IsolationUnset defers to the composition/host default (schema-preferred).
	IsolationUnset = persistence.IsolationUnset
	// IsolationSchemaRequired demands a Postgres schema per module (fails on
	// prefix-only engines).
	IsolationSchemaRequired = persistence.IsolationSchemaRequired
	// IsolationSchemaPreferred uses a schema where available, else a table prefix.
	// The default.
	IsolationSchemaPreferred = persistence.IsolationSchemaPreferred
	// IsolationPrefixRequired uses a table prefix on every engine.
	IsolationPrefixRequired = persistence.IsolationPrefixRequired
	// IsolationDedicatedRequired demands a separate DB/DSN per module (full fault
	// isolation).
	IsolationDedicatedRequired = persistence.IsolationDedicatedRequired
)

// MigrationsFS is the minimal read-only filesystem a module exposes its
// migration files through (satisfied by embed.FS). The host reads + runs them in
// P2; P1 only carries the handle. It is an alias for the stdlib io/fs.FS shape so
// servicekit adds no dependency.
type MigrationsFS = fsFS

// EventDescriptor declares a module's event surface: the event types it
// publishes and subscribes to, plus its outbox config. The host uses Publishes/
// Subscribes to validate a coherent, conflict-free event graph at boot and (P3)
// to start exactly one relay + consumer per module outbox.
type EventDescriptor struct {
	// Publishes are the globally-unique event type names the module emits.
	Publishes []EventType
	// Subscribes are the event type names the module consumes.
	Subscribes []EventType
	// Outbox describes the module's transactional outbox (P3 host-owned relay).
	Outbox OutboxDescriptor
}

// EventType is a globally-unique event type name (e.g. "orders.order.created").
type EventType string

// OutboxDescriptor describes a module's transactional outbox so the host can own
// its relay + consumer lifecycle in P3 (exactly one per module outbox). P1
// carries the shape only.
type OutboxDescriptor struct {
	// Enabled reports whether the module uses a transactional outbox. When false
	// the host starts no relay/consumer for the module.
	Enabled bool
}

// HealthDescriptor declares a readiness check the module contributes to the
// host's aggregate readiness. The host registers Check on the shared server.
type HealthDescriptor struct {
	// Name is the check's stable identifier (used in the /readyz failure body).
	// It should be module-qualified to avoid collisions (e.g. "orders.db").
	Name string
}

// JobDescriptor declares a supervised background job the module runs (P3). P1
// carries the shape only; host-side supervision + failurePolicy land in P3.
type JobDescriptor struct {
	// Name is the job's stable identifier (module-qualified).
	Name string
}

// Compatibility declares the version ranges a module needs from its environment,
// for composition-time gating (P4/P5). P1 carries the shape only.
type Compatibility struct {
	// SDK is the devedge-sdk version range the module requires (e.g. ">=0.27.0").
	SDK string
	// Go is the Go toolchain version range (e.g. ">=1.25").
	Go string
	// Postgres is the Postgres version range when the module needs Postgres.
	Postgres string
}

// App is the running host's shared services, handed to each [Module.Register].
// It carries the ONE shared [server.Server] (built by [Run] via server.New) plus
// the per-module registry interfaces. The host hands each module a per-module App
// (its registry seams are scoped to that module's ID), so app.Config.Load fills only
// the module's config slice (P3 prefix layering), app.DB.Namespace resolves the
// module's namespace (P2), and the event/job helpers attribute their work to the
// module (P3 host-owned dispatchers + bulkheads).
type App struct {
	// Server is the ONE shared server every module registers on. Its boot-time
	// union completeness gate (server.Serve) validates the combined surface — the
	// host does NOT invent a parallel gate.
	Server *server.Server
	// Config resolves a module's namespaced configuration. P3 scopes it to the
	// module's ConfigDescriptor.Prefix (or its ID); a module's Load fills only its
	// own slice from the host's shared sources.
	Config ConfigProvider
	// DB resolves a module's namespaced persistence handle. P2 implements
	// DatabaseNamespace allocation; a single-module / unshared-DB host yields a
	// zero-qualification namespace (unchanged).
	DB DatabaseRegistry
	// Events registers the module's outbox relay + handlers. P3: the HOST owns one
	// relay + one consumer per module outbox over the shared bus (use
	// [App.RegisterOutboxRelay] + [App.Subscribe], the per-module-attributed helpers).
	Events EventRegistry
	// Health registers the module's readiness checks on the shared server.
	Health HealthRegistry
	// Logger is the host's structured logger; a module derives a child logger
	// scoped to its ID rather than reading global logging config.
	Logger *slog.Logger
	// Metrics registers the module's metrics. P3 scopes per module (a thin
	// per-module-labeled wrapper over the SDK's existing metrics — no new backend).
	Metrics MetricsRegistry

	// moduleID is the ID of the module this App is scoped to (the host hands each
	// module its own App). It keys the per-module helpers below so a module need not
	// repeat its own ID. Set by the host.
	moduleID string
	// host is the back-reference the per-module helpers use to record relay/consumer/
	// job registrations against the host's shared dispatcher + supervisor. Set by Run.
	host *hostState
}

// ModuleID returns the ID of the module this App is scoped to.
func (a *App) ModuleID() string { return a.moduleID }

// Bus returns the host's shared event [events.Bus] (in-process membus by default).
// A module rarely needs it directly — publishing flows through the transactional
// outbox and subscribing flows through [App.Subscribe] — but a module that bridges to
// an external transport can reach the shared bus here. Same binary != direct calls:
// even over this bus, cross-module reactions go through the durable outbox→relay→bus
// pipeline, never a direct handler import.
func (a *App) Bus() events.Bus { return a.host.events.sharedBus() }

// RegisterOutboxRelay declares this module's transactional outbox so the HOST starts
// exactly ONE relay for it (proposal §5.5) — fixing the "dispatchers double-started"
// risk a composed binary would have if each module started its own relay in main().
// The module supplies its NAMESPACED outbox + cursor stores (built from its
// [DatabaseNamespace] per P2); the host owns the relay loop over the shared bus, under
// the module's bulkhead. Call it once from Register; a second call for the same module
// is an error.
func (a *App) RegisterOutboxRelay(cfg OutboxRelayConfig) error {
	return a.host.events.registerRelay(a.moduleID, cfg)
}

// Subscribe registers this module's event handlers so the HOST starts exactly ONE
// consumer for the module over the shared bus (proposal §5.5), in the module's own
// consumer group. The handlers commit through cfg.Tx with cfg.Idem (the exactly-once
// guard). The host runs the consumer under the module's bulkhead. Subscribing to an
// event type with no publisher in the composition is rejected at boot (orphan
// subscriber).
func (a *App) Subscribe(cfg ConsumerConfig, handlers ...EventHandler) error {
	return a.host.events.registerConsumer(a.moduleID, cfg, handlers...)
}

// RegisterBackgroundJob registers a supervised background job the HOST runs for this
// module (proposal §5.9). The host runs fn in the module's bulkhead: a panic or error
// is contained, attributed to the module, and routed through its [FailurePolicy]
// (Degraded marks the module unready; FailHost fails the host fast). fn should run
// until ctx is cancelled (host shutdown). name is module-qualified for logs.
func (a *App) RegisterBackgroundJob(name string, fn func(ctx context.Context) error) error {
	return a.host.registerJob(a.moduleID, name, fn)
}
