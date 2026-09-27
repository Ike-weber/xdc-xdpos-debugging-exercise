#!/bin/bash
_interupt() {
    echo "Shutdown $child_proc"
    kill -TERM $child_proc
    exit
}

trap _interupt INT TERM

cd "$(dirname "$0")" || exit 1

# Sourced early (before flag parsing) purely so DEFAULT_ALL_CLIENTS below can
# reference lib.sh's shared constant instead of keeping its own copy; nothing
# lib.sh sets at source-time depends on run.sh's flags/CHAINID validation, so
# this does not reorder anything the network itself sees.
source ./lib.sh

# --mixed (or MIXED=1 in the environment/.env): also launch a 5th node --
# modern go-ethereum, sync-only (no --mine), joining from genesis alongside
# the 4 legacy oldxdc sealers below. Default is unchanged (4 legacy sealers
# only).
MIXED="${MIXED:-0}"
# --all: the 4 legacy sealers PLUS one sync-only follower per modern client, so
# a single command gives a cross-client network on the local genesis. Each
# follower is launched through join.sh rather than reimplemented here -- join.sh
# already knows every client's flag dialect (geth's --http vs xone's single-dash
# vs besu's --genesis-file vs erigon's --chain <file>), and netlab/node.sh
# already drives it the same way.
ALL="${ALL:-0}"
# Modern clients --all will try, in port order. Override to narrow it, e.g.
#   ALL_CLIENTS="geth erigon" ./run.sh --all
#
# NOTE: this file used to carry a long block explaining why erigon, reth and
# nethermind were "deliberately absent" from this default -- each supposedly
# starting but never following the chain. That is no longer true and the block
# has been removed rather than left to mislead: all three now follow a local
# genesis (erigon via --chain <file>, nethermind via a generated chainspec, reth
# once its EIP-1559-from-genesis fix landed). They are in the default set below.
# If one regresses, prefer fixing it or narrowing ALL_CLIENTS at the call site
# over re-adding prose here that the code contradicts.
# --all means ALL modern clients, matching join.sh's own --client all list. It
# used to default to just "geth xone besu", so `./setup.sh --all` quietly gave a
# 3-client net while the docs said "one follower per modern client", and a user
# had to know to set ALL_CLIENTS by hand to get the rest (#152). Anything whose
# binary is missing or unrunnable is skipped and REPORTED (see ALL_SKIP_WHY
# below), so widening the default degrades gracefully on a partial bin/ instead
# of failing. Narrow it explicitly when you want to:
#   ALL_CLIENTS="geth erigon" ./run.sh --all
ALL_CLIENTS="${ALL_CLIENTS:-$DEFAULT_ALL_CLIENTS}"
CHAINID="${CHAINID:-20118}"
while [ $# -gt 0 ]; do
  case "$1" in
    --mixed) MIXED=1; shift;;
    --all) ALL=1; shift;;
    --chainid) CHAINID=$2; shift 2;;
    # Machine component of every node identity (see node_name() in lib.sh).
    # Exported so it reaches the join.sh children --all launches, too.
    --name) export MACHINE=$2; shift 2;;
    --build) BIN_SOURCE_oldxdc=build; shift;;
    -h|--help)
      echo "usage: ./run.sh [--mixed | --all] [--chainid N] [--build]"
      echo "  --mixed       also start node 5: modern geth, sync-only, from genesis"
      echo "                (same as MIXED=1 ./run.sh)"
      echo "  --all         also start ONE sync-only follower per modern client"
      echo "                (default: $ALL_CLIENTS; env: ALL_CLIENTS to narrow it)"
      echo "                Clients whose binary is absent are skipped with a"
      echo "                reason rather than taking the network down."
      echo "  --chainid N   chain id (default: 20118; env: CHAINID)"
      echo "  --build       clone XDPoSChain (XDPOS_REPO) and build the oldxdc binary"
      echo "                from source instead of downloading a prebuilt one (same as"
      echo "                BIN_SOURCE_oldxdc=build ./run.sh). Default is download."
      exit 0;;
    *) echo "usage: ./run.sh [--mixed | --all] [--chainid N] [--build]" >&2; exit 1;;
  esac
done

# --all is a superset of --mixed (it starts a modern geth follower too), so
# accepting both would launch two geth nodes fighting for the same ports.
if [ "$ALL" = 1 ] && [ "$MIXED" = 1 ]; then
  echo "run.sh: --all already includes a modern geth follower; drop --mixed (or MIXED=1)" >&2
  exit 1
fi

# A non-numeric chain id is accepted by the node binary but produces a network
# nothing else can join -- reject it here rather than at peering time.
case "$CHAINID" in
  ''|*[!0-9]*) echo "run.sh: --chainid must be a positive integer (got '$CHAINID')" >&2; exit 1;;
esac

# Same pattern join.sh uses. Only SKYNET_BASE_URL is consumed here; the file's
# other keys (DEFAULT_HOST/BOOTNODE_PORT/GENESIS/BOOTNODES) describe a REMOTE
# network to join and are deliberately unreferenced by run.sh, which always
# builds its own local topology. Checked: run.sh names none of them.
# shellcheck disable=SC1091  # local, operator-editable config; nothing to lint here
[ -f ./network.env ] && . ./network.env
require_oldxdc

touch .pwd
set -a
# shellcheck disable=SC1091
[ -f .env ] && source .env
set +a

Bin_NAME=XDC
# Not currently operator-overridable (run.sh always launches exactly this
# many legacy sealers) -- set from the shared default so the manifest below
# and status.sh's own fallback can never disagree with it.
NUM_NODES=$DEFAULT_NUM_NODES

# Base ports for the whole local net (nodes 1-4 use BASE+1..BASE+4, and when
# --mixed is set node 5 uses BASE+5) -- override to move the entire topology
# off a set of ports already in use on a shared host, e.g.:
#   BASE_P2P_PORT=31302 BASE_RPC_PORT=9544 BASE_WS_PORT=9554 ./run.sh --mixed
_BASE_P2P_EXPLICIT="${BASE_P2P_PORT:+1}"
BASE_P2P_PORT="${BASE_P2P_PORT:-$DEFAULT_BASE_P2P_PORT}"
BASE_RPC_PORT="${BASE_RPC_PORT:-$DEFAULT_BASE_RPC_PORT}"
BASE_WS_PORT="${BASE_WS_PORT:-$DEFAULT_BASE_WS_PORT}"
# --all only: p2p ports per follower. A client may need more than one
# listener (erigon: one per eth protocol version), so each follower gets a
# block this wide rather than a single port.
ALL_P2P_STRIDE="${ALL_P2P_STRIDE:-$DEFAULT_ALL_P2P_STRIDE}"

# The local bootnode's port. Was the bare literal 30301 in three places, which
# meant BASE_P2P_PORT/BASE_RPC_PORT/BASE_WS_PORT could move the whole node 1..N
# topology out of the way but NOT the bootnode -- so on a host where 30301 was
# already taken there was no documented way to start at all, and the pre-flight
# correctly refused every attempt. Note the name: network.env uses the
# DEFAULT_BOOTNODE_PORT spelling for the REMOTE network join.sh dials, so this
# does not collide with it.
BOOTNODE_PORT="${BOOTNODE_PORT:-$DEFAULT_BOOTNODE_PORT}"

# --all followers get their OWN rpc/ws/authrpc block, one decade each, well clear
# of the sealers' ranges. Previously they were numbered BASE_RPC_PORT+idx and
# BASE_WS_PORT+idx, which OVERLAPPED the sealers on a default run:
#
#   sealer  node i  rpc = 8545..8549   ws = 8555..8559        (BASE_*+i, i=1..5)
#   follower idx    rpc = 8549..8555   ws = 8559..8565        (BASE_*+idx, idx=5..11)
#
# so follower 11's rpc (8555) landed on sealer 1's ws, and follower 5's ws (8559)
# on sealer 5's ws. reth (last in ALL_CLIENTS) died at startup with "address
# 127.0.0.1:8555 already in use"; nethermind's process survived but its JSON-RPC
# never bound, leaving a node that syncs internally and is invisible to every
# tool -- a worse failure than crashing. Deterministic on any clean host with
# pure defaults, which is why a warm bin/ and pre-existing datadirs hid it.
#
# One decade per follower also leaves room for clients that derive extra ports
# from their rpc port: nethermind opens a second JSON-RPC URL at rpc+1, which
# under consecutive numbering took the NEXT follower's rpc port.
ALL_RPC_BASE="${ALL_RPC_BASE:-$((BASE_RPC_PORT + 200))}"
ALL_PORT_STRIDE="${ALL_PORT_STRIDE:-$DEFAULT_ALL_PORT_STRIDE}"

# Follower port helpers (all_follower_rpc/_ws/_auth) now live in lib.sh --
# used by both the pre-flight check and the launch loop below, AND by
# status.sh's report, so all three compute this arithmetic exactly once.

# Ensure platform-native binaries exist (builds from XDPoSChain if needed).
ensure_bins
XDC="$XDC_BIN"

# Modern geth binary for node 5 (--mixed only). Never built here -- setup.sh
# (or an explicit `CLIENT=geth ./lib.sh`-style build) must have produced it
# already; resolve it via lib.sh's per-client BIN_DIR unless overridden.
MODERN_GETH_BIN="${MODERN_GETH_BIN:-$_LIB_DIR/bin/geth/$PLATFORM/geth}"
if [ "$MIXED" = 1 ] && [ ! -x "$MODERN_GETH_BIN" ]; then
  echo "run.sh --mixed: modern geth binary not found at $MODERN_GETH_BIN" >&2
  echo "  build it first, e.g.:  (CLIENT=geth; source ./lib.sh; ensure_bins)" >&2
  echo "  or set MODERN_GETH_BIN=/path/to/geth" >&2
  exit 1
fi

# --all: resolve which modern clients can actually run here, BEFORE starting
# anything. Same reasoning as the port pre-flight below -- a follower whose
# binary is missing would have join.sh exit in the background where nobody sees
# it, leaving a topology quietly smaller than the one that was asked for.
#
# The binary name per client mirrors lib.sh's NODE_NAME_BIN (reth's is
# "xdc-reth", not "reth"); besu/reth/nethermind additionally honour the
# BESU_BIN/RETH_BIN/NETHERMIND_DIST overrides join.sh itself uses, so a client
# installed outside bin/ is still found.
_client_binary() {   # -> prints the path it will look for
  case "$1" in
    geth)       echo "${MODERN_GETH_BIN:-$_LIB_DIR/bin/geth/$PLATFORM/geth}" ;;
    # geth4 is a SEPARATE client token with its own bin/ tree, but its
    # executable is still named "geth" (see lib.sh client_exe_names). Without this
    # arm it fell to the *) empty case and could never be selected by --all --
    # the same "client token missing from one of several parallel lists" shape as
    # the three defects that made geth4 unlaunchable in #143.
    geth4)   echo "$_LIB_DIR/bin/geth4/$PLATFORM/geth" ;;
    xone)       echo "$_LIB_DIR/bin/xone/$PLATFORM/xone" ;;
    erigon)     echo "$_LIB_DIR/bin/erigon/$PLATFORM/erigon" ;;
    besu)       echo "${BESU_BIN:-$_LIB_DIR/bin/besu/$PLATFORM/besu}" ;;
    reth)       echo "${RETH_BIN:-$_LIB_DIR/bin/reth/$PLATFORM/xdc-reth}" ;;
    # The nethermind tarball unpacks its runtime into a .dist-nethermind/
    # subdirectory, so the binary is one level DEEPER than the other clients'.
    # Checking only $PLATFORM/nethermind reported "binary not found" and silently
    # dropped nethermind from --all on a host that had it installed correctly.
    # Honour NETHERMIND_DIST first (join.sh's override), then the real layout,
    # then the flat path in case a dist is ever unpacked without the subdir.
    nethermind)
      if [ -n "$NETHERMIND_DIST" ]; then echo "$NETHERMIND_DIST/nethermind"
      elif [ -x "$_LIB_DIR/bin/nethermind/$PLATFORM/.dist-nethermind/nethermind" ]; then
        echo "$_LIB_DIR/bin/nethermind/$PLATFORM/.dist-nethermind/nethermind"
      else echo "$_LIB_DIR/bin/nethermind/$PLATFORM/nethermind"; fi ;;
    *)          echo "" ;;
  esac
}

# besu is a Java app, so having the launcher script is not enough to call it
# runnable -- but testing for a SYSTEM java is the wrong test. The besu tarball we
# publish is self-contained and ships its own JRE, so besu runs fine on a host
# with no JDK at all; gating on `java -version` skipped it on every such host
# (i.e. most fresh boxes) while its own launcher worked perfectly. macOS makes the
# inverse mistake possible too: it ships a /usr/bin/java SHIM that exists and
# satisfies `command -v` but then exits 1 with "Unable to locate a Java Runtime".
# Both cases are settled by asking BESU ITSELF, which resolves its bundled JRE
# exactly as it will at launch. Wrapped in a timeout so a wedged JVM can't stall
# startup (absent on stock macOS, hence the fallback).
_client_runnable() {
  case "$1" in
    besu)
      _bb="$(_client_binary besu)"
      [ -n "$_bb" ] && [ -x "$_bb" ] || return 1
      if command -v timeout >/dev/null 2>&1; then timeout 30 "$_bb" --version >/dev/null 2>&1
      else "$_bb" --version >/dev/null 2>&1; fi ;;
    *) : ;;
  esac
}
_client_unrunnable_reason() {
  case "$1" in
    besu) echo "besu --version failed (its bundled JRE is broken, or set BESU_BIN)" ;;
    *) echo "binary not found" ;;
  esac
}

ALL_SELECTED=""
ALL_SKIPPED=""
ALL_SKIP_WHY=""
if [ "$ALL" = 1 ]; then
  for _c in $ALL_CLIENTS; do
    _bin="$(_client_binary "$_c")"
    # Fetch a missing prebuilt instead of just reporting it absent.
    #
    # This loop only ever TESTED for the binary, so on a fresh clone -- where
    # bin/ is empty by definition -- every follower was reported "binary not
    # found" and skipped. `./setup.sh --new --all` then produced a network with
    # exactly ONE follower (geth, and only because setup.sh fetches that one
    # explicitly), while both the --all help text and the README promise "one
    # sync-only follower per modern client". Measured on a clean clone: 6 of 7
    # skipped. join.sh has always downloaded on demand via ensure_bins, so
    # `./join.sh --client all` gave the documented result and `--all` did not --
    # the same flag, two different outcomes, which is the confusing part.
    #
    # download_bins, NOT ensure_bins: ensure_bins falls back to a from-source
    # clone+build when no prebuilt exists for this platform. For a client with
    # no darwin artifact (geth4, xone today) that would silently turn `--all`
    # into a multi-minute compile of a third-party repo nobody asked for. A
    # download that fails just leaves the client skipped, with the reason it
    # already prints. Source builds stay opt-in via BIN_SOURCE/--build.
    if [ -n "$_bin" ] && [ ! -x "$_bin" ] && ! command -v "$_c" >/dev/null 2>&1; then
      echo "run.sh --all: $_c binary missing -- trying the published prebuilt ..."
      ( CLIENT="$_c"; . ./lib.sh; download_bins ) >/dev/null 2>&1 || true
      _bin="$(_client_binary "$_c")"
    fi
    if { [ -n "$_bin" ] && [ -x "$_bin" ]; } || command -v "$_c" >/dev/null 2>&1; then
      if _client_runnable "$_c"; then
        ALL_SELECTED="${ALL_SELECTED:+$ALL_SELECTED }$_c"
      else
        ALL_SKIPPED="${ALL_SKIPPED:+$ALL_SKIPPED }$_c"
        ALL_SKIP_WHY="${ALL_SKIP_WHY}  $_c -> $(_client_unrunnable_reason "$_c")
"
      fi
    else
      ALL_SKIPPED="${ALL_SKIPPED:+$ALL_SKIPPED }$_c"
      ALL_SKIP_WHY="${ALL_SKIP_WHY}  $_c -> binary not found: $_bin
"
    fi
  done

  # erigon 3.5.2 binds ONE p2p port per eth protocol version and takes them from
  # a HARDCODED 30303..30307, using our --port only for the XDC protocol (100).
  # Verified: with that range free it came up on 30317(v100) + 30303(69) +
  # 30304(68) + 30305(63). --p2p.allowed-ports, which older builds used to
  # redirect them, is rejected outright by 3.5.2 ("flag provided but not
  # defined"), so the range cannot be moved -- the SEALERS have to move instead.
  # Their default 30303..30306 collides head-on, which is why the erigon follower
  # died with "run out of allowed ports".
  case " $ALL_SELECTED " in
    *" erigon "*)
      if [ "$_BASE_P2P_EXPLICIT" != 1 ] && [ "$BASE_P2P_PORT" = 30302 ]; then
        BASE_P2P_PORT=31302
        echo "run.sh --all: moving the local topology to BASE_P2P_PORT=$BASE_P2P_PORT"
        echo "  (erigon needs the hardcoded 30303-30307 range free; override BASE_P2P_PORT to pin it)"
      fi ;;
  esac
  if [ -z "$ALL_SELECTED" ]; then
    echo "run.sh --all: none of the requested modern clients has a usable binary." >&2
    echo "  looked for:" >&2
    for _c in $ALL_CLIENTS; do echo "    $_c -> $(_client_binary "$_c")" >&2; done
    echo "  build one, e.g.:  ./setup.sh --client geth" >&2
    exit 1
  fi
fi

# Machine identity: hostname + LAN IP (advertised to peers so this works across
# machines on the same network, not just localhost).
HOST=$(hostname -s)
# Advertised address. NODE_IP overrides the autodetected one, for hosts where
# detect_ip picks an interface peers cannot reach (NAT, WSL2, multi-homed boxes).
# Deliberately NOT keyed on bare $IP: that name is far too generic to capture
# from the environment safely.
IP="${NODE_IP:-$(detect_ip)}"
echo "Machine: host=$HOST ip=$IP platform=$PLATFORM mixed=$MIXED all=$ALL"
if [ "$ALL" = 1 ]; then
  echo "Modern followers: ${ALL_SELECTED:-<none>}"
  if [ -n "$ALL_SKIPPED" ]; then
    echo "Skipped:"
    printf '%s' "$ALL_SKIP_WHY"
  fi
fi

# Record the topology this invocation actually resolved (ports, --all's
# ALL_SELECTED set, chain id, machine label) so status.sh can report on it
# later without the operator re-passing the same overrides (ItWorksinMyLocal
# #181). Every value above is final by this point (ALL_SELECTED resolved,
# the erigon BASE_P2P_PORT bump already applied); nothing below this line
# changes any of them. Reporting-only: run.sh itself never reads this file.
MACHINE_LABEL="$(machine_name)"
write_topology_manifest ./nodes/.topology

# The chain id the nodes run with and the chainId baked into genesis.json must
# agree. When they don't, nothing here fails loudly: the nodes start, seal, and
# simply never peer (the eth handshake rejects a mismatched network), which
# surfaces much later as an unexplained "peers=0". Assert it up front instead.
# Checked on every run, not just first init -- the re-run path reuses chaindata
# that may have been inited from a *different* genesis than the current one.
if [ -f ./genesis/genesis.json ]; then
  GENESIS_CHAINID=$(python3 -c "import json;print(json.load(open('./genesis/genesis.json'))['config']['chainId'])" 2>/dev/null)
  if [ -n "$GENESIS_CHAINID" ] && [ "$GENESIS_CHAINID" != "$CHAINID" ]; then
    echo "run.sh: --chainid $CHAINID != genesis chainId $GENESIS_CHAINID (./genesis/genesis.json)." >&2
    echo "  The nodes would start but never peer. Regenerate genesis for this id:" >&2
    echo "    ./gen-genesis.sh --chainid $CHAINID --signers <a,b,c> && ./reset.sh" >&2
    echo "  or run with the genesis's own id:  ./run.sh --chainid $GENESIS_CHAINID" >&2
    exit 1
  fi
  [ -n "$GENESIS_CHAINID" ] || echo "run.sh: warning: could not read chainId from ./genesis/genesis.json -- skipping the chain id match check." >&2
fi

# Import keys + init on first run, otherwise read the existing accounts.
declare -a WALLETS
if [ ! -d ./nodes/1/$Bin_NAME/chaindata ]; then
  for i in $(seq 1 $NUM_NODES); do
    pk_var="PRIVATE_KEY_$i"
    WALLETS[$i]=$("$XDC" account import --password .pwd --datadir ./nodes/$i <(echo "${!pk_var}") | awk -v FS="({|})" '{print $2}')
    if [ -z "${WALLETS[$i]}" ]; then
      # `account import` fails (e.g. "account already exists") if the
      # keystore already holds this key even though chaindata is missing --
      # fall back to the already-imported account instead of silently
      # unlocking an empty address (which crashes the sealer at startup
      # with "etherbase must be explicitly specified").
      WALLETS[$i]=$("$XDC" account list --datadir ./nodes/$i | head -n 1 | awk -v FS="({|})" '{print $2}')
    fi
    [ -n "${WALLETS[$i]}" ] || { echo "run.sh: could not determine a wallet address for node $i (check PRIVATE_KEY_$i in .env)" >&2; exit 1; }
    "$XDC" --datadir ./nodes/$i init ./genesis/genesis.json
  done
else
  for i in $(seq 1 $NUM_NODES); do
    WALLETS[$i]=$("$XDC" account list --datadir ./nodes/$i | head -n 1 | awk -v FS="({|})" '{print $2}')
    [ -n "${WALLETS[$i]}" ] || { echo "run.sh: no account found in ./nodes/$i (empty keystore?)" >&2; exit 1; }
  done
fi

# Node 5 (--mixed only): modern geth, sync-only follower, inited from the
# SAME genesis file as nodes 1-4. Its chaindata lives under nodes/5/XDC (the
# modern go-ethereum fork keeps the legacy "XDC" datadir subdirectory name),
# same layout join.sh already assumes for this client.
if [ "$MIXED" = 1 ] && [ ! -d ./nodes/5/XDC/chaindata ]; then
  echo "Initialising node 5 (modern geth) from ./genesis/genesis.json ..."
  "$MODERN_GETH_BIN" --datadir ./nodes/5 init ./genesis/genesis.json
fi

# Genesis-hash agreement check (--mixed only): modern geth and legacy oldxdc
# must compute the SAME genesis hash from the same genesis.json, or geth will
# be rejected at the eth/XDPoS handshake (0 peers, never syncs). Compare them
# up front so a mismatch is obvious immediately rather than discovered as a
# silent "node 5 stuck at peers=0".
if [ "$MIXED" = 1 ]; then
  # --port 0 --nodiscover (+ a throwaway --authrpc.port for geth): these are
  # one-shot local reads of the already-inited genesis block, not real nodes
  # -- keep them off the network entirely so they can't collide with any
  # port already in use on the host (including the P2P ports nodes 1-4/5
  # are about to bind below).
  LEGACY_GENESIS_HASH=$("$XDC" --datadir ./nodes/1 --port 0 --nodiscover --exec 'eth.getBlock(0).hash' console 2>/dev/null | tail -1 | tr -d '"')
  MODERN_GENESIS_HASH=$("$MODERN_GETH_BIN" --datadir ./nodes/5 --port 0 --nodiscover --authrpc.port 0 --exec 'eth.getBlock(0).hash' console 2>/dev/null | tail -1 | tr -d '"')
  echo "Genesis hash (legacy XDC,   node1): ${LEGACY_GENESIS_HASH:-<unavailable>}"
  echo "Genesis hash (modern geth,  node5): ${MODERN_GENESIS_HASH:-<unavailable>}"
  if [ -n "$LEGACY_GENESIS_HASH" ] && [ -n "$MODERN_GENESIS_HASH" ]; then
    if [ "$LEGACY_GENESIS_HASH" = "$MODERN_GENESIS_HASH" ]; then
      echo "Genesis hashes MATCH -- node 5 can peer with the legacy sealers."
    else
      echo "WARNING: genesis hashes DO NOT MATCH -- node 5 will be rejected at handshake (0 peers) and never sync." >&2
    fi
  else
    echo "WARNING: could not read one or both genesis hashes (see above) -- comparison inconclusive." >&2
  fi
fi

VERBOSITY=3
GASPRICE="1"
STATS_SECRET="xdc_openscan_stats_2026"
VERSION=$("$XDC" version 2>/dev/null | awk '/^Version:/{print $2}')
# Short git commit of the sealer binary, for the node name. `version` prints the
# full 40-char hash on its own line ("Git Commit: af7de721...") -- take 8, which
# is what every other client reports and enough to identify a build. Empty when
# the binary was built without git metadata; node_name() renders that as
# "nocommit" rather than silently dropping the field.
COMMIT=$("$XDC" version 2>/dev/null | awk '/^Git Commit:/{print substr($3,1,8); exit}')
# The node binaries' --networkid is dynamic: setup.sh or the operator can pass CHAINID in the
# environment or --chainid on the command line. All nodes (legacy + modern)
# must share the same value or peering will fail.
networkid="$CHAINID"

# Local bootnode enode, built from bootnode.key + this machine's IP so every
# node connects to a bootnode within the network.
BOOTNODE_PUBKEY=$("$BOOTNODE_BIN" -nodekey ./bootnode.key -writeaddress)
BOOTNODE_ENODE="enode://${BOOTNODE_PUBKEY}@${IP}:${BOOTNODE_PORT}"
echo "Local bootnode: $BOOTNODE_ENODE"

# Pre-flight: every port this topology needs must be free BEFORE anything
# starts. A busy port doesn't stop the run today -- each node is launched into
# the background and never checked, so the one that loses the bind dies alone
# ("Fatal: listen udp :30304: bind: address already in use") while its siblings
# keep sealing. The result is a silently degraded network -- 4 masternodes in
# genesis but only 3 ever sealing -- with nothing in the output saying so.
# Two checkouts of this repo on one host hit this instantly: both default to
# the same BASE_*_PORT range.
ports_in_use() {
  command -v lsof >/dev/null 2>&1 || {
    echo "run.sh: lsof not found -- skipping the port pre-flight check" >&2; return 1; }
  local busy=1 spec p label holder h
  for spec in "$@"; do
    p="${spec%%:*}"; label="${spec#*:}"
    holder=$( { lsof -nP -iTCP:"$p" -sTCP:LISTEN -t 2>/dev/null; lsof -nP -iUDP:"$p" -t 2>/dev/null; } | sort -u | tr '\n' ' ' )
    holder="${holder% }"
    [ -z "$holder" ] && continue
    busy=0
    echo "run.sh: port $p ($label) already in use by PID(s): $holder" >&2
    for h in $holder; do
      echo "          $(ps -p "$h" -o command= 2>/dev/null | cut -c1-96)" >&2
    done
  done
  return $busy
}

PORT_SPECS=("${BOOTNODE_PORT}:bootnode")
for i in $(seq 1 $NUM_NODES); do
  PORT_SPECS+=("$((BASE_P2P_PORT + i)):node $i p2p" "$((BASE_RPC_PORT + i)):node $i rpc" "$((BASE_WS_PORT + i)):node $i ws")
done
if [ "$MIXED" = 1 ]; then
  PORT_SPECS+=("$((BASE_P2P_PORT + 5)):node 5 p2p" "$((BASE_RPC_PORT + 5)):node 5 rpc" \
               "$((BASE_WS_PORT + 5)):node 5 ws" "$((BASE_RPC_PORT + 55)):node 5 authrpc")
fi
if [ "$ALL" = 1 ]; then
  # Followers continue the same index series as node 5, one index per client, so
  # the ports stay derivable from BASE_* and a whole --all topology can still be
  # moved aside with one set of overrides.
  _idx=5
  for _c in $ALL_SELECTED; do
    # p2p strides by ALL_P2P_STRIDE, not 1: erigon opens one listener per
    # --p2p.protocol version (default 69,70,71) counting upward from its --port,
    # so consecutive followers would have erigon squatting the next client's p2p
    # port. Reserve the whole block and check every port in it, so a collision
    # shows up here instead of as a follower that dies in the background.
    _p2p=$((BASE_P2P_PORT + 5 + (_idx - 5) * ALL_P2P_STRIDE))
    _k=0
    while [ "$_k" -lt "$ALL_P2P_STRIDE" ]; do
      PORT_SPECS+=("$((_p2p + _k)):node $_idx ($_c) p2p block")
      _k=$((_k + 1))
    done
    PORT_SPECS+=("$(all_follower_rpc "$_idx"):node $_idx ($_c) rpc" \
                 "$(($(all_follower_rpc "$_idx") + 1)):node $_idx ($_c) rpc+1 (nethermind 2nd url)" \
                 "$(all_follower_ws "$_idx"):node $_idx ($_c) ws" \
                 "$(all_follower_auth "$_idx"):node $_idx ($_c) authrpc")
    # erigon additionally takes its eth-protocol listeners from the hardcoded
    # 30303..30307 (see the ALL_SELECTED block above). Reserve them so a clash
    # is reported here by name instead of surfacing as "run out of allowed
    # ports" in a background follower log.
    if [ "$_c" = erigon ]; then
      _e=30303
      while [ "$_e" -le 30307 ]; do
        PORT_SPECS+=("$_e:node $_idx (erigon) hardcoded eth-protocol port")
        _e=$((_e + 1))
      done
    fi
    _idx=$((_idx + 1))
  done
fi
if ports_in_use "${PORT_SPECS[@]}"; then
  echo "" >&2
  echo "  Refusing to start: the node that loses the bind would die silently and" >&2
  echo "  leave a degraded network. Stop whatever holds the port(s) above, or move" >&2
  echo "  this whole topology out of the way:" >&2
  echo "    BASE_P2P_PORT=31302 BASE_RPC_PORT=9544 BASE_WS_PORT=9554 ./run.sh$([ "$MIXED" = 1 ] && echo ' --mixed')" >&2
  exit 1
fi

echo Starting the bootnode ...
"$BOOTNODE_BIN" -nodekey ./bootnode.key -addr ${IP}:${BOOTNODE_PORT} &
child_proc=$!

echo Starting the nodes ...
for i in $(seq 1 $NUM_NODES); do
  n=$(printf '%02d' $i)
  P2P_PORT=$((BASE_P2P_PORT + i))   # node1 -> BASE+1 ... node4 -> BASE+4 (default 30303..30306)
  WS_PORT=$((BASE_WS_PORT + i))     # default 8555..8558
  RPC_PORT=$((BASE_RPC_PORT + i))   # default 8545..8548
  # Canonical shared identity (lib.sh node_name): machine-client-version-commit-
  # nodetype-ip. The node INDEX rides in the nodetype ("seal01") because every
  # sealer in this topology runs on the SAME box: machine, version, commit and ip
  # are identical across nodes 1..N, so without the index all of them would claim
  # one ethstats name -- and colliding identities make healthy nodes look stuck.
  NODE_NAME=$(node_name "$(machine_name)" "$CLIENT" "$VERSION" "$COMMIT" "seal${n}" "$IP")
  NODE_IDENTITY="${HOST}-MasterNode-${n}"
  ETHSTATS="${NODE_NAME}:${STATS_SECRET}@stats.xdcindia.com:443"
  "$XDC" --bootnodes "$BOOTNODE_ENODE" --syncmode "full" --datadir ./nodes/$i \
    --networkid "${networkid}" --port $P2P_PORT --nat "extip:${IP}" --identity "${NODE_IDENTITY}" \
    --rpc --rpccorsdomain "*" --ws --wsaddr="0.0.0.0" --wsorigins "*" --wsport $WS_PORT \
    --rpcaddr 0.0.0.0 --rpcport $RPC_PORT --rpcvhosts "*" \
    --unlock "${WALLETS[$i]}" --etherbase "${WALLETS[$i]}" --password ./.pwd --mine --gasprice "${GASPRICE}" \
    --targetgaslimit "420000000" --verbosity ${VERBOSITY} \
    --rpcapi admin,db,eth,debug,miner,net,shh,txpool,personal,web3,XDPoS \
    --ethstats "${ETHSTATS}" &
  child_proc="$child_proc $!"
done

if [ "$MIXED" = 1 ]; then
  echo "Starting node 5 (modern geth, sync-only follower) ..."
  P2P_PORT5=$((BASE_P2P_PORT + 5))   # default 30307
  WS_PORT5=$((BASE_WS_PORT + 5))     # default 8559
  RPC_PORT5=$((BASE_RPC_PORT + 5))   # default 8549
  # Modern geth always starts an authrpc (Engine API) listener even though
  # XDPoS doesn't use it here; it defaults to 127.0.0.1:8551, which is
  # commonly already taken by another node on a shared host and would
  # otherwise crash node 5 at startup ("address already in use"). Give it
  # its own offset, well clear of the BASE+1..BASE+5 range used above.
  AUTHRPC_PORT5=$((BASE_RPC_PORT + 55))
  NODE5_IDENTITY="${HOST}-ModernGeth-05"
  # Node 5 is launched directly here rather than through join.sh, so it has to
  # compose the canonical identity itself. Resolve geth's own version/commit in a
  # subshell so CLIENT=geth does not clobber the oldxdc-scoped vars this script
  # relies on. nodetype is "sync": node 5 follows, it never seals.
  NODE5_VC=$( (CLIENT=geth; client_version_commit "$MODERN_GETH_BIN") )
  ETHSTATS5="$(node_name "$(machine_name)" geth "${NODE5_VC%% *}" "${NODE5_VC##* }" sync "$IP"):${STATS_SECRET}@stats.xdcindia.com:443"
  "$MODERN_GETH_BIN" --datadir ./nodes/5 --networkid "${networkid}" --syncmode full \
    --bootnodes "$BOOTNODE_ENODE" --port "$P2P_PORT5" --nat "extip:${IP}" \
    --http --http.addr 0.0.0.0 --http.port "$RPC_PORT5" --http.vhosts "*" --http.corsdomain "*" \
    --http.api eth,net,web3,txpool,debug,admin,XDPoS \
    --ws --ws.addr 0.0.0.0 --ws.origins "*" --ws.port "$WS_PORT5" --ws.api eth,net,web3,XDPoS \
    --authrpc.port "$AUTHRPC_PORT5" \
    --identity "${NODE5_IDENTITY}" --ethstats "${ETHSTATS5}" &
  child_proc="$child_proc $!"
fi

# --all: one sync-only follower per modern client, each via join.sh.
#
# Delegating rather than open-coding six launches is the whole point: join.sh
# owns each client's flag dialect and its binary/genesis quirks (erigon wants
# --chain <genesis file>, besu --genesis-file, xone single-dash flags, reth a
# wss:// ethstats URL), and it is the same entry point netlab/node.sh uses. A
# copy here would drift from it silently.
#
# --gas-limit is passed explicitly and NOT left to join.sh's per---network
# default: this is a local genesis with no --network, and #94/#96/#98 were all
# variants of a follower running on a gas-limit plateau that disagreed with the
# sealers. 420000000 is exactly what the legacy sealers above use
# (--targetgaslimit), so the followers agree by construction.
if [ "$ALL" = 1 ]; then
  # Followers get the live sealer enodes as --staticpeers, harvested from the
  # sealers' own admin_nodeInfo once their RPC answers. The bootnode alone is
  # not enough: erigon's discv4 does not discover peers through an XDPoSChain
  # bootnode (join.sh says so in its --staticpeers doc), so without this it sits
  # at "[p2p] No GoodPeers", peers=0, block=0 -- started but never syncing. Left
  # to itself join.sh falls back to peers.list, which is the PUBLIC devnet-5151
  # set and useless on a local chain id.
  # Same approach netlab/fleet.sh uses to feed netlab/node.sh.
  _sealer_enodes() {
    local i p en out=""
    for i in $(seq 1 $NUM_NODES); do
      p=$((BASE_RPC_PORT + i))
      en=$(curl -s -m 3 -X POST -H 'Content-Type: application/json' \
             --data '{"jsonrpc":"2.0","method":"admin_nodeInfo","params":[],"id":1}' \
             "http://127.0.0.1:$p" 2>/dev/null \
           | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["enode"])' 2>/dev/null)
      # admin_nodeInfo advertises 127.0.0.1 for a locally-bound node; peers on
      # the LAN need the routable address the sealers were started with.
      en=$(printf '%s' "$en" | sed "s|@127\.0\.0\.1:|@${IP}:|")
      [ -n "$en" ] && out="${out:+$out,}$en"
    done
    printf '%s' "$out"
  }
  STATIC_PEERS=""
  _waited=0
  while [ "$_waited" -lt 60 ]; do
    STATIC_PEERS="$(_sealer_enodes)"
    case "$STATIC_PEERS" in *enode://*) break ;; esac
    sleep 3; _waited=$((_waited + 3))
  done
  if [ -n "$STATIC_PEERS" ]; then
    echo "Harvested $(printf '%s' "$STATIC_PEERS" | tr ',' '\n' | grep -c enode://) sealer enode(s) for the followers"
  else
    echo "WARNING: no sealer enodes after ${_waited}s -- followers may not find peers" >&2
  fi

  idx=5
  for c in $ALL_SELECTED; do
    P2P=$((BASE_P2P_PORT + 5 + (idx - 5) * ALL_P2P_STRIDE))
    RPC=$(all_follower_rpc "$idx"); WS=$(all_follower_ws "$idx"); AUTH=$(all_follower_auth "$idx")
    DD="./nodes/${idx}-${c}"
    echo "Starting node $idx ($c, sync-only follower) rpc=$RPC ..."
    # Deliberately NO --name below. join.sh composes the canonical identity
    # itself (machine-client-version-commit-nodetype-ip) and takes the machine
    # component from the exported MACHINE, so the operator's --name -- or the
    # hostname fallback -- reaches every follower unchanged. Passing a composed
    # name here would land in the MACHINE slot and duplicate the client token:
    # "itlocal-xone-<host>-06-xone-vxdc-xone-mint-...". The node index is not
    # needed for uniqueness here -- exactly one follower runs per client, so the
    # client token already disambiguates them.
    bash ./join.sh --client "$c" --genesis ./genesis/genesis.json \
      --chainid "$CHAINID" --bootnodes "$BOOTNODE_ENODE" --ip "$IP" \
      --datadir "$DD" --port "$P2P" --rpcport "$RPC" --wsport "$WS" \
      --authrpc-port "$AUTH" --gas-limit 420000000 \
      ${STATIC_PEERS:+--staticpeers "$STATIC_PEERS"} \
      > "${DD}.log" 2>&1 &
    child_proc="$child_proc $!"
    idx=$((idx + 1))
  done
  echo "Follower logs: ./nodes/5-*.log ... (join.sh output per client)"
fi

# Last line before we block on `wait`, so it survives the wall of block logs that
# follows and is what the operator actually sees when setup.sh hands over. The
# chain id is interpolated rather than hardcoded: every network started from this
# repo gets a link to ITSELF, not to whichever net the dashboard defaults to.
if _skynet=$(skynet_url "$CHAINID"); then
  echo ""
  echo "Skynet stats: $_skynet"
fi

wait
