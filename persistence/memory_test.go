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
