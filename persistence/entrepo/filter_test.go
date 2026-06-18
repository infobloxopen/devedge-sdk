package entrepo_test

import (
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
)

var (
	filterCols     = map[string]string{"name": "name", "label": "label"}
	filterJSONCols = map[string]string{"tags": "tags"}
)

// render parses expr into an ent predicate and renders the SQL it produces for
// the given dialect (no database needed).
func render(t *testing.T, dia, expr string) (string, []interface{}) {
	t.Helper()
	pred, err := entrepo.FilterPredicate(expr, filterCols, filterJSONCols)
	if err != nil {
		t.Fatalf("FilterPredicate(%q): %v", expr, err)
	}
	if pred == nil {
		return "", nil
	}
	s := sql.Dialect(dia).Select("*").From(sql.Table("apikeys"))
	pred(s)
	return s.Query()
}

// TestFilterPredicate_tagEquality_sqlite renders a JSON-extract predicate for
// SQLite with the value as a bind arg.
func TestFilterPredicate_tagEquality_sqlite(t *testing.T) {
	q, args := render(t, dialect.SQLite, `tags.env = "prod"`)
	if !strings.Contains(strings.ToUpper(q), "JSON_EXTRACT") {
		t.Errorf("expected JSON_EXTRACT in %q", q)
	}
	if !strings.Contains(q, "tags") {
		t.Errorf("expected the tags column in %q", q)
	}
	if len(args) != 1 || args[0] != "prod" {
		t.Errorf("args: got %v, want [prod]", args)
	}
}

// TestFilterPredicate_tagEquality_postgres renders the Postgres JSON operator.
func TestFilterPredicate_tagEquality_postgres(t *testing.T) {
	q, _ := render(t, dialect.Postgres, `tags.env = "prod"`)
	if !strings.Contains(q, "tags") || !strings.Contains(q, "->") {
		t.Errorf("expected a postgres json operator on tags in %q", q)
	}
}

// TestFilterPredicate_tagPresence renders a key-presence predicate.
func TestFilterPredicate_tagPresence(t *testing.T) {
	q, _ := render(t, dialect.SQLite, `has(tags.team)`)
	up := strings.ToUpper(q)
	if !strings.Contains(up, "JSON_TYPE") && !strings.Contains(up, "JSON_EXTRACT") {
		t.Errorf("expected a JSON presence check in %q", q)
	}
}

// TestFilterPredicate_combined verifies AND composition keeps the bind arg.
func TestFilterPredicate_combined(t *testing.T) {
	q, args := render(t, dialect.SQLite, `tags.env = "prod" AND has(tags.team)`)
	if !strings.Contains(q, "AND") {
		t.Errorf("expected AND in %q", q)
	}
	if len(args) != 1 || args[0] != "prod" {
		t.Errorf("args: got %v, want [prod]", args)
	}
}

// TestFilterPredicate_scalar verifies a plain scalar column predicate.
func TestFilterPredicate_scalar(t *testing.T) {
	q, args := render(t, dialect.SQLite, `name = "alice"`)
	if !strings.Contains(q, "name") {
		t.Errorf("expected the name column in %q", q)
	}
	if len(args) != 1 || args[0] != "alice" {
		t.Errorf("args: got %v, want [alice]", args)
	}
}

// TestFilterPredicate_empty returns no predicate for an empty expression.
func TestFilterPredicate_empty(t *testing.T) {
	pred, err := entrepo.FilterPredicate("", filterCols, filterJSONCols)
	if err != nil {
		t.Errorf("empty expr: unexpected error %v", err)
	}
	if pred != nil {
		t.Error("empty expr: expected a nil predicate")
	}
}

// TestFilterPredicate_errors rejects unknown fields and unsupported tag operators.
func TestFilterPredicate_errors(t *testing.T) {
	for _, expr := range []string{`bogus = "x"`, `tags.env < "x"`, `meta.k = "v"`} {
		_, err := entrepo.FilterPredicate(expr, filterCols, filterJSONCols)
		if err == nil {
			t.Errorf("expected error for %q", expr)
			continue
		}
		if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
			t.Errorf("%q: expected InvalidArgument, got %v", expr, err)
		}
	}
}
