package persistence

import (
	"context"
	"errors"
	"testing"
)

type zone struct {
	ID   string
	Name string
}

func TestMemoryRepository_SoftDelete(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })

	// Create two items; z1 will be soft-deleted, z2 stays live.
	if _, err := r.Create(ctx, zone{ID: "z1", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "z2", Name: "b"}); err != nil {
		t.Fatal(err)
	}

	// Delete z1 (soft).
	if err := r.Delete(ctx, "z1"); err != nil {
		t.Fatal(err)
	}

	// Get on soft-deleted → ErrNotFound.
	if _, err := r.Get(ctx, "z1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}

	// List default excludes soft-deleted.
	items, _, err := r.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "z2" {
		t.Fatalf("List default: want [z2], got %+v", items)
	}

	// List with ShowDeleted includes soft-deleted.
	all, _, err := r.List(ctx, ListOptions{ShowDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List ShowDeleted: want 2, got %d", len(all))
	}

	// Second Delete → ErrNotFound.
	if err := r.Delete(ctx, "z1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: want ErrNotFound, got %v", err)
	}

	// Undelete on a live item → ErrNotFound.
	if _, err := r.Undelete(ctx, "z2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Undelete live: want ErrNotFound, got %v", err)
	}

	// Undelete restores z1.
	restored, err := r.Undelete(ctx, "z1")
	if err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	if restored.ID != "z1" {
		t.Fatalf("Undelete returned wrong item: %+v", restored)
	}

	// After Undelete, Get and List default include z1 again.
	if _, err := r.Get(ctx, "z1"); err != nil {
		t.Fatalf("Get after Undelete: %v", err)
	}
	after, _, _ := r.List(ctx, ListOptions{})
	if len(after) != 2 {
		t.Fatalf("List after Undelete: want 2, got %d", len(after))
	}

	// Undelete on a non-existent key → ErrNotFound.
	if _, err := r.Undelete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Undelete missing: want ErrNotFound, got %v", err)
	}
}

func TestMemoryRepository_BatchGet_Success(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "b", Name: "beta"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.BatchGet(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatalf("BatchGet: unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("BatchGet: want 2 items, got %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("BatchGet: wrong order: %+v", got)
	}
}

func TestMemoryRepository_BatchGet_EmptyKeys(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })

	got, err := r.BatchGet(ctx, []string{})
	if err != nil {
		t.Fatalf("BatchGet empty: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BatchGet empty: want 0 items, got %d", len(got))
	}
}

func TestMemoryRepository_BatchGet_MissingKey(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.BatchGet(ctx, []string{"a", "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchGet missing: want ErrNotFound, got %v", err)
	}
}

func TestMemoryRepository_BatchGet_SoftDeletedKey(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	_, err := r.BatchGet(ctx, []string{"a"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchGet soft-deleted: want ErrNotFound, got %v", err)
	}
}

func TestMemoryRepository_BatchDelete_Success(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "b", Name: "beta"}); err != nil {
		t.Fatal(err)
	}

	if err := r.BatchDelete(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("BatchDelete: unexpected error: %v", err)
	}
	if _, err := r.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get a after BatchDelete: want ErrNotFound, got %v", err)
	}
	if _, err := r.Get(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get b after BatchDelete: want ErrNotFound, got %v", err)
	}
}

func TestMemoryRepository_BatchDelete_EmptyKeys(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}

	if err := r.BatchDelete(ctx, []string{}); err != nil {
		t.Fatalf("BatchDelete empty: unexpected error: %v", err)
	}
	if _, err := r.Get(ctx, "a"); err != nil {
		t.Fatalf("Get a after BatchDelete empty: want success, got %v", err)
	}
}

func TestMemoryRepository_BatchDelete_MissingKey(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}

	err := r.BatchDelete(ctx, []string{"a", "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchDelete missing: want ErrNotFound, got %v", err)
	}
	// "a" must NOT have been deleted (atomic failure).
	if _, err := r.Get(ctx, "a"); err != nil {
		t.Fatalf("Get a after failed BatchDelete: want success, got %v", err)
	}
}

func TestMemoryRepository_BatchDelete_AlreadyDeleted(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	err := r.BatchDelete(ctx, []string{"a"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchDelete already-deleted: want ErrNotFound, got %v", err)
	}
}

func TestMemoryRepository_BatchUpdate_Success(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "b", Name: "beta"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.BatchUpdate(ctx, []BatchUpdateItem[zone, string]{
		{Key: "a", Entity: zone{ID: "a", Name: "alpha2"}},
		{Key: "b", Entity: zone{ID: "b", Name: "beta2"}},
	})
	if err != nil {
		t.Fatalf("BatchUpdate: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[0].Name != "alpha2" || got[1].ID != "b" || got[1].Name != "beta2" {
		t.Fatalf("BatchUpdate: wrong result/order: %+v", got)
	}
	// Subsequent Get reflects the new values.
	a, _ := r.Get(ctx, "a")
	if a.Name != "alpha2" {
		t.Fatalf("Get after BatchUpdate: want alpha2, got %q", a.Name)
	}
}

func TestMemoryRepository_BatchUpdate_EmptyItems(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })

	got, err := r.BatchUpdate(ctx, []BatchUpdateItem[zone, string]{})
	if err != nil {
		t.Fatalf("BatchUpdate empty: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BatchUpdate empty: want 0 items, got %d", len(got))
	}
}

func TestMemoryRepository_BatchUpdate_MissingKey(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.BatchUpdate(ctx, []BatchUpdateItem[zone, string]{
		{Key: "a", Entity: zone{ID: "a", Name: "alpha2"}},
		{Key: "missing", Entity: zone{ID: "missing", Name: "x"}},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchUpdate missing: want ErrNotFound, got %v", err)
	}
	// Atomic: "a" must be unchanged.
	a, _ := r.Get(ctx, "a")
	if a.Name != "alpha" {
		t.Fatalf("BatchUpdate missing not atomic: a.Name = %q, want alpha", a.Name)
	}
}

func TestMemoryRepository_BatchUpdate_SoftDeletedKey(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })
	if _, err := r.Create(ctx, zone{ID: "a", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "b", Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	_, err := r.BatchUpdate(ctx, []BatchUpdateItem[zone, string]{
		{Key: "a", Entity: zone{ID: "a", Name: "alpha2"}},
		{Key: "b", Entity: zone{ID: "b", Name: "beta2"}},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("BatchUpdate soft-deleted: want ErrNotFound, got %v", err)
	}
	// Atomic: "a" must be unchanged.
	a, _ := r.Get(ctx, "a")
	if a.Name != "alpha" {
		t.Fatalf("BatchUpdate soft-deleted not atomic: a.Name = %q, want alpha", a.Name)
	}
}

func TestMemoryRepository(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(z zone) string { return z.ID })

	if _, err := r.Get(ctx, "z1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.Create(ctx, zone{ID: "z1", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, zone{ID: "z1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict on duplicate, got %v", err)
	}
	got, err := r.Get(ctx, "z1")
	if err != nil || got.Name != "a" {
		t.Fatalf("get: err=%v got=%+v", err, got)
	}
	items, _, err := r.List(ctx, ListOptions{})
	if err != nil || len(items) != 1 {
		t.Fatalf("list: err=%v n=%d", err, len(items))
	}
	if err := r.Delete(ctx, "z1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "z1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on second delete, got %v", err)
	}
}
