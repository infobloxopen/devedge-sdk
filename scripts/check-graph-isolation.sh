#!/usr/bin/env bash
# check-graph-isolation.sh — the graph-level dependency-isolation proof for the
# multi-module split (WS-011 / F039, AC-1 + AC-2).
#
# cleancore_test.go proves the *build* is clean (a server-only consumer compiles
# ZERO OTel-SDK packages). This script proves the *module graph* is clean: a
# throwaway consumer that requires devedge-sdk and imports ONLY .../server pulls
# none of the heavy adapter deps into its go.mod require list or its compiled
# build closure — and the CONVERSE, that adding the observability/otel adapter
# DOES pull the OTel SDK in. The dep arrives only on opt-in.
#
# It runs against the LOCAL working tree (pre-release): the consumer modules
# `replace` devedge-sdk (and, for the converse, observability/otel) to this repo,
# so the check reflects HEAD, not the last published tag. GOWORK=off so the
# consumer resolves through its own go.mod + replaces, never the repo workspace.
#
# IMPORTANT NUANCE (read before extending the guard families in P1/P2):
#   go.mod require list / build closure  vs  go.sum.
#   We assert against the consumer's go.mod (the require graph) and `go list
#   -deps` (the compiled closure) — NOT go.sum. go.sum records a checksum for
#   every module in the *pruned module graph*, which includes the test-only
#   `require`s of a consumer's DIRECT deps. The OTel API contrib handlers that
#   the core legitimately KEEPS (otelgrpc/otelhttp) declare `require
#   go.opentelemetry.io/otel/sdk` in THEIR OWN go.mod (for their tests), so
#   `otel/sdk` lands in any server-consumer's go.sum as a graph checksum even
#   though no otel/sdk package is ever compiled. That is a Go module-graph
#   property of keeping otelgrpc, not a dependency the consumer ships. The
#   exporters (.../otel/exporters/*) have no such back-reference and leave go.sum
#   entirely — so we assert the strongest achievable claim per family:
#     * exporters: absent from go.mod, build closure AND go.sum;
#     * otel/sdk : absent from go.mod require list and build closure (compiled
#                  packages); present in go.sum only via otelgrpc's test require.
#   P1/P2: a heavy dep that NO retained core dep back-references (koanf,
#   franz-go, gorm, ent) should leave go.sum too — assert it there. Verify per
#   family rather than assuming go.sum is clean.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK_PATH="github.com/infobloxopen/devedge-sdk"
OTEL_MOD="${SDK_PATH}/observability/otel"

# Guard fragments that must NOT appear in a server-only consumer's go.mod require
# list or compiled build closure (the exporters additionally must not appear in
# go.sum). Phase 0 covers the OTel adapter; P1/P2 extend this list.
GOMOD_GUARDS=(
  "go.opentelemetry.io/otel/sdk"
  "go.opentelemetry.io/otel/exporters/"
)
# Subset of the above that must ALSO be absent from go.sum (no retained core dep
# back-references them). otel/sdk is intentionally NOT here — see the nuance note.
GOSUM_GUARDS=(
  "go.opentelemetry.io/otel/exporters/"
)

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

fail=0

# scaffold_consumer <dir> <extra-import-block> <extra-require-line> <extra-replace-line>
# Writes a minimal consumer module that always imports .../server, plus any extra
# import/require/replace the caller supplies, then `go mod tidy`s it (GOWORK=off).
scaffold_consumer() {
  local dir="$1" extra_import="$2" extra_require="$3" extra_replace="$4"
  mkdir -p "$dir/p"
  cat > "$dir/go.mod" <<EOF
module example.com/isolation-consumer

go 1.25.5

require ${SDK_PATH} v0.0.0
${extra_require}

replace ${SDK_PATH} => ${REPO_ROOT}
${extra_replace}
EOF
  cat > "$dir/p/p.go" <<EOF
package p

import (
	_ "${SDK_PATH}/server"
${extra_import}
)
EOF
  ( cd "$dir" && GOWORK=off GOFLAGS=-mod=mod go mod tidy ) >/dev/null 2>&1
}

# assert_absent <label> <file> <guard...> — fail if any guard fragment is present.
assert_absent() {
  local label="$1" file="$2"; shift 2
  local g hit=0
  for g in "$@"; do
    if grep -qF "$g" "$file"; then
      red "  LEAK: ${label} contains \"${g}\""
      grep -F "$g" "$file" | sed 's/^/    /'
      hit=1
    fi
  done
  if [ "$hit" -eq 0 ]; then green "  OK: ${label} free of: $*"; fi
  return $hit
}

# assert_present <label> <file> <guard...> — fail unless at least one guard present.
assert_present() {
  local label="$1" file="$2"; shift 2
  local g
  for g in "$@"; do
    if grep -qF "$g" "$file"; then green "  OK: ${label} contains \"${g}\""; return 0; fi
  done
  red "  MISSING: ${label} expected one of: $* (none found)"
  return 1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ---------------------------------------------------------------------------
echo "== AC-1: a server-ONLY consumer's graph is free of the OTel SDK + exporters =="
c1="$work/server-only"
scaffold_consumer "$c1" "" "" ""
# go.mod require list: none of the guarded families.
assert_absent "server-only go.mod" "$c1/go.mod" "${GOMOD_GUARDS[@]}" || fail=1
# Compiled build closure: zero otel/sdk + exporter PACKAGES.
closure="$(cd "$c1" && GOWORK=off go list -deps ./p 2>/dev/null || true)"
if printf '%s\n' "$closure" | grep -qE "go.opentelemetry.io/otel/sdk|go.opentelemetry.io/otel/exporters/"; then
  red "  LEAK: server-only build closure compiles an otel/sdk or exporter package:"
  printf '%s\n' "$closure" | grep -E "go.opentelemetry.io/otel/sdk|go.opentelemetry.io/otel/exporters/" | sed 's/^/    /'
  fail=1
else
  green "  OK: server-only build closure compiles ZERO otel/sdk + exporter packages"
fi
# go.sum: exporters must be wholly absent (otel/sdk excepted — see nuance note).
assert_absent "server-only go.sum" "$c1/go.sum" "${GOSUM_GUARDS[@]}" || fail=1

echo ""
# ---------------------------------------------------------------------------
echo "== AC-2: adding the observability/otel adapter pulls the OTel SDK in (opt-in) =="
c2="$work/otel-consumer"
scaffold_consumer "$c2" \
  "	_ \"${OTEL_MOD}\"" \
  "require ${OTEL_MOD} v0.0.0" \
  "replace ${OTEL_MOD} => ${REPO_ROOT}/observability/otel"
# The OTel SDK must now appear in the build closure (the adapter compiles it).
closure2="$(cd "$c2" && GOWORK=off go list -deps ./p 2>/dev/null || true)"
if printf '%s\n' "$closure2" | grep -q "go.opentelemetry.io/otel/sdk"; then
  green "  OK: otel-importing consumer COMPILES go.opentelemetry.io/otel/sdk (dep arrived on opt-in)"
else
  red "  MISSING: otel-importing consumer does not compile otel/sdk — adapter wiring broken"
  fail=1
fi
# And the exporters (absent in the server-only case) now appear in go.sum.
assert_present "otel-consumer go.sum" "$c2/go.sum" \
  "go.opentelemetry.io/otel/sdk" "go.opentelemetry.io/otel/exporters/" || fail=1

echo ""
if [ "$fail" -ne 0 ]; then
  red "graph-isolation check FAILED"
  exit 1
fi
green "graph-isolation check PASSED (AC-1 + AC-2)"
