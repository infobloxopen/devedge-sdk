---
title: Operate
weight: 1
sidebar:
  open: true
---

This section covers how to operate a devedge-sdk service in production. Use the guides below to wire observability, configure health and readiness probes, apply resilience policies, and deploy to Kubernetes or Docker Compose.

{{< cards >}}
  {{< card link="observability/" title="Observability" icon="chart-bar" subtitle="Traces, RED metrics, and trace-correlated logs — wired and backend-neutral." >}}
  {{< card link="health/" title="Health & Readiness Probes" icon="heart" subtitle="Liveness and readiness endpoints, custom checks, Kubernetes wiring." >}}
  {{< card link="resilience/" title="Resilience" icon="lightning-bolt" subtitle="Request timeouts, rate limiting, and circuit-breaker seam." >}}
  {{< card link="deploy/" title="Deploy" icon="cloud-upload" subtitle="Kubernetes/k3s (Helm + Flux GitOps) and Docker Compose deploy targets." >}}
  {{< card link="publish-openapi/" title="Publish OpenAPI v3" icon="globe-alt" subtitle="Publish your service's public API to the apx catalog and generate typed clients." >}}
{{< /cards >}}
