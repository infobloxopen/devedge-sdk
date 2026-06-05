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
