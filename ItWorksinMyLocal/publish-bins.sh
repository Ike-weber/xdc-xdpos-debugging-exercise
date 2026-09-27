#!/usr/bin/env bash
# publish-bins.sh -- build, checksum, and publish this repo's six prebuilt
# client binaries (geth, xone, erigon, reth, besu, nethermind) to the
# xdc.network snapshot host, then update binaries.json so lib.sh's
# download_bins() picks them up. oldxdc/XDPoSChain is EXCLUDED on purpose:
# it has no prebuilt binary and lib.sh always source-builds it (see
# ensure_bins()'s `[ "$CLIENT" = oldxdc ] && src=build`), so publishing one
# would be dead weight no consumer ever downloads.
#
# OS-agnostic note: this script targets both macOS (bash 3.2 -- the system
# /bin/bash on every Mac, NOT upgraded by default) and Linux. That rules out
# associative arrays, the bash-4 array-reading builtins, and the bash-4
# lower/upper-casing parameter expansions -- see platform_slug() below for the
# `tr`-based replacement. (Those constructs are deliberately not spelled out
# literally here: check-portability.sh greps comments as well as code, so
# naming them would fail the very lint this note is about.) Darwin
# **fat (lipo) binaries can only be produced on an actual macOS host** (lipo,
# and Apple's clang -arch cross flag, don't exist anywhere else); Linux
# slices fall back to Docker (`docker run --platform linux/<arch>`) when the
# host can't build them natively or cross-compile them, using pinned images
# (see the CONFIG block below).
#
# Phases: preflight -> plan -> build -> checksum -> publish -> remote-verify
#         -> manifest -> summary. A work item is a (client, platform-slug)
# pair; every item lives in one of: PLANNED, BUILT, PUBLISHED, VERIFIED,
# SKIPPED(reason), FAILED(phase). Only VERIFIED items are written into
# binaries.json. Progress is persisted to a flat `state.tsv` ledger in the
# staging directory so `--publish-only` can resume a run that already built
# artifacts (e.g. on a different, more capable host) without rebuilding them.
#
# Usage:
#   ./publish-bins.sh --dry-run
#   ./publish-bins.sh --build-only --clients geth,xone
#   ./publish-bins.sh --clients erigon --platforms linux-amd64 --yes
#   ./publish-bins.sh --publish-only --staging dist/publish
#
# Flags:
#   --clients LIST        comma-separated client list (default: all six
#                          publishable clients -- geth,xone,erigon,reth,besu,
#                          nethermind). "oldxdc" is a HARD usage error: it has
#                          no prebuilt binary and is always source-built by
#                          consumers (lib.sh forces BIN_SOURCE=build for it).
#   --platforms LIST       comma-separated {os}-{arch} slugs (darwin-amd64,
#                          darwin-arm64, linux-amd64, linux-arm64), intersected
#                          per client against what that client actually
#                          supports (see the header "Clients" facts). besu is
#                          platform-independent and ignores this flag.
#                          nethermind never gets linux-arm64 -- dropped with a
#                          warning even if explicitly requested.
#   --build-only           stop after checksum + the sanity gate: no SSH, no
#                          binaries.json update.
#   --publish-only         skip the build phase; publish whatever is already
#                          staged (from state.tsv + stage/) and passes the
#                          sanity gate. Mutually exclusive with --build-only.
#   --dry-run              plan + print what would happen; no clone/build/
#                          docker/ssh/rsync/manifest writes.
#   --force-rebuild        ignore the state.tsv build cache; rebuild every
#                          requested item even if already BUILT for the same
#                          commit.
#   --no-docker            disable the Docker fallback path entirely; items
#                          that need it are SKIPPED instead.
#   --strict               promote any SKIPPED/FAILED item (or a plan-time
#                          platform-drop warning) to a nonzero exit, even in
#                          --dry-run.
#   --staging DIR          staging root (default <repo>/dist/publish).
#   --src-root DIR         where per-client checkouts go (default
#                          <staging>/src).
#   --ref CLIENT=REF       pin one client's checkout to REF (branch, tag, or
#                          commit); repeatable, one per client.
#   --ssh-key PATH         same as PUBLISH_SSH_KEY (overrides it).
#   --verify-public        after activating, curl the public URL and compare
#                          its sha256 against what we just published. Off by
#                          default (CDN propagation lag makes this noisy);
#                          a mismatch is a warning, never fails the item.
#   --no-manifest          publish but leave binaries.json untouched.
#   --yes                  skip the confirmation prompt shown before the
#                          first remote write (which lists exactly what/where).
#   -h, --help             print this help and exit.
#
# Hard aborts (before any side effect -- clone, build, docker, mkdir, ssh):
#   unknown flag; --clients oldxdc; missing python3/git/curl/rsync;
#   --build-only + --publish-only together; a non-build-only mode with
#   PUBLISH_DIR unset; the staging directory isn't writable; the SSH
#   preflight (reachability, PUBLISH_DIR writable, remote sha256sum present)
#   fails. Exit code 1.
#
# Per-item soft failures (that one item is marked FAILED/SKIPPED and
# excluded from the manifest; every other item keeps going): clone, build,
# sanity gate, upload, remote verify. A darwin-universal build failure fails
# BOTH darwin slugs for that client (they're the same lipo binary).
#
# Exit codes: 0 = every requested item VERIFIED, or a clean --dry-run;
#             1 = hard abort (see above);
#             2 = completed, but with skips/failures (binaries.json is still
#                 updated for whichever subset DID verify). --strict makes
#                 skips/failures (or dropped-platform warnings) nonzero even
#                 under --dry-run.
#
# ---------------------------------------------------------------------------
# OPEN QUESTIONS / TODO (do not silently resolve these -- ask a human):
#
#   1. PUBLISH_DIR is genuinely unknown to this script and has no default --
#      it MUST be set in publish.env (see publish.env.sample) before any
#      publish (non---build-only) run. This is intentional: guessing a
#      remote web root wrong would mean silently writing files nobody serves,
#      or worse, into whatever unrelated directory happens to exist.
#
#   2. besu's bundled-JRE platform story under the manifest's "*" (platform-
#      independent) key is unresolved: a real Temurin JRE is native per OS/
#      arch, so a single besu-latest.tar.gz genuinely can't be "*" once you
#      look inside jre/. This script builds the JVM artifact once (which
#      really is platform-independent) and bundles a linux-x64 JRE as the
#      lowest common denominator; see the long comment in build_besu() below.
#      Splitting this into per-platform besu-{os}-{arch}.tar.gz archives is
#      the likely real fix, but that's a manifest-shape change and is left
#      for a human to decide, not silently done here.
#
#   3. RESOLVED 2026-07-30: nethermind's canonical source repo is
#      XDCIndia/xdc-nethermind-private, and client_repo() now points there.
#      The public XDCIndia/nethermind this used to default to DOES NOT EXIST
#      (404 against the org), so nethermind could never be built by this
#      script at all -- which is exactly why its published artifact sat seven
#      days behind xdc-nethermind-private#253. binaries.json's own
#      nethermind._comment was right. Resolving this also turned up the same
#      class of breakage for two more clients: reth pointed at the
#      non-existent XDCIndia/reth-xdc (real: XDCIndia/reth), and besu pointed
#      at XDCIndia/besu-xdc, a fork last pushed 2026-03-24 that does not
#      contain the shipped build's commit 9d64f86 (real: XDCIndia/besu).
#      See the comment above client_repo() for the evidence per client.
# ---------------------------------------------------------------------------

# shellcheck disable=SC2029  # file-wide: every `$(shq VALUE)` used to build an
# ssh remote-command string below is intentionally expanded client-side --
# shq() pre-quotes VALUE for the REMOTE shell, so local expansion IS the
# point. The classic SC2029 footgun this rule warns about is an *unquoted*
# $VAR spliced directly into a remote command string, which this file never
# does. (This directive must precede the first statement to apply file-wide.)
set -euo pipefail
IFS=$' \t\n'

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$SELF_DIR"
BINARIES_JSON="$REPO_ROOT/binaries.json"

# ---------------------------------------------------------------------------
# CONFIG: pinned Docker builder images (Docker is only ever a *fallback* --
# host toolchains are always preferred; see resolve/run_step()). Bump these
# deliberately, as their own change, not as a drive-by in an unrelated diff.
# ---------------------------------------------------------------------------
IMG_GOLANG="golang:1.23-bookworm"                   # geth, xone, erigon (linux slices)
IMG_RUST="rust:1.82-bookworm"                       # reth (linux slices)
IMG_TEMURIN21="eclipse-temurin:21-jdk"              # besu (build + bundled JRE source)
IMG_DOTNET_SDK="mcr.microsoft.com/dotnet/sdk:8.0"   # nethermind

ALL_CLIENTS="geth xone erigon reth besu nethermind"
DEFAULT_CLIENTS="geth,xone,erigon,reth,besu,nethermind"

# ---------------------------------------------------------------------------
# Config (env vars). Mirrors the repo's network.env pattern: a gitignored
# publish.env at the repo root is sourced if present, then every PUBLISH_*
# var gets its documented default via ${VAR:-default} (so publish.env only
# needs to set what it wants to override -- see publish.env.sample).
# ---------------------------------------------------------------------------
# shellcheck disable=SC1091  # publish.env is a local, gitignored, optional file -- nothing to lint in this checkout
[ -f "$REPO_ROOT/publish.env" ] && . "$REPO_ROOT/publish.env"
PUBLISH_HOST="${PUBLISH_HOST:-95.217.56.168}"
PUBLISH_PORT="${PUBLISH_PORT:-12141}"
PUBLISH_USER="${PUBLISH_USER:-root}"
PUBLISH_DIR="${PUBLISH_DIR:-}"
PUBLISH_SSH_KEY="${PUBLISH_SSH_KEY:-}"
PUBLISH_BASE_URL="${PUBLISH_BASE_URL:-https://xdc.network/snapshots/bin/}"

# ---------------------------------------------------------------------------
# Two tiny helpers mirroring lib.sh's sha256_file()/platform_slug() verbatim.
# Copied inline (rather than `source ./lib.sh`) because lib.sh has real side
# effects on load -- it sets CLIENT/BIN_DIR/XDPOS_* from $CLIENT and `exit 1`s
# on an unrecognized one -- none of which this script wants or needs. Keep
# these two in sync with lib.sh by hand if lib.sh's versions ever change.
# ---------------------------------------------------------------------------
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "no sha256sum/shasum found -- cannot verify downloads" >&2
    return 1
  fi
}

platform_slug() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *)      os=$(uname -s | tr '[:upper:]' '[:lower:]') ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *)             arch=$(uname -m) ;;
  esac
  echo "${os}-${arch}"
}

# ---------------------------------------------------------------------------
# Client facts (hard-coded; these are NOT read from binaries.json -- this
# script decides what to build, binaries.json only records the result).
# ---------------------------------------------------------------------------
# Verified 2026-07-30 by resolving every URL below against the XDCIndia org:
# three of the six pointed at repositories that do not exist (or, for besu, a
# stale fork), so `publish-bins.sh` could not fetch a source for reth, besu or
# nethermind AT ALL -- which is why their published artifacts had drifted days
# to months behind their merged fixes while geth/xone/erigon stayed current.
#
#   reth        reth-xdc  -> reth                       (reth-xdc: 404; reth has crates/xdc)
#   besu        besu-xdc  -> besu                       (besu-xdc last pushed 2026-03-24 and
#                                                        does NOT contain the shipped build's
#                                                        commit 9d64f86; XDCIndia/besu does)
#   nethermind  nethermind -> xdc-nethermind-private    (nethermind: 404; the private repo is
#                                                        what the shipped dist's stack traces
#                                                        already name, and carries merged #253)
#
# Symptom this fixes, concretely: the published nethermind-linux-amd64.tar.gz
# (sha256 876335b9..., matching binaries.json) was built 2026-07-22, seven days
# before xdc-nethermind-private#253 landed, so it still activates TIPSigning at
# block 0, deletes the BlockSigners contract 0x89 during genesis construction and
# cannot start on any custom chainId. There was no way to rebuild it through this
# script, because the repo it tried to clone is not there.
client_repo() {
  case "$1" in
    geth)  echo 'https://github.com/XDCIndia/go-ethereum' ;;
    xone)  echo 'https://github.com/XDCIndia/xOneGo' ;;
    erigon) echo 'https://github.com/XDCIndia/erigon-xdc' ;;
    reth)  echo 'https://github.com/XDCIndia/reth' ;;
    besu)  echo 'https://github.com/XDCIndia/besu' ;;
    nethermind) echo 'https://github.com/XDCIndia/xdc-nethermind-private' ;;
    *) echo "" ;;
  esac
}

client_allowed_platforms() {
  case "$1" in
    geth|reth)    echo 'darwin-amd64 darwin-arm64 linux-amd64 linux-arm64' ;;
    xone|erigon)  echo 'linux-amd64 darwin-amd64 darwin-arm64' ;;
    besu)         echo '*' ;;
    nethermind)   echo 'darwin-amd64 darwin-arm64 linux-amd64' ;;
    *)            echo '' ;;
  esac
}

client_file_pattern() {
  case "$1" in
    geth)   echo 'geth-{os}-{arch}' ;;
    xone)   echo 'xone-{os}-{arch}' ;;
    erigon) echo 'erigon-{os}-{arch}' ;;
    reth)   echo 'reth-{os}-{arch}' ;;
    besu)   echo 'besu-latest.tar.gz' ;;
    nethermind) echo 'nethermind-{os}-{arch}.tar.gz' ;;
    *) echo "" ;;
  esac
}

# The manifest "produces" key -- also the produced binary's own filename
# inside a checkout's build output (used to locate it after `make`/`cargo`).
# Where `make <target>` leaves its binary, relative to the checkout root.
# geth/erigon use go-ethereum's build/bin/<target>. xOneGo does not: its Go module
# lives in pkg/, and its root Makefile (added in xOneGo#710 so `make xone` works
# from the root at all) copies output to ./bin/.
#
# xone also needs a DIFFERENT binary than its make target. `make xone` builds two:
#   xone         - a launcher that only parses --engine and execs an engine binary
#   xone-native  - the actual standalone client (the default engine)
# The published artifact is a single bare file, so shipping the launcher would give
# users something that starts and then immediately fails for want of an engine.
# Ship xone-native under the published name instead; that is also what the working
# deployed binary has always been (monolithic, no launcher).
client_build_out() {
  case "$1" in
    xone) echo "bin/xone-native" ;;
    *)    echo "build/bin/$2" ;;
  esac
}

client_binary_name() {
  case "$1" in
    geth) echo geth ;;
    xone) echo xone-native ;;
    erigon) echo erigon ;;
    reth) echo xdc-reth ;;
    besu) echo besu ;;
    nethermind) echo nethermind ;;
    *) echo "" ;;
  esac
}

# resolved_filename CLIENT SLUG -> the exact publish-time filename (this is
# what lands in stage/ and, unchanged, at $PUBLISH_DIR/<this>).
resolved_filename() {
  local client="$1" slug="$2" pattern os_ arch_
  pattern="$(client_file_pattern "$client")"
  case "$client" in
    besu) echo "$pattern"; return 0 ;;
  esac
  os_="${slug%-*}"; arch_="${slug#*-}"
  pattern="${pattern//\{os\}/$os_}"
  pattern="${pattern//\{arch\}/$arch_}"
  echo "$pattern"
}

# ---------------------------------------------------------------------------
# CLI parsing
# ---------------------------------------------------------------------------
CLIENTS_ARG=""
PLATFORMS_ARG=""
MODE="full"          # full | build-only | publish-only
DRY_RUN=0
FORCE_REBUILD=0
NO_DOCKER=0
STRICT=0
STAGING_ARG=""
SRC_ROOT_ARG=""
REF_ENTRIES=()
SSH_KEY_ARG=""
VERIFY_PUBLIC=0
NO_MANIFEST=0
ASSUME_YES=0

hard_abort() {
  echo "ABORT: $*" >&2
  exit 1
}

# Classify a binary by its MAGIC BYTES: elf | macho-universal | macho | unknown.
#
# This used to call `file -b`, which is not in coreutils and is absent from plenty
# of build hosts -- including the one this script normally runs on. When `file` is
# missing the command substitution yields an EMPTY string, so the sanity gate
# reported "expected an ELF binary, got: " and rejected a perfectly good binary.
# That is exactly how the erigon artifact silently stopped being republishable: a
# valid 155MB ELF was built and then thrown away by the gate, every run.
#
# `od` is POSIX and always present, so read the first four bytes ourselves:
#   ELF                  7f 45 4c 46   (\177ELF)
#   Mach-O universal/fat  ca fe ba be  (big-endian) or be ba fe ca (byte-swapped)
#   Mach-O thin          cf fa ed fe / ce fa ed fe (64/32-bit little-endian)
# A fat binary is what `lipo` produces and is what the darwin artifacts must be,
# so thin Mach-O is reported separately rather than being conflated with it.
binary_magic() {
  local f="$1" m
  [ -r "$f" ] || { echo "unreadable"; return 0; }
  m=$(od -An -tx1 -N4 "$f" 2>/dev/null | tr -d ' \n')
  case "$m" in
    7f454c46)          echo elf ;;
    cafebabe|bebafeca) echo macho-universal ;;
    cffaedfe|cefaedfe|feedface|feedfacf) echo macho ;;
    "")                echo "unreadable" ;;
    *)                 echo "unknown(magic=$m)" ;;
  esac
}

print_usage() {
  sed -n '2,94p' "$SELF_DIR/publish-bins.sh" | sed 's/^# \{0,1\}//'
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --clients)       [ $# -ge 2 ] || hard_abort "--clients needs a value"; CLIENTS_ARG="$2"; shift 2 ;;
      --platforms)      [ $# -ge 2 ] || hard_abort "--platforms needs a value"; PLATFORMS_ARG="$2"; shift 2 ;;
      --build-only)
        [ "$MODE" = publish-only ] && hard_abort "--build-only and --publish-only are mutually exclusive"
        MODE=build-only; shift ;;
      --publish-only)
        [ "$MODE" = build-only ] && hard_abort "--build-only and --publish-only are mutually exclusive"
        MODE=publish-only; shift ;;
      --dry-run)        DRY_RUN=1; shift ;;
      --force-rebuild)  FORCE_REBUILD=1; shift ;;
      --no-docker)      NO_DOCKER=1; shift ;;
      --strict)         STRICT=1; shift ;;
      --staging)        [ $# -ge 2 ] || hard_abort "--staging needs a value"; STAGING_ARG="$2"; shift 2 ;;
      --src-root)       [ $# -ge 2 ] || hard_abort "--src-root needs a value"; SRC_ROOT_ARG="$2"; shift 2 ;;
      --ref)
        [ $# -ge 2 ] || hard_abort "--ref needs CLIENT=REF"
        case "$2" in
          *=*) REF_ENTRIES+=("$2") ;;
          *) hard_abort "--ref needs CLIENT=REF (got '$2')" ;;
        esac
        shift 2 ;;
      --ssh-key)        [ $# -ge 2 ] || hard_abort "--ssh-key needs a value"; SSH_KEY_ARG="$2"; shift 2 ;;
      --verify-public)  VERIFY_PUBLIC=1; shift ;;
      --no-manifest)    NO_MANIFEST=1; shift ;;
      --yes)            ASSUME_YES=1; shift ;;
      -h|--help)        print_usage; exit 0 ;;
      *) hard_abort "unknown flag: $1 (see --help)" ;;
    esac
  done
}

get_ref() {
  local client="$1" e
  for e in "${REF_ENTRIES[@]-}"; do
    [ -n "$e" ] || continue
    case "$e" in
      "$client"=*) echo "${e#*=}"; return 0 ;;
    esac
  done
  echo ""
}

validate_clients_arg() {
  local requested c
  requested="${CLIENTS_ARG:-$DEFAULT_CLIENTS}"
  for c in $(echo "$requested" | tr ',' ' '); do
    if [ "$c" = oldxdc ]; then
      hard_abort "--clients oldxdc: oldxdc has no prebuilt binary and is always source-built by consumers (lib.sh forces BIN_SOURCE=build for it, since ensure_bins() has no manifest entry to download for oldxdc) -- it is intentionally excluded from publish-bins.sh."
    fi
    case " $ALL_CLIENTS " in
      *" $c "*) : ;;
      *) hard_abort "unknown client '$c' (known: $ALL_CLIENTS)" ;;
    esac
  done
}

check_required_tools() {
  local t missing=""
  for t in git python3 curl rsync; do
    command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
  done
  [ -z "$missing" ] || hard_abort "missing required tool(s):$missing"
}

# ---------------------------------------------------------------------------
# Staging layout + the state.tsv ledger (client, slug, status, sha256,
# commit, where, reason -- tab-separated; "reason" is an extra trailing
# column beyond the 6 the design calls out, needed for the summary table and
# harmless to anyone treating this as a plain 6-column TSV with `cut`).
# ---------------------------------------------------------------------------
setup_staging() {
  STAGING="${STAGING_ARG:-$REPO_ROOT/dist/publish}"
  SRC_ROOT="${SRC_ROOT_ARG:-$STAGING/src}"
  STAGE_DIR="$STAGING/stage"
  LOG_DIR="$STAGING/logs"
  STATE_FILE="$STAGING/state.tsv"
  mkdir -p "$STAGING" "$SRC_ROOT" "$STAGE_DIR" "$LOG_DIR" 2>/dev/null \
    || hard_abort "cannot create staging directory tree under $STAGING"
  if : > "$STAGING/.writetest.$$" 2>/dev/null; then
    rm -f "$STAGING/.writetest.$$"
  else
    hard_abort "staging directory not writable: $STAGING"
  fi
}

state_upsert() {
  local c="$1" s="$2" status="$3" sha="$4" commit="$5" where="$6" reason="$7" tmp
  # `var="$(cmd)"` as a bare statement is NOT `set -e`-exempt: if mktemp
  # fails (called constantly here, deep in the build loop), an unguarded
  # assignment would abort the whole run over a ledger-persistence hiccup.
  tmp="$(mktemp "$STATE_FILE.XXXXXX")" || { echo "WARNING: mktemp failed; state.tsv not updated for $c/$s" >&2; return 0; }
  if [ -f "$STATE_FILE" ]; then
    awk -F'\t' -v c="$c" -v s="$s" 'BEGIN{OFS="\t"} !($1==c && $2==s)' "$STATE_FILE" > "$tmp"
  else
    : > "$tmp"
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$c" "$s" "$status" "$sha" "$commit" "$where" "$reason" >> "$tmp"
  # Guarded (not a bare mv): state_upsert is called as a plain statement from
  # deep inside the build loop, so an unguarded failure would trip `set -e`
  # and abort the whole run over what's only a ledger-persistence hiccup
  # (--publish-only resumability), never fatal to the in-memory item table.
  mv "$tmp" "$STATE_FILE" || echo "WARNING: could not persist state.tsv (continuing; --publish-only resume may be incomplete)" >&2
}

# state_field CLIENT SLUG FIELD_INDEX(1-based) -> value, or "" if no row
state_field() {
  [ -f "$STATE_FILE" ] || { echo ""; return 0; }
  awk -F'\t' -v c="$1" -v s="$2" -v f="$3" '$1==c && $2==s {print $f; found=1} END{if (!found) print ""}' "$STATE_FILE"
}

# ---------------------------------------------------------------------------
# In-memory work-item table (parallel arrays -- bash 3.2 has no assoc arrays)
# ---------------------------------------------------------------------------
ITEM_CLIENT=(); ITEM_SLUG=(); ITEM_STATUS=(); ITEM_SHA=(); ITEM_COMMIT=(); ITEM_WHERE=(); ITEM_REASON=()
PLAN_DONE=0
HAD_FAILURE=0
HAD_SKIP=0
HAD_WARNING=0

# _slug_in <slug> <space-separated list>: word-exact membership test. Used by
# plan_items to keep the darwin pair together; a plain case-glob on the raw
# string would match darwin-amd64 inside darwin-amd64-something.
_slug_in() {
  case " $2 " in *" $1 "*) return 0 ;; esac
  return 1
}

add_item() {
  ITEM_CLIENT+=("$1"); ITEM_SLUG+=("$2")
  ITEM_STATUS+=("PLANNED"); ITEM_SHA+=(""); ITEM_COMMIT+=(""); ITEM_WHERE+=(""); ITEM_REASON+=("")
  # Do NOT stamp PLANNED over a row that already reached a terminal state. The
  # plan phase runs on EVERY invocation, including --publish-only, so an
  # unconditional upsert here overwrote the BUILT row that --publish-only then
  # read back two phases later -- it destroyed exactly the state it exists to
  # resume from, and failed with "not previously built (ledger status:
  # 'PLANNED')" while a perfectly good artifact sat in stage/. That made the
  # documented build-on-one-host / publish-from-another workflow impossible.
  #
  # cached_ok() reads the same rows to skip rebuilds for an unchanged commit, so
  # preserving them fixes that too. --force-rebuild still wins: it ignores the
  # cache in cached_ok() rather than needing the row erased here.
  case "$(state_field "$1" "$2" 3)" in
    BUILT|PUBLISHED|VERIFIED) return 0 ;;
  esac
  state_upsert "$1" "$2" PLANNED "" "" "" ""
}

find_item_index() {
  local c="$1" s="$2" i
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    if [ "${ITEM_CLIENT[$i]}" = "$c" ] && [ "${ITEM_SLUG[$i]}" = "$s" ]; then
      echo "$i"; return 0
    fi
  done
  echo "-1"
}

set_item_status() {
  local c="$1" s="$2" status="$3" sha="${4:-}" commit="${5:-}" where="${6:-}" reason="${7:-}" idx
  # bash 3.2 (macOS) has no negative array subscripts (that needs bash>=4.2),
  # so idx=-1 (item not in the plan -- shouldn't happen, but stay defensive)
  # must never be used to index ITEM_*[] below.
  idx="$(find_item_index "$c" "$s")"
  local out_sha="" out_commit="" out_where=""
  if [ "$idx" -ge 0 ]; then
    ITEM_STATUS[idx]="$status"
    [ -n "$sha" ] && ITEM_SHA[idx]="$sha"
    [ -n "$commit" ] && ITEM_COMMIT[idx]="$commit"
    [ -n "$where" ] && ITEM_WHERE[idx]="$where"
    ITEM_REASON[idx]="$reason"
    out_sha="${ITEM_SHA[idx]:-}"; out_commit="${ITEM_COMMIT[idx]:-}"; out_where="${ITEM_WHERE[idx]:-}"
  fi
  state_upsert "$c" "$s" "$status" "$out_sha" "$out_commit" "$out_where" "$reason"
  printf '  [%-11s %-13s] %-16s %-8s %s\n' "$c" "$s" "$status" "${where:--}" "$reason"
}

mark_built()    { set_item_status "$1" "$2" BUILT "" "$3" "$4" ""; }
mark_verified() { set_item_status "$1" "$2" VERIFIED "$3" "$4" "$5" ""; }
mark_failed() {
  set_item_status "$1" "$2" "FAILED($3)" "" "" "" "$4"
  HAD_FAILURE=1
}
warn_skip() {
  set_item_status "$1" "$2" SKIPPED "" "" "" "$3"
  echo "WARNING: SKIPPING $1/$2: $3" >&2
  HAD_SKIP=1
}
mark_all_failed_for_client() {
  local client="$1" slugs="$2" phase="$3" reason="$4" s
  for s in $slugs; do mark_failed "$client" "$s" "$phase" "$reason"; done
}

cached_ok() {
  local c="$1" s="$2" commit="$3" prev_status prev_commit file
  [ "$FORCE_REBUILD" != 1 ] || return 1
  prev_status="$(state_field "$c" "$s" 3)"
  case "$prev_status" in BUILT|VERIFIED) : ;; *) return 1 ;; esac
  prev_commit="$(state_field "$c" "$s" 5)"
  [ -n "$commit" ] && [ "$prev_commit" = "$commit" ] || return 1
  file="$STAGE_DIR/$(resolved_filename "$c" "$s")"
  [ -s "$file" ] || return 1
  return 0
}

items_for_client() {
  local c="$1" i out=""
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_CLIENT[$i]}" = "$c" ] && out="$out ${ITEM_SLUG[$i]}"
  done
  echo "${out# }"
}

# ---------------------------------------------------------------------------
# Plan phase
# ---------------------------------------------------------------------------
plan_items() {
  echo "== Plan =="
  local requested_clients c allowed want s
  requested_clients="${CLIENTS_ARG:-$DEFAULT_CLIENTS}"

  for c in $(echo "$requested_clients" | tr ',' ' '); do
    allowed="$(client_allowed_platforms "$c")"
    if [ "$allowed" = "*" ]; then
      add_item "$c" "*"
      [ -n "$PLATFORMS_ARG" ] && echo "NOTE: $c is platform-independent -- ignoring --platforms for it."
      continue
    fi
    if [ -n "$PLATFORMS_ARG" ]; then
      want="$(echo "$PLATFORMS_ARG" | tr ',' ' ')"
    else
      want="$allowed"
    fi
    # The two darwin slugs are ONE artifact: build_darwin_universal lipo's a
    # single fat binary and stages it under both filenames with an identical
    # sha256 (the "Mac-universal rule"), and it marks BOTH slugs BUILT even when
    # only one was asked for. The publish phase, however, walks the PLAN -- so
    # `--platforms darwin-arm64` used to upload one filename and leave the other
    # on whatever build it had before. Not a cosmetic gap: it puts two different
    # binaries behind names that must be byte-identical, and binaries.json then
    # advertises two different sha256 for them. Observed live on 2026-07-30 --
    # geth-darwin-arm64 went to 1.17.5-xdc.8 while geth-darwin-amd64 stayed on
    # the 6-day-old 1.17.4-xdc.7.
    #
    # Expanded here, at plan time, so every later phase (build, checksum,
    # publish, manifest, summary) covers the pair without special-casing.
    if _slug_in darwin-amd64 "$allowed" && _slug_in darwin-arm64 "$allowed"; then
      if _slug_in darwin-amd64 "$want" && ! _slug_in darwin-arm64 "$want"; then
        want="$want darwin-arm64"
        echo "NOTE: $c darwin is one universal binary -- adding darwin-arm64 so both filenames stay identical."
      elif _slug_in darwin-arm64 "$want" && ! _slug_in darwin-amd64 "$want"; then
        want="$want darwin-amd64"
        echo "NOTE: $c darwin is one universal binary -- adding darwin-amd64 so both filenames stay identical."
      fi
    fi
    for s in $want; do
      case " $allowed " in
        *" $s "*) add_item "$c" "$s" ;;
        *)
          if [ "$c" = nethermind ] && [ "$s" = linux-arm64 ]; then
            echo "WARNING: nethermind has no linux-arm64 build (never published) -- dropping it from the plan." >&2
            HAD_WARNING=1
          elif [ -n "$PLATFORMS_ARG" ]; then
            echo "WARNING: $c does not support platform '$s' -- dropping it from the plan." >&2
            HAD_WARNING=1
          fi
          ;;
      esac
    done
  done

  PLAN_DONE=1
  [ "${#ITEM_CLIENT[@]}" -gt 0 ] || hard_abort "nothing to do: no (client, platform) combination matched --clients/--platforms"
  echo "Planned ${#ITEM_CLIENT[@]} item(s)."
  # Per-client breakdown so it's clear up front exactly which clients (and
  # which platform slugs of each) this run will build. Reuses the plan-phase
  # locals c/s; runs after the arrays are populated by add_item above.
  echo "Clients to build:"
  for c in $ALL_CLIENTS; do
    s="$(items_for_client "$c")"
    # `if`, not `[ -n "$s" ] && printf`: on the LAST iteration a false test
    # leaves that as the loop's -- and so the function's -- exit status, and
    # plan_items is called bare under `set -e`, which aborts the whole run.
    # Any --clients subset not containing the last entry of ALL_CLIENTS hit
    # this, i.e. the header's own `--build-only --clients geth,xone` example.
    if [ -n "$s" ]; then
      printf '  %-11s %s\n' "$c" "$s"
    fi
  done
}

# ---------------------------------------------------------------------------
# --publish-only resume: read state.tsv instead of building.
# ---------------------------------------------------------------------------
resume_from_ledger() {
  echo "== Resume from ledger (--publish-only) =="
  local i c s status sha commit where file
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    c="${ITEM_CLIENT[$i]}"; s="${ITEM_SLUG[$i]}"
    status="$(state_field "$c" "$s" 3)"
    file="$STAGE_DIR/$(resolved_filename "$c" "$s")"
    case "$status" in
      BUILT|VERIFIED)
        if [ -s "$file" ]; then
          sha="$(state_field "$c" "$s" 4)"; commit="$(state_field "$c" "$s" 5)"; where="$(state_field "$c" "$s" 6)"
          ITEM_STATUS[i]="BUILT"; ITEM_SHA[i]="$sha"; ITEM_COMMIT[i]="$commit"; ITEM_WHERE[i]="${where:-resumed}"
          echo "  [$c/$s] resuming from ledger (staged artifact present)"
        else
          mark_failed "$c" "$s" missing-build "ledger says $status but $file is missing -- rebuild without --publish-only first"
        fi
        ;;
      *)
        mark_failed "$c" "$s" missing-build "not previously built (ledger status: '${status:-none}') -- run a build first"
        ;;
    esac
  done
}

# ---------------------------------------------------------------------------
# Docker helpers. Emulation note: `docker run --platform linux/<arch>` on a
# host whose own arch differs runs the container under QEMU (when binfmt is
# registered, which Docker Desktop / most modern Docker installs do by
# default) -- so a command run *inside* that container is NATIVE to the
# emulated arch, not cross-compiled. That's why linux slices below use the
# exact same build command whether we end up on "host" or "docker": the only
# difference is which CPU is pretending to be which.
# ---------------------------------------------------------------------------
DOCKER_CHECKED=0
DOCKER_OK=0
docker_available() {
  [ "$NO_DOCKER" != 1 ] || return 1
  if [ "$DOCKER_CHECKED" = 0 ]; then
    DOCKER_CHECKED=1
    if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; then DOCKER_OK=1; fi
  fi
  [ "$DOCKER_OK" = 1 ]
}

DOCKER_CIDS=()
docker_run_build() {
  local image="$1" platform="$2" srcdir="$3" cmd="$4" log="$5" cname rc=0
  cname="publish-bins-$$-$RANDOM"
  DOCKER_CIDS+=("$cname")
  docker run --rm --name "$cname" --platform "$platform" \
    -v "$srcdir:/work" -w /work "$image" bash -lc "$cmd" >>"$log" 2>&1 || rc=$?
  local j
  for (( j = 0; j < ${#DOCKER_CIDS[@]}; j++ )); do
    [ "${DOCKER_CIDS[$j]}" = "$cname" ] && DOCKER_CIDS[j]=""
  done
  return "$rc"
}

# run_step CLIENT SLUG SRCDIR CMD IMAGE LOG
# Runs CMD (a single shell command string) either natively on the host (when
# the host's own platform_slug matches SLUG) or inside a --platform-pinned
# Docker container (when SLUG's os is linux and Docker is available). Sets
# RUN_STEP_WHERE to "host"/"docker" on success. Returns 0 on success, 1 on a
# real build failure, 2 when neither capability is available (SKIP, not
# FAILED).
RUN_STEP_WHERE=""
# No client is currently known to cross-compile to a DIFFERENT arch on the host.
# geth was tried and does not: despite being mostly Go it pulls cgo dependencies
# with arch-specific assembly, so an x86_64 host assembles gcc_arm64.S with the
# wrong assembler ("no such instruction: ldp x23,x24"). Supplying
# CC=aarch64-linux-gnu-gcc does not rescue it either, because build/ci.go injects
# the x86-only `-m64` flag, which that compiler rejects. CGO_ENABLED=0 does not
# avoid it. A foreign-arch linux slice therefore needs a real machine of that arch,
# or docker with QEMU/binfmt registered.
#
# Kept as a hook so the next person has somewhere obvious to add a client that
# genuinely does cross-compile, with the evidence rather than an assumption.
client_cross_compiles_on_host() {
  case "$1" in
    *) return 1 ;;
  esac
}

run_step() {
  local client="$1" slug="$2" srcdir="$3" cmd="$4" image="$5" log="$6" os_ arch_
  os_="${slug%-*}"; arch_="${slug#*-}"
  # Build on the host when the target IS this machine, OR when the client
  # cross-compiles on the host anyway. That second case was missing: a
  # linux-arm64 slug on an x86_64 host fell straight through to docker with
  # --platform linux/arm64, which pulls an arm64 image and dies with
  # "exec /usr/bin/bash: exec format error" unless QEMU/binfmt happens to be
  # registered. The command is `GOOS=linux GOARCH=arm64 make geth` -- a plain Go
  # cross-compile needing no container at all. That is why geth's linux-arm64
  # slice quietly stopped being republishable and drifted a release behind its
  # own linux-amd64 sibling.
  if [ "${os_}-${arch_}" = "$(platform_slug)" ] || client_cross_compiles_on_host "$client"; then
    echo "[$client/$slug] host build: $cmd" >>"$log"
    if ( cd "$srcdir" && eval "$cmd" ) >>"$log" 2>&1; then RUN_STEP_WHERE=host; return 0; fi
    # Fall through to docker rather than failing outright -- a cross-compile can
    # still fail for a reason a matching-arch container would solve.
    echo "[$client/$slug] host build failed; falling back to docker" >>"$log"
  fi
  if [ "$os_" = linux ] && docker_available; then
    echo "[$client/$slug] docker build ($image, linux/$arch_): $cmd" >>"$log"
    if docker_run_build "$image" "linux/$arch_" "$srcdir" "$cmd" "$log"; then RUN_STEP_WHERE=docker; return 0; fi
    return 1
  fi
  return 2
}

# ---------------------------------------------------------------------------
# Source checkout (shallow clone, or fetch+reset if already present)
# ---------------------------------------------------------------------------
clone_or_update_src() {
  local client="$1" repo="$2" srcdir="$3" log="$4" ref
  ref="$(get_ref "$client")"
  if [ "$DRY_RUN" = 1 ]; then
    echo "[$client] DRY-RUN: would clone/update $repo (ref=${ref:-default branch}) at $srcdir" | tee -a "$log" || true
    return 0
  fi
  if [ -d "$srcdir/.git" ]; then
    # Report the commit BEFORE and AFTER, on stdout not just the log. "would
    # clone/update" told you nothing about whether the tip actually moved, so a
    # checkout left behind by an earlier run looked identical to a fresh pull.
    # Observed for real: this tree sat at adf46fc while origin/HEAD had already
    # moved to f39bb07.
    local _before _after
    _before="$(git -C "$srcdir" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
    echo "[$client] updating existing checkout at $srcdir (at $_before)" | tee -a "$log" || true
    # `if cmd; then return 0; fi; return 1` (rather than `cmd; return $?`) is
    # required for this to be `set -e`-safe independent of how the caller
    # invokes us: a failing standalone command outside an if/&&/|| context
    # would otherwise abort the whole script right here instead of letting
    # this one client's build be marked FAILED and the run continue.
    if ( cd "$srcdir" && git fetch --depth 1 origin "${ref:-HEAD}" && git reset --hard FETCH_HEAD ) >>"$log" 2>&1; then
      _after="$(git -C "$srcdir" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
      if [ "$_before" = "$_after" ]; then
        echo "[$client] already at latest ${ref:-default branch}: $_after" | tee -a "$log" || true
      else
        echo "[$client] pulled ${ref:-default branch}: $_before -> $_after" | tee -a "$log" || true
      fi
      return 0
    fi
    return 1
  fi
  echo "[$client] cloning $repo (ref=${ref:-default branch}) -> $srcdir" >>"$log"
  mkdir -p "$(dirname "$srcdir")" || true   # a failure here surfaces as the guarded git clone below failing anyway
  if [ -n "$ref" ]; then
    if git clone --depth 1 --branch "$ref" "$repo" "$srcdir" >>"$log" 2>&1; then return 0; fi
    # Branch/tag clone failed -- REF might be a bare commit sha, which needs full history.
    rm -rf "$srcdir" || true
    if git clone "$repo" "$srcdir" >>"$log" 2>&1 && ( cd "$srcdir" && git checkout "$ref" ) >>"$log" 2>&1; then
      return 0
    fi
    return 1
  fi
  if git clone --depth 1 "$repo" "$srcdir" >>"$log" 2>&1; then return 0; fi
  return 1
}

client_commit() {
  git -C "$1" rev-parse --short=12 HEAD 2>/dev/null || echo unknown
}

# ---------------------------------------------------------------------------
# Shared darwin-universal (lipo) helper. Builds two slices via the two
# caller-supplied commands (each run with cwd=srcdir), lipo's them together,
# validates both slices are present, then stages the SAME fat binary under
# both darwin filenames (identical sha256) -- the "Mac-universal rule".
# Selecting only one darwin slug still builds (and marks BUILT) both.
# ---------------------------------------------------------------------------
build_darwin_universal() {
  local client="$1" srcdir="$2" commit="$3" log="$4" arm64_cmd="$5" amd64_cmd="$6"
  local arm64_outbin_relpath="$7" amd64_outbin_relpath="$8" need_cmd="$9"
  local missing="" c hostos reason
  hostos="$(uname -s)"
  for c in $need_cmd; do command -v "$c" >/dev/null 2>&1 || missing="$missing $c"; done
  if [ "$hostos" != Darwin ] || [ -n "$missing" ]; then
    if [ "$hostos" != Darwin ]; then
      reason="darwin universal build needs a macOS host with:$need_cmd (this host is $hostos)"
    else
      reason="darwin universal build is missing required tool(s):$missing"
    fi
    warn_skip "$client" darwin-amd64 "$reason"
    warn_skip "$client" darwin-arm64 "$reason"
    return 0
  fi
  if cached_ok "$client" darwin-amd64 "$commit" && cached_ok "$client" darwin-arm64 "$commit"; then
    mark_built "$client" darwin-amd64 "$commit" host
    mark_built "$client" darwin-arm64 "$commit" host
    echo "[$client] darwin-amd64/darwin-arm64 already built for commit $commit (cached; --force-rebuild to redo)"
    return 0
  fi

  echo "[$client] building darwin universal (arm64 native + amd64 cross, then lipo) ..." | tee -a "$log" || true
  local ok=1
  local slice_arm64="$STAGE_DIR/.${client}-darwin-arm64.slice"
  local slice_amd64="$STAGE_DIR/.${client}-darwin-amd64.slice"
  rm -f "$slice_arm64" "$slice_amd64"

  # Delete the expected output before each build. `git reset --hard` does not
  # remove untracked build products, so build/bin/<target> survives a pull to a
  # new commit. If a build then no-ops or exits 0 without rewriting it, the copy
  # below would stage a binary from the PREVIOUS commit while client_commit
  # reports the new HEAD -- a published artifact whose recorded provenance is
  # simply wrong, and nothing downstream could detect it. Removing it first
  # turns that silent mismatch into an honest FAILED (the cp cannot find it).
  rm -f "$srcdir/$arm64_outbin_relpath" "$srcdir/$amd64_outbin_relpath"
  ( cd "$srcdir" && eval "$arm64_cmd" ) >>"$log" 2>&1 || ok=0
  if [ "$ok" = 1 ]; then cp "$srcdir/$arm64_outbin_relpath" "$slice_arm64" 2>>"$log" || ok=0; fi
  if [ "$ok" = 1 ]; then ( cd "$srcdir" && eval "$amd64_cmd" ) >>"$log" 2>&1 || ok=0; fi
  if [ "$ok" = 1 ]; then cp "$srcdir/$amd64_outbin_relpath" "$slice_amd64" 2>>"$log" || ok=0; fi

  if [ "$ok" != 1 ]; then
    mark_failed "$client" darwin-amd64 build "build failed (see $log)"
    mark_failed "$client" darwin-arm64 build "build failed (see $log)"
    rm -f "$slice_arm64" "$slice_amd64"
    return 0
  fi

  local fat="$STAGE_DIR/.${client}-darwin.universal"
  if ! lipo -create -output "$fat" "$slice_arm64" "$slice_amd64" >>"$log" 2>&1; then
    mark_failed "$client" darwin-amd64 build "lipo -create failed (see $log)"
    mark_failed "$client" darwin-arm64 build "lipo -create failed (see $log)"
    rm -f "$slice_arm64" "$slice_amd64" "$fat"
    return 0
  fi
  # `|| true`: a bare failing command-substitution assignment would trip
  # `set -e` here (this function isn't called from an if/&&/! context); the
  # `case` below already treats an empty/unexpected $info as "missing a
  # slice" via its catch-all branch, so falling through with info="" on a
  # lipo failure is exactly the right degraded behavior.
  local info; info="$(lipo -info "$fat" 2>>"$log")" || true
  case "$info" in
    *x86_64*arm64*|*arm64*x86_64*) : ;;
    *)
      mark_failed "$client" darwin-amd64 build "lipo -info missing a slice: $info"
      mark_failed "$client" darwin-arm64 build "lipo -info missing a slice: $info"
      rm -f "$slice_arm64" "$slice_amd64" "$fat"
      return 0
      ;;
  esac

  # Guard the staging cp/chmod explicitly (rather than leaving them as bare
  # commands): this function is called as a plain statement, not from an
  # if/&&/! context, so an unguarded failure here (e.g. a full disk) would
  # trip `set -e` and abort the ENTIRE script instead of just failing this
  # one client's two darwin items.
  if ! cp "$fat" "$STAGE_DIR/$(resolved_filename "$client" darwin-amd64)" \
     || ! cp "$fat" "$STAGE_DIR/$(resolved_filename "$client" darwin-arm64)" \
     || ! chmod +x "$STAGE_DIR/$(resolved_filename "$client" darwin-amd64)" "$STAGE_DIR/$(resolved_filename "$client" darwin-arm64)"; then
    mark_failed "$client" darwin-amd64 build "failed staging the universal binary into $STAGE_DIR"
    mark_failed "$client" darwin-arm64 build "failed staging the universal binary into $STAGE_DIR"
    rm -f "$slice_arm64" "$slice_amd64" "$fat"
    return 0
  fi
  rm -f "$slice_arm64" "$slice_amd64" "$fat"
  mark_built "$client" darwin-amd64 "$commit" host
  mark_built "$client" darwin-arm64 "$commit" host
}

# build_linux_slice CLIENT MAKE_TARGET ARCH SRCDIR COMMIT LOG [EXTRA_ENV]
# Generic linux-slice builder for the `make <target>` clients (geth, xone,
# erigon -- erigon passes EXTRA_ENV="CGO_ENABLED=1"). See run_step() above
# for the host-vs-docker resolution and the emulation note explaining why
# the exact same command works either way.
build_linux_slice() {
  local client="$1" make_target="$2" arch="$3" srcdir="$4" commit="$5" log="$6" extra_env="${7:-}"
  local slug="linux-$arch"
  if cached_ok "$client" "$slug" "$commit"; then
    mark_built "$client" "$slug" "$commit" "$(state_field "$client" "$slug" 6)"
    echo "[$client] $slug already built for commit $commit (cached; --force-rebuild to redo)"
    return 0
  fi
  # go-ethereum-derived trees drive their Makefile through `go run build/ci.go`.
  # Setting GOOS/GOARCH there cross-compiles CI.GO ITSELF for the target, so `go
  # run` then tries to execute an arm64 helper on an amd64 host and dies with
  # "fork/exec ...: exec format error". ci.go takes the target as a flag instead.
  # build_darwin_universal already does this; the linux path did not, which is the
  # real reason geth's linux-arm64 slice could not be rebuilt (the docker fallback
  # then failed too, for the unrelated foreign-image reason). Trees without
  # build/ci.go keep the plain GOOS/GOARCH make invocation that already worked.
  local cmd
  if [ -f "$srcdir/build/ci.go" ]; then
    cmd="$extra_env go run build/ci.go install -arch $arch ./cmd/$make_target"
  else
    cmd="GOOS=linux GOARCH=$arch $extra_env make $make_target"
  fi
  local rc=0
  local out; out="$(client_build_out "$client" "$make_target")"
  # Same reason as build_darwin_universal: drop any output left by an earlier
  # commit's build so a no-op build cannot stage it under this commit's name.
  rm -f "$srcdir/$out"
  run_step "$client" "$slug" "$srcdir" "$cmd" "$IMG_GOLANG" "$log" || rc=$?
  if [ "$rc" = 0 ]; then
    # Guarded explicitly (not bare cp/chmod): this function is called as a
    # plain statement, so an unguarded staging failure would trip `set -e`
    # and abort the whole script instead of just failing this one item.
    if cp "$srcdir/$out" "$STAGE_DIR/$(resolved_filename "$client" "$slug")" \
       && chmod +x "$STAGE_DIR/$(resolved_filename "$client" "$slug")"; then
      mark_built "$client" "$slug" "$commit" "$RUN_STEP_WHERE"
    else
      mark_failed "$client" "$slug" build "failed staging the built binary into $STAGE_DIR"
    fi
  elif [ "$rc" = 2 ]; then
    warn_skip "$client" "$slug" "no matching host and no Docker available (need $IMG_GOLANG, linux/$arch)"
  else
    mark_failed "$client" "$slug" build "build failed (see $log)"
  fi
}

# ---------------------------------------------------------------------------
# Per-client build functions
# ---------------------------------------------------------------------------

# geth / xone: both pure-Go, `make <target>` -> build/bin/<target>. Only the
# make target and allowed linux slugs differ (xone has no linux-arm64).
build_pure_go_client() {
  local client="$1" make_target="$2" requested="$3" repo srcdir log commit want_darwin=0 s
  repo="$(client_repo "$client")"; srcdir="$SRC_ROOT/$client"; log="$LOG_DIR/${client}.log"; : > "$log"
  echo "[$client] preparing source ..."
  if ! clone_or_update_src "$client" "$repo" "$srcdir" "$log"; then
    mark_all_failed_for_client "$client" "$requested" build "clone/update failed (see $log)"
    return 0
  fi
  [ "$DRY_RUN" = 1 ] && { echo "[$client] DRY-RUN: would build [$requested]"; return 0; }
  commit="$(client_commit "$srcdir")"

  for s in $requested; do case "$s" in darwin-amd64|darwin-arm64) want_darwin=1 ;; esac; done
  if [ "$want_darwin" = 1 ]; then
    # Cross slice: `GOOS=darwin GOARCH=amd64 make <target>` does NOT work on a
    # go-ethereum-derived tree. Its Makefile target is a thin wrapper around
    # `go run build/ci.go install`, and ci.go cross-builds via its OWN -arch
    # flag -- it never reads GOARCH, so the env var is silently ignored and the
    # "amd64" slice comes out arm64. lipo then refuses the pair:
    #   have the same architectures (arm64) and can't be in the same fat file
    # so geth darwin builds failed outright on Apple Silicon.
    #
    # cgo flags do not rescue this: adding CGO_*FLAGS='-arch x86_64' (the trick
    # erigon's recipe uses, correctly, because erigon's Makefile calls `go
    # build` and so does honour GOARCH) only retargets the C half. The Go
    # objects stay arm64 and the link fails with "found architecture 'arm64',
    # required architecture 'x86_64'". Verified both failure modes here.
    #
    # Detected per-tree rather than hardcoded per-client: this helper is shared
    # with xone, whose source is private and may not use ci.go at all. Trees
    # with build/ci.go get the -arch invocation; anything else keeps the
    # GOARCH+make path that already worked for them.
    local amd64_cmd="GOOS=darwin GOARCH=amd64 make $make_target"
    if [ -f "$srcdir/build/ci.go" ]; then
      amd64_cmd="go run build/ci.go install -arch amd64 ./cmd/$make_target"
    fi
    build_darwin_universal "$client" "$srcdir" "$commit" "$log" \
      "GOOS=darwin GOARCH=arm64 make $make_target" \
      "$amd64_cmd" \
      "$(client_build_out "$client" "$make_target")" "$(client_build_out "$client" "$make_target")" "go make"
  fi
  for s in $requested; do
    case "$s" in
      linux-amd64) build_linux_slice "$client" "$make_target" amd64 "$srcdir" "$commit" "$log" ;;
      linux-arm64) build_linux_slice "$client" "$make_target" arm64 "$srcdir" "$commit" "$log" ;;
    esac
  done
}
build_geth() { build_pure_go_client geth geth "$1"; }
build_xone() { build_pure_go_client xone xone "$1"; }

# erigon: Go + cgo. Darwin universal needs Apple clang's -arch cross flag
# (macOS-only); linux slices can't cross from macOS at all (cgo) so they
# always go through run_step's host-or-docker resolution.
build_erigon() {
  local requested="$1" client=erigon repo srcdir log commit want_darwin=0 s
  repo="$(client_repo "$client")"; srcdir="$SRC_ROOT/$client"; log="$LOG_DIR/${client}.log"; : > "$log"
  echo "[$client] preparing source ..."
  if ! clone_or_update_src "$client" "$repo" "$srcdir" "$log"; then
    mark_all_failed_for_client "$client" "$requested" build "clone/update failed (see $log)"
    return 0
  fi
  [ "$DRY_RUN" = 1 ] && { echo "[$client] DRY-RUN: would build [$requested]"; return 0; }
  commit="$(client_commit "$srcdir")"

  for s in $requested; do case "$s" in darwin-amd64|darwin-arm64) want_darwin=1 ;; esac; done
  if [ "$want_darwin" = 1 ]; then
    build_darwin_universal "$client" "$srcdir" "$commit" "$log" \
      "CGO_ENABLED=1 CC=clang CXX=clang++ make erigon" \
      "GOARCH=amd64 CGO_ENABLED=1 CC=clang CXX=clang++ CGO_CFLAGS='-arch x86_64' CGO_LDFLAGS='-arch x86_64' make erigon" \
      "build/bin/erigon" "build/bin/erigon" "clang lipo"
  fi
  for s in $requested; do
    case "$s" in
      linux-amd64) build_linux_slice "$client" erigon amd64 "$srcdir" "$commit" "$log" "CGO_ENABLED=1" ;;
      linux-arm64) build_linux_slice "$client" erigon arm64 "$srcdir" "$commit" "$log" "CGO_ENABLED=1" ;;
    esac
  done
}

# reth: Rust via cargo, binary name xdc-reth. Darwin universal needs rustup's
# apple targets + Xcode's macOS SDK (macOS-only); linux slices via cargo,
# same host-or-docker resolution as everyone else.
build_reth() {
  local requested="$1" client=reth repo srcdir log commit binname want_darwin=0 s
  repo="$(client_repo "$client")"; srcdir="$SRC_ROOT/$client"; log="$LOG_DIR/${client}.log"; : > "$log"
  binname="$(client_binary_name "$client")"
  echo "[$client] preparing source ..."
  if ! clone_or_update_src "$client" "$repo" "$srcdir" "$log"; then
    mark_all_failed_for_client "$client" "$requested" build "clone/update failed (see $log)"
    return 0
  fi
  [ "$DRY_RUN" = 1 ] && { echo "[$client] DRY-RUN: would build [$requested]"; return 0; }
  commit="$(client_commit "$srcdir")"

  for s in $requested; do case "$s" in darwin-amd64|darwin-arm64) want_darwin=1 ;; esac; done
  if [ "$want_darwin" = 1 ]; then
    # Unlike the make-based clients above, cargo puts each --target's output
    # under its own target-triple directory, so (unlike geth/xone/erigon)
    # the arm64 and amd64 slices have DIFFERENT output paths -- pass both.
    build_darwin_universal "$client" "$srcdir" "$commit" "$log" \
      "rustup target add aarch64-apple-darwin --toolchain stable && cargo +stable build --release -p xdc-reth --bin xdc-reth --target aarch64-apple-darwin" \
      "rustup target add x86_64-apple-darwin --toolchain stable && cargo +stable build --release -p xdc-reth --bin xdc-reth --target x86_64-apple-darwin" \
      "target/aarch64-apple-darwin/release/$binname" "target/x86_64-apple-darwin/release/$binname" "rustup cargo lipo"
  fi
  for s in $requested; do
    case "$s" in
      linux-amd64) build_reth_linux_slice amd64 "$srcdir" "$commit" "$log" "$binname" ;;
      linux-arm64) build_reth_linux_slice arm64 "$srcdir" "$commit" "$log" "$binname" ;;
    esac
  done
}
build_reth_linux_slice() {
  local arch="$1" srcdir="$2" commit="$3" log="$4" binname="$5"
  local client=reth slug rc=0
  slug="linux-$arch"
  if cached_ok "$client" "$slug" "$commit"; then
    mark_built "$client" "$slug" "$commit" "$(state_field "$client" "$slug" 6)"
    echo "[$client] $slug already built for commit $commit (cached; --force-rebuild to redo)"
    return 0
  fi
  # -p xdc-reth --bin xdc-reth, NOT a bare `cargo build --release`. The XDC binary
  # is its own crate at bin/xdc-reth; the workspace default-run target is upstream's
  # vanilla `reth`. A bare build therefore produced target/release/reth and the
  # staging copy of target/release/xdc-reth failed with "failed staging the built
  # binary" -- after a full 13-minute compile, every run. Worse, the vanilla binary
  # is not a silent substitute: it lacks the XDC feature set and panics at startup
  # in rustls ("Could not automatically determine the process-level CryptoProvider")
  # as soon as ethstats is enabled, so shipping it would be worse than failing.
  run_step "$client" "$slug" "$srcdir" "cargo build --release -p xdc-reth --bin xdc-reth" "$IMG_RUST" "$log" || rc=$?
  if [ "$rc" = 0 ]; then
    if cp "$srcdir/target/release/$binname" "$STAGE_DIR/$(resolved_filename "$client" "$slug")" \
       && chmod +x "$STAGE_DIR/$(resolved_filename "$client" "$slug")"; then
      mark_built "$client" "$slug" "$commit" "$RUN_STEP_WHERE"
    else
      mark_failed "$client" "$slug" build "failed staging the built binary into $STAGE_DIR"
    fi
  elif [ "$rc" = 2 ]; then
    warn_skip "$client" "$slug" "no linux/$arch host and no Docker available (need $IMG_RUST, linux/$arch)"
  else
    mark_failed "$client" "$slug" build "build failed (see $log)"
  fi
}

# besu: Java/Gradle, platform-independent ("*"). Build once, then bundle a
# Temurin 21 JRE we extract from the pinned image (docker create + docker cp
# -- no long-running container left behind) so the archive needs no host
# Java to run. See header OPEN QUESTIONS #2 for the unresolved platform
# question this raises.
build_besu() {
  local requested="$1" client=besu repo srcdir log commit
  repo="$(client_repo "$client")"; srcdir="$SRC_ROOT/$client"; log="$LOG_DIR/${client}.log"; : > "$log"
  echo "[$client] preparing source ..."
  if ! clone_or_update_src "$client" "$repo" "$srcdir" "$log"; then
    mark_failed "$client" "*" build "clone/update failed (see $log)"
    return 0
  fi
  [ "$DRY_RUN" = 1 ] && { echo "[$client] DRY-RUN: would build (platform-independent)"; return 0; }
  commit="$(client_commit "$srcdir")"

  if cached_ok "$client" "*" "$commit"; then
    mark_built "$client" "*" "$commit" "$(state_field "$client" "*" 6)"
    echo "[$client] already built for commit $commit (cached; --force-rebuild to redo)"
    return 0
  fi

  local java_ok=0
  if command -v java >/dev/null 2>&1; then
    # `|| true`: under `pipefail`, grep finding no match fails the whole
    # pipeline even though head/tr succeed -- an unguarded bare assignment
    # would trip `set -e` (this function isn't called from an if/&&/!
    # context) instead of just falling through to the Docker path below.
    local jv; jv="$(java -version 2>&1 | grep -oE '"[0-9]+' | head -1 | tr -d '"')" || true
    if [ -n "${jv:-}" ] && [ "$jv" -ge 21 ] 2>/dev/null; then java_ok=1; fi
  fi

  local ok=1 where=host
  if [ "$java_ok" = 1 ] && [ -x "$srcdir/gradlew" ]; then
    echo "[$client] building via host JDK 21 + gradle ..." | tee -a "$log" || true
    ( cd "$srcdir" && ./gradlew --no-daemon installDist ) >>"$log" 2>&1 || ok=0
    where=host
  elif docker_available; then
    echo "[$client] building via Docker ($IMG_TEMURIN21) ..." | tee -a "$log" || true
    docker_run_build "$IMG_TEMURIN21" linux/amd64 "$srcdir" "./gradlew --no-daemon installDist" "$log" || ok=0
    where=docker
  else
    warn_skip "$client" "*" "no JDK 21 + gradle on host and no Docker available (need $IMG_TEMURIN21)"
    return 0
  fi
  if [ "$ok" != 1 ]; then
    mark_failed "$client" "*" build "gradle installDist failed (see $log)"
    return 0
  fi

  # `|| true`: find exits non-zero if build/install doesn't exist at all
  # (already a real scenario if the gradle project layout differs), and
  # under `pipefail` that fails the whole pipeline even though head
  # succeeds -- an unguarded assignment would trip `set -e` and abort the
  # script instead of hitting the `-z "$installdir"` FAILED(build) below.
  local installdir
  installdir="$(find "$srcdir/build/install" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)" || true
  if [ -z "$installdir" ]; then
    mark_failed "$client" "*" build "installDist produced no build/install/* directory (see $log)"
    return 0
  fi

  local root="$STAGE_DIR/.besu-root"
  rm -rf "$root" || true; mkdir -p "$root" || true
  # Guarded explicitly: this function is called as a plain statement, so an
  # unguarded cp failure here would trip `set -e` and abort the whole
  # script instead of just failing this one item.
  if ! cp -R "$installdir/bin" "$root/bin" || ! cp -R "$installdir/lib" "$root/lib"; then
    mark_failed "$client" "*" build "failed staging bin/lib from $installdir into $root"
    rm -rf "$root" || true
    return 0
  fi

  if ! command -v docker >/dev/null 2>&1; then
    mark_failed "$client" "*" build "Docker is required to extract the bundled Temurin 21 JRE from $IMG_TEMURIN21, even when the build itself ran on host Java"
    rm -rf "$root" || true
    return 0
  fi
  local cid jre_ok=1
  cid="$(docker create "$IMG_TEMURIN21" true 2>>"$log")" || jre_ok=0
  if [ "$jre_ok" = 1 ]; then
    DOCKER_CIDS+=("$cid")
    docker cp "$cid:/opt/java/openjdk" "$root/jre" >>"$log" 2>&1 || jre_ok=0
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  if [ "$jre_ok" != 1 ] || [ ! -d "$root/jre" ]; then
    mark_failed "$client" "*" build "could not extract a Temurin 21 JRE from $IMG_TEMURIN21 (see $log)"
    rm -rf "$root" || true
    return 0
  fi

  # See header OPEN QUESTIONS #2: this bundles a single (linux-x64) JRE under
  # a manifest entry keyed "*" (platform-independent) -- the launcher (bin/
  # besu) is expected to resolve its runtime from $APP_HOME/jre, matching the
  # existing manifest _comment's description of this bundle. We do NOT patch
  # the launcher script here (too fragile to do blind, without the real
  # upstream launcher content in hand); if bin/besu doesn't already resolve
  # $APP_HOME/jre on its own, that patch is a manual TODO before publishing.
  echo "NOTE: verify bin/besu resolves \$APP_HOME/jre (bundled runtime) before trusting this archive -- see header OPEN QUESTIONS #2." | tee -a "$log" >&2 || true

  # Guarded explicitly (see the cp -R comment above): tar failing here must
  # not abort the whole script via set -e.
  if ( cd "$root" && tar czf "$STAGE_DIR/$(resolved_filename "$client" "*")" bin lib jre ); then
    mark_built "$client" "*" "$commit" "$where"
  else
    mark_failed "$client" "*" build "tar czf failed while packaging the besu bundle"
  fi
  rm -rf "$root" || true
}

# nethermind: .NET, self-contained `dotnet publish -r <RID>` per slug (darwin
# -amd64/-arm64/linux-amd64 ONLY -- linux-arm64 is dropped at plan time). A
# self-contained, non-AOT publish produces files for the target RID
# regardless of the *building* machine's own arch/OS, so unlike the cgo/rust
# linux slices above we don't need the container's arch to MATCH the RID --
# any dotnet SDK (host or the pinned image) can produce any RID's output.
build_nethermind() {
  local requested="$1" client=nethermind repo srcdir log commit
  repo="$(client_repo "$client")"; srcdir="$SRC_ROOT/$client"; log="$LOG_DIR/${client}.log"; : > "$log"
  echo "[$client] preparing source ..."
  if ! clone_or_update_src "$client" "$repo" "$srcdir" "$log"; then
    mark_all_failed_for_client "$client" "$requested" build "clone/update failed (see $log)"
    return 0
  fi
  [ "$DRY_RUN" = 1 ] && { echo "[$client] DRY-RUN: would build [$requested]"; return 0; }
  commit="$(client_commit "$srcdir")"

  local proj="src/Nethermind/Nethermind.Runner/Nethermind.Runner.csproj"
  if [ ! -f "$srcdir/$proj" ]; then
    echo "WARNING: [$client] expected project not found at $proj -- the repo layout may have moved (TODO: update this path)" >&2
  fi

  local s rid
  for s in $requested; do
    case "$s" in
      darwin-amd64) rid=osx-x64 ;;
      darwin-arm64) rid=osx-arm64 ;;
      linux-amd64)  rid=linux-x64 ;;
      *) continue ;;
    esac
    build_nethermind_rid "$s" "$rid" "$proj" "$srcdir" "$commit" "$log"
  done
}
build_nethermind_rid() {
  local slug="$1" rid="$2" proj="$3" srcdir="$4" commit="$5" log="$6" client=nethermind
  if cached_ok "$client" "$slug" "$commit"; then
    mark_built "$client" "$slug" "$commit" "$(state_field "$client" "$slug" 6)"
    echo "[$client] $slug already built for commit $commit (cached; --force-rebuild to redo)"
    return 0
  fi
  local outdir="$STAGE_DIR/.nethermind-$slug"
  rm -rf "$outdir" || true; mkdir -p "$outdir" || true
  local cmd="dotnet publish $proj -r $rid --self-contained -c Release -o $outdir"
  local ok=1 where=host
  if command -v dotnet >/dev/null 2>&1; then
    echo "[$client/$slug] building via host dotnet SDK ($rid) ..." | tee -a "$log" || true
    ( cd "$srcdir" && eval "$cmd" ) >>"$log" 2>&1 || ok=0
    where=host
  elif docker_available; then
    echo "[$client/$slug] building via Docker ($IMG_DOTNET_SDK, $rid) ..." | tee -a "$log" || true
    docker_run_build "$IMG_DOTNET_SDK" linux/amd64 "$srcdir" "$cmd" "$log" || ok=0
    where=docker
  else
    warn_skip "$client" "$slug" "no dotnet SDK on host and no Docker available (need $IMG_DOTNET_SDK)"
    rm -rf "$outdir" || true
    return 0
  fi
  if [ "$ok" != 1 ]; then
    mark_failed "$client" "$slug" build "dotnet publish failed for $rid (see $log)"
    rm -rf "$outdir" || true
    return 0
  fi
  # Guarded explicitly: tar failing here must not abort the whole script via
  # set -e (see the equivalent comment in build_besu()).
  if ( cd "$outdir" && tar czf "$STAGE_DIR/$(resolved_filename "$client" "$slug")" . ); then
    mark_built "$client" "$slug" "$commit" "$where"
  else
    mark_failed "$client" "$slug" build "tar czf failed while packaging the publish output"
  fi
  rm -rf "$outdir" || true
}

# render_bar DONE TOTAL [WIDTH] -> a fixed-width ASCII bar like "[####------]".
# Deliberately ASCII (no unicode blocks) so it renders identically in an
# interactive terminal and in a piped/redirected CI log, and prints as a plain
# line (no carriage-return trickery) so it interleaves cleanly with the
# per-item BUILT/SKIPPED/FAILED status lines the build functions emit.
render_bar() {
  local done="$1" total="$2" width="${3:-20}" filled i bar=""
  [ "$total" -gt 0 ] || total=1
  filled=$(( done * width / total ))
  [ "$filled" -gt "$width" ] && filled=$width
  [ "$filled" -lt 0 ] && filled=0
  for (( i = 0; i < width; i++ )); do
    if [ "$i" -lt "$filled" ]; then bar="$bar#"; else bar="$bar-"; fi
  done
  printf '[%s]' "$bar"
}

build_items() {
  echo "== Build =="
  # Collect the clients that actually have planned items so the progress bar
  # counts real work (a client with no selected slugs is skipped entirely).
  # Builds are multi-minute, so each client gets a banner up front naming it
  # and showing "N/total" progress; the per-slug BUILT/SKIPPED/FAILED lines
  # from set_item_status still print underneath each banner.
  local c slugs build_clients="" total=0 done=0
  for c in $ALL_CLIENTS; do
    [ -n "$(items_for_client "$c")" ] && build_clients="$build_clients $c"
  done
  build_clients="${build_clients# }"
  for c in $build_clients; do total=$(( total + 1 )); done
  for c in $build_clients; do
    slugs="$(items_for_client "$c")"
    done=$(( done + 1 ))
    printf '\n%s building %d/%d: %-11s (%s)\n' \
      "$(render_bar "$done" "$total")" "$done" "$total" "$c" "$slugs"
    case "$c" in
      geth) build_geth "$slugs" ;;
      xone) build_xone "$slugs" ;;
      erigon) build_erigon "$slugs" ;;
      reth) build_reth "$slugs" ;;
      besu) build_besu "$slugs" ;;
      nethermind) build_nethermind "$slugs" ;;
    esac
  done
  [ "$total" -gt 0 ] && printf '\n%s build phase complete (%d client(s))\n' \
    "$(render_bar "$total" "$total")" "$total"
}

# ---------------------------------------------------------------------------
# Checksum + sanity gate. Only items still BUILT after this are publish-
# eligible; a failure here is FAILED(sanity), not published.
# ---------------------------------------------------------------------------
sanity_check_file() {
  local c="$1" s="$2" file="$3" ftype
  case "$file" in
    *.tar.gz)
      tar -tzf "$file" >/dev/null 2>&1 || { mark_failed "$c" "$s" sanity "tar -tzf failed on $file"; return 1; }
      ;;
    *)
      [ -x "$file" ] || { mark_failed "$c" "$s" sanity "staged binary not executable: $file"; return 1; }
      case "$s" in
        darwin-*)
          ftype="$(binary_magic "$file")"
          case "$ftype" in
            macho-universal) : ;;
            *) mark_failed "$c" "$s" sanity "expected a Mach-O universal (lipo) binary, got: $ftype"; return 1 ;;
          esac
          ;;
        linux-*)
          ftype="$(binary_magic "$file")"
          case "$ftype" in
            elf) : ;;
            *) mark_failed "$c" "$s" sanity "expected an ELF binary, got: $ftype"; return 1 ;;
          esac
          ;;
      esac
      ;;
  esac
  return 0
}

checksum_and_sanity() {
  echo "== Checksum + sanity =="
  [ "$DRY_RUN" = 1 ] && { echo "[dry-run] skipping checksum + sanity (nothing was built)"; return 0; }
  local i c s file base sha
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_STATUS[$i]}" = BUILT ] || continue
    c="${ITEM_CLIENT[$i]}"; s="${ITEM_SLUG[$i]}"
    file="$STAGE_DIR/$(resolved_filename "$c" "$s")"
    if [ ! -s "$file" ]; then mark_failed "$c" "$s" sanity "staged artifact missing or empty: $file"; continue; fi
    sanity_check_file "$c" "$s" "$file" || continue
    base="$(basename "$file")"
    # Both guarded explicitly (not bare `sha=$(...)` / bare redirection):
    # this loop runs unconditionally, so an unguarded sha256_file failure
    # (no sha256sum/shasum) or a write failure would trip `set -e` and
    # abort the whole run instead of just failing this one item.
    if ! sha="$(sha256_file "$file")"; then
      mark_failed "$c" "$s" sanity "could not compute sha256 for $file"
      continue
    fi
    if ! printf '%s  %s\n' "$sha" "$base" > "$file.sha256"; then
      mark_failed "$c" "$s" sanity "could not write $file.sha256"
      continue
    fi
    ITEM_SHA[i]="$sha"
    state_upsert "$c" "$s" BUILT "$sha" "${ITEM_COMMIT[$i]:-}" "${ITEM_WHERE[$i]:-}" ""
    echo "  [$c/$s] sha256 $sha"
  done
}

# ---------------------------------------------------------------------------
# Publish phase: SSH-key-only, never touches a password. SSH_OPT_ARGS is
# built once in main() from PUBLISH_PORT/PUBLISH_SSH_KEY. (See the file-wide
# SC2029 disable near the top of this file for why $(shq ...) below is
# deliberately expanded client-side.)
# ---------------------------------------------------------------------------
shq() { printf '%q' "$1"; }

ssh_cmd_string() {
  local out="ssh" a
  for a in "${SSH_OPT_ARGS[@]}"; do out="$out $(printf '%q' "$a")"; done
  echo "$out"
}

SSH_REACHABLE=0
ssh_preflight() {
  echo "== SSH preflight =="
  if ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" \
       "test -d $(shq "$PUBLISH_DIR") && test -w $(shq "$PUBLISH_DIR") && command -v sha256sum >/dev/null 2>&1"; then
    SSH_REACHABLE=1
    echo "  OK: $PUBLISH_HOST:$PUBLISH_DIR is reachable, writable, and has sha256sum."
  else
    hard_abort "SSH preflight failed: cannot reach $PUBLISH_HOST:$PUBLISH_PORT as $PUBLISH_USER, or $PUBLISH_DIR is missing/not writable, or remote sha256sum is missing. Check PUBLISH_HOST/PUBLISH_PORT/PUBLISH_USER/PUBLISH_DIR/PUBLISH_SSH_KEY in publish.env."
  fi
}

confirm_publish() {
  [ "$MODE" != build-only ] || return 0
  [ "$ASSUME_YES" = 1 ] && return 0
  [ "$DRY_RUN" = 1 ] && return 0
  echo ""
  echo "About to publish to $PUBLISH_USER@$PUBLISH_HOST:$PUBLISH_DIR (port $PUBLISH_PORT):"
  local i any=0
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_STATUS[$i]}" = BUILT ] || continue
    any=1
    echo "  ${ITEM_CLIENT[$i]}/${ITEM_SLUG[$i]} -> $PUBLISH_DIR/$(resolved_filename "${ITEM_CLIENT[$i]}" "${ITEM_SLUG[$i]}") (+ .sha256)"
  done
  [ "$any" = 1 ] || { echo "  (nothing passed the sanity gate -- nothing to publish)"; return 0; }
  echo ""
  if [ ! -t 0 ]; then
    hard_abort "refusing to publish without --yes: stdin is not a terminal (non-interactive run)"
  fi
  local ans=""
  read -r -p "Proceed? [y/N] " ans </dev/tty || true
  case "$ans" in
    y|Y|yes|YES) : ;;
    *) hard_abort "aborted by user at the publish confirmation" ;;
  esac
}

publish_one_item() {
  local i="$1" c s file base rsync_ssh
  c="${ITEM_CLIENT[$i]}"; s="${ITEM_SLUG[$i]}"
  file="$STAGE_DIR/$(resolved_filename "$c" "$s")"; base="$(basename "$file")"
  rsync_ssh="$(ssh_cmd_string)"

  if ! rsync --partial --checksum -e "$rsync_ssh" "$file" "$file.sha256" \
        "$PUBLISH_USER@$PUBLISH_HOST:$REMOTE_INCOMING/" >/dev/null; then
    mark_failed "$c" "$s" upload "rsync to $REMOTE_INCOMING failed"
    return 0
  fi

  if ! ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" \
        "cd $(shq "$REMOTE_INCOMING") && sha256sum -c $(shq "$base.sha256")" >/dev/null 2>&1; then
    ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" \
      "rm -f $(shq "$REMOTE_INCOMING/$base") $(shq "$REMOTE_INCOMING/$base.sha256")" >/dev/null 2>&1 || true
    mark_failed "$c" "$s" verify "remote sha256sum -c mismatch after upload"
    return 0
  fi

  if ! ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" \
        "mv $(shq "$REMOTE_INCOMING/$base") $(shq "$PUBLISH_DIR/$base") && mv $(shq "$REMOTE_INCOMING/$base.sha256") $(shq "$PUBLISH_DIR/$base.sha256")"; then
    mark_failed "$c" "$s" verify "atomic activation (mv into $PUBLISH_DIR) failed"
    return 0
  fi

  if [ "$VERIFY_PUBLIC" = 1 ]; then
    local url tmp got want
    url="${PUBLISH_BASE_URL%/}/$base"
    # `|| true`: this whole check is optional (off by default) and must
    # never take down the run over a bare mktemp failure; an empty $tmp
    # just makes the curl below fail cleanly, caught by its own if/&&.
    tmp="$(mktemp)" || true
    want="${ITEM_SHA[$i]}"
    if curl -fsSL --retry 2 --connect-timeout 10 -o "$tmp" "$url" 2>/dev/null && got="$(sha256_file "$tmp")" && [ "$got" = "$want" ]; then
      echo "  [$c/$s] --verify-public OK ($url)"
    else
      echo "WARNING: [$c/$s] --verify-public could not confirm $url yet (CDN lag is expected; not fatal)" >&2
    fi
    rm -f "$tmp" || true
  fi

  mark_verified "$c" "$s" "${ITEM_SHA[$i]}" "${ITEM_COMMIT[$i]}" "${ITEM_WHERE[$i]}"
}

REMOTE_INCOMING=""
publish_items() {
  echo "== Publish =="
  local i any=0
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do [ "${ITEM_STATUS[$i]}" = BUILT ] && any=1; done
  [ "$any" = 1 ] || { echo "Nothing passed the sanity gate -- nothing to publish."; return 0; }

  REMOTE_INCOMING="$PUBLISH_DIR/.incoming.$$"
  if [ "$DRY_RUN" = 1 ]; then
    for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
      [ "${ITEM_STATUS[$i]}" = BUILT ] || continue
      echo "  [dry-run] would publish ${ITEM_CLIENT[$i]}/${ITEM_SLUG[$i]} -> $PUBLISH_HOST:$PUBLISH_DIR/$(resolved_filename "${ITEM_CLIENT[$i]}" "${ITEM_SLUG[$i]}")"
    done
    return 0
  fi

  # Guarded explicitly (a hard_abort, not a bare command): this runs once
  # per run, not per-item, so a failure here means NO item can be published
  # -- fatal, like the SSH preflight -- and letting it fall through to a raw
  # `set -e` abort would only produce ssh's bare stderr, not a clear message.
  if ! ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" "mkdir -p $(shq "$REMOTE_INCOMING")"; then
    hard_abort "could not create remote staging dir $REMOTE_INCOMING on $PUBLISH_HOST"
  fi
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_STATUS[$i]}" = BUILT ] || continue
    publish_one_item "$i"
  done
  ssh "${SSH_OPT_ARGS[@]}" "$PUBLISH_USER@$PUBLISH_HOST" \
    "rm -rf $(shq "$REMOTE_INCOMING")" >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# binaries.json update -- python3 heredoc, mirroring lib.sh's own python3
# usage. Only VERIFIED items feed in; never touches _comment fields, never
# removes/reorders existing platforms, never touches an untouched client.
# ---------------------------------------------------------------------------
update_manifest() {
  echo "== Manifest =="
  if [ "$NO_MANIFEST" = 1 ]; then
    echo "Skipping binaries.json update (--no-manifest)."
    return 0
  fi
  local i any=0
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_STATUS[$i]}" = VERIFIED ] && any=1
  done
  if [ "$any" != 1 ]; then
    echo "No VERIFIED items this run; binaries.json left untouched."
    return 0
  fi

  local today tmp verified_tsv
  today="$(date +%F)"
  # Guarded explicitly (update_manifest is called as a plain statement, not
  # from an if/&&/! context): an unguarded mktemp failure here would trip
  # `set -e` and abort via the trap with no clear message, after the
  # publish itself already succeeded.
  tmp="$(mktemp "$BINARIES_JSON.XXXXXX")" || hard_abort "mktemp failed while preparing the binaries.json update"
  verified_tsv="$(mktemp "$STAGING/verified.XXXXXX.tsv")" || hard_abort "mktemp failed while preparing the binaries.json update"

  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    [ "${ITEM_STATUS[$i]}" = VERIFIED ] || continue
    printf '%s\t%s\t%s\n' "${ITEM_CLIENT[$i]}" "${ITEM_SLUG[$i]}" "${ITEM_SHA[$i]}"
  done > "$verified_tsv"

  # NOTE: the verified-items list is passed via a temp file (TSV env var),
  # NOT piped into this heredoc -- `cmd <<'PY'` redirects fd0 to the heredoc
  # itself, so a pipe into the same command would be silently discarded and
  # `sys.stdin` would see the *script text*, not our data.
  #
  # The `if ... ; then ... fi` (rather than checking $? afterwards) is
  # required for this to be `set -e`-safe: a heredoc-fed command is a plain
  # simple command, and errexit would abort the script on a nonzero exit
  # before a separate "capture $?" line ever got to run.
  if BJSON="$BINARIES_JSON" TMP="$tmp" DATE="$today" TSV="$verified_tsv" python3 - <<'PY'
import collections
import json
import os

path = os.environ["BJSON"]
tmp = os.environ["TMP"]
today = os.environ["DATE"]
tsv = os.environ["TSV"]

with open(path) as f:
    manifest = json.load(f, object_pairs_hook=collections.OrderedDict)

touched = set()
with open(tsv) as f:
    for line in f:
        line = line.rstrip("\n")
        if not line:
            continue
        client, slug, sha = line.split("\t")
        c = manifest["clients"][client]
        c.setdefault("sha256", collections.OrderedDict())[slug] = sha
        platforms = c.setdefault("platforms", [])
        if slug not in platforms and "*" not in platforms:
            platforms.append(slug)
        touched.add(client)

manifest["updated"] = today

# Self-check the invariant download_bins() depends on: every platform this
# client claims to support must have a matching sha256 entry (or "*").
for client in touched:
    c = manifest["clients"][client]
    platforms = c.get("platforms") or []
    sha = c.get("sha256") or {}
    for slug in platforms:
        key = slug if slug in sha else ("*" if "*" in sha else None)
        assert key is not None, "invariant broken: %s platform %s has no matching sha256 entry" % (client, slug)

with open(tmp, "w") as f:
    json.dump(manifest, f, indent=2, ensure_ascii=True)
    f.write("\n")

# Re-parse what we just wrote as a self-check before it replaces the original.
with open(tmp) as f:
    json.load(f)

print("touched clients: " + ", ".join(sorted(touched)))
PY
  then
    rm -f "$verified_tsv"
  else
    rm -f "$verified_tsv" "$tmp"
    hard_abort "binaries.json update failed (see the python3 traceback above) -- left the original binaries.json untouched"
  fi

  # Guarded explicitly (not a bare mv): update_manifest is called as a plain
  # statement, so letting this fail unguarded would trip `set -e` after the
  # publish already succeeded, aborting via the trap instead of a clear
  # message here.
  if ! mv "$tmp" "$BINARIES_JSON"; then
    rm -f "$tmp"
    hard_abort "could not move the updated manifest into place at $BINARIES_JSON (published binaries are still live; only the manifest write failed)"
  fi
  echo ""
  echo "binaries.json updated (updated=$today). Diff:"
  ( cd "$REPO_ROOT" && git diff --stat -- binaries.json ) || true
  echo ""
  echo "REMINDER: per-client \"_comment\" fields in binaries.json are human-owned --"
  echo "this script never touches them; update them yourself if the story changed."
  echo "binaries.json was NOT committed -- review the diff and commit it yourself."
}

# ---------------------------------------------------------------------------
# Summary (always printed, even on a partial run) + exit code.
# ---------------------------------------------------------------------------
SUMMARY_PRINTED=0
print_summary() {
  SUMMARY_PRINTED=1
  [ "$PLAN_DONE" = 1 ] || return 0
  echo ""
  echo "== Summary =="
  printf '%-11s %-14s %-16s %-8s %-14s %s\n' CLIENT SLUG STATUS WHERE SHA256 REASON
  local i c s st where sha reason n_ver=0 n_fail=0 n_skip=0 n_other=0
  for (( i = 0; i < ${#ITEM_CLIENT[@]}; i++ )); do
    c="${ITEM_CLIENT[$i]}"; s="${ITEM_SLUG[$i]}"; st="${ITEM_STATUS[$i]}"
    where="${ITEM_WHERE[$i]:--}"; sha="${ITEM_SHA[$i]:-}"; reason="${ITEM_REASON[$i]:-}"
    printf '%-11s %-14s %-16s %-8s %-14s %s\n' "$c" "$s" "$st" "$where" "${sha:0:12}" "$reason"
    case "$st" in
      VERIFIED) n_ver=$((n_ver + 1)) ;;
      FAILED*)  n_fail=$((n_fail + 1)) ;;
      SKIPPED)  n_skip=$((n_skip + 1)) ;;
      *)        n_other=$((n_other + 1)) ;;
    esac
  done
  echo ""
  echo "verified=$n_ver failed=$n_fail skipped=$n_skip other=$n_other (of ${#ITEM_CLIENT[@]} planned)"
  # `if`, not `[ ... ] && echo`: this is the last statement in the function,
  # so with no warnings the false test became print_summary's exit status.
  # main() calls it bare under `set -e`, so EVERY clean run aborted here and
  # exited 1 -- never reaching `exit "$(final_exit_code)"`, which would have
  # returned 0. A fully successful publish looked like a failure to any
  # caller checking the exit code.
  if [ "$HAD_WARNING" = 1 ]; then
    echo "note: one or more requested platforms were dropped at plan time (see WARNINGs above)"
  fi
}

final_exit_code() {
  if [ "$HAD_FAILURE" = 1 ] || [ "$HAD_SKIP" = 1 ]; then
    echo 2
  elif [ "$STRICT" = 1 ] && [ "$HAD_WARNING" = 1 ]; then
    echo 2
  else
    echo 0
  fi
}

# ---------------------------------------------------------------------------
# Cleanup trap: kills any in-flight docker containers we started, removes
# the remote per-run .incoming.$$ dir (best-effort), prints the summary if
# main() hasn't already (e.g. we're unwinding from an interrupt), then exits
# with whatever code triggered the trap (or main()'s own explicit exit).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via `trap cleanup_and_exit ...` below, not a direct call
cleanup_and_exit() {
  local exit_code=$?
  trap - EXIT INT TERM
  local c
  for c in "${DOCKER_CIDS[@]-}"; do
    [ -n "$c" ] || continue
    docker rm -f "$c" >/dev/null 2>&1 || true
  done
  if [ -n "$REMOTE_INCOMING" ] && [ "$SSH_REACHABLE" = 1 ]; then
    ssh "${SSH_OPT_ARGS[@]-}" "$PUBLISH_USER@$PUBLISH_HOST" "rm -rf $(shq "$REMOTE_INCOMING")" >/dev/null 2>&1 || true
  fi
  if [ "$SUMMARY_PRINTED" = 0 ]; then
    print_summary || true
  fi
  exit "$exit_code"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
SSH_OPT_ARGS=()

main() {
  parse_args "$@"
  check_required_tools
  validate_clients_arg

  # --dry-run is exempt: it has no side effects, so demanding the production
  # publish target before it will even describe itself made the header's own
  # first usage example (`./publish-bins.sh --dry-run`) hard-abort on any
  # checkout without a publish.env. Reviewing the plan is exactly what you
  # want to do BEFORE configuring where things get written. Same
  # MODE+DRY_RUN pairing the ssh_preflight/update_manifest gates below use.
  if [ "$MODE" != build-only ] && [ "$DRY_RUN" != 1 ] && [ -z "$PUBLISH_DIR" ]; then
    hard_abort "PUBLISH_DIR is not set -- copy publish.env.sample to publish.env and set it (the remote web root serving $PUBLISH_BASE_URL), or pass --build-only."
  fi

  [ -n "$SSH_KEY_ARG" ] && PUBLISH_SSH_KEY="$SSH_KEY_ARG"
  SSH_OPT_ARGS=(-o BatchMode=yes -o PasswordAuthentication=no -p "$PUBLISH_PORT")
  if [ -n "$PUBLISH_SSH_KEY" ]; then
    SSH_OPT_ARGS+=(-o IdentitiesOnly=yes -i "$PUBLISH_SSH_KEY")
  fi

  echo "publish-bins.sh: mode=$MODE clients=${CLIENTS_ARG:-$DEFAULT_CLIENTS} platforms=${PLATFORMS_ARG:-<per-client default>} dry-run=$DRY_RUN strict=$STRICT"

  setup_staging
  plan_items

  if [ "$MODE" != build-only ] && [ "$DRY_RUN" != 1 ]; then
    ssh_preflight   # before any build, so a broken remote config fails fast
  fi

  if [ "$MODE" = publish-only ]; then
    resume_from_ledger
  else
    build_items
    checksum_and_sanity
  fi

  if [ "$MODE" != build-only ]; then
    confirm_publish
    publish_items
  fi

  if [ "$MODE" != build-only ] && [ "$DRY_RUN" != 1 ]; then
    update_manifest
  fi

  print_summary
  local code; code="$(final_exit_code)"
  exit "$code"
}

trap cleanup_and_exit EXIT INT TERM
main "$@"
