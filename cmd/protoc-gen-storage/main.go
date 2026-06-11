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
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
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
	var messages []messageInfo
	for _, m := range f.Messages {
		name := string(m.GoIdent.GoName)
		// Skip RPC request/response wrapper types — only resource messages get
		// a GORM model + Repository. Resource messages don't follow the
		// <Method>Request / <Method>Response naming convention.
		if strings.HasSuffix(name, "Request") || strings.HasSuffix(name, "Response") {
			continue
		}
		msg := messageInfo{
			MessageName:  name,
			PbPkgName:    string(f.GoPackageName),
			PbImportPath: string(f.GoImportPath),
		}
		for _, field := range m.Fields {
			msg.Fields = append(msg.Fields, fieldInfo{
				Name:       string(field.Desc.Name()),
				GoFieldName: string(field.GoName), // Go field name (e.g. "PageSize" for "page_size")
				SnakeName:  toSnake(string(field.Desc.Name())),
				IsRepeated: field.Desc.IsList(),
				IsMessage:  field.Desc.Kind() == protoreflect.MessageKind,
				IsID:       string(field.Desc.Name()) == "id",
				GoType:     protoKindToGoType(field.Desc.Kind()),
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
