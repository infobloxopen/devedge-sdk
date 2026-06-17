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
	// DeleteFn (optional) soft-deletes the resource as PrincipalA.
	// Used together with ListDeletedFn to verify soft-deleted isolation.
	DeleteFn func(ctx context.Context, id string) error
	// ListDeletedFn (optional) lists soft-deleted resources (show_deleted=true) as PrincipalB.
	// Must return count=0 for isolation to hold. Requires DeleteFn to be set.
	ListDeletedFn func(ctx context.Context) (count int, err error)
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
	// Soft-delete isolation: delete as A, then verify B cannot see the deleted resource.
	if cfg.DeleteFn != nil && cfg.ListDeletedFn != nil {
		if err := cfg.DeleteFn(ctxA, id); err != nil {
			findings = append(findings, Finding{
				Method:   "(delete)",
				Severity: Warning,
				Message:  fmt.Sprintf("DeleteFn returned error: %v", err),
			})
		} else {
			// B must not see A's soft-deleted resource via show_deleted list.
			if count, err := cfg.ListDeletedFn(ctxB); err != nil {
				findings = append(findings, Finding{
					Method:   "(list-deleted)",
					Severity: Warning,
					Message:  fmt.Sprintf("ListDeletedFn returned error: %v", err),
				})
			} else if count > 0 {
				findings = append(findings, Finding{
					Method:   "(list-deleted)",
					Severity: Error,
					Message:  fmt.Sprintf("PrincipalB show_deleted list returned %d soft-deleted item(s) owned by PrincipalA; expected 0", count),
				})
			}
			// B must not see A's soft-deleted resource via direct read.
			if err := cfg.ReadFn(ctxB, id); status.Code(err) != codes.NotFound {
				findings = append(findings, Finding{
					Method:   "(read-deleted)",
					Severity: Error,
					Message:  fmt.Sprintf("PrincipalB read soft-deleted PrincipalA resource (id=%s): expected NotFound, got %v", id, status.Code(err)),
				})
			}
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
	// DB constraint leakage: a raw driver constraint error (e.g. SQLite's
	// "UNIQUE constraint failed: foo_models.name", PostgreSQL's "duplicate key
	// value violates unique constraint", a SQLSTATE code, or a generated
	// "<resource>_models." table name) carries none of the SQL keywords above,
	// so without these the gate would mark a schema-leaking error "clean".
	"constraint failed", "duplicate key", "Duplicate entry",
	"violates ", "SQLSTATE", "_models.",
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
