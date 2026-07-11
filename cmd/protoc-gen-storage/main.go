// Command protoc-gen-storage is a protoc/buf plugin that emits, for every
// proto message, a GORM-backed repository (.storage.go) implementing
// persistence.Repository[*pb.<Message>, string]:
//
//   - <Message>Model GORM struct with snake_case columns
//   - <Message>Repository with CRUD methods
//   - New<Message>Repository(*gorm.DB) constructor
//   - Compile-time persistence.Repository satisfaction check
//
// Generated code imports gorm.io/gorm; the consumer's go.mod provides GORM. The
// SDK's own clean core (top-level persistence, authz, grpcauthz) stays ORM-free —
// gorm.io/gorm is a dependency only of the sibling adapter persistence/gormtx,
// exactly as entgo.io/ent is confined to persistence/entrepo.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"strings"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	"github.com/infobloxopen/devedge-sdk/cmd/internal/searchgen"
	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	"github.com/infobloxopen/devedge-sdk/internal/aip"
	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flags flag.FlagSet
	// dialect selects the per-tenant-unique + soft-delete strategy (see
	// render.go targetDialect): "mysql" → a soft_delete_key discriminator column;
	// "postgres"/"sqlite" → a partial unique index (WHERE deleted_at IS NULL).
	dialect := flags.String("dialect", "postgres", "target SQL dialect: postgres|sqlite|mysql")
	protogen.Options{ParamFunc: flags.Set}.Run(func(gen *protogen.Plugin) error {
		targetDialect = *dialect
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		for _, f := range gen.Files {
			if f.Generate {
				generateFile(gen, f)
			}
		}
		return nil
	})
}

func generateFile(gen *protogen.Plugin, f *protogen.File) {
	var messages []messageInfo
	for _, m := range f.Messages {
		name := string(m.GoIdent.GoName)
		// Skip RPC request/response wrapper types — only resource messages get
		// a GORM model + Repository. Resource messages don't follow the
		// <Method>Request / <Method>Response naming convention.
		if strings.HasSuffix(name, "Request") || strings.HasSuffix(name, "Response") {
			continue
		}
		// Skip messages that have no "id" field — they are not storable resources
		// (e.g., AIP-151 operation-status and other value-object types).
		hasID := false
		for _, f := range m.Fields {
			if string(f.Desc.Name()) == "id" {
				hasID = true
				break
			}
		}
		if !hasID {
			continue
		}
		// F027 Phase 5b multi-surface: (infoblox.storage.v1.model) binds a message to
		// a backing storage model so several API surfaces can project ONE table.
		// Resolve the model name now (absent/empty => the message's own name, the
		// normal single-surface case). A surface (model != name) emits a repository
		// adapter + projection over its owner's GORM type but NO table of its own.
		resolvedModel := name
		if opts := m.Desc.Options(); opts != nil && proto.HasExtension(opts, storagev1.E_Model) {
			if mv, _ := proto.GetExtension(opts, storagev1.E_Model).(string); mv != "" {
				resolvedModel = mv
			}
		}
		msg := messageInfo{
			MessageName:  name,
			Model:        resolvedModel,
			PbPkgName:    string(f.GoPackageName),
			PbImportPath: string(f.GoImportPath),
		}
		// F031 DDD: read the SDK-owned infoblox.ddd.v1 message options (mirror
		// protoc-gen-ent main.go). (aggregate) marks an aggregate ROOT; (member)
		// marks a resource OWNED by a named root. A member→root containment drives
		// OnDelete: CASCADE on the owning GORM edge, and a root with containment
		// members gets a Load<Root>Aggregate graph-load primitive (Phase 2).
		if mopts := m.Desc.Options(); mopts != nil {
			if proto.HasExtension(mopts, dddv1.E_Aggregate) {
				if ag, ok := proto.GetExtension(mopts, dddv1.E_Aggregate).(*dddv1.Aggregate); ok && ag != nil {
					msg.AggregateRoot = ag.GetRoot()
				}
			}
			if proto.HasExtension(mopts, dddv1.E_Member) {
				if mb, ok := proto.GetExtension(mopts, dddv1.E_Member).(*dddv1.Member); ok && mb != nil {
					msg.MemberRoot = mb.GetRoot()
				}
			}
		}
		// Extract (google.api.resource) pattern from message options.
		if mopts := m.Desc.Options(); mopts != nil {
			if proto.HasExtension(mopts, apiannotations.E_Resource) {
				if rd, ok := proto.GetExtension(mopts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); ok {
					if patterns := rd.GetPattern(); len(patterns) > 0 {
						msg.ResourcePattern = patterns[0]
						// Format<R>Name fills exactly one id variable; a multi-segment
						// pattern leaves the parent variables empty and resourcename.Format
						// errors, so the AIP-122 name renders SILENTLY blank. Fail loud
						// until nested naming is implemented (DX run 26, finding 116).
						if strings.Count(msg.ResourcePattern, "{") > 1 {
							gen.Error(fmt.Errorf("protoc-gen-storage: %s: multi-segment resource pattern %q is not supported — Format%sName fills only one id, so parent segments render empty and the AIP-122 name is silently blank; use a single-segment pattern (e.g. %q)", msg.MessageName, msg.ResourcePattern, msg.MessageName, "entries/{entry}"))
						}
					}
				}
			}
		}
		for _, field := range m.Fields {
			var (
				isSecret         bool
				isCredential     bool
				credentialPrefix string
				isOutputOnly     bool
				isInputOnly      bool
				notNull          bool
				unique           bool
				uniqueWith       []string
				index            bool
				columnName       string
				columnType       string
				hasOne           *fieldv1.HasOne
				hasMany          *fieldv1.HasMany
				belongsTo        *fieldv1.BelongsTo
				manyToMany       *fieldv1.ManyToMany
				references       *dddv1.References
			)
			if opts := field.Desc.Options(); opts != nil {
				if proto.HasExtension(opts, fieldv1.E_Opts) {
					if fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions); ok {
						isSecret = fopts.GetSecret()
						isCredential = fopts.GetCredential()
						credentialPrefix = fopts.GetCredentialPrefix()
						notNull = fopts.GetNotNull()
						unique = fopts.GetUnique()
						uniqueWith = fopts.GetUniqueWith()
						index = fopts.GetIndex()
						columnName = fopts.GetColumnName()
						columnType = fopts.GetColumnType()
						hasOne = fopts.GetHasOne()
						hasMany = fopts.GetHasMany()
						belongsTo = fopts.GetBelongsTo()
						manyToMany = fopts.GetManyToMany()
						// BC-12 resource identity: the (infoblox.field.v1.opts).id
						// annotation on the id field controls how the primary key is
						// produced (server-generated vs user-settable; which built-in
						// generator). Captured on the message so Create can emit the
						// generate/guard before toModel_. Absent annotation => the
						// message's zero IdStrategy/IdGenerator, which render treats as
						// SERVER_GENERATED + UUID7 (the default).
						if string(field.Desc.Name()) == "id" {
							if id := fopts.GetId(); id != nil {
								msg.IdStrategy = id.GetStrategy()
								msg.IdGenerator = id.GetGenerator()
							}
						}
					}
				}
				// F031 DDD: (infoblox.ddd.v1.references) is a CROSS-aggregate link (scalar
				// FK + ID, no association) — distinct from the WITHIN-aggregate containment
				// edges that drive cascade/graph-load. Like ent, the references field is
				// dropped from the model (only the sibling scalar FK column persists), so
				// events reference the other aggregate by id only. Reading it here marks
				// the field as relationship-mapped for the fail-closed coverage check.
				if proto.HasExtension(opts, dddv1.E_References) {
					if rf, ok := proto.GetExtension(opts, dddv1.E_References).(*dddv1.References); ok && rf != nil {
						references = rf
					}
				}
				oo, err := aip.IsOutputOnly(field.Desc)
				if err != nil {
					gen.Error(err)
				}
				isOutputOnly = oo
				// SEC-007: an effective INPUT_ONLY field (explicit field_behavior or
				// derived from secret) is write-only — omitted from the response
				// projection, matching the OpenAPI writeOnly stamp.
				if bs, berr := aip.ResolveFieldBehavior(field.Desc); berr != nil {
					gen.Error(berr)
				} else {
					isInputOnly = aip.HasBehavior(bs, aip.InputOnly)
				}
			}
			// AIP-148: detect soft-delete and TTL markers. These are handled specially
			// by the renderer and must NOT be added to msg.Fields as ordinary columns.
			fieldName := string(field.Desc.Name())
			isTimestamp := field.Desc.Kind() == protoreflect.MessageKind &&
				field.Desc.Message() != nil &&
				field.Desc.Message().FullName() == "google.protobuf.Timestamp"
			if isOutputOnly && isTimestamp {
				if fieldName == "delete_time" {
					msg.SoftDelete = true
					continue // handled by renderer; not an ordinary column
				}
				if fieldName == "expire_time" {
					msg.HasExpireTime = true
					continue // handled by renderer; not an ordinary column
				}
			}
			// AIP-154 ETag: a string `etag` field is framework-managed. The model
			// already carries an `etag` column unconditionally; the renderer stamps
			// it on every write and surfaces it on read. Skip it as an ordinary
			// column (emitting it would duplicate the `etag` column).
			if fieldName == "etag" && field.Desc.Kind() == protoreflect.StringKind {
				msg.HasETag = true
				continue
			}
			// For message-kind fields (relationships), capture the related message's
			// Go type name so the renderer can emit a concrete GORM association
			// (*<Related>Model) instead of an unusable interface{}.
			relatedGoType := ""
			if field.Message != nil {
				relatedGoType = string(field.Message.GoIdent.GoName)
			}
			// A proto map<string, string> is the Tags field kind: persisted as a
			// single JSONB column (types.Tags), not a nested message or relation. A
			// map arrives as Kind()==MessageKind with IsMap()==true and a synthetic
			// map-entry message, so it must be detected here or it falls into the
			// nested-message skip below. Non-string maps keep the old skip behavior.
			isStringMap := field.Desc.IsMap() &&
				field.Desc.MapKey().Kind() == protoreflect.StringKind &&
				field.Desc.MapValue().Kind() == protoreflect.StringKind
			msg.Fields = append(msg.Fields, fieldInfo{
				Name:             string(field.Desc.Name()),
				GoFieldName:      string(field.GoName),
				SnakeName:        toSnake(string(field.Desc.Name())),
				IsRepeated:       field.Desc.IsList(),
				IsMessage:        field.Desc.Kind() == protoreflect.MessageKind && !isStringMap,
				IsEnum:           field.Desc.Kind() == protoreflect.EnumKind,
				IsTags:           isStringMap,
				IsID:             string(field.Desc.Name()) == "id",
				GoType:           protoKindToGoType(field.Desc.Kind()),
				RelatedGoType:    relatedGoType,
				IsSecret:         isSecret,
				IsCredential:     isCredential,
				CredentialPrefix: credentialPrefix,
				IsOutputOnly:     isOutputOnly,
				IsInputOnly:      isInputOnly,
				NotNull:          notNull,
				Unique:           unique,
				UniqueWith:       uniqueWith,
				Index:            index,
				ColumnName:       columnName,
				ColumnType:       columnType,
				HasOne:           hasOne,
				HasMany:          hasMany,
				BelongsTo:        belongsTo,
				ManyToMany:       manyToMany,
				References:       references,
			})
		}

		// WS-041 full-text search: resolve the declared search surface (FR-A1) and
		// compile it now so the generated GORM List can embed the `q` predicate for
		// BOTH runtime dialects. Compiling for Postgres is authoritative: it never
		// fails on portability and yields the Postgres to_tsvector argument, the
		// parallel SQLite concatenation (for a portable resource), and the
		// PostgresOnly flag — everything the runtime dialect branch in render.go
		// needs from one call. It DOES fail loud (aborting codegen) on a leaky or
		// non-textual searchable field, a reserved PROJECTED strategy, an unknown
		// flavor, or an unsupported sql dialect (FR-A2/A3/A5, FM-1/FM-7).
		if sc, err := aip.ResolveSearchConfig(m.Desc); err != nil {
			gen.Error(err)
		} else if sc.IsSearchable() {
			compiled, cerr := searchgen.Compile(sc, m.Desc, searchgen.DialectPostgres)
			if cerr != nil {
				gen.Error(cerr)
			} else if compiled != nil {
				msg.Search = &searchInfo{
					PostgresVector: compiled.PostgresVector,
					SQLiteVector:   compiled.SQLiteVector,
					PostgresOnly:   compiled.PostgresOnly,
					TextConfig:     compiled.TextConfig,
					Indexed:        compiled.IsIndexed(),
				}
				// INDEXED needs a pinned backing-table name (emitted TableName + the
				// migration's ALTER TABLE / CREATE INDEX target, FR-C2).
				if compiled.IsIndexed() {
					msg.Search.Table = searchTableName(name)
				}
			}
		}

		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		return
	}

	// BC-07 scoped unique (unique_with): validate before rendering. A field's
	// unique_with lists sibling columns that join its per-tenant composite unique
	// index. It requires unique=true and an account_id field on the message, and
	// each named field must be a scalar sibling (not the field itself).
	for _, msg := range messages {
		hasTenant := false
		scalar := map[string]bool{}
		for _, f := range msg.Fields {
			if f.Name == "account_id" || f.SnakeName == "account_id" {
				hasTenant = true
			}
			if !f.IsID && !f.IsRepeated && !f.IsMessage && !f.IsSecret && !f.IsCredential {
				scalar[f.Name] = true
				scalar[f.SnakeName] = true
			}
		}
		for _, f := range msg.Fields {
			if len(f.UniqueWith) == 0 {
				continue
			}
			if !f.Unique {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: unique_with requires unique: true", msg.MessageName, f.Name))
			}
			if !hasTenant {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: unique_with requires an account_id field on the message", msg.MessageName, f.Name))
			}
			for _, w := range f.UniqueWith {
				if w == f.Name || w == f.SnakeName {
					gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: unique_with cannot reference the field itself", msg.MessageName, f.Name))
				} else if !scalar[w] {
					gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: unique_with references %q, which is not a scalar field on the message", msg.MessageName, f.Name, w))
				}
			}
		}
	}

	// WS-033 credential guards (fail-loud in codegen): a verify-only credential
	// field is mutually exclusive with secret and its type must be string. Fail
	// generation with an actionable error rather than emitting wrong storage.
	credOK := true
	for _, msg := range messages {
		for _, f := range msg.Fields {
			if !f.IsCredential {
				continue
			}
			if f.IsSecret {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: credential and secret are mutually exclusive on a field", msg.MessageName, f.Name))
				credOK = false
			}
			if f.GoType != "string" {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: credential requires a string field", msg.MessageName, f.Name))
				credOK = false
			}
		}
	}
	if !credOK {
		return
	}

	// F027 fail-closed (G-002): every resource field must be deterministically
	// wirable into the generated repository adapter. A field with no mapping — a
	// nested non-relationship message, a repeated non-relationship field, an enum,
	// a non-string map — would otherwise be silently dropped. Fail generation
	// instead, naming the field and the remedy, using the engine-neutral classifier
	// shared with protoc-gen-ent (G-005).
	failed := false
	for _, msg := range messages {
		_, unmapped := storagegen.Classify(toStorageFields(msg))
		for _, uf := range unmapped {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: %s", msg.MessageName, uf.Name, storagegen.Reason(uf)))
			failed = true
		}
	}
	if failed {
		return
	}

	// F027 Phase 5b multi-surface: index every message by name so a surface can be
	// matched to its owner, then validate the surfaces fail-closed.
	ownerByName := make(map[string]messageInfo, len(messages))
	for _, msg := range messages {
		ownerByName[msg.MessageName] = msg
	}
	if !validateSurfaces(gen, messages, ownerByName) {
		return
	}

	// Storage code lives in the same package as the pb types (same directory).
	// This keeps the generated file co-located with widgets.pb.go so the GORM
	// model can reference proto types without a package qualifier.
	// Pass an empty PbPkgName to renderStorageFile to skip the qualifier.
	for i := range messages {
		messages[i].PbPkgName = "" // same package — no qualifier needed
	}
	content := renderStorageFile(string(f.GoPackageName), messages, ownerByName)
	if content == "" {
		return
	}
	// Emit gofmt-clean output so a repo that gates CI on `gofmt -l` does not fail
	// on generated code it is told never to hand-edit. Fall back to the raw
	// content if formatting fails, so a render bug still emits debuggable output.
	if formatted, err := format.Source([]byte(content)); err == nil {
		content = string(formatted)
	}

	outPath := f.GeneratedFilenamePrefix + ".storage.go"
	g := gen.NewGeneratedFile(outPath, f.GoImportPath)
	g.P(content)

	emitSearchMigrations(gen, f, messages)
}

// emitSearchMigrations writes the Postgres migration files for every INDEXED
// searchable resource in the proto file: a persisted `search_vector` generated
// column and its CONCURRENTLY GIN index (WS-041 SD-7, FR-C2). Files land under the
// module's `migrations/` dir (the WS-012 module-owned convention) as generated
// artifacts, so emission is deterministic and idempotent — `make generate` twice
// yields byte-identical files (FM-4/AC-7).
//
// The version is a FIXED number in a reserved band (searchMigrationBaseVersion),
// allocated by the resource's order among the file's INDEXED resources — NOT the
// next-free number on disk, which is what makes it diff-clean. The band sits far
// above a service's hand-authored 0002+ domain migrations so a generated file never
// collides with (or is confused for) a hand-written one; the migrate engine applies
// in numeric order and permits gaps.
func emitSearchMigrations(gen *protogen.Plugin, f *protogen.File, messages []messageInfo) {
	// Module-relative migrations dir: the generated package sits one level under the
	// module root (e.g. …/testdata/toy/widgetsv1), so the module's migrations dir is
	// its parent + /migrations. The path must stay module-qualified for protogen's
	// module= trimming; it strips the module prefix to place files under buf's out.
	pkg := string(f.GoImportPath)
	slash := strings.LastIndex(pkg, "/")
	if slash < 0 {
		return
	}
	migDir := pkg[:slash] + "/migrations"

	version := searchMigrationBaseVersion
	for _, msg := range messages {
		if msg.Search == nil || !msg.Search.Indexed {
			continue
		}
		files := searchgen.BuildIndexedMigration(msg.Search.Table, msg.Search.TextConfig, msg.Search.PostgresVector, version)
		for _, mf := range files {
			gf := gen.NewGeneratedFile(migDir+"/"+mf.Name, "")
			gf.P(mf.Body)
		}
		version += 2 // this resource used columnVersion and columnVersion+1
	}
}

// searchMigrationBaseVersion is the reserved starting version for generated
// full-text-search migrations. It sits far above hand-authored domain migrations
// (the scaffold README starts those at 0002) so a generated search_vector/GIN file
// never collides with a hand-written migration; the migrate engine applies by
// numeric order and allows the gap.
const searchMigrationBaseVersion = 9001

// searchTableName derives the backing table name pinned for an INDEXED resource:
// the snake_case resource name, simply pluralized. It is emitted as the model's
// TableName so the GORM table and the generated migration cannot drift.
func searchTableName(message string) string {
	return pluralize(toSnakeCase(message))
}

// toSnakeCase converts a CamelCase proto message name to snake_case.
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pluralize applies the minimal English pluralization the fixtures need (regular
// nouns + the common -y/-s/-x/-ch/-sh cases). Table names for INDEXED resources are
// chosen to pluralize regularly.
func pluralize(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "y") && !endsInVowelY(s):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	default:
		return s + "s"
	}
}

func endsInVowelY(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[len(s)-2] {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// validateSurfaces enforces the F027 Phase 5b multi-surface contract fail-closed.
// A SURFACE message (one whose (infoblox.storage.v1.model) names a different
// message) projects an OWNER's table, so the generated adapter writes/reads the
// owner's GORM columns. Returns true when all surfaces are valid.
func validateSurfaces(gen *protogen.Plugin, messages []messageInfo, ownerByName map[string]messageInfo) bool {
	ok := true
	for _, msg := range messages {
		if !msg.isSurface() {
			continue
		}
		owner, found := ownerByName[msg.Model]
		if !found {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: (infoblox.storage.v1.model)=%q names a model with no message in this proto; declare a message %s or correct the annotation", msg.MessageName, msg.Model, msg.Model))
			ok = false
			continue
		}
		if owner.isSurface() {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: (infoblox.storage.v1.model)=%q points at another surface; a surface must name a base model", msg.MessageName, msg.Model))
			ok = false
			continue
		}
		ownerFields := make(map[string]fieldInfo, len(owner.Fields))
		for _, of := range owner.Fields {
			ownerFields[of.SnakeName] = of
		}
		if msgHasTenantField(owner) && !msgHasTenantField(msg) {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: a surface of the tenant-scoped model %s must include the account_id field so the adapter can scope and stamp the tenant", msg.MessageName, msg.Model))
			ok = false
		}
		for _, f := range msg.Fields {
			if f.IsID {
				continue // every model has the id primary key
			}
			if f.IsRepeated || f.IsMessage {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: relationships/nested messages are not supported on a model surface; declare them on the model %s", msg.MessageName, f.Name, msg.Model))
				ok = false
				continue
			}
			of, has := ownerFields[f.SnakeName]
			if !has {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: a surface must project a subset of model %s's fields, but %s has no field %q", msg.MessageName, f.Name, msg.Model, msg.Model, f.SnakeName))
				ok = false
				continue
			}
			if f.GoType != of.GoType || f.IsSecret != of.IsSecret || f.IsCredential != of.IsCredential || f.IsOutputOnly != of.IsOutputOnly {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s conflicts with model %s.%s: a surface field must match the model column's type and secret/credential/output classification", msg.MessageName, f.Name, msg.Model, of.Name))
				ok = false
			}
		}
		// Framework parity checks: surface flags must be compatible with owner.
		if msg.SoftDelete && !owner.SoftDelete {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares soft-delete but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
		if msg.HasExpireTime && !owner.HasExpireTime {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares expire_time but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
		if msg.HasETag && !owner.HasETag {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares etag but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
	}
	return ok
}

func protoKindToGoType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	case protoreflect.BytesKind:
		return "[]byte"
	default:
		return "interface{}" // enum, message — caller checks IsMessage separately
	}
}

// toSnake converts camelCase or snake_case to snake_case.
func toSnake(s string) string {
	// proto field names are already snake_case by convention.
	return strings.ToLower(s)
}
