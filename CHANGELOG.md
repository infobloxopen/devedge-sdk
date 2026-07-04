# Changelog

The canonical source of truth for releases is the Git release tags / GitHub Releases
of this repository. This file summarizes notable changes in a human-readable form,
following the spirit of [Keep a Changelog](https://keepachangelog.com/).

Where the underlying source explicitly stated a version, the change is grouped under
that version. Notable changes that were not tied to a specific version are collected
under [History](#history).

## History

### Security: principal-authoritative, fail-closed tenant fence (SEC-001..004)

**BREAKING (tenant fence).** The multi-tenant storage fence now anchors on the
**verified principal** and **fails closed** on an absent tenant, closing four
confirmed vulnerabilities:

- **SEC-002 (wrong tenant source) + SEC-001 (fail-open policy).** `middleware.TenantIDFromContext`
  now returns the **verified principal's** `Tenant` (stashed by the authn/authz stage
  via `WithPrincipal`) as the authority; the `account-id` metadata header is only a
  routing/cells hint used as a fallback on principal-less paths (e.g. the event
  consumer) and can never widen a request's tenant scope. The `entrepo.TenantMixin`
  query interceptor, `checkTenantWrite`, and every generated ent/GORM read/write/batch
  fence now reject a tenant-scoped operation that has no established tenant with
  `codes.PermissionDenied` instead of silently running it unscoped. The generated ent
  `Get` applies an explicit `AccountID` clause rather than trusting the interceptor.
- **SEC-004 (missing audience).** `oidc.NewAuthenticator` now returns an error when
  `Config.ExpectedAudience` is empty, mirroring the existing `ExpectedIssuer` guard.
  Set the new `Config.AllowAnyAudience` to opt out explicitly (single-issuer bootstrap
  only).
- **SEC-003 (unbounded page size).** Generated `List` clamps a caller-requested
  `page_size` to `persistence.MaxPageSize` (1000, AIP-158); the response still
  paginates via `next_page_token`.

**Migration.**

- Ensure your `Authenticator`/`ClaimsMapper` populates `Principal.Tenant` (in dev,
  wire `grpcauthz.DevPrincipalFunc()`, which promotes the `account-id` header to the
  principal's tenant at the identity stage). A tenant-scoped request that reaches the
  repository without an established tenant now gets `PermissionDenied`.
- Run legitimate cross-tenant/system work (admin, migrations, background jobs) under
  `middleware.WithSystemContext(ctx)` — the sanctioned, auditable opt-out that bypasses
  the fence. Never derive it from client input.
- If you construct `oidc.Authenticator` directly with an empty audience, set
  `AllowAnyAudience: true`.

### Authentication (two-tier token model, WS-026)

- A new `authn` package defines the pluggable authentication seams — the "verify"
  and "mint" halves of a configurable two-tier token model — kept free of any
  JOSE/JWKS types so the SDK root stays dependency-light:
  - `authn.Authenticator` (Role 3, verify): `Authenticate(ctx, bearer) → authz.Principal`,
    fail-closed.
  - `authn.Issuer` (Role 2, mint): mints + signs the app bearer for an authored principal.
  - `authn.ClaimsMapper` (Role 2, claim authoring): maps a COARSE upstream identity
    (subject + app-access only) to the rich app-specific principal; `StaticClaimsMapper`
    is the hot-reloadable dev default.
  - `authn.UnaryServerInterceptor` runs before the authz interceptor, verifies the
    bearer, and stashes the verified principal (read via `authn.VerifiedPrincipal`) —
    the trusted-path replacement for `grpcauthz.DevPrincipalFunc`. `TokenTopology`
    selects two-tier (default) or single-issuer; the verify seam is topology-agnostic.
- `server.Config.Authenticator` and `servicekit.HostConfig.Authenticator` wire that
  interceptor before authz. Nil preserves prior behavior.
- The nested `authn/oidc` module holds the go-jose backend: `Issuer` (RS256 mint +
  JWKS), `Authenticator` (verify against a static or remote JWKS, fail-closed), and
  `RelyingParty` (the confidential auth-code + PKCE dance with an upstream IdP →
  coarse identity). The JOSE/OIDC dependencies arrive only on opt-in (proven by the
  graph-isolation check).
- `server.Config.HTTPHandlers` (and the `servicekit` passthrough) mount arbitrary
  net/http handlers on the server's HTTP endpoint — for an OIDC provider's
  authorization/token/JWKS/discovery endpoints, webhooks, a login UI, or static
  assets — alongside the gRPC gateway, through the same lifecycle/tracing/probes.
- `authz/devsvc` is the out-of-process, hot-reloadable sibling of the in-process
  `authz.DevAuthorizer`: a `Client` (implements `authz.Authorizer` over HTTP, the same
  seam `opaauthz` implements) and a `Handler` serving decisions from a grant `Store`
  with edit-on-disk hot-reload and an optional admin endpoint — the dev-manipulable
  authz reference. Production authz stays OPA/PARGS.
- The dev-manipulable config files are **YAML** (friendlier to hand-edit and
  hot-reload than JSON; a `.json` file with the same keys is still accepted since YAML
  is a superset). `authz/devsvc.LoadGrantsFile`/`WatchGrantsFile` parse a YAML grant
  list, and `authz.Grant` carries lowercase `json`+`yaml` tags
  (`tenant`/`subjects`/`verbs`/`resource`) so the grants file, the `devsvc` admin
  endpoint, and any serialization share one documented schema. (The IdP's
  `idp-clients.yaml` and `de idp clients sync` move to YAML in the companion repos.)

### Security (tenant isolation on Create)

- Generated `Create` stamps `account_id` from the caller's tenant context unconditionally, ignoring
  any client-supplied value, on both the GORM and ent backends. A caller can no longer plant a
  resource under another tenant's `account_id`; this mirrors the existing `Update` tenant-key guard
  and is covered by a `seccheck.AssertNoCrossTenantCreate` regression test.

### Reliability (SLI/SLO codification)

- Services declare reliability targets as data. The `slo` package is an OpenSLO v1 intermediate
  representation plus a derivation step that turns a service's enriched OpenAPI contract into GOOD
  default service-level objectives: grouped read/write availability and latency, with the client-fault
  status codes excluded from the valid denominator, a 28-day rolling window, a mandatory error-budget
  policy stub, and objectives marked un-calibrated until measured.
- A fail-loud classifier enforces the three declaration layers. `slogen lint` fails the build when a
  saturation or resource metric is declared as an SLI ("that is a Layer-0 signal"), when an SLO has no
  error-budget policy, or when an objective is missing; it flags cause-based indicators and warns on
  un-calibrated targets.
- The `slogen` tool derives (`generate`), validates (`lint`), and projects (`render`) objectives, and
  prints the Layer-0 KPI reference (`kpis`). `de slo` orchestrates it with the generator pinned to the
  SDK version. Open-core emitters project OpenSLO to a Cortex-ruler `PrometheusRule` (SLI recording
  rules plus multi-window multi-burn-rate alerts), a portable Grafana dashboard per service, and Loki
  log-derived rules. A `--preset-dir` seam lets an overlay swap the emitters.
- The generated queries reference the metric names the SDK emits by default — the new OpenTelemetry
  semantic convention (`rpc_server_call_duration_seconds_*` with `rpc_response_status_code`) after the
  Alloy-to-Cortex normalization. The metric prefix, unit suffix, and label names are configurable, so a
  semantic-convention change is a configuration change; a `LegacyGRPCNaming` preset covers the
  `OTEL_SEMCONV_STABILITY_OPT_IN` opt-in.
- A scaffolded service ships a starter `slo.yaml` with the four grouped defaults on disk. The deploy
  Helm chart gains gated `PrometheusRule` and `ServiceMonitor` templates (off by default). The
  `define-slo` skill drives an author to a good objective.
- Business/journey SLOs (Layer 2) render end to end. A hand-authored SLI can use a raw backend query
  on its `good`/`total` metric source to compose an objective across services — for example the product
  of two services' availability recording rules. The Prometheus, Grafana, and Loki emitters use the
  raw query directly (a `$window` token is filled with each burn-rate window, so a composed journey
  gets the same multi-window multi-burn-rate alerting); a raw query takes precedence over the typed
  otel-rpc source, and a source with neither fails loud. Journey SLOs are authored in a separate file
  from the derived service `slo.yaml` and carry `devedge.io/layer: journey`. The latency-bucket-
  boundary lint applies only to typed otel-rpc latency sources.

### Resource identity

- Resource ids are server-generated by default: a `Create` with an empty id mints one (a uuid7 by
  default), a caller-supplied id is honored, and an empty id is never persisted. This follows
  AIP-133, where user-specified ids are an opt-in.
- Identity is declared on a resource's id field with `(infoblox.field.v1.opts).id` — `strategy`
  (server-generated or user-settable) and `generator` (uuid7, uuid4, or custom). A user-settable id
  field rejects an empty value with `InvalidArgument`.
- Generation runs in the generated repository on both the ent and GORM backends, so every create
  path — the default CRUD handler and custom methods alike — produces an id with no consumer code.
- The `persistence.IDGenerator` seam (built-in uuid7 and uuid4, overridable per repository with
  `WithIDGenerator`) lets a host swap in its own identifier format.

### Aggregates

- The aggregate machinery ships on top of the [transaction seam](docs/content/docs/concepts/transactions/):
  the SDK-owned `infoblox.ddd.v1` annotations (`aggregate` / `member` / `references`),
  an `AggregateRepository[Root, ID]` with `Load`/`Save`, a fail-closed boundary gate at
  `Serve`, member write-redirection in the generated handlers, cascade-on-delete for owned
  members, and `etag`-as-aggregate-version.
- Backends supported: ent, GORM, and in-memory.
- The GORM backend reaches parity with ent: a tx-aware generated repository (`conn(ctx)`),
  `Load<Root>Aggregate` eager-load, cascade-on-delete tags, and a reusable
  `persistence/gormtx` adapter (`GormTxRunner`, `GormOutboxStore`, `GormIdempotencyStore`)
  wired through the same backend-neutral seams. The IAM fixture runs the F032
  transactional-outbox worked example on GORM as well as ent.

### Codegen

- **F027 (AC-007) — eliminated hand-written ent adapter (Run 9 `coupond`, before/after).**
  Before F027, the single largest build cost of a new ent service was hand-transcribing the
  ~300-line `ent_wiring.go` adapter plus its test harness (~85% of the effort; devedge-sdk #53).
  After F027 plus multi-surface support, an owner **and** its surfaces are fully generated on
  **both** backends with **zero** hand-written adapter — the apikey fixture's `APIKeySummary`
  surface (a tenant-scoped projection over `APIKey`) round-trips owner-write → surface-read on
  ent and GORM with no hand-written persistence code at all
  (`testdata/apikey/.../multisurface_test.go`).
- The generated `servicekit` module has a handler-override seam: `<Service>ModuleOptions` carries an
  optional `Handler`. A service that adds a custom or non-CRUD method sets `Handler` (an override
  that embeds the generated CRUD handler) instead of forking a hand-written module, and keeps the
  generated `Descriptor` — methods, authz rules, resource names. When `Handler` is unset the module
  takes the default `Repo` CRUD path; neither set fails closed at registration.

### API contract — field_behavior + lossless enriched OpenAPI

- **The full AIP `field_behavior` contract is now a first-class signal.** Previously only
  `OUTPUT_ONLY` was read (in three generators); `REQUIRED`, `IMMUTABLE`, and `INPUT_ONLY` are now
  resolved too, by a single shared resolver (`internal/aip`) that all codegen plugins **and** the
  OpenAPI enrichment pass call — so a service's compiled behavior and its published OpenAPI cannot
  drift. `field_behavior` is **derived** from `infoblox.field.v1.opts` where the mapping is sound
  (`secret` → `INPUT_ONLY`; `id.strategy` server-generated → `OUTPUT_ONLY`, user-settable →
  `IMMUTABLE`; `allowed_values` → enum) so services annotate once. `not_null` is **never** mapped to
  `REQUIRED` (storage nullability is not client-requiredness), and a contradictory field (e.g.
  `OUTPUT_ONLY` + `INPUT_ONLY`) fails codegen loud, naming the message, field, and behaviors.
- **`INPUT_ONLY` is honored at runtime.** `middleware/redact` strips write-only fields (the `secret`
  case plus explicit `INPUT_ONLY`) — `redact.Message` masks them for logging, and the new opt-in
  `redact.ResponseUnary()` interceptor clears them from responses so a write-only field is never
  returned on the wire (wire it via `server.Config.Interceptors`; it is not a framework default,
  since some services return a secret exactly once on create).
- **The published OpenAPI v3 is now lossless.** `cmd/openapiv2to3` reads a `FileDescriptorSet`
  (built by `buf build` in `make generate`) and runs a proto-**authoritative** enrichment pass:
  native `readOnly`/`writeOnly`/`required`/`enum` where OpenAPI can express the behavior, plus
  consumer-neutral `x-aip-field-behavior` (carries `IMMUTABLE`), `x-aip-resource` (AIP-122
  type/pattern/id-vs-name key), `x-aip-method` (AIP standard-method classification), `x-aip-pagination`
  (the `page_size`/`page_token`/`next_page_token` triad), and `x-aip-references` (WS-021 cross-service
  reference targets). The pass fails loud on a missing FDS or FDS↔swagger drift, so a downstream Go
  client, CLI, or Terraform generator can project the whole contract from one interchange. This is the
  P0 keystone of WS-024. See the [publish-OpenAPI how-to](docs/content/docs/how-to/operate/publish-openapi.md).

### Scaffold

- A new service is generated with a `<svc>_security_test.go` alongside its smoke test. It runs the
  `seccheck` assertions — complete authz rules, unknown-principal deny, tenant isolation, clean error
  messages, no secret-field leaks — as standard `go test` sub-tests, so "provable security in CI" is
  true for the generated service out of the box (its CI runs `go test ./...`). See the
  [Security Check how-to](docs/content/docs/how-to/secure/security-check.md).
- The generated `module/module.go` surfaces the module `Handler` seam: `Module()` shows the override
  option and an in-place recipe, so a scaffolded service grows past pure CRUD on the generated module
  instead of forking one. See the
  [custom-methods how-to](docs/content/docs/how-to/model-and-persist/custom-methods.md).

### Observability

- The access log records the gRPC code the client sees. The `LoggingUnary` interceptor now sits
  outside `ErrorMapperUnary`, so a handler that returns a persistence sentinel (mapped to, e.g.,
  `NotFound`) is logged as that mapped code rather than the raw-sentinel `Unknown` — the access log,
  the client response, and the RED metrics now agree, keeping SLO/error-budget dashboards keyed on
  `grpc.code` correct.
