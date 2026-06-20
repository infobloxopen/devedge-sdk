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
	out := renderEntSchema(msg, nil)

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

// AIP-122: an OUTPUT_ONLY `name` (derived from id) must NOT be stored as an ent
// column — it is recomputed on read by fromEnt<R>, mirroring the GORM backend
// (protoc-gen-storage omits OUTPUT_ONLY fields from the model). A plain `name`
// WITHOUT OUTPUT_ONLY is still a normal stored column (covered above).
func TestRenderEntSchema_outputOnlyNameNotStored(t *testing.T) {
	msg := entMessageInfo{
		MessageName:     "Widget",
		ResourcePattern: "widgets/{widget}",
		Fields: []entFieldInfo{
			{Name: "name", SnakeName: "name", EntType: "String", OutputOnly: true},
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "display_name", SnakeName: "display_name", EntType: "String"},
		},
	}
	out := renderEntSchema(msg, nil)
	// The derived name must not become a stored column.
	mustNotContain(t, out, `field.String("name")`)
	// A non-OUTPUT_ONLY scalar is still stored.
	mustContain(t, out, `field.String("display_name").Optional()`)
}

// A map<string,string> field is the Tags kind: an Optional JSON ent field,
// never a skipped nested message.
func TestRenderEntSchema_tagsField(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Resource",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "tags", SnakeName: "tags", IsTags: true},
		},
	}
	out := renderEntSchema(msg, nil)
	mustContain(t, out, `field.JSON("tags", map[string]string{}).Optional()`)
	mustNotContain(t, out, "TODO: nested message tags skipped")
}

// The generated batch wrapper sets a tags field via the generic writable path
// (proto GetTags() and ent SetTags() are both map[string]string — no conversion).
func TestRenderEntRepository_tagsInBatchUpdate(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Resource",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "tags", SnakeName: "tags", IsTags: true},
		},
	}
	out := renderEntRepository(msg, msg, "resv1", "example/res/v1")
	mustContain(t, out, "u = u.SetTags(it.Entity.GetTags())")
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
	out := renderEntSchema(msg, nil)

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
	out := renderEntSchema(apiKeyMessage(), nil)

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
	out := renderEntSchema(msg, nil)

	mustContain(t, out, "// TODO: repeated field tags skipped")
	mustContain(t, out, "// TODO: nested message meta skipped")
	// No real field emitted for the skipped ones.
	mustNotContain(t, out, `field.String("tags")`)
	mustNotContain(t, out, `field.String("meta")`)
}

func TestRenderEntSchema_emptyMessage(t *testing.T) {
	out := renderEntSchema(entMessageInfo{MessageName: "Empty"}, nil)
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
	out := renderEntSchema(msg, nil)
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
	out := renderEntSchema(msg, nil)
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

// Regression for issue #26: a belongs_to with no reciprocal has_many on the
// parent (no siblings) must emit a self-contained forward edge
// (edge.To(...).Unique()), NOT an inverse edge.From(...).Ref(...) — the latter
// requires a matching edge.To on the parent type that is absent, so ent codegen
// aborts with "edge <x> is missing for inverse edge". (When the parent DOES
// declare the has_many, the inverse IS emitted — see the pairing test below.)
func TestRenderEntSchema_belongsToEdge(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Booking",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "venue", SnakeName: "venue", EntType: "String",
				IsMessage: true, RelatedType: "Venue", BelongsTo: &fieldv1.BelongsTo{ForeignKey: "venue_id"}},
		},
	}
	out := renderEntSchema(msg, nil)
	mustContain(t, out, "func (Booking) Edges() []ent.Edge {")
	mustContain(t, out, `edge.To("venue", Venue.Type).Unique()`)
	// Must NOT emit the inverse edge that needs an absent counterpart.
	mustNotContain(t, out, ".Ref(")
	// No scalar venue_id field present → no .Field() binding.
	mustNotContain(t, out, ".Field(")
}

// Regression for issue #30: a has_many edge must reference the related message's
// singular ent type (Vehicle.Type, captured in RelatedType) — NOT a name derived
// from the pluralized field name, which produced the undefined Vehicles.Type and
// broke `go generate ./ent`.
func TestRenderEntSchema_hasManyUsesSingularRelatedType(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Fleet",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "vehicles", SnakeName: "vehicles", IsRepeated: true, IsMessage: true,
				RelatedType: "Vehicle", HasMany: &fieldv1.HasMany{ForeignKey: "fleet_id"}},
		},
	}
	out := renderEntSchema(msg, nil)
	mustContain(t, out, `edge.To("vehicles", Vehicle.Type)`)
	mustNotContain(t, out, "Vehicles.Type") // the pluralized, undefined type
}

// Regression for issues #30 (inverse pairing) and #31 (scalar-FK/edge collision):
// when a belongs_to's parent declares the reciprocal has_many, the belongs_to is
// emitted as the inverse edge.From(...).Ref(...), and the scalar FK is bound via
// .Field() so ent generates a single SetFleetID rather than colliding setters.
func TestRenderEntSchema_belongsToPairsWithHasManyAndBindsFK(t *testing.T) {
	parent := entMessageInfo{
		MessageName: "Fleet",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "vehicles", SnakeName: "vehicles", IsRepeated: true, IsMessage: true,
				RelatedType: "Vehicle", HasMany: &fieldv1.HasMany{ForeignKey: "fleet_id"}},
		},
	}
	child := entMessageInfo{
		MessageName: "Vehicle",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "fleet_id", SnakeName: "fleet_id", EntType: "String"},
			{Name: "fleet", SnakeName: "fleet", IsMessage: true, RelatedType: "Fleet",
				BelongsTo: &fieldv1.BelongsTo{ForeignKey: "fleet_id"}},
		},
	}
	siblings := []entMessageInfo{parent, child}

	out := renderEntSchema(child, siblings)
	mustContain(t, out, `edge.From("fleet", Fleet.Type).Ref("vehicles").Unique().Field("fleet_id")`)
	mustNotContain(t, out, `edge.To("fleet"`)

	pout := renderEntSchema(parent, siblings)
	mustContain(t, pout, `edge.To("vehicles", Vehicle.Type)`)
}

// A standalone belongs_to (no reciprocal has_many) with a scalar FK present still
// binds the FK via .Field() to avoid the duplicate SetFleetID collision (#31),
// while staying a self-contained forward edge.
func TestRenderEntSchema_belongsToStandaloneBindsScalarFK(t *testing.T) {
	child := entMessageInfo{
		MessageName: "Vehicle",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "fleet_id", SnakeName: "fleet_id", EntType: "String"},
			{Name: "fleet", SnakeName: "fleet", IsMessage: true, RelatedType: "Fleet",
				BelongsTo: &fieldv1.BelongsTo{ForeignKey: "fleet_id"}},
		},
	}
	out := renderEntSchema(child, nil) // no siblings → standalone
	mustContain(t, out, `edge.To("fleet", Fleet.Type).Unique().Field("fleet_id")`)
	mustNotContain(t, out, ".Ref(")
}

// entSetterGoName must match ent's setter naming (applies Go initialisms), so the
// batch wrapper calls the methods ent actually generates (issue surfaced by the
// fleet_id FK: ent emits SetFleetID, not SetFleetId).
func TestEntSetterGoName(t *testing.T) {
	cases := map[string]string{
		"fleet_id":     "FleetID",
		"display_name": "DisplayName",
		"vin":          "Vin",
		"key_prefix":   "KeyPrefix",
		"api_url":      "APIURL",
	}
	for in, want := range cases {
		if got := entSetterGoName(in); got != want {
			t.Errorf("entSetterGoName(%q) = %q, want %q", in, got, want)
		}
	}
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
	out := renderEntRepository(msg, msg, "apikeyv1", "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1")

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

// A message with a string `etag` field (HasETag, set in main.go) must embed
// entrepo.EtagMixin so the etag column + auto-stamping hook are generated — and
// must NOT emit a stray field.String("etag") (the mixin owns it). This is the
// ent-backend half of the AIP-154 ETag parity with the GORM storage layer (#49).
func TestRenderEntSchema_etagMixin(t *testing.T) {
	// main.go detects the etag field, sets HasETag, and does NOT add it to
	// Fields — so the mixin owns it. Model that here (no etag entFieldInfo).
	msg := entMessageInfo{
		MessageName: "APIKey",
		HasETag:     true,
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "label", SnakeName: "label", EntType: "String"},
		},
	}
	out := renderEntSchema(msg, nil)

	mustContain(t, out, "func (APIKey) Mixin() []ent.Mixin {")
	mustContain(t, out, "entrepo.TenantMixin{},")
	mustContain(t, out, "entrepo.EtagMixin{},")
	mustContain(t, out, "github.com/infobloxopen/devedge-sdk/persistence/entrepo")
	// The mixin owns the column — no stray etag field in Fields().
	mustNotContain(t, out, `field.String("etag")`)
}

// EtagMixin must only appear when the message actually has an etag field; a
// resource without one gets neither the mixin nor (absent a tenant) the import.
func TestRenderEntSchema_noEtagNoMixin(t *testing.T) {
	msg := entMessageInfo{
		MessageName: "Widget",
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "name", SnakeName: "name", EntType: "String"},
		},
	}
	out := renderEntSchema(msg, nil)
	mustNotContain(t, out, "EtagMixin")
}

// softDeleteUniqueMsg is a soft-delete, per-tenant-unique resource: the exact
// shape where a unique key must be re-creatable after the holder is soft-deleted.
func softDeleteUniqueMsg() entMessageInfo {
	return entMessageInfo{
		MessageName: "Order",
		SoftDelete:  true,
		Fields: []entFieldInfo{
			{Name: "id", SnakeName: "id", EntType: "String", IsID: true},
			{Name: "account_id", SnakeName: "account_id", EntType: "String"},
			{Name: "source_ref", SnakeName: "source_ref", EntType: "String", Unique: true},
		},
	}
}

// Default dialect (postgres/sqlite): the per-tenant unique on a soft-delete
// resource is a PARTIAL index (WHERE delete_time IS NULL) — no sentinel column.
func TestRenderEntSchema_softDeleteUnique_partial(t *testing.T) {
	targetDialect = "postgres"
	out := renderEntSchema(softDeleteUniqueMsg(), nil)

	mustContain(t, out, `entsql.IndexWhere("delete_time IS NULL")`)
	mustContain(t, out, `"entgo.io/ent/dialect/entsql"`)
	mustContain(t, out, `index.Fields("account_id", "source_ref").Unique()`)
	mustNotContain(t, out, "soft_delete_key")
	mustNotContain(t, out, "SoftDeleteUniqueMixin")
}

// dialect=mysql: no partial index — a soft_delete_key discriminator joins the
// composite unique, and SoftDeleteUniqueMixin maintains it.
func TestRenderEntSchema_softDeleteUnique_sentinel(t *testing.T) {
	targetDialect = "mysql"
	defer func() { targetDialect = "postgres" }()
	out := renderEntSchema(softDeleteUniqueMsg(), nil)

	mustContain(t, out, `index.Fields("account_id", "source_ref", "soft_delete_key").Unique()`)
	mustContain(t, out, "entrepo.SoftDeleteUniqueMixin{},")
	mustNotContain(t, out, "IndexWhere")
	mustNotContain(t, out, "entgo.io/ent/dialect/entsql")
}

// A per-tenant unique on a resource WITHOUT soft-delete is unchanged on every
// dialect: a plain composite unique, no partial clause, no sentinel column.
func TestRenderEntSchema_uniqueNoSoftDelete_unchanged(t *testing.T) {
	msg := softDeleteUniqueMsg()
	msg.SoftDelete = false
	for _, d := range []string{"postgres", "mysql"} {
		targetDialect = d
		out := renderEntSchema(msg, nil)
		mustContain(t, out, `index.Fields("account_id", "source_ref").Unique().StorageKey("ux_order_account_source_ref")`)
		mustNotContain(t, out, "soft_delete_key")
		mustNotContain(t, out, "IndexWhere")
		mustNotContain(t, out, "SoftDeleteUniqueMixin")
	}
	targetDialect = "postgres"
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
