package scaffold

import (
	"strings"
	"testing"
)

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr bool
		check   func(t *testing.T, m *Model)
	}{
		{
			name: "explicit resource",
			opts: Options{Service: "orders", Resource: "Order", Backend: BackendGORM},
			check: func(t *testing.T, m *Model) {
				assertEq(t, "Service", m.Service, "Orders")
				assertEq(t, "ServiceType", m.ServiceType, "OrderService")
				assertEq(t, "Resource", m.Resource, "Order")
				assertEq(t, "ResourceSnake", m.ResourceSnake, "order")
				assertEq(t, "ResourcePlural", m.ResourcePlural, "orders")
				assertEq(t, "Module", m.Module, "github.com/infobloxopen/orders")
				assertEq(t, "RepoName", m.RepoName, "orders")
				assertEq(t, "ProtoPackage", m.ProtoPackage, "orders.v1")
				assertEq(t, "ProtoPathSuffix", m.ProtoPathSuffix, "orders/v1")
				assertEq(t, "GoPkg", m.GoPkg, "ordersv1")
				// Single-segment gen path (gen/<svc>v1), NOT gen/<svc>/v1: required
				// for protoc-gen-ent's sibling ent/ package to line up. Same for
				// both backends so there is one convention.
				assertEq(t, "GoImportPath", m.GoImportPath, "github.com/infobloxopen/orders/gen/ordersv1")
				assertEq(t, "ProtoFile", m.ProtoFile, "orders.proto")
			},
		},
		{
			name: "resource defaults from singularized service",
			opts: Options{Service: "widgets", Backend: BackendGORM},
			check: func(t *testing.T, m *Model) {
				assertEq(t, "Resource", m.Resource, "Widget")
				assertEq(t, "ServiceType", m.ServiceType, "WidgetService")
			},
		},
		{
			name: "custom module + org",
			opts: Options{Service: "catalog", Resource: "Item", Module: "go.acme.dev/catalog", Org: "acme", Backend: BackendGORM},
			check: func(t *testing.T, m *Model) {
				assertEq(t, "Module", m.Module, "go.acme.dev/catalog")
				assertEq(t, "Org", m.Org, "acme")
				assertEq(t, "RepoName", m.RepoName, "catalog")
			},
		},
		{name: "empty service", opts: Options{Backend: BackendGORM}, wantErr: true},
		{name: "uppercase service", opts: Options{Service: "Orders", Backend: BackendGORM}, wantErr: true},
		{name: "bad module path", opts: Options{Service: "orders", Module: "notapath", Backend: BackendGORM}, wantErr: true},
		{name: "missing backend", opts: Options{Service: "orders"}, wantErr: true},
		{name: "unknown backend", opts: Options{Service: "orders", Backend: "mongo"}, wantErr: true},
		{name: "ent is accepted at validate (rejected later)", opts: Options{Service: "orders", Backend: BackendEnt}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := tc.opts.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (model=%+v)", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestSDKVersionResolves(t *testing.T) {
	if v := resolveSDKVersion(); v == "" {
		t.Fatal("resolveSDKVersion returned empty")
	}
}

// TestMakefileIsThinDeShim asserts the WS-023 conversion: the generated top-level
// Makefile is a THIN shim that reads the managed `de sync` fragment and adds only
// project-specific targets. All codegen/build/test/lint logic (and the SDK-version
// pinning that was finding-051/#61) now lives in `de`, so the Makefile must carry
// NO `go install`/`@latest`/`make tools`/`SDK_VERSION` tool blocks, and must pin
// the `de` install to an exact version (never @latest).
func TestMakefileIsThinDeShim(t *testing.T) {
	for _, backend := range []Backend{BackendGORM, BackendEnt} {
		t.Run(string(backend), func(t *testing.T) {
			m, err := Options{Service: "orders", Resource: "Order", Backend: backend}.Validate()
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			out, err := renderTemplate("Makefile.tmpl", m)
			if err != nil {
				t.Fatalf("render Makefile.tmpl: %v", err)
			}
			mk := string(out)

			// Reads the managed fragment `de sync` writes.
			if !strings.Contains(mk, "-include .devedge/make/devedge.mk") {
				t.Errorf("thin Makefile must `-include .devedge/make/devedge.mk`:\n%s", mk)
			}
			// No baked-in codegen-tool installs — the logic lives in `de`. (The one
			// legitimate `go install` is the pinned `de` install hint, checked below.)
			for _, banned := range []string{"@latest", "SDK_VERSION", "protoc-gen-", "make tools", "make bootstrap"} {
				if strings.Contains(mk, banned) {
					t.Errorf("thin Makefile must not contain %q (that logic moved to `de`):\n%s", banned, mk)
				}
			}
			// `de` install hint must pin an exact version.
			if !strings.Contains(mk, "cmd/de@"+m.DeVersion) {
				t.Errorf("Makefile must pin the `de` install to %q:\n%s", m.DeVersion, mk)
			}
			// The project-specific targets survive (they don't delegate to `de`).
			for _, tgt := range []string{"run:", "api-lint:", "api-breaking:", "api-release:"} {
				if !strings.Contains(mk, tgt) {
					t.Errorf("thin Makefile missing project target %q:\n%s", tgt, mk)
				}
			}
		})
	}
}

// TestManagedFragmentDelegatesToDe asserts the committed `.devedge/make/devedge.mk`
// (byte-identical to `de sync` output) carries the generated-code header and
// delegates every build verb to `de`, the hermetic build authority.
func TestManagedFragmentDelegatesToDe(t *testing.T) {
	m, err := Options{Service: "orders", Resource: "Order", Backend: BackendGORM}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := renderTemplate("devedge.mk.tmpl", m)
	if err != nil {
		t.Fatalf("render devedge.mk.tmpl: %v", err)
	}
	frag := string(out)
	if !strings.HasPrefix(frag, "# Code generated by `de sync`. DO NOT EDIT.") {
		t.Errorf("managed fragment must carry the DO NOT EDIT header:\n%s", frag)
	}
	for _, want := range []string{
		"generate: ; @de generate",
		"build:    ; @de build",
		"test:     ; @de test",
		"lint:     ; @de lint",
		"image:    ; @de image",
		"migrate-lint: ; @de migrate lint",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("managed fragment missing target %q:\n%s", want, frag)
		}
	}
}

func assertEq(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}
