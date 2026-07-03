---
name: define-slo
description: Author a GOOD SLI/SLO for a devedge service — pick the user journey and SLI type, express good/valid with an explicit client-fault-excluded denominator, set the target from a MEASURED baseline (not aspiration), choose the window, compute the error budget, and write the mandatory error-budget policy. Use whenever you add or change a service's reliability targets, review an slo.yaml, or an SLO must be defined, instead of hand-writing Prometheus rules or picking a number out of the air. Produces OpenSLO that `de slo lint`'s classifier accepts and that projects to Cortex/Grafana.
---

# Define a GOOD SLI/SLO for a devedge service

## When this fires

Any time reliability becomes a target: a new service, a new critical user journey, a customer SLA, or
a review of an existing `slo.yaml`. The danger is treating an SLO like a dashboard — picking 99.9%
because it looks nice, or pointing an "SLO" at a CPU graph. Every reliability target becomes a
**versioned OpenSLO objective** derived from the contract and calibrated against a **measured
baseline**, projected to Cortex/Grafana by `de slo render` — never a hand-written Prometheus rule,
never an aspirational number.

## The three layers (do not conflate them)

The classifier (`de slo lint` / `slogen lint`) enforces this separation and will **reject** a
violation:

| Layer | What | Target? | Owner |
|-------|------|---------|-------|
| **0 — Signals / API KPIs** | RED per method, the four golden signals, USE for resources (OTel semconv). Diagnostic. | **No target.** Page nobody on them alone. | Platform (always-on) |
| **1 — Service SLOs** | Per-service availability/latency as a good/valid ratio + error budget + burn alerts. | Yes | Service team |
| **2 — Journey SLOs** | A critical user journey's outcome, composed across services. Ties to the SLA. | Yes | Product |

**A saturation/resource metric (cpu, memory, queue depth, pool, `*_utilization`) is a Layer-0
signal, never an SLI.** `de slo lint` fails loud on it. If you care about memory pressure, measure it
as a signal; the SLI is the user-visible symptom it causes (latency/availability).

## The non-negotiables

- **CUJ-first, not endpoint-first.** Start from a journey a user completes ("place an order"), then
  map it to the method(s) that serve it. The scaffold's `slo.yaml` groups methods for you
  (read = Get/List/BatchGet, write = Create/Update/Delete/Undelete); refine from there.
- **Symptom, not cause.** The SLI measures what the user feels (did the request succeed? was it
  fast?), not why it failed (a retry, a GC pause). Causes are Layer-0 signals.
- **Explicit valid denominator — exclude client faults.** Availability = good / **valid**, where
  **valid EXCLUDES client-fault codes** (the caller's mistake is not your outage):
  - **bad (server fault):** gRPC `UNKNOWN`, `DEADLINE_EXCEEDED`, `INTERNAL`, `UNAVAILABLE`,
    `DATA_LOSS` (HTTP 5xx).
  - **excluded from valid (client fault):** gRPC `INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS`,
    `PERMISSION_DENIED`, `UNAUTHENTICATED` (HTTP 4xx).
  - **good:** everything else, incl. `OK`. So `availability = 1 − server_faults / (all − client_faults)`.
  The derived defaults do this by construction; if you override it, keep the client-fault exclusion.
- **Measured target, not aspiration.** Set the objective from a baseline you measured
  (`de slo check` / query Cortex over the last window), not a round number. A generated default is
  marked `devedge.io/uncalibrated: "true"` and `de slo lint` **warns** until you calibrate it.
- **A window.** 28d rolling by default. Pick the window before the target — the target is meaningless
  without it.
- **An error budget.** budget = `1 − target`. At 99.9% over 28d that is ~40 min/28d. Compute it; it
  is the number the burn-rate alerts and the policy act on.
- **A mandatory error-budget policy.** Write what happens when the budget is spent (e.g. "freeze
  feature releases until the budget recovers over a full window"). An SLO with no policy is
  decoration — `de slo lint` **rejects** it.

## Pick the SLI type (SRE menu)

| Type | Good event | Use when |
|------|-----------|----------|
| **Availability** | request did not server-fault | every request/response service (the default) |
| **Latency** | request completed under a threshold | users feel slowness (the default, paired with availability) |
| **Freshness** | data served is younger than N | caches, pipelines, read replicas |
| **Correctness** | output matches the expected result | data-processing / derived data |
| **Coverage** | records processed / records eligible | batch / backfill jobs |
| **Throughput** | sustained rate met | streaming / ingest |
| **Durability** | data retained without loss | storage |

Availability + latency are derivable from the AIP contract; the others you author by hand (a
threshold/ratio SLI over a metric or a Loki log-derived SLI).

## The workflow (verbs)

1. **Start from the derived defaults.** The scaffold shipped `slo.yaml` with grouped read/write
   availability + latency SLOs. Regenerate after adding custom methods:
   `de slo generate` (orchestrates `slogen generate --openapi openapi/<svc>.openapi.yaml
   --service <proto-fqn>`).
2. **Name the journey + SLI type.** Rename/split a grouped SLO to a CUJ if that is clearer; add a
   freshness/correctness SLI by hand if the menu above calls for it.
3. **Measure the baseline** over the window (`de slo check`, or a Cortex query of the error ratio).
   Set the objective just below the achieved baseline — never above what you can sustain.
4. **Remove the un-calibrated marker** (`devedge.io/uncalibrated`) once the target is measured.
5. **Write the error-budget policy** in the `devedge.io/error-budget-policy` annotation (replace the
   TODO). State the consequence.
6. **Lint.** `de slo lint slo.yaml` — the classifier rejects a signal-as-SLI, a missing policy, a
   missing objective; it flags cause-based indicators and un-calibrated targets.
7. **Render + deploy.** `de slo render --target prometheus --in slo.yaml --out deploy/prometheus`
   (SLI recording rules + multi-window multi-burn-rate alerts), then set
   `monitoring.enabled: true` and paste the rule `groups` into your Helm values overlay. Dashboards:
   `--target grafana`.

## Burn-rate alerting (what render emits)

Multi-window multi-burn-rate (SRE Workbook ch. 5), so you page on real budget burn, not noise:

| Tier | Burn | Windows | Action |
|------|------|---------|--------|
| Fast | 14.4× | 1h ∧ 5m | **Page** |
| Medium | 6× | 6h ∧ 30m | **Page** |
| Slow | 1× | 3d ∧ 6h | Ticket |

Threshold = burn × budget = `burn × (1 − target)`. The queries reference the SDK's actual RED metric
(`rpc_server_call_duration_seconds_*` with `rpc_response_status_code`), so you write no PromQL.

## Anti-patterns the classifier catches

- **Signal as SLI** — `container_memory_utilization`, `queue_depth`, `cpu_usage` declared as an SLI →
  rejected. Measure the symptom.
- **No error-budget policy** — rejected. Decoration, not an objective.
- **Cause-based indicator** — `retries`, `restarts`, `gc_pause` → flagged. Measure what users feel.
- **Aspirational target** — a number with no measured baseline → warned. Calibrate first.
- **Availability counting client faults as failures** — 4xx / client-fault codes in the bad set →
  the derived denominator excludes them; keep it that way when you override.

## KPIs (Layer 0) — the vocabulary, not an SLO

Run `de slo kpis` for the reference: RED (Rate/Errors/Duration), the four golden signals
(Latency/Traffic/Errors/Saturation), and USE (Utilization/Saturation/Errors) in OTel semconv terms.
These are always-on and have **no target** — they diagnose; the SLO turns the symptom into the
objective.
