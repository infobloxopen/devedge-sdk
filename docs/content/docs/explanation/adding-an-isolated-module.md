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
every nested module's `require github.com/infobloxopen/devedge-sdk` to `vX.Y.Z`, bumps the scaffold
version source, `go mod tidy`s **each module individually**, and tags the root `vX.Y.Z` plus every
submodule at `<path>/vX.Y.Z` — all on the **one** release commit. Default is a dry run; `--push` does
it for real.

Two load-bearing rules the script encodes (and that any new module must respect):

- **Never `go work sync`.** In this nested layout it rewrites/empties the member `require` blocks.
  Tidy each module individually instead.
- **Tidy needs the shed root resolvable before the tag exists.** The script sets the require to
  `vX.Y.Z`, adds a *temporary* local `replace` to the working-tree root for the duration of the tidy
  (because `vX.Y.Z` is not tagged yet), then drops the replace. The committed `go.mod` carries the
  clean real `require`, which resolves the instant the `vX.Y.Z` tag is pushed.

**Why all tags can sit on one commit:** Go resolves `require …devedge-sdk vX.Y.Z` to the tag `vX.Y.Z`
and `…/observability/otel vX.Y.Z` to `observability/otel/vX.Y.Z`. A tag is just a commit pointer;
nothing requires the root tag to be on an *earlier* commit. After the release commit, every adapter's
real require points at a tag that exists on that same commit and contains the shed root — a coherent
set. **External-resolution caveat:** until the tags are pushed, a `GOWORK=off` build of an adapter
fails (`missing go.sum entry`) because the required `vX.Y.Z` root is not yet on the remote/proxy —
expected; local builds keep working via `go.work`. After pushing, the module proxy may take minutes to
observe the new tags; a brief `go get …@vX.Y.Z` 404 is **proxy lag**, not a release defect (retry or
use `GOPROXY=direct`).

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
