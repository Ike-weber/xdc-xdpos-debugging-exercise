#!/usr/bin/env bash
# scripts/devnet-mint/03-launch-nodes.sh
# A.97-M.C devnet gate — Step 3: Init datadirs and launch 3 masternode nodes.
#
# Node layout:
#   node1 — OUR geth (feat/m-devnet-gate), --mine, masternode key 1
#   node2 — OUR geth (feat/m-devnet-gate), --mine, masternode key 2
#   node3 — XDPoSChain geth, --mine, masternode key 3 (cross-client gate)
#
# Port plan (verify free with ss -tlnp before running):
#   node1: RPC 9551, p2p 31301, authrpc 9651
#   node2: RPC 9552, p2p 31302, authrpc 9652
#   node3: RPC 9553, p2p 31303, authrpc 9653
#
# Usage:
#   OUT_DIR=/root/devnet-a97m \
#   OUR_GETH=/root/workspace/go-ethereum-a97m/build/bin/geth \
#   XDC_GETH=/root/workspace/XDPoSChain/build/bin/geth \
#   ./scripts/devnet-mint/03-launch-nodes.sh
#
# Prereqs: 01-gen-keys.sh and 02-gen-genesis.sh already run.

set -euo pipefail

OUT_DIR="${OUT_DIR:-/root/devnet-a97m}"
OUR_GETH="${OUR_GETH:-/root/workspace/go-ethereum-a97m/build/bin/geth}"
XDC_GETH="${XDC_GETH:-/root/workspace/XDPoSChain/build/bin/XDC}"
LOG_LEVEL="${LOG_LEVEL:-5}"  # verbosity 5 = debug — needed to see BFT log lines

source "$OUT_DIR/keys.env"

log() { echo "[03-launch-nodes] $*" >&2; }

# Verify binaries exist
for bin in "$OUR_GETH" "$XDC_GETH"; do
    if [[ ! -x "$bin" ]]; then
        log "ERROR: binary not found or not executable: $bin"
        exit 1
    fi
done

OUR_VER=$("$OUR_GETH" version 2>&1 | head -1 || echo "unknown")
XDC_VER=$("$XDC_GETH" version 2>&1 | head -1 || echo "unknown")
log "OUR_GETH=$OUR_GETH  ($OUR_VER)"
log "XDC_GETH=$XDC_GETH  ($XDC_VER)"

# Check ports are free
for PORT in 9551 9552 9553 31301 31302 31303 9651 9652 9653; do
    if ss -tlnp 2>/dev/null | grep -q ":$PORT "; then
        log "WARNING: port $PORT is already in use — check with: ss -tlnp | grep $PORT"
    fi
done

# ---- Init datadirs --------------------------------------------------------
log "Initializing datadirs with genesis..."
for i in 1 2 3; do
    DATADIR="$OUT_DIR/node$i"
    mkdir -p "$DATADIR/logs"

    # Our geth uses XDC/ subdirectory; XDPoSChain uses XDC/ as well
    if [[ -d "$DATADIR/XDC/chaindata" ]] || [[ -d "$DATADIR/geth/chaindata" ]]; then
        log "node$i chaindata already exists — skipping init (delete $DATADIR/XDC to re-init)"
    else
        if [[ $i -le 2 ]]; then
            log "Init node$i with OUR_GETH (path scheme)..."
            "$OUR_GETH" init \
                --datadir "$DATADIR" \
                --state.scheme path \
                "$OUT_DIR/genesis.json" \
                2>&1 | tee -a "$DATADIR/logs/init.log"
        else
            log "Init node$i with XDC_GETH..."
            "$XDC_GETH" init \
                --datadir "$DATADIR" \
                "$OUT_DIR/genesis.json" \
                2>&1 | tee -a "$DATADIR/logs/init.log"
        fi
    fi
done

# ---- Helper: get enode from IPC -------------------------------------------
get_enode() {
    local IPC="$1"
    local GETH="$2"
    local MAX=30
    for i in $(seq 1 $MAX); do
        if [[ -S "$IPC" ]]; then
            E=$("$GETH" attach "$IPC" --exec 'admin.nodeInfo.enode' 2>/dev/null | tr -d '"' | tr -d "'" || true)
            if [[ -n "$E" && "$E" != *"error"* ]]; then
                echo "$E"
                return 0
            fi
        fi
        sleep 2
    done
    echo ""
    return 1
}

# ---- Launch node1 (OUR geth, --mine, key 1) --------------------------------
log "Launching node1 (OUR geth, mine, ADDR1=$ADDR1)..."
nohup "$OUR_GETH" \
    --datadir "$OUT_DIR/node1" \
    --networkid 551 \
    --port 31301 \
    --mine \
    --miner.etherbase "$ADDR1" \
    --unlock "$ADDR1" \
    --password "$OUT_DIR/password.txt" \
    --http \
    --http.addr 127.0.0.1 \
    --http.port 9551 \
    --http.api eth,net,web3,admin,XDPoS,xdc,miner,debug \
    --authrpc.port 9651 \
    --authrpc.addr 127.0.0.1 \
    --gcmode full \
    --snapshot=false \
    --nodiscover \
    --maxpeers 10 \
    --syncmode full \
    --verbosity $LOG_LEVEL \
    2>&1 | tee "$OUT_DIR/node1/logs/geth.log" &
NODE1_PID=$!
echo $NODE1_PID > "$OUT_DIR/node1.pid"
log "node1 PID=$NODE1_PID"

# ---- Launch node2 (OUR geth, --mine, key 2) --------------------------------
log "Launching node2 (OUR geth, mine, ADDR2=$ADDR2)..."
nohup "$OUR_GETH" \
    --datadir "$OUT_DIR/node2" \
    --networkid 551 \
    --port 31302 \
    --mine \
    --miner.etherbase "$ADDR2" \
    --unlock "$ADDR2" \
    --password "$OUT_DIR/password.txt" \
    --http \
    --http.addr 127.0.0.1 \
    --http.port 9552 \
    --http.api eth,net,web3,admin,XDPoS,xdc,miner,debug \
    --authrpc.port 9652 \
    --authrpc.addr 127.0.0.1 \
    --gcmode full \
    --snapshot=false \
    --nodiscover \
    --maxpeers 10 \
    --syncmode full \
    --verbosity $LOG_LEVEL \
    2>&1 | tee "$OUT_DIR/node2/logs/geth.log" &
NODE2_PID=$!
echo $NODE2_PID > "$OUT_DIR/node2.pid"
log "node2 PID=$NODE2_PID"

# ---- Launch node3 (XDPoSChain XDC binary, --mine, key 3) -------------------
# XDPoSChain v2.7.0 uses --mine --miner-etherbase (note: dash not dot)
# No --state.scheme (XDPoSChain doesn't have PBSS)
# No --snapshot (different flag set)
log "Launching node3 (XDPoSChain XDC v2.7.0, mine, ADDR3=$ADDR3)..."
nohup "$XDC_GETH" \
    --datadir "$OUT_DIR/node3" \
    --networkid 551 \
    --port 31303 \
    --mine \
    --miner-etherbase "$ADDR3" \
    --unlock "$ADDR3" \
    --password "$OUT_DIR/password.txt" \
    --http \
    --http-addr 127.0.0.1 \
    --http-port 9553 \
    --http-api "eth,net,web3,admin,XDPoS,miner,debug" \
    --gcmode full \
    --nodiscover \
    --maxpeers 10 \
    --verbosity $LOG_LEVEL \
    2>&1 | tee "$OUT_DIR/node3/logs/geth.log" &
NODE3_PID=$!
echo $NODE3_PID > "$OUT_DIR/node3.pid"
log "node3 PID=$NODE3_PID"

# ---- Wait for IPC sockets -------------------------------------------------
log "Waiting for IPC sockets..."
for i in 1 2 3; do
    IPC="$OUT_DIR/node$i/XDC.ipc"
    for attempt in $(seq 1 60); do
        if [[ -S "$IPC" ]]; then
            log "node$i IPC ready"
            break
        fi
        if [[ $attempt -eq 60 ]]; then
            log "ERROR: node$i IPC not ready after 120s"
            exit 1
        fi
        sleep 2
    done
done

# ---- Get enodes and wire static peers -------------------------------------
log "Collecting enodes..."
GETH1="$OUR_GETH"
GETH2="$OUR_GETH"
GETH3="$XDC_GETH"

ENODE1=$(get_enode "$OUT_DIR/node1/XDC.ipc" "$GETH1")
ENODE2=$(get_enode "$OUT_DIR/node2/XDC.ipc" "$GETH2")
ENODE3=$(get_enode "$OUT_DIR/node3/XDC.ipc" "$GETH3")

log "ENODE1=$ENODE1"
log "ENODE2=$ENODE2"
log "ENODE3=$ENODE3"

# Replace 127.0.0.1 with actual bind addresses for cross-node peering
# (all on same host, loopback is fine for devnet)
ENODE1_LOC=$(echo "$ENODE1" | sed 's/@[^:]*:/@127.0.0.1:/')
ENODE2_LOC=$(echo "$ENODE2" | sed 's/@[^:]*:/@127.0.0.1:/')
ENODE3_LOC=$(echo "$ENODE3" | sed 's/@[^:]*:/@127.0.0.1:/')

# Write enodes to env
cat >> "$OUT_DIR/keys.env" <<EOF
export ENODE1="$ENODE1_LOC"
export ENODE2="$ENODE2_LOC"
export ENODE3="$ENODE3_LOC"
EOF

# Add peers: each node connects to the other two
log "Wiring static peers..."

"$GETH1" attach "$OUT_DIR/node1/XDC.ipc" --exec "admin.addPeer(\"$ENODE2_LOC\"); admin.addPeer(\"$ENODE3_LOC\")" 2>&1
"$GETH2" attach "$OUT_DIR/node2/XDC.ipc" --exec "admin.addPeer(\"$ENODE1_LOC\"); admin.addPeer(\"$ENODE3_LOC\")" 2>&1
"$GETH3" attach "$OUT_DIR/node3/XDC.ipc" --exec "admin.addPeer(\"$ENODE1_LOC\"); admin.addPeer(\"$ENODE2_LOC\")" 2>&1

log "Peer wiring complete."
log ""
log "Nodes are UP. Monitor:"
log "  tail -f $OUT_DIR/node1/logs/geth.log | grep -E 'yourturn|sealed|QC|minted|vote'"
log "  tail -f $OUT_DIR/node2/logs/geth.log | grep -E 'yourturn|sealed|QC|minted|vote'"
log "  tail -f $OUT_DIR/node3/logs/geth.log | grep -E 'yourturn|sealed|QC|minted'"
log ""
log "Check block heights:"
log "  curl -s http://127.0.0.1:9551 -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}'"
log ""
log "Relaunch commands (if nodes die):"
log "  OUR_GETH=$OUR_GETH XDC_GETH=$XDC_GETH OUT_DIR=$OUT_DIR $0"
log ""
log "Done — devnet running."
