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
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	"github.com/infobloxopen/devedge-sdk/internal/aip"
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
	// resourceType is the (google.api.resource).type of the message (the AIP-122
	// resource type, e.g. "region.example.com/Region"). It is the identity a
	// cross-service reference names (F041): a service that serves BatchGet over a
	// resource with this type is the batch-fetchable TARGET the fail-loud gate
	// matches a reference's TargetType against.
	resourceType string
}

func generateFile(gen *protogen.Plugin, f *protogen.File) {
	// Pass 1: index every message's resource facts so a service's methods can be
	// classified against the resource type they operate on.
	facts := map[string]resourceFacts{}
	msgByName := map[string]*protogen.Message{}
	for _, m := range f.Messages {
		af := aip.MessageFacts(m.Desc)
		facts[string(m.GoIdent.GoName)] = resourceFacts{
			softDelete:   af.SoftDelete,
			hasName:      af.HasName,
			memberRoot:   af.MemberRoot,
			resourceType: af.Type,
		}
		msgByName[string(m.GoIdent.GoName)] = m
	}

	// protoPackage is the proto package (e.g. "orders.v1"); its first segment is
	// the module ID the generated servicekit.Module reports (e.g. "orders").
	protoPackage := string(f.Desc.Package())

	var services []serviceInfo
	for _, s := range f.Services {
		svc := serviceInfo{ServiceName: s.GoName, ProtoPackage: protoPackage}
		// Resolve the resource type the service operates on from its standard
		// method shapes (the resource message field on Create/Update requests, the
		// return type of Create/Get, the repeated field on the List response),
		// via the shared aip resolver so compiled behavior matches the enriched
		// OpenAPI (D-new-1).
		svc.Resource = detectServiceResource(s, msgByName)
		var resDesc protoreflect.MessageDescriptor
		if rm, ok := msgByName[svc.Resource]; ok {
			resDesc = rm.Desc
		}
		if r, ok := facts[svc.Resource]; ok {
			svc.ResourceSoftDelete = r.softDelete
			svc.MemberRoot = r.memberRoot
			svc.ResourceType = r.resourceType
		}
		// BC-08: string fields on the resource carrying an allowed_values
		// constraint drive a generated validate<Resource> the handlers call.
		svc.EnumFields = resourceEnumFields(msgByName[svc.Resource])
		// F041 (WS-021 P1): cross-service references declared on the resource's
		// scalar FK fields via the standard google.api.resource_reference (AIP-124)
		// drive the emitted <Svc>References metadata table + the fail-loud gate.
		refs, err := resourceReferences(msgByName[svc.Resource], facts)
		if err != nil {
			gen.Error(err)
			return
		}
		svc.References = refs
		for _, m := range s.Methods {
			mi := methodInfo{
				Name:          m.GoName,
				InputGoIdent:  string(m.Input.GoIdent.GoName),
				OutputGoIdent: string(m.Output.GoIdent.GoName),
			}
			mi.Std = classifyMethod(m, resDesc, svc.ResourceSoftDelete)
			if mi.Std == stdGet || mi.Std == stdDelete || mi.Std == stdUndelete {
				mi.KeyByName = methodKeyByName(m)
			}
			if mi.Std == stdList {
				mi.ListItemsField = listItemsField(m, svc.Resource)
				mi.ListHasFilter = hasStringField(m.Input, "filter")
				mi.ListHasOrderBy = hasStringField(m.Input, "order_by")
				mi.ListHasShowDeleted = hasBoolField(m.Input, "show_deleted")
				mi.ListHasSearch = hasStringField(m.Input, "q")
			}
			if mi.Std == stdCreate || mi.Std == stdUpdate {
				mi.ResourceField = requestResourceField(m, svc.Resource)
			}
			if mi.Std == stdBatchGet {
				mi.BatchIDsField = batchIDsField(m)
				mi.BatchItemsField = listItemsField(m, svc.Resource)
			}
			// #191: a nested AIP-122 URL (e.g. accounts/{ledger_account_id}/entries/
			// {id}) binds a parent segment the generated Get/List/Delete must ENFORCE,
			// not merely address around. Resolve the parent foreign key; a nested
			// pattern with no resolvable FK is a fail-loud codegen error.
			if mi.Std == stdGet || mi.Std == stdList || mi.Std == stdDelete {
				ps, perr := detectParentScope(m, msgByName[svc.Resource])
				if perr != nil {
					gen.Error(perr)
					return
				}
				mi.Parent = ps
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

// resourceEnumFields returns the resource's string fields that carry an
// allowed_values constraint (BC-08). The option is meaningful only for
// string-backed enums, so non-string and repeated fields are ignored.
func resourceEnumFields(m *protogen.Message) []enumField {
	if m == nil {
		return nil
	}
	var out []enumField
	for _, field := range m.Fields {
		if field.Desc.Kind() != protoreflect.StringKind || field.Desc.IsList() {
			continue
		}
		opts := field.Desc.Options()
		if opts == nil || !proto.HasExtension(opts, fieldv1.E_Opts) {
			continue
		}
		fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions)
		if !ok || fopts == nil || len(fopts.GetAllowedValues()) == 0 {
			continue
		}
		out = append(out, enumField{
			Getter:    "Get" + field.GoName,
			ProtoName: string(field.Desc.Name()),
			Allowed:   fopts.GetAllowedValues(),
		})
	}
	return out
}

// detectServiceResource resolves the resource Go type a service manages, using
// the shared aip.DetectServiceResource classifier (protoreflect) and mapping the
// resulting message descriptor back to its Go type name. Returns "" when no
// resource is detectable (custom-only svc).
func detectServiceResource(s *protogen.Service, msgByName map[string]*protogen.Message) string {
	rd := aip.DetectServiceResource(s.Desc)
	if rd == nil {
		return ""
	}
	for name, m := range msgByName {
		if m.Desc.FullName() == rd.FullName() {
			return name
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
	// stdBatchGet is an AIP-137 BatchGet<R>: a read that returns many resources
	// by id in one call. F041 (WS-021 P1) generates its handler (delegating to the
	// repository's persistence.BatchRepository.BatchGet) so a reference-target
	// resource batch-fetches by construction. It is a READ (never a member-write
	// redirect); BatchCreate/BatchUpdate/BatchDelete stay unclassified.
	stdBatchGet
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

// classifyMethod detects which AIP standard method an RPC implements. It is a
// thin adapter over the shared aip.ClassifyMethod (protoreflect) so the svc
// plugin and the OpenAPI enrichment pass classify identically (D-new-1); it maps
// the shared aip.StdMethod onto this package's local stdMethod, which render.go
// and render_test.go depend on. resource is the service's resource message
// descriptor (nil disables detection).
func classifyMethod(m *protogen.Method, resource protoreflect.MessageDescriptor, softDelete bool) stdMethod {
	switch aip.ClassifyMethod(m.Desc, resource, softDelete) {
	case aip.MethodCreate:
		return stdCreate
	case aip.MethodGet:
		return stdGet
	case aip.MethodList:
		return stdList
	case aip.MethodUpdate:
		return stdUpdate
	case aip.MethodDelete:
		return stdDelete
	case aip.MethodUndelete:
		return stdUndelete
	case aip.MethodBatchGet:
		return stdBatchGet
	default:
		return stdNone
	}
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

// batchIDsField returns the Go field name of the repeated-string key list on a
// BatchGet request (proto `repeated string ids` -> Go `Ids`, or `names` ->
// `Names`). Prefers "ids" (the direct repo key) over "names".
func batchIDsField(m *protogen.Method) string {
	var idsGo, namesGo string
	for _, field := range m.Input.Fields {
		if !field.Desc.IsList() || field.Desc.Kind() != protoreflect.StringKind {
			continue
		}
		switch string(field.Desc.Name()) {
		case "ids":
			idsGo = string(field.GoName)
		case "names":
			namesGo = string(field.GoName)
		}
	}
	if idsGo != "" {
		return idsGo
	}
	return namesGo
}

// resourceReferences extracts the cross-service references declared on a resource
// message's scalar FK fields via the standard google.api.resource_reference
// annotation (AIP-124, F041 D-1). It reads the same annotation Google/AIP tooling
// uses — there is NO new infoblox annotation. The target module/endpoint is NOT
// annotated: TargetType is a globally unique AIP-122 type, catalog-resolved at
// use (WP-A).
//
// Coverage guard (spec failure mode): the annotation is meaningful only on a
// scalar string FK field of a RESOURCE message. A resource_reference on a
// non-resource message, or on a message-typed / non-string field, is a codegen
// error — surfaced loud, never silently ignored. Returns nil for a nil message
// (custom-only service).
func resourceReferences(m *protogen.Message, facts map[string]resourceFacts) ([]referenceInfo, error) {
	if m == nil {
		return nil, nil
	}
	msgIsResource := false
	if f, ok := facts[string(m.GoIdent.GoName)]; ok && isResourceFacts(f) {
		msgIsResource = true
	}
	var out []referenceInfo
	for _, field := range m.Fields {
		opts := field.Desc.Options()
		if opts == nil || !proto.HasExtension(opts, apiannotations.E_ResourceReference) {
			continue
		}
		rr, _ := proto.GetExtension(opts, apiannotations.E_ResourceReference).(*apiannotations.ResourceReference)
		if rr == nil || rr.GetType() == "" {
			continue
		}
		if !msgIsResource {
			return nil, fmt.Errorf("protoc-gen-svc: %s.%s: google.api.resource_reference is only valid on a field of a resource message (a message carrying google.api.resource)", m.GoIdent.GoName, field.Desc.Name())
		}
		if field.Desc.Kind() != protoreflect.StringKind {
			return nil, fmt.Errorf("protoc-gen-svc: %s.%s: google.api.resource_reference must annotate a scalar string foreign-key field, not a %s field (references are metadata over a scalar FK, never a traversable edge)", m.GoIdent.GoName, field.Desc.Name(), field.Desc.Kind())
		}
		card := "one"
		if field.Desc.IsList() {
			card = "many"
		}
		out = append(out, referenceInfo{
			FieldGoName: string(field.GoName),
			FKField:     string(field.Desc.Name()),
			TargetType:  rr.GetType(),
			Cardinality: card,
		})
	}
	return out, nil
}

// detectParentScope resolves the nested-resource parent scope a Get/List/Delete
// must enforce (#191). It reads the method's google.api.http path template: any
// path variable that is a single-segment field OTHER than the resource key
// ("id"/"name") is a parent segment (e.g. {ledger_account_id} in
// accounts/{ledger_account_id}/entries/{id}). The immediate parent (closest to
// the resource) must map to a scalar string foreign-key field on BOTH the request
// and the managed resource; the handler then filters List by it and denies a
// cross-parent Get/Delete.
//
// Fail-loud contract: a method whose URL nests under a parent segment but whose
// resource declares no matching scalar FK field is a codegen ERROR — the parent
// would otherwise be bound by the gateway and silently ignored (the #191 bug).
// Returns (nil, nil) for a non-nested method.
func detectParentScope(m *protogen.Method, resource *protogen.Message) (*parentScope, error) {
	fieldName, nested := parentSegment(httpRulePath(m))
	if !nested {
		return nil, nil
	}
	if resource == nil {
		return nil, fmt.Errorf("protoc-gen-svc: %s: URL nests under parent segment {%s} but the service manages no resolvable resource to scope by (#191)", m.GoName, fieldName)
	}
	resGetter := scalarStringGetter(resource, fieldName)
	if resGetter == "" {
		return nil, fmt.Errorf("protoc-gen-svc: %s.%s: URL nests under parent segment {%s}, but %s has no matching scalar string foreign-key field — the generated handler cannot enforce the parent and refuses to bind-and-ignore it (#191). Add a scalar string %q field to the resource (e.g. via belongs_to) or flatten the URL.",
			resource.GoIdent.GoName, m.GoName, fieldName, resource.GoIdent.GoName, fieldName)
	}
	reqGetter := scalarStringGetter(m.Input, fieldName)
	if reqGetter == "" {
		return nil, fmt.Errorf("protoc-gen-svc: %s.%s: URL nests under parent segment {%s}, but request %s has no scalar string field bound to it (#191)",
			resource.GoIdent.GoName, m.GoName, fieldName, m.Input.GoIdent.GoName)
	}
	return &parentScope{ReqGetter: reqGetter, ResGetter: resGetter, FKField: fieldName}, nil
}

// parentSegment reports the immediate nested-resource parent segment of an
// AIP/gRPC-gateway URL template and whether the URL nests at all. A parent segment
// is a single-field {..} variable OTHER than the resource key ("id"/"name") and
// other than a dotted body path (e.g. {widget.id}). When several parent segments
// are present (deep nesting) the immediate one — closest to the resource key — is
// returned, since that is the resource's direct owner. Returns ("", false) for a
// flat URL.
func parentSegment(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	var candidates []string
	for _, v := range pathVariables(path) {
		// A dotted field path (e.g. {widget.id}) binds a field on the request's
		// resource body — the resource's own key on Update, never a parent scope.
		if strings.Contains(v, ".") {
			continue
		}
		// The resource's own key is not a parent.
		if v == "id" || v == "name" {
			continue
		}
		candidates = append(candidates, v)
	}
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[len(candidates)-1], true
}

// scalarStringGetter returns the Go getter ("Get<GoName>") for a non-repeated
// string field of msg with the given proto field name, or "" when absent.
func scalarStringGetter(msg *protogen.Message, protoName string) string {
	for _, f := range msg.Fields {
		if string(f.Desc.Name()) == protoName && f.Desc.Kind() == protoreflect.StringKind && !f.Desc.IsList() {
			return "Get" + f.GoName
		}
	}
	return ""
}

// httpRulePath returns the URL path template of a method's google.api.http rule
// (the first bound HTTP method or a custom binding), or "" when the method has no
// HTTP annotation.
func httpRulePath(m *protogen.Method) string {
	opts := m.Desc.Options()
	if opts == nil || !proto.HasExtension(opts, apiannotations.E_Http) {
		return ""
	}
	rule, _ := proto.GetExtension(opts, apiannotations.E_Http).(*apiannotations.HttpRule)
	if rule == nil {
		return ""
	}
	switch {
	case rule.GetGet() != "":
		return rule.GetGet()
	case rule.GetPut() != "":
		return rule.GetPut()
	case rule.GetPost() != "":
		return rule.GetPost()
	case rule.GetPatch() != "":
		return rule.GetPatch()
	case rule.GetDelete() != "":
		return rule.GetDelete()
	case rule.GetCustom() != nil:
		return rule.GetCustom().GetPath()
	}
	return ""
}

// pathVariables returns the field paths bound by the {..} variables of an
// AIP/gRPC-gateway URL template, stripping any "=pattern" suffix (so
// "{name=accounts/*/entries/*}" yields "name" and "{ledger_account_id}" yields
// "ledger_account_id").
func pathVariables(path string) []string {
	var out []string
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			break
		}
		rest := path[i+1:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			break
		}
		inner := rest[:j]
		if eq := strings.IndexByte(inner, '='); eq >= 0 {
			inner = inner[:eq]
		}
		out = append(out, strings.TrimSpace(inner))
		path = rest[j+1:]
	}
	return out
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
