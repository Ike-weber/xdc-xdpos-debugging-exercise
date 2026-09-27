#!/bin/bash
# status.sh - Check health of local network nodes and join clients.
# Works with ItWorksinMyLocal layout: run.sh (4 local oldxdc nodes + optional
# geth node 5, or a --all follower per modern client) and join.sh (sync
# clients in nodes/<client>-sync).
#
# Reports on the topology run.sh/setup.sh actually launched, whatever ports
# or client set was used -- see the "Topology manifest" section below.
#
# Usage: ./status.sh

set -uo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_DIR"

# shellcheck disable=SC1091
source ./lib.sh

rpc_call() {
  local port=$1
  local method=$2
  local params=${3:-[]}
  curl -s -m 2 -X POST "http://localhost:${port}" \
    -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"${method}\",\"params\":${params},\"id\":1}" 2>/dev/null
}

# Query one RPC endpoint's live stats. Sets RS_BLOCK/RS_PEERS/RS_SYNCING/
# RS_CHAINID/RS_UP ("?" / 0 when unavailable). ONE definition -- this exact
# four-call probe used to be pasted separately into the sealers loop, the
# node-5 block, the followers loop and the join-clients loop; four copies
# that only coincidentally agreed.
query_node() {
  local port="$1"
  RS_BLOCK="?" RS_PEERS="?" RS_SYNCING="?" RS_CHAINID="?" RS_UP=0
  local resp

  resp=$(rpc_call "$port" "eth_blockNumber")
  if [ -n "$resp" ] && echo "$resp" | grep -q '"result"'; then
    RS_BLOCK=$(echo "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(int(d.get("result","0x0"),16))' 2>/dev/null || echo "?")
    RS_UP=1
  fi

  resp=$(rpc_call "$port" "net_peerCount")
  if [ -n "$resp" ] && echo "$resp" | grep -q '"result"'; then
    RS_PEERS=$(echo "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(int(d.get("result","0x0"),16))' 2>/dev/null || echo "?")
  fi

  resp=$(rpc_call "$port" "eth_syncing")
  if [ -n "$resp" ] && echo "$resp" | grep -q '"result"'; then
    RS_SYNCING=$(echo "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get("result",False); print("true" if r else "false")' 2>/dev/null || echo "?")
  fi

  resp=$(rpc_call "$port" "eth_chainId")
  if [ -n "$resp" ] && echo "$resp" | grep -q '"result"'; then
    RS_CHAINID=$(echo "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(int(d.get("result","0x0"),16))' 2>/dev/null || echo "?")
    RS_UP=1
  fi
}

print_table_row() {
  printf "%-18s %6s %10s %6s %-10s %-12s %-10s\n" "$1" "$2" "$3" "$4" "$5" "$6" "$7"
}

print_summary() {
  local name=$1 port=$2 block=$3 peers=$4 syncing=$5 chainId=$6 status=$7
  local block_str="$block"
  [ "$block" = "?" ] && block_str="0"
  print_table_row "$name" "$port" "$block_str" "$peers" "$syncing" "$chainId" "$status"
}

# --- Topology manifest (ItWorksinMyLocal#181) -------------------------------
# run.sh writes nodes/.topology after resolving the ports/client-set it
# actually used (see write_topology_manifest in lib.sh). Sourcing it here is
# what makes a bare ./status.sh -- with NO environment variables -- report
# the right thing after a custom-port setup.sh/run.sh invocation: previously
# nobody told the operator they had to re-pass the same BASE_RPC_PORT/
# ALL_RPC_BASE/ALL_CLIENTS overrides to status.sh too, so it silently probed
# today's hardcoded defaults and reported a fully healthy custom-port network
# as entirely DOWN (network tip 0).
#
# Precedence: explicit environment variable (an operator inspecting a net
# they did NOT start) > the manifest (the net THIS status.sh call is next to)
# > today's hardcoded defaults (fresh checkout, no manifest yet -- #4 below).
TOPOLOGY_FILE="${TOPOLOGY_FILE:-./nodes/.topology}"

# Snapshot which manifest keys the operator's own environment already set,
# BEFORE sourcing the manifest -- `.` below is a plain assignment and would
# otherwise just clobber an explicit override with the file's value.
for _tk in $TOPOLOGY_MANIFEST_KEYS; do
  printf -v "_ENV_SET_${_tk}" '%s' "${!_tk:+x}"
  printf -v "_ENV_VAL_${_tk}" '%s' "${!_tk:-}"
done

if [ -f "$TOPOLOGY_FILE" ]; then
  # shellcheck disable=SC1090  # per-run local state written by run.sh; nothing to lint
  . "$TOPOLOGY_FILE"
  echo "(topology: $TOPOLOGY_FILE)"
fi

# Restore any key the operator's environment set explicitly -- it wins over
# whatever the manifest just sourced.
for _tk in $TOPOLOGY_MANIFEST_KEYS; do
  _tk_set="_ENV_SET_${_tk}"
  if [ -n "${!_tk_set}" ]; then
    _tk_val="_ENV_VAL_${_tk}"
    printf -v "$_tk" '%s' "${!_tk_val}"
  fi
done

# Fall back to run.sh's own defaults (lib.sh's DEFAULT_* constants) for
# anything still unset -- a fresh checkout with no manifest and no overrides.
BASE_P2P_PORT="${BASE_P2P_PORT:-$DEFAULT_BASE_P2P_PORT}"
BASE_RPC_PORT="${BASE_RPC_PORT:-$DEFAULT_BASE_RPC_PORT}"
BASE_WS_PORT="${BASE_WS_PORT:-$DEFAULT_BASE_WS_PORT}"
BOOTNODE_PORT="${BOOTNODE_PORT:-$DEFAULT_BOOTNODE_PORT}"
ALL_RPC_BASE="${ALL_RPC_BASE:-$((BASE_RPC_PORT + 200))}"
ALL_PORT_STRIDE="${ALL_PORT_STRIDE:-$DEFAULT_ALL_PORT_STRIDE}"
ALL_P2P_STRIDE="${ALL_P2P_STRIDE:-$DEFAULT_ALL_P2P_STRIDE}"
NUM_NODES="${NUM_NODES:-$DEFAULT_NUM_NODES}"
MIXED="${MIXED:-0}"
ALL="${ALL:-0}"
CHAINID="${CHAINID:-}"
ALL_SELECTED="${ALL_SELECTED:-}"
MACHINE_LABEL="${MACHINE_LABEL:-}"

echo "Reporting: BASE_RPC_PORT=$BASE_RPC_PORT ALL_RPC_BASE=$ALL_RPC_BASE ALL_PORT_STRIDE=$ALL_PORT_STRIDE NUM_NODES=$NUM_NODES${MACHINE_LABEL:+ machine=$MACHINE_LABEL}${CHAINID:+ chainId=$CHAINID}"

# --- Local nodes ---
echo ""
echo "=== Local Network Nodes (run.sh / setup.sh) ==="
print_table_row "NAME" "PORT" "BLOCK" "PEERS" "SYNCING" "CHAINID" "STATUS"

LOCAL_BLOCKS=()
LOCAL_CHAINID=""

i=1
while [ "$i" -le "$NUM_NODES" ]; do
  port=$((BASE_RPC_PORT + i))
  name="local-oldxdc-$i"
  query_node "$port"
  [ -n "$LOCAL_CHAINID" ] || LOCAL_CHAINID="$RS_CHAINID"
  status="DOWN"
  [ "$RS_UP" = 1 ] && status="RUNNING"
  print_summary "$name" "$port" "$RS_BLOCK" "$RS_PEERS" "$RS_SYNCING" "$RS_CHAINID" "$status"
  [ "$RS_BLOCK" != "?" ] && LOCAL_BLOCKS+=("$RS_BLOCK")
  i=$((i + 1))
done

# Node 5 (optional --mixed modern geth, or the first --all follower's
# neighbour slot) -- BASE_RPC_PORT+(NUM_NODES+1), same as run.sh assigns.
# Probed unconditionally: MIXED/ALL only record what was REQUESTED, and a
# live probe of what's actually bound is the more trustworthy signal.
port=$((BASE_RPC_PORT + NUM_NODES + 1))
name="local-node-$((NUM_NODES + 1))"
query_node "$port"
if [ "$RS_UP" = 1 ]; then
  print_summary "$name" "$port" "$RS_BLOCK" "$RS_PEERS" "$RS_SYNCING" "$RS_CHAINID" "RUNNING"
  [ "$RS_BLOCK" != "?" ] && LOCAL_BLOCKS+=("$RS_BLOCK")
else
  print_summary "$name" "$port" "?" "?" "?" "?" "DOWN"
fi

# --- run.sh --all followers -------------------------------------------------
# Port math (all_follower_rpc) lives in lib.sh -- the SAME function run.sh's
# own pre-flight check and launch loop use, so this can never drift from it
# the way two independently-computed copies did before.
#
# Client list: an explicit ALL_CLIENTS env wins (operator says "check these");
# else the manifest's ALL_SELECTED (what run.sh --all actually started this
# run -- may be fewer than the requested set if a binary was missing); else
# lib.sh's DEFAULT_ALL_CLIENTS (fresh checkout, no manifest).
STATUS_ALL_CLIENTS="${ALL_CLIENTS:-${ALL_SELECTED:-$DEFAULT_ALL_CLIENTS}}"

_any_all=0
_idx=5
for _c in $STATUS_ALL_CLIENTS; do
  _p=$(all_follower_rpc "$_idx")
  _r=$(rpc_call "$_p" "eth_blockNumber")
  echo "$_r" | grep -q result && _any_all=1
  _idx=$((_idx + 1))
done

if [ "$_any_all" = 1 ]; then
  echo ""
  echo "=== Modern Followers (run.sh --all) ==="
  print_table_row "NAME" "PORT" "BLOCK" "PEERS" "SYNCING" "CHAINID" "STATUS"
  _idx=5
  for _c in $STATUS_ALL_CLIENTS; do
    port=$(all_follower_rpc "$_idx")
    name="all-$_c"
    query_node "$port"
    if [ "$RS_UP" = 1 ]; then
      print_summary "$name" "$port" "$RS_BLOCK" "$RS_PEERS" "$RS_SYNCING" "$RS_CHAINID" "RUNNING"
      LOCAL_BLOCKS+=("$RS_BLOCK")
    else
      # Not started, or started and its RPC never bound. The latter is a real
      # failure shape (process syncs internally while invisible), so show the
      # row rather than hiding it.
      print_summary "$name" "$port" "?" "?" "?" "?" "DOWN"
    fi
    _idx=$((_idx + 1))
  done
fi

# --- Join clients ------------------------------------------------------------
# Client set + per-client RPC port come from lib.sh's JOIN_ALL_CLIENTS /
# join_client_rpc_port -- the SAME source `join.sh --client all` fans out
# from, so this table can never again miss a client join.sh actually starts
# (it used to hardcode a 7-entry list with no geth4, so an 8-client
# `join.sh --client all` run was reported as only 7).
echo ""
echo "=== Join Clients (join.sh) ==="
print_table_row "NAME" "PORT" "BLOCK" "PEERS" "SYNCING" "CHAINID" "STATUS"

JOIN_BLOCKS=()
JOIN_CLIENTS_LIST="${JOIN_CLIENTS:-$JOIN_ALL_CLIENTS}"

for client in $JOIN_CLIENTS_LIST; do
  port=$(join_client_rpc_port "$client")
  name="join-$client"

  query_node "$port"
  status="DOWN"
  if [ "$RS_UP" = 1 ]; then
    if [ "$RS_CHAINID" != "$LOCAL_CHAINID" ] && [ "$LOCAL_CHAINID" != "?" ] && [ -n "$LOCAL_CHAINID" ] && [ "$RS_CHAINID" != "?" ]; then
      status="WRONG_NET"
    elif [ "$RS_BLOCK" != "?" ] && [ "$RS_BLOCK" -gt 0 ]; then
      status="RUNNING"
    elif [ "$RS_SYNCING" = "true" ]; then
      status="SYNCING"
    else
      status="STUCK"
    fi
  fi
  print_summary "$name" "$port" "$RS_BLOCK" "$RS_PEERS" "$RS_SYNCING" "$RS_CHAINID" "$status"
  [ "$RS_BLOCK" != "?" ] && [ "$status" != "WRONG_NET" ] && JOIN_BLOCKS+=("$RS_BLOCK")
done

# --- Summary ---
echo ""
max_local=0
for b in "${LOCAL_BLOCKS[@]}"; do
  [ "$b" -gt "$max_local" ] 2>/dev/null && max_local=$b
done

max_join=0
for b in "${JOIN_BLOCKS[@]}"; do
  [ "$b" -gt "$max_join" ] 2>/dev/null && max_join=$b
done

max_tip=$(( max_local > max_join ? max_local : max_join ))
echo "=== Summary ==="
echo "  Local chainId:    $LOCAL_CHAINID"
echo "  Network tip:      $max_tip"
echo ""
echo "Legend:"
echo "  RUNNING   = At/near tip, not actively syncing"
echo "  SYNCING   = Catching up to network tip"
echo "  STUCK     = RPC up but block 0 or no peers"
echo "  DOWN      = RPC not responding"
echo "  WRONG_NET = Connected to wrong chainId"
