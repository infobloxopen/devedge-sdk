# F035 — Health & readiness probes: gRPC health service + HTTP /healthz, /readyz, with a readiness-check seam

**Status**: design locked. **Issue**: #91 (DX 055, P0). **Initiative**: WS-007.
**Depends on**: `server.New`/`Serve`; lands AFTER F034 (#90) — both touch `Serve`'s gateway HTTP handler, so
F035 integrates with F034's `otelhttp`-wrapped mux. **Pre-GA**: clean implementation over back-compat.

**Origin / user directive**: "the gRPC health service is low-dep; the **readiness hooks** are an interface
deps report against." Health is an operational *ility WS-007 closes; k8s/Helm (#95) will point liveness at
`/healthz` (or gRPC health) and readiness at `/readyz`.

## Problem
At v0.23.0 there is no health surface: no `google.golang.org/grpc/health`, no `/healthz` or `/readyz` HTTP
handler (grep-confirmed). A k8s/k3s deployment (the #95 first-class target) has nothing to point liveness and
readiness probes at; a service reports "up" before its DB is reachable.

## Decision (locked)
- **gRPC health (low-dep).** Register `google.golang.org/grpc/health` `health.Server` (implements
  `grpc_health_v1.HealthServer`) on the gRPC server in `server.New`. `grpc/health` is part of the grpc-go
  module already in the dep graph — no new transitive dependency. Overall service status starts `SERVING`
  for liveness; the readiness aggregator drives the per-service / overall status to `NOT_SERVING` when a
  readiness check fails (so gRPC-native probes reflect readiness too).
- **HTTP probes when the gateway is enabled.** When `HTTPAddr != ""`, mount `/healthz` (liveness — 200 as
  long as the process serves) and `/readyz` (runs the readiness aggregator; 200 if all checks pass, 503 +
  a JSON body listing failed checks otherwise) on the HTTP handler alongside the gateway mux. When
  `HTTPAddr == ""`, only gRPC health is exposed (a gRPC-native probe is used). Probes are **unauthenticated**
  and excluded from the authz path (they must be reachable by kubelet).
- **Readiness-check seam (the interface deps report against).**
  ```go
  package health // github.com/infobloxopen/devedge-sdk/health  (or server/health)
  type Check interface {
      Name() string
      Check(ctx context.Context) error // nil = ready
  }
  ```
  An aggregator runs all checks (bounded per-check timeout), reports the combined result to both `/readyz`
  and the gRPC health status. `server.Config` gains `ReadinessChecks []health.Check` (default empty →
  always ready). Liveness is intentionally check-free (process-up only) — readiness is where deps report.
- **Dependency-light built-in check.** A DB-ping readiness check expressed against the **stdlib**
  `interface{ PingContext(context.Context) error }` (satisfied by `*sql.DB`, which both gorm `db.DB()` and
  ent expose) — no ORM dependency enters core. Lives next to the seam as the one provided check.

## Locked defaults
- Liveness = process-up only (no checks); readiness = aggregate of `ReadinessChecks` (empty ⇒ ready).
- Per-check readiness timeout: 2s (bounded so a hung dep can't wedge the probe).
- `/readyz` failure → HTTP 503 + JSON `{"status":"unready","checks":{"<name>":"<err>"}}`.
- gRPC overall status flips to `NOT_SERVING` while any readiness check fails.

## Design — files
- `health/check.go` (new): `Check` interface + aggregator + the `PingContext` DB check.
- `server/server.go`: register `health.Server` on the gRPC server in `New`; add `Config.ReadinessChecks`;
  in `Serve`, when HTTPAddr set, compose `/healthz` + `/readyz` onto the HTTP handler (integrating with
  F034's `otelhttp` wrapper — health routes sit on the same outer `http.ServeMux` the gateway mounts under).
  Wire the readiness aggregator to drive the gRPC health status.
- Scaffold: `main.go.tmpl` + `main.ent.go.tmpl` register a DB-ping readiness check (the scaffold already
  opens a DB); `README.md.tmpl` notes the probe endpoints. (Helm/k8s probe wiring is F035-consumed-by-#95.)
- Docs: `docs/content/docs/guides/health.md`.

## Acceptance criteria
- **AC-1.** `server.New` registers a gRPC health service; `grpc_health_v1.Health/Check` returns SERVING for
  a healthy server.
- **AC-2.** With `HTTPAddr` set, `GET /healthz` → 200 whenever the process serves; `GET /readyz` → 200 when
  all readiness checks pass, 503 + JSON listing failures when any fails.
- **AC-3.** A failing readiness check flips both `/readyz` (503) and the gRPC overall health status
  (NOT_SERVING); recovery flips both back.
- **AC-4.** Probes bypass authz (reachable unauthenticated) and each check is bounded by the per-check timeout.
- **AC-5 (dependency-light).** No new heavy transitive dep: `grpc/health` is already in-graph; the DB check
  uses only `database/sql`/`context` (the `PingContext` interface) — no ORM import in core. Guard: core import
  list gains nothing beyond `grpc/health`.
- **AC-6 (gates).** build/vet/test/security green; scaffold E2E green.

## Failure modes
- **Probe behind authz** → kubelet gets 401/PermissionDenied, pod never ready. Mitigation: register health
  routes/service outside the authz interceptor path; test an unauthenticated probe.
- **Readiness check hangs** → probe wedges. Mitigation: per-check `context.WithTimeout`.
- **Liveness coupled to deps** → DB blip kills an otherwise-live pod (restart storm). Mitigation: liveness is
  process-only; deps go in readiness, never liveness.

## Tasks
- **T1 [S]** `health/check.go` — `Check` interface, aggregator (bounded timeout), `PingContext` DB check.
- **T2 [S]** `server/server.go` — register gRPC `health.Server`; `Config.ReadinessChecks`; `/healthz`+`/readyz`
  on the HTTP handler; wire aggregator → gRPC health status; keep probes off the authz path.
- **T3 [S]** tests — gRPC health Check; `/healthz` 200; `/readyz` 200↔503 on a toggled check; gRPC status
  flips with readiness; unauthenticated-probe test.
- **T4 [S]** scaffold — register a DB-ping readiness check in `main.go.tmpl`/`main.ent.go.tmpl`; README note.
- **T5 [S]** docs — `guides/health.md`.

## Exit
All ACs green; PR merged; tag cut. DX cadence re-run shows health/readiness **present**. Feeds #95 (probes
wired by the Helm chart / compose healthcheck).
