# F026 Tasks — Batch Method Codegen + BatchUpdate

Model routing: `[S]` = Sonnet subagent (mechanical), `[C]` = Opus (generator/design logic).
Gate: each phase's tests green before the next.

## Phase 1 — Persistence core (foundation) ✅ done
- [X] T101 `[S]` Add `BatchUpdateItem[T,K]` + `BatchUpdate` to `BatchRepository` (`persistence/repository.go`). FR-001
- [X] T102 `[S]` Implement `MemoryRepository.BatchUpdate` (atomic two-pass) (`persistence/memory.go`). FR-002
- [X] T103 `[S]` Tests: `BatchUpdate` Success / EmptyItems / MissingKey / SoftDeletedKey (`persistence/memory_test.go`). AC-001..004
- [X] T104 `[S]` `go test ./persistence/...` green.

## Phase 2 — GORM codegen (`protoc-gen-storage`) ✅ done
- [X] T201 `[C]` Emit `BatchGet` (`IN` + reorder by key + missing/soft-deleted → NotFound, tenant-scoped). FR-010
- [X] T202 `[C]` Emit `BatchDelete` (`db.Transaction` + dedup + bulk `IN` delete + RowsAffected check, tenant-scoped). FR-010/011
- [X] T203 `[C]` Emit `BatchUpdate` (`db.Transaction` reusing single `Update` per item). FR-010/011
- [X] T204 `[C]` Flip compile-check to `persistence.BatchRepository`; update generator render tests.
- [X] T205 `[S]` Regenerated toy + apikey storage; `go build` + `go test ./cmd/protoc-gen-storage/` green.
- [X] T206 `[C]` GORM-sqlite tests (`batch_sqlite_test.go`): happy path, atomic-missing, soft-delete, tenant scoping. AC-010/012/013/014/015

## Phase 3 — ent codegen (`protoc-gen-ent`, wrapper)
- [ ] T301 `[C]` Emit `<Resource>EntRepository` wrapper file (embeds adapter; resource/field metadata).
- [ ] T302 `[C]` `BatchGet`/`BatchDelete` via `client.Tx` + explicit tenant + soft-delete predicates. FR-020, FM-004
- [ ] T303 `[C]` `BatchUpdate` per-field mask setters + secret re-encryption inside Tx. FR-020, FM-006
- [ ] T304 `[C]` Compile-check `persistence.BatchRepository`; update render tests.
- [ ] T305 `[S]` Regenerate apikey ent wrapper; `go build` + `go test ./cmd/protoc-gen-ent/...`.
- [ ] T306 `[C]` ent-sqlite tests: tenant isolation, soft-delete, atomicity. AC-011/014/015

## Phase 4 — Toy fixture (BatchUpdate end-to-end)
- [ ] T401 `[S]` Proto: add `BatchUpdateWidgets` (+messages), keep BatchGet/Delete; regenerate pb/gw/svc. FR-030
- [ ] T402 `[S]` Handler: implement `BatchUpdateWidgets`. FR-031
- [ ] T403 `[S]` Integration tests: `TestBatchUpdateWidgets` happy + atomic-missing. AC-021/022

## Phase 5 — Cross-backend conformance
- [ ] T501 `[C]` Shared batch behavior suite run against Memory + GORM-sqlite + ent-sqlite. FR-040, AC-020

## Phase 6 — Docs + tracker
- [ ] T601 `[S]` `persistence.md`: document all three batch methods; remove "codegen not implemented" warning; cover ent. FR-050
- [ ] T602 `[S]` Update hub `specs/aip-gap-tracker.md` AIP-137 row + add F026 backlog entry.

## Phase 7 — Verification gate
- [ ] T701 `[C]` `/verify-change`: full build + lint + tests; scope diff vs acceptance criteria.
