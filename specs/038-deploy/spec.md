# F038 — Multi-runtime deploy behind a target seam: k8s/k3s (framework-owned Helm chart + Flux) + Docker Compose

**Status**: design locked. **Issue**: #95 (DX 059, P1). **Initiative**: WS-007 (the last P1 capability).
**Depends on**: #90 (OTEL_* env), #91 (health probes), #93 (`config.ServerOptions` env surface), and the
scaffold (F028). **Pre-GA**: clean implementation over back-compat.

**Origin / user directive (verbatim requirements, WS-007 brief):** the deploy feature must be
**multi-runtime by design — ≥2 targets** behind a deployment-target seam (targets are adapters; adding one
must not require core changes). The developer picks a target; the framework renders the artifacts.
- **Target 1 — Kubernetes / k3s: FIRST-CLASS and required. Helm charts are a first-class requirement.** Ship
  a **framework-owned, built-in Helm chart that is rendered for the developer — they never author or even
  see the Helm internals**, only a thin values overlay. The chart wires the rest of the foundation: health
  probes (#91), config (#93), the observability exporter (#90), the DSN secret, ingress, resource limits.
  Aligns with the standing devedge convention of rendering all k8s objects via an embedded Helm chart
  (memory `devedge-helm-and-dsn-conventions`) and reuses/extends devedge's `service` chart (feature 005).
- **Deployment environment = Flux GitOps: required.** The k8s output must be Flux-reconcilable: emit a Flux
  `HelmRelease` + a source (`OCIRepository`/`HelmRepository` for the published chart, or a Git source).
  "Devs never see the chart" = the chart is published/embedded by the framework; the service repo carries
  only the `HelmRelease` + values overlay that Flux reconciles.
- **Target 2 — Docker Compose.** Render `docker-compose.yml` (the service + its declared dependencies) wired
  to the same config/health/observability surface — a pure-local/lightweight runtime that proves the seam
  with **two real adapters, not one**.
- **Target 3+ — AWS ECS (future, open).** Design the seam so ECS slots in as another adapter without core
  changes; **do not build it now** (ship a documented seam stub).
- **Dev↔prod coherence.** Dev deploy stays devedge (`de project up --deploy`, feature 005 — the same chart);
  prod deploy is Flux. Same chart, two reconcilers — keep them aligned.

## Problem
At v0.24.0 a scaffolded service has **no deploy path** — no chart, no compose, no GitOps artifact. The
operational foundation (#90/#91/#93) is wired in code but there's nothing that *runs* it in a real runtime.

## Decision (locked)
- **Deployment-target seam.** A `deploy` package in the scaffold tooling:
  ```go
  type Target interface {
      Name() string                              // "k8s", "compose", (future) "ecs"
      Render(svc ServiceModel, opts Options) ([]Artifact, error) // Artifact = {Path, Contents}
  }
  ```
  A registry maps target name → adapter. The scaffold renders the artifacts for the selected target(s) into
  the generated service repo. Adding a target = registering a new `Target` adapter — **no core change** (gate).
- **Framework-owned Helm chart, embedded — devs never author it.** The canonical chart lives in the SDK at
  `deploy/helm/chart/` and is **`go:embed`-ed** into the tooling. The chart templates render: Deployment
  (with `livenessProbe`→`/healthz` and `readinessProbe`→`/readyz` from #91; `OTEL_*` env from #90; the
  `config.ServerOptions` env from #93; `terminationGracePeriodSeconds` paired with the graceful-shutdown fix
  below), Service, optional Ingress, the **DSN as a Secret** (indirect-DSN + real-DSN-file pattern, memory
  `devedge-helm-and-dsn-conventions`), and resource requests/limits. Devs touch only a thin `values.yaml`.
- **k8s target output (Flux-reconcilable, devs never see the chart).** Into `deploy/k8s/`:
  a Flux `HelmRelease` referencing the published chart via an `OCIRepository` source (chart published by the
  framework — see "chart publication"), plus the `values.yaml` overlay. The service repo carries ONLY the
  HelmRelease + source + values — never the chart internals.
- **compose target output.** Into `deploy/compose/`: a `docker-compose.yml` with the service + its declared
  dependencies (e.g. Postgres), wired to the SAME surface — config via `environment:`, a `healthcheck:`
  hitting `/healthz`, `OTEL_*` env (+ an optional `otel-collector` service), the DSN, and `stop_grace_period`
  matching the graceful-shutdown window.
- **Graceful shutdown (folds in the P0-hardening follow-up).** The scaffold `main` templates use
  `signal.NotifyContext(ctx, SIGTERM, SIGINT)` so `Serve` returns on SIGTERM → graceful gRPC/HTTP shutdown +
  OTel flush + readiness-loop stop. The chart's `terminationGracePeriodSeconds` and compose's
  `stop_grace_period` are set to match (default 30s).
- **ECS seam stub.** A documented `Target` stub (interface satisfied, returns "not implemented" + a doc note)
  proving the seam is open for ECS without core changes.

## Chart publication (how "devs never author the chart" actually works)
The embedded chart is the single source of truth. Two consumers, one chart:
1. **Prod/GitOps:** a release step publishes the embedded chart to an OCI registry (or GH Pages Helm repo);
   the emitted `HelmRelease`/`OCIRepository` reference it by version. (For this feature: emit the artifacts +
   wire the publish step; the actual registry coordinates are a values/release config, documented.)
2. **Local/dev:** devedge renders the SAME embedded chart directly (`helm template`) for `de project up
   --deploy` (feature 005). Coherence requirement noted; the devedge-side wiring is a follow-up in the
   devedge repo (WS-007 is devedge-sdk-scoped).

## Acceptance criteria
- **AC-1 (seam, ≥2 targets).** `Target` interface + registry with **k8s** and **compose** adapters; a unit
  test renders both; adding a (stub) ECS target requires only registering an adapter (no core edit) — proven
  by the stub compiling against the same interface.
- **AC-2 (framework-owned chart, devs never author it).** The chart is `go:embed`-ed; the k8s target emits a
  Flux `HelmRelease` + `OCIRepository` source + a `values.yaml` overlay — and **no editable chart** — into
  the service repo. `helm lint` / `helm template` on the embedded chart passes (run in a test if helm is
  available; else validate the rendered YAML parses).
- **AC-3 (chart wires the foundation).** Rendered Deployment has liveness `/healthz` + readiness `/readyz`
  (#91), `OTEL_*` env (#90), `config.ServerOptions` env (#93), the DSN Secret, ingress, resource limits, and
  `terminationGracePeriodSeconds`.
- **AC-4 (compose parity).** `docker-compose.yml` brings up the service + its declared deps wired to the same
  config/health/observability surface; `docker compose config` validates (in a test if docker available;
  else YAML parses).
- **AC-5 (graceful shutdown).** Scaffold `main` uses `signal.NotifyContext` (SIGTERM/SIGINT); a test asserts
  `Serve` returns on signal and shutdown runs. Chart/compose grace periods set.
- **AC-6 (dependency-light gate).** Deploy artifacts are **templates, not dependencies** — the root module
  gains no new runtime dependency from this feature (a YAML lib for rendering, if any, stays in the scaffold
  tooling path, not the service runtime). `go list -deps ./server/... ./middleware/... | grep` unchanged.
- **AC-7 (gates).** build/vet/test/security green; scaffold E2E green; the generated service still builds.

## Failure modes
- **Chart leaks into the service repo as editable Helm** → violates "devs never author it". Mitigation: emit
  only HelmRelease+source+values; keep the chart embedded; test the emitted file set.
- **Probe/Config/OTEL drift** between code and chart → chart references a probe path or env name the code
  doesn't serve. Mitigation: derive env names from the `config.ServerOptions` tags + the #91 probe paths;
  a test cross-checks the rendered values against the known endpoints/env.
- **No graceful shutdown** → k8s SIGTERM kills mid-request, traces don't flush. Mitigated by the
  `signal.NotifyContext` fix + grace periods (AC-5).
- **Over-building ECS now** → scope gate: ship the stub + docs only.

## Tasks
- **T1 [C]** `deploy` seam — `Target` interface + registry; `Artifact` model; wire into the scaffold pipeline
  (`--deploy k8s,compose`, default both). ECS stub.
- **T2 [C]** framework-owned Helm chart under `deploy/helm/chart/` (`go:embed`), wiring health/config/otel/
  DSN/ingress/limits/grace; the k8s `Target` emits Flux `HelmRelease` + `OCIRepository` + `values.yaml`.
- **T3 [S]** compose `Target` — `docker-compose.yml` + optional `otel-collector`, same surface + grace period.
- **T4 [S]** graceful shutdown — `signal.NotifyContext` in `main.go.tmpl`/`main.ent.go.tmpl`; grace periods.
- **T5 [S]** tests — render both targets; chart lint/template (or YAML parse); compose validate (or parse);
  signal-shutdown test; the ECS-stub-compiles test. Docs: `docs/content/docs/guides/deploy.md` (targets, the
  GitOps model, chart publication, dev↔prod coherence).

## Exit
All ACs green; PR merged; tag cut. A scaffolded service comes up with a **deploy path supporting ≥2 runtime
targets**, k8s/k3s first-class via a **framework-owned Helm chart the developer never hand-writes**, rendered
into a Flux `HelmRelease`. Closes the last P1 capability; the DX cadence re-run should show deploy **present**.
Follow-up (devedge repo, not WS-007): wire `de project up --deploy` to render the same embedded chart.
