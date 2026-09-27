#!/usr/bin/env bash
# upstream-sync.sh — detect the newest upstream release and publish its tag as a sync
# branch so GitHub computes the merge itself and shows conflicts NATIVELY.
#
# MODEL (why there are no committed conflict markers):
#   The sync branch head == the upstream tag. We do NOT pre-merge. GitHub therefore has
#   to merge <tag> into the stable branch on its own, so its real "conflicts must be
#   resolved" banner + file list appear on the PR. A throwaway trial-merge here only
#   CLASSIFIES clean vs conflicts and builds a file+line manifest for the PR body; it is
#   aborted and never pushed. The reviewer resolves conflicts LOCALLY (consensus is
#   sacred — never GitHub's web editor) and merges, which brings the full upstream
#   history in, authored at merge time.
#
#   Publishing the tag branch includes upstream's .github/workflows changes, so the push
#   REQUIRES a token with `workflow` scope — set secrets.UPSTREAM_SYNC_TOKEN (a PAT).
#
# Reusable across every XDC client fork — only the env vars change per client:
#   go-ethereum : UPSTREAM_URL=https://github.com/ethereum/go-ethereum.git
#   erigon-xdc  : UPSTREAM_URL=https://github.com/erigontech/erigon.git
#   reth        : UPSTREAM_URL=https://github.com/paradigmxyz/reth.git
#   besu        : UPSTREAM_URL=https://github.com/hyperledger/besu.git       (TAG_REGEX ^[0-9]+\.[0-9]+\.[0-9]+$)
#   nethermind  : UPSTREAM_URL=https://github.com/NethermindEth/nethermind.git (TAG_REGEX ^[0-9]+\.[0-9]+\.[0-9]+$)
#
# Env:
#   UPSTREAM_URL   upstream git URL                          (default: ethereum/go-ethereum)
#   STABLE_BRANCH  our stable/sync branch (merge target)     (default: main)
#   TAG_REGEX      ERE for release tags to track             (default: ^v[0-9]+\.[0-9]+\.[0-9]+$)
#   REVIEWER       github handle to assign/review            (default: AnilChinchawale)
#   DRY_RUN        1 = detect + report only, no branch/push  (default: 0)
#   GIT_AUTHOR_NAME/EMAIL  commit identity for the trial     (default: anilchinchawale <anil24593@gmail.com>)
#
# Outputs (appended to $GITHUB_OUTPUT when set, always echoed):
#   latest_tag, sync_branch, status, conflict_files, conflict_manifest
#   status ∈ {uptodate|exists|clean|conflicts|would-sync|error}
set -uo pipefail

UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/ethereum/go-ethereum.git}"
STABLE_BRANCH="${STABLE_BRANCH:-main}"
TAG_REGEX="${TAG_REGEX:-^v[0-9]+\.[0-9]+\.[0-9]+$}"
REVIEWER="${REVIEWER:-AnilChinchawale}"
DRY_RUN="${DRY_RUN:-0}"

emit() { [ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >> "$GITHUB_OUTPUT"; echo ">> $1=$2"; }
die()  { emit status error; echo "ERROR: $*" >&2; exit 1; }

# Author the throwaway trial-merge commit as the maintainer (attributed to AnilChinchawale).
git config user.name  "${GIT_AUTHOR_NAME:-anilchinchawale}"
git config user.email "${GIT_AUTHOR_EMAIL:-anil24593@gmail.com}"

echo "== upstream-sync: $UPSTREAM_URL -> $STABLE_BRANCH (reviewer @$REVIEWER) =="
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream "$UPSTREAM_URL"
git remote set-url upstream "$UPSTREAM_URL"
# Full history + tags (GitHub needs the upstream commit history to compute a real merge).
git fetch --quiet --tags upstream || die "cannot fetch upstream"
git fetch --quiet origin "$STABLE_BRANCH" || die "cannot fetch origin/$STABLE_BRANCH"

# Newest upstream STABLE release tag. Cap the major to 1-3 digits so stray CalVer tags
# (e.g. erigon's v20201.01.02) can't out-sort the real semver line under `sort -V`.
LATEST=$(git ls-remote --tags --refs upstream | awk -F/ '{print $NF}' \
          | grep -E "$TAG_REGEX" | grep -Ev '^v?[0-9]{4,}\.' | sort -V | tail -1)
[ -n "$LATEST" ] || die "no upstream release tag matched /$TAG_REGEX/"
emit latest_tag "$LATEST"
git fetch --quiet upstream "refs/tags/$LATEST:refs/tags/$LATEST" 2>/dev/null || true

# Already merged? (tag is an ancestor of our stable line)
if git merge-base --is-ancestor "$LATEST" "origin/$STABLE_BRANCH" 2>/dev/null; then
  emit status uptodate; echo "$LATEST already in $STABLE_BRANCH — nothing to do"; exit 0
fi

SYNC_BRANCH="upstream-sync/$LATEST"
emit sync_branch "$SYNC_BRANCH"
if git ls-remote --exit-code --heads origin "$SYNC_BRANCH" >/dev/null 2>&1; then
  emit status exists; echo "$SYNC_BRANCH already open — nothing to do"; exit 0
fi

if [ "$DRY_RUN" = "1" ]; then emit status would-sync; echo "DRY_RUN: would sync $LATEST"; exit 0; fi

# --- Classify clean vs conflicts with a THROWAWAY trial merge (aborted, never pushed) ---
git checkout -q -B _sync_trial "origin/$STABLE_BRANCH" || die "cannot start trial merge"
STATUS=clean; CF=""; MANIFEST=""
if git merge --no-commit --no-ff "$LATEST" >/dev/null 2>&1; then
  STATUS=clean
else
  STATUS=conflicts
  # Every unmerged path is a real conflict the reviewer resolves (incl. .github/workflows).
  FILES=$(git diff --name-only --diff-filter=U 2>/dev/null || true)
  CF=$(printf '%s' "$FILES" | tr '\n' ',' | sed 's/,$//')
  # Per-file marker line numbers. No backticks/metacharacters — this text is injected via
  # an env var into the PR body, so keep it shell-safe. Use while-read (not `for f in
  # $FILES`) so it word-splits correctly under both bash and zsh.
  MANIFEST=$(printf '%s\n' "$FILES" | while IFS= read -r f; do
    [ -n "$f" ] && [ -f "$f" ] || continue
    LNS=$(grep -nE '^<<<<<<< ' "$f" 2>/dev/null | cut -d: -f1 | tr '\n' ',' | sed 's/,$//')
    if [ -n "$LNS" ]; then echo "- $f (L$LNS)"; else echo "- $f"; fi
  done)
fi
git merge --abort 2>/dev/null || true
git checkout -q "origin/$STABLE_BRANCH" 2>/dev/null || true
git branch -q -D _sync_trial 2>/dev/null || true

# --- Publish the upstream tag AS the sync branch so GitHub computes the merge itself ---
git branch -f "$SYNC_BRANCH" "refs/tags/$LATEST"

emit status "$STATUS"
emit conflict_files "$CF"
if [ -n "$MANIFEST" ]; then
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    { echo "conflict_manifest<<__MANI__"; echo "$MANIFEST"; echo "__MANI__"; } >> "$GITHUB_OUTPUT"
  fi
  echo ">> conflict_manifest:"; echo "$MANIFEST"
fi
echo "prepared $SYNC_BRANCH @ $LATEST (status=$STATUS) — GitHub shows conflicts natively; reviewer resolves locally + merges"
