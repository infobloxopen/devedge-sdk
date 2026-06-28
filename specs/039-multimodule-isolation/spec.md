# F039 — Multi-module adapter isolation: split backend adapters into nested Go modules (graph-level dependency isolation)

**Status**: design locked (pending owner review of this spec before the restructure begins).
**Initiative**: WS-011. **Decisions (owner, 2026-06-27):** scope = the 3 adapters **+ the gorm/ent persistence
adapters**; versioning = **synchronized** (all modules one version per release). **Pre-GA**: clean
implementation over back-compat; the **devedge CLI** stays back-compatible to the extent `platform.data.kit`
needs (and that consumer's contract is untouched here).

## Problem
devedge-sdk is a SINGLE Go module. Backend adapters (`observability/otel`→OTel SDK+exporters,
`config/koanf`→koanf, `events/kafkabus`→franz-go, `persistence/gormtx`→gorm, `persistence/entrepo`→ent) are
sub-packages, so their heavy deps are **direct requires in the root `go.mod`** and therefore land in **every
consumer's module graph + `go.sum`** even when unused. `cleancore_test.go` guards the **build/binary** (a
`server`-only consumer compiles zero OTel-SDK packages — verified) but **not the dependency graph**. WS-011
makes a consumer pull an adapter's deps **only when they `require` that module**.

## Decision (locked)
**Six nested modules in the same repo, at their existing import paths** (Go module path == import path, so
**no import statement anywhere changes** — only `go.mod`/`go.work`/tags/CI):

| Module (path) | Heavy deps it owns | Imports from core (→ requires root) |
|---|---|---|
| `github.com/infobloxopen/devedge-sdk` (**root**) | none of the below | — |
| `…/observability/otel` | otel **SDK** + exporters | authz, server (tests) |
| `…/config/koanf` | koanf | config |
| `…/events/kafkabus` | franz-go | events |
| `…/persistence/gormtx` | gorm | persistence, events |
| `…/persistence/entrepo` | ent | persistence, persistence/filter, middleware(+etag), secret |

- **Root sheds** `gorm.io/gorm`, `entgo.io/ent`, `github.com/knadh/koanf/*`, `github.com/twmb/franz-go`,
  `go.opentelemetry.io/otel/sdk` + `…/exporters/*`. **Root keeps** the OTel **API** + contrib
  (`otelgrpc`, `otelhttp`, `otel`, `otel/trace`, `otel/metric`) — those are API-level, already permitted.
- **Core stays as-is** (ORM-free, verified): `persistence` interfaces + `memory*` + `filter/` + `resourcename/`,
  plus server/middleware/authz/lro/secret/config(stdlib)/resilience/events(+membus)/health, and the codegen
  plugins (`cmd/protoc-gen-*`) — which **string-emit** imports and pull no ORM themselves.
- **Module dependency direction:** every adapter module **requires the root module** (adapter → core). No
  cycles (core never imports an adapter — now structurally impossible across a module boundary).

### The one real code change — decouple the CLI from ent
`cmd/devedge-sdk` (root module) currently **imports `entgo.io/ent/cmd/ent`** and calls `entc.Generate(...)`
during scaffolding — this hard-wires `entgo.io/ent` into the root module's graph. Change the CLI to **invoke
ent codegen as a subprocess** (`go run entgo.io/ent/cmd/ent …`, or emit a `//go:generate` + run `go generate`
in the generated module) so the root module no longer imports ent. The ent dep then exists only in the
generated service's own module (which already requires ent) — exactly where it belongs.

### Tooling, dev loop, releases (synchronized)
- **`go.work`** at repo root listing all 6 modules → local dev/build/test resolves cross-module references
  with no `replace` directives or version pins. (Committed; CI uses it for the local matrix, but published
  module `go.mod`s carry real `require`s.)
- **Synchronized tags:** each release tags the root **and every submodule at the same version** —
  `v0.27.0`, `observability/otel/v0.27.0`, `config/koanf/v0.27.0`, `events/kafkabus/v0.27.0`,
  `persistence/gormtx/v0.27.0`, `persistence/entrepo/v0.27.0`. A **release script** encodes the ordering:
  (1) tag root `vX.Y.Z`; (2) in each adapter `go.mod`, set `require github.com/infobloxopen/devedge-sdk
  vX.Y.Z` (+ `go mod tidy`); commit; (3) tag each adapter `path/vX.Y.Z`. One command.
- **Scaffold/codegen go.mod templates:** `go.mod.tmpl` (gorm) adds `require …/persistence/gormtx vX.Y.Z`;
  `go.mod.ent.tmpl` adds `require …/persistence/entrepo vX.Y.Z`; both keep the direct gorm/ent require the
  generated service already has. A generated service that wants observability adds `…/observability/otel`.
  Version vars (`GormtxVersion`/`EntrepoVersion`/`OTelAdapterVersion`) derive from the SDK version, asserted
  like the existing `SDK_VERSION` (memory: cadence pins+asserts the CLI/module version).
- **testdata fixtures** (`testdata/{toy,apikey,fleet,iam}`, separate test modules with `replace` to the SDK):
  add `require` + `replace` for the adapter modules they use (gormtx/entrepo).
- **CI** (`.github/workflows/ci.yml` + Makefile): Build & Test runs the gates across **all 6 modules**
  (a module matrix / loop: `go build|vet|test ./...` per module), using `go.work` locally; the Postgres/
  MySQL/Kafka integration gates move to the modules that own them (gormtx/entrepo/kafkabus). Security Check
  (cleancore) is retained and **reinforced** — the module boundary now makes a core→backend import a compile
  error, not just a test failure.

## Acceptance criteria
- **AC-1 (the graph-level proof — the whole point).** A throwaway consumer module that `require`s
  devedge-sdk `vX.Y.Z` and imports **only `…/server`**, after `go mod tidy`, has a `go.sum` containing
  **none** of: `gorm.io/gorm`, `entgo.io/ent`, `github.com/knadh/koanf`, `github.com/twmb/franz-go`,
  `go.opentelemetry.io/otel/sdk`, `…/otel/exporters/*`. (A test/script asserts this.)
- **AC-2 (opt-in pulls deps).** The same consumer, after adding `import ".../observability/otel"` +
  `require`, DOES get the OTel SDK in its graph — proving the dep arrives only on opt-in.
- **AC-3 (root go.mod is light).** Root `go.mod` no longer direct-requires the 5 heavy dep families; it
  retains the OTel API/contrib. Each adapter `go.mod` requires the root + its own backend.
- **AC-4 (no import churn).** Zero `.go` import statements change (paths are stable); only `go.mod`/`go.work`/
  CI/templates and the CLI ent-subprocess change.
- **AC-5 (CLI/scaffold still work).** `devedge-sdk new service` (gorm AND ent) scaffolds → builds → smoke-tests
  green; the generated `go.mod` requires the right adapter module(s); the CLI runs ent codegen without
  importing ent. The `platform.data.kit`-relevant CLI surface is unchanged.
- **AC-6 (all modules green).** build/vet/test pass in every module; integration gates (PG/MySQL/Kafka) run in
  their owning modules; cleancore/security green; Scaffold E2E green.

## Failure modes
- **CLI ent-subprocess regression** — ent codegen invoked wrong → scaffold breaks. Mitigation: the Scaffold
  E2E (real `new service` ent path) is the gate; verify `entc` runs via subprocess on a clean checkout.
- **Release-ordering mistake** — an adapter tagged requiring an unpublished root version → consumers can't
  resolve. Mitigation: the release script enforces tag-root-first; a post-release smoke that `go get`s each
  module at the new version.
- **go.work masking a missing require** — local builds pass via workspace but a published module lacks a
  `require`. Mitigation: a CI step that builds each module **with `GOWORK=off`** (real requires only).
- **testdata/scaffold go.mod drift** — fixtures don't require the adapter modules → fail to build. Mitigation:
  AC-5/AC-6 cover generated + fixture builds.

## Phased delivery (incremental; one synchronized release at the end)
- **Phase 0 — pilot + tooling.** Split **`observability/otel`** into a nested module (cleanest: only test-time
  core imports). Add `go.work`, the multi-module Makefile/CI matrix, the `GOWORK=off` per-module build, and
  the AC-1 graph-isolation assertion script. Proves the entire mechanism end-to-end. (1 PR + hardening.)
- **Phase 1 — the low-risk adapters.** Split **`config/koanf`** + **`events/kafkabus`** (same pattern).
- **Phase 2 — persistence (the hard part).** Decouple the CLI from ent (subprocess); split
  **`persistence/gormtx`** + **`persistence/entrepo`**; update scaffold `go.mod.tmpl`/`go.mod.ent.tmpl` +
  testdata fixtures + the codegen-emitted requires.
- **Phase 3 — release + validate.** Root `go.mod` shed-and-tidy; the synchronized **v0.27.0** release across
  all 6 modules via the release script; hardening loop; re-run the consumer-graph isolation check + a
  scaffold dogfood. Document "how to add a new isolated module" (the future **k8s-controller-scaffolding**
  pattern).

## Tasks
- **T1 [C]** Phase 0 — `observability/otel` nested module + `go.work` + multi-module Makefile/CI matrix +
  `GOWORK=off` per-module build + AC-1/AC-2 graph-isolation assertion script.
- **T2 [S]** Phase 1 — `config/koanf` + `events/kafkabus` modules.
- **T3 [C]** Phase 2a — decouple CLI from `entgo.io/ent/cmd/ent` (run ent codegen as a subprocess); Scaffold
  E2E proves the ent path.
- **T4 [C]** Phase 2b — `persistence/gormtx` + `persistence/entrepo` modules; update scaffold go.mod templates
  + testdata fixtures + codegen-emitted requires.
- **T5 [C]** Phase 3 — root shed/tidy; the synchronized release script (tag-root → bump-adapter-requires →
  tag-adapters); cut v0.27.0; consumer-graph isolation re-check; "add a new isolated module" doc.

## Exit
Root `go.mod`/`go.sum` no longer carries gorm/ent/koanf/franz-go/otel-SDK; a `server`-only consumer's graph is
provably free of them (AC-1); each backend arrives only on `require`; all 6 modules release synchronized at
v0.27.0; the multi-module pattern is documented for future heavy components (k8s controller scaffolding).
