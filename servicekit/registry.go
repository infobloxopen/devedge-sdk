package servicekit

import (
	"io/fs"

	"github.com/infobloxopen/devedge-sdk/health"
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

// DatabaseRegistry resolves a module's namespaced persistence handle. The P2
// DatabaseNamespace allocation (schema/prefix, host-run migrations) plugs in
// behind this interface; P1's implementation performs no namespacing.
type DatabaseRegistry interface {
	// Namespace returns the resolved database namespace identity for the module
	// with the given descriptor. P1 returns the module ID with no isolation
	// applied; P2 fills in the schema/prefix/migration-table and runs migrations.
	Namespace(moduleID string, db DatabaseDescriptor) (DatabaseNamespace, error)
}

// DatabaseNamespace is the resolved isolation identity for a module's data — the
// second axis beneath tenant (account_id) scoping. P2 makes the gormtx/entrepo
// repository + outbox/idempotency table naming honor it; P1 only carries the
// resolved value (no enforcement).
type DatabaseNamespace struct {
	// ModuleID is the owning module's ID.
	ModuleID string
	// Engine is the database engine (e.g. "postgres"). Empty in P1.
	Engine string
	// Schema is the Postgres schema the module's tables live in (e.g. "orders").
	// Empty when prefix isolation is used or in P1.
	Schema string
	// TablePrefix is the table-name prefix for prefix isolation (e.g. "ord_").
	// Empty when schema isolation is used or in P1.
	TablePrefix string
	// MigrationTable is the module's own migration-state table name. Empty in P1.
	MigrationTable string
	// Role is the DB role the module connects as (for per-module grants). Empty
	// in P1.
	Role string
}

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
