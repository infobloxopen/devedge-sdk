package main

import (
	"strings"
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// TestGenerate_ContradictoryFieldBehaviorAborts asserts FM-1: a field whose
// resolved field_behavior is contradictory (explicit OUTPUT_ONLY + secret, which
// derives INPUT_ONLY) aborts codegen with an error naming the field — surfaced
// through the shared aip resolver the storage plugin now uses.
func TestGenerate_ContradictoryFieldBehaviorAborts(t *testing.T) {
	// A field that is both secret (→ INPUT_ONLY) and explicitly OUTPUT_ONLY.
	badOpts := &descriptorpb.FieldOptions{}
	proto.SetExtension(badOpts, apiannotations.E_FieldBehavior, []apiannotations.FieldBehavior{apiannotations.FieldBehavior_OUTPUT_ONLY})
	proto.SetExtension(badOpts, fieldv1.E_Opts, &fieldv1.FieldOptions{Secret: true})

	strField := func(name string, num int32, opts *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(num),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(name),
			Options:  opts,
		}
	}

	testFile := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("contradiction.proto"),
		Syntax:     proto.String("proto3"),
		Package:    proto.String("contradiction.v1"),
		Options:    &descriptorpb.FileOptions{GoPackage: proto.String("example.com/contradiction;contradiction")},
		Dependency: []string{"google/api/field_behavior.proto", "infoblox/field/v1/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Thing"),
			Field: []*descriptorpb.FieldDescriptorProto{
				strField("id", 1, nil),
				strField("bad_field", 2, badOpts),
			},
		}},
	}

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"contradiction.proto"},
		ProtoFile:      append(depProtos(t, "google/api/field_behavior.proto", "infoblox/field/v1/field.proto"), testFile),
	}
	gen, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	for _, f := range gen.Files {
		if f.Generate {
			generateFile(gen, f)
		}
	}
	resp := gen.Response()
	if resp.GetError() == "" {
		t.Fatal("expected codegen to abort with a contradiction error, got none")
	}
	if !strings.Contains(resp.GetError(), "bad_field") {
		t.Errorf("error must name the field, got: %q", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "contradictory") {
		t.Errorf("error must describe the contradiction, got: %q", resp.GetError())
	}
}

// depProtos collects, in dependency order, the FileDescriptorProtos for the given
// import paths and their transitive deps from the global registry.
func depProtos(t *testing.T, paths ...string) []*descriptorpb.FileDescriptorProto {
	t.Helper()
	var out []*descriptorpb.FileDescriptorProto
	seen := map[string]bool{}
	var add func(path string)
	add = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
		if err != nil {
			t.Fatalf("find %s: %v", path, err)
		}
		fdp := protodesc.ToFileDescriptorProto(fd)
		for _, dep := range fdp.GetDependency() {
			add(dep)
		}
		out = append(out, fdp)
	}
	for _, p := range paths {
		add(p)
	}
	return out
}
