package cells

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTableCASLifecycle(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "routes.json")
	tbl := NewFileTable(path)
	ctx := context.Background()

	// Missing tenant → ErrNoRoute.
	if _, err := tbl.Get(ctx, "t1"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Get on empty: want ErrNoRoute, got %v", err)
	}

	// Create first route (expect zero).
	r1 := TenantRoute{TenantID: "t1", RouteEpoch: 1, ActiveCell: "a", State: StateActive}
	if err := tbl.CompareAndSet(ctx, TenantRoute{}, r1); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := tbl.Get(ctx, "t1")
	if err != nil || got.ActiveCell != "a" || got.RouteEpoch != 1 {
		t.Fatalf("Get after create: %+v err=%v", got, err)
	}

	// Re-create with zero expect must conflict (already present).
	if err := tbl.CompareAndSet(ctx, TenantRoute{}, r1); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("recreate: want ErrCASConflict, got %v", err)
	}

	// CAS with wrong expected state conflicts.
	bad := r1
	bad.State = StateDraining
	if err := tbl.CompareAndSet(ctx, TenantRoute{TenantID: "t1", RouteEpoch: 1, State: StateQuiescing}, bad); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("wrong-expect CAS: want ErrCASConflict, got %v", err)
	}

	// Valid CAS advances epoch+state.
	r2 := TenantRoute{TenantID: "t1", RouteEpoch: 3, ActiveCell: "b", State: StateActive}
	if err := tbl.CompareAndSet(ctx, r1, r2); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// Epoch regression rejected.
	rback := TenantRoute{TenantID: "t1", RouteEpoch: 2, ActiveCell: "a", State: StateActive}
	if err := tbl.CompareAndSet(ctx, r2, rback); !errors.Is(err, ErrEpochRegression) {
		t.Fatalf("regression: want ErrEpochRegression, got %v", err)
	}

	// List + Delete.
	all, err := tbl.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("List: %v len=%d", err, len(all))
	}
	if err := tbl.Delete(ctx, "t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := tbl.Get(ctx, "t1"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Get after delete: want ErrNoRoute, got %v", err)
	}
}

// TestFileTableCrossProcessWatch proves a change written through one handle is
// observed on another handle's watch — the CLI-writes / service-observes path.
func TestFileTableCrossProcessWatch(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "routes.json")
	writer := NewFileTable(path, WithPollInterval(20*time.Millisecond))
	reader := NewFileTable(path, WithPollInterval(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := reader.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	r := TenantRoute{TenantID: "t9", RouteEpoch: 1, ActiveCell: "cell-7", State: StateActive}
	if err := writer.CompareAndSet(ctx, TenantRoute{}, r); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.TenantID != "t9" || ev.Route.ActiveCell != "cell-7" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe cross-process route change")
	}

	// Deletion is observed too.
	if err := writer.Delete(ctx, "t9"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.TenantID != "t9" || !ev.Deleted {
			t.Fatalf("want delete event, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe cross-process delete")
	}
}
