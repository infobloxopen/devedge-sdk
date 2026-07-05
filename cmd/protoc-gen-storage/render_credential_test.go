package main

import (
	"go/format"
	"testing"
)

// TestRenderStorageFile_credentialField covers the WS-033 verify-only credential
// field on the GORM backend: split columns, minter constructor, mint-on-Create,
// Verify<Field>, and omission from the projection.
func TestRenderStorageFile_credentialField(t *testing.T) {
	msg := messageInfo{
		MessageName:  "ServiceToken",
		PbPkgName:    "", // storage is generated in-package (same as the pb types)
		PbImportPath: "example/apikey/apikeyv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "label", GoType: "string", SnakeName: "label"},
			{Name: "secret_value", GoFieldName: "SecretValue", GoType: "string", SnakeName: "secret_value", IsCredential: true, CredentialPrefix: "st"},
		},
	}
	out := renderStorageFile("apikeyv1", []messageInfo{msg}, nil)
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}

	// secret package imported for the minter.
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/secret"`)

	// The four split columns are present; public_id is a UNIQUE index. No plaintext
	// column and no reversible cipher column.
	mustContain(t, out, "column:secret_value_public_id;uniqueIndex")
	mustContain(t, out, "column:secret_value_salt")
	mustContain(t, out, "column:secret_value_hash")
	mustContain(t, out, "column:secret_value_hashspec")
	mustNotContain(t, out, "`gorm:\"column:secret_value\"`")
	mustNotContain(t, out, "secret_value_cipher")

	// Constructor + struct carry the minter.
	mustContain(t, out, "func NewServiceTokenRepository(db *gorm.DB, minter *secret.CredentialMinter, opts ...persistence.RepoOption)")
	mustContain(t, out, "minter *secret.CredentialMinter")

	// Create mints (fail-loud on nil), sets columns, and returns the token once.
	mustContain(t, out, "persistence.ErrNoMinter")
	mustContain(t, out, `mSecretValue.Prefix = "st"`)
	mustContain(t, out, "mSecretValue.Mint()")
	mustContain(t, out, "m.SecretValuePublicID = credSecretValue.PublicID")
	mustContain(t, out, "m.SecretValueHashspec = credSecretValue.Spec.Algo")
	mustContain(t, out, "p.SecretValue = tokSecretValue")

	// Verify<Field> method exists and looks up by public_id.
	mustContain(t, out, "func (r *ServiceTokenRepository) VerifySecretValue(ctx context.Context, token string) (*ServiceToken, bool, error)")
	mustContain(t, out, "secret.Parse(token)")
	mustContain(t, out, `Where("secret_value_public_id = ?", publicID)`)
	mustContain(t, out, "secret.Verify(presented, secret.StoredCredential{")

	// toModel/fromModel never touch the raw credential field.
	mustNotContain(t, out, "m.SecretValue = p.SecretValue")
	mustNotContain(t, out, "p.SecretValue = m.SecretValue")

	// Non-credential field still normal.
	mustContain(t, out, `gorm:"column:label"`)
}

// TestRenderStorageFile_remint covers #187: a credential field also gets a
// Remint<Field> that rotates the four columns in place and returns the new token.
func TestRenderStorageFile_remint(t *testing.T) {
	msg := messageInfo{
		MessageName:  "ServiceToken",
		PbImportPath: "example/apikey/apikeyv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "secret_value", GoFieldName: "SecretValue", GoType: "string", SnakeName: "secret_value", IsCredential: true, CredentialPrefix: "st"},
		},
	}
	out := renderStorageFile("apikeyv1", []messageInfo{msg}, nil)
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}
	mustContain(t, out, "func (r *ServiceTokenRepository) RemintSecretValue(ctx context.Context, id string) (string, error)")
	mustContain(t, out, "persistence.ErrNoMinter")
	mustContain(t, out, "q := r.conn(ctx).Model(&ServiceTokenModel{}).Where(\"id = ?\", id)")
	mustContain(t, out, "q.Updates(map[string]interface{}{")
	mustContain(t, out, `"secret_value_public_id": cred.PublicID,`)
	mustContain(t, out, `"secret_value_hashspec": cred.Spec.Algo,`)
	mustContain(t, out, "if res.RowsAffected == 0 { return \"\", persistence.ErrNotFound }")
	mustContain(t, out, "return tok, nil")
}

// TestRenderStorageFile_getByUnique covers #173: a plain unique string field gets
// a tenant-scoped GetBy<Field> natural-key lookup.
func TestRenderStorageFile_getByUnique(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Link",
		PbImportPath: "example/link/linkv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoType: "string", SnakeName: "account_id"},
			{Name: "slug", GoFieldName: "Slug", GoType: "string", SnakeName: "slug", Unique: true},
		},
	}
	out := renderStorageFile("linkv1", []messageInfo{msg}, nil)
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}
	mustContain(t, out, "func (r *LinkRepository) GetBySlug(ctx context.Context, value string) (")
	mustContain(t, out, "if value == \"\" {")
	mustContain(t, out, `q := r.conn(ctx).Where("slug = ?", value)`)
	// Tenant-scoped: excludes other tenants' rows.
	mustContain(t, out, `q = q.Where("account_id = ?", tenantID)`)
	mustContain(t, out, "persistence.ErrNotFound")
}

// TestRenderStorageFile_noGetByForScopedUnique confirms a unique_with (unique
// within a scope) field is NOT given a single-value GetBy (it would be ambiguous).
func TestRenderStorageFile_noGetByForScopedUnique(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Seat",
		PbImportPath: "example/seat/seatv1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "account_id", GoType: "string", SnakeName: "account_id"},
			{Name: "row_id", GoType: "string", SnakeName: "row_id"},
			{Name: "number", GoFieldName: "Number", GoType: "string", SnakeName: "number", Unique: true, UniqueWith: []string{"row_id"}},
		},
	}
	out := renderStorageFile("seatv1", []messageInfo{msg}, nil)
	mustNotContain(t, out, "func (r *SeatRepository) GetByNumber(")
}
