package scaffold

import "testing"

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

func assertEq(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}
