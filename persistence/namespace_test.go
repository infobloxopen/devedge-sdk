package persistence

import "testing"

func TestQualifyTable(t *testing.T) {
	cases := []struct {
		name string
		ns   DatabaseNamespace
		base string
		want string
	}{
		{"schema", DatabaseNamespace{Schema: "orders"}, "outbox", "orders.outbox"},
		{"prefix", DatabaseNamespace{TablePrefix: "ord_"}, "outbox", "ord_outbox"},
		{"none", DatabaseNamespace{}, "outbox", "outbox"},
		{"schema-wins-over-prefix", DatabaseNamespace{Schema: "orders", TablePrefix: "ord_"}, "outbox", "orders.outbox"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ns.QualifyTable(c.base); got != c.want {
				t.Errorf("QualifyTable(%q) = %q, want %q", c.base, got, c.want)
			}
		})
	}
}

func TestDatabaseNamespace_IsZero(t *testing.T) {
	if !(DatabaseNamespace{ModuleID: "orders"}).IsZero() {
		t.Error("namespace with only ModuleID should be zero for qualification")
	}
	if (DatabaseNamespace{Schema: "orders"}).IsZero() {
		t.Error("namespace with Schema should not be zero")
	}
	if (DatabaseNamespace{TablePrefix: "ord_"}).IsZero() {
		t.Error("namespace with TablePrefix should not be zero")
	}
}

func TestResolveNamespace(t *testing.T) {
	cases := []struct {
		name       string
		policy     IsolationPolicy
		engine     string
		wantSchema string
		wantPrefix string
		wantErr    bool
		wantMigTbl string
	}{
		{"schema-preferred-postgres", IsolationSchemaPreferred, "postgres", "orders", "", false, "schema_migrations"},
		{"schema-preferred-sqlite-falls-back", IsolationSchemaPreferred, "sqlite", "", "orders_", false, "orders_schema_migrations"},
		{"unset-defaults-to-schema-preferred-postgres", IsolationUnset, "postgres", "orders", "", false, "schema_migrations"},
		{"schema-required-postgres", IsolationSchemaRequired, "postgres", "orders", "", false, "schema_migrations"},
		{"schema-required-sqlite-fails", IsolationSchemaRequired, "sqlite", "", "", true, ""},
		{"prefix-required-postgres", IsolationPrefixRequired, "postgres", "", "orders_", false, "orders_schema_migrations"},
		{"prefix-required-sqlite", IsolationPrefixRequired, "sqlite", "", "orders_", false, "orders_schema_migrations"},
		{"dedicated-required", IsolationDedicatedRequired, "postgres", "", "", false, "schema_migrations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns, err := ResolveNamespace(c.policy, "orders", c.engine, "", "")
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got ns=%+v", ns)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns.Schema != c.wantSchema {
				t.Errorf("Schema = %q, want %q", ns.Schema, c.wantSchema)
			}
			if ns.TablePrefix != c.wantPrefix {
				t.Errorf("TablePrefix = %q, want %q", ns.TablePrefix, c.wantPrefix)
			}
			if ns.MigrationTable != c.wantMigTbl {
				t.Errorf("MigrationTable = %q, want %q", ns.MigrationTable, c.wantMigTbl)
			}
			if ns.ModuleID != "orders" {
				t.Errorf("ModuleID = %q, want orders", ns.ModuleID)
			}
		})
	}
}

func TestResolveNamespace_PreferredOverrides(t *testing.T) {
	ns, err := ResolveNamespace(IsolationSchemaRequired, "orders", "postgres", "custom_schema", "")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Schema != "custom_schema" {
		t.Errorf("Schema = %q, want custom_schema (preferred override)", ns.Schema)
	}
}

func TestResolveNamespace_SanitizesModuleID(t *testing.T) {
	ns, err := ResolveNamespace(IsolationSchemaPreferred, "Orders.V1", "postgres", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Schema != "orders_v1" {
		t.Errorf("Schema = %q, want sanitized orders_v1", ns.Schema)
	}
}

func TestResolveNamespace_EmptyModuleID(t *testing.T) {
	if _, err := ResolveNamespace(IsolationSchemaPreferred, "  ", "postgres", "", ""); err == nil {
		t.Error("expected error for empty module ID")
	}
}

func TestNamespacedPostgresDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		ns   DatabaseNamespace
		want string
	}{
		{"url-no-query", "postgres://u:p@h:5432/db", DatabaseNamespace{Schema: "orders"}, "postgres://u:p@h:5432/db?search_path=orders,public"},
		{"url-with-query", "postgres://u:p@h:5432/db?sslmode=disable", DatabaseNamespace{Schema: "orders"}, "postgres://u:p@h:5432/db?sslmode=disable&search_path=orders,public"},
		{"keyword", "host=h port=5432 dbname=db", DatabaseNamespace{Schema: "orders"}, "host=h port=5432 dbname=db search_path=orders,public"},
		{"no-schema-unchanged", "postgres://u:p@h/db", DatabaseNamespace{TablePrefix: "ord_"}, "postgres://u:p@h/db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NamespacedPostgresDSN(c.dsn, c.ns); got != c.want {
				t.Errorf("NamespacedPostgresDSN = %q, want %q", got, c.want)
			}
		})
	}
}
