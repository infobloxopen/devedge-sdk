package iamv1_test

// postgres_events_test.go — Phase-2 validation of the transactional-outbox +
// exactly-once-dispatch machinery on REAL Postgres (the production target). Each
// test either runs against a testcontainers postgres:16 server or SKIPS cleanly
// when Docker is unavailable (see pgtest_test.go).
//
// Why Postgres matters for the outbox: the exactly-once guarantee rests on the
// idempotency-marker UNIQUE constraint. On SQLite, writes are serialized, so a
// "concurrent" double-apply can never truly overlap — the UNIQUE conflict is only
// approximated. On Postgres two dispatchers genuinely race to INSERT the same
// marker key; exactly one transaction commits (effect + marker) and the other
// gets a real engine-level unique violation that rolls its effect back. These
// tests exercise that on the actual engine.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/secret"
	entiam "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// TestPG_Ent_AtomicOutboxEnlist proves AC-1 on the ent backend over real
// Postgres: a Publish inside Atomically writes the outbox row THROUGH the *ent.Tx,
// so a rollback discards the outbox row (no orphan dual write) and a commit leaves
// exactly one row.
func TestPG_Ent_AtomicOutboxEnlist(t *testing.T) {
	client := openIAMEntPG(t)
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
		if _, uerr := users.Update(ctx, "u1", u, "display_name"); uerr != nil {
			return uerr
		}
		if perr := pub.Publish(ctx, events.Event{ID: "evt-rollback", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")}); perr != nil {
			return perr
		}
		return boom // force rollback after BOTH writes
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	// The user change rolled back...
	if got, _ := users.Get(ctx, "u1"); got.GetDisplayName() == "changed" {
		t.Fatal("user change must have rolled back")
	}
	// ...and so did the outbox row.
	if n := entOutboxCount(t, client); n != 0 {
		t.Fatalf("rollback must discard the outbox row (atomic enlist), found %d", n)
	}

	// A committed Publish leaves exactly one row.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "evt-commit", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")})
	}); err != nil {
		t.Fatalf("committed publish: %v", err)
	}
	if n := entOutboxCount(t, client); n != 1 {
		t.Fatalf("a committed Publish must leave exactly one outbox row, found %d", n)
	}
}

// TestPG_Gorm_AtomicOutboxEnlist proves AC-1 on the GORM backend over real
// Postgres: the GORM outbox enlist is atomic with the user write (rollback
// discards the row; commit keeps exactly one).
func TestPG_Gorm_AtomicOutboxEnlist(t *testing.T) {
	db := openIAMGormPG(t)
	ctx := tenantCtx("acme")
	users := iamv1.NewUserRepository(db)
	tx := gormtx.NewGormTxRunner(db)
	store := gormtx.NewGormOutboxStore(db)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	boom := errors.New("boom")
	err := tx.Atomically(ctx, func(ctx context.Context) error {
		u, _ := users.Get(ctx, "u1")
		u.DisplayName = "changed"
		if _, uerr := users.Update(ctx, "u1", u, "display_name"); uerr != nil {
			return uerr
		}
		if perr := pub.Publish(ctx, events.Event{ID: "evt-rollback", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")}); perr != nil {
			return perr
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if got, _ := users.Get(ctx, "u1"); got.GetDisplayName() == "changed" {
		t.Fatal("user change must have rolled back")
	}
	if n := outboxCount(t, db, "evt-rollback"); n != 0 {
		t.Fatalf("rollback must discard the outbox row (atomic enlist), found %d", n)
	}

	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "evt-commit", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")})
	}); err != nil {
		t.Fatalf("committed publish: %v", err)
	}
	if n := outboxCount(t, db, "evt-commit"); n != 1 {
		t.Fatalf("a committed Publish must leave exactly one outbox row, found %d", n)
	}
}

// TestPG_Gorm_ExactlyOnceUnderConcurrentDispatch is the headline outbox proof on
// real Postgres: two dispatchers race to deliver the SAME event concurrently. The
// SQL-backed GormIdempotencyStore inserts the (event, handler) marker INSIDE the
// handler's transaction, so on Postgres exactly one delivery commits its effect and
// the other collides on the marker's UNIQUE primary key (a real engine-level
// conflict, surfaced as events.ErrAlreadyApplied) and rolls its whole tx back. The
// invariant is exactly-once: the user's key is revoked once and stays revoked,
// regardless of which racer wins, and no second effect ever commits.
//
// This is the race SQLite cannot genuinely exhibit (it serializes writers); here it
// runs against the production engine where the UNIQUE conflict is the real thing.
func TestPG_Gorm_ExactlyOnceUnderConcurrentDispatch(t *testing.T) {
	db := openIAMGormPG(t)
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserRepository(db)
	apiKeys := iamv1.NewApiKeyRepository(db, enc)
	tx := gormtx.NewGormTxRunner(db)
	store := gormtx.NewGormOutboxStore(db)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if err := suspendUserGorm(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	var mu sync.Mutex
	committedEffects := 0
	revoke := revokeKeysHandlerGorm(apiKeys)
	idem := gormtx.NewGormIdempotencyStore(db)
	mkDispatcher := func() *events.Dispatcher {
		d := events.NewDispatcher(store, tx, idem)
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

	// Make the row claimable by both racers (clear the lease) so they genuinely
	// contend for the same delivery.
	if err := db.WithContext(ctx).Model(&gormtx.OutboxRow{}).
		Where("id IS NOT NULL").
		Updates(map[string]any{"delivered_time": nil, "leased_until": nil}).Error; err != nil {
		t.Fatalf("reset lease: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A racer that hits the marker conflict / a busy row is acceptable
			// (at-least-once: the row stays for retry); exactly-once is asserted on the
			// live key count and the committed-effect count below.
			_, _ = mkDispatcher().RunOnce(ctx, 10)
		}()
	}
	wg.Wait()

	// Exactly-once: the key is revoked and stays revoked...
	if remaining, _, _ := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10}); len(remaining) != 0 {
		t.Fatalf("the key must be revoked exactly once and stay revoked, %d present", len(remaining))
	}
	// ...and the handler's committed effect ran at most once across the two racers
	// (the marker UNIQUE rolled back the loser's effect). A surviving undelivered row
	// would be re-delivered later; drive a convergence pass so the row is terminal.
	var undelivered int64
	db.WithContext(ctx).Model(&gormtx.OutboxRow{}).Where("delivered_time IS NULL").Count(&undelivered)
	if undelivered != 0 {
		if _, err := mkDispatcher().RunOnce(ctx, 10); err != nil {
			t.Fatalf("convergence pass: %v", err)
		}
	}
	// The handler effect (revoking the key) committed exactly once: the committed
	// idempotency marker is unique per (event, handler), so a single marker row in
	// the table is the engine-level proof that exactly one delivery committed.
	var markers int64
	if err := db.WithContext(ctx).Model(&gormtx.IdemMarker{}).Count(&markers).Error; err != nil {
		t.Fatalf("count idempotency markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("exactly-once: exactly one idempotency marker must commit across the two racers, found %d", markers)
	}
	mu.Lock()
	effects := committedEffects
	mu.Unlock()
	// committedEffects counts handler bodies that reached the end. A loser whose
	// marker INSERT conflicts rolls its tx back AFTER the body ran, so effects may be
	// 1 or 2; the single committed marker (asserted above) is what proves the effect
	// applied exactly once. effects must be at least 1 (someone delivered).
	if effects < 1 {
		t.Fatalf("at least one dispatcher must have delivered the event, got %d", effects)
	}
}

// entOutboxCount counts ent outbox rows on a fresh (non-tx) query.
func entOutboxCount(t *testing.T, client *entiam.Client) int64 {
	t.Helper()
	n, err := client.Outbox.Query().Count(context.Background())
	if err != nil {
		t.Fatalf("count ent outbox: %v", err)
	}
	return int64(n)
}
