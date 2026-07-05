package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/infobloxopen/devedge-sdk/internal/aip"
)

// loadDescriptors reads a serialized FileDescriptorSet (as produced by
// `buf build -o <file>`) and returns a protoreflect file registry. The idiom
// mirrors cmd/security-check.
func loadDescriptors(path string) (*protoregistry.Files, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read descriptor %s: %w", path, err)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, fds); err != nil {
		return nil, fmt.Errorf("unmarshal descriptor %s: %w", path, err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("build file registry from %s: %w", path, err)
	}
	return files, nil
}

// enrich makes the OpenAPI v3 document lossless and AUTHORITATIVE for the
// proto-derived contract semantics: field_behavior (readOnly/writeOnly/required),
// enum (from allowed_values), AIP-122 resource identity, AIP standard-method
// classification, cross-service references, and pagination. Where the proto truth
// and the base spec (kin-openapi's ToV3 of the gateway swagger) disagree on any of
// these, the proto value WINS (overwrite, never paper over). Structural schema
// (types/nesting) and HTTP paths are inherited from the base spec unchanged.
//
// It fails loud (returns an error → non-zero exit) if a resource message/field the
// classifier sees is absent from the spec, or a resource schema property or an
// operation has no proto counterpart — losslessness is enforced, not best-effort.
func enrich(doc *openapi3.T, files *protoregistry.Files) error {
	if doc.Components == nil {
		return fmt.Errorf("enrich: spec has no components")
	}

	// Index proto facts: schema-name → message, operationId → (method, std),
	// and the set of resource messages.
	schemaToMsg := map[string]protoreflect.MessageDescriptor{}
	opToMethod := map[string]classifiedMethod{}
	var resourceMsgs []protoreflect.MessageDescriptor

	var rangeErr error
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Package()), "google.") {
			return true // well-known / annotation packages are not app contract
		}
		msgs := fd.Messages()
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			schemaToMsg[schemaName(md)] = md
			if _, ok := aip.ResolveResourceIdentity(md); ok {
				resourceMsgs = append(resourceMsgs, md)
			}
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			sd := svcs.Get(i)
			res := aip.DetectServiceResource(sd)
			softDelete := res != nil && aip.MessageFacts(res).SoftDelete
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				if !proto.HasExtension(md.Options(), apiannotations.E_Http) {
					continue // only REST-exposed methods become operations
				}
				opID := fmt.Sprintf("%s_%s", sd.Name(), md.Name())
				opToMethod[opID] = classifiedMethod{
					method: md,
					std:    aip.ClassifyMethod(md, res, softDelete),
				}
			}
		}
		return true
	})
	if rangeErr != nil {
		return rangeErr
	}

	// Enrich every schema that maps to a proto message.
	for name, ref := range doc.Components.Schemas {
		md, ok := schemaToMsg[name]
		if !ok || ref == nil || ref.Value == nil {
			continue
		}
		if err := enrichSchema(name, ref.Value, md); err != nil {
			return err
		}
	}

	// Enrich every operation, keyed by operationId.
	seenOps := map[string]bool{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.OperationID == "" {
				continue
			}
			cm, ok := opToMethod[op.OperationID]
			if !ok {
				return fmt.Errorf("enrich: operation %q has no matching proto method (FDS/swagger drift)", op.OperationID)
			}
			seenOps[op.OperationID] = true
			setExt(&op.Extensions, "x-aip-method", cm.std.String())
			if cm.std == aip.MethodList {
				if pg := paginationExt(cm.method); pg != nil {
					setExt(&op.Extensions, "x-aip-pagination", pg)
				}
			}
		}
	}

	// Losslessness gates (FM-3): every resource message + its fields must appear,
	// and every REST-exposed method must have an operation.
	for _, md := range resourceMsgs {
		sn := schemaName(md)
		ref := doc.Components.Schemas[sn]
		if ref == nil || ref.Value == nil {
			return fmt.Errorf("enrich: resource %s has no schema %q in the spec (FDS/swagger drift)", md.FullName(), sn)
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			jn := string(fields.Get(i).JSONName())
			if _, has := ref.Value.Properties[jn]; !has {
				return fmt.Errorf("enrich: resource %s field %q absent from schema %q (FDS/swagger drift)", md.FullName(), jn, sn)
			}
		}
	}
	for opID := range opToMethod {
		if !seenOps[opID] {
			return fmt.Errorf("enrich: method %q (REST-exposed) has no operation in the spec (FDS/swagger drift)", opID)
		}
	}
	return nil
}

type classifiedMethod struct {
	method protoreflect.MethodDescriptor
	std    aip.StdMethod
}

// propKeyMode selects how schema properties are keyed against proto fields:
// by fd.JSONName() (camelCase, the proto-JSON and gateway-v2 default) or by
// fd.Name() (snake_case, gw-v1 emitters run with json_names_for_fields=false).
type propKeyMode int

const (
	keyCamel propKeyMode = iota
	keySnake
)

// enrichSchema writes the authoritative proto-derived semantics onto a schema and
// its properties, and (for resource messages) attaches x-aip-resource. It also
// enforces that every property maps to a proto field of md (vice-versa drift).
func enrichSchema(name string, schema *openapi3.Schema, md protoreflect.MessageDescriptor) error {
	return enrichSchemaCore(name, schema, md, keyCamel, nil)
}

// enrichSchemaCore is the mode-aware body of enrichSchema. With rep == nil it is
// the fail-loud default path, byte-identical to the historical behavior. With a
// non-nil rep (gateway-v1 compat) every gate failure degrades to a coverage
// entry: unmatched properties are counted as skipped instead of erroring, and
// enrichment continues.
func enrichSchemaCore(name string, schema *openapi3.Schema, md protoreflect.MessageDescriptor, mode propKeyMode, rep *coverageReport) error {
	fieldsByKey := map[string]protoreflect.FieldDescriptor{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		key := string(fd.JSONName())
		if mode == keySnake {
			key = string(fd.Name())
		}
		fieldsByKey[key] = fd
	}

	_, isResource := aip.ResolveResourceIdentity(md)

	// Rebuild the schema's required list authoritatively from proto REQUIRED.
	var required []string
	for jn, ref := range schema.Properties {
		fd, ok := fieldsByKey[jn]
		if !ok {
			if rep != nil {
				rep.fieldSkipped(name, jn, fmt.Sprintf("no proto field with this name on %s", md.FullName()))
				continue
			}
			if isResource {
				return fmt.Errorf("enrich: schema %q property %q has no proto field on %s (FDS/swagger drift)", name, jn, md.FullName())
			}
			continue
		}
		if ref == nil || ref.Value == nil {
			continue
		}
		bs, err := aip.ResolveFieldBehavior(fd)
		if err != nil {
			if rep != nil {
				rep.fieldSkipped(name, jn, err.Error())
				continue
			}
			return fmt.Errorf("enrich: schema %q: %w", name, err)
		}
		p := ref.Value
		// Authoritative native fields: proto wins over ToV3.
		p.ReadOnly = aip.HasBehavior(bs, aip.OutputOnly)
		p.WriteOnly = aip.HasBehavior(bs, aip.InputOnly)
		if allowed := aip.AllowedValues(fd); len(allowed) > 0 {
			enum := make([]any, len(allowed))
			for i, v := range allowed {
				enum[i] = v
			}
			p.Enum = enum
		}
		if aip.HasBehavior(bs, aip.Required) {
			required = append(required, jn)
		}
		// x-aip-field-behavior: the raw enum names (carries IMMUTABLE, which OpenAPI
		// cannot express natively, plus any others — losslessly).
		if names := behaviorNames(bs); len(names) > 0 {
			setExt(&p.Extensions, "x-aip-field-behavior", names)
		} else {
			delExt(p.Extensions, "x-aip-field-behavior")
		}
		// x-aip-references: WS-021 cross-service reference target.
		if target, ok := aip.ReferenceTarget(fd); ok {
			setExt(&p.Extensions, "x-aip-references", map[string]any{"type": target})
		}
		if rep != nil {
			rep.fieldEnriched()
		}
	}
	schema.Required = sortedUnique(required)

	if isResource {
		id, _ := aip.ResolveResourceIdentity(md)
		setExt(&schema.Extensions, "x-aip-resource", map[string]any{
			"type":    id.Type,
			"pattern": id.Patterns,
			"key":     id.Key,
		})
	}
	return nil
}

// schemaName reconstructs the grpc-gateway (protoc-gen-openapiv2) default schema
// name for a message: the last package segment + the message short name, e.g.
// toy.v1.Widget → "v1Widget".
func schemaName(md protoreflect.MessageDescriptor) string {
	pkg := string(md.ParentFile().Package())
	last := pkg
	if i := strings.LastIndex(pkg, "."); i >= 0 {
		last = pkg[i+1:]
	}
	return last + string(md.Name())
}

// paginationExt builds the x-aip-pagination triad (query params + response field)
// from a List method's request/response messages.
func paginationExt(md protoreflect.MethodDescriptor) map[string]any {
	in := md.Input()
	out := md.Output()
	ext := map[string]any{}
	if fd := in.Fields().ByName("page_size"); fd != nil {
		ext["pageSizeParam"] = string(fd.JSONName())
	}
	if fd := in.Fields().ByName("page_token"); fd != nil {
		ext["pageTokenParam"] = string(fd.JSONName())
	}
	if fd := out.Fields().ByName("next_page_token"); fd != nil {
		ext["nextPageTokenField"] = string(fd.JSONName())
	}
	if len(ext) == 0 {
		return nil
	}
	return ext
}

// behaviorNames returns the raw field_behavior enum names for the extension.
func behaviorNames(bs []aip.FieldBehavior) []string {
	if len(bs) == 0 {
		return nil
	}
	names := make([]string, 0, len(bs))
	for _, b := range bs {
		names = append(names, b.String())
	}
	return names
}

func setExt(m *map[string]any, key string, val any) {
	if *m == nil {
		*m = map[string]any{}
	}
	(*m)[key] = val
}

func delExt(m map[string]any, key string) {
	if m != nil {
		delete(m, key)
	}
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	// Deterministic order.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
