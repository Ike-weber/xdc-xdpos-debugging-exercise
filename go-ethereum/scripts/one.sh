#!/usr/bin/env bash
# =============================================================================
#  XDC NODE  —  one.sh  —  all-in-one manager  (mainnet · apothem · devnet)
# -----------------------------------------------------------------------------
#  Zero prerequisites. One script does everything — it even fetches the node
#  program for your OS/CPU automatically from xdc.network, so you never scp a
#  binary. Just type a word after it:
#
#  Pick a CLIENT, NETWORK and MODE by exporting before the command, or via flags:
#     CLIENT=geth|erigon|reth|besu|nethermind|xone   (default geth)
#     NETWORK=mainnet|apothem|devnet             (default mainnet)
#     NODETYPE=fast|full|minimal|snap|archive|validator  (default per-client)
#  e.g.  CLIENT=erigon NETWORK=apothem ./one.sh setup
#        ./one.sh --client reth --network apothem start
#        ./one.sh clients        ← print the capability matrix
#
#     ./one.sh setup      First time: downloads the matching binary +
#                         snapshot (geth) so your node starts near tip in
#                         minutes — or prepares an empty datadir for genesis sync.
#     ./one.sh start      Start the node (auto-fetches binary if missing).
#     ./one.sh status     Pretty sync status: % synced, block, peers, timings.
#     ./one.sh stop       Stop the node (safely).
#     ./one.sh restart    Stop, then start.
#     ./one.sh logs       Watch the live log.
#     ./one.sh attach     Open the geth console (advanced, geth only).
#     ./one.sh clients    Print the per-client capability matrix.
#
#  ONE_SH_VERSION: 1.2.0
# =============================================================================
set -uo pipefail

ONE_SH_VERSION="1.2.0"

# ----------------------------- PRE-PARSE FLAGS --------------------------------
# Allow --client/--mode/--network flags anywhere before the action word.
# Keeps CLIENT/NODETYPE/NETWORK env fallbacks working (existing pattern).
_pre_client="${CLIENT:-}"; _pre_mode="${NODETYPE:-}"; _pre_net="${NETWORK:-}"
_args_remaining=""
while [ "${1+set}" = "set" ] && [ $# -gt 0 ]; do
  case "$1" in
    --client|-c)  _pre_client="${2:?'--client requires a value'}"; shift 2 ;;
    --mode|-m)    _pre_mode="${2:?'--mode requires a value'}";     shift 2 ;;
    --network|-n) _pre_net="${2:?'--network requires a value'}";   shift 2 ;;
    --no-snapshot|--no-snap) NO_SNAPSHOT=1; shift ;;
    --)           shift; _args_remaining="$*"; break ;;
    *)            _args_remaining="${_args_remaining:+$_args_remaining }$1"; shift ;;
  esac
done
# Re-set positional args to the remaining (action) words
# shellcheck disable=SC2086
set -- ${_args_remaining:-}

CLIENT="${_pre_client:-geth}"
NODETYPE="${_pre_mode:-}"       # resolved per-client below
NO_SNAPSHOT="${NO_SNAPSHOT:-0}" # --no-snapshot: skip snapshot restore, sync from genesis
NETWORK="${_pre_net:-mainnet}"

# Fleet mode: if CLIENT=all, handle it entirely in cmd_all before client_info
# is called — client_info does not know about "all".
_CLIENT_ALL=0
[ "$CLIENT" = "all" ] && _CLIENT_ALL=1

# ----------------------------- CAPABILITY REGISTRY ----------------------------
# client_info <client> → validates (client, network, mode) triple; sets:
#   CI_DEFAULT_MODE  CI_MODES  CI_NETWORKS  CI_DIST  CI_STATUS  CI_BUILD_URL
# Errors with a human-readable supported-list on bad combos.
client_info(){
  local _c="$1" _net="$2" _mode="$3"
  case "$_c" in
    geth)
      CI_NETWORKS="mainnet apothem devnet net5050"
      CI_MODES="fast full snap archive"
      CI_DEFAULT_MODE="fast"
      CI_DIST="binary"
      CI_STATUS="Stable"
      CI_BUILD_URL="https://github.com/XDCIndia/XDPoS-Testnet-Network-Stats"
      ;;
    erigon)
      CI_NETWORKS="mainnet apothem net5050 devnet"
      CI_MODES="minimal full archive"
      CI_DEFAULT_MODE="minimal"
      CI_DIST="binary"
      CI_STATUS="Beta"
      CI_BUILD_URL="https://github.com/XDCIndia/erigon-xdc"
      ;;
    reth)
      CI_NETWORKS="apothem mainnet net5050 devnet"
      CI_MODES="full"
      CI_DEFAULT_MODE="full"
      CI_DIST="binary"
      CI_STATUS="Experimental"
      CI_BUILD_URL="https://github.com/XDCIndia/reth-xdc"
      ;;
    besu)
      CI_NETWORKS="apothem mainnet net5050 devnet"
      CI_MODES="full"
      CI_DEFAULT_MODE="full"
      CI_DIST="tarball"
      CI_STATUS="Experimental"
      CI_BUILD_URL="https://github.com/XDCIndia/besu/pull/29"
      ;;
    nethermind)
      CI_NETWORKS="mainnet apothem net5050 devnet"
      CI_MODES="fast full archive"
      CI_DEFAULT_MODE="fast"
      CI_DIST="tarball"
      CI_STATUS="Beta"
      CI_BUILD_URL="https://github.com/XDCIndia/nethermind-xdc"
      ;;
    xone)
      CI_NETWORKS="apothem mainnet devnet net5050"
      CI_MODES="snap full archive validator"
      CI_DEFAULT_MODE="snap"
      CI_DIST="binary"
      CI_STATUS="Experimental"
      CI_BUILD_URL="https://github.com/XDCIndia/xOneGo"
      ;;
    *)
      err "Unknown client '$_c'. Valid: geth erigon reth besu nethermind xone"; exit 1 ;;
  esac

  # Validate network
  local _nok=0
  for _n in $CI_NETWORKS; do [ "$_net" = "$_n" ] && _nok=1 && break; done
  if [ "$_nok" = "0" ]; then
    err "Client '$_c' does not support network '$_net'."
    err "  Supported networks: $CI_NETWORKS"
    exit 1
  fi

  # Validate mode (empty = use default, set it)
  if [ -z "$_mode" ]; then
    NODETYPE="$CI_DEFAULT_MODE"
  else
    local _mok=0
    for _m in $CI_MODES; do [ "$_mode" = "$_m" ] && _mok=1 && break; done
    if [ "$_mok" = "0" ]; then
      err "Client '$_c' does not support mode '$_mode'."
      err "  Supported modes: $CI_MODES"
      exit 1
    fi
    NODETYPE="$_mode"
  fi

  # Experimental clients + mainnet: gate behind confirm / XDC_EXPERIMENTAL=1
  if { [ "$_c" = "reth" ] || [ "$_c" = "xone" ]; } && [ "$_net" = "mainnet" ]; then
    if [ "${XDC_EXPERIMENTAL:-0}" != "1" ]; then
      warn "$_c on mainnet is EXPERIMENTAL — consensus parity with mainnet is not yet confirmed."
      warn "Do not use it to validate mainnet blocks or manage real funds."
      if [ -t 0 ]; then
        printf '   Continue anyway? [y/N] '
        read -r _yn; [ "$_yn" = "y" ] || [ "$_yn" = "Y" ] || { warn "Aborted. Use --network apothem for tested $_c support."; exit 1; }
      else
        warn "Non-interactive run. Set XDC_EXPERIMENTAL=1 to proceed with $_c+mainnet."
        exit 1
      fi
    fi
  fi
}

# ----------------------------- PRETTY OUTPUT (hoisted — used by client_info + banner) ---
if [ -t 1 ]; then B=$'\033[1m'; G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[31m'; C=$'\033[36m'; D=$'\033[2m'; N=$'\033[0m'; TTY=1; else B=""; G=""; Y=""; R=""; C=""; D=""; N=""; TTY=0; fi
hr(){ printf '%s\n' "──────────────────────────────────────────────────────────────"; }
ok(){   printf '   %s✓%s %s\n' "$G" "$N" "$*"; }
warn(){ printf '   %s•%s %s\n' "$Y" "$N" "$*"; }
err(){  printf '   %s✗%s %s\n' "$R" "$N" "$*"; }
step(){ printf '\n%s▶ %s%s\n' "$B" "$*" "$N"; }

# ----------------------------- CONFIG (edit if needed) -----------------------
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ZSTDDEC="$DIR/zstddec"
LOGDIR="$DIR/logs"; LOG="$LOGDIR/node.log"
PIDFILE="$DIR/node.pid"
# Everything is served from xdc.network (host 95.217.56.168 : /mnt/data/snapshots)
BASE_URL="${BASE_URL:-https://xdc.network/snapshots}"
BIN_BASE_URL="${BIN_BASE_URL:-$BASE_URL/bin}"

# ----------------------------- NETWORK RESOLUTION ----------------------------
case "$NETWORK" in
  apothem|testnet)    NETWORK="apothem";   CHAIN_FLAG="--apothem";        NET_LABEL="APOTHEM"  ;;
  devnet|5551)        NETWORK="devnet";    CHAIN_FLAG="--xdcdevnet";       NET_LABEL="DEVNET"   ;;
  devnet5151|5151)    NETWORK="devnet5151"; CHAIN_FLAG="--networkid 5151"; NET_LABEL="DEVNET5151" ;;
  net5050|5050)       NETWORK="net5050";   CHAIN_FLAG="--networkid 5050";  NET_LABEL="NET5050"  ;;
  *)                  NETWORK="mainnet";   CHAIN_FLAG="--xdcmainnet";      NET_LABEL="MAINNET"  ;;
esac

# Validate the (client, network, mode) triple — also resolves NODETYPE default.
# Skip for CLIENT=all — cmd_all handles each individual client's validation.
if [ "$_CLIENT_ALL" = "0" ]; then
  client_info "$CLIENT" "$NETWORK" "$NODETYPE"
fi

# net5050 / devnet5151 mode-force: snap/fast have no peer support (#96); full-from-genesis
# is correct (these nets are small so it is the storage-correct path too).
# devnet (5551): geth uses --xdcdevnet (embedded bootnodes, no snapshot) so force full as well.
if { [ "$NETWORK" = "net5050" ] || [ "$NETWORK" = "devnet5151" ] || [ "$NETWORK" = "devnet" ]; } \
    && [ "$NODETYPE" != "full" ] && [ "$NODETYPE" != "archive" ]; then
  warn "$NETWORK peers serve no snap/fast state (#96) — forcing --mode full"
  warn "($NETWORK is small; full-from-genesis reaches tip with modest storage)"
  NODETYPE="full"
fi

# ----------------------------- CLIENT-SPECIFIC PATHS -------------------------
# Datadir is $DIR/data for all clients; binary name matches client
DATADIR="$DIR/data"
IPC="$DATADIR/XDC.ipc"
CONFIG="$DIR/config.toml"
CLIENT_MARKER="$DIR/.client"

# Protect against running a different client over an existing client's dir
if [ -f "$CLIENT_MARKER" ]; then
  _existing="$(cat "$CLIENT_MARKER" 2>/dev/null || true)"
  if [ -n "$_existing" ] && [ "$_existing" != "$CLIENT" ]; then
    err "Directory '$DIR' was set up for client '$_existing', not '$CLIENT'."
    err "Use a different directory: XDC_DIR=\$HOME/xdc-node-${CLIENT} ./one.sh setup"
    exit 1
  fi
fi

# ----------------------------- GETH SPECIFICS --------------------------------
# These vars are only meaningful for the geth path; kept inline (no refactor).
GETH="$DIR/geth"
CHAINDATA="$DATADIR/XDC/chaindata"
SNAP="$DIR/snapshot.tar.zst"
CACHE=2048; MEMLIMIT="3GiB"

# geth snapshot URL (network-aware)
case "$NETWORK" in
  apothem) _DEFAULT_SNAP="xdc168-apothem-fast-latest.tar.zst" ;;
  mainnet) _DEFAULT_SNAP="xdc168-mainnet-fast-latest.tar.zst" ;;
  *)       _DEFAULT_SNAP="" ;;
esac
SNAP_URL="${SNAP_URL:-${_DEFAULT_SNAP:+$BASE_URL/$_DEFAULT_SNAP}}"
# --no-snapshot: force genesis sync (no snapshot restore) for geth.
[ "${NO_SNAPSHOT:-0}" = "1" ] && SNAP_URL=""

# geth NODETYPE → sync args + state scheme + snapshot restore flag
SNAP_ENV=""; RESTORE=1
case "$NODETYPE" in
  snap)    SYNC_ARGS="--syncmode snap --gcmode full";    SCHEME="path"; RESTORE=0; SNAP_ENV="XDC_EXPERIMENTAL_SNAPSYNC=1" ;;
  full)    SYNC_ARGS="--syncmode full --gcmode full";    SCHEME="hash"; RESTORE=1 ;;
  archive) SYNC_ARGS="--syncmode full --gcmode archive"; SCHEME="hash"; RESTORE=0 ;;
  *)       SYNC_ARGS="--syncmode fast --gcmode full";    SCHEME="hash"; RESTORE=1 ;; # fast
esac
[ -z "${SNAP_URL:-}" ] && RESTORE=0

# ----------------------------- PLATFORM DETECTION ----------------------------
OSID="$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')"; [ "$OSID" = "darwin" ] && OSID="macos"; [ -z "$OSID" ] && OSID="os"
detect_platform(){
  local os arch
  os="$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')"
  case "$os" in linux) os=linux ;; darwin) os=darwin ;; *) os=linux ;; esac
  case "$(uname -m 2>/dev/null)" in
    x86_64|amd64)         arch=amd64 ;;
    arm64|aarch64|armv8*) arch=arm64 ;;
    *)                    arch=amd64 ;;
  esac
  PLATFORM="${os}-${arch}"
}

# ----------------------------- NODE NAME ------------------------------------
HOST="${HOST:-$(hostname -s 2>/dev/null | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]//g')}"; [ -z "$HOST" ] && HOST="xdcnode"
LOCATION="${LOCATION:-$(curl -s --max-time 5 https://ipinfo.io/country 2>/dev/null | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z')}"; [ -z "$LOCATION" ] && LOCATION="xx"
NODE_NAME="${HOST}-${NETWORK}-${NODETYPE}-${OSID}-${LOCATION}"

# geth-specific
STATS_SECRET="xdc_openscan_stats_2026"
ETHSTATS="${NODE_NAME}:${STATS_SECRET}@stats.xdcindia.com:443"
PEER1="enode://7603346c94fbb6e8f6351062bd9ee784605268dae543bc67cd542fc430dee48b4b733c153a2606f5429ea00a8d900b489a7a6e84eb80416862c466ee3834b105@167.235.13.113:30310"
PEER2="enode://86d5581bf8e7c1fb76677a4245c62587769319fad0986bb4d9ff4c8357f604e8dc4b66b7120dc8fa316c6042e51a0cf2a1921cffad432d1488b134fde3717006@185.180.220.183:30320"

# Per-client port assignments (distinct defaults so two clients coexist on one box)
# CLIENT=all: ports are assigned per-client inside cmd_all; use geth defaults as a placeholder.
case "$CLIENT" in
  geth)        RPC_PORT=8565; WS_PORT=8566; P2P_PORT=30306; AUTH_PORT=8567 ;;
  erigon)      RPC_PORT=8575; WS_PORT=8576; P2P_PORT=30316; AUTH_PORT=8577 ;;
  reth)        RPC_PORT=8585; WS_PORT=8586; P2P_PORT=30326; AUTH_PORT=8587 ;;
  besu)        RPC_PORT=8595; WS_PORT=8596; P2P_PORT=30336; AUTH_PORT=8597 ;;
  nethermind)  RPC_PORT=8605; WS_PORT=8606; P2P_PORT=30346; AUTH_PORT=8607 ;;
  *)           RPC_PORT=8565; WS_PORT=8566; P2P_PORT=30306; AUTH_PORT=8567 ;;
esac

# ----------------------------- CROSS-PLATFORM HELPERS -----------------------
fsize(){ [ -e "$1" ] || { echo 0; return; }; stat -c%s "$1" 2>/dev/null || stat -f%z "$1" 2>/dev/null || wc -c <"$1" 2>/dev/null | tr -d ' ' || echo 0; }
sha256_of(){ if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | awk '{print $1}'; else shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; fi; }
ZDEC(){ if command -v zstd >/dev/null 2>&1; then zstd -dc; else "$ZSTDDEC"; fi; }
fmt_dur(){ local s=${1:-0}; [ "$s" -lt 0 ] 2>/dev/null && s=0; printf '%dm %02ds' "$((s/60))" "$((s%60))"; }
gb(){ awk "BEGIN{printf \"%.2f\", ${1:-0}/1073741824}"; }
mbps(){ awk "BEGIN{printf \"%.0f\", ${1:-0}/1048576}"; }

banner(){ hr; printf '%s   XDC %s %s NODE%s   %s(%s · %s)%s\n' "$B" "$NET_LABEL" "$(printf '%s' "$NODETYPE" | tr '[:lower:]' '[:upper:]')" "$N" "$D" "$(basename "$DIR")" "$CLIENT" "$N"; printf '%s   node name:%s %s\n' "$D" "$N" "$NODE_NAME"; hr; }

draw_bar(){
  local cur=$1 total=$2 label=${3:-}
  local pct=0; [ "${total:-0}" -gt 0 ] 2>/dev/null && pct=$(( cur*100/total )); [ "$pct" -gt 100 ] && pct=100
  local width=32 filled i=0 bar=""
  filled=$(( pct*width/100 ))
  while [ "$i" -lt "$filled" ]; do bar="$bar#"; i=$((i+1)); done
  while [ "$i" -lt "$width" ]; do bar="$bar."; i=$((i+1)); done
  if [ "$TTY" = 1 ]; then
    printf '\r   %s[%s]%s %3d%%  %s / %s GB  %s   ' "$C" "$bar" "$N" "$pct" "$(gb "$cur")" "$(gb "$total")" "$label"
  else
    printf '   [%s] %3d%%  %s / %s GB  %s\n' "$bar" "$pct" "$(gb "$cur")" "$(gb "$total")" "$label"
  fi
}

is_running(){ [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; }
rpc(){ curl -s -m6 -X POST -H 'Content-Type:application/json' --data "{\"jsonrpc\":\"2.0\",\"method\":\"$1\",\"params\":${2:-[]},\"id\":1}" "http://127.0.0.1:$RPC_PORT" 2>/dev/null; }
h2d(){ local h; h=$(echo "$1"|grep -oE '0x[0-9a-f]+'|head -1); [ -n "$h" ] && printf '%d' "$h" || echo ""; }

# ----------------------------- DISK SPACE WARN (non-blocking) ----------------
_warn_disk(){
  local need_gb="$1" label="$2"
  local avail_kb; avail_kb=$(df -k "$DIR" 2>/dev/null | awk 'NR==2{print $4}')
  local avail_gb; avail_gb=$(( ${avail_kb:-0} / 1048576 ))
  if [ "${avail_gb:-0}" -lt "$need_gb" ]; then
    warn "Disk space looks tight (~${avail_gb} GB free, ${label} may need ~${need_gb} GB)."
    warn "This is a warning only — continuing. Add more disk if the setup stalls."
  fi
}

# ==================== GETH PATH (UNCHANGED) ==================================
ensure_config(){
  [ -f "$CONFIG" ] && return 0
  if [ "$NETWORK" = "apothem" ]; then
    cat > "$CONFIG" <<TOML
[Node.P2P]
TOML
  elif [ "$NETWORK" = "net5050" ]; then
    cat > "$CONFIG" <<TOML
[Node.P2P]
StaticNodes  = [
  "enode://507918630af3a6080867ec3987e9d0054bbb18c97ef38ae39f05f27a13371aab067b2c9e97099c554582b542946742d999e77a84da33f2ac07498a8f03d6c261@65.21.71.4:30550",
  "enode://7d78c32e5dfade9319b1fb564d02fac8aa1474fd01cfa6a6dce689c4f16aa2f742d4492db09930124185f8942abe1bd6b5fa1b2b572f1cbe2641e7411299e14d@65.21.71.4:30552",
  "enode://a4b966a579d1c7abe9f8856a612978db20483ce0eabca2c9ed9768a05d08e2114d14568931fbd3e62fa5f533bfe65d40381ed014c56a67ed8ab212d85bb9d403@65.21.71.4:30554",
  "enode://d2c5ccdc96fe0d4574242572e98057834e7bf5ea6a01afd1e15e81c0bdda735788367581c57f6a79502664d6163c4b3bbb223163382daa1f0c2d3f77d326c709@185.180.220.183:30550",
  "enode://5baefc523ad83eccf9021a1ccb7695496cd4bf82fe96c82557e4238869e50bb8ede9cdb2417d6bde1e04ce000a0186bf3f4f08992fea80703a2788c4342ebf34@95.217.56.168:30551"
]
TrustedNodes = [
  "enode://507918630af3a6080867ec3987e9d0054bbb18c97ef38ae39f05f27a13371aab067b2c9e97099c554582b542946742d999e77a84da33f2ac07498a8f03d6c261@65.21.71.4:30550",
  "enode://7d78c32e5dfade9319b1fb564d02fac8aa1474fd01cfa6a6dce689c4f16aa2f742d4492db09930124185f8942abe1bd6b5fa1b2b572f1cbe2641e7411299e14d@65.21.71.4:30552",
  "enode://a4b966a579d1c7abe9f8856a612978db20483ce0eabca2c9ed9768a05d08e2114d14568931fbd3e62fa5f533bfe65d40381ed014c56a67ed8ab212d85bb9d403@65.21.71.4:30554",
  "enode://d2c5ccdc96fe0d4574242572e98057834e7bf5ea6a01afd1e15e81c0bdda735788367581c57f6a79502664d6163c4b3bbb223163382daa1f0c2d3f77d326c709@185.180.220.183:30550",
  "enode://5baefc523ad83eccf9021a1ccb7695496cd4bf82fe96c82557e4238869e50bb8ede9cdb2417d6bde1e04ce000a0186bf3f4f08992fea80703a2788c4342ebf34@95.217.56.168:30551"
]
TOML
  elif [ "$NETWORK" = "devnet5151" ]; then
    # devnet5151 (chainId 5151) has no live nodes at time of wiring — no enodes.
    # When a validator is spun up, add its enode here as a StaticNode.
    # Genesis: https://xdc.network/snapshots/devnet5151-genesis.json (sha256 96986a4a)
    cat > "$CONFIG" <<TOML
[Node.P2P]
StaticNodes  = []
TrustedNodes = []
TOML
  elif [ "$NETWORK" = "devnet" ]; then
    # devnet (5551): geth uses --xdcdevnet which carries embedded bootnodes — no
    # static peers needed in config.toml for geth.  Non-geth clients must pass
    # --bootnodes / --trusted-peers explicitly in their start-arg fragments.
    cat > "$CONFIG" <<TOML
[Node.P2P]
TOML
  else
    cat > "$CONFIG" <<TOML
[Node.P2P]
StaticNodes  = ["$PEER1","$PEER2"]
TrustedNodes = ["$PEER1","$PEER2"]
TOML
  fi
}

ensure_geth(){
  if [ -x "$GETH" ] && "$GETH" version >/dev/null 2>&1; then return 0; fi
  detect_platform
  local url="$BIN_BASE_URL/geth-$PLATFORM"
  step "Get the node program — geth for $PLATFORM"
  printf '   %ssource:%s %s\n' "$D" "$N" "$url"
  local T0; T0=$(date +%s)
  if [ "$TTY" = 1 ]; then
    curl -fL --retry 10 --retry-delay 3 --retry-all-errors -A 'Mozilla/5.0' --progress-bar -o "$GETH.tmp" "$url" || { err "Could not download geth for $PLATFORM from $url"; err "Check the platform is published under $BIN_BASE_URL/"; exit 1; }
  else
    curl -fL --retry 10 --retry-delay 3 --retry-all-errors -A 'Mozilla/5.0' -s -o "$GETH.tmp" "$url" || { err "Could not download geth for $PLATFORM from $url"; exit 1; }
  fi
  local want; want=$(curl -s -A 'Mozilla/5.0' --max-time 20 "$url.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
  if [ -n "$want" ]; then
    local got; got=$(sha256_of "$GETH.tmp")
    if [ "$got" != "$want" ]; then err "geth integrity check FAILED (sha256 mismatch) — aborting."; rm -f "$GETH.tmp"; exit 1; fi
    ok "binary integrity verified (sha256)"
  fi
  chmod +x "$GETH.tmp"
  if ! "$GETH.tmp" version >/dev/null 2>&1; then err "downloaded geth will not run on this platform ($PLATFORM)."; rm -f "$GETH.tmp"; exit 1; fi
  mv "$GETH.tmp" "$GETH"
  local ver; ver=$("$GETH" version 2>/dev/null | awk -F': ' '/^Version/{print $2; exit}')
  ok "node program ready (geth ${ver:-?}, $PLATFORM, $(gb "$(fsize "$GETH")") GB)"
  printf '   %s⏱  binary download stage: %s%s\n' "$D" "$(fmt_dur $(( $(date +%s) - T0 )))" "$N"
}

cmd_setup_geth(){
  local SETUP_T0; SETUP_T0=$(date +%s); local BIN_SECS=0 DL_SECS=0 EX_SECS=0
  step "Step 1 of 5  —  Get the node program (geth) for your OS/CPU"
  local _b0; _b0=$(date +%s); ensure_geth; BIN_SECS=$(( $(date +%s) - _b0 ))

  step "Step 2 of 5  —  Check for existing chain data"
  if [ -d "$CHAINDATA" ] && [ -n "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
    ok "Chain data already here ($(du -sh "$CHAINDATA" 2>/dev/null|cut -f1)). No download needed."
    printf '\n   %sNext step:%s %s./one.sh start%s\n' "$B" "$N" "$G" "$N"; return 0
  fi
  if [ "${RESTORE:-1}" != "1" ]; then
    mkdir -p "$DATADIR/XDC"
    warn "Node type '$NODETYPE' on '$NETWORK' syncs over the network (no snapshot restore)."
    warn "First sync to the tip can take a while — watch it with ./one.sh status."
    # net5050 / devnet5151: download genesis + run geth init so the datadir carries
    # the correct chainId rather than defaulting to mainnet.
    # devnet (5551): geth uses --xdcdevnet which has the genesis embedded — no init needed.
    # Skip if chaindata already populated.
    local _needs_genesis_init=0
    local _genesis_url="" _genesis_file="" _genesis_chain_label=""
    if [ "$NETWORK" = "net5050" ] && [ -z "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
      _needs_genesis_init=1
      _genesis_url="$BASE_URL/net5050-genesis.json"
      _genesis_file="$DIR/net5050-genesis.json"
      _genesis_chain_label="net5050 (chainId 5050)"
    fi
    if [ "$NETWORK" = "devnet5151" ] && [ -z "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
      _needs_genesis_init=1
      _genesis_url="$BASE_URL/devnet5151-genesis.json"
      _genesis_file="$DIR/devnet5151-genesis.json"
      _genesis_chain_label="devnet5151 (chainId 5151)"
    fi
    if [ "$_needs_genesis_init" = "1" ]; then
      if [ ! -f "$_genesis_file" ]; then
        step "Downloading $_genesis_chain_label genesis…"
        curl -fsSL -A 'Mozilla/5.0' --max-time 60 "$_genesis_url" -o "$_genesis_file" || {
          err "Failed to download genesis from $_genesis_url"
          exit 1
        }
        ok "$_genesis_chain_label genesis downloaded"
      fi
      step "Initialising datadir with $_genesis_chain_label genesis…"
      "$GETH" --datadir "$DATADIR" --state.scheme "$SCHEME" init "$_genesis_file" >/dev/null 2>&1 || {
        err "geth init with $_genesis_chain_label genesis failed — check genesis file integrity"
        exit 1
      }
      ok "Datadir initialised with $_genesis_chain_label genesis"
    fi
    date +%s > "$DIR/.setup_done_at" 2>/dev/null
    ok "Datadir prepared."
    printf '\n   %sNext step:%s  %s./one.sh start%s     %sthen:%s ./one.sh status\n' "$B" "$N" "$G" "$N" "$D" "$N"
    return 0
  fi
  command -v zstd >/dev/null 2>&1 || [ -x "$ZSTDDEC" ] || { err "need a zstd decompressor: install 'zstd' (apt install zstd / brew install zstd) or place 'zstddec' here."; exit 1; }
  _warn_disk 20 "geth fast snapshot"
  warn "No chain data yet. Downloading the latest snapshot so your node starts"
  warn "near the live chain (a few minutes) instead of from block 0 (many days)."

  step "Step 3 of 5  —  Download latest snapshot"
  printf '   %ssource:%s %s\n' "$D" "$N" "$SNAP_URL"
  local total; total=$(curl -sIL --retry 3 -A 'Mozilla/5.0' "$SNAP_URL" | tr -d '\r' | awk 'tolower($1)=="content-length:"{v=$2} END{print v+0}')
  printf '   %ssize:%s ~%s GB   %s(resumable — safe to re-run if interrupted)%s\n' "$D" "$N" "$(gb "$total")" "$D" "$N"
  local DL_T0; DL_T0=$(date +%s)
  curl -fL -C - --retry 30 --retry-delay 5 --retry-all-errors -A 'Mozilla/5.0' -s -o "$SNAP" "$SNAP_URL" &
  local dl=$!
  local pc; pc=$(fsize "$SNAP"); local pt; pt=$(date +%s)
  while kill -0 "$dl" 2>/dev/null; do
    local cur; cur=$(fsize "$SNAP")
    local now; now=$(date +%s); local dt=$(( now-pt )); local dc=$(( cur-pc )); local sp=0
    [ "$dt" -gt 0 ] && [ "$dc" -gt 0 ] && sp=$(( dc/dt ))
    local eta="--"; [ "$sp" -gt 0 ] && [ "${total:-0}" -gt "$cur" ] && eta="$(( (total-cur)/sp/60 ))m"
    draw_bar "$cur" "$total" "$(mbps "$sp") MB/s  eta ${eta}"
    pc=$cur; pt=$now
    sleep 3
  done
  wait "$dl" || { echo; err "Download failed. Re-run ./one.sh setup (it resumes)."; exit 1; }
  draw_bar "$total" "$total" "done"; echo
  ok "Downloaded $(gb "$(fsize "$SNAP")") GB"
  DL_SECS=$(( $(date +%s) - DL_T0 )); printf '   %s⏱  snapshot download stage: %s%s\n' "$D" "$(fmt_dur "$DL_SECS")" "$N"

  local want; want=$(curl -s -A 'Mozilla/5.0' --max-time 25 "${SNAP_URL}.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
  if [ -n "$want" ]; then
    step "Verifying snapshot integrity (sha256)…"
    local got; got=$(sha256_of "$SNAP")
    if [ "$got" != "$want" ]; then
      err "Integrity check FAILED — file is corrupt (likely a leftover partial from a different snapshot)."
      warn "Deleting and re-downloading fresh (no resume)…"
      rm -f "$SNAP"
      curl -fL --retry 30 --retry-delay 5 --retry-all-errors -A 'Mozilla/5.0' -s -o "$SNAP" "$SNAP_URL" || { err "Re-download failed."; exit 1; }
      got=$(sha256_of "$SNAP")
      [ "$got" = "$want" ] || { err "Still corrupt after fresh download — aborting (check network)."; exit 1; }
    fi
    ok "Integrity verified — sha256 matches"
  else
    warn "No published checksum found — skipping integrity check"
  fi

  step "Step 4 of 5  —  Restore snapshot (extract — takes a few minutes)"
  local EX_T0; EX_T0=$(date +%s)
  rm -rf "$DIR/_stage"; mkdir -p "$DIR/_stage"
  local expect=$(( total*3 ))
  ZDEC < "$SNAP" | tar -xf - -C "$DIR/_stage" &
  local xp=$!
  while kill -0 "$xp" 2>/dev/null; do
    local xs; xs=$(du -sk "$DIR/_stage" 2>/dev/null | cut -f1); xs=$(( ${xs:-0}*1024 ))
    draw_bar "$xs" "$expect" "extracting…"
    sleep 3
  done
  wait "$xp" || { echo; err "Extraction failed."; exit 1; }
  echo
  local cd; cd=$(find "$DIR/_stage" -maxdepth 3 -type d -name chaindata | head -1)
  [ -z "$cd" ] && ls "$DIR/_stage"/*.sst >/dev/null 2>&1 && cd="$DIR/_stage"
  [ -z "$cd" ] && { err "chaindata not found inside snapshot."; exit 1; }
  mkdir -p "$DATADIR/XDC"; rm -rf "$CHAINDATA"; mv "$cd" "$CHAINDATA"
  rm -rf "$DIR/_stage" "$SNAP"
  ok "Restored to chaindata ($(du -sh "$CHAINDATA" 2>/dev/null|cut -f1))"
  EX_SECS=$(( $(date +%s) - EX_T0 )); printf '   %s⏱  extract stage:  %s%s\n' "$D" "$(fmt_dur "$EX_SECS")" "$N"

  step "Step 5 of 5  —  Done"
  ok "Your node is set up with a recent snapshot of the XDC $NETWORK."
  local SETUP_SECS=$(( $(date +%s) - SETUP_T0 ))
  printf '   %s⏱  setup total: %s%s   %s(binary %s · snapshot %s · extract %s)%s\n' "$B" "$(fmt_dur "$SETUP_SECS")" "$N" "$D" "$(fmt_dur "$BIN_SECS")" "$(fmt_dur "$DL_SECS")" "$(fmt_dur "$EX_SECS")" "$N"
  date +%s > "$DIR/.setup_done_at" 2>/dev/null
  printf '\n   %sNext step:%s  %s./one.sh start%s     %sthen:%s ./one.sh status\n' "$B" "$N" "$G" "$N" "$D" "$N"
}

cmd_start_geth(){
  if is_running; then ok "Already running (pid $(cat "$PIDFILE")). See: ./one.sh status"; return 0; fi
  ensure_geth
  if [ "$NETWORK" = "devnet" ] && ! "$GETH" --help 2>&1 | grep -q -- "--xdcdevnet"; then
    err "This node binary does not support the XDC devnet (chainId 5551) yet."
    err "Force a re-fetch: rm -f \"$GETH\" && ./one.sh start"
    return 1
  fi
  if [ ! -d "$CHAINDATA" ] || [ -z "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
    warn "No chain data found — running first-time setup automatically…"
    cmd_setup_geth || { err "Setup failed; not starting."; return 1; }
  fi
  ensure_config; mkdir -p "$LOGDIR"; ulimit -n 65536 2>/dev/null || true
  step "Starting the node…"
  # SNAP_ENV holds a VAR=val string (snap mode). A bare ${SNAP_ENV} in the
  # command prefix is parsed as a command word (not an assignment) after
  # expansion, so export it instead of inlining it.
  [ -n "${SNAP_ENV:-}" ] && export "${SNAP_ENV?}"
  XDC_SNAP_HEAL=1 GOMEMLIMIT="$MEMLIMIT" nohup "$GETH" \
    --datadir "$DATADIR" --config "$CONFIG" $CHAIN_FLAG --port "$P2P_PORT" \
    $SYNC_ARGS --state.scheme "$SCHEME" \
    --http --http.addr 127.0.0.1 --http.port "$RPC_PORT" --http.api eth,net,web3,debug,admin,XDPoS \
    --ws --ws.port "$WS_PORT" --authrpc.addr 127.0.0.1 --authrpc.port "$AUTH_PORT" \
    --cache "$CACHE" --maxpeers 50 --verbosity 3 \
    --ethstats "$ETHSTATS" --ipcpath "$IPC" >> "$LOG" 2>&1 &
  echo $! > "$PIDFILE"; sleep 6
  if is_running; then ok "Started (pid $(cat "$PIDFILE"))."; date +%s > "$DIR/.started_at" 2>/dev/null
    printf '\n   %sWatch progress:%s %s./one.sh status%s   %s(shows catch-up time + time-to-tip)%s\n' "$B" "$N" "$G" "$N" "$D" "$N"
  else err "Did not start — check: ./one.sh logs"; fi
}

# ==================== NON-GETH PATH ==========================================

# ensure_client_fragment: source the per-client logic fragment.
# Looks for a local ./clients.d/$CLIENT.sh first (re-run safe, offline safe),
# then fetches from $BASE_URL/clients.d/$CLIENT.sh with sha256 sidecar check.
ensure_client_fragment(){
  local frag="$DIR/clients.d/${CLIENT}.sh"
  if [ -f "$frag" ]; then
    ok "Using local fragment: $frag"
    # shellcheck source=/dev/null
    . "$frag"; return 0
  fi
  local url="$BASE_URL/clients.d/${CLIENT}.sh"
  step "Fetching client fragment for $CLIENT"
  printf '   %ssource:%s %s\n' "$D" "$N" "$url"
  mkdir -p "$DIR/clients.d"
  curl -fsSL -A 'Mozilla/5.0' --max-time 60 "$url" -o "$frag.tmp" 2>/dev/null || {
    err "Could not fetch fragment for '$CLIENT' from $url"
    err "  This client's fragment may not be published yet."
    err "  Build pointer: $CI_BUILD_URL"
    err "  Place '$DIR/clients.d/${CLIENT}.sh' manually and re-run."
    rm -f "$frag.tmp"; exit 1
  }
  # sha256 sidecar (optional — skip if not published yet, same trust level as one.sh)
  local want; want=$(curl -s -A 'Mozilla/5.0' --max-time 20 "${url}.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
  if [ -n "$want" ]; then
    local got; got=$(sha256_of "$frag.tmp")
    if [ "$got" != "$want" ]; then err "Fragment sha256 mismatch — aborting."; rm -f "$frag.tmp"; exit 1; fi
    ok "Fragment integrity verified (sha256)"
  else
    warn "No published sha256 sidecar for fragment yet — skipping integrity check."
    warn "  Fetched from: $url  (same trust model as one.sh bootstrap)"
  fi
  mv "$frag.tmp" "$frag"
  # shellcheck source=/dev/null
  . "$frag"
}

# ensure_client_binary: HEAD-probe the binary URL; on 404 print boxed message
# and exit non-zero WITHOUT creating a half-populated datadir.
ensure_client_binary(){
  detect_platform
  local bin_name url
  case "$CI_DIST" in
    binary)  bin_name="${CLIENT}-${PLATFORM}"; url="$BIN_BASE_URL/${bin_name}" ;;
    tarball)
      case "$CLIENT" in
        besu)       bin_name="besu-latest.tar.gz" ;;
        nethermind) bin_name="nethermind-${PLATFORM}.tar.gz" ;;
        *)          bin_name="${CLIENT}-${PLATFORM}.tar.gz" ;;
      esac
      url="$BIN_BASE_URL/${bin_name}"
      ;;
  esac

  local client_bin="$DIR/${CLIENT}"
  # Allow an operator-provided binary to override the download
  if [ -x "$client_bin" ]; then ok "Using existing binary: $client_bin"; return 0; fi

  step "Checking binary availability for $CLIENT ($PLATFORM)"
  printf '   %ssource:%s %s\n' "$D" "$N" "$url"
  local http_code; http_code=$(curl -sfIL -A 'Mozilla/5.0' --max-time 20 -o /dev/null -w "%{http_code}" "$url" 2>/dev/null || echo "000")
  if [ "$http_code" = "404" ] || [ "$http_code" = "000" ]; then
    printf '\n'
    hr
    warn "Binary for $CLIENT on $PLATFORM is not yet published at xdc.network."
    printf '   %sExact URL checked:%s  %s\n' "$D" "$N" "$url"
    printf '   %sBuild from source:%s  %s\n' "$D" "$N" "$CI_BUILD_URL"
    printf '   %sLocal binary override:%s Place your compiled binary at:\n' "$D" "$N"
    printf '     %s%s%s\n' "$G" "$client_bin" "$N"
    printf '   %sand re-run./one.sh setup%s\n' "$D" "$N"
    printf '   %sOr point to a different mirror:%s  BIN_BASE_URL=https://your.mirror ./one.sh setup\n' "$D" "$N"
    hr
    exit 1
  fi
  ok "Binary available (HTTP $http_code) — will download during setup."

  # Check per-client requirements before touching the datadir
  if declare -f client_requirements >/dev/null 2>&1; then
    client_requirements || { err "Pre-flight requirements check failed."; exit 1; }
  fi
}

cmd_setup_nongeth(){
  ensure_client_fragment

  step "Step 1  —  Check requirements and binary for $CLIENT"
  ensure_client_binary

  step "Step 2  —  Check for existing data"
  if [ -d "$DATADIR" ] && [ -n "$(ls -A "$DATADIR" 2>/dev/null)" ]; then
    ok "Data directory already exists ($(du -sh "$DATADIR" 2>/dev/null|cut -f1)). No download needed."
    printf '\n   %sNext step:%s %s./one.sh start%s\n' "$B" "$N" "$G" "$N"; return 0
  fi

  # Check snapshot availability (HEAD-probe)
  local snap_name=""; [ "${NO_SNAPSHOT:-0}" = "1" ] || snap_name=$(client_snapshot_name "$NETWORK" "$NODETYPE" 2>/dev/null || true)
  local snap_url="" snap_avail=0
  if [ -n "$snap_name" ]; then
    snap_url="$BASE_URL/$snap_name"
    local sc; sc=$(curl -sfI -A 'Mozilla/5.0' --max-time 20 -o /dev/null -w "%{http_code}" "$snap_url" 2>/dev/null || echo "000")
    [ "$sc" = "200" ] && snap_avail=1
  fi
  if [ "$snap_avail" = "0" ]; then
    local _est_days; _est_days=$(client_sync_estimate "$NETWORK" "$NODETYPE" 2>/dev/null || echo "days")
    warn "No snapshot is currently published for ${CLIENT}/${NETWORK}/${NODETYPE}."
    warn "This node will sync from genesis over the network — expect ${_est_days}."
    warn "Check $CI_BUILD_URL for snapshot availability updates."
  fi

  _warn_disk 50 "$CLIENT $NODETYPE sync"

  step "Step 3  —  Download $CLIENT binary"
  detect_platform
  local bin_url
  case "$CI_DIST" in
    binary)  bin_url="$BIN_BASE_URL/${CLIENT}-${PLATFORM}" ;;
    tarball)
      case "$CLIENT" in
        besu)       bin_url="$BIN_BASE_URL/besu-latest.tar.gz" ;;
        nethermind) bin_url="$BIN_BASE_URL/nethermind-${PLATFORM}.tar.gz" ;;
        *)          bin_url="$BIN_BASE_URL/${CLIENT}-${PLATFORM}.tar.gz" ;;
      esac
      ;;
  esac
  local dl_dest="$DIR/${CLIENT}.bin.tmp"
  printf '   %ssource:%s %s\n' "$D" "$N" "$bin_url"
  if [ "$TTY" = 1 ]; then
    curl -fL --retry 10 --retry-delay 3 --retry-all-errors -A 'Mozilla/5.0' --progress-bar -o "$dl_dest" "$bin_url" || { err "Binary download failed."; exit 1; }
  else
    curl -fL --retry 10 --retry-delay 3 --retry-all-errors -A 'Mozilla/5.0' -s -o "$dl_dest" "$bin_url" || { err "Binary download failed."; exit 1; }
  fi
  local want; want=$(curl -s -A 'Mozilla/5.0' --max-time 20 "${bin_url}.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
  if [ -n "$want" ]; then
    local got; got=$(sha256_of "$dl_dest")
    [ "$got" = "$want" ] || { err "Binary sha256 mismatch — aborting."; rm -f "$dl_dest"; exit 1; }
    ok "Binary integrity verified (sha256)"
  else
    warn "No published sha256 for binary — skipping integrity check"
  fi
  # Install binary/tarball
  case "$CI_DIST" in
    binary)
      chmod +x "$dl_dest"; mv "$dl_dest" "$DIR/$CLIENT"
      ok "Binary installed: $DIR/$CLIENT"
      ;;
    tarball)
      mkdir -p "$DIR/${CLIENT}-dist"
      tar -xf "$dl_dest" -C "$DIR/${CLIENT}-dist" 2>/dev/null || { err "Failed to extract $CLIENT tarball."; rm -f "$dl_dest"; exit 1; }
      rm -f "$dl_dest"
      # Let the fragment's install_from_tarball hook finalise (symlink or copy the real executable)
      if declare -f client_install_tarball >/dev/null 2>&1; then
        client_install_tarball "$DIR/${CLIENT}-dist" "$DIR/$CLIENT" || { err "client_install_tarball hook failed."; exit 1; }
      else
        warn "No client_install_tarball hook — binary may need manual setup from $DIR/${CLIENT}-dist"
      fi
      ;;
  esac

  # Restore snapshot if available
  if [ "$snap_avail" = "1" ]; then
    step "Step 4  —  Download and restore snapshot"
    local SNAP_TMP="$DIR/snapshot-${CLIENT}.tar.zst"
    printf '   %ssource:%s %s\n' "$D" "$N" "$snap_url"
    command -v zstd >/dev/null 2>&1 || [ -x "$ZSTDDEC" ] || { err "need zstd to restore snapshot (apt install zstd / brew install zstd)"; exit 1; }
    curl -fL -C - --retry 30 --retry-delay 5 --retry-all-errors -A 'Mozilla/5.0' -s -o "$SNAP_TMP" "$snap_url" || { err "Snapshot download failed."; exit 1; }
    local want2; want2=$(curl -s -A 'Mozilla/5.0' --max-time 25 "${snap_url}.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
    if [ -n "$want2" ]; then
      local got2; got2=$(sha256_of "$SNAP_TMP"); [ "$got2" = "$want2" ] || { err "Snapshot sha256 mismatch."; rm -f "$SNAP_TMP"; exit 1; }
      ok "Snapshot integrity verified"
    fi
    mkdir -p "$DATADIR"
    ZDEC < "$SNAP_TMP" | tar -xf - -C "$DATADIR" || { err "Snapshot extraction failed."; exit 1; }
    rm -f "$SNAP_TMP"
    ok "Snapshot restored to $DATADIR"
  else
    mkdir -p "$DATADIR"
    ok "Datadir prepared (genesis sync — no snapshot available)"
  fi

  echo "$CLIENT" > "$CLIENT_MARKER"
  date +%s > "$DIR/.setup_done_at" 2>/dev/null
  step "Done"
  ok "Your $CLIENT node is ready (${NET_LABEL}, ${NODETYPE})."
  printf '\n   %sNext step:%s  %s./one.sh start%s     %sthen:%s ./one.sh status\n' "$B" "$N" "$G" "$N" "$D" "$N"
}

cmd_start_nongeth(){
  if is_running; then ok "Already running (pid $(cat "$PIDFILE")). See: ./one.sh status"; return 0; fi
  ensure_client_fragment
  if [ ! -d "$DATADIR" ] || [ -z "$(ls -A "$DATADIR" 2>/dev/null)" ]; then
    warn "No data found — running first-time setup…"
    cmd_setup_nongeth || { err "Setup failed; not starting."; return 1; }
  fi
  if declare -f client_requirements >/dev/null 2>&1; then
    client_requirements || { err "Pre-flight requirements check failed."; return 1; }
  fi
  local _args; _args=$(client_start_args "$NETWORK" "$NODETYPE")
  mkdir -p "$LOGDIR"; ulimit -n 65536 2>/dev/null || true
  step "Starting $CLIENT node ($NET_LABEL, $NODETYPE)…"
  # Fragments emit a shell-quoted argument line (e.g. --http.port "8585"); eval
  # so those quotes are honoured. Plain word-splitting would pass the quote
  # characters literally and strict flag parsers (reth/erigon/xone) reject them.
  # Fragments are integrity-checked (sha256) before sourcing.
  eval "nohup \"\$DIR/\$CLIENT\" $_args >> \"\$LOG\" 2>&1 &"
  echo $! > "$PIDFILE"; sleep 6
  if is_running; then
    ok "Started (pid $(cat "$PIDFILE"))."
    date +%s > "$DIR/.started_at" 2>/dev/null
    printf '\n   %sWatch progress:%s %s./one.sh status%s\n' "$B" "$N" "$G" "$N"
  else
    err "Did not start — check: ./one.sh logs"
  fi
}

# ==================== SHARED COMMANDS =========================================

cmd_setup(){
  banner
  echo "$CLIENT" > "$CLIENT_MARKER"
  if [ "$CLIENT" = "geth" ]; then cmd_setup_geth; else cmd_setup_nongeth; fi
}

cmd_start(){
  banner
  if [ "$CLIENT" = "geth" ]; then cmd_start_geth; else cmd_start_nongeth; fi
}

cmd_stop(){
  banner
  if ! is_running; then ok "Already stopped."; return 0; fi
  local p; p=$(cat "$PIDFILE"); step "Stopping (pid $p) — allowing a clean save…"
  kill "$p" 2>/dev/null
  local i=0; while [ "$i" -lt 40 ] && kill -0 "$p" 2>/dev/null; do sleep 1; i=$((i+1)); done
  if kill -0 "$p" 2>/dev/null; then kill -9 "$p" 2>/dev/null; warn "Force-stopped."; else ok "Stopped cleanly."; fi
  rm -f "$PIDFILE"
}

cmd_status(){
  banner
  if ! is_running; then
    err "Node is NOT running."
    printf '   %sFirst time?%s %s./one.sh setup%s   %sAlready set up?%s %s./one.sh start%s\n' "$B" "$N" "$G" "$N" "$B" "$N" "$G" "$N"; return 0
  fi
  ok "Node is running (pid $(cat "$PIDFILE"))."
  local head peers sy cur high tip
  head=$(h2d "$(rpc eth_blockNumber)"); peers=$(h2d "$(rpc net_peerCount)"); sy=$(rpc eth_syncing)
  step "Sync progress"
  if echo "$sy" | grep -q '"highestBlock"'; then
    cur=$(h2d "$(echo "$sy"|grep -oE '"currentBlock":"0x[0-9a-f]+"')"); high=$(h2d "$(echo "$sy"|grep -oE '"highestBlock":"0x[0-9a-f]+"')")
    draw_bar "${cur:-0}" "${high:-0}" "block ${cur:-0} of ${high:-0}"; [ "$TTY" = 1 ] && echo
    warn "Catching up to the network — please wait."
    local _sa; _sa=$(cat "$DIR/.started_at" 2>/dev/null); [ -n "$_sa" ] && printf '   %s⏱  catching up for %s%s\n' "$D" "$(fmt_dur $(( $(date +%s) - _sa )))" "$N"
  else
    tip=$(rpc admin_peers | grep -oE '"head":"0x[0-9a-f]+"' | grep -oE '0x[0-9a-f]+' | while read -r x; do printf '%d\n' "$x"; done | sort -n | tail -1)
    if [ -n "${head:-}" ] && [ -n "${tip:-}" ] && [ "$tip" -gt 0 ] && [ "$head" -lt "$tip" ]; then
      draw_bar "$head" "$tip" "block $head of ~$tip"; [ "$TTY" = 1 ] && echo
      warn "Almost there — finishing the last blocks."
      local _sa; _sa=$(cat "$DIR/.started_at" 2>/dev/null); [ -n "$_sa" ] && printf '   %s⏱  catching up for %s%s\n' "$D" "$(fmt_dur $(( $(date +%s) - _sa )))" "$N"
    else
      draw_bar 100 100 "at the chain tip"; [ "$TTY" = 1 ] && echo
      ok "SYNCED — your node is at the live chain tip."
      [ -f "$DIR/.tip_reached_at" ] || date +%s > "$DIR/.tip_reached_at" 2>/dev/null
      local _t0 _tr; _t0=$(cat "$DIR/.setup_done_at" 2>/dev/null || cat "$DIR/.started_at" 2>/dev/null); _tr=$(cat "$DIR/.tip_reached_at" 2>/dev/null)
      [ -n "$_t0" ] && [ -n "$_tr" ] && printf '   %s⏱  reached tip in %s (clean dir → tip)%s\n' "$D" "$(fmt_dur $(( _tr - _t0 )))" "$N"
    fi
  fi
  printf '\n   %sBlock height:%s %s        %sConnected peers:%s %s\n' "$B" "$N" "${head:-?}" "$B" "$N" "${peers:-0}"
  printf '   %sCommands:%s start | stop | restart | status | logs | attach | clients\n' "$D" "$N"
}

cmd_clients(){
  hr
  printf '%s   XDC Client Capability Matrix%s\n' "$B" "$N"
  hr
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "CLIENT" "DEF.MODE" "MODES" "NETWORKS" "SNAPSHOT" "STATUS"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "--------------" "------------" "----------------------------" "------------------" "----------" "----------"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "geth"        "fast"    "fast full snap archive"    "mainnet apothem devnet" "yes"  "Stable"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "erigon"      "minimal" "minimal full archive"      "mainnet apothem"       "none" "Beta"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "reth"        "full"    "full"                      "apothem net5050 [mainnet*]" "none" "Experimental"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "besu"        "full"    "full"                      "apothem mainnet"       "none" "Experimental"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "nethermind"  "fast"    "fast full archive"         "mainnet apothem"       "none" "Beta"
  printf '%-14s %-12s %-28s %-18s %-10s %s\n' "xone"        "snap"    "snap full archive validator" "apothem [mainnet*] devnet" "none" "Experimental"
  hr
  printf '  %s* reth/xone mainnet requires XDC_EXPERIMENTAL=1 or interactive confirm%s\n' "$Y" "$N"
  printf '  %sgeth is the only client with a published snapshot today — others sync from genesis%s\n' "$D" "$N"
  printf '\n  Examples:\n'
  printf '  %scurl -fsSL https://xdc.network/install.sh | bash%s                         # geth fast mainnet (unchanged)\n' "$G" "$N"
  printf '  %scurl -fsSL https://xdc.network/install.sh | bash -s -- --client erigon --mode minimal%s\n' "$G" "$N"
  printf '  %scurl -fsSL https://xdc.network/install.sh | bash -s -- --client nethermind%s\n' "$G" "$N"
  printf '  %sCLIENT=reth NETWORK=apothem ./one.sh start%s\n' "$G" "$N"
  printf '\n  Fleet mode (all 6 clients, non-colliding ports and dirs):\n'
  printf '  %scurl -fsSL https://xdc.network/install.sh | bash -s -- --client all%s\n' "$G" "$N"
  printf '  %s./one.sh --client all status%s                                       # fleet table\n' "$G" "$N"
  printf '  %sXDC_DRY_RUN=1 ./one.sh --client all setup%s                         # dry-run (no download)\n' "$G" "$N"
}

# ==================== FLEET MODE (--client all) ==============================
#
# cmd_all <action>  — setup|start|stop|status for the full 6-client fleet.
# Each client gets its canonical best sync mode and a non-colliding port block.
# DRY_RUN: XDC_DRY_RUN=1 prints intent without downloading or starting anything.
cmd_all(){
  local _action="${1:-setup}"

  # Fleet definition: client  best-mode  xdc_dir
  # Ports are already defined in the case statement above (per-client).
  local _fleet="geth erigon reth besu nethermind xone"

  _all_client_dir(){
    local _c="$1"
    if [ "$_c" = "geth" ]; then echo "$HOME/xdc-node"
    else echo "$HOME/xdc-node-${_c}"
    fi
  }

  _all_client_mode(){
    case "$1" in
      geth)        echo "fast" ;;
      nethermind)  echo "fast" ;;
      erigon)      echo "minimal" ;;
      besu)        echo "full" ;;
      reth)        echo "full" ;;
      xone)        echo "snap" ;;
      *)           echo "full" ;;
    esac
  }

  _all_client_rpc_port(){
    case "$1" in
      geth)        echo "8565" ;;
      erigon)      echo "8575" ;;
      reth)        echo "8585" ;;
      besu)        echo "8595" ;;
      nethermind)  echo "8605" ;;
      xone)        echo "8615" ;;
      *)           echo "8565" ;;
    esac
  }

  case "$_action" in
    # ---- setup / start / stop: iterate over each client ----
    setup|start|stop)
      hr
      printf '%s   XDC Fleet Mode  —  --client all  —  %s%s   %s(%s)%s\n' "$B" "$_action" "$N" "$D" "$NET_LABEL" "$N"
      hr
      local _c _mode _cdir _ec
      for _c in $_fleet; do
        _mode=$(_all_client_mode "$_c")
        _cdir=$(_all_client_dir "$_c")
        if [ "${XDC_DRY_RUN:-0}" = "1" ]; then
          printf '   %sDRY_RUN%s  CLIENT=%-12s MODE=%-8s DIR=%-35s PORT_RPC=%s\n' \
            "$Y" "$N" "$_c" "$_mode" "$_cdir" "$(_all_client_rpc_port "$_c")"
          continue
        fi
        printf '\n%s▶ Fleet: %s  (%s · %s · %s)%s\n' "$B" "$_c" "$_mode" "$NET_LABEL" "$_cdir" "$N"
        _ec=0
        bash "$0" --client "$_c" --network "$NETWORK" --mode "$_mode" "$_action" \
          XDC_DIR="$_cdir" 2>&1 || _ec=$?
        if [ "$_ec" != "0" ]; then
          warn "Fleet: $CLIENT $_action exited $EC — continuing with remaining clients."
        fi
      done
      hr
      if [ "${XDC_DRY_RUN:-0}" = "1" ]; then
        printf '   %sDry-run complete. Set XDC_DRY_RUN=0 (or unset) to run for real.%s\n' "$Y" "$N"
      else
        printf '   %sFleet %s complete. Use:%s ./one.sh --client all status\n' "$G" "$_action" "$N"
      fi
      ;;

    # ---- status: fleet table ----
    status)
      hr
      printf '%s   XDC Fleet Status  —  %s%s\n' "$B" "$NET_LABEL" "$N"
      hr
      printf '%-14s %-35s %-8s %-10s %-8s\n' "CLIENT" "DIR" "RUNNING" "BLOCK" "PEERS"
      printf '%-14s %-35s %-8s %-10s %-8s\n' "--------------" "-----------------------------------" "--------" "----------" "--------"
      local _c _cdir _pidfile _running _block _peers _rport
      for _c in $_fleet; do
        _cdir=$(_all_client_dir "$_c")
        _pidfile="$_cdir/node.pid"
        _rport=$(_all_client_rpc_port "$_c")
        _running="stopped"
        _block="--"
        _peers="--"
        if [ -f "$_pidfile" ] && kill -0 "$(cat "$_pidfile" 2>/dev/null)" 2>/dev/null; then
          _running="running"
          local _rsp
          _rsp=$(curl -s -m4 -X POST -H 'Content-Type:application/json' \
            --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
            "http://127.0.0.1:${_rport}" 2>/dev/null || true)
          _block=$(printf '%s' "$_rsp" | grep -oE '"result":"0x[0-9a-f]+"' | grep -oE '0x[0-9a-f]+' | head -1)
          [ -n "$_block" ] && _block=$(printf '%d' "$_block" 2>/dev/null || echo "--")
          local _prsp
          _prsp=$(curl -s -m4 -X POST -H 'Content-Type:application/json' \
            --data '{"jsonrpc":"2.0","method":"net_peerCount","params":[],"id":1}' \
            "http://127.0.0.1:${_rport}" 2>/dev/null || true)
          _peers=$(printf '%s' "$_prsp" | grep -oE '"result":"0x[0-9a-f]+"' | grep -oE '0x[0-9a-f]+' | head -1)
          [ -n "$_peers" ] && _peers=$(printf '%d' "$_peers" 2>/dev/null || echo "--")
        fi
        printf '%-14s %-35s %-8s %-10s %-8s\n' "$_c" "$_cdir" "$_running" "${_block:---}" "${_peers:---}"
      done
      hr
      ;;

    *)
      err "Fleet mode: unknown action '$_action'. Valid: setup start stop status"
      exit 1
      ;;
  esac
}

usage(){
  banner
  printf '   One script does everything — it even fetches the node binary for your OS/CPU.\n'
  printf '   Type a word after it (optionally preceded by --client/--mode/--network flags):\n\n'
  printf '   %ssetup%s     First time: download the matching binary + snapshot (geth)\n' "$G" "$N"
  printf '             so your node starts near the live chain in minutes.\n'
  printf '   %sstart%s     Start the node (auto-fetches binary if missing).\n' "$G" "$N"
  printf '   %sstatus%s    Pretty sync status: %% synced, block height, peers, timings.\n' "$G" "$N"
  printf '   %sstop%s      Stop the node safely.\n' "$G" "$N"
  printf '   %srestart%s   Stop, then start.\n' "$G" "$N"
  printf '   %slogs%s      Watch the live log.\n' "$G" "$N"
  printf '   %sattach%s    Open the geth console (advanced, geth only).\n' "$G" "$N"
  printf '   %sclients%s   Print the per-client capability matrix.\n' "$G" "$N"
  printf '\n   %sFlags:%s --client geth|erigon|reth|besu|nethermind|xone|all  --mode fast|full|minimal|snap|archive|validator\n' "$B" "$N"
  printf '          --network mainnet|apothem|devnet|net5050   --no-snapshot (sync from genesis, skip snapshot)\n'
  printf '\n   %sFleet mode:%s --client all setup|start|stop|status  (runs all 6 clients)\n' "$B" "$N"
  printf '          XDC_DRY_RUN=1 ./one.sh --client all setup  (print plan without downloading)\n'
  printf '\n   %sOne-liner:%s curl -fsSL %s/one.sh -o one.sh && bash one.sh setup\n' "$B" "$N" "$BASE_URL"
  printf '\n   %sTip:%s if anything looks stuck, re-run the same command — it is safe\n' "$D" "$N"
  printf '   to repeat (downloads resume where they left off).\n'
}

# Fleet mode: dispatch to cmd_all before the single-client case statement.
if [ "$_CLIENT_ALL" = "1" ]; then
  cmd_all "${1:-setup}"
  exit 0
fi

case "${1:-start}" in
  setup|init|download) cmd_setup ;;
  start|up)            cmd_start ;;
  stop|down)           cmd_stop ;;
  restart)             cmd_stop; cmd_start ;;
  status|info)         cmd_status ;;
  logs|log)            tail -f "$LOG" ;;
  attach|console)
    if [ "$CLIENT" != "geth" ]; then
      warn "The 'attach' command opens the geth JavaScript console and is not available for '$CLIENT'."
      warn "Check your client's documentation for an equivalent command."
      exit 0
    fi
    "$GETH" attach "$IPC" ;;
  clients|matrix)      cmd_clients ;;
  help|-h|--help|*)    usage ;;
esac
