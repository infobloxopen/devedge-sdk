package migrate

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	migratelib "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

const (
	// defaultLockTimeout / defaultStatementTimeout are the SAFE migration-connection
	// defaults (F043 G-3/D-4): a contended migration fails fast instead of queueing
	// behind live queries. Overridable per [Config].
	defaultLockTimeout      = 2 * time.Second
	defaultStatementTimeout = 60 * time.Second
	// defaultMigrateTimeout bounds a single Apply when the caller's context carries no
	// deadline (parity with devedge's ForkApplier backstop).
	defaultMigrateTimeout = 5 * time.Minute
)

// migrationFileRE matches a golang-migrate file name: <version>_<desc>.(up|down).sql.
// The applier applies files in strict numeric order and rejects a malformed name.
var migrationFileRE = regexp.MustCompile(`^[0-9]+_.+\.(up|down)\.sql$`)

// Config configures one module's versioned-SQL migration run.
type Config struct {
	// DSN is the module's Postgres connection string (postgres://…). The applier
	// normalizes it to the pgx/v5 scheme and derives the SAFE migration connection.
	DSN string
	// Namespace is the module's resolved isolation identity. Its Schema sets the
	// connection search_path (so schema_migrations lands per-module) and its ModuleID
	// keys the single advisory lock.
	Namespace persistence.DatabaseNamespace
	// FrameworkBaseline is the SDK-owned migrations FS composed AHEAD of the module FS
	// (the 0001_framework_init baseline; see [FrameworkBaseline]). nil skips it (the
	// module owns 0001 itself).
	FrameworkBaseline fs.FS
	// ModuleMigrations is the module's embedded migrations FS (its 0002+ files). nil
	// applies the framework baseline alone.
	ModuleMigrations fs.FS
	// LockTimeout / StatementTimeout bound the MIGRATION connection (not the advisory
	// lock, which blocks so replicas serialize). Zero uses the safe defaults.
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	// DownStoreDir persists applied up/down files so a rollback survives the running
	// image's source tree changing, and dirty-state recovers on a corrected re-run.
	// Empty defaults to a per-module directory under os.TempDir(); production deploys
	// point it at a persistent volume (that hardening is a later WS-022 phase).
	DownStoreDir string
	// Logger is the applier's structured logger; nil uses slog.Default().
	Logger *slog.Logger
}

// Result reports the outcome of an [Apply] call.
type Result struct {
	FromVersion    uint
	ToVersion      uint
	Applied        int
	AlreadyCurrent bool
}

// Apply brings the module's Postgres schema to the highest version declared by the
// composed migrations (framework baseline AHEAD of the module FS) through the
// infobloxopen/migrate fork. It is idempotent, recovers a dirty database left by a prior
// failed run (WithDirtyStateConfig), and serializes concurrent hosts/replicas on the
// single SDK advisory lock (persistence.ModuleMigrationLockKey) — never migrate's own
// lock (F043 D-6). The migration connection carries lock_timeout + statement_timeout and
// a per-module search_path so schema_migrations lands in the module schema (D-4/D-6).
func Apply(ctx context.Context, cfg Config) (Result, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return Result{}, errors.New("migrate: a database DSN is required")
	}
	if cfg.Namespace.ModuleID == "" {
		return Result{}, errors.New("migrate: a module ID is required (the advisory-lock authority)")
	}
	if cfg.FrameworkBaseline == nil && cfg.ModuleMigrations == nil {
		return Result{}, errors.New("migrate: nothing to apply (framework baseline and module FS both nil)")
	}
	lockTimeout := cfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = defaultLockTimeout
	}
	stmtTimeout := cfg.StatementTimeout
	if stmtTimeout <= 0 {
		stmtTimeout = defaultStatementTimeout
	}

	store := cfg.DownStoreDir
	if store == "" {
		store = filepath.Join(os.TempDir(), "devedge-migrate", sanitizeDir(cfg.Namespace.ModuleID))
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		return Result{}, fmt.Errorf("migrate: prepare down-store %s: %w", store, err)
	}

	// Materialize the composed FS (framework baseline AHEAD of the module FS) into the
	// persisted down-store dir, in strict numeric order; a duplicate version fails loud.
	// The dir accumulates versions across runs so a persisted down step survives the
	// current source no longer shipping it.
	target, err := materialize(store, cfg.FrameworkBaseline, cfg.ModuleMigrations)
	if err != nil {
		return Result{}, err
	}

	// (1) advisory lock — the SINGLE authority (blocking, so a second replica WAITS and
	// then finds the schema already current rather than racing to CREATE tables). Held
	// on a pinned session connection with no lock_timeout.
	unlock, err := acquireModuleLock(ctx, cfg.DSN, cfg.Namespace.ModuleID)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	// (2) ensure the module schema exists before migrate resolves search_path; the
	// advisory lock above serializes the CREATE across replicas.
	if cfg.Namespace.Schema != "" {
		if err := ensureSchema(ctx, cfg.DSN, cfg.Namespace.Schema); err != nil {
			return Result{}, err
		}
	}

	// (3) SAFE migration connection URL: pgx5 scheme + options carrying search_path
	// (per-module schema_migrations) + lock_timeout + statement_timeout.
	pgxURL, err := migrationURL(cfg.DSN, cfg.Namespace.Schema, lockTimeout, stmtTimeout)
	if err != nil {
		return Result{}, err
	}

	// (4) open the engine with migrate's OWN advisory lock DISABLED (lockless wrapper);
	// the SDK lock in (1) is the single authority (D-6).
	drv, err := database.Open(pgxURL)
	if err != nil {
		return Result{}, fmt.Errorf("migrate: open database: %w", err)
	}
	m, err := migratelib.NewWithDatabaseInstance("file://"+filepath.ToSlash(store), "pgx5", locklessDriver{Driver: drv})
	if err != nil {
		return Result{}, fmt.Errorf("migrate: open migrator: %w", err)
	}
	defer m.Close()
	// Dirty-state recovery + persisted down-store, both rooted at the store dir.
	if err := m.WithDirtyStateConfig(store, store, true); err != nil {
		return Result{}, fmt.Errorf("migrate: enable dirty-state recovery: %w", err)
	}

	from, dirty := versionAndDirty(m)
	if from == target && !dirty {
		log.Info("migrate: already current", "module", cfg.Namespace.ModuleID, "version", from)
		return Result{FromVersion: from, ToVersion: from, AlreadyCurrent: true}, nil
	}

	log.Info("migrate: applying", "module", cfg.Namespace.ModuleID, "from", from, "target", target, "dirty", dirty, "store", store)
	// Drive the schema to target. When the DB is dirty, the fork's dirty-state recovery
	// (handleDirtyState) CLEANS it back to the last successful version within a Migrate
	// call, but plans that call's steps from the version read BEFORE recovery — so a
	// single Migrate may only clean the dirty state, not migrate forward. Re-run to reach
	// the target; bounded so a genuine mismatch surfaces instead of looping.
	for attempt := 0; ; attempt++ {
		err := runBounded(ctx, m, func() error { return m.Migrate(target) })
		if err != nil && !errors.Is(err, migratelib.ErrNoChange) {
			// A failed apply leaves the DB dirty; the next corrected run recovers it.
			log.Error("migrate: failed", "module", cfg.Namespace.ModuleID, "target", target, "error", err)
			return Result{}, fmt.Errorf("migrate: module %q to v%d: %w", cfg.Namespace.ModuleID, target, err)
		}
		cur, curDirty := versionAndDirty(m)
		if cur == target && !curDirty {
			break
		}
		if attempt >= 2 {
			return Result{}, fmt.Errorf("migrate: module %q did not reach v%d (stuck at v%d, dirty=%v)", cfg.Namespace.ModuleID, target, cur, curDirty)
		}
	}

	to, _ := versionAndDirty(m)
	applied := 0
	if to > from {
		applied = countInRange(store, from, to)
	}
	if to < from {
		log.Info("migrate: rolled back", "module", cfg.Namespace.ModuleID, "from", from, "to", to)
	} else {
		log.Info("migrate: applied", "module", cfg.Namespace.ModuleID, "from", from, "to", to, "count", applied)
	}
	return Result{FromVersion: from, ToVersion: to, Applied: applied}, nil
}

// locklessDriver wraps the pgx/v5 database.Driver and makes Lock/Unlock no-ops, so the
// SDK's per-module advisory lock is the SINGLE serialization authority (F043 D-6) rather
// than migrate's own per-(database,schema,table) lock. All other operations delegate.
type locklessDriver struct{ database.Driver }

func (locklessDriver) Lock() error   { return nil }
func (locklessDriver) Unlock() error { return nil }

// materialize stages the composed migration sets (framework baseline first, then module)
// into the store dir, validating names and returning the highest version. A file whose
// version duplicates one already staged fails loud (the framework baseline owns 0001, so
// module migrations must start at 0002). Files are written additively (overwriting same
// names) so previously-applied versions persist for a down-migration the current source
// no longer ships.
func materialize(store string, sets ...fs.FS) (uint, error) {
	seen := map[uint]string{}
	var maxV uint
	for _, fsys := range sets {
		if fsys == nil {
			continue
		}
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return 0, fmt.Errorf("migrate: read migrations FS: %w", err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if !migrationFileRE.MatchString(name) {
				// A stray .sql with a non-conforming name is a mistake — fail loud;
				// other files (README, .keep) are ignored.
				if strings.HasSuffix(name, ".sql") {
					return 0, fmt.Errorf("migrate: malformed migration file name %q (want NNNN_<desc>.{up,down}.sql)", name)
				}
				continue
			}
			b, err := fs.ReadFile(fsys, name)
			if err != nil {
				return 0, fmt.Errorf("migrate: read %s: %w", name, err)
			}
			if err := os.WriteFile(filepath.Join(store, name), b, 0o644); err != nil {
				return 0, fmt.Errorf("migrate: stage %s: %w", name, err)
			}
			if strings.HasSuffix(name, ".up.sql") {
				v, err := parseVersion(strings.TrimSuffix(name, ".up.sql"))
				if err != nil {
					return 0, err
				}
				if prev, dup := seen[v]; dup {
					return 0, fmt.Errorf("migrate: duplicate migration version %d (%q and %q) — the framework baseline owns 0001; module migrations must start at 0002", v, prev, name)
				}
				seen[v] = name
				if v > maxV {
					maxV = v
				}
			}
		}
	}
	if maxV == 0 {
		return 0, errors.New("migrate: no *.up.sql migrations found in the composed FS")
	}
	return maxV, nil
}

// parseVersion extracts the leading numeric version from a golang-migrate base name
// ("0001_framework_init" -> 1).
func parseVersion(base string) (uint, error) {
	i := strings.IndexByte(base, '_')
	if i <= 0 {
		return 0, fmt.Errorf("migrate: cannot parse version from %q", base)
	}
	n, err := strconv.ParseUint(base[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrate: cannot parse version from %q: %w", base, err)
	}
	return uint(n), nil
}

// countInRange reports how many *.up.sql files in dir have a version in (from, to].
func countInRange(dir string, from, to uint) int {
	matches, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return 0
	}
	n := 0
	for _, f := range matches {
		v, err := parseVersion(strings.TrimSuffix(filepath.Base(f), ".up.sql"))
		if err != nil {
			continue
		}
		if v > from && v <= to {
			n++
		}
	}
	return n
}

// versionAndDirty returns the database's current schema version and dirty flag, treating
// "no migration applied yet" and any read error as (0, false).
func versionAndDirty(m *migratelib.Migrate) (uint, bool) {
	v, dirty, err := m.Version()
	if err != nil {
		return 0, false
	}
	return v, dirty
}

// runBounded runs the migrate operation under a bounded deadline (defaultMigrateTimeout
// when ctx has none). On timeout it asks the engine to stop after the current migration
// (GracefulStop) and waits for it to unwind so the connection is released, then returns a
// clear timeout error (parity with devedge's ForkApplier).
func runBounded(ctx context.Context, m *migratelib.Migrate, run func() error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultMigrateTimeout)
		defer cancel()
	}
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		select {
		case m.GracefulStop <- true:
		default:
		}
		<-done
		return fmt.Errorf("migrate: migration timed out: %w", ctx.Err())
	}
}

// migrationURL normalizes dsn to the pgx/v5 scheme and appends the SAFE migration
// connection options: lock_timeout + statement_timeout (so a contended DDL fails fast)
// and, when a module schema is set, search_path (so schema_migrations + unqualified
// CREATE TABLEs land in the module schema). It targets the MIGRATION connection only.
func migrationURL(dsn, schema string, lockTimeout, stmtTimeout time.Duration) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("migrate: parse DSN: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql", "pgx", "pgx5":
		u.Scheme = "pgx5"
	case "":
		return "", fmt.Errorf("migrate: DSN %q has no scheme (want postgres://…)", dsn)
	default:
		return "", fmt.Errorf("migrate: unsupported DSN scheme %q (want a postgres DSN; MySQL is fail-loud unsupported in P1)", u.Scheme)
	}
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
	}
	opts := []string{
		fmt.Sprintf("-c lock_timeout=%dms", lockTimeout.Milliseconds()),
		fmt.Sprintf("-c statement_timeout=%dms", stmtTimeout.Milliseconds()),
	}
	if schema != "" {
		opts = append(opts, fmt.Sprintf("-c search_path=%s,public", schema))
	}
	joined := strings.Join(opts, " ")
	if existing := q.Get("options"); existing != "" {
		joined = existing + " " + joined
	}
	q.Set("options", joined)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// stdlibDSN normalizes dsn to the standard postgres scheme for sql.Open("pgx", …) — the
// advisory-lock / schema-create connections, which deliberately carry NO lock_timeout so
// pg_advisory_lock blocks (replicas serialize) rather than failing fast.
func stdlibDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("migrate: parse DSN: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql", "pgx", "pgx5":
		u.Scheme = "postgres"
	case "":
		return "", fmt.Errorf("migrate: DSN %q has no scheme (want postgres://…)", dsn)
	default:
		return "", fmt.Errorf("migrate: unsupported DSN scheme %q (want a postgres DSN)", u.Scheme)
	}
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// acquireModuleLock takes the single per-module Postgres advisory lock on a pinned
// session connection (blocking, so a second replica waits), returning an unlock func
// that releases it and closes the connection.
func acquireModuleLock(ctx context.Context, dsn, moduleID string) (func(), error) {
	connStr, err := stdlibDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := stdsql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("migrate: open lock connection: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: pin lock session: %w", err)
	}
	key := persistence.ModuleMigrationLockKey(moduleID)
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("migrate: pg_advisory_lock for module %q: %w", moduleID, err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		_ = conn.Close()
		_ = db.Close()
	}, nil
}

// ensureSchema creates the module's Postgres schema if it does not exist, so migrate can
// resolve search_path and schema_migrations lands per-module. Serialized by the caller's
// advisory lock.
func ensureSchema(ctx context.Context, dsn, schema string) error {
	connStr, err := stdlibDSN(dsn)
	if err != nil {
		return err
	}
	db, err := stdsql.Open("pgx", connStr)
	if err != nil {
		return fmt.Errorf("migrate: open schema connection: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
		return fmt.Errorf("migrate: create schema %q: %w", schema, err)
	}
	return nil
}

// sanitizeDir maps a module ID to a filesystem-safe default down-store subdirectory.
func sanitizeDir(moduleID string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, moduleID)
	if s == "" {
		return "module"
	}
	return s
}
