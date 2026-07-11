# Feature Specification: Full-text search — a declared, generated `q` collection operator with a pluggable Postgres FTS strategy

**Feature Branch**: `046-fulltext-search`
**Created**: 2026-07-10
**Status**: Draft (clarified)
**Initiative**: WS-041 (annotation-driven full-text search as a devedge-sdk AIP) — **P0**
**Siblings**: WS-042 (global/cross-service search — the `PROJECTED` materialization; reserved here),
WS-039 (outbox→projection ingestion shape both reuse).

## Context

A large class of user-facing List surfaces needs "type some words in a box, get matching
records" — free-text search across a handful of fields. That is not what a structured `filter`
(AIP-160) is for, and devedge-sdk has **no full-text search seam at all today**: an exhaustive
`grep -rniE "tsvector|tsquery|websearch|searchable|_fts|fulltext"` over `**/*.go` + `**/*.proto`
returns zero hits outside one prose mention in a skill doc. This feature adds that seam.

The proven Infoblox pattern for this is the `infobloxopen/atlas-app-toolkit` `q`/`_fts` collection
operator: `infoblox.api.Searching{ string query = 1 }` is added to a List request, the gateway
lifts the `_fts` query param into metadata, and a GORM converter builds a **query-time** predicate
`to_tsvector('simple', <fields concatenated>) @@ to_tsquery('simple', ?)` with **no persisted
column and no index**. A production consumer of exactly this (the DDI/Northstar "RFE-10869 Searching
Within a Dataset" HLD) found the query-time approach **too slow at scale (5–30 s/query)** and
migrated to a **persisted `tsvector` column + GIN index**; it also needed **calculated search
sources** — an enum pair `type`/`primary_type` mapped to a display string (`map_zone_type`), a value
pulled from an `external_providers` JSON blob, `sourceBitToStringMap` bit→string mapping, and tag
keys/values — plus prefix matching, quoted exact-match, per-view searchable field sets, and had to
**fork** the toolkit to get there. WS-041's assessment (hub `work/WS-041-brief.md`) draws two
load-bearing lessons: (1) query-time FTS alone does not scale — an index-backed mode is mandatory;
(2) doing it as a hand-wired fork is the anti-pattern — **declare** the searchable surface and
**generate** the wiring, the way devedge-sdk already treats every other API-contract fact.

This feature is the devedge-native, AIP-style successor to `_fts`: the searchable surface is
**declared once** on the schema (plain fields flagged where they live; transforms/calculated values
declared at the message level), the `q` operator and its Postgres predicate are **generated** across
both persistence backends behind the existing `persistence.Repository` seam, and the strategy
(query-time vs index-backed) is **selectable per surface**. The capability flows into the apx
contract + enriched OpenAPI so it is discoverable.

**The seams this feature touches (ground truth, this repo):**

- **List has no search field.** `persistence.ListOptions` carries only `Filter, OrderBy, PageSize,
  PageToken, ShowDeleted` (`persistence/repository.go:53-61`); `Repository[T,K].List` at
  `persistence/repository.go:66-76`. No `Search`/`Q`.
- **List-request fields are conventional scalars, detected by name.** `protoc-gen-svc` sets
  `ListHasFilter = hasStringField(m.Input,"filter")` / `order_by` / `show_deleted`
  (`cmd/protoc-gen-svc/main.go:122-124`) and maps them onto `ListOptions` in the `stdList` handler
  (`cmd/protoc-gen-svc/render.go:501-534`, `req.GetFilter()` at `:524`). There is **no nested
  `Searching` message idiom** here — `filter`/`order_by` are plain request fields. `q` follows suit.
- **Per-field annotations resolve through one shared package.** `internal/aip` reads
  `infoblox.field.v1` field options once (`internal/aip/behavior.go:59-101,185-192`) and is imported
  by all three generators, the OpenAPI enrichment pass, and `middleware/redact`. Feature 044
  established the **no-drift principle**: field/method facts are resolved in `internal/aip` and never
  reimplemented per generator.
- **The WHERE clause is built in generated code, per backend.** GORM: generated `.storage.go` List
  does `filter.Parse(...).SQL()` → `q.Where(sql, args...)` (`testdata/toy/widgetsv1/widgets.storage.go:167-172`;
  emitter `cmd/protoc-gen-storage/render.go:1159`). ent: `entrepo.FilterPredicate` →
  `toEntPredicate` type-switch over the exported filter AST (`persistence/entrepo/filter.go:28-107`),
  applied at `cmd/protoc-gen-ent/render.go:1513-1518`. The AIP-160 parser exposes a `Condition`
  interface `SQL() (string, []interface{})` (`persistence/filter/filter.go:17-22`) and is
  dialect-aware (`WithDialect` postgres/sqlite, `filter.go:42-73`).
- **Postgres is the schema-of-record; SQLite is the test/dev driver.** `persistence/migrate`
  registers only pgx/v5 (`persistence/migrate/drivers.go:19-25`) and the baseline DDL is Postgres
  (`persistence/migrate/baseline/0001_framework_init.up.sql`); SQLite (`modernc.org/sqlite`) backs
  the fast unit tests (`persistence/gormtx/sqlite_test.go`) and its AutoMigrate **ignores** the SQL
  migration files. SQLite has no `to_tsvector`. This is the central portability constraint.
- **The annotation namespace is canonical + mirrored.** `infoblox.field.v1` and `infoblox.storage.v1`
  live in `github.com/infobloxopen/apis`; this repo carries byte-identical **mirrors**
  (`proto/infoblox/field/v1/field.proto:1-4`, `FieldOptions` at `:17-78`, extension `opts = 50003` at
  `:138-142`; storage `model` message option at `proto/infoblox/storage/v1/storage.proto:15-23`) and
  scaffold mirrors kept in sync by `make sync-scaffold-mirrors` (`Makefile:60-64`) under drift test
  `cmd/devedge-sdk/internal/scaffold/mirror_drift_test.go`. Existing field storage flags
  `not_null/unique/index/column_name/column_type` sit at `field.proto:25-29`.
- **The change feed already exists** (for the reserved `PROJECTED` value): `events.ChangeEvent`
  (`devedge.change.v1`) + the `ChangeEmitting` decorator over any `Repository` (same-tx emit), the
  transactional outbox (`persistence/outbox.go`), and the relay → `events.Bus` → `kafkabus`. WS-041
  does not consume it; WS-042 does.
- **Enriched OpenAPI has the exact insertion point.** `cmd/openapiv2to3/enrich.go` emits
  `x-aip-pagination` (`enrich.go:121-123`) built by `paginationExt(md)` (`enrich.go:273-292`), gated
  on `aip.ClassifyMethod == aip.MethodList`. Golden: `testdata/toy/openapi/toy.openapi.yaml`.
- **Canonical end-to-end fixture = `testdata/toy`.** `ListWidgetsRequest{ page_size; page_token;
  show_deleted }` (`testdata/toy/widgets.proto:65-70`) has no `filter`/`order_by`/`q`; Widget fields
  already carry `(infoblox.field.v1.opts)` (`:30-44`). It is the only fixture wired through OpenAPI
  enrichment (`buf.gen.toy.yaml`, `Makefile:36-39`).

**Non-goals of this feature** (deferred / later phases): global/cross-service search (WS-042, the
`PROJECTED` materialization), external search engines (Elasticsearch/OpenSearch), fuzzy/typo
tolerance beyond Postgres FTS, relevance-tuning UIs and `ts_rank` ordering, and analytics. This
feature does not change authz (propagate principal, decide nothing) and does not touch the DDD write
boundary.

## Clarifications (session 2026-07-10)

Resolved with the user during the Analyze phase; each updates a decision below.

- **Q1 (annotation shape) → hybrid.** Plain columns are flagged field-locally with a `searchable`
  bool; transforms/**calculated** values live in a message-level `search { sources: [...] }` list,
  because a calculated value has no single field to annotate (SD-3).
- **Q2 (strategy home + "per surface") → message-level, three-value axis.** Strategy + text-search
  config live in the message-level `search {}` annotation; the strategy axis is
  `JIT | INDEXED | PROJECTED`, default `JIT`, overridable per storage surface. `PROJECTED` is
  **reserved now, not built here** (D2, SD-7).
- **Expression flavors → tagged + versioned; v1 = `field` + `sql/postgres`, `cel` reserved.** A
  non-field source carries `{ flavor, dialect, version, expr }` with a per-flavor validator. v1 ships
  the portable `field` source and a Postgres-locked raw-`sql` escape hatch — enough for the DDI
  `map_zone_type` (`CASE`) and `external_providers ->> 'name'` (JSON) transforms today; `cel` is a
  reserved flavor that compiles to SQL in a fast-follow (SD-9, RQ-1).
- **CEL / local materialization → compile-to-SQL generated column** (not write-time eval). The local
  search vector is always DB-computed — a generated column (`INDEXED`) or inline `to_tsvector`
  (`JIT`); write-time evaluation in Go is deferred (SD-7).
- **Q3 (SQLite) → per-flavor portability.** `field` sources keep the SQLite `LIKE` fallback; a
  resource whose search text needs a `sql/postgres` source is Postgres-only and fails loud at codegen
  for the SQLite backend (SD-4).
- **Q4 (vector scope) → single-table only in v1.** Owner-table scalar fields (`field`) plus
  single-row transforms authored as a `sql/postgres` expr (which may read the row's own JSON/`tags`
  column). Joined/related-table values are deferred — they cannot be a generated column and need
  triggers or the `PROJECTED` path.
- **Q5 (ranking) → deferred.** `q` is a pure filter; order stays with `order_by`. No `ts_rank`.
- **Q6 (query modes) → `websearch_to_tsquery` as-is** (quoted phrases native); explicit prefix (`:*`)
  and quoted-exact→non-FTS fallback deferred.

## Ratified design decisions

D1/D2 are ratified by the user; SD-* are decisions from the ground-truth survey + the clarifications
above. Settled inputs unless listed under Open Questions.

- **D1 — Home = devedge-sdk, annotation-driven.** Net-new seam, declared on the schema and generated.
  Not a contribution to atlas-app-toolkit; no fork.
- **D2 — v1 supports BOTH local strategies; the axis reserves a third.** Strategy is declared in the
  message-level `search {}` config, one of `JIT | INDEXED | PROJECTED`, default `JIT`, overridable
  per storage surface (WS-005 "surface = projection"):
  - `JIT` — `to_tsvector(...) @@ …` computed per query; zero migration; dev/small tables.
  - `INDEXED` — a persisted generated `tsvector` column + GIN index; for scale.
  - `PROJECTED` — **reserved, not built in this feature.** Marks a resource whose searchable
    projection is materialized *remotely* (a cross-service/global index fed by the
    `events.ChangeEvent` outbox feed) — the hook for WS-042 (sibling of WS-039). In WS-041 a
    `PROJECTED` value MUST validate but MUST fail loud at codegen ("not yet supported"); it MUST
    never silently emit a local predicate.
- **SD-1 — `q` is a plain `string q` field on the List request → `ListOptions.Search`.** Detected by
  name like `filter`/`order_by` (`ListHasSearch = hasStringField(m.Input,"q")`, extending
  `cmd/protoc-gen-svc/main.go:122-124`) and mapped in the `stdList` handler (`render.go:501-534`). A
  `Search string` field is added to `persistence.ListOptions` (`persistence/repository.go:53-61`).
  REST spelling is `q`. An empty `q` is a no-op (matches the parser's empty-expression contract,
  `filter.go:84-102`).
- **SD-3 — The searchable surface is a hybrid: field-level flag + message-level sources.** Plain
  columns are flagged where they live — `bool searchable` on `infoblox.field.v1.FieldOptions` beside
  `index`/`not_null` (`field.proto:25-29`), auto-normalized by the generator (coalesce nulls,
  atlas-style `@`/`.`→space). Transforms and **calculated** values — which have no single field to
  annotate — live in a message-level `search { sources: [...], text_config, strategy }` annotation
  (home: `infoblox.storage.v1`, beside `model`, `storage.proto:15-23`). A `source` is either a field
  reference or a flavored expression (SD-9). Both additions require a new
  `infoblox.field.v1`/`infoblox.storage.v1` release in `github.com/infobloxopen/apis` + a
  byte-identical mirror sync here (`make sync-scaffold-mirrors`, `mirror_drift_test.go`) — the
  WS-031/033 cross-repo path.
- **SD-4 — Portability is per-source-flavor; Postgres is the FTS engine.** On Postgres the predicate
  is true FTS. A resource whose sources are all portable (`field`, and `cel` when it lands) degrades
  on SQLite (the unit-test/dev driver, no `to_tsvector`) to a case-insensitive `LIKE '%term%'`
  OR-across-fields contains, so fixtures stay testable without a Postgres container. A resource that
  needs a `sql/postgres` source is **Postgres-only**: the generator fails loud at codegen when asked
  to emit for the SQLite backend (the flavor system makes the gap explicit, not a silent runtime
  break). Dialect is already threaded through the generators (`cmd/protoc-gen-ent/main.go:37-49`) and
  the parser (`filter.go:64-73`).
- **SD-5 — Safe query parsing uses `websearch_to_tsquery`.** Not the raw `to_tsquery` atlas used
  (which needs sanitizing). `websearch_to_tsquery` accepts free-form text — words, `"quoted
  phrases"`, `or`, `-negation` — without throwing on syntax. Text-search config defaults to `simple`
  (no stemming; matches atlas) and is overridable per source/message via `text_config`.
- **SD-6 — The search predicate is ANDed alongside the existing filter, not routed through the
  AIP-160 grammar.** `q` is its own operator, composing with `filter`/`order_by`/pagination in one
  List. GORM: the generated List adds one more `q.Where("<vector> @@ websearch_to_tsquery(?, ?)",
  cfg, search)` after the `filter.Parse` WHERE (`widgets.storage.go:167-172`). ent: the generated
  List ANDs a raw `sql.P(func(*sql.Builder))` predicate with the `toEntPredicate` result
  (`entrepo/filter.go:45-82`, `render.go:1513-1518`). The user string is always a bound parameter.
- **SD-7 — Local materialization is compile-to-SQL; `INDEXED` = generated column + GIN migration.**
  The local search vector is always computed in the database — inline `to_tsvector(<expr>)` for
  `JIT`, or a `search_vector tsvector GENERATED ALWAYS AS (to_tsvector('<cfg>', <coalesced sources>))
  STORED` column for `INDEXED`, with a `CREATE INDEX CONCURRENTLY … USING GIN (search_vector)` as the
  **sole statement** in its own migration file (`migrations.README.md.tmpl:40-47`). `sql/postgres`
  sources are dropped into that expression; `cel` (when it lands, RQ-1) compiles to the same SQL.
  **Write-time evaluation in Go is deferred** — it would give full-CEL fidelity and unify with the
  WS-042 projection document, but changes the model to compute-on-write + backfill (out of scope).
  Generated columns (PG12+) avoid triggers; `INDEXED` is Postgres-only, exercised via
  `persistence/migrate/pgtest_test.go`.
- **SD-8 — OpenAPI carries `q` + an `x-aip-search` extension.** `cmd/openapiv2to3/enrich.go` adds a
  `q` query parameter and an `x-aip-search` extension (searchable field/source list, `strategy`,
  `text_config`) to List operations whose resource is searchable, parallel to `paginationExt` and
  gated on the same `aip.MethodList` branch (`enrich.go:121-123,273-292`). Regenerated golden proves
  it.
- **SD-9 — Transform expressions are tagged by flavor + version, validated per flavor.** A source
  that is not a plain field is `{ flavor, dialect, version, expr }`: `flavor` = the expression
  language (`field` | `sql` | `cel`); `dialect` = the target backend when language-specific (e.g.
  `sql`+`postgres`); `version` = the flavor's own spec version (pins meaning; the validator declares
  accepted versions). Each flavor supplies a validator: `field` checks the field exists + is a
  supported type; `sql/<dialect>` best-effort-parses and rejects obviously non-immutable functions +
  cross-table references; `cel` (reserved) compiles + type-checks against the message descriptor
  (strongest). A flavor declares which backend(s) it compiles to, so the backend×flavor matrix stays
  bounded and the flavor set stays **closed** (no open plugin surface) until there is demand. **v1
  ships `field` + `sql/postgres` + `cel`** — `field`/`sql` cover the DDI `map_zone_type` (`CASE`) and
  `external_providers ->> 'name'` (JSON) transforms directly; `cel` is the portable, strongly
  validated flavor — it compiles to SQL (SD-7), is type-checked against the message descriptor, and
  keeps calculated fields working on the SQLite fallback too. The CEL→SQL compiler for the supported
  subset is in v1 scope (FR-A6). (RQ-1 resolved 2026-07-10.)

## Requirements

### Part A — the searchable-surface contract

- **FR-A1**: `internal/aip` MUST expose a resolver (working name `SearchConfig(md) (SearchConfig,
  error)`) returning a message's ordered searchable **sources** — each a field reference (with DB
  column + type) or a flavored expression (`{flavor, dialect, version, expr}`, SD-9) — plus the
  message `strategy` and `text_config`. It MUST return an empty config (not an error) for a
  non-searchable message. It is the single source consumed by `protoc-gen-storage`,
  `protoc-gen-ent`, and `cmd/openapiv2to3` (044 no-drift rule).
- **FR-A2**: The resolver MUST run each source through its **flavor validator** and fail loud
  (abort codegen, naming message + source) when: a `field` source is of a type FTS cannot cover
  (anything but `string`, `enum`, string-typed repeated/`tags`, or timestamp-to-text); a
  `sql/<dialect>` expr references another table or an obviously non-immutable function (best-effort);
  or a source declares an unknown flavor or a flavor `version` the validator does not accept.
- **FR-A3**: A field marked `searchable` (or referenced by a source) that is also
  `secret`/`INPUT_ONLY` MUST be rejected with a named error — searching a write-only/redacted value
  leaks it through match behavior. An `OUTPUT_ONLY` server-computed string MAY be searchable.
- **FR-A4**: The `searchable` field option and the `search` message option MUST be added to the
  canonical schemas in `github.com/infobloxopen/apis`, released, and mirrored into this repo + the
  scaffold mirror byte-identically (`make sync-scaffold-mirrors`; `mirror_drift_test.go` green).
- **FR-A5**: The v1 flavor set MUST be exactly `field`, `sql` (dialect `postgres`), and `cel`. An
  unknown flavor, an unsupported `sql` dialect, or `strategy: PROJECTED` MUST validate-then-fail-loud
  as "declared but not built in this feature" (never silently ignored, never emitted as a local
  predicate) — reserving those surfaces without implementing them.
- **FR-A6**: A `cel` source MUST be compiled to an equivalent SQL expression (fed to `to_tsvector`,
  SD-7) by a `cel`-flavor compiler in a shared internal package. It MUST support the subset the DDI
  shapes need — field refs; `?:` / map-literal lookup → `CASE`; string ops (`+`, `.replace`,
  `.lowerAscii`); structured message / `map` / `tags` access → the Postgres equivalent — and MUST
  fail loud (naming the source) on any CEL construct outside the supported subset rather than emit
  wrong SQL. The CEL MUST be type-checked against the resource message descriptor before compilation
  (the strongest per-flavor validation, SD-9). The same compiled form MUST also yield the
  SQLite-fallback expression (FR-B5), so a `cel`-only resource stays portable.

### Part B — the `q` operator and the query-time (JIT) predicate

- **FR-B1**: `persistence.ListOptions` MUST gain a `Search string` field; `Repository[T,K].List`
  MUST honor it (JIT default). An empty `Search` MUST be a no-op.
- **FR-B2**: `protoc-gen-svc` MUST detect a `string q` field on a List request
  (`hasStringField(m.Input,"q")`) and map `req.GetQ()` onto `ListOptions.Search` in `stdList`,
  composing with the existing filter/order_by/pagination mapping (`render.go:501-534`).
- **FR-B3**: For a searchable resource on **Postgres**, the generated GORM List
  (`protoc-gen-storage/render.go`) MUST, when `Search` is non-empty, AND a parameterized
  `to_tsvector('<cfg>', <coalesced sources>) @@ websearch_to_tsquery('<cfg>', ?)` predicate onto the
  query after the AIP-160 filter WHERE (`widgets.storage.go:167-172`). The `<coalesced sources>`
  expression unions the field-flagged columns and any `sql/postgres` source exprs (SD-9). The user
  string MUST be a bound parameter.
- **FR-B4**: The generated ent List (`protoc-gen-ent/render.go:1513-1518`) MUST AND an equivalent
  full-text `sql.P(...)` predicate with the `toEntPredicate` result, using the same source set,
  config, and strategy from the shared resolver (FR-A1).
- **FR-B5**: On **SQLite**, for a resource whose sources are all portable (`field`), the generated
  `q` predicate MUST degrade to a case-insensitive `LIKE '%'||?||'%'` OR-across-field-columns
  contains (SD-4), selecting the dialect from the backend available to the generated code
  (`db.Dialector.Name()` for GORM; the `dialect` plugin flag for ent). A resource with a
  `sql/postgres` source MUST instead fail loud at codegen for the SQLite backend (FR-A2/SD-4).

### Part C — the index-backed strategy

- **FR-C1**: A message MUST be able to declare `strategy: INDEXED` (default `JIT`) in its `search {}`
  config; the value is overridable per storage surface (WS-005). `strategy: PROJECTED` MUST be
  accepted by the schema but rejected at codegen per FR-A5.
- **FR-C2**: For an `INDEXED` searchable resource, `make generate` MUST emit, under the resource's
  migrations dir, (a) a `search_vector tsvector GENERATED ALWAYS AS (to_tsvector('<cfg>', <coalesced
  sources incl. sql/postgres exprs>)) STORED` column and (b) a `CREATE INDEX CONCURRENTLY … USING GIN
  (search_vector)` as the **sole statement** in its own up file, with a matching `DROP INDEX` down
  (SD-7). Emission MUST be idempotent (`make generate` twice → no diff).
- **FR-C3**: The generated List for an `INDEXED` resource MUST match against the persisted
  `search_vector` column on Postgres; its SQLite fallback remains the FR-B5 `LIKE` contains (no
  persisted column on SQLite; `sql/postgres`-sourced resources remain Postgres-only).

### Part D — contract / OpenAPI surface

- **FR-D1**: `cmd/openapiv2to3/enrich.go` MUST add a `q` query parameter and an `x-aip-search`
  extension (searchable source JSON names, `strategy`, `text_config`) to each List operation whose
  resource is searchable — parallel to `paginationExt`, gated on `aip.MethodList`
  (`enrich.go:121-123`).
- **FR-D2**: The `testdata/toy` fixture MUST gain a `string q` field on `ListWidgetsRequest`, at
  least one field-flagged `searchable` string field on `Widget`, and one message-level
  `sql/postgres` calculated source exercising the DDI `CASE`/JSON shape; the golden
  `testdata/toy/openapi/toy.openapi.yaml` MUST be regenerated to show the `q` param + `x-aip-search`.

### Cross-cutting

- **FR-X1**: `go build ./... && make test` clean; `make generate` idempotent (no diff on a second
  run); `scripts/check-graph-isolation.sh` green — no new heavy dependency in the root module (v1
  uses generated SQL + the existing `protoreflect`/`descriptorpb` + present dialects; a `sql`
  validator parser, if added, must not enter the root module's runtime dep graph).
- **FR-X2**: End-to-end proof over a **real** running boundary, not just unit assertions: the toy
  service MUST answer `GET …/widgets?q=<term>` via the generated gateway returning only matching rows
  — on SQLite (FR-B5 fallback, `field` sources) in the fast test, and on Postgres (true FTS, both
  strategies, incl. the `sql/postgres` calculated source) in the `pgtest` container harness. An
  ent-backed fixture (`apikey` or `iam`) MUST also round-trip `q` (FR-B4), not only GORM.
- **FR-X3**: Scaffold mirrors stay byte-identical to canonical (`mirror_drift_test.go` green); no
  unrelated regeneration churn in other fixtures.

## Acceptance Criteria

- **AC-1**: A `Widget` with a field-flagged `searchable` `display_name` and a row containing "acme"
  is returned by `GET /v1/widgets?q=acme` and a non-matching row is not — proven over the real
  generated REST gateway on SQLite (FR-B5) and Postgres (FR-B3).
- **AC-2**: `q` composes: `?q=acme&filter=state="ACTIVE"&order_by=create_time` returns only rows
  matching both, ordered — no operator dropped (SD-6).
- **AC-3**: On Postgres, `websearch_to_tsquery` input safety holds: `q="ac(me\" or"` returns a
  well-formed result (no SQL error, no injection) because the term is a bound parameter (SD-5).
- **AC-4**: An `INDEXED` `Widget` surface produces the `search_vector` generated column + a `CREATE
  INDEX CONCURRENTLY … USING GIN` migration (sole statement); applying it in `pgtest` and running
  `EXPLAIN` shows the GIN index is used (FR-C2/C3).
- **AC-5**: The enriched `toy.openapi.yaml` shows the `q` query parameter and an `x-aip-search`
  extension enumerating the sources + `strategy` + `text_config` (FR-D1/D2).
- **AC-6**: A `searchable` field that is `secret`/`INPUT_ONLY`, or of an unsupported type, fails
  `make generate` with an error naming message + field + reason (FR-A2/A3), proven by a regression
  fixture.
- **AC-7**: `make generate && git diff --exit-code` clean (deterministic golden + migrations);
  `mirror_drift_test.go` + `check-graph-isolation.sh` green (FR-X1/X3).
- **AC-8**: A message-level `sql/postgres` source expressing a `CASE`-based enum→string mapping (the
  DDI `map_zone_type` shape) is compiled into the generated `to_tsvector` expression and matched by
  `q` on Postgres — proven in `pgtest` (FR-B3/C2, SD-9).
- **AC-9**: A resource declaring `strategy: PROJECTED` (or an unknown flavor) fails `make generate`
  with a clear "declared but not built in this feature" error — reserved, never silently emitted
  (FR-A5, D2).
- **AC-10**: A resource with a `sql/postgres` source fails loud when generating the SQLite backend,
  while a `field`-only resource generates a working SQLite `LIKE` path (SD-4, FR-B5).

## Failure Modes (must be handled, fail-loud not silent)

- **FM-1 — Unsupported/leaky searchable field**: a `searchable` flag on a non-textual type, or on a
  `secret`/`INPUT_ONLY` field, aborts codegen with a named error (FR-A2/A3). A silent skip would give
  a `q` param that quietly ignores the field, or leak a write-only value via match timing.
- **FM-2 — SQLite `to_tsvector` crash**: the generated code MUST NOT emit Postgres FTS SQL against
  SQLite; the dialect branch (FR-B5) is mandatory and a SQLite unit test asserts the `LIKE` fallback.
- **FM-3 — Unparameterized interpolation**: the `q` string is always a bound parameter; a generated
  fragment that concatenates the term is a correctness+security defect (AC-3 guards).
- **FM-4 — Non-idempotent migration emission**: emitting a fresh `search_vector`/GIN migration on
  every `make generate` would corrupt the version sequence; emission MUST be idempotent (FR-C2, AC-7).
- **FM-5 — Resolver drift**: the source set, config, and strategy used by the GORM generator, the ent
  generator, and the OpenAPI pass MUST come from the one `internal/aip` resolver (FR-A1); a
  per-generator reimplementation would desync the compiled predicate from the published contract.
- **FM-6 — `CREATE INDEX CONCURRENTLY` in a transaction**: `CONCURRENTLY` cannot run inside a txn;
  the migration MUST place it in its own file as the sole statement (SD-7/FR-C2), else it fails at
  apply in `pgtest`.
- **FM-7 — Reserved value silently emitted**: `strategy: PROJECTED` (or an unknown flavor) MUST NOT
  fall through to a local predicate or a no-op; it MUST fail loud (FR-A5, AC-9). Emitting a local
  predicate for `PROJECTED` would silently give the wrong (local) semantics for a resource meant to
  be globally indexed.
- **FM-8 — Flavor/backend gap silently generated**: a `sql/postgres`-only resource MUST fail loud
  when generating for SQLite (FR-B5/AC-10), never emit broken SQLite SQL that crashes at runtime.

## Open Questions (remaining after clarification)

- **RQ-1 — `cel` flavor: v1 or fast-follow?** `cel` is reserved in the annotation now (SD-9). Ship the
  CEL→SQL compiler (for the `CASE`/map/string/JSON subset) in v1 so calculated fields are portable +
  strongly validated from day one, or ship `field` + `sql/postgres` in v1 and add `cel` next
  (calculated fields are Postgres-locked until then)? Current spec assumes fast-follow.
- **RQ-2 — `sql` validator depth.** How hard the `sql/postgres` validator tries to prove immutability
  and single-table scope at codegen (best-effort static check vs. lean on Postgres rejecting a bad
  generated-column expr at migration-apply time). Current spec: best-effort static + apply-time
  fail-loud.
- **RQ-3 — Per-surface strategy mechanics.** Exactly how a storage *surface* (WS-005 projection)
  overrides the message-level `strategy`/`text_config` — an annotation on the surface projection vs.
  a `kind:Storage` setting. Confirm the override carrier.
- **RQ-4 (deferred, confirm) — ranking (Q5)** stays out (`q` is a pure filter); **query modes (Q6)**
  stay at `websearch_to_tsquery` as-is (no explicit prefix/quoted-exact). Confirm these remain
  deferred rather than in-v1.

## Out of scope (WS-041 later / other workstreams)

- **Global/cross-service search — WS-042.** The `PROJECTED` materialization: a search-projection
  service consuming the `events.ChangeEvent` outbox feed and maintaining a cross-type/cross-moat
  index (sibling of WS-039; shares the outbox→projection ingestion shape). WS-041 only *reserves* the
  strategy value + keeps the declaration materialization-agnostic.
- **Write-time (Go) evaluation of search sources** (the full-CEL-fidelity model that would compute
  one document for both the local column and the WS-042 projection) — deferred (SD-7).
- **The CEL→SQL compiler** if RQ-1 chooses fast-follow; `mysql`/other `sql` dialects; any open flavor
  plugin surface.
- **Joined/related-table search sources** (the DDI htree/dhcpview cross-table shape) — needs triggers
  or the `PROJECTED` path.
- **Ranking/`ts_rank` ordering; prefix (`:*`)/quoted-exact modes; `pg_trgm` similarity; external
  engines.** Any authz change; any DDD write-boundary change; any change to the AIP-160 `filter`
  grammar.
