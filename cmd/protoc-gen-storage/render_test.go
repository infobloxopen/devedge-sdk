package main

import (
	"strings"
	"testing"
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
	mustContain(t, out, "gorm.DeletedAt")
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
