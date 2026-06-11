# Tasks: security-check

**Branch**: `012-security-check`
**Spec**: `specs/012-security-check/spec.md`
**Plan**: `specs/012-security-check/plan.md`

---

## Phase 1: Tests (red first)

- [ ] T001 [S] Write `seccheck/seccheck_test.go` — unit tests for static assertions (must be
  red until T002):
  - `TestAssertRulesComplete_EmptyVerb`: rule with `Verb: ""`, `Public: false` → finding naming
    method and "verb".
  - `TestAssertRulesComplete_EmptyResource`: rule with `Resource: ""`, `Public: false` → finding
    naming "resource".
  - `TestAssertRulesComplete_PublicExempt`: rule with `Public: true`, empty Verb/Resource → no
    finding (public methods are exempt).
  - `TestAssertRulesComplete_AllValid`: 3 fully-declared rules → no findings.
  - `TestAssertRulesComplete_EmptySlice`: empty rules → one finding ("no methods declared").
  - `TestRunT_ErrorCallsTErrorf`: pass a `Finding{Severity: Error}` to `RunT` with a mock
    `testing.TB` (use a `*mockTB` that records calls) → assert `t.Errorf` was called.
  - `TestRunT_NoticeCallsTLogf`: `Finding{Severity: Notice}` → `t.Logf` called, not `t.Errorf`.

- [ ] T002 [S] Create stub `seccheck/seccheck.go` (package declaration + exported types only,
  no logic) and implement to make T001 green. Then write `seccheck/dynamic_test.go` — unit
  tests for dynamic assertions (must be red until T003):
  - `TestAssertUnknownPrincipalDenied_AllDenied`: mock `CallFn` always returns
    `status.Error(codes.PermissionDenied, "")` → zero findings.
  - `TestAssertUnknownPrincipalDenied_OnePasses`: one `CallFn` returns nil (success) → finding
    for that method with `Severity: Error`.
  - `TestAssertUnknownPrincipalDenied_PublicSkipped`: rule has `Public: true` → no call made,
    zero findings.
  - `TestAssertErrorMessagesClean_Clean`: trigger returns `status.Error(codes.NotFound, "not found")`
    → zero findings.
  - `TestAssertErrorMessagesClean_LeaksPersistencePrefix`: trigger returns
    `status.Error(codes.NotFound, "persistence: not found")` → finding with leaked substring.
  - `TestAssertErrorMessagesClean_LeaksSQLKeyword`: `"WHERE id = 'foo'"` in message → finding.
  - `TestAssertErrorMessagesClean_UnexpectedSuccess`: trigger returns nil → `Warning` finding.

---

## Phase 2: Implementation

- [ ] T003 [S] Implement `seccheck/seccheck.go` fully (FR-001/005/006):
  - `Severity` type + constants (`Notice=0, Warning=1, Error=2`); `String()` method.
  - `Finding` struct.
  - `RunT(t testing.TB, findings []Finding)` — `t.Errorf` for Warning+Error, `t.Logf` for Notice.
  - `AssertRulesComplete(rules []authz.MethodRule) []Finding` — iterate rules; emit finding for
    each where `!rule.Public && (rule.Verb == "" || rule.Resource == "")`; emit one finding for
    empty slice.
  Run T001 — all must pass.

- [ ] T004 [S] Implement `seccheck/dynamic.go` (FR-002/003/004):
  - `CallFn` type alias + `AssertUnknownPrincipalDenied(ctx, rules, calls) []Finding`:
    for each non-public rule, inject `account-id: __seccheck_unknown__` via
    `metadata.AppendToOutgoingContext`; call `calls[rule.Method](ctx)`; if
    `status.Code(err) != codes.PermissionDenied` → Error finding.
    If `calls[rule.Method]` is nil → Notice finding ("no CallFn provided, skipped").
  - `IsolationConfig` + `AssertCrossAccountIsolation(ctx, cfg) []Finding`:
    build ctxA/ctxB with respective `account-id` metadata; call `cfg.CreateFn(ctxA)` to get id;
    call `cfg.ReadFn(ctxB, id)` — if not `codes.NotFound` → Error finding;
    if `cfg.ListFn != nil`: call `cfg.ListFn(ctxB)` — if count > 0 → Error finding.
  - `ErrorTrigger` + `forbiddenSubstrings` var + `AssertErrorMessagesClean(ctx, triggers) []Finding`:
    for each trigger call `trigger.Fn(ctx)`; extract message via `status.FromError(err).Message()`;
    if err == nil → Warning finding; else scan for each forbidden substring → Error finding per hit.
  Run T002 — all dynamic tests must pass.

---

## Phase 3: CLI binary

- [ ] T005 [S] Implement `cmd/security-check/main.go` (FR-007):
  - Flags: `--descriptor string` (required), `--rules string` (optional; if absent, only check
    for `(infoblox.authz.v1.rule)` presence in proto annotations).
  - Parse descriptor: `os.ReadFile(descriptorPath)` → `proto.Unmarshal` into
    `*descriptorpb.FileDescriptorSet` → `protodesc.NewFiles`.
  - If `--rules` provided: `json.Unmarshal` into `[]authz.MethodRule`; build a set of method names.
  - Iterate all services in the descriptor: for each RPC, check if
    `proto.HasExtension(method.Options(), authzv1.E_Rule)` OR method full name is in rules set.
    Neither → Error finding.
  - Print findings to stdout (`fmt.Printf("[ERROR] %s: %s\n", f.Method, f.Message)`).
  - Exit 1 if any `Severity >= Error`, else 0.
  Write a unit test `cmd/security-check/main_test.go` using a synthetic FileDescriptorSet built
  with `protodesc` from the toy's generated descriptor. Run `go test ./cmd/security-check/... -count=1`.

---

## Phase 4: Toy security test + Makefile

- [ ] T006 [S] Write `testdata/toy/security_test.go` (SC-005):
  Package `widgetsv1_test`. Reuses `newTestServer` pattern from `server_test.go`.

  `TestSecurity_AuthZ` (US2): boot server with zero-grant DevAuthorizer; build `calls` map from
  gRPC client stubs for all 5 methods (each making a minimal valid request); call
  `seccheck.AssertUnknownPrincipalDenied(ctx, widgetsv1.WidgetServiceAuthzRules, calls)`;
  `seccheck.RunT(t, findings)` — expect zero findings.

  `TestSecurity_VerboseErrors` (US4): boot server with full-grant DevAuthorizer; build triggers
  for GetWidget("nonexistent") → NotFound, CreateWidget(duplicate) → AlreadyExists;
  `seccheck.AssertErrorMessagesClean(ctx, triggers)`;
  `seccheck.RunT(t, findings)` — expect zero findings.

  `TestSecurity_CrossAccountIsolation` (US3): boot server; create a widget as "alice"; build
  isolation config with ReadFn + ListFn as "bob"; call `AssertCrossAccountIsolation`;
  — MemoryRepository is NOT tenant-scoped, so findings WILL be non-empty; use `t.Logf` to
  document: `t.Logf("cross-account isolation findings (expected — MemoryRepository not scoped): %v", findings)`.
  Do NOT call `RunT` for US3 in the toy (this is the documented known gap).

  Run `cd testdata/toy && go test -run Security -v -count=1 -timeout 30s`.

- [ ] T007 [S] Add `security-check` target to `Makefile`:
  ```makefile
  security-check: ## Run security assertions (go test -run Security)
  	go test ./... -run Security -v
  ```
  Run `make security-check` — must exit 0.

---

## Phase 5: Verify + commit

- [ ] T008 [S] `go build ./... && go vet ./... && make test` from the repo root — all clean (SC-006).

- [ ] T009 [S] `cd testdata/toy && go test -run Security -v -count=1 -timeout 30s` — SC-005 passes.

- [ ] T010 [S] Commit all: spec + plan + tasks + implementation.
  Message: `012: security-check — static authz completeness + dynamic §3.5 assertions`.

---

## Dependencies & Execution Order

- T001 (red) → T003 (green T001)
- T002 (red) → T004 (green T002 dynamic tests)
- T003 + T004 → T005 (CLI uses seccheck)
- T003 + T004 → T006 (toy test uses seccheck)
- T006 → T007 → T008 → T009 → T010

## Complexity Tags

| Task | Tag | Reason |
|------|-----|--------|
| T001 | [S] | Table-driven unit tests for static assertions |
| T002 | [S] | Stub + unit tests for dynamic assertions with mock CallFns |
| T003 | [S] | Static assertion logic: iterate + condition check (~40 LOC) |
| T004 | [S] | Dynamic assertions: metadata injection + status code checks (~80 LOC) |
| T005 | [S] | CLI: flag parsing + proto descriptor iteration (~60 LOC) |
| T006 | [S] | Integration test: reuses newTestServer pattern, mechanical |
| T007 | [S] | One-line Makefile target |
| T008 | [S] | Run commands, check output |
| T009 | [S] | Run security test |
| T010 | [S] | Git commit |
