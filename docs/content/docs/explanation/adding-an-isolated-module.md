---
title: Adding an Isolated Module
weight: 4
---

devedge-sdk is a **multi-module repository** (WS-011 / F039). The root module is the dep-light
**library** that apps import; every heavy backend lives in its **own nested Go module** so its heavy
dependency lands in a consumer's module graph **only when that consumer `require`s the module**. This
page is the concrete checklist for carving a *future* heavy component into its own isolated module.

If you only want to understand *why* the split exists, read [Pluggability Model](../pluggability/)
first — this page is the **how-to for maintainers**.

## The modules today (worked examples)

| Module (path) | Heavy dep it owns | Tag |
|---|---|---|
| `github.com/infobloxopen/devedge-sdk` (root) | none — the dep-light library | `vX.Y.Z` |
| `…/cmd` | the CLI + `protoc-gen-*` (may import ent/gorm freely) | `cmd/vX.Y.Z` |
| `…/observability/otel` | OTel SDK + exporters | `observability/otel/vX.Y.Z` |
| `…/config/koanf` | koanf | `config/koanf/vX.Y.Z` |
| `…/events/kafkabus` | franz-go | `events/kafkabus/vX.Y.Z` |
| `…/persistence/gormtx` | gorm | `persistence/gormtx/vX.Y.Z` |
| `…/persistence/entrepo` | ent | `persistence/entrepo/vX.Y.Z` |

All seven are released **synchronized** — one version per release, tagged on one commit (see
[Releasing](#releasing) below). Because the Go **module path equals the import path**, splitting a
package into its own module changes **no `.go` import statement** anywhere — only `go.mod` /
`go.work` / tags / CI / templates.

## When to carve out a new module

Carve a package into its own module when it pulls a **heavy or contentious dependency** that the core
library must not impose on every consumer — a Kubernetes client, a cloud SDK, a second ORM, a policy
engine. The test: *would a `server`-only consumer that never touches this feature still drag the dep
into its `go.sum` and build closure?* If yes, isolate it.

**Worked future example used throughout this page: a k8s-controller-scaffolding module**
(`…/controller/k8sscaffold`) that would import `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`,
etc. Those are exactly the kind of heavy deps that must not enter a plain gRPC service's graph.

## The checklist

Carving `…/controller/k8sscaffold` (substitute your own path) is eight steps. Use any existing
module as a copy-from template — `observability/otel` is the smallest (one heavy dep, test-only core
imports); `persistence/entrepo` is the richest (multiple core imports + generated code).

### 1. Create the nested `go.mod` (it `require`s the root)

```bash
mkdir -p controller/k8sscaffold
cd controller/k8sscaffold
go mod init github.com/infobloxopen/devedge-sdk/controller/k8sscaffold
```

Then give it a `require` on the root module **at the latest released version** (the placeholder the
release script will bump), plus its own heavy deps. The module **depends on the root** (adapter →
core); the root must **never** import it back — that would be a cycle, now structurally impossible
across a module boundary.

```
// controller/k8sscaffold/go.mod
module github.com/infobloxopen/devedge-sdk/controller/k8sscaffold

go 1.25.5

require (
	github.com/infobloxopen/devedge-sdk v0.26.1   // ← the placeholder; release.sh bumps it
	sigs.k8s.io/controller-runtime v0.x.y
	// … the rest of the heavy deps
)
```

> The `v0.26.1` placeholder is a **published** root tag that predates the carve-out. Local builds
> never use it (go.work resolves the root require to the working tree); it exists only so the file is
> valid before the next release tag exists. The release script replaces it with the new version. Mirror
> the package comment the existing adapters carry explaining this.

### 2. Add it to `go.work`

Add the directory to the `use` block so local dev/build/test resolves the cross-module reference
(adapter → root) to the working tree with no `replace` or version pins:

```
// go.work
use (
	.
	./cmd
	./config/koanf
	./controller/k8sscaffold   // ← new
	./events/kafkabus
	./observability/otel
	./persistence/entrepo
	./persistence/gormtx
)
```

### 3. Add it to the Makefile `MODULES` list

So `make build` / `make vet` / `make test` **and** `make build-gowork-off` cover the new module:

```make
MODULES := . cmd config/koanf controller/k8sscaffold events/kafkabus observability/otel \
           persistence/entrepo persistence/gormtx
```

`build-gowork-off` is the gate that catches a published module **missing a `require`** (which
`go.work` would silently satisfy from the working tree) — the failure mode that bit earlier phases.
Adding the module here is non-negotiable.

### 4. Extend `scripts/check-graph-isolation.sh` with its dep family

The graph-isolation check proves the whole point: a server-only consumer is free of the heavy dep,
and adding the module pulls it in (opt-in). For the new module:

- Add its module path variable (e.g. `K8SSCAFFOLD_MOD="${SDK_PATH}/controller/k8sscaffold"`).
- Add its heavy-dep fragments to `GOMOD_GUARDS` **and** — only if the core retains **no**
  back-reference to them (the usual case) — to `GOSUM_GUARDS` (the **strong** claim: absent from
  `go.mod`, build closure **and** `go.sum`).
- Add an **opt-in (converse) assertion**: scaffold a consumer that imports the new module and assert
  the heavy dep now appears in its build closure + `go.sum`.

**Per-family claim — pick the right one.** Most deps have no retained core back-reference, so they
leave a server-only consumer's `go.sum` entirely (assert the strong claim, like koanf/franz-go/gorm/
ent). The exception is `otel/sdk`: the contrib handlers the core *keeps* (`otelgrpc`/`otelhttp`)
declare `require go.opentelemetry.io/otel/sdk` in **their own** go.mod for their tests, so `otel/sdk`
lingers in a consumer's `go.sum` as a pruned-graph checksum even though **no `otel/sdk` package is
ever compiled**. For such a dep, assert the achievable claim: absent from `go.mod` + **build closure**
(compiled packages), present in `go.sum` only via the retained handler's test require — and document
why. The existing script's header comment is the worked rationale; copy its shape.

### 5. Add the scaffold `require` + version var (only if generated code imports it)

If the `devedge-sdk new service` scaffold **emits code that imports the new module** (the way the
generated `main` imports `observability/otel`, or the generated repository imports `persistence/*`):

- Add the module path constant + a `resolve…Version()` in
  `cmd/devedge-sdk/internal/scaffold/model.go` that returns `resolveSDKVersion()` (every module
  releases synchronized, so it tracks the SDK version exactly):

  ```go
  const k8sScaffoldModulePath = "github.com/infobloxopen/devedge-sdk/controller/k8sscaffold"
  func resolveK8sScaffoldVersion() string { return resolveSDKVersion() }
  ```

- Add the fields to the template `Model` and populate them in `New…Model`.
- Add the `require {{.K8sScaffoldModulePath}} {{.K8sScaffoldVersion}}` line to `go.mod.tmpl` and/or
  `go.mod.ent.tmpl`.
- Add the path to `nestedAdapterModules` in `scaffold_integration_test.go` so `injectLocalReplace`
  resolves it to the working tree during the E2E.

If generated code does **not** import the new module (a consumer opts in by hand, like
`config/koanf`), skip this step — there is nothing to scaffold.

> **Single version source.** `SDKVersion` and every adapter-version resolver fall back to one
> constant — `fallbackSDKVersion` in `model.go`. The release script bumps that one constant, which
> bumps them all. Do **not** add a second hard-coded version constant.

### 6. Add it to the release script's module list

Add the directory to `NESTED_MODULES` in `scripts/release.sh` so the synchronized release bumps its
root `require`, tidies it, and tags it at `controller/k8sscaffold/vX.Y.Z` with everything else:

```bash
NESTED_MODULES=(
  "cmd"
  "config/koanf"
  "controller/k8sscaffold"   # ← new
  "events/kafkabus"
  "observability/otel"
  "persistence/gormtx"
  "persistence/entrepo"
)
```

### 7. Add a cleancore guard + its converse (the boundary proof)

In `cleancore_test.go`, add a guard asserting the heavy dep never enters the **clean core's**
transitive closure, plus the **converse** asserting the new module's closure **does** import it (so
the guard is meaningful). The module boundary makes a core→backend import a **compile error**, not
just a test failure — but the guard documents the intent and catches a regression before the boundary
does. Copy the `TestCleanCore_NoORMImport` / `TestEntrepoAdapter_DoesImportEnt` pair.

### 8. Wire CI cache + any integration gate

In `.github/workflows/ci.yml`, add `controller/k8sscaffold/go.sum` to the `cache-dependency-path`
lists. If the module owns an integration gate (a real k8s API via `envtest`/kind, the way
gormtx/entrepo own the Postgres/MySQL gates and kafkabus owns Kafka), add that step in the module that
owns it — and a "must run, not skip" guard mirroring the existing PG/MySQL/Kafka guards.

## Releasing

The release is **synchronized**: `scripts/release.sh vX.Y.Z` (or `make release VERSION=vX.Y.Z`) bumps
every nested module's `require github.com/infobloxopen/devedge-sdk` to `vX.Y.Z` and the scaffold
version source, then runs a **two-phase, tag-root-first** sequence and tags the root `vX.Y.Z` plus
every submodule at `<path>/vX.Y.Z`. Default is a dry run; `--push` does it for real; `--validate`
proves the go.sum mechanic locally (no real tag).

Three load-bearing rules the script encodes (and that any new module must respect):

- **Never `go work sync`.** In this nested layout it rewrites/empties the member `require` blocks.
  Tidy each module individually instead.
- **Tag the root FIRST, then finalize adapter go.sums against it.** An adapter's go.sum must carry the
  real checksum for `root@vX.Y.Z`, or any `-mod=readonly` build (CI's `GOWORK=off` per-module build, a
  standalone external consumer) fails with `missing go.sum entry … to verify package … is provided by
  exactly one module`. The **only** way `go mod tidy` writes that checksum is to resolve `root@vX.Y.Z`
  from its published source — the **git tag**. A filesystem `replace` to the working tree makes tidy
  *succeed* but writes **no** go.sum hash for the version, and pushing the tag later does **not**
  retro-fill a committed go.sum. So the script does **phase 1** — commit the version-var bump + each
  adapter's `require root vX.Y.Z` (`go mod edit` only), tag root `vX.Y.Z`, **push the root tag** — then
  **phase 2** — with `root@vX.Y.Z` resolvable from the remote, `GOWORK=off go mod tidy` each adapter so
  the real hash lands in its go.sum, commit, tag the adapters, push.
- **The root tag and the adapter tags sit on two different commits.** That is standard and correct for
  a multi-module repo: each adapter is tagged only *after* its go.sum is complete, so a standalone
  `GOWORK=off` build at any adapter tag resolves cleanly.

**Post-push verification (proxy lag):** after `--push`, confirm an external consumer resolves each
module — `make release-verify VERSION=vX.Y.Z` does `go get <mod>@vX.Y.Z` for every module. Use the
**explicit** `@vX.Y.Z`, not `@latest`: the proxy's `@latest` view lags a few minutes behind a fresh
tag, while an explicit version fetches the tag directly. A brief 404 on an explicit version is proxy
lag (retry, or prefix `GOPROXY=direct` for an immediate VCS fetch); `GONOSUMCHECK` is **not** needed
for these public modules.

## The gates a new module must pass

After wiring all eight steps, the same gates the existing modules pass must stay green:

- `make build` / `make vet` / `make test` — all modules via `go.work`.
- `make build-gowork-off` — every module builds with the workspace disabled (real requires only).
- `make check-graph-isolation` — the server-only consumer is free of the new dep; opt-in pulls it.
- `make security-check` + the cleancore guards — the core never reaches the heavy dep.
- Scaffold E2E (`go test ./cmd/devedge-sdk -run TestScaffold`) — if step 5 applied, the generated
  `go.mod` requires the new module and the project still builds.
- `make release VERSION=vX.Y.Z` (dry run) prints a plan that **includes the new module's tag** without
  mutating any tag.
