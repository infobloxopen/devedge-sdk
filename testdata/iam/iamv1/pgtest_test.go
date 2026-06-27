package iamv1_test

// pgtest_test.go — a testcontainers-backed PostgreSQL harness for the IAM fixture
// (Phase 2). It proves the transactional-outbox + exactly-once-dispatch machinery
// on REAL Postgres (the production target), where the idempotency UNIQUE conflict
// that serializes a concurrent double-apply is the genuine engine-level thing — not
// the write-serialized SQLite approximation.
//
// Docker-optional contract: startPostgres calls t.Skip("docker unavailable: ...")
// cleanly when testcontainers / Docker cannot start, so `go test ./...` on a
// machine WITHOUT Docker still passes (the PG tests skip, they do not fail). When
// Docker IS available (e.g. CI's ubuntu-latest) the PG tests run for real.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq" // registers the "postgres" database/sql driver for ent.Open and the wait.ForSQL ping

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	nat "github.com/moby/moby/api/types/network"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	entiam "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

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
	// F033 P2: also reap the shared MySQL container (mysqltest_test.go) for the same
	// Ryuk-off-local-workflow reason.
	terminateMySQL()
	os.Exit(code)
}

// startPostgres starts (once) a postgres:16 container via testcontainers and
// returns a base DSN. If Docker/testcontainers cannot start it SKIPS the calling
// test rather than failing, so the suite is green without Docker.
func startPostgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("iam"),
			tcpostgres.WithUsername("iam"),
			tcpostgres.WithPassword("iam"),
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2).
						WithStartupTimeout(60*time.Second),
					wait.ForSQL("5432/tcp", "postgres", func(host string, port nat.Port) string {
						return fmt.Sprintf("host=%s port=%s user=iam password=iam dbname=iam sslmode=disable",
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
// container and returns a DSN for it, so each test starts from an empty schema.
func freshPGDatabase(t *testing.T, baseDSN string) string {
	t.Helper()
	dbName := pgDBName(t.Name())
	admin, err := sql.Open("postgres", baseDSN)
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
		t.Fatalf("drop db %s: %v", dbName, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	return replaceDBName(baseDSN, dbName)
}

// openIAMGormPG opens a GORM client on a fresh Postgres database and AutoMigrates
// the IAM models PLUS the reusable outbox/idempotency tables (the PG twin of
// openIAMGormDB on sqlite). The GormIdempotencyStore's IdemMarker primary key is a
// real Postgres UNIQUE — a concurrent double-apply collides on it, which is the
// genuine exactly-once guard this test exercises.
func openIAMGormPG(t *testing.T) *gorm.DB {
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
	if err := db.AutoMigrate(
		&iamv1.UserModel{},
		&iamv1.ApiKeyModel{},
		&gormtx.OutboxRow{},
		&gormtx.OutboxCursorRow{},
		&gormtx.OutboxDeadLetterRow{},
		&gormtx.IdemMarker{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// openIAMEntPG opens an ent client on a fresh Postgres database and migrates the
// IAM schema (including the Outbox table) via ent.Open("postgres", dsn) +
// Schema.Create — the PG twin of enttest.Open on sqlite3. lib/pq supplies the
// "postgres" database/sql driver ent's Postgres dialect expects.
func openIAMEntPG(t *testing.T) *entiam.Client {
	t.Helper()
	dsn := freshPGDatabase(t, startPostgres(t))
	client, err := entiam.Open("postgres", dsn)
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
