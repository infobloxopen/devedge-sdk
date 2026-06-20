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
		MessageName:     "Coupon",
		SoftDelete:      true,
		HasExpireTime:   true,
		HasETag:         true,
		ResourcePattern: "coupons/{coupon}",
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
	out := renderEntRepoAdapter(msg, msg, "couponv1", "github.com/example/coupond/couponv1")
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
		// AIP-122: name is OUTPUT_ONLY + DERIVED from id — recomputed on read via the
		// generated helper, never read from a stored column (it is not stored).
		"p.Name = FormatCouponName(e.ID)",
		"const CouponNamePattern = \"coupons/{coupon}\"",
		"func FormatCouponName(id string) string",
		"func ParseCouponName(name string) (string, error)",
		`resourcename.Format(CouponNamePattern, map[string]string{"coupon": id})`,
		"Etag: e.Etag",
		"p.DeleteTime = timestamppb.New(*e.DeleteTime)",
		"p.ExpireTime = timestamppb.New(*e.ExpireTime)",
		"func LookupBySigningKeyHash(ctx context.Context, client *ent.Client, hash string) (*Coupon, error)",
		"entcoupon.SigningKeyHash(hash)",
		// Owned override seam (Phase 4): exported hook vars + call sites.
		"var FromEntCouponCustom func(e *ent.Coupon, p *Coupon)",
		"var ToEntCouponOnCreate func(p *Coupon, b *ent.CouponCreate)",
		"var ToEntCouponOnUpdate func(p *Coupon, u *ent.CouponUpdateOne)",
		"if FromEntCouponCustom != nil {",
		"if ToEntCouponOnCreate != nil {",
		"if ToEntCouponOnUpdate != nil {",
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
	// The derived AIP-122 name is never stored, so it must NOT be read from a column.
	if strings.Contains(out, "Name: e.Name") || strings.Contains(out, "p.Name = e.Name") {
		t.Error("derived AIP-122 name must be recomputed from id, not read from a stored column")
	}
}

// TestRenderEntRepoAdapter_updateHonorsFieldMask is the GH #60 gate: the single
// Update_ adapter must honor the field mask exactly like the GORM Update and the ent
// batch wrapper (G-005 cross-backend parity). Every writable Set — plain scalar,
// foreign key, and secret — is gated on the sibling <maskLower>InMask helper, which
// returns true for an empty mask (full update) and only the named proto fields for a
// non-empty mask. Without this, a partial update overwrites an unmasked not_null +
// unique business key (e.g. code) with its zero value and fails the ent validator.
func TestRenderEntRepoAdapter_updateHonorsFieldMask(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Coupon",
		SoftDelete:  true,
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "code", SnakeName: "code", EntType: "String", Unique: true, NotNull: true},
			{Name: "discount_bps", SnakeName: "discount_bps", EntType: "Int32"},
			{Name: "fleet_id", SnakeName: "fleet_id", EntType: "String"},
			{Name: "fleet", SnakeName: "fleet", IsMessage: true, BelongsTo: &fieldv1.BelongsTo{ForeignKey: "fleet_id"}},
			{Name: "signing_key", SnakeName: "signing_key", EntType: "String", IsSecret: true},
		},
	}
	out := renderEntRepoAdapter(msg, msg, "couponv1", "github.com/example/coupond/couponv1")
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}

	// Isolate the Update_ closure so the assertions below cannot accidentally match
	// the Create_ / batch sets, which legitimately set fields unconditionally.
	start := strings.Index(out, "Update_: func(ctx context.Context, key string, entity *Coupon")
	if start < 0 {
		t.Fatal("Update_ closure not found in generated adapter")
	}
	end := strings.Index(out[start:], "Delete_: func(")
	if end < 0 {
		t.Fatal("could not bound the Update_ closure (Delete_ not found)")
	}
	upd := out[start : start+end]

	// Plain scalar set must be gated on the InMask helper (matching the batch wrapper
	// name strings.ToLower(res) = "coupon").
	wantGates := []string{
		`if couponInMask(fieldMask, "discount_bps") {`,
		`u = u.SetDiscountBps(entity.GetDiscountBps())`,
		// Foreign key: mask AND non-empty value.
		`if couponInMask(fieldMask, "fleet_id") && entity.GetFleetId() != "" {`,
		// Secret: mask AND enc != nil AND non-empty value.
		`if couponInMask(fieldMask, "signing_key") && enc != nil && entity.GetSigningKey() != "" {`,
	}
	for _, w := range wantGates {
		if !strings.Contains(upd, w) {
			t.Errorf("masked Update_ missing gate %q\n--- Update_ ---\n%s", w, upd)
		}
	}

	// Regression guard for the exact #60 bug: the plain scalar must never be set
	// unconditionally. The old code emitted a bare, un-indented `u = u.SetX(...)` as
	// a top-level statement in the closure (one tab past the closure body). The fixed
	// code only ever emits that statement inside an `if couponInMask(...) {` block, so
	// the bare statement must not appear immediately after the UpdateOneID line.
	if strings.Contains(upd, "u := client.Coupon.UpdateOneID(key)\n\t\t\tu = u.SetDiscountBps(entity.GetDiscountBps())") {
		t.Error("plain scalar set is ungated in Update_ — #60 regression")
	}
	// The tenant key is never a Set in Update_ (only the WHERE guard).
	if strings.Contains(upd, "SetAccountID(entity.GetAccountId())") {
		t.Error("account_id (tenant key) must never be Set in Update_; it is only the WHERE guard")
	}
	if !strings.Contains(upd, "entcoupon.AccountID(tenantID)") {
		t.Error("Update_ must still apply the tenant WHERE guard")
	}

	// The empty-mask (full update) path is provided by couponInMask returning true on
	// an empty mask. That helper is emitted by the sibling batch wrapper; the adapter
	// must NOT redefine it (duplicate symbol). Confirm the adapter only CALLS it.
	if strings.Contains(out, "func couponInMask(") {
		t.Error("adapter must reuse the batch wrapper's couponInMask helper, not redefine it")
	}
}

// TestRenderEntRepository_emitsInMaskHelper proves the sibling batch wrapper (always
// generated alongside the adapter for the same message) defines the couponInMask
// helper the masked Update_ relies on, with an empty-mask-returns-true contract.
func TestRenderEntRepository_emitsInMaskHelper(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Coupon",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "code", SnakeName: "code", EntType: "String"},
		},
	}
	out := renderEntRepository(msg, msg, "couponv1", "github.com/example/coupond/couponv1")
	if !strings.Contains(out, "func couponInMask(mask []string, field string) bool {") {
		t.Fatal("batch wrapper must define couponInMask for the masked adapter to reuse")
	}
	if !strings.Contains(out, "if len(mask) == 0 {\n\t\treturn true\n\t}") {
		t.Error("couponInMask must return true for an empty mask (full update semantics)")
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
	out := renderEntRepoAdapter(msg, msg, "notev1", "github.com/example/noted/notev1")
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
	if out := renderEntRepoAdapter(entMessageInfo{MessageName: "Empty"}, entMessageInfo{MessageName: "Empty"}, "v1", "m/v1"); out != "" {
		t.Errorf("expected empty output for a fieldless message, got %q", out)
	}
}

// TestRenderEntRepoAdapter_multiSurface is the F027 Phase 5b gate at the render
// level (AC-004): a SURFACE message (CouponSummary, (infoblox.storage.v1.model)=
// "Coupon") projecting a subset of a tenant + soft-delete + secret owner (Coupon)
// generates a New<Surface>EntRepository that operates over the OWNER's ent type
// (client.Coupon, ent.Coupon, the ent/coupon predicate package) while projecting
// to/from the surface proto — and emits NO table of its own. Mutation semantics
// (tenant guard, soft-delete, undelete) follow the owner; the written/projected
// fields follow the surface.
func TestRenderEntRepoAdapter_multiSurface(t *testing.T) {
	owner := entMessageInfo{
		MessageName: "Coupon",
		Model:       "Coupon",
		SoftDelete:  true,
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "code", SnakeName: "code", EntType: "String", Unique: true, NotNull: true},
			{Name: "discount_bps", SnakeName: "discount_bps", EntType: "Int32"},
			{Name: "signing_key", SnakeName: "signing_key", EntType: "String", IsSecret: true},
		},
	}
	surface := entMessageInfo{
		MessageName: "CouponSummary",
		Model:       "Coupon", // a surface over Coupon — no table of its own
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "code", SnakeName: "code", EntType: "String"},
		},
	}
	out := renderEntRepoAdapter(surface, owner, "couponv1", "github.com/example/coupond/couponv1")
	if out == "" {
		t.Fatal("expected non-empty output for a surface adapter")
	}
	// Strongest check: the whole file must be syntactically valid Go.
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated surface code is not valid Go: %v\n--- generated ---\n%s", err, out)
	}
	wants := []string{
		// Constructor named for the surface, serving the surface domain type; no enc
		// (the surface projects no secret field).
		"func NewCouponSummaryEntRepository(client *ent.Client) persistence.Repository[*CouponSummary, string]",
		"entrepo.EntRepository[*CouponSummary, string]",
		// Operates over the OWNER's ent type / client / predicate package.
		"client.Coupon.Create()",
		"client.Coupon.Get(ctx, key)",
		"client.Coupon.Query()",
		`entcoupon "github.com/example/coupond/ent/coupon"`,
		// Projection input is the owner ent struct; output is the surface proto.
		"func fromEntCouponSummary(e *ent.Coupon) *CouponSummary",
		// Mutation semantics follow the owner: tenant guard + soft-delete + undelete.
		"entcoupon.AccountID(tenantID)",
		"Undelete_:",
		"SetDeleteTime(time.Now())",
		// List filtering uses the surface's own column map.
		"CouponSummaryEntColumns",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("surface adapter missing %q\n--- generated ---\n%s", w, out)
		}
	}
	// A surface must NOT create a table/query type of its own, and must not carry the
	// secret machinery (it projects no secret field).
	for _, bad := range []string{"client.CouponSummary.", "secret.Encryptor", "ent.CouponSummary"} {
		if strings.Contains(out, bad) {
			t.Errorf("surface adapter must not reference %q (it projects the owner's type)\n--- generated ---\n%s", bad, out)
		}
	}
}

// TestRenderEntFilterers_surfaceSkipped confirms a surface emits no filterers — the
// WhereAccountID/WhereDeleteTimeIsNil methods live on the OWNER's <Model>Query type,
// which the owner already emits; re-emitting them for the surface would redeclare
// the same method and fail to compile (F027 Phase 5b).
func TestRenderEntFilterers_surfaceSkipped(t *testing.T) {
	surface := entMessageInfo{
		MessageName: "CouponSummary",
		Model:       "Coupon",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
		},
	}
	if out := renderEntFilterers(surface, "github.com/example/coupond/couponv1"); out != "" {
		t.Errorf("expected no filterers for a surface, got:\n%s", out)
	}
}
