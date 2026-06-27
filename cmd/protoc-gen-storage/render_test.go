package main

import (
	"go/format"
	"strings"
	"testing"

	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// T002: unit tests for renderStorageFile — pure function, no protogen/buf needed.

func TestRenderStorageFile_basic(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Widget",
		PbPkgName:    "widgetsv1",
		PbImportPath: "github.com/example/widgets/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
			{Name: "weight", GoType: "int32", SnakeName: "weight"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg}, nil)

	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package widgetsv1storage")
	mustContain(t, out, "type WidgetModel struct")
	mustContain(t, out, `gorm:"primaryKey`)
	mustContain(t, out, `gorm:"column:etag"`)
	mustContain(t, out, "ETag")
	mustContain(t, out, "CreatedAt")
	mustContain(t, out, "UpdatedAt")
	// F020: no delete_time field → no DeletedAt column, no ShowDeleted, no real Undelete.
	mustNotContain(t, out, "gorm.DeletedAt")
	mustNotContain(t, out, "opts.ShowDeleted")
	mustNotContain(t, out, "deleted_at IS NOT NULL")
	// Stub Undelete is emitted to satisfy the persistence.Repository interface.
	mustContain(t, out, "func (r *WidgetRepository) Undelete(")
	// Hard delete uses Unscoped().
	mustContain(t, out, "Unscoped()")
	mustContain(t, out, "type WidgetRepository struct")
	mustContain(t, out, "NewWidgetRepository")
	mustContain(t, out, "persistence.Repository")
	mustContain(t, out, "func (r *WidgetRepository) Get(")
	mustContain(t, out, "func (r *WidgetRepository) List(")
	mustContain(t, out, "func (r *WidgetRepository) Create(")
	mustContain(t, out, "func (r *WidgetRepository) Update(")
	mustContain(t, out, "func (r *WidgetRepository) Delete(")
	// F026: batch methods (AIP-137) generated; repo satisfies BatchRepository.
	mustContain(t, out, "func (r *WidgetRepository) BatchGet(ctx context.Context, keys []string) ([]*widgetsv1.Widget, error)")
	mustContain(t, out, "func (r *WidgetRepository) BatchUpdate(ctx context.Context, items []persistence.BatchUpdateItem[*widgetsv1.Widget, string])")
	mustContain(t, out, "func (r *WidgetRepository) BatchDelete(ctx context.Context, keys []string) error")
	mustContain(t, out, "r.db.Transaction(func(tx *gorm.DB) error")
	mustContain(t, out, "var _ persistence.BatchRepository")
	mustContain(t, out, "protoc-gen-storage")
	// F017: safe filter/order_by — column map and safe parse calls must be present.
	mustContain(t, out, "WidgetColumns")
	mustContain(t, out, "filter.Parse")
	mustContain(t, out, "filter.ParseOrderBy")
	mustNotContain(t, out, "q.Where(opts.Filter)")
	// F018: field mask must be translated to DB columns, not passed raw.
	mustNotContain(t, out, "q.Select(fieldMask)")
	mustContain(t, out, "Columns[f]")
}

func TestRenderStorageFile_resourceName(t *testing.T) {
	msg := messageInfo{
		MessageName:     "Widget",
		ResourcePattern: "widgets/{widget}",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name", IsOutputOnly: true},
			{Name: "display_name", GoType: "string", SnakeName: "display_name"},
		},
	}
	out := renderStorageFile("widgetsv1", []messageInfo{msg}, nil)

	mustContain(t, out, "WidgetNamePattern")
	mustContain(t, out, `"widgets/{widget}"`)
	mustContain(t, out, "FormatWidgetName")
	mustContain(t, out, "ParseWidgetName")
	mustContain(t, out, "resourcename.Format")
	mustContain(t, out, "resourcename.IDFromName")
	mustContain(t, out, "p.Name = FormatWidgetName(m.ID)")
	// output-only field must not appear in the GORM model struct or toModel.
	mustNotContain(t, out, "m.Name = p.Name")
}

func TestRenderStorageFile_repeatedFieldSkipped(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Foo",
		PbPkgName:    "foov1",
		PbImportPath: "example/foo",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "tags", GoType: "string", SnakeName: "tags", IsRepeated: true},
		},
	}
	out := renderStorageFile("foov1storage", []messageInfo{msg}, nil)
	mustContain(t, out, "TODO: repeated field tags skipped")
}

func TestRenderStorageFile_messageFieldSkipped(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Bar",
		PbPkgName:    "barv1",
		PbImportPath: "example/bar",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "meta", GoType: "*SomeMeta", SnakeName: "meta", IsMessage: true},
		},
	}
	out := renderStorageFile("barv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, "TODO: nested message meta skipped")
}

func TestRenderStorageFile_noMessages(t *testing.T) {
	out := renderStorageFile("emptystorage", nil, nil)
	if out != "" {
		t.Fatalf("expected empty output for no messages, got:\n%s", out)
	}
}

func TestRenderStorageFile_secretField(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Credential",
		PbPkgName:    "credv1",
		PbImportPath: "example/cred/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "label", GoType: "string", SnakeName: "label"},
			{Name: "api_key", GoFieldName: "ApiKey", GoType: "string", SnakeName: "api_key", IsSecret: true},
		},
	}
	out := renderStorageFile("credv1storage", []messageInfo{msg}, nil)

	// Secret import must be present.
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/secret"`)

	// Hash and cipher columns must be present; raw column must NOT be present.
	mustContain(t, out, `ApiKeyHash`)
	mustContain(t, out, `ApiKeyCipher`)
	mustContain(t, out, `column:api_key_hash;index`)
	mustContain(t, out, `column:api_key_cipher`)
	mustNotContain(t, out, "`gorm:\"column:api_key\"`")

	// Constructor must take enc secret.Encryptor.
	mustContain(t, out, "func NewCredentialRepository(db *gorm.DB, enc secret.Encryptor)")

	// Repo struct must have enc field.
	mustContain(t, out, "enc secret.Encryptor")

	// Create/Update must contain hash and encrypt calls.
	mustContain(t, out, "r.enc.Hash(ctx, entity.ApiKey)")
	mustContain(t, out, "r.enc.Encrypt(ctx, entity.ApiKey)")

	// toModel and fromModel must NOT reference the raw ApiKey field.
	mustNotContain(t, out, "m.ApiKey = p.ApiKey")
	mustNotContain(t, out, "p.ApiKey = m.ApiKey")

	// Non-secret field must still be present normally.
	mustContain(t, out, `gorm:"column:label"`)
}

func TestRenderStorageFile_noSecretNoImport(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Plain",
		PbPkgName:    "plainv1",
		PbImportPath: "example/plain/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "value", GoType: "string", SnakeName: "value"},
		},
	}
	out := renderStorageFile("plainv1storage", []messageInfo{msg}, nil)

	// No secret import when no secret fields.
	mustNotContain(t, out, `"github.com/infobloxopen/devedge-sdk/secret"`)

	// Constructor must NOT take enc.
	mustContain(t, out, "func NewPlainRepository(db *gorm.DB)")
	mustNotContain(t, out, "enc secret.Encryptor")
}

// T001: tenant isolation tests.

func TestRenderStorageFile_tenantIsolation(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Record",
		PbPkgName:    "recordv1",
		PbImportPath: "example/record/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "value", GoType: "string", SnakeName: "value"},
		},
	}
	out := renderStorageFile("recordv1storage", []messageInfo{msg}, nil)

	// Middleware import must be present when account_id field exists.
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/middleware"`)

	// TenantIDFromContext must appear in List, Get, Update, Delete.
	mustContain(t, out, "TenantIDFromContext")

	// Tenant WHERE clause must be present.
	mustContain(t, out, `"account_id = ?"`)
}

func TestRenderStorageFile_noTenantWhenNoAccountID(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Simple",
		PbPkgName:    "simplev1",
		PbImportPath: "example/simple/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("simplev1storage", []messageInfo{msg}, nil)

	// No middleware import when no account_id field and no secret fields.
	mustNotContain(t, out, `"github.com/infobloxopen/devedge-sdk/middleware"`)
	mustNotContain(t, out, "TenantIDFromContext")
	mustNotContain(t, out, `"account_id = ?"`)
}

// T002: LookupByHash tests.

func TestRenderStorageFile_lookupByHash(t *testing.T) {
	msg := messageInfo{
		MessageName:  "KeyValue",
		PbPkgName:    "kvv1",
		PbImportPath: "example/kv/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "key_value", GoFieldName: "KeyValue", GoType: "string", SnakeName: "key_value", IsSecret: true},
		},
	}
	out := renderStorageFile("kvv1storage", []messageInfo{msg}, nil)

	// LookupByKeyValueHash method must be present.
	mustContain(t, out, "func (r *KeyValueRepository) LookupByKeyValueHash(")

	// Must check for empty hash.
	mustContain(t, out, "persistence.ErrNotFound")

	// Must query on key_value_hash column.
	mustContain(t, out, "key_value_hash = ?")

	// Middleware import must be present (secret fields trigger it).
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/middleware"`)
}

func TestRenderStorageFile_lookupByHashWithTenant(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Secret",
		PbPkgName:    "secretv1",
		PbImportPath: "example/secret/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "token", GoFieldName: "Token", GoType: "string", SnakeName: "token", IsSecret: true},
		},
	}
	out := renderStorageFile("secretv1storage", []messageInfo{msg}, nil)

	// LookupByTokenHash must be present.
	mustContain(t, out, "func (r *SecretRepository) LookupByTokenHash(")

	// Tenant filter must also appear inside LookupByTokenHash.
	mustContain(t, out, "token_hash = ?")
	mustContain(t, out, "TenantIDFromContext")
	mustContain(t, out, `"account_id = ?"`)
}

// T007: constraint and relationship field tests.

func TestRenderStorageFile_notNullField(t *testing.T) {
	msg := messageInfo{
		MessageName: "Thing",
		PbPkgName:   "thingv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name", NotNull: true},
		},
	}
	out := renderStorageFile("thingv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, `gorm:"column:name;not null"`)
}

func TestRenderStorageFile_uniqueField(t *testing.T) {
	msg := messageInfo{
		MessageName: "Uniq",
		PbPkgName:   "uniqv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "email", GoType: "string", SnakeName: "email", Unique: true},
		},
	}
	out := renderStorageFile("uniqv1storage", []messageInfo{msg}, nil)
	// No tenant column → a plain global unique index is correct.
	mustContain(t, out, "uniqueIndex")
	mustNotContain(t, out, "priority:")
}

// Issue 014: in a tenant-scoped message, `unique` must produce a composite
// unique index over (account_id, <field>), not a global one — otherwise one
// tenant can deny another the use of a name and probe its existence.
func TestRenderStorageFile_uniqueFieldIsPerTenant(t *testing.T) {
	msg := messageInfo{
		MessageName: "Destination",
		PbPkgName:   "destv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "name", GoFieldName: "Name", GoType: "string", SnakeName: "name", Unique: true, NotNull: true},
		},
	}
	out := renderStorageFile("destv1", []messageInfo{msg}, nil)
	// The unique field and account_id share one composite index name, with
	// account_id as the leading column.
	mustContain(t, out, "uniqueIndex:ux_destination_account_name,priority:2")
	mustContain(t, out, "uniqueIndex:ux_destination_account_name,priority:1")
	// Never a bare/global unique index on the tenant-scoped field.
	mustNotContain(t, out, "column:name;not null;uniqueIndex\"")
}

// #49 follow-up: on a soft-delete + per-tenant-unique resource the unique key
// must be re-creatable after the holder is soft-deleted.
func softDeleteUniqueStorageMsg() messageInfo {
	return messageInfo{
		MessageName: "Order",
		PbPkgName:   "orderv1",
		SoftDelete:  true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "source_ref", GoFieldName: "SourceRef", GoType: "string", SnakeName: "source_ref", Unique: true},
		},
	}
}

// Default dialect (postgres/sqlite): the composite unique is PARTIAL
// (WHERE deleted_at IS NULL) — no discriminator column.
func TestRenderStorageFile_softDeleteUnique_partial(t *testing.T) {
	targetDialect = "postgres"
	out := renderStorageFile("orderv1", []messageInfo{softDeleteUniqueStorageMsg()}, nil)
	// The partial predicate rides the GORM index `option` (the `where` tag is
	// dropped by GORM's migrator), on the leading account_id (priority 1) tag.
	mustContain(t, out, "uniqueIndex:ux_order_account_source_ref,priority:1,option:WHERE deleted_at IS NULL")
	mustNotContain(t, out, "soft_delete_key")
}

// dialect=mysql: a soft_delete_key column joins the composite (priority 3),
// Delete stamps the row id into it, Undelete clears it; no partial clause.
func TestRenderStorageFile_softDeleteUnique_sentinel(t *testing.T) {
	targetDialect = "mysql"
	defer func() { targetDialect = "postgres" }()
	out := renderStorageFile("orderv1", []messageInfo{softDeleteUniqueStorageMsg()}, nil)
	mustContain(t, out, "column:soft_delete_key")
	mustContain(t, out, "uniqueIndex:ux_order_account_source_ref,priority:3")
	mustContain(t, out, `"soft_delete_key": key`)  // Delete stamps the id
	mustContain(t, out, `"soft_delete_key": ""`)    // Undelete clears it
	mustNotContain(t, out, "option:WHERE deleted_at IS NULL")
}

// A per-tenant unique WITHOUT soft-delete is unchanged on every dialect.
func TestRenderStorageFile_uniqueNoSoftDelete_unchanged(t *testing.T) {
	msg := softDeleteUniqueStorageMsg()
	msg.SoftDelete = false
	for _, d := range []string{"postgres", "mysql"} {
		targetDialect = d
		out := renderStorageFile("orderv1", []messageInfo{msg}, nil)
		mustContain(t, out, "uniqueIndex:ux_order_account_source_ref,priority:2")
		mustNotContain(t, out, "soft_delete_key")
		mustNotContain(t, out, "option:WHERE deleted_at IS NULL")
	}
	targetDialect = "postgres"
}

// Issue 017: generated Create/Update must translate driver constraint errors
// to clean persistence sentinels (so a unique violation becomes AlreadyExists,
// not a 500 leaking raw SQL).
func TestRenderStorageFile_mapsConstraintErrors(t *testing.T) {
	msg := messageInfo{
		MessageName: "Destination",
		PbPkgName:   "destv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoFieldName: "Name", GoType: "string", SnakeName: "name", Unique: true},
		},
	}
	out := renderStorageFile("destv1", []messageInfo{msg}, nil)
	// At least the Create and the field-mask + no-mask Update paths are guarded.
	if n := strings.Count(out, "persistence.ConstraintError(err)"); n < 3 {
		t.Errorf("expected ConstraintError check on Create + both Update paths (>=3), got %d", n)
	}
}

func TestRenderStorageFile_hasOneMessageField(t *testing.T) {
	msg := messageInfo{
		MessageName: "Order",
		PbPkgName:   "orderv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "address", GoFieldName: "Address", RelatedGoType: "Address", SnakeName: "address",
				IsMessage: true, HasOne: &fieldv1.HasOne{ForeignKey: "order_id"}},
		},
	}
	out := renderStorageFile("orderv1storage", []messageInfo{msg}, nil)
	// Should emit a concrete pointer association to the related GORM model
	// (issue 013: never interface{}), with a Go-name foreign key.
	mustNotContain(t, out, "TODO: nested message address skipped")
	mustContain(t, out, `Address *AddressModel`)
	mustContain(t, out, `foreignKey:OrderId`)
	mustNotContain(t, out, "Address interface{}")
}

func TestRenderStorageFile_hasManyRepeatedField(t *testing.T) {
	msg := messageInfo{
		MessageName: "Post",
		PbPkgName:   "postv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "comments", GoFieldName: "Comments", RelatedGoType: "Comment", SnakeName: "comments",
				IsRepeated: true, HasMany: &fieldv1.HasMany{ForeignKey: "post_id"}},
		},
	}
	out := renderStorageFile("postv1storage", []messageInfo{msg}, nil)
	// Should emit a slice of the concrete related model, not []interface{}.
	mustNotContain(t, out, "TODO: repeated field comments skipped")
	mustContain(t, out, `Comments []*CommentModel`)
	mustContain(t, out, `foreignKey:PostId`)
	mustNotContain(t, out, "Comments []interface{}")
}

// Issue 013: a belongs_to whose proto ALSO exposes the FK as a scalar field
// (the docs' own Order shape) must not emit a duplicate FK field — that fails
// to compile. The scalar field provides the column; the association reuses it.
func TestRenderStorageFile_belongsToDedupesScalarFK(t *testing.T) {
	msg := messageInfo{
		MessageName: "Export",
		PbPkgName:   "exportsv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "destination_id", GoFieldName: "DestinationId", GoType: "string", SnakeName: "destination_id"},
			{Name: "destination", GoFieldName: "Destination", RelatedGoType: "Destination", SnakeName: "destination",
				IsMessage: true, BelongsTo: &fieldv1.BelongsTo{ForeignKey: "destination_id"}},
		},
	}
	out := renderStorageFile("exportsv1", []messageInfo{msg}, nil)
	// Concrete association, keyed by the existing scalar FK's Go field name.
	mustContain(t, out, `Destination *DestinationModel`)
	mustContain(t, out, `foreignKey:DestinationId`)
	// The scalar FK column appears exactly once (no auto-emitted duplicate).
	if n := strings.Count(out, "\tDestinationId "); n != 1 {
		t.Errorf("expected exactly one DestinationId field, got %d\n--- output ---\n%s", n, out)
	}
	mustNotContain(t, out, "Destination interface{}")
}

// Issue 013: a belongs_to WITHOUT a sibling scalar FK still emits the FK column
// (with a gorm column tag) so GORM can resolve the association.
func TestRenderStorageFile_belongsToEmitsFKWhenNoScalar(t *testing.T) {
	msg := messageInfo{
		MessageName: "Export",
		PbPkgName:   "exportsv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "destination", GoFieldName: "Destination", RelatedGoType: "Destination", SnakeName: "destination",
				IsMessage: true, BelongsTo: &fieldv1.BelongsTo{ForeignKey: "destination_id"}},
		},
	}
	out := renderStorageFile("exportsv1", []messageInfo{msg}, nil)
	mustContain(t, out, `Destination *DestinationModel`)
	mustContain(t, out, "DestinationId string `gorm:\"column:destination_id\"`")
}

// F020: soft-delete tests (AC-001 through AC-007).

func TestRenderStorageFile_softDelete(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		SoftDelete:  true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg}, nil)

	// AC-001: soft-delete model carries gorm.DeletedAt.
	mustContain(t, out, "gorm.DeletedAt")

	// AC-002: full Undelete implementation (not a stub).
	mustContain(t, out, "func (r *WidgetRepository) Undelete(")
	mustContain(t, out, "Unscoped()")
	mustContain(t, out, "deleted_at IS NOT NULL")
	mustContain(t, out, `Update("deleted_at", nil)`)
	mustContain(t, out, "RowsAffected == 0")
	mustContain(t, out, "persistence.ErrNotFound")

	// AC-003: soft Delete does NOT chain Unscoped before Delete.
	mustNotContain(t, out, "Unscoped().Delete(")

	// AC-004: List has ShowDeleted branch.
	mustContain(t, out, "opts.ShowDeleted")
	mustContain(t, out, "q.Unscoped()")

	// AC-005: fromModel populates delete_time.
	mustContain(t, out, "m.DeletedAt.Valid")
	mustContain(t, out, "timestamppb.New(m.DeletedAt.Time)")
	mustNotContain(t, out, "m.DeleteTime = ") // OUTPUT_ONLY: toModel never copies it

	// AC-007: column map includes delete_time.
	mustContain(t, out, `"delete_time": "deleted_at"`)

	// timestamppb import present.
	mustContain(t, out, "timestamppb")
}

func TestRenderStorageFile_expireTime(t *testing.T) {
	msg := messageInfo{
		MessageName:   "APIKey",
		SoftDelete:    true,
		HasExpireTime: true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "label", GoType: "string", SnakeName: "label"},
		},
	}
	out := renderStorageFile("apikeyv1storage", []messageInfo{msg}, nil)

	// Model has both DeletedAt and ExpireTime.
	mustContain(t, out, "gorm.DeletedAt")
	mustContain(t, out, "ExpireTime sql.NullTime")
	mustContain(t, out, "column:expire_time")

	// fromModel populates expire_time.
	mustContain(t, out, "m.ExpireTime.Valid")
	mustContain(t, out, "timestamppb.New(m.ExpireTime.Time)")

	// AC-007: column map includes expire_time.
	mustContain(t, out, `"expire_time": "expire_time"`)

	// PurgeExpired emitted.
	mustContain(t, out, "func (r *APIKeyRepository) PurgeExpired(")
	mustContain(t, out, "expire_time IS NOT NULL AND expire_time <= ?")
	mustContain(t, out, "res.RowsAffected")

	// #34: the cutoff is normalized to UTC so a seam-stamped (UTC) expire_time is
	// reaped even when the caller passes a local-zone time on SQLite.
	mustContain(t, out, "expire_time <= ?\", before.UTC()")

	// database/sql import present.
	mustContain(t, out, `"database/sql"`)

	// #27: toModel carries expire_time so a Create handler that stamps the
	// OUTPUT_ONLY TTL actually persists it (otherwise PurgeExpired reaps nothing).
	mustContain(t, out, "if p.ExpireTime != nil {")
	mustContain(t, out, "m.ExpireTime = sql.NullTime{Time: p.ExpireTime.AsTime(), Valid: true}")
}

// Regression for issue #33: a message with an `etag` field must bridge the model
// ETag column in fromModel and stamp a fresh token on every write — otherwise the
// proto ETag is always empty and the documented If-Match/412 flow is inert.
func TestRenderStorageFile_etagBridgedAndStamped(t *testing.T) {
	msg := messageInfo{
		MessageName: "Doc",
		HasETag:     true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "title", GoType: "string", SnakeName: "title"},
		},
	}
	out := renderStorageFile("docv1storage", []messageInfo{msg}, nil)

	// fromModel surfaces the stored ETag.
	mustContain(t, out, "p.Etag = m.ETag")
	// Create and Update stamp a fresh token.
	mustContain(t, out, "m.ETag = etag.New()")
	// A no-mask update writes the etag column via the updates map.
	mustContain(t, out, `updates["etag"] = m.ETag`)
	// A masked update still bumps the etag column.
	mustContain(t, out, `dbCols = append(dbCols, "etag")`)
	// The etag package is imported.
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/middleware/etag"`)
}

// A message WITHOUT an etag field must not stamp or import etag (no behavior change).
func TestRenderStorageFile_noETagNoStamp(t *testing.T) {
	msg := messageInfo{
		MessageName: "Plain",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("plainv1storage", []messageInfo{msg}, nil)
	mustNotContain(t, out, "etag.New()")
	mustNotContain(t, out, "p.Etag = m.ETag")
	mustNotContain(t, out, "middleware/etag")
	// And no AIP-154 compare-and-set machinery on a non-etag resource.
	mustNotContain(t, out, "IfMatchFromContext")
	mustNotContain(t, out, "ErrPreconditionFailed")
}

// AIP-154 optimistic concurrency: an etag-bearing resource's Update must become a
// compare-and-set when an If-Match precondition is present — the UPDATE is narrowed
// by `etag = <if-match>` and a 0-affected result is resolved to
// ErrPreconditionFailed (row still present) or ErrNotFound (row gone). A stale
// If-Match on a SQL backend used to silently succeed; this test pins the emission.
func TestRenderStorageFile_etagCompareAndSet(t *testing.T) {
	msg := messageInfo{
		MessageName: "Doc",
		HasETag:     true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "title", GoType: "string", SnakeName: "title"},
		},
	}
	out := renderStorageFile("docv1storage", []messageInfo{msg}, nil)

	// The If-Match precondition is read and, when present, narrows the UPDATE.
	mustContain(t, out, "ifMatch := etag.IfMatchFromContext(ctx)")
	mustContain(t, out, `q = q.Where("etag = ?", ifMatch)`)
	// A conditioned UPDATE that touched no rows is disambiguated.
	mustContain(t, out, "ifMatch != \"\" && res.RowsAffected == 0")
	mustContain(t, out, "persistence.ErrPreconditionFailed")
	mustContain(t, out, "persistence.ErrNotFound")
}

// The CAS WHERE must be tenant-scoped on a per-tenant etag resource: the existence
// re-check after a 0-affected update narrows by account_id too, so it cannot leak a
// PreconditionFailed/NotFound signal across tenants.
func TestRenderStorageFile_etagCompareAndSetTenantScoped(t *testing.T) {
	msg := messageInfo{
		MessageName: "Doc",
		HasETag:     true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "title", GoType: "string", SnakeName: "title"},
		},
	}
	out := renderStorageFile("docv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, `q = q.Where("etag = ?", ifMatch)`)
	// The precondition existence re-check carries the tenant predicate.
	mustContain(t, out, `check = check.Where("account_id = ?", tenantID)`)
}

func TestRenderStorageFile_softDeleteWithTenant(t *testing.T) {
	msg := messageInfo{
		MessageName: "Record",
		SoftDelete:  true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "value", GoType: "string", SnakeName: "value"},
		},
	}
	out := renderStorageFile("recordv1storage", []messageInfo{msg}, nil)

	// Undelete has tenant scoping BEFORE the deleted_at predicate.
	mustContain(t, out, "func (r *RecordRepository) Undelete(")
	mustContain(t, out, "TenantIDFromContext")
	mustContain(t, out, "deleted_at IS NOT NULL")

	// ShowDeleted appears before tenant WHERE in List.
	mustContain(t, out, "opts.ShowDeleted")
	mustContain(t, out, `"account_id = ?"`)

	// Soft delete: no Unscoped in the Delete method itself.
	mustNotContain(t, out, "Unscoped().Delete(")
}

// Regression for issue 011 (devedge-assessment-2026-06-16): the no-field-mask
// Update path must write a column map so zero values (false, 0, "") persist;
// a bare struct Updates would silently drop them.
func TestRenderStorageFile_updatePersistsZeroValues(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		PbPkgName:   "widgetsv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "display_name", GoFieldName: "DisplayName", GoType: "string", SnakeName: "display_name"},
			{Name: "weight", GoFieldName: "Weight", GoType: "int32", SnakeName: "weight"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg}, nil)

	// No-field-mask branch updates via a map of every writable column.
	mustContain(t, out, "updates := map[string]interface{}{")
	mustContain(t, out, `"display_name": m.DisplayName,`)
	mustContain(t, out, `"weight": m.Weight,`)
	mustContain(t, out, "q.Updates(updates)")
	// Field-mask branch keeps Select (which also forces zero-value writes).
	mustContain(t, out, "q.Select(dbCols).Updates(m)")
}

// Regression for issue 011: with secret fields, the no-field-mask map update must
// only rewrite the secret hash/cipher columns when the caller supplied a new
// value, so a non-secret update never wipes the stored secret.
func TestRenderStorageFile_updateZeroValuesPreservesSecret(t *testing.T) {
	msg := messageInfo{
		MessageName: "Cred",
		PbPkgName:   "credv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "label", GoFieldName: "Label", GoType: "string", SnakeName: "label"},
			{Name: "token", GoFieldName: "Token", GoType: "string", SnakeName: "token", IsSecret: true},
		},
	}
	out := renderStorageFile("credv1storage", []messageInfo{msg}, nil)

	mustContain(t, out, "updates := map[string]interface{}{")
	mustContain(t, out, `"label": m.Label,`)
	// Secret columns are guarded by a presence check, not unconditionally written.
	mustContain(t, out, `if entity.Token != "" {`)
	mustContain(t, out, `updates["token_hash"] = m.TokenHash`)
	mustContain(t, out, `updates["token_cipher"] = m.TokenCipher`)
	// The plaintext secret is never a column in the update map.
	mustNotContain(t, out, `"token": m.Token,`)
}

// Regression for issue #24: the tenant scoping key (account_id) must never be a
// writable column on Update. In the no-field-mask branch it must be absent from the
// updates map (otherwise an update that omits it writes account_id="" and orphans the
// row from its tenant); in the field-mask branch it must be rejected even when named
// explicitly. It must still appear as a WHERE predicate so tenant scoping is intact.
func TestRenderStorageFile_updateNeverWritesTenantKey(t *testing.T) {
	msg := messageInfo{
		MessageName: "Campaign",
		PbPkgName:   "campaignv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "subject", GoFieldName: "Subject", GoType: "string", SnakeName: "subject"},
		},
	}
	out := renderStorageFile("campaignv1storage", []messageInfo{msg}, nil)

	// No-field-mask map writes the regular column but NOT the tenant key.
	mustContain(t, out, "updates := map[string]interface{}{")
	mustContain(t, out, `"subject": m.Subject,`)
	mustNotContain(t, out, `"account_id": m.AccountId,`)

	// Field-mask branch rejects account_id even if explicitly named.
	mustContain(t, out, `if col == "account_id" {`)
	mustContain(t, out, "account_id is the tenant key and cannot be updated")

	// Tenant scoping is still enforced as a WHERE predicate.
	mustContain(t, out, `"account_id = ?"`)
}

// A map<string,string> field is the Tags kind: a single types.Tags JSONB column
// with both-direction conversions, persisted by a full update, and deliberately
// absent from the filter/order_by column map.
func TestRenderStorageFile_tagsField(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Resource",
		PbPkgName:    "resv1",
		PbImportPath: "example/res/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "label", GoFieldName: "Label", GoType: "string", SnakeName: "label"},
			{Name: "tags", GoFieldName: "Tags", SnakeName: "tags", IsTags: true},
		},
	}
	out := renderStorageFile("resv1storage", []messageInfo{msg}, nil)

	// types import + a JSONB column backed by types.Tags.
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/types"`)
	mustContain(t, out, "Tags types.Tags")
	mustContain(t, out, "column:tags;type:jsonb")
	// Conversions both directions (map <-> types.Tags).
	mustContain(t, out, "m.Tags = types.Tags(p.Tags)")
	mustContain(t, out, "p.Tags = map[string]string(m.Tags)")
	// A no-field-mask full update persists tags.
	mustContain(t, out, `"tags": m.Tags,`)
	// Never treated as a skipped nested message or repeated field.
	mustNotContain(t, out, "TODO: nested message tags skipped")
	mustNotContain(t, out, "TODO: repeated field tags skipped")
	// Present in exactly one column map — the JSON map for `tags.<key>` filtering,
	// never the scalar filter/order_by map (so `tags` isn't order-by-able and a
	// bare `tags = 'x'` can't hit a Postgres jsonb column).
	if n := strings.Count(out, `"tags": "tags",`); n != 1 {
		t.Errorf(`expected "tags" in exactly one (JSON) column map, found %d`, n)
	}
}

// A column_type annotation overrides the default jsonb on a tags column.
func TestRenderStorageFile_tagsColumnTypeOverride(t *testing.T) {
	msg := messageInfo{
		MessageName: "Resource",
		PbPkgName:   "resv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "labels", GoFieldName: "Labels", SnakeName: "labels", IsTags: true, ColumnType: "json", ColumnName: "lbls"},
		},
	}
	out := renderStorageFile("resv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, "column:lbls;type:json")
}

// No tags field → no types import.
func TestRenderStorageFile_noTagsNoImport(t *testing.T) {
	msg := messageInfo{
		MessageName: "Plain",
		PbPkgName:   "plainv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("plainv1storage", []messageInfo{msg}, nil)
	mustNotContain(t, out, `"github.com/infobloxopen/devedge-sdk/types"`)
	mustNotContain(t, out, "types.Tags")
}

// A tags field makes List dialect-aware: it emits a JSON column whitelist and
// passes it plus the live dialect to filter.Parse.
func TestRenderStorageFile_tagsFiltering(t *testing.T) {
	msg := messageInfo{
		MessageName: "Resource",
		PbPkgName:   "resv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "tags", GoFieldName: "Tags", SnakeName: "tags", IsTags: true},
		},
	}
	out := renderStorageFile("resv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, "var ResourceJSONColumns = map[string]string{")
	mustContain(t, out, `"tags": "tags",`)
	mustContain(t, out, "filter.WithJSONColumns(ResourceJSONColumns)")
	mustContain(t, out, "filter.WithDialect(r.db.Dialector.Name())")
}

// A message with no tags keeps the plain (scalar-only) filter.Parse call and
// emits no JSON column map.
func TestRenderStorageFile_noTagsPlainFilter(t *testing.T) {
	msg := messageInfo{
		MessageName: "Plain",
		PbPkgName:   "plainv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("plainv1storage", []messageInfo{msg}, nil)
	mustContain(t, out, "filter.Parse(opts.Filter, PlainColumns)")
	mustNotContain(t, out, "WithJSONColumns")
	mustNotContain(t, out, "JSONColumns")
}

// TestRenderStorageFile_multiSurface is the F027 Phase 5b gate at the render
// level: a SURFACE message (CouponSummary, Model="Coupon") projecting a subset
// of a tenant + soft-delete + secret owner (Coupon) generates:
//
//   (a) valid Go (go/format.Source gate)
//   (b) type CouponModel struct (owner struct) but NOT type CouponSummaryModel struct
//   (c) func NewCouponSummaryRepository
//   (d) func toModel_CouponSummary(...) *CouponModel and func fromModel_CouponSummary(m *CouponModel)
//   (e) surface CRUD uses CouponModel (var models []CouponModel appears for List)
//   (f) owned hook var FromModelCouponSummaryCustom
//
// Also checks that single-surface output (existing Coupon) is unaffected.
func TestRenderStorageFile_multiSurface(t *testing.T) {
	owner := messageInfo{
		MessageName: "Coupon",
		Model:       "Coupon",
		SoftDelete:  true,
		Fields: []fieldInfo{
			{Name: "id", GoFieldName: "Id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "code", GoFieldName: "Code", GoType: "string", SnakeName: "code", Unique: true, NotNull: true},
			{Name: "discount_bps", GoFieldName: "DiscountBps", GoType: "int32", SnakeName: "discount_bps"},
			{Name: "signing_key", GoFieldName: "SigningKey", GoType: "string", SnakeName: "signing_key", IsSecret: true},
		},
	}
	surface := messageInfo{
		MessageName: "CouponSummary",
		Model:       "Coupon", // a surface over Coupon — no table of its own
		Fields: []fieldInfo{
			{Name: "id", GoFieldName: "Id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "code", GoFieldName: "Code", GoType: "string", SnakeName: "code"},
		},
	}
	ownerByName := map[string]messageInfo{
		"Coupon":        owner,
		"CouponSummary": owner, // surface maps to owner
	}
	out := renderStorageFile("couponv1", []messageInfo{owner, surface}, ownerByName)
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	// (a) strongest check: valid Go syntax
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}

	// (b) owner struct present; surface struct absent
	mustContain(t, out, "type CouponModel struct")
	mustNotContain(t, out, "type CouponSummaryModel struct")

	// (c) surface constructor present
	mustContain(t, out, "func NewCouponSummaryRepository(")

	// (d) toModel/fromModel use the owner model type for the surface
	mustContain(t, out, "func toModel_CouponSummary(p *CouponSummary) *CouponModel")
	mustContain(t, out, "func fromModel_CouponSummary(m *CouponModel) *CouponSummary")

	// (e) CRUD for the surface uses CouponModel (List declares []CouponModel, BatchGet too)
	if n := strings.Count(out, "var models []CouponModel"); n < 2 {
		t.Errorf("expected var models []CouponModel at least 2 times (owner+surface List), got %d", n)
	}
	mustNotContain(t, out, "CouponSummaryModel")

	// (f) owned hook
	mustContain(t, out, "var FromModelCouponSummaryCustom func(")

	// Also check single-surface (owner) still works
	mustContain(t, out, "func NewCouponRepository(")
	mustContain(t, out, "func toModel_Coupon(p *Coupon) *CouponModel")
	mustContain(t, out, "func fromModel_Coupon(m *CouponModel) *Coupon")
}

// F030 GORM tx seam: every generated repository must emit a conn(ctx) resolver
// that returns the ctx-bound transaction *gorm.DB when one is enrolled, and the
// CRUD bodies must route through r.conn(ctx) (never r.db.WithContext directly,
// except inside the resolver itself), so a write issued inside Atomically
// participates in the surrounding transaction.
func TestRenderStorageFile_txConnResolver(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		PbPkgName:   "widgetsv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg}, nil)

	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}

	// The resolver itself.
	mustContain(t, out, "func (r *WidgetRepository) conn(ctx context.Context) *gorm.DB {")
	mustContain(t, out, "if h, ok := persistence.TxFromContext(ctx); ok {")
	mustContain(t, out, "if tx, ok := h.(*gorm.DB); ok {")
	mustContain(t, out, "return tx.WithContext(ctx)")

	// CRUD routes through r.conn(ctx).
	mustContain(t, out, "r.conn(ctx)")
	// The only r.db.WithContext(ctx) left is inside the resolver (the fallback).
	if n := strings.Count(out, "r.db.WithContext(ctx)"); n != 1 {
		t.Errorf("expected exactly one r.db.WithContext(ctx) (the resolver fallback), got %d", n)
	}
}

// F030 batch reconciliation: BatchUpdate/BatchDelete must JOIN an already-present
// ctx transaction (run directly against the ctx-tx repo, no inner db.Transaction)
// when one is enrolled, and open their own r.db.Transaction otherwise.
func TestRenderStorageFile_batchJoinsOuterTx(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		PbPkgName:   "widgetsv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg}, nil)

	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}

	// Both batch methods consult ctx for an existing *gorm.DB before opening a tx.
	if n := strings.Count(out, "if tx, ok := h.(*gorm.DB); ok {"); n < 3 {
		// conn resolver (1) + BatchUpdate join (1) + BatchDelete join (1) = 3
		t.Errorf("expected the *gorm.DB ctx check in conn + both batch methods (>=3), got %d", n)
	}
	// The own-tx fallback is still present.
	mustContain(t, out, "r.db.Transaction(func(tx *gorm.DB) error")
	// BatchDelete runs the bulk delete via a run(db) closure so it can join or open.
	mustContain(t, out, "run := func(db *gorm.DB) error {")
	mustContain(t, out, "return run(tx)")
}

// The conn resolver and batch join must also be emitted for SURFACE adapters
// (a projection over another message's model, e.g. APIKeySummaryRepository).
func TestRenderStorageFile_txConnResolverOnSurface(t *testing.T) {
	owner := messageInfo{
		MessageName: "Coupon",
		Model:       "Coupon",
		Fields: []fieldInfo{
			{Name: "id", GoFieldName: "Id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "code", GoFieldName: "Code", GoType: "string", SnakeName: "code"},
		},
	}
	surface := messageInfo{
		MessageName: "CouponSummary",
		Model:       "Coupon",
		Fields: []fieldInfo{
			{Name: "id", GoFieldName: "Id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "code", GoFieldName: "Code", GoType: "string", SnakeName: "code"},
		},
	}
	ownerByName := map[string]messageInfo{"Coupon": owner, "CouponSummary": owner}
	out := renderStorageFile("couponv1", []messageInfo{owner, surface}, ownerByName)

	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}
	// The surface adapter gets its own conn resolver.
	mustContain(t, out, "func (r *CouponSummaryRepository) conn(ctx context.Context) *gorm.DB {")
}

// fleetAggregateMessages mirrors the testdata/fleet proto shape: Fleet is an
// aggregate ROOT with a has_many Vehicles (FK fleet_id); Vehicle is its MEMBER
// with a scalar fleet_id and a belongs_to Fleet (FK fleet_id) — the inverse of
// Fleet's has_many. It is the canonical F031 containment fixture for the GORM
// generator render tests.
func fleetAggregateMessages() []messageInfo {
	fleet := messageInfo{
		MessageName:   "Fleet",
		AggregateRoot: true,
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "vehicles", GoFieldName: "Vehicles", RelatedGoType: "Vehicle", SnakeName: "vehicles",
				IsRepeated: true, HasMany: &fieldv1.HasMany{ForeignKey: "fleet_id"}},
		},
	}
	vehicle := messageInfo{
		MessageName: "Vehicle",
		MemberRoot:  "Fleet",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoFieldName: "AccountId", GoType: "string", SnakeName: "account_id"},
			{Name: "fleet_id", GoFieldName: "FleetId", GoType: "string", SnakeName: "fleet_id"},
			{Name: "fleet", GoFieldName: "Fleet", RelatedGoType: "Fleet", SnakeName: "fleet",
				IsMessage: true, BelongsTo: &fieldv1.BelongsTo{ForeignKey: "fleet_id"}},
		},
	}
	return []messageInfo{fleet, vehicle}
}

// F031 (GORM Phase 2): the aggregate root's containment has_many carries
// OnDelete:CASCADE so deleting the root row deletes its owned members at the DB
// level, while the member's belongs_to INVERSE stays uncascaded (the root's
// has_many owns the FK constraint).
func TestRenderStorageFile_aggregateCascadeOnContainmentHasMany(t *testing.T) {
	out := renderStorageFile("fleetv1", fleetAggregateMessages(), nil)
	// Root has_many → cascade (raw, pre-gofmt single-space tags).
	mustContain(t, out, "Vehicles []*VehicleModel `gorm:\"foreignKey:FleetId;constraint:OnDelete:CASCADE\"`")
	// Member belongs_to inverse → NO cascade (plain foreignKey only).
	mustContain(t, out, "Fleet *FleetModel `gorm:\"foreignKey:FleetId\"`")
	// Exactly one cascade in the whole file (only the root's has_many).
	if n := strings.Count(out, "constraint:OnDelete:CASCADE"); n != 1 {
		t.Errorf("expected exactly one cascade tag (the root has_many), got %d\n--- output ---\n%s", n, out)
	}
}

// A plain (non-aggregate) has_many / belongs_to must NOT get the cascade tag:
// cascade is gated on DDD containment markers, so it is purely additive.
func TestRenderStorageFile_noCascadeWithoutContainmentMarkers(t *testing.T) {
	msgs := fleetAggregateMessages()
	// Strip the DDD markers — same edges, no aggregate/member declaration.
	msgs[0].AggregateRoot = false
	msgs[1].MemberRoot = ""
	out := renderStorageFile("fleetv1", msgs, nil)
	mustNotContain(t, out, "constraint:OnDelete:CASCADE")
	mustNotContain(t, out, "LoadFleetAggregateGorm")
}

// F031: a cross-aggregate references link is never a containment edge, so it
// never cascades (and emits no association at all).
func TestRenderStorageFile_referencesNeverCascades(t *testing.T) {
	msg := messageInfo{
		MessageName: "Order",
		PbPkgName:   "orderv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "customer_id", GoFieldName: "CustomerId", GoType: "string", SnakeName: "customer_id"},
			{Name: "customer", GoFieldName: "Customer", RelatedGoType: "Customer", SnakeName: "customer",
				IsMessage: true, References: &dddv1.References{ForeignKey: "customer_id"}},
		},
	}
	out := renderStorageFile("orderv1", []messageInfo{msg}, nil)
	mustNotContain(t, out, "constraint:OnDelete:CASCADE")
}

// F031 (GORM Phase 2): an aggregate root with containment members gets a
// Load<Root>AggregateGorm graph-load primitive that Preloads each member edge,
// tenant-scopes (the root has account_id), maps NotFound, and explicitly projects
// each preloaded member onto the root (fromModel_<Root> ignores members).
func TestRenderStorageFile_loadAggregateEmitted(t *testing.T) {
	out := renderStorageFile("fleetv1", fleetAggregateMessages(), nil)
	mustContain(t, out, "func LoadFleetAggregateGorm(ctx context.Context, db *gorm.DB, id string) (*Fleet, error) {")
	mustContain(t, out, `q = q.Preload("Vehicles")`)
	mustContain(t, out, "if tenantID := middleware.TenantIDFromContext(ctx); tenantID != \"\" {")
	mustContain(t, out, "return nil, persistence.ErrNotFound")
	// Explicit member projection (the load primitive must append members, since
	// fromModel_Fleet does not).
	mustContain(t, out, "root.Vehicles = append(root.Vehicles, fromModel_Vehicle(mm))")
	// The member (Vehicle) is not a root, so it gets no aggregate loader.
	mustNotContain(t, out, "func LoadVehicleAggregateGorm(")
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", substr, s)
	}
}

func mustNotContain(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected output NOT to contain %q\n--- output ---\n%s", substr, s)
	}
}
