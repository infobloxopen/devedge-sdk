package events_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// counter is a trivial aggregate whose Count the handler increments transactionally.
type counter struct {
	ID    string
	Count int
}

// TestAC2_ConcurrentDoubleClaimAppliesEffectOnce drives two dispatchers concurrently
// against the SAME event with a near-zero (lapsed) lease so BOTH claim it before
// either records the idempotency marker — the worst-case at-least-once double-fire
// the spec's failure-mode list calls out. The handler does a transactional
// read-modify-write increment; the unique in-tx marker must serialize the apply so
// the increment commits exactly once even though the event is delivered twice
// (AC-2). Run with -race to catch the data-race form of the bug too.
func TestAC2_ConcurrentDoubleClaimAppliesEffectOnce(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(c *counter) string { return c.ID })
	store := persistence.NewMemoryOutboxStore(1) // ~1ns lease → re-claim allowed immediately
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	if _, err := repo.Create(ctx, &counter{ID: "c", Count: 0}); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "dup", Type: "Thing"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var attempts int64
	idem := events.NewMemoryIdempotencyStore() // SHARED idempotency store
	gate := make(chan struct{})
	mk := func() *events.Dispatcher {
		d := events.NewDispatcher(store, tx, idem)
		// Transactional read-modify-write: increment the counter in the handler's tx.
		// A duplicate apply that loses the marker race rolls this increment back.
		d.Subscribe("Thing", "incr", func(ctx context.Context, evt events.Event) error {
			atomic.AddInt64(&attempts, 1)
			<-gate // hold both handlers open until both have claimed+entered
			c, err := repo.Get(ctx, "c")
			if err != nil {
				return err
			}
			c.Count++
			_, err = repo.Update(ctx, "c", c)
			return err
		})
		return d
	}
	d1, d2 := mk(), mk()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = d1.RunOnce(ctx, 10) }()
	go func() { defer wg.Done(); _, _ = d2.RunOnce(ctx, 10) }()
	close(gate)
	wg.Wait()

	// The handler may be ATTEMPTED twice (at-least-once) — that is fine — but its
	// transactional effect must land exactly once: the counter is incremented once.
	got, err := repo.Get(ctx, "c")
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if got.Count != 1 {
		t.Fatalf("the handler's transactional effect must commit exactly once across a concurrent double-claim, Count=%d (attempts=%d)", got.Count, atomic.LoadInt64(&attempts))
	}
}
