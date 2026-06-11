package main

import (
	"strings"
	"testing"
)

// T002: unit tests for renderStorageFile — pure function, no protogen/buf needed.

func TestRenderStorageFile_basic(t *testing.T) {
	msg := messageInfo{
		MessageName: "Widget",
		PbPkgName:   "widgetsv1",
		PbImportPath: "github.com/example/widgets/v1",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "name", GoType: "string", SnakeName: "name"},
			{Name: "weight", GoType: "int32", SnakeName: "weight"},
		},
	}
	out := renderStorageFile("widgetsv1storage", []messageInfo{msg})

	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package widgetsv1storage")
	mustContain(t, out, "type WidgetModel struct")
	mustContain(t, out, `gorm:"primaryKey`)
	mustContain(t, out, `gorm:"column:etag"`)
	mustContain(t, out, "ETag")
	mustContain(t, out, "CreatedAt")
	mustContain(t, out, "UpdatedAt")
	mustContain(t, out, "gorm.DeletedAt")
	mustContain(t, out, "type WidgetRepository struct")
	mustContain(t, out, "NewWidgetRepository")
	mustContain(t, out, "persistence.Repository")
	mustContain(t, out, "func (r *WidgetRepository) Get(")
	mustContain(t, out, "func (r *WidgetRepository) List(")
	mustContain(t, out, "func (r *WidgetRepository) Create(")
	mustContain(t, out, "func (r *WidgetRepository) Update(")
	mustContain(t, out, "func (r *WidgetRepository) Delete(")
	mustContain(t, out, "var _ persistence.Repository")
	mustContain(t, out, "protoc-gen-storage")
}

func TestRenderStorageFile_repeatedFieldSkipped(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Foo",
		PbPkgName:    "foov1",
		PbImportPath: "example/foo",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "tags", GoType: "string", SnakeName: "tags", IsRepeated: true},
		},
	}
	out := renderStorageFile("foov1storage", []messageInfo{msg})
	mustContain(t, out, "TODO: repeated field tags skipped")
}

func TestRenderStorageFile_messageFieldSkipped(t *testing.T) {
	msg := messageInfo{
		MessageName:  "Bar",
		PbPkgName:    "barv1",
		PbImportPath: "example/bar",
		Fields: []fieldInfo{
			{Name: "id", GoType: "string", SnakeName: "id", IsID: true},
			{Name: "meta", GoType: "*SomeMeta", SnakeName: "meta", IsMessage: true},
		},
	}
	out := renderStorageFile("barv1storage", []messageInfo{msg})
	mustContain(t, out, "TODO: nested message meta skipped")
}

func TestRenderStorageFile_noMessages(t *testing.T) {
	out := renderStorageFile("emptystorage", nil)
	if out != "" {
		t.Fatalf("expected empty output for no messages, got:\n%s", out)
	}
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", substr, s)
	}
}
