package resourcename_test

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/persistence/resourcename"
)

func TestParse_flat(t *testing.T) {
	vars, err := resourcename.Parse("widgets/{widget}", "widgets/abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["widget"] != "abc123" {
		t.Errorf("got %q, want %q", vars["widget"], "abc123")
	}
}

func TestParse_hierarchical(t *testing.T) {
	vars, err := resourcename.Parse("projects/{project}/widgets/{widget}", "projects/p1/widgets/w2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["project"] != "p1" {
		t.Errorf("project: got %q, want p1", vars["project"])
	}
	if vars["widget"] != "w2" {
		t.Errorf("widget: got %q, want w2", vars["widget"])
	}
}

func TestParse_segmentCountMismatch(t *testing.T) {
	_, err := resourcename.Parse("widgets/{widget}", "widgets/abc/extra")
	if err == nil {
		t.Fatal("expected error for segment count mismatch")
	}
}

func TestParse_literalMismatch(t *testing.T) {
	_, err := resourcename.Parse("widgets/{widget}", "things/abc123")
	if err == nil {
		t.Fatal("expected error for literal segment mismatch")
	}
}

func TestFormat_flat(t *testing.T) {
	name, err := resourcename.Format("widgets/{widget}", map[string]string{"widget": "abc123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "widgets/abc123" {
		t.Errorf("got %q, want %q", name, "widgets/abc123")
	}
}

func TestFormat_hierarchical(t *testing.T) {
	name, err := resourcename.Format("projects/{project}/widgets/{widget}", map[string]string{
		"project": "p1",
		"widget":  "w2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "projects/p1/widgets/w2" {
		t.Errorf("got %q, want %q", name, "projects/p1/widgets/w2")
	}
}

func TestFormat_missingVar(t *testing.T) {
	_, err := resourcename.Format("widgets/{widget}", map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing variable")
	}
}

func TestFormat_roundTrip(t *testing.T) {
	pattern := "projects/{project}/apikeys/{api_key}"
	original := "projects/proj-1/apikeys/key-abc"
	vars, err := resourcename.Parse(pattern, original)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rebuilt, err := resourcename.Format(pattern, vars)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if rebuilt != original {
		t.Errorf("round-trip mismatch: got %q, want %q", rebuilt, original)
	}
}

func TestIDFromName_flat(t *testing.T) {
	id, err := resourcename.IDFromName("widgets/{widget}", "widgets/abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "abc123" {
		t.Errorf("got %q, want abc123", id)
	}
}

func TestIDFromName_hierarchical(t *testing.T) {
	id, err := resourcename.IDFromName("projects/{project}/widgets/{widget}", "projects/p1/widgets/w99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "w99" {
		t.Errorf("got %q, want w99", id)
	}
}

func TestIDFromName_invalidName(t *testing.T) {
	_, err := resourcename.IDFromName("widgets/{widget}", "things/abc")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIDVarName(t *testing.T) {
	cases := []struct {
		pattern string
		want    string
	}{
		{"widgets/{widget}", "widget"},
		{"projects/{project}/apikeys/{api_key}", "api_key"},
	}
	for _, tc := range cases {
		got := resourcename.IDVarName(tc.pattern)
		if got != tc.want {
			t.Errorf("IDVarName(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
