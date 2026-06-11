package entrepo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
)

func TestEntRepository_Create_DelegatesToCreateFn(t *testing.T) {
	want := "created-value"
	called := false
	repo := &entrepo.EntRepository[string, string]{
		Create_: func(_ context.Context, entity string) (string, error) {
			called = true
			return want, nil
		},
		Get_: func(_ context.Context, _ string) (string, error) { return "", nil },
		List_: func(_ context.Context, _ persistence.ListOptions) ([]string, string, error) {
			return nil, "", nil
		},
		Update_: func(_ context.Context, _ string, entity string, _ ...string) (string, error) {
			return entity, nil
		},
		Delete_: func(_ context.Context, _ string) error { return nil },
	}

	got, err := repo.Create(context.Background(), "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Create_ was not called")
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEntRepository_Get_ReturnsErrNotFound(t *testing.T) {
	repo := &entrepo.EntRepository[string, string]{
		Create_: func(_ context.Context, entity string) (string, error) { return entity, nil },
		Get_: func(_ context.Context, _ string) (string, error) {
			return "", persistence.ErrNotFound
		},
		List_: func(_ context.Context, _ persistence.ListOptions) ([]string, string, error) {
			return nil, "", nil
		},
		Update_: func(_ context.Context, _ string, entity string, _ ...string) (string, error) {
			return entity, nil
		},
		Delete_: func(_ context.Context, _ string) error { return nil },
	}

	_, err := repo.Get(context.Background(), "missing-key")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestEntRepository_ImplementsRepository documents that the compile-time check
// in repository.go (var _ persistence.Repository[any, string] = ...) already
// enforces interface satisfaction at build time.
func TestEntRepository_ImplementsRepository(t *testing.T) {
	t.Log("interface satisfaction is enforced at compile time via var _ in repository.go")
}
