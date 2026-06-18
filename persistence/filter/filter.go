// Package filter implements an AIP-160 subset parser for list filter expressions
// and an AIP-132 parser for order_by strings.
//
// All parsed filter conditions produce parameterized SQL — literal values are
// always bind arguments, never interpolated into the SQL string.
package filter

import (
	"fmt"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Condition is a DB-agnostic parameterized WHERE condition.
type Condition interface {
	// SQL returns a parameterized SQL fragment and its bind arguments.
	// Values are always bind args — never string-interpolated into sql.
	SQL() (string, []interface{})
}

// OrderClause is a single validated ORDER BY term.
type OrderClause struct {
	Column string // DB column name (already validated against the whitelist)
	Desc   bool
}

// GORMExpr returns the GORM-compatible order string, e.g. "created_at DESC".
func (c OrderClause) GORMExpr() string {
	if c.Desc {
		return c.Column + " DESC"
	}
	return c.Column + " ASC"
}

// options carries the dialect-aware settings needed for JSON/tag column access.
// JSON path syntax (`tags.key`) differs by database, so the parser must know the
// target dialect and which columns are JSON/JSONB. The parser stays ORM-agnostic:
// it takes a dialect string, never a *gorm.DB.
type options struct {
	dialect     string            // normalized: "postgres" or "sqlite"
	jsonColumns map[string]string // proto base field name → DB column (JSON/JSONB)
}

// Option configures Parse.
type Option func(*options)

// WithDialect tells the parser which SQL dialect to target for tag/JSON access.
// Recognized values are the gorm dialector names "postgres" and "sqlite" (and
// their aliases). It is required for any filter that references a tag column path
// such as `tags.env`; without it, tag predicates are rejected.
func WithDialect(name string) Option {
	return func(o *options) { o.dialect = normalizeDialect(name) }
}

// WithJSONColumns registers the JSON/JSONB columns (proto field name → DB column)
// that admit `field.key` value access and the has(field.key) presence function.
func WithJSONColumns(cols map[string]string) Option {
	return func(o *options) { o.jsonColumns = cols }
}

func normalizeDialect(name string) string {
	switch strings.ToLower(name) {
	case "postgres", "postgresql", "pgx":
		return "postgres"
	case "sqlite", "sqlite3":
		return "sqlite"
	default:
		return strings.ToLower(name)
	}
}

// Parse parses an AIP-160 filter expression and validates every referenced field
// name against the columns whitelist.
//
// columns maps proto field name → DB column name.
// An empty expr returns (nil, nil) — no condition needs to be applied.
// Unknown field names return a codes.InvalidArgument error.
//
// Pass WithJSONColumns + WithDialect to enable tag (map<string,string>) filtering:
// `tags.<key> = "v"`, `tags.<key> != "v"`, and `has(tags.<key>)`.
func Parse(expr string, columns map[string]string, opts ...Option) (Condition, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	p := &parser{tokens: tokenize(expr), pos: 0, columns: columns, opts: o}
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected token %q in filter", p.peek().val)
	}
	return cond, nil
}

// ParseOrderBy parses a comma-separated AIP-132 order_by string.
// Each term is "field", "field asc", or "field desc" (case-insensitive).
// Returns (nil, nil) for an empty string.
// Unknown fields return a codes.InvalidArgument error.
func ParseOrderBy(orderBy string, columns map[string]string) ([]OrderClause, error) {
	orderBy = strings.TrimSpace(orderBy)
	if orderBy == "" {
		return nil, nil
	}
	parts := strings.Split(orderBy, ",")
	clauses := make([]OrderClause, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) == 0 || len(fields) > 2 {
			return nil, status.Errorf(codes.InvalidArgument, "invalid order_by term %q", part)
		}
		fieldName := fields[0]
		col, ok := columns[fieldName]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown field %q in order_by", fieldName)
		}
		clause := OrderClause{Column: col}
		if len(fields) == 2 {
			switch strings.ToLower(fields[1]) {
			case "asc":
				// default
			case "desc":
				clause.Desc = true
			default:
				return nil, status.Errorf(codes.InvalidArgument, "invalid order direction %q (want asc or desc)", fields[1])
			}
		}
		clauses = append(clauses, clause)
	}
	return clauses, nil
}

// ---- token types ----

type tokKind int

const (
	tokIdent  tokKind = iota // identifier or keyword
	tokString                // double-quoted string literal
	tokNumber                // numeric literal
	tokEQ                    // =
	tokNEQ                   // !=
	tokLT                    // <
	tokLTE                   // <=
	tokGT                    // >
	tokGTE                   // >=
	tokHAS                   // : (has / contains)
	tokLParen                // (
	tokRParen                // )
	tokEOF
)

type token struct {
	kind tokKind
	val  string
}

// ---- lexer ----

func tokenize(s string) []token {
	var tokens []token
	i := 0
	for i < len(s) {
		// Skip whitespace.
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		if i >= len(s) {
			break
		}
		switch {
		case s[i] == '"':
			// Double-quoted string.
			j := i + 1
			var buf strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					switch s[j] {
					case '"':
						buf.WriteByte('"')
					case '\\':
						buf.WriteByte('\\')
					default:
						buf.WriteByte('\\')
						buf.WriteByte(s[j])
					}
				} else {
					buf.WriteByte(s[j])
				}
				j++
			}
			j++ // consume closing "
			tokens = append(tokens, token{tokString, buf.String()})
			i = j

		case s[i] == '(':
			tokens = append(tokens, token{tokLParen, "("})
			i++

		case s[i] == ')':
			tokens = append(tokens, token{tokRParen, ")"})
			i++

		case s[i] == ':':
			tokens = append(tokens, token{tokHAS, ":"})
			i++

		case s[i] == '!' && i+1 < len(s) && s[i+1] == '=':
			tokens = append(tokens, token{tokNEQ, "!="})
			i += 2

		case s[i] == '<' && i+1 < len(s) && s[i+1] == '=':
			tokens = append(tokens, token{tokLTE, "<="})
			i += 2

		case s[i] == '>' && i+1 < len(s) && s[i+1] == '=':
			tokens = append(tokens, token{tokGTE, ">="})
			i += 2

		case s[i] == '<':
			tokens = append(tokens, token{tokLT, "<"})
			i++

		case s[i] == '>':
			tokens = append(tokens, token{tokGT, ">"})
			i++

		case s[i] == '=':
			tokens = append(tokens, token{tokEQ, "="})
			i++

		case unicode.IsDigit(rune(s[i])) || (s[i] == '-' && i+1 < len(s) && unicode.IsDigit(rune(s[i+1]))):
			// Numeric literal.
			j := i
			if s[j] == '-' {
				j++
			}
			for j < len(s) && (unicode.IsDigit(rune(s[j])) || s[j] == '.') {
				j++
			}
			tokens = append(tokens, token{tokNumber, s[i:j]})
			i = j

		case isIdentStart(s[i]):
			// Identifier or keyword.
			j := i
			for j < len(s) && isIdentContinue(s[j]) {
				j++
			}
			tokens = append(tokens, token{tokIdent, s[i:j]})
			i = j

		default:
			// Unknown character — emit as an ident-like token so the parser
			// can produce a meaningful error.
			tokens = append(tokens, token{tokIdent, string(s[i])})
			i++
		}
	}
	tokens = append(tokens, token{tokEOF, ""})
	return tokens
}

func isIdentStart(c byte) bool {
	return unicode.IsLetter(rune(c)) || c == '_'
}

func isIdentContinue(c byte) bool {
	return unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' || c == '.'
}

// ---- parser ----

type parser struct {
	tokens  []token
	pos     int
	columns map[string]string
	opts    *options
}

func (p *parser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{tokEOF, ""}
	}
	return p.tokens[p.pos]
}

func (p *parser) peekAt(n int) token {
	if p.pos+n >= len(p.tokens) {
		return token{tokEOF, ""}
	}
	return p.tokens[p.pos+n]
}

func (p *parser) consume() token {
	t := p.peek()
	p.pos++
	return t
}

// parseExpr is the entry point; maps to or_expr.
func (p *parser) parseExpr() (Condition, error) {
	return p.parseOrExpr()
}

func (p *parser) parseOrExpr() (Condition, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokIdent && strings.ToUpper(p.peek().val) == "OR" {
		p.consume()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = &Logical{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAndExpr() (Condition, error) {
	left, err := p.parseNotExpr()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokIdent && strings.ToUpper(p.peek().val) == "AND" {
		p.consume()
		right, err := p.parseNotExpr()
		if err != nil {
			return nil, err
		}
		left = &Logical{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseNotExpr() (Condition, error) {
	if p.peek().kind == tokIdent && strings.ToUpper(p.peek().val) == "NOT" {
		p.consume()
		inner, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return &Negation{Inner: inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Condition, error) {
	// has(<json_col>.<key>) — JSON key-presence predicate. Recognized only as a
	// function call (`has(`); a bare field literally named "has" still parses as a
	// normal comparison.
	if t := p.peek(); t.kind == tokIdent && strings.EqualFold(t.val, "has") && p.peekAt(1).kind == tokLParen {
		return p.parseHas()
	}
	if p.peek().kind == tokLParen {
		p.consume()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, status.Errorf(codes.InvalidArgument, "expected ')' in filter, got %q", p.peek().val)
		}
		p.consume()
		return inner, nil
	}
	return p.parseComparison()
}

// parseHas parses has(<json_col>.<key>) into a JSON key-presence condition.
func (p *parser) parseHas() (Condition, error) {
	p.consume() // has
	p.consume() // (
	pathTok := p.consume()
	if pathTok.kind != tokIdent {
		return nil, status.Errorf(codes.InvalidArgument, "has() expects a tag field path like `tags.key`, got %q", pathTok.val)
	}
	col, key, err := p.resolveJSONPath(pathTok.val)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokRParen {
		return nil, status.Errorf(codes.InvalidArgument, "expected ')' after has(...), got %q", p.peek().val)
	}
	p.consume()
	return &TagPresence{Column: col, Key: key, dialect: p.opts.dialect}, nil
}

// resolveJSONPath splits a `base.key` path and validates base against the JSON
// column whitelist and a supported dialect. Returns the DB column and the key.
func (p *parser) resolveJSONPath(path string) (col, key string, err error) {
	base, key, ok := strings.Cut(path, ".")
	if !ok || base == "" || key == "" {
		return "", "", status.Errorf(codes.InvalidArgument, "expected a tag field path like `tags.key`, got %q", path)
	}
	dbcol, ok := p.opts.jsonColumns[base]
	if !ok {
		return "", "", status.Errorf(codes.InvalidArgument, "unknown tag field %q", path)
	}
	// A dialect is required only to render SQL (the GORM backend). When no dialect
	// is given the parse still succeeds and yields the AST, which alternative
	// backends (e.g. ent) translate themselves; an explicitly unsupported dialect
	// is still rejected.
	if p.opts.dialect != "" && p.opts.dialect != "postgres" && p.opts.dialect != "sqlite" {
		return "", "", status.Errorf(codes.InvalidArgument, "tag filtering is not supported on this database dialect")
	}
	return dbcol, key, nil
}

func (p *parser) parseComparison() (Condition, error) {
	fieldTok := p.peek()
	if fieldTok.kind != tokIdent {
		return nil, status.Errorf(codes.InvalidArgument, "expected field name in filter, got %q", fieldTok.val)
	}
	// Reject keywords used as field names.
	upper := strings.ToUpper(fieldTok.val)
	if upper == "AND" || upper == "OR" || upper == "NOT" {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected keyword %q where field name expected", fieldTok.val)
	}
	p.consume()

	// A dotted path (e.g. `tags.env`) that isn't a literal column name is a JSON
	// (tag) value access. No proto scalar column contains a dot, so this is
	// unambiguous.
	if _, isCol := p.columns[fieldTok.val]; !isCol && strings.Contains(fieldTok.val, ".") {
		col, key, err := p.resolveJSONPath(fieldTok.val)
		if err != nil {
			return nil, err
		}
		return p.parseJSONComparison(col, key)
	}

	col, ok := p.columns[fieldTok.val]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown field %q", fieldTok.val)
	}

	opTok := p.consume()
	var op string
	switch opTok.kind {
	case tokEQ:
		op = "="
	case tokNEQ:
		op = "!="
	case tokLT:
		op = "<"
	case tokLTE:
		op = "<="
	case tokGT:
		op = ">"
	case tokGTE:
		op = ">="
	case tokHAS:
		op = "LIKE"
	default:
		return nil, status.Errorf(codes.InvalidArgument, "expected operator in filter, got %q", opTok.val)
	}

	valTok := p.consume()
	var val interface{}
	switch valTok.kind {
	case tokString:
		val = valTok.val
	case tokNumber:
		val = valTok.val // keep as string; GORM will bind it correctly
	case tokIdent:
		// bare identifier value (e.g. true, false, an enum name)
		val = valTok.val
	default:
		return nil, status.Errorf(codes.InvalidArgument, "expected value in filter, got %q", valTok.val)
	}

	if op == "LIKE" {
		val = fmt.Sprintf("%%%v%%", val)
	}

	return &Comparison{Column: col, Op: op, Value: val}, nil
}

// parseJSONComparison parses the operator and value after a `tags.key` path.
// Only = and != are supported (tag values are text); the key and value are bind
// args, never interpolated.
func (p *parser) parseJSONComparison(col, key string) (Condition, error) {
	opTok := p.consume()
	var op string
	switch opTok.kind {
	case tokEQ:
		op = "="
	case tokNEQ:
		op = "!="
	default:
		return nil, status.Errorf(codes.InvalidArgument, "operator %q is not supported on a tag field (use = or !=)", opTok.val)
	}
	valTok := p.consume()
	var val interface{}
	switch valTok.kind {
	case tokString, tokNumber, tokIdent:
		val = valTok.val
	default:
		return nil, status.Errorf(codes.InvalidArgument, "expected value after tag field, got %q", valTok.val)
	}
	return &TagComparison{Column: col, Key: key, Op: op, Value: val, dialect: p.opts.dialect}, nil
}

// ---- condition AST ----
//
// The conditions returned by Parse form a small, exported AST. SQL() renders
// dialect SQL for the GORM backend; alternative backends (e.g. ent) type-switch
// on these nodes and build their own predicates. The dialect needed by SQL() for
// tag predicates is captured at Parse via WithDialect and is irrelevant to a
// caller that translates the AST itself.

// Comparison is a scalar column comparison: Column Op Value.
type Comparison struct {
	Column string
	Op     string // =, !=, <, <=, >, >=, LIKE
	Value  interface{}
}

func (c *Comparison) SQL() (string, []interface{}) {
	return fmt.Sprintf("%s %s ?", c.Column, c.Op), []interface{}{c.Value}
}

// TagComparison is an equality/inequality test on a single key of a JSON/JSONB
// (tag) column: Column.Key Op Value. Column is a generator-controlled column name
// (safe to interpolate); Key and Value are always bind args. The extracted value
// is text:
//
//	postgres: <col> ->> ? <op> ?            args: [key, val]
//	sqlite:   json_extract(<col>, ?) <op> ? args: ["$."+key, val]
type TagComparison struct {
	Column  string
	Key     string
	Op      string // "=" or "!="
	Value   interface{}
	dialect string // for SQL() rendering only; ignored by AST translators
}

func (c *TagComparison) SQL() (string, []interface{}) {
	if c.dialect == "postgres" {
		return fmt.Sprintf("%s ->> ? %s ?", c.Column, c.Op), []interface{}{c.Key, c.Value}
	}
	// sqlite (and other json_extract dialects)
	return fmt.Sprintf("json_extract(%s, ?) %s ?", c.Column, c.Op), []interface{}{"$." + c.Key, c.Value}
}

// TagPresence tests whether a JSON/JSONB (tag) column contains a given key:
// has(Column.Key). On Postgres it uses jsonb_exists rather than the `?` operator,
// which would collide with the bind-parameter placeholder.
//
//	postgres: jsonb_exists(<col>, ?)            args: [key]
//	sqlite:   json_extract(<col>, ?) IS NOT NULL args: ["$."+key]
type TagPresence struct {
	Column  string
	Key     string
	dialect string // for SQL() rendering only; ignored by AST translators
}

func (c *TagPresence) SQL() (string, []interface{}) {
	if c.dialect == "postgres" {
		return fmt.Sprintf("jsonb_exists(%s, ?)", c.Column), []interface{}{c.Key}
	}
	return fmt.Sprintf("json_extract(%s, ?) IS NOT NULL", c.Column), []interface{}{"$." + c.Key}
}

// Logical is AND/OR over two sub-conditions.
type Logical struct {
	Op    string // "AND" or "OR"
	Left  Condition
	Right Condition
}

func (b *Logical) SQL() (string, []interface{}) {
	lsql, largs := b.Left.SQL()
	rsql, rargs := b.Right.SQL()
	sql := fmt.Sprintf("(%s) %s (%s)", lsql, b.Op, rsql)
	args := append(largs, rargs...) //nolint:gocritic
	return sql, args
}

// Negation is NOT over a sub-condition.
type Negation struct {
	Inner Condition
}

func (n *Negation) SQL() (string, []interface{}) {
	isql, iargs := n.Inner.SQL()
	return fmt.Sprintf("NOT (%s)", isql), iargs
}
