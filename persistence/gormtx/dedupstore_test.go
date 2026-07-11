package gormtx_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// idemReq is a test mutation request: a real proto.Message (embedded StringValue)
// carrying an AIP-155 request_id.
type idemReq struct {
	*wrapperspb.StringValue
	requestID string
}

func (r *idemReq) GetRequestId() string { return r.requestID }

// The GORM store must satisfy the interceptor's interface (structural check).
var _ middleware.DurableIdempotencyStore = (*gormtx.GormDurableDedupStore)(nil)

// dedupEffect is a tiny "aggregate write" row used to prove the idempotency
// claim/complete commits/rolls back ATOMICALLY with the handler's own write.
type dedupEffect struct {
	ID string `gorm:"primaryKey"`
}

func openDedupDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.IdempotencyKeyRow{}, &dedupEffect{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// clock is a controllable time source for TTL/GC tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func TestDurableDedupStore_ClaimCompleteAndReplayAcrossRestart(t *testing.T) {
	db := openDedupDB(t, "dedup_replay")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, err := store.Claim(ctx, key, "fp-1", time.Hour)
		if err != nil || !claimed {
			return fmt.Errorf("fresh claim must succeed: claimed=%v err=%v", claimed, err)
		}
		if e := txConn(ctx, db).Create(&dedupEffect{ID: "fx-1"}).Error; e != nil {
			return e
		}
		return store.Complete(ctx, key, "toy.v1.Widget", []byte("response-bytes"))
	})
	if err != nil {
		t.Fatalf("claim+complete tx: %v", err)
	}

	// Durable across a fresh store instance (restart proxy): the completed record
	// and its response bytes are read back.
	store2 := gormtx.NewGormDurableDedupStore(db)
	rec, ok, err := store2.Lookup(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("completed record must be durably retrievable: ok=%v err=%v", ok, err)
	}
	if rec.Status != persistence.StatusCompleted || string(rec.Response) != "response-bytes" ||
		rec.ResponseType != "toy.v1.Widget" || rec.Fingerprint != "fp-1" {
		t.Fatalf("record round-trip mismatch: %+v", rec)
	}
}

func TestDurableDedupStore_ExactlyOnceUnderDoubleApply(t *testing.T) {
	db := openDedupDB(t, "dedup_exactly_once")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "dup"}

	applyOnce := func(effectID string) error {
		return tx.Atomically(context.Background(), func(ctx context.Context) error {
			existing, claimed, err := store.Claim(ctx, key, "", time.Hour)
			if err != nil {
				return err
			}
			if !claimed {
				if existing.Status == persistence.StatusCompleted {
					return nil // replay path — do NOT re-apply the effect
				}
				return errors.New("unexpected in-progress")
			}
			if e := txConn(ctx, db).Create(&dedupEffect{ID: effectID}).Error; e != nil {
				return e
			}
			return store.Complete(ctx, key, "toy.v1.Widget", []byte(effectID))
		})
	}

	if err := applyOnce("fx-A"); err != nil {
		t.Fatalf("first delivery must commit: %v", err)
	}
	if err := applyOnce("fx-B"); err != nil {
		t.Fatalf("second delivery must be a no-op replay: %v", err)
	}

	var n int64
	db.Model(&dedupEffect{}).Count(&n)
	if n != 1 {
		t.Fatalf("exactly-once: want exactly one effect row, got %d", n)
	}
	var got dedupEffect
	db.First(&got)
	if got.ID != "fx-A" {
		t.Fatalf("surviving effect must be the first delivery's, got %q", got.ID)
	}
}

func TestDurableDedupStore_AtomicRollbackOnHandlerError(t *testing.T) {
	db := openDedupDB(t, "dedup_rollback")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "err"}
	boom := errors.New("handler failed after claim")

	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, _, e := store.Claim(ctx, key, "", time.Hour); e != nil {
			return e
		}
		if e := txConn(ctx, db).Create(&dedupEffect{ID: "fx-rollback"}).Error; e != nil {
			return e
		}
		return boom // roll back the claim AND the effect
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}

	// The claim rolled back with the effect: no record, no effect, and a fresh
	// claim succeeds (nothing leaked → the retry re-executes).
	if _, ok, _ := store.Lookup(context.Background(), key); ok {
		t.Fatal("a rolled-back claim must not leave a durable record")
	}
	var n int64
	db.Model(&dedupEffect{}).Count(&n)
	if n != 0 {
		t.Fatalf("the effect must roll back with the claim, found %d", n)
	}
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, e := store.Claim(ctx, key, "", time.Hour)
		if e != nil || !claimed {
			return fmt.Errorf("fresh claim after rollback must succeed: claimed=%v err=%v", claimed, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("fresh claim: %v", err)
	}
}

func TestDurableDedupStore_InProgressPersistsForConflict(t *testing.T) {
	db := openDedupDB(t, "dedup_inprogress")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "ip"}

	// Commit an in_progress claim WITHOUT completing (models the saga reserve path
	// or a crash between claim-commit and complete).
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, _, e := store.Claim(ctx, key, "", time.Hour)
		return e
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	rec, ok, err := store.Lookup(context.Background(), key)
	if err != nil || !ok || rec.Status != persistence.StatusInProgress {
		t.Fatalf("in-progress record must be observable for the 409 path: ok=%v status=%v err=%v", ok, rec.Status, err)
	}

	// A second Claim over the live in-progress row returns claimed=false, in_progress.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		existing, claimed, e := store.Claim(ctx, key, "", time.Hour)
		if e != nil {
			return e
		}
		if claimed || existing.Status != persistence.StatusInProgress {
			return fmt.Errorf("second claim must observe the live in-progress row: claimed=%v status=%v", claimed, existing.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("second claim: %v", err)
	}
}

func TestDurableDedupStore_TTLExpiryReclaimAndGC(t *testing.T) {
	db := openDedupDB(t, "dedup_ttl")
	clk := &clock{t: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}
	store := gormtx.NewGormDurableDedupStore(db, gormtx.WithDurableDedupClock(clk.Now))
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "ttl"}

	// Claim+complete with a 1h TTL, an "old" fingerprint, and a stored response.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, _, e := store.Claim(ctx, key, "fp-old", time.Hour); e != nil {
			return e
		}
		return store.Complete(ctx, key, "toy.v1.Widget", []byte("v"))
	}); err != nil {
		t.Fatalf("claim+complete: %v", err)
	}

	// Before expiry: Lookup hits.
	if _, ok, _ := store.Lookup(context.Background(), key); !ok {
		t.Fatal("record must be live before expiry")
	}

	// Advance past expiry: Lookup treats it as absent (re-executable).
	clk.Advance(2 * time.Hour)
	if _, ok, _ := store.Lookup(context.Background(), key); ok {
		t.Fatal("an expired record must Lookup as absent")
	}

	// An expired conflicting row is RECLAIMED as a fresh claim (not a 409), and the
	// reclaim RESETS the record: status back to in_progress, response/type cleared,
	// fingerprint replaced.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, e := store.Claim(ctx, key, "fp-new", time.Hour)
		if e != nil || !claimed {
			return fmt.Errorf("expired row must be reclaimable: claimed=%v err=%v", claimed, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	rec, ok, err := store.Lookup(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("reclaimed row must be live: ok=%v err=%v", ok, err)
	}
	if rec.Status != persistence.StatusInProgress || len(rec.Response) != 0 || rec.ResponseType != "" || rec.Fingerprint != "fp-new" {
		t.Fatalf("reclaim must reset the record (in_progress, no response, new fingerprint), got %+v", rec)
	}

	// GC removes rows already expired at the given instant. Seed a second, already
	// expired key and sweep.
	old := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "old"}
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, _, e := store.Claim(ctx, old, "", time.Nanosecond) // expires effectively now
		return e
	}); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	clk.Advance(time.Hour)
	removed, err := store.GC(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if removed < 1 {
		t.Fatalf("GC must remove at least the expired 'old' key, removed %d", removed)
	}
	if _, ok, _ := store.Lookup(context.Background(), old); ok {
		t.Fatal("GC'd key must be gone")
	}
}

// TestDurableDedup_Interceptor_ExactlyOnce_RealDB is the end-to-end proof: the
// durable interceptor + GormTxRunner + GormDurableDedupStore make a retried
// mutation run its handler exactly once and replay the ORIGINAL server-generated
// response — with the effect committed exactly once, atomically with the claim.
func TestDurableDedup_Interceptor_ExactlyOnce_RealDB(t *testing.T) {
	db := openDedupDB(t, "dedup_interceptor")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	intc := middleware.DurableDeduplicateUnary(middleware.DurableDedup{Store: store, Tx: tx})
	info := &grpc.UnaryServerInfo{FullMethod: "/toy.v1.WidgetService/CreateWidget"}

	seq := 0
	handler := func(ctx context.Context, req any) (any, error) {
		seq++
		id := fmt.Sprintf("gen-%d", seq)
		if e := txConn(ctx, db).Create(&dedupEffect{ID: id}).Error; e != nil {
			return nil, e
		}
		return wrapperspb.String(id), nil
	}
	ctx := middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: "t1"})
	req := &idemReq{StringValue: wrapperspb.String("body"), requestID: "r1"}

	r1, err := intc(ctx, req, info, handler)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	r2, err := intc(ctx, req, info, handler)
	if err != nil {
		t.Fatalf("call 2 (retry): %v", err)
	}
	if seq != 1 {
		t.Fatalf("handler must execute exactly once across the retry, ran %d", seq)
	}
	if r1.(*wrapperspb.StringValue).GetValue() != "gen-1" || r2.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("retry must replay the ORIGINAL id gen-1, got %v / %v", r1, r2)
	}
	var n int64
	db.Model(&dedupEffect{}).Count(&n)
	if n != 1 {
		t.Fatalf("exactly-once: want one effect row, got %d", n)
	}
}

// TestDurableDedup_Interceptor_ConcurrentExactlyOnce fires N concurrent duplicates
// of the same request through the interceptor. The DB's primary-key uniqueness
// serializes the claim: exactly ONE goroutine executes the handler and writes the
// effect; the losers block on the claim, then replay the winner's response (or
// observe a committed in_progress → AlreadyExists). Never a second execution.
// SQLite is single-writer, so a busy timeout lets the losers wait for the winner
// rather than error — the same block-then-replay shape a real Postgres store gives
// via row-level locking under READ COMMITTED.
func TestDurableDedup_Interceptor_ConcurrentExactlyOnce(t *testing.T) {
	// Unique shared-cache name per run: all pooled connections share the in-memory
	// DB within this test (real concurrency), but a -count=N re-run does not collide.
	dsn := fmt.Sprintf("file:dedup_conc_%d?mode=memory&cache=shared&_pragma=busy_timeout(10000)", time.Now().UnixNano())
	db, err := gorm.Open(openTestSQLite(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.IdempotencyKeyRow{}, &dedupEffect{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	intc := middleware.DurableDeduplicateUnary(middleware.DurableDedup{Store: store, Tx: tx})
	info := &grpc.UnaryServerInfo{FullMethod: "/toy.v1.WidgetService/CreateWidget"}

	var executions int64
	handler := func(ctx context.Context, req any) (any, error) {
		n := atomic.AddInt64(&executions, 1)
		id := fmt.Sprintf("gen-%d", n)
		if e := txConn(ctx, db).Create(&dedupEffect{ID: id}).Error; e != nil {
			return nil, e
		}
		return wrapperspb.String(id), nil
	}
	ctx := middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: "t1"})
	req := &idemReq{StringValue: wrapperspb.String("body"), requestID: "r-conc"}

	const N = 6
	var wg sync.WaitGroup
	results := make([]string, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			r, e := intc(ctx, req, info, handler)
			if e != nil {
				errs[i] = e
				return
			}
			results[i] = r.(*wrapperspb.StringValue).GetValue()
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&executions); got != 1 {
		t.Fatalf("exactly-once under concurrency: handler must run exactly once, ran %d", got)
	}
	var effects int64
	db.Model(&dedupEffect{}).Count(&effects)
	if effects != 1 {
		t.Fatalf("exactly-once under concurrency: want one effect row, got %d", effects)
	}
	// Every successful caller must have gotten the ONE winner's response; any error
	// must be the in-flight conflict (never a second execution / different result).
	winner := ""
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			if status.Code(errs[i]) != codes.AlreadyExists {
				t.Fatalf("caller %d unexpected error: %v", i, errs[i])
			}
			continue
		}
		if winner == "" {
			winner = results[i]
		}
		if results[i] != winner {
			t.Fatalf("callers must all replay the same winning response, got %q and %q", winner, results[i])
		}
	}
	if winner != "gen-1" {
		t.Fatalf("the winning response must be the single execution's gen-1, got %q", winner)
	}
}
