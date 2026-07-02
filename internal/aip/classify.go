package aip

import (
	"strings"

	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// StdMethod is the classified AIP standard-method kind for an RPC.
type StdMethod int

const (
	// MethodNone is a custom / unclassified RPC (not a standard AIP method).
	MethodNone StdMethod = iota
	MethodCreate
	MethodGet
	MethodList
	MethodUpdate
	MethodDelete
	MethodUndelete
	// MethodBatchGet is an AIP-137 BatchGet<R>: a read returning many resources
	// by id/name in one call.
	MethodBatchGet
)

// String returns the AIP standard-method name for x-aip-method. A custom /
// unclassified method reports "Custom".
func (s StdMethod) String() string {
	switch s {
	case MethodCreate:
		return "Create"
	case MethodGet:
		return "Get"
	case MethodList:
		return "List"
	case MethodUpdate:
		return "Update"
	case MethodDelete:
		return "Delete"
	case MethodUndelete:
		return "Undelete"
	case MethodBatchGet:
		return "BatchGet"
	default:
		return "Custom"
	}
}

// IsWrite reports whether a classified standard method is write-capable
// (Create/Update/Delete/Undelete).
func (s StdMethod) IsWrite() bool {
	switch s {
	case MethodCreate, MethodUpdate, MethodDelete, MethodUndelete:
		return true
	default:
		return false
	}
}

// ResourceFacts summarizes a message's AIP resource markers, used both to decide
// whether a message qualifies as a resource for method classification and to
// surface its AIP-122 identity.
type ResourceFacts struct {
	// IsResource reports whether the message qualifies as a resource for method
	// classification: it has an AIP-122 name (a (google.api.resource) pattern or a
	// string `name` field), opts into soft-delete, or is a DDD aggregate member.
	IsResource bool
	// SoftDelete reports an OUTPUT_ONLY delete_time Timestamp field (AIP-148).
	SoftDelete bool
	// HasName reports a (google.api.resource) pattern or a string `name` field.
	HasName bool
	// MemberRoot is the (infoblox.ddd.v1.member).root ("" when not a member).
	MemberRoot string
	// Type is the (google.api.resource).type ("" when the message has no
	// (google.api.resource) annotation).
	Type string
	// Patterns are the (google.api.resource).pattern values.
	Patterns []string
}

// MessageFacts inspects a message for AIP resource markers. It mirrors the
// resource-detection the codegen plugins previously carried in
// package-main-local helpers, so behavior is identical for existing fixtures.
func MessageFacts(md protoreflect.MessageDescriptor) ResourceFacts {
	var r ResourceFacts
	opts := md.Options()
	if opts != nil {
		if proto.HasExtension(opts, apiannotations.E_Resource) {
			if rd, _ := proto.GetExtension(opts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); rd != nil {
				if len(rd.GetPattern()) > 0 {
					r.HasName = true
				}
				r.Patterns = rd.GetPattern()
				r.Type = rd.GetType()
			}
		}
		if proto.HasExtension(opts, dddv1.E_Member) {
			if mb, _ := proto.GetExtension(opts, dddv1.E_Member).(*dddv1.Member); mb != nil {
				r.MemberRoot = mb.GetRoot()
			}
		}
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		name := string(fd.Name())
		if name == "name" && fd.Kind() == protoreflect.StringKind {
			r.HasName = true
		}
		if name == "delete_time" && isTimestamp(fd) {
			if oo, _ := IsOutputOnly(fd); oo {
				r.SoftDelete = true
			}
		}
	}
	r.IsResource = r.HasName || r.SoftDelete || r.MemberRoot != ""
	return r
}

// ResourceIdentity is the AIP-122 identity of a resource message.
type ResourceIdentity struct {
	// Type is the (google.api.resource).type, e.g. "toy.example.com/Widget".
	Type string
	// Patterns are the (google.api.resource).pattern values, e.g. "widgets/{widget}".
	Patterns []string
	// Key is how the resource is addressed: "id" when it has a string id field,
	// else "name".
	Key string
}

// ResolveResourceIdentity returns the AIP-122 identity of a message that carries
// a (google.api.resource) annotation, and true when present. Messages without
// (google.api.resource) (including DDD members addressed only through their root)
// return false — x-aip-resource is recovered from (google.api.resource) only.
func ResolveResourceIdentity(md protoreflect.MessageDescriptor) (ResourceIdentity, bool) {
	facts := MessageFacts(md)
	if facts.Type == "" {
		return ResourceIdentity{}, false
	}
	key := "name"
	if hasField(md, "id", protoreflect.StringKind) {
		key = "id"
	}
	return ResourceIdentity{Type: facts.Type, Patterns: facts.Patterns, Key: key}, true
}

// DetectServiceResource resolves the resource message a service manages, using
// the same precedence the svc plugin previously used: a standard-method return
// type that is a resource message (Create/Get/Update), else a request's singular
// message-typed field whose type is a message declared in the same file
// (Create/Update), else a List response's repeated message element declared in
// the same file. Returns nil when no resource is detectable (custom-only svc).
func DetectServiceResource(sd protoreflect.ServiceDescriptor) protoreflect.MessageDescriptor {
	file := sd.ParentFile()
	known := map[protoreflect.FullName]protoreflect.MessageDescriptor{}
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		known[m.FullName()] = m
	}

	methods := sd.Methods()
	// A return type that is itself a resource message.
	for i := 0; i < methods.Len(); i++ {
		out := methods.Get(i).Output()
		if m, ok := known[out.FullName()]; ok && MessageFacts(m).IsResource {
			return m
		}
	}
	// A request carrying a single message-typed field declared in this file.
	for i := 0; i < methods.Len(); i++ {
		in := methods.Get(i).Input()
		fs := in.Fields()
		for j := 0; j < fs.Len(); j++ {
			fd := fs.Get(j)
			if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() && fd.Message() != nil {
				if m, ok := known[fd.Message().FullName()]; ok {
					return m
				}
			}
		}
	}
	// A List response carrying a repeated message field declared in this file.
	for i := 0; i < methods.Len(); i++ {
		out := methods.Get(i).Output()
		fs := out.Fields()
		for j := 0; j < fs.Len(); j++ {
			fd := fs.Get(j)
			if fd.Kind() == protoreflect.MessageKind && fd.IsList() && fd.Message() != nil {
				if m, ok := known[fd.Message().FullName()]; ok {
					return m
				}
			}
		}
	}
	return nil
}

// ClassifyMethod detects which AIP standard method an RPC implements, by the
// shape of its request/response messages (tolerating extra optional fields).
// resource is the service's managed resource message (nil disables detection);
// softDelete enables the Undelete shape. This is the single classifier both the
// codegen plugins and the OpenAPI enrichment pass use (D-new-1).
func ClassifyMethod(md protoreflect.MethodDescriptor, resource protoreflect.MessageDescriptor, softDelete bool) StdMethod {
	if resource == nil {
		return MethodNone
	}
	in := md.Input()
	out := md.Output()
	resName := resource.FullName()

	hasResourceField := func(msg protoreflect.MessageDescriptor) bool {
		fs := msg.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() &&
				fd.Message() != nil && fd.Message().FullName() == resName {
				return true
			}
		}
		return false
	}
	hasUpdateMask := func(msg protoreflect.MessageDescriptor) bool {
		fs := msg.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			if string(fd.Name()) != "update_mask" {
				continue
			}
			// Accept a repeated-string update_mask (the SDK convention) or a
			// google.protobuf.FieldMask-typed update_mask (canonical AIP-134).
			if fd.IsList() && fd.Kind() == protoreflect.StringKind {
				return true
			}
			if fd.Kind() == protoreflect.MessageKind && fd.Message() != nil &&
				fd.Message().FullName() == "google.protobuf.FieldMask" {
				return true
			}
		}
		return false
	}
	hasRepeatedResource := func(msg protoreflect.MessageDescriptor) bool {
		fs := msg.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			if fd.Kind() == protoreflect.MessageKind && fd.IsList() &&
				fd.Message() != nil && fd.Message().FullName() == resName {
				return true
			}
		}
		return false
	}
	hasRepeatedStringField := func(msg protoreflect.MessageDescriptor, name string) bool {
		fs := msg.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			if string(fd.Name()) == name && fd.IsList() && fd.Kind() == protoreflect.StringKind {
				return true
			}
		}
		return false
	}

	idIn := hasField(in, "id", protoreflect.StringKind)
	nameIn := hasField(in, "name", protoreflect.StringKind)
	returnsResource := out.FullName() == resName
	methodName := string(md.Name())
	hasPrefix := func(p string) bool { return strings.HasPrefix(methodName, p) }

	// BatchGet (AIP-137): detected before Get so its repeated key list is not
	// mistaken for a single-id Get.
	if hasPrefix("BatchGet") &&
		(hasRepeatedStringField(in, "ids") || hasRepeatedStringField(in, "names")) &&
		hasRepeatedResource(out) {
		return MethodBatchGet
	}
	// Update: resource + update_mask, returns the resource.
	if hasResourceField(in) && hasUpdateMask(in) && returnsResource {
		return MethodUpdate
	}
	// Create: resource (no update_mask), returns the resource.
	if hasResourceField(in) && !hasUpdateMask(in) && returnsResource {
		return MethodCreate
	}
	// List: page_size + page_token, response has repeated resource + next_page_token.
	if hasField(in, "page_size", protoreflect.Int32Kind) &&
		hasField(in, "page_token", protoreflect.StringKind) &&
		hasRepeatedResource(out) &&
		hasField(out, "next_page_token", protoreflect.StringKind) {
		return MethodList
	}
	// Undelete (soft-delete only): keyed by id/name, returns the resource, named Undelete<R>.
	if softDelete && (idIn || nameIn) && returnsResource && hasPrefix("Undelete") {
		return MethodUndelete
	}
	// Get: keyed by id OR name, returns the resource (and not Undelete).
	if (idIn || nameIn) && returnsResource && hasPrefix("Get") {
		return MethodGet
	}
	// Delete: keyed by id OR name, returns a non-resource, named Delete<R>.
	if (idIn || nameIn) && !returnsResource && hasPrefix("Delete") {
		return MethodDelete
	}
	return MethodNone
}

func hasField(msg protoreflect.MessageDescriptor, name string, kind protoreflect.Kind) bool {
	fs := msg.Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		if string(fd.Name()) == name && fd.Kind() == kind {
			return true
		}
	}
	return false
}

func isTimestamp(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.MessageKind &&
		fd.Message() != nil &&
		fd.Message().FullName() == "google.protobuf.Timestamp"
}
