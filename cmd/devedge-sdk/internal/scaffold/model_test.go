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

// TestMakefileSDKVersionFromGoMod is the finding-051 (#61) regression: the
// generated Makefile must DERIVE SDK_VERSION from the project's go.mod at
// make-time (go list -m), so `make tools` can never install plugins that lag the
// project's own devedge-sdk require — rather than baking the CLI binary's version
// in as a literal pin. The scaffold-time version is permitted only as the `echo`
// fallback when `go list` can't resolve it yet (fresh clone, pre-download).
func TestMakefileSDKVersionFromGoMod(t *testing.T) {
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

			var sdkLine string
			for _, ln := range strings.Split(mk, "\n") {
				if strings.HasPrefix(ln, "SDK_VERSION") {
					sdkLine = ln
					break
				}
			}
			if sdkLine == "" {
				t.Fatalf("no SDK_VERSION assignment in rendered Makefile:\n%s", mk)
			}

			// Must derive from go.mod via `go list -m`, not a baked literal pin.
			if !strings.Contains(sdkLine,
				"go list -m -f '{{.Version}}' github.com/infobloxopen/devedge-sdk") {
				t.Errorf("SDK_VERSION must derive from go.mod via `go list -m`, got:\n%s", sdkLine)
			}
			// The scaffold-time version may appear ONLY as the `echo` fallback.
			if strings.Contains(sdkLine, m.SDKVersion) && !strings.Contains(sdkLine, "echo "+m.SDKVersion) {
				t.Errorf("scaffold-time version %q must appear only as the `echo` fallback, got:\n%s", m.SDKVersion, sdkLine)
			}
			// `make tools` must install the plugins at $(SDK_VERSION) — i.e. the
			// go.mod-derived value — not at a hardcoded version.
			if !strings.Contains(mk, "protoc-gen-svc@$(SDK_VERSION)") {
				t.Errorf("`make tools` must install SDK plugins at $(SDK_VERSION):\n%s", mk)
			}
		})
	}
}

// TestMakefileGenerateLocksDeps is the dogfood-F-4 regression: the generated
// Makefile's `generate` target must lock the buf deps (googleapis: google/api/*)
// before `buf generate`, so a tree that never locked them — e.g. scaffolded with
// `--no-generate` — does not fail with `import "google/api/annotations.proto": file
// does not exist`. The scaffold pipeline runs `buf dep update` itself on the default
// path; the Makefile must do the same for the user-driven generate step. It is
// guarded on a missing buf.lock so a committed lock is left untouched.
func TestMakefileGenerateLocksDeps(t *testing.T) {
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
			if !strings.Contains(mk, "buf dep update") {
				t.Errorf("generate target must run `buf dep update` to lock googleapis deps:\n%s", mk)
			}
			// The lock must be created only when absent, so a committed buf.lock
			// (fresh-clone, reproducible) is not silently rewritten on every generate.
			if !strings.Contains(mk, "test -f buf.lock || buf dep update") {
				t.Errorf("`buf dep update` must be guarded on a missing buf.lock:\n%s", mk)
			}
		})
	}
}

func assertEq(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}
