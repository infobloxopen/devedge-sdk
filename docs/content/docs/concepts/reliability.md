---
title: Reliability
weight: 9
---

Reliability in devedge-sdk is declared, not hand-wired: a service states its objectives as data, and
the SDK derives the monitoring plumbing from them. This page explains the model behind that — the
three declaration layers, what a good service-level indicator measures, and how an error budget turns
an objective into an operational decision. Read it before you author or review an `slo.yaml`; the
step-by-step is in [Define SLOs](../../how-to/operate/slo/).

## The three layers

Reliability is declared in three separate layers, each with a distinct owner and consequence. Keeping
them separate is what stops a monitoring signal from masquerading as a business objective.

1. **Signals (API KPIs).** The always-on telemetry every request produces: rate, errors, and duration
   per method (RED), the four golden signals, and resource utilization (USE). A signal is
   diagnostic. It has no target, and it pages nobody on its own. The SDK emits these automatically;
   the vocabulary is in the [KPI reference](../../reference/slo-kpis/).
2. **Service SLOs.** A per-service objective over an indicator: "99.9% of read requests succeed over
   28 days." A service SLO has a target, an error budget, and burn-rate alerts. The service team owns
   it. The SDK derives a good default from the API contract.
3. **Journey SLOs.** The outcome of a critical user journey, composed across several services, tied to
   the customer SLA. Product owns it. A journey SLO resolves each participating service's indicator
   across service boundaries.

A metric belongs to exactly one layer. A resource or saturation metric — CPU, memory, queue depth,
pool utilization — is a **signal**. Declaring it as an SLI is a category error: it measures how full
the service is, not whether a user's request succeeded. The [classifier](#the-classifier) rejects it.

## What a good SLI measures

A service-level indicator measures a user-visible **symptom**, expressed as a ratio of good events to
valid events.

- **Symptom, not cause.** The indicator captures what the user experiences — did the request succeed,
  was it fast — not the internal reason a request failed. A retry count or a garbage-collection pause
  is a cause; measure it as a signal.
- **Good over valid, with client faults excluded.** Availability is `good / valid`, where **valid
  excludes client-fault responses**. A caller that sends an invalid argument or asks for something
  that does not exist has not caused an outage, so those responses do not count against you:
  - **Bad** (server fault, counts against the objective): gRPC `UNKNOWN`, `DEADLINE_EXCEEDED`,
    `INTERNAL`, `UNAVAILABLE`, `DATA_LOSS`; HTTP 5xx.
  - **Excluded from valid** (client fault): gRPC `INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS`,
    `PERMISSION_DENIED`, `UNAUTHENTICATED`; HTTP 4xx.
  - **Good**: everything else, including `OK`.

  So `availability = 1 − server_faults / (all − client_faults)`. The derived defaults compute this
  denominator by construction.
- **A single latency threshold.** Latency is the fraction of valid requests that complete under a
  threshold, read from the same duration histogram the SDK already emits.

## Windows and error budgets

An objective is meaningless without a window. devedge defaults to a **28-day rolling window**.

The **error budget** is the failure the objective permits: `budget = 1 − target`. At 99.9% over 28
days, the budget is roughly 40 minutes of failed requests. The budget is the number operations acts
on:

- **Burn rate** is how fast the budget is being spent. A burn rate of 1 spends the whole budget
  exactly over the window; a burn rate of 14.4 spends 2% of it in an hour.
- **Multi-window multi-burn-rate alerts** page on fast burn and open a ticket on slow burn, so an
  alert corresponds to real budget loss rather than a single failed request. The
  [how-to](../../how-to/operate/slo/#burn-rate-alerting) lists the tiers.
- **The error-budget policy** states what happens when the budget is spent — for example, freezing
  feature releases until the budget recovers over a full window. Every SLO carries one. An SLO with
  no policy is a chart, not an objective.

## Objectives are calibrated, not guessed

A target is set from a **measured baseline**, not a round number. The SDK generates defaults (99.9%
availability, 99% of requests under a threshold) and marks them un-calibrated. You measure the
service's actual behavior over the window, then set the target just below what the service sustains.
An un-calibrated target is a placeholder; it warns on lint until you calibrate it.

## One interchange, many backends

Every objective compiles to **OpenSLO**, a vendor-neutral format. OpenSLO is the single source that
emitters read to project the objective to a backend: Cortex recording and burn-rate rules, Grafana
dashboards, or Loki log rules. The service declares the objective once; the projection to each backend
is generated. This mirrors how the enriched OpenAPI contract is the single source for client, CLI, and
Terraform generation.

## The classifier

`de slo lint` (and `slogen lint`) validates an `slo.yaml` and enforces the layer separation. It fails
the build on an error and reports a warning otherwise:

- A saturation or resource metric declared as an SLI — **error** ("that is a Layer-0 signal").
- An SLO with no error-budget policy — **error**.
- A cause-based indicator — **flagged**.
- An un-calibrated or aspirational target — **warning**.

Because the check is part of the build, an objective that conflates the layers cannot ship silently.
