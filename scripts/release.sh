#!/usr/bin/env bash
# release.sh — the SYNCHRONIZED multi-module release for devedge-sdk (WS-011 / F039).
#
# The repo is one git repo holding SEVEN Go modules (the root LIBRARY + six nested
# ones: cmd, config/koanf, events/kafkabus, observability/otel, persistence/gormtx,
# persistence/entrepo). Every release tags ALL of them at the SAME version so a
# consumer pins one version and the cross-module `require`s resolve coherently.
#
# Usage:
#   scripts/release.sh vX.Y.Z            # DRY RUN: bump requires + version vars,
#                                        # print the exact tag/push plan, tag NOTHING,
#                                        # leave the bumps STAGED for review.
#   scripts/release.sh vX.Y.Z --push     # the real thing: bump, commit, create all
#                                        # SEVEN tags, push the branch + tags.
#
# WHAT IT DOES (the ordering is load-bearing — see the block comments):
#   1. Bump each of the six non-root modules' `require github.com/infobloxopen/
#      devedge-sdk` from the pre-release placeholder to vX.Y.Z.
#   2. Bump the scaffold version vars (fallbackSDKVersion — the single source the
#      adapter-version resolvers all derive from) to vX.Y.Z.
#   3. `go mod tidy` EACH module individually (NEVER `go work sync` — see the note).
#   4. (--push only) commit the bumps, then create tags: root vX.Y.Z + each submodule
#      at <path>/vX.Y.Z, all on the one commit, then push branch + tags.
#
# WHY ALL TAGS SIT ON ONE COMMIT
#   Go resolves `require github.com/infobloxopen/devedge-sdk vX.Y.Z` to the git tag
#   `vX.Y.Z`, and `…/observability/otel vX.Y.Z` to the tag `observability/otel/vX.Y.Z`.
#   A tag is just a pointer to a commit; nothing requires the root tag to be on an
#   EARLIER commit than the adapter tags. After this commit, the adapter go.mods carry
#   the real `require …devedge-sdk vX.Y.Z` (no replace); that require resolves the
#   instant the root `vX.Y.Z` tag exists on the same commit. So all seven tags on the
#   single release commit form a coherent, self-consistent set: every cross-module
#   require points at a tag that exists and contains the shed root. (The spec's
#   "tag-root-first" ordering is about not PUBLISHING an adapter that requires an
#   UNpublished root version — here we push all tags together, which is strictly
#   safe: the root tag is present before any consumer can fetch an adapter tag.)
#
# EXTERNAL-RESOLUTION + PROXY-LAG CAVEAT
#   Until the tags are PUSHED, a `GOWORK=off go build` in an adapter module fails with
#   "missing go.sum entry … to verify package … is provided by exactly one module":
#   the committed go.mod requires vX.Y.Z but that tag does not yet exist on the remote
#   / proxy, so the root cannot be downloaded. This is EXPECTED and is exactly why the
#   dry run stops before tagging. Local builds keep working via go.work (it resolves
#   the root require to the working tree). After `--push`, the Go module proxy
#   (proxy.golang.org) may take a few minutes to observe the new tags; a `go get
#   …@vX.Y.Z` that 404s briefly is proxy lag, not a release defect — retry, or set
#   GOPROXY=direct for an immediate VCS fetch.
#
# RE-RUNNABLE / IDEMPOTENT
#   Re-running for the same vX.Y.Z is safe: the require/version-var edits converge to
#   the same value, and tag creation is guarded (refuses to clobber an existing tag
#   unless --force-tags). The script refuses to run on a dirty tree EXCEPT for the
#   bumps it itself produces (so a dry run → review → `--push` of the SAME version
#   works without a stash dance).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SDK_PATH="github.com/infobloxopen/devedge-sdk"

# The six nested modules, as <dir>:<module-suffix>. The suffix is appended to the
# version to form the tag (Go's nested-module tag convention). The root is tagged
# bare (vX.Y.Z). Keep this list in sync with go.work `use` and the Makefile MODULES.
NESTED_MODULES=(
  "cmd"
  "config/koanf"
  "events/kafkabus"
  "observability/otel"
  "persistence/gormtx"
  "persistence/entrepo"
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
usage: $0 vX.Y.Z [--push] [--force-tags]

  vX.Y.Z         release version (must be a valid semver tag, e.g. v0.27.0).
  --push         create the tags and push branch + tags (default: DRY RUN — print
                 the plan, stage the bumps, tag/push NOTHING).
  --force-tags   allow re-creating tags that already exist (re-release of a version).
EOF
  exit 2
}

# ---- argument parsing -------------------------------------------------------
VERSION=""
PUSH=0
FORCE_TAGS=0
for arg in "$@"; do
  case "$arg" in
    --push)        PUSH=1 ;;
    --force-tags)  FORCE_TAGS=1 ;;
    -h|--help)     usage ;;
    v*)            [ -z "$VERSION" ] && VERSION="$arg" || usage ;;
    *)             red "unknown argument: $arg"; usage ;;
  esac
done
[ -n "$VERSION" ] || usage

# Validate semver-ish vX.Y.Z (allow a -prerelease suffix for pre-GA, e.g. v0.27.0-rc1).
if ! printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  red "invalid version '$VERSION' — expected vMAJOR.MINOR.PATCH (optionally -prerelease)"
  exit 2
fi

if [ "$PUSH" -eq 1 ]; then
  bold "== devedge-sdk synchronized release $VERSION (REAL RUN: will commit, tag, push) =="
else
  bold "== devedge-sdk synchronized release $VERSION (DRY RUN: prints the plan, stages bumps, tags/pushes NOTHING) =="
fi
echo ""

# ---- working-tree safety ----------------------------------------------------
# Refuse if the tree is dirty in files the release does NOT touch. The files the
# release legitimately mutates are the six adapter go.mod/go.sum and model.go — a
# prior dry run for the SAME version leaves exactly those staged, so allow them.
ALLOWED_DIRTY_RE='^(cmd/go\.(mod|sum)|config/koanf/go\.(mod|sum)|events/kafkabus/go\.(mod|sum)|observability/otel/go\.(mod|sum)|persistence/gormtx/go\.(mod|sum)|persistence/entrepo/go\.(mod|sum)|cmd/devedge-sdk/internal/scaffold/model\.go)$'
unexpected="$(git status --porcelain | awk '{print $2}' | grep -Ev "$ALLOWED_DIRTY_RE" || true)"
if [ -n "$unexpected" ]; then
  red "working tree has unexpected changes (commit/stash them first):"
  printf '%s\n' "$unexpected" | sed 's/^/    /'
  exit 1
fi

# ---- 1 + 2 + 3: bump requires, bump version vars, tidy each module ----------
# The require-bump ordering footgun: the adapters' placeholder require points at a
# PUBLISHED root version (v0.26.1) that PRE-DATES the carve-out and still CONTAINS the
# adapter subtree, so `go mod tidy` there hits an "ambiguous import" (the package is
# in both the adapter module AND old-root@v0.26.1). After bumping to vX.Y.Z (the SHED
# root) the ambiguity is gone — but vX.Y.Z is not tagged yet, so a bare tidy 404s
# ("unknown revision"). We bridge that with a TEMPORARY local replace pointing
# devedge-sdk@vX.Y.Z at the working-tree root (which IS the shed root) for the
# duration of the tidy, then drop it. The committed go.mod ends up with the clean
# real require and NO replace; it resolves for real once the vX.Y.Z tag is pushed.
#
# NEVER `go work sync` here: in this nested layout it rewrites/empties the member
# require blocks (a known footgun that bit P2). We tidy each module INDIVIDUALLY.

# rel_to_root <module-dir> — relative path from the module dir up to the repo root.
rel_to_root() {
  local depth; depth="$(printf '%s' "$1" | tr -cd '/' | wc -c | tr -d ' ')"
  local up=".."; local i; for ((i=0; i<depth; i++)); do up="$up/.."; done
  printf '%s' "$up"
}

bold "-- bumping the scaffold version source ($MODEL_GO: fallbackSDKVersion → $VERSION) --"
# Single source of truth: SDKVersion / OTelAdapterVersion / GormtxVersion /
# EntrepoVersion all derive from resolveSDKVersion(), which falls back to this const.
if grep -q "const fallbackSDKVersion = \"$VERSION\"" "$MODEL_GO"; then
  yellow "   already $VERSION (idempotent no-op)"
else
  # macOS/BSD + GNU sed compatible in-place edit via a temp file.
  perl -i -pe "s/(const fallbackSDKVersion = )\"v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?\"/\${1}\"$VERSION\"/" "$MODEL_GO"
  green "   set fallbackSDKVersion = \"$VERSION\""
fi
echo ""

bold "-- bumping + tidying the six nested modules (require $SDK_PATH → $VERSION) --"
for dir in "${NESTED_MODULES[@]}"; do
  echo "  == $dir =="
  ( cd "$dir"
    up="$(rel_to_root "$dir")"
    # (a) set the real require to vX.Y.Z (replacing the placeholder).
    go mod edit -require="$SDK_PATH@$VERSION"
    # (b) temporary local replace so tidy resolves the (unpublished) shed root locally.
    go mod edit -replace="$SDK_PATH@$VERSION=$up"
    # (c) tidy with the workspace OFF (the published require graph, not go.work).
    GOWORK=off go mod tidy
    # (d) drop the temporary replace — the committed go.mod keeps only the real require.
    go mod edit -dropreplace="$SDK_PATH@$VERSION"
  )
  green "     require $SDK_PATH $VERSION  (tidied, replace dropped)"
done
echo ""

# Stage exactly the release-owned files (the version source + each module's
# go.mod/go.sum) so the diff shown — and, on --push, the commit — is precisely the
# release bumps and nothing else.
bold "-- staged release bumps (git diff --stat) --"
STAGE_PATHS=("$MODEL_GO")
for dir in "${NESTED_MODULES[@]}"; do
  STAGE_PATHS+=("$dir/go.mod" "$dir/go.sum")
done
git add -- "${STAGE_PATHS[@]}"
git --no-pager diff --cached --stat
echo ""

# ---- the tag plan -----------------------------------------------------------
# Root tagged bare; each nested module at <suffix>/vX.Y.Z. All on ONE commit.
PLANNED_TAGS=("$VERSION")
for dir in "${NESTED_MODULES[@]}"; do
  PLANNED_TAGS+=("$dir/$VERSION")
done

bold "-- tag plan (all on the single release commit) --"
for t in "${PLANNED_TAGS[@]}"; do echo "    git tag $t"; done
echo ""
CUR_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
bold "-- push plan --"
echo "    git push origin $CUR_BRANCH"
for t in "${PLANNED_TAGS[@]}"; do echo "    git push origin refs/tags/$t"; done
echo ""

# ---- dry-run stop -----------------------------------------------------------
if [ "$PUSH" -ne 1 ]; then
  yellow "DRY RUN — no commit, no tags, no push. The require + version-var bumps above are STAGED."
  yellow "Review them, then re-run with --push to commit, tag all seven modules, and push:"
  yellow "    scripts/release.sh $VERSION --push"
  exit 0
fi

# ---- 4: commit, tag, push (real run only) -----------------------------------
bold "-- committing the release bumps --"
if git diff --cached --quiet; then
  yellow "   nothing to commit (bumps already committed) — proceeding to tag"
else
  git commit -m "release: synchronize all modules at $VERSION (WS-011 / F039)"
  green "   committed"
fi
RELEASE_COMMIT="$(git rev-parse HEAD)"
echo ""

bold "-- creating tags on $RELEASE_COMMIT --"
for t in "${PLANNED_TAGS[@]}"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    if [ "$FORCE_TAGS" -eq 1 ]; then
      git tag -f -a "$t" -m "devedge-sdk $t" "$RELEASE_COMMIT"
      yellow "   re-tagged $t (--force-tags)"
    else
      red "   tag $t already exists — re-run with --force-tags to move it, or pick a new version"
      exit 1
    fi
  else
    git tag -a "$t" -m "devedge-sdk $t" "$RELEASE_COMMIT"
    green "   tagged $t"
  fi
done
echo ""

bold "-- pushing branch + tags --"
git push origin "$CUR_BRANCH"
PUSH_FLAGS=""
[ "$FORCE_TAGS" -eq 1 ] && PUSH_FLAGS="--force"
for t in "${PLANNED_TAGS[@]}"; do
  git push $PUSH_FLAGS origin "refs/tags/$t"
  green "   pushed $t"
done
echo ""
green "release $VERSION pushed: root + ${#NESTED_MODULES[@]} submodule tags on $RELEASE_COMMIT"
yellow "NOTE: the module proxy may take a few minutes to observe the new tags. If a"
yellow "      'go get …@$VERSION' 404s briefly, that is proxy lag — retry or use GOPROXY=direct."
yellow "POST-RELEASE SMOKE (recommended): for each module, GOPROXY=direct go get \$mod@$VERSION"
