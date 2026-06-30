package events_test

import (
	"context"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// searchDoc is the kind of denormalized, search-optimised shape a service would
// feed an index: a subset of fields plus a denormalized reference (account_name)
// carried so search can display/sort by name with no runtime join (P10, search
// scope). It is NOT the entity — that is the point of a projection.
type searchDoc struct {
	Name        string `json:"name"`
	AccountName string `json:"account_name"`
}

func userSearchProjection(emitAll bool) events.Projection[*usr] {
	return events.Projection[*usr]{
		ResourceType: "search.user",
		NameOf:       func(u *usr) string { return "search/users/" + u.ID },
		Project: func(u *usr) (any, bool) {
			if !emitAll && strings.HasPrefix(u.Name, "draft:") {
				return nil, false // drafts are not indexable
			}
			return searchDoc{Name: u.Name, AccountName: "Acme Corp"}, true
		},
	}
}

func byType(evts []events.ChangeEvent, rt string) []events.ChangeEvent {
	var out []events.ChangeEvent
	for _, e := range evts {
		if e.ResourceType == rt {
			out = append(out, e)
		}
	}
	return out
}

// Acceptance: every CUD fans out to the entity's own change event PLUS the
// declared search projection — one feed, two shapes — both tenant-correct and
// atomic. The search projection carries the denormalized doc, not the entity.
func TestProjections_FanOutOnCUD(t *testing.T) {
	feed, store, tx := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{
		Projections: []events.Projection[*usr]{userSearchProjection(true)},
	})
	ctx := middleware.WithTenantID(context.Background(), "acme")

	if _, err := feed.Create(ctx, &usr{ID: "u1", Name: "Ann"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := feed.Update(ctx, "u1", &usr{ID: "u1", Name: "Ann B"}, "name"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := feed.Delete(ctx, "u1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := drainChanges(t, store, tx)
	entity := byType(got, "test.user")
	search := byType(got, "search.user")
	if len(entity) != 3 || len(search) != 3 {
		t.Fatalf("want 3 entity + 3 search events, got %d entity / %d search (total %d)", len(entity), len(search), len(got))
	}
	for _, ce := range search {
		if ce.Tenant != "acme" {
			t.Errorf("projection must carry the tenant, got %q", ce.Tenant)
		}
		if ce.ResourceName != "search/users/u1" {
			t.Errorf("projection name: want search/users/u1, got %q", ce.ResourceName)
		}
	}
	// The projected CREATE/UPDATE carry the search doc (denormalized account_name);
	// the DELETE carries only identity so the index drops the doc.
	if !strings.Contains(string(search[0].After), "Acme Corp") {
		t.Errorf("projection after-image should be the search doc, got %s", search[0].After)
	}
	if search[2].Change != events.ChangeDelete || search[2].After != nil {
		t.Errorf("projected delete should be a tombstone with no after-image, got %s / %s", search[2].Change, search[2].After)
	}
}

// Acceptance: a projection that elects not to emit (emit=false) is skipped — the
// entity event still fires, but the index is not told about a non-indexable row.
func TestProjections_SkipWhenNotIndexable(t *testing.T) {
	feed, store, tx := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{
		Projections: []events.Projection[*usr]{userSearchProjection(false)},
	})
	ctx := middleware.WithTenantID(context.Background(), "acme")

	if _, err := feed.Create(ctx, &usr{ID: "d1", Name: "draft:wip"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := drainChanges(t, store, tx)
	if n := len(byType(got, "test.user")); n != 1 {
		t.Fatalf("entity event should still fire, got %d", n)
	}
	if n := len(byType(got, "search.user")); n != 0 {
		t.Fatalf("a non-indexable row must not be projected, got %d search events", n)
	}
}

// Acceptance: ProjectExisting backfills the index from the repository's current
// live state — the bootstrap half of "reactive + bootstrap".
func TestProjectExisting_Backfill(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(u *usr) string { return u.ID })
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	ctx := middleware.WithTenantID(context.Background(), "acme")

	for _, u := range []*usr{{ID: "u1", Name: "Ann"}, {ID: "u2", Name: "Bo"}, {ID: "u3", Name: "Cy"}} {
		if _, err := repo.Create(ctx, u); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := events.ProjectExisting[*usr, string](ctx, repo, tx, pub, userSearchProjection(true),
		func(u *usr) string { return "search/users/" + u.ID }, 2)
	if err != nil {
		t.Fatalf("ProjectExisting: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 docs backfilled, got %d", n)
	}
	got := drainChanges(t, store, tx)
	search := byType(got, "search.user")
	if len(search) != 3 {
		t.Fatalf("want 3 projected CREATE events, got %d", len(search))
	}
	for _, ce := range search {
		if ce.Change != events.ChangeCreate || !strings.Contains(string(ce.After), "Acme Corp") {
			t.Errorf("backfill event should be a CREATE carrying the search doc, got %s / %s", ce.Change, ce.After)
		}
	}
}

// Acceptance: a backfill without a tenant on context fails closed — the same leak
// guard the live feed enforces.
func TestProjectExisting_FailsClosedWithoutTenant(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(u *usr) string { return u.ID })
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)

	_, err := events.ProjectExisting[*usr, string](context.Background(), repo, tx, pub,
		userSearchProjection(true), nil, 10)
	if err == nil || !strings.Contains(err.Error(), "requires a tenant") {
		t.Fatalf("want fail-closed tenant error, got %v", err)
	}
}
