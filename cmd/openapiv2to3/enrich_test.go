package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/infobloxopen/devedge-sdk/internal/aip"
)

func toyPaths(t *testing.T) (fds, swagger string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "toy")
	return filepath.Join(root, "toy.binpb"), filepath.Join(root, "toy.swagger.json")
}

// enrichedToyDoc converts the toy swagger to v3 and runs the enrichment pass,
// returning the in-memory document (the exact object serialized to the golden).
func enrichedToyDoc(t *testing.T) *openapi3.T {
	t.Helper()
	fdsPath, swaggerPath := toyPaths(t)
	if _, err := os.Stat(fdsPath); err != nil {
		t.Skipf("toy.binpb absent (%v) — run 'make generate' first", err)
	}
	files, err := loadDescriptors(fdsPath)
	if err != nil {
		t.Fatalf("loadDescriptors: %v", err)
	}
	data, err := os.ReadFile(swaggerPath)
	if err != nil {
		t.Fatalf("read swagger: %v", err)
	}
	var doc2 openapi2.T
	if err := json.Unmarshal(data, &doc2); err != nil {
		t.Fatalf("parse v2: %v", err)
	}
	doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		t.Fatalf("ToV3: %v", err)
	}
	if err := enrich(doc, files); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	return doc
}

// TestEnrichToyContract asserts the enrichment writes the authoritative
// proto-derived semantics onto the toy Widget schema and operations (AC-1..AC-5).
func TestEnrichToyContract(t *testing.T) {
	doc := enrichedToyDoc(t)
	w := doc.Components.Schemas["v1Widget"]
	if w == nil || w.Value == nil {
		t.Fatal("v1Widget schema missing")
	}
	props := w.Value.Properties

	// AC-3: secret_token is writeOnly + x-aip-field-behavior [INPUT_ONLY].
	if !props["secretToken"].Value.WriteOnly {
		t.Error("secretToken must be writeOnly")
	}
	assertBehavior(t, props["secretToken"].Value, "INPUT_ONLY")
	// OUTPUT_ONLY fields readOnly + x-aip-field-behavior [OUTPUT_ONLY].
	if !props["name"].Value.ReadOnly {
		t.Error("name must be readOnly")
	}
	assertBehavior(t, props["name"].Value, "OUTPUT_ONLY")
	// AC-4: category carries enum from allowed_values.
	if got := props["category"].Value.Enum; len(got) != 2 || got[0] != "standard" || got[1] != "premium" {
		t.Errorf("category enum = %v, want [standard premium]", got)
	}
	// AC-1: displayName (explicit REQUIRED) is in required; sku (not_null) is NOT.
	if !contains(w.Value.Required, "displayName") {
		t.Errorf("required must contain displayName, got %v", w.Value.Required)
	}
	if contains(w.Value.Required, "sku") {
		t.Errorf("required must NOT contain sku (not_null ≠ REQUIRED), got %v", w.Value.Required)
	}
	// AC-2: id (USER_SETTABLE, derived) and color (explicit) both carry IMMUTABLE.
	assertBehavior(t, props["id"].Value, "IMMUTABLE")
	assertBehavior(t, props["color"].Value, "IMMUTABLE")
	// AC-5: parentId carries x-aip-references to the target type.
	refs, ok := props["parentId"].Value.Extensions["x-aip-references"].(map[string]any)
	if !ok || refs["type"] != "toy.example.com/Widget" {
		t.Errorf("parentId x-aip-references = %v, want {type: toy.example.com/Widget}", props["parentId"].Value.Extensions["x-aip-references"])
	}
	// AC-5: schema carries x-aip-resource with type + key.
	res, ok := w.Value.Extensions["x-aip-resource"].(map[string]any)
	if !ok || res["type"] != "toy.example.com/Widget" || res["key"] != "id" {
		t.Errorf("v1Widget x-aip-resource = %v", w.Value.Extensions["x-aip-resource"])
	}

	// AC-5: operations carry x-aip-method; List carries x-aip-pagination.
	methods := map[string]string{}
	var listOp *openapi3.Operation
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.OperationID == "" {
				continue
			}
			if m, ok := op.Extensions["x-aip-method"].(string); ok {
				methods[op.OperationID] = m
			}
			if op.OperationID == "WidgetService_ListWidgets" {
				listOp = op
			}
		}
	}
	for opID, want := range map[string]string{
		"WidgetService_CreateWidget":    "Create",
		"WidgetService_GetWidget":       "Get",
		"WidgetService_ListWidgets":     "List",
		"WidgetService_UpdateWidget":    "Update",
		"WidgetService_DeleteWidget":    "Delete",
		"WidgetService_BatchGetWidgets": "BatchGet",
		"WidgetService_ArchiveWidget":   "Custom",
	} {
		if methods[opID] != want {
			t.Errorf("x-aip-method[%s] = %q, want %q", opID, methods[opID], want)
		}
	}
	if listOp == nil {
		t.Fatal("ListWidgets operation not found")
	}
	pg, ok := listOp.Extensions["x-aip-pagination"].(map[string]any)
	if !ok || pg["pageSizeParam"] != "pageSize" || pg["nextPageTokenField"] != "nextPageToken" {
		t.Errorf("ListWidgets x-aip-pagination = %v", listOp.Extensions["x-aip-pagination"])
	}
}

// TestClassifierParity_Toy feeds the toy FDS to the shared classifier (the same
// aip path the codegen plugins use) and asserts the AIP classification matches
// what the generated widgets.svc.go implements — AC-6 / FM-5, guarding against a
// divergent classifier being reintroduced in either path.
func TestClassifierParity_Toy(t *testing.T) {
	fdsPath, _ := toyPaths(t)
	if _, err := os.Stat(fdsPath); err != nil {
		t.Skipf("toy.binpb absent (%v)", err)
	}
	files, err := loadDescriptors(fdsPath)
	if err != nil {
		t.Fatalf("loadDescriptors: %v", err)
	}
	want := map[string]string{
		"CreateWidget": "Create", "GetWidget": "Get", "ListWidgets": "List",
		"UpdateWidget": "Update", "DeleteWidget": "Delete", "BatchGetWidgets": "BatchGet",
		"ArchiveWidget": "Custom", "BatchDeleteWidgets": "Custom", "BatchUpdateWidgets": "Custom",
		"ProcessWidget": "Custom", "GetOperationStatus": "Custom", "CancelWidgetOperation": "Custom",
	}
	var found bool
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			sd := svcs.Get(i)
			if sd.Name() != "WidgetService" {
				continue
			}
			found = true
			res := aip.DetectServiceResource(sd)
			if res == nil || res.Name() != "Widget" {
				t.Fatalf("DetectServiceResource = %v, want Widget", res)
			}
			softDelete := aip.MessageFacts(res).SoftDelete
			ms := sd.Methods()
			for j := 0; j < ms.Len(); j++ {
				md := ms.Get(j)
				got := aip.ClassifyMethod(md, res, softDelete).String()
				if got != want[string(md.Name())] {
					t.Errorf("ClassifyMethod(%s) = %s, want %s", md.Name(), got, want[string(md.Name())])
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("WidgetService not found in toy FDS")
	}
}

// TestRun_MissingFDS asserts FM-2: without a readable FDS the tool exits non-zero
// and never writes an un-enriched spec.
func TestRun_MissingFDS(t *testing.T) {
	_, swaggerPath := toyPaths(t)
	outDir := t.TempDir()

	// No -descriptor flag → hard error.
	if code := run([]string{swaggerPath, outDir}); code == 0 {
		t.Error("run without -descriptor: want non-zero exit")
	}
	// Descriptor path that does not exist → hard error.
	if code := run([]string{"-descriptor", filepath.Join(t.TempDir(), "nope.binpb"), swaggerPath, outDir}); code == 0 {
		t.Error("run with missing descriptor file: want non-zero exit")
	}
	// It must not have written any output.
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Errorf("expected no output written on failure, found %d entries", len(entries))
	}
}

func assertBehavior(t *testing.T, s *openapi3.Schema, want string) {
	t.Helper()
	raw, ok := s.Extensions["x-aip-field-behavior"]
	if !ok {
		t.Errorf("missing x-aip-field-behavior (want %s)", want)
		return
	}
	list, ok := raw.([]string)
	if !ok {
		t.Errorf("x-aip-field-behavior is %T, want []string", raw)
		return
	}
	for _, b := range list {
		if b == want {
			return
		}
	}
	t.Errorf("x-aip-field-behavior = %v, want to contain %s", list, want)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
