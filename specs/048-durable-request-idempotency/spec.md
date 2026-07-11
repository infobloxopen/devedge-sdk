# F048 — Durable, tenant-scoped request idempotency (exactly-once retries)

**Status:** in progress · **Workstream:** WS-043 · **Upgrades:** spec `023-request-dedup-validate-only`
· **Composes with:** `047-row-level-security` (WS-029), WS-014 Wave 1 (outbox/tx seams).

## Summary

devedge-sdk already implements the AIP-155 shape (`request_id` field + `middleware.DeduplicateUnary`
caching the response), but the guarantees are **best-effort**: the default store is in-memory,
per-pod, TTL-only, and `Store()` runs *after* the handler returns — not atomic with the domain
commit. A crash between commit and cache, a retry landing on another pod, or a restart re-executes
the handler (e.g. a second `Create` mints a *new* server id). Separately, the existing interceptor
keys the cache by the **bare `request_id`** with **no tenant or method scope**, so on one pod a
`request_id` reused across tenants (or across methods) collides — a cross-tenant confidentiality
leak and a cross-method aliasing bug.

This feature adds a **durable, tenant-scoped, exactly-once** idempotency path — a DB-backed store
that claims and completes the idempotency record **inside the handler's transaction**, so a
committed effect always has a retrievable response — while keeping the existing `request_id` field,
the chain position, and the in-memory store as the zero-config default.

## Design decisions

- **D1 — Additive, not breaking.** The in-memory `DeduplicationStore`/`DeduplicateUnary` stay. A new
  `middleware.DurableIdempotencyStore` interface + `middleware.DurableDeduplicateUnary` interceptor
  provide the durable path, selected via `server.Config.DurableDedup`. Zero-config services are
  unchanged (still best-effort memory).
- **D2 — Scoped key everywhere.** Both paths key by **(tenant, method, request_id)**, never the bare
  `request_id`. Tenant = `middleware.VerifiedTenantID` (the verified principal; never the
  client-settable `account-id` header). This closes the cross-tenant/cross-method collision in the
  existing memory path too (security fix, regression-tested).
- **D3 — Claim/complete ride the handler's `Atomically`.** The durable interceptor opens the outer
  `persistence.TxRunner.Atomically`; the generated CRUD handler's repo write **nest-joins** it
  (`GormTxRunner.Atomically` reuses an on-ctx tx). Claim row + domain effect + completion commit as
  one unit → exactly-once. Handler error rolls the claim back with the effect (errors never cached,
  same as spec 023).
- **D4 — Two-phase store interface.** `Lookup` (non-tx fast replay), `Claim` (in-tx insert
  `in_progress`, returns the existing record on conflict), `Complete` (in-tx transition to
  `completed` with the serialized response), `GC` (delete expired). Replaces the too-thin
  `Load`/`Store` for the durable case.
- **D5 — Response replay by proto bytes.** `Complete` stores `proto.Marshal(resp)` + the message's
  full name. Replay resolves the type from `protoregistry.GlobalTypes`, unmarshals, and returns the
  message — so the client gets **exactly** the original response, including server-generated ids and
  etag. A non-proto response fails loud (`codes.Internal`) — durable replay requires protobuf.
- **D6 — `idempotency_keys` carries `account_id`.** PK = `(account_id, method, request_id)`, plus
  `expires_at` (GC) and an optional `fingerprint`. The `account_id` column makes it RLS-coverable
  (WS-029 X4): the tenant GUC set at `Begin` covers the claim/complete inside `Atomically`.
- **D7 — Optional param fingerprint (Stripe-style).** When enabled (default on for the durable
  store), the SHA-256 of the deterministically-marshaled request is stored; a later request reusing
  the key with a *different* body is rejected with `InvalidArgument`.
- **D8 — Bounded retention.** Records carry `expires_at = now + TTL` (default 24h, configurable).
  `Lookup`/`Claim` treat an expired record as absent (re-executable); `Claim` reclaims an expired
  conflicting row. `GC(ctx, now)` deletes expired rows for a periodic sweep.

## Acceptance criteria

- **AC-01** Given a completed durable record, a retry with the same (tenant, method, request_id)
  returns the **stored response verbatim** (same server-generated id/etag) and **does not** execute
  the handler again — even across process restart (persisted in the DB).
- **AC-02** The claim row commits **atomically** with the domain effect: if the handler errors, the
  claim rolls back and a retry re-executes; if it succeeds, the completed record and the effect are
  both present or both absent.
- **AC-03** A duplicate that observes an *already-committed* `in_progress` record (the saga
  reserve→remote→complete shape) returns `codes.AlreadyExists` (HTTP 409) — not a second execution.
  A *concurrent* duplicate on the single-transaction handler path does **not** 409: its claim blocks
  on the winner's uncommitted row and then replays the winner's committed response (see AC-04).
- **AC-04** Two concurrent fresh requests with the same key execute the handler **exactly once**;
  the loser blocks on the claim and then replays the winner's response (or re-claims after the winner
  rolls back) — never a double effect. Verified under `-race` with N concurrent callers.
- **AC-05** A `request_id` reused across **different tenants** never replays another tenant's
  response (both memory and durable paths). Reused across **different methods** does not collide.
- **AC-06** An empty `request_id` or `validate_only=true` passes through untouched (no claim, no
  replay), preserving spec 023 behavior.
- **AC-07** With fingerprinting on, reusing a key with a **different request body** is rejected
  `codes.InvalidArgument`; reusing it with the same body replays.
- **AC-08** Expired records (`expires_at <= now`) are treated as absent: a retry re-executes; `GC`
  removes them.
- **AC-09** The in-memory default path is unchanged for zero-config services (still best-effort,
  now additionally tenant/method-scoped).

## Failure modes

- **FM-01 Non-proto response on the durable path** → fail loud `codes.Internal` ("durable idempotency
  requires a protobuf response"); never silently skip persistence.
- **FM-02 Store/DB error on `Lookup`** → fall through to the transactional path (which re-derives
  correctness), never serve a stale/guessed response.
- **FM-03 Store/DB error on `Claim`/`Complete`** → the whole `Atomically` rolls back (no effect, no
  half-written record); the error propagates.
- **FM-04 Handler panics** → `Atomically` rolls back the claim and re-panics (nothing cached).
- **FM-05 RLS enabled but fast-path `Lookup` runs without the tenant GUC** → returns no row (RLS
  denies) → falls through to the in-tx path where the GUC is set → correct replay. Degraded
  efficiency, never incorrectness.
- **FM-06 Two requests race to claim an expired row** → the reclaim `UPDATE ... WHERE expires_at<=now`
  admits exactly one; the loser re-reads and replays/conflicts.

## Operational requirements (from the hardening review)

- **Effect must join the interceptor's transaction.** The handler's domain write must go through an
  SDK repository / `TxRunner` over the **same** backend as `Store`/`Tx` (the generated repositories
  do). Not runtime-enforced; a handler writing via a different DB breaks exactly-once.
- **Local DB-effect handlers only.** The handler runs inside the transaction; remote-effect handlers
  want the saga path (a slow remote call otherwise holds a DB connection + claim lock).
- **READ COMMITTED.** The conflict/reclaim reads assume READ COMMITTED (the default); a stricter
  global isolation must run the idempotency transaction at READ COMMITTED.
- **GC is host-scheduled.** Expired records read as absent but are not auto-deleted; the host runs a
  periodic `Store.GC` sweep (like the outbox relay).
- **Migrate `idempotency_keys`.** Distinct from the event-dedup `idempotency_markers` table; in the
  baseline, and AutoMigrate users add `gormtx.RequestIdempotencyMigrationModels()`.
- **High-entropy `request_id` + Authenticator.** The key is tenant-scoped, not subject-scoped;
  guessable ids let a tenant peer replay. `request_id` is capped at 255 chars (rejected
  `InvalidArgument` over that).

## Non-goals

- Optimistic concurrency / etags (AIP-154 — orthogonal, already present).
- Making the durable store the *forced* default (it is opt-in via config; memory stays the
  zero-config default — D1).
- Cross-service saga orchestration. The schema supports a committed `in_progress` (for a future
  reserve→remote-effect→complete path) and the interceptor returns 409 on it, but this feature ships
  only the single-service transactional claim/complete.
- A distributed idempotency service (the store is per-service, behind the seam).
- Changing the `request_id` field contract (AIP-155 stays).

---

# Increment 2 — deferred follow-ups (v0.63.0)

The v0.62.0 ship left two documented follow-ups (this file's original "Non-goals" #3 and the
hardening note "GC is host-scheduled"). This increment delivers both, still inside the
single-service lane (no cross-service orchestration).

## Deliverable A — servicekit auto-wiring + host-scheduled GC

**Problem.** Enabling the durable path required a service to hand-build `server.Config.DurableDedup`
(store + tx). A `servicekit`-composed service had no seam for it, and `Store.GC` was never scheduled
(retention grew unbounded — the v0.62.0 security-review finding).

### Design decisions

- **DA-1 — HostConfig opt-in.** New `HostConfig.DurableIdempotency *DurableIdempotencyConfig`
  (TTL / DisableFingerprint / MaxResponseBytes / Mode / GCInterval / DisableGC). nil = disabled =
  the best-effort in-memory `DeduplicateUnary` default, unchanged. Non-nil = the host installs the
  durable path.
- **DA-2 — Module supplies the namespaced store + tx** through a new `App.EnableDurableIdempotency`
  helper, exactly mirroring `App.Subscribe(ConsumerConfig{Tx, Idem})`: `servicekit` stays ORM-free
  (never imports gormtx), so the module — which owns its `*gorm.DB` and its `DatabaseNamespace` —
  builds `gormtx.NewGormDurableDedupStore(db, WithDurableDedupNamespace(ns))` + its `TxRunner` and
  hands them over. Calling the helper when the host did not opt in is a fail-loud error.
- **DA-3 — Late-bound host router.** `server.New` bakes the interceptor chain before modules
  register, so the host installs a `hostDurableDedup` holder (implements both
  `middleware.DurableIdempotencyStore` and `persistence.TxRunner`) as `DurableDedup.Store`/`.Tx` at
  `New`, and modules populate it during `Register`. It routes each request to the owning module's
  store by `key.Method` (and its tx by the gRPC method / the sole registration), so a composed
  multi-module host with per-module-namespaced `idempotency_keys` tables is correct.
- **DA-4 — Safe dev fallback (no forced DB).** When the opt-in is on but no module registers a
  store (persistence-free dev), the holder routes to an in-process `memDurableStore` — a correct,
  transactional (rollback-on-error), TTL+GC in-memory `DurableIdempotencyStore`. Idempotency still
  holds per-pod; no DB is required.
- **DA-5 — Host-scheduled GC.** When durable idempotency is enabled (and not `DisableGC`), the host
  starts one background sweep goroutine tied to the host lifecycle (started after `Register`,
  stopped on ctx cancel and waited on at shutdown) calling `holder.GC(ctx, now)` every `GCInterval`
  (default 15m). This closes the unbounded-retention finding.
- **DA-6 — Fail loud if enabled-but-not-migrated.** At boot (after `Register`, before `Serve`) the
  host probes each registered store with a sentinel `Lookup`; a missing `idempotency_keys` table
  surfaces a clear boot error naming `gormtx.RequestIdempotencyMigrationModels()` — mirroring the
  framework-table posture. The scaffold's host migration path is updated to include those models
  when durable idempotency is enabled.

### Acceptance criteria (A)

- **AC-A1** A single-module servicekit host with `HostConfig.DurableIdempotency` set and a module
  that calls `App.EnableDurableIdempotency` gets durable, exactly-once idempotency end-to-end
  (verbatim replay across a fresh request) without hand-building `server.Config.DurableDedup`.
- **AC-A2** With the opt-in OFF (nil), behavior is byte-for-byte unchanged (best-effort memory dedup).
- **AC-A3** With the opt-in ON but no module registering a store (no DB), the host boots and dedups
  in-process via the fallback — it does NOT force a DB or crash.
- **AC-A4** GC runs on the configured interval and removes expired records; it stops on host
  shutdown (no leaked goroutine, verified under `-race`).
- **AC-A5** Enabling durable idempotency against an un-migrated DB fails loudly at boot with a
  message naming the migration models — never a silent per-request failure.
- **AC-A6** `App.EnableDurableIdempotency` called without the HostConfig opt-in returns a clear
  error at Register (fail loud).

## Deliverable B — reserve→remote→complete saga path (remote-effect idempotency)

**Problem.** `DurableDeduplicateUnary` runs the handler *inside* the claim transaction — correct for
a LOCAL DB effect, wrong for a REMOTE effect (it holds a DB connection + the claim row lock across
the remote call, and the remote effect is outside the rollback).

### Design decisions

- **DB-1 — New interceptor variant, selected by a `Mode`.** `middleware.DurableDedup` gains
  `Mode DurableDedupMode` (`DurableModeTransactional` default | `DurableModeReserve`).
  `server.New` picks `DurableReserveUnary` for Reserve mode, else the existing
  `DurableDeduplicateUnary`. Same ergonomics, same `DurableDedup` config, same store interface.
- **DB-2 — Three short transactions, none held across the remote call.**
  1. **Reserve**: `Tx.Atomically{ Store.Claim }` — commits an `in_progress` record, then the tx is
     released. 2. **Remote effect**: `handler(ctx, req)` runs OUTSIDE any tx; it performs the remote
  side effect, made idempotent downstream by passing the same `request_id`/key. 3. **Complete**:
  `Tx.Atomically{ Store.Complete }` — transitions to `completed` with the stored response.
- **DB-3 — No schema change.** Reuses the existing `idempotency_keys` table, the `in_progress` /
  `completed` states, and the existing 409 branch. The only store addition is
  `Abandon(ctx, key) (bool, error)` — a DELETE **guarded to `status = in_progress`** so it can never
  erase a durable (`completed`) response. (Confirmed: the baseline is unchanged.)
- **DB-4 — Error semantics (documented choice).**
  - *Handler (remote) error*: **release** the reservation (`Abandon`, best-effort) so an immediate
    retry re-executes — safe because the remote is idempotent by `request_id`. A release failure
    leaves the record to expire by TTL (still correct).
  - *Handler succeeded but `Complete` failed* (the "remote succeeded, record lost" gap): the
    reservation **stays `in_progress`** and the error propagates. A duplicate within TTL gets 409;
    after TTL expiry a retry re-executes and the remote dedups. Not released here — releasing would
    invite a needless re-run of a succeeded remote effect and drop the 409 guard. The remote MUST be
    idempotent for recovery to be safe; operators give saga methods a short TTL.
- **DB-5 — Retry semantics.** A duplicate observing a committed `in_progress` reservation returns
  `ErrIdempotencyInProgress` (`AlreadyExists` / HTTP 409) — chosen over a 202 because gRPC has no
  Accepted code and `AlreadyExists`→409 is the idiomatic map already used by the transactional path.
  A `completed` duplicate replays verbatim. Fingerprint, TTL, and tenant-scoping behave exactly as
  the local path.

### Acceptance criteria (B)

- **AC-B1** In Reserve mode the claim commits and is released BEFORE the handler runs (no DB
  connection or row lock held across the remote effect) — the completion runs in a separate short tx.
- **AC-B2** A duplicate arriving while a committed reservation is `in_progress` gets 409; a
  `completed` duplicate replays the stored response verbatim.
- **AC-B3** A handler error releases the reservation (`Abandon`) so an immediate retry re-executes;
  `Abandon` never deletes a `completed` record.
- **AC-B4** A `Complete` failure leaves the reservation `in_progress` (documented gap); the error
  propagates and recovery relies on TTL + remote idempotency.
- **AC-B5** Fingerprint mismatch, over-length request_id, empty/validate_only pass-through, tenant
  scoping, and non-proto-response fail-loud all behave identically to the transactional path.

### Failure modes (increment 2)

- **FM2-01** GC goroutine panics → contained (recovered + logged); the host keeps serving.
- **FM2-02** `App.EnableDurableIdempotency` with a nil Store or Tx → fail loud at Register.
- **FM2-03** Reserve-mode `Claim` fails → no reservation, error propagates, retry re-executes.
- **FM2-04** Reserve-mode remote succeeds but process crashes before `Complete` → reservation is a
  committed `in_progress`; a retry within TTL gets 409, after TTL re-executes (remote dedups).
- **FM2-05** Fallback `memDurableStore` used outside its `Atomically` for a write → fail loud.

### Tasks (increment 2)

- **T12 [C]** `middleware`: add `DurableDedupMode` + `DurableDedup.Mode`; add `Abandon` to
  `DurableIdempotencyStore`; implement `DurableReserveUnary` (reserve→handler→complete, release on
  error, Complete-gap handling); factor the shared gate/key/fingerprint/replay. (DB-1..5, AC-B*)
- **T13 [C]** `persistence/gormtx`: implement `GormDurableDedupStore.Abandon` (guarded DELETE). (DB-3)
- **T14 [C]** `server/server.go`: select `DurableReserveUnary` when `DurableDedup.Mode ==
  DurableModeReserve`. (DB-1)
- **T15 [C]** `servicekit`: `HostConfig.DurableIdempotency`, `App.EnableDurableIdempotency`,
  `hostDurableDedup` router, `memDurableStore` fallback, boot migration probe, GC ticker. (DA-1..6)
- **T16 [S]** `servicekit`/scaffold: include `gormtx.RequestIdempotencyMigrationModels()` in the
  host migration path when durable idempotency is enabled; docs touchpoint. (DA-6)
- **T17 [C]** Tests: middleware unit tests for `DurableReserveUnary` (reserve-before-handler,
  409-on-in-progress, replay, release-on-error, Complete-gap); gormtx SQLite integration for
  `Abandon` + a full saga round-trip; servicekit host tests (AC-A1..A6) incl. GC lifecycle under
  `-race`. Every hardening defect gets a regression test.
- **T18 [S]** `make vet` / `make test` / `make build-gowork-off`; CHANGELOG; synchronized v0.63.0
  release.

### Hardening loops (increment 2) — findings + fixes

Two independent reviews (security + correctness). Each fix has a regression test.

- **H1 (correctness, HIGH — fixed).** Shutdown deadlock: the GC goroutine only exits on ctx cancel,
  but the cleanup defer waited on it *before* the host ctx was cancelled, so a `Serve`-time error with
  a still-live ctx (port bind, boot gate) hung forever. Fix: cancel the host ctx first in the cleanup
  defer. Test: `TestRun_DurableIdempotency_ServeErrorDoesNotDeadlock`.
- **H2 (security MEDIUM / correctness MEDIUM — fixed).** Router sent an owned-but-unregistered
  module's request to a *different* module's store/tx (via the single-registration branch) —
  wrong-backend binding under dedicated-DB isolation. Fix: an owned method whose module did not
  register routes to the isolated in-process fallback, never another module; a boot WARNING names the
  unregistered modules (no longer silent). Test: `TestHostDurableDedup_OwnedButUnregistered_RoutesToFallback`.
- **H3 (both, LOW — fixed).** Store routed by `key.Method`, tx by `grpc.Method(ctx)` — divergent if
  the gRPC method is absent. Fix: the interceptor stashes the method on ctx
  (`middleware.IdempotencyMethodFromContext`); the router binds the tx by the SAME string (dropped the
  grpc dependency). Covered by the routing + exactly-once holder tests.
- **H4 (Abandon fence, LOW — documented).** `Abandon` matches `status=in_progress`, not the claim
  instance, so a TTL shorter than the remote latency permits a stale winner to delete a reclaimed
  reservation (bounded, self-healing, remote-idempotent). Fix: documented the TTL-must-exceed-max-
  latency requirement (interceptor doc + resilience how-to) rather than an interface change.
- **H5 (nits — fixed).** `hostDurableDedup.GC` now continues past a per-store error (`errors.Join`) so
  one failing store cannot starve the others/fallback; the duplicated fast-path replay block was
  extracted to `cfg.fastReplay`; the `memDurableStore` dev-only caveats (mutex-across-handler, growth
  when GC disabled) are documented.
- **Cleared (no change):** no cross-tenant/cross-method leak (key carries verified tenant + method
  end-to-end); no injection (parameterized queries; request_id capped; table name from namespace, not
  input); `inMemTx` sentinel unspoofable (unexported); GC panic contained + `done` always closed.
