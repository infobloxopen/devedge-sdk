package aip

import (
	"fmt"

	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// This file is the shared, generator-agnostic resolver for a resource's
// full-text search surface (WS-041, FR-A1). Like the rest of internal/aip it is
// imported at RUNTIME (middleware/redact) as well as by every code generator, so
// it MUST stay free of the cel-go / SQL compiler: it returns the declared search
// surface RAW — field references keep their descriptor, flavored expressions keep
// their untouched text — and never compiles an expression or resolves a DB
// column. Compilation of the raw sources into to_tsvector arguments lives in the
// codegen-only cmd/internal/searchgen (which may import cel-go); resolving it in
// one place is what keeps a service's compiled predicate and its published
// OpenAPI from drifting (FR-A1 / FM-5).

// SearchStrategy is the resolved materialization strategy for a resource's
// full-text search vector (SD-7, D2).
type SearchStrategy int

const (
	// SearchJIT computes to_tsvector(...) at query time (the default; also the
	// resolution of STRATEGY_UNSPECIFIED).
	SearchJIT SearchStrategy = iota
	// SearchIndexed persists a generated tsvector column + GIN index.
	SearchIndexed
	// SearchProjected is RESERVED (cross-service/global search, WS-042): it is
	// declared but not built in this feature and MUST fail loud at codegen — never
	// silently emitted as a local predicate (FR-A5, FM-7).
	SearchProjected
)

// String renders the strategy for diagnostics.
func (s SearchStrategy) String() string {
	switch s {
	case SearchIndexed:
		return "INDEXED"
	case SearchProjected:
		return "PROJECTED"
	default:
		return "JIT"
	}
}

// DefaultTextConfig is the Postgres text-search configuration applied when a
// resource declares none (SD-5, matches the atlas `_fts` default).
const DefaultTextConfig = "simple"

// SearchExpr is one flavored, versioned expression contributing to a calculated
// search source (SD-9). The resolver returns it RAW; compilation and per-flavor
// validation happen in cmd/internal/searchgen.
type SearchExpr struct {
	Flavor  string // expression language: "sql" | "cel"
	Dialect string // target backend when language-specific (sql: "postgres")
	Version string // flavor spec version (pins meaning)
	Expr    string // the expression text
}

// SearchSource is one contributor to a resource's search vector: either a plain
// field reference (Field non-nil) or a set of flavored expressions (Exprs
// non-empty). Exactly one of the two is populated for a well-formed source.
type SearchSource struct {
	// Name is a logical/diagnostic name; it is also the x-aip-search name for an
	// expression source (a field source uses its JSON name instead).
	Name string
	// Field is the resolved descriptor of a plain field source (implicit
	// searchable=true column or an explicit message-level `field:` reference).
	// nil for an expression source.
	Field protoreflect.FieldDescriptor
	// Exprs are the raw flavored expressions of a calculated source. Empty for a
	// field source.
	Exprs []SearchExpr
	// TextConfig optionally overrides the message-level text_config for this
	// source ("" inherits).
	TextConfig string
}

// IsField reports whether the source is a plain field reference.
func (s SearchSource) IsField() bool { return s.Field != nil }

// SearchConfig is the resolved full-text search surface of a resource message:
// the ordered searchable sources (implicit field-flagged columns in field order,
// then the message-level calculated sources), plus the strategy and the Postgres
// text-search config.
type SearchConfig struct {
	Strategy   SearchStrategy
	TextConfig string
	Sources    []SearchSource
}

// IsSearchable reports whether the message declares a searchable surface. A
// PROJECTED resource counts as searchable even with no local sources so the
// reserved-value fail-loud (FR-A5) stays reachable by callers that gate on this.
func (c SearchConfig) IsSearchable() bool {
	return len(c.Sources) > 0 || c.Strategy == SearchProjected
}

// ResolveSearchConfig resolves the full-text search surface declared on md: the
// ordered searchable sources (field-flagged columns in field order, then the
// message-level SearchConfig.sources), the strategy (STRATEGY_UNSPECIFIED => JIT),
// and the text-search config (default "simple"). It returns an EMPTY config (not
// an error) for a message that declares no search surface (FR-A1).
//
// The sources are returned RAW — a field source carries its resolved
// FieldDescriptor; an expression source carries its untouched flavored exprs. No
// expression is compiled and no DB column is resolved here (that is
// cmd/internal/searchgen's job), keeping this package cel-go-free for its runtime
// importers.
//
// It fails loud only on a structural resolution failure: a message-level source
// whose `field:` names a field absent from md, or a source declaring neither a
// field nor any expressions. Semantic validation (leaky/non-textual fields,
// reserved strategies, unknown flavors) is the compiler's responsibility.
func ResolveSearchConfig(md protoreflect.MessageDescriptor) (SearchConfig, error) {
	cfg := SearchConfig{Strategy: SearchJIT, TextConfig: DefaultTextConfig}
	if md == nil {
		return cfg, nil
	}

	// Implicit sources: fields flagged searchable=true, in field order.
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fo := fieldOpts(fd); fo != nil && fo.GetSearchable() {
			cfg.Sources = append(cfg.Sources, SearchSource{
				Name:  string(fd.Name()),
				Field: fd,
			})
		}
	}

	// Message-level SearchConfig: strategy, text_config, and calculated sources.
	sc := searchExtension(md)
	if sc == nil {
		return cfg, nil
	}
	cfg.Strategy = strategyFromProto(sc.GetStrategy())
	if tc := sc.GetTextConfig(); tc != "" {
		cfg.TextConfig = tc
	}
	for _, src := range sc.GetSources() {
		switch src.GetFrom().(type) {
		case *storagev1.SearchSource_Field:
			name := src.GetField()
			fd := fields.ByName(protoreflect.Name(name))
			if fd == nil {
				return cfg, fmt.Errorf("aip: search source %q on %s references unknown field %q",
					src.GetName(), md.FullName(), name)
			}
			logical := src.GetName()
			if logical == "" {
				logical = name
			}
			cfg.Sources = append(cfg.Sources, SearchSource{
				Name:       logical,
				Field:      fd,
				TextConfig: src.GetTextConfig(),
			})
		case *storagev1.SearchSource_Exprs:
			var exprs []SearchExpr
			for _, e := range src.GetExprs().GetExpr() {
				exprs = append(exprs, SearchExpr{
					Flavor:  e.GetFlavor(),
					Dialect: e.GetDialect(),
					Version: e.GetVersion(),
					Expr:    e.GetExpr(),
				})
			}
			cfg.Sources = append(cfg.Sources, SearchSource{
				Name:       src.GetName(),
				Exprs:      exprs,
				TextConfig: src.GetTextConfig(),
			})
		default:
			return cfg, fmt.Errorf("aip: search source %q on %s declares neither a field nor expressions",
				src.GetName(), md.FullName())
		}
	}
	return cfg, nil
}

// searchExtension returns the (infoblox.storage.v1.search) message option on md,
// or nil when absent.
func searchExtension(md protoreflect.MessageDescriptor) *storagev1.SearchConfig {
	opts := md.Options()
	if opts == nil || !proto.HasExtension(opts, storagev1.E_Search) {
		return nil
	}
	sc, _ := proto.GetExtension(opts, storagev1.E_Search).(*storagev1.SearchConfig)
	return sc
}

// strategyFromProto maps the canonical enum to the resolved strategy, treating
// STRATEGY_UNSPECIFIED as the JIT default.
func strategyFromProto(s storagev1.SearchConfig_Strategy) SearchStrategy {
	switch s {
	case storagev1.SearchConfig_STRATEGY_INDEXED:
		return SearchIndexed
	case storagev1.SearchConfig_STRATEGY_PROJECTED:
		return SearchProjected
	default: // STRATEGY_UNSPECIFIED and STRATEGY_JIT
		return SearchJIT
	}
}
