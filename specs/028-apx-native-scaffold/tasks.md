# F028 — Tasks (apx-native service scaffold)

Lifecycle tags: **[S]** = simple/mechanical (Sonnet subagent) · **[C]** = complex (Opus).
Gate: **🔒F027** = blocked on F027 (`feat/027-repo-adapter-codegen`) being on `main` (or rebased in) —
the `--backend ent` path consumes the generated ent adapter.

Reference layout to template from: `testdata/apikey/`. Decisions: see spec `## Design decisions` (D-1..D-5).

**Status (gorm path):** Phases 1, 2, 3 SHIPPED on `feat/028-apx-native-scaffold`
(`cmd/devedge-sdk`); docs T-601/T-602 done. AC-001/002/004/005-gorm/006/007 proven by the
in-repo integration tests; AC-003 (`apx lint`/`config validate`) asserted in the pipeline.
**Deferred:** Phase 4 (ent, 🔒F027), and Phase 5's *CI-embedded* `apx breaking`/`release
prepare --dry-run`/`policy check` assertions (manually verified during dev; not yet wired as a
generator test). Known follow-up: align the proto `go_package` with apx's expected
`<mod>/proto/<svc>/v1` so `apx release prepare --dry-run` is warning-free (currently a soft
non-`--strict` warning because generated Go lands under `gen/`).

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

## Phase 4 — ent backend parity 🔒F027

- **T-401 [C] 🔒F027** Once F027 is on `main`: enable `--backend ent`; the ent `buf.gen.yaml` template
  (T-105) produces schema + generated `*_repo.ent.go` with **zero** hand-written `ent_wiring`. *(AC-005
  ent.)*
- **T-402 [C] 🔒F027** Run T-301/T-302/T-303 for the ent backend; assert parity with gorm.

## Phase 5 — apx governance assertions (the public-surface contract)

- **T-501 [C]** In the scaffold's emitted CI (and as a generator test): `apx lint` passes on the proto;
  `apx breaking --against` the empty baseline passes; `apx release prepare … --dry-run` succeeds. *(AC-003;
  G-002.)*
- **T-502 [C]** `apx policy check` passes against `forbidden_proto_options: ^gorm\.` on the generated
  surface (proves no engine leak into the public contract). *(AC-004; G-003; failure mode: policy
  violation.)*

## Phase 6 — Docs + DX record

- **T-601 [S]** Rewrite `docs/.../getting-started/quickstart.md` around `devedge-sdk new service`; update
  `define-a-service.md` to point hand-wiring at the scaffold as the default path.
- **T-602 [S]** Record the before/after: ~10 manual steps (proposal Appendix A) → one command + zero
  hand-edits. *(AC-008.)*

---

## Sequencing notes

- **Unblocked now:** Phases 1, 2, 3, 5, 6 on the **gorm** path — no F027 dependency.
- **Gated:** Phase 4 (ent) on **F027 → `main`**. Recommended: land F027 (currently shipped-core,
  unmerged on `feat/027`) to `main`, then either rebase `feat/028` onto it or merge main in, before T-401.
- **Cross-artifact consistency (the Plan gate):** every AC maps to ≥1 task (AC-001→T-301/T-206,
  AC-002→T-303/T-102, AC-003→T-501, AC-004→T-304/T-502, AC-005→T-301/T-401, AC-006→T-203, AC-007→T-302,
  AC-008→T-602); every D-decision has a task (D-1→T-201, D-2→T-203, D-3→T-201/T-401, D-4→T-205,
  D-5→T-206).
