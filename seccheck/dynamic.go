package seccheck

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// CallFn makes one gRPC call and returns its error.
type CallFn func(ctx context.Context) error

// AssertUnknownPrincipalDenied verifies that every non-public method denies
// a principal with no grants. The caller provides a CallFn for each method
// (keyed by full method name, e.g. "/toy.v1.WidgetService/CreateWidget").
// Methods with Public:true are skipped.
func AssertUnknownPrincipalDenied(
	ctx context.Context,
	rules []authz.MethodRule,
	calls map[string]CallFn,
) []Finding {
	const unknownPrincipal = "__seccheck_unknown__"
	callCtx := metadata.AppendToOutgoingContext(ctx, "account-id", unknownPrincipal)

	var findings []Finding
	for _, r := range rules {
		if r.Public {
			continue
		}
		fn, ok := calls[r.Method]
		if !ok || fn == nil {
			findings = append(findings, Finding{
				Method:   r.Method,
				Severity: Notice,
				Message:  "no CallFn provided; method skipped",
			})
			continue
		}
		err := fn(callCtx)
		if status.Code(err) != codes.PermissionDenied {
			findings = append(findings, Finding{
				Method:   r.Method,
				Severity: Error,
				Message:  fmt.Sprintf("expected PermissionDenied for unknown principal, got %v", status.Code(err)),
			})
		}
	}
	return findings
}

// IsolationConfig describes a cross-account isolation test.
type IsolationConfig struct {
	PrincipalA string
	PrincipalB string
	// CreateFn creates a resource as PrincipalA and returns its ID.
	CreateFn func(ctx context.Context) (id string, err error)
	// ReadFn attempts to read the resource by ID as PrincipalB.
	// Must return codes.NotFound for isolation to hold.
	ReadFn func(ctx context.Context, id string) error
	// ListFn (optional) lists resources as PrincipalB.
	// Must return count=0 for isolation to hold.
	ListFn func(ctx context.Context) (count int, err error)
}

// AssertCrossAccountIsolation verifies that resources created by PrincipalA
// are not visible to PrincipalB.
func AssertCrossAccountIsolation(ctx context.Context, cfg IsolationConfig) []Finding {
	ctxA := metadata.AppendToOutgoingContext(ctx, "account-id", cfg.PrincipalA)
	ctxB := metadata.AppendToOutgoingContext(ctx, "account-id", cfg.PrincipalB)

	id, err := cfg.CreateFn(ctxA)
	if err != nil {
		return []Finding{{
			Method:   "(create)",
			Severity: Warning,
			Message:  fmt.Sprintf("CreateFn returned error: %v", err),
		}}
	}

	var findings []Finding
	if err := cfg.ReadFn(ctxB, id); status.Code(err) != codes.NotFound {
		findings = append(findings, Finding{
			Method:   "(read)",
			Severity: Error,
			Message:  fmt.Sprintf("PrincipalB read PrincipalA resource (id=%s): expected NotFound, got %v", id, status.Code(err)),
		})
	}
	if cfg.ListFn != nil {
		count, err := cfg.ListFn(ctxB)
		if err != nil {
			findings = append(findings, Finding{
				Method:   "(list)",
				Severity: Warning,
				Message:  fmt.Sprintf("ListFn returned error: %v", err),
			})
		} else if count > 0 {
			findings = append(findings, Finding{
				Method:   "(list)",
				Severity: Error,
				Message:  fmt.Sprintf("PrincipalB list returned %d item(s) owned by PrincipalA; expected 0", count),
			})
		}
	}
	return findings
}

// ErrorTrigger pairs a method name with a function that should produce an error.
type ErrorTrigger struct {
	Method string
	Fn     func(ctx context.Context) error
}

// forbiddenSubstrings are strings that must not appear in gRPC error messages.
var forbiddenSubstrings = []string{
	"persistence:", "SELECT ", "INSERT ", "UPDATE ", "WHERE ", "ERROR:",
	"/home/", "/Users/", "/app/", "goroutine ", ".go:",
}

// AssertErrorMessagesClean verifies that error responses do not leak internal details.
func AssertErrorMessagesClean(ctx context.Context, triggers []ErrorTrigger) []Finding {
	var findings []Finding
	for _, trig := range triggers {
		err := trig.Fn(ctx)
		if err == nil {
			findings = append(findings, Finding{
				Method:   trig.Method,
				Severity: Warning,
				Message:  "trigger returned nil error (expected an error)",
			})
			continue
		}
		msg := status.Convert(err).Message()
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(msg, forbidden) {
				findings = append(findings, Finding{
					Method:   trig.Method,
					Severity: Error,
					Message:  fmt.Sprintf("error message leaks %q: %q", forbidden, msg),
				})
			}
		}
	}
	return findings
}
