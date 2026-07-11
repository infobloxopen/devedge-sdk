package iamv1_test

// dedup_store_test.go — WS-043 / F048 Increment 3, Deliverable C: exactly-once PARITY of
// the ent-backed durable idempotency store (entrepo.EntDurableDedupStore wired to the ent
// client in iamv1.NewEntDurableDedupStore) with the gorm store's dedupstore_test. It runs
// on the fast SQLite (enttest) backend; the PostgreSQL concurrency semantics ride the
// testcontainers harness in CI.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register the SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
	entiam "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// The ent store must satisfy the interceptor's interface (structural check).
var _ middleware.DurableIdempotencyStore = (*entrepo.EntDurableDedupStore)(nil)

type entClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *entClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *entClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func openEntDedup(t *testing.T, name string) *entiam.Client {
	t.Helper()
	return enttest.Open(t, "sqlite3", "file:"+name+"?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
}

// TestEntDedup_ClaimCompleteReplayAcrossRestart mirrors the gorm store: a claim + complete
// inside one tx commits atomically, and a retry replays the stored response verbatim —
// even from a FRESH store instance (restart), because it is persisted.
func TestEntDedup_ClaimCompleteReplayAcrossRestart(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_replay")
	clk := &entClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := iamv1.NewEntDurableDedupStore(client, entrepo.WithEntDurableDedupClock(clk.Now))
	tx := iamv1.NewEntTxRunner(client)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, err := store.Claim(ctx, key, "fp-1", time.Hour)
		if err != nil || !claimed {
			return fmt.Errorf("fresh claim must succeed: claimed=%v err=%v", claimed, err)
		}
		return store.Complete(ctx, key, "some.Type", []byte("RESP-BYTES"))
	}); err != nil {
		t.Fatalf("claim/complete tx: %v", err)
	}

	// Fresh store instance = process restart. The completed record replays verbatim.
	store2 := iamv1.NewEntDurableDedupStore(client, entrepo.WithEntDurableDedupClock(clk.Now))
	rec, ok, err := store2.Lookup(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("lookup after restart: ok=%v err=%v", ok, err)
	}
	if rec.Status != persistence.StatusCompleted || rec.ResponseType != "some.Type" || string(rec.Response) != "RESP-BYTES" {
		t.Fatalf("replayed record wrong: %+v", rec)
	}
}

// TestEntDedup_AtomicRollback: a handler error rolls the claim back WITH the effect — a
// retry re-executes (no record persisted).
func TestEntDedup_AtomicRollback(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_rollback")
	store := iamv1.NewEntDurableDedupStore(client)
	tx := iamv1.NewEntTxRunner(client)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	wantErr := errors.New("handler boom")
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour); cerr != nil || !claimed {
			return fmt.Errorf("claim: claimed=%v err=%v", claimed, cerr)
		}
		return wantErr // handler fails after claim
	}); !errors.Is(err, wantErr) {
		t.Fatalf("want handler error, got %v", err)
	}
	if _, ok, err := store.Lookup(context.Background(), key); err != nil || ok {
		t.Fatalf("claim must have rolled back: ok=%v err=%v", ok, err)
	}
}

// TestEntDedup_LiveInProgressConflict: a committed in_progress reservation makes a second
// claim return claimed=false with in_progress status (the caller 409s).
func TestEntDedup_LiveInProgressConflict(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_inprogress")
	store := iamv1.NewEntDurableDedupStore(client)
	tx := iamv1.NewEntTxRunner(client)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	// Reserve (commit an in_progress record) — the saga reserve step.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour)
		if cerr != nil || !claimed {
			return fmt.Errorf("reserve claim: claimed=%v err=%v", claimed, cerr)
		}
		return nil // commit the in_progress reservation
	}); err != nil {
		t.Fatalf("reserve tx: %v", err)
	}

	// A duplicate observes the committed in_progress record.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		rec, claimed, cerr := store.Claim(ctx, key, "fp", time.Hour)
		if cerr != nil {
			return cerr
		}
		if claimed {
			return errors.New("second claim must NOT be fresh")
		}
		if rec.Status != persistence.StatusInProgress {
			return fmt.Errorf("want in_progress, got %q", rec.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("duplicate claim tx: %v", err)
	}
}

// TestEntDedup_ExpiredReclaim: an expired record is reclaimed as a fresh claim.
func TestEntDedup_ExpiredReclaim(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_reclaim")
	clk := &entClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := iamv1.NewEntDurableDedupStore(client, entrepo.WithEntDurableDedupClock(clk.Now))
	tx := iamv1.NewEntTxRunner(client)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	// Commit an in_progress record with a short TTL.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, _, cerr := store.Claim(ctx, key, "fp", time.Minute)
		return cerr
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	clk.Advance(2 * time.Minute) // record is now expired

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, cerr := store.Claim(ctx, key, "fp2", time.Hour)
		if cerr != nil {
			return cerr
		}
		if !claimed {
			return errors.New("expired record must be reclaimable (fresh claim)")
		}
		return cerr
	}); err != nil {
		t.Fatalf("reclaim tx: %v", err)
	}
}

// TestEntDedup_AbandonGuarded: Abandon deletes an in_progress reservation but NEVER a
// completed one (never erases a durable response).
func TestEntDedup_AbandonGuarded(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_abandon")
	store := iamv1.NewEntDurableDedupStore(client)
	tx := iamv1.NewEntTxRunner(client)
	completed := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "done"}
	inflight := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "wip"}

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, _, e := store.Claim(ctx, completed, "fp", time.Hour); e != nil {
			return e
		}
		if e := store.Complete(ctx, completed, "T", []byte("R")); e != nil {
			return e
		}
		_, _, e := store.Claim(ctx, inflight, "fp", time.Hour)
		return e
	}); err != nil {
		t.Fatalf("seed tx: %v", err)
	}

	// Abandon the completed key: must be a no-op (guarded), record intact.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		deleted, e := store.Abandon(ctx, completed)
		if e != nil {
			return e
		}
		if deleted {
			return errors.New("Abandon must NOT delete a completed record")
		}
		return nil
	}); err != nil {
		t.Fatalf("abandon completed tx: %v", err)
	}
	if _, ok, _ := store.Lookup(context.Background(), completed); !ok {
		t.Fatal("completed record must survive Abandon")
	}
	// Abandon the in_progress key: deletes it.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		deleted, e := store.Abandon(ctx, inflight)
		if e != nil {
			return e
		}
		if !deleted {
			return errors.New("Abandon must delete an in_progress reservation")
		}
		return nil
	}); err != nil {
		t.Fatalf("abandon inflight tx: %v", err)
	}
}

// TestEntDedup_TenantAndMethodScoping: the encoded id fences tenants and methods — a
// request_id reused across tenants or methods never collides.
func TestEntDedup_TenantAndMethodScoping(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_scope")
	store := iamv1.NewEntDurableDedupStore(client)
	tx := iamv1.NewEntTxRunner(client)

	a := persistence.IdempotencyKey{Tenant: "tenantA", Method: "m", RequestID: "shared"}
	b := persistence.IdempotencyKey{Tenant: "tenantB", Method: "m", RequestID: "shared"}
	c := persistence.IdempotencyKey{Tenant: "tenantA", Method: "other", RequestID: "shared"}

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, _, e := store.Claim(ctx, a, "fp", time.Hour); e != nil {
			return e
		}
		return store.Complete(ctx, a, "T", []byte("A-RESP"))
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	// B (different tenant) and C (different method) must NOT see A's completed record.
	if _, ok, _ := store.Lookup(context.Background(), b); ok {
		t.Fatal("tenant B must not replay tenant A's response")
	}
	if _, ok, _ := store.Lookup(context.Background(), c); ok {
		t.Fatal("a different method must not replay")
	}
}

// TestEntDedup_BatchedGC drains a backlog larger than the batch and returns the total.
func TestEntDedup_BatchedGC(t *testing.T) {
	client := openEntDedup(t, "ent_dedup_gc")
	clk := &entClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := iamv1.NewEntDurableDedupStore(client,
		entrepo.WithEntDurableDedupClock(clk.Now),
		entrepo.WithEntDurableDedupGCBatch(10),
	)
	tx := iamv1.NewEntTxRunner(client)

	const n = 25
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		for i := 0; i < n; i++ {
			key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: fmt.Sprintf("r%d", i)}
			if _, _, e := store.Claim(ctx, key, "fp", time.Minute); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	clk.Advance(2 * time.Minute) // all expired

	deleted, err := store.GC(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if deleted != n {
		t.Fatalf("GC deleted %d, want %d", deleted, n)
	}
}
