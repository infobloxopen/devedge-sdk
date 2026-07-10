# Tasks: 046-fulltext-search (WS-041 foundation)

`[S]` = simple/mechanical → Sonnet subagent · `[C]` = complex → Opus. Order respects dependencies;
WP-0 blocks all codegen that reads the annotations. Each task cites the spec FR/AC it satisfies.

## WP-0 — Annotations + release (BLOCKING)
- **T001 [C]** Design-freeze the `searchable` field option + `SearchConfig` message option per
  `plan.md`; add to canonical `infoblox.field.v1` + `infoblox.storage.v1` in `infobloxopen/apis`
  (+ `Infoblox-CTO/apis`), lint/breaking-check via apx. (FR-A4)
- **T002 [S]** Release a new alpha of both modules through the apx canonical pipeline (public release
  authorized); record the versions. (FR-A4)
- **T003 [S]** Bump devedge-sdk to the new apis modules; mirror-sync `proto/infoblox/{field,storage}/v1`
  + scaffold mirrors byte-identically (`make sync-scaffold-mirrors`; `mirror_drift_test.go` green).
  (FR-A4/FR-X3)

## WP-1 — Shared resolver + field/sql validators `[C]`
- **T010 [C]** `internal/aip.SearchConfig(md)` resolver: union field-flagged columns + message
  `sources`; strategy + text_config; empty-when-absent. (FR-A1)
- **T011 [C]** `compileSource` interface + `field` and `sql/postgres` flavors (normalize; best-effort
  immutable/single-table check for sql). (FR-A2, SD-9, RQ-2)
- **T012 [S]** Reject rules: `secret`/`INPUT_ONLY`/non-textual searchable → named error; `PROJECTED`/
  unknown flavor/unsupported dialect → validate-then-fail-loud. (FR-A2/A3/A5, FM-1/FM-7)

## WP-2 — CEL→SQL compiler `[C]`
- **T020 [C]** CEL environment + type-check against the message descriptor. (FR-A6, SD-9)
- **T021 [C]** Compile the supported subset (field refs; `?:`/map-literal→`CASE`; string ops;
  message/`map`/`tags` access) to a Postgres expr AND a SQLite-fallback expr from one AST walk;
  fail loud on any unsupported construct. (FR-A6)
- **T022 [S]** Unit tests incl. the DDI `map_zone_type` shape + an out-of-subset construct aborting.
  (AC-11)

## WP-3 — `q` operator plumbing `[S]`
- **T030 [S]** Add `Search string` to `persistence.ListOptions`; honor in `List` (JIT default,
  empty=no-op). (FR-B1)
- **T031 [S]** `protoc-gen-svc`: detect `string q`, map `req.GetQ()`→`ListOptions.Search` in
  `stdList`, composing with filter/order_by/pagination. (FR-B2, AC-2)

## WP-4 — GORM emission + INDEXED migration `[C]`
- **T040 [C]** `protoc-gen-storage`: emit the JIT `to_tsvector @@ websearch_to_tsquery(?)` predicate
  (bound term) after the filter WHERE; use the compiled sources. (FR-B3, AC-3)
- **T041 [C]** SQLite `LIKE` OR-contains fallback for portable resources; `sql/postgres`-source
  resource → fail loud for SQLite. (FR-B5, AC-10, FM-2/FM-8)
- **T042 [C]** `INDEXED`: emit the `search_vector` generated column + `CREATE INDEX CONCURRENTLY … GIN`
  (sole statement, own migration file); idempotent; List matches the persisted column. (FR-C2/C3,
  AC-4, FM-4/FM-6)

## WP-5 — ent emission `[C]`
- **T050 [C]** `protoc-gen-ent`: AND a raw `sql.P(...)` full-text predicate with `toEntPredicate`,
  same sources/config/strategy; SQLite fallback parity. (FR-B4)

## WP-6 — OpenAPI `[S]`
- **T060 [S]** `openapiv2to3/enrich.go`: add the `q` param + `x-aip-search` on searchable List ops.
  (FR-D1, AC-5)

## WP-7 — Fixtures + e2e `[C]`
- **T070 [S]** Toy: add `string q` + `searchable display_name` + one `sql/postgres` + one `cel`
  source; regen; update golden `toy.openapi.yaml`. (FR-D2, AC-1/5/8/11)
- **T071 [S]** ent fixture (`apikey`/`iam`): add `q`; regen. (FR-X2)
- **T072 [C]** e2e over the real gateway: SQLite fast test (fallback) + Postgres `pgtest` (both
  strategies, GIN via `EXPLAIN`, injection-safety). (FR-X2, AC-1/3/4/8)
- **T073 [S]** Regression test per failure mode FM-1..FM-8. (all FM)

## WP-8 — Reserved fail-loud `[S]`
- **T080 [S]** `strategy: PROJECTED` + unknown flavor abort `make generate` with a clear
  "declared but not built" error + a fixture proving it. (FR-A5, AC-9, FM-7)

## WP-9 — Hardening (≥2 loops)
- **T090** Loop 1 — DX/correctness (`devedge-dx-builder` builds a searching service from docs only →
  `devedge-dx-critic` verifies, falsifies, ranks); fix findings; add regression tests.
- **T091** Loop 2 — security (`devedge-pentest` attacks the FTS surface: SQL/tsquery injection,
  cross-tenant search leakage, filter-composition bypass → `devedge-security-validator` reproduces/
  falsifies); fix; add regression tests. Gate for "hardened".

## WP-10 — Docs `[S]`
- **T100 [S]** How-to (declare `searchable` + `search{}`, choose a strategy, use `q`) + reference
  (the annotation + `x-aip-search`) + CHANGELOG; conform to `docs/STYLE-GUIDE.md`.

## WP-11 — Release
- **T110** devedge-sdk PR (feature/046) → verify gate green → admin-merge; cut the release; confirm
  the apis modules are consumed. Then WS-042 unblocks.
