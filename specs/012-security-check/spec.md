# Feature Specification: security-check

**Feature Branch**: `012-security-check`
**Created**: 2026-06-11
**Status**: Draft

## Context

The framework's security promise — "every RPC is authz-governed, cross-account data never
leaks, errors expose nothing internal" — is only as strong as the tool that enforces it at
every commit. Without a runnable gate, framework invariants drift: a new RPC is added without
a rule, a handler leaks a SQL error, a second account reads the first account's resources.

`security-check` is that gate. It ships as:
1. A **`seccheck/` library** — assertion functions that services embed in a `security_test.go`
   and run with `go test`. The test file is developer-written and lean (as AGENTS.md §2 requires
   — LLM-generated ones hurt); the library gives it expressive, reusable primitives.
2. A **`cmd/security-check` binary** — wraps the static assertions for CI use against a proto
   FileDescriptorSet (from `buf build`) without requiring a running service.

**Scope: assertions reachable with the current stack.** Two of the five vision §3.5 families
require Portunus/ib-stk infrastructure not available in the public SDK:
- §3.5-1 (S2S token / `ib-stk`) → Portunus-gated, **out of scope**
- §3.5-4 (token-audience confusion / `ib-ctk` vs `ib-stk`) → Portunus-gated, **out of scope**

Three families are fully achievable with DevAuthorizer and are **in scope**:
- §3.5-2 **Missing authz** (static: every RPC has a rule; boot gate already enforces at runtime)
- §3.5-3 **Cross-account leak** (dynamic: principal A cannot read principal B's resources)
- §3.5-5 **Verbose error** (dynamic: DB/input errors produce clean gRPC status messages)

Plus a pure-static check not in the original five but required by the framework: **authz
annotation completeness** against the proto descriptor (catches RPCs added to proto but missing
from the generated rules table before the service is ever run).

## Clarifications

- **Library vs binary split**: `seccheck/` is the library (all assertion logic); `cmd/security-check`
  is a thin CLI wrapping only the static checks (binary → proto descriptor comparison). Dynamic
  checks require a live `*grpc.ClientConn` and belong in service-specific security tests.
- **Static check target**: `seccheck.AssertRulesComplete(rules []authz.MethodRule)` validates the
  rules slice itself (non-empty Verb+Resource or Public:true for every entry). The binary check
  (`security-check --descriptor`) cross-references a proto FileDescriptorSet against a compiled
  rules JSON file, catching RPCs that exist in proto but are absent from the rules table.
- **Dynamic principal model**: all dynamic assertions use `authz.DevAuthorizer`. The
  cross-account test constructs two separate DevAuthorizer configurations and two clients. No
  JWT, no Portunus.
- **Cross-account isolation test**: caller provides a `seccheck.IsolationConfig` with a
  `CreateFn` (create a resource as principal A) and a `ReadFn` + `ListFn` (try to access as
  principal B). The library calls these and asserts `codes.NotFound` / empty list.
- **Verbose error test**: caller provides `seccheck.ErrorTrigger` functions that each expect to
  produce a specific gRPC error code. The library calls each, inspects the status message for
  forbidden strings (SQL keywords, `"persistence:"` prefix, file paths, stack-trace markers),
  and fails if any appear.
- **`public: true` exemption**: a rule with `Public: true` is explicitly reviewed and may opt
  out of authz. The static checker accepts it; it logs it as a notice (not a failure) so
  reviewers can audit the list.
- **Output format**: findings are `seccheck.Finding{Method, Severity, Message}` slices returned
  from every check function. Callers decide whether to `t.Error` or `t.Fatal`. The CLI binary
  prints findings to stdout and exits non-zero if any `Severity == Error`.
- **Secret-field annotation**: not in scope (would require a new `authz.proto` field-level
  annotation and a new canonical release). Noted in the spec as the natural next static check.
- **`make security-check` in Makefile**: add a target that runs `go test ./... -run Security`
  so the verify gate can invoke it with one command.

## User Scenarios & Testing

### User Story 1 — Static: undeclared RPC caught before the service runs (P1) 🎯 MVP

A developer adds a new `RPC BulkDeleteWidgets` to their proto service and regenerates code.
They run `security-check --descriptor widget.binpb --rules widget.rules.json` in CI. The tool
reports `BulkDeleteWidgets` is undeclared and exits non-zero — before the service is ever
deployed.

**Acceptance Scenarios**:

1. **Given** a rules JSON that covers 4 of 5 methods, **When** `security-check` is run against
   a descriptor containing all 5, **Then** it exits non-zero and names the undeclared method.
2. **Given** all 5 methods are covered, **When** `security-check` is run, **Then** it exits 0.
3. **Given** a rule with empty Verb, **When** `seccheck.AssertRulesComplete(rules)` is called,
   **Then** it returns a finding naming the method and field.

**Independent Test**: unit test of `AssertRulesComplete` and the binary's descriptor comparison.

---

### User Story 2 — Dynamic: unknown principal denied on every RPC (P1)

A developer runs `seccheck.AssertUnknownPrincipalDenied(ctx, t, conn, rules)` against their
running service. Any RPC that passes a request through without checking authz causes the test
to fail.

**Acceptance Scenarios**:

1. **Given** a service whose authz interceptor has a DevAuthorizer with no grants, **When**
   `AssertUnknownPrincipalDenied` is called, **Then** every non-public RPC returns
   `codes.PermissionDenied`.
2. **Given** one RPC is marked `Public: true`, **When** the assertion runs, **Then** that RPC
   is skipped (public methods are exempt from the denial check).
3. **Given** a service with a broken interceptor that accidentally allows all traffic, **When**
   `AssertUnknownPrincipalDenied` is called, **Then** a finding is returned for each
   unprotected method.

**Independent Test**: integration test against the toy WidgetService with DevAuthorizer(no grants).

---

### User Story 3 — Dynamic: cross-account resource isolation (P2)

A developer runs `seccheck.AssertCrossAccountIsolation(ctx, t, cfg)` to prove that resources
created by account A are invisible to account B.

**Acceptance Scenarios**:

1. **Given** principal A creates a widget, **When** principal B calls `GetWidget` with A's ID,
   **Then** `codes.NotFound` is returned (B cannot see A's resource).
2. **Given** principal A creates two widgets, **When** principal B calls `ListWidgets`, **Then**
   the list is empty (B sees only its own resources; the toy's MemoryRepository is global, so
   this test validates that the service correctly scopes queries by tenant — the developer must
   implement scoping in their handler; the assertion catches cases where they forget).
3. **Note**: the toy MemoryRepository is NOT scoped by tenant (it's a dev backend). This
   scenario tests the *framework contract* — a service using it in dev mode may fail US3 until
   tenant-scoped reads are implemented. The test is therefore a *red gate* for services that
   haven't implemented isolation yet, which is intentional.

**Independent Test**: integration test with two DevAuthorizer principals and the toy handler.

---

### User Story 4 — Dynamic: error messages expose no internal details (P2)

A developer runs `seccheck.AssertErrorMessagesClean(ctx, t, triggers)` to verify that error
responses from their service don't leak SQL, stack traces, or internal paths.

**Acceptance Scenarios**:

1. **Given** a `GetWidget("nonexistent-id")` call that triggers `ErrNotFound` → `codes.NotFound`,
   **When** the response status message is inspected, **Then** it does not contain `"persistence:"`,
   SQL keywords (`SELECT`, `WHERE`, `ERROR:`), or file-path separators (`/Users`, `/home`, `/app`).
2. **Given** a `CreateWidget` that triggers `ErrConflict` → `codes.AlreadyExists`, **When**
   inspected, **Then** message is clean.
3. **Given** a custom error that a handler wraps with SQL text, **When** the trigger fires,
   **Then** a finding is returned naming the method and the leaked substring.

**Independent Test**: unit test of the clean-message checker function + integration test
with intentionally leaky and clean handlers.

---

### User Story 5 — Toy security test: full suite in one `go test` run (P1)

`testdata/toy/security_test.go` runs all three dynamic seccheck assertions (US2, US3, US4)
against the toy WidgetService server. It passes as-is for US2 and US4; US3 fails with a
finding (MemoryRepository is not tenant-scoped — this is the expected, documented outcome).

**Independent Test**: `cd testdata/toy && go test -run Security -v`

---

### Edge Cases

- Empty rules slice → `AssertRulesComplete` returns a finding (no methods declared at all).
- `AssertUnknownPrincipalDenied` on a service with streaming RPCs → skip streams (framework
  currently has no stream interceptor wired; note as a finding with Severity=Notice).
- CLI binary with a descriptor that has no `(infoblox.authz.v1.rule)` annotations → every RPC
  is reported as undeclared.
- A rule with `Verb: ""` and `Public: false` → static check reports it as incomplete.

## Requirements

### Functional Requirements

- **FR-001**: `seccheck.AssertRulesComplete(rules []authz.MethodRule) []Finding` MUST return a
  finding for every rule where `Verb == ""` and `Public == false`, and for every rule where
  `Resource == ""` and `Public == false`.
- **FR-002**: `seccheck.AssertUnknownPrincipalDenied(ctx, conn, rules, callFns map[string]CallFn) []Finding`
  MUST call each `callFn` (keyed by full method name) with a context carrying a principal that
  has no grants in any DevAuthorizer; MUST return a finding for each method that does NOT return
  `codes.PermissionDenied` (unless `Public: true` in the rule).
- **FR-003**: `seccheck.AssertCrossAccountIsolation(ctx, cfg IsolationConfig) []Finding` MUST
  call `cfg.CreateFn(ctxA)`, then call `cfg.ReadFn(ctxB, id)` and `cfg.ListFn(ctxB)`. If
  `ReadFn` does not return `codes.NotFound` or `ListFn` returns a non-empty list, MUST return a
  finding.
- **FR-004**: `seccheck.AssertErrorMessagesClean(ctx, triggers []ErrorTrigger) []Finding` MUST
  call each trigger, extract the gRPC status message, and scan for forbidden substrings:
  `"persistence:"`, `"SELECT"`, `"INSERT"`, `"UPDATE"` (SQL), `"WHERE"`, `"ERROR:"`, `"/home/"`,
  `"/Users/"`, `"/app/"`, `"goroutine "` (stack trace marker). MUST return a finding for each
  match naming the trigger method and the leaked substring.
- **FR-005**: `seccheck.Finding` MUST have fields `Method string`, `Severity Severity`,
  `Message string`. `Severity` is an enum: `Notice`, `Warning`, `Error`.
- **FR-006**: `seccheck.RunT(t testing.TB, findings []Finding)` MUST call `t.Errorf` for each
  finding with `Severity >= Warning` and log notices with `t.Logf`. This is the standard adapter
  from `[]Finding` to a `testing.T`.
- **FR-007**: `cmd/security-check` binary MUST accept `--descriptor <path>` (proto
  FileDescriptorSet binary) and `--rules <path>` (JSON file containing `[]MethodRule` as
  produced by `encoding/json` on the generated rules table). It MUST cross-reference them:
  for each service RPC in the descriptor that lacks a `(infoblox.authz.v1.rule)` annotation
  AND is absent from the rules JSON, emit a finding. Exit 0 if no Error-severity findings.
- **FR-008**: `Makefile` MUST add a `security-check` target: `go test ./... -run Security -v`.

### Key Entities

- **`Finding`**: `{Method string, Severity Severity, Message string}`.
- **`CallFn`**: `func(ctx context.Context) error` — a function that calls one RPC and returns
  the error (caller extracts the gRPC status from it).
- **`IsolationConfig`**: `{CreateFn func(ctx) (id string, err error), ReadFn func(ctx, id string) error, ListFn func(ctx) (int, error)}`.
- **`ErrorTrigger`**: `{Method string, Fn func(ctx context.Context) error}`.

## Success Criteria

- **SC-001**: `seccheck.AssertRulesComplete` returns findings for rules with empty Verb/Resource
  (unit test; table-driven).
- **SC-002**: `seccheck.AssertUnknownPrincipalDenied` against the toy WidgetService with a
  zero-grant DevAuthorizer returns zero findings (all 5 RPCs return PermissionDenied).
- **SC-003**: `seccheck.AssertErrorMessagesClean` against the toy WidgetService returns zero
  findings (ErrorMapper strips internal details).
- **SC-004**: `security-check --descriptor` exits non-zero when any service RPC lacks an authz
  annotation AND is absent from the rules file (unit test with a synthetic descriptor).
- **SC-005**: `testdata/toy/security_test.go` runs with `go test -run Security -v` and passes
  for US2 + US4; for US3 it records findings (expected — MemoryRepository is not tenant-scoped)
  but does not `t.Fatal` (uses `t.Log` for the US3 result to document the known gap).
- **SC-006**: `go build ./...` and `go vet ./...` clean; `make test` green; `make security-check`
  runs without error.
