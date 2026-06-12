# Feature Specification: F019 — Resource name generation (AIP-122)

**Feature Branch**: `019-resource-names`
**Created**: 2026-06-12
**Status**: Draft

## Context

Resources currently use a flat `string id` as their primary identifier. AIP-122
requires resources to have a `name` field containing a hierarchical path like
`"widgets/abc123"` or `"projects/p1/apikeys/k7"`. Without name generation,
services cannot reference resources from other services by a stable identifier,
and the framework doesn't compose into a cross-service resource graph.

Today there is no:
- `google.api.resource` annotation support in `protoc-gen-storage`
- `Format<Msg>Name(id) string` helper in generated code
- `Parse<Msg>Name(name) (id, error)` helper
- Population of the `name` field in `fromModel_<Msg>`

## Clarifications

- **Scope boundary.** F019 adds *generation* of name helpers and *population* of
  the `name` field in `fromModel`. It does NOT migrate `Get`/`Delete`/`Update`
  to accept resource names instead of raw IDs — that is a follow-on step. The
  key argument to Repository methods remains the raw string key; callers that
  have migrated their protos will use `Parse<Msg>Name` before calling the repo.
- **Pattern source.** `protoc-gen-storage` reads the `(google.api.resource)`
  message-level annotation (already in the `buf.build/googleapis/googleapis` dep)
  and uses the first element of its `pattern` field (e.g., `"widgets/{widget}"`).
- **Resource name field.** A field named `name` annotated with
  `(google.api.field_behavior) = OUTPUT_ONLY` is the AIP-122 resource name.
  `fromModel_<Msg>` sets it from `Format<Msg>Name(m.ID)`. `toModel_<Msg>` skips
  it (not persisted). The `IsOutputOnly` flag is detected by the generator.
- **`persistence/resourcename` package.** A new, dependency-free package provides
  the underlying parse/format logic. Generated code calls it; the package has no
  gRPC, GORM, or proto imports.
- **Test fixtures.** Both `testdata/toy/widgets.proto` and
  `testdata/apikey/apikey.proto` are updated to add the annotation and fix the
  naming collision (existing `name = 2` fields are renamed to `display_name` and
  `label` respectively).
- **Existing `id` field stays.** Removing `id` from resource protos is a
  separate migration; for now both `id` and `name` coexist.

## Requirements

- **FR-001**: Add `persistence/resourcename` package with:
  - `Parse(pattern, name string) (map[string]string, error)` — matches a name
    against a pattern, returning the variable bindings. Returns error if the
    segment count doesn't match or a literal segment mismatches.
  - `Format(pattern string, vars map[string]string) (string, error)` — replaces
    `{var}` placeholders in the pattern with the given values. Returns error if a
    required variable is missing.
  - `IDFromName(pattern, name string) (string, error)` — convenience: returns
    the value of the last variable in the pattern (the resource ID).

- **FR-002**: Add `ResourcePattern string` to `messageInfo` and `IsOutputOnly bool`
  to `fieldInfo` in `cmd/protoc-gen-storage/render.go`.

- **FR-003**: Update `cmd/protoc-gen-storage/main.go` to:
  - Import `apiannotations "google.golang.org/genproto/googleapis/api/annotations"`.
  - For each message: if `(google.api.resource)` option is present, set
    `msg.ResourcePattern` to `rd.GetPattern()[0]` (first pattern).
  - For each field: if `(google.api.field_behavior)` includes `OUTPUT_ONLY`,
    set `fi.IsOutputOnly = true`.

- **FR-004**: Update `cmd/protoc-gen-storage/render.go`:
  - When `msg.ResourcePattern != ""`, add to the generated file's import block:
    `"github.com/infobloxopen/devedge-sdk/persistence/resourcename"`.
  - After the column map, emit:
    ```go
    const <Msg>NamePattern = "<pattern>"
    func Format<Msg>Name(id string) string {
        name, _ := resourcename.Format(<Msg>NamePattern, map[string]string{"<idvar>": id})
        return name
    }
    func Parse<Msg>Name(name string) (string, error) {
        return resourcename.IDFromName(<Msg>NamePattern, name)
    }
    ```
    where `<idvar>` is the last variable name in the pattern (e.g., `"widget"`
    for `"widgets/{widget}"`).
  - In `toModel_<Msg>`: skip fields where `IsOutputOnly = true` (they are computed,
    not persisted). Do not copy them from proto to model.
  - In `fromModel_<Msg>`: after setting the `id` field, if `msg.ResourcePattern != ""`
    and there is a field with `IsOutputOnly = true` and `Name == "name"`, emit:
    `p.Name = Format<Msg>Name(m.ID)`.
  - In `<Msg>Columns` map: skip `IsOutputOnly` fields (they have no DB column).

- **FR-005**: Add `render_test.go` assertions for a message with `ResourcePattern`:
  - `mustContain(t, out, "<Msg>NamePattern")`
  - `mustContain(t, out, "Format<Msg>Name")`
  - `mustContain(t, out, "Parse<Msg>Name")`
  - `mustContain(t, out, "resourcename.Format")`
  - `mustNotContain(t, out, `p.Name = p.Name`)` (output-only field not copied in toModel)

- **FR-006**: Update `testdata/toy/widgets.proto`:
  - Add `import "google/api/resource.proto"` and `import "google/api/field_behavior.proto"`.
  - Add `option (google.api.resource) = { type: "toy.example.com/Widget" pattern: "widgets/{widget}" }` on Widget.
  - Rename field `name = 2` to `display_name = 3`; shift color→4, weight→5, etag→6.
  - Add `string name = 1 [(google.api.field_behavior) = OUTPUT_ONLY]`.

- **FR-007**: Update `testdata/apikey/apikey.proto`:
  - Add `import "google/api/resource.proto"` and `import "google/api/field_behavior.proto"`.
  - Add `option (google.api.resource) = { type: "apikey.example.com/APIKey" pattern: "apikeys/{api_key}" }` on APIKey.
  - Rename field `name = 2` to `label = 6`.
  - Add `string name = 1 [(google.api.field_behavior) = OUTPUT_ONLY]`.

- **FR-008**: Regenerate all testdata. `make build && make test` clean.

- **FR-009**: Unit tests for `persistence/resourcename` covering:
  - Flat pattern parse + format round-trip.
  - Hierarchical pattern parse + format.
  - `IDFromName` returns the last variable.
  - Mismatched segment count → error.
  - Literal segment mismatch → error.
  - Missing variable in Format → error.

## Success Criteria

- **SC-001**: `go test ./persistence/resourcename/...` passes.
- **SC-002**: Generated `widgets.storage.go` contains `WidgetNamePattern`, `FormatWidgetName`, `ParseWidgetName`, and `p.Name = FormatWidgetName(m.ID)`.
- **SC-003**: `make build && make test` clean.
