package servicekit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// fakeModule is a minimal Module used to exercise the servicekit contract without
// the generated wrapper (the root module cannot import the gorm/ent testdata
// fixtures). Its Register records its methods + rules on the shared server
// exactly as the generated Register<Svc> would, so server.Serve's union
// completeness gate runs over the real combined surface.
type fakeModule struct {
	desc       servicekit.Descriptor
	registered *bool // flipped true when Register runs (wiring assertion)
	regErr     error // forced Register error (failure-path assertion)
}

func (m fakeModule) Descriptor() servicekit.Descriptor { return m.desc }

func (m fakeModule) Register(_ context.Context, app *servicekit.App) error {
	if m.regErr != nil {
		return m.regErr
	}
	if m.registered != nil {
		*m.registered = true
	}
	// Mirror the generated Register<Svc>: record methods + contribute rules so the
	// server's boot-time union gate sees a complete, conflict-free surface.
	app.Server.RecordMethods(m.desc.Methods...)
	app.Server.AddRules(m.desc.AuthzRules...)
	return nil
}

// ordersModule is a self-consistent fake (every method has a rule).
func ordersModule(registered *bool) fakeModule {
	return fakeModule{
		registered: registered,
		desc: servicekit.Descriptor{
			ID:      "orders",
			Version: "v0.1.0",
			Methods: []string{
				"/orders.v1.OrderService/CreateOrder",
				"/orders.v1.OrderService/GetOrder",
			},
			AuthzRules: []authz.MethodRule{
				{Method: "/orders.v1.OrderService/CreateOrder", Verb: authz.Verb("create"), Resource: "orders.order"},
				{Method: "/orders.v1.OrderService/GetOrder", Verb: authz.Verb("read"), Resource: "orders.order"},
			},
			Routes:    []servicekit.RouteDescriptor{{Prefix: "/api/orders/v1"}},
			Resources: []servicekit.ResourceDescriptor{{Name: "orders.order", Plural: "orders"}},
		},
	}
}

// billingModule is a second self-consistent fake with a DISTINCT service name,
// route prefix, and resource — a conflict-free 2-module composition.
func billingModule(registered *bool) fakeModule {
	return fakeModule{
		registered: registered,
		desc: servicekit.Descriptor{
			ID:      "billing",
			Version: "v0.2.0",
			Methods: []string{
				"/billing.v1.BillingService/CreateInvoice",
			},
			AuthzRules: []authz.MethodRule{
				{Method: "/billing.v1.BillingService/CreateInvoice", Verb: authz.Verb("create"), Resource: "billing.invoice"},
			},
			Routes:    []servicekit.RouteDescriptor{{Prefix: "/api/billing/v1"}},
			Resources: []servicekit.ResourceDescriptor{{Name: "billing.invoice", Plural: "invoices"}},
		},
	}
}

// runUntilCancel runs servicekit.Run with a ":0" gRPC addr and cancels the
// context immediately, so Run reaches and passes the server's boot gate then
// returns cleanly. It returns Run's error (nil on a clean validated start).
func runUntilCancel(t *testing.T, mods ...servicekit.Module) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  mods,
			GRPCAddr: ":0",
			Context:  ctx,
		})
	}()
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after cancel")
		return nil
	}
}

func TestRun_SingleModule_Serves(t *testing.T) {
	var registered bool
	if err := runUntilCancel(t, ordersModule(&registered)); err != nil {
		t.Fatalf("Run single module: %v", err)
	}
	if !registered {
		t.Fatal("module Register was not called")
	}
}

func TestRun_TwoModules_Serves(t *testing.T) {
	var o, b bool
	if err := runUntilCancel(t, ordersModule(&o), billingModule(&b)); err != nil {
		t.Fatalf("Run two modules: %v", err)
	}
	if !o || !b {
		t.Fatalf("both modules should register; orders=%v billing=%v", o, b)
	}
}

func TestRun_NoModules_Fails(t *testing.T) {
	err := servicekit.Run(servicekit.HostConfig{GRPCAddr: ":0", Context: cancelledCtx()})
	if err == nil {
		t.Fatal("Run with no modules should fail")
	}
	if !strings.Contains(err.Error(), "no modules") {
		t.Fatalf("error = %q, want it to mention 'no modules'", err)
	}
}

func TestRun_ModuleRegisterError_Propagates(t *testing.T) {
	boom := errors.New("boom")
	bad := fakeModule{desc: servicekit.Descriptor{ID: "bad"}, regErr: boom}
	err := servicekit.Run(servicekit.HostConfig{Modules: []servicekit.Module{bad}, GRPCAddr: ":0", Context: cancelledCtx()})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Run should propagate Register error; got %v", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error should name the module; got %q", err)
	}
}

func TestRun_UndeclaredMethod_FailsClosedViaServerGate(t *testing.T) {
	// A module that records a method but contributes no rule must fail the
	// server's EXISTING union completeness gate (not a parallel servicekit gate).
	leaky := fakeModule{desc: servicekit.Descriptor{
		ID:      "leaky",
		Methods: []string{"/leaky.v1.Svc/Orphan"},
		// no AuthzRules -> orphan method
	}}
	err := servicekit.Run(servicekit.HostConfig{Modules: []servicekit.Module{leaky}, GRPCAddr: ":0", Context: cancelledCtx()})
	if err == nil {
		t.Fatal("Run should fail closed on an undeclared method")
	}
	if !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("error = %q, want it to mention 'undeclared' (the server gate)", err)
	}
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// --- WS-012 P2: DB namespace allocation + host-run migration wiring ---

// TestRun_AllocatesNamespaceAndRunsMigration proves the P2 host wiring: with a
// shared Database engine declared, Run allocates a per-module DatabaseNamespace
// (schema-preferred -> Postgres schema) and calls the host's MigrationRunner once
// per module, BEFORE the module registers, with that namespace.
func TestRun_AllocatesNamespaceAndRunsMigration(t *testing.T) {
	type call struct {
		schema   string
		moduleID string
	}
	var calls []call
	migrate := func(_ context.Context, ns servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		calls = append(calls, call{schema: ns.Schema, moduleID: ns.ModuleID})
		return nil
	}

	err := servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{ordersModule(nil), billingModule(nil)},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
		Database: &servicekit.DatabaseConfig{Engine: "postgres", DefaultIsolation: servicekit.IsolationSchemaPreferred},
		Migrate:  migrate,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 migration calls, got %d (%+v)", len(calls), calls)
	}
	if calls[0].moduleID != "orders" || calls[0].schema != "orders" {
		t.Errorf("first migration call = %+v, want orders/orders", calls[0])
	}
	if calls[1].moduleID != "billing" || calls[1].schema != "billing" {
		t.Errorf("second migration call = %+v, want billing/billing", calls[1])
	}
}

// TestRun_NoDatabase_RunsMigrationWithZeroNamespace proves the standalone path:
// with no HostConfig.Database the runner still runs (host-run migration), but the
// namespace is zero-qualification (bare tables) — behavior unchanged.
func TestRun_NoDatabase_RunsMigrationWithZeroNamespace(t *testing.T) {
	var gotNS servicekit.DatabaseNamespace
	called := false
	migrate := func(_ context.Context, ns servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		called = true
		gotNS = ns
		return nil
	}
	if err := servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{ordersModule(nil)},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
		Migrate:  migrate,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("migration runner should run even without HostConfig.Database")
	}
	if !gotNS.IsZero() {
		t.Errorf("namespace should be zero-qualification (bare tables), got %+v", gotNS)
	}
	if gotNS.ModuleID != "orders" {
		t.Errorf("namespace ModuleID = %q, want orders", gotNS.ModuleID)
	}
}

// TestRun_MigrationError_Propagates proves a failed host-run migration aborts boot
// (the module never registers, the host never serves).
func TestRun_MigrationError_Propagates(t *testing.T) {
	boom := errors.New("migrate boom")
	var registered bool
	migrate := func(_ context.Context, _ servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		return boom
	}
	err := servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{ordersModule(&registered)},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
		Database: &servicekit.DatabaseConfig{Engine: "postgres"},
		Migrate:  migrate,
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Run should propagate migration error; got %v", err)
	}
	if registered {
		t.Error("module must NOT register when its migration failed")
	}
}

// TestApp_DBNamespace_ResolvesPerModule proves a module can read its allocated
// namespace from app.DB in Register (the seam it uses to build namespaced stores).
func TestApp_DBNamespace_ResolvesPerModule(t *testing.T) {
	var resolved servicekit.DatabaseNamespace
	probe := probeModule{id: "orders", onRegister: func(app *servicekit.App) {
		ns, err := app.DB.Namespace("orders", servicekit.DatabaseDescriptor{})
		if err != nil {
			t.Errorf("app.DB.Namespace: %v", err)
		}
		resolved = ns
	}}
	if err := servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{probe},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
		Database: &servicekit.DatabaseConfig{Engine: "postgres", DefaultIsolation: servicekit.IsolationSchemaPreferred},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resolved.Schema != "orders" {
		t.Errorf("resolved schema = %q, want orders", resolved.Schema)
	}
}

// probeModule is a Module whose Register runs a probe callback (so a test can
// inspect the App the host hands it). It records one public method so the boot gate
// passes.
type probeModule struct {
	id         string
	onRegister func(app *servicekit.App)
}

func (m probeModule) Descriptor() servicekit.Descriptor {
	method := "/" + m.id + ".v1.Svc/Noop"
	return servicekit.Descriptor{
		ID:         m.id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
	}
}

func (m probeModule) Register(_ context.Context, app *servicekit.App) error {
	method := "/" + m.id + ".v1.Svc/Noop"
	app.Server.RecordMethods(method)
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	if m.onRegister != nil {
		m.onRegister(app)
	}
	return nil
}

// --- descriptor validation ---

func TestValidateModules_OK(t *testing.T) {
	if err := servicekit.ValidateModules([]servicekit.Module{ordersModule(nil), billingModule(nil)}); err != nil {
		t.Fatalf("valid composition rejected: %v", err)
	}
}

func TestValidateModules_DuplicateID(t *testing.T) {
	mustValidateErr(t, []servicekit.Module{ordersModule(nil), ordersModule(nil)}, "duplicate module ID")
}

func TestValidateModules_EmptyID(t *testing.T) {
	bad := fakeModule{desc: servicekit.Descriptor{ID: ""}}
	mustValidateErr(t, []servicekit.Module{bad}, "empty Descriptor.ID")
}

func TestValidateModules_DuplicateServiceName(t *testing.T) {
	a := fakeModule{desc: servicekit.Descriptor{ID: "a", Methods: []string{"/dup.v1.Svc/M1"}}}
	b := fakeModule{desc: servicekit.Descriptor{ID: "b", Methods: []string{"/dup.v1.Svc/M2"}}}
	mustValidateErr(t, []servicekit.Module{a, b}, "gRPC service")
}

func TestValidateModules_DuplicateRoutePrefix(t *testing.T) {
	a := fakeModule{desc: servicekit.Descriptor{ID: "a", Routes: []servicekit.RouteDescriptor{{Prefix: "/api/x"}}}}
	b := fakeModule{desc: servicekit.Descriptor{ID: "b", Routes: []servicekit.RouteDescriptor{{Prefix: "/api/x"}}}}
	mustValidateErr(t, []servicekit.Module{a, b}, "route prefix")
}

func TestValidateModules_DuplicatePermission(t *testing.T) {
	a := fakeModule{desc: servicekit.Descriptor{ID: "a", AuthzRules: []authz.MethodRule{
		{Method: "/a.v1.Svc/M", Verb: authz.Verb("read"), Resource: "shared"},
	}}}
	b := fakeModule{desc: servicekit.Descriptor{ID: "b", AuthzRules: []authz.MethodRule{
		{Method: "/b.v1.Svc/M", Verb: authz.Verb("read"), Resource: "shared"},
	}}}
	mustValidateErr(t, []servicekit.Module{a, b}, "permission")
}

func TestValidateModules_NilModule(t *testing.T) {
	mustValidateErr(t, []servicekit.Module{nil}, "nil")
}

// TestValidateModules_SameServiceAcrossOwnMethods proves the per-module dedup:
// one module's many methods share its service name without tripping the
// cross-module duplicate-service check.
func TestValidateModules_SameServiceAcrossOwnMethods(t *testing.T) {
	if err := servicekit.ValidateModules([]servicekit.Module{ordersModule(nil)}); err != nil {
		t.Fatalf("single module with repeated service name across its methods rejected: %v", err)
	}
}

func mustValidateErr(t *testing.T, mods []servicekit.Module, want string) {
	t.Helper()
	err := servicekit.ValidateModules(mods)
	if err == nil {
		t.Fatalf("expected validation error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
}
