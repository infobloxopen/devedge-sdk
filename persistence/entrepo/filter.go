package entrepo

import (
	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence/filter"
)

// FilterPredicate parses an AIP-160 list filter expression and returns an ent
// selector predicate that applies it, or (nil, nil) for an empty expression.
//
// It supports the same grammar as the GORM backend, including tag
// (map<string,string>) access: `tags.<key> = "v"`, `tags.<key> != "v"`, and
// `has(tags.<key>)`. columns and jsonColumns are the scalar and tag (JSON)
// column whitelists (proto field name → DB column) — the generated storage code
// exposes these as `<Message>Columns` and `<Message>JSONColumns`.
//
// The returned predicate is dialect-agnostic: ent renders the dialect-correct
// JSON SQL (via sqljson) when the query is built, so no dialect is supplied here.
// Use it with the generated typed query, converting to the entity predicate type:
//
//	pred, err := entrepo.FilterPredicate(opts.Filter, FooColumns, FooJSONColumns)
//	if err != nil { return ... }
//	if pred != nil { q = q.Where(predicate.Foo(pred)) }
func FilterPredicate(expr string, columns, jsonColumns map[string]string) (func(*sql.Selector), error) {
	cond, err := filter.Parse(expr, columns, filter.WithJSONColumns(jsonColumns))
	if err != nil {
		return nil, err
	}
	if cond == nil {
		return nil, nil
	}
	pred, err := toEntPredicate(cond)
	if err != nil {
		return nil, err
	}
	return func(s *sql.Selector) { s.Where(pred) }, nil
}

// toEntPredicate translates a parsed filter AST into an ent predicate. Tag (JSON)
// predicates go through sqljson, which emits the dialect-appropriate JSON SQL.
func toEntPredicate(c filter.Condition) (*sql.Predicate, error) {
	switch n := c.(type) {
	case *filter.Logical:
		left, err := toEntPredicate(n.Left)
		if err != nil {
			return nil, err
		}
		right, err := toEntPredicate(n.Right)
		if err != nil {
			return nil, err
		}
		if n.Op == "OR" {
			return sql.Or(left, right), nil
		}
		return sql.And(left, right), nil
	case *filter.Negation:
		inner, err := toEntPredicate(n.Inner)
		if err != nil {
			return nil, err
		}
		return sql.Not(inner), nil
	case *filter.Comparison:
		return scalarPredicate(n)
	case *filter.TagComparison:
		switch n.Op {
		case "=":
			return sqljson.ValueEQ(n.Column, n.Value, sqljson.Path(n.Key)), nil
		case "!=":
			return sqljson.ValueNEQ(n.Column, n.Value, sqljson.Path(n.Key)), nil
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported tag operator %q", n.Op)
		}
	case *filter.TagPresence:
		return sqljson.HasKey(n.Column, sqljson.Path(n.Key)), nil
	default:
		return nil, status.Errorf(codes.Internal, "unsupported filter condition %T", c)
	}
}

func scalarPredicate(n *filter.Comparison) (*sql.Predicate, error) {
	switch n.Op {
	case "=":
		return sql.EQ(n.Column, n.Value), nil
	case "!=":
		return sql.NEQ(n.Column, n.Value), nil
	case "<":
		return sql.LT(n.Column, n.Value), nil
	case "<=":
		return sql.LTE(n.Column, n.Value), nil
	case ">":
		return sql.GT(n.Column, n.Value), nil
	case ">=":
		return sql.GTE(n.Column, n.Value), nil
	case "LIKE":
		pat, ok := n.Value.(string)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "the : (has) operator requires a string value")
		}
		return sql.Like(n.Column, pat), nil
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported operator %q", n.Op)
	}
}
