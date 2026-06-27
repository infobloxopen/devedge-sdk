package iamv1_test

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent/enttest"
	entoutbox "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/outbox"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

const eventUserSuspended = "iam.v1.UserSuspended"

// suspendUser is the worked-example write (F032 AC-3): in ONE transaction it
// performs a real User mutation (marking the user suspended via its display_name)
// AND publishes a UserSuspended event into the outbox — so the event commits
// atomically with the user change. The cross-aggregate reaction (revoking the
// user's API keys, a SEPARATE aggregate) is NOT done here; it happens later, on
// dispatch — eventual consistency, not one big transaction.
func suspendUser(ctx context.Context, tx persistence.TxRunner, users persistence.Repository[*iamv1.User, string], pub events.Publisher, userID string) error {
	return tx.Atomically(ctx, func(ctx context.Context) error {
		u, err := users.Get(ctx, userID)
		if err != nil {
			return err
		}
		u.DisplayName = "[suspended] " + u.GetDisplayName()
		if _, err := users.Update(ctx, userID, u, "display_name"); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{
			Type:          eventUserSuspended,
			AggregateType: "User",
			AggregateID:   userID,
			Payload:       []byte(userID),
		})
	})
}

// revokeKeysHandler is the registered reaction to UserSuspended. It runs in its OWN
// aggregate transaction (the dispatcher wraps it), and revokes every API key that
// references the suspended user — a write to a DIFFERENT aggregate (ApiKey). Here
// "revoke" is deletion of the referencing api-key rows.
func revokeKeysHandler(client *ent.Client, apiKeys persistence.Repository[*iamv1.ApiKey, string]) events.Handler {
	return func(ctx context.Context, evt events.Event) error {
		userID := string(evt.Payload)
		keys, _, err := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "` + userID + `"`, PageSize: 1000})
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := apiKeys.Delete(ctx, k.GetId()); err != nil && !errors.Is(err, persistence.ErrNotFound) {
				return err
			}
		}
		return nil
	}
}

// TestAC3_UserSuspendedRevokesKeysEventually proves the F032 worked example:
// suspending a user emits UserSuspended in the suspend tx; the user's API keys are
// revoked only AFTER dispatch, in a SEPARATE aggregate transaction — eventual
// consistency across aggregates, not one cross-aggregate transaction.
func TestAC3_UserSuspendedRevokesKeysEventually(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_events_ac3?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserEntRepository(client)
	apiKeys := iamv1.NewApiKeyEntRepository(client, enc)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	// Seed a user and two api-keys that reference it.
	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key1: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k2", UserId: "u1", KeyValue: "tok2", KeyPrefix: "k2"}); err != nil {
		t.Fatalf("seed key2: %v", err)
	}

	// Suspend the user. This commits the user change + the outbox event atomically.
	if err := suspendUser(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspendUser: %v", err)
	}

	// The user is suspended (mutation committed)...
	got, err := users.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.GetDisplayName() != "[suspended] Alice" {
		t.Fatalf("user must be suspended in the suspend tx, got %q", got.GetDisplayName())
	}
	// ...but the keys are NOT YET revoked: the reaction has not been dispatched.
	if k1Keys, _, _ := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10}); len(k1Keys) != 2 {
		t.Fatalf("keys must still exist before dispatch (eventual consistency), got %d", len(k1Keys))
	}

	// Dispatch: the registered handler revokes the keys in its OWN aggregate tx.
	d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe(eventUserSuspended, "revoke-api-keys", revokeKeysHandler(client, apiKeys))
	delivered, err := d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("the UserSuspended event must be delivered once, got %d", delivered)
	}

	// AFTER dispatch the keys are revoked — the cross-aggregate reaction completed
	// in a separate transaction (eventual consistency demonstrated).
	remaining, _, err := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10})
	if err != nil {
		t.Fatalf("list keys after dispatch: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("the user's API keys must be revoked after dispatch, %d survived", len(remaining))
	}
}

// TestAC1_Ent_RollbackDiscardsOutboxRow proves AC-1 on the ENT backend: a Publish
// inside Atomically writes the outbox row THROUGH the *ent.Tx, so a rollback of the
// aggregate tx discards the outbox row too — no orphan row on a separate connection.
func TestAC1_Ent_RollbackDiscardsOutboxRow(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_events_ac1?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")

	users := iamv1.NewUserEntRepository(client)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	boom := errors.New("boom")
	err := tx.Atomically(ctx, func(ctx context.Context) error {
		u, _ := users.Get(ctx, "u1")
		u.DisplayName = "changed"
		if _, err := users.Update(ctx, "u1", u, "display_name"); err != nil {
			return err
		}
		if err := pub.Publish(ctx, events.Event{ID: "evt-rollback", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")}); err != nil {
			return err
		}
		return boom // force rollback after both writes
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}

	// The user change rolled back...
	if got, _ := users.Get(ctx, "u1"); got.GetDisplayName() == "changed" {
		t.Fatal("user change must have rolled back")
	}
	// ...and so did the outbox row: the table has no orphan.
	n, err := client.Outbox.Query().Where(entoutbox.ID("evt-rollback")).Count(ctx)
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("rollback must discard the outbox row (atomic enlist), found %d", n)
	}

	// A committed Publish, by contrast, leaves exactly one row.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "evt-commit", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")})
	}); err != nil {
		t.Fatalf("committed publish: %v", err)
	}
	n, _ = client.Outbox.Query().Where(entoutbox.ID("evt-commit")).Count(ctx)
	if n != 1 {
		t.Fatalf("a committed Publish must leave exactly one outbox row, found %d", n)
	}
}

// TestAC4_Ent_PublishOutsideTxErrors proves D-1 on the ent backend: Publish without
// an enclosing Atomically returns ErrNoTransaction and writes nothing.
func TestAC4_Ent_PublishOutsideTxErrors(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_events_ac4?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	err := pub.Publish(ctx, events.Event{ID: "no-tx", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1"})
	if !errors.Is(err, persistence.ErrNoTransaction) {
		t.Fatalf("Publish outside a tx must return ErrNoTransaction, got %v", err)
	}
	n, _ := client.Outbox.Query().Count(ctx)
	if n != 0 {
		t.Fatalf("a refused Publish must write no outbox row, found %d", n)
	}
}

// TestTenantIsolation_DispatchScopesHandlerToEventTenant proves the dispatcher runs
// a handler in the EVENT's tenant context, not the dispatcher's. The outbox has no
// TenantMixin so a background poller claims across all tenants — but a tenant-scoped
// repository (User/ApiKey) reads middleware.TenantIDFromContext to filter its writes.
// If the handler ran on the poller's (empty) tenant, the ApiKey List/Delete would be
// UNSCOPED and revoke EVERY tenant's keys for that user id — a cross-tenant write.
// Here acme and globex both have a user "u1" with keys; suspending acme's u1 (event
// account_id=acme) and dispatching with a NO-tenant background ctx must revoke ONLY
// acme's keys and leave globex's intact.
func TestTenantIsolation_DispatchScopesHandlerToEventTenant(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_events_tenant?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserEntRepository(client)
	apiKeys := iamv1.NewApiKeyEntRepository(client, enc)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	acme := tenantCtx("acme")
	globex := tenantCtx("globex")

	// acme has user "u1"; BOTH tenants have an api-key referencing user_id "u1"
	// (the User id is a global PK so it cannot be reused; the api-key's user_id is
	// the field the revoke handler filters on, and it collides across tenants on
	// purpose — an UNSCOPED revoke would delete both).
	if _, err := users.Create(acme, &iamv1.User{Id: "u1", Email: "u1@acme.test"}); err != nil {
		t.Fatalf("seed acme user: %v", err)
	}
	if _, err := apiKeys.Create(acme, &iamv1.ApiKey{Id: "acme-key", UserId: "u1", KeyValue: "v", KeyPrefix: "acme-key"}); err != nil {
		t.Fatalf("seed acme key: %v", err)
	}
	if _, err := apiKeys.Create(globex, &iamv1.ApiKey{Id: "globex-key", UserId: "u1", KeyValue: "v", KeyPrefix: "globex-key"}); err != nil {
		t.Fatalf("seed globex key: %v", err)
	}

	// Suspend acme's u1: the event carries account_id=acme (filled from the suspend ctx).
	if err := suspendUser(acme, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspendUser: %v", err)
	}

	// Dispatch with a BACKGROUND ctx that has NO tenant — modelling a real poller.
	bg := context.Background()
	d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe(eventUserSuspended, "revoke-api-keys", revokeKeysHandler(client, apiKeys))
	if _, err := d.RunOnce(bg, 10); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// acme's key is revoked...
	if acmeKeys, _, _ := apiKeys.List(acme, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10}); len(acmeKeys) != 0 {
		t.Fatalf("acme's keys must be revoked, %d survived", len(acmeKeys))
	}
	// ...but globex's key is UNTOUCHED — no cross-tenant revoke.
	globexKeys, _, err := apiKeys.List(globex, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10})
	if err != nil {
		t.Fatalf("list globex keys: %v", err)
	}
	if len(globexKeys) != 1 {
		t.Fatalf("globex's key must NOT be revoked by acme's event (cross-tenant leak), %d remain", len(globexKeys))
	}
}
