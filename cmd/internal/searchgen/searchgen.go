// Package searchgen is the codegen-only compiler for a resource's resolved
// full-text search surface (WS-041). Given the raw aip.SearchConfig produced by
// internal/aip.ResolveSearchConfig and a target backend dialect, it validates
// every source and compiles them into the argument for a Postgres to_tsvector(…)
// call plus a parallel SQLite text concatenation for the LIKE fallback
// (FR-A2/A3/A5, FR-B3/B5, SD-4/SD-9).
//
// # Why this lives under cmd/ (WS-011 / FR-X1 graph isolation)
//
// It may import cmd/internal/celsql, which pulls github.com/google/cel-go to
// compile `cel`-flavored sources. cel-go is a build-time-only concern and MUST
// NOT enter the devedge-sdk root module's runtime dependency graph
// (scripts/check-graph-isolation.sh). So — exactly like celsql — this package
// lives in the cmd module (a separate nested module carrying the codegen-only
// heavy deps) and is meant to be consumed ONLY by the cmd/protoc-gen-* plugins +
// the OpenAPI enrichment pass. The runtime-safe half (resolving the declared
// surface raw) stays in internal/aip; only compilation is here.
//
// # What it produces
//
// A [Compiled] carries BOTH the Postgres to_tsvector argument and the parallel
// SQLite concatenation, computed in one pass (celsql yields both; a field is
// normalized for both), together with a PostgresOnly flag (set when any source is
// a raw sql/postgres expression, which has no SQLite form) and the ordered
// searchable source names for x-aip-search. The predicate-emission itself (the
// to_tsvector @@ websearch_to_tsquery / LIKE wrappers and the INDEXED migration)
// is a later task; this package only builds the vector argument + validates.
package searchgen

import (
	"fmt"
	"regexp"
	"strings"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	"github.com/infobloxopen/devedge-sdk/cmd/internal/celsql"
	"github.com/infobloxopen/devedge-sdk/internal/aip"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Target backend dialects a Compile call may emit for.
const (
	DialectPostgres = "postgres"
	DialectSQLite   = "sqlite"
)

// sqlFlavor / celFlavor are the v1 expression flavors (FR-A5). `field` is not an
// expression flavor — it is the field-reference oneof handled by the resolver.
const (
	sqlFlavor = "sql"
	celFlavor = "cel"
)

// Compiled is the result of compiling a resolved search config: the vector
// arguments for both backends plus the metadata a generator needs.
type Compiled struct {
	// PostgresVector is the argument passed to to_tsvector('<TextConfig>', …):
	// every source fragment concatenated with " || ' ' || ". For an INDEXED
	// resource it is also the expression of the persisted generated column
	// (BuildIndexedMigration).
	PostgresVector string
	// SQLiteVector is the parallel SQLite text concatenation used by the
	// case-insensitive LIKE fallback. It is empty when PostgresOnly is true.
	SQLiteVector string
	// PostgresOnly reports that at least one source is a raw sql/postgres
	// expression (no SQLite form): the resource cannot generate a SQLite
	// full-text predicate and degrades to Postgres only (SD-4).
	PostgresOnly bool
	// TextConfig is the resolved message-level Postgres text-search config
	// (default "simple").
	TextConfig string
	// Strategy is the resolved materialization strategy (JIT default, or
	// INDEXED). PROJECTED never reaches here — Compile fails loud on it (FR-A5).
	// It selects the generated List predicate: JIT recomputes to_tsvector(…) per
	// query; INDEXED matches the persisted search_vector column (SD-7, FR-C3).
	Strategy aip.SearchStrategy
	// SourceNames are the searchable source JSON names, in vector order — a
	// field source's JSON name, an expression source's logical name — for the
	// x-aip-search OpenAPI extension (FR-D1).
	SourceNames []string
}

// IsIndexed reports whether the resource uses the persisted-column (INDEXED)
// strategy, so a generator emits the search_vector migration + a persisted-column
// List predicate rather than the JIT inline to_tsvector(…).
func (c *Compiled) IsIndexed() bool { return c != nil && c.Strategy == aip.SearchIndexed }

// Compile validates and compiles cfg (from aip.ResolveSearchConfig over md) for
// the given target backend dialect. It returns (nil, nil) for a non-searchable
// resource, so a generator can call it unconditionally for every message and get
// a no-op.
//
// It fails loud — naming the message and the offending field/source — on:
//   - a searchable field that is secret or INPUT_ONLY (write-only/redacted: a
//     search match would leak it) or of a non-textual type (FR-A2/A3, FM-1);
//   - strategy PROJECTED, an unknown flavor, or an unsupported sql dialect —
//     "declared but not built in this feature" (FR-A5, FM-7); and
//   - when dialect is DialectSQLite, a resource carrying a sql/postgres source,
//     which is Postgres-only and cannot emit a SQLite predicate (FR-B5, FM-8).
//
// When dialect is DialectPostgres it never fails on portability; both vectors are
// still computed and PostgresOnly is set, so a runtime-branching generator (GORM)
// can decide whether to emit an SQLite branch.
func Compile(cfg aip.SearchConfig, md protoreflect.MessageDescriptor, dialect string) (*Compiled, error) {
	switch dialect {
	case DialectPostgres, DialectSQLite:
	default:
		return nil, fmt.Errorf("searchgen: unsupported target dialect %q (want %q or %q)",
			dialect, DialectPostgres, DialectSQLite)
	}

	msgName := "<message>"
	if md != nil {
		msgName = string(md.FullName())
	}

	// Reserved strategy: declared but not built here (FR-A5, FM-7, AC-9). Checked
	// before the searchable short-circuit so a bare PROJECTED declaration still
	// fails loud rather than being silently treated as "not searchable".
	if cfg.Strategy == aip.SearchProjected {
		return nil, fmt.Errorf("searchgen: %s declares strategy PROJECTED, which is declared but not built in this feature (reserved for cross-service/global search, WS-042)", msgName)
	}

	if !cfg.IsSearchable() {
		return nil, nil // not a searchable resource — nothing to emit
	}

	out := &Compiled{TextConfig: cfg.TextConfig, Strategy: cfg.Strategy}
	if out.TextConfig == "" {
		out.TextConfig = aip.DefaultTextConfig
	}

	var pgFrags, ltFrags []string
	for _, src := range cfg.Sources {
		f, err := compileSource(src, md, msgName)
		if err != nil {
			return nil, err
		}
		pgFrags = append(pgFrags, f.postgres)
		out.SourceNames = append(out.SourceNames, f.jsonName)
		if f.postgresOnly {
			out.PostgresOnly = true
		} else {
			ltFrags = append(ltFrags, f.sqlite)
		}
	}

	out.PostgresVector = strings.Join(pgFrags, " || ' ' || ")
	if out.PostgresOnly {
		// A single non-portable source makes the whole resource Postgres-only.
		out.SQLiteVector = ""
		if dialect == DialectSQLite {
			return nil, fmt.Errorf("searchgen: %s has a sql/postgres search source and is Postgres-only; it cannot generate a SQLite full-text predicate (SD-4)", msgName)
		}
	} else {
		out.SQLiteVector = strings.Join(ltFrags, " || ' ' || ")
	}
	return out, nil
}

// fragment is one source compiled to its per-dialect text expressions.
type fragment struct {
	postgres     string // to_tsvector argument fragment (always set)
	sqlite       string // SQLite concat fragment (unset when postgresOnly)
	postgresOnly bool   // true => no SQLite form (a raw sql/postgres source)
	jsonName     string // x-aip-search name
}

func compileSource(src aip.SearchSource, md protoreflect.MessageDescriptor, msgName string) (fragment, error) {
	if src.IsField() {
		return compileFieldSource(src, msgName)
	}
	return compileExprSource(src, md, msgName)
}

// compileFieldSource normalizes a field-flagged column: Postgres coalesces nulls
// and splits emails/domains (atlas-style '@'/'.' -> space) so the tokenizer
// indexes their parts; SQLite keeps the coalesced column text for a LIKE
// contains (SD-3/SD-4). It rejects a leaky or non-textual field (FR-A2/A3).
func compileFieldSource(src aip.SearchSource, msgName string) (fragment, error) {
	fd := src.Field
	if err := validateSearchableField(fd, msgName); err != nil {
		return fragment{}, err
	}
	col := columnName(fd)
	return fragment{
		postgres: fmt.Sprintf("replace(replace(coalesce(CAST(%s AS text), ''), '@', ' '), '.', ' ')", quoteIdent(col)),
		sqlite:   fmt.Sprintf("coalesce(CAST(%s AS text), '')", quoteIdent(col)),
		jsonName: fd.JSONName(),
	}, nil
}

// compileExprSource compiles a calculated source's flavored expressions. It
// selects, for Postgres, an author-written sql/postgres expr if present else the
// cel-compiled expr; the SQLite form comes only from cel (a raw sql/postgres expr
// has no portable form, marking the source Postgres-only, SD-4/SD-9).
func compileExprSource(src aip.SearchSource, md protoreflect.MessageDescriptor, msgName string) (fragment, error) {
	if len(src.Exprs) == 0 {
		return fragment{}, fmt.Errorf("searchgen: %s search source %q has no expressions", msgName, src.Name)
	}
	var (
		pgSQL           string
		pgCEL, ltCEL    string
		haveSQL, haveCEL bool
	)
	for _, e := range src.Exprs {
		switch e.Flavor {
		case sqlFlavor:
			if e.Dialect != DialectPostgres {
				return fragment{}, fmt.Errorf("searchgen: %s search source %q declares sql dialect %q, which is declared but not built in this feature (only %q is supported)", msgName, src.Name, e.Dialect, DialectPostgres)
			}
			if err := validateSQLExpr(e.Expr, src.Name, msgName); err != nil {
				return fragment{}, err
			}
			pgSQL = "(" + strings.TrimSpace(e.Expr) + ")"
			haveSQL = true
		case celFlavor:
			pg, lt, err := celsql.CompileCEL(e.Expr, md)
			if err != nil {
				return fragment{}, fmt.Errorf("searchgen: %s search source %q: %w", msgName, src.Name, err)
			}
			pgCEL, ltCEL = pg, lt
			haveCEL = true
		default:
			return fragment{}, fmt.Errorf("searchgen: %s search source %q declares unknown flavor %q, which is declared but not built in this feature (only %q and %q are supported)", msgName, src.Name, e.Flavor, sqlFlavor, celFlavor)
		}
	}

	f := fragment{jsonName: src.Name}
	switch {
	case haveSQL:
		f.postgres = pgSQL // the author's explicit Postgres expression wins
		if haveCEL {
			f.sqlite = ltCEL // a cel alternate keeps the source portable
		} else {
			f.postgresOnly = true
		}
	case haveCEL:
		f.postgres = pgCEL
		f.sqlite = ltCEL
	default:
		return fragment{}, fmt.Errorf("searchgen: %s search source %q has no usable expression", msgName, src.Name)
	}
	return f, nil
}

// validateSearchableField rejects a field that FTS cannot safely cover: a secret
// or INPUT_ONLY (write-only/redacted) field — searching it leaks the value via
// match behavior — or a non-textual type (FR-A2/A3, FM-1).
func validateSearchableField(fd protoreflect.FieldDescriptor, msgName string) error {
	if fieldIsSecret(fd) {
		return fmt.Errorf("searchgen: %s.%s is marked searchable but is secret (write-only/redacted); searching it would leak the value via match behavior", msgName, fd.Name())
	}
	bs, err := aip.ResolveFieldBehavior(fd)
	if err != nil {
		return err
	}
	if aip.HasBehavior(bs, aip.InputOnly) {
		return fmt.Errorf("searchgen: %s.%s is marked searchable but is INPUT_ONLY (write-only); searching it would leak the value via match behavior", msgName, fd.Name())
	}
	if !isTextualField(fd) {
		return fmt.Errorf("searchgen: %s.%s is marked searchable but its type (%s) is not full-text searchable (only string, enum, repeated string, map<string,string> tags, or timestamp)", msgName, fd.Name(), fieldTypeName(fd))
	}
	return nil
}

// isTextualField reports whether a field's type can contribute text to a search
// vector: string (incl. repeated string), enum, map<string,string> tags, or a
// google.protobuf.Timestamp (rendered to text).
func isTextualField(fd protoreflect.FieldDescriptor) bool {
	if fd.IsMap() {
		return fd.MapKey().Kind() == protoreflect.StringKind &&
			fd.MapValue().Kind() == protoreflect.StringKind
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return true // singular or repeated string
	case protoreflect.EnumKind:
		return true
	case protoreflect.MessageKind:
		return fd.Message() != nil && fd.Message().FullName() == "google.protobuf.Timestamp"
	default:
		return false
	}
}

// fieldTypeName renders a field's type for diagnostics.
func fieldTypeName(fd protoreflect.FieldDescriptor) string {
	if fd.IsMap() {
		return "map"
	}
	name := fd.Kind().String()
	if fd.IsList() {
		return "repeated " + name
	}
	if fd.Kind() == protoreflect.MessageKind && fd.Message() != nil {
		return string(fd.Message().FullName())
	}
	return name
}

// columnName resolves the DB column for a field: the (infoblox.field.v1.opts)
// column_name override, else the field's snake_case name (proto field names are
// already snake_case by convention).
func columnName(fd protoreflect.FieldDescriptor) string {
	if fo := fieldOptsOf(fd); fo != nil && fo.GetColumnName() != "" {
		return fo.GetColumnName()
	}
	return strings.ToLower(string(fd.Name()))
}

func fieldIsSecret(fd protoreflect.FieldDescriptor) bool {
	fo := fieldOptsOf(fd)
	return fo != nil && fo.GetSecret()
}

func fieldOptsOf(fd protoreflect.FieldDescriptor) *fieldv1.FieldOptions {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, fieldv1.E_Opts) {
		return nil
	}
	fo, _ := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions)
	return fo
}

// volatileTokens are non-immutable/volatile Postgres constructs a search source
// must not use: a generated column / query-time to_tsvector argument must be
// deterministic and single-row (best-effort static check, RQ-2; Postgres also
// rejects a non-immutable generated-column expr at apply time).
var volatileTokens = []string{
	"now(", "random(", "current_timestamp", "current_date", "current_time",
	"clock_timestamp(", "statement_timestamp(", "timeofday(", "nextval(",
	"currval(", "gen_random_uuid(", "uuid_generate", "current_user", "session_user",
}

// crossTableRe matches SQL keywords that imply a subquery / another table — a
// search source must read only the owner row (Q4 single-table, SD-4).
var crossTableRe = regexp.MustCompile(`(?i)\b(select|from|join)\b`)

// validateSQLExpr is the best-effort sql/postgres validator (SD-9, RQ-2): it
// rejects an obviously non-immutable function or a cross-table reference, failing
// loud and naming the source. It is intentionally conservative, not a SQL parser;
// Postgres is the authoritative backstop at migration/query time.
func validateSQLExpr(expr, srcName, msgName string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("searchgen: %s search source %q has an empty sql expression", msgName, srcName)
	}
	lower := strings.ToLower(expr)
	for _, tok := range volatileTokens {
		if strings.Contains(lower, tok) {
			return fmt.Errorf("searchgen: %s search source %q sql expression uses the non-immutable/volatile construct %q; a search source must be immutable and single-row", msgName, srcName, strings.TrimSuffix(tok, "("))
		}
	}
	if crossTableRe.MatchString(expr) {
		return fmt.Errorf("searchgen: %s search source %q sql expression appears to reference another table (SELECT/FROM/JOIN); only single-row expressions over the owner table are supported", msgName, srcName)
	}
	return nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
