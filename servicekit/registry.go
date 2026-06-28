package servicekit

import (
	"io/fs"

	"github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// fsFS is the read-only filesystem shape a module exposes migrations through.
// Aliased so [MigrationsFS] adds no servicekit-specific type and embed.FS
// satisfies it directly.
type fsFS = fs.FS

// The registry INTERFACES below are the per-module seams the host hands each
// module via [App]. P1 defines the contracts and ships minimal implementations
// (see run.go) sufficient for the standalone single-module path:
//   - HealthRegistry is fully functional in P1 (it registers checks on the
//     shared server, which already aggregates readiness).
//   - ConfigProvider, DatabaseRegistry, EventRegistry, and MetricsRegistry are
//     contracts whose per-module behavior (namespacing, prefix scoping,
//     host-owned relays) is P2/P3. P1's implementations are inert but valid, so a
//     single-module host compiles and serves identically to today.

// ConfigProvider hands a module its configuration scoped to its config prefix.
// In P3 the host layers a platform-global config beneath each module's prefixed
// layer; in P1 the provider is host-backed and unscoped.
type ConfigProvider interface {
	// Load populates dst (a pointer to the module's typed config struct) from the
	// resolved configuration. It mirrors config.Load semantics. A module calls it
	// from Register instead of reading raw global env/flags itself.
	Load(dst any) error
}

// DatabaseRegistry resolves a module's namespaced persistence handle (WS-012 P2).
// The host's [DatabaseRegistry] allocates a [DatabaseNamespace] (schema/prefix +
// migration table) per module from the host's engine + the module's
// DatabaseDescriptor, applying the [IsolationPolicy] (schema-preferred by default).
// The module then constructs its namespaced stores/repo from that identity (the
// gormtx With*Namespace options / entrepo NamespacedDSN), and the host runs the
// module's migrations under a per-module advisory lock (see [MigrationRunner]).
type DatabaseRegistry interface {
	// Namespace returns the resolved database namespace identity for the module
	// with the given descriptor. The real registry resolves schema/prefix/migration
	// table from the host engine + policy; a single-module / unshared-DB host yields
	// a zero-qualification namespace (behavior unchanged from a non-composable service).
	Namespace(moduleID string, db DatabaseDescriptor) (DatabaseNamespace, error)
}

// DatabaseNamespace is the resolved isolation identity for a module's data — the
// second axis beneath tenant (account_id) scoping. It is an ALIAS of
// persistence.DatabaseNamespace, the single source of truth honored by the gormtx/
// entrepo repository + outbox/idempotency table naming. A module reads its namespace
// from [App.DB] in Register and passes it to its store constructors.
type DatabaseNamespace = persistence.DatabaseNamespace

// EventRegistry registers a module's outbox relay and event handlers with the
// host. In P3 the host starts exactly one relay + one consumer per module outbox
// (each namespaced, each with its own cursor); P1's implementation is inert.
type EventRegistry interface {
	// RegisterOutbox declares the module's outbox so the host can own its
	// relay/consumer lifecycle (P3). P1 records nothing actionable.
	RegisterOutbox(moduleID string, d OutboxDescriptor) error
}

// HealthRegistry registers a module's readiness checks. It is fully functional
// in P1: checks land on the shared server's readiness aggregator.
type HealthRegistry interface {
	// Register adds a readiness check the host aggregates into /readyz and the
	// gRPC health status. name should be module-qualified.
	Register(name string, check health.Check) error
}

// MetricsRegistry registers a module's metrics, scoped per module in P3. P1's
// implementation is inert (the SDK's OTel seam already emits per-RPC RED metrics
// globally without per-module wiring).
type MetricsRegistry interface {
	// Namespace returns a per-module metrics namespace (P3). P1 returns the
	// module ID unchanged.
	Namespace(moduleID string) string
}
