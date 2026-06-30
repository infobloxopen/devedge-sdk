package iamv1_test

// ws012_events_pg_test.go — the WS-012 P3 ACCEPTANCE PROOF on REAL Postgres: two
// composable MODULES booted in ONE host on ONE database, where module A PUBLISHES a
// domain event via its (namespaced) transactional outbox and module B's SUBSCRIBER
// receives it through the HOST-OWNED in-process dispatcher (relay → shared bus →
// consumer) — NOT a direct handler call. It proves:
//
//   - "same binary != direct calls": the cross-module reaction flows through the
//     durable outbox→relay→bus→consumer pipeline, never an imported handler;
//   - exactly ONE relay + ONE consumer per module (no double-start) — the whole point
//     of the host OWNING the dispatcher lifecycle;
//   - the namespaced outboxes from P2 carry it (module A's relay reads orders.outbox,
//     advances its own orders cursor; module B's consumer records its marker in the
//     billing schema).
//
// Docker-optional: it reuses startPostgres/freshPGDatabase (pgtest_test.go), which
// t.Skip() cleanly when Docker is unavailable — so `go test ./...` is green without
// Docker, and runs for real when Docker is up.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// pubModule is the PUBLISHER side: it owns a namespaced outbox the host runs a relay
// over. Register hands the host the module's outbox + cursor stores (built from its
// DatabaseNamespace per P2) so the host starts exactly one relay.
type pubModule struct {
	id      string
	store   *gormtx.GormOutboxStore
	cursors *gormtx.GormOutboxCursorStore
	relays  *int32
}

func (m *pubModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events: servicekit.EventDescriptor{
			Publishes: []servicekit.EventType{servicekit.EventType(eventUserSuspended)},
			Outbox:    servicekit.OutboxDescriptor{Enabled: true},
		},
	}
}

func (m *pubModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	if err := app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{
		Store:        m.store,
		Cursors:      m.cursors,
		PollInterval: 5 * time.Millisecond,
		Batch:        10,
	}); err != nil {
		return err
	}
	atomic.AddInt32(m.relays, 1)
	return nil
}

// subModule is the SUBSCRIBER side: it reacts to the publisher's event via a
// host-owned consumer. Register hands the host its tx + idempotency store (namespaced)
// and the handler, so the host starts exactly one consumer.
type subModule struct {
	id        string
	tx        persistence.TxRunner
	idem      events.IdempotencyStore
	got       *atomic.Value
	consumers *int32
}

func (m *subModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events: servicekit.EventDescriptor{
			Subscribes: []servicekit.EventType{servicekit.EventType(eventUserSuspended)},
		},
	}
}

func (m *subModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	if err := app.Subscribe(
		servicekit.ConsumerConfig{Tx: m.tx, Idem: m.idem},
		servicekit.EventHandler{
			EventType: eventUserSuspended,
			Name:      m.id + ":on-suspend",
			Handle: func(ctx context.Context, evt events.Event) error {
				m.got.Store(evt.AggregateID)
				return nil
			},
		},
	); err != nil {
		return err
	}
	atomic.AddInt32(m.consumers, 1)
	return nil
}

// TestWS012_P3_ComposedHost_EventFlow_RealPostgres is the P3 real-DB acceptance proof.
func TestWS012_P3_ComposedHost_EventFlow_RealPostgres(t *testing.T) {
	baseDSN := freshPGDatabase(t, startPostgres(t)) // skips cleanly without Docker
	engine := "postgres"

	ordersNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "orders", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}
	billingNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "billing", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Per-module schema-scoped handles (search_path = module schema), as P2 establishes.
	ordersDB := openSchemaScopedPG(t, baseDSN, ordersNS)
	billingDB := openSchemaScopedPG(t, baseDSN, billingNS)
	dbByModule := map[string]*gorm.DB{"orders": ordersDB, "billing": billingDB}

	fw := gormtx.MigrationModelsFor(true /*outbox*/, true /*idempotency*/)
	migrate := func(ctx context.Context, ns servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		return gormtx.MigrateModule(ctx, dbByModule[ns.ModuleID], gormtx.MigrateOptions{
			Namespace:       ns,
			FrameworkModels: fw,
		})
	}

	// Module A (orders): namespaced outbox + cursor → the host runs ONE relay.
	ordersOutbox := gormtx.NewGormOutboxStore(ordersDB, gormtx.WithOutboxNamespace(ordersNS))
	ordersCursors := gormtx.NewGormOutboxCursorStore(ordersDB, gormtx.WithCursorNamespace(ordersNS))
	var relays, consumers int32
	pub := &pubModule{id: "orders", store: ordersOutbox, cursors: ordersCursors, relays: &relays}

	// Module B (billing): namespaced tx + idempotency → the host runs ONE consumer.
	billingTx := gormtx.NewGormTxRunner(billingDB)
	billingIdem := gormtx.NewGormIdempotencyStore(billingDB, gormtx.WithIdempotencyNamespace(billingNS))
	var got atomic.Value
	sub := &subModule{id: "billing", tx: billingTx, idem: billingIdem, got: &got, consumers: &consumers}

	// Drive the FULL host path with a shared in-process bus (same binary, one DB).
	addr := freeLoopbackAddrWS012(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  []servicekit.Module{pub, sub},
			GRPCAddr: addr,
			Context:  ctx,
			Bus:      membus.New(),
			Database: &servicekit.DatabaseConfig{Engine: engine, DefaultIsolation: servicekit.IsolationSchemaPreferred},
			Migrate:  migrate,
		})
	}()
	waitForListener(t, addr) // migration + boot completed once the port accepts

	// PUBLISH from module A's domain: an event appended to orders.outbox inside a tx.
	ordersTx := gormtx.NewGormTxRunner(ordersDB)
	ordersPub := events.NewOutboxPublisher(ordersOutbox)
	if perr := ordersTx.Atomically(tenantCtx("acme"), func(c context.Context) error {
		return ordersPub.Publish(c, events.Event{
			ID: "evt-p3", Type: eventUserSuspended, AggregateType: "User", AggregateID: "u-42", Payload: []byte("u-42"),
		})
	}); perr != nil {
		t.Fatalf("orders publish: %v", perr)
	}

	// Module B's subscriber receives it through the HOST-OWNED relay→bus→consumer
	// pipeline — not a direct call (the two modules never import each other). Gate the
	// shutdown below on the DURABLE delivery proof — the idempotency marker COMMITTED in
	// the billing schema — NOT the in-memory got flag. consumer.deliver sets got at the
	// TOP of the handler's transaction, but the marker (and the real exactly-once effect)
	// only lands when that tx commits a moment later. Cancelling the host — whose context
	// the consumer's tx runs under — on the in-memory signal races that commit: under
	// -race / CI load the cancel can abort the COMMIT, rolling the marker back (the
	// observed flake: got == "u-42" but 0 markers).
	billingMarkers := func() int64 {
		var n int64
		if cerr := billingDB.WithContext(context.Background()).
			Table(billingNS.QualifyTable("idempotency_markers")).
			Count(&n).Error; cerr != nil {
			t.Fatalf("count billing idempotency markers: %v", cerr)
		}
		return n
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if billingMarkers() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v, _ := got.Load().(string); v != "u-42" {
		t.Fatalf("module B's subscriber did not receive module A's event through the host dispatcher (got %q)", v)
	}

	cancel()
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("servicekit.Run: %v", rerr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("servicekit.Run did not return within 30s")
	}

	// Exactly one relay (orders) + one consumer (billing) — no double-start.
	if r := atomic.LoadInt32(&relays); r != 1 {
		t.Errorf("expected exactly 1 relay, got %d", r)
	}
	if c := atomic.LoadInt32(&consumers); c != 1 {
		t.Errorf("expected exactly 1 consumer, got %d", c)
	}

	// The delivery committed module B's idempotency marker IN THE BILLING SCHEMA (the
	// engine-level proof the event flowed through the consumer's exactly-once path, not
	// a side channel). The wait loop above already gated on this marker before we
	// cancelled, so the count is stable here.
	if markers := billingMarkers(); markers != 1 {
		t.Errorf("expected exactly 1 idempotency marker in the billing schema (delivery proof), got %d", markers)
	}

	// The orders relay advanced its OWN namespaced cursor past the event.
	c, _, lerr := ordersCursors.LoadCursor(context.Background(), events.DefaultCursorName)
	if lerr != nil {
		t.Fatalf("load orders relay cursor: %v", lerr)
	}
	if c.ID != "evt-p3" {
		t.Errorf("orders relay cursor should have advanced past evt-p3, at %q", c.ID)
	}
}
