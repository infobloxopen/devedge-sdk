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
	// MemberRoot names the owning aggregate root when the service's resource is a
	// DDD member (infoblox.ddd.v1.member). When set, write-capable standard methods
	// are emitted as Unimplemented (route through the root) and the service records
	// a server.MemberBinding so the boot-time boundary gate fails closed.
	MemberRoot string
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
	}

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n\n")
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
	b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/server\"\n")
	if needServicekit {
		b.WriteString("\t\"github.com/infobloxopen/devedge-sdk/servicekit\"\n")
	}
	b.WriteString(")\n\n")

	for _, svc := range services {
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
	fmt.Fprintf(b, "\tRepo persistence.Repository[*%s, string]\n", res)
	b.WriteString("}\n\n")

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
			fmt.Fprintf(b, "\treturn h.Repo.Create(ctx, req.Get%s())\n", m.ResourceField)
			b.WriteString("}\n\n")
		case stdGet:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			b.WriteString("\treturn h.Repo.Get(ctx, key)\n")
			b.WriteString("}\n\n")
		case stdList:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			b.WriteString("\titems, next, err := h.Repo.List(ctx, persistence.ListOptions{\n")
			b.WriteString("\t\tPageSize:  int(req.GetPageSize()),\n")
			b.WriteString("\t\tPageToken: req.GetPageToken(),\n")
			if m.ListHasFilter {
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
			fmt.Fprintf(b, "\treturn h.Repo.Update(ctx, req.Get%s().GetId(), req.Get%s(), req.GetUpdateMask()...)\n",
				m.ResourceField, m.ResourceField)
			b.WriteString("}\n\n")
		case stdDelete:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			b.WriteString("\tif err := h.Repo.Delete(ctx, key); err != nil {\n\t\treturn nil, err\n\t}\n")
			fmt.Fprintf(b, "\treturn &%s{}, nil\n", m.OutputGoIdent)
			b.WriteString("}\n\n")
		case stdUndelete:
			fmt.Fprintf(b, "func (h *%sCRUDHandler) %s(ctx context.Context, req *%s) (*%s, error) {\n",
				svc.ServiceName, m.Name, m.InputGoIdent, m.OutputGoIdent)
			renderKeyResolve(b, m, res)
			b.WriteString("\treturn h.Repo.Undelete(ctx, key)\n")
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

// renderHandlerConstructors emits New<Svc>Handler (returns the default handler so
// it can be embedded/wrapped) and Register<Svc>WithRepository (the one-call CRUD
// path: construct the default handler + Register<Svc>).
func renderHandlerConstructors(b *strings.Builder, svc serviceInfo) {
	res := svc.Resource
	fmt.Fprintf(b, "// New%sHandler returns the generated default CRUD handler backed by repo. Embed\n", svc.ServiceName)
	b.WriteString("// the returned type (or this struct) to override individual methods before\n")
	fmt.Fprintf(b, "// registering it via Register%s.\n", svc.ServiceName)
	fmt.Fprintf(b, "func New%sHandler(repo persistence.Repository[*%s, string]) *%sCRUDHandler {\n",
		svc.ServiceName, res, svc.ServiceName)
	fmt.Fprintf(b, "\treturn &%sCRUDHandler{Repo: repo}\n", svc.ServiceName)
	b.WriteString("}\n\n")

	fmt.Fprintf(b, "// Register%sWithRepository is the one-call CRUD path: it constructs the\n", svc.ServiceName)
	fmt.Fprintf(b, "// generated default handler over repo and registers it via Register%s\n", svc.ServiceName)
	b.WriteString("// (gRPC + REST gateway + authz rules). Use the New<Svc>Handler + Register<Svc>\n")
	b.WriteString("// pair instead when you need to wrap/override the default handler.\n")
	fmt.Fprintf(b, "func Register%sWithRepository(s *server.Server, repo persistence.Repository[*%s, string]) error {\n",
		svc.ServiceName, res)
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

	// Options: the hand-written parts. P1 carries the repository (required for the
	// CRUD Register path). Later phases extend this struct (health/events/jobs/
	// handler override) without changing the Module() contract.
	fmt.Fprintf(b, "// %sModuleOptions are the hand-written parts the generated %sModule needs that\n", svc.ServiceName, svc.ServiceName)
	b.WriteString("// the generator cannot derive from the proto. P1 carries the repository the\n")
	b.WriteString("// module's CRUD path registers over; later phases add custom health checks,\n")
	b.WriteString("// event handlers, background jobs, and handler overrides here.\n")
	fmt.Fprintf(b, "type %sModuleOptions struct {\n", svc.ServiceName)
	b.WriteString("\t// Repo is the persistence repository the module's generated CRUD handler\n")
	b.WriteString("\t// registers over (via Register" + svc.ServiceName + "WithRepository). Required.\n")
	fmt.Fprintf(b, "\tRepo persistence.Repository[*%s, string]\n", res)
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
	b.WriteString("\treturn servicekit.Descriptor{\n")
	fmt.Fprintf(b, "\t\tID: %q,\n", svc.moduleID())
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
	fmt.Fprintf(b, "\t\tResources: []servicekit.ResourceDescriptor{{Name: %q}},\n", svc.moduleID()+"."+snakeIdent(res))
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	// Register().
	fmt.Fprintf(b, "// Register implements servicekit.Module: wire %s onto the shared server via\n", svc.ServiceName)
	fmt.Fprintf(b, "// the existing Register%sWithRepository (gRPC + REST gateway + authz rules).\n", svc.ServiceName)
	fmt.Fprintf(b, "func (m *%s) Register(_ context.Context, app *servicekit.App) error {\n", typ)
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
