---
title: Publish your service's public API as OpenAPI v3
weight: 5
aliases:
  - /docs/guides/publish-openapi/
---

`de api publish` publishes your service's gRPC-gateway REST surface as a versioned OpenAPI v3 specification to the **apx catalog** — a package registry for canonical API schemas. Use this workflow when you want external consumers to discover your API and optionally generate a typed Angular client from it, or when you need a stable, versioned artifact for catalog-driven tooling.

The end-to-end path is:

```
proto (google.api.http annotations)
  → buf generate → openapi/<svc>.openapi.yaml
    → de api publish → apx catalog (OCI artifact on GHCR)
      → apx client generate → @scope/<svc>-client (typed Angular package)
```

## Prerequisites

- The `devedge-sdk` CLI and `de` on PATH (see [Installation](../../../getting-started/installation/)).
- `apx` on PATH: `go install github.com/infobloxopen/apx@latest`
- `buf` on PATH (scaffolded services already have it): `go install github.com/bufbuild/buf/cmd/buf@latest`

## Step 1 — annotate RPCs with `google.api.http`

The gRPC-gateway HTTP/JSON transcoder and the OpenAPI emitter both read `google.api.http` options. Every RPC you want in the public REST surface needs one.

```proto {filename="proto/orders/v1/orders.proto"}
import "google/api/annotations.proto";

service OrderService {
  rpc CreateOrder(CreateOrderRequest) returns (Order) {
    option (google.api.http) = {post: "/v1/orders", body: "order"};
  }
  rpc GetOrder(GetOrderRequest) returns (Order) {
    option (google.api.http) = {get: "/v1/orders/{id}"};
  }
  rpc ListOrders(ListOrdersRequest) returns (ListOrdersResponse) {
    option (google.api.http) = {get: "/v1/orders"};
  }
  rpc DeleteOrder(DeleteOrderRequest) returns (DeleteOrderResponse) {
    option (google.api.http) = {delete: "/v1/orders/{id}"};
  }
}
```

Services scaffolded with `devedge-sdk new service` already import `google/api/annotations.proto` and include the `protoc-gen-openapiv2` entries in `buf.gen.yaml`. If you are adding HTTP annotations to an existing service, add those `buf.gen.yaml` entries manually.

## Step 2 — generate `openapi/<svc>.openapi.yaml`

```sh
make generate
```

`make generate` runs `buf generate`, which invokes both the Go/gRPC plugins and the `protoc-gen-openapiv2` plugin. The SDK's codegen step converts the grpc-gateway OpenAPI v2 output to a flat OpenAPI v3 YAML file:

```
openapi/
  orders.openapi.yaml    # ← the flat v3 spec; commit this
```

Inspect the file to confirm it lists your operations and schemas before publishing.

### The spec is enriched from proto — losslessly

The converter does more than a v2→v3 shape change: it reads a `FileDescriptorSet` (built by
`buf build` in the same `make generate` step) and runs a **proto-authoritative enrichment pass** so
the published spec carries the full AIP contract a downstream client, CLI, or Terraform generator
needs — the single interchange they project from. Where the proto truth and the base conversion
disagree, the proto value wins. Per property and operation you get:

- native `readOnly` (`OUTPUT_ONLY`), `writeOnly` (`INPUT_ONLY`, incl. `secret`), `required`
  (`REQUIRED`), and `enum` (`allowed_values`);
- `x-aip-field-behavior` — the raw behavior list, so `IMMUTABLE` (which OpenAPI cannot express
  natively) survives;
- `x-aip-resource` — the AIP-122 resource type, pattern(s), and whether the resource is addressed by
  `id` or `name`;
- `x-aip-method` — the AIP standard-method classification (`Create`/`Get`/`List`/…);
- `x-aip-pagination` — the `page_size`/`page_token`/`next_page_token` triad on List operations;
- `x-aip-references` — cross-service reference targets (see [cross-service references](../../model-and-persist/)).

The extensions are consumer-neutral (`x-aip-*`, never `x-terraform-*` or similar): the contract
names AIP facts, and each consumer's generator maps them in its own glue (e.g. Terraform maps
`IMMUTABLE` → `ForceNew`). The pass fails loud if the descriptor set is missing, so the spec is
never silently missing contract; drift between the swagger and the descriptor set is reported (see
below), or made a hard failure with `-strict`.

### How the converter matches the swagger to the proto contract

The converter behind `make generate` is `openapiv2to3` (in this repo under `cmd/openapiv2to3`). It
matches swagger operations to proto methods by verb and path template from the `google.api.http`
rules (tolerating a patched `basePath` prefix), rewrites each matched `operationId` to the canonical
`Service_Method` form (the original is kept as `x-legacy-operation-id` when it differs), auto-detects
snake_case vs camelCase property names, and resolves definition names back to proto messages. The
swagger's `basePath` survives as `servers:`.

This tolerant matching is the **default for every input** — swagger emitted by gateway-v2
(`protoc-gen-openapiv2`) and by the old grpc-gateway v1 / atlas toolchain (`protoc-gen-swagger`,
whose ids are not `Service_Method` and whose properties are often snake_case) convert the same way,
with no flag:

```sh
openapiv2to3 -descriptor api.binpb service.swagger.json openapi/
```

| Flag | Description |
|---|---|
| `-json-names` | `auto` (default), `snake`, or `camel` — how schema properties are keyed against proto fields |
| `-strict` | Fail (non-zero exit, no output) when an operation, schema, or field is unmatched or ambiguous, instead of reporting |
| `-compat=gateway-v1` | **Deprecated no-op** (accepted for one release): the tolerant matching it used to gate is now the default |

Rather than failing loud, the converter writes a per-file coverage report — operations and schemas
matched/unmatched, fields enriched/skipped, formats sanitized, path templates repaired — to stderr on
every run, and to `<name>.openapi.yaml.coverage.json` next to the output **when there is something to
review** (a clean conversion writes just the spec). Review the report before publishing; pass
`-strict` to make any gap a hard failure. Proto methods that have no swagger operation — a swagger
that exposes only part of a service is a legitimate partial view — are always a report section, never
a `-strict` failure.

## Step 3 — publish via `de api publish`

`de api publish` is a thin wrapper that:

1. Re-runs `make generate` to ensure the spec is fresh.
2. Arranges the flat spec into the apx directory layout.
3. Shells out to `apx release prepare`.

```sh
de api publish \
  --api-id openapi/platform.data/orders/v1 \
  --version v1.0.0-beta.1 \
  --lifecycle beta \
  --canonical-repo github.com/infobloxopen/apis
```

{{< callout type="warning" >}}
**The version must match the lifecycle and the API line.** A `beta`, `rc`, or `experimental`
lifecycle requires a prerelease tag (`v1.0.0-beta.1`), and a plain `v1.0.0` requires
`--lifecycle stable`. The version's major must also match the API line — an `.../v1` API takes
`v1.x.x`, not `v0.x.x`. `apx` rejects a mismatch (`LIFECYCLE_MISMATCH` / `VERSION_LINE_MISMATCH`).
{{< /callout >}}

**Flags:**

| Flag | Required | Description |
|---|---|---|
| `--api-id` | yes | Full apx API ID, e.g. `openapi/platform.data/orders/v1` |
| `--version` | yes | Semantic version to publish; must match the lifecycle and line, e.g. `v1.0.0-beta.1` for beta on `/v1` |
| `--canonical-repo` | yes | Canonical APIs repo, e.g. `github.com/infobloxopen/apis` |
| `--lifecycle` | no | `beta` (default) or `stable` |
| `--service-dir` | no | Service root; defaults to the current directory |
| `--skip-generate` | no | Skip `make generate`; use the existing `openapi/<svc>.openapi.yaml` |
| `--submit` | no | Also run `apx release submit` automatically (opens the PR) |
| `--client` | no | After publishing, also generate a typed client with `apx client generate` (see Step 4) |
| `--client-out` | no | Output directory for the generated client (default: `clients/<svc>-client`) |
| `--client-scope` | no | npm scope for the client package, e.g. `@acme` |
| `--publish-client` | no | Publish the client to GitHub Packages instead of only generating it |

By default the command **prepares only** and prints the two follow-on commands:

```
prepare complete — next:
  apx release submit
  # (after the PR is merged on the canonical repo:)
  apx release finalize --api openapi/platform.data/orders/v1 --version v1.0.0-beta.1
```

Review the PR, merge it, then run `apx release finalize` in canonical-repo CI to land the OCI artifact in the apx catalog.

### Using the raw `apx release` sequence directly

If you prefer to skip `de api publish`, the equivalent manual steps are:

```sh
# 1. Generate the spec.
make generate

# 2. Arrange into the apx layout (apx resolves the ID to this directory).
mkdir -p openapi/platform.data/orders/v1
cp openapi/orders.openapi.yaml openapi/platform.data/orders/v1/orders.openapi.yaml

# 3. Prepare the release (adds a release entry + writes the OCI manifest locally).
apx release prepare openapi/platform.data/orders/v1 \
  --version v1.0.0-beta.1 \
  --lifecycle beta \
  --canonical-repo github.com/infobloxopen/apis

# 4. Open the PR on the canonical repo.
apx release submit

# 5. After the PR is merged — run this in canonical-repo CI:
apx release finalize \
  --api openapi/platform.data/orders/v1 \
  --version v1.0.0-beta.1
```

The `apx.yaml` in your service repo must declare:

```yaml {filename="apx.yaml"}
version: 1
org: infobloxopen
repo: apis            # the canonical-apis-repo short name
module_roots:
  - openapi
```

## Step 4 — generate a typed Angular client

`apx client generate` produces a packaged, buildable TypeScript/Angular client from the OpenAPI v3 spec. It emits a consumable `@<scope>/<svc>-client` npm module — a barrel of typed operations, models, and an `ApiConfiguration` — rather than loose files you copy into an app. Run it against the same flat spec `make generate` produced, or against a copy fetched from the apx catalog.

```sh
apx client generate \
  --input openapi/orders.openapi.yaml \
  --scope @acme \
  --package orders-client
```

This writes the package to `clients/orders-client/` by default. Pass `--build` to also run `npm install` and `npm run build`, so the package's `dist/` is ready for a consumer.

{{< callout type="info" >}}
**apx runs ng-openapi-gen under the hood.** The generator reads the OpenAPI v3 spec that the `make generate` converter step writes to `openapi/<svc>.openapi.yaml`. apx wraps the output in a versioned npm package; you consume the package, not the raw generator output.
{{< /callout >}}

{{< callout type="info" >}}
**The generated model is a single read-and-write interface.** ng-openapi-gen emits one TypeScript
interface per schema, so `readOnly` fields (`etag`, the server-set `name`, `deleteTime`) appear as
optional properties rather than being removed from the write shape. Treat them as server-owned when
you build a create or update payload; the `x-aip-*`/`readOnly` enrichment documents which fields
those are.
{{< /callout >}}

The generated barrel exports typed operations, models, and `provideApiConfiguration`, a one-line Angular provider that sets the client's base URL:

```typescript
import { provideApiConfiguration, noteServiceListNotes } from '@acme/orders-client';

// In your Angular module or bootstrap providers:
providers: [
  provideApiConfiguration('https://orders.example.com'),
]
```

To publish the package to GitHub Packages for other repos to install, run `apx client publish`. It generates, builds, and publishes in one step. The default is a validating dry run; pass `--dry-run=false` to publish for real:

```sh
apx client publish \
  --input openapi/orders.openapi.yaml \
  --scope @acme \
  --package orders-client \
  --dry-run=false
```

`de api publish --client` runs this client generation as part of the publish flow. Add `--publish-client` to publish the package instead of only generating it. See [`de api publish`](https://github.com/infobloxopen/devedge) for the flags.

## Versioning and lifecycle

| Lifecycle | When to use |
|---|---|
| `beta` | API shape is still evolving; consumers should pin to a minor range |
| `stable` | API shape is committed; breaking changes require a new `<line>` (e.g. `v2`) |

Publish a new `--version` for each backwards-compatible change. For a breaking change, start a new line (`openapi/platform.data/orders/v2`) and deprecate the old one via `apx deprecate`.

Two rules the version must satisfy, or `apx` rejects the release:

- **Major matches the line.** An `.../v1` API takes `v1.x.x`; an `.../v2` API takes `v2.x.x`.
- **Prerelease matches the lifecycle.** `beta`, `rc`, and `experimental` require a prerelease tag
  (`v1.0.0-beta.1`); `stable` requires a plain version (`v1.0.0`).

## Next: host the client in a micro-frontend

The generated client issues HTTP calls but does not attach an access token. In a devedge micro-frontend, the shell owns the session and a bearer interceptor attaches the token to every request the generated client makes.

To host this client in an Angular micro-frontend:

1. Scaffold the micro-frontend with `de ufe new <name>` from the [devedge](https://github.com/infobloxopen/devedge) CLI (the same tool that scaffolds backend services with `de new service`).
2. Add the generated client as a dependency. For a local hot loop, point at the package directory with a `file:` link and skip publishing:

   ```json {filename="package.json"}
   "dependencies": {
     "@acme/orders-client": "file:../../clients/orders-client"
   }
   ```

   Once you publish with `apx client publish`, switch the `file:` link to the published version.
3. Set the client's base URL with `provideApiConfiguration(rootUrl)`, pointing at the service's stable HTTPS hostname:

   ```typescript
   import { provideApiConfiguration } from '@acme/orders-client';

   providers: [
     provideApiConfiguration('https://orders.example.com'),
   ]
   ```

4. Wire the session with [`devedge-ufe-sdk`](https://github.com/infobloxopen/devedge-ufe-sdk): the shell instantiates the OIDC `SessionProvider`, and `provideDevedgeSession` plus the bearer interceptor attach the token to the generated client's requests.

For a complete backend-and-frontend example — a devedge-sdk service and an Angular micro-frontend that consumes the generated client via a `file:` link and `provideApiConfiguration` — see [`examples/fullstack-oss`](https://github.com/infobloxopen/devedge-ufe-sdk/tree/main/examples/fullstack-oss) in the micro-frontend SDK.
