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

## Phase 3 — ent codegen (`protoc-gen-ent`, wrapper) ✅ done
- [X] T301 `[C]` Emit `<Resource>EntRepository` wrapper file (embeds hand-written adapter; OUTPUT_ONLY propagated to field metadata).
- [X] T302 `[C]` `BatchGet` rides query interceptors; `BatchDelete` via `client.Tx` + explicit tenant + soft-delete predicates. FR-020, FM-004
- [X] T303 `[C]` `BatchUpdate` per-field mask setters (output-only excluded) + secret re-encryption inside Tx. FR-020, FM-006
- [X] T304 `[C]` Compile-check `persistence.BatchRepository`; `TestRenderEntRepository` added.
- [X] T305 `[S]` Regenerated apikey ent wrapper; `go build` + `go test ./cmd/protoc-gen-ent/` green.
- [X] T306 `[C]` ent-sqlite tests (`batch_ent_sqlite_test.go`): happy path, atomic-missing, soft-delete, tenant isolation. AC-011/014/015/020

## Phase 4 — Toy fixture (BatchUpdate end-to-end) ✅ done
- [X] T401 `[S]` Proto: added `BatchUpdateWidgets` (+messages), kept BatchGet/Delete; regenerated pb/gw/svc/authz (boot-gate + `/v1/widgets:batchUpdate` route). FR-030
- [X] T402 `[S]` Handler: `BatchUpdateWidgets` maps requests→BatchUpdateItem→repo.BatchUpdate. FR-031
- [X] T403 `[S]` Integration tests: `TestBatchUpdateWidgets` happy + `_MissingId_IsAtomic`. AC-021/022

## Phase 5 — Cross-backend conformance ✅ done
- [X] T501 `[C]` `batch_conformance_test.go`: one table-driven suite runs the same matrix against Memory + GORM-sqlite + ent-sqlite (folds in the per-backend files). FR-040, AC-020

## Phase 6 — Docs + tracker ✅ done
- [X] T601 `[S]` `persistence.md`: documented all three batch methods + `BatchUpdateItem`; replaced the warning with "both generators emit batch" (covers ent + the per-item mask note). FR-050
- [X] T602 `[S]` Updated hub `specs/aip-gap-tracker.md` AIP-137 row + added F026 backlog entry.

## Phase 7 — Verification gate ✅ done
- [X] T701 `[C]` Full build + `go vet` + tests green across SDK + toy + apikey modules; toy security suite green; scope diff vs acceptance criteria clean (no gold-plating — `OutputOnly` propagation traces to FR-020).
