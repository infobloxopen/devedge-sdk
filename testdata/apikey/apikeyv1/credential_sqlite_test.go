package apikeyv1_test

// credential_sqlite_test.go — WS-033 verify-only credential primitive, end-to-end
// on BOTH backends (ent + GORM). It proves, for the ServiceToken.secret_value
// credential field, that:
//
//	(a) Create returns the minted token ONCE (prefixed split token "st_...");
//	(b) at rest the row has public_id + salt + hash + hashspec and NO plaintext
//	    or reversible cipher column;
//	(c) Verify<Field> accepts the returned token and rejects a wrong/tampered one;
//	(d) Get/List never return the secret_value;
//	(e) a nil minter fails LOUD (persistence.ErrNoMinter), never silently.

import (
	"context"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // register the SQLite driver for enttest + GORM

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// ---- ent backend ----

func TestCredential_Ent_RoundTrip(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:cred_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := context.Background()
	minter := &secret.CredentialMinter{Prefix: "st"}
	repo := apikeyv1.NewServiceTokenEntRepository(client, minter)

	created, err := repo.Create(ctx, &apikeyv1.ServiceToken{Id: "st-1", Label: "ci deploy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// (a) Create returns the minted token ONCE, in secret_value, with the prefix.
	token := created.SecretValue
	if token == "" {
		t.Fatal("Create response secret_value is empty — the minted token must be returned once")
	}
	if !strings.HasPrefix(token, "st_") {
		t.Fatalf("minted token %q lacks the configured prefix st_", token)
	}
	if _, _, _, perr := secret.Parse(token); perr != nil {
		t.Fatalf("minted token does not parse: %v", perr)
	}

	// (b) At rest: public_id + salt + hash + hashspec present; no plaintext column.
	row, err := client.ServiceToken.Get(ctx, "st-1")
	if err != nil {
		t.Fatalf("load ent row: %v", err)
	}
	if row.SecretValuePublicID == "" || row.SecretValueSalt == "" || row.SecretValueHash == "" {
		t.Fatalf("stored credential columns not populated: %+v", row)
	}
	if row.SecretValueHashspec != secret.AlgoSHA512_256 {
		t.Errorf("stored hashspec = %q, want default %q", row.SecretValueHashspec, secret.AlgoSHA512_256)
	}
	// The ent entity has no plaintext/cipher field for the credential — that the
	// struct exposes only the split columns is a compile-time guarantee (a
	// row.SecretValue / row.SecretValueCipher reference below would not compile).

	// (c) Verify accepts the real token, rejects a tampered one and an unknown one.
	rec, ok, err := apikeyv1.VerifySecretValue(ctx, client, token)
	if err != nil {
		t.Fatalf("verify (valid): %v", err)
	}
	if !ok {
		t.Fatal("verify rejected the freshly minted token")
	}
	if rec == nil || rec.Id != "st-1" {
		t.Fatalf("verify returned wrong record: %+v", rec)
	}
	if _, ok, _ := apikeyv1.VerifySecretValue(ctx, client, tamper(token)); ok {
		t.Fatal("verify accepted a tampered token")
	}
	if _, ok, _ := apikeyv1.VerifySecretValue(ctx, client, "st_deadbeef_deadbeef"); ok {
		t.Fatal("verify accepted an unknown token")
	}
	if _, ok, err := apikeyv1.VerifySecretValue(ctx, client, "not a token"); ok || err != nil {
		t.Fatalf("verify of a malformed token = (ok=%v err=%v), want (false, nil)", ok, err)
	}

	// (d) Get never returns the secret.
	got, err := repo.Get(ctx, "st-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SecretValue != "" {
		t.Errorf("Get returned secret_value %q — a credential must never be returned on read", got.SecretValue)
	}
	// ...and neither does List.
	list, _, err := repo.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, st := range list {
		if st.SecretValue != "" {
			t.Errorf("List returned secret_value for %s — must never be returned", st.Id)
		}
	}
	// "No plaintext at rest" on the ent backend is a compile-time guarantee: the
	// generated ent entity exposes only the split columns (SecretValuePublicID/
	// Salt/Hash/Hashspec) and no SecretValue/SecretValueCipher field, so a reference
	// to a plaintext column would not compile. The GORM test additionally asserts
	// the physical table has no plaintext/cipher column (assertNoPlaintextColumn).
}

func TestCredential_Ent_NilMinterFailsLoud(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:cred_ent_nil?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewServiceTokenEntRepository(client, nil)
	_, err := repo.Create(context.Background(), &apikeyv1.ServiceToken{Id: "x"})
	if !errors.Is(err, persistence.ErrNoMinter) {
		t.Fatalf("create with nil minter = %v, want persistence.ErrNoMinter", err)
	}
}

// ---- GORM backend ----

func TestCredential_GORM_RoundTrip(t *testing.T) {
	db, err := gorm.Open(openTestSQLite("file:cred_gorm?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.ServiceTokenModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	ctx := context.Background()
	minter := &secret.CredentialMinter{Prefix: "st"}
	repo := apikeyv1.NewServiceTokenRepository(db, minter)

	created, err := repo.Create(ctx, &apikeyv1.ServiceToken{Id: "st-1", Label: "ci deploy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// (a) Create returns the minted token once.
	token := created.SecretValue
	if !strings.HasPrefix(token, "st_") {
		t.Fatalf("minted token %q lacks the prefix st_", token)
	}

	// (b) At rest: split columns present, and NO plaintext/cipher column exists.
	var m apikeyv1.ServiceTokenModel
	if err := db.Where("id = ?", "st-1").First(&m).Error; err != nil {
		t.Fatalf("load model: %v", err)
	}
	if m.SecretValuePublicID == "" || m.SecretValueSalt == "" || m.SecretValueHash == "" {
		t.Fatalf("stored credential columns not populated: %+v", m)
	}
	if m.SecretValueHashspec != secret.AlgoSHA512_256 {
		t.Errorf("stored hashspec = %q, want default %q", m.SecretValueHashspec, secret.AlgoSHA512_256)
	}
	assertNoPlaintextColumn(t, gormColumns(t, db, "service_token_models"))

	// (c) Verify accepts the real token, rejects a tampered / unknown / malformed one.
	rec, ok, err := repo.VerifySecretValue(ctx, token)
	if err != nil || !ok {
		t.Fatalf("verify (valid): ok=%v err=%v", ok, err)
	}
	if rec == nil || rec.Id != "st-1" {
		t.Fatalf("verify returned wrong record: %+v", rec)
	}
	if _, ok, _ := repo.VerifySecretValue(ctx, tamper(token)); ok {
		t.Fatal("verify accepted a tampered token")
	}
	if _, ok, err := repo.VerifySecretValue(ctx, "garbage"); ok || err != nil {
		t.Fatalf("verify of a malformed token = (ok=%v err=%v), want (false, nil)", ok, err)
	}

	// (d) Get never returns the secret.
	got, err := repo.Get(ctx, "st-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SecretValue != "" {
		t.Errorf("Get returned secret_value %q — must never be returned on read", got.SecretValue)
	}
}

func TestCredential_GORM_NilMinterFailsLoud(t *testing.T) {
	db, err := gorm.Open(openTestSQLite("file:cred_gorm_nil?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.ServiceTokenModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	repo := apikeyv1.NewServiceTokenRepository(db, nil)
	_, err = repo.Create(context.Background(), &apikeyv1.ServiceToken{Id: "x"})
	if !errors.Is(err, persistence.ErrNoMinter) {
		t.Fatalf("create with nil minter = %v, want persistence.ErrNoMinter", err)
	}
}

// ---- Remint (#187): rotate a credential in place ----

// TestRemint_GORM_RotatesToken proves #187 on the GORM backend: RemintSecretValue
// mints a fresh token, the OLD token stops verifying, the NEW one verifies to the
// same record, and an unknown id returns ErrNotFound.
func TestRemint_GORM_RotatesToken(t *testing.T) {
	db, err := gorm.Open(openTestSQLite("file:remint_gorm?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.ServiceTokenModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	ctx := context.Background()
	repo := apikeyv1.NewServiceTokenRepository(db, &secret.CredentialMinter{Prefix: "st"})

	created, err := repo.Create(ctx, &apikeyv1.ServiceToken{Id: "st-1", Label: "ci"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	old := created.SecretValue
	if _, ok, _ := repo.VerifySecretValue(ctx, old); !ok {
		t.Fatal("freshly created token should verify")
	}

	newTok, err := repo.RemintSecretValue(ctx, "st-1")
	if err != nil {
		t.Fatalf("remint: %v", err)
	}
	if newTok == "" || newTok == old {
		t.Fatalf("remint returned an empty or unchanged token: %q (old %q)", newTok, old)
	}
	if !strings.HasPrefix(newTok, "st_") {
		t.Errorf("reminted token %q lacks the configured prefix st_", newTok)
	}
	// The OLD token no longer verifies; the NEW token verifies to the same record —
	// the id/label/relationships are preserved (rotation, not delete+recreate).
	if _, ok, _ := repo.VerifySecretValue(ctx, old); ok {
		t.Error("the OLD token still verifies after remint — rotation must invalidate it")
	}
	rec, ok, err := repo.VerifySecretValue(ctx, newTok)
	if err != nil || !ok {
		t.Fatalf("verify (reminted): ok=%v err=%v", ok, err)
	}
	if rec.GetId() != "st-1" || rec.GetLabel() != "ci" {
		t.Errorf("reminted record changed identity: %+v", rec)
	}
	// Unknown id → ErrNotFound (no row rotated).
	if _, err := repo.RemintSecretValue(ctx, "nope"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint unknown id = %v, want ErrNotFound", err)
	}
}

// TestRemint_Ent_RotatesToken proves #187 on the ent backend via the generated
// RemintSecretValue package helper.
func TestRemint_Ent_RotatesToken(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:remint_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := context.Background()
	minter := &secret.CredentialMinter{Prefix: "st"}
	repo := apikeyv1.NewServiceTokenEntRepository(client, minter)

	created, err := repo.Create(ctx, &apikeyv1.ServiceToken{Id: "st-1", Label: "ci"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	old := created.SecretValue

	newTok, err := apikeyv1.RemintSecretValue(ctx, client, minter, "st-1")
	if err != nil {
		t.Fatalf("remint: %v", err)
	}
	if newTok == "" || newTok == old {
		t.Fatalf("remint returned an empty or unchanged token: %q (old %q)", newTok, old)
	}
	if _, ok, _ := apikeyv1.VerifySecretValue(ctx, client, old); ok {
		t.Error("the OLD token still verifies after remint")
	}
	rec, ok, err := apikeyv1.VerifySecretValue(ctx, client, newTok)
	if err != nil || !ok || rec.GetId() != "st-1" {
		t.Fatalf("verify (reminted): ok=%v err=%v rec=%+v", ok, err, rec)
	}
	if _, err := apikeyv1.RemintSecretValue(ctx, client, minter, "nope"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint unknown id = %v, want ErrNotFound", err)
	}
}

// ---- helpers ----

// tamper flips the last character of a token's secret so verification fails while
// the public_id (and thus the lookup) stays valid.
func tamper(token string) string {
	if token == "" {
		return token
	}
	b := []byte(token)
	last := b[len(b)-1]
	if last == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}

// assertNoPlaintextColumn fails if the credential's plaintext or reversible cipher
// column leaked into the table, and confirms the four split columns are present.
func assertNoPlaintextColumn(t *testing.T, cols map[string]bool) {
	t.Helper()
	if cols["secret_value"] {
		t.Error("table has a plaintext secret_value column — a credential must never be stored as plaintext")
	}
	if cols["secret_value_cipher"] {
		t.Error("table has a secret_value_cipher column — a credential must never keep a reversible copy")
	}
	for _, want := range []string{"secret_value_public_id", "secret_value_salt", "secret_value_hash", "secret_value_hashspec"} {
		if !cols[want] {
			t.Errorf("table is missing the credential column %q", want)
		}
	}
}

// gormColumns returns the column-name set of a GORM table via PRAGMA table_info.
func gormColumns(t *testing.T, db *gorm.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Raw("PRAGMA table_info(" + table + ")").Rows()
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		cols[name] = true
	}
	return cols
}
