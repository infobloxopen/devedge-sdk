# 030 — Composable Services: importable Module + executable Host (`servicekit`)

**Status**: P1 + P2 + P3 implemented. P4–P6 planned.
**Initiative**: WS-012 (cross-repo proposal: development-hub `specs/composable-services-proposal.md`).
**Decisions (ratified P0):** package `servicekit` in the ROOT module; generated Module via
`protoc-gen-svc`; static composition only (no Go plugins); one shared `server.Server`;
events-only cross-module coupling; no cross-module FKs; schema-preferred DB default (P2).

This is the **SDK-side umbrella** for the whole feature; later phases append to this spec.

## Problem

The SDK already runs many services in one process by hand (`server.New` once →
`Register<Svc>WithRepository(s, repo)` per service → `s.Serve(ctx)`; the boot-time **union
completeness gate** at `Serve` validates the combined surface). What is missing is an
**importable, self-describing unit** so a service can be composed without hand-merging
`main.go`s — and the boundary discipline that keeps services separable when co-resident.

## Decision — the module / host split

Split "a service" into two artifacts:

- a **Module** (importable library) that owns **domain behavior**, and
- one or more **Hosts** (executables) that own **process behavior**.

The **same module** runs **standalone** (one module per host) or **composed** into a "suite"
binary (N modules, one host) by changing the *host*, not the *module*.

### "Module owns domain, host owns process" — the rule list

A module **MAY** own (domain): resources, handlers, repositories, migration **files**, event
publishes/subscribes, config **schema**, health checks, background jobs, authz rules, routes,
and its self-describing `Descriptor`.

A module **MUST NOT** own (process — the host's job): process lifetime, listen ports, global
flags / global env / global logging config, **DB connection/creation** outside its namespace,
listener startup, signal handling, `os.Exit` / `log.Fatal`, running its own migrations from
`init()`, or any cross-module table ownership / cross-module foreign key.

## The contract (P1 — `servicekit`, root module)

```go
type Module interface {
    Descriptor() Descriptor                       // static, introspectable before boot
    Register(ctx context.Context, app *App) error // wire into the shared host
}

type Descriptor struct {
    ID, DisplayName, Version string
    Methods        []string            // gRPC FullMethods (from the generator)
    AuthzRules     []authz.MethodRule  // the generated <Svc>AuthzRules
    Routes         []RouteDescriptor   // HTTP prefixes / hostnames
    Resources      []ResourceDescriptor
    Config         ConfigDescriptor    // typed schema + prefix (P3)
    Database       DatabaseDescriptor  // isolation policy + migrations FS (P2)
    Events         EventDescriptor     // publishes/subscribes + outbox (P3)
    HealthChecks   []HealthDescriptor
    BackgroundJobs []JobDescriptor
    Requires       Compatibility       // sdk/go/postgres ranges (P4/P5 gating)
}

type App struct {                       // the running host's shared services
    Server  *server.Server              // the ONE shared server (server.New)
    Config  ConfigProvider              // per-module config (P3 prefix scoping)
    DB      DatabaseRegistry            // namespaced DB handle (P2)
    Events  EventRegistry               // host-owned relay/consumer per module (P3)
    Health  HealthRegistry              // registers checks on the shared server (P1: live)
    Logger  *slog.Logger
    Metrics MetricsRegistry             // per-module metrics (P3)
}

func Run(HostConfig) error              // build server ONCE, register each module, Serve
type HostConfig struct {
    Modules []Module
    GRPCAddr, HTTPAddr string
    Authorizer authz.Authorizer
    PrincipalFunc grpcauthz.PrincipalFunc
    Logger *slog.Logger
    ConfigSources []config.Source
    Context context.Context             // nil => host installs SIGTERM/Interrupt handler
}
```

`Run`'s P1 lifecycle: resolve host defaults + root context (host owns signals) →
`ValidateModules` (unique IDs; no duplicate gRPC service names / route prefixes / permission
names) → `server.New(cfg)` **once** → for each module `module.Register(ctx, app)` → `server.Serve(ctx)`.
The fail-closed **union completeness gate is reused** from `server.Serve` — `Run` does not
invent a parallel gate. DB namespacing (P2), host-owned relays / config layering / bulkheads /
failurePolicy (P3) are explicit extension points marked `P3:` in `run.go`.

### Generated Module (protoc-gen-svc)

For a CRUD service, the generator emits, alongside the existing `Register<Svc>` /
`Register<Svc>WithRepository` (unchanged), a `<Svc>Module(<Svc>ModuleOptions) servicekit.Module`
whose `Descriptor()` is the proto facts (module ID = proto-package first segment; methods; the
generated `<Svc>AuthzRules`; module-qualified resource name) and whose `Register` wraps
`Register<Svc>WithRepository(app.Server, opts.Repo)`. Hand-written extras flow through
`<Svc>ModuleOptions` (P1: the repo; later: health/events/jobs/handler overrides).

### Scaffold (`devedge-sdk new service`) — composable shape

The scaffold emits an importable `module/` package (`module/module.go` exposing `Module(repo,
db)` + the embedded `module/migrations/` placeholder) and a thin `cmd/<svc>/main.go` host that
does the process wiring (flags/env/config, DB open + migrate, otel, signals) then calls
`servicekit.Run` with the one module. The module registers the DB readiness check via the
Health registry, so `/readyz` behavior is unchanged.

## Acceptance criteria

- **AC-1 (contract):** `servicekit` defines `Module`, `Descriptor` (+ the descriptor sub-types),
  `App`, the five registry interfaces, and `Run(HostConfig)`; it imports only root SDK packages
  + `log/slog` and builds with `GOWORK=off`. ✅
- **AC-2 (validation):** `ValidateModules` rejects duplicate module IDs, duplicate gRPC service
  names, duplicate route prefixes, and duplicate permission names; it does **not** duplicate the
  rule-completeness gate. ✅
- **AC-3 (run, 1 and N):** `Run` serves a single module and trivially N modules; an undeclared
  method fails closed via the **existing** `server.Serve` gate. ✅
- **AC-4 (generated Module):** the generator emits `<Svc>Module`/`<Svc>ModuleOptions` for a CRUD
  service, with the descriptor populated from proto facts and `Register` wrapping
  `Register<Svc>WithRepository`; it does **not** replace the existing registrars. ✅
- **AC-5 (fixture exercises the shape):** at least one testdata fixture (toy) constructs the
  generated Module and serves it end-to-end through `servicekit.Run`. ✅
- **AC-6 (no behavior change, standalone):** a regenerated scaffold builds and serves
  identically — `make generate && make build && make test` green for gorm/ent and the aggregate
  variants; the fail-closed authz gate still triggers at boot through `runHost` → `servicekit.Run`. ✅
- **AC-7 (scope):** every change traces to P1 scope; P2/P3 stubs (DatabaseDescriptor/Namespace,
  inert DB/Event/Metrics registries, config layering) are inert and marked. ✅

## Migration note (existing standalone services)

P1 changes **no runtime behavior**; it formalizes the unit. To adopt the composable shape in an
existing service generated by an earlier scaffold:

1. `make generate` to pick up the generator's new `<Svc>Module()` (additive — the existing
   `Register<Svc>`/`Register<Svc>WithRepository` are unchanged, so a service that keeps calling
   them directly needs no change at all).
2. Optionally move `server/main.go` → `cmd/<svc>/main.go` and add a `module/module.go` that
   returns `Module(repo, db)`; replace the in-line `server.New(...) + Serve(...)` with
   `servicekit.Run(servicekit.HostConfig{Modules: []servicekit.Module{module.Module(repo, db)}, …})`.
   Behavior is identical (same server config, same DB readiness check, same boot gate).
3. Update `Makefile` `run:` and the smoke test to the `cmd/<svc>` path if you moved `main`.

A service that does nothing keeps working — the change is opt-in until P3/P4 composition is wanted.

## Phase checklist

- [x] **P1 — Module contract + generated `Module()` + composable scaffold.** `servicekit`
  package; `protoc-gen-svc` emits `Module()`/`Descriptor()`; scaffold emits `module/` + thin
  `cmd/<svc>/main.go`. Standalone binaries keep working. (this commit)
- [x] **P2 — DB module-namespacing.** `DatabaseNamespace` allocation; namespaced framework +
  domain tables (outbox / idempotency / migrations); host-run, advisory-locked per-module
  migrations; no-cross-module-FK rule. Postgres schema isolation + prefix fallback tests.
  (this commit — see the P2 section above)
- [x] **P3 — Composition host.** Enrich `servicekit.Run`: shared backends resolved once,
  host-owned relay/consumer per module outbox, config prefix layering, in-process bulkheads +
  `failurePolicy`. Full ConfigProvider / DatabaseRegistry / EventRegistry / MetricsRegistry impls.
  (this commit — see the P3 section below)
- [ ] **P4 — `kind: Composition` + `de compose`** (devedge side): resource kind +
  `de compose init/add/tidy/build/test/up`; generate `cmd/<comp>/main.go` + `composition.lock`.
- [ ] **P5 — Composition test harness.** `servicekittest.AssertModule` + `AssertComposition`;
  version-compatibility gating.
- [ ] **P6 — Deployment rendering.** `de compose chart` → single-binary / multi-daemon / hybrid
  from one descriptor set (reuse WS-007 Helm/Flux/Compose seam).

## Files (P1)

- `servicekit/{servicekit,registry,validate,run,config}.go` + `servicekit/servicekit_test.go`
- `server/server.go` — `AddReadinessCheck` (live append, mirrors `AddRules`)
- `cmd/protoc-gen-svc/{render,main}.go` + `render_test.go` — `renderModule` + proto-package threading
- `cmd/devedge-sdk/internal/scaffold/templates/{module.go,migrations.README.md,main.go,main.ent.go,smoke_test.go,Makefile,README.md}.tmpl`
  + `render.go` (new outputs) + the integration/deploy test path updates
- `testdata/{toy,apikey,iam}/…svc.go` (regenerated) + `testdata/toy/module_test.go`

---

# P2 — DB module-namespacing (the load-bearing new work)

**Status:** P2 implemented (this commit). The DB axis is real and wired; events
relay-per-module, config prefix layering, and bulkheads remain P3 (inert + marked).

## Problem (P2)

`AccountID` is **tenant** scoping only. Two co-resident modules sharing one database
would collide on the **framework** tables (`outbox`, `idempotency_markers`, the
dispatcher `outbox_dispatch_cursor`/`outbox_dead_letter` sidecars, `schema_migrations`)
and on any same-named **domain** table. Module isolation is a SECOND axis beneath
tenant isolation — both coexist (`orders.*` vs `billing.*`; `account_id` A vs B).

## Decision — namespace identity + policy in `persistence` (single source of truth)

`persistence.DatabaseNamespace{ModuleID, Engine, Schema, TablePrefix, MigrationTable,
Role}` is the resolved isolation identity, defined in the **root `persistence`**
package (not servicekit) because the gormtx/entrepo adapters that HONOR it import
`persistence`, never `servicekit`. `servicekit.DatabaseNamespace` and
`servicekit.IsolationPolicy` are **type aliases** of the persistence types.

`persistence.ResolveNamespace(policy, moduleID, engine, schema, prefix)` is the host
allocation rule (the §5.4 table):

| Policy | Postgres | prefix-only engines (sqlite) |
|---|---|---|
| `schema-required` | schema per module | **error** (fail fast) |
| `schema-preferred` *(default)* | schema | table prefix |
| `prefix-required` | table prefix | table prefix |
| `dedicated-required` | separate DB/DSN (no in-DB qualification) | same |

`DatabaseNamespace.QualifyTable(base)` is the one qualification rule the framework
stores apply: `schema.base` / `prefix+base` / bare. Domain tables are qualified by the
ORM (Postgres `search_path` for schema, naming-strategy `TablePrefix` for prefix) —
the SDK's generated domain models do NOT pin `TableName`, so the strategy applies; the
framework rows DO pin `TableName`, so the stores override it explicitly.

## How table qualification works per adapter

- **gormtx:** the `GormOutboxStore` / `GormIdempotencyStore` / `GormOutboxCursorStore`
  constructors take a namespace via `With*Namespace(ns)` options and qualify their
  (otherwise hard-coded) table names through `db.Table(ns.QualifyTable(base))`. The
  partition DDL gained a namespace-aware twin (`EnsureOutboxPartitionsNS`,
  `RunRetentionNS`, namespace-scoped `dropPGPartitionsBefore`). The zero namespace
  reproduces the bare names exactly (single-module behavior unchanged).
- **entrepo:** the ent CLIENT owns the schema (not the adapter), so namespacing is
  applied one level up — at the connection — via `entrepo.NamespacedDSN(dsn, ns)`
  (delegating to `persistence.NamespacedPostgresDSN`), which appends the module schema
  to the Postgres `search_path`. `ent.Schema.Create` + queries then resolve into the
  module schema. Prefix isolation on ent is not implemented (use `dedicated-required`).

## Host-run migration (host-owned, advisory-locked, per-module table)

`gormtx.MigrateModule(ctx, db, MigrateOptions{Namespace, DomainModels, FrameworkModels})`:

- acquires a **Postgres advisory lock keyed by the module ID** (a domain distinct from
  the relay leader lock), so two hosts booting the same composition serialize per
  module; skipped on engines without one (SQLite single-process);
- creates the module **schema** (`CREATE SCHEMA IF NOT EXISTS`) under schema isolation;
- **AutoMigrates** the framework + domain models INTO the module namespace (prefix
  isolation overrides each framework table name; a dialect-portable `tableExists`
  probe keeps it idempotent);
- stamps the module's **own** `schema_migrations` table (`ns.MigrationTable`) — never a
  shared one.

`servicekit.HostConfig` gained `Database *DatabaseConfig{Engine, DefaultIsolation}` and
`Migrate MigrationRunner`. `servicekit.Run`, per module, **allocates the namespace**
(real `hostDatabaseRegistry` replacing the inert one) → **runs the migration** (the
host-supplied runner, before Register, never module-init) → **registers** the module,
which reads its namespace from `app.DB.Namespace(...)` and binds its stores to it. A
module never migrates from `init()`, never assumes it owns the whole DB.

**No cross-module FKs** (the recomposability rule): documented in `migrate.go` + this
spec; the framework tables reference aggregates by ID only (no FK), and the convention
is exercised by the two-module tests (each module's tables live in a separate
schema/prefix, so a cross-module FK is structurally impossible). A static lint is P4
(`de compose`)/P5 (`AssertModule`).

## Scaffold (standalone host = host-run migration, behavior unchanged)

The GORM scaffold replaced the inline `db.AutoMigrate(...)` with a host-owned
`moduleMigrate` `MigrationRunner` (calling `gormtx.MigrateModule`) passed to
`servicekit.Run`. A standalone host shares its DB with no one → **zero-qualification
namespace → bare tables → byte-for-byte unchanged**. The generated `module/`
descriptor now self-describes its `Database{Migrations: Migrations()}` (Isolation
unset → composition default). The ent scaffold's `Schema.Create` is already host-run;
P2 documents the `entrepo.NamespacedDSN` namespacing path.

## Acceptance criteria (P2)

- **AC-P2-1 (namespace + policy):** `persistence.DatabaseNamespace` + `IsolationPolicy`
  + `ResolveNamespace` (the §5.4 table) + `QualifyTable`; servicekit aliases them. ✅
- **AC-P2-2 (framework tables namespaced):** gormtx outbox/idempotency/cursor/
  dead-letter constructors qualify ALL table names per namespace; zero namespace =
  bare (unchanged). ✅
- **AC-P2-3 (host-run migration):** advisory-locked, per-module schema/prefix, per-
  module migration table; wired into `servicekit.Run` via `MigrationRunner`. ✅
- **AC-P2-4 (real-DB proof):** a **real-Postgres** test boots **two modules in one
  host on one DB** and proves `outbox`/`idempotency_markers`/`schema_migrations` + a
  same-named domain table live in **distinct schemas** (no collision), plus behavioral
  isolation (orders' outbox append invisible to billing) — `testdata/iam/iamv1/
  ws012_namespace_pg_test.go` (Docker-gated, clean skip without Docker). ✅
- **AC-P2-5 (prefix fallback):** an always-available SQLite test proves the same
  guarantee under prefix isolation — `persistence/gormtx/namespace_test.go`. ✅
- **AC-P2-6 (standalone unchanged):** the scaffold integration tests
  (`TestScaffold_GORM*`/`ENT*`) build + serve identically; single-module = bare tables. ✅
- **AC-P2-7 (scope):** P3 (events relay-per-module, config prefix layering, bulkheads,
  failurePolicy) remain inert + marked. ✅

## Files (P2)

- `persistence/namespace.go` + `persistence/namespace_test.go` — `DatabaseNamespace`,
  `IsolationPolicy`, `ResolveNamespace`, `QualifyTable`, `NamespacedPostgresDSN`.
- `persistence/gormtx/{outbox,idempotency,outbox_partition}.go` — `With*Namespace`
  options + qualified table names + namespace-aware partition DDL/retention.
- `persistence/gormtx/migrate.go` + `migrate_test.go` — `MigrateModule`,
  `MigrationModelsFor`, advisory lock, schema create, idempotent migration, stamp.
- `persistence/gormtx/namespace_test.go` — SQLite prefix-isolation two-module proof.
- `persistence/entrepo/namespace.go` — `NamespacedDSN` + ent namespacing doc.
- `servicekit/{servicekit,registry,config,run}.go` — alias the persistence types;
  real `hostDatabaseRegistry`; `DatabaseConfig` + `MigrationRunner`; wired into `Run`.
- `servicekit/servicekit_test.go` — namespace allocation + host-run migration wiring.
- `cmd/devedge-sdk/internal/scaffold/templates/{main.go,migrations.README.md,module.go,
  main.ent.go}.tmpl` — host-run migration runner; self-describing Database descriptor.
- `testdata/iam/iamv1/ws012_namespace_pg_test.go` — real-Postgres two-module proof.

---

# P3 — Composition host (the full `servicekit.Run` lifecycle)

**Status:** P3 implemented (this commit). The host now resolves shared backends once,
OWNS one relay + one consumer per module outbox, layers config per module prefix, and
contains per-module failures with `failurePolicy`. P4 (`de compose`), P5 (test harness),
and P6 (deploy rendering) remain.

## Problem (P3)

P1 formalized the unit; P2 made the DB axis real. What was still inert (marked `P3:`):
the **event-dispatcher composition** (a composed binary that naively started a relay +
consumer per service's `main()` would **double-start** dispatchers), **config layering**
(one struct per service, no per-module prefix), and **in-process bulkheads** (one process
loses OS isolation — a panic/failure in one module would take the host down).

## Decision — the host owns process behavior across modules

`servicekit.Run` now executes the full §5.3 lifecycle. New host-owned machinery (the
ONLY genuinely new code — relay/consumer/bus/breaker/health/config primitives are reused):

### Event composition (load-bearing — "same binary ≠ direct calls")

- The host owns ONE shared `events.Bus` (`HostConfig.Bus`; default in-process `membus`
  for the same-binary/one-DB case). It starts **exactly one `events.Relay` + one
  `events.Consumer` per module outbox** over that bus — no double-start.
- A module hands the host its **namespaced** (P2) stores from `Register`:
  `app.RegisterOutboxRelay(OutboxRelayConfig{Store, Cursors, Leader, …})` (publisher
  side, one relay) and `app.Subscribe(ConsumerConfig{Tx, Idem}, EventHandler…)`
  (subscriber side, one consumer in the module's own consumer **group** so every module
  sees every event it subscribes to). The host never reaches into a module's DB — it
  only drives the relay/consumer loops over the module's supplied stores.
- Cross-module communication stays through the **durable outbox → relay → bus →
  consumer** pipeline even in one process (in-process bus). A module never imports
  another module's handler. This is what preserves recomposability.
- **Boot-time event-graph validation** (`validateEventGraph`, wired into
  `ValidateModules`): event type names must be **globally unique** across publishers
  (a duplicate publisher is a boot error), and every **subscribed** type must have a
  publisher in the composition (an **orphan subscriber** is a boot error). A published
  type with no in-composition subscriber is allowed (another daemon may consume it).
  `Descriptor.Events.{Publishes,Subscribes,Outbox}` is the declared graph.

### Config layering

- The host loads ALL config from one source set, then hands each module a
  `ConfigProvider` **scoped to its prefix** (`ConfigDescriptor.Prefix`, default = module
  ID): module `orders` reading `config:"NAME"` resolves `ORDERS_NAME`; `billing` reads
  `BILLING_NAME` — isolated slices from one source set. A `prefixSource` decorator wraps
  the existing `config.Source`s; the loader (`config.Load`) is reused unchanged.
- The reserved **platform-global** namespaces (`runtime/database/observability/authz/
  events`) are host-owned; a module that claims one as its prefix is **rejected at boot**.
- Enforced: a module reads config only via `app.Config.Load` (never raw global env,
  flags, `os.Exit`/`log.Fatal`, or a listener) — the host owns all of that.

### Bulkheads + failure policy

- A `supervisor` runs every module's background work (relay, consumer, jobs) in its own
  goroutine **keyed by module ID** with a **panic boundary** (`recoverModule` +
  `guardCall` around `Register`): a panic is recovered, attributed to its module, and
  routed through the module's `FailurePolicy`. It never unwinds past the module boundary.
- **`failurePolicy`** (`Descriptor.FailurePolicy`, overridable per composition via
  `HostConfig.FailurePolicies`; host default `fail-host`): `fail-host` (CORE — a failure
  cancels the host context with the attributed cause, fail-fast) vs `degraded` (OPTIONAL
  — the failure ISOLATES to that module, which marks **itself** unready via a per-module
  `moduleReadiness` check on the shared server's readiness aggregator, and the host +
  other modules stay up).
- **Background-job supervision:** `app.RegisterBackgroundJob(name, fn)` — the host runs
  `fn` in the module's bulkhead; a panic/error is contained + routed through the policy.
- Per-module health is attributed via `health.Aggregate` (reused) over the per-module
  readiness gates. The **circuit-breaker / rate-limit** seams (`server.Config.Resilience`)
  and per-module **DB-pool limits** are CONFIGURED, not rebuilt (no new breaker).

### MetricsRegistry

- `hostMetricsRegistry.Namespace(moduleID)` returns a metric-safe per-module token — a
  thin per-module label over the SDK's existing OTel RED metrics. No new metrics backend.

## The lifecycle as it now stands (`servicekit.Run`, §5.3)

1. resolve host defaults + a **cancel-cause** root context (host owns signals + fail-fast); ✅ real
2. `ValidateModules` — IDs / service names / route prefixes / permissions **+ the event graph**; ✅ real
3. resolve shared backends ONCE: the one `server.New`, the one `events.Bus`, the host
   config store, the per-module `DatabaseRegistry`, the `supervisor`; ✅ real
4. per module: allocate `DatabaseNamespace` (P2) → host-run advisory-locked migration (P2)
   → register the module's readiness gate → `Register` inside its **panic boundary**, with a
   **per-module `App`** (config scoped to its prefix, event/job helpers keyed to its ID); ✅ real
5. start **one relay + one consumer per module outbox** + every supervised background job,
   each in its module's bulkhead; ✅ real
6. `server.Serve` — the EXISTING fail-closed union completeness gate validates the union; ✅ reused
7. on shutdown: close the shared bus + wait for supervised goroutines (clean stop). ✅ real

## Acceptance criteria (P3)

- **AC-P3-1 (composed event flow, real DB):** two modules in one host on one Postgres DB —
  module A publishes via its namespaced outbox, module B's subscriber receives it through the
  HOST-OWNED in-process dispatcher (relay→bus→consumer), with exactly one relay + one consumer
  asserted (no double-start). `testdata/iam/iamv1/ws012_events_pg_test.go` (Docker-gated;
  **RAN green** against postgres:16). ✅
- **AC-P3-2 (panic containment):** a `degraded` module's job panic is recovered, attributed to
  it, marks it unready, and does NOT crash a co-resident module or the host; a `fail-host`
  module's panic fails the host fast with the attributed cause. ✅ (`servicekit/p3_test.go`)
- **AC-P3-3 (config layering):** two modules get isolated, prefix-scoped config from one
  source set; a reserved platform-global prefix is rejected at boot. ✅
- **AC-P3-4 (boot validation):** a duplicate event type / orphan subscriber is rejected at
  boot (event-graph gate). ✅
- **AC-P3-5 (standalone unchanged):** a single-module host serves identically — empty config
  prefix = verbatim, no shared bus needed for a module that neither publishes nor subscribes,
  zero-qualification DB namespace; scaffold integration + fixtures green. ✅
- **AC-P3-6 (reuse / scope):** relay/consumer/bus/breaker/health/config primitives are reused;
  the only new code is host orchestration (per-module relay lifecycle, config prefix scoping,
  panic boundary, failurePolicy, event-graph validation). No new runtime/backend. ✅

## Files (P3)

- `servicekit/run.go` — full §5.3 lifecycle: cancel-cause context, shared-backend
  resolution, per-module `App` + panic-guarded `Register`, dispatcher + job start, clean
  shutdown; `HostConfig.{Bus,FailurePolicies,DefaultFailurePolicy}`; `hostState`/`registerJob`.
- `servicekit/events_registry.go` (new) — `hostEventRegistry`: one shared bus, one relay +
  one consumer per module outbox; `OutboxRelayConfig`/`ConsumerConfig`/`EventHandler`.
- `servicekit/config.go` — `hostConfigStore` + `scopedConfigProvider` + `prefixSource`
  (per-module prefix layering, reserved global namespaces); `hostMetricsRegistry`.
- `servicekit/bulkhead.go` (new) — `FailurePolicy`, `supervisor` (panic boundary + per-module
  readiness + failure routing), `guardCall`, `moduleReadiness`.
- `servicekit/servicekit.go` — per-module `App` helpers (`RegisterOutboxRelay`, `Subscribe`,
  `RegisterBackgroundJob`, `Bus`, `ModuleID`); `Descriptor.FailurePolicy`.
- `servicekit/validate.go` — `validateEventGraph` (globally-unique types + coherent graph).
- `servicekit/p3_test.go` (new) — composed in-process event flow, panic containment
  (degraded + fail-host), config layering, boot validation (all Docker-free).
- `testdata/iam/iamv1/ws012_events_pg_test.go` (new) — real-Postgres composed-host event-flow
  proof (Docker-gated; clean skip without Docker).
