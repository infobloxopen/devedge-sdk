package seccheck

import (
	"fmt"
	"testing"

	authzv1 "github.com/infobloxopen/apis/proto/infoblox/authz/v1"
	"github.com/infobloxopen/devedge-sdk/authz"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Severity classifies the urgency of a Finding.
type Severity int

const (
	Notice  Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Notice:
		return "notice"
	case Warning:
		return "warning"
	case Error:
		return "error"
	default:
		return "unknown"
	}
}

// Finding is a single diagnostic produced by a static security check.
type Finding struct {
	Method   string
	Severity Severity
	Message  string
}

// RunT maps findings to testing.TB calls.
// Error+Warning → t.Errorf; Notice → t.Logf.
func RunT(t testing.TB, findings []Finding) {
	t.Helper()
	for _, f := range findings {
		switch f.Severity {
		case Error, Warning:
			t.Errorf("[%s] %s: %s", f.Severity, f.Method, f.Message)
		default:
			t.Logf("[%s] %s: %s", f.Severity, f.Method, f.Message)
		}
	}
}

// AssertNoSecretFieldsLeaked walks each response proto message and returns an
// Error finding for any field annotated (infoblox.authz.v1.field).secret = true
// that contains a non-empty (string) or non-zero (other) value.
func AssertNoSecretFieldsLeaked(responses ...proto.Message) []Finding {
	var findings []Finding
	for _, m := range responses {
		if m == nil {
			continue
		}
		walkForLeaks(m.ProtoReflect(), "", &findings)
	}
	return findings
}

func walkForLeaks(msg protoreflect.Message, prefix string, findings *[]Finding) {
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := string(fd.Name())
		if prefix != "" {
			path = prefix + "." + path
		}
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
			walkForLeaks(v.Message(), path, findings)
			return true
		}
		opts := fd.Options()
		if opts == nil || !proto.HasExtension(opts, authzv1.E_Field) {
			return true
		}
		rule, ok := proto.GetExtension(opts, authzv1.E_Field).(*authzv1.FieldRule)
		if !ok || rule == nil || !rule.GetSecret() {
			return true
		}
		switch fd.Kind() {
		case protoreflect.StringKind:
			s := v.String()
			if s != "" && s != "[REDACTED]" {
				*findings = append(*findings, Finding{
					Method:   path,
					Severity: Error,
					Message:  fmt.Sprintf("secret field %q contains a non-empty value in response", path),
				})
			}
		default:
			if msg.Has(fd) {
				*findings = append(*findings, Finding{
					Method:   path,
					Severity: Error,
					Message:  fmt.Sprintf("secret field %q is non-zero in response", path),
				})
			}
		}
		return true
	})
}

// AssertRulesComplete checks that every non-public rule has a non-empty Verb and Resource.
// An empty rules slice is itself a finding.
func AssertRulesComplete(rules []authz.MethodRule) []Finding {
	if len(rules) == 0 {
		return []Finding{{Method: "(all)", Severity: Error, Message: "no methods declared; authz rules table is empty"}}
	}
	var findings []Finding
	for _, r := range rules {
		if r.Public {
			continue
		}
		if r.Verb == "" {
			findings = append(findings, Finding{
				Method:   r.Method,
				Severity: Error,
				Message:  "rule has empty verb; every non-public method must declare a verb",
			})
		}
		if r.Resource == "" {
			findings = append(findings, Finding{
				Method:   r.Method,
				Severity: Error,
				Message:  "rule has empty resource; every non-public method must declare a resource",
			})
		}
	}
	return findings
}
