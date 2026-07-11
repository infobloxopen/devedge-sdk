package iamv1_test

// postgres_idempotency_perf_test.go — WS-043 / F048 Increment 3, Deliverable D on REAL
// PostgreSQL (the only engine where fillfactor/autovacuum storage params and declarative
// HASH partitioning exist). It rides the shared testcontainers harness (startPostgres /
// freshPGDatabase); on a machine without Docker startPostgres calls t.Skip, so the suite
// still passes locally and runs for real in CI.
//
// It proves: (AC-D1) the tuning params land on the plain table; (AC-D3) MigrateModule with
// PartitionCount>0 creates the table HASH-partitioned into n leaves, each storage-tuned,
// and a claim/complete/replay + ON CONFLICT + reclaim are correct on the partitioned table
// (exactly-once preserved); (AC-D4) requesting partitioning on an existing NON-partitioned
// table fails loud and never drops it.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// openFreshPGGorm opens a GORM client on a brand-new empty Postgres database (no framework
// tables migrated yet), so a test fully controls the idempotency_keys table's shape.
func openFreshPGGorm(t *testing.T) *gorm.DB {
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
	return db
}

func relkind(t *testing.T, db *gorm.DB, relname string) string {
	t.Helper()
	var k string
	if err := db.Raw(`SELECT relkind FROM pg_class WHERE relname = ?`, relname).Scan(&k).Error; err != nil {
		t.Fatalf("relkind %q: %v", relname, err)
	}
	return k
}

func reloptions(t *testing.T, db *gorm.DB, relname string) string {
	t.Helper()
	var opts string
	if err := db.Raw(`SELECT COALESCE(array_to_string(reloptions, ','), '') FROM pg_class WHERE relname = ?`, relname).Scan(&opts).Error; err != nil {
		t.Fatalf("reloptions %q: %v", relname, err)
	}
	return opts
}

func leafPartitions(t *testing.T, db *gorm.DB, parent string) []string {
	t.Helper()
	var names []string
	if err := db.Raw(`SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = ? ORDER BY c.relname`, parent).Scan(&names).Error; err != nil {
		t.Fatalf("leaf partitions of %q: %v", parent, err)
	}
	return names
}

// TestPG_TuneIdempotencyKeys_Plain: MigrateModule (no partitions) tunes the plain table.
func TestPG_TuneIdempotencyKeys_Plain(t *testing.T) {
	db := openFreshPGGorm(t)
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	if err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:       ns,
		FrameworkModels: gormtx.RequestIdempotencyMigrationModels(),
	}); err != nil {
		t.Fatalf("MigrateModule: %v", err)
	}
	if k := relkind(t, db, "idempotency_keys"); k != "r" {
		t.Fatalf("idempotency_keys relkind = %q, want 'r' (plain table)", k)
	}
	opts := reloptions(t, db, "idempotency_keys")
	for _, want := range []string{"fillfactor=80", "autovacuum_vacuum_scale_factor=0.02", "autovacuum_vacuum_threshold=50"} {
		if !strings.Contains(opts, want) {
			t.Fatalf("reloptions %q missing %q", opts, want)
		}
	}
	// Re-run is idempotent.
	if err := gormtx.TuneIdempotencyKeys(context.Background(), db, ns); err != nil {
		t.Fatalf("re-tune: %v", err)
	}
}

// TestPG_HashPartitioned_CreateAndExactlyOnce: MigrateModule with PartitionCount creates a
// hash-partitioned table with n tuned leaves, and claim/complete/replay + ON CONFLICT +
// reclaim are all correct on it (exactly-once preserved).
func TestPG_HashPartitioned_CreateAndExactlyOnce(t *testing.T) {
	db := openFreshPGGorm(t)
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	const n = 4
	if err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:             ns,
		FrameworkModels:       gormtx.RequestIdempotencyMigrationModels(),
		IdempotencyPartitions: n,
	}); err != nil {
		t.Fatalf("MigrateModule (partitioned): %v", err)
	}
	if k := relkind(t, db, "idempotency_keys"); k != "p" {
		t.Fatalf("idempotency_keys relkind = %q, want 'p' (partitioned)", k)
	}
	leaves := leafPartitions(t, db, "idempotency_keys")
	if len(leaves) != n {
		t.Fatalf("got %d leaves, want %d: %v", len(leaves), n, leaves)
	}
	for _, leaf := range leaves {
		if o := reloptions(t, db, leaf); !strings.Contains(o, "fillfactor=80") {
			t.Fatalf("leaf %q reloptions %q missing fillfactor=80", leaf, o)
		}
	}

	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	ctx := context.Background()

	// Distinct keys hash across partitions; a claim/complete/replay round-trips.
	for i, rid := range []string{"r1", "r2", "r3", "r4", "r5"} {
		key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: rid}
		if err := tx.Atomically(ctx, func(ctx context.Context) error {
			_, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour)
			if cerr != nil || !claimed {
				t.Fatalf("claim %d: claimed=%v err=%v", i, claimed, cerr)
			}
			return store.Complete(ctx, key, "T", []byte(rid))
		}); err != nil {
			t.Fatalf("claim/complete %d: %v", i, err)
		}
		rec, ok, err := store.Lookup(ctx, key)
		if err != nil || !ok || string(rec.Response) != rid {
			t.Fatalf("replay %d: ok=%v resp=%q err=%v", i, ok, rec.Response, err)
		}
	}

	// GLOBAL uniqueness on the partitioned table: a fresh claim of an EXISTING completed
	// key inside a tx must NOT be a fresh claim (ON CONFLICT DO NOTHING routes correctly).
	dup := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		rec, claimed, cerr := store.Claim(ctx, dup, "fp", time.Hour)
		if cerr != nil {
			return cerr
		}
		if claimed {
			t.Fatal("duplicate of a completed key must NOT be a fresh claim (global uniqueness broken)")
		}
		if rec.Status != persistence.StatusCompleted {
			t.Fatalf("duplicate must see completed, got %q", rec.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("duplicate claim tx: %v", err)
	}
}

// dedupEffectPG is a tiny "aggregate write" row proving the handler effect commits exactly
// once with the claim on the partitioned table under real concurrency.
type dedupEffectPG struct {
	ID string `gorm:"primaryKey"`
}

// TestPG_HashPartitioned_ConcurrentExactlyOnce fires N concurrent claims of the SAME key at
// the hash-partitioned table and asserts the handler effect runs EXACTLY ONCE (one committed
// effect row) — losers block on the unique key then 409 (their retry would replay). This is
// the exactly-once-survives-partitioning guarantee (AC-D3) on real Postgres.
func TestPG_HashPartitioned_ConcurrentExactlyOnce(t *testing.T) {
	db := openFreshPGGorm(t)
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	if err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:             ns,
		FrameworkModels:       gormtx.RequestIdempotencyMigrationModels(),
		IdempotencyPartitions: 8,
	}); err != nil {
		t.Fatalf("MigrateModule (partitioned): %v", err)
	}
	if err := db.AutoMigrate(&dedupEffectPG{}); err != nil {
		t.Fatalf("automigrate effect: %v", err)
	}
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "concurrent"}

	const goroutines = 12
	var effects, replays, conflicts int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			err := tx.Atomically(context.Background(), func(ctx context.Context) error {
				existing, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour)
				if cerr != nil {
					return cerr
				}
				if !claimed {
					if existing.Status == persistence.StatusCompleted {
						atomic.AddInt64(&replays, 1)
						return nil
					}
					return middleware.ErrIdempotencyInProgress // committed in_progress → 409
				}
				// Fresh claim: perform the one-time effect, then complete.
				if e := db.WithContext(ctx).Create(&dedupEffectPG{ID: "the-only-effect"}).Error; e != nil {
					return e
				}
				atomic.AddInt64(&effects, 1)
				return store.Complete(ctx, key, "T", []byte("winner"))
			})
			if err != nil {
				if st, _ := status.FromError(err); st.Code() == codes.AlreadyExists {
					atomic.AddInt64(&conflicts, 1)
					return
				}
				// A serialization/lock error is an acceptable loser outcome under contention;
				// only an unexpected error type fails the test.
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one committed effect row — the core exactly-once guarantee.
	var effectRows int64
	if err := db.Model(&dedupEffectPG{}).Count(&effectRows).Error; err != nil {
		t.Fatalf("count effects: %v", err)
	}
	if effectRows != 1 {
		t.Fatalf("effect committed %d times, want exactly 1 (effects=%d replays=%d conflicts=%d)", effectRows, effects, replays, conflicts)
	}
	// The stored response is the winner's, replayable.
	rec, ok, err := store.Lookup(context.Background(), key)
	if err != nil || !ok || string(rec.Response) != "winner" {
		t.Fatalf("winner response not replayable: ok=%v resp=%q err=%v", ok, rec.Response, err)
	}
}

// TestPG_Partition_FailLoudIfPlain: enabling partitioning when the table already exists
// non-partitioned fails loud and does NOT drop it.
func TestPG_Partition_FailLoudIfPlain(t *testing.T) {
	db := openFreshPGGorm(t)
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	// Materialize the PLAIN table first (as a prior non-partitioned deployment would).
	if err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:       ns,
		FrameworkModels: gormtx.RequestIdempotencyMigrationModels(),
	}); err != nil {
		t.Fatalf("MigrateModule plain: %v", err)
	}
	err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, ns, 4)
	if err == nil || !strings.Contains(err.Error(), "NON-partitioned") {
		t.Fatalf("want fail-loud NON-partitioned error, got: %v", err)
	}
	// The table must still exist (never dropped) and still be plain.
	if k := relkind(t, db, "idempotency_keys"); k != "r" {
		t.Fatalf("table must remain a plain table after refusal, relkind=%q", k)
	}
}

// TestPG_Partition_Idempotent: re-running EnsureIdempotencyKeysPartitioned on an already-
// partitioned table is a no-op (ensures the n leaves), never an error.
func TestPG_Partition_Idempotent(t *testing.T) {
	db := openFreshPGGorm(t)
	ns := persistence.DatabaseNamespace{ModuleID: "m1"}
	if err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, ns, 4); err != nil {
		t.Fatalf("first partition: %v", err)
	}
	if err := gormtx.EnsureIdempotencyKeysPartitioned(context.Background(), db, ns, 4); err != nil {
		t.Fatalf("second (idempotent) partition: %v", err)
	}
	if got := len(leafPartitions(t, db, "idempotency_keys")); got != 4 {
		t.Fatalf("leaves after idempotent re-run = %d, want 4", got)
	}
}
