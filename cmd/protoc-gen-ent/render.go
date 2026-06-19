package main

import (
	"fmt"
	"path"
	"strings"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// entMessageInfo describes a proto resource message for ent schema generation.
type entMessageInfo struct {
	MessageName   string // Go message name (e.g. "APIKey")
	Fields        []entFieldInfo
	SoftDelete    bool // true when the message has a delete_time OUTPUT_ONLY Timestamp field (AIP-148)
	HasExpireTime bool // true when the message has an expire_time OUTPUT_ONLY Timestamp field (AIP-148)
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

	var b strings.Builder

	b.WriteString("// Code generated by protoc-gen-ent. DO NOT EDIT.\n")
	b.WriteString("package schema\n\n")

	// Imports. The index package is only needed when at least one index is
	// emitted (secret fields or index-annotated fields). The edge package is
	// only needed when relationship annotations are present. The entrepo
	// package is only needed when TenantMixin or SoftDeleteMixin is referenced.
	b.WriteString("import (\n")
	b.WriteString("\t\"entgo.io/ent\"\n")
	if hasEdges {
		b.WriteString("\t\"entgo.io/ent/schema/edge\"\n")
	}
	b.WriteString("\t\"entgo.io/ent/schema/field\"\n")
	if hasSecret || hasIndex || hasTenantUnique {
		b.WriteString("\t\"entgo.io/ent/schema/index\"\n")
	}
	if hasTenant || hasSoftDelete {
		b.WriteString("\n\t\"github.com/infobloxopen/devedge-sdk/persistence/entrepo\"\n")
	}
	b.WriteString(")\n\n")

	// Schema type.
	fmt.Fprintf(&b, "// %s holds the ent schema definition for the %s entity.\n", msg.MessageName, msg.MessageName)
	fmt.Fprintf(&b, "type %s struct {\n", msg.MessageName)
	b.WriteString("\tent.Schema\n")
	b.WriteString("}\n\n")

	// Mixin(): emitted when any mixin is needed (TenantMixin, SoftDeleteMixin).
	if hasTenant || hasSoftDelete {
		fmt.Fprintf(&b, "// Mixin returns the mixins applied to %s.\n", msg.MessageName)
		fmt.Fprintf(&b, "func (%s) Mixin() []ent.Mixin {\n", msg.MessageName)
		b.WriteString("\treturn []ent.Mixin{\n")
		if hasTenant {
			b.WriteString("\t\tentrepo.TenantMixin{},\n")
		}
		if hasSoftDelete {
			b.WriteString("\t\tentrepo.SoftDeleteMixin{},\n")
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
				// reused by another tenant; rejected only within one tenant.
				fmt.Fprintf(&b, "\t\tindex.Fields(\"account_id\", \"%s\").Unique().StorageKey(\"ux_%s_account_%s\"),\n", f.SnakeName, lowerMsg, f.SnakeName)
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
func renderEntRepository(msg entMessageInfo, pkgName, goImportPath string) string {
	if len(msg.Fields) == 0 {
		return ""
	}
	res := msg.MessageName        // e.g. "APIKey"
	lower := strings.ToLower(res) // ent predicate pkg + helper prefix, e.g. "apikey"
	hasTenant := msgHasTenantField(msg)
	hasSecret := msgHasSecretField(msg)
	soft := msg.SoftDelete

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
	fmt.Fprintf(&b, "func %sInMask(mask []string, field string) bool {\n", lower)
	b.WriteString("\tif len(mask) == 0 {\n\t\treturn true\n\t}\n")
	b.WriteString("\tfor _, m := range mask {\n\t\tif m == field {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n")

	// BatchGet — rides the tenant + soft-delete query interceptors automatically.
	fmt.Fprintf(&b, "func (r *%sEntRepository) BatchGet(ctx context.Context, keys []string) ([]*%s, error) {\n", res, res)
	fmt.Fprintf(&b, "\tif len(keys) == 0 {\n\t\treturn []*%s{}, nil\n\t}\n", res)
	fmt.Fprintf(&b, "\trows, err := r.client.%s.Query().Where(ent%s.IDIn(keys...)).All(ctx)\n", res, lower)
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
	fmt.Fprintf(&b, "\t\tu := tx.%s.UpdateOneID(it.Key)\n", res)
	if hasTenant {
		fmt.Fprintf(&b, "\t\tif tenantID != \"\" {\n\t\t\tu = u.Where(ent%s.AccountID(tenantID))\n\t\t}\n", lower)
	}
	if soft {
		fmt.Fprintf(&b, "\t\tu = u.Where(ent%s.DeleteTimeIsNil())\n", lower)
	}
	for _, f := range writable {
		getName := entGoName(f.SnakeName)       // protoc-gen-go getter (no initialisms)
		setName := entSetterGoName(f.SnakeName) // ent setter (applies initialisms)
		fmt.Fprintf(&b, "\t\tif %sInMask(it.FieldMask, %q) {\n\t\t\tu = u.Set%s(it.Entity.Get%s())\n\t\t}\n", lower, f.SnakeName, setName, getName)
	}
	for _, f := range secrets {
		getName := entGoName(f.SnakeName)
		setName := entSetterGoName(f.SnakeName)
		fmt.Fprintf(&b, "\t\tif %sInMask(it.FieldMask, %q) && it.Entity.Get%s() != \"\" {\n", lower, f.SnakeName, getName)
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
		fmt.Fprintf(&b, "\tupd := tx.%s.Update().Where(ent%s.IDIn(uniq...))\n", res, lower)
		if hasTenant {
			fmt.Fprintf(&b, "\tif tenantID != \"\" {\n\t\tupd = upd.Where(ent%s.AccountID(tenantID))\n\t}\n", lower)
		}
		fmt.Fprintf(&b, "\tupd = upd.Where(ent%s.DeleteTimeIsNil())\n", lower)
		b.WriteString("\tn, derr := upd.SetDeleteTime(time.Now()).Save(ctx)\n")
	} else {
		fmt.Fprintf(&b, "\tdel := tx.%s.Delete().Where(ent%s.IDIn(uniq...))\n", res, lower)
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
