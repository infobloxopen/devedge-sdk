# Tasks: F018 — Field-mask application in GORM Update

**Branch**: `018-fieldmask-apply`

---

- [ ] T001 [S] Update `cmd/protoc-gen-storage/render.go` — translate mask fields to DB columns.

  Find the block that generates the fieldMask Select in the Update method:
  ```go
  b.WriteString("\tif len(fieldMask) > 0 {\n")
  b.WriteString("\t\tq = q.Select(fieldMask)\n")
  b.WriteString("\t}\n")
  ```
  Replace it with:
  ```go
  fmt.Fprintf(b, "\tif len(fieldMask) > 0 {\n")
  fmt.Fprintf(b, "\t\tdbCols := make([]string, 0, len(fieldMask))\n")
  fmt.Fprintf(b, "\t\tfor _, f := range fieldMask {\n")
  fmt.Fprintf(b, "\t\t\tcol, ok := %sColumns[f]\n", msg.MessageName)
  fmt.Fprintf(b, "\t\t\tif !ok {\n")
  fmt.Fprintf(b, "\t\t\t\treturn nil, status.Errorf(codes.InvalidArgument, \"unknown field in update_mask: %%q\", f)\n")
  fmt.Fprintf(b, "\t\t\t}\n")
  fmt.Fprintf(b, "\t\t\tdbCols = append(dbCols, col)\n")
  fmt.Fprintf(b, "\t\t}\n")
  fmt.Fprintf(b, "\t\tq = q.Select(dbCols)\n")
  fmt.Fprintf(b, "\t}\n")
  ```
  `codes`, `status`, and `<Msg>Columns` are all already imported by F017 changes.

  Update `render_test.go` assertions in `TestRenderStorageFile_basic`:
  ```go
  mustNotContain(t, out, "q.Select(fieldMask)")
  mustContain(t, out, "Columns[f]")
  ```

  Run `go test ./cmd/protoc-gen-storage/... -count=1`.

- [ ] T002 [S] Regenerate testdata and verify.

  Run `make generate` from the devedge-sdk root.
  Verify:
  - `testdata/toy/widgetsv1/widgets.storage.go` contains the column-map lookup loop.
  - `testdata/apikey/apikeyv1/apikey.storage.go` contains the column-map lookup loop.
  - Neither file contains `q.Select(fieldMask)`.
  Run `make build && make test`.

- [ ] T003 [S] Commit + merge.

  ```bash
  git add cmd/protoc-gen-storage/ testdata/ specs/018-fieldmask-apply/
  git commit -m "018: translate field_mask proto names to DB columns in generated Update"
  git checkout main && git merge --no-ff 018-fieldmask-apply -m "018: merge fieldmask apply"
  ```

## Complexity Tags

All [S] — one targeted substitution in render.go, regenerate, verify.
