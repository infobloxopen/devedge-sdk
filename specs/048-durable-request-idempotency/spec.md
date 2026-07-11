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
