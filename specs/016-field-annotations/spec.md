# Feature Specification: infoblox.field.v1 — field annotation contract

**Feature Branch**: `016-field-annotations`
**Created**: 2026-06-11
**Status**: Draft

## Context

`(infoblox.authz.v1.field).secret` was added in F013 as a quick path to secret
field support, but it belongs in the wrong package. Authorization (`authz`) is
about *who can do what*; field sensitivity and storage modeling are *data*
concerns. The coupling means `middleware/redact`, `seccheck`, and both codegen
plugins import an authz package for non-authz behavior.

Additionally, the framework has no way to declare field-level storage properties
— constraints (`NOT NULL`, `UNIQUE`, `INDEX`), column overrides, or relationships
(`HasOne`, `HasMany`, `BelongsTo`, `ManyToMany`). `protoc-gen-storage` and
`protoc-gen-ent` skip message and repeated fields with TODO comments because
there is no contract for them.

This feature does two things:

1. **Creates `infoblox.field.v1`** — a purpose-built proto package for field-level
   data modeling: sensitivity, storage constraints, and relationships.
2. **Removes `FieldRule` from `authz.proto`** with no deprecation. Both packages
   are in alpha; there is nothing to preserve.

The result: `authz` is authz. `field` is field modeling. Every codegen plugin and
middleware reads from the right package.

## Clarifications

- **Extension number 50003** for `(infoblox.field.v1.opts)` on
  `google.protobuf.FieldOptions`. Extension 50002 (now freed from authz) is not
  reused — clean break, clean numbers.
- **`authz.proto` change**: `FieldRule` message and `extend google.protobuf.FieldOptions`
  block removed entirely. The `authz.pb.go` regenerated. Released as
  `infobloxopen/apis authz v1.0.0-alpha.4`.
- **`field.proto` new release**: `infoblox.field.v1` released as
  `infobloxopen/apis field v1.0.0-alpha.1`.
- **`internal/testpb/secretpb`**: updated to use `(infoblox.field.v1.opts).secret`.
- **Relationship messages** map to both GORM associations and ent edges:
  - `HasOne` — FK on the associated table; proto field is a message type.
  - `HasMany` — FK on the associated table; proto field is `repeated message`.
  - `BelongsTo` — FK on this table; proto field is a message type (or a string ID).
  - `ManyToMany` — join table; proto field is `repeated message`.
- **`protoc-gen-storage`**: reads `(infoblox.field.v1.opts)` for secret (existing
  behavior, just re-pointed), constraints (`not_null`, `unique`, `index`,
  `column_name`, `column_type`), and relationships (generate GORM `gorm:"..."` tags
  for associations). Relationship fields were previously TODO-commented; now
  they generate real GORM association fields.
- **`protoc-gen-ent`**: reads same opts; relationship fields generate ent `Edges()`.
- **`middleware/redact`** and **`seccheck`**: re-pointed from `authzv1.E_Field`
  to `fieldv1.E_Opts`. No behavior change.
- **`testdata/apikey/apikey.proto`**: `key_value` annotation changes from
  `(infoblox.authz.v1.field).secret = true` to
  `(infoblox.field.v1.opts) = {secret: true}`.

## Requirements

- **FR-001**: `authz.proto` MUST NOT contain `FieldRule`, `field = 50002`, or
  any `google.protobuf.FieldOptions` extension. Released as `authz v1.0.0-alpha.4`.
- **FR-002**: New `proto/infoblox/field/v1/field.proto` MUST define `FieldOptions`
  with: `bool secret`, `bool not_null`, `bool unique`, `bool index`,
  `string column_name`, `string column_type`, `HasOne has_one`, `HasMany has_many`,
  `BelongsTo belongs_to`, `ManyToMany many_to_many`. Released as `field v1.0.0-alpha.1`.
- **FR-003**: `HasOne/HasMany/BelongsTo/ManyToMany` messages MUST have at minimum:
  `string foreign_key`, `string association_foreign_key`. `ManyToMany` also has
  `string join_table`.
- **FR-004**: `middleware/redact` MUST use `fieldv1.E_Opts` (not `authzv1.E_Field`).
- **FR-005**: `seccheck.AssertNoSecretFieldsLeaked` MUST use `fieldv1.E_Opts`.
- **FR-006**: `protoc-gen-storage` MUST use `fieldv1.E_Opts` for: secret columns,
  storage constraints in GORM struct tags (`not null`, `uniqueIndex`, `index`),
  column name/type overrides, and relationship fields (generate proper GORM
  association struct fields with `gorm:"foreignKey:..."` tags).
- **FR-007**: `protoc-gen-ent` MUST use `fieldv1.E_Opts` for: secret columns,
  field constraints (`NotEmpty()`, `Unique()`), and relationship edges.
- **FR-008**: `go.mod` gains `github.com/infobloxopen/apis/proto/infoblox/field`
  dep; `authz` dep bumped to alpha.4 (no `FieldRule`).
- **FR-009**: All existing tests pass. `testdata/apikey` compiles with updated
  annotation.

## Success Criteria

- **SC-001**: `grep -r "authzv1.E_Field\|E_Field" --include="*.go"` returns no
  matches outside of the (now empty) authzpb test fixture.
- **SC-002**: `(infoblox.field.v1.opts).secret = true` on `key_value` in
  `testdata/apikey/apikey.proto` compiles and generates hash+cipher columns.
- **SC-003**: A proto field with `(infoblox.field.v1.opts) = {has_many: {foreign_key: "owner_id"}}`
  generates a real GORM association struct field (not a TODO comment).
- **SC-004**: `go build ./... && make test` clean.
