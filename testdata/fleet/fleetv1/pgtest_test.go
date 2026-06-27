package fleetv1_test

// pgtest_test.go — a testcontainers-backed PostgreSQL harness for the fleet
// fixture (Phase 2). It proves the aggregate / tx / cascade / CAS machinery on
// the REAL production target (Postgres), not just SQLite.
//
// Why Postgres matters here: SQLite serializes writes, so the Phase-1 If-Match
// CAS is only *functionally* exercised on SQLite — two concurrent aggregate Saves
// can never truly overlap. On Postgres under READ COMMITTED, two Saves that both
// read the same root etag and then write would, WITHOUT the CAS, both succeed (a
// classic lost update). With the CAS (UPDATE ... WHERE etag = <If-Match>) exactly
// one wins. postgres_test.go exercises that race.
//
// Docker-optional contract: startPostgres calls t.Skip("docker unavailable: ...")
// cleanly when testcontainers / Docker cannot start, so `go test ./...` on a
// machine WITHOUT Docker still passes (the PG tests skip, they do not fail). When
// Docker IS available the PG tests run for real.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	nat "github.com/moby/moby/api/types/network"

	_ "github.com/lib/pq" // registers the "postgres" database/sql driver for ent.Open

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

// sharedPGContainer is started once per test binary and reused across PG tests
// (container start is the slow part; per-test isolation comes from a fresh,
// uniquely named database carved out of the one server).
var (
	pgOnce      sync.Once
	pgContainer *tcpostgres.PostgresContainer
	pgBaseDSN   string
	pgStartErr  error
)

// TestMain terminates the shared postgres container after the suite so the
// container does not LEAK. testcontainers' Ryuk reaper normally cleans up on
// session end, but the documented local workflow runs with
// TESTCONTAINERS_RYUK_DISABLED=true (Rancher Desktop) — with Ryuk off, nothing
// reaps the container and each `go test` run would otherwise leave a postgres:16
// container running. Terminating it here makes cleanup independent of Ryuk; it is
// a no-op when Docker was unavailable (the container was never started).
func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = pgContainer.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

// startPostgres starts (once) a postgres:16 container via testcontainers and
// returns a base DSN pointing at it. If Docker/testcontainers cannot start, it
// SKIPS the calling test rather than failing — so the suite is green on machines
// without Docker. In CI (ubuntu-latest has Docker) the container starts and the
// PG tests run for real.
func startPostgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("fleet"),
			tcpostgres.WithUsername("fleet"),
			tcpostgres.WithPassword("fleet"),
			// Wait on the log AND a real SQL connection: postgres logs "ready" once
			// during its init bootstrap and again after the final restart, so we
			// require the log twice; then ForSQL confirms the mapped host port is
			// actually accepting connections before any test queries it.
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2).
						WithStartupTimeout(60*time.Second),
					wait.ForSQL("5432/tcp", "postgres", func(host string, port nat.Port) string {
						return fmt.Sprintf("host=%s port=%s user=fleet password=fleet dbname=fleet sslmode=disable",
							host, port.Port())
					}).WithStartupTimeout(60*time.Second),
				),
			),
		)
		if err != nil {
			pgStartErr = err
			return
		}
		dsn, err := c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgStartErr = err
			_ = c.Terminate(ctx)
			return
		}
		pgContainer = c
		pgBaseDSN = dsn
	})
	if pgStartErr != nil {
		t.Skipf("docker unavailable: %v", pgStartErr)
	}
	return pgBaseDSN
}

// freshPGDatabase creates a brand-new, uniquely named database on the shared
// container and returns a DSN for it, so each test starts from an empty schema
// without tearing the container down. The name is derived from t.Name().
func freshPGDatabase(t *testing.T, baseDSN string) string {
	t.Helper()
	dbName := pgDBName(t.Name())
	admin, err := sql.Open("postgres", baseDSN)
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	defer admin.Close()
	// DROP first so a re-run (or a name collision) starts clean.
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
		t.Fatalf("drop db %s: %v", dbName, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	return replaceDBName(baseDSN, dbName)
}

// openFleetEntPG opens an ent client on a fresh Postgres database and migrates
// the fleet schema (ent.Open("postgres", dsn) + Schema.Create — the PG twin of
// enttest.Open on sqlite3). Postgres enforces foreign keys natively, so the
// Fleet→Vehicle ON DELETE CASCADE works with no per-connection pragma.
func openFleetEntPG(t *testing.T) *ent.Client {
	t.Helper()
	dsn := freshPGDatabase(t, startPostgres(t))
	client, err := ent.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("ent.Open postgres: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("ent migrate (Schema.Create): %v", err)
	}
	return client
}

// openFleetGormPG opens a GORM client on a fresh Postgres database and
// AutoMigrates the fleet models (gorm.Open(postgres.Open(dsn)) + AutoMigrate —
// the PG twin of openFleetGormDBFK on sqlite). The FleetModel declares its
// Vehicles relation with `constraint:OnDelete:CASCADE`, so AutoMigrate emits the
// real Fleet→Vehicle ON DELETE CASCADE foreign key; Postgres enforces it natively
// (no per-connection pragma, unlike SQLite).
func openFleetGormPG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := freshPGDatabase(t, startPostgres(t))
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open postgres: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&fleetv1.FleetModel{}, &fleetv1.VehicleModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// pgDBName turns a test name into a safe, lowercase Postgres identifier.
func pgDBName(testName string) string {
	out := make([]rune, 0, len(testName)+4)
	out = append(out, []rune("tc_")...)
	for _, r := range testName {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	// Postgres identifiers cap at 63 bytes.
	if len(out) > 60 {
		out = out[:60]
	}
	return string(out)
}

// replaceDBName swaps the path (database name) component of a postgres:// DSN.
func replaceDBName(dsn, dbName string) string {
	// testcontainers ConnectionString returns postgres://user:pass@host:port/db?opts
	scheme := "postgres://"
	rest := dsn[len(scheme):]
	slash := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			slash = i
			break
		}
	}
	hostPart := rest[:slash]
	query := ""
	tail := rest[slash+1:]
	for i := 0; i < len(tail); i++ {
		if tail[i] == '?' {
			query = tail[i:]
			break
		}
	}
	return fmt.Sprintf("%s%s/%s%s", scheme, hostPart, dbName, query)
}
