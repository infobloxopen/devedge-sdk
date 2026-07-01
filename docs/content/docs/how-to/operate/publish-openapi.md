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

## Step 3 — publish via `de api publish`

`de api publish` is a thin wrapper that:

1. Re-runs `make generate` to ensure the spec is fresh.
2. Arranges the flat spec into the apx directory layout.
3. Shells out to `apx release prepare`.

```sh
de api publish \
  --api-id openapi/platform.data/orders/v1 \
  --version v0.1.0 \
  --lifecycle beta \
  --canonical-repo github.com/infobloxopen/apis
```

**Flags:**

| Flag | Required | Description |
|---|---|---|
| `--api-id` | yes | Full apx API ID, e.g. `openapi/platform.data/orders/v1` |
| `--version` | yes | Semantic version to publish, e.g. `v0.1.0` |
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
  apx release finalize --api openapi/platform.data/orders/v1 --version v0.1.0
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
  --version v0.1.0 \
  --lifecycle beta \
  --canonical-repo github.com/infobloxopen/apis

# 4. Open the PR on the canonical repo.
apx release submit

# 5. After the PR is merged — run this in canonical-repo CI:
apx release finalize \
  --api openapi/platform.data/orders/v1 \
  --version v0.1.0
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
