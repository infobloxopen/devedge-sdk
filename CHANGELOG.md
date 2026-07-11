# Changelog

The canonical source of truth for releases is the Git release tags / GitHub Releases
of this repository. This file summarizes notable changes in a human-readable form,
following the spirit of [Keep a Changelog](https://keepachangelog.com/).

Where the underlying source explicitly stated a version, the change is grouped under
that version. Notable changes that were not tied to a specific version are collected
under [History](#history).

## History

### v0.61.1 — Tenant-seam hardening (WS-042)

Two multi-tenancy seams surfaced by the WS-042 security pentest, hardened for every consumer.
No API is removed or changed; a verified-tenant accessor is added and the change feed's tenant is
made strictly envelope-authoritative.

- **Change-feed tenant is always envelope-authoritative, never payload-derived (SEC-042-03).**
  `events.ChangeEventFromEvent` now assigns `ChangeEvent.Tenant` from the `Event` envelope's
  `AccountID` UNCONDITIONALLY, instead of only when `AccountID` was non-empty. Previously an
  `Event` with an empty `AccountID` kept the tenant decoded from the opaque payload, so a producer
  emitting a raw event with `AccountID: ""` and a forged payload `{"tenant":"victim"}` decoded to
  `Tenant: "victim"`. Now an empty envelope tenant CLEARS the payload value, so the change decodes
  to an empty tenant and the consumer's fail-closed handling applies. Legitimate tenantless changes
  (a resource with `ChangeFeedOptions.AllowMissingTenant`, e.g. system bootstrap / global
  resources) are unaffected — they carry `AccountID: ""` and correctly decode to an empty tenant.
- **`middleware.VerifiedTenantID(ctx) (string, bool)` (new).** Returns the tenant of the VERIFIED
  principal on context and `true`, or `""`/`false` when no verified principal is present. Unlike
  `TenantIDFromContext` it NEVER falls back to the client-settable `account-id` header, so it is the
  safe basis for a tenant/confidentiality decision on a path that does not rely on the generated
  repository's authz-gated scoping.
- **`TenantIDFromContext` documented as a routing convenience, not an authz boundary (SEC-042-01).**
  Its `account-id` header fallback is client-settable and, absent a verified principal, returns
  whatever the caller sent. The behaviour is unchanged (the event consumer and cell routing rely on
  it), but the godoc now states plainly that it MUST NOT back a tenant fence without an
  `Authenticator` in the chain — use `VerifiedTenantID` for confidentiality decisions. The fallback
  is deliberately NOT gated: it shares one context key with the trusted `WithTenantID` injection the
  event consumer uses, so gating on "an authn interceptor ran" cannot be done without breaking that
  legitimate tenantless path.

### v0.61.0 — Full-text search (WS-041)

A `q` collection operator for List, declared on the schema and generated across both storage
backends: mark a field `searchable`, optionally add a message-level calculated source, and query
it over the REST gateway with no hand-written search code.

- **`searchable` field option (new).** `infoblox.field.v1.FieldOptions.searchable` includes a
  plain `string`, `enum`, string-typed repeated, tags, or `Timestamp` field in a resource's search
  vector. A `secret` or `INPUT_ONLY` field cannot be marked searchable — `make generate` fails loud
  and names the field, because matching against a write-only or redacted value would leak it.
- **`(infoblox.storage.v1.search)` message option (new).** Declares calculated sources beyond
  field-flagged columns (`SearchSource`, either a `field` reference or a set of flavored `sql`/`cel`
  expressions — `SearchExprSet`/`SearchExpr`), the materialization strategy
  (`STRATEGY_JIT`/`STRATEGY_INDEXED`/reserved `STRATEGY_PROJECTED`), and the Postgres text-search
  config. Both options ship in `infoblox.field.v1` v1.0.0-alpha.5 and `infoblox.storage.v1`
  v1.0.0-alpha.2.
- **`cel`→SQL compiler (new).** A `cel`-flavored source compiles to a Postgres expression and a
  parallel SQLite expression from one type-checked AST, so a calculated value (an enum mapped to a
  display label, for example) stays portable to the SQLite dev/test driver without a hand-written
  second expression. A `sql/postgres` source with no `cel` alternate is Postgres-only: it generates
  without error on every backend, but fails at runtime — not at generation time — when queried over
  a non-Postgres connection. Both backends fail loud identically: `codes.Unimplemented`
  ("full-text search for `<Resource>` requires PostgreSQL").
- **`q` on List (new).** `persistence.ListOptions` gains `Search string`; `protoc-gen-svc` detects a
  `string q` field on a List request (the same convention as `filter`/`order_by`, each wired
  independently) and maps it in automatically. Both `protoc-gen-storage` (GORM) and `protoc-gen-ent`
  (ent) AND a full-text predicate onto the query after the AIP-160 filter `WHERE` —
  `to_tsvector(...) @@ websearch_to_tsquery(...)` on Postgres, a single case-insensitive `LIKE`
  contains over every source concatenated into one string on SQLite (a dev-only approximation, not
  equivalent to Postgres full-text semantics) — with the query term always a bound parameter. An
  empty or whitespace-only `q` is a no-op, returning every row. `q` composes with `filter`,
  `order_by`, and pagination in one List call, provided the request declares those fields too.
- **`STRATEGY_INDEXED` (new).** A resource that outgrows query-time search declares
  `strategy: STRATEGY_INDEXED` and gets a generated `search_vector` column
  (`GENERATED ALWAYS AS (...) STORED`) plus a `CREATE INDEX CONCURRENTLY ... USING GIN` migration,
  emitted as its own file so Postgres does not reject `CONCURRENTLY` inside a transaction. Emission
  is idempotent and uses a reserved migration-version band so a generated file never collides with a
  module's hand-authored migrations. The scaffold's `make sync-migrations` target (a prerequisite of
  `build`/`test`/`run`) copies the generated files from buf's git-ignored output directory into the
  module's committed, embedded migrations directory; running `de generate` directly requires a
  manual `make sync-migrations` afterward.
- **`x-aip-search` OpenAPI extension (new).** The enriched OpenAPI spec adds a `q` query parameter
  and an `x-aip-search` extension (searchable source names, strategy, text config) to a searchable
  resource's List operation, parallel to `x-aip-pagination`.
- **Toolchain prerequisite.** `searchable` and `(infoblox.storage.v1.search)` are additive schema
  options: an older `de`/toolchain generates a resource carrying them without error, but silently
  ignores both, emitting no `q` predicate and no migration. Full-text search requires the v0.61.0
  toolchain or later.
- **Docs.** [Add full-text search to a resource](https://github.com/infobloxopen/devedge-sdk/blob/main/docs/content/docs/how-to/model-and-persist/add-full-text-search.md),
  the `Annotations` concept page, and the `persistence` reference now cover the annotations, the
  three source flavors, choosing a strategy, and the generated migration files.

`STRATEGY_PROJECTED` is reserved for a future cross-service/global search index and is not
implemented in this release: declaring it is valid schema, but `make generate` fails loud rather
than silently emitting a local predicate.

### v0.59.0 — Nested-resource parent enforcement, credential rotation, and DX fixes

An issue-sweep release: a P1 authorization fix in generated CRUD, a credential
rotation primitive, the ent composition seam, and several developer-experience
fixes.

- **Nested URL parent enforcement (`protoc-gen-svc`).** For a nested AIP-122
  resource (`accounts/{parent}/entries/{id}`) the gateway bound the parent segment
  to a request field but the generated `Get`/`List`/`Delete` ignored it — leaking
  siblings across parents within a tenant. The generated handler now scopes `List`
  to the parent's foreign key (pushed down through an AIP-160 filter) and denies a
  cross-parent `Get`/`Delete` with `NotFound`. A nested URL whose resource has no
  matching scalar FK field is now a **fail-loud** codegen error, never a silent
  bind-and-ignore.
- **Per-service module ID override (`protoc-gen-svc`).** `Descriptor.ID` was derived
  only from the proto package short-name, so two or more services declared in one
  proto file collided (`duplicate module ID`) at `servicekit.Run`.
  `<Service>ModuleOptions` gains an optional `ID` field (package-derived default
  unchanged); the module-qualified resource name uses the effective ID.
- **`Remint<Field>` credential rotation (`protoc-gen-storage`, `protoc-gen-ent`).**
  A verify-only `credential` field now gets a generated `Remint<Field>` alongside
  `Verify<Field>`: it mints a fresh token, overwrites the row's four
  `_public_id`/`_salt`/`_hash`/`_hashspec` columns in place (tenant-scoped), and
  returns the new token once — the old token stops verifying immediately. No more
  delete-and-recreate to rotate a leaked token.
- **`GetBy<Field>` natural-key lookup (`protoc-gen-storage`, `protoc-gen-ent`).** A
  plain `unique: true` string field now gets a generated `GetBy<Field>` lookup
  (symmetric with `LookupBy<Field>Hash`), tenant-scoped and soft-delete aware — so
  a "resolve by natural key" needs no hand-formatted filter string. Fields unique
  only within a scope (`unique_with`) are excluded (ambiguous by a single value).
- **ent composition seam (scaffold, WS-012).** The `module/compose.go` seam now has
  an ent variant — `NewModule(*ent.Client) servicekit.Module` plus a host-owned
  `CreateSchema` migration path — so `de compose` can compose ent modules, mirroring
  the gorm `NewModule(db)`/`Models()` template.
- **Per-module minter/encryptor injection (scaffold).** The compose seam's
  `NewModule` now takes functional options (`WithCredentialMinter`/`WithEncryptor`)
  so a composed host can supply a policy-configured credential minter or secret
  encryptor per module instead of a hard-coded zero value. Default behavior is
  unchanged.
- **`devedge-sdk --version` / `version`.** The CLI now reports its build version
  (from `runtime/debug` build info); `installation.md` documents pinning
  `@vX.Y.Z` and verifying the installed version.
- **`OTEL_TRACES_EXPORTER` runtime selection.** `observability/otel` `Setup` now
  honors the `OTEL_TRACES_EXPORTER` env (`otlp`/`stdout`/`none`, plus the
  OTel-standard `console` alias for `stdout`) when `Config.Exporter` is empty, so
  dev can flip to stdout tracing without a code change. An explicit `Config.Exporter`
  still wins.

### v0.58.0 — Verify-only credential field mode (WS-033)

A new field mode for API keys and tokens: **`credential`**, the gold-standard "hash, never encrypt"
storage. Unlike `secret` (which keeps a reversible cipher so the value can be read back), a
credential is **minted by the server, returned to the client once, and only ever verified** — there
is no reversible copy at rest.

- **`secret.CredentialMinter` (new).** `Mint()` produces a prefixed split token
  `<prefix>_<public_id>_<secret>` (the client's copy) and a `StoredCredential` (public id + salt +
  salted one-way hash + hash spec) to persist. `Parse` splits a presented token; `Verify` recomputes
  the stored hash of `(salt‖secret)` and constant-time-compares. The default hash is **SHA-512/256**
  over a 256-bit CSPRNG secret (fast, FIPS-approved, length-extension-safe); **SHA-384** and
  **PBKDF2-SHA256** are selectable via a self-describing `HashSpec` for verify-time agility. Stdlib
  crypto only (`sha512`, `pbkdf2`, `rand`, `subtle`) — FIPS-clean, no new dependencies, **no HMAC**.
- **`credential: true` annotation (new).** `infoblox.field.v1.FieldOptions.credential` (with an
  optional `credential_prefix`). Both storage generators (`protoc-gen-ent`, `protoc-gen-storage`)
  emit `<field>_public_id` (UNIQUE), `<field>_salt`, `<field>_hash`, `<field>_hashspec` — and **no**
  plaintext column, cipher, or `secret`-style `_hash` — for the field. `Create` mints via a
  `*secret.CredentialMinter` and returns the token once; `Verify<Field>` parses a token, looks the
  record up by `public_id` (a global unique, so verification needs no tenant), and verifies in
  constant time. The field is omitted from every read response. Codegen fails loud when `credential`
  and `secret` collide on one field, or when a credential field is not a string.
- **`persistence.ErrNoMinter` (new).** A `Create` that would mint a credential with a nil
  `*secret.CredentialMinter` returns this (wrapped with the field name) rather than storing an
  unusable credential — the verify-only analogue of `ErrNoEncryptor`.

The `credential` / `credential_prefix` options ship in the canonical
`github.com/infobloxopen/apis/proto/infoblox/field` **v1.0.0-alpha.4**, which the SDK now consumes
directly (no workspace override).

Additive: `credential` is a new annotation, so existing services are unaffected until they adopt it.
However, switching a field from `secret` to `credential` **changes its schema** (the plaintext/cipher
and `secret` hash columns are replaced by the four credential columns), and old values cannot be
migrated — the framework never held a safe copy — so re-issue those credentials.

### Security: secrets-handling hardening (SEC-005..008)

A secrets-sweep white-box pass fixed four confirmed findings. SEC-006 and SEC-007
change generated-code behavior and take effect **on regeneration** (`make generate`
/ `de generate`). SEC-008 is a **breaking** change for the dev encryptor when given
an over-length key.

- **SEC-005 (DSN password leak in migrate errors).** `persistence/migrate` no longer
  formats a raw DSN into an error string. A new `redactDSN` helper blanks the URL
  userinfo password (and strips the `password=`/`pgpassword=` token from a libpq
  keyword/value DSN); the `url.Parse` branches wrap the redacted DSN and the parse
  reason (never the `*url.Error`, which embeds the raw URL), and the no-scheme
  branches emit a scheme-only message. A production DB password no longer reaches
  stderr / CI logs. Non-breaking (error-text only).
- **SEC-006 (silent secret drop / cross-backend panic on nil encryptor).**
  **Behavior change on regen.** The generated ent singular repository no longer
  *silently drops* a non-empty secret value when constructed with a nil
  `secret.Encryptor`; the ent-batch and GORM repositories no longer *panic*. All
  paths now fail loud with the new sentinel `persistence.ErrNoEncryptor` (wrapped
  with the field name). A service currently relying on the buggy nil-encryptor state
  will now get a hard error at Create/Update — wire a real encryptor.
- **SEC-007 (INPUT_ONLY field returned in responses).** **Behavior change on regen.**
  The ent and GORM generators now OMIT effective-`INPUT_ONLY` (write-only) fields
  from the generated response projection (`fromEnt<R>` / `fromModel_<R>`), exactly as
  they already omit `secret:true` fields — reusing `internal/aip.ResolveFieldBehavior`.
  The field is still persisted (write-only means "not echoed", not "not stored"), so
  the runtime now matches the emitted OpenAPI `writeOnly` contract. A consumer that
  (against the field's own contract) read back an `INPUT_ONLY` value will no longer
  receive it. The scaffold's proto comment was corrected to describe this mechanism.
- **SEC-008 (dev encryptor key handling + Vault error body).** **BREAKING (dev
  encryptor).** `secret.NewDev` now requires an **exactly 32-byte** key and panics on
  any other length; an over-length key is rejected rather than silently truncated to
  its first 32 bytes (which made two keys sharing a 32-byte prefix interchangeable and
  a partial rotation a no-op). This is breaking only for a caller that passed a key
  longer than 32 bytes — trim it to the intended 32-byte key. `secret/vault.go` no
  longer embeds the raw Vault HTTP response body in the returned error (status code
  only). Known limitation (out of scope): the dev encryptor's AES-GCM ciphertext still
  carries no key-version tag, so the dev encryptor has no safe key rotation — use the
  Vault Transit backend (which supports `Rewrap`) where rotation matters.

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

### Service-to-service tokens + multi-audience verification (WS-028)

Additive. An ergonomic, cached helper for calling another service, plus the
SEC-004 refinement it builds on. The trust boundary is the audience: within one
audience (an app's own services) the caller's inbound bearer is passed through;
across audiences the caller obtains a token scoped to the target via RFC 8693
token exchange, cached and one line for the developer.

- **Multi-audience verification (SEC-004 refinement).** `oidc.Config` now takes
  `ExpectedAudiences []string`; a token validates when ANY of its `aud` values
  matches ANY accepted audience, so one audience can cover a whole app's services.
  `ExpectedAudience` remains a convenience alias appended to the set. The
  fail-closed default (an empty set errors) and the `AllowAnyAudience` opt-out are
  unchanged.
- **Inbound-bearer stashing.** `authn.UnaryServerInterceptor` now stashes the raw
  verified bearer via `middleware.WithInboundBearer` next to the principal;
  `middleware.InboundBearerFromContext` reads it. This is the `subject_token` a
  delegation exchange acts on behalf of. Purely additive.
- **Outbound token seam (root `authn`, stdlib + grpc only).** `authn.TokenSource`
  (`TokenFor(ctx, targetAudience)`), `authn.AudienceResolver` +
  `authn.StaticAudiences` (target → audience map), `authn.UnaryClientInterceptor`
  (sets `authorization: Bearer` for outbound gRPC; fail-closed on an unmapped
  target), and `authn.NewRoundTripper` (the REST equivalent). No new root
  dependencies — the graph-isolation gate stays green.
- **The Exchanger (nested `authn/oidc` module).** `oidc.NewExchanger` implements
  `authn.TokenSource`: passthrough when the target audience is one the caller
  already holds, else an RFC 8693 form-POST to the STS token endpoint, with an
  in-memory cache keyed by `(subject, target-audience)` (TTL = token lifetime −
  skew) and single-flight to avoid a stampede. Fail-closed on a missing inbound
  bearer or any exchange error. go-jose stays confined to this module.

See the how-to: [Call another service](docs/content/docs/how-to/secure/call-another-service.md).

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
