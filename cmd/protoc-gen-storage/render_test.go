package main

import (
	"strings"
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// T002: unit tests for renderStorageFile — pure function, no protogen/buf needed.

func TestRenderStorageFile_basic(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		PbPkgName:   "widgetsv1",
		PbImportPath: "github.com/example/widgets/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
			{Name: "weight", GoType: "int32", SnakeName: "weight"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg})

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
	mustContain(t, out, "var _ persistence.Repository")
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
	out := renderStorageFile("widgetsv1", []messageInfo{msg})

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
	out := renderStorageFile("foov1storage", []messageInfo{msg})
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
	out := renderStorageFile("barv1storage", []messageInfo{msg})
	mustContain(t, out, "TODO: nested message meta skipped")
}

func TestRenderStorageFile_noMessages(t *testing.T) {
	out := renderStorageFile("emptystorage", nil)
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
	out := renderStorageFile("credv1storage", []messageInfo{msg})

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
	out := renderStorageFile("plainv1storage", []messageInfo{msg})

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
	out := renderStorageFile("recordv1storage", []messageInfo{msg})

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
	out := renderStorageFile("simplev1storage", []messageInfo{msg})

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
	out := renderStorageFile("kvv1storage", []messageInfo{msg})

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
	out := renderStorageFile("secretv1storage", []messageInfo{msg})

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
	out := renderStorageFile("thingv1storage", []messageInfo{msg})
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
	out := renderStorageFile("uniqv1storage", []messageInfo{msg})
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
	out := renderStorageFile("destv1", []messageInfo{msg})
	// The unique field and account_id share one composite index name, with
	// account_id as the leading column.
	mustContain(t, out, "uniqueIndex:ux_destination_account_name,priority:2")
	mustContain(t, out, "uniqueIndex:ux_destination_account_name,priority:1")
	// Never a bare/global unique index on the tenant-scoped field.
	mustNotContain(t, out, "column:name;not null;uniqueIndex\"")
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
	out := renderStorageFile("destv1", []messageInfo{msg})
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
	out := renderStorageFile("orderv1storage", []messageInfo{msg})
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
	out := renderStorageFile("postv1storage", []messageInfo{msg})
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
	out := renderStorageFile("exportsv1", []messageInfo{msg})
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
	out := renderStorageFile("exportsv1", []messageInfo{msg})
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
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg})

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
	mustNotContain(t, out, "m.DeleteTime = ")  // OUTPUT_ONLY: toModel never copies it

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
	out := renderStorageFile("apikeyv1storage", []messageInfo{msg})

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

	// database/sql import present.
	mustContain(t, out, `"database/sql"`)
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
	out := renderStorageFile("recordv1storage", []messageInfo{msg})

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
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg})

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
	out := renderStorageFile("credv1storage", []messageInfo{msg})

	mustContain(t, out, "updates := map[string]interface{}{")
	mustContain(t, out, `"label": m.Label,`)
	// Secret columns are guarded by a presence check, not unconditionally written.
	mustContain(t, out, `if entity.Token != "" {`)
	mustContain(t, out, `updates["token_hash"] = m.TokenHash`)
	mustContain(t, out, `updates["token_cipher"] = m.TokenCipher`)
	// The plaintext secret is never a column in the update map.
	mustNotContain(t, out, `"token": m.Token,`)
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
