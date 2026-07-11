// idempotency_perf.go — WS-043 / F048 Increment 3, Deliverable D: keep the hot
// `idempotency_keys` table fast on PostgreSQL.
//
// Three engine-level (PostgreSQL) knobs, ALL idempotent and applied through the HOST
// migration path (gormtx.MigrateModule), NEVER in the Atlas-generated 0001 baseline
// (the drift gate regenerates 0001 from the GORM models and would fail on hand-added
// storage params). They are a no-op on SQLite/other so the dev backend is unaffected:
//
//   1. TuneIdempotencyKeys — ALTER TABLE ... SET (fillfactor + aggressive autovacuum +
//      toast.*). fillfactor < 100 keeps the per-request Complete UPDATE (status /
//      response_type / response — none indexed) HOT-eligible, so it never bloats the
//      index. Applied to the plain table, or to each leaf when the table is partitioned.
//   2. EnsureIdempotencyKeysPartitioned — OPT-IN, CREATE-TIME hash partitioning by the
//      FULL primary key (account_id, method, request_id), so per-partition uniqueness ==
//      GLOBAL uniqueness and exactly-once is preserved. NEVER time-partitioned (a
//      within-TTL duplicate straddling a boundary would re-execute). Fails loud if the
//      table already exists non-partitioned (it may hold durable responses — never
//      dropped). Non-opted-in / SQLite services stay on the plain baseline table,
//      byte-for-byte unaffected.
//
// Every identifier interpolated into DDL is validated against a strict identifier
// pattern and double-quoted; the only values that reach a WHERE/bound are integers we
// generate. No caller input is ever concatenated into SQL.
package gormtx

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// MaxIdempotencyPartitions bounds the opt-in hash-partition count so a typo cannot ask
// PostgreSQL to create an absurd number of child tables. 8192 is far past any real need
// (partitioning helps autovacuum/lock contention, not cardinality).
const MaxIdempotencyPartitions = 8192

// defaultGCBatchSize is the chunk size the batched GC deletes per round-trip so a large
// expired backlog never becomes one giant table-locking DELETE.
const defaultGCBatchSize = 1000

// idempotencyStorageParams is the storage/autovacuum tuning shared by the plain-table
// ALTER and the per-partition CREATE. fillfactor=80 leaves 20% free space per page so the
// Complete UPDATE stays HOT (no index churn); the aggressive autovacuum thresholds keep a
// hot, high-turnover table (rows live ~TTL then expire) from accumulating dead tuples; the
// toast.* mirror covers any large `response` payloads spilled to TOAST.
var idempotencyStorageParams = []string{
	"fillfactor = 80",
	"autovacuum_enabled = true",
	"autovacuum_vacuum_scale_factor = 0.02",
	"autovacuum_vacuum_threshold = 50",
	"autovacuum_analyze_scale_factor = 0.02",
	"autovacuum_analyze_threshold = 50",
	"autovacuum_vacuum_cost_delay = 0",
	"toast.autovacuum_vacuum_scale_factor = 0.05",
	"toast.autovacuum_vacuum_threshold = 50",
}

// safeIdentRE bounds an unquoted SQL identifier we are willing to double-quote and
// interpolate into DDL. Namespace schema/prefix come from sanitizeIdent (moduleID) and the
// base table names are compile-time constants, so this is defense-in-depth, not the only
// guard.
var safeIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// splitQualified decomposes a QualifyTable result into an optional schema and the table
// name: "schema.base" -> ("schema","base"); "prefix_base" or "base" -> ("","prefix_base").
// It splits on the FIRST '.' only; neither a schema (sanitized ident) nor our base table
// names contain a dot.
func splitQualified(qualified string) (schema, table string) {
	if i := strings.IndexByte(qualified, '.'); i >= 0 {
		return qualified[:i], qualified[i+1:]
	}
	return "", qualified
}

// quoteIdent validates an identifier and returns it double-quoted for PostgreSQL. It
// fails loud on anything outside safeIdentRE so a malformed namespace can never inject SQL.
func quoteIdent(ident string) (string, error) {
	if !safeIdentRE.MatchString(ident) {
		return "", fmt.Errorf("gormtx: refusing to build DDL with unsafe identifier %q", ident)
	}
	return `"` + ident + `"`, nil
}

// quoteQualified validates + double-quotes a (possibly schema-qualified) name into
// `"schema"."table"` or `"table"`.
func quoteQualified(qualified string) (string, error) {
	schema, table := splitQualified(qualified)
	qt, err := quoteIdent(table)
	if err != nil {
		return "", err
	}
	if schema == "" {
		return qt, nil
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return "", err
	}
	return qs + "." + qt, nil
}

// storageParamsClause renders the shared tuning params as a `(a = x, b = y, ...)` body
// for a WITH (...) or SET (...) clause. The values are all fixed constants above.
func storageParamsClause() string {
	return "(" + strings.Join(idempotencyStorageParams, ", ") + ")"
}

// TuneIdempotencyKeys applies the PostgreSQL storage/autovacuum tuning to the module's
// idempotency_keys table (plain or, when partitioned, each leaf partition). It is a
// PG-only, idempotent no-op on any other dialect, so it is safe to call unconditionally
// from the host migration path. It must run AFTER the table exists (AutoMigrate / the
// partition step created it).
func TuneIdempotencyKeys(ctx context.Context, db *gorm.DB, ns persistence.DatabaseNamespace) error {
	if db.Dialector.Name() != "postgres" {
		return nil // SQLite/dev and other engines: no storage-parameter concept.
	}
	qualified := ns.QualifyTable(idempotencyKeysBaseTable)
	schema, table := splitQualified(qualified)

	partitioned, err := isPartitionedTable(ctx, db, schema, table)
	if err != nil {
		return err
	}
	if !partitioned {
		// Plain table: a partitioned table has no storage of its own, but a plain one
		// does — set the params directly. Idempotent: SET overwrites the same values.
		quoted, qerr := quoteQualified(qualified)
		if qerr != nil {
			return qerr
		}
		if err := db.WithContext(ctx).Exec("ALTER TABLE " + quoted + " SET " + storageParamsClause()).Error; err != nil {
			return fmt.Errorf("gormtx: tune idempotency_keys %q: %w", qualified, err)
		}
		return nil
	}
	// Partitioned: storage params live on the leaves. Tune every current leaf; new leaves
	// created by EnsureIdempotencyKeysPartitioned carry the params at CREATE time.
	leaves, err := listPartitions(ctx, db, schema, table)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		quoted, qerr := quoteChildInSchema(schema, leaf)
		if qerr != nil {
			return qerr
		}
		if err := db.WithContext(ctx).Exec("ALTER TABLE " + quoted + " SET " + storageParamsClause()).Error; err != nil {
			return fmt.Errorf("gormtx: tune idempotency_keys partition %q: %w", leaf, err)
		}
	}
	return nil
}

// EnsureIdempotencyKeysPartitioned creates the module's idempotency_keys table as a
// PostgreSQL HASH-partitioned table over the FULL primary key (account_id, method,
// request_id) with n partitions, each storage-tuned. It is the OPT-IN, PG-only,
// CREATE-TIME performance path (DD-3).
//
// INVARIANT: the partition key is the whole primary key, so a row's (account_id, method,
// request_id) determines its partition and per-partition uniqueness == GLOBAL uniqueness —
// ON CONFLICT (account_id, method, request_id) DO NOTHING and the expired-row reclaim
// UPDATE route to exactly one partition and exactly-once is preserved. HASH (not time):
// a within-TTL duplicate always hashes to the same partition, so it can never straddle a
// boundary and re-execute.
//
// It is create-time only and self-protecting:
//   - table absent            -> CREATE partitioned parent + n leaves + expires_at index.
//   - table present, HASH-part -> idempotent: ensure the n leaves exist (no data touched).
//   - table present, PLAIN     -> FAIL LOUD (never dropped: it may hold durable responses).
//
// It errors on a non-Postgres dialect (the caller only invokes it when partitioning is
// explicitly opted in; SQLite/dev stays on the plain table via AutoMigrate).
func EnsureIdempotencyKeysPartitioned(ctx context.Context, db *gorm.DB, ns persistence.DatabaseNamespace, n int) error {
	if db.Dialector.Name() != "postgres" {
		return fmt.Errorf("gormtx: idempotency_keys hash partitioning is PostgreSQL-only (dialect %q); leave PartitionCount=0 for other engines", db.Dialector.Name())
	}
	if n <= 0 {
		return fmt.Errorf("gormtx: EnsureIdempotencyKeysPartitioned needs n > 0 (got %d)", n)
	}
	if n > MaxIdempotencyPartitions {
		return fmt.Errorf("gormtx: idempotency_keys partition count %d exceeds the max %d", n, MaxIdempotencyPartitions)
	}
	if ns.TablePrefix != "" {
		// Prefix isolation would need the gorm-generated prefixed index name threaded
		// through the raw DDL (like the outbox MySQL-prefix limitation). Schema / no
		// isolation is the supported partitioning posture.
		return fmt.Errorf("gormtx: prefix-namespaced idempotency_keys partitioning is not supported (use schema isolation or a dedicated database); module %q", ns.ModuleID)
	}
	qualified := ns.QualifyTable(idempotencyKeysBaseTable)
	schema, table := splitQualified(qualified)
	quotedParent, err := quoteQualified(qualified)
	if err != nil {
		return err
	}

	exists, err := tableExists(ctx, db, qualified)
	if err != nil {
		return err
	}
	if exists {
		partitioned, perr := isPartitionedTable(ctx, db, schema, table)
		if perr != nil {
			return perr
		}
		if !partitioned {
			return fmt.Errorf("gormtx: cannot enable hash partitioning: table %q already exists NON-partitioned "+
				"(it may hold durable idempotency responses and is never dropped) — enable partitioning only on a fresh database", qualified)
		}
		// Already partitioned: idempotently ensure the n leaves exist.
		return ensureHashPartitions(ctx, db, schema, table, quotedParent, n)
	}

	// Fresh: create the partitioned parent, its propagated expires_at index, then the leaves.
	if schema != "" {
		qs, qerr := quoteIdent(schema)
		if qerr != nil {
			return qerr
		}
		if err := db.WithContext(ctx).Exec("CREATE SCHEMA IF NOT EXISTS " + qs).Error; err != nil {
			return fmt.Errorf("gormtx: create schema for partitioned idempotency_keys: %w", err)
		}
	}
	parentDDL := "CREATE TABLE IF NOT EXISTS " + quotedParent + ` (
	account_id    varchar(255) NOT NULL,
	method        varchar(255) NOT NULL,
	request_id    varchar(255) NOT NULL,
	status        varchar(16)  NOT NULL,
	response_type varchar(255),
	response      bytea,
	fingerprint   varchar(64),
	created_at    timestamptz  NOT NULL,
	expires_at    timestamptz  NOT NULL,
	PRIMARY KEY (account_id, method, request_id)
) PARTITION BY HASH (account_id, method, request_id)`
	if err := db.WithContext(ctx).Exec(parentDDL).Error; err != nil {
		return fmt.Errorf("gormtx: create partitioned idempotency_keys parent %q: %w", qualified, err)
	}
	// A partitioned index on the parent propagates to every (current and future) leaf.
	// Match the gorm-generated index name so AutoMigrate finds it and does not add a twin.
	idxName := "idx_" + idempotencyKeysBaseTable + "_expires_at"
	qIdx, err := quoteIdent(idxName)
	if err != nil {
		return err
	}
	if err := db.WithContext(ctx).Exec("CREATE INDEX IF NOT EXISTS " + qIdx + " ON " + quotedParent + " (expires_at)").Error; err != nil {
		return fmt.Errorf("gormtx: create expires_at index on partitioned idempotency_keys: %w", err)
	}
	return ensureHashPartitions(ctx, db, schema, table, quotedParent, n)
}

// ensureHashPartitions creates (idempotently) the n hash-partition leaves of the parent,
// each storage-tuned. Leaf i covers FOR VALUES WITH (MODULUS n, REMAINDER i). The leaf name
// is the base table + "_p<i>" (integer only) so nothing from the caller reaches the SQL.
func ensureHashPartitions(ctx context.Context, db *gorm.DB, schema, table, quotedParent string, n int) error {
	for i := 0; i < n; i++ {
		leaf := fmt.Sprintf("%s_p%d", table, i)
		quotedLeaf, err := quoteChildInSchema(schema, leaf)
		if err != nil {
			return err
		}
		ddl := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES WITH (MODULUS %d, REMAINDER %d) WITH %s",
			quotedLeaf, quotedParent, n, i, storageParamsClause(),
		)
		if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
			return fmt.Errorf("gormtx: create idempotency_keys partition %s: %w", leaf, err)
		}
	}
	return nil
}

// quoteChildInSchema quotes a child/leaf table name in the (optional) parent schema.
func quoteChildInSchema(schema, child string) (string, error) {
	qc, err := quoteIdent(child)
	if err != nil {
		return "", err
	}
	if schema == "" {
		return qc, nil
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return "", err
	}
	return qs + "." + qc, nil
}

// isPartitionedTable reports whether (schema, table) is a PostgreSQL partitioned table
// (relkind 'p'). schema "" means search_path / public.
func isPartitionedTable(ctx context.Context, db *gorm.DB, schema, table string) (bool, error) {
	var relkind string
	q := `SELECT c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	      WHERE c.relname = ? AND (? = '' OR n.nspname = ?)
	      ORDER BY (n.nspname = current_schema()) DESC LIMIT 1`
	err := db.WithContext(ctx).Raw(q, table, schema, schema).Scan(&relkind).Error
	if err != nil {
		return false, fmt.Errorf("gormtx: inspect idempotency_keys relkind: %w", err)
	}
	return relkind == "p", nil
}

// listPartitions returns the leaf partition relnames of the (schema, table) partitioned
// parent, discovered from the inheritance catalog.
func listPartitions(ctx context.Context, db *gorm.DB, schema, table string) ([]string, error) {
	var children []string
	q := `SELECT c.relname
	      FROM pg_inherits i
	      JOIN pg_class c     ON c.oid = i.inhrelid
	      JOIN pg_class p     ON p.oid = i.inhparent
	      JOIN pg_namespace n ON n.oid = p.relnamespace
	      WHERE p.relname = ? AND (? = '' OR n.nspname = ?)`
	if err := db.WithContext(ctx).Raw(q, table, schema, schema).Scan(&children).Error; err != nil {
		return nil, fmt.Errorf("gormtx: list idempotency_keys partitions: %w", err)
	}
	return children, nil
}
