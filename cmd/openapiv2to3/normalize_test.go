package main

// Tests for the two gateway-v1 compat NORMALIZATIONS (WS-035): (a) invalid
// type/format sanitization and (b) duplicate path-parameter restoration /
// mechanical de-duplication. These use dedicated fixtures so the existing
// compat tests (gw1FDS / gw1Swagger) stay untouched.

import (
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// restoreFDS builds a small FDS with UNIQUE proto path-variable names, mirroring
// the athena/sso rules whose swagger flattening collapsed them to duplicates.
func restoreFDS(t *testing.T) *protoregistry.Files {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("group/v1/group.proto"),
		Package: proto.String("group.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			message("AddUserRequest", strField("group_id", 1), strField("user_id", 2)),
			message("AddUserResponse", strField("ok", 1)),
			message("RemoveUserResponse", strField("ok", 1)),
			message("GetItemRequest", strField("id", 1)),
			message("Item", strField("id", 1)),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("GroupService"),
			Method: []*descriptorpb.MethodDescriptorProto{
				method("AddUser", ".group.v1.AddUserRequest", ".group.v1.AddUserResponse",
					post("/groups/{group_id}/users/{user_id}", "")),
				method("RemoveUser", ".group.v1.AddUserRequest", ".group.v1.RemoveUserResponse",
					del("/groups/{group_id}/users/{user_id}")),
				method("GetItem", ".group.v1.GetItemRequest", ".group.v1.Item",
					get("/items/{id}")),
			},
		}},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{file},
	})
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	return files
}

// convertWith runs the compat enrichment against a caller-supplied FDS.
func convertWith(t *testing.T, files *protoregistry.Files, swagger string, opts compatOptions) (*openapi3.T, *coverageReport, error) {
	t.Helper()
	var doc2 openapi2.T
	if err := json.Unmarshal([]byte(swagger), &doc2); err != nil {
		t.Fatalf("parse v2 fixture: %v", err)
	}
	doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		t.Fatalf("ToV3: %v", err)
	}
	rep, cerr := enrichCompat(doc, files, opts)
	return doc, rep, cerr
}

// pathParamNames returns the in:path parameter names of (verb, path).
func pathParamNames(t *testing.T, doc *openapi3.T, verb, path string) []string {
	t.Helper()
	op := findOp(t, doc, verb, path)
	var names []string
	for _, pr := range op.Parameters {
		if pr.Value != nil && pr.Value.In == "path" {
			names = append(names, pr.Value.Name)
		}
	}
	return names
}

// TestRestoreDuplicateParamsFromRule: a swagger /groups/{id}/users/{id} whose
// two params both flattened to "id" is repaired from the matched google.api.http
// rule's unique names (group_id, user_id) — path key AND every operation's
// parameters, positionally. A non-duplicated matched path (/items/{thing}) is
// left completely untouched (proves the repair fires only on the defect).
func TestRestoreDuplicateParamsFromRule(t *testing.T) {
	const swagger = `{
	  "swagger": "2.0",
	  "info": {"title": "group", "version": "1.0"},
	  "paths": {
	    "/groups/{id}/users/{id}": {
	      "post": {
	        "operationId": "AddUser",
	        "parameters": [
	          {"name": "id", "in": "path", "required": true, "type": "string"},
	          {"name": "id", "in": "path", "required": true, "type": "string"}
	        ],
	        "responses": {"200": {"description": "ok"}}
	      },
	      "delete": {
	        "operationId": "RemoveUser",
	        "parameters": [
	          {"name": "id", "in": "path", "required": true, "type": "string"},
	          {"name": "id", "in": "path", "required": true, "type": "string"}
	        ],
	        "responses": {"200": {"description": "ok"}}
	      }
	    },
	    "/items/{thing}": {
	      "get": {
	        "operationId": "GetItem",
	        "parameters": [{"name": "thing", "in": "path", "required": true, "type": "string"}],
	        "responses": {"200": {"description": "ok"}}
	      }
	    }
	  },
	  "definitions": {}
	}`
	doc, rep, err := convertWith(t, restoreFDS(t), swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}

	const restored = "/groups/{group_id}/users/{user_id}"
	if doc.Paths.Value(restored) == nil {
		t.Fatalf("restored path %q not present; keys=%v", restored, doc.Paths.Keys())
	}
	if doc.Paths.Value("/groups/{id}/users/{id}") != nil {
		t.Error("original duplicate-param path key must be removed")
	}
	for _, verb := range []string{"post", "delete"} {
		if got := pathParamNames(t, doc, verb, restored); !equalStrs(got, []string{"group_id", "user_id"}) {
			t.Errorf("%s %s path params = %v, want [group_id user_id]", verb, restored, got)
		}
	}
	// Non-duplicated matched path untouched.
	if doc.Paths.Value("/items/{thing}") == nil {
		t.Error("non-duplicated matched path was rewritten but must be left untouched")
	}
	if got := pathParamNames(t, doc, "get", "/items/{thing}"); !equalStrs(got, []string{"thing"}) {
		t.Errorf("/items/{thing} params = %v, want [thing] (untouched)", got)
	}

	if rep.PathParams.Restored != 1 || rep.PathParams.Deduped != 0 {
		t.Errorf("pathParams coverage = %d restored / %d deduped, want 1/0", rep.PathParams.Restored, rep.PathParams.Deduped)
	}
	if len(rep.PathParams.Details) != 1 || rep.PathParams.Details[0].Kind != "restored" ||
		rep.PathParams.Details[0].To != restored {
		t.Errorf("pathParams details = %+v, want one restored -> %s", rep.PathParams.Details, restored)
	}
}

// TestMechanicalDedupFallback: a duplicate-param path with no matching proto
// rule (unmatched op) is de-duplicated mechanically ({id},{id2}) so the spec is
// still buildable, and the fix is report-visible.
func TestMechanicalDedupFallback(t *testing.T) {
	const swagger = `{
	  "swagger": "2.0",
	  "info": {"title": "orphan", "version": "1.0"},
	  "paths": {
	    "/foos/{id}/bars/{id}": {
	      "get": {
	        "operationId": "GetFooBar",
	        "parameters": [
	          {"name": "id", "in": "path", "required": true, "type": "string"},
	          {"name": "id", "in": "path", "required": true, "type": "string"}
	        ],
	        "responses": {"200": {"description": "ok"}}
	      }
	    }
	  },
	  "definitions": {}
	}`
	doc, rep, err := convertWith(t, restoreFDS(t), swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}

	const deduped = "/foos/{id}/bars/{id2}"
	if doc.Paths.Value(deduped) == nil {
		t.Fatalf("de-duplicated path %q not present; keys=%v", deduped, doc.Paths.Keys())
	}
	if got := pathParamNames(t, doc, "get", deduped); !equalStrs(got, []string{"id", "id2"}) {
		t.Errorf("path params = %v, want [id id2]", got)
	}
	if rep.PathParams.Restored != 0 || rep.PathParams.Deduped != 1 {
		t.Errorf("pathParams coverage = %d restored / %d deduped, want 0/1", rep.PathParams.Restored, rep.PathParams.Deduped)
	}
	if len(rep.PathParams.Details) != 1 || rep.PathParams.Details[0].Kind != "deduped" {
		t.Errorf("pathParams details = %+v, want one deduped", rep.PathParams.Details)
	}
}

// TestSanitizeInvalidFormat: `format: boolean` is dropped from a boolean schema
// PROPERTY and a boolean PARAMETER (type preserved), a valid format (int32) is
// kept, and the drops are counted.
func TestSanitizeInvalidFormat(t *testing.T) {
	const swagger = `{
	  "swagger": "2.0",
	  "info": {"title": "fmt", "version": "1.0"},
	  "paths": {
	    "/widgets": {
	      "get": {
	        "operationId": "ListWidgets",
	        "parameters": [
	          {"name": "generate_full_paths", "in": "query", "type": "boolean", "format": "boolean"}
	        ],
	        "responses": {"200": {"description": "ok"}}
	      }
	    }
	  },
	  "definitions": {
	    "Widget": {"type": "object", "properties": {
	      "enabled": {"type": "boolean", "format": "boolean"},
	      "count": {"type": "integer", "format": "int32"}
	    }}
	  }
	}`
	doc, rep, err := compatConvert(t, swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}

	prop := doc.Components.Schemas["Widget"].Value.Properties["enabled"].Value
	if prop.Format != "" {
		t.Errorf("property enabled format = %q, want dropped", prop.Format)
	}
	if prop.Type == nil || !prop.Type.Is("boolean") {
		t.Errorf("property enabled type = %v, want boolean preserved", prop.Type)
	}
	if count := doc.Components.Schemas["Widget"].Value.Properties["count"].Value; count.Format != "int32" {
		t.Errorf("property count format = %q, want int32 preserved (valid)", count.Format)
	}

	param := findOp(t, doc, "get", "/widgets").Parameters[0].Value
	if param.Name != "generate_full_paths" || param.Schema == nil || param.Schema.Value.Format != "" {
		t.Errorf("param schema format not dropped: %+v", param.Schema.Value)
	}
	if !param.Schema.Value.Type.Is("boolean") {
		t.Errorf("param type = %v, want boolean preserved", param.Schema.Value.Type)
	}

	if rep.FormatsSanitized != 2 {
		t.Errorf("formatsSanitized = %d, want 2 (property + parameter)", rep.FormatsSanitized)
	}
}

// TestSanitizeFormatUnitTable exercises the format helpers directly.
func TestSanitizeFormatUnitTable(t *testing.T) {
	if !badFormatsByType["boolean"]["boolean"] {
		t.Error("boolean/boolean must be in the known-bad table")
	}
	if badFormatsByType["string"]["int64"] {
		t.Error("string/int64 is a legitimate convention, must not be sanitized")
	}
}

// TestTemplateVarNamesAndRewrite covers the template helpers, including the
// deep/multi-segment leaf-name rule ({url=**} -> url).
func TestTemplateVarNamesAndRewrite(t *testing.T) {
	for tmpl, want := range map[string][]string{
		"/groups/{id}/users/{id}":  {"id", "id"},
		"/session/verify/{url=**}": {"url"},
		"/a/{x.y.z}/b/{w}":         {"x.y.z", "w"},
		"/no/vars":                 nil,
		"/managed_accounts/{account_id}/address/{addr}": {"account_id", "addr"},
	} {
		got := templateVarNames(tmpl)
		if !equalStrs(got, want) {
			t.Errorf("templateVarNames(%q) = %v, want %v", tmpl, got, want)
		}
	}
	if got := rewriteTemplate("/groups/{id}/users/{id}", []string{"group_id", "user_id"}); got != "/groups/{group_id}/users/{user_id}" {
		t.Errorf("rewriteTemplate = %q", got)
	}
	if got := mechanicalDedup([]string{"id", "id", "id"}); !equalStrs(got, []string{"id", "id2", "id3"}) {
		t.Errorf("mechanicalDedup = %v", got)
	}
}
