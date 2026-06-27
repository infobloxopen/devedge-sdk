// Command protoc-gen-svc is a protoc/buf plugin that emits, for every proto
// service, the server-package wiring (.svc.go):
//   - Register<Service>(*server.Server, <Service>Server) — record methods,
//     contribute the service's authz rules, register gRPC + the REST gateway;
//   - a generated default CRUD handler (<Service>CRUDHandler) backed by the
//     generated persistence.Repository, with one method per detected AIP standard
//     RPC (Create/Get/List/Update/Delete, plus Undelete for soft-delete
//     resources); custom/unmatched RPCs stay Unimplemented (escape hatch);
//   - New<Service>Handler(repo) and Register<Service>WithRepository(s, repo) —
//     the one-call CRUD path (construct + register + contribute rules).
//
// The <Service>Server interface, Unimplemented<Service>Server, and the gRPC/
// gateway registrars come from protoc-gen-go-grpc + protoc-gen-grpc-gateway in
// the same package. The <Service>AuthzRules table comes from
// protoc-gen-devedge-authz. The persistence.Repository comes from
// protoc-gen-{storage,ent}.
package main

import (
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
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

// resourceMessages collects, across the whole file, which messages are
// API resources (carry a (google.api.resource) annotation) and whether they
// opt into soft-delete (OUTPUT_ONLY delete_time Timestamp). Keyed by Go type name.
type resourceFacts struct {
	softDelete bool
	hasName    bool   // has an AIP-122 name field / resource pattern
	memberRoot string // (infoblox.ddd.v1.member).root — owning aggregate root, "" when not a member
}

func generateFile(gen *protogen.Plugin, f *protogen.File) {
	// Pass 1: index every message's resource facts so a service's methods can be
	// classified against the resource type they operate on.
	facts := map[string]resourceFacts{}
	for _, m := range f.Messages {
		facts[string(m.GoIdent.GoName)] = messageResourceFacts(m)
	}

	var services []serviceInfo
	for _, s := range f.Services {
		svc := serviceInfo{ServiceName: s.GoName}
		// Resolve the resource type the service operates on from its standard
		// method shapes (the resource message field on Create/Update requests, the
		// return type of Create/Get, the repeated field on the List response).
		svc.Resource = detectServiceResource(s, facts)
		if r, ok := facts[svc.Resource]; ok {
			svc.ResourceSoftDelete = r.softDelete
			svc.MemberRoot = r.memberRoot
		}
		for _, m := range s.Methods {
			mi := methodInfo{
				Name:          m.GoName,
				InputGoIdent:  string(m.Input.GoIdent.GoName),
				OutputGoIdent: string(m.Output.GoIdent.GoName),
			}
			mi.Std = classifyMethod(m, svc.Resource, svc.ResourceSoftDelete)
			if mi.Std == stdGet || mi.Std == stdDelete || mi.Std == stdUndelete {
				mi.KeyByName = methodKeyByName(m)
			}
			if mi.Std == stdList {
				mi.ListItemsField = listItemsField(m, svc.Resource)
				mi.ListHasFilter = hasStringField(m.Input, "filter")
				mi.ListHasOrderBy = hasStringField(m.Input, "order_by")
				mi.ListHasShowDeleted = hasBoolField(m.Input, "show_deleted")
			}
			if mi.Std == stdCreate || mi.Std == stdUpdate {
				mi.ResourceField = requestResourceField(m, svc.Resource)
			}
			svc.Methods = append(svc.Methods, mi)
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

// messageResourceFacts inspects a message for resource markers.
func messageResourceFacts(m *protogen.Message) resourceFacts {
	var r resourceFacts
	if mopts, ok := m.Desc.Options().(*descriptorpb.MessageOptions); ok && mopts != nil {
		if proto.HasExtension(mopts, apiannotations.E_Resource) {
			if rd, _ := proto.GetExtension(mopts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); rd != nil {
				if len(rd.GetPattern()) > 0 {
					r.hasName = true
				}
			}
		}
		// F031 DDD: (infoblox.ddd.v1.member) marks this resource as owned by a root
		// aggregate. A member's write-capable standard methods are emitted as
		// Unimplemented (route through the root) and recorded for the boundary gate.
		if proto.HasExtension(mopts, dddv1.E_Member) {
			if mb, _ := proto.GetExtension(mopts, dddv1.E_Member).(*dddv1.Member); mb != nil {
				r.memberRoot = mb.GetRoot()
			}
		}
	}
	for _, field := range m.Fields {
		fname := string(field.Desc.Name())
		if fname == "name" && field.Desc.Kind() == protoreflect.StringKind {
			r.hasName = true
		}
		isTimestamp := field.Desc.Kind() == protoreflect.MessageKind &&
			field.Desc.Message() != nil &&
			field.Desc.Message().FullName() == "google.protobuf.Timestamp"
		if fname == "delete_time" && isTimestamp && fieldIsOutputOnly(field) {
			r.softDelete = true
		}
	}
	return r
}

func fieldIsOutputOnly(field *protogen.Field) bool {
	opts := field.Desc.Options()
	if opts == nil {
		return false
	}
	if !proto.HasExtension(opts, apiannotations.E_FieldBehavior) {
		return false
	}
	behaviors, _ := proto.GetExtension(opts, apiannotations.E_FieldBehavior).([]apiannotations.FieldBehavior)
	for _, b := range behaviors {
		if b == apiannotations.FieldBehavior_OUTPUT_ONLY {
			return true
		}
	}
	return false
}

// detectServiceResource resolves the resource Go type a service manages. It
// prefers a Create/Get return type that is a known resource message, else the
// resource-typed field on a Create/Update request, else the repeated element of
// a List response. Returns "" when no resource is detectable (custom-only svc).
func detectServiceResource(s *protogen.Service, facts map[string]resourceFacts) string {
	for _, m := range s.Methods {
		out := string(m.Output.GoIdent.GoName)
		if _, ok := facts[out]; ok && isResourceFacts(facts[out]) {
			// A return type that is itself a resource message (Create/Get/Update).
			return out
		}
	}
	for _, m := range s.Methods {
		// A request carrying a single resource-typed message field (Create/Update).
		for _, field := range m.Input.Fields {
			if field.Message == nil || field.Desc.IsList() || field.Desc.IsMap() {
				continue
			}
			rt := string(field.Message.GoIdent.GoName)
			if _, ok := facts[rt]; ok {
				return rt
			}
		}
	}
	for _, m := range s.Methods {
		// A List response carrying a repeated resource field.
		for _, field := range m.Output.Fields {
			if field.Message == nil || !field.Desc.IsList() {
				continue
			}
			rt := string(field.Message.GoIdent.GoName)
			if _, ok := facts[rt]; ok {
				return rt
			}
		}
	}
	return ""
}

func isResourceFacts(r resourceFacts) bool { return r.hasName || r.softDelete || r.memberRoot != "" }

// stdMethod is the classified AIP standard-method kind for an RPC.
type stdMethod int

const (
	stdNone stdMethod = iota
	stdCreate
	stdGet
	stdList
	stdUpdate
	stdDelete
	stdUndelete
)

// isWrite reports whether a classified standard method is write-capable
// (Create/Update/Delete/Undelete). For a DDD aggregate member these are
// suppressed (emitted Unimplemented, route through the root) and recorded for the
// boundary gate; Get/List stay addressable (reads ≠ write authority).
func (s stdMethod) isWrite() bool {
	switch s {
	case stdCreate, stdUpdate, stdDelete, stdUndelete:
		return true
	default:
		return false
	}
}

// classifyMethod detects which AIP standard method an RPC implements, by the
// shape of its request/response messages (D-2), tolerating extra optional
// fields. resource is the service's resource Go type ("" disables detection).
func classifyMethod(m *protogen.Method, resource string, softDelete bool) stdMethod {
	if resource == "" {
		return stdNone
	}
	in := m.Input
	out := m.Output

	hasResourceField := func(msg *protogen.Message) bool {
		for _, field := range msg.Fields {
			if field.Message != nil && !field.Desc.IsList() && !field.Desc.IsMap() &&
				string(field.Message.GoIdent.GoName) == resource {
				return true
			}
		}
		return false
	}
	hasField := func(msg *protogen.Message, name string, kind protoreflect.Kind) bool {
		for _, field := range msg.Fields {
			if string(field.Desc.Name()) == name && field.Desc.Kind() == kind {
				return true
			}
		}
		return false
	}
	hasUpdateMask := func(msg *protogen.Message) bool {
		for _, field := range msg.Fields {
			if string(field.Desc.Name()) != "update_mask" {
				continue
			}
			// Accept either a repeated-string update_mask (the SDK convention) or a
			// google.protobuf.FieldMask-typed update_mask (the canonical AIP-134 form).
			if field.Desc.IsList() && field.Desc.Kind() == protoreflect.StringKind {
				return true
			}
			if field.Message != nil &&
				field.Message.Desc.FullName() == "google.protobuf.FieldMask" {
				return true
			}
		}
		return false
	}
	hasRepeatedResource := func(msg *protogen.Message) bool {
		for _, field := range msg.Fields {
			if field.Message != nil && field.Desc.IsList() &&
				string(field.Message.GoIdent.GoName) == resource {
				return true
			}
		}
		return false
	}

	idIn := hasField(in, "id", protoreflect.StringKind)
	nameIn := hasField(in, "name", protoreflect.StringKind)
	returnsResource := string(out.GoIdent.GoName) == resource

	// Update: request carries the resource + an update_mask, returns the resource.
	if hasResourceField(in) && hasUpdateMask(in) && returnsResource {
		return stdUpdate
	}
	// Create: request carries the resource (no update_mask), returns the resource.
	if hasResourceField(in) && !hasUpdateMask(in) && returnsResource {
		return stdCreate
	}
	// List: request has page_size + page_token, response has repeated resource +
	// next_page_token.
	if hasField(in, "page_size", protoreflect.Int32Kind) &&
		hasField(in, "page_token", protoreflect.StringKind) &&
		hasRepeatedResource(out) &&
		hasField(out, "next_page_token", protoreflect.StringKind) {
		return stdList
	}
	// Undelete (soft-delete only): keyed by id/name, returns the resource, named
	// Undelete<R>. Detection by name guards against a near-Get shape.
	if softDelete && (idIn || nameIn) && returnsResource && hasMethodPrefix(m, "Undelete") {
		return stdUndelete
	}
	// Get: keyed by id OR name, returns the resource (and not Undelete).
	if (idIn || nameIn) && returnsResource && hasMethodPrefix(m, "Get") {
		return stdGet
	}
	// Delete: keyed by id OR name, returns a delete response or Empty (not the
	// resource), named Delete<R>.
	if (idIn || nameIn) && !returnsResource && hasMethodPrefix(m, "Delete") {
		return stdDelete
	}
	return stdNone
}

// listItemsField returns the Go field name of the repeated-resource field on a
// List response message (e.g. proto `repeated APIKey api_keys` -> Go `ApiKeys`).
func listItemsField(m *protogen.Method, resource string) string {
	for _, field := range m.Output.Fields {
		if field.Message != nil && field.Desc.IsList() &&
			string(field.Message.GoIdent.GoName) == resource {
			return string(field.GoName)
		}
	}
	return ""
}

func hasStringField(msg *protogen.Message, name string) bool {
	for _, field := range msg.Fields {
		if string(field.Desc.Name()) == name && field.Desc.Kind() == protoreflect.StringKind {
			return true
		}
	}
	return false
}

func hasBoolField(msg *protogen.Message, name string) bool {
	for _, field := range msg.Fields {
		if string(field.Desc.Name()) == name && field.Desc.Kind() == protoreflect.BoolKind {
			return true
		}
	}
	return false
}

// requestResourceField returns the Go field name of the singular resource-typed
// field on a Create/Update request (e.g. proto `APIKey api_key` -> Go `ApiKey`).
func requestResourceField(m *protogen.Method, resource string) string {
	for _, field := range m.Input.Fields {
		if field.Message != nil && !field.Desc.IsList() && !field.Desc.IsMap() &&
			string(field.Message.GoIdent.GoName) == resource {
			return string(field.GoName)
		}
	}
	return ""
}

func hasMethodPrefix(m *protogen.Method, prefix string) bool {
	n := m.GoName
	return len(n) >= len(prefix) && n[:len(prefix)] == prefix
}

// methodKeyByName reports whether a Get/Delete/Undelete RPC is keyed by an
// AIP-122 name field (so the handler must parse it via Parse<R>Name) rather than
// a plain id. Prefers id when both are present (id is the direct repo key).
func methodKeyByName(m *protogen.Method) bool {
	hasID, hasName := false, false
	for _, field := range m.Input.Fields {
		switch string(field.Desc.Name()) {
		case "id":
			if field.Desc.Kind() == protoreflect.StringKind {
				hasID = true
			}
		case "name":
			if field.Desc.Kind() == protoreflect.StringKind {
				hasName = true
			}
		}
	}
	return !hasID && hasName
}
