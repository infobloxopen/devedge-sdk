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
	"strings"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
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
		msg := entMessageInfo{MessageName: name}
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
			// AIP-154 ETag is framework-managed and not auto-stamped on the ent
			// backend (the GORM storage layer computes it). Skip the `etag` field
			// so it does not become a stray, never-populated ent column.
			if fieldName == "etag" && field.Desc.Kind() == protoreflect.StringKind {
				continue
			}
			msg.Fields = append(msg.Fields, entFieldInfo{
				Name:        string(field.Desc.Name()),
				SnakeName:   toSnake(string(field.Desc.Name())),
				EntType:     protoKindToEntType(field.Desc.Kind()),
				IsID:        string(field.Desc.Name()) == "id",
				IsRepeated:  field.Desc.IsList(),
				IsMessage:   field.Desc.Kind() == protoreflect.MessageKind && !isStringMap,
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
