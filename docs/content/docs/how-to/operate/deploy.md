---
title: Deploy
weight: 4
aliases:
  - /docs/guides/deploy/
---

The deploy scaffolding generates ready-to-use deployment artifacts for your service. It supports two runtime targets through a shared deployment-target seam (the `Target` interface): Kubernetes/k3s is the first-class target, rendered via a framework-owned Helm chart and Flux GitOps; Docker Compose covers local development. Both targets wire the same operational foundation your service serves out of the box — liveness and readiness probes, config env, the OTEL exporter, the DSN secret, and the shutdown grace period — so your local environment stays shaped like production. Use this page when you need to configure, publish, or update either target after running `devedge-sdk new service`.

## Choosing a target

```sh
# Render both targets (the default):
devedge-sdk new service orders --resource Order --backend gorm

# Only one:
devedge-sdk new service orders --resource Order --deploy compose

# None:
devedge-sdk new service orders --resource Order --deploy none
```

The `--deploy` flag accepts a comma-separated list of targets (`k8s`, `compose`). The default renders both. Adding a target means adding a `Target` adapter behind the shared interface with no change to the framework core.

## Files generated in your repo

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

Your repo contains only the Flux glue and a values overlay, not a Helm chart. The chart itself is embedded in the SDK and managed by the framework.

## Kubernetes / k3s (Flux GitOps)

The Helm chart is framework-owned. The canonical chart lives embedded in the SDK. The planned publish target is `ghcr.io/infobloxopen/charts/devedge-service`.

{{< callout type="warning" >}}
**The framework does not yet publish the chart to that registry.** Until a publish step ships, you must extract the embedded chart and push it to your own OCI registry. See [Publishing the chart](#publishing-the-chart) below.
{{< /callout >}}

Your repo carries:

- a Flux **`HelmRelease`** that reconciles the chart with your values,
- an **`OCIRepository`** source pointing at the chart registry, and
- a thin **`values.yaml`** overlay — the only file you edit.

The chart renders a `Deployment` with the following configuration:

- `livenessProbe` → `/healthz`
- `readinessProbe` → `/readyz`
- config env
- `OTEL_*` export
- DSN as a Secret
- `terminationGracePeriodSeconds`

It also renders a `Service`, an optional `Ingress`, and resource requests/limits.

### Why the chart is not in your repo

A chart you can edit is a chart that drifts from the SDK. Keeping the chart embedded and published by the framework means every service gets the same probe paths, env names, secret handling, and grace period. A chart fix ships to all consumers via a version bump, not a copy-paste. You express service-specific intent through the values overlay.

### Publishing the chart

Publishing the chart yourself is required today: until the framework publishes the chart to its own registry, extract and push the embedded chart to your own OCI registry:

```sh
# 1. Extract the embedded chart (requires devedge-sdk CLI on PATH):
devedge-sdk chart export --out ./devedge-service

# 2. Package and push to your registry:
helm package ./devedge-service
helm push devedge-service-*.tgz oci://<your-registry>/charts

# 3. Update deploy/k8s/oci-repository.yaml spec.url:
#    url: oci://<your-registry>/charts
```

Once the framework publishes the chart, update `oci-repository.yaml` to point at `oci://ghcr.io/infobloxopen/charts` and remove your private copy.

### Wiring it up

1. Publish the embedded chart to your registry (see [Publishing the chart](#publishing-the-chart) above).
2. Edit `deploy/k8s/values.yaml`: set `image.repository` (and `image.tag` for a pinned release), the OTEL collector endpoint, and the DSN.

   {{< callout type="info" >}}
   **In production, reference a pre-provisioned Secret via `dsn.existingSecret`** so the connection string never lands in git.
   {{< /callout >}}

3. Point `deploy/k8s/oci-repository.yaml` `spec.url` at your published chart registry.
4. Apply the overlay as a ConfigMap the `HelmRelease` references:
   ```sh
   kubectl create configmap orders-values -n orders \
     --from-file=values.yaml=deploy/k8s/values.yaml \
     --dry-run=client -o yaml | kubectl apply -f -
   ```
5. Commit `deploy/k8s/` and let Flux reconcile it.

## Docker Compose

`deploy/compose/docker-compose.yml` brings up the service and its declared dependencies (for example, a `postgres` instance). It uses the same configuration surface as the Helm chart: the config env, a `healthcheck:` hitting `/healthz`, the `OTEL_*` export env (with a commented `otel-collector` service you can enable), and a `stop_grace_period` that matches the chart's grace period.

```sh
docker compose -f deploy/compose/docker-compose.yml up --build
```

Use the Compose target for purely local development when you do not need a cluster. It is a second real adapter behind the deployment-target seam, and it wires the same operational surface as the chart, so your local environment stays shaped like production.

## Chart publication

The embedded chart is the single source of truth. It is consumed two ways:

1. **Production / GitOps.** The chart is published to an OCI registry. The emitted `HelmRelease` and `OCIRepository` reference it by version. Until the framework publishes to `ghcr.io/infobloxopen/charts/devedge-service`, publish to your own registry (see [Publishing the chart](#publishing-the-chart) above).
2. **Local / dev.** `de project up --deploy` renders the same embedded chart directly via `helm template`. Both paths use the same chart source and cannot drift from each other.

## Graceful shutdown

The generated `main` wires `signal.NotifyContext(ctx, SIGTERM, os.Interrupt)`. When Kubernetes sends SIGTERM to terminate a pod, this cancels the serve context: the gRPC and HTTP servers drain in-flight requests, the readiness loop stops, and the OTel exporter flushes before exit.

The chart's `terminationGracePeriodSeconds` and Compose's `stop_grace_period` (both 30 seconds by default) set the time budget for that drain.

{{< callout type="warning" >}}
**Keep `terminationGracePeriodSeconds` and `stop_grace_period` in sync.** If they diverge, one target will force-kill the process before the drain completes.
{{< /callout >}}

## Future targets

AWS ECS is a registered seam stub. It satisfies the same `Target` interface, so the seam is proven open, but renders nothing yet — `--deploy ecs` returns a "not implemented" message. Implementing it requires adding an adapter with no change to the core interface.

## Runtime dependencies

Deploy artifacts are templates, not runtime dependencies. Rendering them adds nothing to your service's runtime dependency closure. The chart and its YAML tooling live entirely in the CLI, not in the service binary.
