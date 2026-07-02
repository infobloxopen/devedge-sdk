package migrate_test

// pgtest_test.go — a testcontainers-backed PostgreSQL harness for the versioned-SQL
// migration engine (F043 / WS-022 P1). It proves the engine on REAL Postgres (the
// production target), where the per-module schema, advisory lock, dirty-state recovery,
// persisted down-store, and CONCURRENTLY-outside-a-transaction behavior are the genuine
// engine-level things — not a SQLite approximation.
//
// Docker-optional contract (mirrors testdata/iam/iamv1/pgtest_test.go): startPostgres
// SKIPS the calling test cleanly when Docker/testcontainers cannot start, so a machine
// without Docker still passes; CI (ubuntu-latest) runs them for real and asserts the
// TestPG_ tests did not skip.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for verification queries

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	pgOnce      sync.Once
	pgContainer *tcpostgres.PostgresContainer
	pgBaseDSN   string
	pgStartErr  error
)

// TestMain terminates the shared container after the suite so it does not leak when
// testcontainers' Ryuk reaper is disabled (the documented local Rancher-Desktop flow).
func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = pgContainer.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

// startPostgres starts (once) a postgres:16 container and returns a base DSN, or SKIPS
// the calling test if Docker/testcontainers is unavailable.
func startPostgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("app"),
			tcpostgres.WithUsername("app"),
			tcpostgres.WithPassword("app"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
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

// freshPGDatabase creates a uniquely named database on the shared container and returns
// a DSN for it, so each test starts from an empty schema.
func freshPGDatabase(t *testing.T, baseDSN string) string {
	t.Helper()
	dbName := pgDBName(t.Name())
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	defer admin.Close()
	// The container reports ready via the log line, but a brief connect retry avoids a
	// race with the first admin statement.
	var pingErr error
	for i := 0; i < 20; i++ {
		if pingErr = admin.Ping(); pingErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("ping admin conn: %v", pingErr)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
		t.Fatalf("drop db %s: %v", dbName, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	return replaceDBName(baseDSN, dbName)
}

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
	if len(out) > 60 {
		out = out[:60]
	}
	return string(out)
}

func replaceDBName(dsn, dbName string) string {
	const scheme = "postgres://"
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

// pgQueryInt opens a short-lived pgx connection and scans a single int result.
func pgQueryInt(t *testing.T, dsn, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}
