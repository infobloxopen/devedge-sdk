package servicekit_test

// p3_test.go — WS-012 P3 (composition host) acceptance tests, all using the SDK's
// in-process substrate (membus + in-memory outbox/cursor/idempotency/tx stores), so
// they need NO Docker and run on every `go test`. The real-Postgres composed-host
// event-flow proof lives in testdata/iam/iamv1/ws012_events_pg_test.go (Docker-gated).

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/config"
	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// --- shared helpers for the P3 tests ---

func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", why)
}

// runHostAsync starts servicekit.Run in a goroutine and returns a cancel + a channel
// for its error, so a test can drive the host then shut it down cleanly.
func runHostAsync(t *testing.T, hc servicekit.HostConfig) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hc.Context = ctx
	if hc.GRPCAddr == "" {
		hc.GRPCAddr = ":0"
	}
	done := make(chan error, 1)
	go func() { done <- servicekit.Run(hc) }()
	return cancel, done
}

const (
	eventOrderCreated = "orders.order.created"
)

// --- 1. Composed multi-module host: in-process event flow over namespaced outboxes ---

// eventModule is a module that publishes (via a host-owned relay over its own outbox)
// and/or subscribes (via a host-owned consumer). It mirrors what a generated module's
// Register does for the event axis, using the in-memory substrate.
type eventModule struct {
	id         string
	publishes  []servicekit.EventType
	subscribes []servicekit.EventType

	// supplied by the test so it can drive a publish + observe a delivery.
	store   persistence.OutboxStore
	cursors persistence.OutboxCursorStore
	tx      persistence.TxRunner
	idem    events.IdempotencyStore
	onEvent func(evt events.Event) // called by the subscriber handler

	relayStarted    *int32 // incremented by the relay registration (assert exactly one)
	consumerStarted *int32
}

func (m *eventModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events: servicekit.EventDescriptor{
			Publishes:  m.publishes,
			Subscribes: m.subscribes,
			Outbox:     servicekit.OutboxDescriptor{Enabled: m.store != nil},
		},
	}
}

func (m *eventModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})

	// Publisher side: hand the host this module's namespaced outbox so it starts ONE relay.
	if m.store != nil {
		if err := app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{
			Store:        m.store,
			Cursors:      m.cursors,
			PollInterval: time.Millisecond,
			Batch:        10,
		}); err != nil {
			return err
		}
		if m.relayStarted != nil {
			atomic.AddInt32(m.relayStarted, 1)
		}
	}

	// Subscriber side: register handlers so the host starts ONE consumer for this module.
	if len(m.subscribes) > 0 {
		handlers := make([]servicekit.EventHandler, 0, len(m.subscribes))
		for _, et := range m.subscribes {
			handlers = append(handlers, servicekit.EventHandler{
				EventType: string(et),
				Name:      m.id + ":on:" + string(et),
				Handle: func(ctx context.Context, evt events.Event) error {
					if m.onEvent != nil {
						m.onEvent(evt)
					}
					return nil
				},
			})
		}
		if err := app.Subscribe(servicekit.ConsumerConfig{Tx: m.tx, Idem: m.idem}, handlers...); err != nil {
			return err
		}
		if m.consumerStarted != nil {
			atomic.AddInt32(m.consumerStarted, 1)
		}
	}
	return nil
}

// TestP3_ComposedHost_InProcessEventFlow is the headline P3 acceptance: two modules in
// ONE host share ONE in-process bus; module A publishes via its outbox and module B's
// subscriber receives it through the HOST-OWNED dispatcher (relay → bus → consumer) —
// NOT a direct handler call. It asserts exactly one relay + one consumer were wired
// (no double-start).
func TestP3_ComposedHost_InProcessEventFlow(t *testing.T) {
	// Module A (orders): publishes. Its own namespaced (here in-memory) outbox + cursor.
	aStore := persistence.NewMemoryOutboxStore()
	aCursors := persistence.NewMemoryOutboxCursorStore()
	aTx := persistence.NewMemoryTxRunner(aStore)
	aPub := events.NewOutboxPublisher(aStore)

	var relays, consumers int32
	moduleA := &eventModule{
		id:           "orders",
		publishes:    []servicekit.EventType{eventOrderCreated},
		store:        aStore,
		cursors:      aCursors,
		relayStarted: &relays,
	}

	// Module B (billing): subscribes to orders.order.created. Its own tx + idem.
	bStore := persistence.NewMemoryOutboxStore()
	bTx := persistence.NewMemoryTxRunner(bStore)
	var got atomic.Value
	moduleB := &eventModule{
		id:              "billing",
		subscribes:      []servicekit.EventType{eventOrderCreated},
		tx:              bTx,
		idem:            events.NewMemoryIdempotencyStore(),
		onEvent:         func(evt events.Event) { got.Store(string(evt.Payload)) },
		consumerStarted: &consumers,
	}

	cancel, done := runHostAsync(t, servicekit.HostConfig{
		Modules: []servicekit.Module{moduleA, moduleB},
		Bus:     membus.New(),
	})
	defer func() { cancel(); <-done }()

	// Publish from module A's domain (inside its tx, the only legal publish path).
	if err := aTx.Atomically(context.Background(), func(ctx context.Context) error {
		return aPub.Publish(ctx, events.Event{
			ID: "evt-1", Type: eventOrderCreated, AggregateID: "o1", Payload: []byte("order-1"),
		})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Module B receives it through the host-owned relay→bus→consumer pipeline.
	waitFor(t, "module B's subscriber to receive module A's event", func() bool {
		v, _ := got.Load().(string)
		return v == "order-1"
	})

	// Exactly one relay (module A) and one consumer (module B) — no double-start.
	if r := atomic.LoadInt32(&relays); r != 1 {
		t.Fatalf("expected exactly 1 relay registered, got %d", r)
	}
	if c := atomic.LoadInt32(&consumers); c != 1 {
		t.Fatalf("expected exactly 1 consumer registered, got %d", c)
	}
}

// TestP3_DoubleRelayRegistration_Rejected proves the host rejects a module that tries
// to register two outbox relays — the "exactly one relay per module outbox" invariant.
func TestP3_DoubleRelayRegistration_Rejected(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
	mod := doubleRelayModule{store: store, cursors: cursors}
	err := servicekit.Run(servicekit.HostConfig{
		Modules: []servicekit.Module{mod}, GRPCAddr: ":0", Context: cancelledCtx(),
	})
	if err == nil || !strings.Contains(err.Error(), "already registered an outbox relay") {
		t.Fatalf("expected double-relay rejection, got %v", err)
	}
}

type doubleRelayModule struct {
	store   persistence.OutboxStore
	cursors persistence.OutboxCursorStore
}

func (doubleRelayModule) Descriptor() servicekit.Descriptor {
	return servicekit.Descriptor{
		ID:         "dup",
		Methods:    []string{"/dup.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/dup.v1.Svc/Noop", Public: true}},
	}
}
func (m doubleRelayModule) Register(_ context.Context, app *servicekit.App) error {
	app.Server.RecordMethods("/dup.v1.Svc/Noop")
	app.Server.AddRules(authz.MethodRule{Method: "/dup.v1.Svc/Noop", Public: true})
	if err := app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{Store: m.store, Cursors: m.cursors}); err != nil {
		return err
	}
	return app.RegisterOutboxRelay(servicekit.OutboxRelayConfig{Store: m.store, Cursors: m.cursors})
}

// --- 2. Panic containment + failure policy ---

// panicJobModule registers a background job that panics, to exercise the bulkhead.
type panicJobModule struct {
	id      string
	policy  servicekit.FailurePolicy
	started chan struct{}
}

func (m panicJobModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:            m.id,
		Methods:       []string{method},
		AuthzRules:    []authz.MethodRule{{Method: method, Public: true}},
		FailurePolicy: m.policy,
	}
}
func (m panicJobModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return app.RegisterBackgroundJob("boom", func(ctx context.Context) error {
		if m.started != nil {
			close(m.started)
		}
		panic("module job exploded")
	})
}

// liveModule is a healthy module whose background job runs until ctx is cancelled, so a
// test can prove a co-resident degraded module's panic did NOT crash it.
type liveModule struct {
	id    string
	alive *int32
}

func (m liveModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{ID: m.id, Methods: []string{method}, AuthzRules: []authz.MethodRule{{Method: method, Public: true}}, FailurePolicy: servicekit.Degraded}
}
func (m liveModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return app.RegisterBackgroundJob("heartbeat", func(ctx context.Context) error {
		atomic.StoreInt32(m.alive, 1)
		<-ctx.Done()
		return nil
	})
}

// TestP3_PanicContainment_Degraded proves a Degraded module's job panic is recovered,
// attributed to that module, and does NOT crash a co-resident module or the host.
func TestP3_PanicContainment_Degraded(t *testing.T) {
	var bAlive int32
	started := make(chan struct{})
	modA := panicJobModule{id: "analytics", policy: servicekit.Degraded, started: started}
	modB := liveModule{id: "billing", alive: &bAlive}

	cancel, done := runHostAsync(t, servicekit.HostConfig{
		Modules: []servicekit.Module{modA, modB},
	})

	<-started // module A's job ran (and panicked)
	// The host must stay up: module B keeps running.
	waitFor(t, "module B to stay alive after module A's panic", func() bool {
		return atomic.LoadInt32(&bAlive) == 1
	})

	// The host did not exit on its own (Degraded isolates the failure).
	select {
	case err := <-done:
		t.Fatalf("host exited on a degraded module's panic (should stay up): %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("clean shutdown should return nil, got %v", err)
	}
}

// TestP3_PanicContainment_FailHost proves a FailHost module's job panic takes the host
// down fail-fast (the host context is cancelled with the attributed cause).
func TestP3_PanicContainment_FailHost(t *testing.T) {
	started := make(chan struct{})
	modA := panicJobModule{id: "billing", policy: servicekit.FailHost, started: started}

	_, done := runHostAsync(t, servicekit.HostConfig{
		Modules: []servicekit.Module{modA},
	})

	<-started
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "billing") {
			t.Fatalf("FailHost panic should fail the host fast with the module's cause, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FailHost module's panic did not bring the host down")
	}
}

// --- 3. Config layering ---

type cfgModule struct {
	id     string
	prefix string
	got    *string
}

type moduleCfg struct {
	Name string `config:"NAME"`
}

func (m cfgModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Config:     servicekit.ConfigDescriptor{Prefix: m.prefix},
	}
}
func (m cfgModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	var c moduleCfg
	if err := app.Config.Load(&c); err != nil {
		return err
	}
	*m.got = c.Name
	return nil
}

// TestP3_ConfigLayering_PerModulePrefix proves two modules read ISOLATED, prefix-scoped
// config from ONE source set: module "orders" sees ORDERS_NAME, "billing" sees BILLING_NAME.
func TestP3_ConfigLayering_PerModulePrefix(t *testing.T) {
	var ordersName, billingName string
	src := config.Map(map[string]string{
		"ORDERS_NAME":  "orders-config",
		"BILLING_NAME": "billing-config",
	})
	err := servicekit.Run(servicekit.HostConfig{
		Modules: []servicekit.Module{
			cfgModule{id: "orders", prefix: "orders", got: &ordersName},
			cfgModule{id: "billing", prefix: "billing", got: &billingName},
		},
		GRPCAddr:      ":0",
		Context:       cancelledCtx(),
		ConfigSources: []config.Source{src},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ordersName != "orders-config" {
		t.Errorf("orders config = %q, want orders-config", ordersName)
	}
	if billingName != "billing-config" {
		t.Errorf("billing config = %q, want billing-config", billingName)
	}
}

// TestP3_ConfigPrefix_DefaultsToModuleID proves a module that declares no prefix scopes
// to its module ID.
func TestP3_ConfigPrefix_DefaultsToModuleID(t *testing.T) {
	var name string
	src := config.Map(map[string]string{"ORDERS_NAME": "by-id"})
	if err := servicekit.Run(servicekit.HostConfig{
		Modules:       []servicekit.Module{cfgModule{id: "orders", prefix: "", got: &name}},
		GRPCAddr:      ":0",
		Context:       cancelledCtx(),
		ConfigSources: []config.Source{src},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if name != "by-id" {
		t.Errorf("config scoped to module ID = %q, want by-id", name)
	}
}

// TestP3_ConfigPrefix_ReservedRejected proves a module cannot claim a host-owned
// platform-global config namespace.
func TestP3_ConfigPrefix_ReservedRejected(t *testing.T) {
	var name string
	err := servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{cfgModule{id: "x", prefix: "database", got: &name}},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
	})
	if err == nil || !strings.Contains(err.Error(), "host-owned platform-global namespace") {
		t.Fatalf("expected reserved-prefix rejection, got %v", err)
	}
}

// --- 4. Boot validation: event graph ---

func TestP3_BootValidation_DuplicateEventType(t *testing.T) {
	a := graphModule{id: "a", publishes: []servicekit.EventType{"thing.happened"}}
	b := graphModule{id: "b", publishes: []servicekit.EventType{"thing.happened"}}
	err := servicekit.ValidateModules([]servicekit.Module{a, b})
	if err == nil || !strings.Contains(err.Error(), "globally unique") {
		t.Fatalf("duplicate event type should be rejected at boot, got %v", err)
	}
}

func TestP3_BootValidation_OrphanSubscriber(t *testing.T) {
	a := graphModule{id: "a", subscribes: []servicekit.EventType{"never.published"}}
	err := servicekit.ValidateModules([]servicekit.Module{a})
	if err == nil || !strings.Contains(err.Error(), "orphan subscriber") {
		t.Fatalf("orphan subscriber should be rejected at boot, got %v", err)
	}
}

func TestP3_BootValidation_CoherentGraph_OK(t *testing.T) {
	a := graphModule{id: "a", publishes: []servicekit.EventType{"thing.happened"}}
	b := graphModule{id: "b", subscribes: []servicekit.EventType{"thing.happened"}}
	if err := servicekit.ValidateModules([]servicekit.Module{a, b}); err != nil {
		t.Fatalf("a coherent publisher/subscriber graph should validate, got %v", err)
	}
}

// graphModule is a descriptor-only module for event-graph validation tests.
type graphModule struct {
	id         string
	publishes  []servicekit.EventType
	subscribes []servicekit.EventType
}

func (m graphModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Events:     servicekit.EventDescriptor{Publishes: m.publishes, Subscribes: m.subscribes},
	}
}
func (m graphModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return nil
}
