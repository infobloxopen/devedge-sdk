package seccheck

import (
	"fmt"
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// mockTB records Errorf and Logf calls for assertion in tests.
type mockTB struct {
	testing.TB // embed to satisfy interface; will panic if unexpected methods called
	errors     []string
	logs       []string
}

func (m *mockTB) Helper() {}
func (m *mockTB) Errorf(format string, args ...any) {
	m.errors = append(m.errors, fmt.Sprintf(format, args...))
}
func (m *mockTB) Logf(format string, args ...any) {
	m.logs = append(m.logs, fmt.Sprintf(format, args...))
}

func TestAssertRulesComplete_EmptyVerb(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/DoThing", Verb: "", Resource: "zone"},
	}
	findings := AssertRulesComplete(rules)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Method != "/svc/DoThing" {
		t.Errorf("unexpected method %q", f.Method)
	}
	if f.Severity != Error {
		t.Errorf("expected Error severity, got %s", f.Severity)
	}
}

func TestAssertRulesComplete_EmptyResource(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/DoThing", Verb: "read", Resource: ""},
	}
	findings := AssertRulesComplete(rules)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Method != "/svc/DoThing" {
		t.Errorf("unexpected method %q", f.Method)
	}
	if f.Severity != Error {
		t.Errorf("expected Error severity, got %s", f.Severity)
	}
}

func TestAssertRulesComplete_PublicExempt(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/Health", Public: true, Verb: "", Resource: ""},
	}
	findings := AssertRulesComplete(rules)
	if len(findings) != 0 {
		t.Errorf("expected no findings for public method, got %d: %+v", len(findings), findings)
	}
}

func TestAssertRulesComplete_AllValid(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/CreateZone", Verb: "create", Resource: "zone"},
		{Method: "/svc/GetZone", Verb: "read", Resource: "zone"},
		{Method: "/svc/Health", Public: true},
	}
	findings := AssertRulesComplete(rules)
	if len(findings) != 0 {
		t.Errorf("expected no findings for valid rules, got %d: %+v", len(findings), findings)
	}
}

func TestAssertRulesComplete_EmptySlice(t *testing.T) {
	findings := AssertRulesComplete(nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for empty slice, got %d", len(findings))
	}
	f := findings[0]
	if f.Severity != Error {
		t.Errorf("expected Error severity, got %s", f.Severity)
	}
	if f.Method != "(all)" {
		t.Errorf("expected method '(all)', got %q", f.Method)
	}
}

func TestRunT_ErrorCallsTErrorf(t *testing.T) {
	mock := &mockTB{}
	findings := []Finding{
		{Method: "/svc/DoThing", Severity: Error, Message: "empty verb"},
	}
	RunT(mock, findings)
	if len(mock.errors) != 1 {
		t.Fatalf("expected 1 Errorf call, got %d", len(mock.errors))
	}
	if len(mock.logs) != 0 {
		t.Errorf("expected no Logf calls, got %d", len(mock.logs))
	}
}

func TestRunT_NoticeCallsTLogf(t *testing.T) {
	mock := &mockTB{}
	findings := []Finding{
		{Method: "/svc/DoThing", Severity: Notice, Message: "informational note"},
	}
	RunT(mock, findings)
	if len(mock.logs) != 1 {
		t.Fatalf("expected 1 Logf call, got %d", len(mock.logs))
	}
	if len(mock.errors) != 0 {
		t.Errorf("expected no Errorf calls, got %d", len(mock.errors))
	}
}
