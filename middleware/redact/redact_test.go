package redact_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/infobloxopen/devedge-sdk/middleware/redact"
	"github.com/infobloxopen/devedge-sdk/internal/testpb/secretpb"
)

// TestMessage_RedactsSecretStringField verifies that redact.Message returns a
// clone where the secret field is "[REDACTED]" and the original is untouched.
func TestMessage_RedactsSecretStringField(t *testing.T) {
	original := &secretpb.SecretMsg{
		Id:          "id-1",
		KeyValue:    "sk_live_abc123",
		PublicValue: "open",
	}

	result := redact.Message(original)
	if result == nil {
		t.Fatal("redact.Message returned nil for non-nil input")
	}

	redacted, ok := result.(*secretpb.SecretMsg)
	if !ok {
		t.Fatalf("expected *secretpb.SecretMsg, got %T", result)
	}

	// Secret field must be replaced.
	if redacted.KeyValue != "[REDACTED]" {
		t.Errorf("expected KeyValue = \"[REDACTED]\", got %q", redacted.KeyValue)
	}

	// Non-secret fields must be preserved.
	if redacted.Id != "id-1" {
		t.Errorf("expected Id = \"id-1\", got %q", redacted.Id)
	}
	if redacted.PublicValue != "open" {
		t.Errorf("expected PublicValue = \"open\", got %q", redacted.PublicValue)
	}

	// Original must be unchanged.
	if original.KeyValue != "sk_live_abc123" {
		t.Errorf("original mutated: KeyValue = %q", original.KeyValue)
	}
}

// TestMessage_LeavesNonSecretFieldsAlone verifies that non-secret fields in
// a message that also has secret fields are not modified.
func TestMessage_LeavesNonSecretFieldsAlone(t *testing.T) {
	original := &secretpb.SecretMsg{
		Id:          "id-2",
		KeyValue:    "secret",
		PublicValue: "stay",
	}

	result := redact.Message(original)
	redacted, ok := result.(*secretpb.SecretMsg)
	if !ok {
		t.Fatalf("expected *secretpb.SecretMsg, got %T", result)
	}

	if redacted.PublicValue != "stay" {
		t.Errorf("PublicValue was modified: got %q", redacted.PublicValue)
	}
	if redacted.Id != "id-2" {
		t.Errorf("Id was modified: got %q", redacted.Id)
	}
}

// TestMessage_NilMessage verifies that redact.Message(nil) returns nil without
// panicking.
func TestMessage_NilMessage(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("redact.Message(nil) panicked: %v", r)
		}
	}()

	result := redact.Message(nil)
	if result != nil {
		t.Errorf("expected nil result for nil input, got %v", result)
	}
}

// TestMessage_RedactsNestedSecretField verifies that redact.Message recursively
// walks nested messages and redacts secret fields in the inner message.
func TestMessage_RedactsNestedSecretField(t *testing.T) {
	original := &secretpb.NestedMsg{
		Outer: "outer-value",
		Inner: &secretpb.SecretMsg{
			Id:          "inner-id",
			KeyValue:    "inner-secret",
			PublicValue: "inner-public",
		},
	}

	result := redact.Message(original)
	if result == nil {
		t.Fatal("redact.Message returned nil for non-nil input")
	}

	redacted, ok := result.(*secretpb.NestedMsg)
	if !ok {
		t.Fatalf("expected *secretpb.NestedMsg, got %T", result)
	}

	// Outer field unchanged.
	if redacted.Outer != "outer-value" {
		t.Errorf("Outer was modified: got %q", redacted.Outer)
	}

	if redacted.Inner == nil {
		t.Fatal("Inner field is nil after redaction")
	}

	// Inner secret field must be redacted.
	if redacted.Inner.KeyValue != "[REDACTED]" {
		t.Errorf("Inner.KeyValue not redacted: got %q", redacted.Inner.KeyValue)
	}

	// Inner non-secret fields preserved.
	if redacted.Inner.PublicValue != "inner-public" {
		t.Errorf("Inner.PublicValue was modified: got %q", redacted.Inner.PublicValue)
	}

	// Original unchanged.
	if original.Inner.KeyValue != "inner-secret" {
		t.Errorf("original Inner.KeyValue mutated: got %q", original.Inner.KeyValue)
	}
}

// TestMessage_NoSecretFields verifies that a message with no secret fields is
// returned as an equal clone without modification.
func TestMessage_NoSecretFields(t *testing.T) {
	original := &secretpb.PlainMsg{
		Name:  "foo",
		Value: "bar",
	}

	result := redact.Message(original)
	if result == nil {
		t.Fatal("redact.Message returned nil for non-nil input")
	}

	plain, ok := result.(*secretpb.PlainMsg)
	if !ok {
		t.Fatalf("expected *secretpb.PlainMsg, got %T", result)
	}

	if !proto.Equal(original, plain) {
		t.Errorf("message with no secret fields was modified: got %v", plain)
	}
}
