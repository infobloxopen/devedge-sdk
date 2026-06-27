package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// This file is the cross-feature integration test the per-feature passes lacked:
// it composes ALL THREE seams in one flow — an aggregate Save (F031) that publishes
// a domain event (F032) inside the Save's own Atomically (F030) — and asserts the
// three effects (member write, root etag bump, outbox event) are ONE atomic unit:
// they all commit together and all roll back together. It then drives the dispatcher
// so the published event reaches a handler (the full F030→F031→F032 loop).
//
// The earlier tests cover the features in isolation or in pairs (events_test.go
// publishes alongside a bare repo write; aggregate_test.go saves without an event).
// None drove a Publish from INSIDE AggregateRepository.Save, where the event must
// enlist in the tx that Save itself opens — the re-entrancy the three features must
// compose through.

// ordr is the aggregate root for the composition test; it owns line items and
// carries the aggregate-version etag.
type ordr struct {
	ID    string
	Etag  string
	Items []*lineItem
}

type lineItem struct {
	ID      string
	OrderID string
}

// newOrderAggregateWithOutbox wires a memory AggregateRepository whose SaveMembers
// publishes an "ItemAdded" event through the SAME ctx tx Save opens. The runner
// enrolls the root repo, the item repo, AND the outbox store, so the Publish enlists
// in Save's transaction (the F030 ctx-propagation the three features share). failAfter,
// when set, makes SaveMembers return an error AFTER the publish so the whole tx —
// member write, etag bump, and outbox row — rolls back as one unit.
func newOrderAggregateWithOutbox(failAfter error) (
	*persistence.MemoryAggregateRepository[*ordr, string],
	*persistence.MemoryRepository[*ordr, string],
	*persistence.MemoryRepository[*lineItem, string],
	*persistence.MemoryOutboxStore,
	*persistence.MemoryTxRunner,
) {
	roots := persistence.NewMemoryRepository(func(o *ordr) string { return o.ID })
	items := persistence.NewMemoryRepository(func(i *lineItem) string { return i.ID })
	store := persistence.NewMemoryOutboxStore(0)
	// One runner spanning all three participants: this is what lets a Publish issued
	// from inside Save's tx share the aggregate write's commit.
	tx := persistence.NewMemoryTxRunner(roots, items, store)
	pub := events.NewOutboxPublisher(store)

	loadMembers := func(ctx context.Context, root *ordr) (*ordr, error) {
		all, _, err := items.List(ctx, persistence.ListOptions{PageSize: 1000})
		if err != nil {
			return nil, err
		}
		root.Items = nil
		for _, it := range all {
			if it.OrderID == root.ID {
				root.Items = append(root.Items, it)
			}
		}
		return root, nil
	}

	spec := persistence.AggregateSpec[*ordr, string]{
		Tx:          tx,
		RootRepo:    roots,
		KeyOf:       func(o *ordr) string { return o.ID },
		EtagOf:      func(o *ordr) string { return o.Etag },
		LoadEtag:    func(ctx context.Context, id string) (string, error) { return roots.GetETagForKeyTx(ctx, id), nil },
		LoadMembers: loadMembers,
		SaveMembers: func(ctx context.Context, root *ordr) (bool, error) {
			stored := &ordr{ID: root.ID}
			if _, err := loadMembers(ctx, stored); err != nil {
				return false, err
			}
			have := map[string]struct{}{}
			for _, it := range stored.Items {
				have[it.ID] = struct{}{}
			}
			changed := false
			for _, it := range root.Items {
				if _, ok := have[it.ID]; ok {
					continue
				}
				it.OrderID = root.ID
				if _, err := items.Create(ctx, it); err != nil {
					return false, err
				}
				// F032: publish a domain event through the SAME ctx tx Save opened.
				// It must enlist (RequireTx passes; the store is a runner participant),
				// so the event commits with the member write or rolls back with it.
				if err := pub.Publish(ctx, events.Event{
					ID:            "evt-" + it.ID,
					Type:          "ItemAdded",
					AggregateType: "Order",
					AggregateID:   root.ID,
					Payload:       []byte(it.ID),
				}); err != nil {
					return false, err
				}
				changed = true
			}
			if failAfter != nil {
				return changed, failAfter // force rollback AFTER the writes + publish
			}
			return changed, nil
		},
	}
	return persistence.NewMemoryAggregateRepository(spec), roots, items, store, tx
}

// TestIntegration_AggregateSavePublishesEventAtomically is the commit half of the
// full-stack composition: an aggregate Save (F031) that publishes an event (F032)
// inside one Atomically (F030) lands the member write, the single root etag bump,
// AND the outbox event together; a dispatcher then delivers the event to a handler.
func TestIntegration_AggregateSavePublishesEventAtomically(t *testing.T) {
	ctx := context.Background()
	agg, roots, items, store, tx := newOrderAggregateWithOutbox(nil)

	if _, err := roots.Create(ctx, &ordr{ID: "o1"}); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	root, err := agg.Load(ctx, "o1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	etagBefore := roots.GetETagForKey("o1")
	root.Etag = etagBefore
	root.Items = append(root.Items, &lineItem{ID: "i1"})

	if _, err := agg.Save(ctx, root); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// All three effects committed together:
	// 1) the member write,
	if all, _, _ := items.List(ctx, persistence.ListOptions{}); len(all) != 1 {
		t.Fatalf("the member write must commit, got %d items", len(all))
	}
	// 2) the single root etag bump (etag-as-aggregate-version),
	if after := roots.GetETagForKey("o1"); after == etagBefore || after == "" {
		t.Fatalf("the root etag must be bumped exactly once: before=%q after=%q", etagBefore, after)
	}
	// 3) the outbox event.
	if pending := store.Pending(); len(pending) != 1 || pending[0] != "evt-i1" {
		t.Fatalf("the published event must commit with the aggregate change, pending=%v", pending)
	}

	// The full loop: a dispatcher delivers the committed event to a handler in its
	// own Atomically (F030 again, on the same runner).
	got := ""
	d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe("ItemAdded", "record", func(hctx context.Context, evt events.Event) error {
		if err := persistence.RequireTx(hctx); err != nil {
			return err // the handler must run inside its own tx
		}
		got = string(evt.Payload)
		return nil
	})
	delivered, err := d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if delivered != 1 || got != "i1" {
		t.Fatalf("the event must reach the handler: delivered=%d payload=%q", delivered, got)
	}
	// F033 (append-only): delivery NEVER deletes the event row — it gets a single
	// terminal delivered-mark and then survives until a partition drop. The handler ran
	// (got=="i1"); the row count only ever grows (no per-row DELETE on the dispatch path).
	if all := store.All(); len(all) != 1 {
		t.Fatalf("append-only: the delivered event row must survive (count only grows), got %d", len(all))
	}
}

// TestIntegration_AggregateSaveRollbackDiscardsEvent is the rollback half — the load-
// bearing guarantee: a failure INSIDE Save's tx (after the member write and the
// Publish) discards ALL THREE effects as one unit. The member write reverts, the
// root etag is NOT bumped, and crucially the outbox event leaves NO orphan row (the
// dual-write the transactional outbox exists to prevent). This proves the event
// enlisted in the aggregate's own transaction, not a separate one.
func TestIntegration_AggregateSaveRollbackDiscardsEvent(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("invariant rejected after publish")
	agg, roots, items, store, _ := newOrderAggregateWithOutbox(boom)

	if _, err := roots.Create(ctx, &ordr{ID: "o1"}); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	root, _ := agg.Load(ctx, "o1")
	etagBefore := roots.GetETagForKey("o1")
	root.Etag = etagBefore
	root.Items = append(root.Items, &lineItem{ID: "i1"})

	if _, err := agg.Save(ctx, root); !errors.Is(err, boom) {
		t.Fatalf("Save must surface the forced failure, got %v", err)
	}

	// The member write rolled back...
	if all, _, _ := items.List(ctx, persistence.ListOptions{}); len(all) != 0 {
		t.Fatalf("a rolled-back Save must persist no members, got %d", len(all))
	}
	// ...the root etag was NOT bumped...
	if after := roots.GetETagForKey("o1"); after != etagBefore {
		t.Fatalf("a rolled-back Save must not bump the root etag: before=%q after=%q", etagBefore, after)
	}
	// ...and the outbox event left no orphan row (the whole point of the outbox).
	if all := store.All(); len(all) != 0 {
		t.Fatalf("a rolled-back Save must discard the published event (no orphan outbox row), got %d rows", len(all))
	}
}
