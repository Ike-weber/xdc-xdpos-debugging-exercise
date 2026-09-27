#!/bin/bash
# Shared helpers, sourced by the other scripts. Keeps everything OS-agnostic
# (macOS / Linux) by resolving platform-specific binaries and network info.

_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Platform key, e.g. Darwin-arm64, Linux-x86_64, Linux-aarch64
PLATFORM="$(uname -s)-$(uname -m)"

# Prebuilt-binary manifest (repo root); see download_bins() below.
BINARIES_JSON="$_LIB_DIR/binaries.json"

# Where each client's binary comes from, default "download" (fetch a prebuilt
# binary from binaries.json for this platform; falls back to a source build
# if unavailable). Set BIN_SOURCE=build to always build from source, or
# override a single client with BIN_SOURCE_<client>= (e.g. BIN_SOURCE_erigon=build).
BIN_SOURCE="${BIN_SOURCE:-download}"

# Which client to build/run:
#   oldxdc  (default) -> XDPoSChain fork (binary: XDC + bootnode + puppeth)
#   geth              -> modern go-ethereum fork (binary: geth)
#   erigon            -> erigon fork (binary: erigon) -- joins built-in XDC
#                        chains via --chain, not a custom genesis file
CLIENT="${CLIENT:-oldxdc}"
case "$CLIENT" in
  oldxdc|xdc|XDC)
    CLIENT=oldxdc
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/OLDXDC}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../XDPoSChain}"
    CLIENT_TARGETS="XDC bootnode puppeth"
    NODE_NAME_BIN="XDC"
    ;;
  geth|modern|gp5)
    CLIENT=geth
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/go-ethereum}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../go-ethereum}"
    CLIENT_TARGETS="geth"
    NODE_NAME_BIN="geth"
    ;;
  geth4)
    # geth 1.17.4-xdc.7, PINNED. `geth` now tracks the base branch (1.17.5-xdc.8),
    # so this token exists for anyone who needs to stay on -- or bisect against --
    # 1.17.4. Same repo and make target (it is geth at an older tag) but a separate
    # binaries.json key and its own BIN_DIR, so the two coexist.
    #
    # This slot was `geth1175` back when `geth` meant 1.17.4 and 1.17.5 was the
    # opt-in. Now that geth IS 1.17.5 that token would be an exact duplicate, so it
    # is retired and the arm repurposed for the older pin. The deprecation arm below
    # gives `--client geth1175` a real error instead of silently resolving to
    # something it no longer means.
    CLIENT=geth4
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/go-ethereum}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../go-ethereum}"
    CLIENT_TARGETS="geth"
    NODE_NAME_BIN="geth"
    ;;
  erigon)
    CLIENT=erigon
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/erigon-xdc}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../erigon-xdc}"
    CLIENT_TARGETS="erigon"
    NODE_NAME_BIN="erigon"
    ;;
  besu)
    # Prebuilt (Java): located by join.sh via BESU_BIN / PATH, not built here.
    CLIENT=besu
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/besu-xdc}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../besu-xdc}"
    CLIENT_TARGETS=""
    NODE_NAME_BIN="besu"
    ;;
  nethermind)
    # Prebuilt (.NET): located by join.sh via NETHERMIND_DIST / PATH, not built here.
    CLIENT=nethermind
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/nethermind}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../nethermind}"
    CLIENT_TARGETS=""
    NODE_NAME_BIN="nethermind"
    ;;
  reth)
    # Prebuilt (Rust): located by join.sh via RETH_BIN / PATH, not built here.
    CLIENT=reth
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/reth-xdc}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../reth-xdc}"
    CLIENT_TARGETS=""
    NODE_NAME_BIN="xdc-reth"
    ;;
  xone)
    # xone-native (XDCIndia/xOneGo): the go-ethereum-derived XDPoS client, the
    # reference mainnet-parity node. Prebuilt is downloaded (binaries.json xone);
    # a source build is the fallback when present. Uses single-dash flags.
    CLIENT=xone
    XDPOS_REPO="${XDPOS_REPO:-https://github.com/XDCIndia/xOneGo}"
    XDPOS_DIR="${XDPOS_DIR:-$_LIB_DIR/../xOneGo}"
    CLIENT_TARGETS="xone"
    NODE_NAME_BIN="xone"
    ;;
  all)
    # Meta-client: join.sh fans "all" out to one child process per real client
    # (each re-sources lib.sh with its own concrete client). The dispatcher itself
    # needs no per-client repo / binary setup, so accept it and set no targets.
    CLIENT=all
    CLIENT_TARGETS=""
    NODE_NAME_BIN=""
    ;;
  geth1175)
    # RETIRED. This meant "geth 1.17.5, opt-in" back when `geth` was 1.17.4.
    # `geth` now IS 1.17.5, so the token had no distinct meaning and silently
    # resolving it would hand someone a different build than they asked for.
    # Fail with the mapping instead.
    echo "CLIENT 'geth1175' has been retired: \`geth\` now tracks 1.17.5-xdc.8." >&2
    echo "  use --client geth    for 1.17.5 (what geth1175 used to give you)" >&2
    echo "  use --client geth4   to stay pinned to 1.17.4-xdc.7" >&2
    exit 1;;
  *)
    echo "unknown CLIENT '$CLIENT' (use: oldxdc | geth | geth4 | erigon | besu | nethermind | reth | xone | all)" >&2; exit 1;;
esac

# Binaries live under a per-client, per-platform dir so the clients coexist.
BIN_DIR="$_LIB_DIR/bin/$CLIENT/$PLATFORM"
NODE_BIN="$BIN_DIR/$NODE_NAME_BIN"   # the node binary (XDC / geth / erigon)
# shellcheck disable=SC2034  # XDC_BIN/BOOTNODE_BIN/PUPPETH_BIN are used by scripts that `source ./lib.sh`
XDC_BIN="$NODE_BIN"                  # back-compat alias used by the scripts
# shellcheck disable=SC2034  # used by scripts that `source ./lib.sh` (e.g. join.sh)
BOOTNODE_BIN="$BIN_DIR/bootnode"     # only exists for the oldxdc client
# shellcheck disable=SC2034  # used by scripts that `source ./lib.sh` (e.g. gen-genesis.sh)
PUPPETH_BIN="$BIN_DIR/puppeth"       # only exists for the oldxdc client

# Best-effort LAN IP on macOS or Linux (falls back to 127.0.0.1).
#
# Ask the ROUTING TABLE which interface actually carries traffic, rather than
# guessing en0-then-en1. The guess is not merely imprecise, it silently breaks
# the whole network: every node is started with `--nat extip:$(detect_ip)`, so
# an address that is real-but-not-current makes all of them advertise an
# unreachable endpoint. Measured on a Mac that had just moved between a hotspot
# and wifi: en0 momentarily had no address, so the old `en0 || en1` fallback
# returned en1's 192.168.1.107 while the live address was 172.20.10.3. All four
# sealers then came up with net_peerCount=0, could not see each other, and the
# chain deadlocked at block 5 in XDPoS V1 with "Signed recently, must wait for
# others" (only 2 of 4 masternodes ever taking a slot). It reads as a consensus
# bug and is really a one-line IP bug, so resolve it deterministically.
detect_ip() {
  local ip="" _if=""
  # macOS: `route get default` names the interface the default route uses;
  # ask ipconfig for THAT interface instead of assuming en0/en1.
  if command -v route >/dev/null 2>&1 && command -v ipconfig >/dev/null 2>&1; then
    _if=$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}')
    [ -n "$_if" ] && ip=$(ipconfig getifaddr "$_if" 2>/dev/null)
  fi
  # Linux: the src address the kernel would pick to reach the outside world.
  [ -z "$ip" ] && command -v ip >/dev/null 2>&1 && \
    ip=$(ip -4 route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
  # Fallbacks, in descending order of trustworthiness.
  if [ -z "$ip" ] && command -v ipconfig >/dev/null 2>&1; then
    ip=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null)
  fi
  [ -z "$ip" ] && ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  [ -z "$ip" ] && ip=127.0.0.1
  echo "$ip"
}

# This platform's {os}-{arch} key as used in binaries.json -- distinct from
# $PLATFORM (which is "uname -s-uname -m" / used for BIN_DIR): lowercase os,
# and machine arch normalised to amd64/arm64.
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

# --- node naming -------------------------------------------------------------
# ONE canonical node identity, shared by run.sh (legacy sealers), join.sh
# (followers/joiners) and setup.sh, so a name means the same thing everywhere.
# Shape -- fixed order, every part always present:
#
#   <machine>-<client>-v<version>-<commit>-<nodetype>-<ip>
#
# Each part answers a question you actually ask when a dashboard row misbehaves:
# WHICH box (machine), WHICH implementation (client), WHICH build
# (version + commit), what the node is FOR (nodetype: seal|sync), and WHERE to
# reach it (ip). The commit is the part that earns its keep: two nodes on the
# same released version -- one carrying a merged consensus fix, one predating it
# -- print an identical version string and are indistinguishable on the
# dashboard. That exact ambiguity has repeatedly cost us hours (a nethermind
# artifact 7 days stale still reported the same version as the fixed build).
#
# These live in lib.sh rather than in each script because the naming code used to
# be duplicated three times and drifted; a client added to one list and missed in
# another is this repo's most recurring bug shape.

# The machine component: explicit MACHINE (set from --name) wins, else this
# host's short hostname, else the literal "node" so the name is never malformed.
machine_name() {
  local m="${MACHINE:-}"
  [ -z "$m" ] && m=$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo node)
  echo "${m:-node}"
}

# Sanitise ONE name component. ethstats packs the name into a
# "name:secret@host:port" URL, so ':' '@' '/' would break that parsing -- and
# some clients' --version output legitimately contains '/' (besu prints the one
# token "besu/v<ver>/<os>/<jvm>"). Collapse anything outside [A-Za-z0-9._-] to
# '-', then squeeze runs and trim, so a missing part can never leave "--".
name_part() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '-' | sed -E 's/-+/-/g; s/^-+//; s/-+$//'
}

# Compose the full name. Args: machine client version commit nodetype ip.
# Any empty part falls back to a visible placeholder rather than collapsing the
# shape -- a name with "nocommit" in it tells you the build lacked git info,
# whereas a silently shortened name just looks like a different scheme.
node_name() {
  local m c v k t i
  m=$(name_part "${1:-$(machine_name)}")
  c=$(name_part "${2:-unknown}")
  v=$(name_part "${3:-0}")
  k=$(name_part "${4:-nocommit}")
  t=$(name_part "${5:-sync}")
  # IP dots -> dashes so the address reads as one token in the name (dots are
  # legal here, but mixing them with the version's dots makes the name hard to
  # split by eye on a dashboard row).
  i=$(name_part "$(printf '%s' "${6:-0.0.0.0}" | tr '.' '-')")
  echo "${m}-${c}-v${v}-${k}-${t}-${i}"
}

# --- local-topology defaults + run manifest (ItWorksinMyLocal#181) ---------
# run.sh's local net accepts BASE_P2P_PORT/BASE_RPC_PORT/BASE_WS_PORT/
# BOOTNODE_PORT/ALL_RPC_BASE/ALL_PORT_STRIDE/ALL_CLIENTS overrides so it can be
# moved off busy ports. status.sh has to report the SAME topology, which means
# it needs the SAME defaults -- previously it kept its own copy of a subset of
# them (and simply lacked the rest), so a run started with overrides was
# reported as entirely DOWN by a bare ./status.sh (#181: the network tip read
# 0 because status.sh silently probed the wrong ports).
#
# These are ONLY the fallback used when neither the manifest below nor an
# explicit environment variable says otherwise -- run.sh's own port math is
# unchanged; this just names the same literals once instead of twice.
DEFAULT_BASE_P2P_PORT=30302
DEFAULT_BASE_RPC_PORT=8544
DEFAULT_BASE_WS_PORT=8554
DEFAULT_BOOTNODE_PORT=30301
DEFAULT_ALL_PORT_STRIDE=10
DEFAULT_ALL_P2P_STRIDE=10
DEFAULT_NUM_NODES=4
# run.sh --all's default modern-client set. Shared so status.sh's "Modern
# Followers" section falls back to the exact same list run.sh would launch,
# rather than a second hand-maintained copy that can drift from it (as the
# in-repo one did: it was missing geth4 until fixed here).
DEFAULT_ALL_CLIENTS="geth geth4 erigon xone besu nethermind reth"

# --all follower port helpers -- ONE definition, used by run.sh's pre-flight
# check, its launch loop, AND status.sh's "Modern Followers" report. All three
# used to compute this arithmetic independently, which is exactly how they
# were free to disagree (and did: status.sh's copy went stale more than once).
# Depend on ALL_RPC_BASE/ALL_PORT_STRIDE being set in the caller's shell
# (by run.sh directly, or by status.sh via the manifest/defaults below).
all_follower_rpc()  { echo $(( ALL_RPC_BASE + ($1 - 5) * ALL_PORT_STRIDE )); }
all_follower_ws()   { echo $(( $(all_follower_rpc "$1") + 5 )); }
all_follower_auth() { echo $(( $(all_follower_rpc "$1") + 7 )); }

# The manifest's field set. ONE list, so the writer (run.sh, below) and the
# reader (status.sh) can never disagree about what the file contains.
TOPOLOGY_MANIFEST_KEYS="BASE_P2P_PORT BASE_RPC_PORT BASE_WS_PORT BOOTNODE_PORT ALL_RPC_BASE ALL_PORT_STRIDE ALL_P2P_STRIDE NUM_NODES MIXED ALL CHAINID ALL_SELECTED MACHINE_LABEL"

# write_topology_manifest <path>: record the topology THIS run.sh invocation
# actually resolved (after every override/adjustment -- e.g. the erigon
# BASE_P2P_PORT bump, or a --all client dropped for a missing binary) into a
# small sourceable file, so a later bare ./status.sh (run by anyone, in any
# shell, any time later) reports on the topology that is actually running
# instead of today's hardcoded guesses. Per-run local state: not the ports a
# future run.sh call will necessarily reuse, so it belongs in nodes/ (already
# gitignored) and is never read by run.sh itself.
#
# Plain `KEY=value` lines via printf %q, so `. ./nodes/.topology` (status.sh's
# side) restores ALL_SELECTED's embedded spaces correctly -- a naive
# `IFS='=' read` parse would not.
write_topology_manifest() {
  local path="$1" k v
  mkdir -p "$(dirname "$path")" 2>/dev/null
  {
    echo "# Written by run.sh -- the topology THIS invocation actually used."
    echo "# Sourced by status.sh (ItWorksinMyLocal#181) so a bare ./status.sh"
    echo "# reports it without re-passing the same overrides given to"
    echo "# setup.sh/run.sh. An explicit environment variable at status.sh's"
    echo "# own invocation still wins over any value below."
    echo "# Per-run local state -- gitignored (nodes*); do not commit, do not"
    echo "# hand-edit (run ./run.sh again to regenerate it)."
    for k in $TOPOLOGY_MANIFEST_KEYS; do
      v="${!k:-}"
      printf '%s=%q\n' "$k" "$v"
    done
  } > "$path"
}

# `timeout` is GNU coreutils and is absent on stock macOS, which this harness
# also targets (see platform_slug). Wrap version queries so a wedged binary can
# never hang a launch, WITHOUT making the query fail outright where timeout does
# not exist -- a failed query would silently degrade every node name to
# "v0-nocommit", which looks like a naming bug rather than a missing tool.
_vquery() {
  if command -v timeout >/dev/null 2>&1; then timeout 20 "$@"; else "$@"; fi
}

# Resolve this client's build identity. Echoes "<version> <commit>"; commit is
# the literal "nocommit" when the build carries no git metadata -- a real state
# for shipped artifacts (geth4 and besu both lack it today), not an error.
#
# Every client formats its version differently and TWO are booby-trapped:
# xone's `version` SUBCOMMAND boots a full node (unknown subcommand -> node
# start) and hangs, and nethermind's bare `version` starts loading plugins
# before failing argument parsing. Both MUST use --version. oldxdc and the geth
# family are the only ones whose `version` subcommand is safe, and it is also
# their cleanest source: it prints "Git Commit:" on a line of its own.
client_version_commit() {
  local bin="${1:-$CLIENT_BIN}" out ver="" commit="" tok
  case "$CLIENT" in
    oldxdc|geth|geth4)
      out=$(_vquery "$bin" version 2>/dev/null)
      ver=$(printf '%s\n' "$out" | awk '/^Version:/{print $2; exit}')
      commit=$(printf '%s\n' "$out" | awk '/^Git Commit:/{print substr($3,1,8); exit}')
      ;;
    reth)
      out=$(_vquery "$bin" --version 2>/dev/null)
      ver=$(printf '%s\n' "$out" | awk '/^Reth Version:/{print $NF; exit}')
      commit=$(printf '%s\n' "$out" | awk '/^Commit SHA:/{print substr($NF,1,8); exit}')
      ;;
    nethermind)
      out=$(_vquery "$bin" --version 2>/dev/null)
      # Version line carries the commit twice over: as a "+<hash>" build-metadata
      # suffix here, and in full on its own Commit: line. Strip the suffix so the
      # version part stays a version.
      ver=$(printf '%s\n' "$out" | awk '/^Version:/{print $2; exit}' | sed -E 's/\+[0-9a-f]+$//')
      commit=$(printf '%s\n' "$out" | awk '/^Commit:/{print substr($2,1,8); exit}')
      ;;
    xone)
      # "xone v<ver> (commit <hash>)" -- the commit is PARENTHESISED, not glued as
      # a suffix, so it needs its own rule. Do NOT chain greps to pull it out:
      # `grep -oE '[0-9a-f]+'` also matches the 'c' in the literal word "commit"
      # (a valid 1-char hex run) and returns that instead of the hash.
      out=$(_vquery "$bin" --version 2>/dev/null | head -1)
      ver=$(printf '%s\n' "$out" | awk '{print $2}' | sed 's/^v//')
      commit=$(printf '%s\n' "$out" | sed -nE 's/.*\(commit ([0-9a-f]{7,}).*/\1/p' | cut -c1-8)
      ;;
    *)
      # erigon ("erigon version 3.5.2" or "... 3.5.2-<commit>") and besu (ONE
      # slash-delimited token "besu/v<ver>/<os>/<jvm>", no spaces, so $NF would
      # grab the lot). Isolate the version token per client, then ONE shared rule
      # splits a glued "-<commit>" suffix off it. Builds made without git info
      # print the bare version and fall through to nocommit -- as does besu,
      # whose "xxxxxxxx" is a literal placeholder and correctly fails the hex test.
      case "$CLIENT" in
        besu) tok=$(_vquery "$bin" --version 2>/dev/null | head -1 | sed -E 's#^besu/v?([^/]+).*#\1#') ;;
        *)    tok=$(_vquery "$bin" --version 2>/dev/null | head -1 | awk '{print $NF}') ;;
      esac
      commit=$(printf '%s' "$tok" | grep -oE '[0-9a-f]{7,8}$')
      ver=$(printf '%s' "$tok" | sed -E 's/-[0-9a-f]{7,8}$//')
      ;;
  esac
  echo "${ver:-0} ${commit:-nocommit}"
}

# Apply the puppeth genesis-storage fix to the XDPoSChain source (idempotent).
# Upstream's cmd/puppeth/wizard_genesis.go re-RLP-decodes each storage word,
# truncating masternode addresses in the 0x88 validator contract and stalling
# the chain at the first epoch. We store the word verbatim instead.
patch_puppeth() {
  local f="$XDPOS_DIR/cmd/puppeth/wizard_genesis.go"
  [ -f "$f" ] || return 0
  python3 - "$f" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'rlp.DecodeBytes' not in s:
    sys.exit(0)  # already patched (or upstream changed shape)
s2 = re.sub(
    r'\n[ \t]*decode := \[\]byte\{\}\n.*?log\.Info\("DecodeBytes".*?\)\n',
    '\n\t\t\tstorage[key] = val\n',
    s, count=1, flags=re.DOTALL)
s2 = re.sub(r'\n[ \t]*"github\.com/XinFinOrg/XDPoSChain/rlp"', '', s2, count=1)
if 'rlp.DecodeBytes' in s2:
    sys.stderr.write(
      "WARNING: could not apply the puppeth genesis fix automatically.\n"
      "         Your XDPoSChain source differs from the expected version.\n"
      "         Pin it first:  (cd \"$XDPOS_DIR\" && git checkout 68b00c7e5)\n")
    sys.exit(1)
open(p, 'w').write(s2)
sys.stderr.write("Applied puppeth genesis-storage fix to wizard_genesis.go\n")
PY
}

# sha256 of a file, using whichever tool this host has (sha256sum is typical
# on Linux, shasum -a 256 on macOS; some hosts have both, either works).
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

# Read binaries.json for $1=client/$2=platform-slug and print bash assignments
# to eval: _BJ_OK=1 (only when there's a manifest entry supporting this
# platform), _BJ_URL/_BJ_FILE/_BJ_ARCHIVE/_BJ_SHA256, plus one
# `_bj_produce NAME RELPATH` call per entry in that client's "produces" map.
_binjson_query() {
  local client="$1" slug="$2"
  python3 - "$BINARIES_JSON" "$client" "$slug" <<'PY'
import json, sys
path, client, slug = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    m = json.load(open(path))
except Exception:
    sys.exit(0)  # caller checks _BJ_OK; no valid manifest
c = (m.get("clients") or {}).get(client)
if not c:
    sys.exit(0)
platforms = c.get("platforms") or []
if slug not in platforms and "*" not in platforms:
    sys.exit(0)
os_, arch = slug.split("-", 1)
file = (c.get("file") or "").replace("{os}", os_).replace("{arch}", arch)
archive = c.get("archive", "none")
sha = (c.get("sha256") or {}).get(slug) or (c.get("sha256") or {}).get("*") or ""
# Per-client baseUrl override: almost every client is mirrored under the
# single global xdc.network baseUrl; a client entry may set its own "baseUrl"
# to pull from a different (public) mirror instead. Absent that, fall back to
# the global one.
base = (c.get("baseUrl") or m.get("baseUrl") or "").rstrip("/")
# ghRelease (ItWorksinMyLocal#114): an urgent fix can land as a release asset
# on a client's own (often PRIVATE) GitHub repo before it's mirrored to
# xdc.network -- e.g. xone's genesis-hash-parity build. {"repo":"owner/name",
# "tag":"vX"} tells download_bins() to fetch $file via `gh release download`
# (which carries the caller's own gh auth, unlike a plain curl URL) when the
# ordinary curl fetch fails. Both may be present; curl (against the possibly-
# stale xdc.network mirror) is always tried first, gh is the fallback.
gh = c.get("ghRelease") or {}

def q(s):
    return "'" + str(s).replace("'", "'\\''") + "'"

print("_BJ_OK=1")
print("_BJ_URL=" + q(base + "/" + file if base else ""))
print("_BJ_FILE=" + q(file))
print("_BJ_GH_REPO=" + q(gh.get("repo", "")))
print("_BJ_GH_TAG=" + q(gh.get("tag", "")))
# noMirror: this client is published ONLY as a private GitHub release asset, so
# its mirror URL is known-absent and must not be attempted -- doing so emits a
# bare `curl: (22) 404` as the first output a newcomer ever sees (#147).
print("_BJ_NO_MIRROR=" + q("1" if c.get("noMirror") else ""))
print("_BJ_ARCHIVE=" + q(archive))
print("_BJ_SHA256=" + q(sha))
for k, v in (c.get("produces") or {}).items():
    print("_bj_produce " + q(k) + " " + q(v))
PY
}

# Download this $CLIENT's prebuilt binary for the current platform from
# binaries.json, verify its sha256 (mandatory -- refuses to proceed without a
# recorded hash, and deletes the file on mismatch), extract it if it's an
# archive, and place the resulting binary(ies) at $BIN_DIR/<name> (chmod +x,
# plus an ad-hoc macOS codesign so Gatekeeper doesn't SIGKILL it).
#
# Idempotent: a cached download that still matches its recorded sha256 is
# reused as-is; REBUILD=1 forces a fresh fetch. Returns 1 (after a clear
# message) if this client has no manifest entry, no prebuilt binary for this
# platform, or the download/verification fails -- callers should fall back to
# a source build.
#
# On success, sets DOWNLOADED_DIST to the extraction directory for archive
# clients whose "produces" path is "." (a whole-directory product, e.g.
# nethermind's dist, as opposed to a single extracted binary).
download_bins() {
  # shellcheck disable=SC2034  # DOWNLOADED_DIST is used by scripts that `source ./lib.sh` (e.g. join.sh)
  DOWNLOADED_DIST=""
  [ -f "$BINARIES_JSON" ] || { echo "binaries.json not found at $BINARIES_JSON" >&2; return 1; }
  command -v python3 >/dev/null 2>&1 || { echo "python3 is required to read binaries.json" >&2; return 1; }
  command -v curl >/dev/null 2>&1 || { echo "curl is required to download prebuilt binaries" >&2; return 1; }

  local slug; slug=$(platform_slug)
  local _BJ_OK="" _BJ_URL="" _BJ_FILE="" _BJ_ARCHIVE="" _BJ_SHA256="" _BJ_GH_REPO="" _BJ_GH_TAG=""
  local _BJ_PRODUCE_NAMES=() _BJ_PRODUCE_PATHS=()
  _bj_produce() { _BJ_PRODUCE_NAMES+=("$1"); _BJ_PRODUCE_PATHS+=("$2"); }
  eval "$(_binjson_query "$CLIENT" "$slug")"

  if [ "$_BJ_OK" != 1 ]; then
    echo "binaries.json: no prebuilt $CLIENT binary for platform $slug" >&2
    return 1
  fi
  if [ -z "$_BJ_SHA256" ]; then
    echo "binaries.json: no sha256 recorded for $CLIENT/$slug -- refusing to trust an unverifiable download" >&2
    return 1
  fi

  mkdir -p "$BIN_DIR"
  local dl="$BIN_DIR/.download-$_BJ_FILE"

  if [ "$REBUILD" != "1" ] && [ -f "$dl" ] && [ "$(sha256_file "$dl" 2>/dev/null)" = "$_BJ_SHA256" ]; then
    echo "$CLIENT: cached download for $slug already verified ($_BJ_FILE)"
  else
    rm -f "$dl"
    local verified=""
    # --retry rides out transient failures; --speed-time/-limit aborts a truly
    # stalled connection (no bytes for 60s) instead of hanging forever; -C -
    # resumes a partial temp from a prior interrupted attempt.
    #
    # "verified" only becomes true once a fetched file's sha256 actually
    # matches -- a curl fetch can succeed (HTTP-wise) against a mirror that's
    # simply serving a STALE build (e.g. xdc.network hasn't caught up with an
    # urgent fix yet), which must NOT short-circuit the gh-release fallback
    # below (ItWorksinMyLocal#114 caught this: an earlier version of this
    # function treated "curl exited 0" as success and never tried gh at all).
    # A client whose manifest entry sets "noMirror": true is published ONLY as a
    # private GitHub release asset, so the mirror URL is known-absent. Attempting
    # it just prints a raw `curl: (22) 404` as the very first thing a newcomer
    # sees, which reads like the tool is broken. Skip straight to the gh fallback.
    # (Note $_BJ_FILE is still needed below as the gh asset pattern, so the
    # manifest cannot simply omit "file".)
    if [ "$_BJ_NO_MIRROR" = "1" ] && [ -n "$_BJ_GH_REPO" ]; then
      echo "$CLIENT: published only as a private GitHub release asset; skipping the mirror URL"
    elif [ -n "$_BJ_URL" ]; then
      echo "Downloading $CLIENT for $slug: $_BJ_URL"
      if curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 20 \
              --speed-time 60 --speed-limit 1024 -C - -o "$dl" "$_BJ_URL"; then
        if [ "$(sha256_file "$dl" 2>/dev/null)" = "$_BJ_SHA256" ]; then
          verified=1
        else
          echo "  sha256 mismatch from $_BJ_URL (mirror may be stale)" >&2
          rm -f "$dl"
        fi
      fi
    fi
    # gh-release fallback (ItWorksinMyLocal#114): a client entry's "ghRelease"
    # names an asset on that client's own (possibly PRIVATE) GitHub repo --
    # curl against a plain URL 404s on a private repo even with a bearer
    # token (GitHub serves release assets via a signed redirect that needs
    # the gh CLI's own handling), so this shells out to `gh release download`
    # instead, which carries the caller's own `gh auth` session. Only
    # attempted if curl above didn't already produce a verified file, and
    # only if `gh` is on PATH -- silently unavailable (not a hard error) for
    # anyone without gh or without access to that private repo, since the
    # normal xdc.network mirror is still the primary, documented path.
    if [ -z "$verified" ] && [ -n "$_BJ_GH_REPO" ] && [ -n "$_BJ_GH_TAG" ] && command -v gh >/dev/null 2>&1; then
      echo "$CLIENT: no verified curl fetch; trying gh release download $_BJ_GH_TAG -R $_BJ_GH_REPO -p $_BJ_FILE" >&2
      rm -f "$dl"
      if gh release download "$_BJ_GH_TAG" -R "$_BJ_GH_REPO" -p "$_BJ_FILE" -O "$dl" --clobber 2>&1 \
         && [ "$(sha256_file "$dl" 2>/dev/null)" = "$_BJ_SHA256" ]; then
        verified=1
      else
        echo "  gh release download did not produce a file matching the recorded sha256" >&2
        rm -f "$dl"
      fi
    fi
    if [ -z "$verified" ]; then
      # Distinguish "attempted and failed" from "NOT ATTEMPTED because the tool is
      # missing". The old message listed the gh fallback under "tried" even when gh
      # was absent, so a reader concluded the release asset was broken and went
      # hunting for it, when the real answer was "install gh". That wording cost a
      # newcomer their whole first run (ItWorksinMyLocal#147).
      local _tried="" _notattempted=""
      [ -n "$_BJ_URL" ] && _tried="curl $_BJ_URL"
      if [ -n "$_BJ_GH_REPO" ]; then
        if command -v gh >/dev/null 2>&1; then
          _tried="${_tried:+$_tried, }gh release $_BJ_GH_REPO@$_BJ_GH_TAG"
        else
          _notattempted="gh release $_BJ_GH_REPO@$_BJ_GH_TAG (SKIPPED: the gh CLI is not installed)"
        fi
      fi
      echo "download failed for $CLIENT/$slug ($_BJ_FILE): nothing produced a file matching the recorded sha256." >&2
      [ -n "$_tried" ] && echo "  tried:         $_tried" >&2
      if [ -n "$_notattempted" ]; then
        echo "  NOT attempted: $_notattempted" >&2
        echo "  => This client is distributed as a PRIVATE GitHub release asset, so gh is" >&2
        echo "     required, not optional. Install https://cli.github.com/ then run:" >&2
        echo "       gh auth login" >&2
        echo "     A source build is NOT an alternative here: the client source repos are" >&2
        echo "     private too, so cloning them needs the same credentials." >&2
      fi
      rm -f "$dl"
      return 1
    fi
    local got; got=$(sha256_file "$dl")
    echo "  sha256 OK ($got)"
  fi

  local distdir="$BIN_DIR/.dist-$CLIENT"
  if [ "$_BJ_ARCHIVE" = tar.gz ]; then
    rm -rf "$distdir"; mkdir -p "$distdir"
    tar xzf "$dl" -C "$distdir" || { echo "extract failed: $dl" >&2; return 1; }
    # Sign what stays INSIDE the dist. The per-product codesign further down only
    # covers binaries copied out to $BIN_DIR/<name>; both `continue` paths below
    # -- the whole-directory product (produces "." , e.g. nethermind) and the
    # nested launcher (produces "bin/besu") -- skip it, so an arm64 executable
    # extracted from a tarball was never signed at all.
    #
    # On Apple Silicon that is fatal, not cosmetic: an unsigned arm64 Mach-O is
    # SIGKILLed by the kernel on exec. nethermind died as a bare
    # "Killed: 9" from join.sh with no diagnostic, which reads like an OOM or a
    # crash rather than a signature problem. Measured: `codesign -dv` reported
    # "code object is not signed at all"; after an ad-hoc re-sign the same binary
    # runs and reports 1.36.0-unstable+151fe96b on macOS arm64.
    resign_macho_tree "$distdir"
    # Same class of problem, other runtime: a bundled JRE built for another OS/arch
    # would otherwise shadow a perfectly good host JDK. See the function comment.
    neutralise_foreign_jre "$distdir"
  fi

  local i name relpath src_path dest
  for (( i=0; i<${#_BJ_PRODUCE_NAMES[@]}; i++ )); do
    name="${_BJ_PRODUCE_NAMES[$i]}"; relpath="${_BJ_PRODUCE_PATHS[$i]}"
    if [ "$_BJ_ARCHIVE" = none ]; then
      src_path="$dl"
    elif [ -z "$relpath" ] || [ "$relpath" = "." ]; then
      # shellcheck disable=SC2034  # DOWNLOADED_DIST is used by scripts that `source ./lib.sh` (e.g. join.sh)
      DOWNLOADED_DIST="$distdir"   # whole-directory product; nothing to copy
      continue
    else
      src_path="$distdir/$relpath"
      # A launcher nested inside the extracted dist (e.g. besu's bin/besu) resolves
      # its own APP_HOME from its location and needs sibling dirs (lib/) alongside
      # it. Copying just the launcher out to $BIN_DIR orphans it from lib/, so it
      # dies with "Could not find or load main class". Install a thin wrapper at
      # $BIN_DIR/$name that execs the launcher in place instead, keeping join.sh's
      # "$BIN_DIR/<name>" entry point working while preserving lib/ resolution.
      case "$relpath" in
        */*)
          dest="$BIN_DIR/$name"
          abs_src="$(cd "$(dirname "$src_path")" && pwd)/$(basename "$src_path")"
          printf '#!/bin/sh\nexec "%s" "$@"\n' "$abs_src" > "$dest"
          chmod +x "$dest"
          continue ;;
      esac
    fi
    dest="$BIN_DIR/$name"
    cp -f "$src_path" "$dest" || { echo "could not place $name from $src_path" >&2; return 1; }
    chmod +x "$dest"
    # macOS: cp invalidates the binary's ad-hoc code signature, so Gatekeeper
    # SIGKILLs it on exec. Re-sign it ad-hoc so it runs (same as the build path).
    if [ "$(uname -s)" = Darwin ] && command -v codesign >/dev/null 2>&1; then
      codesign -f -s - "$dest" >/dev/null 2>&1 || true
    fi
  done

  echo "$CLIENT: prebuilt binary ready in $BIN_DIR (downloaded for $slug)"
  return 0
}

# Verify the build toolchain is present before attempting a source build.
check_toolchain() {
  local missing=""
  command -v git >/dev/null 2>&1 || missing="$missing git"
  command -v make >/dev/null 2>&1 || missing="$missing make"
  command -v go >/dev/null 2>&1 || missing="$missing go(golang)"
  { command -v gcc >/dev/null 2>&1 || command -v cc >/dev/null 2>&1; } || missing="$missing gcc"
  if [ -z "$missing" ]; then
    # Go must be >= 1.23 (go.mod). Distro packages are often older.
    local gv maj min
    gv=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | sed 's/go//')
    maj=${gv%%.*}; min=${gv#*.}
    if [ -n "$gv" ] && { [ "${maj:-0}" -lt 1 ] || { [ "$maj" -eq 1 ] && [ "${min:-0}" -lt 23 ]; }; }; then
      echo "Go $gv is too old to build (need >= 1.23)." >&2
      echo "Ubuntu's 'golang' package is often too old -- install a current Go:" >&2
      echo "  wget https://go.dev/dl/go1.23.4.linux-amd64.tar.gz" >&2
      echo "  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-amd64.tar.gz" >&2
      echo "  export PATH=\$PATH:/usr/local/go/bin   # add to ~/.profile" >&2
      exit 1
    fi
    return 0
  fi
  echo "Missing build tools:$missing" >&2
  echo "Install them once, then re-run:" >&2
  echo "  Ubuntu/Debian: sudo apt update && sudo apt install -y build-essential git   (+ Go >=1.23, see below)" >&2
  echo "  Fedora/RHEL:   sudo dnf install -y @development-tools git" >&2
  echo "  macOS:         xcode-select --install && brew install go" >&2
  echo "  Go >= 1.23:    https://go.dev/dl  (distro 'golang' packages are often too old)" >&2
  exit 1
}

# Ensure this client's binaries exist for this platform: prefer downloading a
# prebuilt binary from binaries.json (BIN_SOURCE=download, the default);
# build from source if unavailable, unsupported on this platform, or
# BIN_SOURCE(_<client>)=build. REBUILD=1 always forces a source build.
# Warn ONCE, before any download is attempted, if the single genuinely required
# prerequisite is missing. Every client source repo in this org is private and
# oldxdc -- the DEFAULT client -- ships only as a private release asset, so gh is
# required rather than optional. Without this, a newcomer's first command emitted
# a 404, then a message implying gh had been tried, then advice to install a Go
# toolchain for a source build that also cannot work (ItWorksinMyLocal#147).
# Deliberately a warning, not a hard abort: someone with a fully populated bin/
# (or their own OLDXDC_BIN/BESU_BIN/... overrides) needs no credentials at all.
_PREFLIGHT_DONE=""
preflight_credentials() {
  [ -n "$_PREFLIGHT_DONE" ] && return 0
  _PREFLIGHT_DONE=1
  command -v gh >/dev/null 2>&1 || {
    echo "NOTE: the GitHub CLI (gh) is not installed. This is FINE for the normal path --" >&2
    echo "  every client, including oldxdc, is downloadable anonymously from the public" >&2
    echo "  mirror and verified by sha256, so no GitHub account or org membership is needed." >&2
    echo "  gh is only required if you force a SOURCE BUILD (BIN_SOURCE=build), because the" >&2
    echo "  client source repos are private." >&2
    echo "    if you need it: https://cli.github.com/    then: gh auth login" >&2
    return 0
  }
}

ensure_bins() {
  local t ready=1
  for t in $CLIENT_TARGETS; do [ -x "$BIN_DIR/$t" ] || ready=0; done
  [ "$ready" = 1 ] && [ "${REBUILD:-0}" != "1" ] && return 0
  # Only reached when something actually has to be fetched or built.
  preflight_credentials

  local src="$BIN_SOURCE"
  case "$CLIENT" in
    geth)   src="${BIN_SOURCE_geth:-$src}" ;;
    geth4) src="${BIN_SOURCE_geth4:-$src}" ;;
    erigon) src="${BIN_SOURCE_erigon:-$src}" ;;
    oldxdc) src="${BIN_SOURCE_oldxdc:-$src}" ;;
  esac
  [ "$REBUILD" = "1" ] && src=build

  if [ "$src" != build ]; then
    if download_bins; then
      echo "$CLIENT binaries ready in $BIN_DIR (downloaded)"
      return 0
    fi
    echo "$CLIENT: no usable prebuilt binary for $PLATFORM; falling back to a source build." >&2
  fi

  check_toolchain   # need git + make + go + gcc to build from source

  if [ ! -d "$XDPOS_DIR" ]; then
    echo "No $CLIENT binaries for $PLATFORM yet; cloning source from $XDPOS_REPO ..."
    if ! git clone --depth 1 "$XDPOS_REPO" "$XDPOS_DIR"; then
      echo "Could not clone $XDPOS_REPO (private repo needs access?)." >&2
      echo "Clone the source manually, then re-run:" >&2
      echo "    git clone $XDPOS_REPO $XDPOS_DIR" >&2
      [ "$CLIENT" = oldxdc ] && echo "  or upstream pinned:  git clone https://github.com/XinFinOrg/XDPoSChain $XDPOS_DIR && (cd $XDPOS_DIR && git checkout 68b00c7e5)" >&2
      exit 1
    fi
  fi

  [ "$CLIENT" = oldxdc ] && patch_puppeth   # fix the genesis-storage bug first

  # build/ci.go reads the GOPATH env var directly and fails if it's unset,
  # even though modern Go defaults it. Set it for the build.
  if [ -z "$GOPATH" ]; then GOPATH="$(go env GOPATH 2>/dev/null)"; export GOPATH; fi
  [ -z "$GOPATH" ] && export GOPATH="$HOME/go"
  mkdir -p "$GOPATH"

  echo "Building [$CLIENT_TARGETS] for $CLIENT/$PLATFORM from $XDPOS_DIR (GOPATH=$GOPATH) ..."
  ( cd "$XDPOS_DIR" && for t in $CLIENT_TARGETS; do make "$t" || exit 1; done ) || {
    echo "Build failed. Ensure Go and a C compiler (build-essential) are installed." >&2
    exit 1
  }
  mkdir -p "$BIN_DIR"
  for t in $CLIENT_TARGETS; do
    cp "$XDPOS_DIR/build/bin/$t" "$BIN_DIR/"
    # macOS: cp invalidates the binary's ad-hoc code signature, so Gatekeeper
    # SIGKILLs the copy on exec. Re-sign it ad-hoc so it runs.
    if [ "$(uname -s)" = Darwin ] && command -v codesign >/dev/null 2>&1; then
      codesign -f -s - "$BIN_DIR/$t" >/dev/null 2>&1 || true
    fi
  done
  echo "$CLIENT binaries ready in $BIN_DIR"
}

# Ad-hoc re-sign every Mach-O executable/dylib under a directory (macOS only;
# a no-op everywhere else, and never fatal -- a client that signs fine already
# must not be blocked by a codesign hiccup).
#
# Needed because an arm64 Mach-O with no signature at all is SIGKILLed on exec by
# the kernel on Apple Silicon. Archives published from Linux CI carry no macOS
# signature, so anything run straight out of an extracted dist dies as a bare
# "Killed: 9" with no message pointing at the cause.
#
# Scoped deliberately: only files that are executable or *.dylib, and only those
# whose magic really is Mach-O. A .NET dist like nethermind's ships thousands of
# managed .dll files that are NOT Mach-O -- signing them would be pointless work
# and, on a 336MB tree, slow enough to look like a hang.
resign_macho_tree() {
  local root="$1" f
  [ -d "$root" ] || return 0
  [ "$(uname -s)" = Darwin ] || return 0
  command -v codesign >/dev/null 2>&1 || return 0
  while IFS= read -r f; do
    case "$(file -b "$f" 2>/dev/null)" in
      Mach-O*) codesign -f -s - "$f" >/dev/null 2>&1 || true ;;
    esac
  done < <(find "$root" -type f \( -perm -u+x -o -name '*.dylib' \) 2>/dev/null)
}

# Neutralise a bundled JRE that cannot run on THIS platform, so the launcher
# falls back to host Java instead of dying.
#
# besu ships as one "platform-independent" archive (binaries.json platforms: ["*"])
# whose bin/besu launcher prefers $APP_HOME/jre. The jars really are portable, but
# the bundled runtime is not: it is extracted from the linux/amd64 Temurin image,
# so on macOS jre/bin/java is an "ELF 64-bit LSB executable, x86-64" and the whole
# archive is unusable. Measured here: the launcher fell through to host Java and
# aborted with "Java version must be at least 21 (detected: 8)".
#
# Renaming the unusable runtime aside is the smallest honest fix at THIS layer: it
# does not touch the manifest shape (per-platform besu archives vs a JRE-less one
# is a human decision -- publish-bins.sh header OPEN QUESTION #2, ItWorksinMyLocal
# #159) and it does not pretend the archive is self-contained. It just stops a
# runtime that provably cannot exec here from shadowing one that can, and says so.
neutralise_foreign_jre() {
  local distdir="$1" jre="$1/jre"
  [ -d "$jre" ] || return 0
  [ -x "$jre/bin/java" ] || return 0
  # Exec is the only honest test -- a Mach-O check would still pass for the wrong
  # ARCH, and a fat/thin mismatch fails just as hard as an ELF does.
  "$jre/bin/java" -version >/dev/null 2>&1 && return 0
  local tag; tag="$(uname -s)-$(uname -m)"
  mv "$jre" "$distdir/jre.unusable-on-$tag" 2>/dev/null || return 0
  echo "  note: the archive's bundled jre/ cannot execute on $tag -- moved aside." >&2
  echo "        This client now needs Java 21+ on the host (JAVA_HOME or PATH)." >&2
  echo "        See ItWorksinMyLocal #159 / publish-bins.sh OPEN QUESTION #2." >&2
}

# Collapse a comma-separated list to its first-seen unique entries, order
# preserved. Used for enode lists, where a duplicate is never harmless: geth logs
# "bad bootstrap node" for a repeated bootnode, and a double-counted peer list
# misleads whoever reads it next while debugging a peering problem.
# Pure shell (no python) so it stays usable on a host mid-bootstrap.
dedupe_csv() {
  printf '%s' "$1" | tr ',' '\n' | awk 'NF && !seen[$0]++' | paste -sd, -
}

# The Skynet dashboard URL for a given chain id. Nodes report to the ethstats
# collector (join.sh's ETHSTATS, default stats.xdcindia.com:443); Skynet is the
# front end that renders those reports and selects a network with a ?net=<chainId>
# query. Printed on bring-up so the operator gets a link straight to THIS network
# instead of having to know the convention.
#   $1 chain id -- REQUIRED. Returns 1 rather than emitting a bare base URL,
#   because a link with no ?net= silently shows whichever network the dashboard
#   defaults to, which is worse than no link at all.
# Override the base with SKYNET_BASE_URL (network.env or the environment).
skynet_url() {
  local chainid="$1"
  [ -n "$chainid" ] || return 1
  echo "${SKYNET_BASE_URL:-https://skynet.xdcindia.com/stats}?net=${chainid}"
}

# Guarantee a bootnode is LISTENING on this host, using the repo's static
# ./bootnode.key, and print its enode.
#
# The key has always been static, so the enode is deterministic across restarts
# -- but only run.sh ever actually STARTED a bootnode. join.sh merely *derived*
# the enode from the same key (see its key-based derivation block) and handed it
# to every client, so a join on a host where run.sh was not running pointed all
# of them at an address nothing was bound to. Discovery then silently never
# completed. Starting it here makes "connect to the bootnode first, then
# discover" true for setup.sh, run.sh and join.sh alike.
#
#   $1 ip   $2 port (default 30301)
# Prints the enode on stdout; logs to stderr. Non-fatal: prints nothing and
# returns 1 if no bootnode binary or key is available, so a join degrades to
# whatever static/trusted peers it already has rather than aborting.
ensure_bootnode() {
  local ip="${1:-$(detect_ip)}" port="${2:-30301}" bn pub
  [ -f "$_LIB_DIR/bootnode.key" ] || { echo "  ensure_bootnode: no ./bootnode.key" >&2; return 1; }
  bn="$BOOTNODE_BIN"
  [ -x "$bn" ] || bn=$(ls "$_LIB_DIR"/bin/*/*/bootnode 2>/dev/null | head -1)
  [ -x "$bn" ] || { echo "  ensure_bootnode: no bootnode binary (build the oldxdc client once)" >&2; return 1; }
  pub=$("$bn" -nodekey "$_LIB_DIR/bootnode.key" -writeaddress 2>/dev/null)
  [ -n "$pub" ] || { echo "  ensure_bootnode: could not read the pubkey from ./bootnode.key" >&2; return 1; }

  # Already up? The bootnode is UDP-only (discv4), so check the UDP bind rather
  # than a TCP listener -- lsof -iTCP would always miss it and we would spawn a
  # second one on every join, each fighting for the same port.
  if lsof -nP -iUDP:"$port" >/dev/null 2>&1; then
    echo "  bootnode already listening on :$port (static key)" >&2
  else
    echo "  starting bootnode on ${ip}:${port} (static ./bootnode.key)" >&2
    "$bn" -nodekey "$_LIB_DIR/bootnode.key" -addr "${ip}:${port}" >/dev/null 2>&1 &
    # discv4 binds immediately; a short settle keeps the first dial from racing it.
    sleep 1
  fi
  echo "enode://${pub}@${ip}:${port}"
}

# Harvest live peer enodes from a network by running a short-lived DISCOVERING
# client (oldxdc/geth) against a bootnode, then reading its admin_peers. This is
# how erigon gets its --staticpeers automatically from just an --ip: erigon's
# discv4 doesn't discover through an XDPoSChain bootnode, but oldxdc/geth do
# (exactly what geth does when it joins), so we borrow their discovery.
#   $1 genesis file   $2 bootnode enode   $3 networkid
# Prints a comma-separated enode list on stdout ("" if none). Logs to stderr.
harvest_peers() {
  local genesis="$1" bootnode="$2" nid="$3"
  local xdc="$_LIB_DIR/bin/oldxdc/$PLATFORM/XDC"
  [ -x "$xdc" ] || xdc="$_LIB_DIR/bin/geth/$PLATFORM/geth"
  [ -x "$xdc" ] || { echo "harvest: no oldxdc/geth binary to discover peers" >&2; return 1; }
  local dd rp=8799; dd=$(mktemp -d)
  "$xdc" --datadir "$dd" init "$genesis" >/dev/null 2>&1
  echo "Harvesting peers from bootnode via $(basename "$xdc") discovery ..." >&2
  "$xdc" --datadir "$dd" --networkid "$nid" --syncmode full --bootnodes "$bootnode" \
    --port 30399 --nat "extip:$(detect_ip)" --maxpeers 25 \
    --http --http.addr 127.0.0.1 --http.port "$rp" --http.api admin,net >/dev/null 2>&1 &
  local hpid=$! enodes=""
  for _ in $(seq 1 30); do
    enodes=$(curl -s -m3 "http://127.0.0.1:$rp" -X POST -H 'Content-Type: application/json' \
      --data '{"jsonrpc":"2.0","method":"admin_peers","params":[],"id":1}' 2>/dev/null \
      | python3 -c "import sys,json
try: ps=json.load(sys.stdin).get('result',[]) or []
except Exception: ps=[]
out=['enode://%s@%s'%(p.get('id',''),(p.get('network') or {}).get('remoteAddress','')) for p in ps if p.get('id') and (p.get('network') or {}).get('remoteAddress')]
print(','.join(out))" 2>/dev/null)
    [ -n "$enodes" ] && break
    sleep 2
  done
  kill -9 "$hpid" 2>/dev/null
  rm -rf "$dd"
  echo "$enodes"
}

# The run/genesis/join scripts are wired for the oldxdc (XDPoSChain) client:
# separate bootnode binary, puppeth genesis, XDPoS-v1 flags. The modern geth
# fork uses a different run model (no bootnode binary, --http.* flags, its own
# genesis flow), so those scripts refuse to run against it for now.
require_oldxdc() {
  [ "$CLIENT" = oldxdc ] && return 0
  echo "'$CLIENT' client: this script isn't wired for it yet." >&2
  echo "The modern geth run/genesis profile is still TODO -- CLIENT=geth can" >&2
  echo "build the binary, but launching a local net needs its own flags." >&2
  echo "Use the default (oldxdc) to run a network: unset CLIENT" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# Safe process discovery for teardown (ItWorksinMyLocal#97).
#
# Shared by netlab/node.sh (`stop`/`wipe`/`status`) and netlab/fleet.sh
# (`down`/`wipe`'s post-teardown assertion). Both need the SAME answer to
# "is a real client process still holding this datadir, right now, no
# matter what a pidfile or a listening-port check believes?" -- so it lives
# here once rather than being reimplemented per caller.
#
# Two live incidents drove this:
#   - A node whose RPC never bound (mid-shutdown, wedged, or a bind
#     failure) is INVISIBLE to any resolution that only checks a listening
#     RPC port. On netv12, exactly this let a stale node3 survive a full
#     net wipe for ~40 minutes, still holding its XDCx LOCK -- so the real
#     node3's restart failed outright ("Can't create new DB error=...
#     resource temporarily unavailable"), and the net silently ran on 4 of
#     5 seated validators. Every fair-share number measured in that window
#     was meaningless.
#   - The obvious "just pgrep for the datadir path" fix is worse:
#     `pgrep -f <datadir path>` matches ANY process whose command line
#     contains that path as a plain string -- including the shell running
#     the teardown script itself, and any agent/tool/tmux shell whose own
#     command line happens to mention the same path. That footgun killed a
#     live session (exit 144) during this investigation. NEVER use it.
#
# The safe replacement requires BOTH a cmdline match AND that
# /proc/<pid>/exe actually resolves to a known client binary -- and always
# excludes our own pid and every ancestor pid (the calling shell, its
# parent script, an agent shell, tmux/ssh, ...), so it can never signal or
# report its own caller.
# ---------------------------------------------------------------------------

# client_exe_names <client>: prints (space-separated, possibly more than
# one) the realistic /proc/<pid>/exe basename(s) for a running instance of
# <client> -- what proc_pids_for_datadir below must match against. Most
# clients are one self-contained binary (exe basename == the client name);
# besu and nethermind are launched by their own JVM/.NET runtime, so the
# OS-level exe is actually java/dotnet, not "besu"/"nethermind" -- for
# those two, the cmdline-contains-this-datadir check (always a highly
# specific, per-node absolute path) is what actually disambiguates; the exe
# check there only rules out "some unrelated process whose args happen to
# mention the same path", which is already the point of requiring it.
client_exe_names() {
  case "$1" in
    oldxdc|xdc|XDC) echo "XDC" ;;
    geth)           echo "geth" ;;
    geth4)          echo "geth" ;;
    erigon)         echo "erigon" ;;
    besu)           echo "besu java" ;;
    nethermind)     echo "nethermind dotnet" ;;
    reth)           echo "xdc-reth" ;;
    xone)           echo "xone" ;;
    bootnode)       echo "bootnode" ;;
    *)              echo "" ;;
  esac
}

# join.sh's `--client all` fan-out and per-client default RPC port. ONE
# definition, shared by join.sh (which launches this set) and status.sh
# (which reports a "Join Clients" table) -- previously status.sh kept its own
# 7-entry copy with no geth4, so a `join.sh --client all` run (which starts 8
# clients) was reported as only 7 (ItWorksinMyLocal#181 investigation).
JOIN_ALL_CLIENTS="oldxdc geth erigon besu nethermind reth xone geth4"

# join_client_port_offset <client>: the "O" in join.sh's per-client port
# block (case "$CLIENT" in geth) O=100;; ...). RPC port = 9645 + offset (see
# join_client_rpc_port below); p2p/authrpc/etc. follow the same offset in
# join.sh directly.
join_client_port_offset() {
  case "$1" in
    geth)       echo 100 ;;
    erigon)     echo 200 ;;
    besu)       echo 300 ;;
    nethermind) echo 400 ;;
    reth)       echo 500 ;;
    xone)       echo 600 ;;
    geth4)      echo 700 ;;
    *)          echo 0 ;;
  esac
}

# join_client_rpc_port <client>: default JSON-RPC port join.sh gives this
# client under `--client all` (or bare `--client <c>` with no --rpcport).
join_client_rpc_port() { echo $(( 9645 + $(join_client_port_offset "$1") )); }

# proc_pids_for_datadir <datadir_abspath> [exe_basename ...]: one live pid
# per line -- any process whose /proc/<pid>/cmdline contains
# datadir_abspath as a plain substring AND whose /proc/<pid>/exe resolves
# to one of the given exe basenames (if none given, the exe check is
# skipped -- not used by any caller today, kept only because it's the
# natural degenerate case). Always excludes our own pid and every ancestor
# pid, computed by walking /proc/<pid>/stat's ppid field up to 16 hops
# (far deeper than any realistic shell/script/agent nesting).
proc_pids_for_datadir() {
  local datadir="$1"; shift
  python3 - "$datadir" "$@" <<'PY'
import os
import sys

datadir = sys.argv[1]
exe_names = set(sys.argv[2:])

anc, p = set(), os.getpid()
for _ in range(16):
    anc.add(p)
    try:
        p = int(open('/proc/%d/stat' % p).read().rsplit(')', 1)[1].split()[1])
    except Exception:
        break
    if p <= 1:
        anc.add(p)
        break

for d in os.listdir('/proc'):
    if not d.isdigit() or int(d) in anc:
        continue
    try:
        cmd = open('/proc/%s/cmdline' % d, 'rb').read().replace(b'\0', b' ').decode(errors='replace')
        exe = os.path.realpath('/proc/%s/exe' % d)
    except Exception:
        continue
    if datadir in cmd and (not exe_names or os.path.basename(exe) in exe_names):
        print(d)
PY
}

# proc_pids_holding_lockfiles <datadir_abspath>: one "<pid> <lockfile>"
# pair per line -- a live process that currently has an open file
# descriptor on one of this datadir's embedded-DB LOCK files (found by
# walking the datadir, up to 4 levels deep, for files literally named
# `LOCK` -- covers XDCx/LOCK, chaindata/LOCK, geth/chaindata/LOCK,
# erigon/LOCK, rocksdb/LOCK, besu's database/LOCK, ...). This is the
# PROXIMATE cause of ItWorksinMyLocal#97's "Can't create new DB ...
# resource temporarily unavailable": a stale holder's flock on this exact
# file is what blocks a fresh node from starting, even if that holder's
# own /proc/<pid>/exe didn't happen to match a name proc_pids_for_datadir
# knows about (a renamed/newer client binary, say). Always excludes our
# own pid and every ancestor pid, same rule as proc_pids_for_datadir.
proc_pids_holding_lockfiles() {
  local datadir="$1"
  [ -d "$datadir" ] || return 0
  python3 - "$datadir" <<'PY'
import os
import sys

datadir = os.path.realpath(sys.argv[1])

locks = []
for root, dirs, files in os.walk(datadir):
    depth = root[len(datadir):].count(os.sep)
    if depth >= 4:
        dirs[:] = []
    if 'LOCK' in files:
        locks.append(os.path.join(root, 'LOCK'))
if not locks:
    sys.exit(0)
locks = set(os.path.realpath(p) for p in locks)

anc, p = set(), os.getpid()
for _ in range(16):
    anc.add(p)
    try:
        p = int(open('/proc/%d/stat' % p).read().rsplit(')', 1)[1].split()[1])
    except Exception:
        break
    if p <= 1:
        anc.add(p)
        break

for d in os.listdir('/proc'):
    if not d.isdigit() or int(d) in anc:
        continue
    fddir = '/proc/%s/fd' % d
    try:
        fds = os.listdir(fddir)
    except Exception:
        continue
    for fd in fds:
        try:
            target = os.path.realpath(os.path.join(fddir, fd))
        except Exception:
            continue
        if target in locks:
            print('%s %s' % (d, target))
            break
PY
}
