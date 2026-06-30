# Changelog

The canonical source of truth for releases is the Git release tags / GitHub Releases
of this repository. This file summarizes notable changes in a human-readable form,
following the spirit of [Keep a Changelog](https://keepachangelog.com/).

Where the underlying source explicitly stated a version, the change is grouped under
that version. Notable changes that were not tied to a specific version are collected
under [History](#history).

## History

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
