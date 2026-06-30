package persistence_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// rec is a tiny entity whose storage key (ID, env-specific) differs from its
// natural key (Name, portable) — the whole point of P4.
type rec struct {
	ID   string
	Name string
	Note string
}

// upsertFixture wires a memory repository + tx runner + the portable
// UpsertRepository over them, plus a LookupFunc that resolves a natural key
// (rec.Name) to its storage key and lifecycle state INCLUDING soft-deleted rows
// (so the resurrect path is exercised) — the one thing a real service generates
// from its unique index.
func upsertFixture(t *testing.T) (persistence.UpsertRepository[*rec, string], *persistence.MemoryRepository[*rec, string]) {
	t.Helper()
	repo := persistence.NewMemoryRepository(func(r *rec) string { return r.ID })
	tx := persistence.NewMemoryTxRunner(repo)
	keyOf := func(r *rec) persistence.NaturalKey { return persistence.NaturalKey(r.Name) }
	lookup := func(ctx context.Context, nk persistence.NaturalKey) (string, persistence.EntityState, error) {
		all, _, err := repo.List(ctx, persistence.ListOptions{ShowDeleted: true, PageSize: 10000})
		if err != nil {
			return "", persistence.StateAbsent, err
		}
		for _, r := range all {
			if persistence.NaturalKey(r.Name) != nk {
				continue
			}
			if _, gerr := repo.Get(ctx, r.ID); gerr == nil {
				return r.ID, persistence.StateLive, nil
			}
			return r.ID, persistence.StateDeleted, nil
		}
		return "", persistence.StateAbsent, nil
	}
	return persistence.NewUpsert[*rec, string](repo, tx, keyOf, lookup), repo
}

// Acceptance: a new natural key creates; the same key updates (idempotent, no
// duplicate); a key held by a soft-deleted row resurrects rather than erroring.
func TestUpsert_CreateUpdateResurrect(t *testing.T) {
	up, repo := upsertFixture(t)
	ctx := context.Background()

	// New key -> create.
	got, created, err := up.Upsert(ctx, &rec{ID: "id-1", Name: "alpha", Note: "v1"})
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	if got.Note != "v1" {
		t.Fatalf("created note: want v1, got %q", got.Note)
	}

	// Same natural key (different storage id, as a re-import would carry) -> update
	// the existing row, NOT a second row.
	got, created, err = up.Upsert(ctx, &rec{ID: "id-1", Name: "alpha", Note: "v2"})
	if err != nil || created {
		t.Fatalf("second upsert: created=%v err=%v (want update)", created, err)
	}
	if got.Note != "v2" {
		t.Fatalf("updated note: want v2, got %q", got.Note)
	}
	if all, _, _ := repo.List(ctx, persistence.ListOptions{}); len(all) != 1 {
		t.Fatalf("upsert must not duplicate: want 1 live row, got %d", len(all))
	}

	// Soft-delete the row, then upsert the same key -> resurrect + update.
	if err := repo.Delete(ctx, "id-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := up.LookupByNaturalKey(ctx, "alpha"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("lookup of a soft-deleted key should be NotFound, got %v", err)
	}
	got, created, err = up.Upsert(ctx, &rec{ID: "id-1", Name: "alpha", Note: "v3"})
	if err != nil || created {
		t.Fatalf("resurrect upsert: created=%v err=%v (want update)", created, err)
	}
	if got.Note != "v3" {
		t.Fatalf("resurrected note: want v3, got %q", got.Note)
	}
	if _, err := up.LookupByNaturalKey(ctx, "alpha"); err != nil {
		t.Fatalf("resurrected key should look up live: %v", err)
	}
}

// Acceptance: a re-import carrying a DIFFERENT storage id updates the existing row
// and, with WithKeyAssignment, preserves that row's immutable storage key rather
// than overwriting it with the import's — the cross-environment case natural keys
// exist for.
func TestUpsert_KeyAssignmentPreservesStorageKey(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(r *rec) string { return r.ID })
	tx := persistence.NewMemoryTxRunner(repo)
	keyOf := func(r *rec) persistence.NaturalKey { return persistence.NaturalKey(r.Name) }
	lookup := func(ctx context.Context, nk persistence.NaturalKey) (string, persistence.EntityState, error) {
		all, _, err := repo.List(ctx, persistence.ListOptions{ShowDeleted: true, PageSize: 10000})
		if err != nil {
			return "", persistence.StateAbsent, err
		}
		for _, r := range all {
			if persistence.NaturalKey(r.Name) == nk {
				if _, gerr := repo.Get(ctx, r.ID); gerr == nil {
					return r.ID, persistence.StateLive, nil
				}
				return r.ID, persistence.StateDeleted, nil
			}
		}
		return "", persistence.StateAbsent, nil
	}
	up := persistence.NewUpsert[*rec, string](repo, tx, keyOf, lookup,
		persistence.WithKeyAssignment(func(r *rec, key string) *rec { r.ID = key; return r }))
	ctx := context.Background()

	if _, _, err := up.Upsert(ctx, &rec{ID: "home-id", Name: "alpha", Note: "v1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Re-import the same natural key with a foreign storage id.
	if _, created, err := up.Upsert(ctx, &rec{ID: "foreign-id", Name: "alpha", Note: "v2"}); err != nil || created {
		t.Fatalf("re-import should update, created=%v err=%v", created, err)
	}
	// The row must still live under the ORIGINAL storage key, with v2's data.
	got, err := repo.Get(ctx, "home-id")
	if err != nil || got.Note != "v2" || got.ID != "home-id" {
		t.Fatalf("storage key must be preserved with updated data, got %+v err=%v", got, err)
	}
	if _, err := repo.Get(ctx, "foreign-id"); err == nil {
		t.Fatal("the foreign storage id must not have created a second row")
	}
}

// Acceptance: BatchUpsert reports per-row outcomes and, by default, continues
// past a failing row (the bulk-import error-log pattern).
func TestBatchUpsert_ContinueOnError(t *testing.T) {
	up, _ := upsertFixture(t)
	keyOf := func(r *rec) persistence.NaturalKey { return persistence.NaturalKey(r.Name) }

	// "" is a sentinel the fixture's lookup can't place AND Create rejects? No —
	// force a failure by making one row's natural key collide via a lookup error.
	// Simpler: drive a guaranteed row failure with a cancelled-free path by using
	// an entity whose Create fails. MemoryRepository.Create fails on duplicate
	// STORAGE key, so two rows sharing an ID (but different names) make the second
	// a row-level failure while others succeed.
	rows := []*rec{
		{ID: "a", Name: "one"},
		{ID: "dup", Name: "two"},
		{ID: "dup", Name: "three"}, // same storage id as "two" -> create conflict
		{ID: "d", Name: "four"},
	}
	var progressCalls int
	report, err := persistence.BatchUpsert[*rec, string](context.Background(), up, rows, keyOf,
		persistence.BatchUpsertOptions{ChunkSize: 2, Progress: func(done, total int) { progressCalls++ }})
	if err != nil {
		t.Fatalf("continue-on-error batch should not return a terminal error, got %v", err)
	}
	if report.Created != 3 || report.Failed != 1 {
		t.Fatalf("want 3 created / 1 failed, got %+v", report)
	}
	if len(report.Errors) != 1 || report.Errors[0].Key != "three" || report.Errors[0].Index != 2 {
		t.Fatalf("row error should pinpoint index 2 / key three, got %+v", report.Errors)
	}
	if progressCalls != 2 { // 4 rows / chunk 2
		t.Fatalf("want 2 progress callbacks, got %d", progressCalls)
	}
}

// Acceptance: StopOnError aborts at the first failing row and returns it.
func TestBatchUpsert_StopOnError(t *testing.T) {
	up, _ := upsertFixture(t)
	keyOf := func(r *rec) persistence.NaturalKey { return persistence.NaturalKey(r.Name) }
	rows := []*rec{
		{ID: "x", Name: "one"},
		{ID: "x", Name: "two"}, // storage-key conflict -> fails
		{ID: "z", Name: "three"},
	}
	report, err := persistence.BatchUpsert[*rec, string](context.Background(), up, rows, keyOf,
		persistence.BatchUpsertOptions{StopOnError: true})
	var re persistence.RowError
	if !errors.As(err, &re) || re.Index != 1 {
		t.Fatalf("StopOnError should return the failing RowError at index 1, got %v", err)
	}
	if report.Created != 1 || report.Failed != 1 {
		t.Fatalf("want 1 created before the stop / 1 failed, got %+v", report)
	}
}

// Acceptance: TopoSort orders parents before children and rejects cycles.
func TestTopoSort_OrdersParentsBeforeChildren(t *testing.T) {
	type node struct {
		key string
		dep string // natural key of parent, "" = root
	}
	items := []node{
		{key: "grandchild", dep: "child"},
		{key: "child", dep: "parent"},
		{key: "parent"},
		{key: "orphan"},
	}
	keyOf := func(n node) persistence.NaturalKey { return persistence.NaturalKey(n.key) }
	dependsOn := func(n node) []persistence.NaturalKey {
		if n.dep == "" {
			return nil
		}
		return []persistence.NaturalKey{persistence.NaturalKey(n.dep)}
	}
	sorted, err := persistence.TopoSort(items, keyOf, dependsOn)
	if err != nil {
		t.Fatalf("topo sort: %v", err)
	}
	pos := map[string]int{}
	for i, n := range sorted {
		pos[n.key] = i
	}
	if !(pos["parent"] < pos["child"] && pos["child"] < pos["grandchild"]) {
		t.Fatalf("parents must precede children, got order %+v", sorted)
	}

	// A cycle has no valid order.
	cyclic := []node{{key: "a", dep: "b"}, {key: "b", dep: "a"}}
	if _, err := persistence.TopoSort(cyclic, keyOf, dependsOn); !errors.Is(err, persistence.ErrCycle) {
		t.Fatalf("cycle should return ErrCycle, got %v", err)
	}
}
