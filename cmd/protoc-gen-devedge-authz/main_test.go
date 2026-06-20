package main

import (
	"strings"
	"testing"
)

// TestAllAuthzRulesLines_multiService verifies the AllAuthzRules aggregate
// concatenates every service's per-service slice in declaration order (F029 D-3).
func TestAllAuthzRulesLines_multiService(t *testing.T) {
	got := strings.Join(allAuthzRulesLines([]string{"OrderService", "ItemService"}, "slices.Concat"), "\n")

	want := "var AllAuthzRules = slices.Concat(\n\tOrderServiceAuthzRules,\n\tItemServiceAuthzRules,\n)"
	if !strings.Contains(got, want) {
		t.Fatalf("AllAuthzRules aggregate = %q, want it to contain %q", got, want)
	}
	if !strings.Contains(got, "AllAuthzRules is every service's rules") {
		t.Fatalf("missing doc comment:\n%s", got)
	}
}

// TestAllAuthzRulesLines_singleService verifies the aggregate is still emitted
// for a single-service file (used as the one wiring reference).
func TestAllAuthzRulesLines_singleService(t *testing.T) {
	got := strings.Join(allAuthzRulesLines([]string{"WidgetService"}, "slices.Concat"), "\n")
	if !strings.Contains(got, "var AllAuthzRules = slices.Concat(\n\tWidgetServiceAuthzRules,\n)") {
		t.Fatalf("single-service aggregate wrong:\n%s", got)
	}
}
