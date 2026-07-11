package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// usr is a plain (non-proto) entity for the decorator unit tests; it carries an
// explicit Marshal so the default protojson/redact path (exercised in the audit
// e2e against a real proto resource) is not needed here.
type usr struct {
	ID   string
	Name string
}

// changeFeedFixture wires a memory repository, outbox, single tx runner spanning
// both, a publisher, and the ChangeEmitting decorator over the repo — the
// minimal stack that makes a write and its change event one atomic unit.
func changeFeedFixture(t *testing.T, opts events.ChangeFeedOptions[*usr]) (
	persistence.Repository[*usr, string], *persistence.MemoryOutboxStore, *persistence.MemoryTxRunner,
) {
	t.Helper()
	repo := persistence.NewMemoryRepository(func(u *usr) string { return u.ID })
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	if opts.ResourceType == "" {
		opts.ResourceType = "test.user"
	}
	if opts.NameOf == nil {
		opts.NameOf = func(u *usr) string { return "users/" + u.ID }
	}
	if opts.Marshal == nil {
		opts.Marshal = func(u *usr) (json.RawMessage, error) { return json.Marshal(u) }
	}
	feed := events.ChangeEmitting[*usr, string](repo, tx, pub, opts)
	return feed, store, tx
}

// drainChanges delivers every committed outbox event through a dispatcher and
// returns the decoded change events in delivery order.
func drainChanges(t *testing.T, store *persistence.MemoryOutboxStore, tx *persistence.MemoryTxRunner) []events.ChangeEvent {
	t.Helper()
	var got []events.ChangeEvent
	d := events.NewDispatcher(store, persistence.NewMemoryOutboxCursorStore(), tx, events.NewMemoryIdempotencyStore())
	d.Subscribe(events.ChangeEventType, "capture", func(hctx context.Context, evt events.Event) error {
		ce, err := events.ChangeEventFromEvent(evt)
		if err != nil {
			return err
		}
		got = append(got, ce)
		return nil
	})
	if _, err := d.RunOnce(context.Background(), 100); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return got
}

func TestChangeEmitting_EmitsTypedEventPerMutation(t *testing.T) {
	feed, store, tx := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{})
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
	if len(got) != 3 {
		t.Fatalf("want 3 change events (create/update/delete), got %d", len(got))
	}

	create, update, del := got[0], got[1], got[2]
	for _, ce := range got {
		if ce.Tenant != "acme" {
			t.Errorf("every change must carry tenant=acme, got %q for %s", ce.Tenant, ce.Change)
		}
		if ce.ResourceType != "test.user" {
			t.Errorf("resource type: want test.user, got %q", ce.ResourceType)
		}
		if ce.ResourceName != "users/u1" {
			t.Errorf("resource name: want users/u1, got %q", ce.ResourceName)
		}
	}
	if create.Change != events.ChangeCreate || update.Change != events.ChangeUpdate || del.Change != events.ChangeDelete {
		t.Fatalf("change types out of order: %s/%s/%s", create.Change, update.Change, del.Change)
	}
	if !strings.Contains(string(create.After), "Ann") {
		t.Errorf("create after-image should contain the name, got %s", create.After)
	}
	if len(update.FieldMask) != 1 || update.FieldMask[0] != "name" {
		t.Errorf("update should carry the field mask [name], got %v", update.FieldMask)
	}
	if del.After != nil {
		t.Errorf("delete should have no after-image, got %s", del.After)
	}
}

func TestChangeEmitting_FailsClosedWithoutTenant(t *testing.T) {
	feed, store, _ := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{})

	// No tenant on context — the fail-closed default must reject the write.
	_, err := feed.Create(context.Background(), &usr{ID: "u1", Name: "Ann"})
	if err == nil || !strings.Contains(err.Error(), "requires a tenant") {
		t.Fatalf("want fail-closed tenant error, got %v", err)
	}
	// And the write must have rolled back with the rejected emit — no orphan row
	// and no persisted entity (atomicity of the guard).
	if all := store.All(); len(all) != 0 {
		t.Errorf("a rejected change must leave no outbox row, got %d", len(all))
	}
	if items, _, _ := feed.List(context.Background(), persistence.ListOptions{}); len(items) != 0 {
		t.Errorf("a rejected change must roll back the write, got %d items", len(items))
	}
}

func TestChangeEmitting_AllowMissingTenantOptOut(t *testing.T) {
	feed, store, tx := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{AllowMissingTenant: true})

	if _, err := feed.Create(context.Background(), &usr{ID: "sys", Name: "system"}); err != nil {
		t.Fatalf("Create with AllowMissingTenant should succeed: %v", err)
	}
	got := drainChanges(t, store, tx)
	if len(got) != 1 || got[0].Tenant != "" {
		t.Fatalf("want one tenantless change event, got %+v", got)
	}
}

func TestChangeEmitting_RollsBackWriteWhenEmitFails(t *testing.T) {
	boom := errors.New("marshal boom")
	feed, store, _ := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{
		Marshal: func(*usr) (json.RawMessage, error) { return nil, boom },
	})
	ctx := middleware.WithTenantID(context.Background(), "acme")

	_, err := feed.Create(ctx, &usr{ID: "u1", Name: "Ann"})
	if !errors.Is(err, boom) {
		t.Fatalf("Create should surface the emit failure, got %v", err)
	}
	// The write and the (failed) emit are one tx: both rolled back.
	if all := store.All(); len(all) != 0 {
		t.Errorf("no outbox row should survive a failed emit, got %d", len(all))
	}
	if items, _, _ := feed.List(ctx, persistence.ListOptions{}); len(items) != 0 {
		t.Errorf("the write must roll back with the failed emit, got %d items", len(items))
	}
}

// SEC-042-03 regression: the change event's tenant is ALWAYS the envelope's
// AccountID and NEVER the encoded payload's "tenant". A producer emitting a raw
// Event with an EMPTY AccountID and a forged payload {"tenant":"victim"} must not
// yield ce.Tenant == "victim" — the empty envelope tenant clears it so downstream
// fail-closed handling applies. Fails on the old code, which kept the payload
// tenant whenever AccountID was empty.
func TestChangeEventFromEvent_PayloadTenantNeverOverridesEmptyEnvelope(t *testing.T) {
	payload, err := json.Marshal(events.ChangeEvent{Tenant: "victim", ResourceType: "test.user"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evt := events.Event{
		ID:        "e1",
		Type:      events.ChangeEventType,
		AccountID: "", // empty authoritative envelope tenant
		Payload:   payload,
	}
	ce, err := events.ChangeEventFromEvent(evt)
	if err != nil {
		t.Fatalf("ChangeEventFromEvent: %v", err)
	}
	if ce.Tenant == "victim" {
		t.Fatalf("payload tenant must never become ce.Tenant: got %q", ce.Tenant)
	}
	if ce.Tenant != "" {
		t.Fatalf("empty envelope AccountID must clear the tenant, got %q", ce.Tenant)
	}
}

// The envelope's AccountID wins even when a (forged or stale) payload tenant
// disagrees: the tenant is always envelope-authoritative, not payload-derived.
func TestChangeEventFromEvent_EnvelopeTenantOverridesPayload(t *testing.T) {
	payload, err := json.Marshal(events.ChangeEvent{Tenant: "victim", ResourceType: "test.user"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evt := events.Event{
		ID:        "e2",
		Type:      events.ChangeEventType,
		AccountID: "acme", // authoritative envelope tenant
		Payload:   payload,
	}
	ce, err := events.ChangeEventFromEvent(evt)
	if err != nil {
		t.Fatalf("ChangeEventFromEvent: %v", err)
	}
	if ce.Tenant != "acme" {
		t.Fatalf("tenant must come from the envelope AccountID, got %q", ce.Tenant)
	}
}

func TestChangeEmitting_UndeleteEmitsUndelete(t *testing.T) {
	feed, store, tx := changeFeedFixture(t, events.ChangeFeedOptions[*usr]{})
	ctx := middleware.WithTenantID(context.Background(), "acme")

	if _, err := feed.Create(ctx, &usr{ID: "u1", Name: "Ann"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := feed.Delete(ctx, "u1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := feed.Undelete(ctx, "u1"); err != nil {
		t.Fatalf("Undelete: %v", err)
	}

	got := drainChanges(t, store, tx)
	if len(got) != 3 {
		t.Fatalf("want create/delete/undelete, got %d", len(got))
	}
	if got[2].Change != events.ChangeUndelete {
		t.Errorf("last change should be UNDELETE, got %s", got[2].Change)
	}
}
