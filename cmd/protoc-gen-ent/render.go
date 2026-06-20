package main

import (
	"fmt"
	"path"
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// toStorageFields projects a message's fields onto the engine-neutral
// storagegen.Field view used by the fail-closed coverage check (F027 G-002/G-005).
// It derives the same facts from the same proto annotations protoc-gen-storage
// will, so the auto-wire-vs-fail verdict is identical across backends.
func toStorageFields(msg entMessageInfo) []storagegen.Field {
	fks := msgForeignKeyFields(msg)
	out := make([]storagegen.Field, 0, len(msg.Fields))
	for _, f := range msg.Fields {
		out = append(out, storagegen.Field{
			Name:           f.Name,
			IsID:           f.IsID,
			IsTenant:       f.Name == "account_id" || f.SnakeName == "account_id",
			IsSecret:       f.IsSecret,
			IsTags:         f.IsTags,
			OutputOnly:     f.OutputOnly,
			IsRepeated:     f.IsRepeated,
			IsMessage:      f.IsMessage,
			IsEnum:         f.IsEnum,
			IsRelationship: f.HasOne != nil || f.HasMany != nil || f.BelongsTo != nil || f.ManyToMany != nil,
			IsScalarFK:     fks[f.SnakeName] || fks[f.Name],
			HasColumnType:  f.EntType != "",
		})
	}
	return out
}

// targetDialect is the SQL dialect selected via the `dialect` plugin option
// (see main.go). It picks the soft-delete + per-tenant-unique strategy:
// "mysql" → a soft_delete_key discriminator column (entrepo.SoftDeleteUniqueMixin)
// + a 3-column composite unique, since MySQL has no partial indexes; any other
// value ("postgres"/"sqlite") → a partial unique index (WHERE delete_time IS
// NULL). Set once at startup; "postgres" by default.
var targetDialect = "postgres"

// useSoftDeleteSentinel reports whether the sentinel-column strategy (MySQL) is
// in effect rather than the partial-index strategy (PostgreSQL/SQLite).
func useSoftDeleteSentinel() bool { return targetDialect == "mysql" }

// withStorage reports whether protoc-gen-storage (the GORM backend) also runs in
// the same buf.gen invocation, into the same Go package. It is set by the
// `with_storage=true` plugin option (see main.go). When true, protoc-gen-storage
// already emits the package-level AIP-122 resource-name helpers (<R>NamePattern,
// Format<R>Name, Parse<R>Name) and this plugin must NOT re-emit them, or the
// package would have duplicate symbols. An ent-only service (the normal scaffold)
// leaves it false, so this plugin owns those helpers. Default false.
var withStorage = false

// resourcenameIDVarName extracts the last {var} name from a resource name pattern.
// "widgets/{widget}" → "widget"; "projects/{p}/widgets/{widget}" → "widget".
// Mirrors protoc-gen-storage's helper so the two backends produce identical
// Format/Parse helpers for the same pattern.
func resourcenameIDVarName(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			return s[1 : len(s)-1]
		}
	}
	return "id"
}

// entMessageInfo describes a proto resource message for ent schema generation.
type entMessageInfo struct {
	MessageName     string // Go message name (e.g. "APIKey")
	Model           string // resolved (infoblox.storage.v1.model): the backing storage model name (== MessageName for an owner/single-surface resource; the OWNER's name for a surface)
	Fields          []entFieldInfo
	SoftDelete      bool   // true when the message has a delete_time OUTPUT_ONLY Timestamp field (AIP-148)
	HasExpireTime   bool   // true when the message has an expire_time OUTPUT_ONLY Timestamp field (AIP-148)
	HasETag         bool   // true when the message has a string `etag` field (AIP-154); supplied by EtagMixin
	ResourcePattern string // AIP-122 resource name pattern from (google.api.resource), e.g. "apikeys/{api_key}"
}

// isSurface reports whether msg is a projection over ANOTHER message's storage
// model — its (infoblox.storage.v1.model) names a different message (F027 Phase
// 5b). A surface emits a repository adapter + projection over the owner's ent
// type but no schema/table of its own.
func (msg entMessageInfo) isSurface() bool {
	return msg.Model != "" && msg.Model != msg.MessageName
}

// modelType returns the ent type backing msg: its resolved model. For an owner /
// single-surface resource this is its own name; for a surface it is the owner
// message's name — the ent struct, client field and predicate package it projects.
func (msg entMessageInfo) modelType() string {
	if msg.Model == "" {
		return msg.MessageName
	}
	return msg.Model
}

// msgHasResourceName reports whether the message participates in AIP-122 resource
// naming: it carries a (google.api.resource) pattern AND declares an OUTPUT_ONLY
// `name` field. When true, `name` is OUTPUT_ONLY and DERIVED from id (never stored)
// — the ent schema omits the column and fromEnt<R> recomputes it via Format<R>Name,
// mirroring the GORM backend (protoc-gen-storage).
func msgHasResourceName(msg entMessageInfo) bool {
	if msg.ResourcePattern == "" {
		return false
	}
	for _, f := range msg.Fields {
		if f.OutputOnly && f.Name == "name" {
			return true
		}
	}
	return false
}

// entFieldInfo describes a single proto message field for ent schema generation.
type entFieldInfo struct {
	Name       string // proto field name (e.g. "key_value")
	SnakeName  string // snake_case field name (ent storage key)
	EntType    string // ent field constructor: "String", "Int32", "Bool", ...
	IsID       bool   // the resource primary key
	IsRepeated bool   // repeated field — skipped with a TODO comment
	IsMessage  bool   // nested message field — skipped with a TODO comment
	IsTags     bool   // map<string,string> field — emitted as a JSON field
	IsEnum     bool   // enum field — not deterministically wirable (F027 fail-closed)
	IsSecret   bool   // secret field — emitted as _hash + _cipher, never plaintext
	OutputOnly bool   // AIP-203 OUTPUT_ONLY — never written by Create/Update/batch
	// Storage constraints (from field.v1.FieldOptions).
	NotNull bool
	Unique  bool
	Index   bool
	// RelatedType is the Go type name of the message a relationship field points
	// to (e.g. "Vehicle" for a `repeated Vehicle vehicles` has_many). The ent
	// edge target must reference this schema struct, not a name derived from the
	// proto field name (which may be pluralized).
	RelatedType string
	// ORM relationship opts.
	HasOne     *fieldv1.HasOne
	HasMany    *fieldv1.HasMany
	BelongsTo  *fieldv1.BelongsTo
	ManyToMany *fieldv1.ManyToMany
}

// msgHasTenantField reports whether the message has an account_id field, which
// is supplied by TenantMixin rather than emitted directly in Fields().
func msgHasTenantField(msg entMessageInfo) bool {
	for _, f := range msg.Fields {
		if f.Name == "account_id" || f.SnakeName == "account_id" {
			return true
		}
	}
	return false
}

// msgHasSecretField reports whether the message has any secret field, which
// drives both the split _hash/_cipher fields and a _hash index.
func msgHasSecretField(msg entMessageInfo) bool {
	for _, f := range msg.Fields {
		if f.IsSecret {
			return true
		}
	}
	return false
}

// msgHasIndexField reports whether any non-secret field requests a DB index.
func msgHasIndexField(msg entMessageInfo) bool {
	for _, f := range msg.Fields {
		if f.Index && !f.IsSecret {
			return true
		}
	}
	return false
}

// msgHasTenantUnique reports whether the message has account_id AND at least one
// non-id, non-secret unique field. Such fields become a composite unique index
// (account_id, <field>) so uniqueness is enforced per tenant rather than globally
// (GH #44). Only meaningful on tenant-scoped messages.
func msgHasTenantUnique(msg entMessageInfo) bool {
	if !msgHasTenantField(msg) {
		return false
	}
	for _, f := range msg.Fields {
		if f.Unique && !f.IsID && !f.IsSecret {
			return true
		}
	}
	return false
}

// msgHasEdges reports whether any field carries a relationship annotation.
func msgHasEdges(msg entMessageInfo) bool {
	for _, f := range msg.Fields {
		if f.HasOne != nil || f.HasMany != nil || f.BelongsTo != nil || f.ManyToMany != nil {
			return true
		}
	}
	return false
}

// edgeName derives a lowercase edge name from a proto field name.
// e.g. "credit_card" → "credit_card", "CreditCard" → "credit_card".
func edgeName(fieldName string) string {
	return strings.ToLower(fieldName)
}

// entTypeName strips a leading "*" and returns the bare type name.
func entTypeName(goType string) string {
	return strings.TrimPrefix(goType, "*")
}

// edgeTargetType returns the ent schema struct name an edge points to. It
// prefers the related message's actual Go type (captured by main.go) and falls
// back to the capitalized field name only when that is unavailable (e.g. unit
// tests that construct fields directly). Using the field name is unsafe for
// has_many, where the field is pluralized (vehicles → "Vehicles") but the
// schema struct is singular (Vehicle).
func edgeTargetType(f entFieldInfo) string {
	if f.RelatedType != "" {
		return f.RelatedType
	}
	return strings.Title(entTypeName(f.SnakeName))
}

// msgHasScalarField reports whether the message declares snake as an ordinary
// scalar column (not id, not a relationship, not secret). Used to decide
// whether a belongs_to edge must bind its FK to that field via .Field(), so
// ent emits a single Set<FK> setter instead of one for the field and one for
// the edge.
func msgHasScalarField(msg entMessageInfo, snake string) bool {
	for _, f := range msg.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret {
			continue
		}
		if f.SnakeName == snake || f.Name == snake {
			return true
		}
	}
	return false
}

// belongsToInverseRef finds, among siblings, the parent message's has_many edge
// that this belongs_to is the inverse of, returning that edge's name for use as
// .Ref(...). Pairing requires a sibling message named parentType with a
// has_many pointing back to this child; when both annotations name a
// foreign_key, the two must agree. Returns ("", false) when there is no
// matching has_many — the belongs_to then stays a standalone forward edge.
func belongsToInverseRef(child entMessageInfo, bt entFieldInfo, parentType string, siblings []entMessageInfo) (string, bool) {
	if bt.BelongsTo == nil {
		return "", false
	}
	fk := bt.BelongsTo.GetForeignKey()
	for _, sib := range siblings {
		if sib.MessageName != parentType {
			continue
		}
		for _, pf := range sib.Fields {
			if pf.HasMany == nil {
				continue
			}
			if edgeTargetType(pf) != child.MessageName {
				continue
			}
			if fk != "" && pf.HasMany.GetForeignKey() != "" && pf.HasMany.GetForeignKey() != fk {
				continue
			}
			return edgeName(pf.Name), true
		}
	}
	return "", false
}

// renderEntSchema generates the ent/schema/<snake>.go content for a single
// resource message. siblings is the full set of resource messages in the proto
// file, used to pair a belongs_to with its parent's has_many as a proper ent
// inverse edge; pass nil to render edges standalone. Returns an empty string
// when the message has no fields.
func renderEntSchema(msg entMessageInfo, siblings []entMessageInfo) string {
	if len(msg.Fields) == 0 {
		return ""
	}

	hasTenant := msgHasTenantField(msg)
	hasSoftDelete := msg.SoftDelete
	hasSecret := msgHasSecretField(msg)
	hasIndex := msgHasIndexField(msg)
	hasTenantUnique := msgHasTenantUnique(msg)
	hasEdges := msgHasEdges(msg)

	// A resource that is BOTH soft-delete and per-tenant `unique` needs special
	// handling so a unique key can be re-created after the holding row is
	// soft-deleted (otherwise the dead row keeps the key reserved). On
	// PostgreSQL/SQLite the composite unique index is made partial
	// (WHERE delete_time IS NULL); on MySQL (no partial indexes) a soft_delete_key
	// discriminator column joins the composite instead (SoftDeleteUniqueMixin).
	softDeleteUnique := hasSoftDelete && hasTenantUnique
	useSentinel := softDeleteUnique && useSoftDeleteSentinel()
	usePartial := softDeleteUnique && !useSoftDeleteSentinel()

	var b strings.Builder

	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	b.WriteString("package schema\n\n")

	// Imports. The index package is only needed when at least one index is
	// emitted (secret fields or index-annotated fields). The edge package is
	// only needed when relationship annotations are present. The entrepo
	// package is only needed when TenantMixin, SoftDeleteMixin, or EtagMixin is
	// referenced.
	b.WriteString("import (\n")
	b.WriteString("\t\"entgo.io/ent\"\n")
	if hasEdges {
		b.WriteString("\t\"entgo.io/ent/schema/edge\"\n")
	}
	b.WriteString("\t\"entgo.io/ent/schema/field\"\n")
	if hasSecret || hasIndex || hasTenantUnique {
		b.WriteString("\t\"entgo.io/ent/schema/index\"\n")
	}
	if usePartial {
		// entsql.IndexWhere builds the partial unique index (PostgreSQL/SQLite).
		b.WriteString("\t\"entgo.io/ent/dialect/entsql\"\n")
	}
	if hasTenant || hasSoftDelete || msg.HasETag {
		b.WriteString("\n\t\"github.com/infobloxopen/devedge-sdk/persistence/entrepo\"\n")
	}
	b.WriteString(")\n\n")

	// Schema type.
	fmt.Fprintf(&b, "// %s holds the ent schema definition for the %s entity.\n", msg.MessageName, msg.MessageName)
	fmt.Fprintf(&b, "type %s struct {\n", msg.MessageName)
	b.WriteString("\tent.Schema\n")
	b.WriteString("}\n\n")

	// Mixin(): emitted when any mixin is needed (TenantMixin, SoftDeleteMixin,
	// EtagMixin).
	if hasTenant || hasSoftDelete || msg.HasETag {
		fmt.Fprintf(&b, "// Mixin returns the mixins applied to %s.\n", msg.MessageName)
		fmt.Fprintf(&b, "func (%s) Mixin() []ent.Mixin {\n", msg.MessageName)
		b.WriteString("\treturn []ent.Mixin{\n")
		if hasTenant {
			b.WriteString("\t\tentrepo.TenantMixin{},\n")
		}
		if hasSoftDelete {
			b.WriteString("\t\tentrepo.SoftDeleteMixin{},\n")
		}
		if msg.HasETag {
			// EtagMixin supplies the etag column and stamps a fresh token on every
			// Create/Update (AIP-154), mirroring the GORM backend.
			b.WriteString("\t\tentrepo.EtagMixin{},\n")
		}
		if useSentinel {
			// MySQL: SoftDeleteUniqueMixin supplies the soft_delete_key column +
			// the hook that frees a per-tenant unique key on soft-delete (no
			// partial indexes on MySQL).
			b.WriteString("\t\tentrepo.SoftDeleteUniqueMixin{},\n")
		}
		b.WriteString("\t}\n")
		b.WriteString("}\n\n")
	}

	// Fields().
	fmt.Fprintf(&b, "// Fields defines the fields of %s.\n", msg.MessageName)
	fmt.Fprintf(&b, "func (%s) Fields() []ent.Field {\n", msg.MessageName)
	b.WriteString("\treturn []ent.Field{\n")
	for _, f := range msg.Fields {
		switch {
		case f.IsID:
			// ent owns primary keys; pin the storage column to "id" and make it immutable.
			b.WriteString("\t\tfield.String(\"id\").StorageKey(\"id\").Immutable(),\n")
		case f.Name == "account_id" || f.SnakeName == "account_id":
			// Supplied by TenantMixin — never emitted directly.
			continue
		case f.IsTags:
			// Tags (map<string,string>) persist as a JSON field; ent picks the
			// dialect-appropriate column (jsonb on Postgres). Optional so an absent
			// or empty map is allowed.
			fmt.Fprintf(&b, "\t\tfield.JSON(\"%s\", map[string]string{}).Optional(),\n", f.SnakeName)
		case f.IsRepeated:
			if f.HasMany != nil || f.ManyToMany != nil {
				// Relationships are in Edges() — not a field.
				continue
			}
			fmt.Fprintf(&b, "\t\t// TODO: repeated field %s skipped\n", f.Name)
		case f.IsMessage:
			if f.HasOne != nil || f.BelongsTo != nil {
				// Relationships are in Edges() — not a field.
				continue
			}
			fmt.Fprintf(&b, "\t\t// TODO: nested message %s skipped\n", f.Name)
		case f.IsSecret:
			// Secret fields are never stored as plaintext: a lookup hash and a
			// recovery cipher take the place of the raw value.
			fmt.Fprintf(&b, "\t\t// %s is a secret field — stored as hash+cipher, never plaintext\n", f.Name)
			fmt.Fprintf(&b, "\t\tfield.String(\"%s_hash\").Optional().Comment(\"HMAC-SHA256 of %s for lookup\"),\n", f.SnakeName, f.SnakeName)
			fmt.Fprintf(&b, "\t\tfield.String(\"%s_cipher\").Optional().Comment(\"encrypted %s for recovery\"),\n", f.SnakeName, f.SnakeName)
		case f.OutputOnly:
			// AIP-203 OUTPUT_ONLY non-framework field (e.g. the AIP-122 resource
			// `name`): server-computed, derived from id, NEVER stored — matching the
			// GORM backend, which omits OUTPUT_ONLY fields from the model entirely.
			// The framework OUTPUT_ONLY fields (etag, delete_time, expire_time) are
			// owned by their mixins and excluded upstream in main.go, so the only
			// fields reaching here are derived projections that fromEnt<R> recomputes.
			fmt.Fprintf(&b, "\t\t// %s is OUTPUT_ONLY (derived, e.g. AIP-122 name) — never stored; fromEnt%s computes it\n", f.SnakeName, msg.MessageName)
		default:
			// Build the field chain with optional constraints.
			chain := fmt.Sprintf("field.%s(\"%s\")", f.EntType, f.SnakeName)
			if f.NotNull {
				if f.EntType == "String" {
					chain += ".NotEmpty()"
				}
				// For non-string types, not emitting .Optional() achieves NOT NULL.
			} else {
				chain += ".Optional()"
			}
			// A unique field on a tenant-scoped message (one with account_id) must
			// be unique PER TENANT, so it joins account_id in a composite unique
			// index emitted in Indexes() below — NOT a global field-level .Unique(),
			// which would collide across tenants and leak existence (GH #44). On a
			// message with no account_id, a plain global unique index is correct.
			if f.Unique && !hasTenant {
				chain += ".Unique()"
			}
			fmt.Fprintf(&b, "\t\t%s,\n", chain)
		}
	}
	// AIP-148 TTL: expire_time is excluded from msg.Fields (detected in main.go)
	// but still needs a Time field in the ent schema.
	if msg.HasExpireTime {
		b.WriteString("\t\tfield.Time(\"expire_time\").Optional().Nillable().\n")
		b.WriteString("\t\t\tComment(\"AIP-148 TTL: soft-delete rows may be purged after this time.\"),\n")
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	// Edges(): emitted when any relationship annotation is present.
	if hasEdges {
		fmt.Fprintf(&b, "// Edges defines the edges (relationships) of %s.\n", msg.MessageName)
		fmt.Fprintf(&b, "func (%s) Edges() []ent.Edge {\n", msg.MessageName)
		b.WriteString("\treturn []ent.Edge{\n")
		for _, f := range msg.Fields {
			ename := edgeName(f.Name)
			target := edgeTargetType(f)
			switch {
			case f.HasOne != nil:
				// One-to-one, FK on the associated table: a required unique edge.
				fmt.Fprintf(&b, "\t\tedge.To(\"%s\", %s.Type).Unique().Required(),\n", ename, target)
			case f.HasMany != nil:
				// The "one" side of one-to-many owns the assoc edge; the child's
				// belongs_to is its inverse (edge.From below).
				fmt.Fprintf(&b, "\t\tedge.To(\"%s\", %s.Type),\n", ename, target)
			case f.BelongsTo != nil:
				fk := f.BelongsTo.GetForeignKey()
				if ref, ok := belongsToInverseRef(msg, f, target, siblings); ok {
					// Inverse of the parent's has_many. .Field binds the edge's FK to
					// the scalar FK column the proto exposes, so ent emits a single
					// Set<FK> setter (declaring both a scalar field and the edge would
					// otherwise collide on the by-ID setter name).
					line := fmt.Sprintf("edge.From(\"%s\", %s.Type).Ref(\"%s\").Unique()", ename, target, ref)
					if fk != "" && msgHasScalarField(msg, fk) {
						line += fmt.Sprintf(".Field(\"%s\")", fk)
					}
					fmt.Fprintf(&b, "\t\t%s,\n", line)
				} else {
					// No matching has_many on the parent: a self-contained forward
					// edge that needs no counterpart and compiles standalone. Bind
					// the scalar FK when present so the by-ID setter does not collide.
					line := fmt.Sprintf("edge.To(\"%s\", %s.Type).Unique()", ename, target)
					if fk != "" && msgHasScalarField(msg, fk) {
						line += fmt.Sprintf(".Field(\"%s\")", fk)
					}
					fmt.Fprintf(&b, "\t\t%s,\n", line)
				}
			case f.ManyToMany != nil:
				joinType := strings.Title(f.ManyToMany.GetJoinTable())
				fmt.Fprintf(&b, "\t\tedge.To(\"%s\", %s.Type).Through(\"%s\", %s.Type),\n", ename, target, f.ManyToMany.GetJoinTable(), joinType)
			}
		}
		b.WriteString("\t}\n")
		b.WriteString("}\n\n")
	}

	// Indexes(): emitted when there is at least one index. Each secret field gets
	// a key index on its _hash column to support LookupByHash. Non-secret fields
	// with Index=true get a plain index. A unique field on a tenant-scoped message
	// becomes a COMPOSITE unique index (account_id, <field>) — account_id leading —
	// so uniqueness holds per tenant, not globally (GH #44); the index is named
	// ux_<message>_account_<field> to match the GORM backend.
	if hasSecret || hasIndex || hasTenantUnique {
		lowerMsg := strings.ToLower(msg.MessageName)
		fmt.Fprintf(&b, "// Indexes defines the indexes of %s.\n", msg.MessageName)
		fmt.Fprintf(&b, "func (%s) Indexes() []ent.Index {\n", msg.MessageName)
		b.WriteString("\treturn []ent.Index{\n")
		for _, f := range msg.Fields {
			switch {
			case f.IsSecret:
				fmt.Fprintf(&b, "\t\tindex.Fields(\"%s_hash\"),\n", f.SnakeName)
			case hasTenant && f.Unique && !f.IsID:
				// Per-tenant composite unique (account_id leading). Same value may be
				// reused by another tenant; rejected only within one tenant. On a
				// soft-delete resource the key must also be re-creatable after the
				// holder is soft-deleted: partial index on PostgreSQL/SQLite, or a
				// soft_delete_key discriminator on MySQL.
				switch {
				case useSentinel:
					fmt.Fprintf(&b, "\t\tindex.Fields(\"account_id\", \"%s\", \"soft_delete_key\").Unique().StorageKey(\"ux_%s_account_%s\"),\n", f.SnakeName, lowerMsg, f.SnakeName)
				case usePartial:
					fmt.Fprintf(&b, "\t\tindex.Fields(\"account_id\", \"%s\").Unique().\n\t\t\tAnnotations(entsql.IndexWhere(\"delete_time IS NULL\")).StorageKey(\"ux_%s_account_%s\"),\n", f.SnakeName, lowerMsg, f.SnakeName)
				default:
					fmt.Fprintf(&b, "\t\tindex.Fields(\"account_id\", \"%s\").Unique().StorageKey(\"ux_%s_account_%s\"),\n", f.SnakeName, lowerMsg, f.SnakeName)
				}
			case f.Index:
				fmt.Fprintf(&b, "\t\tindex.Fields(\"%s\"),\n", f.SnakeName)
			}
		}
		b.WriteString("\t}\n")
		b.WriteString("}\n")
	}

	return b.String()
}

// renderEntFilterers generates ent/<snake>_filter.ent.go — the WhereAccountID
// (and, for soft-delete resources, WhereDeleteTimeIsNil) query methods that make
// each generated <Resource>Query satisfy entrepo.TenantFilterer /
// entrepo.SoftDeleteFilterer. The TenantMixin / SoftDeleteMixin interceptors call
// these by interface assertion; without them the interceptors have nothing to
// apply and SILENTLY scope nothing (a cross-tenant leak that still compiles — see
// GH #39), so they MUST be generated whenever the mixins are present. The methods
// live in package ent (alongside the generated client) because they extend its
// query types. Returns "" for messages that are neither tenant-scoped nor
// soft-deletable.
func renderEntFilterers(msg entMessageInfo, goImportPath string) string {
	// A surface projects the owner's ent query type; the owner already emits these
	// filterers on it. Re-emitting them here would redeclare the same method on the
	// same <Model>Query type (same package ent) — a compile error. Skip (F027 5b).
	if msg.isSurface() {
		return ""
	}
	hasTenant := msgHasTenantField(msg)
	soft := msg.SoftDelete
	if !hasTenant && !soft {
		return ""
	}
	res := msg.MessageName        // e.g. "APIKey"
	lower := strings.ToLower(res) // ent predicate package, e.g. "apikey"
	entPredImport := path.Dir(goImportPath) + "/ent/" + lower

	var b strings.Builder
	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	b.WriteString("package ent\n\n")
	fmt.Fprintf(&b, "import ent%s %q\n\n", lower, entPredImport)
	if hasTenant {
		b.WriteString("// WhereAccountID scopes the query to rows belonging to the given tenant. It\n")
		b.WriteString("// satisfies entrepo.TenantFilterer, which the TenantMixin interceptor calls to\n")
		b.WriteString("// automatically scope Get/List queries to the calling tenant.\n")
		fmt.Fprintf(&b, "func (q *%sQuery) WhereAccountID(id string) {\n", res)
		fmt.Fprintf(&b, "\tq.Where(ent%s.AccountID(id))\n", lower)
		b.WriteString("}\n")
	}
	if soft {
		if hasTenant {
			b.WriteString("\n")
		}
		b.WriteString("// WhereDeleteTimeIsNil restricts the query to live (not soft-deleted) rows. It\n")
		b.WriteString("// satisfies entrepo.SoftDeleteFilterer, which the SoftDeleteMixin interceptor\n")
		b.WriteString("// calls to hide soft-deleted rows unless the context opts in via show_deleted.\n")
		fmt.Fprintf(&b, "func (q *%sQuery) WhereDeleteTimeIsNil() {\n", res)
		fmt.Fprintf(&b, "\tq.Where(ent%s.DeleteTimeIsNil())\n", lower)
		b.WriteString("}\n")
	}
	return b.String()
}

// renderGenerateFile generates the ent/generate.go content that drives entc
// over the schema package. It is emitted once per proto file.
func renderGenerateFile() string {
	var b strings.Builder
	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	b.WriteString("package ent\n\n")
	b.WriteString("//go:generate go run entgo.io/ent/cmd/ent generate ./schema\n")
	return b.String()
}

// entGoName converts a snake_case proto field name to the CamelCase identifier
// used by the generated protoc-gen-go getters (e.g. "key_prefix" ->
// "KeyPrefix", "fleet_id" -> "FleetId"). protoc-gen-go does NOT apply Go
// initialisms, so "fleet_id" stays "FleetId" — use entSetterGoName for the ent
// side, which does.
func entGoName(snake string) string {
	var b strings.Builder
	upNext := true
	for _, r := range snake {
		if r == '_' {
			upNext = true
			continue
		}
		if upNext && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 'a' + 'A')
		} else {
			b.WriteRune(r)
		}
		upNext = false
	}
	return b.String()
}

// entInitialisms mirrors the acronyms entgo.io/ent registers on its inflection
// ruleset (entc/gen/func.go). ent upper-cases these whole words when it names
// generated setters/fields, so e.g. a "fleet_id" field becomes SetFleetID (not
// SetFleetId). The batch wrapper must spell ent calls the same way or it will
// not compile against the generated client.
var entInitialisms = map[string]string{
	"acl": "ACL", "api": "API", "ascii": "ASCII", "cpu": "CPU", "css": "CSS",
	"dns": "DNS", "eof": "EOF", "guid": "GUID", "html": "HTML", "http": "HTTP",
	"https": "HTTPS", "id": "ID", "ip": "IP", "json": "JSON", "lhs": "LHS",
	"qps": "QPS", "ram": "RAM", "rhs": "RHS", "rpc": "RPC", "sla": "SLA",
	"smtp": "SMTP", "sql": "SQL", "ssh": "SSH", "tcp": "TCP", "tls": "TLS",
	"ttl": "TTL", "udp": "UDP", "ui": "UI", "uid": "UID", "uuid": "UUID",
	"uri": "URI", "url": "URL", "utf8": "UTF8", "vm": "VM", "xml": "XML",
	"xmpp": "XMPP", "xsrf": "XSRF", "xss": "XSS",
}

// entSetterGoName converts a snake_case field name to the CamelCase identifier
// ent uses for its generated Set<Field> methods, applying ent's acronym rules
// (e.g. "fleet_id" -> "FleetID", "display_name" -> "DisplayName"). Use this for
// the ent client calls in the batch wrapper; use entGoName for the proto getter.
func entSetterGoName(snake string) string {
	var b strings.Builder
	for _, part := range strings.Split(snake, "_") {
		if part == "" {
			continue
		}
		if up, ok := entInitialisms[strings.ToLower(part)]; ok {
			b.WriteString(up)
		} else {
			b.WriteString(strings.Title(part))
		}
	}
	return b.String()
}

// renderEntRepository generates the per-resource batch repository wrapper
// (<pkg>/<snake>.batch.ent.go). It embeds the hand-written ent adapter
// (New<R>EntRepository) for the standard methods and adds atomic AIP-137
// BatchGet/BatchUpdate/BatchDelete over an ent transaction, so the ent-backed
// repository satisfies persistence.BatchRepository. Reads ride the existing
// Tenant/SoftDelete query interceptors; mutations carry explicit tenant +
// soft-delete predicates because ent interceptors do not cover mutations.
//
// Requires hand-written New<R>EntRepository and fromEnt<R> in the proto's Go
// package (the standard ent wiring convention). Returns "" for non-resource
// messages (no fields).
func renderEntRepository(msg entMessageInfo, owner entMessageInfo, pkgName, goImportPath string) string {
	if len(msg.Fields) == 0 {
		return ""
	}
	// res names the proto/surface type (wrapper type, constructors, domain type,
	// fromEnt<res> and the per-surface InMask helper); model is the ent type backing
	// it — for a SURFACE the owner, so its client, predicate package and transaction
	// builders. Mutation guards (tenant, soft-delete) follow the OWNER's table; the
	// mask-driven writable/secret fields follow the SURFACE's own fields (F027 5b).
	res := msg.MessageName
	model := owner.MessageName
	lower := strings.ToLower(model)   // ent predicate pkg, e.g. "coupon"
	maskLower := strings.ToLower(res) // per-surface InMask helper prefix (unique per surface)
	hasTenant := msgHasTenantField(owner)
	hasSecret := msgHasSecretField(msg)
	soft := owner.SoftDelete

	entImport := path.Dir(goImportPath) + "/ent"
	entPredImport := entImport + "/" + lower

	// Partition fields into mask-driven writable scalars and secret fields.
	// Skip id, the tenant discriminator, output-only, repeated and message fields.
	var writable, secrets []entFieldInfo
	for _, f := range msg.Fields {
		if f.IsSecret {
			secrets = append(secrets, f)
			continue
		}
		if f.IsID || f.OutputOnly || f.IsRepeated || f.IsMessage {
			continue
		}
		if f.Name == "account_id" || f.SnakeName == "account_id" {
			continue
		}
		writable = append(writable, f)
	}

	var b strings.Builder
	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "package %s\n\n", pkgName)

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"fmt\"\n")
	if soft {
		b.WriteString("\t\"time\"\n")
	}
	b.WriteString("\n")
	if hasTenant {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/middleware\"\n")
	}
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence\"\n")
	if hasSecret {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/secret\"\n")
	}
	fmt.Fprintf(&b, "\tent %q\n", entImport)
	fmt.Fprintf(&b, "\tent%s %q\n", lower, entPredImport)
	b.WriteString(")\n\n")

	// Wrapper type + constructor.
	fmt.Fprintf(&b, "// %sEntRepository adds atomic AIP-137 batch methods to the ent-backed %s,\n", res, res)
	fmt.Fprintf(&b, "// making it a persistence.BatchRepository. Standard methods come from the\n")
	fmt.Fprintf(&b, "// embedded hand-written adapter.\n")
	fmt.Fprintf(&b, "type %sEntRepository struct {\n", res)
	fmt.Fprintf(&b, "\tpersistence.Repository[*%s, string]\n", res)
	b.WriteString("\tclient *ent.Client\n")
	if hasSecret {
		b.WriteString("\tenc    secret.Encryptor\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// New%sEntBatchRepository wraps the hand-written ent adapter with batch support.\n", res)
	if hasSecret {
		fmt.Fprintf(&b, "func New%sEntBatchRepository(client *ent.Client, enc secret.Encryptor) *%sEntRepository {\n", res, res)
		fmt.Fprintf(&b, "\treturn &%sEntRepository{Repository: New%sEntRepository(client, enc), client: client, enc: enc}\n}\n\n", res, res)
	} else {
		fmt.Fprintf(&b, "func New%sEntBatchRepository(client *ent.Client) *%sEntRepository {\n", res, res)
		fmt.Fprintf(&b, "\treturn &%sEntRepository{Repository: New%sEntRepository(client), client: client}\n}\n\n", res, res)
	}

	// Mask helper.
	fmt.Fprintf(&b, "func %sInMask(mask []string, field string) bool {\n", maskLower)
	b.WriteString("\tif len(mask) == 0 {\n\t\treturn true\n\t}\n")
	b.WriteString("\tfor _, m := range mask {\n\t\tif m == field {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n")

	// BatchGet — rides the tenant + soft-delete query interceptors automatically.
	fmt.Fprintf(&b, "func (r *%sEntRepository) BatchGet(ctx context.Context, keys []string) ([]*%s, error) {\n", res, res)
	fmt.Fprintf(&b, "\tif len(keys) == 0 {\n\t\treturn []*%s{}, nil\n\t}\n", res)
	fmt.Fprintf(&b, "\trows, err := r.client.%s.Query().Where(ent%s.IDIn(keys...)).All(ctx)\n", model, lower)
	fmt.Fprintf(&b, "\tif err != nil {\n\t\treturn nil, fmt.Errorf(\"batch get %s: %%w\", err)\n\t}\n", lower)
	fmt.Fprintf(&b, "\tbyID := make(map[string]*%s, len(rows))\n", res)
	fmt.Fprintf(&b, "\tfor _, e := range rows {\n\t\tbyID[e.ID] = fromEnt%s(e)\n\t}\n", res)
	fmt.Fprintf(&b, "\tout := make([]*%s, 0, len(keys))\n", res)
	b.WriteString("\tfor _, k := range keys {\n\t\tp, ok := byID[k]\n\t\tif !ok {\n\t\t\treturn nil, persistence.ErrNotFound\n\t\t}\n\t\tout = append(out, p)\n\t}\n")
	b.WriteString("\treturn out, nil\n}\n\n")

	// BatchUpdate — one transaction; per item an UpdateOneID guarded by tenant +
	// soft-delete predicates (mutations bypass interceptors), with mask-driven sets.
	fmt.Fprintf(&b, "func (r *%sEntRepository) BatchUpdate(ctx context.Context, items []persistence.BatchUpdateItem[*%s, string]) ([]*%s, error) {\n", res, res, res)
	fmt.Fprintf(&b, "\tif len(items) == 0 {\n\t\treturn []*%s{}, nil\n\t}\n", res)
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
	}
	b.WriteString("\ttx, err := r.client.Tx(ctx)\n\tif err != nil {\n\t\treturn nil, fmt.Errorf(\"begin tx: %w\", err)\n\t}\n")
	fmt.Fprintf(&b, "\tout := make([]*%s, 0, len(items))\n", res)
	b.WriteString("\tfor _, it := range items {\n")
	fmt.Fprintf(&b, "\t\tu := tx.%s.UpdateOneID(it.Key)\n", model)
	if hasTenant {
		fmt.Fprintf(&b, "\t\tif tenantID != \"\" {\n\t\t\tu = u.Where(ent%s.AccountID(tenantID))\n\t\t}\n", lower)
	}
	if soft {
		fmt.Fprintf(&b, "\t\tu = u.Where(ent%s.DeleteTimeIsNil())\n", lower)
	}
	for _, f := range writable {
		getName := entGoName(f.SnakeName)       // protoc-gen-go getter (no initialisms)
		setName := entSetterGoName(f.SnakeName) // ent setter (applies initialisms)
		fmt.Fprintf(&b, "\t\tif %sInMask(it.FieldMask, %q) {\n\t\t\tu = u.Set%s(it.Entity.Get%s())\n\t\t}\n", maskLower, f.SnakeName, setName, getName)
	}
	for _, f := range secrets {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\tif %sInMask(it.FieldMask, %q) && it.Entity.Get%s() != \"\" {\n", maskLower, f.SnakeName, getName)
		fmt.Fprintf(&b, "\t\t\th, herr := r.enc.Hash(ctx, it.Entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\tif herr != nil {\n\t\t\t\t_ = tx.Rollback()\n\t\t\t\treturn nil, fmt.Errorf(\"hash %s: %%w\", herr)\n\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tc, cerr := r.enc.Encrypt(ctx, it.Entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\tif cerr != nil {\n\t\t\t\t_ = tx.Rollback()\n\t\t\t\treturn nil, fmt.Errorf(\"encrypt %s: %%w\", cerr)\n\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tu = u.Set%sHash(h).Set%sCipher(c)\n", setName, setName)
		b.WriteString("\t\t}\n")
	}
	b.WriteString("\t\tsaved, serr := u.Save(ctx)\n")
	b.WriteString("\t\tif serr != nil {\n\t\t\t_ = tx.Rollback()\n")
	b.WriteString("\t\t\tif ent.IsNotFound(serr) {\n\t\t\t\treturn nil, persistence.ErrNotFound\n\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\treturn nil, fmt.Errorf(\"batch update %s: %%w\", serr)\n\t\t}\n", lower)
	fmt.Fprintf(&b, "\t\tout = append(out, fromEnt%s(saved))\n", res)
	b.WriteString("\t}\n")
	b.WriteString("\tif err := tx.Commit(); err != nil {\n\t\treturn nil, fmt.Errorf(\"commit tx: %w\", err)\n\t}\n")
	b.WriteString("\treturn out, nil\n}\n\n")

	// BatchDelete — one transactional bulk soft-delete (or hard delete); affected
	// count must equal the de-duplicated key count, else ErrNotFound (rollback).
	fmt.Fprintf(&b, "func (r *%sEntRepository) BatchDelete(ctx context.Context, keys []string) error {\n", res)
	b.WriteString("\tif len(keys) == 0 {\n\t\treturn nil\n\t}\n")
	b.WriteString("\tseen := make(map[string]struct{}, len(keys))\n\tuniq := make([]string, 0, len(keys))\n")
	b.WriteString("\tfor _, k := range keys {\n\t\tif _, ok := seen[k]; ok {\n\t\t\tcontinue\n\t\t}\n\t\tseen[k] = struct{}{}\n\t\tuniq = append(uniq, k)\n\t}\n")
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
	}
	b.WriteString("\ttx, err := r.client.Tx(ctx)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"begin tx: %w\", err)\n\t}\n")
	if soft {
		fmt.Fprintf(&b, "\tupd := tx.%s.Update().Where(ent%s.IDIn(uniq...))\n", model, lower)
		if hasTenant {
			fmt.Fprintf(&b, "\tif tenantID != \"\" {\n\t\tupd = upd.Where(ent%s.AccountID(tenantID))\n\t}\n", lower)
		}
		fmt.Fprintf(&b, "\tupd = upd.Where(ent%s.DeleteTimeIsNil())\n", lower)
		b.WriteString("\tn, derr := upd.SetDeleteTime(time.Now()).Save(ctx)\n")
	} else {
		fmt.Fprintf(&b, "\tdel := tx.%s.Delete().Where(ent%s.IDIn(uniq...))\n", model, lower)
		if hasTenant {
			fmt.Fprintf(&b, "\tif tenantID != \"\" {\n\t\tdel = del.Where(ent%s.AccountID(tenantID))\n\t}\n", lower)
		}
		b.WriteString("\tn, derr := del.Exec(ctx)\n")
	}
	b.WriteString("\tif derr != nil {\n\t\t_ = tx.Rollback()\n")
	fmt.Fprintf(&b, "\t\treturn fmt.Errorf(\"batch delete %s: %%w\", derr)\n\t}\n", lower)
	b.WriteString("\tif n != len(uniq) {\n\t\t_ = tx.Rollback()\n\t\treturn persistence.ErrNotFound\n\t}\n")
	b.WriteString("\treturn tx.Commit()\n}\n\n")

	fmt.Fprintf(&b, "// compile-time check.\n")
	fmt.Fprintf(&b, "var _ persistence.BatchRepository[*%s, string] = (*%sEntRepository)(nil)\n", res, res)

	return b.String()
}

// renderEntColumns generates <pkg>/<snake>.columns.ent.go — the <Msg>EntColumns
// and <Msg>EntJSONColumns maps that translate proto field names to ent DB column
// names for safe AIP-160 filter / AIP-132 order_by parsing on the ent backend.
// An ent-only service (no protoc-gen-storage run) otherwise has no column map and
// cannot wire filtering at all (GH #47). The names are ent-suffixed so they never
// collide with the GORM backend's <Msg>Columns when both generators run — and the
// values genuinely differ (ent stores delete_time as "delete_time"; GORM as
// "deleted_at"). Returns "" for non-resource messages.
func renderEntColumns(msg entMessageInfo, pkgName string) string {
	if len(msg.Fields) == 0 {
		return ""
	}
	res := msg.MessageName

	var b strings.Builder
	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "package %s\n\n", pkgName)

	fmt.Fprintf(&b, "// %sEntColumns maps proto field names to ent DB column names for safe\n", res)
	b.WriteString("// AIP-160 filter / AIP-132 order_by parsing on the ent backend.\n")
	fmt.Fprintf(&b, "var %sEntColumns = map[string]string{\n", res)
	b.WriteString("\t\"id\": \"id\",\n")
	for _, f := range msg.Fields {
		// Tags are handled by the JSON column map below. Skip id, relationships,
		// secrets (stored hashed) and output-only computed fields — none are
		// filterable scalar columns. A scalar FK (belongs_to) stays in.
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsTags || f.IsSecret || f.OutputOnly {
			continue
		}
		fmt.Fprintf(&b, "\t%q: %q,\n", f.Name, f.SnakeName)
	}
	if msg.SoftDelete {
		b.WriteString("\t\"delete_time\": \"delete_time\",\n")
	}
	if msg.HasExpireTime {
		b.WriteString("\t\"expire_time\": \"expire_time\",\n")
	}
	b.WriteString("}\n\n")

	hasTags := false
	for _, f := range msg.Fields {
		if f.IsTags {
			hasTags = true
			break
		}
	}
	if hasTags {
		fmt.Fprintf(&b, "// %sEntJSONColumns maps tag (map<string,string>) field names to ent DB\n", res)
		b.WriteString("// columns for `tags.<key>` filtering on the ent backend.\n")
		fmt.Fprintf(&b, "var %sEntJSONColumns = map[string]string{\n", res)
		for _, f := range msg.Fields {
			if !f.IsTags {
				continue
			}
			fmt.Fprintf(&b, "\t%q: %q,\n", f.Name, f.SnakeName)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// msgHasTags reports whether the message has a map<string,string> (Tags) field.
func msgHasTags(msg entMessageInfo) bool {
	for _, f := range msg.Fields {
		if f.IsTags {
			return true
		}
	}
	return false
}

// msgForeignKeyFields returns the set of scalar field names that are the foreign
// key of a belongs_to edge on this message. ent binds the edge to the scalar via
// .Field(fk), so the FK is written through the edge-backed Set<FK> setter and must
// be set ONLY when non-empty — an empty FK would create a dangling edge / violate
// a foreign-key constraint. Such fields are emitted as guarded conditional sets
// rather than in the unconditional create/update chain.
func msgForeignKeyFields(msg entMessageInfo) map[string]bool {
	fks := map[string]bool{}
	for _, f := range msg.Fields {
		if f.BelongsTo != nil {
			if fk := f.BelongsTo.GetForeignKey(); fk != "" {
				fks[fk] = true
			}
		}
	}
	return fks
}

// renderEntRepoAdapter generates <pkg>/<snake>_repo.ent.go — the repository
// adapter that bridges the generated ent client to persistence.Repository[*<R>,
// string], plus the deterministic fromEnt<R> projection and a LookupBy<Secret>Hash
// helper per secret field. It replaces the hand-written ent_wiring.go: it fills
// the six entrepo.EntRepository closures (Create/Get/List/Update/Delete/Undelete)
// with ConstraintError classification, ent.IsNotFound→persistence.ErrNotFound
// mapping, tenant + soft-delete mutation guards (ent interceptors do not cover
// mutations), the secret hash/cipher block, and AIP-160 filter / paging wired from
// the generated <R>EntColumns maps. Output equivalence with the prior hand-written
// adapter is the bar (F027). Returns "" for non-resource messages (no fields).
func renderEntRepoAdapter(msg entMessageInfo, owner entMessageInfo, pkgName, goImportPath string) string {
	if len(msg.Fields) == 0 {
		return ""
	}
	// res is the proto/surface type — it names the constructor, the domain type the
	// repository serves, the projection (fromEnt<res>), the column map and the owned
	// hooks. model is the ent type backing it — for an owner / single-surface
	// resource it equals res; for a SURFACE (F027 5b) it is the owner message, so the
	// adapter reads/writes the owner's ent client, struct, predicate package and
	// builders while still projecting to/from the surface proto. Mutation semantics
	// (tenant guard, soft-delete, undelete) follow the OWNER's schema; the written
	// and projected fields follow the SURFACE's own field set.
	res := msg.MessageName
	model := owner.MessageName
	lower := strings.ToLower(model) // ent predicate pkg + ent client prefix, e.g. "coupon"
	ownerTenant := msgHasTenantField(owner)
	soft := owner.SoftDelete // Delete_/Undelete_ semantics follow the model's table
	hasSecret := msgHasSecretField(msg)
	hasTags := msgHasTags(msg)
	projectSoft := msg.SoftDelete    // fromEnt projects delete_time only if the surface declares it
	projectExpire := msg.HasExpireTime

	entImport := path.Dir(goImportPath) + "/ent"
	entPredImport := entImport + "/" + lower
	entPredicatePkgImport := entImport + "/predicate"

	// Partition the SURFACE's fields into plain writable scalars, foreign-key scalars
	// (set conditionally), and secret fields. Skip id, the tenant discriminator,
	// output-only, repeated and message fields. Foreign-key detection uses the
	// OWNER's belongs_to bindings, since the columns live on the owner's table.
	fkSet := msgForeignKeyFields(owner)
	var plainWritable, fkWritable, secrets []entFieldInfo
	for _, f := range msg.Fields {
		if f.IsSecret {
			secrets = append(secrets, f)
			continue
		}
		if f.IsID || f.OutputOnly || f.IsRepeated || f.IsMessage {
			continue
		}
		if f.Name == "account_id" || f.SnakeName == "account_id" {
			continue
		}
		if fkSet[f.SnakeName] || fkSet[f.Name] {
			fkWritable = append(fkWritable, f)
			continue
		}
		plainWritable = append(plainWritable, f)
	}

	var b strings.Builder
	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "package %s\n\n", pkgName)

	// Imports — included conditionally so the file compiles with no unused imports.
	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"fmt\"\n")
	if soft {
		b.WriteString("\t\"time\"\n")
	}
	b.WriteString("\n")
	if projectSoft || projectExpire {
		b.WriteString("\t\"google.golang.org/protobuf/types/known/timestamppb\"\n\n")
	}
	if ownerTenant {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/middleware\"\n")
	}
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence\"\n")
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence/entrepo\"\n")
	// resourcename backs the AIP-122 Format<R>Name helper this plugin emits — but
	// only when it owns those helpers (ent-only service). With the GORM backend also
	// present (withStorage), protoc-gen-storage emits both the helpers and their
	// resourcename import in the same package, so this file must NOT import it again
	// (it references the storage-emitted Format<R>Name, which carries its own import).
	if msgHasResourceName(msg) && !withStorage {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence/resourcename\"\n")
	}
	if hasSecret {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/secret\"\n")
	}
	fmt.Fprintf(&b, "\tent %q\n", entImport)
	// The field-predicate package is always needed: every Delete_ references it
	// (ent<lower>.ID for hard delete, ent<lower>.DeleteTimeIsNil for soft delete),
	// as do the tenant guards, Undelete, and LookupBy<Secret>Hash.
	fmt.Fprintf(&b, "\tent%s %q\n", lower, entPredImport)
	fmt.Fprintf(&b, "\tentpredicate %q\n", entPredicatePkgImport)
	b.WriteString(")\n\n")

	// Constructor signature: enc only when there are secret fields.
	fmt.Fprintf(&b, "// New%sEntRepository wires the generated ent client into a\n", res)
	fmt.Fprintf(&b, "// persistence.Repository[*%s, string].\n", res)
	if hasSecret {
		b.WriteString("// enc may be nil only if no secret values will be written.\n")
		fmt.Fprintf(&b, "func New%sEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*%s, string] {\n", res, res)
		fmt.Fprintf(&b, "\treturn &entrepo.EntRepository[*%s, string]{\n", res)
		b.WriteString("\t\tEnc: enc,\n")
	} else {
		fmt.Fprintf(&b, "func New%sEntRepository(client *ent.Client) persistence.Repository[*%s, string] {\n", res, res)
		fmt.Fprintf(&b, "\treturn &entrepo.EntRepository[*%s, string]{\n", res)
	}

	// ---- Create_ ----
	fmt.Fprintf(&b, "\t\tCreate_: func(ctx context.Context, entity *%s) (*%s, error) {\n", res, res)
	if ownerTenant {
		b.WriteString("\t\t\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\t\t\tif entity.GetAccountId() == \"\" && tenantID != \"\" {\n\t\t\t\tentity.AccountId = tenantID\n\t\t\t}\n")
	}
	// Build the create setter chain: id, account_id (if tenant), then writable.
	b.WriteString("\t\t\tb := client." + model + ".Create().\n")
	chain := []string{"SetID(entity.GetId())"}
	if ownerTenant {
		chain = append(chain, "SetAccountID(entity.GetAccountId())")
	}
	for _, f := range plainWritable {
		chain = append(chain, fmt.Sprintf("Set%s(entity.Get%s())", entSetterGoName(f.SnakeName), entGoName(f.SnakeName)))
	}
	for i, c := range chain {
		if i == len(chain)-1 {
			fmt.Fprintf(&b, "\t\t\t\t%s\n", c)
		} else {
			fmt.Fprintf(&b, "\t\t\t\t%s.\n", c)
		}
	}
	// Foreign keys: set only when non-empty (an empty FK would dangle the edge).
	for _, f := range fkWritable {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tif entity.Get%s() != \"\" {\n\t\t\t\tb = b.Set%s(entity.Get%s())\n\t\t\t}\n", getName, setName, getName)
	}
	for _, f := range secrets {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tif enc != nil && entity.Get%s() != \"\" {\n", getName)
		fmt.Fprintf(&b, "\t\t\t\th, herr := enc.Hash(ctx, entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\t\tif herr != nil {\n\t\t\t\t\treturn nil, fmt.Errorf(\"hash %s: %%w\", herr)\n\t\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\t\tc, cerr := enc.Encrypt(ctx, entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\t\tif cerr != nil {\n\t\t\t\t\treturn nil, fmt.Errorf(\"encrypt %s: %%w\", cerr)\n\t\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\t\tb = b.Set%sHash(h).Set%sCipher(c)\n", setName, setName)
		fmt.Fprintf(&b, "\t\t\t\tentity.%s = \"\" // never persist plaintext\n", getName)
		b.WriteString("\t\t\t}\n")
	}
	fmt.Fprintf(&b, "\t\t\tif ToEnt%sOnCreate != nil {\n\t\t\t\tToEnt%sOnCreate(entity, b)\n\t\t\t}\n", res, res)
	b.WriteString("\t\t\tcreated, err := b.Save(ctx)\n")
	b.WriteString("\t\t\tif err != nil {\n")
	b.WriteString("\t\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\t\treturn nil, ce\n\t\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\t\treturn nil, fmt.Errorf(\"create %s: %%w\", err)\n\t\t\t}\n", lower)
	fmt.Fprintf(&b, "\t\t\treturn fromEnt%s(created), nil\n", res)
	b.WriteString("\t\t},\n")

	// ---- Get_ ----
	fmt.Fprintf(&b, "\t\tGet_: func(ctx context.Context, key string) (*%s, error) {\n", res)
	fmt.Fprintf(&b, "\t\t\te, err := client.%s.Get(ctx, key)\n", model)
	b.WriteString("\t\t\tif err != nil {\n\t\t\t\tif ent.IsNotFound(err) {\n\t\t\t\t\treturn nil, persistence.ErrNotFound\n\t\t\t\t}\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\treturn fromEnt%s(e), nil\n", res)
	b.WriteString("\t\t},\n")

	// ---- List_ ----
	fmt.Fprintf(&b, "\t\tList_: func(ctx context.Context, opts persistence.ListOptions) ([]*%s, string, error) {\n", res)
	if soft {
		b.WriteString("\t\t\tif opts.ShowDeleted {\n\t\t\t\tctx = entrepo.WithShowDeleted(ctx)\n\t\t\t}\n")
	}
	fmt.Fprintf(&b, "\t\t\tq := client.%s.Query()\n", model)
	b.WriteString("\t\t\tif opts.Filter != \"\" {\n")
	if hasTags {
		fmt.Fprintf(&b, "\t\t\t\tpred, perr := entrepo.FilterPredicate(opts.Filter, %sEntColumns, %sEntJSONColumns)\n", res, res)
	} else {
		fmt.Fprintf(&b, "\t\t\t\tpred, perr := entrepo.FilterPredicate(opts.Filter, %sEntColumns, nil)\n", res)
	}
	b.WriteString("\t\t\t\tif perr != nil {\n\t\t\t\t\treturn nil, \"\", perr\n\t\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\t\tif pred != nil {\n\t\t\t\t\tq = q.Where(entpredicate.%s(pred))\n\t\t\t\t}\n", model)
	b.WriteString("\t\t\t}\n")
	b.WriteString("\t\t\tif opts.PageSize <= 0 {\n\t\t\t\topts.PageSize = 50\n\t\t\t}\n")
	b.WriteString("\t\t\toffset := 0\n\t\t\tif opts.PageToken != \"\" {\n\t\t\t\tfmt.Sscanf(opts.PageToken, \"%d\", &offset) //nolint:errcheck\n\t\t\t}\n")
	b.WriteString("\t\t\titems, err := q.Limit(opts.PageSize).Offset(offset).All(ctx)\n")
	b.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, \"\", err\n\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\tout := make([]*%s, len(items))\n", res)
	fmt.Fprintf(&b, "\t\t\tfor i, e := range items {\n\t\t\t\tout[i] = fromEnt%s(e)\n\t\t\t}\n", res)
	b.WriteString("\t\t\tnextToken := \"\"\n\t\t\tif len(items) == opts.PageSize {\n\t\t\t\tnextToken = fmt.Sprintf(\"%d\", offset+opts.PageSize)\n\t\t\t}\n")
	b.WriteString("\t\t\treturn out, nextToken, nil\n")
	b.WriteString("\t\t},\n")

	// ---- Update_ ----
	fmt.Fprintf(&b, "\t\tUpdate_: func(ctx context.Context, key string, entity *%s, fieldMask ...string) (*%s, error) {\n", res, res)
	fmt.Fprintf(&b, "\t\t\tu := client.%s.UpdateOneID(key)\n", model)
	for _, f := range plainWritable {
		fmt.Fprintf(&b, "\t\t\tu = u.Set%s(entity.Get%s())\n", entSetterGoName(f.SnakeName), entGoName(f.SnakeName))
	}
	for _, f := range fkWritable {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tif entity.Get%s() != \"\" {\n\t\t\t\tu = u.Set%s(entity.Get%s())\n\t\t\t}\n", getName, setName, getName)
	}
	if ownerTenant {
		// Tenant guard: ent query interceptors do NOT run for mutations, so the
		// account_id predicate must be applied explicitly.
		fmt.Fprintf(&b, "\t\t\tif tenantID := middleware.TenantIDFromContext(ctx); tenantID != \"\" {\n\t\t\t\tu = u.Where(ent%s.AccountID(tenantID))\n\t\t\t}\n", lower)
	}
	for _, f := range secrets {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\t\tif enc != nil && entity.Get%s() != \"\" {\n", getName)
		fmt.Fprintf(&b, "\t\t\t\th, herr := enc.Hash(ctx, entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\t\tif herr != nil {\n\t\t\t\t\treturn nil, fmt.Errorf(\"hash %s: %%w\", herr)\n\t\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\t\tc, cerr := enc.Encrypt(ctx, entity.Get%s())\n", getName)
		fmt.Fprintf(&b, "\t\t\t\tif cerr != nil {\n\t\t\t\t\treturn nil, fmt.Errorf(\"encrypt %s: %%w\", cerr)\n\t\t\t\t}\n", f.SnakeName)
		fmt.Fprintf(&b, "\t\t\t\tu = u.Set%sHash(h).Set%sCipher(c)\n", setName, setName)
		b.WriteString("\t\t\t}\n")
	}
	fmt.Fprintf(&b, "\t\t\tif ToEnt%sOnUpdate != nil {\n\t\t\t\tToEnt%sOnUpdate(entity, u)\n\t\t\t}\n", res, res)
	b.WriteString("\t\t\tupdated, err := u.Save(ctx)\n")
	b.WriteString("\t\t\tif err != nil {\n")
	b.WriteString("\t\t\t\tif ent.IsNotFound(err) {\n\t\t\t\t\treturn nil, persistence.ErrNotFound\n\t\t\t\t}\n")
	b.WriteString("\t\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\t\treturn nil, ce\n\t\t\t\t}\n")
	b.WriteString("\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\treturn fromEnt%s(updated), nil\n", res)
	b.WriteString("\t\t},\n")

	// ---- Delete_ ----
	fmt.Fprintf(&b, "\t\tDelete_: func(ctx context.Context, key string) error {\n")
	if soft {
		fmt.Fprintf(&b, "\t\t\tq := client.%s.UpdateOneID(key)\n", model)
		if ownerTenant {
			fmt.Fprintf(&b, "\t\t\tif tenantID := middleware.TenantIDFromContext(ctx); tenantID != \"\" {\n\t\t\t\tq = q.Where(ent%s.AccountID(tenantID))\n\t\t\t}\n", lower)
		}
		fmt.Fprintf(&b, "\t\t\tq = q.Where(ent%s.DeleteTimeIsNil())\n", lower)
		b.WriteString("\t\t\terr := q.SetDeleteTime(time.Now()).Exec(ctx)\n")
		b.WriteString("\t\t\tif ent.IsNotFound(err) {\n\t\t\t\treturn persistence.ErrNotFound\n\t\t\t}\n\t\t\treturn err\n")
	} else {
		fmt.Fprintf(&b, "\t\t\tdel := client.%s.Delete().Where(ent%s.ID(key))\n", model, lower)
		if ownerTenant {
			fmt.Fprintf(&b, "\t\t\tif tenantID := middleware.TenantIDFromContext(ctx); tenantID != \"\" {\n\t\t\t\tdel = del.Where(ent%s.AccountID(tenantID))\n\t\t\t}\n", lower)
		}
		b.WriteString("\t\t\tn, err := del.Exec(ctx)\n")
		b.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tif n == 0 {\n\t\t\t\treturn persistence.ErrNotFound\n\t\t\t}\n\t\t\treturn nil\n")
	}
	b.WriteString("\t\t},\n")

	// ---- Undelete_ (soft-delete resources only) ----
	if soft {
		fmt.Fprintf(&b, "\t\tUndelete_: func(ctx context.Context, key string) (*%s, error) {\n", res)
		b.WriteString("\t\t\tshowCtx := entrepo.WithShowDeleted(ctx)\n")
		fmt.Fprintf(&b, "\t\t\texisting, err := client.%s.Query().Where(\n\t\t\t\tent%s.ID(key),\n\t\t\t\tent%s.DeleteTimeNotNil(),\n\t\t\t).Only(showCtx)\n", model, lower, lower)
		b.WriteString("\t\t\tif err != nil {\n\t\t\t\tif ent.IsNotFound(err) {\n\t\t\t\t\treturn nil, persistence.ErrNotFound\n\t\t\t\t}\n")
		fmt.Fprintf(&b, "\t\t\t\treturn nil, fmt.Errorf(\"undelete %s: %%w\", err)\n\t\t\t}\n", lower)
		b.WriteString("\t\t\trestored, err := existing.Update().ClearDeleteTime().Save(ctx)\n")
		b.WriteString("\t\t\tif err != nil {\n")
		b.WriteString("\t\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\t\treturn nil, ce\n\t\t\t\t}\n")
		fmt.Fprintf(&b, "\t\t\t\treturn nil, fmt.Errorf(\"undelete %s: %%w\", err)\n\t\t\t}\n", lower)
		fmt.Fprintf(&b, "\t\t\treturn fromEnt%s(restored), nil\n", res)
		b.WriteString("\t\t},\n")
	}

	b.WriteString("\t}\n}\n\n")

	// ---- Owned customization hooks (F027 split-files override seam) ----
	// Nil by default; the developer registers them from their OWN (regen-safe)
	// file. The generated file only declares and calls them, so re-running codegen
	// never disturbs custom logic. No scaffolded file is required.
	fmt.Fprintf(&b, "// FromEnt%sCustom, if set, runs at the end of fromEnt%s to populate fields the\n", res, res)
	b.WriteString("// generator cannot derive (computed/derived values). Register it from your own\n")
	b.WriteString("// (regen-safe) file — e.g. in an init(); this generated file never assigns it.\n")
	fmt.Fprintf(&b, "var FromEnt%sCustom func(e *ent.%s, p *%s)\n\n", res, model, res)
	fmt.Fprintf(&b, "// ToEnt%sOnCreate / ToEnt%sOnUpdate, if set, run just before the ent builder is\n", res, res)
	b.WriteString("// saved, to set columns the generator does not (e.g. a custom-encoded field).\n")
	fmt.Fprintf(&b, "var ToEnt%sOnCreate func(p *%s, b *ent.%sCreate)\n", res, res, model)
	fmt.Fprintf(&b, "var ToEnt%sOnUpdate func(p *%s, u *ent.%sUpdateOne)\n\n", res, res, model)

	// ---- fromEnt<R> projection ----
	fmt.Fprintf(&b, "// fromEnt%s converts a generated ent.%s to the proto *%s. Secret fields are\n", res, model, res)
	b.WriteString("// intentionally omitted — they are never returned from storage after creation.\n")
	fmt.Fprintf(&b, "func fromEnt%s(e *ent.%s) *%s {\n", res, model, res)
	b.WriteString("\tif e == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(&b, "\tp := &%s{\n", res)
	for _, f := range msg.Fields {
		switch {
		case f.IsSecret:
			fmt.Fprintf(&b, "\t\t// %s omitted — secret, never returned\n", f.SnakeName)
		case f.IsRepeated || f.IsMessage:
			// Relationships / repeated are ent edges, not scalar columns.
			continue
		case f.OutputOnly:
			// OUTPUT_ONLY fields are not stored ent columns: etag comes from the mixin
			// (below), the soft-delete/TTL timestamps need timestamppb conversion
			// (below), and a derived field like the AIP-122 `name` is recomputed from
			// id after the literal (see msgHasResourceName below) — none can be read
			// from e.<Field> because the schema no longer carries the column.
			continue
		default:
			fmt.Fprintf(&b, "\t\t%s: e.%s,\n", entGoName(f.SnakeName), entSetterGoName(f.SnakeName))
		}
	}
	if msg.HasETag {
		b.WriteString("\t\tEtag: e.Etag, // AIP-154: the EtagMixin-stamped token a client echoes as If-Match\n")
	}
	b.WriteString("\t}\n")
	// AIP-122: the resource `name` is OUTPUT_ONLY and DERIVED from id — recompute it
	// on read (it is never stored), mirroring protoc-gen-storage's fromModel.
	if msgHasResourceName(msg) {
		fmt.Fprintf(&b, "\tp.Name = Format%sName(e.ID)\n", res)
	}
	if projectSoft {
		b.WriteString("\tif e.DeleteTime != nil {\n\t\tp.DeleteTime = timestamppb.New(*e.DeleteTime)\n\t}\n")
	}
	if msg.HasExpireTime {
		b.WriteString("\tif e.ExpireTime != nil {\n\t\tp.ExpireTime = timestamppb.New(*e.ExpireTime)\n\t}\n")
	}
	fmt.Fprintf(&b, "\tif FromEnt%sCustom != nil {\n\t\tFromEnt%sCustom(e, p)\n\t}\n", res, res)
	b.WriteString("\treturn p\n}\n")

	// ---- AIP-122 resource-name helpers (<R>NamePattern / Format<R>Name / Parse<R>Name) ----
	// Emitted only when this is an ent-only service (withStorage=false). When the GORM
	// backend also runs in the same package (with_storage=true), protoc-gen-storage
	// already owns these symbols and re-emitting them here would duplicate them.
	// fromEnt<R> above references Format<R>Name regardless — in the both-backends case
	// it resolves to the storage-emitted helper in the same package.
	if msgHasResourceName(msg) && !withStorage {
		idVar := resourcenameIDVarName(msg.ResourcePattern)
		fmt.Fprintf(&b, "\n// %sNamePattern is the AIP-122 resource name pattern for %s.\n", res, res)
		fmt.Fprintf(&b, "const %sNamePattern = %q\n\n", res, msg.ResourcePattern)
		fmt.Fprintf(&b, "// Format%sName builds the AIP-122 resource name for the given ID.\n", res)
		fmt.Fprintf(&b, "func Format%sName(id string) string {\n", res)
		fmt.Fprintf(&b, "\tname, _ := resourcename.Format(%sNamePattern, map[string]string{%q: id})\n", res, idVar)
		b.WriteString("\treturn name\n}\n\n")
		fmt.Fprintf(&b, "// Parse%sName extracts the resource ID from the given resource name.\n", res)
		fmt.Fprintf(&b, "func Parse%sName(name string) (string, error) {\n", res)
		fmt.Fprintf(&b, "\treturn resourcename.IDFromName(%sNamePattern, name)\n}\n", res)
	}

	// ---- LookupBy<Secret>Hash helpers ----
	for _, f := range secrets {
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\n// LookupBy%sHash finds a %s by the HMAC-SHA256 hash of its %s.\n", setName, res, f.SnakeName)
		b.WriteString("// Returns persistence.ErrNotFound when no record matches or hash is empty.\n")
		fmt.Fprintf(&b, "func LookupBy%sHash(ctx context.Context, client *ent.Client, hash string) (*%s, error) {\n", setName, res)
		b.WriteString("\tif hash == \"\" {\n\t\treturn nil, persistence.ErrNotFound\n\t}\n")
		fmt.Fprintf(&b, "\te, err := client.%s.Query().Where(ent%s.%sHash(hash)).Only(ctx)\n", model, lower, setName)
		b.WriteString("\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, persistence.ErrNotFound\n\t\t}\n\t\treturn nil, err\n\t}\n")
		fmt.Fprintf(&b, "\treturn fromEnt%s(e), nil\n}\n", res)
	}

	return b.String()
}
