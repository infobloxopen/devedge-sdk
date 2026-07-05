package main

// Tests for -compat=gateway-v1 (WS-035): a synthetic FileDescriptorSet built
// in code (two services, google.api.http rules incl. an additional_binding and
// path variables) plus swagger 2.0 documents mimicking protoc-gen-swagger /
// atlas output: legacy operationIds, snake_case properties, and a patched
// basePath (`/api/thing/v1` vs proto rules under `/thing/v1/...`).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// --- synthetic gateway-v1 fixture: FileDescriptorSet built in code ---

func strField(name string, num int32, behaviors ...apiannotations.FieldBehavior) *descriptorpb.FieldDescriptorProto {
	f := &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
	if len(behaviors) > 0 {
		opts := &descriptorpb.FieldOptions{}
		proto.SetExtension(opts, apiannotations.E_FieldBehavior, behaviors)
		f.Options = opts
	}
	return f
}

func msgField(name string, num int32, typeName string, repeated bool) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(num),
		Label:    label.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(typeName),
	}
}

func message(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
}

func method(name, in, out string, rule *apiannotations.HttpRule) *descriptorpb.MethodDescriptorProto {
	opts := &descriptorpb.MethodOptions{}
	proto.SetExtension(opts, apiannotations.E_Http, rule)
	return &descriptorpb.MethodDescriptorProto{
		Name:       proto.String(name),
		InputType:  proto.String(in),
		OutputType: proto.String(out),
		Options:    opts,
	}
}

func get(path string) *apiannotations.HttpRule {
	return &apiannotations.HttpRule{Pattern: &apiannotations.HttpRule_Get{Get: path}}
}

func del(path string) *apiannotations.HttpRule {
	return &apiannotations.HttpRule{Pattern: &apiannotations.HttpRule_Delete{Delete: path}}
}

func post(path, body string, additional ...*apiannotations.HttpRule) *apiannotations.HttpRule {
	return &apiannotations.HttpRule{
		Pattern:            &apiannotations.HttpRule_Post{Post: path},
		Body:               body,
		AdditionalBindings: additional,
	}
}

// gw1FDS builds the two-service FileDescriptorSet the fixture swagger docs are
// matched against.
func gw1FDS(t *testing.T) (*protoregistry.Files, *descriptorpb.FileDescriptorSet) {
	t.Helper()

	thingFile := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("thing/v1/thing.proto"),
		Package: proto.String("thing.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			message("Thing",
				strField("id", 1),
				strField("display_name", 2, apiannotations.FieldBehavior_REQUIRED),
				strField("created_at", 3, apiannotations.FieldBehavior_OUTPUT_ONLY),
			),
			message("Metadata", strField("value", 1)),
			message("TagSet", strField("tags", 1)),
			message("Dup", strField("x", 1)),
			message("ListThingsRequest"),
			message("ListThingsResponse", msgField("things", 1, ".thing.v1.Thing", true)),
			message("GetThingRequest", strField("id", 1)),
			message("CreateThingRequest", msgField("thing", 1, ".thing.v1.Thing", false)),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("ThingService"),
			Method: []*descriptorpb.MethodDescriptorProto{
				method("ListThings", ".thing.v1.ListThingsRequest", ".thing.v1.ListThingsResponse",
					get("/thing/v1/things")),
				method("GetThing", ".thing.v1.GetThingRequest", ".thing.v1.Thing",
					get("/thing/v1/things/{id}")),
				method("CreateThing", ".thing.v1.CreateThingRequest", ".thing.v1.Thing",
					post("/thing/v1/things", "thing", post("/thing/v1/legacy_things", "thing"))),
				method("PingThing", ".thing.v1.GetThingRequest", ".thing.v1.Metadata",
					get("/thing/v1/ping")),
				method("DeleteOrphan", ".thing.v1.GetThingRequest", ".thing.v1.Metadata",
					del("/thing/v1/orphans/{id}")),
			},
		}},
	}

	identityFile := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("identity/identity.proto"),
		Package: proto.String("identity"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			message("User",
				strField("id", 1),
				strField("full_name", 2, apiannotations.FieldBehavior_REQUIRED),
			),
			message("PageInfo", strField("offset", 1)),
			message("Dup", strField("y", 1)),
			message("GetUserRequest", strField("user_id", 1)),
			message("ListUsersRequest"),
			message("ListUsersResponse",
				msgField("users", 1, ".identity.User", true),
				msgField("page", 2, ".identity.PageInfo", false),
			),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("UserService"),
			Method: []*descriptorpb.MethodDescriptorProto{
				method("GetUser", ".identity.GetUserRequest", ".identity.User",
					get("/identity/v1/users/{user_id}")),
				method("ListUsers", ".identity.ListUsersRequest", ".identity.ListUsersResponse",
					get("/identity/v1/users")),
				method("Ping", ".identity.GetUserRequest", ".identity.PageInfo",
					get("/ping")),
				method("DeleteOrphan", ".identity.GetUserRequest", ".identity.PageInfo",
					del("/identity/v1/orphans/{user_id}")),
			},
		}},
	}

	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{thingFile, identityFile},
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	return files, fds
}

// gw1Swagger mimics protoc-gen-swagger (gateway v1 / atlas) output: legacy
// operationIds, snake_case properties, atlas-style definition names, and a
// patched basePath (`/api/thing/v1` while the proto rules live under
// `/thing/v1/...`).
const gw1Swagger = `{
  "swagger": "2.0",
  "info": {"title": "thing.proto", "version": "1.0"},
  "basePath": "/api/thing/v1",
  "consumes": ["application/json"],
  "produces": ["application/json"],
  "paths": {
    "/things": {
      "get": {
        "operationId": "ListThings",
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/v1ListThingsResponse"}}}
      },
      "post": {
        "operationId": "CreateThing",
        "parameters": [{"name": "body", "in": "body", "required": true, "schema": {"$ref": "#/definitions/v1Thing"}}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/v1Thing"}}}
      }
    },
    "/things/{thing_id}": {
      "get": {
        "operationId": "ReadThing",
        "parameters": [{"name": "thing_id", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/v1Thing"}}}
      }
    },
    "/legacy_things": {
      "post": {
        "operationId": "CreateThing2",
        "parameters": [{"name": "body", "in": "body", "required": true, "schema": {"$ref": "#/definitions/v1Thing"}}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/v1Thing"}}}
      }
    },
    "/identity/v1/users": {
      "get": {
        "operationId": "ListUsers",
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/identityUser"}}}
      }
    },
    "/identity/v1/users/{id}": {
      "get": {
        "operationId": "ReadUser",
        "parameters": [{"name": "id", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/identityUser"}}}
      }
    },
    "/unknown_things": {
      "get": {
        "operationId": "MysteryOp",
        "responses": {"200": {"description": "ok"}}
      }
    }
  },
  "definitions": {
    "thing.v1.Metadata": {"type": "object", "properties": {"value": {"type": "string"}}},
    "v1Thing": {"type": "object", "properties": {
      "id": {"type": "string"},
      "display_name": {"type": "string"},
      "created_at": {"type": "string"}
    }},
    "v1ListThingsResponse": {"type": "object", "properties": {
      "things": {"type": "array", "items": {"$ref": "#/definitions/v1Thing"}}
    }},
    "identityUser": {"type": "object", "properties": {
      "id": {"type": "string"},
      "full_name": {"type": "string"}
    }},
    "PageInfo": {"type": "object", "properties": {"offset": {"type": "string"}}},
    "v1tagset": {"type": "object", "properties": {"tags": {"type": "string"}}},
    "Dup": {"type": "object", "properties": {"x": {"type": "string"}}},
    "SomethingElse": {"type": "object", "properties": {"foo": {"type": "string"}}}
  }
}`

// compatConvert converts a swagger doc and runs the gateway-v1 enrichment.
func compatConvert(t *testing.T, swagger string, opts compatOptions) (*openapi3.T, *coverageReport, error) {
	t.Helper()
	files, _ := gw1FDS(t)
	var doc2 openapi2.T
	if err := json.Unmarshal([]byte(swagger), &doc2); err != nil {
		t.Fatalf("parse v2 fixture: %v", err)
	}
	doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		t.Fatalf("ToV3: %v", err)
	}
	if len(doc.Servers) == 0 && doc2.BasePath != "" {
		doc.Servers = openapi3.Servers{&openapi3.Server{URL: doc2.BasePath}}
	}
	rep, cerr := enrichCompat(doc, files, opts)
	return doc, rep, cerr
}

// findOp returns the operation at (verb, path), failing the test if absent.
func findOp(t *testing.T, doc *openapi3.T, verb, path string) *openapi3.Operation {
	t.Helper()
	item := doc.Paths.Find(path)
	if item == nil {
		t.Fatalf("path %q not in document", path)
	}
	op := item.Operations()[strings.ToUpper(verb)]
	if op == nil {
		t.Fatalf("no %s operation at %q", verb, path)
	}
	return op
}

// TestCompatOperationMatching covers spec items 1 + 2: (verb, path) matching
// with prefix tolerance and positional path variables, additional_bindings as
// distinct bindings, canonical operationId synthesis with the legacy id kept
// as x-legacy-operation-id, and unmatched operations left untouched.
func TestCompatOperationMatching(t *testing.T) {
	doc, rep, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}

	want := map[[2]string][2]string{ // (verb,path) → (canonical id, legacy id)
		{"get", "/things"}:                 {"ThingService_ListThings", "ListThings"},
		{"post", "/things"}:                {"ThingService_CreateThing", "CreateThing"},
		{"get", "/things/{thing_id}"}:      {"ThingService_GetThing", "ReadThing"},
		{"post", "/legacy_things"}:         {"ThingService_CreateThing2", "CreateThing2"},
		{"get", "/identity/v1/users"}:      {"UserService_ListUsers", "ListUsers"},
		{"get", "/identity/v1/users/{id}"}: {"UserService_GetUser", "ReadUser"},
	}
	for key, ids := range want {
		op := findOp(t, doc, key[0], key[1])
		if op.OperationID != ids[0] {
			t.Errorf("%s %s operationId = %q, want %q", key[0], key[1], op.OperationID, ids[0])
		}
		if legacy, _ := op.Extensions["x-legacy-operation-id"].(string); legacy != ids[1] {
			t.Errorf("%s %s x-legacy-operation-id = %q, want %q", key[0], key[1], legacy, ids[1])
		}
		if _, ok := op.Extensions["x-aip-method"]; !ok {
			t.Errorf("%s %s missing x-aip-method", key[0], key[1])
		}
	}

	// The unmatched operation keeps its legacy id and gains no extensions.
	mystery := findOp(t, doc, "get", "/unknown_things")
	if mystery.OperationID != "MysteryOp" {
		t.Errorf("unmatched operationId = %q, want MysteryOp", mystery.OperationID)
	}
	if _, ok := mystery.Extensions["x-legacy-operation-id"]; ok {
		t.Error("unmatched operation must not carry x-legacy-operation-id")
	}

	if rep.Operations.Total != 7 || rep.Operations.Matched != 6 {
		t.Errorf("operations coverage = %d total / %d matched, want 7/6", rep.Operations.Total, rep.Operations.Matched)
	}
	if len(rep.Operations.Unmatched) != 1 || rep.Operations.Unmatched[0].Path != "/unknown_things" {
		t.Errorf("unmatched = %+v, want exactly /unknown_things", rep.Operations.Unmatched)
	}
	if len(rep.Operations.Ambiguous) != 0 {
		t.Errorf("ambiguous = %+v, want none", rep.Operations.Ambiguous)
	}
	// The four fixture methods with no swagger operation are reported, not fatal.
	if len(rep.ProtoMethodsUnmatched) != 4 {
		t.Errorf("protoMethodsUnmatched = %v, want 4 entries", rep.ProtoMethodsUnmatched)
	}
}

// TestCompatExactBeatsSuffix: a path that aligns exactly with one rule is not
// ambiguous merely because it is also a suffix of a longer rule elsewhere.
func TestCompatExactBeatsSuffix(t *testing.T) {
	doc := `{
	  "swagger": "2.0",
	  "info": {"title": "ping", "version": "1.0"},
	  "paths": {
	    "/ping": {"get": {"operationId": "Ping", "responses": {"200": {"description": "ok"}}}},
	    "/thing/v1/ping": {"get": {"operationId": "PingThing", "responses": {"200": {"description": "ok"}}}}
	  },
	  "definitions": {}
	}`
	v3, rep, err := compatConvert(t, doc, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}
	if len(rep.Operations.Ambiguous) != 0 {
		t.Fatalf("ambiguous = %+v, want none (exact match must win)", rep.Operations.Ambiguous)
	}
	if got := findOp(t, v3, "get", "/ping").OperationID; got != "UserService_Ping" {
		t.Errorf("/ping resolved to %q, want UserService_Ping", got)
	}
	if got := findOp(t, v3, "get", "/thing/v1/ping").OperationID; got != "ThingService_PingThing" {
		t.Errorf("/thing/v1/ping resolved to %q, want ThingService_PingThing", got)
	}
}

// TestCompatAmbiguousOperation: a swagger (verb, path) that suffix-matches
// rules in two services is a report entry (with all candidates) in report
// mode, and an error under strict.
func TestCompatAmbiguousOperation(t *testing.T) {
	doc := `{
	  "swagger": "2.0",
	  "info": {"title": "orphans", "version": "1.0"},
	  "paths": {
	    "/orphans/{id}": {"delete": {"operationId": "DeleteOrphan", "responses": {"200": {"description": "ok"}}}}
	  },
	  "definitions": {}
	}`
	v3, rep, err := compatConvert(t, doc, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("report mode must not error, got %v", err)
	}
	if len(rep.Operations.Ambiguous) != 1 {
		t.Fatalf("ambiguous = %+v, want exactly one", rep.Operations.Ambiguous)
	}
	amb := rep.Operations.Ambiguous[0]
	if len(amb.Candidates) != 2 {
		t.Errorf("candidates = %v, want both DeleteOrphan methods", amb.Candidates)
	}
	joined := strings.Join(amb.Candidates, " ")
	for _, want := range []string{"thing.v1.ThingService.DeleteOrphan", "identity.UserService.DeleteOrphan"} {
		if !strings.Contains(joined, want) {
			t.Errorf("candidates %v missing %s", amb.Candidates, want)
		}
	}
	// The ambiguous operation is left untouched.
	if got := findOp(t, v3, "delete", "/orphans/{id}").OperationID; got != "DeleteOrphan" {
		t.Errorf("ambiguous operationId = %q, want the original DeleteOrphan", got)
	}

	// Strict opts back into hard failure.
	if _, _, err := compatConvert(t, doc, compatOptions{jsonNames: "auto", strict: true}); err == nil {
		t.Error("strict mode must fail on an ambiguous operation")
	}
}

// TestCompatReverseAmbiguity: one proto binding claimed by two swagger paths
// (e.g. both the prefixed and the relative spelling in one document) flags
// both operations.
func TestCompatReverseAmbiguity(t *testing.T) {
	doc := `{
	  "swagger": "2.0",
	  "info": {"title": "dup", "version": "1.0"},
	  "paths": {
	    "/things": {"get": {"operationId": "ListA", "responses": {"200": {"description": "ok"}}}},
	    "/thing/v1/things": {"get": {"operationId": "ListB", "responses": {"200": {"description": "ok"}}}}
	  },
	  "definitions": {}
	}`
	v3, rep, err := compatConvert(t, doc, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("report mode must not error, got %v", err)
	}
	if len(rep.Operations.Ambiguous) != 2 || rep.Operations.Matched != 0 {
		t.Fatalf("want both operations ambiguous and none matched, got matched=%d ambiguous=%+v",
			rep.Operations.Matched, rep.Operations.Ambiguous)
	}
	// Neither id was rewritten.
	if got := findOp(t, v3, "get", "/things").OperationID; got != "ListA" {
		t.Errorf("operationId = %q, want ListA untouched", got)
	}
	if got := findOp(t, v3, "get", "/thing/v1/things").OperationID; got != "ListB" {
		t.Errorf("operationId = %q, want ListB untouched", got)
	}
}

// TestCompatSnakeAutoDetect covers spec item 3: the probe detects snake_case
// properties, enrichment keys by fd.Name(), and the -json-names override wins.
func TestCompatSnakeAutoDetect(t *testing.T) {
	doc, rep, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}
	if rep.JSONNames != "snake" || rep.JSONNamesSource != "auto" {
		t.Fatalf("jsonNames = %s (%s), want snake (auto)", rep.JSONNames, rep.JSONNamesSource)
	}
	thing := doc.Components.Schemas["v1Thing"].Value
	if !contains(thing.Required, "display_name") {
		t.Errorf("v1Thing.required = %v, want display_name (REQUIRED via snake key)", thing.Required)
	}
	if !thing.Properties["created_at"].Value.ReadOnly {
		t.Error("created_at must be readOnly (OUTPUT_ONLY via snake key)")
	}
	user := doc.Components.Schemas["identityUser"].Value
	if !contains(user.Required, "full_name") {
		t.Errorf("identityUser.required = %v, want full_name", user.Required)
	}
	if rep.Fields.Skipped != 0 {
		t.Errorf("fields skipped = %d (%+v), want 0", rep.Fields.Skipped, rep.Fields.SkippedDetail)
	}
	if rep.Fields.Enriched == 0 {
		t.Error("fields enriched = 0, want > 0")
	}
}

const camelSwagger = `{
  "swagger": "2.0",
  "info": {"title": "thing.proto", "version": "1.0"},
  "paths": {
    "/thing/v1/things": {"get": {"operationId": "ListThings", "responses": {"200": {"description": "ok"}}}}
  },
  "definitions": {
    "v1Thing": {"type": "object", "properties": {
      "id": {"type": "string"},
      "displayName": {"type": "string"},
      "createdAt": {"type": "string"}
    }}
  }
}`

// TestCompatCamelAutoDetect: a camelCase document (json_names_for_fields=true)
// probes to camel mode and enriches by fd.JSONName().
func TestCompatCamelAutoDetect(t *testing.T) {
	doc, rep, err := compatConvert(t, camelSwagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}
	if rep.JSONNames != "camel" {
		t.Fatalf("jsonNames = %s, want camel", rep.JSONNames)
	}
	thing := doc.Components.Schemas["v1Thing"].Value
	if !contains(thing.Required, "displayName") {
		t.Errorf("required = %v, want displayName", thing.Required)
	}
	if rep.Fields.Skipped != 0 {
		t.Errorf("fields skipped = %d, want 0", rep.Fields.Skipped)
	}
}

// TestCompatJSONNamesOverride: forcing snake on a camel document skips the
// camel-only properties (and would fail under strict) — the flag wins over
// the probe.
func TestCompatJSONNamesOverride(t *testing.T) {
	_, rep, err := compatConvert(t, camelSwagger, compatOptions{jsonNames: "snake"})
	if err != nil {
		t.Fatalf("report mode must not error, got %v", err)
	}
	if rep.JSONNames != "snake" || rep.JSONNamesSource != "flag" {
		t.Fatalf("jsonNames = %s (%s), want snake (flag)", rep.JSONNames, rep.JSONNamesSource)
	}
	if rep.Fields.Skipped != 2 {
		t.Errorf("fields skipped = %d (%+v), want 2 (displayName, createdAt)", rep.Fields.Skipped, rep.Fields.SkippedDetail)
	}
	if _, _, err := compatConvert(t, camelSwagger, compatOptions{jsonNames: "snake", strict: true}); err == nil {
		t.Error("strict mode must fail on skipped fields")
	}
}

// TestCompatSchemaNameResolution covers spec item 4: the tiered gw-v1/atlas
// definition-name resolver — exact FQN, package concat, unique bare name,
// case-insensitive variants — with ambiguity skipped and reported.
func TestCompatSchemaNameResolution(t *testing.T) {
	_, rep, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}

	tiers := map[string][2]string{} // definition → (message, tier)
	for _, m := range rep.Schemas.Matched {
		tiers[m.Name] = [2]string{m.Message, m.Tier}
	}
	for name, want := range map[string][2]string{
		"thing.v1.Metadata":    {"thing.v1.Metadata", "fqn"},
		"v1Thing":              {"thing.v1.Thing", "package-concat"},
		"v1ListThingsResponse": {"thing.v1.ListThingsResponse", "package-concat"},
		"identityUser":         {"identity.User", "package-concat"},
		"PageInfo":             {"identity.PageInfo", "bare"},
		"v1tagset":             {"thing.v1.TagSet", "package-concat (case-insensitive)"},
	} {
		got, ok := tiers[name]
		if !ok {
			t.Errorf("definition %q did not resolve (matched: %v)", name, rep.Schemas.Matched)
			continue
		}
		if got != want {
			t.Errorf("definition %q resolved to %v, want %v", name, got, want)
		}
	}

	if len(rep.Schemas.Ambiguous) != 1 || rep.Schemas.Ambiguous[0].Name != "Dup" {
		t.Errorf("ambiguous schemas = %+v, want exactly Dup", rep.Schemas.Ambiguous)
	} else if got := rep.Schemas.Ambiguous[0].Candidates; len(got) != 2 {
		t.Errorf("Dup candidates = %v, want thing.v1.Dup + identity.Dup", got)
	}
	if len(rep.Schemas.Unmatched) != 1 || rep.Schemas.Unmatched[0].Name != "SomethingElse" {
		t.Errorf("unmatched schemas = %+v, want exactly SomethingElse", rep.Schemas.Unmatched)
	}
	if rep.Schemas.Total != 8 || rep.Schemas.Enriched != 6 {
		t.Errorf("schemas coverage = %d total / %d enriched, want 8/6", rep.Schemas.Total, rep.Schemas.Enriched)
	}
}

// TestCompatServersFromBasePath: exit criterion 3 — a basePath-only swagger
// (no host) keeps its base URL as servers[0].
func TestCompatServersFromBasePath(t *testing.T) {
	doc, _, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto"})
	if err != nil {
		t.Fatalf("enrichCompat: %v", err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "/api/thing/v1" {
		t.Fatalf("servers = %+v, want [{url: /api/thing/v1}]", doc.Servers)
	}
}

// TestCompatStrictOnFixture: the main fixture has an unmatched operation, an
// unmatched schema, and an ambiguous schema — strict must fail on it while
// report mode passes.
func TestCompatStrictOnFixture(t *testing.T) {
	if _, _, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto"}); err != nil {
		t.Fatalf("report mode: %v", err)
	}
	_, rep, err := compatConvert(t, gw1Swagger, compatOptions{jsonNames: "auto", strict: true})
	if err == nil {
		t.Fatal("strict mode must fail on the fixture's gaps")
	}
	if rep == nil || !rep.hasGaps() {
		t.Fatal("strict failure must still return the coverage report")
	}
}

// --- run()-level end-to-end ---

// writeGW1Fixture materializes the FDS + swagger to disk for run()-level tests.
func writeGW1Fixture(t *testing.T) (fdsPath, swaggerPath string) {
	t.Helper()
	_, fds := gw1FDS(t)
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	dir := t.TempDir()
	fdsPath = filepath.Join(dir, "thing.binpb")
	if err := os.WriteFile(fdsPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	swaggerPath = filepath.Join(dir, "thing.swagger.json")
	if err := os.WriteFile(swaggerPath, []byte(gw1Swagger), 0o644); err != nil {
		t.Fatal(err)
	}
	return fdsPath, swaggerPath
}

// TestRunCompatEndToEnd: the CLI writes the enriched v3 spec plus the
// machine-readable coverage report next to it (spec item 5).
func TestRunCompatEndToEnd(t *testing.T) {
	fdsPath, swaggerPath := writeGW1Fixture(t)
	outDir := t.TempDir()
	if code := run([]string{"-descriptor", fdsPath, "-compat=gateway-v1", swaggerPath, outDir}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}

	outPath := filepath.Join(outDir, "thing.openapi.yaml")
	yamlBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output spec: %v", err)
	}
	for _, want := range []string{
		"servers:",
		"url: /api/thing/v1",
		"operationId: ThingService_ListThings",
		"x-legacy-operation-id: ReadThing",
		"x-aip-method:",
		"- display_name", // snake-mode required list
	} {
		if !bytes.Contains(yamlBytes, []byte(want)) {
			t.Errorf("output spec missing %q", want)
		}
	}

	covPath := outPath + ".coverage.json"
	covBytes, err := os.ReadFile(covPath)
	if err != nil {
		t.Fatalf("coverage report: %v", err)
	}
	var rep coverageReport
	if err := json.Unmarshal(covBytes, &rep); err != nil {
		t.Fatalf("parse coverage report: %v", err)
	}
	if rep.Mode != "gateway-v1" || rep.JSONNames != "snake" {
		t.Errorf("coverage mode/jsonNames = %s/%s, want gateway-v1/snake", rep.Mode, rep.JSONNames)
	}
	if rep.Operations.Total != 7 || rep.Operations.Matched != 6 {
		t.Errorf("coverage operations = %d/%d, want 7 total / 6 matched", rep.Operations.Total, rep.Operations.Matched)
	}
	if rep.Input == "" {
		t.Error("coverage input path missing")
	}
}

// TestRunCompatStrictFails: under -strict the fixture's gaps are a hard
// failure and nothing is written.
func TestRunCompatStrictFails(t *testing.T) {
	fdsPath, swaggerPath := writeGW1Fixture(t)
	outDir := t.TempDir()
	if code := run([]string{"-descriptor", fdsPath, "-compat=gateway-v1", "-strict", swaggerPath, outDir}); code == 0 {
		t.Fatal("run with -strict on a gappy fixture: want non-zero exit")
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Errorf("expected no output written on strict failure, found %d entries", len(entries))
	}
}

// TestRunFlagValidation: the compat sub-flags are rejected without -compat,
// and unknown mode/json-names values are rejected.
func TestRunFlagValidation(t *testing.T) {
	fdsPath, swaggerPath := writeGW1Fixture(t)
	outDir := t.TempDir()
	for name, args := range map[string][]string{
		"strict without compat":     {"-descriptor", fdsPath, "-strict", swaggerPath, outDir},
		"json-names without compat": {"-descriptor", fdsPath, "-json-names=snake", swaggerPath, outDir},
		"unknown compat mode":       {"-descriptor", fdsPath, "-compat=gateway-v3", swaggerPath, outDir},
		"unknown json-names":        {"-descriptor", fdsPath, "-compat=gateway-v1", "-json-names=kebab", swaggerPath, outDir},
	} {
		if code := run(args); code == 0 {
			t.Errorf("%s: want non-zero exit", name)
		}
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Errorf("expected no output from rejected invocations, found %d entries", len(entries))
	}
}

// TestDefaultModeGoldenByteIdentical guards the gateway-v2 default path: with
// no -compat flag the conversion of the toy fixture must stay byte-identical
// to the checked-in golden (existing consumers: `de api publish`, apx).
func TestDefaultModeGoldenByteIdentical(t *testing.T) {
	fdsPath, swaggerPath := toyPaths(t)
	if _, err := os.Stat(fdsPath); err != nil {
		t.Skipf("toy.binpb absent (%v) — run 'make generate' first", err)
	}
	outDir := t.TempDir()
	if code := run([]string{"-descriptor", fdsPath, swaggerPath, outDir}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "toy.openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join(filepath.Dir(swaggerPath), "openapi", "toy.openapi.yaml"))
	if err != nil {
		t.Skipf("golden absent (%v) — run 'make generate' first", err)
	}
	if !bytes.Equal(got, golden) {
		t.Error("default-mode output diverged from the checked-in golden — gateway-v2 behavior must stay byte-identical")
	}
	// And no coverage report in default mode.
	if _, err := os.Stat(filepath.Join(outDir, "toy.openapi.yaml.coverage.json")); !os.IsNotExist(err) {
		t.Error("default mode must not write a coverage report")
	}
}
