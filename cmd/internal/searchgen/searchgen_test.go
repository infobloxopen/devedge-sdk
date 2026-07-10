package searchgen

import (
	"strings"
	"testing"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	"github.com/infobloxopen/devedge-sdk/internal/aip"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// buildM compiles a one-message file "sgtest.M" with the given message options
// (carrying the storage.v1 search extension) and returns its descriptor. The
// message shape mirrors a DDI-style resource: textual scalar/enum/map/repeated
// fields plus a non-textual int, an id, and a secret.
//
//	message M {
//	  string             display_name = 1;   // searchable candidate
//	  string             description  = 2;
//	  MType              type         = 3;    // enum
//	  map<string,string> tags         = 4;
//	  repeated string    aliases      = 5;
//	  int64              rank         = 6;    // non-textual
//	  string             token        = 7;    // secret
//	  string             labeled      = 8;    // column_name = "lbl"
//	  enum MType { M_TYPE_UNSPECIFIED = 0; PRIMARY = 1; SECONDARY = 2; }
//	}
func buildM(t *testing.T, mo *descriptorpb.MessageOptions, fieldOpts map[string]*fieldv1.FieldOptions, behaviors ...map[string]apiannotations.FieldBehavior) protoreflect.MessageDescriptor {
	t.Helper()

	var behavior map[string]apiannotations.FieldBehavior
	if len(behaviors) > 0 {
		behavior = behaviors[0]
	}
	opt := func(name string) *descriptorpb.FieldOptions {
		fo := fieldOpts[name]
		b, hasB := behavior[name]
		if fo == nil && !hasB {
			return nil
		}
		o := &descriptorpb.FieldOptions{}
		if fo != nil {
			proto.SetExtension(o, fieldv1.E_Opts, fo)
		}
		if hasB {
			proto.SetExtension(o, apiannotations.E_FieldBehavior, []apiannotations.FieldBehavior{b})
		}
		return o
	}
	str := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(jsonName(name)), Options: opt(name),
		}
	}

	msg := &descriptorpb.DescriptorProto{
		Name: proto.String("M"),
		Field: []*descriptorpb.FieldDescriptorProto{
			str("display_name", 1),
			str("description", 2),
			{
				Name: proto.String("type"), Number: proto.Int32(3),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
				TypeName: proto.String(".sgtest.M.MType"), JsonName: proto.String("type"),
				Options: opt("type"),
			},
			{
				Name: proto.String("tags"), Number: proto.Int32(4),
				Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".sgtest.M.TagsEntry"), JsonName: proto.String("tags"),
				Options: opt("tags"),
			},
			{
				Name: proto.String("aliases"), Number: proto.Int32(5),
				Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				JsonName: proto.String("aliases"), Options: opt("aliases"),
			},
			{
				Name: proto.String("rank"), Number: proto.Int32(6),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				JsonName: proto.String("rank"), Options: opt("rank"),
			},
			str("token", 7),
			str("labeled", 8),
		},
		NestedType: []*descriptorpb.DescriptorProto{{
			Name:    proto.String("TagsEntry"),
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("key"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("value"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
			},
		}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("MType"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("M_TYPE_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("PRIMARY"), Number: proto.Int32(1)},
				{Name: proto.String("SECONDARY"), Number: proto.Int32(2)},
			},
		}},
		Options: mo,
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("sgtest/sgtest.proto"),
		Syntax:  proto.String("proto3"),
		Package: proto.String("sgtest"),
		Dependency: []string{
			"infoblox/field/v1/field.proto",
			"infoblox/storage/v1/storage.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{msg},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

// jsonName gives the lowerCamel JSON name for a snake_case proto field name.
func jsonName(snake string) string {
	parts := strings.Split(snake, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func searchOpts(sc *storagev1.SearchConfig) *descriptorpb.MessageOptions {
	mo := &descriptorpb.MessageOptions{}
	proto.SetExtension(mo, storagev1.E_Search, sc)
	return mo
}

func mustCompile(t *testing.T, md protoreflect.MessageDescriptor, dialect string) *Compiled {
	t.Helper()
	cfg, err := aip.ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	c, err := Compile(cfg, md, dialect)
	if err != nil {
		t.Fatalf("Compile(%s): %v", dialect, err)
	}
	return c
}

func compileErr(t *testing.T, md protoreflect.MessageDescriptor, dialect string) error {
	t.Helper()
	cfg, err := aip.ResolveSearchConfig(md)
	if err != nil {
		return err
	}
	_, err = Compile(cfg, md, dialect)
	return err
}

const (
	dnPG = `replace(replace(coalesce(CAST("display_name" AS text), ''), '@', ' '), '.', ' ')`
	dnLT = `coalesce(CAST("display_name" AS text), '')`
)

func TestCompile_FieldNormalization(t *testing.T) {
	md := buildM(t, nil, map[string]*fieldv1.FieldOptions{
		"display_name": {Searchable: true},
	})
	c := mustCompile(t, md, DialectPostgres)
	if c.PostgresVector != dnPG {
		t.Errorf("PostgresVector\n got: %s\nwant: %s", c.PostgresVector, dnPG)
	}
	if c.SQLiteVector != dnLT {
		t.Errorf("SQLiteVector\n got: %s\nwant: %s", c.SQLiteVector, dnLT)
	}
	if c.PostgresOnly {
		t.Errorf("PostgresOnly = true, want false for a field-only resource")
	}
	if c.TextConfig != "simple" {
		t.Errorf("TextConfig = %q, want simple", c.TextConfig)
	}
	if len(c.SourceNames) != 1 || c.SourceNames[0] != "displayName" {
		t.Errorf("SourceNames = %v, want [displayName] (JSON name)", c.SourceNames)
	}
}

func TestCompile_ColumnNameOverride(t *testing.T) {
	md := buildM(t, nil, map[string]*fieldv1.FieldOptions{
		"labeled": {Searchable: true, ColumnName: "lbl"},
	})
	c := mustCompile(t, md, DialectPostgres)
	want := `replace(replace(coalesce(CAST("lbl" AS text), ''), '@', ' '), '.', ' ')`
	if c.PostgresVector != want {
		t.Errorf("PostgresVector\n got: %s\nwant: %s", c.PostgresVector, want)
	}
}

func TestCompile_TextualTypesAccepted(t *testing.T) {
	// string, enum, repeated string, and map<string,string> tags are all textual.
	md := buildM(t, nil, map[string]*fieldv1.FieldOptions{
		"display_name": {Searchable: true},
		"type":         {Searchable: true},
		"tags":         {Searchable: true},
		"aliases":      {Searchable: true},
	})
	c := mustCompile(t, md, DialectPostgres)
	// Four sources concatenated in field order.
	if got := strings.Count(c.PostgresVector, " || ' ' || "); got != 3 {
		t.Errorf("expected 4 concatenated fragments (3 joiners), got %d in %q", got, c.PostgresVector)
	}
	if !equalStrs(c.SourceNames, []string{"displayName", "type", "tags", "aliases"}) {
		t.Errorf("SourceNames = %v", c.SourceNames)
	}
}

func TestCompile_SQLPostgresPassthroughAndPostgresOnly(t *testing.T) {
	sc := &storagev1.SearchConfig{
		Sources: []*storagev1.SearchSource{
			{Name: "display_name", From: &storagev1.SearchSource_Field{Field: "display_name"}},
			exprS("zone_type", &storagev1.SearchExpr{Flavor: "sql", Dialect: "postgres",
				Expr: `CASE "type" WHEN 1 THEN 'Primary' ELSE 'Secondary' END`}),
		},
	}
	md := buildM(t, searchOpts(sc), nil)

	// Postgres target: both fragments present, PostgresOnly set, no SQLite vector.
	c := mustCompile(t, md, DialectPostgres)
	if !c.PostgresOnly {
		t.Errorf("PostgresOnly = false, want true (a sql/postgres source is present)")
	}
	if c.SQLiteVector != "" {
		t.Errorf("SQLiteVector = %q, want empty for a Postgres-only resource", c.SQLiteVector)
	}
	wantPG := dnPG + " || ' ' || " + `(CASE "type" WHEN 1 THEN 'Primary' ELSE 'Secondary' END)`
	if c.PostgresVector != wantPG {
		t.Errorf("PostgresVector\n got: %s\nwant: %s", c.PostgresVector, wantPG)
	}
	if !equalStrs(c.SourceNames, []string{"displayName", "zone_type"}) {
		t.Errorf("SourceNames = %v, want [displayName zone_type]", c.SourceNames)
	}

	// SQLite target: a Postgres-only resource must fail loud (SD-4, AC-10, FM-8).
	err := compileErr(t, md, DialectSQLite)
	if err == nil || !strings.Contains(err.Error(), "Postgres-only") {
		t.Errorf("SQLite compile of a Postgres-only resource: got %v, want a Postgres-only fail-loud", err)
	}
}

func TestCompile_CELSource(t *testing.T) {
	sc := &storagev1.SearchConfig{
		Sources: []*storagev1.SearchSource{
			exprS("zone_type", &storagev1.SearchExpr{Flavor: "cel",
				Expr: `{1: "Primary", 2: "Secondary"}[msg.type]`}),
		},
	}
	md := buildM(t, searchOpts(sc), nil)
	c := mustCompile(t, md, DialectSQLite) // cel is portable -> SQLite is fine
	wantCase := `CASE "type" WHEN 1 THEN 'Primary' WHEN 2 THEN 'Secondary' ELSE NULL END`
	if c.PostgresVector != wantCase {
		t.Errorf("PostgresVector\n got: %s\nwant: %s", c.PostgresVector, wantCase)
	}
	if c.SQLiteVector != wantCase {
		t.Errorf("SQLiteVector\n got: %s\nwant: %s", c.SQLiteVector, wantCase)
	}
	if c.PostgresOnly {
		t.Errorf("PostgresOnly = true, want false for a cel (portable) source")
	}
}

func TestCompile_FieldPlusCELPortable(t *testing.T) {
	sc := &storagev1.SearchConfig{
		Sources: []*storagev1.SearchSource{
			exprS("norm", &storagev1.SearchExpr{Flavor: "cel", Expr: `msg.display_name.lowerAscii()`}),
		},
	}
	md := buildM(t, searchOpts(sc), map[string]*fieldv1.FieldOptions{
		"description": {Searchable: true},
	})
	c := mustCompile(t, md, DialectSQLite)
	if c.PostgresOnly {
		t.Fatalf("PostgresOnly = true, want false (field + cel are both portable)")
	}
	if !strings.Contains(c.SQLiteVector, `coalesce(CAST("description" AS text), '')`) ||
		!strings.Contains(c.SQLiteVector, `lower("display_name")`) {
		t.Errorf("SQLiteVector missing expected fragments: %s", c.SQLiteVector)
	}
}

func TestCompile_RejectRules(t *testing.T) {
	tests := []struct {
		name      string
		mo        *descriptorpb.MessageOptions
		fopts     map[string]*fieldv1.FieldOptions
		behaviors map[string]apiannotations.FieldBehavior
		dialect   string
		wantPart  string
	}{
		{
			name:     "secret searchable field",
			fopts:    map[string]*fieldv1.FieldOptions{"token": {Searchable: true, Secret: true}},
			dialect:  DialectPostgres,
			wantPart: "secret",
		},
		{
			name:      "INPUT_ONLY searchable field",
			fopts:     map[string]*fieldv1.FieldOptions{"description": {Searchable: true}},
			behaviors: map[string]apiannotations.FieldBehavior{"description": apiannotations.FieldBehavior_INPUT_ONLY},
			dialect:   DialectPostgres,
			wantPart:  "INPUT_ONLY",
		},
		{
			name:     "non-textual searchable field",
			fopts:    map[string]*fieldv1.FieldOptions{"rank": {Searchable: true}},
			dialect:  DialectPostgres,
			wantPart: "not full-text searchable",
		},
		{
			name:     "strategy PROJECTED",
			mo:       searchOpts(&storagev1.SearchConfig{Strategy: storagev1.SearchConfig_STRATEGY_PROJECTED}),
			dialect:  DialectPostgres,
			wantPart: "PROJECTED",
		},
		{
			name: "unknown flavor",
			mo: searchOpts(&storagev1.SearchConfig{Sources: []*storagev1.SearchSource{
				exprS("x", &storagev1.SearchExpr{Flavor: "regex", Expr: "whatever"}),
			}}),
			dialect:  DialectPostgres,
			wantPart: "unknown flavor",
		},
		{
			name: "unsupported sql dialect",
			mo: searchOpts(&storagev1.SearchConfig{Sources: []*storagev1.SearchSource{
				exprS("x", &storagev1.SearchExpr{Flavor: "sql", Dialect: "mysql", Expr: "1"}),
			}}),
			dialect:  DialectPostgres,
			wantPart: "sql dialect",
		},
		{
			name: "non-immutable sql function",
			mo: searchOpts(&storagev1.SearchConfig{Sources: []*storagev1.SearchSource{
				exprS("x", &storagev1.SearchExpr{Flavor: "sql", Dialect: "postgres", Expr: "now()::text"}),
			}}),
			dialect:  DialectPostgres,
			wantPart: "non-immutable",
		},
		{
			name: "cross-table sql reference",
			mo: searchOpts(&storagev1.SearchConfig{Sources: []*storagev1.SearchSource{
				exprS("x", &storagev1.SearchExpr{Flavor: "sql", Dialect: "postgres",
					Expr: "(SELECT name FROM other WHERE other.id = id)"}),
			}}),
			dialect:  DialectPostgres,
			wantPart: "another table",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := buildM(t, tc.mo, tc.fopts, tc.behaviors)
			err := compileErr(t, md, tc.dialect)
			if err == nil {
				t.Fatalf("Compile: want error containing %q, got nil", tc.wantPart)
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantPart)
			}
		})
	}
}

func TestCompile_NotSearchableIsNoOp(t *testing.T) {
	md := buildM(t, nil, nil) // no searchable fields, no annotation
	cfg, err := aip.ResolveSearchConfig(md)
	if err != nil {
		t.Fatalf("ResolveSearchConfig: %v", err)
	}
	c, err := Compile(cfg, md, DialectPostgres)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c != nil {
		t.Errorf("Compile of a non-searchable resource = %+v, want nil", c)
	}
}

// TestCompile_IndexedStrategy proves an INDEXED resource compiles, surfaces the
// strategy (IsIndexed), and still yields the same PostgresVector the migration's
// generated column and the JIT predicate share (FR-C2/C3).
func TestCompile_IndexedStrategy(t *testing.T) {
	sc := &storagev1.SearchConfig{Strategy: storagev1.SearchConfig_STRATEGY_INDEXED}
	md := buildM(t, searchOpts(sc), map[string]*fieldv1.FieldOptions{"display_name": {Searchable: true}})
	c := mustCompile(t, md, DialectPostgres)
	if c.Strategy != aip.SearchIndexed {
		t.Errorf("Strategy = %v, want INDEXED", c.Strategy)
	}
	if !c.IsIndexed() {
		t.Error("IsIndexed() = false, want true for an INDEXED resource")
	}
	if c.PostgresVector != dnPG {
		t.Errorf("PostgresVector = %q, want %q", c.PostgresVector, dnPG)
	}
}

// TestBuildIndexedMigration proves the emitted migration file set (FR-C2, FM-6):
// a versioned column up/down pair whose up ADDs a GENERATED … STORED tsvector, and
// a CONCURRENTLY GIN index as the SOLE statement in its own up file (+ DROP down),
// numbered columnVersion / columnVersion+1.
func TestBuildIndexedMigration(t *testing.T) {
	files := BuildIndexedMigration("gizmos", "simple", `coalesce("label", '')`, 9001)
	if len(files) != 4 {
		t.Fatalf("want 4 files, got %d", len(files))
	}
	byName := map[string]string{}
	for _, f := range files {
		byName[f.Name] = f.Body
	}
	colUp, ok := byName["9001_gizmos_search_vector.up.sql"]
	if !ok {
		t.Fatalf("missing column up file; got %v", keysOf(byName))
	}
	if !strings.Contains(colUp, "ADD COLUMN search_vector tsvector") ||
		!strings.Contains(colUp, "GENERATED ALWAYS AS (to_tsvector('simple', coalesce(\"label\", ''))) STORED") {
		t.Errorf("column up file lacks the GENERATED tsvector column:\n%s", colUp)
	}
	if _, ok := byName["9001_gizmos_search_vector.down.sql"]; !ok {
		t.Error("missing column down file")
	}
	idxUp, ok := byName["9002_gizmos_search_gin.up.sql"]
	if !ok {
		t.Fatalf("missing index up file; got %v", keysOf(byName))
	}
	if !strings.Contains(idxUp, "CREATE INDEX CONCURRENTLY gizmos_search_gin ON gizmos USING GIN (search_vector)") {
		t.Errorf("index up file lacks the CONCURRENTLY GIN index:\n%s", idxUp)
	}
	// FM-6: the index up file must carry exactly ONE SQL statement (one ';').
	if n := strings.Count(idxUp, ";"); n != 1 {
		t.Errorf("index up file must have exactly one statement, found %d ';':\n%s", n, idxUp)
	}
	idxDown, ok := byName["9002_gizmos_search_gin.down.sql"]
	if !ok {
		t.Fatal("missing index down file")
	}
	if !strings.Contains(idxDown, "DROP INDEX CONCURRENTLY IF EXISTS gizmos_search_gin") {
		t.Errorf("index down file lacks DROP INDEX:\n%s", idxDown)
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestCompile_UnsupportedTargetDialect(t *testing.T) {
	md := buildM(t, nil, map[string]*fieldv1.FieldOptions{"display_name": {Searchable: true}})
	cfg, _ := aip.ResolveSearchConfig(md)
	if _, err := Compile(cfg, md, "mysql"); err == nil {
		t.Fatal("Compile with an unsupported target dialect: want error, got nil")
	}
}

// --- helpers ---

func exprS(name string, exprs ...*storagev1.SearchExpr) *storagev1.SearchSource {
	return &storagev1.SearchSource{
		Name: name,
		From: &storagev1.SearchSource_Exprs{Exprs: &storagev1.SearchExprSet{Expr: exprs}},
	}
}

func equalStrs(a, b []string) bool {
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
