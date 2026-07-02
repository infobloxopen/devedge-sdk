# Feature Specification: Contract enrichment — full AIP `field_behavior` + a lossless enriched OpenAPI

**Feature Branch**: `044-contract-enrichment`
**Created**: 2026-07-02
**Status**: Draft
**Initiative**: WS-024 (out-of-the-box CLI & Terraform-provider surfaces) — **P0 keystone**

## Context

WS-024 gives devedge services two new interaction surfaces out of the box — an app CLI
and a Terraform provider — as **generated projections of the one API contract** a service
already declares (proto → gRPC + REST + OpenAPI v3, AIP resource-oriented). A three-repo
survey (specs/cli-and-terraform-seam-proposal.md in the hub) found the generation plumbing
~80% ready but the **contract too thin to project from**. Nothing good ships until the
contract carries the semantics a CLI or a Terraform provider needs. This feature is that
keystone. It changes the *contract*, not the surfaces; the CLI (P1), Terraform provider
(P2), and Go client (apx, fast-follow) all read what this feature produces.

Two gaps, both evidenced in this repo today:

1. **`google.api.field_behavior` is impoverished.** Only `OUTPUT_ONLY` is read, in three
   generators via an identical `HasExtension → range → == OUTPUT_ONLY` idiom
   (`cmd/protoc-gen-svc/main.go:200-215` `fieldIsOutputOnly`;
   `cmd/protoc-gen-storage/main.go:172-179`; `cmd/protoc-gen-ent/main.go:176-183`).
   `REQUIRED`, `IMMUTABLE`, and `INPUT_ONLY` are **entirely absent** (exhaustive grep).
   Without them there is no signal for a Terraform `Required`/`ForceNew`/`Sensitive`
   schema, and no signal for a CLI required-flag or secret prompt. This is the #1 blocker.

2. **OpenAPI is lossy.** `cmd/openapiv2to3/main.go` converts a grpc-gateway v2 swagger JSON
   to v3 (`openapi2conv.ToV3`) and writes YAML; it reads **nothing else**. Resource
   identity (AIP-122 type/pattern), the id-vs-name key, method classification, cross-service
   references (WS-021), the pagination triad, and the full `field_behavior` never reach the
   spec — they are Go-only generator internals (`classifyMethod` at
   `cmd/protoc-gen-svc/main.go:293-398`; AIP-122 consts emitted into Go by
   `cmd/protoc-gen-storage/render.go:986-997`). A separate-process generator (Go client, CLI,
   Terraform) that reads OpenAPI cannot see any of it. There is **no serialized proto
   descriptor produced anywhere in the build** to recover it from, either.

This feature closes both, coupled: Part A adopts the full AIP `field_behavior` contract as
the single client-facing signal; Part B makes OpenAPI **lossless** so that contract (and the
rest of the AIP metadata) reaches every downstream generator through one interchange. Part B
depends on Part A — the enriched OpenAPI surfaces the behaviors Part A introduces.

**Non-goals of this feature** (WS-024 later phases / deferred): the CLI, the Terraform
provider, the Go client generator (apx), and a pollable LRO contract (`google.longrunning`,
G7c) are out of scope. This feature produces the contract they consume; it builds none of
them. It also does not change authz (propagate principal, decide nothing — WS-021 posture)
and does not touch the DDD write boundary.

## Ratified design decisions

These come from the hub proposal §11 (D1/D3, ratified 2026-07-02) plus decisions this spec
adds from the ground-truth survey. They are settled inputs, not open questions.

- **D1 — Enriched OpenAPI v3 is the single codegen interchange.** Proto stays the source of
  truth, but rather than choose proto-native generators vs. lossy gateway OpenAPI, OpenAPI is
  made lossless via an enrichment pass. All downstream generators read that one spec. A
  serialized descriptor is a secondary artifact only if a consumer needs what OpenAPI cannot
  carry.
- **D3 — `google.api.field_behavior` is the single client-facing contract axis.** The whole
  toolchain (grpc-gateway, HashiCorp tooling, oapi-codegen) understands it; `infoblox.field.v1`
  stays private, for storage/sensitivity mechanics. `field_behavior` is **derived** from
  `infoblox.field.v1` only where the mapping is sound (below), so services keep annotating
  once. A fail-loud consistency lint rejects contradictions.
- **Derivation table (sound mappings only).**
  - `field.v1.opts.secret = true` → `INPUT_ONLY` (write-only; never returned).
  - `field.v1.opts.id.strategy = STRATEGY_SERVER_GENERATED` → `OUTPUT_ONLY`.
  - `field.v1.opts.id.strategy = STRATEGY_USER_SETTABLE` → `IMMUTABLE`.
  - `field.v1.opts.allowed_values = [...]` → OpenAPI `enum` (not a field_behavior).
  - **`not_null` is NEVER mapped to `REQUIRED`.** Storage nullability ≠ API contract
    requiredness (a server-defaulted column is `NOT NULL` yet not client-required). `REQUIRED`
    is only ever explicit `google.api.field_behavior = REQUIRED` on the proto field.
- **D-new-1 — One shared AIP resolver, no drift.** The behavior resolution + AIP method
  classification + AIP-122 resource identity logic is extracted into a shared internal package
  (working name `internal/aip`) imported by all three generators **and** the OpenAPI enrichment
  pass. The classifier must not be reimplemented in the enrichment tool, or a service's compiled
  behavior and its published OpenAPI will diverge silently. `classifyMethod`,
  `detectServiceResource`, and the resource-pattern parsing currently living in
  `cmd/protoc-gen-svc/main.go` (`package main`, unimportable) move there behind exported funcs.
- **D-new-2 — The enrichment pass reads a FileDescriptorSet.** No FDS is produced in the build
  today. A `buf build -o <name>.binpb` (or `protoc --descriptor_set_out`) step is added to
  `make generate`, and `cmd/openapiv2to3` gains an FDS input arg. The FDS is unmarshalled with
  `descriptorpb.FileDescriptorSet` (the idiom already in `cmd/security-check/main.go:32-51`).
- **D-new-3 — Vendor extensions are consumer-neutral.** Where OpenAPI has a native field
  (`required`, `readOnly`, `writeOnly`, `enum`) the enrichment writes it. Everything else is an
  `x-aip-*` extension carrying the raw AIP fact, never a consumer-specific key. The proposal's
  illustrative `x-terraform-force-new` is **rejected**: the contract must not name Terraform.
  `IMMUTABLE` is surfaced as `x-aip-field-behavior: [IMMUTABLE]` and the Terraform generator (P2)
  maps it to `ForceNew` in its own glue.

## Requirements

### Part A — the `field_behavior` contract

- **FR-A1**: A shared package (`internal/aip`) MUST expose a function that resolves the
  **effective** `field_behavior` set for a proto field: the union of explicit
  `google.api.field_behavior` values and the values derived from `infoblox.field.v1.opts` per
  the D3 derivation table. It MUST NOT derive `REQUIRED` from `not_null`.
- **FR-A2**: The resolver MUST fail loud (return an error that aborts codegen) on a
  contradictory field: `OUTPUT_ONLY` combined with `REQUIRED` or `INPUT_ONLY`; an explicit
  `field_behavior` that contradicts a derived one (e.g. `secret`→`INPUT_ONLY` on a field also
  marked `OUTPUT_ONLY`). The error MUST name the message, field, and the conflicting behaviors.
- **FR-A3**: `protoc-gen-svc`, `protoc-gen-storage`, and `protoc-gen-ent` MUST replace their
  three ad-hoc `== OUTPUT_ONLY` checks with the shared resolver. Existing `OUTPUT_ONLY`-driven
  behavior (soft-delete `delete_time` detection, output-only column omission, ent output-only
  fields) MUST be unchanged for existing fixtures (regression-safe re-point).
- **FR-A4**: `INPUT_ONLY` MUST be honored by the runtime the same way `secret` is today —
  `middleware/redact` MUST strip an `INPUT_ONLY` field from responses (an `INPUT_ONLY` field is
  a superset of the secret case: never returned). `secret` continues to also drive
  storage encryption/hashing; `INPUT_ONLY` alone drives redaction only.
- **FR-A5**: The scaffold proto template
  (`cmd/devedge-sdk/internal/scaffold/templates/proto.proto.tmpl`) and the toy/apikey/iam
  fixtures MUST gain at least one `REQUIRED`, one `IMMUTABLE`, and one `INPUT_ONLY` field so the
  new behaviors are exercised end-to-end by generated code and the openapi golden.
- **FR-A6**: A `USER_SETTABLE` id (`STRATEGY_USER_SETTABLE`) MUST resolve to `IMMUTABLE` and a
  `SERVER_GENERATED` id to `OUTPUT_ONLY` without the service having to also write
  `google.api.field_behavior` — the one annotation suffices (the "annotate once" property).

### Part B — the lossless enriched OpenAPI

- **FR-B1**: `make generate` MUST produce a serialized `FileDescriptorSet` for the toy fixture
  (`buf build -o testdata/toy/toy.binpb` or equivalent) as a build artifact.
- **FR-B2**: `cmd/openapiv2to3` MUST accept the FDS as an additional input and run an enrichment
  pass on the in-memory `openapi3.T` (between `ToV3` and serialization,
  `cmd/openapiv2to3/main.go` ~L46–L49) using the **same** `internal/aip` resolver/classifier as
  the generators (D-new-1).
- **FR-B3**: For every schema property, the enriched spec MUST carry the field's effective
  behavior: native `readOnly: true` for `OUTPUT_ONLY`; native `writeOnly: true` for
  `INPUT_ONLY`; native `enum: [...]` for `allowed_values`; membership in the schema's `required`
  array for `REQUIRED`; and an `x-aip-field-behavior` list of the raw behavior enum names
  (so `IMMUTABLE`, which OpenAPI cannot express natively, and any others survive losslessly).
- **FR-B4**: Each resource schema MUST carry an `x-aip-resource` extension with its AIP-122
  resource `type`, `pattern`(s), and the id-vs-name key (whether the resource is addressed by
  `id` or by `name`), recovered from the `google.api.resource` option via the shared resolver.
- **FR-B5**: Each operation MUST carry an `x-aip-method` extension naming its AIP standard-method
  classification (`Create`/`Get`/`List`/`Update`/`Delete`/`Undelete`/`BatchGet`/none), computed
  by the shared classifier — the CRUD-op→resource mapping a Terraform/CLI generator needs.
- **FR-B6**: List operations MUST carry an `x-aip-pagination` extension identifying the
  `page_size`/`page_token`/`next_page_token` triad, and cross-service reference fields MUST carry
  `x-aip-references` reusing the WS-021 `resource_reference` metadata (target type/module).
- **FR-B7**: The enrichment pass MUST fail loud (non-zero exit) if the FDS is missing/unreadable,
  or if a message/field present in the FDS classification is absent from the swagger (or vice
  versa) in a way that would silently drop contract — losslessness is enforced, not best-effort.
- **FR-B8**: `testdata/toy/openapi/toy.openapi.yaml` MUST be regenerated as the golden and
  checked in, demonstrating `readOnly`/`writeOnly`/`required`/`enum` and every `x-aip-*`
  extension on the toy resources.

### Cross-cutting

- **FR-X1**: `go build ./... && make test` clean; `make generate` produces no diff on a second
  run (idempotent); `scripts/check-graph-isolation.sh` stays green (no new heavy deps in the
  root module — `internal/aip` uses only `protoreflect`/`descriptorpb`, and kin-openapi is
  already a dependency).
- **FR-X2**: The scaffold mirror of `field.proto`
  (`cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/field/v1/field.proto`) stays byte-identical
  to the canonical (`make sync-scaffold-mirrors` clean). No proto/annotation *schema* change is
  required by this feature — `field_behavior` already exists in `google/api`; this feature starts
  *using* its full range. (No new `infoblox.field.v1` release.)

## Acceptance Criteria

- **AC-1**: A proto field annotated `[(google.api.field_behavior) = REQUIRED]` appears in its
  message schema's `required` array in the enriched OpenAPI; a field with only
  `field.v1.opts.not_null = true` (and no explicit `REQUIRED`) does **not**.
- **AC-2**: A field annotated `[(google.api.field_behavior) = IMMUTABLE]`, and a
  `field.v1.opts.id.strategy = STRATEGY_USER_SETTABLE` id field with no other annotation, both
  carry `x-aip-field-behavior: [IMMUTABLE]` (proving derivation + explicit both work).
- **AC-3**: A `field.v1.opts.secret = true` field (or an explicit `INPUT_ONLY` field) is
  `writeOnly: true` in OpenAPI and is stripped from a real gRPC/REST response by
  `middleware/redact` (exercised by the toy fixture, not only asserted statically).
- **AC-4**: A `field.v1.opts.allowed_values = ["A","B"]` field carries `enum: ["A","B"]` in the
  enriched schema.
- **AC-5**: Each toy resource schema carries `x-aip-resource` with the correct
  type/pattern/key; each RPC carries the correct `x-aip-method`; the List RPC carries
  `x-aip-pagination`; a cross-service reference field carries `x-aip-references`.
- **AC-6**: The compiled generator and the published OpenAPI agree — a golden test feeds the
  same toy FDS to both the generator path and the enrichment pass and asserts the classification
  matches (no drift; enforces D-new-1).
- **AC-7**: `make generate && git diff --exit-code` is clean (deterministic, checked-in golden).

## Failure Modes (must be handled, fail-loud not silent)

- **FM-1 — Contradictory behaviors** (`OUTPUT_ONLY`+`REQUIRED`, `OUTPUT_ONLY`+`INPUT_ONLY`,
  explicit-vs-derived conflict): codegen aborts with a message naming message/field/behaviors
  (FR-A2). A regression fixture proves the abort.
- **FM-2 — Missing/unreadable FDS**: `openapiv2to3` exits non-zero; it never emits a spec that
  silently lacks the `x-aip-*` enrichment (FR-B7).
- **FM-3 — FDS/swagger drift**: a resource/field the classifier sees in the FDS but the swagger
  lacks (or vice versa) is a hard error, not a dropped field (FR-B7).
- **FM-4 — Over-derivation**: `not_null` must never surface as `REQUIRED`; a test asserts a
  not-null, server-defaulted field is absent from `required` (FR-A1, AC-1).
- **FM-5 — Classifier drift over time**: because both paths import `internal/aip`, a change to
  classification that would desync compiled behavior from OpenAPI is caught by AC-6's shared-source
  golden. (Guards against someone reintroducing a local classifier in the enrichment tool.)

## Out of scope (WS-024 later / deferred)

- The Go client generator (apx `go-client`, G7d) — fast-follow, a separate apx feature that
  *reads* this enriched OpenAPI.
- The CLI shell + plugins (P1), the Terraform provider (P2), the `de` scaffold verbs.
- A pollable LRO contract (`google.longrunning.Operation`, AIP-151, G7c) — fast-follow only if a
  consumer needs async on day one.
- Any authz change; any DDD write-boundary change.
