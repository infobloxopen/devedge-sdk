package main

import (
	"go/format"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// TestToStorageFields_failClosed verifies the neutral bridge + classifier (F027
// Phase 3): unmappable fields (nested non-relationship message, repeated
// non-relationship, enum) are flagged so generation can fail, while a
// belongs_to message + its scalar FK are NOT flagged.
func TestToStorageFields_failClosed(t *testing.T) {
	unmappable := entMessageInfo{
		MessageName: "Thing",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "spec", SnakeName: "spec", IsMessage: true},                    // nested non-relationship message
			{Name: "aliases", SnakeName: "aliases", IsRepeated: true},             // repeated non-relationship
			{Name: "state", SnakeName: "state", EntType: "String", IsEnum: true},  // enum
		},
	}
	if _, unmapped := storagegen.Classify(toStorageFields(unmappable)); len(unmapped) != 3 {
		t.Fatalf("expected 3 unmapped fields (spec, aliases, state), got %d", len(unmapped))
	}

	rel := entMessageInfo{
		MessageName: "Vehicle",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "fleet_id", SnakeName: "fleet_id", EntType: "String"},
			{Name: "fleet", SnakeName: "fleet", IsMessage: true, BelongsTo: &fieldv1.BelongsTo{ForeignKey: "fleet_id"}},
		},
	}
	if _, unmapped := storagegen.Classify(toStorageFields(rel)); len(unmapped) != 0 {
		t.Errorf("belongs_to message + scalar FK must be fully mappable, got %d unmapped", len(unmapped))
	}
}

// TestRenderEntRepoAdapter_fullShape exercises the richest message shape — tenant
// + secret + soft-delete + TTL + etag + tags + a plain scalar — and proves the
// generated adapter is valid, gofmt-able Go (a real syntax gate) and carries every
// load-bearing construct (F027 T101/T102).
func TestRenderEntRepoAdapter_fullShape(t *testing.T) {
	msg := entMessageInfo{
		MessageName:   "Coupon",
		SoftDelete:    true,
		HasExpireTime: true,
		HasETag:       true,
		Fields: []entFieldInfo{
			{Name: "name", SnakeName: "name", EntType: "String", OutputOnly: true},
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "code", SnakeName: "code", EntType: "String", Unique: true, NotNull: true},
			{Name: "discount_bps", SnakeName: "discount_bps", EntType: "Int32"},
			{Name: "signing_key", SnakeName: "signing_key", EntType: "String", IsSecret: true},
			{Name: "tags", SnakeName: "tags", EntType: "JSON", IsTags: true},
		},
	}
	out := renderEntRepoAdapter(msg, "couponv1", "github.com/example/coupond/couponv1")
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	// Strongest check: the whole file must be syntactically valid Go.
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}

	wants := []string{
		"func NewCouponEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*Coupon, string]",
		"Create_: func(ctx context.Context, entity *Coupon) (*Coupon, error)",
		"Get_: func(ctx context.Context, key string) (*Coupon, error)",
		"List_: func(ctx context.Context, opts persistence.ListOptions) ([]*Coupon, string, error)",
		"Update_: func(ctx context.Context, key string, entity *Coupon, fieldMask ...string) (*Coupon, error)",
		"Delete_: func(ctx context.Context, key string) error",
		"Undelete_: func(ctx context.Context, key string) (*Coupon, error)",
		"persistence.ConstraintError(err)",
		"ent.IsNotFound(err)",
		"middleware.TenantIDFromContext(ctx)",
		"SetAccountID(entity.GetAccountId())",
		"entcoupon.AccountID(tenantID)",
		"entcoupon.DeleteTimeIsNil()",
		"SetDeleteTime(time.Now())",
		"entcoupon.DeleteTimeNotNil()",
		"ClearDeleteTime()",
		"enc.Hash(ctx, entity.GetSigningKey())",
		"SetSigningKeyHash(h).SetSigningKeyCipher(c)",
		`entity.SigningKey = ""`,
		"entrepo.FilterPredicate(opts.Filter, CouponEntColumns, CouponEntJSONColumns)",
		"entpredicate.Coupon(pred)",
		"func fromEntCoupon(e *ent.Coupon) *Coupon",
		"Name: e.Name", // OUTPUT_ONLY scalar still surfaced on read (not written)
		"Etag: e.Etag",
		"p.DeleteTime = timestamppb.New(*e.DeleteTime)",
		"p.ExpireTime = timestamppb.New(*e.ExpireTime)",
		"func LookupBySigningKeyHash(ctx context.Context, client *ent.Client, hash string) (*Coupon, error)",
		"entcoupon.SigningKeyHash(hash)",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated output missing %q", w)
		}
	}
	// The secret must never be projected back onto the proto.
	if strings.Contains(out, "SigningKey: e.SigningKey") {
		t.Error("secret field leaked into fromEnt projection")
	}
	// OUTPUT_ONLY `name` must NOT be written by Create/Update (client can't set it).
	if strings.Contains(out, "SetName(") {
		t.Error("OUTPUT_ONLY field 'name' must not be written by the adapter")
	}
}

// TestRenderEntRepoAdapter_noTenantNoSecretHardDelete covers the minimal shape:
// no account_id (no tenant guard / no middleware import), no secret (no enc
// param), no soft-delete (hard-delete path, no Undelete_).
func TestRenderEntRepoAdapter_noTenantNoSecretHardDelete(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Note",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "body", SnakeName: "body", EntType: "String"},
		},
	}
	out := renderEntRepoAdapter(msg, "notev1", "github.com/example/noted/notev1")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}
	if !strings.Contains(out, "func NewNoteEntRepository(client *ent.Client) persistence.Repository[*Note, string]") {
		t.Error("expected no-enc constructor for a message without secret fields")
	}
	if !strings.Contains(out, "client.Note.Delete().Where(entnote.ID(key))") {
		t.Error("expected hard-delete path for a non-soft-delete message")
	}
	if strings.Contains(out, "Undelete_:") {
		t.Error("did not expect Undelete_ for a non-soft-delete message")
	}
	if strings.Contains(out, "middleware.TenantIDFromContext") {
		t.Error("did not expect a tenant guard for a message without account_id")
	}
	if strings.Contains(out, "secret.Encryptor") {
		t.Error("did not expect the secret import for a message without secret fields")
	}
}

// TestRenderEntRepoAdapter_empty returns "" for a fieldless (non-resource) message.
func TestRenderEntRepoAdapter_empty(t *testing.T) {
	if out := renderEntRepoAdapter(entMessageInfo{MessageName: "Empty"}, "v1", "m/v1"); out != "" {
		t.Errorf("expected empty output for a fieldless message, got %q", out)
	}
}
