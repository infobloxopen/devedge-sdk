// atlas.hcl — build-time Atlas config for the devedge-sdk framework migration
// baseline (WS-022 F043 / T2). The `framework` env diffs the canonical gormtx
// framework model set (printed by ./schemagen, an isolated tool module) against the
// committed baseline/ directory, which is kept in golang-migrate up/down format so the
// SAME files the infobloxopen/migrate engine applies are the ones Atlas drift-checks.
// Regenerate:  atlas migrate diff framework_init --env framework
// Drift gate:  atlas migrate diff --env framework   (must report "no changes")
data "external_schema" "framework" {
  program = ["go", "-C", "schemagen", "run", "-mod=mod", ".", "postgres"]
}

env "framework" {
  src = data.external_schema.framework.url
  dev = "docker://postgres/16/dev?search_path=public"
  migration {
    dir    = "file://baseline"
    format = golang-migrate
  }
}
