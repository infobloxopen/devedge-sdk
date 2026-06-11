# Feature Specification: F018 — Field-mask application in GORM Update

**Feature Branch**: `018-fieldmask-apply`
**Created**: 2026-06-11
**Status**: Draft

## Context

`protoc-gen-storage` generates Update methods that accept a `fieldMask ...string`
variadic of proto field names and pass them directly to GORM's `Select`:

```go
if len(fieldMask) > 0 {
    q = q.Select(fieldMask)   // proto field names, not DB column names
}
```

GORM v2's `Select([]string).Updates(struct)` matches by Go struct field name or
DB column name. For simple resources where `proto_name == column_name`, this
happens to work. But when a field carries a `(infoblox.field.v1.opts).column_name`
override, the proto name diverges from the DB column name, and `Select` silently
touches the wrong column (or skips the update entirely).

Additionally, there is no validation: if a caller includes a nonexistent or
misspelled field in the mask, GORM silently ignores it — no error is returned,
and the user's intent is lost.

F017 added `<Msg>Columns` (proto name → DB column name) to every generated file.
F018 uses that map to translate the mask and reject unknown fields.

## Clarifications

- **Only non-secret fields** are eligible in the mask. Secret fields (`IsSecret`)
  are handled by dedicated logic that checks the value and writes `_hash` /
  `_cipher` columns — they cannot be individually masked.
- **Unknown mask field** → `codes.InvalidArgument`.
- **Empty mask** → update all non-zero-value fields (existing GORM behavior,
  unchanged).
- No change to the `Repository` interface or `ListOptions`.

## Requirements

- **FR-001**: In `cmd/protoc-gen-storage/render.go`, replace the generated
  `q.Select(fieldMask)` block with a translation loop:
  ```go
  if len(fieldMask) > 0 {
      dbCols := make([]string, 0, len(fieldMask))
      for _, f := range fieldMask {
          col, ok := <Msg>Columns[f]
          if !ok {
              return nil, status.Errorf(codes.InvalidArgument,
                  "unknown field in update_mask: %q", f)
          }
          dbCols = append(dbCols, col)
      }
      q = q.Select(dbCols)
  }
  ```
  `codes`, `status`, and `<Msg>Columns` are already imported/emitted by F017.

- **FR-002**: Regenerate `testdata/toy/widgetsv1/widgets.storage.go` and
  `testdata/apikey/apikeyv1/apikey.storage.go`.

- **FR-003**: Add a `render_test.go` assertion:
  `mustNotContain(t, out, "q.Select(fieldMask)")` — raw mask pass-through is gone.
  `mustContain(t, out, `Columns[f]`)` — column map lookup is present.

- **FR-004**: `make build && make test` clean.

## Success Criteria

- **SC-001**: `grep -rn 'q\.Select(fieldMask)' --include="*.go" .` → zero matches
  outside test assertions.
- **SC-002**: `make build && make test` clean.
- **SC-003**: Generated `widgets.storage.go` contains the column-map lookup loop
  instead of the raw `Select(fieldMask)` call.
