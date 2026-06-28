package iamv1_test

// ws012_p5_pg_test.go — WS-012 P5 ACCEPTANCE PROOF: AssertComposition against
// TWO real fixture modules (orders/billing from the ws012_events_pg_test.go
// pattern) booted via the servicekittest harness on REAL Postgres.
//
// This is the §7 "composition smoke test" wired to existing fixture modules and
// the established testcontainers harness (startPostgres / freshPGDatabase from
// pgtest_test.go). It proves:
//
//   - servicekittest.AssertComposition validates the two-module descriptor set
//     (unique IDs, coherent event graph);
//   - the host boots, runs per-module migrations in namespaced Postgres schemas,
//     registers both modules on the shared server, and the union completeness gate
//     passes;
//   - clean shutdown.
//
// Docker-optional: startPostgres skips cleanly when Docker is unavailable, so
// this test (like all pg tests) is green without Docker. The explicit WaitForReady
// flag causes the harness to dial the gRPC port before asserting — which drives the
// full host lifecycle including migrations.

import (
	"context"
	"net"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/servicekit"
	"github.com/infobloxopen/devedge-sdk/servicekittest"
)

// composedPubModule is a minimal publisher module for the P5 two-module composition.
// It mirrors pubModule from ws012_events_pg_test.go but does not count relays (the
// harness manages the boot lifecycle; we only need the wiring to be correct).
type composedPubModule struct {
	id      string
	store   *gormtx.GormOutboxStore
	cursors *gormtx.GormOutboxCursorStore
}

func (m *composedPubModule) Descriptor() servicekit.Descriptor {
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

func (m *composedPubModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{
		Store:        m.store,
		Cursors:      m.cursors,
		PollInterval: 5 * time.Millisecond,
		Batch:        10,
	})
}

// composedSubModule is a minimal subscriber module for the P5 composition.
type composedSubModule struct {
	id   string
	tx   persistence.TxRunner
	idem events.IdempotencyStore
}

func (m *composedSubModule) Descriptor() servicekit.Descriptor {
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

func (m *composedSubModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return app.Subscribe(
		servicekit.ConsumerConfig{Tx: m.tx, Idem: m.idem},
		servicekit.EventHandler{
			EventType: eventUserSuspended,
			Name:      m.id + ":on-suspend",
			Handle: func(_ context.Context, _ events.Event) error {
				return nil // smoke only — we just need delivery to work
			},
		},
	)
}

// TestP5_AssertComposition_TwoModules_RealPostgres is the P5 composition smoke
// test: AssertComposition is driven against two fixture modules on real Postgres.
// It skips cleanly when Docker is unavailable (via startPostgres → t.Skip).
func TestP5_AssertComposition_TwoModules_RealPostgres(t *testing.T) {
	baseDSN := freshPGDatabase(t, startPostgres(t)) // skips if Docker unavailable
	engine := "postgres"

	ordersNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "orders", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}
	billingNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "billing", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Per-module schema-scoped gorm handles.
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

	// Module A (orders): publisher.
	ordersOutbox := gormtx.NewGormOutboxStore(ordersDB, gormtx.WithOutboxNamespace(ordersNS))
	ordersCursors := gormtx.NewGormOutboxCursorStore(ordersDB, gormtx.WithCursorNamespace(ordersNS))
	pub := &composedPubModule{id: "orders", store: ordersOutbox, cursors: ordersCursors}

	// Module B (billing): subscriber.
	billingTx := gormtx.NewGormTxRunner(billingDB)
	billingIdem := gormtx.NewGormIdempotencyStore(billingDB, gormtx.WithIdempotencyNamespace(billingNS))
	sub := &composedSubModule{id: "billing", tx: billingTx, idem: billingIdem}

	// Allocate a loopback port so WaitForReady can dial it.
	addr := p5FreeLoopbackAddr(t)

	servicekittest.AssertComposition(
		t,
		[]servicekit.Module{pub, sub},
		servicekittest.CompositionOptions{
			GRPCAddr:     addr,
			WaitForReady: true,
			Timeout:      30 * time.Second,
			Database: &servicekit.DatabaseConfig{
				Engine:           engine,
				DefaultIsolation: servicekit.IsolationSchemaPreferred,
			},
			Migrate: migrate,
		},
	)
}

// TestP5_AssertComposition_InProcess_TwoModules is the in-process (no Docker)
// AssertComposition acceptance test: two fake modules with an in-process membus,
// no real DB. Mirrors the pattern of servicekit/p3_test.go but exercised via the
// harness rather than directly calling servicekit.Run.
func TestP5_AssertComposition_InProcess_TwoModules(t *testing.T) {
	pub := &composedNoDBPubModule{id: "analytics"}
	sub := &composedNoDBSubModule{id: "reporting"}

	servicekittest.AssertComposition(t, []servicekit.Module{pub, sub})
}

// composedNoDBPubModule / composedNoDBSubModule are the no-DB (in-memory outbox)
// versions for the in-process smoke: they use the memory stores so no Docker/DB
// is required.
type composedNoDBPubModule struct {
	id    string
	store *persistence.MemoryOutboxStore
}

func (m *composedNoDBPubModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events: servicekit.EventDescriptor{
			Publishes: []servicekit.EventType{"analytics.page.viewed"},
			Outbox:    servicekit.OutboxDescriptor{Enabled: true},
		},
	}
}

func (m *composedNoDBPubModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	if m.store == nil {
		m.store = persistence.NewMemoryOutboxStore()
	}
	cursors := persistence.NewMemoryOutboxCursorStore()
	return app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{
		Store:        m.store,
		Cursors:      cursors,
		PollInterval: time.Millisecond,
		Batch:        10,
	})
}

type composedNoDBSubModule struct {
	id string
}

func (m *composedNoDBSubModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events: servicekit.EventDescriptor{
			Subscribes: []servicekit.EventType{"analytics.page.viewed"},
		},
	}
}

func (m *composedNoDBSubModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(store)
	return app.Subscribe(
		servicekit.ConsumerConfig{
			Tx:   tx,
			Idem: events.NewMemoryIdempotencyStore(),
		},
		servicekit.EventHandler{
			EventType: "analytics.page.viewed",
			Name:      m.id + ":on-view",
			Handle:    func(_ context.Context, _ events.Event) error { return nil },
		},
	)
}

// p5FreeLoopbackAddr mirrors freeLoopbackAddrWS012 but avoids a name collision
// (the original is in ws012_namespace_pg_test.go in the same test package).
func p5FreeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// TestP5_AssertCompatible_TwoModules_WithRequires proves AssertCompatible works
// in the iam test context. Both modules declare Requires; the host satisfies them.
func TestP5_AssertCompatible_TwoModules_WithRequires(t *testing.T) {
	makeModule := func(id, reqSDK string) servicekit.Module {
		method := "/" + id + ".v1.Svc/Noop"
		return &fakeReqModule{
			id:     id,
			method: method,
			req:    servicekit.Compatibility{SDK: reqSDK, Go: ">=1.23"},
		}
	}
	mods := []servicekit.Module{
		makeModule("orders", ">=0.27.0"),
		makeModule("billing", ">=0.25.0"),
	}
	servicekittest.AssertCompatible(t, mods, servicekittest.HostRequires{
		SDK: "v0.28.0",
		Go:  "1.25.5",
	})
}

// fakeReqModule is a minimal Module with Requires for the compatibility assertion.
type fakeReqModule struct {
	id     string
	method string
	req    servicekit.Compatibility
}

func (m *fakeReqModule) Descriptor() servicekit.Descriptor {
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{m.method},
		AuthzRules: []authz.MethodRule{{Method: m.method, Public: true}},
		Requires:   m.req,
	}
}

func (m *fakeReqModule) Register(_ context.Context, app *servicekit.App) error {
	app.Server.RecordMethods(m.method)
	app.Server.AddRules(authz.MethodRule{Method: m.method, Public: true})
	return nil
}

// TestP5_AssertComposition_HandlesHostBus_InProcess proves AssertComposition
// passes the Bus automatically (defaults to membus) — modules with outbox relays
// can be composed without the caller wiring a bus explicitly.
func TestP5_AssertComposition_HandlesHostBus_InProcess(t *testing.T) {
	// Re-use the no-DB pair from TestP5_AssertComposition_InProcess_TwoModules
	// to show the default bus wiring is transparent to callers.
	pub := &composedNoDBPubModule{id: "alerter"}
	sub := &composedNoDBSubModule{id: "notifier"}

	// composedNoDBSubModule subscribes to "analytics.page.viewed" but the pub is
	// "alerter" emitting "analytics.page.viewed". Works as long as IDs differ.
	// Let's use a fresh event type pair to keep this test self-contained.
	pubSpecial := &inlineModule{
		id:        "source",
		method:    "/source.v1.Svc/Noop",
		publishes: []servicekit.EventType{"source.thing.happened"},
		store:     persistence.NewMemoryOutboxStore(),
	}
	subSpecial := &inlineSubModule{
		id:         "sink",
		method:     "/sink.v1.Svc/Noop",
		subscribes: []servicekit.EventType{"source.thing.happened"},
	}
	_ = pub // suppress unused warning; we use pub/sub only as a marker above
	_ = sub

	servicekittest.AssertComposition(t, []servicekit.Module{pubSpecial, subSpecial})
}

// inlineModule is a self-contained pub/sub pair for single-test use.
type inlineModule struct {
	id        string
	method    string
	publishes []servicekit.EventType
	store     *persistence.MemoryOutboxStore
}

func (m *inlineModule) Descriptor() servicekit.Descriptor {
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{m.method},
		AuthzRules: []authz.MethodRule{{Method: m.method, Public: true}},
		Events:     servicekit.EventDescriptor{Publishes: m.publishes, Outbox: servicekit.OutboxDescriptor{Enabled: true}},
	}
}
func (m *inlineModule) Register(_ context.Context, app *servicekit.App) error {
	app.Server.RecordMethods(m.method)
	app.Server.AddRules(authz.MethodRule{Method: m.method, Public: true})
	cursors := persistence.NewMemoryOutboxCursorStore()
	return app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{
		Store: m.store, Cursors: cursors, PollInterval: time.Millisecond, Batch: 10,
	})
}

type inlineSubModule struct {
	id         string
	method     string
	subscribes []servicekit.EventType
}

func (m *inlineSubModule) Descriptor() servicekit.Descriptor {
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{m.method},
		AuthzRules: []authz.MethodRule{{Method: m.method, Public: true}},
		Events:     servicekit.EventDescriptor{Subscribes: m.subscribes},
	}
}
func (m *inlineSubModule) Register(_ context.Context, app *servicekit.App) error {
	app.Server.RecordMethods(m.method)
	app.Server.AddRules(authz.MethodRule{Method: m.method, Public: true})
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(store)
	handlers := make([]servicekit.EventHandler, 0, len(m.subscribes))
	for _, et := range m.subscribes {
		et := et
		handlers = append(handlers, servicekit.EventHandler{
			EventType: string(et),
			Name:      m.id + ":on:" + string(et),
			Handle:    func(_ context.Context, _ events.Event) error { return nil },
		})
	}
	return app.Subscribe(
		servicekit.ConsumerConfig{Tx: tx, Idem: events.NewMemoryIdempotencyStore()},
		handlers...,
	)
}
