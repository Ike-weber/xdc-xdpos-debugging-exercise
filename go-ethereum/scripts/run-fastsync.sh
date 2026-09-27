#!/usr/bin/env bash
# XDC fast-sync runner — mainnet OR Apothem testnet
#
# Supports A.89 dynamic-pivot mode (default — no operator flags required) and
# legacy A.6 operator-pinned pivot mode (set FASTSYNC_PIVOT_* env vars).
#
# USAGE
#   ./scripts/run-fastsync.sh mainnet              # dynamic pivot, foreground
#   ./scripts/run-fastsync.sh apothem              # dynamic pivot, foreground
#   ./scripts/run-fastsync.sh mainnet --wipe       # wipe chaindata and restart
#   ./scripts/run-fastsync.sh apothem --background # run in background (nohup)
#   ./scripts/run-fastsync.sh mainnet --port 30310 --rpc-port 8550
#
# ENV OVERRIDES (optional)
#   DATADIR=/path                     override datadir (default: $HOME/.xdc-fastsync-<network>)
#   PORT=30310                        P2P port
#   RPC_PORT=8550                     HTTP RPC port
#   AUTHRPC_PORT=8551                 authrpc port
#   CACHE=2048                        cache MB
#   MAXPEERS=50                       p2p peer cap
#   VERBOSITY=3                       log verbosity (0-5)
#   GETH_BIN=/path/to/geth            use existing binary; skip build
#   ETHSTATS=label:secret@host:port   optional ethstats reporting
#
# OPERATOR PIVOT (optional — overrides A.89 dynamic selection)
#   FASTSYNC_PIVOT_NUMBER=...         all three must be set together
#   FASTSYNC_PIVOT_HASH=0x...
#   FASTSYNC_PIVOT_ROOT=0x...
#
# REQUIREMENTS
#   - go 1.22+ on PATH (for build)
#   - 50+ GB free disk for mainnet, ~10 GB for Apothem
#   - Outbound TCP/UDP on $PORT
#
# REFERENCES
#   Issue #857 — full A.x patch series
#   PR  #858   — A.11-A.66 stack on reset/integration-1.17.3
#   Issue #877 — A.89 dynamic pivot trusted anchor
#   PR  #883   — A.91 prime walk-back retry on peer drop

set -euo pipefail

# ---------- argument parsing ----------

NETWORK="${1:-}"
if [[ -z "$NETWORK" || "$NETWORK" == "-h" || "$NETWORK" == "--help" ]]; then
    sed -n '2,44p' "$0" | sed 's/^# \?//'
    exit 0
fi

if [[ "$NETWORK" != "mainnet" && "$NETWORK" != "apothem" ]]; then
    echo "ERROR: first arg must be 'mainnet' or 'apothem' (got: $NETWORK)" >&2
    exit 1
fi
shift

WIPE_CHAINDATA=false
BACKGROUND=false
EXTRA_FLAGS=()
while (($#)); do
    case "$1" in
        --wipe)           WIPE_CHAINDATA=true ;;
        --background)     BACKGROUND=true ;;
        --port)           PORT="$2"; shift ;;
        --rpc-port)       RPC_PORT="$2"; shift ;;
        --authrpc-port)   AUTHRPC_PORT="$2"; shift ;;
        --datadir)        DATADIR="$2"; shift ;;
        --cache)          CACHE="$2"; shift ;;
        --maxpeers)       MAXPEERS="$2"; shift ;;
        --verbosity)      VERBOSITY="$2"; shift ;;
        --ethstats)       ETHSTATS="$2"; shift ;;
        *)                EXTRA_FLAGS+=("$1") ;;
    esac
    shift
done

# ---------- network defaults ----------

DATADIR="${DATADIR:-$HOME/.xdc-fastsync-$NETWORK}"
CACHE="${CACHE:-2048}"
MAXPEERS="${MAXPEERS:-50}"
VERBOSITY="${VERBOSITY:-3}"

if [[ "$NETWORK" == "mainnet" ]]; then
    NETWORK_FLAG="--xdcmainnet"
    PORT="${PORT:-30310}"
    RPC_PORT="${RPC_PORT:-8550}"
    AUTHRPC_PORT="${AUTHRPC_PORT:-8551}"
else
    NETWORK_FLAG="--apothem"
    PORT="${PORT:-30311}"
    RPC_PORT="${RPC_PORT:-8552}"
    AUTHRPC_PORT="${AUTHRPC_PORT:-8553}"
fi

# ---------- operator pivot (optional — A.89 dynamic mode is the default) ----------
# If all three FASTSYNC_PIVOT_* vars are set, use operator-pinned pivot (A.6).
# If none are set, the node auto-selects pivot via A.89 dynamic logic.
# Partial set is an error.

PIVOT_FLAGS=()
_pn="${FASTSYNC_PIVOT_NUMBER:-}"
_ph="${FASTSYNC_PIVOT_HASH:-}"
_pr="${FASTSYNC_PIVOT_ROOT:-}"

if [[ -n "$_pn" || -n "$_ph" || -n "$_pr" ]]; then
    if [[ -z "$_pn" || -z "$_ph" || -z "$_pr" ]]; then
        echo "ERROR: FASTSYNC_PIVOT_NUMBER, FASTSYNC_PIVOT_HASH, and FASTSYNC_PIVOT_ROOT must all be set together." >&2
        exit 1
    fi
    PIVOT_FLAGS=(
        --fastsyncpivotnumber "$_pn"
        --fastsyncpivothash   "$_ph"
        --fastsyncpivotroot   "$_pr"
    )
    PIVOT_MODE="operator-pinned (A.6)  number=$_pn"
else
    PIVOT_MODE="dynamic (A.89) — auto-selected at runtime"
fi

# ---------- locate project root ----------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# ---------- build geth if needed ----------

GETH_BIN="${GETH_BIN:-$PROJECT_ROOT/build/bin/geth}"
if [[ ! -x "$GETH_BIN" ]]; then
    echo ">>> geth binary not found at $GETH_BIN — building from source..."
    cd "$PROJECT_ROOT"
    go build -o build/bin/geth ./cmd/geth
    GETH_BIN="$PROJECT_ROOT/build/bin/geth"
fi

echo ">>> geth: $GETH_BIN"
"$GETH_BIN" version | head -4

# ---------- wipe if requested ----------

if [[ "$WIPE_CHAINDATA" == "true" && -d "$DATADIR" ]]; then
    echo ">>> wiping $DATADIR (--wipe)"
    rm -rf "$DATADIR"/{geth,XDC,XDC.ipc,geth.ipc} 2>/dev/null || true
fi

mkdir -p "$DATADIR/logs"
LOG_FILE="$DATADIR/logs/fastsync-$(date +%s).log"

# ---------- build optional flags ----------

ETHSTATS_FLAGS=()
if [[ -n "${ETHSTATS:-}" ]]; then
    ETHSTATS_FLAGS=(--ethstats "$ETHSTATS")
fi

# ---------- summary ----------

echo ""
echo "============================================================"
echo "  XDC fast-sync: $NETWORK"
echo "============================================================"
echo "  binary        $GETH_BIN"
echo "  datadir       $DATADIR"
echo "  p2p port      $PORT"
echo "  rpc port      $RPC_PORT  (http://127.0.0.1:$RPC_PORT)"
echo "  authrpc port  $AUTHRPC_PORT"
echo "  cache         ${CACHE}MB"
echo "  maxpeers      $MAXPEERS"
echo "  pivot mode    $PIVOT_MODE"
if [[ -n "${ETHSTATS:-}" ]]; then
echo "  ethstats      $ETHSTATS"
fi
echo "  log file      $LOG_FILE"
echo "============================================================"
echo ""
echo "Watch progress:"
echo "  tail -f $LOG_FILE | grep -E 'pivot|primed anchor|Imported|syncing'"
echo ""
echo "Check sync status:"
echo "  curl -s http://127.0.0.1:$RPC_PORT -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_syncing\",\"params\":[],\"id\":1}'"
echo ""

# ---------- run ----------

GETH_ARGS=(
    --datadir "$DATADIR"
    $NETWORK_FLAG
    --port "$PORT"
    --syncmode fast
    --gcmode full
    --state.scheme hash
    "${PIVOT_FLAGS[@]+"${PIVOT_FLAGS[@]}"}"
    --http --http.addr 127.0.0.1 --http.port "$RPC_PORT"
    --http.api eth,net,web3,debug,admin,XDPoS
    --authrpc.port "$AUTHRPC_PORT"
    --cache "$CACHE"
    --cache.database 50 --cache.gc 15 --cache.trie 25
    --maxpeers "$MAXPEERS"
    --verbosity "$VERBOSITY"
    "${ETHSTATS_FLAGS[@]+"${ETHSTATS_FLAGS[@]}"}"
    "${EXTRA_FLAGS[@]+"${EXTRA_FLAGS[@]}"}"
)

if [[ "$BACKGROUND" == "true" ]]; then
    echo ">>> launching in background (nohup), log: $LOG_FILE"
    nohup "$GETH_BIN" "${GETH_ARGS[@]}" >> "$LOG_FILE" 2>&1 &
    echo "PID: $!"
    echo "Stop: kill $!"
    echo "Logs: tail -f $LOG_FILE"
else
    set -x
    exec "$GETH_BIN" "${GETH_ARGS[@]}" 2>&1 | tee "$LOG_FILE"
fi
