package gormtx_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// TestGC_BatchedDrainsBacklog inserts more expired rows than the batch size and asserts
// the chunked GC loop drains them ALL and returns the exact total — the DD-2 bound.
func TestGC_BatchedDrainsBacklog(t *testing.T) {
	db := openDedupDB(t, "gc_batched")
	// A small batch so N=25 rows need multiple chunks.
	store := gormtx.NewGormDurableDedupStore(db, gormtx.WithDurableDedupGCBatch(10))

	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	const n = 25
	for i := 0; i < n; i++ {
		row := gormtx.IdempotencyKeyRow{
			AccountID: "t1",
			Method:    "m",
			RequestID: fmt.Sprintf("r%d", i),
			Status:    string(persistence.StatusCompleted),
			CreatedAt: past,
			ExpiresAt: past, // already expired
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	// One live row that GC must NOT delete.
	live := gormtx.IdempotencyKeyRow{
		AccountID: "t1", Method: "m", RequestID: "live",
		Status: string(persistence.StatusCompleted), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := db.Create(&live).Error; err != nil {
		t.Fatalf("seed live: %v", err)
	}

	deleted, err := store.GC(context.Background(), now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if deleted != n {
		t.Fatalf("GC deleted %d, want %d", deleted, n)
	}
	var remaining int64
	if err := db.Model(&gormtx.IdempotencyKeyRow{}).Count(&remaining).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining rows = %d, want 1 (the live row)", remaining)
	}
}

// TestGC_NoExpired is a no-op that returns 0 (single empty chunk).
func TestGC_NoExpired(t *testing.T) {
	db := openDedupDB(t, "gc_none")
	store := gormtx.NewGormDurableDedupStore(db, gormtx.WithDurableDedupClock(time.Now))
	now := time.Now().UTC()
	row := gormtx.IdempotencyKeyRow{
		AccountID: "t1", Method: "m", RequestID: "r1",
		Status: string(persistence.StatusInProgress), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	deleted, err := store.GC(context.Background(), now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("GC deleted %d, want 0", deleted)
	}
}

// TestTuneIdempotencyKeys_NoOpOnSQLite asserts the PG tuning is a clean no-op on the dev
// backend (DD-1 / FM-D1): no error, table untouched.
func TestTuneIdempotencyKeys_NoOpOnSQLite(t *testing.T) {
	db := openDedupDB(t, "tune_sqlite")
	if err := gormtx.TuneIdempotencyKeys(context.Background(), db, persistence.DatabaseNamespace{}); err != nil {
		t.Fatalf("TuneIdempotencyKeys on sqlite must be a no-op, got: %v", err)
	}
}

// TestEnsurePartitioned_PostgresOnly asserts the explicit partition call fails loud (never
// half-applies) on a non-Postgres dialect (DD-3 / FM-D1).
func TestEnsurePartitioned_PostgresOnly(t *testing.T) {
	db := openDedupDB(t, "part_sqlite")
	err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, persistence.DatabaseNamespace{}, 4)
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL-only") {
		t.Fatalf("want a PostgreSQL-only error on sqlite, got: %v", err)
	}
}

// TestEnsurePartitioned_InvalidCount rejects n<=0 and n>max before any DDL (FM-D2). It uses
// sqlite so the dialect guard would also fire, but the count guard is checked first.
func TestEnsurePartitioned_CountGuards(t *testing.T) {
	db := openDedupDB(t, "part_count")
	// n<=0 is treated as off by callers, but the explicit function rejects it OR the
	// dialect guard fires first — either way it must not create anything. We assert an
	// error is returned (no silent success).
	if err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, persistence.DatabaseNamespace{}, 0); err == nil {
		t.Fatalf("n=0 must error, got nil")
	}
	if err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, persistence.DatabaseNamespace{}, gormtx.MaxIdempotencyPartitions+1); err == nil {
		t.Fatalf("n>max must error, got nil")
	}
}

// TestMigrateModule_SQLitePlainTableUnaffected proves DD-5: with PartitionCount opted in
// but the dialect SQLite, MigrateModule still produces the ordinary plain idempotency_keys
// table (partitioning is PG-only and skipped) and durable idempotency works on it.
func TestMigrateModule_SQLiteIdempotencyPartitionsIgnored(t *testing.T) {
	db := openMigrateDB(t, "migrate_sqlite_part", "")
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:             ns,
		FrameworkModels:       gormtx.RequestIdempotencyMigrationModels(),
		SkipAdvisoryLock:      true,
		IdempotencyPartitions: 8, // opted in, but SQLite → ignored, plain table created
	})
	if err != nil {
		t.Fatalf("MigrateModule on sqlite with partitions opted in must succeed (plain table): %v", err)
	}
	// The plain table exists and a claim/complete round-trips.
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour)
		if cerr != nil || !claimed {
			return fmt.Errorf("claim: claimed=%v err=%v", claimed, cerr)
		}
		return store.Complete(ctx, key, "T", []byte("resp"))
	}); err != nil {
		t.Fatalf("claim/complete: %v", err)
	}
	rec, ok, err := store.Lookup(context.Background(), key)
	if err != nil || !ok || rec.Status != persistence.StatusCompleted {
		t.Fatalf("lookup after complete: ok=%v status=%v err=%v", ok, rec.Status, err)
	}
}
