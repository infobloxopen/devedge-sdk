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

// Parse parses an AIP-160 filter expression and validates every referenced field
// name against the columns whitelist.
//
// columns maps proto field name → DB column name.
// An empty expr returns (nil, nil) — no condition needs to be applied.
// Unknown field names return a codes.InvalidArgument error.
func Parse(expr string, columns map[string]string) (Condition, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	p := &parser{tokens: tokenize(expr), pos: 0, columns: columns}
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
}

func (p *parser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{tokEOF, ""}
	}
	return p.tokens[p.pos]
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
		left = &binaryCondition{"OR", left, right}
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
		left = &binaryCondition{"AND", left, right}
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
		return &notCondition{inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Condition, error) {
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

	return &comparisonCondition{col: col, op: op, val: val}, nil
}

// ---- condition implementations ----

type comparisonCondition struct {
	col string
	op  string
	val interface{}
}

func (c *comparisonCondition) SQL() (string, []interface{}) {
	return fmt.Sprintf("%s %s ?", c.col, c.op), []interface{}{c.val}
}

type binaryCondition struct {
	op    string // "AND" or "OR"
	left  Condition
	right Condition
}

func (b *binaryCondition) SQL() (string, []interface{}) {
	lsql, largs := b.left.SQL()
	rsql, rargs := b.right.SQL()
	sql := fmt.Sprintf("(%s) %s (%s)", lsql, b.op, rsql)
	args := append(largs, rargs...) //nolint:gocritic
	return sql, args
}

type notCondition struct {
	inner Condition
}

func (n *notCondition) SQL() (string, []interface{}) {
	isql, iargs := n.inner.SQL()
	return fmt.Sprintf("NOT (%s)", isql), iargs
}
