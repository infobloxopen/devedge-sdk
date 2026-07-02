package aip

import (
	"testing"

	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// buildServiceFile compiles a small resource-oriented service file used to
// exercise DetectServiceResource / ClassifyMethod / ResolveResourceIdentity.
func buildServiceFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()

	msgOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(msgOpts, apiannotations.E_Resource, &apiannotations.ResourceDescriptor{
		Type:    "aiptest.example.com/Widget",
		Pattern: []string{"widgets/{widget}"},
	})

	msg := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
	}
	msgFieldTo := func(name string, num int32, typ string, repeated bool) *descriptorpb.FieldDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		if repeated {
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
		return &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(num),
			Label:    label.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(typ),
			JsonName: proto.String(name),
		}
	}
	i32 := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(), JsonName: proto.String(name)}
	}
	str := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return stringField(name, num, name, nil)
	}

	widget := msg("Widget", stringField("name", 1, "name", behavior(OutputOnly)), str("id", 2))
	widget.Options = msgOpts

	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("aiptest/svc.proto"),
		Syntax:     proto.String("proto3"),
		Package:    proto.String("aiptest"),
		Dependency: []string{"google/api/field_behavior.proto", "google/api/resource.proto", "infoblox/field/v1/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			widget,
			msg("CreateWidgetRequest", msgFieldTo("widget", 1, ".aiptest.Widget", false)),
			msg("GetWidgetRequest", str("id", 1)),
			msg("UpdateWidgetRequest", msgFieldTo("widget", 1, ".aiptest.Widget", false), &descriptorpb.FieldDescriptorProto{Name: proto.String("update_mask"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: proto.String("updateMask")}),
			msg("DeleteWidgetRequest", str("id", 1)),
			msg("DeleteWidgetResponse"),
			msg("ListWidgetsRequest", i32("page_size", 1), str("page_token", 2)),
			msg("ListWidgetsResponse", msgFieldTo("widgets", 1, ".aiptest.Widget", true), str("next_page_token", 2)),
			msg("BatchGetWidgetsRequest", &descriptorpb.FieldDescriptorProto{Name: proto.String("ids"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: proto.String("ids")}),
			msg("BatchGetWidgetsResponse", msgFieldTo("widgets", 1, ".aiptest.Widget", true)),
			msg("ArchiveWidgetRequest", str("id", 1)),
			msg("ArchiveWidgetResponse", msgFieldTo("widget", 1, ".aiptest.Widget", false)),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("WidgetService"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("CreateWidget"), InputType: proto.String(".aiptest.CreateWidgetRequest"), OutputType: proto.String(".aiptest.Widget")},
				{Name: proto.String("GetWidget"), InputType: proto.String(".aiptest.GetWidgetRequest"), OutputType: proto.String(".aiptest.Widget")},
				{Name: proto.String("UpdateWidget"), InputType: proto.String(".aiptest.UpdateWidgetRequest"), OutputType: proto.String(".aiptest.Widget")},
				{Name: proto.String("DeleteWidget"), InputType: proto.String(".aiptest.DeleteWidgetRequest"), OutputType: proto.String(".aiptest.DeleteWidgetResponse")},
				{Name: proto.String("ListWidgets"), InputType: proto.String(".aiptest.ListWidgetsRequest"), OutputType: proto.String(".aiptest.ListWidgetsResponse")},
				{Name: proto.String("BatchGetWidgets"), InputType: proto.String(".aiptest.BatchGetWidgetsRequest"), OutputType: proto.String(".aiptest.BatchGetWidgetsResponse")},
				{Name: proto.String("ArchiveWidget"), InputType: proto.String(".aiptest.ArchiveWidgetRequest"), OutputType: proto.String(".aiptest.ArchiveWidgetResponse")},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd
}

func TestDetectServiceResourceAndClassify(t *testing.T) {
	fd := buildServiceFile(t)
	sd := fd.Services().Get(0)

	res := DetectServiceResource(sd)
	if res == nil || res.Name() != "Widget" {
		t.Fatalf("DetectServiceResource: got %v, want Widget", res)
	}

	want := map[string]StdMethod{
		"CreateWidget":    MethodCreate,
		"GetWidget":       MethodGet,
		"UpdateWidget":    MethodUpdate,
		"DeleteWidget":    MethodDelete,
		"ListWidgets":     MethodList,
		"BatchGetWidgets": MethodBatchGet,
		"ArchiveWidget":   MethodNone, // custom method
	}
	methods := sd.Methods()
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		got := ClassifyMethod(md, res, false)
		if got != want[string(md.Name())] {
			t.Errorf("ClassifyMethod(%s) = %s, want %s", md.Name(), got, want[string(md.Name())])
		}
	}
}

func TestResolveResourceIdentity(t *testing.T) {
	fd := buildServiceFile(t)
	widget := fd.Messages().ByName("Widget")
	id, ok := ResolveResourceIdentity(widget)
	if !ok {
		t.Fatal("ResolveResourceIdentity(Widget): want ok")
	}
	if id.Type != "aiptest.example.com/Widget" {
		t.Errorf("Type = %q", id.Type)
	}
	if len(id.Patterns) != 1 || id.Patterns[0] != "widgets/{widget}" {
		t.Errorf("Patterns = %v", id.Patterns)
	}
	if id.Key != "id" { // Widget has a string id field → addressed by id
		t.Errorf("Key = %q, want id", id.Key)
	}

	// A non-resource message (no google.api.resource) returns ok=false.
	if _, ok := ResolveResourceIdentity(fd.Messages().ByName("ListWidgetsResponse")); ok {
		t.Error("ResolveResourceIdentity(ListWidgetsResponse): want ok=false")
	}
}
