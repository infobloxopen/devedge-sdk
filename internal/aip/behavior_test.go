package aip

import (
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// stringField builds a proto3 optional string field with the given options.
func stringField(name string, num int32, jsonName string, fo *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(num),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		JsonName: proto.String(jsonName),
		Options:  fo,
	}
}

func behavior(bs ...apiannotations.FieldBehavior) *descriptorpb.FieldOptions {
	fo := &descriptorpb.FieldOptions{}
	proto.SetExtension(fo, apiannotations.E_FieldBehavior, bs)
	return fo
}

func withOpts(o *fieldv1.FieldOptions) *descriptorpb.FieldOptions {
	fo := &descriptorpb.FieldOptions{}
	proto.SetExtension(fo, fieldv1.E_Opts, o)
	return fo
}

// buildMessage compiles a one-message file "aiptest.M" with the given fields and
// returns its descriptor. Dependencies (field_behavior, field.v1) resolve from
// the global registry (linked in via the imports above).
func buildMessage(t *testing.T, msgOpts *descriptorpb.MessageOptions, fields ...*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("aiptest/aiptest.proto"),
		Syntax:  proto.String("proto3"),
		Package: proto.String("aiptest"),
		Dependency: []string{
			"google/api/field_behavior.proto",
			"google/api/resource.proto",
			"infoblox/field/v1/field.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:    proto.String("M"),
			Options: msgOpts,
			Field:   fields,
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

func fieldByName(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	return md.Fields().ByName(protoreflect.Name(name))
}

func TestResolveFieldBehavior(t *testing.T) {
	md := buildMessage(t, nil,
		stringField("plain", 1, "plain", nil),
		stringField("required_field", 2, "requiredField", behavior(Required)),
		stringField("immutable_field", 3, "immutableField", behavior(Immutable)),
		stringField("output_only_field", 4, "outputOnlyField", behavior(OutputOnly)),
		stringField("secret_field", 5, "secretField", withOpts(&fieldv1.FieldOptions{Secret: true})),
		stringField("id_user", 6, "idUser", withOpts(&fieldv1.FieldOptions{Id: &fieldv1.IdOptions{Strategy: fieldv1.IdOptions_STRATEGY_USER_SETTABLE}})),
		stringField("id_server", 7, "idServer", withOpts(&fieldv1.FieldOptions{Id: &fieldv1.IdOptions{Strategy: fieldv1.IdOptions_STRATEGY_SERVER_GENERATED}})),
		stringField("not_null_field", 8, "notNullField", withOpts(&fieldv1.FieldOptions{NotNull: true})),
		stringField("allowed", 9, "allowed", withOpts(&fieldv1.FieldOptions{AllowedValues: []string{"A", "B"}})),
	)

	cases := []struct {
		field string
		want  []FieldBehavior
	}{
		{"plain", nil},
		{"required_field", []FieldBehavior{Required}},
		{"immutable_field", []FieldBehavior{Immutable}},
		{"output_only_field", []FieldBehavior{OutputOnly}},
		{"secret_field", []FieldBehavior{InputOnly}}, // derived from secret
		{"id_user", []FieldBehavior{Immutable}},      // derived from USER_SETTABLE
		{"id_server", []FieldBehavior{OutputOnly}},   // derived from SERVER_GENERATED
		{"not_null_field", nil},                      // FM-4: not_null NEVER → REQUIRED
		{"allowed", nil},                             // allowed_values is enum, not a behavior
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			got, err := ResolveFieldBehavior(fieldByName(md, c.field))
			if err != nil {
				t.Fatalf("ResolveFieldBehavior(%s): unexpected error: %v", c.field, err)
			}
			if !equalBehaviors(got, c.want) {
				t.Errorf("ResolveFieldBehavior(%s) = %v, want %v", c.field, got, c.want)
			}
		})
	}
}

func TestResolveFieldBehavior_Contradictions(t *testing.T) {
	md := buildMessage(t, nil,
		stringField("out_and_required", 1, "outAndRequired", behavior(OutputOnly, Required)),
		stringField("out_and_input", 2, "outAndInput", behavior(OutputOnly, InputOnly)),
		// explicit OUTPUT_ONLY vs derived INPUT_ONLY (secret): must fail loud.
		stringField("secret_but_output", 3, "secretButOutput", func() *descriptorpb.FieldOptions {
			fo := behavior(OutputOnly)
			proto.SetExtension(fo, fieldv1.E_Opts, &fieldv1.FieldOptions{Secret: true})
			return fo
		}()),
	)
	for _, name := range []string{"out_and_required", "out_and_input", "secret_but_output"} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveFieldBehavior(fieldByName(md, name))
			if err == nil {
				t.Fatalf("ResolveFieldBehavior(%s): want fail-loud error, got nil", name)
			}
			// FR-A2: the error must name the field.
			if !contains(err.Error(), name) {
				t.Errorf("error %q does not name field %q", err.Error(), name)
			}
		})
	}
}

func TestIsOutputOnly(t *testing.T) {
	md := buildMessage(t, nil,
		stringField("oo", 1, "oo", behavior(OutputOnly)),
		stringField("srv_id", 2, "srvId", withOpts(&fieldv1.FieldOptions{Id: &fieldv1.IdOptions{Strategy: fieldv1.IdOptions_STRATEGY_SERVER_GENERATED}})),
		stringField("plain", 3, "plain", nil),
	)
	for _, c := range []struct {
		field string
		want  bool
	}{{"oo", true}, {"srv_id", true}, {"plain", false}} {
		got, err := IsOutputOnly(fieldByName(md, c.field))
		if err != nil {
			t.Fatalf("IsOutputOnly(%s): %v", c.field, err)
		}
		if got != c.want {
			t.Errorf("IsOutputOnly(%s) = %v, want %v", c.field, got, c.want)
		}
	}
}

func TestAllowedValues(t *testing.T) {
	md := buildMessage(t, nil,
		stringField("cat", 1, "cat", withOpts(&fieldv1.FieldOptions{AllowedValues: []string{"standard", "premium"}})),
		stringField("plain", 2, "plain", nil),
	)
	got := AllowedValues(fieldByName(md, "cat"))
	if len(got) != 2 || got[0] != "standard" || got[1] != "premium" {
		t.Errorf("AllowedValues(cat) = %v, want [standard premium]", got)
	}
	if AllowedValues(fieldByName(md, "plain")) != nil {
		t.Errorf("AllowedValues(plain): want nil")
	}
}

func equalBehaviors(a, b []FieldBehavior) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
