package main

import (
	"strings"
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// T007: unit tests for renderEntSchema / renderGenerateFile — pure functions,
// no protogen/buf needed.

// apiKeyMessage mirrors the testdata/apikey APIKey message: id, name,
// account_id, key_value (secret), key_prefix.
func apiKeyMessage() entMessageInfo {
	return entMessageInfo{
		MessageName: "APIKey",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "name", SnakeName: "name", EntType: "String"},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "key_value", SnakeName: "key_value", EntType: "String", IsSecret: true},
			{Name: "key_prefix", SnakeName: "key_prefix", EntType: "String"},
		},
	}
}

func TestRenderEntSchema_basicNoTenantNoSecret(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Widget",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "name", SnakeName: "name", EntType: "String"},
			{Name: "weight", SnakeName: "weight", EntType: "Int32"},
			{Name: "active", SnakeName: "active", EntType: "Bool"},
		},
	}
	out := renderEntSchema(msg)

	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package schema")
	mustContain(t, out, "type Widget struct {")
	mustContain(t, out, "ent.Schema")
	mustContain(t, out, "func (Widget) Fields() []ent.Field {")

	// id annotated as the primary key.
	mustContain(t, out, `field.String("id").StorageKey("id").Immutable()`)

	// Regular fields by type, all Optional.
	mustContain(t, out, `field.String("name").Optional()`)
	mustContain(t, out, `field.Int32("weight").Optional()`)
	mustContain(t, out, `field.Bool("active").Optional()`)

	// No account_id field → no Mixin(), no entrepo import.
	mustNotContain(t, out, "func (Widget) Mixin()")
	mustNotContain(t, out, "TenantMixin")
	mustNotContain(t, out, "persistence/entrepo")

	// No secret field → no Indexes(), no index import.
	mustNotContain(t, out, "func (Widget) Indexes()")
	mustNotContain(t, out, "entgo.io/ent/schema/index")
}

func TestRenderEntSchema_accountIDAddsTenantMixin(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Record",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "value", SnakeName: "value", EntType: "String"},
		},
	}
	out := renderEntSchema(msg)

	// TenantMixin in Mixin() + entrepo import.
	mustContain(t, out, "func (Record) Mixin() []ent.Mixin {")
	mustContain(t, out, "entrepo.TenantMixin{}")
	mustContain(t, out, `"github.com/infobloxopen/devedge-sdk/persistence/entrepo"`)
	mustContain(t, out, "Mixin returns the mixins applied to Record")

	// account_id is supplied by the mixin — never emitted as a direct field.
	mustNotContain(t, out, `field.String("account_id")`)

	// Other fields still present.
	mustContain(t, out, `field.String("value").Optional()`)
}

func TestRenderEntSchema_secretFieldHashAndCipher(t *testing.T) {
	out := renderEntSchema(apiKeyMessage())

	// Secret field split into _hash + _cipher; raw field NOT emitted.
	mustContain(t, out, `field.String("key_value_hash").Optional().Comment("HMAC-SHA256 of key_value for lookup")`)
	mustContain(t, out, `field.String("key_value_cipher").Optional().Comment("encrypted key_value for recovery")`)
	mustNotContain(t, out, `field.String("key_value").Optional()`)

	// Index on the secret's _hash column + index import.
	mustContain(t, out, "func (APIKey) Indexes() []ent.Index {")
	mustContain(t, out, `index.Fields("key_value_hash")`)
	mustContain(t, out, `"entgo.io/ent/schema/index"`)

	// APIKey carries account_id → TenantMixin present.
	mustContain(t, out, "func (APIKey) Mixin() []ent.Mixin {")
	mustContain(t, out, "entrepo.TenantMixin{}")

	// Non-secret fields still present; account_id suppressed.
	mustContain(t, out, `field.String("name").Optional()`)
	mustContain(t, out, `field.String("key_prefix").Optional()`)
	mustNotContain(t, out, `field.String("account_id")`)
}

func TestRenderEntSchema_repeatedAndMessageSkipped(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Thing",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "tags", SnakeName: "tags", EntType: "String", IsRepeated: true},
			{Name: "meta", SnakeName: "meta", EntType: "String", IsMessage: true},
		},
	}
	out := renderEntSchema(msg)

	mustContain(t, out, "// TODO: repeated field tags skipped")
	mustContain(t, out, "// TODO: nested message meta skipped")
	// No real field emitted for the skipped ones.
	mustNotContain(t, out, `field.String("tags")`)
	mustNotContain(t, out, `field.String("meta")`)
}

func TestRenderEntSchema_emptyMessage(t *testing.T) {
	out := renderEntSchema(entMessageInfo{MessageName: "Empty"})
	if out != "" {
		t.Fatalf("expected empty output for a message with no fields, got:\n%s", out)
	}
}

func TestRenderGenerateFile(t *testing.T) {
	out := renderGenerateFile()
	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package ent")
	mustContain(t, out, "//go:generate go run entgo.io/ent/cmd/ent generate ./schema")
}

// T008: constraint and relationship field tests.

func TestRenderEntSchema_uniqueField(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "User",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "email", SnakeName: "email", EntType: "String", Unique: true},
		},
	}
	out := renderEntSchema(msg)
	mustContain(t, out, `.Unique()`)
	mustContain(t, out, `field.String("email")`)
}

func TestRenderEntSchema_hasOneEdge(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Order",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "address", SnakeName: "address", EntType: "String",
				IsMessage: true, HasOne: &fieldv1.HasOne{ForeignKey: "order_id"}},
		},
	}
	out := renderEntSchema(msg)
	// Should emit Edges() method with edge.To.
	mustContain(t, out, "func (Order) Edges() []ent.Edge {")
	mustContain(t, out, `edge.To("address",`)
	mustContain(t, out, ".Unique()")
	mustContain(t, out, ".Required()")
	// Must import edge package.
	mustContain(t, out, `"entgo.io/ent/schema/edge"`)
	// No TODO comment.
	mustNotContain(t, out, "TODO: nested message address skipped")
}

// TestRenderEntRepository covers the F026 batch wrapper: tenant + secret +
// soft-delete + an OUTPUT_ONLY field (which must NOT be writable).
func TestRenderEntRepository(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "APIKey",
		SoftDelete:  true,
		Fields: []entFieldInfo{
			{Name: "name", SnakeName: "name", EntType: "String", OutputOnly: true},
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "key_value", SnakeName: "key_value", EntType: "String", IsSecret: true},
			{Name: "key_prefix", SnakeName: "key_prefix", EntType: "String"},
			{Name: "label", SnakeName: "label", EntType: "String"},
		},
	}
	out := renderEntRepository(msg, "apikeyv1", "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1")

	mustContain(t, out, "package apikeyv1")
	mustContain(t, out, `ent "github.com/infobloxopen/devedge-sdk/testdata/apikey/ent"`)
	mustContain(t, out, `entapikey "github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/apikey"`)
	mustContain(t, out, "type APIKeyEntRepository struct")
	mustContain(t, out, "func NewAPIKeyEntBatchRepository(client *ent.Client, enc secret.Encryptor) *APIKeyEntRepository")
	mustContain(t, out, "func (r *APIKeyEntRepository) BatchGet(ctx context.Context, keys []string) ([]*APIKey, error)")
	mustContain(t, out, "func (r *APIKeyEntRepository) BatchUpdate(ctx context.Context, items []persistence.BatchUpdateItem[*APIKey, string])")
	mustContain(t, out, "func (r *APIKeyEntRepository) BatchDelete(ctx context.Context, keys []string) error")
	mustContain(t, out, "r.client.Tx(ctx)")
	// Mutations carry explicit tenant + soft-delete predicates (interceptors are query-only).
	mustContain(t, out, "entapikey.AccountID(tenantID)")
	mustContain(t, out, "entapikey.DeleteTimeIsNil()")
	// Writable scalar + secret re-encryption.
	mustContain(t, out, "u.SetKeyPrefix(it.Entity.GetKeyPrefix())")
	mustContain(t, out, "u.SetLabel(it.Entity.GetLabel())")
	mustContain(t, out, "r.enc.Hash(ctx, it.Entity.GetKeyValue())")
	mustContain(t, out, "u.SetKeyValueHash(h).SetKeyValueCipher(c)")
	mustContain(t, out, "var _ persistence.BatchRepository[*APIKey, string]")
	// OUTPUT_ONLY field must never be written by batch update.
	if strings.Contains(out, "SetName(") {
		t.Errorf("OUTPUT_ONLY field 'name' must not be settable in BatchUpdate\n--- output ---\n%s", out)
	}
}

func TestToSnake(t *testing.T) {
	cases := map[string]string{
		"APIKey":     "api_key",
		"Widget":     "widget",
		"key_value":  "key_value",
		"accountId":  "account_id",
		"HTTPServer": "http_server",
		"id":         "id",
	}
	for in, want := range cases {
		if got := toSnake(in); got != want {
			t.Errorf("toSnake(%q) = %q, want %q", in, got, want)
		}
	}
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
