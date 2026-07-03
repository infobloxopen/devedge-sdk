# Feature Specification: SLI/SLO codification seam — derived OpenSLO + fail-loud classifier + open-core emitters

**Feature Branch**: `feat/ws025-slo-core`
**Created**: 2026-07-03
**Status**: Draft
**Initiative**: WS-025 (reliability as declared intent) — **P0 keystone + P1 Prometheus/Cortex emitter**

## Context

WS-025 adds a reliability seam built the devedge way: **codify intent → derive the plumbing →
one tech-agnostic interchange (OpenSLO) → project to any backend**. Every devedge service already
emits RED signals per request (`otelgrpc.NewServerHandler()` at `server/server.go:310`, the gateway
wrapped in `otelhttp.NewHandler` at `server.go:507`), but nothing turns those signals into
**objectives** — no SLO, no error budget, no burn-rate alert, no place to state a business
reliability target distinct from raw monitoring. The projection layer is greenfield.

This feature is the **devedge-sdk side** of WS-025 P0+P1: the OpenSLO IR, contract-derived default
SLOs, the fail-loud three-layer classifier, the `slogen` CLI that `de` orchestrates, the open-core
Prometheus/Grafana/Loki emitters, a scaffold-emitted starter `slo.yaml`, gated Helm
`PrometheusRule`/`ServiceMonitor` templates, the `define-slo` authoring skill, and the KPI/SLO docs.

**Out of scope of this feature** (other WS-025 repos / later phases): the `de slo` verbs (devedge),
the apx OpenSLO catalog artifact, the internal Grafana-Operator emitter overlay
(`devedge-sdk-internal`, consumed via `--preset-dir`), journey SLOs resolved through the apx catalog
(P3), and any re-instrumentation (RED already exists — WS-007). `slogen`'s CLI contract is designed
so `de` can orchestrate it via a pinned `go run` (WS-023 hermetic pattern).

## Ratified design decisions

These come from the hub proposal (`specs/slo-codification-seam-proposal.md`, ratified 2026-07-03)
plus decisions this spec adds from the ground-truth survey of what the SDK's instrumentation
actually emits.

- **D1 — OpenSLO v1 is the single interchange.** All authoring compiles to OpenSLO
  (`apiVersion: openslo/v1`; kinds `SLI`, `SLO`, `Service`, `AlertPolicy`, `AlertCondition`).
  Emitters read only the OpenSLO IR; they never re-derive from proto/OpenAPI.
- **D2 — Derive defaults from the enriched OpenAPI (WS-024).** The `openapi/<svc>.openapi.yaml`
  produced by WS-024 carries `x-aip-method` per operation and `x-aip-resource` per schema. The SDK
  enumerates operations, classifies each by AIP method, and emits **grouped** default SLOs. The
  same defaults are derivable from the standard AIP method names of a resource (no OpenAPI needed),
  so the scaffold can emit a good `slo.yaml` day one.
- **D3 — SLOs are GROUPED to keep the count small and decision-mapped.** Per service: one
  **read availability** + one **read latency** SLO over the read methods (Get/List/BatchGet), one
  **write availability** + one **write latency** SLO over the write methods
  (Create/Update/Delete/Undelete). Per-operation refinement is a later opt-in; custom methods are
  left out of the grouped defaults (a service refines them by hand).
- **D4 — Availability is good/valid with a client-fault-excluded denominator, computed correctly
  for THIS SDK's actual metric.** The default availability SLI's **valid** (denominator) excludes
  client-fault statuses; **bad** = server-fault statuses; **good** = valid − bad. Concretely:
  - **server fault (bad)** = gRPC {`UNKNOWN`(2), `DEADLINE_EXCEEDED`(4), `INTERNAL`(13),
    `UNAVAILABLE`(14), `DATA_LOSS`(15)}.
  - **client fault (excluded from valid)** = gRPC {`INVALID_ARGUMENT`(3), `NOT_FOUND`(5),
    `ALREADY_EXISTS`(6), `PERMISSION_DENIED`(7), `UNAUTHENTICATED`(16)}.
  - All other codes (incl. `OK`) are valid and good.
- **D5 — GROUND-TRUTH METRIC NAMES (refines the proposal).** The proposal's illustrative names
  (`rpc_server_duration_*`, numeric `rpc_grpc_status_code`) describe the **legacy** OTel semantic
  convention. This SDK pins `otelgrpc` **v0.69.0** / `otelhttp` **v0.69.0** on OTel **v1.44.0**, and
  installs the handlers with **no options and no `OTEL_SEMCONV_STABILITY_OPT_IN`** — so the DEFAULT
  emitted metric is the **new** semconv (v1.41.0) instrument:
  - gRPC: **`rpc.server.call.duration`** (unit **seconds**) → Prometheus classic-histogram
    `rpc_server_call_duration_seconds_{bucket,count,sum}`; status label **`rpc_response_status_code`**
    with **UPPER_SNAKE** canonical string values (`OK`, `UNAVAILABLE`, `INTERNAL`, …); method label
    `rpc_method` (short name, e.g. `ListWidgets`), service label `rpc_service` (proto FQN, e.g.
    `toy.v1.WidgetService`).
  - HTTP gateway (alternative "closer to the edge"): **`http.server.request.duration`** (seconds) →
    `http_server_request_duration_seconds_{bucket,count,sum}`; status label
    `http_response_status_code` (numeric; bad = 5xx); method `http_request_method`; route `http_route`.
  - The metric prefix, unit suffix, and label names are a **`MetricNaming` config struct** with the
    new-semconv defaults, plus a **`LegacyGRPCNaming`** preset (`rpc_server_duration_milliseconds_*`,
    numeric `rpc_grpc_status_code`, values `2/4/13/14/15`) for the `OTEL_SEMCONV_STABILITY_OPT_IN`
    opt-in path. A semconv bump is a config change, not a code edit. A **golden test pins the
    generated query strings**.
- **D6 — Multi-window multi-burn-rate alerting (SRE Workbook ch. 5).** The Prometheus emitter emits
  SLI error-ratio **recording rules** (one metric, one series per rate window) + burn-rate
  **alerting rules** in three tiers: fast-burn **PAGE** (burn 14.4, windows 1h ∧ 5m), medium **PAGE**
  (burn 6, windows 6h ∧ 30m), slow **TICKET** (burn 1, windows 3d ∧ 6h). Thresholds are the burn
  multiplier × error budget (`1 − objective`), computed as literals in the expression.
- **D7 — 28d rolling default window; objectives are un-calibrated placeholders.** Every generated
  objective (99.9% availability, 99% latency-under-threshold, threshold on a histogram bucket
  boundary) is marked `devedge.io/uncalibrated: "true"` and MUST be calibrated against a measured
  baseline before it pages anyone.
- **D8 — Error-budget policy is mandatory.** Every generated SLO carries a
  `devedge.io/error-budget-policy` annotation (a TODO stub on generation) and references an
  `AlertPolicy`. An SLO with neither is invalid — the classifier rejects it.
- **D9 — Dependency-light + isolated.** `slo` lives in the **root module** and imports only stdlib +
  `gopkg.in/yaml.v3` (already in the module) + `encoding/json` — no Prometheus/Grafana/Datadog client
  libraries, no kin-openapi (the enriched OpenAPI is parsed with narrow structs). `slogen` lives in
  the **cmd module** (which may import anything). `scripts/check-graph-isolation.sh` stays green.

## The three declaration layers (enforced, not just documented)

1. **Signals / API KPIs (Layer 0).** RED per method + the four golden signals + USE for resources,
   in OTel semconv terms. Always-on, **no target**. `slogen kpis` prints the reference. Declaring a
   Layer-0 signal (a saturation/resource/utilization metric) as an SLI is a **category error** the
   classifier rejects.
2. **Service SLIs/SLOs (Layer 1).** Per-service availability/latency as good/valid ratios + error
   budget + burn-rate alerts. Derived by default from the contract, refined by the developer.
3. **Business / journey SLOs (Layer 2).** Cross-service CUJ outcomes (P3, other repos). This feature
   defines the `journey-SLI` category in the classifier but does not resolve journeys via the catalog.

## Requirements

### A — OpenSLO IR (`slo` package)

- **FR-A1**: The `slo` package MUST define Go structs for OpenSLO v1 `SLI`, `SLO`, `Service`,
  `AlertPolicy`, and `AlertCondition` that round-trip to/from OpenSLO YAML (`apiVersion: openslo/v1`,
  `kind`, `metadata{name,labels,annotations}`, `spec{...}`). A `Document` type holds an ordered set
  of objects and marshals to a multi-document YAML stream (deterministic key/field order).
- **FR-A2**: An `SLO.spec` MUST carry `service`, `indicatorRef`/`indicator`, `budgetingMethod`
  (`Occurrences`), a `timeWindow` (28d rolling default), one or more `objectives` (target ratio),
  and `alertPolicies`. The `SLI.spec` MUST carry a `ratioMetric` whose `good` and `total`
  metric sources use a devedge-neutral `type` and a typed spec capturing the derivation intent
  (SLI type, OTel signal name, transport, service label, method set, excluded statuses, latency
  threshold) — tech-agnostic, so emitters translate rather than the IR hardcoding PromQL.

### B — Derivation

- **FR-B1**: `DefaultsFromOpenAPI(spec, service)` MUST enumerate operations from an enriched OpenAPI
  doc, group them by `x-aip-method` into read {Get,List,BatchGet} and write
  {Create,Update,Delete,Undelete}, and emit the four grouped SLOs (read/write × availability/latency)
  as an OpenSLO `Document`. Custom/unclassified methods are excluded from the grouped defaults.
- **FR-B2**: `DefaultsForResource(...)` MUST emit the same four grouped SLOs from a resource's
  standard AIP method names (no OpenAPI needed), so the scaffold can emit `slo.yaml` day one.
- **FR-B3**: The derived availability SLI MUST exclude client-fault statuses from the valid
  denominator and count only server-fault statuses as bad (D4). The derived latency SLI MUST measure
  the fraction of valid requests under a threshold placed on a histogram bucket boundary.
- **FR-B4**: Every generated SLO MUST carry `devedge.io/error-budget-policy` (TODO stub),
  `devedge.io/uncalibrated: "true"`, reference an `AlertPolicy`, and use the 28d rolling window.

### C — Classifier (fail-loud)

- **FR-C1**: `Lint(doc) []Finding` MUST return structured findings (severity `error`|`warn`,
  message, and the offending SLO/SLI name).
- **FR-C2**: A saturation/resource/utilization metric declared as an SLI MUST be an **error**
  ("that is a Layer-0 signal, not an objective"). Detection is name-based
  (cpu, memory, mem, queue, depth, pool, disk, `*_utilization`, `*_saturation`, connections,
  goroutines, heap, file-descriptor).
- **FR-C3**: An SLO with no error-budget policy (neither the annotation nor an `alertPolicies`
  reference) MUST be an **error**.
- **FR-C4**: A cause-based (non-symptom) indicator MUST be **flagged** (warn); an un-measured /
  aspirational target (`devedge.io/uncalibrated`) MUST be a **warning**.
- **FR-C5**: `ClassifyMetric(name)` MUST return the category (`signal` | `service-SLI` |
  `journey-SLI`) for a candidate, so the authoring skill and `slogen lint` share one classifier.

### D — Emitters (open core, pure text)

- **FR-D1**: A `prometheus` emitter MUST produce a Cortex-ruler-compatible `PrometheusRule`
  (`apiVersion: monitoring.coreos.com/v1`, `kind: PrometheusRule`): SLI error-ratio recording rules
  + MWMBR alerting rules (D6), referencing the ground-truth metric names via `MetricNaming` (D5).
- **FR-D2**: A `grafana` emitter MUST produce importable Grafana dashboard JSON per service: SLI
  trend + error-budget burndown + burn-rate panels.
- **FR-D3**: A `loki` emitter MUST produce a documented, minimal LogQL recording-rule group for
  log-derived SLIs (thinner than the Prometheus emitter).
- **FR-D4**: A `Render(target, doc, opts)` entry point MUST select the built-in emitter by target
  and, when `opts.PresetDir` names a directory containing `<target>.tmpl`, render from that template
  instead (the seam the internal Grafana-Operator overlay uses).

### E — `cmd/slogen` CLI (`de` orchestrates — KEEP STABLE)

- **FR-E1**: `slogen generate --openapi <path> [--service <name>] [--out slo.yaml]` — enriched
  OpenAPI → OpenSLO.
- **FR-E2**: `slogen lint <file...> [--format json]` — validate + classify; **non-zero exit** on any
  error-severity finding.
- **FR-E3**: `slogen render --target prometheus|grafana|loki --in <slo.yaml> --out <dir>
  [--preset-dir <dir>]` — project the IR; `--preset-dir` overrides built-in emitters.
- **FR-E4**: `slogen kpis` — print the Layer-0 API KPI reference (golden signals + RED + USE, OTel
  semconv terms).

### F — Scaffold + Helm

- **FR-F1**: A newly scaffolded service MUST get a starter `slo.yaml` on disk (the four grouped
  defaults from `DefaultsForResource`), and a documented `make slo` regen path from the real OpenAPI.
- **FR-F2**: The deploy Helm chart MUST gain `prometheusrule.yaml` (gated on `monitoring.enabled`)
  and `servicemonitor.yaml` (gated on `serviceMonitor.enabled`) templates + values, **off by
  default**, with the SLO rule groups injectable via `monitoring.prometheusRule.groups`
  (the `slogen render --target prometheus` output).

### G — Skill + Docs

- **FR-G1**: `.claude/skills/define-slo/SKILL.md` MUST drive an author to a GOOD SLO (CUJ-first,
  SLI-type menu, explicit valid denominator, measured baseline, window, error budget, mandatory
  error-budget policy) and teach the three-layer separation + the classifier, referencing
  `slogen`/`de slo`. It MUST be registered in `.claude/skills/README.md`.
- **FR-G2**: Docs MUST add a KPI reference page, an SLO how-to, and a three-layers concept page under
  `docs/content/docs/`, following `docs/STYLE-GUIDE.md`; the observability how-to MUST point at the
  SLO capability; CHANGELOG updated.

### Cross-cutting

- **FR-X1**: `go build ./...`, `go vet ./...`, `go test ./...` clean (root + cmd); `gofmt` clean;
  `scripts/check-graph-isolation.sh` green (no new heavy deps in the root module).
- **FR-X2**: Golden tests pin: the derived OpenSLO (enriched OpenAPI → `slo.yaml`), the classifier
  rejections, and the emitter outputs (PrometheusRule with the exact metric names + burn-rate
  thresholds; Grafana dashboard JSON).

## Acceptance Criteria

- **AC-1**: `DefaultsFromOpenAPI` on the toy enriched OpenAPI yields exactly four SLOs (read/write ×
  availability/latency), each with the 28d window, an error-budget-policy stub, `uncalibrated: true`,
  and an `AlertPolicy` reference. (Golden.)
- **AC-2**: The derived read-availability SLI's PromQL numerator counts server-fault statuses
  (`UNKNOWN|DEADLINE_EXCEEDED|INTERNAL|UNAVAILABLE|DATA_LOSS`) over a denominator that excludes
  client-fault statuses (`INVALID_ARGUMENT|NOT_FOUND|ALREADY_EXISTS|PERMISSION_DENIED|UNAUTHENTICATED`),
  against `rpc_server_call_duration_seconds_count` with label `rpc_response_status_code`. (Golden.)
- **AC-3**: The Prometheus emitter's alerting rules are the three MWMBR tiers with thresholds
  `14.4·(1−obj)` (1h∧5m, page), `6·(1−obj)` (6h∧30m, page), `1·(1−obj)` (3d∧6h, ticket). (Golden.)
- **AC-4**: `slogen lint` on a doc declaring `container_memory_utilization` as an SLI exits non-zero
  with "Layer-0 signal, not an objective"; on a valid derived doc it exits zero.
- **AC-5**: `slogen lint` on an SLO stripped of its error-budget policy exits non-zero.
- **AC-6**: `slogen generate --openapi testdata/toy.openapi.yaml --service toy.v1.WidgetService`
  reproduces the golden `slo.yaml`.
- **AC-7**: `slogen render --target prometheus --in <golden slo.yaml>` reproduces the golden
  PrometheusRule; `--target grafana` the golden dashboard JSON; `--preset-dir <dir with prometheus.tmpl>`
  renders from the template instead.
- **AC-8**: A scaffolded service has a `slo.yaml` that `slogen lint` passes (only `uncalibrated`
  warnings, no errors).
- **AC-9**: `helm template` (or the scaffold render test) renders the `PrometheusRule` +
  `ServiceMonitor` only when their gates are enabled.

## Failure Modes (fail-loud, not silent)

- **FM-1 — Signal declared as an objective** → `Lint` error (AC-4).
- **FM-2 — SLO with no error-budget policy** → `Lint` error (AC-5).
- **FM-3 — Aspirational target** → `Lint` warn; the doc carries `uncalibrated: true` until calibrated.
- **FM-4 — Availability counting client faults as bad** → the derived valid denominator excludes
  client-fault codes by construction; a test asserts a client-fault code is absent from bad (AC-2).
- **FM-5 — Metric-name drift (OTel→Cortex)** → `MetricNaming` owns the normalization; a golden test
  pins the generated query names against the v0.69.0 new-semconv output; `LegacyGRPCNaming` covers the
  opt-in path.
- **FM-6 — Backend library leaking into the root module** → `slo` imports only stdlib + yaml.v3;
  `check-graph-isolation` stays green.
- **FM-7 — Unknown render target / unreadable input** → `slogen` exits non-zero with a clear message.

## Out of scope (WS-025 later / other repos)

- The `de slo` verbs (devedge), the apx OpenSLO catalog artifact, the internal Grafana-Operator
  emitter overlay (`--preset-dir` seam is built here; the overlay ships in `devedge-sdk-internal`).
- Journey SLOs resolved across services via the apx catalog (P3).
- A live in-process error-budget `SLOSink` (the Cortex ruler owns burn-rate alerting).
- Datadog / Google Cloud Monitoring emitters (the registry is designed for them).
- Any re-instrumentation, authz/tenancy change, or DDD write-boundary change.
