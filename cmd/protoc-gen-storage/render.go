package main

import (
	"fmt"
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
)

// targetDialect is the SQL dialect selected via the `dialect` plugin option (see
// main.go). For a resource that is BOTH soft-delete and per-tenant `unique`, it
// picks how the key is freed on soft-delete: "mysql" → a soft_delete_key
// discriminator column joins the composite unique (MySQL has no partial
// indexes); any other value ("postgres"/"sqlite") → a partial unique index
// (WHERE deleted_at IS NULL). "postgres" by default.
var targetDialect = "postgres"

// useSoftDeleteSentinel reports whether the sentinel-column strategy (MySQL) is
// in effect rather than the partial-index strategy (PostgreSQL/SQLite).
func useSoftDeleteSentinel() bool { return targetDialect == "mysql" }

// msgHasTenantUnique reports whether the message has account_id AND at least one
// persisted per-tenant `unique` field (so a composite unique index is emitted).
func msgHasTenantUnique(msg messageInfo) bool {
	if !msgHasTenantField(msg) {
		return false
	}
	for _, f := range msg.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret || f.IsOutputOnly {
			continue
		}
		if f.Unique {
			return true
		}
	}
	return false
}

// hasSecretFields returns true if any field across all messages is marked secret.
func hasSecretFields(messages []messageInfo) bool {
	for _, msg := range messages {
		for _, f := range msg.Fields {
			if f.IsSecret {
				return true
			}
		}
	}
	return false
}

// msgHasTenantField returns true if the message has a field named "account_id".
func msgHasTenantField(msg messageInfo) bool {
	for _, f := range msg.Fields {
		if f.Name == "account_id" || f.SnakeName == "account_id" {
			return true
		}
	}
	return false
}

// hasSoftDeleteMsg returns true when at least one message opts into soft-delete.
func hasSoftDeleteMsg(messages []messageInfo) bool {
	for _, msg := range messages {
		if msg.SoftDelete {
			return true
		}
	}
	return false
}

// hasExpireTimeMsg returns true when at least one message has an expire_time field.
func hasExpireTimeMsg(messages []messageInfo) bool {
	for _, msg := range messages {
		if msg.HasExpireTime {
			return true
		}
	}
	return false
}

func hasETagMsg(messages []messageInfo) bool {
	for _, msg := range messages {
		if msg.HasETag {
			return true
		}
	}
	return false
}

// hasTagsFields reports whether any field across all messages is a tags
// (map<string,string>) field, which makes the generated file import types.
func hasTagsFields(messages []messageInfo) bool {
	for _, msg := range messages {
		for _, f := range msg.Fields {
			if f.IsTags {
				return true
			}
		}
	}
	return false
}

// msgHasTags reports whether the message has a tags (map<string,string>) field,
// which makes List parse tag (JSON) predicates dialect-aware.
func msgHasTags(msg messageInfo) bool {
	for _, f := range msg.Fields {
		if f.IsTags {
			return true
		}
	}
	return false
}

// messageInfo describes a proto message for storage code generation.
type messageInfo struct {
	MessageName     string
	Model           string // resolved (infoblox.storage.v1.model): the backing storage model name (== MessageName for owner/single-surface; owner's name for a surface)
	PbPkgName       string // Go package name of the generated proto package (e.g. "widgetsv1")
	PbImportPath    string // Go import path of the generated proto package
	Fields          []fieldInfo
	ResourcePattern string // AIP-122 resource name pattern, e.g. "widgets/{widget}"
	// AIP-148 soft-delete opt-in: set when the message has a delete_time OUTPUT_ONLY Timestamp field.
	SoftDelete bool
	// AIP-148 TTL: set when the message has an expire_time OUTPUT_ONLY Timestamp field.
	HasExpireTime bool
	// AIP-154 ETag: set when the message has a string `etag` field. The storage
	// layer stamps a fresh token on every write and surfaces it on read.
	HasETag bool
}

// isSurface reports whether msg is a projection over ANOTHER message's storage
// model — its (infoblox.storage.v1.model) names a different message (F027 Phase
// 5b). A surface emits a repository adapter + projection over the owner's GORM
// type but no GORM model struct / table of its own.
func (msg messageInfo) isSurface() bool {
	return msg.Model != "" && msg.Model != msg.MessageName
}

// modelType returns the GORM model type name backing msg. For an owner /
// single-surface resource this is its own name; for a surface it is the owner
// message's name — the GORM struct it projects.
func (msg messageInfo) modelType() string {
	if msg.Model == "" {
		return msg.MessageName
	}
	return msg.Model
}

// fieldInfo describes a single proto message field.
type fieldInfo struct {
	Name        string
	GoFieldName string // Go struct field name (e.g. "PageSize" for proto "page_size")
	GoType      string
	// RelatedGoType is the Go type name of the message a relationship field points
	// at (e.g. "Destination" for a belongs_to Destination). The generated GORM
	// association references the related model type "<RelatedGoType>Model".
	RelatedGoType string
	SnakeName     string
	IsID          bool // this field is the resource primary key
	IsRepeated    bool // repeated field — skipped in GORM model with a TODO
	IsMessage     bool // nested message field — skipped with a TODO
	IsEnum        bool // enum field — not auto-wired (F027 fail-closed)
	IsTags        bool // map<string,string> field — persisted as a types.Tags JSONB column
	IsSecret      bool // secret field — stored as hash+cipher columns; plaintext never persisted
	IsOutputOnly  bool // OUTPUT_ONLY field (google.api.field_behavior) — computed, not persisted
	// Storage constraints (from field.v1.FieldOptions).
	NotNull    bool
	Unique     bool
	Index      bool
	ColumnName string // overrides SnakeName in the GORM column tag
	ColumnType string // overrides the GORM type tag
	// ORM relationship opts.
	HasOne     *fieldv1.HasOne
	HasMany    *fieldv1.HasMany
	BelongsTo  *fieldv1.BelongsTo
	ManyToMany *fieldv1.ManyToMany
}

// msgForeignKeyFields returns the set of snake_case scalar field names that are
// the foreign key of a belongs_to edge on this message. Used by toStorageFields
// to mark them IsScalarFK so the fail-closed classifier passes them.
func msgForeignKeyFields(msg messageInfo) map[string]bool {
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

// toStorageFields projects a message's fields onto the engine-neutral
// storagegen.Field view used by the fail-closed coverage check (F027 G-002/G-005).
func toStorageFields(msg messageInfo) []storagegen.Field {
	fks := msgForeignKeyFields(msg)
	out := make([]storagegen.Field, 0, len(msg.Fields))
	for _, f := range msg.Fields {
		out = append(out, storagegen.Field{
			Name:           f.Name,
			IsID:           f.IsID,
			IsTenant:       f.Name == "account_id" || f.SnakeName == "account_id",
			IsSecret:       f.IsSecret,
			IsTags:         f.IsTags,
			OutputOnly:     f.IsOutputOnly,
			IsRepeated:     f.IsRepeated,
			IsMessage:      f.IsMessage,
			IsEnum:         f.IsEnum,
			IsRelationship: f.HasOne != nil || f.HasMany != nil || f.BelongsTo != nil || f.ManyToMany != nil,
			IsScalarFK:     fks[f.SnakeName] || fks[f.Name],
			HasColumnType:  f.GoType != "" && f.GoType != "interface{}",
		})
	}
	return out
}

// renderStorageFile generates the .storage.go content for the given storage
// package and messages. ownerByName maps each message name to itself (for
// owners/single-surface) so renderMessage can look up the owner for surfaces.
// Returns an empty string when messages is empty.
func renderStorageFile(storagePkgName string, messages []messageInfo, ownerByName map[string]messageInfo) string {
	if len(messages) == 0 {
		return ""
	}
	if ownerByName == nil {
		// Build a default owner map (all owners are themselves) for callers that
		// pre-date the multi-surface parameter (unit tests, etc.).
		ownerByName = make(map[string]messageInfo, len(messages))
		for _, m := range messages {
			ownerByName[m.MessageName] = m
		}
	}

	var b strings.Builder

	b.WriteString("// Code generated by protoc-gen-storage. DO NOT EDIT.\n")
	b.WriteString("// source: (proto input)\n\n")
	fmt.Fprintf(&b, "package %s\n\n", storagePkgName)
	withSecrets := hasSecretFields(messages)
	// For import decisions, check both message flags and their owner's flags: a surface
	// inherits its owner's soft-delete/expire/etag behaviour in the generated code even
	// though the surface messageInfo itself may not have those flags set.
	withSoftDelete := hasSoftDeleteMsg(messages)
	withExpireTime := hasExpireTimeMsg(messages)
	withETag := hasETagMsg(messages)
	if !withSoftDelete || !withExpireTime || !withETag {
		for _, msg := range messages {
			if msg.isSurface() {
				owner := ownerByName[msg.modelType()]
				if owner.SoftDelete {
					withSoftDelete = true
				}
				if owner.HasExpireTime {
					withExpireTime = true
				}
				if owner.HasETag {
					withETag = true
				}
			}
		}
	}
	withTags := hasTagsFields(messages)

	// Determine if any message needs the middleware import (tenant or secret lookup).
	withMiddleware := false
	for _, msg := range messages {
		if msgHasTenantField(msg) {
			withMiddleware = true
			break
		}
	}
	if !withMiddleware {
		for _, msg := range messages {
			for _, f := range msg.Fields {
				if f.IsSecret {
					withMiddleware = true
					break
				}
			}
			if withMiddleware {
				break
			}
		}
	}

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	if withExpireTime {
		b.WriteString("\t\"database/sql\"\n")
	}
	b.WriteString("\t\"encoding/base64\"\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"strconv\"\n")
	b.WriteString("\t\"time\"\n\n")
	b.WriteString("\t\"gorm.io/gorm\"\n")
	if withSoftDelete || withExpireTime {
		b.WriteString("\t\"google.golang.org/protobuf/types/known/timestamppb\"\n")
	}
	b.WriteString("\n")
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence\"\n")
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence/filter\"\n")
	if withTags {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/types\"\n")
	}
	// Emit resourcename import only when at least one message has a resource pattern.
	hasResourcePattern := false
	for _, m := range messages {
		if m.ResourcePattern != "" {
			hasResourcePattern = true
			break
		}
	}
	if hasResourcePattern {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence/resourcename\"\n")
	}
	if withMiddleware {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/middleware\"\n")
	}
	if withETag {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/middleware/etag\"\n")
	}
	if withSecrets {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/secret\"\n")
	}
	b.WriteString("\t\"google.golang.org/grpc/codes\"\n")
	b.WriteString("\t\"google.golang.org/grpc/status\"\n")
	b.WriteString(")\n\n")

	// Suppress unused import warnings for packages that may not be referenced
	// in the small toy case. Generated code always references them via CRUD methods.
	b.WriteString("var (\n")
	b.WriteString("\t_ = base64.StdEncoding\n")
	b.WriteString("\t_ = fmt.Sprintf\n")
	b.WriteString("\t_ = strconv.Itoa\n")
	b.WriteString("\t_ = filter.Parse\n")
	b.WriteString("\t_ = codes.OK\n")
	b.WriteString("\t_ = status.Error\n")
	if hasResourcePattern {
		b.WriteString("\t_ = resourcename.IDVarName\n")
	}
	if withSoftDelete || withExpireTime {
		b.WriteString("\t_ = timestamppb.New\n")
	}
	if withExpireTime {
		b.WriteString("\t_ = sql.NullTime{}\n")
	}
	b.WriteString(")\n\n")

	for _, msg := range messages {
		owner := ownerByName[msg.modelType()]
		renderMessage(&b, msg, owner, withSecrets)
	}

	return b.String()
}

func renderMessage(b *strings.Builder, msg messageInfo, owner messageInfo, withSecrets bool) {
	// model is the GORM model TYPE name (the owner's name). For an owner /
	// single-surface resource this equals msg.MessageName; for a surface it is
	// the owner's name — the struct, table, and query builder the surface reuses.
	model := owner.MessageName

	// Mutation and framework flags follow the OWNER's schema.
	hasTenant := msgHasTenantField(owner)
	// A soft-delete + per-tenant-unique resource needs the unique key freed when
	// the holder is soft-deleted. PostgreSQL/SQLite use a partial unique index
	// (WHERE deleted_at IS NULL); MySQL (no partial indexes) joins a
	// soft_delete_key discriminator column into the composite instead.
	softDeleteUnique := owner.SoftDelete && msgHasTenantUnique(owner)
	useSentinel := softDeleteUnique && useSoftDeleteSentinel()
	usePartial := softDeleteUnique && !useSoftDeleteSentinel()
	// Set of Go field names backed by a proto-declared scalar column. Used to
	// avoid emitting a duplicate belongs_to FK field when the proto already
	// exposes the FK as a scalar (the natural AIP shape). The struct is emitted
	// from owner.Fields so we check the owner's fields here.
	scalarGoNames := map[string]bool{}
	for _, f := range owner.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret || f.IsOutputOnly {
			continue
		}
		scalarGoNames[goFieldName(f)] = true
	}
	// uniqueIndexName returns the composite unique-index name for a tenant-scoped
	// unique field, so the index spans (account_id, <col>) rather than <col>
	// alone — preserving per-tenant uniqueness in a multi-tenant framework.
	uniqueIndexName := func(col string) string {
		return "ux_" + strings.ToLower(model) + "_account_" + col
	}

	// GORM model struct: emitted only for owners (a surface has no table of its own
	// — it reuses <model>Model). Skip the entire struct block for surfaces.
	if !msg.isSurface() {
		fmt.Fprintf(b, "// %sModel is the GORM model for %s.\n", model, model)
		fmt.Fprintf(b, "type %sModel struct {\n", model)
		fmt.Fprintf(b, "\tID        string         `gorm:\"primaryKey;type:varchar(36)\"`\n")

		for _, f := range owner.Fields {
			if f.IsID {
				continue // ID is already emitted above
			}
			if f.IsOutputOnly {
				continue // computed field, not stored in DB
			}
			if f.IsTags {
				// Tags (map<string,string>) persist as a single JSONB column via the
				// ORM-agnostic types.Tags (driver.Valuer/sql.Scanner). jsonb works on
				// Postgres natively and on SQLite via type affinity (stores the JSON
				// text); a column_type annotation overrides the default.
				col := f.SnakeName
				if f.ColumnName != "" {
					col = f.ColumnName
				}
				colType := "jsonb"
				if f.ColumnType != "" {
					colType = f.ColumnType
				}
				fmt.Fprintf(b, "\t%s types.Tags `gorm:\"column:%s;type:%s\"`\n", goFieldName(f), col, colType)
				continue
			}
			if f.IsRepeated {
				if f.HasMany != nil {
					// has_many: a slice of the related GORM model. GORM resolves the
					// association from the concrete element type; foreignKey names the
					// Go field on the related model that holds this resource's key.
					gfn := goFieldName(f)
					fmt.Fprintf(b, "\t%s []*%s `gorm:\"foreignKey:%s\"`\n", gfn, assocModelType(f), snakeToCamel(f.HasMany.GetForeignKey()))
					continue
				}
				if f.ManyToMany != nil {
					gfn := goFieldName(f)
					fmt.Fprintf(b, "\t%s []*%s `gorm:\"many2many:%s\"`\n", gfn, assocModelType(f), f.ManyToMany.GetJoinTable())
					continue
				}
				fmt.Fprintf(b, "\t// TODO: repeated field %s skipped — JSONB serialization needed (W5-6)\n", f.Name)
				continue
			}
			if f.IsMessage {
				if f.HasOne != nil {
					gfn := goFieldName(f)
					fmt.Fprintf(b, "\t%s *%s `gorm:\"foreignKey:%s\"`\n", gfn, assocModelType(f), snakeToCamel(f.HasOne.GetForeignKey()))
					continue
				}
				if f.BelongsTo != nil {
					// belongs_to: the FK lives on THIS table. Emit a concrete pointer
					// association keyed by the FK's Go field name.
					gfn := goFieldName(f)
					fk := f.BelongsTo.GetForeignKey()
					fmt.Fprintf(b, "\t%s *%s `gorm:\"foreignKey:%s\"`\n", gfn, assocModelType(f), snakeToCamel(fk))
					// Emit the FK column field only when the proto does not already
					// expose it as a scalar field — otherwise the field is duplicated
					// and the generated package fails to compile.
					if fk != "" {
						fkGoName := snakeToCamel(fk)
						if !scalarGoNames[fkGoName] {
							fmt.Fprintf(b, "\t%s string `gorm:\"column:%s\"`\n", fkGoName, fk)
						}
					}
					continue
				}
				fmt.Fprintf(b, "\t// TODO: nested message %s skipped — serialization strategy TBD (W5-6)\n", f.Name)
				continue
			}
			gfn := goFieldName(f)
			if f.IsSecret {
				// Secret fields are never stored as plaintext; emit hash + cipher columns.
				fmt.Fprintf(b, "\t%sHash   string `gorm:\"column:%s_hash;index\"`\n", gfn, f.SnakeName)
				fmt.Fprintf(b, "\t%sCipher string `gorm:\"column:%s_cipher\"`\n", gfn, f.SnakeName)
			} else {
				// Build the GORM tag.
				col := f.SnakeName
				if f.ColumnName != "" {
					col = f.ColumnName
				}
				var tagParts []string
				tagParts = append(tagParts, "column:"+col)
				if f.ColumnType != "" {
					tagParts = append(tagParts, "type:"+f.ColumnType)
				}
				if f.NotNull {
					tagParts = append(tagParts, "not null")
				}
				if f.Unique {
					if hasTenant {
						// Tenant-scoped: the unique constraint must be per-account, so
						// the field joins account_id in a composite unique index
						// (priority 2 — account_id is the leading column, priority 1).
						// The partial-index predicate (usePartial) is carried on the
						// account_id (priority 1) tag below, not here.
						tagParts = append(tagParts, "uniqueIndex:"+uniqueIndexName(col)+",priority:2")
					} else {
						// No tenant column: a plain global unique index is correct.
						tagParts = append(tagParts, "uniqueIndex")
					}
				} else if f.Index {
					tagParts = append(tagParts, "index")
				}
				// account_id is the leading column of every per-tenant composite unique
				// index, so a name unique within one tenant can be reused by another.
				if hasTenant && (f.Name == "account_id" || f.SnakeName == "account_id") {
					for _, uf := range owner.Fields {
						if uf.IsID || uf.IsRepeated || uf.IsMessage || uf.IsSecret || uf.IsOutputOnly || !uf.Unique {
							continue
						}
						ucol := uf.SnakeName
						if uf.ColumnName != "" {
							ucol = uf.ColumnName
						}
						ux := "uniqueIndex:" + uniqueIndexName(ucol) + ",priority:1"
						if usePartial {
							// Partial unique index so the key frees on soft-delete. GORM's
							// migrator drops the `where` index tag but appends `option`
							// verbatim after the column list, so the predicate rides there:
							// CREATE UNIQUE INDEX ux ON t (account_id, <field>) WHERE deleted_at IS NULL.
							ux += ",option:WHERE deleted_at IS NULL"
						}
						tagParts = append(tagParts, ux)
					}
				}
				tag := strings.Join(tagParts, ";")
				fmt.Fprintf(b, "\t%s %s `gorm:\"%s\"`\n", gfn, f.GoType, tag)
			}
		}

		b.WriteString("\tETag      string         `gorm:\"column:etag\"`\n")
		b.WriteString("\tCreatedAt time.Time\n")
		b.WriteString("\tUpdatedAt time.Time\n")
		if owner.SoftDelete {
			b.WriteString("\tDeletedAt gorm.DeletedAt `gorm:\"index\"`\n")
		}
		if useSentinel {
			// MySQL soft-delete-unique discriminator: "" while live, the row id once
			// soft-deleted, so a per-tenant unique key can be re-created after the
			// holder is soft-deleted. Joins every per-tenant composite unique as the
			// trailing (priority 3) column. Maintained by Delete/Undelete below.
			parts := []string{"column:soft_delete_key"}
			for _, uf := range owner.Fields {
				if uf.IsID || uf.IsRepeated || uf.IsMessage || uf.IsSecret || uf.IsOutputOnly || !uf.Unique {
					continue
				}
				ucol := uf.SnakeName
				if uf.ColumnName != "" {
					ucol = uf.ColumnName
				}
				parts = append(parts, "uniqueIndex:"+uniqueIndexName(ucol)+",priority:3")
			}
			fmt.Fprintf(b, "\tSoftDeleteKey string `gorm:\"%s\"`\n", strings.Join(parts, ";"))
		}
		if owner.HasExpireTime {
			b.WriteString("\tExpireTime sql.NullTime `gorm:\"column:expire_time;index\"`\n")
		}
		b.WriteString("}\n\n")
	}

	pbType := fmt.Sprintf("*%s", msg.MessageName)
	pbPkg := msg.PbPkgName
	if pbPkg != "" {
		pbType = fmt.Sprintf("*%s.%s", pbPkg, msg.MessageName)
	}

	// toModel helper: function name uses the SURFACE name (msg.MessageName), but
	// the return type uses the OWNER model name (model) — a surface writes the same
	// row as its owner. Field iteration uses msg.Fields (the surface's projection).
	fmt.Fprintf(b, "func toModel_%s(p %s) *%sModel {\n", msg.MessageName, pbType, model)
	b.WriteString("\tif p == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(b, "\tm := &%sModel{ID: p.Id}\n", model)
	for _, f := range msg.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret || f.IsOutputOnly {
			continue // output-only and secret fields are not persisted via toModel
		}
		gfn := goFieldName(f)
		if f.IsTags {
			fmt.Fprintf(b, "\tm.%s = types.Tags(p.%s)\n", gfn, gfn)
			continue
		}
		fmt.Fprintf(b, "\tm.%s = p.%s\n", gfn, gfn)
	}
	// AIP-148 TTL: expire_time is OUTPUT_ONLY (server-managed), so it is not in the
	// loop above — but it must be carried onto the model so a Create handler that
	// stamps it persists a real expiry. Without this, seam-created rows always store
	// expire_time = NULL and PurgeExpired has nothing to reap. Gated on the SURFACE's
	// own field set (msg): a surface that does not expose expire_time has no
	// p.ExpireTime to read, and its writes simply do not carry it.
	if msg.HasExpireTime {
		b.WriteString("\tif p.ExpireTime != nil {\n")
		b.WriteString("\t\tm.ExpireTime = sql.NullTime{Time: p.ExpireTime.AsTime(), Valid: true}\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("\treturn m\n}\n\n")

	// fromModel helper: function name uses the SURFACE name (msg.MessageName), and
	// input type uses the OWNER model name (model). Field projection follows msg.Fields.
	fmt.Fprintf(b, "func fromModel_%s(m *%sModel) %s {\n", msg.MessageName, model, pbType)
	b.WriteString("\tif m == nil {\n\t\treturn nil\n\t}\n")
	if pbPkg != "" {
		fmt.Fprintf(b, "\tp := &%s.%s{Id: m.ID}\n", pbPkg, msg.MessageName)
	} else {
		fmt.Fprintf(b, "\tp := &%s{Id: m.ID}\n", msg.MessageName)
	}
	// Populate AIP-122 resource name field if present (keyed on the surface's own pattern).
	if msg.ResourcePattern != "" {
		for _, f := range msg.Fields {
			if f.IsOutputOnly && f.Name == "name" {
				fmt.Fprintf(b, "\tp.Name = Format%sName(m.ID)\n", msg.MessageName)
				break
			}
		}
	}
	for _, f := range msg.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret || f.IsOutputOnly {
			continue // output-only and secret fields are never copied from model
		}
		gfn := goFieldName(f)
		if f.IsTags {
			fmt.Fprintf(b, "\tp.%s = map[string]string(m.%s)\n", gfn, gfn)
			continue
		}
		fmt.Fprintf(b, "\tp.%s = m.%s\n", gfn, gfn)
	}
	// AIP-148: populate delete_time / expire_time from GORM columns.
	// For a surface these follow the surface's own declaration (it may omit them).
	if msg.SoftDelete {
		b.WriteString("\tif m.DeletedAt.Valid {\n")
		b.WriteString("\t\tp.DeleteTime = timestamppb.New(m.DeletedAt.Time)\n")
		b.WriteString("\t}\n")
	}
	if msg.HasExpireTime {
		b.WriteString("\tif m.ExpireTime.Valid {\n")
		b.WriteString("\t\tp.ExpireTime = timestamppb.New(m.ExpireTime.Time)\n")
		b.WriteString("\t}\n")
	}
	// AIP-154: surface the stored ETag so a Get/Create/Update response carries a
	// value a client can echo as If-Match.
	if msg.HasETag {
		b.WriteString("\tp.Etag = m.ETag\n")
	}
	// Owned customization hook: called after all automatic projection is done.
	fmt.Fprintf(b, "\tif FromModel%sCustom != nil {\n\t\tFromModel%sCustom(m, p)\n\t}\n", msg.MessageName, msg.MessageName)
	b.WriteString("\treturn p\n}\n\n")

	// Owned customization hooks (F027 split-files override seam).
	// Nil by default; the developer registers them from their OWN (regen-safe)
	// file. The generated file only declares and calls them, so re-running codegen
	// never disturbs custom logic.
	fmt.Fprintf(b, "// FromModel%sCustom, if set, runs at the end of fromModel_%s to populate\n", msg.MessageName, msg.MessageName)
	b.WriteString("// fields the generator cannot derive (computed/derived values). Register it\n")
	b.WriteString("// from your own (regen-safe) file — e.g. in an init(); never assigned here.\n")
	fmt.Fprintf(b, "var FromModel%sCustom func(m *%sModel, p %s)\n\n", msg.MessageName, model, pbType)
	fmt.Fprintf(b, "// ToModel%sOnCreate, if set, runs in Create just before the database write,\n", msg.MessageName)
	b.WriteString("// to set columns the generator does not (e.g. a custom-encoded field).\n")
	fmt.Fprintf(b, "var ToModel%sOnCreate func(p %s, m *%sModel)\n\n", msg.MessageName, pbType, model)
	fmt.Fprintf(b, "// ToModel%sOnUpdate, if set, runs in Update just before the database write.\n", msg.MessageName)
	fmt.Fprintf(b, "var ToModel%sOnUpdate func(p %s, m *%sModel)\n\n", msg.MessageName, pbType, model)

	// Column map for safe filter/order_by parsing (AIP-160/132).
	// Column names are keyed on the surface's own field set (msg.Fields).
	fmt.Fprintf(b, "// %sColumns maps proto field names to DB column names for safe filter/order_by parsing.\n", msg.MessageName)
	fmt.Fprintf(b, "var %sColumns = map[string]string{\n", msg.MessageName)
	fmt.Fprintf(b, "\t\"id\": \"id\",\n")
	for _, f := range msg.Fields {
		// Tags are intentionally absent: filtering/ordering on a JSONB map is a
		// distinct feature (inclusion operators) not yet supported, and a plain
		// `tags = 'x'` predicate errors against a Postgres jsonb column.
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsTags || f.IsSecret || f.IsOutputOnly {
			continue
		}
		col := f.SnakeName
		if f.ColumnName != "" {
			col = f.ColumnName
		}
		fmt.Fprintf(b, "\t%q: %q,\n", f.Name, col)
	}
	// AIP-148: soft-delete timestamps admitted into the column map for filter/order_by.
	// Follow the OWNER for column existence; surface declaration may differ.
	if owner.SoftDelete {
		fmt.Fprintf(b, "\t\"delete_time\": \"deleted_at\",\n")
	}
	if owner.HasExpireTime {
		fmt.Fprintf(b, "\t\"expire_time\": \"expire_time\",\n")
	}
	b.WriteString("}\n\n")

	// JSON/tag column map for `tags.<key>` filtering (kept separate from the
	// filter/order_by column map above, which is for scalar columns only).
	if msgHasTags(msg) {
		fmt.Fprintf(b, "// %sJSONColumns maps tag (map<string,string>) field names to DB columns for `tags.<key>` filtering.\n", msg.MessageName)
		fmt.Fprintf(b, "var %sJSONColumns = map[string]string{\n", msg.MessageName)
		for _, f := range msg.Fields {
			if !f.IsTags {
				continue
			}
			col := f.SnakeName
			if f.ColumnName != "" {
				col = f.ColumnName
			}
			fmt.Fprintf(b, "\t%q: %q,\n", f.Name, col)
		}
		b.WriteString("}\n\n")
	}

	// AIP-122 resource name helpers — keyed on the surface's own resource pattern.
	if msg.ResourcePattern != "" {
		idVar := resourcenameIDVarName(msg.ResourcePattern)
		fmt.Fprintf(b, "// %sNamePattern is the AIP-122 resource name pattern for %s.\n", msg.MessageName, msg.MessageName)
		fmt.Fprintf(b, "const %sNamePattern = %q\n\n", msg.MessageName, msg.ResourcePattern)
		fmt.Fprintf(b, "// Format%sName builds the resource name for the given ID.\n", msg.MessageName)
		fmt.Fprintf(b, "func Format%sName(id string) string {\n", msg.MessageName)
		fmt.Fprintf(b, "\tname, _ := resourcename.Format(%sNamePattern, map[string]string{%q: id})\n", msg.MessageName, idVar)
		fmt.Fprintf(b, "\treturn name\n}\n\n")
		fmt.Fprintf(b, "// Parse%sName extracts the resource ID from the given name.\n", msg.MessageName)
		fmt.Fprintf(b, "func Parse%sName(name string) (string, error) {\n", msg.MessageName)
		fmt.Fprintf(b, "\treturn resourcename.IDFromName(%sNamePattern, name)\n}\n\n", msg.MessageName)
	}

	// Determine if this message has any secret fields.
	msgHasSecrets := false
	for _, f := range msg.Fields {
		if f.IsSecret {
			msgHasSecrets = true
			break
		}
	}

	// Repository struct + constructor. Named for the SURFACE (msg.MessageName) so each
	// surface gets its own constructor; the struct's db field operates on the owner's table.
	fmt.Fprintf(b, "// %sRepository is a GORM-backed persistence.Repository for %s.\n", msg.MessageName, pbType)
	if msgHasSecrets {
		fmt.Fprintf(b, "type %sRepository struct {\n\tdb  *gorm.DB\n\tenc secret.Encryptor\n}\n\n", msg.MessageName)
		fmt.Fprintf(b, "// New%sRepository creates a repository backed by db and enc.\n", msg.MessageName)
		fmt.Fprintf(b, "func New%sRepository(db *gorm.DB, enc secret.Encryptor) *%sRepository {\n", msg.MessageName, msg.MessageName)
		fmt.Fprintf(b, "\treturn &%sRepository{db: db, enc: enc}\n}\n\n", msg.MessageName)
	} else {
		fmt.Fprintf(b, "type %sRepository struct{ db *gorm.DB }\n\n", msg.MessageName)
		fmt.Fprintf(b, "// New%sRepository creates a repository backed by db.\n", msg.MessageName)
		fmt.Fprintf(b, "func New%sRepository(db *gorm.DB) *%sRepository {\n", msg.MessageName, msg.MessageName)
		fmt.Fprintf(b, "\treturn &%sRepository{db: db}\n}\n\n", msg.MessageName)
	}

	// Get. Model type follows the OWNER.
	fmt.Fprintf(b, "func (r *%sRepository) Get(ctx context.Context, key string) (%s, error) {\n", msg.MessageName, pbType)
	fmt.Fprintf(b, "\tvar m %sModel\n", model)
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\tq := r.db.WithContext(ctx).Where(\"id = ?\", key)\n")
		b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
		b.WriteString("\tif err := q.First(&m).Error; err != nil {\n")
	} else {
		b.WriteString("\tif err := r.db.WithContext(ctx).Where(\"id = ?\", key).First(&m).Error; err != nil {\n")
	}
	b.WriteString("\t\tif err == gorm.ErrRecordNotFound {\n")
	fmt.Fprintf(b, "\t\t\treturn nil, persistence.ErrNotFound\n")
	b.WriteString("\t\t}\n")
	fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"get %s: %%w\", err)\n", msg.MessageName)
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\treturn fromModel_%s(&m), nil\n}\n\n", msg.MessageName)

	// List. Model type and soft-delete scope follow the OWNER; column map uses the SURFACE.
	fmt.Fprintf(b, "func (r *%sRepository) List(ctx context.Context, opts persistence.ListOptions) ([]%s, string, error) {\n", msg.MessageName, pbType)
	fmt.Fprintf(b, "\tvar models []%sModel\n", model)
	b.WriteString("\tq := r.db.WithContext(ctx)\n")
	// AIP-148: lift soft-delete scope BEFORE tenant predicate (FR-008, FR-014).
	// Scope follows the OWNER's soft-delete setting.
	if owner.SoftDelete {
		b.WriteString("\tif opts.ShowDeleted {\n\t\tq = q.Unscoped()\n\t}\n")
	}
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
	}
	fmt.Fprintf(b, "\tif opts.Filter != \"\" {\n")
	if msgHasTags(msg) {
		// Tag (map) columns admit `tags.<key>` predicates, whose JSON SQL is
		// dialect-specific; pass the JSON column whitelist and the live dialect.
		fmt.Fprintf(b, "\t\tcond, err := filter.Parse(opts.Filter, %sColumns, filter.WithJSONColumns(%sJSONColumns), filter.WithDialect(r.db.Dialector.Name()))\n", msg.MessageName, msg.MessageName)
	} else {
		fmt.Fprintf(b, "\t\tcond, err := filter.Parse(opts.Filter, %sColumns)\n", msg.MessageName)
	}
	fmt.Fprintf(b, "\t\tif err != nil {\n")
	fmt.Fprintf(b, "\t\t\treturn nil, \"\", status.Errorf(codes.InvalidArgument, \"invalid filter: %%v\", err)\n")
	fmt.Fprintf(b, "\t\t}\n")
	fmt.Fprintf(b, "\t\tsql, args := cond.SQL()\n")
	fmt.Fprintf(b, "\t\tq = q.Where(sql, args...)\n")
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\tif opts.OrderBy != \"\" {\n")
	fmt.Fprintf(b, "\t\tclauses, err := filter.ParseOrderBy(opts.OrderBy, %sColumns)\n", msg.MessageName)
	fmt.Fprintf(b, "\t\tif err != nil {\n")
	fmt.Fprintf(b, "\t\t\treturn nil, \"\", status.Errorf(codes.InvalidArgument, \"invalid order_by: %%v\", err)\n")
	fmt.Fprintf(b, "\t\t}\n")
	fmt.Fprintf(b, "\t\tfor _, c := range clauses {\n")
	fmt.Fprintf(b, "\t\t\tq = q.Order(c.GORMExpr())\n")
	fmt.Fprintf(b, "\t\t}\n")
	fmt.Fprintf(b, "\t}\n")
	b.WriteString("\tpageSize := opts.PageSize\n")
	b.WriteString("\tif pageSize <= 0 {\n\t\tpageSize = 50\n\t}\n")
	b.WriteString("\toffset := 0\n")
	b.WriteString("\tif opts.PageToken != \"\" {\n")
	b.WriteString("\t\tif dec, err := base64.StdEncoding.DecodeString(opts.PageToken); err == nil {\n")
	b.WriteString("\t\t\toffset, _ = strconv.Atoi(string(dec))\n")
	b.WriteString("\t\t}\n\t}\n")
	b.WriteString("\tif err := q.Limit(pageSize).Offset(offset).Find(&models).Error; err != nil {\n")
	fmt.Fprintf(b, "\t\treturn nil, \"\", fmt.Errorf(\"list %s: %%w\", err)\n", msg.MessageName)
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\tout := make([]%s, len(models))\n", pbType)
	b.WriteString("\tfor i := range models {\n")
	fmt.Fprintf(b, "\t\tout[i] = fromModel_%s(&models[i])\n", msg.MessageName)
	b.WriteString("\t}\n")
	b.WriteString("\tnextToken := \"\"\n")
	fmt.Fprintf(b, "\tif len(models) == pageSize {\n")
	b.WriteString("\t\tnextToken = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(offset + pageSize)))\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn out, nextToken, nil\n}\n\n")

	// Create. Model type follows the OWNER; hooks use the SURFACE name.
	fmt.Fprintf(b, "func (r *%sRepository) Create(ctx context.Context, entity %s) (%s, error) {\n", msg.MessageName, pbType, pbType)
	fmt.Fprintf(b, "\tm := toModel_%s(entity)\n", msg.MessageName)
	if msgHasSecrets {
		for _, f := range msg.Fields {
			if !f.IsSecret {
				continue
			}
			gfn := goFieldName(f)
			fmt.Fprintf(b, "\tif entity.%s != \"\" {\n", gfn)
			fmt.Fprintf(b, "\t\th, err := r.enc.Hash(ctx, entity.%s)\n", gfn)
			fmt.Fprintf(b, "\t\tif err != nil { return nil, fmt.Errorf(\"hash %s: %%w\", err) }\n", f.Name)
			fmt.Fprintf(b, "\t\tc, err := r.enc.Encrypt(ctx, entity.%s)\n", gfn)
			fmt.Fprintf(b, "\t\tif err != nil { return nil, fmt.Errorf(\"encrypt %s: %%w\", err) }\n", f.Name)
			fmt.Fprintf(b, "\t\tm.%sHash = h\n", gfn)
			fmt.Fprintf(b, "\t\tm.%sCipher = c\n", gfn)
			b.WriteString("\t}\n")
		}
	}
	if owner.HasETag {
		b.WriteString("\tm.ETag = etag.New() // AIP-154: fresh ETag on create\n")
	}
	// ToModel hook before write.
	fmt.Fprintf(b, "\tif ToModel%sOnCreate != nil {\n\t\tToModel%sOnCreate(entity, m)\n\t}\n", msg.MessageName, msg.MessageName)
	b.WriteString("\tif err := r.db.WithContext(ctx).Create(m).Error; err != nil {\n")
	b.WriteString("\t\t// Map driver constraint violations to clean sentinels so callers see\n")
	b.WriteString("\t\t// AlreadyExists/FailedPrecondition (not 500), and no SQL leaks to the client.\n")
	b.WriteString("\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\treturn nil, ce\n\t\t}\n")
	fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"create %s: %%w\", err)\n", msg.MessageName)
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\treturn fromModel_%s(m), nil\n}\n\n", msg.MessageName)

	// Update. Model type and ETag follow the OWNER; field columns follow the SURFACE.
	fmt.Fprintf(b, "func (r *%sRepository) Update(ctx context.Context, key string, entity %s, fieldMask ...string) (%s, error) {\n", msg.MessageName, pbType, pbType)
	fmt.Fprintf(b, "\tm := toModel_%s(entity)\n", msg.MessageName)
	b.WriteString("\tm.ID = key\n")
	if owner.HasETag {
		b.WriteString("\tm.ETag = etag.New() // AIP-154: bump the ETag on every update\n")
	}
	if msgHasSecrets {
		for _, f := range msg.Fields {
			if !f.IsSecret {
				continue
			}
			gfn := goFieldName(f)
			fmt.Fprintf(b, "\tif entity.%s != \"\" {\n", gfn)
			fmt.Fprintf(b, "\t\th, err := r.enc.Hash(ctx, entity.%s)\n", gfn)
			fmt.Fprintf(b, "\t\tif err != nil { return nil, fmt.Errorf(\"hash %s: %%w\", err) }\n", f.Name)
			fmt.Fprintf(b, "\t\tc, err := r.enc.Encrypt(ctx, entity.%s)\n", gfn)
			fmt.Fprintf(b, "\t\tif err != nil { return nil, fmt.Errorf(\"encrypt %s: %%w\", err) }\n", f.Name)
			fmt.Fprintf(b, "\t\tm.%sHash = h\n", gfn)
			fmt.Fprintf(b, "\t\tm.%sCipher = c\n", gfn)
			b.WriteString("\t}\n")
		}
	}
	// ToModel hook before write.
	fmt.Fprintf(b, "\tif ToModel%sOnUpdate != nil {\n\t\tToModel%sOnUpdate(entity, m)\n\t}\n", msg.MessageName, msg.MessageName)
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\tq := r.db.WithContext(ctx).Model(m).Where(\"id = ?\", key)\n")
		b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
	} else {
		b.WriteString("\tq := r.db.WithContext(ctx).Model(m).Where(\"id = ?\", key)\n")
	}
	// Collect the regular (scalar, persisted) columns that a full update writes.
	// The tenant scoping key (account_id) is deliberately excluded: it is assigned at
	// create and is only ever a WHERE predicate, never a writable column.
	var regularFields []fieldInfo
	for _, f := range msg.Fields {
		if f.IsID || f.IsRepeated || f.IsMessage || f.IsSecret || f.IsOutputOnly {
			continue
		}
		if hasTenant && (f.Name == "account_id" || f.SnakeName == "account_id") {
			continue
		}
		regularFields = append(regularFields, f)
	}
	fmt.Fprintf(b, "\tif len(fieldMask) > 0 {\n")
	fmt.Fprintf(b, "\t\tdbCols := make([]string, 0, len(fieldMask))\n")
	fmt.Fprintf(b, "\t\tfor _, f := range fieldMask {\n")
	fmt.Fprintf(b, "\t\t\tcol, ok := %sColumns[f]\n", msg.MessageName)
	fmt.Fprintf(b, "\t\t\tif !ok {\n")
	fmt.Fprintf(b, "\t\t\t\treturn nil, status.Errorf(codes.InvalidArgument, \"unknown field in update_mask: %%q\", f)\n")
	fmt.Fprintf(b, "\t\t\t}\n")
	if hasTenant {
		// The tenant scoping key is never writable, even when explicitly named in the
		// field mask — it must stay a WHERE predicate, never a SET column.
		fmt.Fprintf(b, "\t\t\tif col == \"account_id\" {\n")
		fmt.Fprintf(b, "\t\t\t\treturn nil, status.Errorf(codes.InvalidArgument, \"account_id is the tenant key and cannot be updated\")\n")
		fmt.Fprintf(b, "\t\t\t}\n")
	}
	fmt.Fprintf(b, "\t\t\tdbCols = append(dbCols, col)\n")
	fmt.Fprintf(b, "\t\t}\n")
	if owner.HasETag {
		b.WriteString("\t\tdbCols = append(dbCols, \"etag\") // a masked update still changes the resource\n")
	}
	fmt.Fprintf(b, "\t\t// Select makes GORM write the named columns even when their value is\n")
	fmt.Fprintf(b, "\t\t// the zero value (false, 0, \"\"); a bare struct Updates would skip them.\n")
	fmt.Fprintf(b, "\t\tif err := q.Select(dbCols).Updates(m).Error; err != nil {\n")
	b.WriteString("\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\treturn nil, ce\n\t\t\t}\n")
	fmt.Fprintf(b, "\t\t\treturn nil, fmt.Errorf(\"update %s: %%w\", err)\n", msg.MessageName)
	fmt.Fprintf(b, "\t\t}\n")
	if len(regularFields) > 0 {
		fmt.Fprintf(b, "\t} else {\n")
		fmt.Fprintf(b, "\t\t// No field mask: full update of every writable column via a map, so\n")
		fmt.Fprintf(b, "\t\t// zero values (false, 0, \"\") persist — a struct Updates skips zero fields\n")
		fmt.Fprintf(b, "\t\t// and would silently drop \"disable this\" / \"clear that\" updates.\n")
		fmt.Fprintf(b, "\t\tupdates := map[string]interface{}{\n")
		for _, f := range regularFields {
			col := f.SnakeName
			if f.ColumnName != "" {
				col = f.ColumnName
			}
			fmt.Fprintf(b, "\t\t\t%q: m.%s,\n", col, goFieldName(f))
		}
		fmt.Fprintf(b, "\t\t}\n")
		if msgHasSecrets {
			for _, f := range msg.Fields {
				if !f.IsSecret {
					continue
				}
				gfn := goFieldName(f)
				// Only rewrite the secret columns when the caller supplied a new value,
				// otherwise the stored hash/cipher would be wiped to empty.
				fmt.Fprintf(b, "\t\tif entity.%s != \"\" {\n", gfn)
				fmt.Fprintf(b, "\t\t\tupdates[%q] = m.%sHash\n", f.SnakeName+"_hash", gfn)
				fmt.Fprintf(b, "\t\t\tupdates[%q] = m.%sCipher\n", f.SnakeName+"_cipher", gfn)
				fmt.Fprintf(b, "\t\t}\n")
			}
		}
		if owner.HasETag {
			b.WriteString("\t\tupdates[\"etag\"] = m.ETag\n")
		}
		fmt.Fprintf(b, "\t\tif err := q.Updates(updates).Error; err != nil {\n")
		b.WriteString("\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\treturn nil, ce\n\t\t\t}\n")
		fmt.Fprintf(b, "\t\t\treturn nil, fmt.Errorf(\"update %s: %%w\", err)\n", msg.MessageName)
		fmt.Fprintf(b, "\t\t}\n")
		fmt.Fprintf(b, "\t}\n")
	} else {
		// No writable scalar columns (id + secrets only): a struct update is
		// sufficient — there are no zero-valued scalar columns to lose.
		fmt.Fprintf(b, "\t} else {\n")
		fmt.Fprintf(b, "\t\tif err := q.Updates(m).Error; err != nil {\n")
		b.WriteString("\t\t\tif ce := persistence.ConstraintError(err); ce != nil {\n\t\t\t\treturn nil, ce\n\t\t\t}\n")
		fmt.Fprintf(b, "\t\t\treturn nil, fmt.Errorf(\"update %s: %%w\", err)\n", msg.MessageName)
		fmt.Fprintf(b, "\t\t}\n")
		fmt.Fprintf(b, "\t}\n")
	}
	fmt.Fprintf(b, "\treturn r.Get(ctx, key)\n}\n\n")

	// Delete — soft (AIP-148) or hard depending on OWNER opt-in.
	fmt.Fprintf(b, "func (r *%sRepository) Delete(ctx context.Context, key string) error {\n", msg.MessageName)
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\tq := r.db.WithContext(ctx).Where(\"id = ?\", key)\n")
		b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
		switch {
		case useSentinel:
			// Soft-delete AND stamp the row id into soft_delete_key in one update so
			// the freed key leaves the live (account_id, <field>, "") namespace
			// atomically (MySQL has no partial index). The default soft-delete scope
			// limits this to a currently-live row, so RowsAffected==0 → NotFound.
			fmt.Fprintf(b, "\tres := q.Model(&%sModel{}).Updates(map[string]interface{}{\"deleted_at\": time.Now().UTC(), \"soft_delete_key\": key})\n", model)
		case owner.SoftDelete:
			fmt.Fprintf(b, "\tres := q.Delete(&%sModel{})\n", model)
		default:
			fmt.Fprintf(b, "\tres := q.Unscoped().Delete(&%sModel{})\n", model)
		}
	} else {
		if owner.SoftDelete {
			fmt.Fprintf(b, "\tres := r.db.WithContext(ctx).Where(\"id = ?\", key).Delete(&%sModel{})\n", model)
		} else {
			fmt.Fprintf(b, "\tres := r.db.WithContext(ctx).Where(\"id = ?\", key).Unscoped().Delete(&%sModel{})\n", model)
		}
	}
	b.WriteString("\tif res.Error != nil {\n")
	fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"delete %s: %%w\", res.Error)\n", msg.MessageName)
	b.WriteString("\t}\n")
	b.WriteString("\tif res.RowsAffected == 0 {\n\t\treturn persistence.ErrNotFound\n\t}\n")
	b.WriteString("\treturn nil\n}\n\n")

	// Undelete — full implementation when OWNER has SoftDelete, otherwise a stub.
	if owner.SoftDelete {
		fmt.Fprintf(b, "func (r *%sRepository) Undelete(ctx context.Context, key string) (%s, error) {\n", msg.MessageName, pbType)
		if hasTenant {
			b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
			fmt.Fprintf(b, "\tq := r.db.WithContext(ctx).Unscoped().Model(&%sModel{}).Where(\"id = ?\", key)\n", model)
			b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
			b.WriteString("\tq = q.Where(\"deleted_at IS NOT NULL\")\n")
		} else {
			fmt.Fprintf(b, "\tq := r.db.WithContext(ctx).Unscoped().Model(&%sModel{}).\n", model)
			b.WriteString("\t\tWhere(\"id = ?\", key).Where(\"deleted_at IS NOT NULL\")\n")
		}
		if useSentinel {
			// Clear the discriminator so the row re-enters the live unique namespace
			// (a 409 here, via the unique index, correctly means the key was taken
			// by another live row while this one was soft-deleted).
			b.WriteString("\tres := q.Updates(map[string]interface{}{\"deleted_at\": nil, \"soft_delete_key\": \"\"})\n")
		} else {
			b.WriteString("\tres := q.Update(\"deleted_at\", nil)\n")
		}
		b.WriteString("\tif res.Error != nil {\n")
		fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"undelete %s: %%w\", res.Error)\n", msg.MessageName)
		b.WriteString("\t}\n")
		b.WriteString("\tif res.RowsAffected == 0 {\n\t\treturn nil, persistence.ErrNotFound\n\t}\n")
		fmt.Fprintf(b, "\treturn r.Get(ctx, key)\n}\n\n")
	} else {
		// Stub: hard-delete resources have no soft-deleted rows to restore.
		fmt.Fprintf(b, "func (r *%sRepository) Undelete(_ context.Context, _ string) (%s, error) {\n", msg.MessageName, pbType)
		b.WriteString("\treturn nil, persistence.ErrNotFound\n}\n\n")
	}

	// PurgeExpired — emitted only when OWNER HasExpireTime (AIP-148 TTL hook).
	// The cutoff is normalized to UTC: toModel stores expire_time in UTC (via
	// timestamppb.AsTime), and on SQLite time columns are TZ-suffixed TEXT whose
	// comparison is format-sensitive — a local-time cutoff would not match a
	// UTC-stored value, so PurgeExpired(time.Now()) could silently reap nothing.
	if owner.HasExpireTime {
		fmt.Fprintf(b, "func (r *%sRepository) PurgeExpired(ctx context.Context, before time.Time) (int64, error) {\n", msg.MessageName)
		if hasTenant {
			b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
			b.WriteString("\tq := r.db.WithContext(ctx).Unscoped().Where(\"expire_time IS NOT NULL AND expire_time <= ?\", before.UTC())\n")
			b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
			fmt.Fprintf(b, "\tres := q.Delete(&%sModel{})\n", model)
		} else {
			fmt.Fprintf(b, "\tres := r.db.WithContext(ctx).Unscoped().\n")
			b.WriteString("\t\tWhere(\"expire_time IS NOT NULL AND expire_time <= ?\", before.UTC()).\n")
			fmt.Fprintf(b, "\t\tDelete(&%sModel{})\n", model)
		}
		b.WriteString("\tif res.Error != nil {\n")
		fmt.Fprintf(b, "\t\treturn 0, fmt.Errorf(\"purge expired %s: %%w\", res.Error)\n", msg.MessageName)
		b.WriteString("\t}\n")
		b.WriteString("\treturn res.RowsAffected, nil\n}\n\n")
	}

	// LookupByHash methods for secret fields. Model type follows the OWNER.
	for _, f := range msg.Fields {
		if !f.IsSecret {
			continue
		}
		gfn := goFieldName(f)
		resource := msg.MessageName
		lowerResource := strings.ToLower(resource[:1]) + resource[1:]
		fmt.Fprintf(b, "// LookupBy%sHash finds the %s by the hash of its %s field.\n", gfn, resource, gfn)
		b.WriteString("// Returns ErrNotFound when no record matches or when hash is empty.\n")
		fmt.Fprintf(b, "func (r *%sRepository) LookupBy%sHash(ctx context.Context, hash string) (%s, error) {\n", resource, gfn, pbType)
		b.WriteString("\tif hash == \"\" {\n\t\treturn nil, persistence.ErrNotFound\n\t}\n")
		if hasTenant {
			b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		}
		fmt.Fprintf(b, "\tq := r.db.WithContext(ctx).Where(\"%s_hash = ?\", hash)\n", f.SnakeName)
		if hasTenant {
			b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
		}
		fmt.Fprintf(b, "\tvar m %sModel\n", model)
		b.WriteString("\tif err := q.First(&m).Error; err != nil {\n")
		b.WriteString("\t\tif err == gorm.ErrRecordNotFound {\n")
		b.WriteString("\t\t\treturn nil, persistence.ErrNotFound\n")
		b.WriteString("\t\t}\n")
		fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"lookup %s by %s hash: %%w\", err)\n", lowerResource, f.SnakeName)
		b.WriteString("\t}\n")
		fmt.Fprintf(b, "\treturn fromModel_%s(&m), nil\n}\n\n", resource)
	}

	// Batch methods (AIP-137): atomic BatchGet/BatchUpdate/BatchDelete.
	// BatchGet: single IN query reassembled into key order; a missing or
	// soft-deleted key (excluded by the default scope) yields ErrNotFound.
	fmt.Fprintf(b, "func (r *%sRepository) BatchGet(ctx context.Context, keys []string) ([]%s, error) {\n", msg.MessageName, pbType)
	fmt.Fprintf(b, "\tif len(keys) == 0 {\n\t\treturn []%s{}, nil\n\t}\n", pbType)
	fmt.Fprintf(b, "\tvar models []%sModel\n", model)
	b.WriteString("\tq := r.db.WithContext(ctx).Where(\"id IN ?\", keys)\n")
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
		b.WriteString("\tif tenantID != \"\" {\n\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t}\n")
	}
	b.WriteString("\tif err := q.Find(&models).Error; err != nil {\n")
	fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"batch get %s: %%w\", err)\n", msg.MessageName)
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\tbyID := make(map[string]%s, len(models))\n", pbType)
	b.WriteString("\tfor i := range models {\n")
	fmt.Fprintf(b, "\t\tbyID[models[i].ID] = fromModel_%s(&models[i])\n", msg.MessageName)
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\tout := make([]%s, 0, len(keys))\n", pbType)
	b.WriteString("\tfor _, k := range keys {\n")
	b.WriteString("\t\tp, ok := byID[k]\n")
	b.WriteString("\t\tif !ok {\n\t\t\treturn nil, persistence.ErrNotFound\n\t\t}\n")
	b.WriteString("\t\tout = append(out, p)\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn out, nil\n}\n\n")

	// BatchUpdate: one transaction reusing the single Update per item (which
	// already applies the field mask, tenant scope, and returns ErrNotFound for a
	// missing/soft-deleted key); any item error rolls back the whole batch.
	fmt.Fprintf(b, "func (r *%sRepository) BatchUpdate(ctx context.Context, items []persistence.BatchUpdateItem[%s, string]) ([]%s, error) {\n", msg.MessageName, pbType, pbType)
	fmt.Fprintf(b, "\tif len(items) == 0 {\n\t\treturn []%s{}, nil\n\t}\n", pbType)
	fmt.Fprintf(b, "\tout := make([]%s, 0, len(items))\n", pbType)
	b.WriteString("\terr := r.db.Transaction(func(tx *gorm.DB) error {\n")
	if msgHasSecrets {
		fmt.Fprintf(b, "\t\ttxRepo := &%sRepository{db: tx, enc: r.enc}\n", msg.MessageName)
	} else {
		fmt.Fprintf(b, "\t\ttxRepo := &%sRepository{db: tx}\n", msg.MessageName)
	}
	b.WriteString("\t\tfor _, it := range items {\n")
	b.WriteString("\t\t\tupdated, err := txRepo.Update(ctx, it.Key, it.Entity, it.FieldMask...)\n")
	b.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn err\n\t\t\t}\n")
	b.WriteString("\t\t\tout = append(out, updated)\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t})\n")
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\treturn out, nil\n}\n\n")

	// BatchDelete: one transactional bulk delete; RowsAffected != len(uniq) ⇒
	// ErrNotFound (rollback). Keys are de-duplicated so the count check is exact;
	// already-soft-deleted rows are excluded by the default scope and so count short.
	fmt.Fprintf(b, "func (r *%sRepository) BatchDelete(ctx context.Context, keys []string) error {\n", msg.MessageName)
	b.WriteString("\tif len(keys) == 0 {\n\t\treturn nil\n\t}\n")
	b.WriteString("\tseen := make(map[string]struct{}, len(keys))\n")
	b.WriteString("\tuniq := make([]string, 0, len(keys))\n")
	b.WriteString("\tfor _, k := range keys {\n")
	b.WriteString("\t\tif _, ok := seen[k]; ok {\n\t\t\tcontinue\n\t\t}\n")
	b.WriteString("\t\tseen[k] = struct{}{}\n")
	b.WriteString("\t\tuniq = append(uniq, k)\n")
	b.WriteString("\t}\n")
	if hasTenant {
		b.WriteString("\ttenantID := middleware.TenantIDFromContext(ctx)\n")
	}
	b.WriteString("\treturn r.db.Transaction(func(tx *gorm.DB) error {\n")
	b.WriteString("\t\tq := tx.WithContext(ctx).Where(\"id IN ?\", uniq)\n")
	if hasTenant {
		b.WriteString("\t\tif tenantID != \"\" {\n\t\t\tq = q.Where(\"account_id = ?\", tenantID)\n\t\t}\n")
	}
	if owner.SoftDelete {
		fmt.Fprintf(b, "\t\tres := q.Delete(&%sModel{})\n", model)
	} else {
		fmt.Fprintf(b, "\t\tres := q.Unscoped().Delete(&%sModel{})\n", model)
	}
	b.WriteString("\t\tif res.Error != nil {\n")
	fmt.Fprintf(b, "\t\t\treturn fmt.Errorf(\"batch delete %s: %%w\", res.Error)\n", msg.MessageName)
	b.WriteString("\t\t}\n")
	b.WriteString("\t\tif res.RowsAffected != int64(len(uniq)) {\n\t\t\treturn persistence.ErrNotFound\n\t\t}\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t})\n}\n\n")

	// Compile-time interface check.
	fmt.Fprintf(b, "// compile-time check.\n")
	fmt.Fprintf(b, "var _ persistence.BatchRepository[%s, string] = (*%sRepository)(nil)\n\n", pbType, msg.MessageName)
}

// assocModelType returns the GORM model type name a relationship field points at
// (e.g. "DestinationModel"). It uses the related message's Go type captured by
// the plugin; the unit-test path may set GoType directly, so it falls back to
// stripping list/pointer markers off GoType.
func assocModelType(f fieldInfo) string {
	t := f.RelatedGoType
	if t == "" {
		t = strings.TrimPrefix(strings.TrimPrefix(f.GoType, "[]"), "*")
	}
	return t + "Model"
}

// snakeToCamel converts a snake_case string to CamelCase (e.g. "user_id" → "UserId").
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		parts[i] = ucFirst(p)
	}
	return strings.Join(parts, "")
}

func ucFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// goFieldName returns the Go struct field name for a proto field.
// When GoFieldName is populated by the protogen plugin (the authoritative source),
// use it. Otherwise fall back to ucFirst of the proto field name (unit-test path).
func goFieldName(f fieldInfo) string {
	if f.GoFieldName != "" {
		return f.GoFieldName
	}
	return ucFirst(f.Name)
}

// resourcenameIDVarName extracts the last {var} name from a resource name pattern.
// "widgets/{widget}" → "widget"; "projects/{project}/widgets/{widget}" → "widget".
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
