# Implementation Plan: 046-fulltext-search (WS-041 — the P0 foundation for WS-042)

**Branch**: `feature/046-fulltext-search` · **Spec**: `spec.md` (clarified; CEL in v1) · **Model
routing**: `[S]`→Sonnet subagents, `[C]`→Opus. See `tasks.md` for the tagged task list.

## Approach

Declare the searchable surface on the schema; resolve it once in `internal/aip`; generate the `q`
predicate for both backends behind `persistence.Repository`. Postgres is the FTS engine; SQLite gets
a portable `LIKE` fallback. Strategy (`JIT`/`INDEXED`, plus reserved `PROJECTED`) and text-search
config live on a message-level annotation. Three expression flavors — `field`, `sql/postgres`,
`cel` — all funnel through one compiled SQL form (SD-7/SD-9). Everything is regenerable and
diff-clean; end-to-end proof runs over the real gateway (SQLite fast path + Postgres `pgtest`).

## Annotation design (the apis change — WP-0, blocking)

Two additions to the **canonical** `github.com/infobloxopen/apis`, then mirror-synced here
(`make sync-scaffold-mirrors`, byte-identical, `mirror_drift_test.go`):

```proto
// infoblox/field/v1/field.proto — inside message FieldOptions (beside not_null/unique/index)
bool searchable = <next free>;   // include this column in the resource's full-text search vector

// infoblox/storage/v1/storage.proto — new message-level option (model is 50050)
message SearchConfig {
  enum Strategy { STRATEGY_UNSPECIFIED = 0; JIT = 1; INDEXED = 2; PROJECTED = 3; }
  Strategy strategy    = 1;             // default JIT
  string   text_config = 2;             // Postgres TS config; default "simple"
  repeated SearchSource sources = 3;    // calculated/transformed sources beyond field-flagged columns
}
message SearchSource {
  string name = 1;                      // logical name (diagnostics / x-aip-search)
  oneof from {                          // exactly one:
    string field = 2;                   //  a field reference (portable), OR
    SearchExprSet exprs = 3;            //  flavored expressions
  }
  string text_config = 4;               // optional per-source override
}
message SearchExprSet { repeated SearchExpr expr = 1; }
message SearchExpr {
  string flavor  = 1;   // "sql" | "cel"
  string dialect = 2;   // sql only: "postgres"
  string version = 3;   // flavor spec version (pins meaning)
  string expr    = 4;
}
extend google.protobuf.MessageOptions { SearchConfig search = 50051; }  // next after model=50050
```

Field-flagged columns (`searchable=true`) are implicit sources; `SearchConfig.sources` adds
calculated ones. Release = a new `infoblox.field.v1` + `infoblox.storage.v1` alpha in both apis repos
via the apx canonical pipeline (see memory `apx-canonical-api-schemas` for the gotchas), authorized
for public release this session.

## The shared resolver + flavor compilers (`internal/aip`)

- `SearchConfig(md) (SearchConfig, error)` — the single resolver (FR-A1); unions field-flagged
  columns + message `sources`; carries strategy + text_config. Consumed by both generators + the
  OpenAPI pass (no drift, FR-A1/FM-5).
- Per-flavor validate+compile behind one interface `compileSource(src, md, dialect) (sqlExpr,
  sqliteExpr string, err error)`:
  - `field` — column ref; auto-normalize (`coalesce`, `@`/`.`→space); SQLite = the column.
  - `sql/postgres` — drop the expr in verbatim (best-effort immutability/single-table check, RQ-2);
    **no SQLite form → resource is Postgres-only** (SD-4).
  - `cel` — **CEL→SQL compiler** (FR-A6): type-check against `md`, then compile the supported subset
    (field refs; `?:`/map-literal→`CASE`; string ops `+`/`.replace`/`.lowerAscii`; message/`map`/
    `tags` access→Postgres path). Emits BOTH a Postgres expr and a SQLite-fallback expr from one
    AST walk, so `cel` sources stay portable. Fail loud on any unsupported construct.
- `field_behavior` guard: reject `secret`/`INPUT_ONLY` + non-textual searchable fields (FR-A2/A3).
- Reserved: `PROJECTED` / unknown flavor / unsupported dialect → validate-then-fail-loud (FR-A5).

## Predicate generation (both backends, SD-6)

- `persistence.ListOptions` gains `Search string`; `protoc-gen-svc` detects `string q` on the List
  request and maps `req.GetQ()` → `ListOptions.Search` in `stdList` (`render.go:501-534`).
- **GORM** (`protoc-gen-storage/render.go`): after the `filter.Parse` WHERE
  (`widgets.storage.go:167-172`), AND `to_tsvector('<cfg>', <compiled sources>) @@
  websearch_to_tsquery('<cfg>', ?)` (Postgres) or the `LIKE` OR-contains (SQLite); term bound.
- **ent** (`protoc-gen-ent/render.go:1513-1518`): AND a raw `sql.P(...)` full-text predicate with the
  `toEntPredicate` result, same compiled sources.
- `INDEXED`: emit a Postgres migration — a `search_vector … GENERATED ALWAYS AS (to_tsvector(...))
  STORED` column + `CREATE INDEX CONCURRENTLY … USING GIN` (sole statement, own file); generated List
  matches the persisted column. SQLite keeps the `LIKE` fallback.

## OpenAPI (`cmd/openapiv2to3/enrich.go`)

Add a `q` query param + `x-aip-search` (sources, strategy, text_config) to searchable List ops,
parallel to `paginationExt`, gated on `aip.MethodList` (`enrich.go:121-123`). Regenerate the toy
golden.

## Fixtures & verification

`testdata/toy`: add `string q` to `ListWidgetsRequest` + a `searchable` `display_name` + one message
`sql/postgres` source (DDI `CASE` shape) + one `cel` source; regen golden. Add `q` to an ent fixture
(`apikey`/`iam`). Prove e2e over the gateway: SQLite fast test + Postgres `pgtest` (both strategies,
GIN via `EXPLAIN`, injection-safety AC-3). Regression tests for every FM.

## Work packages (→ tasks.md)

WP-0 apis annotations + release + mirror `[C]/[S]` (blocking) · WP-1 `internal/aip` resolver +
field/sql validators `[C]` · WP-2 CEL→SQL compiler `[C]` · WP-3 `ListOptions.Search` + svc `q`
mapping `[S]` · WP-4 storage (GORM) emission + INDEXED migration `[C]` · WP-5 ent emission `[C]` ·
WP-6 OpenAPI `x-aip-search` `[S]` · WP-7 fixtures + goldens + pgtest e2e `[C]` · WP-8
PROJECTED/reserved fail-loud `[S]` · WP-9 2× hardening loops + regression tests · WP-10 docs `[S]` ·
WP-11 release (apis + devedge-sdk PR + admin-merge).

## Gates

`go build ./... && make test` green; `make generate` idempotent (diff-clean); mirror-drift +
graph-isolation green; e2e over the real gateway on both dialects; scope-diff vs. spec ACs; the
security hardening loop finds no cross-tenant/injection leak.
