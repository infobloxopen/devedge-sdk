package main

import (
	"go/format"
	"strings"
	"testing"
)

// crudService returns a single-resource CRUD service shaped like the apikey/
// scaffold fixtures: Create/Get/List/Update/Delete over a soft-delete resource
// keyed by id, with the resource request field named differently from the Go
// type (ApiKey vs APIKey) to exercise the field-name resolution.
func crudService() serviceInfo {
	return serviceInfo{
		ServiceName:        "APIKeyService",
		Resource:           "APIKey",
		ResourceSoftDelete: true,
		Methods: []methodInfo{
			{Name: "CreateAPIKey", InputGoIdent: "CreateAPIKeyRequest", OutputGoIdent: "APIKey", Std: stdCreate, ResourceField: "ApiKey"},
			{Name: "GetAPIKey", InputGoIdent: "GetAPIKeyRequest", OutputGoIdent: "APIKey", Std: stdGet},
			{Name: "ListAPIKeys", InputGoIdent: "ListAPIKeysRequest", OutputGoIdent: "ListAPIKeysResponse", Std: stdList, ListItemsField: "ApiKeys", ListHasShowDeleted: true},
			{Name: "UpdateAPIKey", InputGoIdent: "UpdateAPIKeyRequest", OutputGoIdent: "APIKey", Std: stdUpdate, ResourceField: "ApiKey"},
			{Name: "DeleteAPIKey", InputGoIdent: "DeleteAPIKeyRequest", OutputGoIdent: "DeleteAPIKeyResponse", Std: stdDelete},
		},
	}
}

// TestRenderSvcFile_validGo gates that the generated file is syntactically valid
// Go (go/format.Source) — the render-drift guard from the spec's failure modes.
func TestRenderSvcFile_validGo(t *testing.T) {
	out := renderSvcFile("apikeyv1", "github.com/example/apikey/v1;apikeyv1", []serviceInfo{crudService()})
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated output is not valid Go: %v\n--- output ---\n%s", err, out)
	}
}

// enumService is the CRUD service with an allowed_values (BC-08) string-backed
// enum field `status` on its resource.
func enumService() serviceInfo {
	s := crudService()
	s.EnumFields = []enumField{
		{Getter: "GetStatus", ProtoName: "status", Allowed: []string{"ACTIVE", "CHECKED_OUT", "ABANDONED"}},
	}
	return s
}

// BC-08: a resource carrying allowed_values gets a generated validate<Resource>
// that Create and Update call before persistence; the output stays valid Go.
func TestRenderSvcFile_enumValidation(t *testing.T) {
	out := renderSvcFile("apikeyv1", "x;apikeyv1", []serviceInfo{enumService()})
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated output is not valid Go: %v\n--- output ---\n%s", err, out)
	}
	mustContain(t, out, "func validateAPIKey(m *APIKey) error {")
	mustContain(t, out, `case "ACTIVE", "CHECKED_OUT", "ABANDONED":`)
	mustContain(t, out, "status.Errorf(codes.InvalidArgument")
	// Create and Update call the validator before delegating to the repository.
	mustContain(t, out, "if err := validateAPIKey(req.GetApiKey()); err != nil {")
	// status/codes are imported because the validator needs them.
	mustContain(t, out, `"google.golang.org/grpc/status"`)
}

// A resource WITHOUT allowed_values generates no validator and no call.
func TestRenderSvcFile_noEnumValidation(t *testing.T) {
	out := renderSvcFile("apikeyv1", "x;apikeyv1", []serviceInfo{crudService()})
	mustNotContain(t, out, "func validateAPIKey")
	mustNotContain(t, out, "if err := validateAPIKey")
}

func TestRenderSvcFile_register(t *testing.T) {
	out := renderSvcFile("apikeyv1", "x;apikeyv1", []serviceInfo{crudService()})

	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package apikeyv1")
	mustContain(t, out, "protoc-gen-svc")

	// protoc-gen-svc no longer re-declares the server interface or unimplemented
	// stub — those are provided by protoc-gen-go-grpc (_grpc.pb.go).
	mustNotContain(t, out, "type APIKeyServiceServer interface")
	mustNotContain(t, out, "type UnimplementedAPIKeyServiceServer struct")

	// Register<Svc> records methods + contributes rules; the completeness gate
	// moved to server.Serve (no per-Register AssertMethodsDeclared).
	mustContain(t, out, "func RegisterAPIKeyService(s *server.Server, srv APIKeyServiceServer) error")
	mustContain(t, out, "s.RecordMethods(")
	mustContain(t, out, "s.AddRules(APIKeyServiceAuthzRules...)")
	mustNotContain(t, out, "AssertMethodsDeclared")
	mustContain(t, out, "APIKeyService_CreateAPIKey_FullMethodName")
	mustContain(t, out, "RegisterAPIKeyServiceServer(s.GRPCServer(), srv)")
	mustContain(t, out, "RegisterAPIKeyServiceHandlerClient(ctx, mux, NewAPIKeyServiceClient(conn))")
}

// TestRenderSvcFile_crudHandler verifies the generated default handler delegates
// each standard method to the repository with the right shape (D-1/D-2/D-4).
func TestRenderSvcFile_crudHandler(t *testing.T) {
	out := renderSvcFile("apikeyv1", "x;apikeyv1", []serviceInfo{crudService()})

	// Struct: embeds the unimplemented stub (custom RPCs stay Unimplemented) and
	// holds the typed repository.
	mustContain(t, out, "type APIKeyServiceCRUDHandler struct {")
	mustContain(t, out, "UnimplementedAPIKeyServiceServer")
	mustContain(t, out, "Repo persistence.Repository[*APIKey, string]")

	// Create delegates to repo.Create using the request's resource accessor
	// (ApiKey, not APIKey — proves field-name resolution).
	mustContain(t, out, "func (h *APIKeyServiceCRUDHandler) CreateAPIKey(ctx context.Context, req *CreateAPIKeyRequest) (*APIKey, error)")
	mustContain(t, out, "return h.Repo.Create(ctx, req.GetApiKey())")

	// Get keyed by id.
	mustContain(t, out, "key := req.GetId()")
	mustContain(t, out, "return h.Repo.Get(ctx, key)")

	// List maps paging + show_deleted (present) but NOT filter/order_by (absent).
	mustContain(t, out, "h.Repo.List(ctx, persistence.ListOptions{")
	mustContain(t, out, "PageSize:  int(req.GetPageSize())")
	mustContain(t, out, "ShowDeleted: req.GetShowDeleted()")
	mustNotContain(t, out, "Filter:")
	mustNotContain(t, out, "OrderBy:")
	mustContain(t, out, "return &ListAPIKeysResponse{ApiKeys: items, NextPageToken: next}, nil")

	// Update delegates with the resource id + update_mask.
	mustContain(t, out, "return h.Repo.Update(ctx, req.GetApiKey().GetId(), req.GetApiKey(), req.GetUpdateMask()...)")

	// Delete delegates and returns the delete response.
	mustContain(t, out, "if err := h.Repo.Delete(ctx, key); err != nil {")
	mustContain(t, out, "return &DeleteAPIKeyResponse{}, nil")

	// Constructors: New<Svc>Handler + Register<Svc>WithRepository.
	mustContain(t, out, "func NewAPIKeyServiceHandler(repo persistence.Repository[*APIKey, string]) *APIKeyServiceCRUDHandler")
	mustContain(t, out, "return &APIKeyServiceCRUDHandler{Repo: repo}")
	mustContain(t, out, "func RegisterAPIKeyServiceWithRepository(s *server.Server, repo persistence.Repository[*APIKey, string]) error")
	mustContain(t, out, "return RegisterAPIKeyService(s, NewAPIKeyServiceHandler(repo))")
}

// TestRenderSvcFile_nameKeyed verifies a Get/Delete keyed by an AIP-122 name
// (not id) parses the name via Parse<R>Name (D-2).
func TestRenderSvcFile_nameKeyed(t *testing.T) {
	svc := serviceInfo{
		ServiceName: "BookService",
		Resource:    "Book",
		Methods: []methodInfo{
			{Name: "GetBook", InputGoIdent: "GetBookRequest", OutputGoIdent: "Book", Std: stdGet, KeyByName: true},
		},
	}
	out := renderSvcFile("bookv1", "x;bookv1", []serviceInfo{svc})
	mustContain(t, out, "key, err := ParseBookName(req.GetName())")
	mustContain(t, out, "return h.Repo.Get(ctx, key)")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("name-keyed output not valid Go: %v\n%s", err, out)
	}
}

// TestRenderSvcFile_undelete verifies Undelete is generated only for soft-delete
// resources and delegates to repo.Undelete.
func TestRenderSvcFile_undelete(t *testing.T) {
	svc := serviceInfo{
		ServiceName:        "DocService",
		Resource:           "Doc",
		ResourceSoftDelete: true,
		Methods: []methodInfo{
			{Name: "UndeleteDoc", InputGoIdent: "UndeleteDocRequest", OutputGoIdent: "Doc", Std: stdUndelete},
		},
	}
	out := renderSvcFile("docv1", "x;docv1", []serviceInfo{svc})
	mustContain(t, out, "func (h *DocServiceCRUDHandler) UndeleteDoc(")
	mustContain(t, out, "return h.Repo.Undelete(ctx, key)")
}

// TestRenderSvcFile_customRPCUnimplemented verifies a service whose only RPC is
// custom (no standard shape) gets Register<Svc> but NO CRUD handler — the custom
// RPC is left to the developer (via the Unimplemented embed / escape hatch).
func TestRenderSvcFile_customRPCUnimplemented(t *testing.T) {
	svc := serviceInfo{
		ServiceName: "ReportService",
		Resource:    "", // no resource detected
		Methods: []methodInfo{
			{Name: "GenerateReport", InputGoIdent: "GenerateReportRequest", OutputGoIdent: "GenerateReportResponse", Std: stdNone},
		},
	}
	out := renderSvcFile("reportv1", "x;reportv1", []serviceInfo{svc})
	mustContain(t, out, "func RegisterReportService(s *server.Server, srv ReportServiceServer) error")
	mustNotContain(t, out, "ReportServiceCRUDHandler")
	mustNotContain(t, out, "RegisterReportServiceWithRepository")
	// No persistence import when no service has a CRUD handler.
	mustNotContain(t, out, "devedge-sdk/persistence")
	// No servicekit Module for a custom-only service (its Register has no
	// Register<Svc>WithRepository to wrap).
	mustNotContain(t, out, "devedge-sdk/servicekit")
	mustNotContain(t, out, "func ReportServiceModule(")
}

// TestRenderSvcFile_module verifies the WS-012 P1 servicekit.Module surface: a
// CRUD service gets <Svc>ModuleOptions, a <Svc>Module(opts) constructor, and an
// impl whose Descriptor reports the proto facts and whose Register wraps the
// existing Register<Svc>WithRepository. The generated Module does NOT replace
// Register<Svc> / Register<Svc>WithRepository.
func TestRenderSvcFile_module(t *testing.T) {
	svc := crudService()
	svc.ProtoPackage = "apikey.v1"
	out := renderSvcFile("apikeyv1", "x;apikeyv1", []serviceInfo{svc})

	// servicekit import + Options struct.
	mustContain(t, out, "github.com/infobloxopen/devedge-sdk/servicekit")
	mustContain(t, out, "type APIKeyServiceModuleOptions struct {")
	mustContain(t, out, "Repo persistence.Repository[*APIKey, string]")
	// BC-09 (#139): the override seam — an optional Handler that lets a service
	// with custom / non-CRUD methods grow in place instead of abandoning the
	// generated module.
	mustContain(t, out, "Handler APIKeyServiceServer")

	// Constructor returns a servicekit.Module.
	mustContain(t, out, "func APIKeyServiceModule(opts APIKeyServiceModuleOptions) servicekit.Module {")

	// Descriptor: module ID from the proto package's first segment; methods from
	// the FullMethod constants; AuthzRules referencing the generated table;
	// module-qualified resource name (snake-cased).
	mustContain(t, out, "func (m *aPIKeyServiceModule) Descriptor() servicekit.Descriptor {")
	mustContain(t, out, `ID: "apikey"`)
	mustContain(t, out, "APIKeyService_CreateAPIKey_FullMethodName,")
	mustContain(t, out, "AuthzRules: APIKeyServiceAuthzRules,")
	mustContain(t, out, `Resources: []servicekit.ResourceDescriptor{{Name: "apikey.api_key"}}`)

	// Register wraps the existing WithRepository over the shared server — it does
	// NOT reimplement registration. The override seam: Handler wins when set,
	// otherwise the default CRUD path; neither set fails closed.
	mustContain(t, out, "func (m *aPIKeyServiceModule) Register(_ context.Context, app *servicekit.App) error {")
	mustContain(t, out, "if m.opts.Handler != nil {")
	mustContain(t, out, "return RegisterAPIKeyService(app.Server, m.opts.Handler)")
	mustContain(t, out, "return RegisterAPIKeyServiceWithRepository(app.Server, m.opts.Repo)")

	// The Module wraps, not replaces: the existing entry points are still present.
	mustContain(t, out, "func RegisterAPIKeyService(s *server.Server, srv APIKeyServiceServer) error")
	mustContain(t, out, "func RegisterAPIKeyServiceWithRepository(s *server.Server, repo persistence.Repository[*APIKey, string]) error")

	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("module output not valid Go: %v\n--- output ---\n%s", err, out)
	}
}

// TestSnakeIdent covers the resource-name snake conversion used in the module's
// module-qualified resource descriptor (acronym handling matters: APIKey).
func TestSnakeIdent(t *testing.T) {
	cases := map[string]string{
		"Order":  "order",
		"APIKey": "api_key",
		"Book":   "book",
		"IAMRole": "iam_role",
	}
	for in, want := range cases {
		if got := snakeIdent(in); got != want {
			t.Errorf("snakeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderSvcFile_mixedStdAndCustom verifies a service with standard methods
// AND a custom method generates the handler for the standard methods only — the
// custom method is left Unimplemented (AC-3).
func TestRenderSvcFile_mixedStdAndCustom(t *testing.T) {
	svc := serviceInfo{
		ServiceName: "WidgetService",
		Resource:    "Widget",
		Methods: []methodInfo{
			{Name: "CreateWidget", InputGoIdent: "CreateWidgetRequest", OutputGoIdent: "Widget", Std: stdCreate, ResourceField: "Widget"},
			{Name: "GetWidget", InputGoIdent: "GetWidgetRequest", OutputGoIdent: "Widget", Std: stdGet},
			{Name: "ArchiveWidget", InputGoIdent: "ArchiveWidgetRequest", OutputGoIdent: "ArchiveWidgetResponse", Std: stdNone},
		},
	}
	out := renderSvcFile("widgetsv1", "x;widgetsv1", []serviceInfo{svc})
	mustContain(t, out, "func (h *WidgetServiceCRUDHandler) CreateWidget(")
	mustContain(t, out, "func (h *WidgetServiceCRUDHandler) GetWidget(")
	// The custom RPC has no generated method — it stays Unimplemented via the embed.
	mustNotContain(t, out, "func (h *WidgetServiceCRUDHandler) ArchiveWidget(")
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("mixed output not valid Go: %v\n%s", err, out)
	}
}

func TestRenderSvcFile_noServices(t *testing.T) {
	out := renderSvcFile("emptypkg", "example/emptypkg;emptypkg", nil)
	if out != "" {
		t.Fatalf("expected empty output for no services, got:\n%s", out)
	}
}

func TestRenderSvcFile_emptyMethodList(t *testing.T) {
	svc := serviceInfo{ServiceName: "EmptyService", Methods: nil}
	out := renderSvcFile("pkg", "example/pkg;pkg", []serviceInfo{svc})
	// A service with no methods still emits a Register helper (no CRUD handler).
	mustContain(t, out, "func RegisterEmptyService(s *server.Server, srv EmptyServiceServer) error")
	mustContain(t, out, "RegisterEmptyServiceServer(s.GRPCServer(), srv)")
	mustNotContain(t, out, "EmptyServiceCRUDHandler")
}

// memberService is a DDD aggregate MEMBER service (Item owned by Order): it
// declares the standard methods but, being a member, its writes must be redirected
// to Unimplemented and a member binding contributed for the boundary gate.
func memberService() serviceInfo {
	return serviceInfo{
		ServiceName: "ItemService",
		Resource:    "Item",
		MemberRoot:  "Order",
		Methods: []methodInfo{
			{Name: "CreateItem", InputGoIdent: "CreateItemRequest", OutputGoIdent: "Item", Std: stdCreate, ResourceField: "Item"},
			{Name: "UpdateItem", InputGoIdent: "UpdateItemRequest", OutputGoIdent: "Item", Std: stdUpdate, ResourceField: "Item"},
			{Name: "GetItem", InputGoIdent: "GetItemRequest", OutputGoIdent: "Item", Std: stdGet},
			{Name: "ListItems", InputGoIdent: "ListItemsRequest", OutputGoIdent: "ListItemsResponse", Std: stdList, ListItemsField: "Items"},
		},
	}
}

// TestRenderSvcFile_memberWriteRedirection is F031 T6/G-4 + AC-2 (codegen half): a
// member resource's write methods are emitted as gRPC Unimplemented (route through
// the root), Get/List still delegate to the repo, and Register<Svc> contributes a
// server.MemberBinding listing the registered write methods for the boundary gate.
func TestRenderSvcFile_memberWriteRedirection(t *testing.T) {
	out := renderSvcFile("orderv1", "x;orderv1", []serviceInfo{memberService()})
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated output is not valid Go: %v\n--- output ---\n%s", err, out)
	}
	// Writes redirected to Unimplemented (not delegated to Repo).
	mustContain(t, out, "func (h *ItemServiceCRUDHandler) CreateItem(")
	mustContain(t, out, "status.Errorf(codes.Unimplemented")
	mustContain(t, out, "func (h *ItemServiceCRUDHandler) UpdateItem(")
	// Reads still delegate to the repo (addressable for reads).
	mustContain(t, out, "h.Repo.Get(ctx, key)")
	mustContain(t, out, "h.Repo.List(ctx, persistence.ListOptions{")
	// The member never calls Repo.Create/Update — those route through the root.
	mustNotContain(t, out, "h.Repo.Create(ctx,")
	mustNotContain(t, out, "h.Repo.Update(ctx,")
	// A member binding is contributed with the write methods for the boundary gate.
	mustContain(t, out, "s.RecordMemberBinding(server.MemberBinding{")
	mustContain(t, out, `Resource: "Item",`)
	mustContain(t, out, `Root:     "Order",`)
	mustContain(t, out, "ItemService_CreateItem_FullMethodName,")
	mustContain(t, out, "ItemService_UpdateItem_FullMethodName,")
}

// memberServiceWithBatch is a DDD member service (Item owned by Order) that ALSO
// exposes AIP-137 batch methods. classifyMethod does not assign a stdMethod to
// batch RPCs, so without explicit batch-write handling a member's BatchCreate/
// BatchUpdate/BatchDelete would be neither redirected to Unimplemented nor recorded
// in the boundary-gate WriteMethods — a fail-open hole. BatchGet is a read and must
// stay addressable.
func memberServiceWithBatch() serviceInfo {
	return serviceInfo{
		ServiceName: "ItemService",
		Resource:    "Item",
		MemberRoot:  "Order",
		Methods: []methodInfo{
			{Name: "GetItem", InputGoIdent: "GetItemRequest", OutputGoIdent: "Item", Std: stdGet},
			{Name: "ListItems", InputGoIdent: "ListItemsRequest", OutputGoIdent: "ListItemsResponse", Std: stdList, ListItemsField: "Items"},
			// Batch RPCs: classifier leaves Std == stdNone for these.
			{Name: "BatchCreateItems", InputGoIdent: "BatchCreateItemsRequest", OutputGoIdent: "BatchCreateItemsResponse", Std: stdNone},
			{Name: "BatchUpdateItems", InputGoIdent: "BatchUpdateItemsRequest", OutputGoIdent: "BatchUpdateItemsResponse", Std: stdNone},
			{Name: "BatchDeleteItems", InputGoIdent: "BatchDeleteItemsRequest", OutputGoIdent: "BatchDeleteItemsResponse", Std: stdNone},
			{Name: "BatchGetItems", InputGoIdent: "BatchGetItemsRequest", OutputGoIdent: "BatchGetItemsResponse", Std: stdNone},
		},
	}
}

// TestRenderSvcFile_memberBatchWriteRedirection guards the F031 boundary-gate
// fail-open hole for a member's AIP-137 batch writes: a member's BatchCreate/
// BatchUpdate/BatchDelete must be redirected to Unimplemented AND recorded in the
// member binding's WriteMethods (so the boot-time boundary gate fails closed if any
// is registered). BatchGet stays addressable.
func TestRenderSvcFile_memberBatchWriteRedirection(t *testing.T) {
	out := renderSvcFile("orderv1", "x;orderv1", []serviceInfo{memberServiceWithBatch()})
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated output is not valid Go: %v\n--- output ---\n%s", err, out)
	}
	// Batch writes redirected to Unimplemented (not delegated to a repo).
	mustContain(t, out, "func (h *ItemServiceCRUDHandler) BatchCreateItems(")
	mustContain(t, out, "func (h *ItemServiceCRUDHandler) BatchUpdateItems(")
	mustContain(t, out, "func (h *ItemServiceCRUDHandler) BatchDeleteItems(")
	// Batch writes recorded in the boundary-gate WriteMethods.
	mustContain(t, out, "ItemService_BatchCreateItems_FullMethodName,")
	mustContain(t, out, "ItemService_BatchUpdateItems_FullMethodName,")
	mustContain(t, out, "ItemService_BatchDeleteItems_FullMethodName,")
	// BatchGet is a READ — never redirected to Unimplemented as a write...
	mustNotContain(t, out, "func (h *ItemServiceCRUDHandler) BatchGetItems(")
	// ...and never recorded in the member binding's WriteMethods (it is still in
	// RecordMethods like every RPC, so we scope the check to the WriteMethods block).
	bindingStart := strings.Index(out, "RecordMemberBinding")
	if bindingStart < 0 {
		t.Fatal("expected a RecordMemberBinding for the member service")
	}
	binding := out[bindingStart:]
	if end := strings.Index(binding, "})"); end >= 0 {
		binding = binding[:end]
	}
	if strings.Contains(binding, "BatchGetItems") {
		t.Errorf("BatchGet (a read) must not be in the member binding WriteMethods:\n%s", binding)
	}
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", substr, s)
	}
}

func mustNotContain(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected output NOT to contain %q\n--- output ---\n%s", substr, s)
	}
}
