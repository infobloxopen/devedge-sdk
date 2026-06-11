package seccheck

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/authz"
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
