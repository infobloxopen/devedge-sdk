# F028 — Tasks (apx-native service scaffold)

Lifecycle tags: **[S]** = simple/mechanical (Sonnet subagent) · **[C]** = complex (Opus).
Gate: **🔒F027** = blocked on F027 (`feat/027-repo-adapter-codegen`) being on `main` (or rebased in) —
the `--backend ent` path consumes the generated ent adapter.

Reference layout to template from: `testdata/apikey/`. Decisions: see spec `## Design decisions` (D-1..D-5).

**Status (gorm path):** Phases 1, 2, 3 SHIPPED on `feat/028-apx-native-scaffold`
(`cmd/devedge-sdk`); docs T-601/T-602 done. AC-001/002/004/005-gorm/006/007 proven by the
in-repo integration tests; AC-003 (`apx lint`/`config validate`) asserted in the pipeline.

**Status (ent path — Phase 4, SHIPPED):** `--backend ent` is implemented and green. The ent
`buf.gen.yaml` wires the six SDK/public plugins INCLUDING `protoc-gen-ent` (no `module=` opt)
and EXCLUDING `protoc-gen-storage` (so an ent service is gorm-free). The scaffold runs the ent
TWO-STEP generate — `buf generate` (protoc-gen-ent emits schemas + F027's
`New<R>EntRepository` adapter) then `go generate ./gen/ent` (entc client), with a `go mod tidy
-e` seed between them to break the ordering hazard. A build-tagged `tools.go` pins entc. Proven
by `TestScaffold_ENT_BuildsAndPasses` (scaffold → buf → entc → `go build/vet/test` incl. the
CRUD smoke, ZERO hand-written ent_wiring) and `TestScaffold_ENT_Boundary`. AC-005-ent ✓.

**Status (Phase 5 — apx governance, SHIPPED):** `TestScaffold_APXGovernance` asserts, on a
freshly scaffolded service's PUBLIC proto: `apx config validate` (AC-006), `apx lint`,
`apx breaking --against HEAD` (the trivial "empty baseline" — a new API compared to itself;
`--against <empty dir>` fails inside buf with "no .proto files"), `apx release prepare
--dry-run`, and `apx policy check` against `forbidden_proto_options: ^gorm\.` (AC-003/004). All
pass.

**go_package warning — RESOLVED as documented (genuinely unavoidable without `--strict`):**
`apx release prepare --dry-run` still emits a NON-FATAL `go_package mismatch` warning (got
`<mod>/gen/<svc>v1`, expected `<mod>/proto/<svc>/v1`). It cannot be cleared without `--strict`
because the two constraints are mutually exclusive:
  - apx derives the expected `go_package` rigidly as `<module>/<api-id>` (= `proto/<svc>/v1`);
    there is no config knob (verified against apx 0.12.1 `ValidateGoPackage`/`ExtractGoPackage`).
  - protoc-gen-ent takes no `module=` opt and emits its `ent/` schema package as a SIBLING of
    the proto's Go package dir (`path.Dir(go_package)+"/ent"`). The generated repository adapter
    (same package as the `.pb.go`) and the ent client only compile together when the proto's Go
    package is a SINGLE directory segment under the buf output root — i.e. `gen/<svc>v1`, with
    `ent/` at `gen/ent`. A `proto/<svc>/v1` go_package makes ent split-brain (adapter in one dir,
    pb.go in another; ent import path mismatched) → it will not build.
  So `go_package` MUST be `gen/<svc>v1` for the ent backend to work, which is incompatible with
  apx's `proto/<svc>/v1` expectation. The warning is non-fatal (release prepare exits 0) and the
  generated `apx-release.yml` CI does not use `--strict`. Both backends use the one `gen/<svc>v1`
  convention. Rationale is also captured in `scaffold/model.go` (GoImportPath) + the
  `buf.gen.ent.yaml.tmpl` header.

---

## Phase 1 — Templates + the example annotated resource (no generator yet)

- **T-101 [C]** Author the example service proto template `proto/api/{svc}/v1/{svc}.proto`: one resource
  message (`id`, a couple of fields incl. one `(infoblox.field.v1.opts)` constraint, `account_id` for
  tenant scoping) + a CRUD `{Svc}Service`, **every RPC carrying `(infoblox.authz.v1.rule)`**. Must pass
  `apx lint` and contain **no** engine options. *(AC-002/003/004 hinge on this being correct.)*
- **T-102 [C]** Author the `server/main.go` template: `server.New(...)` wired to the generated
  `{Svc}AuthzRules` + a `DevAuthorizer` + `Register{Svc}Service`. Must compile against generated output
  and trip the boot-time authz gate when a rule is missing. *(AC-002.)*
- **T-103 [S]** Author the rote templates: `Makefile` (`make generate` = buf, `make api-release` = apx),
  `.gitignore` (ignore generated `*.svc.go`/`*.storage.go`/`ent/` output), `go.mod` template (consumer
  module path + gorm/ent deps + canonical authz/field bindings — **never** the SDK go.mod's deps).
- **T-104 [C]** Author the `{svc}_smoke_test.go` template: boot the server, one tenant-scoped CRUD
  round-trip through the gateway. *(AC-007.)*
- **T-105 [S]** Author `buf.gen.yaml` templates **per backend** (gorm vs ent): the 7 `local:` plugins with
  correct per-plugin opts — `protoc-gen-ent` takes **no** `module=`; `protoc-gen-storage` takes
  `dialect=` — and `buf.yaml` (two modules: `proto/api` + vendored `proto/infoblox`). *(The exact thing
  the manual flow gets wrong.)*

## Phase 2 — The generator (`cmd/devedge-sdk` CLI) — D-1, D-2, D-5

- **T-201 [S]** New `cmd/devedge-sdk` cobra binary; `new service <name> --resource <R>
  --backend <ent|gorm>` (default gorm per D-3) `--no-generate` `--force`. Wire to the renderer.
- **T-202 [C]** Toolchain preflight: require `apx` and `buf` on PATH (min versions); refuse a non-empty
  target dir unless `--force`; never emit a half-written tree. *(Failure modes: toolchain missing,
  non-empty dir.)*
- **T-203 [C]** Shell out `apx init app <module>` in the target dir, then reconcile: keep the scaffold's
  `buf.yaml` module roots aligned with the `apx.yaml` `module_roots` apx wrote. Assert `apx config
  validate` passes on the result. *(D-2; AC-006.)*
- **T-204 [S]** Render the Phase-1 templates with the resolved name/resource/backend into the tree
  (`text/template`, embedded via `embed.FS`).
- **T-205 [S]** Vendor the `infoblox/{authz,field}/v1` mirrors from the SDK module dir, **pinned to the
  resolved SDK module version**; record the pin; error on mismatch. *(D-4; failure mode: mirror drift.)*
- **T-206 [S]** Unless `--no-generate`: run `buf generate` + `go mod tidy` in the tree so it builds
  immediately. *(D-5; AC-001.)*

## Phase 3 — End-to-end build gate (gorm path — no F027 dependency)

- **T-301 [C]** Integration test: scaffold a gorm service into a temp dir → `buf generate && go build
  ./... && go vet ./...` green with **zero** hand-edits. *(AC-001, AC-005 gorm.)*
- **T-302 [C]** Run the generated smoke test for the gorm scaffold; assert green. *(AC-007.)*
- **T-303 [C]** Authz-gate regression: scaffold, remove one `authz.v1.rule`, regenerate → server **fails
  to boot** with the completeness-gate error. *(AC-002.)*
- **T-304 [S]** Boundary test: generated GORM code is git-ignored; the SDK `go.mod` stays engine-free
  (assert gorm/ent appear only in the generated consumer `go.mod`). *(AC-004; failure mode: engine-dep
  leak.)*

## Phase 4 — ent backend parity 🔒F027 ✅ DONE

- **T-401 [C] ✅** `--backend ent` enabled; the ent `buf.gen.yaml` template wires `protoc-gen-ent`
  (no `module=`) in place of `protoc-gen-storage` and produces schema + the generated
  `*_repo.ent.go` adapter (F027) with **zero** hand-written `ent_wiring`. A backend-aware
  `main.ent.go.tmpl` opens an ent client (SQLite `sqlite3` alias over `modernc.org/sqlite`) +
  `Schema.Create` + `New<R>EntRepository`; `go.mod.ent.tmpl` carries ent deps (no gorm);
  `tools.go.tmpl` pins entc; the pipeline + Makefile run the buf→entc two-step. *(AC-005 ent.)*
- **T-402 [C] ✅** `TestScaffold_ENT_BuildsAndPasses` mirrors the gorm gate (scaffold → buf → entc →
  build/vet/test incl. the CRUD smoke) and asserts parity (gorm-free go.mod, generated adapter
  present, no hand-written `ent_wiring.go`). `TestScaffold_ENT_Boundary` covers AC-004 for ent.

## Phase 5 — apx governance assertions (the public-surface contract) ✅ DONE

- **T-501 [C] ✅** `TestScaffold_APXGovernance` (generator test): `apx config validate`, `apx lint`,
  `apx breaking --against HEAD` (trivial empty baseline = the committed proto vs itself; an empty
  dir fails in buf with "no .proto files"), and `apx release prepare proto/<svc>/v1 --version
  v1.0.0-alpha.1 --lifecycle experimental --dry-run` all pass. The scaffold's emitted
  `apx-release.yml` (from `apx init app`) already runs `apx lint` + `apx breaking --against HEAD^`
  + `apx release submit`. *(AC-003; G-002.)* The Makefile `api-release` api-id was fixed to
  `proto/<svc>/v1` (was `<svc>/v1`, which apx rejects with "module path does not exist").
- **T-502 [C] ✅** `apx policy check` passes against `forbidden_proto_options: ^gorm\.` on the
  generated surface (asserted in `TestScaffold_APXGovernance`) — proves no engine leak into the
  public contract. *(AC-004; G-003.)*

## Phase 6 — Docs + DX record

- **T-601 [S]** Rewrite `docs/.../getting-started/quickstart.md` around `devedge-sdk new service`; update
  `define-a-service.md` to point hand-wiring at the scaffold as the default path.
- **T-602 [S]** Record the before/after: ~10 manual steps (proposal Appendix A) → one command + zero
  hand-edits. *(AC-008.)*

---

## Sequencing notes

- **Unblocked now:** Phases 1, 2, 3, 5, 6 on the **gorm** path — no F027 dependency.
- **Gated → now UNBLOCKED:** Phase 4 (ent). F027 is on `main` and on this branch
  (`protoc-gen-ent` emits the `New<R>EntRepository` adapter), so the ent path is implemented and
  green. ✅
- **Cross-artifact consistency (the Plan gate):** every AC maps to ≥1 task (AC-001→T-301/T-206,
  AC-002→T-303/T-102, AC-003→T-501, AC-004→T-304/T-502, AC-005→T-301/T-401, AC-006→T-203, AC-007→T-302,
  AC-008→T-602); every D-decision has a task (D-1→T-201, D-2→T-203, D-3→T-201/T-401, D-4→T-205,
  D-5→T-206).
