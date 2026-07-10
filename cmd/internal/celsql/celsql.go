// Package celsql compiles a CEL expression (a "cel"-flavored calculated search
// source, SD-9/FR-A6) into equivalent SQL text expressions — one for Postgres
// (fed to to_tsvector, SD-7) and one for the SQLite fallback (FR-B5) — after
// type-checking the CEL against the resource's proto message descriptor.
//
// # Why this package lives under cmd/ (WS-011 / FR-X1 graph isolation)
//
// github.com/google/cel-go is a build-time-only concern: search sources are
// compiled to SQL during codegen and never at runtime. cel-go MUST NOT enter the
// devedge-sdk *root* module's runtime dependency graph (a downstream app that
// imports only .../server must stay light; see scripts/check-graph-isolation.sh).
//
// The root package internal/aip is imported at runtime by middleware/redact, so
// the CEL compiler cannot live there. It lives here in the cmd module
// (github.com/infobloxopen/devedge-sdk/cmd), a separate nested module that already
// carries the codegen-only heavy dependencies (the CLI imports ent/gorm). The root
// module does not require cmd — cmd requires root — so cel-go reaches only the cmd
// build closure. This package is internal to cmd and is meant to be consumed ONLY
// by the cmd/protoc-gen-* plugins (wiring is a later task; this file is the
// standalone compiler).
//
// # Supported subset (and nothing more)
//
// The compiler accepts exactly the constructs the DDI search shapes need and fails
// loud, naming the offending construct, on anything else — it never guesses SQL:
//
//   - field references: msg.<field>                → the quoted column
//   - ternary  cond ? a : b                        → CASE WHEN <cond> THEN a ELSE b END
//   - map-literal lookup  {k:v, ...}[key]          → CASE <key> WHEN k THEN v ... END
//   - string concat  a + b                         → a || b
//   - string ops  .lowerAscii(), .replace(f, t)    → lower(...), replace(...)
//   - message / map / tags access  msg.m.f, m[k]   → Postgres ->>/#>> , SQLite json_extract
//   - equality / boolean conditions (only inside a ternary/map condition):
//     ==, !=, &&, ||, !                            → =, <>, AND, OR, NOT
//
// Arithmetic, comparisons other than ==/!=, comprehensions (.map/.filter/.exists),
// list/struct literals, and any function outside {replace, lowerAscii} are rejected.
package celsql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RootVar is the CEL variable bound to the resource message. Field references in a
// search source are written against it, e.g. `msg.display_name`.
const RootVar = "msg"

// CompileCEL type-checks expr against md, then compiles the supported subset into
// an equivalent Postgres text expression (suitable as a to_tsvector argument) and
// an equivalent SQLite-fallback text expression. It returns a descriptive error —
// naming the unsupported construct — for any CEL outside the supported subset,
// rather than emitting wrong or guessed SQL.
func CompileCEL(expr string, md protoreflect.MessageDescriptor) (postgresExpr, sqliteExpr string, err error) {
	if md == nil {
		return "", "", fmt.Errorf("celsql: nil message descriptor")
	}
	if strings.TrimSpace(expr) == "" {
		return "", "", fmt.Errorf("celsql: empty expression")
	}

	env, err := cel.NewEnv(
		cel.TypeDescs(md.ParentFile()),
		cel.Variable(RootVar, cel.ObjectType(string(md.FullName()))),
		ext.Strings(),
	)
	if err != nil {
		return "", "", fmt.Errorf("celsql: build CEL env: %w", err)
	}

	// Type-check against the message descriptor (the strongest per-flavor
	// validation, SD-9). A parse or check failure aborts here.
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return "", "", fmt.Errorf("celsql: type-check failed: %w", iss.Err())
	}
	if ast.OutputType().Kind() != types.StringKind {
		return "", "", fmt.Errorf("celsql: search source must evaluate to a string, got %s", ast.OutputType())
	}

	c := &compiler{ast: ast.NativeRep()}
	node, err := c.compile(c.ast.Expr())
	if err != nil {
		return "", "", err
	}
	return node.render(dialectPostgres), node.render(dialectSQLite), nil
}

type compiler struct {
	ast *celast.AST
}

// typeKind returns the checked type kind of the node with the given id.
func (c *compiler) typeKind(id int64) types.Kind {
	t := c.ast.GetType(id)
	if t == nil {
		return types.UnspecifiedKind
	}
	return t.Kind()
}

func (c *compiler) compile(e celast.Expr) (sqlNode, error) {
	switch e.Kind() {
	case celast.LiteralKind:
		return literalNode(e.AsLiteral().Value())
	case celast.IdentKind:
		// A bare identifier is only ever the root message here (a value on its own
		// is not text). Anything else was rejected by the type-checker.
		if e.AsIdent() == RootVar {
			return nil, fmt.Errorf("celsql: the whole message %q cannot be used as search text; select a field", RootVar)
		}
		return nil, fmt.Errorf("celsql: unsupported identifier %q", e.AsIdent())
	case celast.SelectKind:
		return c.compileSelect(e)
	case celast.CallKind:
		return c.compileCall(e)
	case celast.MapKind:
		return nil, fmt.Errorf("celsql: a bare map literal is not searchable text; index it, e.g. {..}[msg.field]")
	case celast.ListKind:
		return nil, fmt.Errorf("celsql: unsupported CEL construct: list literal")
	case celast.StructKind:
		return nil, fmt.Errorf("celsql: unsupported CEL construct: struct/message literal")
	case celast.ComprehensionKind:
		return nil, fmt.Errorf("celsql: unsupported CEL construct: comprehension (.map/.filter/.all/.exists)")
	default:
		return nil, fmt.Errorf("celsql: unsupported CEL construct (kind %v)", e.Kind())
	}
}

// compileSelect handles field access: a top-level field on msg becomes a column;
// a field selected from a message-typed operand becomes a JSON path access.
func (c *compiler) compileSelect(e celast.Expr) (sqlNode, error) {
	sel := e.AsSelect()
	if sel.IsTestOnly() {
		return nil, fmt.Errorf("celsql: unsupported CEL construct: has() presence test")
	}
	op := sel.Operand()

	// msg.<field> — a top-level column reference.
	if op.Kind() == celast.IdentKind && op.AsIdent() == RootVar {
		return colNode{name: sel.FieldName()}, nil
	}

	// <message>.<field> — a nested field access on a JSON-backed message column.
	if c.typeKind(op.ID()) == types.StructKind {
		base, path, err := c.resolveAccessChain(e)
		if err != nil {
			return nil, err
		}
		return jsonNode{base: base, path: path}, nil
	}

	return nil, fmt.Errorf("celsql: unsupported field access on %v operand", c.typeKind(op.ID()))
}

func (c *compiler) compileCall(e celast.Expr) (sqlNode, error) {
	call := e.AsCall()
	switch call.FunctionName() {
	case operators.Conditional: // cond ? a : b
		return c.compileTernary(e, call)
	case operators.Index: // coll[key]
		return c.compileIndex(e, call)
	case operators.Add: // string concatenation only
		return c.compileConcat(e, call)
	case operators.Equals:
		return c.compileCmp("=", call)
	case operators.NotEquals:
		return c.compileCmp("<>", call)
	case operators.LogicalAnd:
		return c.compileBoolOp("AND", call)
	case operators.LogicalOr:
		return c.compileBoolOp("OR", call)
	case operators.LogicalNot:
		return c.compileNot(call)
	case "lowerAscii":
		return c.compileLower(call)
	case "replace":
		return c.compileReplace(call)
	default:
		return nil, fmt.Errorf("celsql: unsupported function %q", call.FunctionName())
	}
}

func (c *compiler) compileTernary(e celast.Expr, call celast.CallExpr) (sqlNode, error) {
	args := call.Args()
	if len(args) != 3 {
		return nil, fmt.Errorf("celsql: malformed ternary")
	}
	cond, err := c.compile(args[0])
	if err != nil {
		return nil, err
	}
	then, err := c.compile(args[1])
	if err != nil {
		return nil, err
	}
	els, err := c.compile(args[2])
	if err != nil {
		return nil, err
	}
	return caseCondNode{arms: []condArm{{cond: cond, val: then}}, els: els}, nil
}

func (c *compiler) compileIndex(e celast.Expr, call celast.CallExpr) (sqlNode, error) {
	args := call.Args()
	if len(args) != 2 {
		return nil, fmt.Errorf("celsql: malformed index expression")
	}
	coll, key := args[0], args[1]

	// {k:v, ...}[key] — a map-literal lookup compiles to a searched CASE.
	if coll.Kind() == celast.MapKind {
		return c.compileMapLiteralLookup(coll, key)
	}
	if coll.Kind() == celast.ListKind {
		return nil, fmt.Errorf("celsql: unsupported CEL construct: list index")
	}

	// m[key] on a map/tags field — a JSON key access. The key must be a string.
	base, path, err := c.resolveAccessChain(e)
	if err != nil {
		return nil, err
	}
	return jsonNode{base: base, path: path}, nil
}

func (c *compiler) compileMapLiteralLookup(mapExpr, key celast.Expr) (sqlNode, error) {
	keyNode, err := c.compile(key)
	if err != nil {
		return nil, err
	}
	m := mapExpr.AsMap()
	node := caseKeyNode{key: keyNode}
	for _, entry := range m.Entries() {
		me := entry.AsMapEntry()
		if me.Key().Kind() != celast.LiteralKind {
			return nil, fmt.Errorf("celsql: map-literal keys must be literals")
		}
		kn, err := literalNode(me.Key().AsLiteral().Value())
		if err != nil {
			return nil, err
		}
		vn, err := c.compile(me.Value())
		if err != nil {
			return nil, err
		}
		node.whenK = append(node.whenK, kn)
		node.whenV = append(node.whenV, vn)
	}
	return node, nil
}

func (c *compiler) compileConcat(e celast.Expr, call celast.CallExpr) (sqlNode, error) {
	if c.typeKind(e.ID()) != types.StringKind {
		return nil, fmt.Errorf("celsql: unsupported arithmetic '+'; only string concatenation is supported")
	}
	var parts []sqlNode
	var collect func(x celast.Expr) error
	collect = func(x celast.Expr) error {
		if x.Kind() == celast.CallKind && x.AsCall().FunctionName() == operators.Add &&
			c.typeKind(x.ID()) == types.StringKind {
			for _, a := range x.AsCall().Args() {
				if err := collect(a); err != nil {
					return err
				}
			}
			return nil
		}
		n, err := c.compile(x)
		if err != nil {
			return err
		}
		parts = append(parts, n)
		return nil
	}
	for _, a := range call.Args() {
		if err := collect(a); err != nil {
			return nil, err
		}
	}
	return concatNode{parts: parts}, nil
}

func (c *compiler) compileCmp(op string, call celast.CallExpr) (sqlNode, error) {
	args := call.Args()
	if len(args) != 2 {
		return nil, fmt.Errorf("celsql: malformed comparison")
	}
	l, err := c.compile(args[0])
	if err != nil {
		return nil, err
	}
	r, err := c.compile(args[1])
	if err != nil {
		return nil, err
	}
	return cmpNode{op: op, l: l, r: r}, nil
}

func (c *compiler) compileBoolOp(op string, call celast.CallExpr) (sqlNode, error) {
	var args []sqlNode
	for _, a := range call.Args() {
		n, err := c.compile(a)
		if err != nil {
			return nil, err
		}
		args = append(args, n)
	}
	return boolOpNode{op: op, args: args}, nil
}

func (c *compiler) compileNot(call celast.CallExpr) (sqlNode, error) {
	args := call.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("celsql: malformed negation")
	}
	n, err := c.compile(args[0])
	if err != nil {
		return nil, err
	}
	return notNode{arg: n}, nil
}

func (c *compiler) compileLower(call celast.CallExpr) (sqlNode, error) {
	if call.Target() == nil {
		return nil, fmt.Errorf("celsql: lowerAscii requires a string receiver")
	}
	if len(call.Args()) != 0 {
		return nil, fmt.Errorf("celsql: lowerAscii takes no arguments")
	}
	arg, err := c.compile(call.Target())
	if err != nil {
		return nil, err
	}
	return funcNode{fn: "lower", args: []sqlNode{arg}}, nil
}

func (c *compiler) compileReplace(call celast.CallExpr) (sqlNode, error) {
	if call.Target() == nil {
		return nil, fmt.Errorf("celsql: replace requires a string receiver")
	}
	if len(call.Args()) != 2 {
		return nil, fmt.Errorf("celsql: replace(from, to) takes exactly two arguments (count-limited replace is unsupported)")
	}
	recv, err := c.compile(call.Target())
	if err != nil {
		return nil, err
	}
	from, err := c.compile(call.Args()[0])
	if err != nil {
		return nil, err
	}
	to, err := c.compile(call.Args()[1])
	if err != nil {
		return nil, err
	}
	return funcNode{fn: "replace", args: []sqlNode{recv, from, to}}, nil
}

// resolveAccessChain walks a message/map access chain (Select and Index nodes)
// outward to the base column on msg, accumulating the JSON path. e.g.
// msg.provider.name -> base "provider", path ["name"]; msg.tags["env"] -> base
// "tags", path ["env"].
func (c *compiler) resolveAccessChain(e celast.Expr) (base string, path []string, err error) {
	switch e.Kind() {
	case celast.SelectKind:
		sel := e.AsSelect()
		op := sel.Operand()
		if op.Kind() == celast.IdentKind && op.AsIdent() == RootVar {
			return sel.FieldName(), nil, nil
		}
		base, path, err = c.resolveAccessChain(op)
		if err != nil {
			return "", nil, err
		}
		return base, append(path, sel.FieldName()), nil
	case celast.CallKind:
		call := e.AsCall()
		if call.FunctionName() != operators.Index {
			return "", nil, fmt.Errorf("celsql: unsupported access via %q", call.FunctionName())
		}
		args := call.Args()
		if len(args) != 2 {
			return "", nil, fmt.Errorf("celsql: malformed index expression")
		}
		key := args[1]
		if key.Kind() != celast.LiteralKind {
			return "", nil, fmt.Errorf("celsql: map/JSON access key must be a string literal")
		}
		ks, ok := key.AsLiteral().Value().(string)
		if !ok {
			return "", nil, fmt.Errorf("celsql: map/JSON access key must be a string literal")
		}
		base, path, err = c.resolveAccessChain(args[0])
		if err != nil {
			return "", nil, err
		}
		return base, append(path, ks), nil
	default:
		return "", nil, fmt.Errorf("celsql: unsupported access chain (kind %v)", e.Kind())
	}
}

// literalNode maps a CEL scalar literal to its SQL literal node.
func literalNode(v any) (sqlNode, error) {
	switch x := v.(type) {
	case string:
		return strNode{v: x}, nil
	case int64:
		return intNode{v: x}, nil
	case bool:
		return boolNode{v: x}, nil
	default:
		return nil, fmt.Errorf("celsql: unsupported literal type %T", v)
	}
}

// ---- SQL intermediate representation (rendered per dialect from one walk) ----

type dialect int

const (
	dialectPostgres dialect = iota
	dialectSQLite
)

type sqlNode interface {
	render(dialect) string
}

type colNode struct{ name string }

func (n colNode) render(dialect) string { return quoteIdent(n.name) }

type strNode struct{ v string }

func (n strNode) render(dialect) string { return quoteString(n.v) }

type intNode struct{ v int64 }

func (n intNode) render(dialect) string { return strconv.FormatInt(n.v, 10) }

type boolNode struct{ v bool }

func (n boolNode) render(d dialect) string {
	if d == dialectSQLite { // SQLite has no boolean literal; use 1/0.
		if n.v {
			return "1"
		}
		return "0"
	}
	if n.v {
		return "TRUE"
	}
	return "FALSE"
}

type concatNode struct{ parts []sqlNode }

func (n concatNode) render(d dialect) string {
	rendered := make([]string, len(n.parts))
	for i, p := range n.parts {
		rendered[i] = p.render(d)
	}
	return "(" + strings.Join(rendered, " || ") + ")"
}

type funcNode struct {
	fn   string
	args []sqlNode
}

func (n funcNode) render(d dialect) string {
	rendered := make([]string, len(n.args))
	for i, a := range n.args {
		rendered[i] = a.render(d)
	}
	return n.fn + "(" + strings.Join(rendered, ", ") + ")"
}

// caseKeyNode is a simple CASE over a key expression (map-literal lookup).
type caseKeyNode struct {
	key   sqlNode
	whenK []sqlNode
	whenV []sqlNode
}

func (n caseKeyNode) render(d dialect) string {
	var b strings.Builder
	b.WriteString("CASE ")
	b.WriteString(n.key.render(d))
	for i := range n.whenK {
		b.WriteString(" WHEN ")
		b.WriteString(n.whenK[i].render(d))
		b.WriteString(" THEN ")
		b.WriteString(n.whenV[i].render(d))
	}
	b.WriteString(" ELSE NULL END")
	return b.String()
}

type condArm struct {
	cond sqlNode
	val  sqlNode
}

// caseCondNode is a searched CASE (from a ternary).
type caseCondNode struct {
	arms []condArm
	els  sqlNode
}

func (n caseCondNode) render(d dialect) string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, a := range n.arms {
		b.WriteString(" WHEN ")
		b.WriteString(a.cond.render(d))
		b.WriteString(" THEN ")
		b.WriteString(a.val.render(d))
	}
	b.WriteString(" ELSE ")
	b.WriteString(n.els.render(d))
	b.WriteString(" END")
	return b.String()
}

type cmpNode struct {
	op   string
	l, r sqlNode
}

func (n cmpNode) render(d dialect) string {
	return "(" + n.l.render(d) + " " + n.op + " " + n.r.render(d) + ")"
}

type boolOpNode struct {
	op   string
	args []sqlNode
}

func (n boolOpNode) render(d dialect) string {
	rendered := make([]string, len(n.args))
	for i, a := range n.args {
		rendered[i] = a.render(d)
	}
	return "(" + strings.Join(rendered, " "+n.op+" ") + ")"
}

type notNode struct{ arg sqlNode }

func (n notNode) render(d dialect) string { return "(NOT " + n.arg.render(d) + ")" }

// jsonNode extracts text at a JSON path from a base column (map/tags/message).
type jsonNode struct {
	base string
	path []string
}

func (n jsonNode) render(d dialect) string {
	col := quoteIdent(n.base)
	if d == dialectSQLite {
		var p strings.Builder
		p.WriteString("$")
		for _, k := range n.path {
			p.WriteString(`."`)
			p.WriteString(strings.ReplaceAll(k, `"`, `""`))
			p.WriteString(`"`)
		}
		return "json_extract(" + col + ", " + quoteString(p.String()) + ")"
	}
	// Postgres: single key -> ->> ; multi-level -> #>> text-array path.
	if len(n.path) == 1 {
		return "(" + col + " ->> " + quoteString(n.path[0]) + ")"
	}
	elems := make([]string, len(n.path))
	for i, k := range n.path {
		elems[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(k, `\`, `\\`), `"`, `\"`) + `"`
	}
	return "(" + col + " #>> " + quoteString("{"+strings.Join(elems, ",")+"}") + ")"
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
