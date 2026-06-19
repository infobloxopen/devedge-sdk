# F028 — apx-native service scaffold (zero-to-running service, one command)

**Status**: planned — Propose + Clarify done (decisions locked 2026-06-19, below); see `tasks.md`.
Implement gated on **F027 → `main`** for the `--backend ent` path.
**Branch**: `feat/028-apx-native-scaffold` (off `main` @ `49dfa6d`)
**Initiative**: WS-004 (apx-native scaffolding) · hub proposal
`specs/devedge-apx-scaffolding-proposal.md` (Phase 1)
**Depends on**: F027 (generated ent repository adapter — required for the `--backend ent` "zero
hand-written wiring" criterion; F027 core is shipped on `feat/027-repo-adapter-codegen`, must be
on `main` or rebased in before AC-005's ent path is implementable)
**Relates to**: the canonical annotation modules `infoblox.authz.v1` / `infoblox.field.v1` (already
released via apx), the SDK plugins `protoc-gen-{svc,storage,ent,devedge-authz}`, `testdata/apikey`
(the de-facto reference layout), `docs/.../getting-started/quickstart.md` (the manual steps this replaces)

---

## Problem statement

Onboarding a new service onto devedge-sdk is **~10 hand-wired, error-prone steps** and there is **no
scaffold**. A developer must, by hand: create the dir layout; author a two-module `buf.yaml` (their
protos + a vendored-imports module); `buf dep update`; author a `buf.gen.yaml` wiring **seven** `local:`
plugins (and know that `protoc-gen-ent` takes **no** `module=` opt while the rest do, and that
`protoc-gen-storage` takes an optional `dialect=`); copy byte-identical **mirrors** of `infoblox/authz/v1`
and `infoblox/field/v1` out of the SDK module dir (because the canonical `infobloxopen/apis` module ships
only generated Go bindings, not `.proto` source for `buf` to import); `go get` two canonical bindings;
`buf generate`; then hand-wire `server.New(...)` + `Register<Svc>Service`. Get any step wrong and the
failure mode is a cryptic `buf` import error or an init-time panic.

This is the open Run-9 DX finding **#53 ("no consumer scaffold")** — the same assessment that spawned F027.
F027 removed the largest *in-service* churn (the ~300-line hand-written ent adapter); F028 removes the
*onboarding* churn that precedes it.

Two further gaps beyond friction:

1. **The public API surface is ungoverned.** A hand-rolled service proto has no lint gate, no
   breaking-change gate, no versioned release, and no catalog presence. There is no org-standard contract
   lifecycle on it.
2. **The public/private boundary is implicit and easy to violate.** Nothing stops a developer from leaking
   engine/ORM concerns into the published surface, and nothing asserts that the GORM/ent models stay
   consumer-local. The boundary exists in the SDK's design (engine deps kept out of the SDK `go.mod`) but
   is not enforced or scaffolded for the consumer.

devedge's own product vision promised `de init` / `de project init` and never built them; "project
templates" was deferred to v2. This feature delivers the SDK half of that promise (the `de new` UX that
drives it is Phase 2 / a separate devedge feature — see Non-goals).

---

## What this is

A **scaffold generator, shipped in devedge-sdk**, that turns `<service-name>` + one `<resource>` into a
project that **builds, boots, is authz-gated, and persists** — with **zero files hand-edited before the
first run** — where:

- the **public API surface** is an **apx `app`-role module**: the service proto lives under an apx-managed
  module root, carries only neutral annotations (`infoblox.authz.v1.rule`, `infoblox.field.v1.opts`), and
  is governed by `apx lint` / `apx breaking` / `apx release` / `apx catalog` + the generated
  `apx-release.yml` CI;
- the **internal models** (`*.svc.go`, `*.storage.go` or `ent/schema/*` + `*_repo.ent.go`, `*.authz.go`)
  are produced by **`buf generate`** (the SDK `local:` plugins), are **git-ignored**, and pull their
  engine deps (gorm/ent) into the **consumer** `go.mod` only — never the SDK's, never the published schema.

**apx governs the contract; `buf` + the SDK plugins generate the implementation.** This feature does
**not** make apx run the codegen (apx does not run protoc plugins — confirmed: `apx gen` only materializes
*consumed* dependencies as `go.work` overlays; `apx init` itself prints "Run 'buf generate' to generate
code"). The two are complementary layers and the scaffold wires both.

---

## Goals

- **G-001 (one command, zero hand-edits)** A single invocation produces a directory that `buf generate &&
  go build ./... && go vet ./...` passes with **no** manual edits. The count of files a developer must
  hand-touch to reach a first running service drops from ~10 authored artifacts to **0**.
- **G-002 (apx-governed public surface)** The generated proto is a valid apx `app` module: `apx lint`
  passes, `apx breaking` runs, `apx release prepare --dry-run` succeeds, and an `apx-release.yml` workflow
  is present. The service's API is declared the org-standard way.
- **G-003 (private models, enforced boundary)** The generated GORM/ent code is git-ignored; engine deps
  appear only in the generated consumer `go.mod`; and the public proto contains **no** engine/ORM options
  (verifiable via `apx policy check` against the canonical `forbidden_proto_options: ^gorm\.`). "Internal
  models don't have to be public" is the scaffolded default, not a convention to remember.
- **G-004 (authz-gated by construction)** The scaffolded service wires the boot-time authz completeness
  gate; every example RPC carries an `authz.v1.rule`; removing one makes the service refuse to boot. A new
  RPC without a rule is a startup failure, never a silent open endpoint.
- **G-005 (backend choice)** A flag selects `ent` or `gorm`; both produce a building, persisting service
  with **zero hand-written persistence wiring** (ent path depends on F027).

## Non-goals

- The **`de new` UX in devedge** that drives this generator (Phase 2 — a feature in the devedge repo).
- **Canonicalizing `infoblox.storage.v1.model` via apx** (Phase 3 — schema work in `infobloxopen/apis`).
- **Dogfooding devedge's own `/v1/routes` API on apx** (Phase 4).
- Multi-resource graphs, complex relationships, or non-CRUD method shapes in the scaffold output — start
  with **one** resource (CRUD + the framework fields); richer templates iterate later.
- Replacing or wrapping the SDK's codegen with apx. apx governs the contract; buf+SDK generate.
- Solving the proto-mirror problem at the registry level (BSR / buf module) — the scaffold vendors the
  mirror automatically as the working default (see Open questions Q2).

---

## How apx and the SDK divide (the dividing line)

| | Public — apx-governed | Private — buf + SDK, consumer-local |
|---|---|---|
| Artifact | `<svc>.proto` (RPCs, resource messages, neutral annotations) | `*.svc.go`, `*.storage.go` / `ent/* + *_repo.ent.go`, `*.authz.go` |
| Tooling | `apx lint/breaking/release/catalog`; `apx-release.yml` | `buf generate` (SDK `local:` plugins) |
| Published? | yes (versioned release + catalog) | **no** — git-ignored; engine deps in consumer `go.mod` only |
| Guardrail | apx policy `forbidden_proto_options: ^gorm\.`, breaking gate, CODEOWNERS | SDK `security-check`; engine deps never in SDK `go.mod` |

Key implementation fact: **`apx init app` does NOT write `buf.yaml` / `buf.gen.yaml`** (it writes
`apx.yaml`, an example schema, `.gitignore`, and `apx-release.yml`). The scaffold therefore supplies the
buf wiring itself and is responsible for keeping `apx.yaml`'s module root and the buf module paths aligned.

---

## Generated layout (target)

```
<service>/
├── apx.yaml                         # role=app: org, repo, module_roots:[proto], policy
├── apx.lock                         # pinned: infoblox/authz/v1, infoblox/field/v1
├── buf.yaml                         # modules: proto/api (yours) + proto/infoblox (vendored mirrors)
├── buf.gen.yaml                     # 7 SDK local: plugins, pre-wired (ent: no module=; storage: dialect=)
├── proto/
│   ├── api/<svc>/v1/<svc>.proto     # <Svc>Service + <Resource>{ id, …, account_id } with annotations
│   └── infoblox/{authz,field}/v1/   # vendored annotation mirrors (auto; pinned to the SDK version)
├── go.mod                           # gorm/ent deps + canonical authz/field bindings (NOT the SDK go.mod)
├── server/main.go                   # server.New(...) wired to <Svc>AuthzRules + a DevAuthorizer
├── <svc>_smoke_test.go              # generated smoke test (boot + one CRUD round-trip)
├── Makefile                         # `make generate` (buf) · `make api-release` (apx)
├── .gitignore                       # ignores generated *.svc.go/*.storage.go/ent/ output
└── .github/workflows/apx-release.yml
```

---

## Acceptance criteria

- **AC-001 (builds clean, no edits)** `<scaffold> <name> --resource <R>` in an empty dir → `buf generate
  && go build ./... && go vet ./...` all succeed with **no** manual edits. Number of hand-authored files
  to first build = 0.
- **AC-002 (boots + authz gate live)** The generated server boots; with every RPC annotated it serves;
  deleting one `authz.v1.rule` from the example proto and regenerating makes the server **fail to boot**
  with the completeness-gate error (proves G-004).
- **AC-003 (apx-governed surface)** On the generated proto: `apx lint` passes; `apx breaking --against`
  the empty baseline passes; `apx release prepare <api-id> --version v1.0.0-alpha.1 --lifecycle
  experimental --dry-run` succeeds.
- **AC-004 (private models / boundary enforced)** Generated GORM/ent code is git-ignored (not tracked);
  `go.mod` (consumer) carries the engine deps while the SDK `go.mod` does not; the public proto contains
  no `gorm.*` (or other engine) options and `apx policy check` passes against `forbidden_proto_options:
  ^gorm\.`.
- **AC-005 (zero persistence wiring, both backends)** `--backend ent` and `--backend gorm` each produce a
  building, persisting service over SQLite with **no** hand-written `New<R>...Repository` / `ent_wiring`
  (ent path consumes F027). The generated smoke test (`AC-007`) passes for both.
- **AC-006 (app-repo CI present)** The scaffold emits `apx.yaml` with the `app` role and a working
  `apx-release.yml`; `apx config validate` passes on the generated `apx.yaml`.
- **AC-007 (smoke test green)** The generated `<svc>_smoke_test.go` (boot the server + one CRUD
  round-trip through the gateway, tenant-scoped) passes under `go test ./...` immediately after
  `buf generate`.
- **AC-008 (DX recorded)** The new flow is documented as the quickstart, with an explicit before/after
  against the ~10-step manual sequence (Appendix A of the hub proposal); the before/after is captured in
  `docs/`.

## Failure modes to cover

- **Toolchain missing** — `buf` (and, if the scaffold shells out, `apx`) not on PATH or wrong version →
  fail with a clear, actionable message naming the missing tool + min version; never emit a half-written,
  non-building tree.
- **Non-empty / existing target dir** — refuse to clobber; require an empty dir or an explicit
  `--force`; never overwrite a tracked file silently.
- **Mirror drift** — vendored annotation `.proto` out of sync with the SDK version → pin the mirror to the
  resolved SDK module version and record it; a mismatch is a generation-time error, not a silent skew.
- **Engine-dep leak** — gorm/ent must never land in the SDK `go.mod`; the generated `go.mod` is the only
  place they appear. A test asserts the SDK module stays engine-free.
- **Policy violation in the public proto** — an engine option in the surface → `apx policy check` fails
  (this is the guardrail working; the scaffold's example proto must itself pass it).
- **`apx init app` layout drift** — if the scaffold writes `apx.yaml` from its own template rather than
  shelling out (Open Q1), the template can drift from apx's evolving app layout → pin/track an apx version
  and assert `apx config validate` (AC-006) in CI.
- **Authz gate false-negative** — a generated RPC missing a rule must fail boot, not pass (AC-002 is the
  regression guard).

---

## Design decisions (locked in Clarify — 2026-06-19)

- **D-1 (Q3) Form factor = a new `devedge-sdk` CLI binary.** Ships at `cmd/devedge-sdk`
  (`go install …/cmd/devedge-sdk@latest`), invoked `devedge-sdk new service <name> --resource <R>
  --backend <ent|gorm>`. Versioned alongside the plugins it wires; Phase 2's `de new` shells out to /
  vendors this. devedge-sdk has no single CLI today (only the `protoc-gen-*` plugins + `security-check`);
  this introduces one. *(The `new`/`service` noun-verb leaves room for `devedge-sdk new resource` etc.)*
- **D-2 (Q1) apx coupling = shell out to `apx init app`.** apx is the source of truth for the app-repo
  layout: the scaffold runs `apx init app <module>` (writes `apx.yaml`, `.gitignore`, `apx-release.yml`)
  and then supplements the buf wiring + go.mod + server + smoke test it does not write. ⇒ **`apx` is a
  required tool at scaffold time** (add to the toolchain preflight; see failure modes). Keeps the layout
  current with apx automatically (no template drift).
- **D-3 (Q4/scope) Both backends at Phase 1 close (full AC-005).** `--backend ent|gorm`. ⇒ the ent path
  **requires F027 merged to `main`** (or rebased into `feat/028`) before it is implementable. The gorm
  path has no F027 dependency (`protoc-gen-storage` already generates a complete `Repository`), so it is
  built first and the ent path follows F027's merge. Default backend (when `--backend` omitted): **gorm**
  (the unblocked path) for the first running scaffold; revisit once ent parity lands.
- **D-4 (Q2) Proto-mirror = auto-vendor from the SDK module dir,** pinned to the resolved SDK version
  (apx overlays do not provide importable `.proto`). A publish-a-buf-module follow-up is tracked
  separately, out of scope here.
- **D-5 (Q5) The scaffold runs the first `buf generate` + `go mod tidy`** so the emitted tree builds and
  the smoke test passes immediately (satisfies G-001 "zero hand-edits *and* zero follow-up steps"). A
  `--no-generate` flag skips it for offline/inspection use.

---

## Phasing (to be detailed in tasks.md after Plan)

1. Template set + the example annotated proto (one resource, CRUD) — pure, no generator yet.
2. The generator: render the layout from `name`/`resource`/`backend`; emit buf + apx + go.mod + server +
   smoke test; resolve Q1/Q3 form factor.
3. Wire the SDK plugins in `buf.gen.yaml` correctly per backend; vendor the mirrors pinned to SDK version.
4. End-to-end gate: scaffold → buf generate → build/vet/test green for both backends (AC-001/05/07).
5. apx governance assertions: `apx lint`/`breaking`/`release --dry-run`/`config validate`/`policy check`
   in the scaffold's own CI (AC-003/04/06).
6. Docs: replace the manual quickstart; record the before/after (AC-008).
