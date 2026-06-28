package persistence

import (
	"fmt"
	"strings"
)

// DatabaseNamespace is the resolved database isolation identity for one composable
// MODULE's data (WS-012 P2) — the SECOND axis of isolation, beneath the existing
// tenant (account_id) axis. The two axes coexist and are orthogonal:
//
//   - MODULE isolation (this type): two co-resident modules sharing one database
//     must not collide on table names. orders.* lives in one schema/prefix,
//     billing.* in another — including the FRAMEWORK tables (outbox,
//     idempotency_markers, the dispatcher cursor sidecar, and any migration-state
//     table), which are otherwise unqualified and WOULD collide.
//   - TENANT isolation (account_id): every row stays scoped to its tenant inside
//     the module's namespace, exactly as before. Namespacing does not replace it.
//
// It is defined in the persistence package (not servicekit) because the gormtx /
// entrepo adapters that must HONOR it import persistence, never servicekit;
// servicekit aliases this type so the contract has one source of truth.
//
// A zero DatabaseNamespace (or one with neither Schema nor TablePrefix set) is the
// SINGLE-MODULE / unshared-DB case: no qualification is applied and behavior is
// identical to a non-composable service. Qualification only kicks in when the host
// allocates a Schema (Postgres) or a TablePrefix (prefix-only engines) for a module
// that shares a database with another.
type DatabaseNamespace struct {
	// ModuleID is the owning module's ID (e.g. "orders") — the stable namespacing
	// key (the proto-package first segment). It seeds the default Schema/TablePrefix
	// and the per-module advisory-lock key the host uses to serialize migrations.
	ModuleID string
	// Engine is the database engine the namespace was resolved for (e.g.
	// "postgres", "sqlite"). It records which side of the schema-vs-prefix decision
	// the resolver took.
	Engine string
	// Schema is the Postgres schema the module's tables live in (e.g. "orders").
	// When set, every table (domain AND framework) is reached via this schema —
	// the host sets the connection search_path to it, so AutoMigrate and queries
	// resolve into it uniformly. Empty when prefix isolation is used.
	Schema string
	// TablePrefix is the table-name prefix for prefix isolation on engines without
	// schemas (e.g. "ord_"). When set, domain tables are prefixed via the ORM
	// naming strategy and the framework stores prefix their (otherwise hard-coded)
	// table names. Empty when schema isolation is used.
	TablePrefix string
	// MigrationTable is the module's OWN migration-state table name (e.g.
	// "orders_schema_migrations" or, under schema isolation, "schema_migrations"
	// inside the module schema). Each module records its migration state in its own
	// table so two modules never share or clobber one schema_migrations table.
	MigrationTable string
	// Role is the DB role the module connects as, for per-module grants under a
	// dedicated-required posture. Empty for the shared-pool default.
	Role string
}

// IsZero reports whether the namespace applies NO qualification — the
// single-module / unshared-DB case where tables are reached unqualified, exactly
// as a non-composable service. A namespace with only ModuleID set (no Schema, no
// TablePrefix) is still zero for qualification purposes.
func (n DatabaseNamespace) IsZero() bool {
	return n.Schema == "" && n.TablePrefix == ""
}

// QualifyTable returns the namespaced name for a base (unqualified) table name:
//   - schema isolation:  "schema"."base" (caller-quoted forms are left intact)
//   - prefix isolation:  prefix + base
//   - no isolation:      base unchanged
//
// It is the single qualification rule the gormtx framework stores (outbox,
// idempotency, cursor, dead-letter) apply to their otherwise hard-coded table
// names so two co-resident modules do not collide. Domain tables are qualified by
// the ORM (search_path for schema, naming-strategy prefix for prefix) and do not
// go through this helper.
func (n DatabaseNamespace) QualifyTable(base string) string {
	switch {
	case n.Schema != "":
		return n.Schema + "." + base
	case n.TablePrefix != "":
		return n.TablePrefix + base
	default:
		return base
	}
}

// IsolationPolicy is the database module-namespacing policy a module/composition
// declares (WS-012 §5.4). It selects how the host resolves a [DatabaseNamespace]
// for a module given the engine.
type IsolationPolicy string

const (
	// IsolationUnset defers to the composition/host default ([IsolationSchemaPreferred]).
	IsolationUnset IsolationPolicy = ""
	// IsolationSchemaRequired demands a Postgres schema per module; it FAILS on a
	// prefix-only engine (e.g. SQLite) rather than silently degrading.
	IsolationSchemaRequired IsolationPolicy = "schema-required"
	// IsolationSchemaPreferred (the default) uses a Postgres schema where the engine
	// supports one, else falls back to a table prefix. Single shared DB + per-module
	// schema is the cheap, common case.
	IsolationSchemaPreferred IsolationPolicy = "schema-preferred"
	// IsolationPrefixRequired uses a table prefix on EVERY engine (including
	// Postgres), for deployments that want one schema with prefixed tables.
	IsolationPrefixRequired IsolationPolicy = "prefix-required"
	// IsolationDedicatedRequired demands a separate database/DSN per module (full
	// fault isolation). The host wires a distinct pool per module; within that pool
	// no schema/prefix qualification is needed.
	IsolationDedicatedRequired IsolationPolicy = "dedicated-required"
)

// engineSupportsSchema reports whether an engine has SQL schemas (so schema
// isolation is available). Postgres does; SQLite/MySQL do not in the sense this
// SDK uses (MySQL "schema" == database, handled by dedicated-required).
func engineSupportsSchema(engine string) bool {
	return engine == "postgres"
}

// ResolveNamespace applies an [IsolationPolicy] to (moduleID, engine, preferred
// schema/prefix) and returns the resolved [DatabaseNamespace] the adapters honor.
// It is the host-side allocation rule (WS-012 §5.4 table); the caller (the
// servicekit DatabaseRegistry) supplies the engine and any module-declared
// overrides. preferredSchema/preferredPrefix default to forms of moduleID.
//
// It returns an error for the impossible combinations (schema-required on an
// engine without schemas), so a composition fails fast at boot rather than
// silently co-mingling two modules' tables.
func ResolveNamespace(policy IsolationPolicy, moduleID, engine, preferredSchema, preferredPrefix string) (DatabaseNamespace, error) {
	if strings.TrimSpace(moduleID) == "" {
		return DatabaseNamespace{}, fmt.Errorf("persistence: ResolveNamespace requires a module ID")
	}
	if policy == IsolationUnset {
		policy = IsolationSchemaPreferred
	}
	schema := preferredSchema
	if schema == "" {
		schema = sanitizeIdent(moduleID)
	}
	prefix := preferredPrefix
	if prefix == "" {
		prefix = sanitizeIdent(moduleID) + "_"
	}

	ns := DatabaseNamespace{ModuleID: moduleID, Engine: engine}
	switch policy {
	case IsolationSchemaRequired:
		if !engineSupportsSchema(engine) {
			return DatabaseNamespace{}, fmt.Errorf("persistence: isolation %q requires schema support but engine %q has none", policy, engine)
		}
		ns.Schema = schema
	case IsolationSchemaPreferred:
		if engineSupportsSchema(engine) {
			ns.Schema = schema
		} else {
			ns.TablePrefix = prefix
		}
	case IsolationPrefixRequired:
		ns.TablePrefix = prefix
	case IsolationDedicatedRequired:
		// A dedicated DB/DSN per module needs no in-DB qualification: the pool
		// itself is the boundary. The host is responsible for wiring the distinct
		// pool; here we record the identity with no schema/prefix.
	default:
		return DatabaseNamespace{}, fmt.Errorf("persistence: unknown isolation policy %q", policy)
	}
	ns.MigrationTable = migrationTableFor(ns)
	return ns, nil
}

// NamespacedPostgresDSN appends the module's schema to a Postgres DSN's search_path
// so every pooled connection on the resulting handle resolves unqualified table
// names INTO the module schema — the pool-safe way to scope schema isolation (no
// per-connection SET a pooled connection could miss). It is engine-level (a DSN
// string rule), so BOTH the gormtx and entrepo adapters use it: a gorm.DB or ent
// client opened on the returned DSN places its tables in the module schema, and the
// framework stores' bare table names resolve there too.
//
// It supports the URL form ("postgres://...?search_path=x") and the keyword form
// ("host=... search_path=x"), and always includes "public" as a fallback so shared
// extensions resolve. When ns has no Schema it returns dsn unchanged (prefix /
// dedicated isolation need no search_path).
func NamespacedPostgresDSN(dsn string, ns DatabaseNamespace) string {
	if ns.Schema == "" {
		return dsn
	}
	sp := ns.Schema + ",public"
	if isURLDSN(dsn) {
		sep := "?"
		if hasQuery(dsn) {
			sep = "&"
		}
		return dsn + sep + "search_path=" + sp
	}
	return dsn + fmt.Sprintf(" search_path=%s", sp)
}

func isURLDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func hasQuery(dsn string) bool {
	return strings.ContainsRune(dsn, '?')
}

// migrationTableFor returns the module's own migration-state table name. Under
// schema isolation the table lives INSIDE the module schema so a bare name is
// already module-isolated; under prefix isolation the prefix makes it unique.
func migrationTableFor(ns DatabaseNamespace) string {
	const base = "schema_migrations"
	switch {
	case ns.Schema != "":
		return base // qualified by the search_path / schema
	case ns.TablePrefix != "":
		return ns.TablePrefix + base
	default:
		return base
	}
}

// sanitizeIdent lowercases an identifier and replaces any character that is not a
// SQL-safe identifier byte with '_', so a proto-package-derived module ID maps to
// a valid schema/prefix. Leading digits are prefixed with 'm' so the result is a
// legal identifier.
func sanitizeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('m')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "m"
	}
	return b.String()
}
