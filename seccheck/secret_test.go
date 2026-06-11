package seccheck

import (
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/internal/testpb/secretpb"
)

// TestAssertNoSecretFieldsLeaked_Clean verifies that a response with an empty
// secret field produces zero findings (field cleared → no leak).
func TestAssertNoSecretFieldsLeaked_Clean(t *testing.T) {
	resp := &secretpb.SecretMsg{
		Id:          "id-1",
		KeyValue:    "", // cleared — correct post-create behaviour
		PublicValue: "open",
	}

	findings := AssertNoSecretFieldsLeaked(resp)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty secret field, got %d: %+v", len(findings), findings)
	}
}

// TestAssertNoSecretFieldsLeaked_Leaks verifies that a response containing a
// non-empty secret field produces an Error finding that names the field.
func TestAssertNoSecretFieldsLeaked_Leaks(t *testing.T) {
	resp := &secretpb.SecretMsg{
		Id:          "id-2",
		KeyValue:    "sk_live_abc123", // leaked — should never appear in Get/List response
		PublicValue: "open",
	}

	findings := AssertNoSecretFieldsLeaked(resp)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for leaked secret field, got %d: %+v", len(findings), findings)
	}

	f := findings[0]
	if f.Severity != Error {
		t.Errorf("expected Error severity, got %s", f.Severity)
	}

	// The finding must name the field so the caller can identify the leak.
	if !strings.Contains(f.Message, "key_value") {
		t.Errorf("finding message should mention the field name \"key_value\", got: %q", f.Message)
	}
}

// TestAssertNoSecretFieldsLeaked_NoSecretFields verifies that a message with no
// secret-annotated fields at all returns zero findings regardless of field values.
func TestAssertNoSecretFieldsLeaked_NoSecretFields(t *testing.T) {
	resp := &secretpb.PlainMsg{
		Name:  "foo",
		Value: "bar",
	}

	findings := AssertNoSecretFieldsLeaked(resp)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for message with no secret fields, got %d: %+v", len(findings), findings)
	}
}
