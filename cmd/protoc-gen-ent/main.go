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
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	storagev1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/storage/v1"
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
		// F027 Phase 5 (contract): the (infoblox.storage.v1.model) annotation binds a
		// message to a backing storage model so multiple API surfaces can share one
		// table. The option is the locked contract, but the cross-message surface
		// codegen is not yet generated (Phase 5b) — so reject model != the message's
		// own name rather than silently emit a duplicate schema/table. An absent
		// option, or model == the message name, is the normal single-surface case.
		if opts := m.Desc.Options(); opts != nil && proto.HasExtension(opts, storagev1.E_Model) {
			model, _ := proto.GetExtension(opts, storagev1.E_Model).(string)
			if model != "" && model != name && model != string(m.Desc.Name()) {
				gen.Error(fmt.Errorf("protoc-gen-ent: %s: (infoblox.storage.v1.model)=%q — multi-surface model binding is not yet generated (F027 Phase 5b: specs/027-repo-adapter-codegen); remove the annotation or set it to the message's own name", name, model))
				continue
			}
		}
		msg := entMessageInfo{MessageName: name}
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
				index        bool
				hasOne       *fieldv1.HasOne
				hasMany      *fieldv1.HasMany
				belongsTo    *fieldv1.BelongsTo
				manyToMany   *fieldv1.ManyToMany
			)
			if opts := field.Desc.Options(); opts != nil {
				if proto.HasExtension(opts, fieldv1.E_Opts) {
					if fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions); ok {
						isSecret = fopts.GetSecret()
						notNull = fopts.GetNotNull()
						unique = fopts.GetUnique()
						index = fopts.GetIndex()
						hasOne = fopts.GetHasOne()
						hasMany = fopts.GetHasMany()
						belongsTo = fopts.GetBelongsTo()
						manyToMany = fopts.GetManyToMany()
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
				Index:       index,
				RelatedType: relatedType,
				HasOne:      hasOne,
				HasMany:     hasMany,
				BelongsTo:   belongsTo,
				ManyToMany:  manyToMany,
			})
		}
		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		return
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

	// One schema file per resource message: ent/schema/<snake_resource>.go.
	// Pass the full message set so a belongs_to can be paired with its parent's
	// has_many as a proper ent inverse edge (edge.From(...).Ref(...)).
	for _, msg := range messages {
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
		content := renderEntRepository(msg, pkgName, string(f.GoImportPath))
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
		content := renderEntRepoAdapter(msg, pkgName, string(f.GoImportPath))
		if content == "" {
			continue
		}
		outPath := pkgName + "/" + toSnake(msg.MessageName) + "_repo.ent.go"
		rg := gen.NewGeneratedFile(outPath, f.GoImportPath)
		rg.P(content)
	}
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
