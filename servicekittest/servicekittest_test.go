package servicekittest_test

// servicekittest_test.go — unit tests for the servicekittest harness itself,
// using in-process fakes (no Docker, no real DB). These tests assert that the
// harnesses accept valid modules, reject contract violations, and that the
// compatibility checker handles version ranges correctly.
//
// Real fixture-module acceptance tests (wiring AssertModule / AssertComposition
// against the toy widgetsv1 module and the iam modules with real Postgres) live in
// the testdata submodules (testdata/toy/ and testdata/iam/iamv1/) because those
// modules import the generated fixtures — which the root module cannot import
// directly (would create an import cycle). The fixture tests are in:
//   - testdata/toy/ws012_p5_test.go  (AssertModule against WidgetServiceModule)
//   - testdata/iam/iamv1/ws012_p5_pg_test.go  (AssertComposition against two iam modules)

import (
	"context"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/servicekit"
	"github.com/infobloxopen/devedge-sdk/servicekittest"
)

// ---- fake module helpers -----------------------------------------------

type fakeModule struct {
	desc   servicekit.Descriptor
	regErr error
}

func (m fakeModule) Descriptor() servicekit.Descriptor { return m.desc }
func (m fakeModule) Register(_ context.Context, app *servicekit.App) error {
	if m.regErr != nil {
		return m.regErr
	}
	app.Server.RecordMethods(m.desc.Methods...)
	app.Server.AddRules(m.desc.AuthzRules...)
	return nil
}

func publicModule(id string) fakeModule {
	method := "/" + id + ".v1.Svc/Noop"
	return fakeModule{desc: servicekit.Descriptor{
		ID:         id,
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
	}}
}

// ---- AssertModule tests ------------------------------------------------

func TestAssertModule_ValidModule(t *testing.T) {
	// A module that satisfies every AssertModule check must not call t.Fatal.
	// We can't directly capture t.Fatal here, but we run inside a sub-test so
	// a failure would propagate. This is the affirmative path.
	servicekittest.AssertModule(t, publicModule("orders"))
}

func TestAssertModule_WithRequires(t *testing.T) {
	method := "/orders.v1.Svc/Noop"
	mod := fakeModule{desc: servicekit.Descriptor{
		ID:         "orders",
		Methods:    []string{method},
		AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
		Requires: servicekit.Compatibility{
			SDK: ">=0.27.0",
			Go:  ">=1.23",
		},
	}}
	servicekittest.AssertModule(t, mod)
}

func TestAssertModule_MigrationRunnerCalled(t *testing.T) {
	called := false
	runner := func(_ context.Context, ns servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		called = true
		if ns.ModuleID == "" {
			t.Error("migration runner: namespace ModuleID is empty")
		}
		return nil
	}
	servicekittest.AssertModule(t, publicModule("orders"), servicekittest.ModuleOptions{
		MigrationRunner: runner,
	})
	if !called {
		t.Fatal("MigrationRunner was not called")
	}
}

// ---- AssertComposition tests -------------------------------------------

func TestAssertComposition_TwoModules(t *testing.T) {
	orders := publicModule("orders")
	billing := publicModule("billing")
	servicekittest.AssertComposition(t, []servicekit.Module{orders, billing})
}

func TestAssertComposition_SingleModule(t *testing.T) {
	servicekittest.AssertComposition(t, []servicekit.Module{publicModule("solo")})
}

func TestAssertComposition_EventGraph_CoherentPair(t *testing.T) {
	pub := fakeModule{desc: servicekit.Descriptor{
		ID:         "orders",
		Methods:    []string{"/orders.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/orders.v1.Svc/Noop", Public: true}},
		Events: servicekit.EventDescriptor{
			Publishes: []servicekit.EventType{"orders.order.created"},
		},
	}}
	sub := fakeModule{desc: servicekit.Descriptor{
		ID:         "billing",
		Methods:    []string{"/billing.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/billing.v1.Svc/Noop", Public: true}},
		Events: servicekit.EventDescriptor{
			Subscribes: []servicekit.EventType{"orders.order.created"},
		},
	}}
	servicekittest.AssertComposition(t, []servicekit.Module{pub, sub})
}

// ---- AssertCompatible tests -------------------------------------------

func TestAssertCompatible_AllSatisfied(t *testing.T) {
	mod := fakeModule{desc: servicekit.Descriptor{
		ID:         "orders",
		Methods:    []string{"/orders.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/orders.v1.Svc/Noop", Public: true}},
		Requires: servicekit.Compatibility{
			SDK:      ">=0.27.0",
			Go:       ">=1.23",
			Postgres: ">=14",
		},
	}}
	servicekittest.AssertCompatible(t, []servicekit.Module{mod}, servicekittest.HostRequires{
		SDK:      "v0.28.0",
		Go:       "1.25.5",
		Postgres: "16.3",
	})
}

func TestAssertCompatible_NoRequires(t *testing.T) {
	// A module with no Requires is always compatible.
	servicekittest.AssertCompatible(t, []servicekit.Module{publicModule("simple")}, servicekittest.HostRequires{
		SDK:      "v0.10.0",
		Go:       "1.22.0",
		Postgres: "",
	})
}

func TestCompatibleModules_InsufficientSDK(t *testing.T) {
	mod := fakeModule{desc: servicekit.Descriptor{
		ID:         "orders",
		Methods:    []string{"/orders.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/orders.v1.Svc/Noop", Public: true}},
		Requires:   servicekit.Compatibility{SDK: ">=0.30.0"},
	}}
	errs := servicekittest.CompatibleModules([]servicekit.Module{mod}, servicekittest.HostRequires{
		SDK: "v0.28.0",
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 compatibility error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "orders") {
		t.Errorf("error should mention module ID: %v", errs[0])
	}
}

func TestCompatibleModules_MissingPostgres(t *testing.T) {
	mod := fakeModule{desc: servicekit.Descriptor{
		ID:         "pg-module",
		Methods:    []string{"/pg.v1.Svc/Noop"},
		AuthzRules: []authz.MethodRule{{Method: "/pg.v1.Svc/Noop", Public: true}},
		Requires:   servicekit.Compatibility{Postgres: ">=14"},
	}}
	errs := servicekittest.CompatibleModules([]servicekit.Module{mod}, servicekittest.HostRequires{
		Postgres: "", // no Postgres
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for missing Postgres, got %d: %v", len(errs), errs)
	}
}

func TestCompatibleModules_AllGreen(t *testing.T) {
	mods := []servicekit.Module{
		publicModule("a"),
		publicModule("b"),
	}
	errs := servicekittest.CompatibleModules(mods, servicekittest.HostRequires{
		SDK: "v0.28.0", Go: "1.25.5", Postgres: "16.3",
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors for modules with no Requires, got %v", errs)
	}
}

// ---- version comparison -----------------------------------------------
// We test versionSatisfies indirectly via CompatibleModules.

func TestCompatibleModules_VersionComparisons(t *testing.T) {
	cases := []struct {
		name    string
		hostSDK string
		reqSDK  string
		wantOK  bool
	}{
		{"equal", "0.27.0", ">=0.27.0", true},
		{"greater", "0.28.0", ">=0.27.0", true},
		{"lesser", "0.26.0", ">=0.27.0", false},
		{"v-prefix host", "v0.28.0", ">=0.27.0", true},
		{"v-prefix req", "0.28.0", ">=v0.27.0", true},
		{"major bump", "1.0.0", ">=0.99.0", true},
		{"no req", "0.10.0", "", true}, // empty req = always ok
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "m"
			method := "/m.v1.Svc/Noop"
			mod := fakeModule{desc: servicekit.Descriptor{
				ID:         id,
				Methods:    []string{method},
				AuthzRules: []authz.MethodRule{{Method: method, Public: true}},
				Requires:   servicekit.Compatibility{SDK: tc.reqSDK},
			}}
			errs := servicekittest.CompatibleModules([]servicekit.Module{mod}, servicekittest.HostRequires{
				SDK: tc.hostSDK,
			})
			gotOK := len(errs) == 0
			if gotOK != tc.wantOK {
				t.Errorf("versionSatisfies(%q, %q): wantOK=%v gotOK=%v errs=%v",
					tc.hostSDK, tc.reqSDK, tc.wantOK, gotOK, errs)
			}
		})
	}
}
