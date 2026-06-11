# Feature Specification: F017 — Safe filter + sort parser

**Feature Branch**: `017-safe-list-filter`
**Created**: 2026-06-11
**Status**: Draft

## Context

`protoc-gen-storage` generates GORM-backed List methods that pass user-supplied
`filter` and (when added) `order_by` strings directly to GORM:

```go
// VULNERABLE — generated today
if opts.Filter != "" {
    q = q.Where(opts.Filter)   // raw SQL injection surface
}
```

`opts.Filter` comes from the gRPC request field. An authenticated caller can send
`filter = "1=1; DROP TABLE widgets"` or `filter = "1=1 UNION SELECT password FROM users"`.
GORM wraps user input in a `WHERE` clause without parameterization when a plain
string is passed (no `?` placeholders). Every GORM-backed List handler is
vulnerable.

`opts.OrderBy` has no generator path yet, but the field exists in `ListOptions`
and the same class of bug would appear when it is wired.

The in-memory and ent implementations are not affected (ent ignores filter/sort
today; memory ignores them by definition).

## Clarifications

- **Filter syntax**: AIP-160 subset — field comparisons (`=`, `!=`, `<`, `<=`,
  `>`, `>=`), has (`:` operator maps to `LIKE '%value%'`), logical operators
  (`AND`, `OR`, `NOT`), and grouping (`()`). No function calls in Phase 1.
- **No new external dependencies.** A hand-rolled recursive-descent parser keeps
  the module clean. CEL-go is ~1 MB of transitive deps; the subset we need is
  ~250 lines.
- **Column map**: the code generator knows every proto field and its DB column
  name. It will emit a `var <Msg>Columns = map[string]string{...}` constant in
  the generated file that the safe parser uses as a whitelist.
- **Unknown fields in filter or order_by** return `codes.InvalidArgument`.
- **OrderBy**: comma-separated list of `field [asc|desc]`. Each field validated
  against the column map. Direction defaults to `asc`.
- **`persistence/filter` package**: the parser lives here, independent of GORM,
  so it can be tested without a database. GORM-specific adaptation (applying the
  result to a `*gorm.DB`) lives in a thin `persistence/gormfilter` file or inline
  in generated code — TBD during planning.
- **Generated test data** (`testdata/toy/`, `testdata/apikey/`) is regenerated
  after the generator is updated.
- **`MemoryRepository`** continues to silently ignore `Filter` and `OrderBy`
  (documented behavior). No change needed.

## Requirements

- **FR-001**: Add `persistence/filter` package exporting:
  - `Parse(expr string, columns map[string]string) (Condition, error)` — parses an
    AIP-160 filter expression, validates every referenced field against `columns`,
    and returns a structured condition tree (or `InvalidArgument` error).
  - `ParseOrderBy(orderBy string, columns map[string]string) ([]OrderClause, error)` —
    parses a comma-separated order_by string, validates fields, returns ordered
    clauses (or `InvalidArgument` error).
  - `Condition` and `OrderClause` types are DB-agnostic (no GORM import).

- **FR-002**: `persistence/filter` produces **parameterized** output. All literal
  values are placeholders (`?`) with a companion `[]interface{}` args slice —
  never string interpolation into SQL.

- **FR-003**: Update `cmd/protoc-gen-storage/render.go` to:
  - Emit `var <Msg>Columns = map[string]string{...}` mapping proto field names to
    DB column names, using the same derivation logic already used for GORM struct
    tags (snake_case of field name, overridden by `column_name` annotation).
  - Replace the raw `q.Where(opts.Filter)` with a call to the safe parser; apply
    the `Condition` to the GORM query using parameterized `q.Where(sql, args...)`.
  - Emit an `order_by` block using `ParseOrderBy` and `q.Order(clause)` for each
    validated `OrderClause`.
  - Return `status.Errorf(codes.InvalidArgument, ...)` if `Parse` or `ParseOrderBy`
    returns an error.

- **FR-004**: Regenerate `testdata/toy/widgetsv1/widgets.storage.go` and
  `testdata/apikey/apikeyv1/apikey.storage.go` — the vulnerable `.Where(opts.Filter)`
  lines must not appear in the regenerated output.

- **FR-005**: Unit tests for `persistence/filter`:
  - Valid comparisons for all six operators.
  - Has operator maps to `LIKE '%value%'`.
  - `AND`, `OR`, `NOT`, grouping.
  - Unknown field → `InvalidArgument`.
  - SQL metacharacter in value is quoted as a bind param (injection test).
  - Valid and invalid `order_by` strings.

- **FR-006**: `make test` passes. No new `go vet` or lint warnings.

## Success Criteria

- **SC-001**: `grep -rn "q.Where(opts.Filter)\|q.Where(filter)" --include="*.go" .`
  returns zero matches outside of `persistence/filter` test fixtures.
- **SC-002**: `go test ./persistence/filter/...` passes, including an explicit test
  that a filter value containing `'; DROP TABLE` is correctly parameterized (the
  literal value appears as a bind argument, never in the SQL string).
- **SC-003**: `make build && make test` clean across the whole module.
- **SC-004**: The generated `widgets.storage.go` and `apikey.storage.go` contain
  the `Columns` map and use the safe filter/order_by path.
