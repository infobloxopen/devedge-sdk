---
title: Deploy
weight: 4
aliases:
  - /docs/guides/deploy/
---

A scaffolded service ships with a **deploy path for two real runtime targets** — Kubernetes/k3s
(first-class, via a framework-owned Helm chart + Flux GitOps) and Docker Compose (local/lightweight)
— behind a deployment-target seam. You pick a target; the framework renders the artifacts. Both
wire the SAME operational foundation the service serves out of the box: liveness/readiness probes,
the config env, the OTEL exporter, the DSN secret, and a shutdown grace period paired with the
service's graceful shutdown.

## Pick your targets

```sh
# Render both targets (the default):
devedge-sdk new service orders --resource Order --backend gorm

# Only one:
devedge-sdk new service orders --resource Order --deploy compose

# None:
devedge-sdk new service orders --resource Order --deploy none
```

The flag is `--deploy k8s,compose` (comma-separated; default renders both). Adding a runtime is
adding a `Target` adapter behind the seam — **no core change** — which is how AWS ECS will slot in
later (a documented stub is already registered; see *Future targets*).

## What lands in your repo

```
deploy/
  k8s/
    helmrelease.yaml      # Flux HelmRelease — reconciles the framework chart
    oci-repository.yaml   # Flux OCIRepository source — where the chart is published
    values.yaml           # the THIN overlay — the only chart input you author
    README.md
  compose/
    docker-compose.yml    # the service + its declared deps, wired to the same surface
```

You will notice there is **no Helm chart** in your repo — only the Flux glue and a values overlay.
That is by design (see below).

## Target 1 — Kubernetes / k3s (Flux GitOps)

The chart is **framework-owned and you never author it.** The canonical chart lives in the SDK and
is published by the framework to an OCI registry. Your repo carries only:

- a Flux **`HelmRelease`** that reconciles the chart with your values,
- an **`OCIRepository`** source pointing at the published chart, and
- a thin **`values.yaml`** overlay — the only chart input you edit.

The chart renders a `Deployment` (with `livenessProbe` → `/healthz`, `readinessProbe` → `/readyz`,
the config env, `OTEL_*` export, the DSN as a Secret, and `terminationGracePeriodSeconds`), a
`Service`, an optional `Ingress`, and resource requests/limits.

### Wire it up

1. Edit `deploy/k8s/values.yaml`: set `image.repository` (and `image.tag` for a pinned release),
   the OTEL collector endpoint, and the DSN (in prod, reference a pre-provisioned Secret via
   `dsn.existingSecret` so the connection string never lands in git).
2. Point `deploy/k8s/oci-repository.yaml` `spec.url` at your published chart registry.
3. Apply the overlay as a ConfigMap the `HelmRelease` references:
   ```sh
   kubectl create configmap orders-values -n orders \
     --from-file=values.yaml=deploy/k8s/values.yaml \
     --dry-run=client -o yaml | kubectl apply -f -
   ```
4. Commit `deploy/k8s/` and let Flux reconcile it.

### Why "you never see the chart"

A chart you can edit is a chart that drifts from the SDK. Keeping the chart embedded and published
by the framework means every service gets the same probe paths, env names, secret handling, and
grace period — and a chart fix ships to everyone via a version bump, not a copy-paste. You express
service-specific intent through the values overlay, nothing more.

## Target 2 — Docker Compose

`deploy/compose/docker-compose.yml` brings up the service and its declared dependencies (e.g. a
`postgres`), wired to the **same surface** as the chart: the config env, a `healthcheck:` hitting
`/healthz`, the `OTEL_*` export env (with a commented `otel-collector` service to switch on), and a
`stop_grace_period` matching the chart's grace period.

```sh
docker compose -f deploy/compose/docker-compose.yml up --build
```

This is the pure-local runtime — no cluster needed — that proves the seam with a second real
adapter and keeps your local environment shaped like production.

## Chart publication (one chart, two consumers)

The embedded chart is the single source of truth, consumed two ways:

1. **Prod / GitOps.** A release step publishes the chart to an OCI registry (or a GH Pages Helm
   repo); the emitted `HelmRelease` + `OCIRepository` reference it by version.
2. **Local / dev.** `de project up --deploy` renders the SAME embedded chart directly
   (`helm template`). One chart, two reconcilers — they cannot drift.

## Graceful shutdown

The generated `main` wires `signal.NotifyContext(ctx, SIGTERM, os.Interrupt)`, so a SIGTERM (the
k8s pod-termination signal) cancels the serve context: the gRPC and HTTP servers drain in-flight
requests, the readiness loop stops, and the OTel exporter flushes before exit. The chart's
`terminationGracePeriodSeconds` and compose's `stop_grace_period` (both 30s by default) give that
drain its time budget — keep them paired.

## Future targets

AWS ECS is a documented seam **stub**: it is registered and satisfies the same `Target` interface
(so the seam is proven open) but renders nothing yet — `--deploy ecs` returns a clear
"not implemented" message. Implementing it is adding an adapter, with no change to the core seam.

## Dependency-light by design

Deploy artifacts are **templates, not dependencies.** Rendering them adds nothing to your service's
runtime dependency closure — the chart and its YAML tooling live entirely in the CLI, never in the
service binary.
