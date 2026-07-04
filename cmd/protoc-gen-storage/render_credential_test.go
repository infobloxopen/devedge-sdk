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
