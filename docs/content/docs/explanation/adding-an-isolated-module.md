---
title: Adding an Isolated Module
weight: 4
---

An isolated module is a nested Go module within devedge-sdk that owns one heavy backend dependency so that dependency does not enter every consumer's module graph. When you add an isolated module, a service that never uses the new feature sees no change to its `go.sum` or build closure.

Use this page when you are adding a new heavy component — a Kubernetes client, a cloud SDK, a second ORM, a policy engine — and you need to wire it into the repository correctly.

If you want to understand why the split exists, read [Pluggability Model](../pluggability/) first. That page explains the concept; this page is the how-to for maintainers.

## Current modules

| Module path | Heavy dependency it owns | Tag prefix |
|---|---|---|
| `github.com/infobloxopen/devedge-sdk` (root) | none — the dependency-light library | `vX.Y.Z` |
| `…/cmd` | the CLI and `protoc-gen-*` plugins (may import ent and gorm freely) | `cmd/vX.Y.Z` |
| `…/observability/otel` | OTel SDK and exporters | `observability/otel/vX.Y.Z` |
| `…/config/koanf` | koanf | `config/koanf/vX.Y.Z` |
| `…/events/kafkabus` | franz-go | `events/kafkabus/vX.Y.Z` |
| `…/persistence/gormtx` | gorm | `persistence/gormtx/vX.Y.Z` |
| `…/persistence/entrepo` | ent | `persistence/entrepo/vX.Y.Z` |

All seven modules release in sync — one version per release, tagged on one commit (see [Releasing](#releasing) below). Because the Go module path equals the import path, moving a package into its own module changes no `.go` import statement anywhere. Only `go.mod`, `go.work`, tags, CI, and templates are affected.

## When to add a module

Add a package as its own module when it pulls a heavy or contentious dependency that the core library must not impose on every consumer. Ask: would a `server`-only consumer that never touches this feature still drag the dependency into its `go.sum` and build closure? If yes, isolate it.

**Example used throughout this page:** a k8s-controller-scaffolding module (`…/controller/k8sscaffold`) that imports `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`, and similar packages. Those are exactly the kind of heavy dependencies that must not appear in a plain gRPC service's module graph.

## Checklist

Carving `…/controller/k8sscaffold` — substitute your own path — is eight steps. Use any existing module as a copy-from template. `observability/otel` is the smallest (one heavy dependency, test-only core imports). `persistence/entrepo` is the richest (multiple core imports plus generated code).

### Step 1: create the nested go.mod

```bash
mkdir -p controller/k8sscaffold
cd controller/k8sscaffold
go mod init github.com/infobloxopen/devedge-sdk/controller/k8sscaffold
```

Add a `require` on the root module at the latest released version, plus the new module's own heavy dependencies. The new module depends on the root; the root must never import the new module back, because that would be a cycle. The module boundary makes such a cycle a compile error.

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

{{< callout type="info" >}}
**The version placeholder is a published root tag that predates the carve-out.** Local builds never use it — `go.work` resolves the root `require` to the working tree. The placeholder exists only so the file is valid before the next release tag exists. The release script replaces it with the new version. Mirror the package comment the existing adapters carry explaining this.
{{< /callout >}}

### Step 2: add it to go.work

Add the directory to the `use` block so local development, builds, and tests resolve the cross-module reference to the working tree without needing `replace` directives or version pins:

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

### Step 3: add it to the Makefile MODULES list

Add the directory to `MODULES` so `make build`, `make vet`, `make test`, and `make build-gowork-off` cover the new module:

```make
MODULES := . cmd config/koanf controller/k8sscaffold events/kafkabus observability/otel \
           persistence/entrepo persistence/gormtx
```

`build-gowork-off` is the gate that catches a published module missing a `require` that `go.work` would silently satisfy from the working tree. Adding the new module to `MODULES` is required.

### Step 4: extend the graph-isolation check

The graph-isolation check (`scripts/check-graph-isolation.sh`) proves that a server-only consumer is free of the heavy dependency, and that adding the new module pulls it in (opt-in). For the new module:

- Add its module path variable, for example `K8SSCAFFOLD_MOD="${SDK_PATH}/controller/k8sscaffold"`.
- Add its heavy-dependency fragments to `GOMOD_GUARDS` and — only if the core retains no back-reference to them, which is the usual case — to `GOSUM_GUARDS`. The `GOSUM_GUARDS` entry makes the strong claim: the dependency is absent from `go.mod`, the build closure, and `go.sum`.
- Add an opt-in assertion: scaffold a consumer that imports the new module and assert the heavy dependency now appears in its build closure and `go.sum`.

**Choosing the right claim.** Most dependencies have no retained core back-reference, so they leave a server-only consumer's `go.sum` entirely — assert the strong claim, as koanf, franz-go, gorm, and ent do. The exception is `otel/sdk`: the contrib handlers the core keeps (`otelgrpc` and `otelhttp`) declare `require go.opentelemetry.io/otel/sdk` in their own `go.mod` for their tests, so `otel/sdk` appears in a consumer's `go.sum` as a pruned-graph checksum even though no `otel/sdk` package is compiled. For such a dependency, assert the achievable claim: absent from `go.mod` and the build closure, present in `go.sum` only via the retained handler's test require — and document why. The existing script's header comment is the worked rationale; copy its shape.

### Step 5: add the scaffold require and version variable

This step applies only when `devedge-sdk new service` emits code that imports the new module — the way the generated `main` imports `observability/otel`, or the generated repository imports `persistence/*`. If generated code does not import the new module (a consumer opts in by hand, as with `config/koanf`), skip this step.

When it does apply:

1. Add the module path constant and a `resolve…Version()` function in `cmd/devedge-sdk/internal/scaffold/model.go` that returns `resolveSDKVersion()`. Every module releases in sync, so the version tracks the SDK version exactly:

   ```go
   const k8sScaffoldModulePath = "github.com/infobloxopen/devedge-sdk/controller/k8sscaffold"
   func resolveK8sScaffoldVersion() string { return resolveSDKVersion() }
   ```

2. Add the fields to the template `Model` and populate them in `New…Model`.
3. Add the `require {{.K8sScaffoldModulePath}} {{.K8sScaffoldVersion}}` line to `go.mod.tmpl` and/or `go.mod.ent.tmpl`.
4. Add the path to `nestedAdapterModules` in `scaffold_integration_test.go` so `injectLocalReplace` resolves it to the working tree during the end-to-end test.

{{< callout type="warning" >}}
**Use one version source.** `SDKVersion` and every adapter-version resolver fall back to one constant — `fallbackSDKVersion` in `model.go`. The release script bumps that one constant, which bumps them all. Do not add a second hard-coded version constant.
{{< /callout >}}

### Step 6: add it to the release script

Add the directory to `NESTED_MODULES` in `scripts/release.sh` so the synchronized release bumps its root `require`, tidies it, and tags it at `controller/k8sscaffold/vX.Y.Z` together with the other modules:

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

### Step 7: add the cleancore guard and its converse

In `cleancore_test.go`, add a guard asserting the heavy dependency never enters the core module's transitive closure, plus a converse asserting the new module's closure does import it (so the guard is meaningful). The module boundary makes a core-to-backend import a compile error, but the guard documents the intent and catches a regression earlier. Copy the `TestCleanCore_NoORMImport` and `TestEntrepoAdapter_DoesImportEnt` pair as a template.

### Step 8: wire CI cache and any integration gate

In `.github/workflows/ci.yml`, add `controller/k8sscaffold/go.sum` to the `cache-dependency-path` lists. If the module owns an integration gate — a real Kubernetes API via `envtest` or kind, the way `gormtx` and `entrepo` own the Postgres and MySQL gates and `kafkabus` owns the Kafka gate — add that step in the module that owns it, along with a "must run, not skip" guard that mirrors the existing database and message-bus guards.

## Releasing

The release is synchronized: `scripts/release.sh vX.Y.Z` (or `make release VERSION=vX.Y.Z`) bumps every nested module's `require github.com/infobloxopen/devedge-sdk` to `vX.Y.Z` and the scaffold version source, then runs a two-phase, tag-root-first sequence. It tags the root at `vX.Y.Z` and every submodule at `<path>/vX.Y.Z`. Default is a dry run; `--push` executes it; `--validate` proves the go.sum mechanic locally without creating any real tag.

Three rules the script encodes, and that any new module must follow:

- **Never run `go work sync`.** In this nested layout it rewrites or empties the member `require` blocks. Tidy each module individually instead.
- **Tag the root first, then finalize adapter go.sums against it.** An adapter's `go.sum` must carry the real checksum for `root@vX.Y.Z`, or any `-mod=readonly` build — including CI's `GOWORK=off` per-module build and a standalone external consumer — fails with `missing go.sum entry … to verify package … is provided by exactly one module`. The only way `go mod tidy` writes that checksum is to resolve `root@vX.Y.Z` from its published source, which is the git tag. A filesystem `replace` to the working tree makes tidy succeed but writes no `go.sum` hash for the version, and pushing the tag later does not retro-fill a committed `go.sum`. The script handles this in two phases: phase 1 commits the version-var bump and each adapter's `require root vX.Y.Z` (via `go mod edit` only), tags the root at `vX.Y.Z`, and pushes the root tag; phase 2, with `root@vX.Y.Z` resolvable from the remote, runs `GOWORK=off go mod tidy` on each adapter so the real hash lands in its `go.sum`, commits, tags the adapters, and pushes.
- **The root tag and the adapter tags sit on two different commits.** Each adapter is tagged only after its `go.sum` is complete, so a standalone `GOWORK=off` build at any adapter tag resolves cleanly.

**Post-push verification.** After `--push`, confirm an external consumer resolves each module using `make release-verify VERSION=vX.Y.Z`, which runs `go get <mod>@vX.Y.Z` for every module. Use the explicit `@vX.Y.Z` form, not `@latest`: the proxy's `@latest` view lags a few minutes behind a fresh tag, while an explicit version fetches the tag directly.

{{< callout type="info" >}}
**A brief 404 on an explicit version is proxy lag.** Retry after a moment, or prefix `GOPROXY=direct` for an immediate VCS fetch. `GONOSUMCHECK` is not needed for these public modules.
{{< /callout >}}

## Verification gates

After wiring all eight steps, the following gates must stay green:

- `make build`, `make vet`, `make test` — all modules via `go.work`.
- `make build-gowork-off` — every module builds with the workspace disabled, using real `require` entries only.
- `make check-graph-isolation` — the server-only consumer is free of the new dependency; opt-in pulls it in.
- `make security-check` and the cleancore guards — the core never reaches the heavy dependency.
- Scaffold end-to-end (`go test ./cmd/devedge-sdk -run TestScaffold`) — if step 5 applied, the generated `go.mod` requires the new module and the project still builds.
- `make release VERSION=vX.Y.Z` dry run — prints a plan that includes the new module's tag without mutating any tag.
