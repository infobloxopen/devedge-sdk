package celsql

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// zoneDescriptor builds a self-contained proto message resembling a DDI "Zone":
//
//	message Zone {
//	  string display_name  = 1;
//	  string description    = 2;
//	  ZoneType type         = 3;   // enum -> int in CEL
//	  string primary_type   = 4;
//	  map<string,string> tags = 5;
//	  Provider provider     = 6;   // { string name = 1; }
//	  int64 rank            = 7;
//	  repeated string aliases = 8;
//	  enum ZoneType { ZONE_TYPE_UNSPECIFIED = 0; PRIMARY = 1; SECONDARY = 2; FORWARD = 3; }
//	}
func zoneDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()

	strField := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(num),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}
	}

	zone := &descriptorpb.DescriptorProto{
		Name: proto.String("Zone"),
		Field: []*descriptorpb.FieldDescriptorProto{
			strField("display_name", 1),
			strField("description", 2),
			{
				Name:     proto.String("type"),
				Number:   proto.Int32(3),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
				TypeName: proto.String(".ftstest.Zone.ZoneType"),
			},
			strField("primary_type", 4),
			{
				Name:     proto.String("tags"),
				Number:   proto.Int32(5),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".ftstest.Zone.TagsEntry"),
			},
			{
				Name:     proto.String("provider"),
				Number:   proto.Int32(6),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".ftstest.Zone.Provider"),
			},
			{
				Name:   proto.String("rank"),
				Number: proto.Int32(7),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			},
			{
				Name:   proto.String("aliases"),
				Number: proto.Int32(8),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
		NestedType: []*descriptorpb.DescriptorProto{
			{
				Name:    proto.String("TagsEntry"),
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					strField("key", 1),
					strField("value", 2),
				},
			},
			{
				Name:  proto.String("Provider"),
				Field: []*descriptorpb.FieldDescriptorProto{strField("name", 1)},
			},
		},
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: proto.String("ZoneType"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("ZONE_TYPE_UNSPECIFIED"), Number: proto.Int32(0)},
					{Name: proto.String("PRIMARY"), Number: proto.Int32(1)},
					{Name: proto.String("SECONDARY"), Number: proto.Int32(2)},
					{Name: proto.String("FORWARD"), Number: proto.Int32(3)},
				},
			},
		},
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("ftstest/zone.proto"),
		Syntax:      proto.String("proto3"),
		Package:     proto.String("ftstest"),
		MessageType: []*descriptorpb.DescriptorProto{zone},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

func TestCompileCEL_SupportedSubset(t *testing.T) {
	md := zoneDescriptor(t)

	tests := []struct {
		name    string
		expr    string
		wantPG  string
		wantLT  string // SQLite
	}{
		{
			name:   "field reference",
			expr:   `msg.display_name`,
			wantPG: `"display_name"`,
			wantLT: `"display_name"`,
		},
		{
			// DDI map_zone_type shape A: enum -> display via a map literal -> CASE.
			name:   "map_zone_type map-literal lookup -> CASE",
			expr:   `{1: "Primary", 2: "Secondary"}[msg.type]`,
			wantPG: `CASE "type" WHEN 1 THEN 'Primary' WHEN 2 THEN 'Secondary' ELSE NULL END`,
			wantLT: `CASE "type" WHEN 1 THEN 'Primary' WHEN 2 THEN 'Secondary' ELSE NULL END`,
		},
		{
			// DDI map_zone_type shape B: a type/primary_type ternary -> searched CASE.
			name:   "type/primary_type ternary -> CASE",
			expr:   `msg.type == 2 ? "Secondary" : msg.primary_type`,
			wantPG: `CASE WHEN ("type" = 2) THEN 'Secondary' ELSE "primary_type" END`,
			wantLT: `CASE WHEN ("type" = 2) THEN 'Secondary' ELSE "primary_type" END`,
		},
		{
			// String normalization: underscores to spaces, lowercased.
			name:   "string normalization replace + lowerAscii",
			expr:   `msg.display_name.replace("_", " ").lowerAscii()`,
			wantPG: `lower(replace("display_name", '_', ' '))`,
			wantLT: `lower(replace("display_name", '_', ' '))`,
		},
		{
			name:   "string concatenation",
			expr:   `msg.display_name + " " + msg.description`,
			wantPG: `("display_name" || ' ' || "description")`,
			wantLT: `("display_name" || ' ' || "description")`,
		},
		{
			// tags map access: Postgres ->> vs SQLite json_extract.
			name:   "tags map access",
			expr:   `msg.tags["env"]`,
			wantPG: `("tags" ->> 'env')`,
			wantLT: `json_extract("tags", '$."env"')`,
		},
		{
			// nested message field access.
			name:   "message field access",
			expr:   `msg.provider.name`,
			wantPG: `("provider" ->> 'name')`,
			wantLT: `json_extract("provider", '$."name"')`,
		},
		{
			// A calculated source combining a CASE mapping with normalization.
			name:   "map lookup fed through lowerAscii",
			expr:   `{1: "Primary", 2: "Secondary"}[msg.type].lowerAscii()`,
			wantPG: `lower(CASE "type" WHEN 1 THEN 'Primary' WHEN 2 THEN 'Secondary' ELSE NULL END)`,
			wantLT: `lower(CASE "type" WHEN 1 THEN 'Primary' WHEN 2 THEN 'Secondary' ELSE NULL END)`,
		},
		{
			// injection-safe: a single quote in a literal is doubled, not broken out of.
			name:   "string literal quote escaping",
			expr:   `msg.display_name.replace("'", "")`,
			wantPG: `replace("display_name", '''', '')`,
			wantLT: `replace("display_name", '''', '')`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pg, lt, err := CompileCEL(tc.expr, md)
			if err != nil {
				t.Fatalf("CompileCEL(%q) unexpected error: %v", tc.expr, err)
			}
			if pg != tc.wantPG {
				t.Errorf("postgres mismatch\n expr: %s\n  got: %s\n want: %s", tc.expr, pg, tc.wantPG)
			}
			if lt != tc.wantLT {
				t.Errorf("sqlite mismatch\n expr: %s\n  got: %s\n want: %s", tc.expr, lt, tc.wantLT)
			}
		})
	}
}

func TestCompileCEL_OutOfSubsetAborts(t *testing.T) {
	md := zoneDescriptor(t)

	tests := []struct {
		name        string
		expr        string
		wantErrPart string
	}{
		{
			name:        "comprehension (.exists) aborts",
			expr:        `msg.aliases.exists(a, a == "x") ? "y" : "z"`,
			wantErrPart: "comprehension",
		},
		{
			name:        "unsupported function startsWith aborts",
			expr:        `msg.display_name.startsWith("a") ? "y" : "z"`,
			wantErrPart: "startsWith",
		},
		{
			name:        "arithmetic + aborts",
			expr:        `msg.rank + 1 > 0 ? "y" : "z"`,
			wantErrPart: "unsupported", // '>' or arithmetic — either way must not emit SQL
		},
		{
			name:        "non-string output aborts",
			expr:        `msg.rank`,
			wantErrPart: "must evaluate to a string",
		},
		{
			name:        "bare message aborts",
			expr:        `msg`,
			wantErrPart: "must evaluate to a string",
		},
		{
			name:        "list index aborts",
			expr:        `["a", "b"][0]`,
			wantErrPart: "list index",
		},
		{
			name:        "unknown field fails type-check",
			expr:        `msg.nope`,
			wantErrPart: "type-check failed",
		},
		{
			name:        "empty expression",
			expr:        ``,
			wantErrPart: "empty expression",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pg, lt, err := CompileCEL(tc.expr, md)
			if err == nil {
				t.Fatalf("CompileCEL(%q) = (%q, %q), want error", tc.expr, pg, lt)
			}
			if pg != "" || lt != "" {
				t.Errorf("CompileCEL(%q) returned SQL alongside error: pg=%q lt=%q", tc.expr, pg, lt)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("CompileCEL(%q) error = %q, want it to contain %q", tc.expr, err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestCompileCEL_NilDescriptor(t *testing.T) {
	if _, _, err := CompileCEL(`msg.display_name`, nil); err == nil {
		t.Fatal("CompileCEL with nil descriptor: want error, got nil")
	}
}
