# Tasks: F017 — Safe filter + sort parser

**Branch**: `017-safe-list-filter`

---

- [ ] T001 [C] Create `persistence/filter` package — AIP-160 safe filter + order_by parser.

  Create `persistence/filter/filter.go`. The file must:

  **Exported types:**
  ```go
  // Condition is a DB-agnostic parameterized WHERE condition.
  type Condition interface {
      // SQL returns a parameterized SQL fragment and bind args.
      // Values are always bind args — never interpolated into the string.
      SQL() (string, []interface{})
  }

  // OrderClause is a validated ORDER BY term.
  type OrderClause struct {
      Column string // DB column name (already validated)
      Desc   bool
  }

  // GORMExpr returns "col ASC" or "col DESC".
  func (c OrderClause) GORMExpr() string
  ```

  **Exported functions:**
  ```go
  // Parse parses an AIP-160 filter expression.
  // columns maps proto field name → DB column name (whitelist).
  // Unknown field names return codes.InvalidArgument.
  // Empty expr returns nil, nil (no condition to apply).
  func Parse(expr string, columns map[string]string) (Condition, error)

  // ParseOrderBy parses a comma-separated AIP-132 order_by string.
  // Each term: "field" | "field asc" | "field desc" (case-insensitive).
  // Unknown fields return codes.InvalidArgument.
  // Empty string returns nil, nil.
  func ParseOrderBy(orderBy string, columns map[string]string) ([]OrderClause, error)
  ```

  **Internal grammar for Parse (recursive descent):**
  ```
  expr     := or_expr
  or_expr  := and_expr ('OR' and_expr)*
  and_expr := not_expr ('AND' not_expr)*
  not_expr := 'NOT' primary | primary
  primary  := '(' expr ')' | comparison
  comparison := IDENT op value
  op       := '=' | '!=' | '<' | '<=' | '>' | '>=' | ':'
  value    := STRING | NUMBER | IDENT
  ```
  - IDENT tokens that spell `AND`, `OR`, `NOT` are keywords; others are field names.
  - STRING is a double-quoted literal; escapes `\"` and `\\` are honoured.
  - NUMBER is any token matching `[0-9]+(\.[0-9]+)?`.
  - `:` (has operator) maps to `column LIKE ?` with bind arg `%value%`.
  - All six comparison operators produce `column op ?` with the literal as bind arg.
  - Every IDENT used as a field name is looked up in `columns`; unknown → `InvalidArgument`.

  **Concrete condition types (unexported is fine):**
  - `comparison{col, op, val}` — implements `SQL()` returning `"col op ?"` + `[]interface{}{val}`
  - `binary{op, left, right}` — implements `SQL()` returning `"(lsql) op (rsql)"` + combined args
  - `not{inner}` — implements `SQL()` returning `"NOT (sql)"` + args

  **No new external imports.** Use only stdlib + `google.golang.org/grpc/codes` +
  `google.golang.org/grpc/status` (both already in go.mod).

  Create `persistence/filter/filter_test.go` with table-driven tests covering:
  - All six comparison operators with string and numeric values.
  - Has operator produces `LIKE '%value%'`.
  - `AND` and `OR` combinations.
  - `NOT` prefix.
  - Parenthesised grouping `(a=1 OR b=2) AND c=3`.
  - Unknown field name → `codes.InvalidArgument`.
  - Value containing SQL metacharacter (`'`, `;`, `--`) appears as bind arg, never in SQL string.
  - ParseOrderBy: single field, `asc`, `desc`, multiple fields, unknown field → error.
  - Empty filter and empty order_by both return nil, nil.

  Run `go test ./persistence/filter/... -count=1` — must pass.

- [ ] T002 [S] Update `cmd/protoc-gen-storage/render.go` — emit column map + safe filter/order_by.

  **Step 1 — add imports to the generated file.**
  In `renderStorageFile`, unconditionally add these two imports in the generated output:
  ```go
  "google.golang.org/grpc/codes"
  "google.golang.org/grpc/status"
  "github.com/infobloxopen/devedge-sdk/persistence/filter"
  ```
  Add them after the existing `persistence` import. Also add blank-identifier guards for
  `codes` and `status` in the `var (...)` block so they compile even on non-error paths
  (like the existing `_ = base64.StdEncoding` guards). Actually — `codes`/`status` are
  only referenced inside the List function body, so no guard needed; they will always be
  referenced. Just add them to the import block.

  **Step 2 — emit column map in `renderMessage`.**
  After the `fromModel_<Msg>` function and before the repository struct, emit:
  ```go
  // <Msg>Columns maps proto field names to DB column names for safe filter/order_by parsing.
  var <Msg>Columns = map[string]string{
      "id": "id",
      "<field1>": "<col1>",
      ...
  }
  ```
  Include the `id` field (always mapped to `"id"`). For each non-ID, non-repeated, non-message,
  non-secret field, emit `"<f.Name>": "<effectiveColumn>"` where `effectiveColumn` is
  `f.ColumnName` if non-empty, else `f.SnakeName`. Skip secret fields (their DB columns are
  `<name>_hash` / `<name>_cipher` — not directly filterable). Skip relationship fields.

  **Step 3 — replace the raw filter block in List.**
  Replace (render.go line 300):
  ```go
  b.WriteString("\tif opts.Filter != \"\" {\n\t\tq = q.Where(opts.Filter)\n\t}\n")
  ```
  with:
  ```go
  fmt.Fprintf(b, "\tif opts.Filter != \"\" {\n")
  fmt.Fprintf(b, "\t\tcond, err := filter.Parse(opts.Filter, %sColumns)\n", msg.MessageName)
  fmt.Fprintf(b, "\t\tif err != nil {\n")
  fmt.Fprintf(b, "\t\t\treturn nil, \"\", status.Errorf(codes.InvalidArgument, \"invalid filter: %%v\", err)\n")
  fmt.Fprintf(b, "\t\t}\n")
  fmt.Fprintf(b, "\t\tsql, args := cond.SQL()\n")
  fmt.Fprintf(b, "\t\tq = q.Where(sql, args...)\n")
  fmt.Fprintf(b, "\t}\n")
  ```

  **Step 4 — add order_by block in List.**
  After the filter block, add:
  ```go
  fmt.Fprintf(b, "\tif opts.OrderBy != \"\" {\n")
  fmt.Fprintf(b, "\t\tclauses, err := filter.ParseOrderBy(opts.OrderBy, %sColumns)\n", msg.MessageName)
  fmt.Fprintf(b, "\t\tif err != nil {\n")
  fmt.Fprintf(b, "\t\t\treturn nil, \"\", status.Errorf(codes.InvalidArgument, \"invalid order_by: %%v\", err)\n")
  fmt.Fprintf(b, "\t\t}\n")
  fmt.Fprintf(b, "\t\tfor _, c := range clauses {\n")
  fmt.Fprintf(b, "\t\t\tq = q.Order(c.GORMExpr())\n")
  fmt.Fprintf(b, "\t\t}\n")
  fmt.Fprintf(b, "\t}\n")
  ```

  **Step 5 — update `render_test.go`.**
  Add assertions to `TestRenderStorageFile_basic`:
  - `mustContain(t, out, "WidgetColumns")` — column map is present.
  - `mustContain(t, out, `filter.Parse`)` — safe filter path.
  - `mustContain(t, out, `filter.ParseOrderBy`)` — safe order_by path.
  - `mustNotContain(t, out, "q.Where(opts.Filter)")` — raw injection path is gone.
  Add a `mustNotContain` helper if not already present.

  Run `go test ./cmd/protoc-gen-storage/... -count=1`.

- [ ] T003 [S] Regenerate testdata.

  Run:
  ```
  cd /Users/dgarcia/go/src/github.com/infobloxopen/devedge-sdk
  go run ./cmd/protoc-gen-storage ... # check Makefile for the correct regeneration command
  ```
  Check `Makefile` for a `generate` or `regen` target and use it, or run `buf generate`
  with the correct template. After regeneration, verify:
  - `testdata/toy/widgetsv1/widgets.storage.go` contains `WidgetColumns` map and `filter.Parse`.
  - `testdata/apikey/apikeyv1/apikey.storage.go` contains `APIKeyColumns` map and `filter.Parse`.
  - Neither file contains `q.Where(opts.Filter)`.
  Then run `go build ./... && make test` to ensure the regenerated files compile and tests pass.

- [ ] T004 [S] Success-criteria verification.

  Run each check and confirm it passes:
  1. `grep -rn "q\.Where(opts\.Filter)\|q\.Where(filter)" --include="*.go" .` → zero matches
     outside of test fixtures in `persistence/filter/`.
  2. `go test ./persistence/filter/... -count=1 -run TestParse` — look for the injection test
     (`'; DROP TABLE` value lands as bind arg, not in SQL string). Print the SQL string in the
     test assertion failure message to make it clear.
  3. `make build && make test` — clean.
  4. Visually confirm `widgets.storage.go` and `apikey.storage.go` contain the `Columns` map
     and the `filter.Parse` / `filter.ParseOrderBy` blocks.

- [ ] T005 [S] Create branch, commit, merge.

  ```bash
  git checkout -b 017-safe-list-filter
  git add persistence/filter/ cmd/protoc-gen-storage/ testdata/ specs/017-safe-list-filter/
  git commit -m "017: safe filter + order_by parser — fix SQL injection in generated GORM List"
  git checkout main && git merge --no-ff 017-safe-list-filter
  git commit -m "017: merge safe list filter"
  ```

## Complexity Tags

- T001 [C] — non-trivial recursive-descent parser; all literal values must remain as bind args.
- T002 [S] — mechanical substitution in `render.go` + test assertions.
- T003 [S] — regeneration + build check.
- T004 [S] — grep + test run.
- T005 [S] — git workflow.
