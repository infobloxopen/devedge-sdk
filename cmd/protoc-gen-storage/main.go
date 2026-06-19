// Command protoc-gen-storage is a protoc/buf plugin that emits, for every
// proto message, a GORM-backed repository (.storage.go) implementing
// persistence.Repository[*pb.<Message>, string]:
//
//   - <Message>Model GORM struct with snake_case columns
//   - <Message>Repository with CRUD methods
//   - New<Message>Repository(*gorm.DB) constructor
//   - Compile-time persistence.Repository satisfaction check
//
// Generated code imports gorm.io/gorm; the consumer's go.mod provides GORM.
// devedge-sdk's go.mod gains no ORM dependency.
package main

import (
	"flag"
	"strings"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
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
		msg := messageInfo{
			MessageName:  name,
			PbPkgName:    string(f.GoPackageName),
			PbImportPath: string(f.GoImportPath),
		}
		// Extract (google.api.resource) pattern from message options.
		if mopts := m.Desc.Options(); mopts != nil {
			if proto.HasExtension(mopts, apiannotations.E_Resource) {
				if rd, ok := proto.GetExtension(mopts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); ok {
					if patterns := rd.GetPattern(); len(patterns) > 0 {
						msg.ResourcePattern = patterns[0]
					}
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
				columnName   string
				columnType   string
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
						columnName = fopts.GetColumnName()
						columnType = fopts.GetColumnType()
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
				Name:          string(field.Desc.Name()),
				GoFieldName:   string(field.GoName),
				SnakeName:     toSnake(string(field.Desc.Name())),
				IsRepeated:    field.Desc.IsList(),
				IsMessage:     field.Desc.Kind() == protoreflect.MessageKind && !isStringMap,
				IsTags:        isStringMap,
				IsID:          string(field.Desc.Name()) == "id",
				GoType:        protoKindToGoType(field.Desc.Kind()),
				RelatedGoType: relatedGoType,
				IsSecret:      isSecret,
				IsOutputOnly:  isOutputOnly,
				NotNull:       notNull,
				Unique:        unique,
				Index:         index,
				ColumnName:    columnName,
				ColumnType:    columnType,
				HasOne:        hasOne,
				HasMany:       hasMany,
				BelongsTo:     belongsTo,
				ManyToMany:    manyToMany,
			})
		}
		messages = append(messages, msg)
	}

	// Storage code lives in the same package as the pb types (same directory).
	// This keeps the generated file co-located with widgets.pb.go so the GORM
	// model can reference proto types without a package qualifier.
	// Pass an empty PbPkgName to renderStorageFile to skip the qualifier.
	for i := range messages {
		messages[i].PbPkgName = "" // same package — no qualifier needed
	}
	content := renderStorageFile(string(f.GoPackageName), messages)
	if content == "" {
		return
	}

	outPath := f.GeneratedFilenamePrefix + ".storage.go"
	g := gen.NewGeneratedFile(outPath, f.GoImportPath)
	g.P(content)
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
