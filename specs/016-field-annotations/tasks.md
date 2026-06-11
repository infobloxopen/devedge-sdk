# Tasks: infoblox.field.v1 field annotation contract

**Branch**: `016-field-annotations`

---

- [X] T001 [S] Create `proto/infoblox/field/v1/field.proto` (FR-002/003) —
  the mirror copy in devedge-sdk. Also create the canonical version in
  `/Users/dgarcia/go/src/github.com/infobloxopen/apis/proto/infoblox/field/v1/field.proto`.
  Content:
  ```proto
  syntax = "proto3";
  package infoblox.field.v1;
  import "google/protobuf/descriptor.proto";
  option go_package = "github.com/infobloxopen/apis/proto/infoblox/field/v1;fieldv1";

  message FieldOptions {
    bool   secret      = 1;
    bool   not_null    = 2;
    bool   unique      = 3;
    bool   index       = 4;
    string column_name = 5;
    string column_type = 6;
    HasOne      has_one      = 20;
    HasMany     has_many     = 21;
    BelongsTo   belongs_to   = 22;
    ManyToMany  many_to_many = 23;
  }
  message HasOne     { string foreign_key = 1; string association_foreign_key = 2; }
  message HasMany    { string foreign_key = 1; string association_foreign_key = 2; string position_field = 3; }
  message BelongsTo  { string foreign_key = 1; string association_foreign_key = 2; }
  message ManyToMany { string join_table = 1; string foreign_key = 2; string association_foreign_key = 3; }

  extend google.protobuf.FieldOptions {
    FieldOptions opts = 50003;
  }
  ```
  Generate Go bindings for both copies. Run `go build ./...`.

- [X] T002 [S] Remove `FieldRule`, `extend google.protobuf.FieldOptions { FieldRule field = 50002; }` from:
  - `proto/infoblox/authz/v1/authz.proto` (mirror in devedge-sdk)
  - `/Users/dgarcia/go/src/github.com/infobloxopen/apis/proto/infoblox/authz/v1/authz.proto` (canonical)
  Regenerate `authzpb` Go bindings. Run `buf generate --template buf.gen.yaml` and
  regenerate the canonical repo's `authz.pb.go`. Run `go build ./...` (expect failures
  in redact/seccheck/codegen — those get fixed in T004-T007).

- [X] T003 [S] Add `infoblox.field.v1` to `go.mod`:
  - Cut `field v1.0.0-alpha.1` via apx in the canonical repo
  - Cut `authz v1.0.0-alpha.4` (FieldRule removed) via apx
  - `go get github.com/infobloxopen/apis/proto/infoblox/field@v1.0.0-alpha.1`
  - `go get github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.4`
  - `go mod tidy`

- [X] T004 [S] Update `middleware/redact/redact.go` (FR-004):
  Replace import `authzv1 "github.com/infobloxopen/apis/proto/infoblox/authz/v1"`
  with `fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"`.
  Replace `authzv1.E_Field` → `fieldv1.E_Opts`.
  Replace `rule.GetSecret()` call — the new extension returns `*fieldv1.FieldOptions`;
  check `opts.GetSecret()`.
  Run `go test ./middleware/redact/... -count=1`.

- [X] T005 [S] Update `seccheck/seccheck.go` `AssertNoSecretFieldsLeaked` (FR-005):
  Same import swap as T004. Run `go test ./seccheck/... -count=1`.

- [X] T006 [S] Update `internal/testpb/secretpb/test.proto` to use
  `(infoblox.field.v1.opts) = {secret: true}` instead of
  `(infoblox.authz.v1.field).secret = true`. Regenerate the pb.go.
  Run `go build ./internal/...`.

- [X] T007 [S] Update `cmd/protoc-gen-storage/main.go` (FR-006):
  Replace `authzv1.E_Field` → `fieldv1.E_Opts`.
  Add constraint support in `render.go`: when `opts.NotNull`, add `not null` to
  GORM tag; when `opts.Unique`, add `uniqueIndex`; when `opts.Index`, add `index`;
  when `opts.ColumnName != ""`, use it; when `opts.ColumnType != ""`, add `type:...`.
  Add relationship support in `render.go`: for message-kind fields with `has_one`,
  `has_many`, `belongs_to`, or `many_to_many` set, emit a real GORM struct field
  with the appropriate `gorm:"foreignKey:..."` tag instead of a TODO comment.
  Update render_test.go with new test cases. Run `go test ./cmd/protoc-gen-storage/... -count=1`.

- [X] T008 [S] Update `cmd/protoc-gen-ent/main.go` (FR-007):
  Replace `authzv1.E_Field` → `fieldv1.E_Opts`.
  Add constraint support: `field.String(...).NotEmpty()` for `not_null`;
  `.Unique()` for `unique`; index annotation for `index`.
  Add edge support: when `has_one`/`has_many`/`belongs_to`/`many_to_many` set
  on a message field, emit an `Edges()` entry in the schema instead of a TODO.
  Update render_test.go. Run `go test ./cmd/protoc-gen-ent/... -count=1`.

- [X] T009 [S] Update `testdata/apikey/apikey.proto` (FR-009):
  Change `key_value = 4 [(infoblox.authz.v1.field).secret = true]`
  to `key_value = 4 [(infoblox.field.v1.opts) = {secret: true}]`.
  Add `import "infoblox/field/v1/field.proto"`. Remove authz field import if no
  longer needed. Update `testdata/apikey/go.mod` to add the field dep + bump authz.
  Regenerate: `make build && buf generate --template buf.gen.apikey.yaml`.
  `go generate ./ent/...` in testdata/apikey.
  Run `cd testdata/apikey && go build ./... && go test ./... -count=1`.

- [X] T010 [S] `go build ./... && make test` — clean (SC-004).
  `grep -r "authzv1\.E_Field\|\.E_Field" --include="*.go" .` → no matches outside
  authzpb internal (SC-001).

- [X] T011 [S] Commit + merge.
  Message: `016: infoblox.field.v1 — field annotation contract; remove FieldRule from authz`.

## Complexity Tags

All [S] — each task is a targeted find-and-replace or mechanical addition.
The relationship support in T007/T008 is the most LOC but follows established patterns.
