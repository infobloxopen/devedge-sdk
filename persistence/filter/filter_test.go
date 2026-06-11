package filter_test

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence/filter"
)

var cols = map[string]string{
	"name":       "name",
	"status":     "status",
	"weight":     "weight",
	"created_at": "created_at",
	"active":     "active",
}

// TestParse_comparisons verifies all six operators and the has operator.
func TestParse_comparisons(t *testing.T) {
	cases := []struct {
		expr    string
		wantSQL string
		wantArg interface{}
	}{
		{`name = "alice"`, `name = ?`, "alice"},
		{`name != "bob"`, `name != ?`, "bob"},
		{`weight < 10`, `weight < ?`, "10"},
		{`weight <= 10`, `weight <= ?`, "10"},
		{`weight > 5`, `weight > ?`, "5"},
		{`weight >= 5`, `weight >= ?`, "5"},
		{`name:"partial"`, `name LIKE ?`, "%partial%"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			cond, err := filter.Parse(tc.expr, cols)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.expr, err)
			}
			sql, args := cond.SQL()
			if sql != tc.wantSQL {
				t.Errorf("SQL: got %q, want %q", sql, tc.wantSQL)
			}
			if len(args) != 1 || args[0] != tc.wantArg {
				t.Errorf("args: got %v, want [%v]", args, tc.wantArg)
			}
		})
	}
}

// TestParse_and verifies AND combinations.
func TestParse_and(t *testing.T) {
	cond, err := filter.Parse(`name = "alice" AND weight > 5`, cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := cond.SQL()
	if !strings.Contains(sql, "AND") {
		t.Errorf("expected AND in SQL, got %q", sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %v", args)
	}
}

// TestParse_or verifies OR combinations.
func TestParse_or(t *testing.T) {
	cond, err := filter.Parse(`status = "active" OR status = "pending"`, cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := cond.SQL()
	if !strings.Contains(sql, "OR") {
		t.Errorf("expected OR in SQL, got %q", sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %v", args)
	}
}

// TestParse_not verifies NOT prefix.
func TestParse_not(t *testing.T) {
	cond, err := filter.Parse(`NOT name = "alice"`, cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := cond.SQL()
	if !strings.Contains(sql, "NOT") {
		t.Errorf("expected NOT in SQL, got %q", sql)
	}
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %v", args)
	}
}

// TestParse_grouping verifies parenthesised grouping.
func TestParse_grouping(t *testing.T) {
	cond, err := filter.Parse(`(name = "a" OR name = "b") AND weight > 0`, cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := cond.SQL()
	if !strings.Contains(sql, "OR") || !strings.Contains(sql, "AND") {
		t.Errorf("expected OR and AND in SQL, got %q", sql)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(args), args)
	}
}

// TestParse_unknownField verifies that an unknown field returns InvalidArgument.
func TestParse_unknownField(t *testing.T) {
	_, err := filter.Parse(`unknown_field = "x"`, cols)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// TestParse_sqlInjection verifies that SQL metacharacters in values are bind args,
// never interpolated into the SQL string.
func TestParse_sqlInjection(t *testing.T) {
	malicious := `'; DROP TABLE widgets; --`
	cond, err := filter.Parse(`name = "`+malicious+`"`, cols)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	sql, args := cond.SQL()
	// The SQL string must only contain "name = ?" — the malicious value must NOT appear.
	if strings.Contains(sql, "DROP") || strings.Contains(sql, ";") {
		t.Errorf("SQL injection: malicious content in SQL string: %q", sql)
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(args))
	}
	if args[0] != malicious {
		t.Errorf("arg value mismatch: got %v, want %q", args[0], malicious)
	}
}

// TestParse_empty verifies empty expression returns nil condition.
func TestParse_empty(t *testing.T) {
	cond, err := filter.Parse("", cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cond != nil {
		t.Errorf("expected nil condition for empty expr, got %v", cond)
	}
}

// TestParseOrderBy_valid verifies successful order_by parsing.
func TestParseOrderBy_valid(t *testing.T) {
	clauses, err := filter.ParseOrderBy("name asc, weight desc", cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clauses) != 2 {
		t.Fatalf("expected 2 clauses, got %d", len(clauses))
	}
	if clauses[0].GORMExpr() != "name ASC" {
		t.Errorf("got %q, want %q", clauses[0].GORMExpr(), "name ASC")
	}
	if clauses[1].GORMExpr() != "weight DESC" {
		t.Errorf("got %q, want %q", clauses[1].GORMExpr(), "weight DESC")
	}
}

// TestParseOrderBy_defaultAsc verifies that direction defaults to ASC.
func TestParseOrderBy_defaultAsc(t *testing.T) {
	clauses, err := filter.ParseOrderBy("name", cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clauses) != 1 || clauses[0].GORMExpr() != "name ASC" {
		t.Errorf("got %v", clauses)
	}
}

// TestParseOrderBy_unknownField verifies unknown field returns InvalidArgument.
func TestParseOrderBy_unknownField(t *testing.T) {
	_, err := filter.ParseOrderBy("nonexistent desc", cols)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// TestParseOrderBy_empty verifies empty string returns nil.
func TestParseOrderBy_empty(t *testing.T) {
	clauses, err := filter.ParseOrderBy("", cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clauses != nil {
		t.Errorf("expected nil, got %v", clauses)
	}
}
