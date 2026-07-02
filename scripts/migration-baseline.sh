#!/usr/bin/env bash
# migration-baseline.sh — (re)generate and drift-check the SDK framework migration
# baseline (WS-022 F043 / T2). The baseline (persistence/migrate/baseline/0001_framework_
# init.{up,down}.sql) is generated from the CANONICAL gormtx framework model set by Atlas
# (ariga.io/atlas) via the isolated schemagen tool module — a BUILD-TIME concern only;
# neither Atlas nor its gorm provider ever enters a runtime module's dependency graph.
#
# Usage:
#   scripts/migration-baseline.sh generate   # regenerate the committed baseline in place
#   scripts/migration-baseline.sh check      # drift gate: fail if the baseline is stale
#
# Requires the Atlas CLI (ariga.io/atlas — NOT the unrelated `atlas` app tool) on PATH,
# or ATLAS pointing at it, plus a running Docker daemon (Atlas spins an ephemeral dev
# Postgres to normalize the DDL). Both absent -> the script SKIPS with a clear message so
# a local `make` without Atlas/Docker still passes; CI installs Atlas and runs it for real.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIG_DIR="${REPO_ROOT}/persistence/migrate"
ATLAS="${ATLAS:-atlas}"
CMD="${1:-check}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }

# Guard: the `atlas` name collides with an unrelated Infoblox CLI. Verify we have the
# schema-migration Atlas (its `version` prints "atlas version …").
if ! command -v "$ATLAS" >/dev/null 2>&1 || ! "$ATLAS" version 2>/dev/null | grep -qi "atlas version"; then
  yellow "SKIP: ariga.io/atlas CLI not found on PATH (set ATLAS=/path/to/atlas). The committed baseline is authoritative; install Atlas + Docker to (re)generate or drift-check it."
  exit 0
fi
if ! docker info >/dev/null 2>&1; then
  yellow "SKIP: Docker daemon unavailable — Atlas needs an ephemeral dev Postgres to normalize the DDL."
  exit 0
fi

# regenerate <dir> — produce the framework baseline (0001_framework_init.{up,down}.sql +
# atlas.sum) into <dir>, normalizing Atlas's timestamped filename to the 4-digit 0001
# convention (F043 D-7) so re-runs on unchanged models are byte-identical.
regenerate() {
  local out="$1"
  mkdir -p "$out"
  rm -f "$out"/*.sql "$out"/atlas.sum
  # Atlas reads env "framework" from persistence/migrate/atlas.hcl; --dir overrides the
  # output. GOWORK=off so the isolated schemagen tool module resolves via its own go.mod.
  ( cd "$MIG_DIR" && GOWORK=off "$ATLAS" migrate diff framework_init --env framework --dir "file://$out" >/dev/null )
  ( cd "$out"
    for f in *_framework_init.up.sql;   do [ -e "$f" ] && mv -f "$f" 0001_framework_init.up.sql;   done
    for f in *_framework_init.down.sql; do [ -e "$f" ] && mv -f "$f" 0001_framework_init.down.sql; done )
  ( cd "$MIG_DIR" && GOWORK=off "$ATLAS" migrate hash --dir "file://$out" >/dev/null )
}

case "$CMD" in
  generate)
    regenerate "${MIG_DIR}/baseline"
    green "framework baseline regenerated: persistence/migrate/baseline/0001_framework_init.{up,down}.sql"
    ;;
  check)
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    regenerate "$tmp/baseline"
    fail=0
    for f in 0001_framework_init.up.sql 0001_framework_init.down.sql; do
      if ! diff -u "${MIG_DIR}/baseline/$f" "$tmp/baseline/$f" >/dev/null 2>&1; then
        red "DRIFT: persistence/migrate/baseline/$f is stale vs the gormtx framework models."
        diff -u "${MIG_DIR}/baseline/$f" "$tmp/baseline/$f" || true
        fail=1
      fi
    done
    if [ "$fail" -ne 0 ]; then
      red "framework baseline DRIFT — run: make generate-migration-baseline"
      exit 1
    fi
    green "framework baseline is in sync with the gormtx framework models"
    ;;
  *)
    echo "usage: $0 {generate|check}"; exit 2;;
esac
