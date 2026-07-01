// Command protoc-gen-ent is a protoc/buf plugin that emits, for every proto
// resource message, an ent schema definition (ent/schema/<snake_resource>.go)
// plus an ent/generate.go that drives entc code generation:
//
//   - <Message> struct embedding ent.Schema
//   - Mixin() returning entrepo.TenantMixin when the message has account_id
//   - Fields() mirroring proto fields (id annotated as the primary key,
//     account_id supplied by TenantMixin, secret fields split into
//     <name>_hash + <name>_cipher)
//   - Indexes() with a key index per secret field's _hash column
//   - ent/generate.go with the //go:generate entc directive
//
// Generated schemas import entrepo for TenantMixin; the consumer's go.mod
// provides ent. devedge-sdk already depends on entgo.io/ent.
package main

import (
	"flag"
	"fmt"
	"path"
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flags flag.FlagSet
	// dialect selects the per-tenant-unique + soft-delete strategy: on "mysql"
	// (no partial indexes) a soft_delete_key discriminator column is emitted; on
	// "postgres"/"sqlite" a partial unique index (WHERE delete_time IS NULL) is
	// used instead. See render.go (targetDialect).
	dialect := flags.String("dialect", "postgres", "target SQL dialect: postgres|sqlite|mysql")
	// with_storage signals that protoc-gen-storage (the GORM backend) also runs in
	// the same buf.gen invocation into the same Go package. When true, this plugin
	// does NOT emit the package-level AIP-122 resource-name helpers (<R>NamePattern /
	// Format<R>Name / Parse<R>Name) — storage already owns them, and duplicating them
	// would not compile. An ent-only service (the normal scaffold) leaves it false.
	withStorageOpt := flags.Bool("with_storage", false, "protoc-gen-storage also runs into the same package; do not emit shared resource-name helpers")
	protogen.Options{ParamFunc: flags.Set}.Run(func(gen *protogen.Plugin) error {
		targetDialect = *dialect
		withStorage = *withStorageOpt
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
	var messages []entMessageInfo
	for _, m := range f.Messages {
		name := string(m.GoIdent.GoName)
		// Skip RPC request/response wrapper types — only resource messages get
		// an ent schema. Resource messages don't follow the
		// <Method>Request / <Method>Response naming convention.
		if strings.HasSuffix(name, "Request") || strings.HasSuffix(name, "Response") {
			continue
		}
		// Only messages that have an "id" primary key are stored resources.
		// Transport types without an id — request/response payloads not caught
		// by the suffix check, and consumer-declared LRO Operation messages —
		// must NOT get an ent schema (a PK-less ent entity is invalid). This
		// mirrors protoc-gen-storage's resource-detection rule exactly.
		hasID := false
		for _, field := range m.Fields {
			if string(field.Desc.Name()) == "id" {
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
		// adapter + projection over its owner's ent type but NO table of its own;
		// surfaces are validated against their owner and skipped for schema emission
		// below (validateSurfaces / the renderEntSchema loop).
		resolvedModel := name
		if opts := m.Desc.Options(); opts != nil && proto.HasExtension(opts, storagev1.E_Model) {
			if mv, _ := proto.GetExtension(opts, storagev1.E_Model).(string); mv != "" {
				resolvedModel = mv
			}
		}
		msg := entMessageInfo{MessageName: name, Model: resolvedModel}
		// F031 DDD: read the SDK-owned infoblox.ddd.v1 message options. (aggregate)
		// marks an aggregate ROOT; (member) marks a resource OWNED by a named root.
		// A member→root containment drives OnDelete: Cascade on the owning edge and
		// the boundary gate / member write-redirection in protoc-gen-svc.
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
		// AIP-122: capture the (google.api.resource) pattern so an OUTPUT_ONLY `name`
		// field can be DERIVED from id (Format<R>Name) rather than stored — matching
		// protoc-gen-storage. Without the pattern there is nothing to format from.
		if mopts := m.Desc.Options(); mopts != nil && proto.HasExtension(mopts, apiannotations.E_Resource) {
			if rd, ok := proto.GetExtension(mopts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); ok {
				if patterns := rd.GetPattern(); len(patterns) > 0 {
					msg.ResourcePattern = patterns[0]
				}
			}
		}
		for _, field := range m.Fields {
			var (
				isSecret     bool
				isOutputOnly bool
				notNull      bool
				unique       bool
				uniqueWith   []string
				index        bool
				hasOne       *fieldv1.HasOne
				hasMany      *fieldv1.HasMany
				belongsTo    *fieldv1.BelongsTo
				manyToMany   *fieldv1.ManyToMany
				references   *dddv1.References
			)
			if opts := field.Desc.Options(); opts != nil {
				if proto.HasExtension(opts, fieldv1.E_Opts) {
					if fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions); ok {
						isSecret = fopts.GetSecret()
						notNull = fopts.GetNotNull()
						unique = fopts.GetUnique()
						uniqueWith = fopts.GetUniqueWith()
						index = fopts.GetIndex()
						hasOne = fopts.GetHasOne()
						hasMany = fopts.GetHasMany()
						belongsTo = fopts.GetBelongsTo()
						manyToMany = fopts.GetManyToMany()
						// BC-12 resource identity: the (infoblox.field.v1.opts).id
						// annotation on the id field controls how the primary key is
						// produced (server-generated vs user-settable; which built-in
						// generator). Captured on the message so the Create_ closure can
						// emit the generate/guard. Absent annotation => the message's
						// zero IdStrategy/IdGenerator, which render treats as
						// SERVER_GENERATED + UUID7 (the default).
						if string(field.Desc.Name()) == "id" {
							if id := fopts.GetId(); id != nil {
								msg.IdStrategy = id.GetStrategy()
								msg.IdGenerator = id.GetGenerator()
							}
						}
					}
				}
				// F031 DDD: (infoblox.ddd.v1.references) is a CROSS-aggregate link —
				// a scalar FK + ID with NO traversable edge (so code cannot walk or
				// mutate across roots). Unlike belongs_to (within-aggregate
				// containment), the message-kind field carrying it is NOT emitted as
				// an edge; only its foreign_key scalar column is emitted.
				if proto.HasExtension(opts, dddv1.E_References) {
					if rf, ok := proto.GetExtension(opts, dddv1.E_References).(*dddv1.References); ok && rf != nil {
						references = rf
					}
				}
				if proto.HasExtension(opts, apiannotations.E_FieldBehavior) {
					behaviors, _ := proto.GetExtension(opts, apiannotations.E_FieldBehavior).([]apiannotations.FieldBehavior)
					for _, b := range behaviors {
						if b == apiannotations.FieldBehavior_OUTPUT_ONLY {
							isOutputOnly = true
						}
					}
				}
			}
			// For message-kind fields (relationships), capture the related
			// message's Go type name so the edge target references the actual
			// ent schema struct (e.g. Vehicle) rather than a name derived from
			// the — possibly pluralized — proto field name (e.g. "vehicles").
			relatedType := ""
			if field.Message != nil {
				relatedType = string(field.Message.GoIdent.GoName)
			}
			// A proto map<string, string> is the Tags field kind: an ent JSON field,
			// not a nested message or edge. A map arrives as Kind()==MessageKind with
			// IsMap()==true, so detect it here or it falls into the nested-message
			// skip below. Non-string maps keep the old skip behavior.
			isStringMap := field.Desc.IsMap() &&
				field.Desc.MapKey().Kind() == protoreflect.StringKind &&
				field.Desc.MapValue().Kind() == protoreflect.StringKind
			// AIP-148: detect soft-delete and TTL markers.
			fieldName := string(field.Desc.Name())
			isTimestamp := field.Desc.Kind() == protoreflect.MessageKind &&
				field.Desc.Message() != nil &&
				field.Desc.Message().FullName() == "google.protobuf.Timestamp"
			if isOutputOnly && isTimestamp {
				if fieldName == "delete_time" {
					msg.SoftDelete = true
					continue // SoftDeleteMixin owns this field
				}
				if fieldName == "expire_time" {
					msg.HasExpireTime = true
					continue // emitted as a direct Time field in renderEntSchema
				}
			}
			// AIP-154 ETag is framework-managed: EtagMixin supplies the `etag`
			// column AND a mutation hook that stamps a fresh token on every
			// Create/Update (mirroring the GORM storage layer). It is owned by the
			// mixin, so it is not emitted as a direct field here.
			if fieldName == "etag" && field.Desc.Kind() == protoreflect.StringKind {
				msg.HasETag = true
				continue // EtagMixin owns this field
			}
			msg.Fields = append(msg.Fields, entFieldInfo{
				Name:        string(field.Desc.Name()),
				SnakeName:   toSnake(string(field.Desc.Name())),
				EntType:     protoKindToEntType(field.Desc.Kind()),
				IsID:        string(field.Desc.Name()) == "id",
				IsRepeated:  field.Desc.IsList(),
				IsMessage:   field.Desc.Kind() == protoreflect.MessageKind && !isStringMap,
				IsEnum:      field.Desc.Kind() == protoreflect.EnumKind,
				IsTags:      isStringMap,
				IsSecret:    isSecret,
				OutputOnly:  isOutputOnly,
				NotNull:     notNull,
				Unique:      unique,
				UniqueWith:  uniqueWith,
				Index:       index,
				RelatedType: relatedType,
				HasOne:      hasOne,
				HasMany:     hasMany,
				BelongsTo:   belongsTo,
				ManyToMany:  manyToMany,
				References:  references,
			})
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
			if !f.IsID && !f.IsRepeated && !f.IsMessage && !f.IsSecret {
				scalar[f.Name] = true
				scalar[f.SnakeName] = true
			}
		}
		for _, f := range msg.Fields {
			if len(f.UniqueWith) == 0 {
				continue
			}
			if !f.Unique {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: unique_with requires unique: true", msg.MessageName, f.Name))
			}
			if !hasTenant {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: unique_with requires an account_id field on the message", msg.MessageName, f.Name))
			}
			for _, w := range f.UniqueWith {
				if w == f.Name || w == f.SnakeName {
					gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: unique_with cannot reference the field itself", msg.MessageName, f.Name))
				} else if !scalar[w] {
					gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: unique_with references %q, which is not a scalar field on the message", msg.MessageName, f.Name, w))
				}
			}
		}
	}

	// F027 fail-closed (G-002): every resource field must be deterministically
	// wirable into the generated repository adapter. A field with no mapping — a
	// nested non-relationship message, a repeated non-relationship field, an enum,
	// a non-string map — would otherwise be silently dropped from the schema and
	// the adapter. Fail generation instead, naming the field and the remedy, using
	// the engine-neutral classifier shared with protoc-gen-storage (G-005).
	failed := false
	for _, msg := range messages {
		_, unmapped := storagegen.Classify(toStorageFields(msg))
		for _, uf := range unmapped {
			gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: %s", msg.MessageName, uf.Name, storagegen.Reason(uf)))
			failed = true
		}
	}
	if failed {
		return
	}

	// F027 Phase 5b multi-surface: index every message by name so a surface can be
	// matched to its owner, then validate the surfaces fail-closed. A surface that
	// references a missing/invalid owner, projects a field the owner cannot back, or
	// disagrees with the owner on a column's type/secret/output classification would
	// otherwise generate code that does not compile — fail with an actionable error.
	ownerByName := make(map[string]entMessageInfo, len(messages))
	for _, msg := range messages {
		ownerByName[msg.MessageName] = msg
	}
	if !validateSurfaces(gen, messages, ownerByName) {
		return
	}

	// One schema file per OWNER resource message: ent/schema/<snake_resource>.go.
	// A surface (model != name) gets no table of its own — it projects the owner's.
	// Pass the full message set so a belongs_to can be paired with its parent's
	// has_many as a proper ent inverse edge (edge.From(...).Ref(...)).
	for _, msg := range messages {
		if msg.isSurface() {
			continue
		}
		content := renderEntSchema(msg, messages)
		if content == "" {
			continue
		}
		outPath := "ent/schema/" + toSnake(msg.MessageName) + ".go"
		g := gen.NewGeneratedFile(outPath, f.GoImportPath)
		g.P(content)
	}

	// ent/generate.go drives entc once for the whole schema package.
	gg := gen.NewGeneratedFile("ent/generate.go", f.GoImportPath)
	gg.P(renderGenerateFile())

	// F026: per-resource batch repository wrapper, emitted into the proto's Go
	// package (alongside the hand-written ent wiring it embeds). Gives the
	// ent-backed repository atomic AIP-137 BatchGet/BatchUpdate/BatchDelete.
	pkgName := string(f.GoPackageName)
	for _, msg := range messages {
		content := renderEntRepository(msg, ownerByName[msg.modelType()], pkgName, string(f.GoImportPath))
		if content == "" {
			continue
		}
		outPath := pkgName + "/" + toSnake(msg.MessageName) + ".batch.ent.go"
		bg := gen.NewGeneratedFile(outPath, f.GoImportPath)
		bg.P(content)
	}

	// Per-resource query filterers (ent/<snake>_filter.ent.go): WhereAccountID and
	// WhereDeleteTimeIsNil, so each generated <Resource>Query satisfies
	// entrepo.TenantFilterer / SoftDeleteFilterer. The mixin interceptors call
	// these by interface assertion; without them tenant isolation and soft-delete
	// silently no-op while still compiling (GH #39). Emitted into package ent.
	for _, msg := range messages {
		content := renderEntFilterers(msg, string(f.GoImportPath))
		if content == "" {
			continue
		}
		outPath := "ent/" + toSnake(msg.MessageName) + "_filter.ent.go"
		fg := gen.NewGeneratedFile(outPath, f.GoImportPath)
		fg.P(content)
	}

	// GH #47: per-resource ent column maps (<pkg>/<snake>.columns.ent.go) so an
	// ent-only service can wire AIP-160 filter / AIP-132 order_by / tag filtering
	// without hand-maintaining the proto-field→column whitelist. Emitted into the
	// proto's Go package with ent-suffixed names so they coexist with the GORM
	// backend's <Msg>Columns when both generators run.
	for _, msg := range messages {
		content := renderEntColumns(msg, pkgName)
		if content == "" {
			continue
		}
		outPath := pkgName + "/" + toSnake(msg.MessageName) + ".columns.ent.go"
		cg := gen.NewGeneratedFile(outPath, f.GoImportPath)
		cg.P(content)
	}

	// F027: per-resource repository adapter (<pkg>/<snake>_repo.ent.go) — the
	// New<R>EntRepository constructor (the six persistence.Repository closures),
	// the fromEnt<R> projection, and a LookupBy<Secret>Hash per secret field.
	// Generated so an ent service needs NO hand-written ent_wiring.go; the batch
	// wrapper above embeds the New<R>EntRepository this emits.
	for _, msg := range messages {
		content := renderEntRepoAdapter(msg, ownerByName[msg.modelType()], pkgName, string(f.GoImportPath))
		if content == "" {
			continue
		}
		outPath := pkgName + "/" + toSnake(msg.MessageName) + "_repo.ent.go"
		rg := gen.NewGeneratedFile(outPath, f.GoImportPath)
		rg.P(content)
	}

	// F030 (T5): one ent-backed persistence.TxRunner per package, wrapping the
	// *ent.Client. The tx-aware repository resolvers emitted above bind to the
	// *ent.Tx it stashes on ctx, so writes issued inside Atomically are
	// transaction-scoped. The ent import path is the same one the adapters use
	// (path.Dir(goImportPath) + "/ent").
	entImport := path.Dir(string(f.GoImportPath)) + "/ent"
	txg := gen.NewGeneratedFile(pkgName+"/tx.ent.go", f.GoImportPath)
	txg.P(renderEntTxRunner(pkgName, entImport))
}

// validateSurfaces enforces the F027 Phase 5b multi-surface contract fail-closed.
// A SURFACE message (one whose (infoblox.storage.v1.model) names a different
// message) projects an OWNER's table, so the generated adapter writes/reads the
// owner's ent columns. Every way that could produce non-compiling or silently
// wrong code is rejected here with an actionable error, before any file is
// emitted: a missing or non-base owner, a surface that declares a relationship /
// nested message, a surface field the owner has no column for, a column whose
// type or secret/output classification disagrees with the owner, and a surface of
// a tenant-scoped model that omits account_id (the adapter must be able to scope
// and stamp the tenant). Returns true when all surfaces are valid.
func validateSurfaces(gen *protogen.Plugin, messages []entMessageInfo, ownerByName map[string]entMessageInfo) bool {
	ok := true
	for _, msg := range messages {
		if !msg.isSurface() {
			continue
		}
		owner, found := ownerByName[msg.Model]
		if !found {
			gen.Error(fmt.Errorf("protoc-gen-ent: %s: (infoblox.storage.v1.model)=%q names a model with no message in this proto; declare a message %s or correct the annotation", msg.MessageName, msg.Model, msg.Model))
			ok = false
			continue
		}
		if owner.isSurface() {
			gen.Error(fmt.Errorf("protoc-gen-ent: %s: (infoblox.storage.v1.model)=%q points at another surface; a surface must name a base model (a message whose name equals its own model)", msg.MessageName, msg.Model))
			ok = false
			continue
		}
		ownerFields := make(map[string]entFieldInfo, len(owner.Fields))
		for _, of := range owner.Fields {
			ownerFields[of.SnakeName] = of
		}
		if msgHasTenantField(owner) && !msgHasTenantField(msg) {
			gen.Error(fmt.Errorf("protoc-gen-ent: %s: a surface of the tenant-scoped model %s must include the account_id field so the adapter can scope and stamp the tenant", msg.MessageName, msg.Model))
			ok = false
		}
		for _, f := range msg.Fields {
			if f.IsID {
				continue // every model has the id primary key
			}
			if f.IsRepeated || f.IsMessage {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: relationships/nested messages are not supported on a model surface; declare them on the model %s", msg.MessageName, f.Name, msg.Model))
				ok = false
				continue
			}
			of, has := ownerFields[f.SnakeName]
			if !has {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s: a surface must project a subset of model %s's fields, but %s has no field %q", msg.MessageName, f.Name, msg.Model, msg.Model, f.SnakeName))
				ok = false
				continue
			}
			if f.EntType != of.EntType || f.IsSecret != of.IsSecret || f.OutputOnly != of.OutputOnly {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s.%s conflicts with model %s.%s: a surface field must match the model column's type and secret/output classification", msg.MessageName, f.Name, msg.Model, of.Name))
				ok = false
			}
		}
	}
	return ok
}

// protoKindToEntType maps a proto field kind to the ent field constructor name
// (the method on entgo.io/ent/schema/field). Unsupported kinds fall back to
// "String"; callers handle repeated/message fields separately with TODO comments.
func protoKindToEntType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "String"
	case protoreflect.BoolKind:
		return "Bool"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "Int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "Int64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "Uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "Uint64"
	case protoreflect.FloatKind:
		return "Float32"
	case protoreflect.DoubleKind:
		return "Float"
	case protoreflect.BytesKind:
		return "Bytes"
	default:
		return "String" // enum, message — caller checks IsMessage separately
	}
}

// toSnake converts a CamelCase or snake_case identifier to snake_case.
//
// Proto field names already arrive snake_case, so for those it is a simple
// lower-casing. Message names (e.g. "APIKey") arrive CamelCase and must be
// split on case boundaries to produce the schema filename (e.g. "api_key").
func toSnake(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			prev := runes[i-1]
			prevLower := prev >= 'a' && prev <= 'z'
			prevDigit := prev >= '0' && prev <= '9'
			// Insert an underscore at the start of a new word: either a
			// lower→upper boundary (apiKey → api_key) or the end of an
			// acronym run before a trailing word (APIKey → api_key).
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			prevUpper := prev >= 'A' && prev <= 'Z'
			if prevLower || prevDigit || (prevUpper && nextLower) {
				b.WriteByte('_')
			}
		}
		if isUpper {
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
