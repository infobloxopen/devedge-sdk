package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// widget is a trivial aggregate root for the in-memory tx + outbox tests.
type widget struct {
	ID   string
	Name string
}

// setup wires an in-memory aggregate repository, an outbox store, and a tx runner
// that enrolls BOTH — so a Publish inside Atomically shares the aggregate write's
// commit. Returns the publisher, the repo, the store, and the runner.
func setup() (*events.OutboxPublisher, *persistence.MemoryRepository[*widget, string], *persistence.MemoryOutboxStore, *persistence.MemoryTxRunner) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	return pub, repo, store, tx
}

// TestAC1_PublishCommitsAtomically proves that a Publish inside Atomically commits
// the outbox row atomically with the aggregate change: after a successful tx the
// row is present.
func TestAC1_PublishCommitsAtomically(t *testing.T) {
	pub, repo, store, tx := setup()
	ctx := context.Background()

	err := tx.Atomically(ctx, func(ctx context.Context) error {
		if _, err := repo.Create(ctx, &widget{ID: "w1", Name: "alpha"}); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{ID: "e1", Type: "WidgetCreated", AggregateType: "Widget", AggregateID: "w1"})
	})
	if err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	// The aggregate change committed.
	if _, err := repo.Get(ctx, "w1"); err != nil {
		t.Fatalf("widget must be committed: %v", err)
	}
	// The outbox row committed with it.
	pending := store.Pending()
	if len(pending) != 1 || pending[0] != "e1" {
		t.Fatalf("expected one pending outbox row e1, got %v", pending)
	}
}

// TestAC1_RollbackDiscardsEvent proves the transactional-outbox guarantee: when the
// aggregate transaction rolls back, the Published event is discarded too — NO orphan
// outbox row. This is the whole point (no dual write).
func TestAC1_RollbackDiscardsEvent(t *testing.T) {
	pub, repo, store, tx := setup()
	ctx := context.Background()

	boom := errors.New("boom")
	err := tx.Atomically(ctx, func(ctx context.Context) error {
		if _, err := repo.Create(ctx, &widget{ID: "w2", Name: "beta"}); err != nil {
			return err
		}
		if err := pub.Publish(ctx, events.Event{ID: "e2", Type: "WidgetCreated", AggregateType: "Widget", AggregateID: "w2"}); err != nil {
			return err
		}
		return boom // force rollback AFTER the publish
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}

	// The aggregate change rolled back...
	if _, err := repo.Get(ctx, "w2"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("widget must NOT be committed after rollback, got %v", err)
	}
	// ...and so did the event: no orphan outbox row.
	if pending := store.Pending(); len(pending) != 0 {
		t.Fatalf("rollback must discard the event (no orphan outbox row), got %v", pending)
	}
	if all := store.All(); len(all) != 0 {
		t.Fatalf("rollback must leave the outbox empty, got %d rows", len(all))
	}
}

// TestAC4_PublishOutsideTxErrors proves D-1: a Publish NOT inside Atomically returns
// persistence.ErrNoTransaction (the safe choice — refuse rather than write a
// non-atomic outbox row).
func TestAC4_PublishOutsideTxErrors(t *testing.T) {
	pub, _, store, _ := setup()
	ctx := context.Background()

	err := pub.Publish(ctx, events.Event{ID: "e3", Type: "WidgetCreated", AggregateType: "Widget", AggregateID: "w3"})
	if !errors.Is(err, persistence.ErrNoTransaction) {
		t.Fatalf("Publish outside a tx must return ErrNoTransaction, got %v", err)
	}
	if all := store.All(); len(all) != 0 {
		t.Fatalf("a refused Publish must not write any row, got %d", len(all))
	}
}

// TestPublishAssignsDefaults proves Publish fills an empty id with a fresh UUID so
// every event has an idempotency key.
func TestPublishAssignsDefaults(t *testing.T) {
	pub, _, store, tx := setup()
	ctx := context.Background()
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{Type: "T", AggregateType: "Widget", AggregateID: "w"})
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}
	all := store.All()
	if len(all) != 1 || all[0].ID == "" {
		t.Fatalf("Publish must assign a non-empty id, got %+v", all)
	}
}
