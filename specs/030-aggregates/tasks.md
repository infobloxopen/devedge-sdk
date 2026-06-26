# F030 — Tasks (Tier 0: transaction seam)

Scope = the F030 "Scope of THIS feature" section of `spec.md` only. Tags: **[S]** sequential (depends on prior), **[C]** parallelizable. Locked decisions: D-1 = (a) regenerate the ent adapter to resolve tx-or-client from `ctx`; AC-1/AC-2 scope = single parent+child atomic write (not full-cluster Load/Save).

## Phase 1 — seam + in-memory (no codegen)
- **T1 [S]** Add `persistence/tx.go`: `TxRunner` interface (`Atomically(ctx, fn func(ctx) error) error`) + an unexported ctx key type + helpers `txFromContext(ctx)` / `withTx(ctx, h)`. No ORM/driver import (clean-core rule).
- **T2 [S]** In-memory `TxRunner`: implement on `MemoryRepository` (or a small `MemoryTxRunner` wrapping the shared store). Snapshot the maps under the existing `RWMutex` at `Atomically` entry; restore on error/panic; keep on success. Nested `Atomically` joins the outer tx (no-op begin).
- **T3 [C]** Unit tests for the in-memory path proving AC-1 (rollback on mid-`fn` error → no partial write) and AC-2 (writes invisible to a concurrent reader until commit; discarded on rollback).

## Phase 2 — ent tx-aware adapter (codegen)
- **T4 [S]** `cmd/protoc-gen-ent`: change the generated repo so each op resolves the ent client from `ctx` — `tx.<Type>` when `ctx` carries a tx, else the constructor client. Decide the carrier: a tx handle on ctx holding `*ent.Tx`; the generated repo's closures call a tiny resolver instead of capturing the bare client. Update `render.go` + render tests (`go/format.Source` + substring).
- **T5 [S]** ent `TxRunner`: a type wrapping `*ent.Client` whose `Atomically` opens `client.Tx(ctx)`, puts the `*ent.Tx` on ctx, runs `fn`, commits on nil / rolls back on error or panic.
- **T6 [S]** Re-render fixtures: regenerate `apikey`, `fleet`, `toy` ent repos via the updated generator; check in the regenerated files. Confirm `go build ./...` green.
- **T7 [C]** ent integration tests (sqlite) proving AC-1 + AC-2 through the generated repo inside `Atomically` (load parent → check → create child; forced error rolls back; concurrent read sees nothing pre-commit).

## Phase 3 — hardening + docs
- **T8 [S]** Failure-mode hardening: decide & implement the "un-enrolled write inside `Atomically`" signal (★ open Q3) — at minimum document; ideally detect a write that didn't see the ctx tx.
- **T9 [C]** Docs: `docs/content/docs/concepts/transactions.md` (Atomically, atomic check-then-write recipe, etag-as-concurrency-token) + stub `concepts/aggregates.md` (decision test + forward pointer to F031).
- **T10 [S]** Verify gate: `make build`, `make test`, `make vet` green (use `go vet` for lint — golangci-lint panics under go1.26 here); run `/verify-change`. Re-confirm AC-3 (no-opt-in services unaffected aside from the regenerated adapter).

## Exit criteria
AC-1..AC-4 met on **ent and in-memory**; fixtures regenerated and green; docs landed; verify-change passes. Then hardening round + release.
