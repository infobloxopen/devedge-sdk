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
// module via [App]. They are now all backed by real per-module implementations:
//   - HealthRegistry registers checks on the shared server (readiness aggregation);
//   - DatabaseRegistry allocates a per-module schema/prefix namespace (P2);
//   - ConfigProvider scopes config to the module's prefix (P3 layering);
//   - EventRegistry declares a module's outbox so the host owns one relay + one
//     consumer per module over the shared bus (P3) — the richer per-module
//     registration (namespaced stores, handlers) flows through the [App] helpers
//     [App.RegisterOutboxRelay] / [App.Subscribe], keeping this interface minimal;
//   - MetricsRegistry hands a module a metric-safe per-module namespace (P3).

// ConfigProvider hands a module its configuration scoped to its config prefix.
// The host layers a platform-global config (host-owned) beside each module's
// prefixed layer (P3); a module's Load fills only its own slice.
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

// EventRegistry declares a module's outbox to the host (P3). The host starts exactly
// one relay + one consumer per module outbox (each namespaced, each with its own
// cursor) over the one shared [events.Bus]. The descriptor-only RegisterOutbox keeps
// this interface minimal; the namespaced stores + handlers a module supplies flow
// through the [App] helpers [App.RegisterOutboxRelay] and [App.Subscribe], which
// attribute them to the registering module.
type EventRegistry interface {
	// RegisterOutbox declares the module's outbox so the host owns its relay/
	// consumer lifecycle (P3). A disabled outbox (Enabled=false) starts no relay.
	RegisterOutbox(moduleID string, d OutboxDescriptor) error
}

// HealthRegistry registers a module's readiness checks. It is fully functional
// in P1: checks land on the shared server's readiness aggregator.
type HealthRegistry interface {
	// Register adds a readiness check the host aggregates into /readyz and the
	// gRPC health status. name should be module-qualified.
	Register(name string, check health.Check) error
}

// MetricsRegistry hands a module a metric-safe per-module namespace (P3) — a thin
// per-module label over the SDK's existing OTel RED metrics (no new backend).
type MetricsRegistry interface {
	// Namespace returns a metric-safe per-module namespace token derived from the
	// module ID, so a module's own metrics are distinguishable per module.
	Namespace(moduleID string) string
}
