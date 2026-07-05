package main

import (
	"fmt"
	"strings"
)

// serviceInfo describes a proto service for code generation.
type serviceInfo struct {
	ServiceName string
	// ProtoPackage is the proto package (e.g. "orders.v1"); its first segment is
	// the servicekit module ID the generated Module() reports.
	ProtoPackage string
	Methods      []methodInfo
	// Resource is the Go type name of the API resource the service manages (the
	// repository element type). Empty when no resource is detectable, in which
	// case no default CRUD handler is generated for the service.
	Resource           string
	ResourceSoftDelete bool
	// ResourceType is the (google.api.resource).type of the managed resource (the
	// AIP-122 type, e.g. "region.example.com/Region"). When the service serves a
	// generated BatchGet<R>, it declares this type a batch-fetchable reference
	// TARGET so the F041 fail-loud gate can match a reference's TargetType to it.
	ResourceType string
	// MemberRoot names the owning aggregate root when the service's resource is a
	// DDD member (infoblox.ddd.v1.member). When set, write-capable standard methods
	// are emitted as Unimplemented (route through the root) and the service records
	// a server.MemberBinding so the boot-time boundary gate fails closed.
	MemberRoot string
	// EnumFields are the resource's string fields carrying an allowed_values
	// constraint (BC-08). When non-empty, a validate<Resource> function is
	// generated and the Create/Update handlers call it before persistence.
	EnumFields []enumField
	// References are the cross-service resource references declared on the
	// resource's scalar FK fields via google.api.resource_reference (F041). When
	// non-empty, a <Svc>References metadata table is emitted (AC-1) and the service
	// contributes its references to the boot-time fail-loud target gate.
	References []referenceInfo
}

// referenceInfo is one cross-service reference declared on a resource field via
// google.api.resource_reference (AIP-124). It is the generator-side view; the
// emitted table entry is a reference.Reference (the ROOT-module seam type).
type referenceInfo struct {
	FieldGoName string // Go field name holding the FK, e.g. "RegionId"
	FKField     string // proto field name, e.g. "region_id"
	TargetType  string // AIP-122 target type, e.g. "region.example.com/Region"
	Cardinality string // "one" (scalar FK) or "many" (repeated FK)
}

// hasBatchGet reports whether the service exposes a generated AIP-137 BatchGet<R>
// method. When true the CRUD handler's Repo is a persistence.BatchRepository (so a
// non-batch repo is a compile error — the codegen half of the fail-loud gate).
func (s serviceInfo) hasBatchGet() bool {
	for _, m := range s.Methods {
		if m.Std == stdBatchGet {
			return true
		}
	}
	return false
}

// repoInterface is the persistence seam the generated handler/constructors take:
// BatchRepository when the service serves a generated BatchGet (so the batch
// capability is required at compile time), else the plain Repository.
func (s serviceInfo) repoInterface() string {
	if s.hasBatchGet() {
		return "BatchRepository"
	}
	return "Repository"
}

// referenceTargetType is the AIP-122 resource type this service serves as a
// batch-fetchable reference target. Prefers the (google.api.resource).type; falls
// back to the module-qualified resource name when the resource declares no type,
// so the gate still has a stable key to match a reference against.
func (s serviceInfo) referenceTargetType() string {
	if s.ResourceType != "" {
		return s.ResourceType
	}
	return s.moduleID() + "." + snakeIdent(s.Resource)
}

// enumField is a resource string field constrained to a fixed set of values
// (an (infoblox.field.v1.opts).allowed_values annotation, BC-08).
type enumField struct {
	Getter    string   // Go getter on the resource, e.g. "GetStatus"
	ProtoName string   // proto field name, for the error message, e.g. "status"
	Allowed   []string // the permitted values
}

// isMember reports whether the service's resource is a DDD aggregate member.
func (s serviceInfo) isMember() bool { return s.MemberRoot != "" }

// moduleID is the servicekit module ID for the service: the first segment of the
// proto package (e.g. "orders.v1" -> "orders"). Falls back to a lower-cased
// service name when the package is unknown (defensive; real protos always have a
// package). It is the stable, unique key the host validates and namespaces on.
func (s serviceInfo) moduleID() string {
	if s.ProtoPackage != "" {
		if i := strings.IndexByte(s.ProtoPackage, '.'); i > 0 {
			return s.ProtoPackage[:i]
		}
		return s.ProtoPackage
	}
	return strings.ToLower(s.ServiceName)
}

// hasStdMethods reports whether the service has at least one detected standard
// method (so a default CRUD handler is worth generating).
func (s serviceInfo) hasStdMethods() bool {
	if s.Resource == "" {
		return false
	}
	for _, m := range s.Methods {
		if m.Std != stdNone {
			return true
		}
	}
	return false
}

// hasWriteMethods reports whether the service has at least one write-capable
// method a member would redirect (a standard Create/Update/Delete/Undelete or an
// AIP-137 batch write). Used to decide whether a member service needs the
// status/codes imports for its Unimplemented redirects.
func (s serviceInfo) hasWriteMethods() bool {
	for _, m := range s.Methods {
		if m.isMemberSuppressedWrite() {
			return true
		}
	}
	return false
}

// methodInfo describes a single RPC method.
type methodInfo struct {
	Name          string
	InputGoIdent  string // Go type name for the request (within the same package)
	OutputGoIdent string // Go type name for the response
	Std           stdMethod
	// KeyByName reports that a Get/Delete/Undelete is keyed by an AIP-122 name
	// field (parse via Parse<R>Name) rather than a plain id.
	KeyByName bool
	// ListItemsField is the Go field name of the repeated-resource field on a List
	// response (e.g. "Widgets", "ApiKeys"), set only for stdList methods.
	ListItemsField string
	// ResourceField is the Go field name of the resource-typed request field on a
	// Create/Update request (e.g. "Widget", "ApiKey"), set for stdCreate/stdUpdate.
	ResourceField string
	// List optional fields present on the request (drives whether the handler maps
	// them onto persistence.ListOptions — protoc-gen-go only emits a getter for a
	// field that exists). Set only for stdList methods.
	ListHasFilter      bool
	ListHasOrderBy     bool
	ListHasShowDeleted bool
	// BatchIDsField is the Go field name of the repeated-string key list on a
	// BatchGet request (e.g. "Ids"), set only for stdBatchGet methods.
	BatchIDsField string
	// BatchItemsField is the Go field name of the repeated-resource field on a
	// BatchGet response (e.g. "Regions"), set only for stdBatchGet methods.
	BatchItemsField string
	// Parent is the nested-resource parent scope (#191): when a Get/List/Delete's
	// AIP-122 URL nests the resource under a parent segment (e.g.
	// accounts/{ledger_account_id}/entries/{id}), the generated handler must
	// enforce that parent, not just address by the resource key. It is nil for a
	// non-nested method. A nested method whose parent segment resolves to no
	// resource foreign-key field is a fail-loud codegen error (never bound-and-
	// ignored), surfaced in main.go before this is set.
	Parent *parentScope
}

// parentScope describes the enforcement of a nested AIP-122 URL parent segment on
// a Get/List/Delete method (#191). The parent segment binds a request field (a
// foreign key), and the managed resource carries a matching scalar FK field. The
// generated handler filters List by the FK and denies a Get/Delete whose fetched
// resource belongs to a different parent than the URL names.
type parentScope struct {
	// ReqGetter is the Go getter for the parent value on the REQUEST, e.g.
	// "GetLedgerAccountId".
	ReqGetter string
	// ResGetter is the Go getter for the foreign key on the RESOURCE, e.g.
	// "GetLedgerAccountId".
	ResGetter string
	// FKField is the proto field name of the resource's foreign key, e.g.
	// "ledger_account_id" — used to build the AIP-160 List scope filter (the
	// repository maps the proto field name to its DB column).
	FKField string
}

// isBatchWrite reports whether the method is an AIP-137 batch WRITE
// (BatchCreate/BatchUpdate/BatchDelete) by name. classifyMethod does not assign a
// stdMethod to batch RPCs, so a member service's batch writes would otherwise NOT
// be recorded in its boundary-gate WriteMethods nor redirected to Unimplemented — a
// fail-OPEN hole letting a hand-written member handler mutate the member outside its
// aggregate root. BatchGet is a read and is intentionally excluded (reads ≠ write
// authority, mirroring Get/List).
func (m methodInfo) isBatchWrite() bool {
	for _, p := range []string{"BatchCreate", "BatchUpdate", "BatchDelete"} {
		if strings.HasPrefix(m.Name, p) {
			return true
		}
	}
	return false
}

// isMemberSuppressedWrite reports whether, for a DDD aggregate MEMBER service, this
// method is a write that must be redirected to Unimplemented and recorded for the
// boundary gate. It covers both the classified standard writes
// (Create/Update/Delete/Undelete) and the name-detected AIP-137 batch writes
// (Batch{Create,Update,Delete}) that classifyMethod leaves as stdNone — so a member
// cannot independently mutate via a batch RPC either (matching the MemberBinding
// contract, which promises Batch* coverage).
func (m methodInfo) isMemberSuppressedWrite() bool {
	return m.Std.isWrite() || m.isBatchWrite()
}

// renderSvcFile generates the .svc.go content for the given package and services.
// Returns an empty string when services is empty.
//
// This generator assumes protoc-gen-go-grpc and protoc-gen-grpc-gateway have
// already run for the same proto file (same package). Those plugins provide:
//   - <Service>Server interface + Unimplemented<Service>Server (from _grpc.pb.go)
//   - Register<Service>Server(grpc.ServiceRegistrar, <Service>Server) (from _grpc.pb.go)
//   - <Service>_<Method>_FullMethodName constants (from _grpc.pb.go)
//   - Register<Service>HandlerClient + New<Service>Client (from .pb.gw.go)
//
// It also assumes protoc-gen-devedge-authz emitted <Service>AuthzRules and
// protoc-gen-{storage,ent} emitted persistence.Repository constructors +
// Parse<Resource>Name helpers in the same package.
func renderSvcFile(pkgName, _ string, services []serviceInfo) string {
	if len(services) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("// Code generated by protoc-gen-svc. DO NOT EDIT.\n")
	b.WriteString("// source: (proto input)\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "package %s\n\n", pkgName)

	needPersistence := false
	needStatus := false
	needServicekit := false
	needReference := false
	needFmt := false
	for _, svc := range services {
		if svc.hasStdMethods() {
			needPersistence = true
			// A service with a generated CRUD/repo path also gets a servicekit
			// Module() wrapper (its Register goes through Register<Svc>WithRepository).
			needServicekit = true
		}
		if svc.isMember() && svc.hasWriteMethods() {
			// A member service redirects its write methods to gRPC Unimplemented.
			needStatus = true
		}
		if len(svc.EnumFields) > 0 {
			// The generated validate<Resource> rejects out-of-set values with a
			// status.Errorf(codes.InvalidArgument, ...) (BC-08).
			needStatus = true
		}
		if len(svc.References) > 0 {
			// The emitted <Svc>References table is []reference.Reference (F041).
			needReference = true
		}
		for _, m := range svc.Methods {
			if m.Parent == nil {
				continue
			}
			// #191: a nested Get/Delete denies cross-parent access with
			// status.Errorf(codes.NotFound, ...); a nested List builds its scope
			// filter with fmt.Sprintf.
			switch m.Std {
			case stdGet, stdDelete:
				needStatus = true
			case stdList:
				needFmt = true
			}
		}
	}

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	if needServicekit {
		// The Module's Register guards a missing Repo/Handler with errors.New.
		b.WriteString("\t\"errors\"\n")
	}
	if needFmt {
		// A nested List (#191) builds its parent-scope filter with fmt.Sprintf.
		b.WriteString("\t\"fmt\"\n")
	}
	b.WriteString("\n")
	b.WriteString("\t\"github.com/grpc-ecosystem/grpc-gateway/v2/runtime\"\n")
	b.WriteString("\t\"google.golang.org/grpc\"\n")
	if needStatus {
		b.WriteString("\t\"google.golang.org/grpc/codes\"\n")
		b.WriteString("\t\"google.golang.org/grpc/status\"\n")
	}
	b.WriteString("\n")
	if needPersistence {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/persistence\"\n")
	}
	if needReference {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/reference\"\n")
	}
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/server\"\n")
	if needServicekit {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/servicekit\"\n")
	}
	b.WriteString(")\n\n")

	for _, svc := range services {
		renderReferences(&b, svc)
		renderRegister(&b, svc)
		if svc.hasStdMethods() {
			renderCRUDHandler(&b, svc)
			renderHandlerConstructors(&b, svc)
			// The servicekit Module: a thin, generated wrapper over
			// Register<Svc>WithRepository whose Descriptor is the proto facts.
			renderModule(&b, svc)
		}
	}

	return b.String()
}

// renderReferences emits the generated <Svc>References metadata table (F041 AC-1):
// one reference.Reference per cross-service reference declared on the resource's
// scalar FK fields via google.api.resource_reference. It is metadata only (no Go
// edge, no cascade) — a composition layer reads it to batch-resolve targets. Emits
// the same DO-NOT-EDIT style as <Svc>AuthzRules. Nothing is emitted when the
// service declares no references.
func renderReferences(b *strings.Builder, svc serviceInfo) {
	if len(svc.References) == 0 {
		return
	}
	fmt.Fprintf(b, "// %sReferences are the cross-service resource references declared on %s's\n", svc.ServiceName, svc.Resource)
	b.WriteString("// resource via google.api.resource_reference (AIP-124). Each names a target\n")
	b.WriteString("// resource type served by (possibly) another microservice, keyed by a scalar\n")
	b.WriteString("// foreign key. It is metadata only: no traversable Go edge, no cascade. A\n")
	b.WriteString("// composition layer reads it to batch-resolve the targets (F041). DO NOT EDIT.\n")
	fmt.Fprintf(b, "var %sReferences = []reference.Reference{\n", svc.ServiceName)
	for _, r := range svc.References {
		card := "reference.One"
		if r.Cardinality == "many" {
			card = "reference.Many"
		}
		b.WriteString("\t{\n")
		fmt.Fprintf(b, "\t\tFieldName:   %q,\n", r.FieldGoName)
		fmt.Fprintf(b, "\t\tFKField:     %q,\n", r.FKField)
		fmt.Fprintf(b, "\t\tTargetType:  %q,\n", r.TargetType)
		fmt.Fprintf(b, "\t\tCardinality: %s,\n", card)
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")
}

// renderRegister emits Register<Svc>: record methods, contribute authz rules,
// register gRPC + the HTTP gateway. The boot-time completeness gate now runs at
// server.Serve over the accumulated rule set (no per-Register assertion).
func renderRegister(b *strings.Builder, svc serviceInfo) {
	fmt.Fprintf(b, "// Register%s wires srv into the server's gRPC handler and HTTP gateway,\n", svc.ServiceName)
	fmt.Fprintf(b, "// records its methods, and contributes %sAuthzRules to the server. The\n", svc.ServiceName)
	b.WriteString("// boot-time authz completeness gate runs at server.Serve over the accumulated\n")
	b.WriteString("// rule set (fail-closed).\n")
	fmt.Fprintf(b, "func Register%s(s *server.Server, srv %sServer) error {\n", svc.ServiceName, svc.ServiceName)

	b.WriteString("\ts.RecordMethods(\n")
	for _, m := range svc.Methods {
		fmt.Fprintf(b, "\t\t%s_%s_FullMethodName,\n", svc.ServiceName, m.Name)
	}
	b.WriteString("\t)\n")
	fmt.Fprintf(b, "\ts.AddRules(%sAuthzRules...)\n", svc.ServiceName)
	// F041: contribute this service's cross-service references and (when it serves
	// BatchGet) declare its resource a batch-fetchable reference TARGET, so the
	// boot-time fail-loud gate (AssertReferenceTargets) fails closed if a referenced
	// target type has no registered BatchGet — never a silent runtime N+1 (D-4).
	if len(svc.References) > 0 {
		fmt.Fprintf(b, "\ts.RecordReferences(%sReferences...)\n", svc.ServiceName)
	}
	if svc.hasBatchGet() {
		fmt.Fprintf(b, "\ts.RecordBatchTarget(%q)\n", svc.referenceTargetType())
	}
	// F031 DDD: a member service contributes a member→root binding so the boot-time
	// boundary gate fails closed if any of its write methods is registered.
	if svc.isMember() {
		fmt.Fprintf(b, "\ts.RecordMemberBinding(server.MemberBinding{\n")
		fmt.Fprintf(b, "\t\tResource: %q,\n", svc.Resource)
		fmt.Fprintf(b, "\t\tRoot:     %q,\n", svc.MemberRoot)
		b.WriteString("\t\tWriteMethods: []string{\n")
		for _, m := range svc.Methods {
			if m.isMemberSuppressedWrite() {
				fmt.Fprintf(b, "\t\t\t%s_%s_FullMethodName,\n", svc.ServiceName, m.Name)
			}
		}
		b.WriteString("\t\t},\n")
		b.WriteString("\t})\n")
	}
	fmt.Fprintf(b, "\tRegister%sServer(s.GRPCServer(), srv)\n", svc.ServiceName)
	b.WriteString("\ts.RegisterGateway(func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {\n")
	fmt.Fprintf(b, "\t\treturn Register%sHandlerClient(ctx, mux, New%sClient(conn))\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("\t})\n")
	b.WriteString("\treturn nil\n")
	b.WriteString("}\n\n")
}

// renderValidateFunc emits validate<Resource>, which enforces allowed_values
// (BC-08) on the resource's string-backed enum fields. The Create/Update handlers
// call it before persistence, so an out-of-set value is rejected with
// InvalidArgument. Emits nothing when the resource declares no allowed_values.
func renderValidateFunc(b *strings.Builder, svc serviceInfo) {
	if len(svc.EnumFields) == 0 {
		return
	}
	fmt.Fprintf(b, "// validate%s enforces allowed_values on the resource's string-backed enum\n", svc.Resource)
	b.WriteString("// fields. An empty value is treated as unset and skipped. DO NOT EDIT.\n")
	fmt.Fprintf(b, "func validate%s(m *%s) error {\n", svc.Resource, svc.Resource)
	b.WriteString("\tif m == nil {\n\t\treturn nil\n\t}\n")
	for _, ef := range svc.EnumFields {
		quoted := make([]string, len(ef.Allowed))
		for i, v := range ef.Allowed {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		fmt.Fprintf(b, "\tif v := m.%s(); v != \"\" {\n", ef.Getter)
		b.WriteString("\t\tswitch v {\n")
		fmt.Fprintf(b, "\t\tcase %s:\n", strings.Join(quoted, ", "))
		b.WriteString("\t\tdefault:\n")
		fmt.Fprintf(b, "\t\t\treturn status.Errorf(codes.InvalidArgument, \"%s: %%q is not an allowed value (want one of: %s)\", v)\n", ef.ProtoName, strings.Join(ef.Allowed, ", "))
		b.WriteString("\t\t}\n\t}\n")
	}
	b.WriteString("\treturn nil\n}\n\n")
}

// renderCRUDHandler emits the generated default handler: a struct embedding
// Unimplemented<Svc>Server (so custom/unmatched RPCs are Unimplemented) and
// holding the repository, with one method per detected AIP standard RPC
// delegating to the repository. Override by embedding this type and redefining
// the method(s) you change.
func renderCRUDHandler(b *strings.Builder, svc serviceInfo) {
	res := svc.Resource
	fmt.Fprintf(b, "// %sCRUDHandler is the generated default handler for %s. It implements the\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("// detected AIP standard methods by delegating to Repo; custom or unmatched RPCs\n")
	b.WriteString("// remain Unimplemented (override by embedding this type). Tenant stamping and\n")
	b.WriteString("// read_mask/field_mask are handled by the repository and the interceptor chain,\n")
	b.WriteString("// so the handler does not duplicate them. DO NOT EDIT.\n")
	fmt.Fprintf(b, "type %sCRUDHandler struct {\n", svc.ServiceName)
	fmt.Fprintf(b, "\tUnimplemented%sServer\n", svc.ServiceName)
	// F041: a service that serves a generated AIP-137 BatchGet<R> holds a
	// persistence.BatchRepository so BatchGet is guaranteed by construction — a
	// non-batch repository is a COMPILE error (the codegen half of the fail-loud
	// gate, D-4). Otherwise the plain Repository seam suffices.
	fmt.Fprintf(b, "\tRepo persistence.%s[*%s, string]\n", svc.repoInterface(), res)
	b.WriteString("}\n\n")

	renderValidateFunc(b, svc)

	for _, m := range svc.Methods {
		// F031 DDD member write-redirection (G-4): a member resource is addressable
		// for reads but written THROUGH its root, so its write-capable methods —
		// the standard Create/Update/Delete/Undelete AND the AIP-137 batch writes —
		// are emitted as gRPC Unimplemented instead of delegating to the repo.
		// Get/List/BatchGet fall through to the normal cases below. The boundary gate
		// at Serve additionally fails closed if such a write method is registered.
		if svc.isMember() && m.isMemberSuppressedWrite() {
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			fmt.Fprintf(b, "\t// %s is a member of aggregate %s: write through the root, not here.\n", svc.Resource, svc.MemberRoot)
			fmt.Fprintf(b, "\treturn nil, status.Errorf(codes.Unimplemented, \"%s is a member of aggregate %s: write through the aggregate root\")\n", svc.Resource, svc.MemberRoot)
			b.WriteString("}\n\n")
			continue
		}
		switch m.Std {
		case stdCreate:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			if len(svc.EnumFields) > 0 {
				fmt.Fprintf(b, "\tif err := validate%s(req.Get%s()); err != nil {\n\t\treturn nil, err\n\t}\n", res, m.ResourceField)
			}
			fmt.Fprintf(b, "\treturn h.Repo.Create(ctx, req.Get%s())\n", m.ResourceField)
			b.WriteString("}\n\n")
		case stdGet:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			if m.Parent != nil {
				// #191: a nested resource is addressed under a parent; the fetched
				// resource must belong to that parent, else it is addressed at the
				// wrong URL. Deny cross-parent access with NotFound (AIP-correct: the
				// resource does not exist at the requested address, and existence under
				// another parent is not leaked).
				b.WriteString("\tgot, err := h.Repo.Get(ctx, key)\n")
				b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
				renderParentGuard(b, res, m.Parent)
				b.WriteString("\treturn got, nil\n")
			} else {
				b.WriteString("\treturn h.Repo.Get(ctx, key)\n")
			}
			b.WriteString("}\n\n")
		case stdList:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			if m.Parent != nil {
				// #191: scope the list to the parent named in the URL. The scope is an
				// AIP-160 equality on the resource's foreign key, pushed down through
				// ListOptions.Filter (the repository binds the value — never SQL
				// interpolation). A caller-supplied filter is AND-combined so a nested
				// List cannot widen past its parent.
				fmt.Fprintf(b, "\tscope := fmt.Sprintf(\"%s = %%q\", req.%s())\n", m.Parent.FKField, m.Parent.ReqGetter)
				if m.ListHasFilter {
					b.WriteString("\tif f := req.GetFilter(); f != \"\" {\n")
					b.WriteString("\t\tscope = scope + \" AND (\" + f + \")\"\n")
					b.WriteString("\t}\n")
				}
			}
			b.WriteString("\titems, next, err := h.Repo.List(ctx, persistence.ListOptions{\n")
			b.WriteString("\t\tPageSize:  int(req.GetPageSize()),\n")
			b.WriteString("\t\tPageToken: req.GetPageToken(),\n")
			switch {
			case m.Parent != nil:
				b.WriteString("\t\tFilter: scope,\n")
			case m.ListHasFilter:
				b.WriteString("\t\tFilter: req.GetFilter(),\n")
			}
			if m.ListHasOrderBy {
				b.WriteString("\t\tOrderBy: req.GetOrderBy(),\n")
			}
			if m.ListHasShowDeleted {
				b.WriteString("\t\tShowDeleted: req.GetShowDeleted(),\n")
			}
			b.WriteString("\t})\n")
			b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
			fmt.Fprintf(b, "\treturn &%s{%s: items, NextPageToken: next}, nil\n", m.OutputGoIdent, m.ListItemsField)
			b.WriteString("}\n\n")
		case stdUpdate:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			if len(svc.EnumFields) > 0 {
				fmt.Fprintf(b, "\tif err := validate%s(req.Get%s()); err != nil {\n\t\treturn nil, err\n\t}\n", res, m.ResourceField)
			}
			fmt.Fprintf(b, "\treturn h.Repo.Update(ctx, req.Get%s().GetId(), req.Get%s(), req.GetUpdateMask()...)\n",
				m.ResourceField, m.ResourceField)
			b.WriteString("}\n\n")
		case stdDelete:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			if m.Parent != nil {
				// #191: verify the target belongs to the parent named in the URL before
				// deleting, so a cross-parent Delete is denied (NotFound) rather than
				// removing another parent's resource.
				b.WriteString("\tgot, err := h.Repo.Get(ctx, key)\n")
				b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
				renderParentGuard(b, res, m.Parent)
			}
			b.WriteString("\tif err := h.Repo.Delete(ctx, key); err != nil {\n\t\treturn nil, err\n\t}\n")
			fmt.Fprintf(b, "\treturn &%s{}, nil\n", m.OutputGoIdent)
			b.WriteString("}\n\n")
		case stdUndelete:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			b.WriteString("\treturn h.Repo.Undelete(ctx, key)\n")
			b.WriteString("}\n\n")
		case stdBatchGet:
			// F041 (G-2): the guaranteed AIP-137 BatchGet<R>. Delegates to the
			// BatchRepository — read_mask (AIP-157) and the read authz rule apply via
			// the interceptor chain, exactly like Get/List, so the handler adds no
			// projection/authz of its own. "referenced ⇒ batch-fetchable" by construction.
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			fmt.Fprintf(b, "\titems, err := h.Repo.BatchGet(ctx, req.Get%s())\n", m.BatchIDsField)
			b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
			fmt.Fprintf(b, "\treturn &%s{%s: items}, nil\n", m.OutputGoIdent, m.BatchItemsField)
			b.WriteString("}\n\n")
		}
	}
}

// renderKeyResolve emits the code that derives the repository key from the
// request: either req.GetId() directly, or Parse<R>Name(req.GetName()) when the
// method is keyed by an AIP-122 name.
func renderKeyResolve(b *strings.Builder, m methodInfo, res string) {
	if m.KeyByName {
		fmt.Fprintf(b, "\tkey, err := Parse%sName(req.GetName())\n", res)
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		return
	}
	b.WriteString("\tkey := req.GetId()\n")
}

// renderParentGuard emits the cross-parent denial for a nested Get/Delete (#191):
// the fetched resource (bound to `got`) must carry the same foreign-key value as
// the parent segment in the request, else it is addressed under the wrong parent
// and NotFound is returned. Assumes `got` and `err` are already in scope.
func renderParentGuard(b *strings.Builder, res string, p *parentScope) {
	fmt.Fprintf(b, "\tif got.%s() != req.%s() {\n", p.ResGetter, p.ReqGetter)
	fmt.Fprintf(b, "\t\treturn nil, status.Errorf(codes.NotFound, \"%s not found under the requested parent\")\n", res)
	b.WriteString("\t}\n")
}

// renderHandlerConstructors emits New<Svc>Handler (returns the default handler so
// it can be embedded/wrapped) and Register<Svc>WithRepository (the one-call CRUD
// path: construct the default handler + Register<Svc>).
func renderHandlerConstructors(b *strings.Builder, svc serviceInfo) {
	res := svc.Resource
	fmt.Fprintf(b, "// New%sHandler returns the generated default CRUD handler backed by repo. Embed\n", svc.ServiceName)
	b.WriteString("// the returned type (or this struct) to override individual methods before\n")
	fmt.Fprintf(b, "// registering it via Register%s.\n", svc.ServiceName)
	fmt.Fprintf(b, "func New%sHandler(repo persistence.%s[*%s, string]) *%sCRUDHandler {\n",
		svc.ServiceName, svc.repoInterface(), res, svc.ServiceName)
	fmt.Fprintf(b, "\treturn &%sCRUDHandler{Repo: repo}\n", svc.ServiceName)
	b.WriteString("}\n\n")

	fmt.Fprintf(b, "// Register%sWithRepository is the one-call CRUD path: it constructs the\n", svc.ServiceName)
	fmt.Fprintf(b, "// generated default handler over repo and registers it via Register%s\n", svc.ServiceName)
	b.WriteString("// (gRPC + REST gateway + authz rules). Use the New<Svc>Handler + Register<Svc>\n")
	b.WriteString("// pair instead when you need to wrap/override the default handler.\n")
	fmt.Fprintf(b, "func Register%sWithRepository(s *server.Server, repo persistence.%s[*%s, string]) error {\n",
		svc.ServiceName, svc.repoInterface(), res)
	fmt.Fprintf(b, "\treturn Register%s(s, New%sHandler(repo))\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("}\n\n")
}

// renderModule emits the servicekit composition surface (WS-012 P1): a
// <Svc>ModuleOptions struct carrying the hand-written parts the generator can't
// know, a <Svc>Module(opts) constructor, and the implementing type whose
// Descriptor() is the proto facts and whose Register wraps the existing
// Register<Svc>WithRepository over the shared server. The Module is a THIN,
// generated wrapper over the primitives — it does not replace Register<Svc> /
// Register<Svc>WithRepository, which a standalone main may still call directly.
//
// Only emitted for a service with a generated repo/CRUD path (hasStdMethods), so
// the Module's Register has a Register<Svc>WithRepository to call. The
// hand-written extras (custom health checks, event handlers, jobs, handler
// overrides, config schema) attach via the Options callback as later phases add
// the corresponding registries.
func renderModule(b *strings.Builder, svc serviceInfo) {
	res := svc.Resource
	typ := lowerFirst(svc.ServiceName) + "Module" // unexported impl type

	// Options: the hand-written parts. Repo drives the default CRUD path; Handler
	// is the override seam for custom or non-CRUD methods. Later phases extend this
	// struct (health/events/jobs) without changing the Module() contract.
	fmt.Fprintf(b, "// %sModuleOptions are the hand-written parts the generated %sModule needs that\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("// the generator cannot derive from the proto: the repository the module's CRUD\n")
	b.WriteString("// path registers over, OR an override handler for a service that adds custom or\n")
	b.WriteString("// non-CRUD methods. Exactly one of Repo / Handler is used (Handler wins).\n")
	fmt.Fprintf(b, "type %sModuleOptions struct {\n", svc.ServiceName)
	b.WriteString("\t// Repo is the persistence repository the module's generated CRUD handler\n")
	b.WriteString("\t// registers over (via Register" + svc.ServiceName + "WithRepository). Required\n")
	b.WriteString("\t// unless Handler is set.\n")
	fmt.Fprintf(b, "\tRepo persistence.%s[*%s, string]\n", svc.repoInterface(), res)
	b.WriteString("\n")
	// ID override (#190): the Descriptor's module ID defaults to the proto package
	// short-name, which is SHARED by every service declared in one proto file. Two
	// or more such services handed to servicekit.Run collide ("duplicate module
	// ID"). Set ID to a distinct value per service to host them together; leave it
	// empty for the package-derived default (the common single-service case).
	fmt.Fprintf(b, "\t// ID overrides the servicekit module ID this module reports in its\n")
	fmt.Fprintf(b, "\t// Descriptor. When empty, the ID defaults to %q (the proto package\n", svc.moduleID())
	b.WriteString("\t// short-name). Set a distinct ID per service when hosting two or more\n")
	b.WriteString("\t// services from the SAME proto file together, so their module IDs (and the\n")
	b.WriteString("\t// module-qualified resource names) do not collide at servicekit.Run.\n")
	b.WriteString("\tID string\n")
	b.WriteString("\n")
	b.WriteString("\t// Handler is an OPTIONAL override: when set, the module registers it (via\n")
	fmt.Fprintf(b, "\t// Register%s) instead of constructing the default CRUD handler over Repo.\n", svc.ServiceName)
	b.WriteString("\t// Use it to add custom / non-CRUD methods WITHOUT abandoning this generated\n")
	b.WriteString("\t// module: embed the default handler (New" + svc.ServiceName + "Handler(repo)) in your own\n")
	b.WriteString("\t// type, implement the extra methods, and pass it here. When Handler is set,\n")
	b.WriteString("\t// Repo may be nil (your handler owns its repositories).\n")
	fmt.Fprintf(b, "\tHandler %sServer\n", svc.ServiceName)
	b.WriteString("}\n\n")

	// Constructor.
	fmt.Fprintf(b, "// %sModule returns the importable servicekit.Module for %s: an introspectable\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("// unit a host (standalone or composed) can register on a shared server. Its\n")
	b.WriteString("// Descriptor is populated from the proto facts; its Register wraps the existing\n")
	fmt.Fprintf(b, "// Register%sWithRepository over the host's shared server.\n", svc.ServiceName)
	fmt.Fprintf(b, "func %sModule(opts %sModuleOptions) servicekit.Module {\n", svc.ServiceName, svc.ServiceName)
	fmt.Fprintf(b, "\treturn &%s{opts: opts}\n", typ)
	b.WriteString("}\n\n")

	// Impl type.
	fmt.Fprintf(b, "type %s struct {\n", typ)
	fmt.Fprintf(b, "\topts %sModuleOptions\n", svc.ServiceName)
	b.WriteString("}\n\n")

	// Descriptor().
	fmt.Fprintf(b, "// Descriptor implements servicekit.Module: the static proto facts for %s.\n", svc.ServiceName)
	fmt.Fprintf(b, "func (m *%s) Descriptor() servicekit.Descriptor {\n", typ)
	// The effective module ID: opts.ID when set, else the proto-package default
	// (#190). The module-qualified resource name uses the SAME effective ID so
	// distinct-ID co-resident services also get distinct resource qualifiers.
	fmt.Fprintf(b, "\tid := m.opts.ID\n")
	fmt.Fprintf(b, "\tif id == \"\" {\n\t\tid = %q\n\t}\n", svc.moduleID())
	b.WriteString("\treturn servicekit.Descriptor{\n")
	b.WriteString("\t\tID: id,\n")
	b.WriteString("\t\tMethods: []string{\n")
	for _, mth := range svc.Methods {
		fmt.Fprintf(b, "\t\t\t%s_%s_FullMethodName,\n", svc.ServiceName, mth.Name)
	}
	b.WriteString("\t\t},\n")
	// AuthzRules: reference the rules table protoc-gen-devedge-authz emits in the
	// same package — single source of truth, no duplication.
	fmt.Fprintf(b, "\t\tAuthzRules: %sAuthzRules,\n", svc.ServiceName)
	// Resources: the module-qualified resource name (module-qualified so two
	// co-resident modules never collide on a bare resource name, §5.7).
	fmt.Fprintf(b, "\t\tResources: []servicekit.ResourceDescriptor{{Name: id + %q}},\n", "."+snakeIdent(res))
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	// Register(): the override handler wins; otherwise the default CRUD path over
	// Repo. Neither set is a wiring bug, surfaced fail-closed at registration.
	fmt.Fprintf(b, "// Register implements servicekit.Module: wire %s onto the shared server. When\n", svc.ServiceName)
	fmt.Fprintf(b, "// opts.Handler is set it registers that handler (via Register%s); otherwise it\n", svc.ServiceName)
	fmt.Fprintf(b, "// takes the default CRUD path (Register%sWithRepository over opts.Repo).\n", svc.ServiceName)
	fmt.Fprintf(b, "func (m *%s) Register(_ context.Context, app *servicekit.App) error {\n", typ)
	b.WriteString("\tif m.opts.Handler != nil {\n")
	fmt.Fprintf(b, "\t\treturn Register%s(app.Server, m.opts.Handler)\n", svc.ServiceName)
	b.WriteString("\t}\n")
	b.WriteString("\tif m.opts.Repo == nil {\n")
	fmt.Fprintf(b, "\t\treturn errors.New(%q)\n", svc.ServiceName+"ModuleOptions: one of Repo or Handler is required")
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\treturn Register%sWithRepository(app.Server, m.opts.Repo)\n", svc.ServiceName)
	b.WriteString("}\n\n")
}

// lowerFirst lower-cases the first letter of s (PascalCase -> camelCase) for the
// unexported Module impl type name.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// snakeIdent converts a PascalCase/camelCase Go identifier to snake_case for the
// module-qualified resource name (e.g. "APIKey" -> "api_key", "Order" -> "order").
func snakeIdent(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			prev := runes[i-1]
			prevLower := prev >= 'a' && prev <= 'z'
			prevDigit := prev >= '0' && prev <= '9'
			// Underscore at a lower/digit -> upper boundary, or at the end of an
			// acronym run (XXy -> the last X starts a new word).
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLower || prevDigit || nextLower {
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
