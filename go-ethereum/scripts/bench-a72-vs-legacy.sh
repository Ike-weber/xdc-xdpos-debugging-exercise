#!/usr/bin/env bash
# bench-a72-vs-legacy.sh — read-only side-by-side benchmark probe.
#
# Run on the host that has both nodes running. SSH-friendly: no interactive
# prompts, no writes to either datadir, no restarts. Outputs to stdout.
#
# Refs: docs/port/BENCHMARK_A72_VS_LEGACY.md  (#844 #859)
#
# Usage:
#   ssh -p 12141 root@95.217.106.210 'bash -s' < scripts/bench-a72-vs-legacy.sh
#
# Exit codes:
#   0  benchmark completed (output usable)
#   2  host state drift — invariants failed (output partial, do not trust)

set -u
set -o pipefail

# ---------- configuration ----------
MODERN_RPC="http://127.0.0.1:8590"
LEGACY_RPC="http://127.0.0.1:8550"
LEGACY_REFERENCE_RPC="http://127.0.0.1:8548"   # fully-synced reference (optional)

MODERN_DATADIR="/root/workspace/go-ethereum/nodes-local-mainnet/xdcscan-mainnet-fast-v1.17.3-a50580a81"
LEGACY_DATADIR="/root/workspace/XDPoSChain/nodes-local/fresh-control"
LEGACY_REFERENCE_DATADIR="/root/workspace/XDPoSChain/nodes-local/fast"

MODERN_LOG_DIR="${MODERN_DATADIR}/logs"
LEGACY_LOG_DIR="${LEGACY_DATADIR}/logs"

PIVOT_NUMBER="103309269"
PIVOT_HASH="0x32a2aeb22957a7526498a2c2b447762ea2708c42fa43b03ac83d2e3be3d9eea9"
PIVOT_ROOT="0xa9e706f9ca9ec8b58a47740ee43cf860954b7051b09a5653d60cf3de37d9111e"

SAMPLES=10
SAMPLE_INTERVAL_S=6

# ---------- helpers ----------
hr() { printf '%s\n' "------------------------------------------------------------"; }
heading() { hr; printf '%s\n' "$*"; hr; }

rpc() {
    local url="$1" method="$2" params="${3:-[]}"
    curl -s -X POST -H "Content-Type: application/json" --max-time 5 "$url" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$method\",\"params\":$params}" 2>/dev/null
}

hex2dec() {
    local h="$1"
    [ -z "$h" ] && { echo 0; return; }
    h="${h#0x}"
    printf '%d\n' "0x${h}" 2>/dev/null || echo 0
}

probe_node() {
    local label="$1" url="$2"
    local sync block peers ver
    sync=$(rpc "$url" eth_syncing)
    block=$(rpc "$url" eth_blockNumber | grep -oE '0x[0-9a-fA-F]+' | head -1)
    peers=$(rpc "$url" net_peerCount   | grep -oE '0x[0-9a-fA-F]+' | head -1)
    ver=$(rpc "$url" net_version       | grep -oE '"[0-9]+"' | tr -d '"' | head -1)
    echo "[$label] networkid=$ver block=$block ($(hex2dec ${block:-0x0})) peers=$peers ($(hex2dec ${peers:-0x0}))"
    echo "[$label] eth_syncing=$sync"
}

find_pid_by_listen() {
    local port="$1"
    ss -tlnp 2>/dev/null | awk -v p=":$port" '$0 ~ p {match($0,/pid=([0-9]+)/,a); print a[1]; exit}'
}

# ---------- main ----------

heading "1. Host context"
date -u +'%Y-%m-%dT%H:%M:%SZ'
uname -a
df -h / | head -2

heading "2. Process identity & invariants"

MODERN_PID=$(find_pid_by_listen 8590)
LEGACY_PID=$(find_pid_by_listen 8550)
REFERENCE_PID=$(find_pid_by_listen 8548)

invariant_ok=1

if [ -z "${MODERN_PID:-}" ]; then
    echo "INVARIANT FAIL: modern A.72 RPC on :8590 not listening"
    invariant_ok=0
else
    echo "modern A.72 PID = $MODERN_PID"
fi

if [ -z "${LEGACY_PID:-}" ]; then
    echo "INVARIANT FAIL: legacy fresh-control RPC on :8550 not listening"
    invariant_ok=0
else
    echo "legacy fresh-ctrl PID = $LEGACY_PID"
fi

if [ -n "${REFERENCE_PID:-}" ]; then
    echo "(optional) legacy reference PID = $REFERENCE_PID"
fi

# Pivot-triplet check on both fresh-start nodes' command lines
for pid in $MODERN_PID $LEGACY_PID; do
    [ -z "$pid" ] && continue
    cmdline=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)
    if ! echo "$cmdline" | grep -q "$PIVOT_NUMBER" \
        || ! echo "$cmdline" | grep -q "${PIVOT_HASH#0x}" \
        || ! echo "$cmdline" | grep -q "${PIVOT_ROOT#0x}"; then
        echo "INVARIANT FAIL: PID $pid pivot triplet does not match documented benchmark setup"
        invariant_ok=0
    fi
done

heading "3. ps stats (etime/rss/pcpu)"
for pid in $MODERN_PID $LEGACY_PID $REFERENCE_PID; do
    [ -z "$pid" ] && continue
    ps -p "$pid" -o pid,etime,rss,pcpu,vsz,comm --no-headers
done

heading "4. Datadir sizes"
for d in "$MODERN_DATADIR/XDC" "$LEGACY_DATADIR/XDC" "$LEGACY_REFERENCE_DATADIR/XDC"; do
    [ -d "$d" ] && du -sh "$d" 2>/dev/null
done

heading "5. RPC snapshot — modern A.72"
probe_node "modern" "$MODERN_RPC"
heading "5. RPC snapshot — legacy fresh-control v2.7.0"
probe_node "legacy" "$LEGACY_RPC"
if [ -n "${REFERENCE_PID:-}" ]; then
    heading "5. RPC snapshot — legacy reference (4-day fully-synced)"
    probe_node "legacy-ref" "$LEGACY_REFERENCE_RPC"
fi

heading "6. Peer-count + block-height time-series (${SAMPLES} samples × ${SAMPLE_INTERVAL_S} s)"
for i in $(seq 1 $SAMPLES); do
    ts=$(date -u +%H:%M:%S)
    m_peers=$(rpc "$MODERN_RPC" net_peerCount   | grep -oE '0x[0-9a-fA-F]+' | head -1)
    m_block=$(rpc "$MODERN_RPC" eth_blockNumber | grep -oE '0x[0-9a-fA-F]+' | head -1)
    l_peers=$(rpc "$LEGACY_RPC" net_peerCount   | grep -oE '0x[0-9a-fA-F]+' | head -1)
    l_sync=$(rpc "$LEGACY_RPC" eth_syncing | grep -oE 'currentBlock":"0x[0-9a-fA-F]+' | head -1)
    printf '%s  modern peers=%-4s block=%-12s  legacy peers=%-4s %s\n' \
        "$ts" "${m_peers:-?}" "${m_block:-?}" "${l_peers:-?}" "${l_sync:-?}"
    [ "$i" -lt "$SAMPLES" ] && sleep "$SAMPLE_INTERVAL_S"
done

heading "7. State-sync window markers — modern A.72"
M_LOG=$(ls -t "$MODERN_LOG_DIR"/a72*.log 2>/dev/null | head -1)
if [ -n "$M_LOG" ]; then
    echo "log: $M_LOG"
    grep -E "Fast-sync: restored checkpoint|Fast Sync: starting XDC gap-pivot|Block synchronisation started|peer threshold met|engaging fast-sync" "$M_LOG" 2>/dev/null | head -10
    echo "--- first state-sync entry ---"
    grep "Imported new state entries" "$M_LOG" 2>/dev/null | head -1
    echo "--- pending=0 (state-sync done) ---"
    grep -E "Imported new state entries.* pending=0 " "$M_LOG" 2>/dev/null | tail -1
    echo "--- last 5 block imports ---"
    grep -E "Imported new chain segment" "$M_LOG" 2>/dev/null | tail -5
fi

heading "7. State-sync window markers — legacy fresh-control"
L_LOG=$(ls -t "$LEGACY_LOG_DIR"/relaunch-*.log 2>/dev/null | head -1)
if [ -n "$L_LOG" ]; then
    echo "log: $L_LOG"
    grep -E "Block synchronisation started|syncState |Using configured pivot block|Configured gap pivot" "$L_LOG" 2>/dev/null | head -10
    echo "--- first state-sync entry ---"
    grep "Imported new state entries" "$L_LOG" 2>/dev/null | head -1
    echo "--- pending=0 (state-sync done) ---"
    grep -E "Imported new state entries.* pending=0 " "$L_LOG" 2>/dev/null | tail -1
    echo "--- last 5 block-receipt imports ---"
    grep -E "Imported new block receipts" "$L_LOG" 2>/dev/null | tail -5
fi

heading "8. Drop-ratio summary (modern A.72)"
if [ -n "$M_LOG" ]; then
    total_batches=$(grep -c "Imported new state entries" "$M_LOG" 2>/dev/null)
    dup=$(grep -oE 'duplicate=[0-9]+' "$M_LOG" 2>/dev/null | awk -F= '{s+=$2} END {print s+0}')
    unex=$(grep -oE 'unexpected=[0-9]+' "$M_LOG" 2>/dev/null | awk -F= '{s+=$2} END {print s+0}')
    echo "batches=$total_batches  duplicate-sum=$dup  unexpected-sum=$unex"
fi

heading "8. Drop-ratio summary (legacy fresh-control)"
if [ -n "$L_LOG" ]; then
    total_batches=$(grep -c "Imported new state entries" "$L_LOG" 2>/dev/null)
    dup=$(grep -oE 'duplicate=[0-9]+' "$L_LOG" 2>/dev/null | awk -F= '{s+=$2} END {print s+0}')
    unex=$(grep -oE 'unexpected=[0-9]+' "$L_LOG" 2>/dev/null | awk -F= '{s+=$2} END {print s+0}')
    echo "batches=$total_batches  duplicate-sum=$dup  unexpected-sum=$unex"
fi

heading "9. Wire-protocol identification"
if [ -n "$M_LOG" ]; then
    echo "[modern] startup version:"
    grep -E "Starting peer-to-peer node|Initialising Ethereum protocol" "$M_LOG" 2>/dev/null | head -2
fi
if [ -n "$L_LOG" ]; then
    echo "[legacy] startup version:"
    grep -E "Starting peer-to-peer node|Initialising Ethereum protocol" "$L_LOG" 2>/dev/null | head -2
fi

hr
if [ "$invariant_ok" = "0" ]; then
    echo "BENCHMARK INVALID — host state has drifted (see INVARIANT FAIL above)"
    exit 2
fi
echo "BENCHMARK OK — outputs above match documented setup"
exit 0
