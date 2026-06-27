package persistence_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// order is a tiny aggregate root for the memory-backend aggregate test: it owns a
// slice of items (members) and carries an etag (the aggregate version).
type order struct {
	ID     string
	Etag   string
	Status string
	Items  []*item
}

type item struct {
	ID      string
	OrderID string
	SKU     string
}

// errShipped models the AC-6 domain invariant ("no item once SHIPPED").
var errShipped = errors.New("cannot add an item to a SHIPPED order")

// Validate implements persistence.AggregateValidator (the D-7 convention hook).
func (o *order) Validate(_ context.Context) error {
	if o.Status == "SHIPPED" && len(o.Items) > 0 {
		return errShipped
	}
	return nil
}

// newOrderAggregate wires a memory AggregateRepository: the root rows live in one
// MemoryRepository, the member rows in another, and a MemoryTxRunner spans both so
// Save is atomic. SaveMembers does member-mutation tracking against the stored
// items.
func newOrderAggregate() (*persistence.MemoryAggregateRepository[*order, string], *persistence.MemoryRepository[*order, string], *persistence.MemoryRepository[*item, string]) {
	roots := persistence.NewMemoryRepository(func(o *order) string { return o.ID })
	items := persistence.NewMemoryRepository(func(i *item) string { return i.ID })
	tx := persistence.NewMemoryTxRunner(roots, items)

	loadMembers := func(ctx context.Context, root *order) (*order, error) {
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

	spec := persistence.AggregateSpec[*order, string]{
		Tx:          tx,
		RootRepo:    roots,
		KeyOf:       func(o *order) string { return o.ID },
		EtagOf:      func(o *order) string { return o.Etag },
		// Tx-aware etag read: Save re-validates the precondition INSIDE the tx, where
		// the runner already holds the write lock — GetETagForKeyTx respects that.
		LoadEtag: func(ctx context.Context, id string) (string, error) { return roots.GetETagForKeyTx(ctx, id), nil },
		LoadMembers: loadMembers,
		SaveMembers: func(ctx context.Context, root *order) (bool, error) {
			stored := &order{ID: root.ID}
			if _, err := loadMembers(ctx, stored); err != nil {
				return false, err
			}
			storedByID := map[string]*item{}
			for _, it := range stored.Items {
				storedByID[it.ID] = it
			}
			changed := false
			want := map[string]struct{}{}
			for _, it := range root.Items {
				want[it.ID] = struct{}{}
				it.OrderID = root.ID
				if _, ok := storedByID[it.ID]; !ok {
					if _, err := items.Create(ctx, it); err != nil {
						return false, err
					}
					changed = true
				}
			}
			for id := range storedByID {
				if _, keep := want[id]; !keep {
					if err := items.Delete(ctx, id); err != nil {
						return false, err
					}
					changed = true
				}
			}
			return changed, nil
		},
	}
	return persistence.NewMemoryAggregateRepository(spec), roots, items
}

// TestMemoryAggregate_RoundTripAndStaleEtag is F031 AC-3 on the memory backend.
func TestMemoryAggregate_RoundTripAndStaleEtag(t *testing.T) {
	ctx := context.Background()
	agg, roots, _ := newOrderAggregate()

	if _, err := roots.Create(ctx, &order{ID: "o1", Status: "OPEN"}); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	root, err := agg.Load(ctx, "o1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(root.Items) != 0 {
		t.Fatalf("new order should have no items, got %d", len(root.Items))
	}
	etagBefore := roots.GetETagForKey("o1")
	root.Etag = etagBefore // the caller holds the current aggregate version

	root.Items = append(root.Items, &item{ID: "i1", SKU: "ABC"})
	if _, err := agg.Save(ctx, root); err != nil {
		t.Fatalf("Save (add member): %v", err)
	}
	// The out-of-band root etag (the aggregate version) was bumped exactly once.
	if after := roots.GetETagForKey("o1"); after == etagBefore || after == "" {
		t.Fatalf("root etag must be bumped on a member change: before=%q after=%q", etagBefore, after)
	}

	reloaded, err := agg.Load(ctx, "o1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Items) != 1 || reloaded.Items[0].ID != "i1" {
		t.Fatalf("cluster must contain i1, got %+v", reloaded.Items)
	}

	// Stale aggregate version → ErrPreconditionFailed.
	reloaded.Etag = etagBefore
	reloaded.Items = append(reloaded.Items, &item{ID: "i2", SKU: "XYZ"})
	if _, err := agg.Save(ctx, reloaded); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("stale root etag must yield ErrPreconditionFailed, got %v", err)
	}
}

// TestMemoryAggregate_ConcurrentSaveLostUpdate is the F031 lost-update TOCTOU
// regression: two Saves that BOTH load root etag=v5 and BOTH add a member must NOT
// both commit. Optimistic concurrency requires exactly one to win and the other to
// fail with ErrPreconditionFailed — proving the precondition compare-and-set runs
// inside the serialized critical section, not before it. Run under -race.
func TestMemoryAggregate_ConcurrentSaveLostUpdate(t *testing.T) {
	ctx := context.Background()
	agg, roots, items := newOrderAggregate()
	if _, err := roots.Create(ctx, &order{ID: "o1", Status: "OPEN"}); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Both racers load the SAME current aggregate version (v5).
	v5 := roots.GetETagForKey("o1")

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	results := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			// Each racer holds the same loaded version and tries to add its own member.
			root := &order{ID: "o1", Status: "OPEN", Etag: v5}
			root.Items = []*item{{ID: "i" + string(rune('a'+i)), OrderID: "o1", SKU: "S"}}
			<-start // maximize the race window
			_, results[i] = agg.Save(ctx, root)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, persistence.ErrPreconditionFailed):
			// expected loser
		default:
			t.Fatalf("racer %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent Save may win the optimistic-concurrency race; got %d winners (a lost update means >1)", winners)
	}
	// Exactly one member was persisted (the winner's); no lost update silently
	// dropped a concurrent write into the same committed cluster.
	if all, _, _ := items.List(ctx, persistence.ListOptions{}); len(all) != 1 {
		t.Fatalf("exactly one member must be persisted by the single winning Save, got %d", len(all))
	}
}

// TestMemoryAggregate_ValidateRejectsSave is F031 AC-6 on the memory backend: the
// root Validate(ctx) invariant runs pre-persist and rejects the offending Save.
func TestMemoryAggregate_ValidateRejectsSave(t *testing.T) {
	ctx := context.Background()
	agg, roots, items := newOrderAggregate()

	if _, err := roots.Create(ctx, &order{ID: "o1", Status: "SHIPPED"}); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	root, _ := agg.Load(ctx, "o1")
	root.Status = "SHIPPED"
	root.Items = append(root.Items, &item{ID: "i1", SKU: "ABC"})

	if _, err := agg.Save(ctx, root); !errors.Is(err, errShipped) {
		t.Fatalf("SHIPPED order must reject the member-adding Save, got %v", err)
	}
	// Invariant rejected pre-persist: no member was written.
	if all, _, _ := items.List(ctx, persistence.ListOptions{}); len(all) != 0 {
		t.Fatalf("a rejected Save must not persist members, got %d", len(all))
	}
}

// TestMemoryAggregate_NoChangeNoEtagBump verifies the guard against a spurious
// etag bump: a Save that changes no member leaves the root etag untouched.
func TestMemoryAggregate_NoChangeNoEtagBump(t *testing.T) {
	ctx := context.Background()
	agg, roots, _ := newOrderAggregate()
	if _, err := roots.Create(ctx, &order{ID: "o1", Status: "OPEN"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	root, _ := agg.Load(ctx, "o1")
	before := roots.GetETagForKey("o1")
	if _, err := agg.Save(ctx, root); err != nil { // no member change
		t.Fatalf("Save: %v", err)
	}
	if after := roots.GetETagForKey("o1"); after != before {
		t.Fatalf("a no-op Save must not bump the etag: before=%q after=%q", before, after)
	}
}
