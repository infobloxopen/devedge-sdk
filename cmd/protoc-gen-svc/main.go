// Command protoc-gen-svc is a protoc/buf plugin that emits, for every proto
// service, an application-layer handler interface (.svc.go) with:
//   - A clean <Service>Server interface (no gRPC plumbing exposed)
//   - An Unimplemented<Service>Server embed-stub
//   - A Register<Service>(*grpc.Server, <Service>Server) wiring function
//
// W5-6 will add authz, request-id, and tracing middleware into the generated
// adapter. W3-4 emits the compilable skeleton.
package main

import (
	"google.golang.org/protobuf/compiler/protogen"
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
	var services []serviceInfo
	for _, s := range f.Services {
		svc := serviceInfo{ServiceName: s.GoName}
		for _, m := range s.Methods {
			svc.Methods = append(svc.Methods, methodInfo{
				Name:          m.GoName,
				InputGoIdent:  string(m.Input.GoIdent.GoName),
				OutputGoIdent: string(m.Output.GoIdent.GoName),
			})
		}
		services = append(services, svc)
	}

	content := renderSvcFile(string(f.GoPackageName), string(f.GoImportPath), services)
	if content == "" {
		return
	}

	g := gen.NewGeneratedFile(f.GeneratedFilenamePrefix+".svc.go", f.GoImportPath)
	g.P(content)
}
