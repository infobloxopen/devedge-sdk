package iamv1_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
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
	// The idempotency store is the SQL-backed EntIdempotencyStore, so the marker
	// commits with the handler's revoke in one ent transaction (the ent path is now
	// genuinely transactional, not the in-memory store).
	d := events.NewDispatcher(store, iamv1.NewEntOutboxCursorStore(client), tx, iamv1.NewEntIdempotencyStore(client))
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

// TestBusE2E_UserSuspendedRevokesKeysThroughInMemoryBus re-wires the worked example
// through the FULL Phase-1 event-bus stack on the ent/SQL backend: suspending a user
// appends UserSuspended to the WRITE-ONLY outbox; a RELAY reads the outbox forward and
// publishes it to the in-memory BUS (events/membus); a CONSUMER subscribes to the bus and
// runs the revoke-keys handler in its own ent transaction with the SQL idempotency marker
// (exactly-once). The relay and consumer run as the two independent goroutines a real
// service wires — proving the example flows outbox → relay → in-memory-bus → consumer →
// handler, not through the synchronous Dispatcher façade.
func TestBusE2E_UserSuspendedRevokesKeysThroughInMemoryBus(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_bus_e2e?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserEntRepository(client)
	apiKeys := iamv1.NewApiKeyEntRepository(client, enc)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key1: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k2", UserId: "u1", KeyValue: "tok2", KeyPrefix: "k2"}); err != nil {
		t.Fatalf("seed key2: %v", err)
	}

	// Produce: suspend the user (user change + outbox event commit atomically).
	if err := suspendUser(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspendUser: %v", err)
	}

	// Wire the bus stack: membus + a consumer (ent idempotency store) + a leader-elected
	// relay reading the ent outbox via the ent cursor sidecar.
	bus := membus.New()
	consumer := events.NewConsumer(bus, tx, iamv1.NewEntIdempotencyStore(client))
	consumer.Subscribe(eventUserSuspended, "revoke-api-keys", revokeKeysHandler(client, apiKeys))
	relay := events.NewRelay(store, iamv1.NewEntOutboxCursorStore(client), bus)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relay.Run(runCtx, time.Millisecond, 10, nil) }()

	// AFTER the event flows through the bus the keys are revoked (eventual consistency).
	deadline := time.Now().Add(3 * time.Second)
	for {
		remaining, _, err := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10})
		if err != nil {
			t.Fatalf("list keys: %v", err)
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the user's API keys must be revoked after the event flows through the bus, %d survived", len(remaining))
		}
		time.Sleep(3 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	// The relay advanced its ent cursor past the event; the write-only outbox row survives.
	n, err := client.Outbox.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("write-only: the relay must never delete the outbox row, found %d", n)
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
	d := events.NewDispatcher(store, iamv1.NewEntOutboxCursorStore(client), tx, iamv1.NewEntIdempotencyStore(client))
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

// TestAC2_Ent_RecordRollsBackWithHandlerTx proves the gap-closer at the store
// level: the SQL-backed EntIdempotencyStore marker is GENUINELY part of the
// handler's ent transaction (the orphan-marker window the in-memory store leaves
// is now closed). A handler writes its effect AND records the marker, then fails;
// the marker rolls back WITH the effect, so it does not survive on the base client
// and a fresh Record of the same key succeeds. This is the property the in-memory
// store could not give the ent path: the marker and the effect are one atomic unit.
func TestAC2_Ent_RecordRollsBackWithHandlerTx(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_idem_rollback?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")

	users := iamv1.NewUserEntRepository(client)
	tx := iamv1.NewEntTxRunner(client)
	idem := iamv1.NewEntIdempotencyStore(client)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	const key = "evt-rollback\x1fhandler"
	boom := errors.New("handler failed after recording")

	// A handler that mutates the user (the effect) AND records the marker, then fails.
	err := tx.Atomically(ctx, func(ctx context.Context) error {
		u, _ := users.Get(ctx, "u1")
		u.DisplayName = "changed"
		if _, uerr := users.Update(ctx, "u1", u, "display_name"); uerr != nil {
			return uerr
		}
		if rerr := idem.Record(ctx, key); rerr != nil {
			return rerr
		}
		return boom // roll back BOTH the effect and the marker
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}

	// The effect rolled back...
	if got, _ := users.Get(ctx, "u1"); got.GetDisplayName() == "changed" {
		t.Fatal("the user change must have rolled back with the marker")
	}
	// ...and so did the marker: Seen is false, no orphan on the base client.
	if seen, _ := idem.Seen(ctx, key); seen {
		t.Fatal("a rolled-back Record must NOT leave a durable marker (orphan-marker window)")
	}
	// A fresh Record of the same key now succeeds — proving nothing leaked.
	if rerr := tx.Atomically(ctx, func(ctx context.Context) error {
		return idem.Record(ctx, key)
	}); rerr != nil {
		t.Fatalf("a fresh Record after a rolled-back attempt must succeed, got %v", rerr)
	}
	// And a SECOND Record of that now-committed key collides on the PK and maps to
	// ErrAlreadyApplied — the in-tx unique guard that serializes a double-apply.
	dupErr := tx.Atomically(ctx, func(ctx context.Context) error {
		return idem.Record(ctx, key)
	})
	if !errors.Is(dupErr, events.ErrAlreadyApplied) {
		t.Fatalf("duplicate Record must return ErrAlreadyApplied, got %v", dupErr)
	}
}

// TestAC2_Ent_ExactlyOnceUnderConcurrentDispatch is the functional (sqlite) twin
// of TestPG_Ent_ExactlyOnceUnderConcurrentDispatch: two dispatchers race to deliver
// the SAME event. The SQL-backed EntIdempotencyStore records the (event, handler)
// marker INSIDE the handler's ent transaction, so exactly one delivery commits its
// effect and the other is short-circuited (the Seen fast-path or the in-tx PK
// conflict). The invariant is exactly-once: the user's key is revoked once and
// stays revoked, and exactly one idempotency marker commits.
//
// SQLite serializes writers, so this proves the ent path is functionally correct;
// the genuine concurrent UNIQUE race is exercised on real Postgres in postgres_events_test.go.
func TestAC2_Ent_ExactlyOnceUnderConcurrentDispatch(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_idem_conc?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserEntRepository(client)
	apiKeys := iamv1.NewApiKeyEntRepository(client, enc)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if err := suspendUser(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	var mu sync.Mutex
	committedEffects := 0
	revoke := revokeKeysHandler(client, apiKeys)
	idem := iamv1.NewEntIdempotencyStore(client)
	// One SHARED cursor sidecar: both dispatchers read the same head event before either
	// advances, so they genuinely contend for the same delivery. (The SDK assumes one
	// dispatcher per service; this deliberately violates that to prove the idempotency
	// marker — not the cursor — is the exactly-once guard.)
	cursors := iamv1.NewEntOutboxCursorStore(client)
	mkDispatcher := func() *events.Dispatcher {
		d := events.NewDispatcher(store, cursors, tx, idem)
		d.Subscribe(eventUserSuspended, "revoke-api-keys", func(hctx context.Context, evt events.Event) error {
			if err := revoke(hctx, evt); err != nil {
				return err
			}
			mu.Lock()
			committedEffects++
			mu.Unlock()
			return nil
		})
		return d
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A racer that hits the marker conflict is acceptable (at-least-once: the
			// cursor stays put for retry); exactly-once is asserted below.
			_, _ = mkDispatcher().RunOnce(ctx, 10)
		}()
	}
	wg.Wait()

	// Exactly-once: the key is revoked and stays revoked.
	if remaining, _, _ := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10}); len(remaining) != 0 {
		t.Fatalf("the key must be revoked exactly once and stay revoked, %d present", len(remaining))
	}
	// Drive one more pass so the cursor converges past the event before asserting on the
	// marker count (a racer that lost the marker race left the cursor un-advanced).
	if _, err := mkDispatcher().RunOnce(ctx, 10); err != nil {
		t.Fatalf("convergence pass: %v", err)
	}
	// Exactly one idempotency marker committed across the two racers: the marker is
	// unique per (event, handler), so a single marker row is the engine-level proof
	// that exactly one delivery committed its effect.
	markers, err := client.IdemMarker.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count idempotency markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("exactly-once: exactly one idempotency marker must commit, found %d", markers)
	}
	mu.Lock()
	effects := committedEffects
	mu.Unlock()
	// A loser whose marker INSERT conflicts rolls its tx back AFTER the body ran, so
	// effects may be 1 or 2; the single committed marker (above) is what proves the
	// effect applied exactly once. At least one racer must have delivered.
	if effects < 1 {
		t.Fatalf("at least one dispatcher must have delivered the event, got %d", effects)
	}
}
