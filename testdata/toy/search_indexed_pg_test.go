package widgetsv1_test

// search_indexed_pg_test.go — a testcontainers-backed PostgreSQL harness proving the
// WS-041 INDEXED strategy on REAL Postgres (the production target), where the
// persisted `search_vector` generated column, the GIN index, and true FTS matching
// are the genuine engine-level things a SQLite approximation cannot show (FR-C2/C3,
// AC-3/4/8).
//
// Docker-optional contract (mirrors testdata/iam/iamv1/pgtest_test.go): startPostgres
// SKIPS the calling test cleanly when Docker/testcontainers cannot start OR when the
// host cannot reach the container (the documented Rancher-Desktop port-forwarding
// gap), so `go test ./...` stays green without a working container; CI runs it for
// real. When the container is unreachable locally, the identical SQL is proven via
// `docker exec … psql` instead.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver for the wait/admin pings

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	nat "github.com/moby/moby/api/types/network"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
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

// startPostgres starts (once) a postgres:16 container and returns a base DSN, or
// SKIPS the calling test when Docker/testcontainers is unavailable or unreachable.
func startPostgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("toy"),
			tcpostgres.WithUsername("toy"),
			tcpostgres.WithPassword("toy"),
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2).WithStartupTimeout(90*time.Second),
					wait.ForSQL("5432/tcp", "pgx", func(host string, port nat.Port) string {
						return fmt.Sprintf("host=%s port=%s user=toy password=toy dbname=toy sslmode=disable",
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
		// Prove host->container reachability now; if the sandbox cannot forward the
		// port, skip cleanly rather than fail every PG test at query time.
		db, perr := sql.Open("pgx", dsn)
		if perr == nil {
			perr = db.Ping()
			_ = db.Close()
		}
		if perr != nil {
			pgStartErr = fmt.Errorf("container started but unreachable: %w", perr)
			_ = c.Terminate(ctx)
			return
		}
		pgContainer = c
		pgBaseDSN = dsn
	})
	if pgStartErr != nil {
		t.Skipf("docker/postgres unavailable: %v", pgStartErr)
	}
	return pgBaseDSN
}

// openGizmoPG opens a GORM Postgres client, AutoMigrates GizmoModel (creating the
// gizmos table) and then applies the GENERATED migration files verbatim
// (migrations/9001…search_vector + 9002…search_gin), so the persisted column + GIN
// index under test are exactly what `make generate` emitted (FR-C2).
func openGizmoPG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := startPostgres(t)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open postgres: %v", err)
	}
	t.Cleanup(func() {
		// Drop so a re-run on the shared container starts clean.
		_ = db.Exec("DROP TABLE IF EXISTS gizmos").Error
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.Exec("DROP TABLE IF EXISTS gizmos").Error; err != nil {
		t.Fatalf("drop gizmos: %v", err)
	}
	if err := db.AutoMigrate(&widgetsv1.GizmoModel{}); err != nil {
		t.Fatalf("automigrate gizmos: %v", err)
	}
	for _, f := range []string{
		"migrations/9001_gizmos_search_vector.up.sql",
		"migrations/9002_gizmos_search_gin.up.sql", // CONCURRENTLY: gorm.Exec is autocommit, so it runs fine (FM-6)
	} {
		body, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		if err := db.Exec(string(body)).Error; err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// TestPG_Indexed_TrueFTS proves the INDEXED persisted-column path returns only
// matching rows over the real generated repository (FR-C3, AC-8): the field source
// (label) and the sql/postgres CASE source (tier_label) both match through
// search_vector.
func TestPG_Indexed_TrueFTS(t *testing.T) {
	db := openGizmoPG(t)
	repo := widgetsv1.NewGizmoRepository(db)
	seedGizmos(t, repo)
	ctx := context.Background()

	cases := []struct {
		q    string
		want []string
	}{
		{"acme", []string{"g-alpha", "g-gamma"}},   // field source: label
		{"deluxe", []string{"g-beta", "g-gamma"}},  // ONLY via the sql/postgres CASE source (AC-8)
		{"basic", []string{"g-alpha"}},             // CASE 'standard basic'
		{"globex", []string{"g-beta"}},             // field source: label
	}
	for _, tc := range cases {
		items, _, err := repo.List(ctx, persistence.ListOptions{Search: tc.q})
		if err != nil {
			t.Fatalf("List(q=%q): %v", tc.q, err)
		}
		if got := gizmoIDs(items); !eqIDs(got, tc.want) {
			t.Errorf("List(q=%q): got %v, want %v", tc.q, got, tc.want)
		}
	}
}

// TestPG_Indexed_GINUsed proves the List predicate uses the emitted GIN index
// (AC-4). On a tiny fixture the planner prefers a seq scan, so seqscan is disabled
// to show the GIN index is a valid, chosen access path for the predicate.
func TestPG_Indexed_GINUsed(t *testing.T) {
	db := openGizmoPG(t)
	seedGizmos(t, widgetsv1.NewGizmoRepository(db))

	var plan strings.Builder
	rows, err := db.Raw("SET enable_seqscan=off; EXPLAIN (COSTS OFF) SELECT id FROM gizmos WHERE search_vector @@ websearch_to_tsquery('simple', ?)", "premium").Rows()
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "gizmos_search_gin") {
		t.Fatalf("EXPLAIN plan does not use the GIN index gizmos_search_gin:\n%s", plan.String())
	}
}

// TestPG_Indexed_InjectionSafe proves AC-3 on the real INDEXED predicate: a hostile
// term is a bound parameter fed to websearch_to_tsquery, so it is parsed as search
// terms (never SQL) and List returns a well-formed empty result with no error.
func TestPG_Indexed_InjectionSafe(t *testing.T) {
	db := openGizmoPG(t)
	repo := widgetsv1.NewGizmoRepository(db)
	seedGizmos(t, repo)
	ctx := context.Background()

	items, _, err := repo.List(ctx, persistence.ListOptions{Search: `ac(me" or 1=1 --`})
	if err != nil {
		t.Fatalf("hostile q must not error (bound param + websearch_to_tsquery), got: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("hostile q should match nothing, got %d rows", len(items))
	}
	// The table is intact — nothing was injected.
	all, _, err := repo.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 rows intact after hostile query, got %d", len(all))
	}
}
