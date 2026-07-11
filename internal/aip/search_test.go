package aip

import (
	"strings"
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// withSearch builds message options carrying the (infoblox.storage.v1.search)
// extension.
func withSearch(sc *storagev1.SearchConfig) *descriptorpb.MessageOptions {
	mo := &descriptorpb.MessageOptions{}
	proto.SetExtension(mo, storagev1.E_Search, sc)
	return mo
}

// buildSearchMessage compiles a one-message file "srchtest.M" (importing field.v1
// + storage.v1 so both option extensions resolve) and returns its descriptor.
func buildSearchMessage(t *testing.T, msgOpts *descriptorpb.MessageOptions, fields ...*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("srchtest/srchtest.proto"),
		Syntax:  proto.String("proto3"),
		Package: proto.String("srchtest"),
		Dependency: []string{
			"google/api/field_behavior.proto",
			"infoblox/field/v1/field.proto",
			"infoblox/storage/v1/storage.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:    proto.String("M"),
			Options: msgOpts,
			Field:   fields,
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

func fieldSource(name, field string) *storagev1.SearchSource {
	return &storagev1.SearchSource{Name: name, From: &storagev1.SearchSource_Field{Field: field}}
}

func exprSource(name string, exprs ...*storagev1.SearchExpr) *storagev1.SearchSource {
	return &storagev1.SearchSource{
		Name: name,
		From: &storagev1.SearchSource_Exprs{Exprs: &storagev1.SearchExprSet{Expr: exprs}},
	}
}

func TestResolveSearchConfig_Absent(t *testing.T) {
	// No search annotation and no searchable field -> empty (not error) config,
	// defaults applied, not searchable.
	md := buildSearchMessage(t, nil,
		stringField("display_name", 1, "displayName", nil),
		stringField("description", 2, "description", nil),
	)
	cfg, err := ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	if cfg.IsSearchable() {
		t.Errorf("IsSearchable = true, want false for a message with no search surface")
	}
	if cfg.Strategy != SearchJIT {
		t.Errorf("Strategy = %v, want JIT default", cfg.Strategy)
	}
	if cfg.TextConfig != DefaultTextConfig {
		t.Errorf("TextConfig = %q, want %q default", cfg.TextConfig, DefaultTextConfig)
	}
	if len(cfg.Sources) != 0 {
		t.Errorf("Sources = %d, want 0", len(cfg.Sources))
	}
}

func TestResolveSearchConfig_NilDescriptor(t *testing.T) {
	cfg, err := ResolveSearchConfig(nil)
	if err != nil {
		t.Fatalf("ResolveSearchConfig(nil): %v", err)
	}
	if cfg.IsSearchable() || cfg.Strategy != SearchJIT || cfg.TextConfig != DefaultTextConfig {
		t.Errorf("nil descriptor should yield the empty default config, got %+v", cfg)
	}
}

func TestResolveSearchConfig_ImplicitFieldsOnly(t *testing.T) {
	// Field-flagged searchable=true columns become implicit sources in field
	// order; a non-searchable field is skipped. Strategy defaults to JIT even
	// with no message annotation. Secret searchable fields are kept RAW here
	// (semantic rejection is the compiler's job, not the resolver's).
	md := buildSearchMessage(t, nil,
		stringField("display_name", 1, "displayName", withOpts(&fieldv1.FieldOptions{Searchable: true})),
		stringField("description", 2, "description", nil),
		stringField("token", 3, "token", withOpts(&fieldv1.FieldOptions{Searchable: true, Secret: true})),
	)
	cfg, err := ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	if !cfg.IsSearchable() {
		t.Fatal("IsSearchable = false, want true")
	}
	if cfg.Strategy != SearchJIT {
		t.Errorf("Strategy = %v, want JIT", cfg.Strategy)
	}
	if got := sourceNames(cfg); !equal(got, []string{"display_name", "token"}) {
		t.Errorf("implicit sources = %v, want [display_name token] in field order", got)
	}
	for _, s := range cfg.Sources {
		if !s.IsField() {
			t.Errorf("source %q should be a field source", s.Name)
		}
		if len(s.Exprs) != 0 {
			t.Errorf("field source %q should carry no exprs", s.Name)
		}
	}
}

func TestResolveSearchConfig_ExplicitSourcesAndStrategy(t *testing.T) {
	// Implicit field sources come first (field order), then the message-level
	// sources in declared order. Strategy + text_config are read from the
	// annotation; expression sources are returned RAW.
	sc := &storagev1.SearchConfig{
		Strategy:   storagev1.SearchConfig_STRATEGY_INDEXED,
		TextConfig: "english",
		Sources: []*storagev1.SearchSource{
			exprSource("zone_type",
				&storagev1.SearchExpr{Flavor: "cel", Version: "v1", Expr: `msg.display_name`}),
			fieldSource("desc_alias", "description"),
		},
	}
	md := buildSearchMessage(t, withSearch(sc),
		stringField("display_name", 1, "displayName", withOpts(&fieldv1.FieldOptions{Searchable: true})),
		stringField("description", 2, "description", nil),
	)
	cfg, err := ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	if cfg.Strategy != SearchIndexed {
		t.Errorf("Strategy = %v, want INDEXED", cfg.Strategy)
	}
	if cfg.TextConfig != "english" {
		t.Errorf("TextConfig = %q, want english", cfg.TextConfig)
	}
	if got := sourceNames(cfg); !equal(got, []string{"display_name", "zone_type", "desc_alias"}) {
		t.Fatalf("sources = %v, want [display_name zone_type desc_alias] (implicit first, then declared order)", got)
	}
	// zone_type is a raw cel expression source.
	zt := cfg.Sources[1]
	if zt.IsField() {
		t.Errorf("zone_type should be an expression source")
	}
	if len(zt.Exprs) != 1 || zt.Exprs[0].Flavor != "cel" || zt.Exprs[0].Version != "v1" || zt.Exprs[0].Expr != "msg.display_name" {
		t.Errorf("zone_type exprs not preserved raw: %+v", zt.Exprs)
	}
	// desc_alias is a field source resolved to the description descriptor.
	da := cfg.Sources[2]
	if !da.IsField() || string(da.Field.Name()) != "description" {
		t.Errorf("desc_alias should resolve to field 'description', got %+v", da)
	}
}

func TestResolveSearchConfig_StrategyUnspecifiedIsJIT(t *testing.T) {
	sc := &storagev1.SearchConfig{
		Strategy: storagev1.SearchConfig_STRATEGY_UNSPECIFIED,
		Sources:  []*storagev1.SearchSource{fieldSource("dn", "display_name")},
	}
	md := buildSearchMessage(t, withSearch(sc),
		stringField("display_name", 1, "displayName", nil),
	)
	cfg, err := ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	if cfg.Strategy != SearchJIT {
		t.Errorf("STRATEGY_UNSPECIFIED resolved to %v, want JIT", cfg.Strategy)
	}
	if cfg.TextConfig != DefaultTextConfig {
		t.Errorf("empty text_config resolved to %q, want %q", cfg.TextConfig, DefaultTextConfig)
	}
}

func TestResolveSearchConfig_ProjectedIsSearchable(t *testing.T) {
	// PROJECTED must be preserved (not mapped to JIT) so the codegen fail-loud
	// stays reachable; IsSearchable is true even with no local sources.
	sc := &storagev1.SearchConfig{Strategy: storagev1.SearchConfig_STRATEGY_PROJECTED}
	md := buildSearchMessage(t, withSearch(sc),
		stringField("display_name", 1, "displayName", nil),
	)
	cfg, err := ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	if cfg.Strategy != SearchProjected {
		t.Errorf("Strategy = %v, want PROJECTED", cfg.Strategy)
	}
	if !cfg.IsSearchable() {
		t.Errorf("a PROJECTED resource with no sources should still report IsSearchable")
	}
}

func TestResolveSearchConfig_UnknownFieldReferenceFailsLoud(t *testing.T) {
	sc := &storagev1.SearchConfig{
		Sources: []*storagev1.SearchSource{fieldSource("nope", "does_not_exist")},
	}
	md := buildSearchMessage(t, withSearch(sc),
		stringField("display_name", 1, "displayName", nil),
	)
	_, err := ResolveSearchConfig(md)
	if err == nil {
		t.Fatal("expected an error for a source referencing an unknown field")
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("error = %q, want it to name the unknown field", err.Error())
	}
}

// --- helpers ---

func sourceNames(cfg SearchConfig) []string {
	out := make([]string, len(cfg.Sources))
	for i, s := range cfg.Sources {
		out[i] = s.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
