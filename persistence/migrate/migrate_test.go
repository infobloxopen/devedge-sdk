package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// frameworkUp is a stand-in for the SDK baseline's single up file (version 1), used to
// build composed FSs in the docker-free helper tests.
var frameworkUp = fstest.MapFS{
	"0001_framework_init.up.sql":   {Data: []byte(`CREATE TABLE outbox ("id" text);`)},
	"0001_framework_init.down.sql": {Data: []byte(`DROP TABLE outbox;`)},
}

// TestMaterialize_ComposesAndOrders stages the framework baseline ahead of a module FS
// and returns the highest version across both sets.
func TestMaterialize_ComposesAndOrders(t *testing.T) {
	module := fstest.MapFS{
		"0002_widgets.up.sql":   {Data: []byte(`CREATE TABLE widgets ("id" text);`)},
		"0002_widgets.down.sql": {Data: []byte(`DROP TABLE widgets;`)},
		"0003_index.up.sql":     {Data: []byte(`CREATE INDEX ix ON widgets(id);`)},
		"0003_index.down.sql":   {Data: []byte(`DROP INDEX ix;`)},
	}
	target, err := materialize(t.TempDir(), frameworkUp, module)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if target != 3 {
		t.Errorf("target = %d, want 3", target)
	}
}

// TestMaterialize_DuplicateVersionFailsLoud proves a module that ships 0001 (colliding
// with the framework baseline) is rejected with a clear error, not silently merged.
func TestMaterialize_DuplicateVersionFailsLoud(t *testing.T) {
	module := fstest.MapFS{
		"0001_widgets.up.sql":   {Data: []byte(`CREATE TABLE widgets ("id" text);`)},
		"0001_widgets.down.sql": {Data: []byte(`DROP TABLE widgets;`)},
	}
	_, err := materialize(t.TempDir(), frameworkUp, module)
	if err == nil {
		t.Fatal("expected a duplicate-version error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate migration version") {
		t.Errorf("error = %q, want it to mention duplicate migration version", err)
	}
}

// TestMaterialize_MalformedNameFailsLoud proves a stray .sql with a non-conforming name
// is rejected rather than ignored.
func TestMaterialize_MalformedNameFailsLoud(t *testing.T) {
	module := fstest.MapFS{
		"schema.sql": {Data: []byte(`CREATE TABLE x ("id" text);`)},
	}
	_, err := materialize(t.TempDir(), frameworkUp, module)
	if err == nil || !strings.Contains(err.Error(), "malformed migration file name") {
		t.Fatalf("expected malformed-name error, got %v", err)
	}
}

// TestMaterialize_NoUpFails proves an empty composed set is an error (nothing to apply).
func TestMaterialize_NoUpFails(t *testing.T) {
	if _, err := materialize(t.TempDir(), nil, nil); err == nil {
		t.Fatal("expected an error for an empty composed FS, got nil")
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    uint
		wantErr bool
	}{
		"four-digit":   {"0001_framework_init", 1, false},
		"multi-digit":  {"0042_add_thing", 42, false},
		"no-underscore": {"0001", 0, true},
		"non-numeric":  {"abc_thing", 0, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseVersion(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseVersion(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Errorf("parseVersion(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestCountInRange counts up files with a version in (from, to].
func TestCountInRange(t *testing.T) {
	dir := t.TempDir()
	if _, err := materialize(dir, frameworkUp, fstest.MapFS{
		"0002_a.up.sql":   {Data: []byte(`CREATE TABLE a ("id" text);`)},
		"0002_a.down.sql": {Data: []byte(`DROP TABLE a;`)},
		"0003_b.up.sql":   {Data: []byte(`CREATE TABLE b ("id" text);`)},
		"0003_b.down.sql": {Data: []byte(`DROP TABLE b;`)},
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if n := countInRange(dir, 1, 3); n != 2 {
		t.Errorf("countInRange(1,3) = %d, want 2", n)
	}
	if n := countInRange(dir, 0, 3); n != 3 {
		t.Errorf("countInRange(0,3) = %d, want 3", n)
	}
	if n := countInRange(dir, 3, 3); n != 0 {
		t.Errorf("countInRange(3,3) = %d, want 0", n)
	}
}

// TestMigrationURL_SafeConnection asserts the migration connection carries the pgx5
// scheme, lock_timeout, statement_timeout and a per-module search_path.
func TestMigrationURL_SafeConnection(t *testing.T) {
	got, err := migrationURL("postgres://u:p@h:5432/db", "orders", 2*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("migrationURL: %v", err)
	}
	for _, want := range []string{"pgx5://", "lock_timeout%3D2000ms", "statement_timeout%3D60000ms", "search_path%3Dorders%2Cpublic", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("migrationURL missing %q in %q", want, got)
		}
	}
	// An unsupported scheme (MySQL) fails loud in P1.
	if _, err := migrationURL("mysql://u:p@h/db", "", 0, 0); err == nil {
		t.Error("expected mysql scheme to be rejected (fail-loud unsupported), got nil")
	}
}
