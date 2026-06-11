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

	authzv1 "github.com/infobloxopen/apis/proto/infoblox/authz/v1"
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
		msg := entMessageInfo{MessageName: name}
		for _, field := range m.Fields {
			isSecret := false
			if opts := field.Desc.Options(); opts != nil {
				if proto.HasExtension(opts, authzv1.E_Field) {
					if rule, ok := proto.GetExtension(opts, authzv1.E_Field).(*authzv1.FieldRule); ok {
						isSecret = rule.GetSecret()
					}
				}
			}
			msg.Fields = append(msg.Fields, entFieldInfo{
				Name:       string(field.Desc.Name()),
				SnakeName:  toSnake(string(field.Desc.Name())),
				EntType:    protoKindToEntType(field.Desc.Kind()),
				IsID:       string(field.Desc.Name()) == "id",
				IsRepeated: field.Desc.IsList(),
				IsMessage:  field.Desc.Kind() == protoreflect.MessageKind,
				IsSecret:   isSecret,
			})
		}
		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		return
	}

	// One schema file per resource message: ent/schema/<snake_resource>.go.
	for _, msg := range messages {
		content := renderEntSchema(msg)
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
