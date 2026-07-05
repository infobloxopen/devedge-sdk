package main

import (
	"go/format"
	"testing"
)

// serviceTokenMessage mirrors the testdata/apikey ServiceToken message: a
// standalone resource with a verify-only credential field (WS-033).
func serviceTokenMessage() entMessageInfo {
	return entMessageInfo{
		MessageName: "ServiceToken",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "label", SnakeName: "label", EntType: "String"},
			{Name: "secret_value", SnakeName: "secret_value", EntType: "String", IsCredential: true, CredentialPrefix: "st"},
		},
	}
}

func TestRenderEntSchema_credentialField(t *testing.T) {
	out := renderEntSchema(serviceTokenMessage(), nil)

	// The four split columns are emitted; the plaintext column is NOT.
	mustContain(t, out, `field.String("secret_value_public_id")`)
	mustContain(t, out, `field.String("secret_value_salt")`)
	mustContain(t, out, `field.String("secret_value_hash")`)
	mustContain(t, out, `field.String("secret_value_hashspec")`)
	mustNotContain(t, out, `field.String("secret_value").`)

	// public_id carries a UNIQUE index (the lookup handle).
	mustContain(t, out, `index.Fields("secret_value_public_id").Unique()`)
	mustContain(t, out, "entgo.io/ent/schema/index")

	// A credential is not a secret: no HMAC hash/cipher column, no cipher.
	mustNotContain(t, out, "secret_value_cipher")
	mustNotContain(t, out, "HMAC-SHA256 of secret_value")
}

func TestRenderEntRepoAdapter_credential(t *testing.T) {
	msg := serviceTokenMessage()
	out := renderEntRepoAdapter(msg, msg, "apikeyv1", "github.com/example/apikey/apikeyv1")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}

	// Constructor takes a *secret.CredentialMinter and imports secret.
	mustContain(t, out, "func NewServiceTokenEntRepository(client *ent.Client, minter *secret.CredentialMinter, opts ...persistence.RepoOption) persistence.Repository[*ServiceToken, string]")
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/secret"`)

	// Create mints, fails loud on a nil minter, sets the split columns, and returns
	// the token ONCE.
	mustContain(t, out, "if minter == nil {")
	mustContain(t, out, "persistence.ErrNoMinter")
	mustContain(t, out, `mSecretValue.Prefix = "st"`)
	mustContain(t, out, "mSecretValue.Mint()")
	mustContain(t, out, "SetSecretValuePublicID(credSecretValue.PublicID)")
	mustContain(t, out, "SetSecretValueHashspec(credSecretValue.Spec.Algo)")
	mustContain(t, out, "result.SecretValue = tokSecretValue")

	// Verify<Field> is emitted and looks the record up by public_id.
	mustContain(t, out, "func VerifySecretValue(ctx context.Context, client *ent.Client, token string) (*ServiceToken, bool, error)")
	mustContain(t, out, "secret.Parse(token)")
	mustContain(t, out, "entservicetoken.SecretValuePublicID(publicID)")
	mustContain(t, out, "secret.Verify(presented, secret.StoredCredential{")

	// fromEnt omits the credential on read (never returned).
	mustContain(t, out, "secret_value omitted — verify-only credential")
	mustNotContain(t, out, "SecretValue: e.SecretValue")
}

// TestRenderEntRepoAdapter_remint covers #187 on the ent backend: a Remint<Field>
// package helper that mints a fresh token, overwrites the four columns, and
// returns the new token.
func TestRenderEntRepoAdapter_remint(t *testing.T) {
	msg := serviceTokenMessage()
	out := renderEntRepoAdapter(msg, msg, "apikeyv1", "github.com/example/apikey/apikeyv1")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}
	mustContain(t, out, "func RemintSecretValue(ctx context.Context, client *ent.Client, minter *secret.CredentialMinter, id string) (string, error)")
	mustContain(t, out, "if minter == nil {")
	mustContain(t, out, `m.Prefix = "st"`)
	mustContain(t, out, "client.ServiceToken.Update().Where(entservicetoken.ID(id))")
	mustContain(t, out, "SetSecretValuePublicID(cred.PublicID)")
	mustContain(t, out, "SetSecretValueHashspec(cred.Spec.Algo)")
	mustContain(t, out, "if n == 0 {")
	mustContain(t, out, "persistence.ErrNotFound")
	mustContain(t, out, "return tok, nil")
}

// linkMessage is a tenant-scoped resource with a plain unique string field (slug).
func linkMessage() entMessageInfo {
	return entMessageInfo{
		MessageName: "Link",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "slug", SnakeName: "slug", EntType: "String", Unique: true},
		},
	}
}

// TestRenderEntRepoAdapter_getByUnique covers #173 on the ent backend: a tenant-
// scoped GetBy<Field> natural-key lookup for a plain unique string field.
func TestRenderEntRepoAdapter_getByUnique(t *testing.T) {
	msg := linkMessage()
	out := renderEntRepoAdapter(msg, msg, "linkv1", "github.com/example/link/linkv1")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}
	mustContain(t, out, "func GetLinkBySlug(ctx context.Context, client *ent.Client, value string) (*Link, error)")
	mustContain(t, out, "if value == \"\" {")
	mustContain(t, out, "client.Link.Query().Where(entlink.Slug(value))")
	mustContain(t, out, "q = q.Where(entlink.AccountID(tenantID))")
	mustContain(t, out, "persistence.ErrNotFound")
}

func TestRenderEntRepository_credentialBatchForwardsMinter(t *testing.T) {
	msg := serviceTokenMessage()
	out := renderEntRepository(msg, msg, "apikeyv1", "github.com/example/apikey/apikeyv1")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n---\n%s", err, out)
	}
	// The batch constructor forwards the minter to New<R>EntRepository, but never
	// mints itself (credentials are minted on Create only).
	mustContain(t, out, "func NewServiceTokenEntBatchRepository(client *ent.Client, minter *secret.CredentialMinter) *ServiceTokenEntRepository")
	mustContain(t, out, "NewServiceTokenEntRepository(client, minter)")
	mustNotContain(t, out, "minter.Mint()")
}
