#!/usr/bin/env bash
# release.sh — the SYNCHRONIZED multi-module release for devedge-sdk (WS-011 / F039).
#
# The repo is one git repo holding EIGHT Go modules (the root LIBRARY + seven nested
# ones: cmd, config/koanf, events/kafkabus, federationgql, observability/otel,
# persistence/gormtx, persistence/entrepo). Every release tags ALL of them at the
# SAME version so a consumer pins one version and the cross-module `require`s
# resolve coherently.
#
# Usage:
#   scripts/release.sh vX.Y.Z              # DRY RUN: print the two-phase plan + the
#                                          # bumps; tag/commit/push NOTHING.
#   scripts/release.sh vX.Y.Z --push       # the real thing: the two-phase release.
#   scripts/release.sh vX.Y.Z --validate   # LOCAL go.sum smoke (no network, no real
#                                          # tags) — proves the tag-root-first ordering
#                                          # lands the real root@vX.Y.Z hash in each
#                                          # adapter's go.sum. See --validate below.
#
# WHY TAG-ROOT-FIRST (the load-bearing ordering) ──────────────────────────────
#   An adapter's go.mod requires the root module at vX.Y.Z. Go's module security
#   means the adapter's go.sum must carry the CHECKSUM for root@vX.Y.Z, or any
#   `-mod=readonly` build (CI + a standalone external consumer) fails with
#       "missing go.sum entry … to verify package … is provided by exactly one module".
#   The ONLY way `go mod tidy` writes that real checksum is to resolve root@vX.Y.Z
#   from its published source (the git tag). A filesystem `replace` to the working
#   tree makes tidy SUCCEED but writes NO go.sum hash for the version — so a committed
#   adapter go.sum produced that way is INCOMPLETE, and pushing the tag later does
#   NOT retro-fill a committed go.sum. (Validated: see --validate.) Therefore:
#
#     PHASE 1 (root)    : commit the version-var bump + each adapter's
#                         `require root vX.Y.Z` (go mod edit only — do NOT finalize
#                         go.sum yet). Tag root vX.Y.Z and PUSH the root tag FIRST.
#                         Root has no intra-repo module deps, so it tidies/builds
#                         immediately.
#     PHASE 2 (adapters): now that root@vX.Y.Z is resolvable from the remote, run
#                         `GOWORK=off go mod tidy` in each adapter — the REAL
#                         root@vX.Y.Z hash lands in the adapter go.sum, NO replace.
#                         Commit the completed adapter go.mod+go.sum. Tag the 6
#                         adapters at <path>/vX.Y.Z on that second commit and push.
#
#   The root tag and the adapter tags sit on TWO different commits — standard and
#   correct for a multi-module repo. Every adapter is tagged only AFTER its go.sum is
#   complete, so a standalone `GOWORK=off` build at any adapter tag resolves cleanly.
#   NEVER run `go work sync` (in this nested layout it empties member require blocks
#   — a known footgun that bit P2). Tidy each module individually.
#
# POST-PUSH VERIFICATION (proxy lag)
#   After --push, verify an EXTERNAL consumer resolves each module: for every module
#   path, `GOPROXY=direct GOFLAGS=-mod=mod go get <mod>@vX.Y.Z` in a throwaway module
#   (or `go list -m <mod>@vX.Y.Z`). Use the EXPLICIT @vX.Y.Z, not @latest — the
#   proxy's @latest view lags a few minutes behind a fresh tag; an explicit version
#   fetches the tag directly. GONOSUMCHECK is NOT needed (these are public modules in
#   the checksum DB). The script prints the exact per-module commands at the end.
#
# RE-RUNNABLE / IDEMPOTENT
#   Re-running for the same vX.Y.Z is safe: the edits converge; tag creation is
#   guarded (refuses to clobber an existing tag unless --force-tags). The dirty-tree
#   guard allows ONLY the files the release owns, so dry-run → review → --push of the
#   same version works without a stash dance.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SDK_PATH="github.com/infobloxopen/devedge-sdk"

# The six nested modules (dir == module-path suffix). Each is tagged at
# <suffix>/vX.Y.Z (Go's nested-module tag convention); the root is tagged bare
# (vX.Y.Z). Keep this list in sync with go.work `use` and the Makefile MODULES.
NESTED_MODULES=(
  "cmd"
  "config/koanf"
  "events/kafkabus"
  "federationgql"
  "observability/otel"
  "persistence/gormtx"
  "persistence/entrepo"
)

# Testdata fixtures that CONSUME the SDK at a real version (`require root vX.Y.Z`) AND
# locally `replace` the adapter modules. When phase 2 bumps the adapters'
# `require root → vX.Y.Z`, Go's MVS pulls these fixtures up to root@vX.Y.Z too, so
# their go.mods go stale and `GOWORK=off go test ./...` (what main CI runs) fails with
# "updates to go.mod needed" until they are re-tidied. Phase 2 does that re-tidy.
# Inclusion criterion: requires the SDK at a real version (not v0.0.0) and replaces an
# adapter. testdata/toy is EXCLUDED — it pins the SDK at v0.0.0 via a local replace and
# uses no adapters, so a version bump never reaches it. Add a fixture here only if it
# meets the criterion. (These are throwaway modules, NOT part of any tagged module.)
TESTDATA_FIXTURES=(
  "testdata/apikey"
  "testdata/fleet"
  "testdata/iam"
  "testdata/federation"
  "examples/graphql-federation"
)

# The scaffold's single version source: fallbackSDKVersion in model.go. SDKVersion,
# OTelAdapterVersion, GormtxVersion and EntrepoVersion all resolve from the running
# binary's build info and fall back to THIS constant, so bumping it bumps them all.
MODEL_GO="cmd/devedge-sdk/internal/scaffold/model.go"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }

usage() {
  cat >&2 <<EOF
usage: $0 vX.Y.Z [--push | --validate] [--force-tags]

  vX.Y.Z         release version (must be a valid semver tag, e.g. v0.27.0).
  --push         run the real two-phase release (commit, tag, push).
                 Default (no flag) is a DRY RUN — print the plan, tag/push NOTHING.
  --validate     LOCAL go.sum smoke: prove tag-root-first lands the real root hash in
                 each adapter's go.sum, using a throwaway tag in a temp bare clone.
                 No network, no real tags, no working-tree mutation.
  --force-tags   allow re-creating tags that already exist (re-release of a version).
EOF
  exit 2
}

# ---- argument parsing -------------------------------------------------------
VERSION=""
PUSH=0
VALIDATE=0
FORCE_TAGS=0
for arg in "$@"; do
  case "$arg" in
    --push)        PUSH=1 ;;
    --validate)    VALIDATE=1 ;;
    --force-tags)  FORCE_TAGS=1 ;;
    -h|--help)     usage ;;
    v*)            [ -z "$VERSION" ] && VERSION="$arg" || usage ;;
    *)             red "unknown argument: $arg"; usage ;;
  esac
done
[ -n "$VERSION" ] || usage
if [ "$PUSH" -eq 1 ] && [ "$VALIDATE" -eq 1 ]; then
  red "--push and --validate are mutually exclusive"; usage
fi

# Validate semver-ish vX.Y.Z (allow a -prerelease suffix for pre-GA, e.g. v0.27.0-rc1).
if ! printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  red "invalid version '$VERSION' — expected vMAJOR.MINOR.PATCH (optionally -prerelease)"
  exit 2
fi

# ---- helpers ----------------------------------------------------------------
# set_require_version <module-dir> — point the adapter's root require at $VERSION
# (go mod edit only; no go.sum finalization).
set_require_version() {
  ( cd "$1" && go mod edit -require="$SDK_PATH@$VERSION" )
}

# bump_version_source — fallbackSDKVersion → $VERSION (the single scaffold source).
bump_version_source() {
  if grep -q "const fallbackSDKVersion = \"$VERSION\"" "$MODEL_GO"; then
    yellow "   fallbackSDKVersion already $VERSION (idempotent no-op)"
  else
    perl -i -pe "s/(const fallbackSDKVersion = )\"v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?\"/\${1}\"$VERSION\"/" "$MODEL_GO"
    green "   set fallbackSDKVersion = \"$VERSION\""
  fi
}

# ═════════════════════════════════════════════════════════════════════════════
# --validate : LOCAL proof that tag-root-first lands the real root hash in each
# adapter's go.sum, WITHOUT any public release. Mechanism: bare-clone this repo to a
# temp dir, create the throwaway root tag there, point Go at that local clone via a
# scoped `insteadOf` rewrite (GOPROXY=direct), then for each adapter (in a throwaway
# COPY) require root@VERSION and `GOWORK=off go mod tidy` — assert the real
# root@VERSION hash appears AND a `-mod=readonly` build is green. Never touches the
# real tree or creates a real tag.
# ═════════════════════════════════════════════════════════════════════════════
if [ "$VALIDATE" -eq 1 ]; then
  bold "== --validate: local go.sum smoke for tag-root-first ($VERSION) =="
  command -v git >/dev/null || { red "git required"; exit 1; }
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  bare="$tmp/devedge-sdk.git"
  echo "-- bare-cloning the repo + creating throwaway root tag $VERSION in it --"
  git clone --quiet --bare "$REPO_ROOT" "$bare"
  git -C "$bare" tag -f "$VERSION" "$(git -C "$REPO_ROOT" rev-parse HEAD)" >/dev/null
  # Scoped git rewrite so root@VERSION resolves from the local bare clone, not github.
  gitcfg="$tmp/gitconfig"
  git config --file "$gitcfg" "url.file://$bare.insteadOf" "https://$SDK_PATH"
  fail=0
  for dir in "${NESTED_MODULES[@]}"; do
    echo "  == $dir =="
    cp -R "$REPO_ROOT/$dir" "$tmp/m"
    ( cd "$tmp/m"
      rm -f go.sum
      go mod edit -require="$SDK_PATH@$VERSION"
      # Strip any intra-repo replace; this throwaway resolves root via the bare clone.
      go mod edit -dropreplace="$SDK_PATH" 2>/dev/null || true
      GOWORK=off GOFLAGS=-mod=mod GOPROXY=direct GOSUMDB=off GOPRIVATE="$SDK_PATH" \
        GIT_CONFIG_GLOBAL="$gitcfg" go mod tidy >/dev/null 2>&1
    )
    if grep -q "^$SDK_PATH $VERSION " "$tmp/m/go.sum" 2>/dev/null; then
      green "     go.sum carries $SDK_PATH $VERSION (real hash)"
    else
      red   "     go.sum MISSING $SDK_PATH $VERSION hash"; fail=1
    fi
    if ( cd "$tmp/m" && GOWORK=off GOPROXY=direct GOSUMDB=off GOPRIVATE="$SDK_PATH" \
         GIT_CONFIG_GLOBAL="$gitcfg" go build -mod=readonly ./... >/dev/null 2>&1 ); then
      green "     GOWORK=off go build -mod=readonly OK"
    else
      red   "     GOWORK=off go build -mod=readonly FAILED"; fail=1
    fi
    rm -rf "$tmp/m"
  done
  echo ""
  if [ "$fail" -ne 0 ]; then red "validate FAILED"; exit 1; fi
  green "validate PASSED: tag-root-first lands the real root hash + readonly build is green for all ${#NESTED_MODULES[@]} adapters"
  yellow "(this used a throwaway local tag in a temp bare clone — no real tag, no working-tree change)"
  exit 0
fi

if [ "$PUSH" -eq 1 ]; then
  bold "== devedge-sdk synchronized release $VERSION (REAL RUN: two-phase commit/tag/push) =="
else
  bold "== devedge-sdk synchronized release $VERSION (DRY RUN: prints the two-phase plan, mutates NOTHING) =="
fi
echo ""

# ---- working-tree safety ----------------------------------------------------
# Refuse if the tree is dirty in files the release does NOT touch. The files the
# release legitimately mutates are model.go + the six adapter go.mod/go.sum + the
# re-tidied testdata fixture go.mod/go.sum (so a dry-run/--push re-run stays clean).
ALLOWED_DIRTY_RE='^(cmd/go\.(mod|sum)|config/koanf/go\.(mod|sum)|events/kafkabus/go\.(mod|sum)|federationgql/go\.(mod|sum)|observability/otel/go\.(mod|sum)|persistence/gormtx/go\.(mod|sum)|persistence/entrepo/go\.(mod|sum)|testdata/(apikey|fleet|iam|federation)/go\.(mod|sum)|examples/graphql-federation/go\.(mod|sum)|cmd/devedge-sdk/internal/scaffold/model\.go)$'
unexpected="$(git status --porcelain | awk '{print $2}' | grep -Ev "$ALLOWED_DIRTY_RE" || true)"
if [ -n "$unexpected" ]; then
  red "working tree has unexpected changes (commit/stash them first):"
  printf '%s\n' "$unexpected" | sed 's/^/    /'
  exit 1
fi

CUR_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
ADAPTER_TAGS=()
for dir in "${NESTED_MODULES[@]}"; do ADAPTER_TAGS+=("$dir/$VERSION"); done

# ─────────────────────────────────────────────────────────────────────────────
# THE PLAN (printed in both dry-run and real-run so the operator sees the ordering)
# ─────────────────────────────────────────────────────────────────────────────
bold "── PHASE 1 (root) ─────────────────────────────────────────────────────────"
echo "   • bump $MODEL_GO: fallbackSDKVersion → $VERSION"
echo "   • set each adapter's require $SDK_PATH → $VERSION (go mod edit only; go.sum NOT finalized)"
echo "   • commit  ->  tag root $VERSION  ->  PUSH the root tag FIRST"
echo ""
bold "── PHASE 2 (adapters; only after the root tag is pushed) ───────────────────"
echo "   • GOWORK=off go mod tidy each adapter — the real $SDK_PATH@$VERSION hash lands in its go.sum"
echo "   • commit the completed adapter go.mod+go.sum"
echo "   • tag the ${#NESTED_MODULES[@]} adapters on that 2nd commit  ->  push them:"
for t in "${ADAPTER_TAGS[@]}"; do echo "        $t"; done
echo "   • re-tidy the ${#TESTDATA_FIXTURES[@]} testdata fixtures (pulled to root@$VERSION by the adapter bump) +"
echo "     gate each with GOWORK=off go test -run '^\$' ./... -> commit (no tag) so main CI stays green"
echo ""

# ---- always perform the version-var + require edits (so the diff is reviewable) ----
bold "-- applying PHASE-1 edits (version source + adapter requires) --"
bump_version_source
for dir in "${NESTED_MODULES[@]}"; do
  set_require_version "$dir"
  green "   $dir: require $SDK_PATH $VERSION"
done
echo ""

# Stage exactly the release-owned files for the diff preview.
STAGE_PATHS=("$MODEL_GO")
for dir in "${NESTED_MODULES[@]}"; do STAGE_PATHS+=("$dir/go.mod" "$dir/go.sum"); done
git add -- "${STAGE_PATHS[@]}" 2>/dev/null || git add -- "$MODEL_GO" $(for d in "${NESTED_MODULES[@]}"; do echo "$d/go.mod"; done)
bold "-- staged PHASE-1 bumps (require + version source; go.sums finalized in phase 2) --"
git --no-pager diff --cached --stat
echo ""

# ---- dry-run stop -----------------------------------------------------------
if [ "$PUSH" -ne 1 ]; then
  yellow "DRY RUN — nothing committed, no tags, no push. The require + version-var edits above are STAGED."
  yellow "Re-run with --push to execute the two-phase release (root tag first, then adapter go.sums + tags):"
  yellow "    scripts/release.sh $VERSION --push"
  yellow "Tip: scripts/release.sh $VERSION --validate proves the go.sum mechanic locally (no real tag)."
  exit 0
fi

# ═════════════════════════════════════════════════════════════════════════════
# REAL RUN — PHASE 1: commit, tag root, PUSH ROOT TAG FIRST.
# ═════════════════════════════════════════════════════════════════════════════
bold "── PHASE 1: committing version source + adapter requires ──"
if git diff --cached --quiet; then
  yellow "   nothing to commit (phase-1 edits already committed) — proceeding"
else
  git commit -m "release: $VERSION phase 1 — version source + adapter root requires (WS-011 / F039)"
  green "   committed phase 1"
fi
ROOT_COMMIT="$(git rev-parse HEAD)"

# Guard the root tag.
if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
  if [ "$FORCE_TAGS" -eq 1 ]; then
    git tag -f -a "$VERSION" -m "devedge-sdk $VERSION" "$ROOT_COMMIT"; yellow "   re-tagged root $VERSION (--force-tags)"
  else
    red "   root tag $VERSION already exists — use --force-tags or a new version"; exit 1
  fi
else
  git tag -a "$VERSION" -m "devedge-sdk $VERSION" "$ROOT_COMMIT"; green "   tagged root $VERSION on $ROOT_COMMIT"
fi

bold "── PHASE 1: pushing the branch + the ROOT tag (first) ──"
git push origin "$CUR_BRANCH"
PUSH_FLAGS=""; [ "$FORCE_TAGS" -eq 1 ] && PUSH_FLAGS="--force"
git push $PUSH_FLAGS origin "refs/tags/$VERSION"
green "   pushed root tag $VERSION — it is now resolvable from the remote"
echo ""

# ═════════════════════════════════════════════════════════════════════════════
# REAL RUN — PHASE 2: with root@VERSION resolvable, finalize each adapter's go.sum,
# commit, tag the adapters, push.
# ═════════════════════════════════════════════════════════════════════════════
bold "── PHASE 2: finalizing adapter go.sums against the pushed root tag ──"
for dir in "${NESTED_MODULES[@]}"; do
  echo "  == $dir =="
  ( cd "$dir"
    # Fetch the real root@VERSION checksum into this adapter's go.sum (NO replace).
    # GOFLAGS=-mod=mod so tidy may write go.mod/go.sum. GOPROXY default first; if the
    # proxy hasn't observed the fresh tag yet, fall back to direct VCS resolution.
    # The retry MUST also set GOSUMDB=off: GOPROXY=direct fetches the tag straight from
    # the VCS, but Go still tries to verify the hash against sum.golang.org, which 404s
    # for a tag the checksum DB hasn't ingested yet — so direct alone still fails. With
    # GOSUMDB=off the hash is computed from the VCS source and written to go.sum as
    # normal. (Same combination the --validate path uses; verified against v0.27.0.)
    if ! GOWORK=off GOFLAGS=-mod=mod go mod tidy 2>/dev/null; then
      yellow "     proxy lag — retrying $dir tidy with GOPROXY=direct GOSUMDB=off"
      GOWORK=off GOFLAGS=-mod=mod GOPROXY=direct GOSUMDB=off go mod tidy
    fi
  )
  # Assert the real hash landed before we tag this adapter.
  if grep -q "^$SDK_PATH $VERSION " "$dir/go.sum"; then
    green "     $dir/go.sum carries $SDK_PATH $VERSION"
  else
    red   "     $dir/go.sum is MISSING the $SDK_PATH $VERSION hash — aborting before tagging $dir"
    exit 1
  fi
done
echo ""

bold "── PHASE 2: committing completed adapter go.mods + go.sums ──"
git add -- "${STAGE_PATHS[@]}" 2>/dev/null || true
if git diff --cached --quiet; then
  yellow "   no go.sum changes to commit (already complete) — using HEAD for adapter tags"
else
  git commit -m "release: $VERSION phase 2 — adapter go.sums resolved against root $VERSION (WS-011 / F039)"
  green "   committed phase 2"
fi
ADAPTER_COMMIT="$(git rev-parse HEAD)"

bold "── PHASE 2: tagging the ${#NESTED_MODULES[@]} adapters on $ADAPTER_COMMIT ──"
for t in "${ADAPTER_TAGS[@]}"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    if [ "$FORCE_TAGS" -eq 1 ]; then
      git tag -f -a "$t" -m "devedge-sdk $t" "$ADAPTER_COMMIT"; yellow "   re-tagged $t (--force-tags)"
    else
      red "   tag $t already exists — use --force-tags or a new version"; exit 1
    fi
  else
    git tag -a "$t" -m "devedge-sdk $t" "$ADAPTER_COMMIT"; green "   tagged $t"
  fi
done

bold "── PHASE 2: pushing the branch + adapter tags ──"
git push origin "$CUR_BRANCH"
for t in "${ADAPTER_TAGS[@]}"; do
  git push $PUSH_FLAGS origin "refs/tags/$t"
  green "   pushed $t"
done
echo ""

# ═════════════════════════════════════════════════════════════════════════════
# REAL RUN — PHASE 2 (housekeeping): re-tidy the testdata fixtures so main CI stays
# green. The adapter `require root` bump pulls these fixtures (which locally replace the
# adapters) up to root@VERSION via MVS, leaving their go.mods stale. They are throwaway
# modules — NOT part of any tagged module — so this lands as a follow-on branch commit
# with NO tag. Local replaces resolve every devedge-sdk module from the working tree,
# so this step needs no proxy and no pushed tag.
# ═════════════════════════════════════════════════════════════════════════════
bold "── PHASE 2 (housekeeping): re-tidying the ${#TESTDATA_FIXTURES[@]} testdata fixtures after the adapter bump ──"
for fix in "${TESTDATA_FIXTURES[@]}"; do
  echo "  == $fix =="
  ( cd "$fix"
    # tidy brings `require root` up to $VERSION and refreshes the indirect closure.
    GOWORK=off GOFLAGS=-mod=mod go mod tidy
  )
  # Gate with `go test` (NOT go build/vet — test deps differ, and this is exactly what
  # main CI runs). `-run '^$'` matches no test, so it BUILDS every test binary without
  # running anything. Default (readonly) mod mode: a still-stale go.mod fails here with
  # "updates to go.mod needed".
  if ( cd "$fix" && GOWORK=off go test -run '^$' ./... >/dev/null 2>&1 ); then
    green "     $fix: GOWORK=off go test -run '^\$' ./... clean"
  else
    red   "     $fix: GOWORK=off go test build FAILED after re-tidy — aborting (output follows)"
    ( cd "$fix" && GOWORK=off go test -run '^$' ./... ) || true
    exit 1
  fi
done

FIXTURE_STAGE=()
for fix in "${TESTDATA_FIXTURES[@]}"; do FIXTURE_STAGE+=("$fix/go.mod" "$fix/go.sum"); done
git add -- "${FIXTURE_STAGE[@]}" 2>/dev/null || true
if git diff --cached --quiet; then
  yellow "   testdata fixtures already tidy — nothing to commit"
else
  git commit -m "release: $VERSION phase 2 — re-tidy testdata fixtures after the adapter bump (WS-011 / F039)"
  git push origin "$CUR_BRANCH"
  green "   committed + pushed the testdata re-tidy"
fi
echo ""
green "release $VERSION COMPLETE: root tag on $ROOT_COMMIT, ${#NESTED_MODULES[@]} adapter tags on $ADAPTER_COMMIT"
echo ""

# ---- post-push verification ------------------------------------------------
bold "── POST-PUSH VERIFICATION — confirm an external consumer resolves each module ──"
yellow "Run these (explicit @$VERSION, NOT @latest — the proxy's @latest view lags a few"
yellow "minutes behind a fresh tag; GONOSUMCHECK is not needed for these public modules):"
echo "    tmp=\$(mktemp -d) && cd \$tmp && go mod init verify && \\"
printf '    GOFLAGS=-mod=mod go get %s@%s' "$SDK_PATH" "$VERSION"
for dir in "${NESTED_MODULES[@]}"; do printf ' \\\n      %s/%s@%s' "$SDK_PATH" "$dir" "$VERSION"; done
echo ""
yellow "If a fetch 404s briefly, it is proxy lag — retry, or prefix with GOPROXY=direct"
yellow "to fetch the tag straight from VCS. \`make release-verify VERSION=$VERSION\` runs this."
