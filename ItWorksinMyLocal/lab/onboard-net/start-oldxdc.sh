#!/bin/bash
# start-oldxdc.sh — launch a persistent oldxdc (XDPoSChain) validator net that
# other clients can later JOIN as masternodes via onboard.py (uploadKYC+propose).
#
# Starts a bootnode + N validators (--mine) from a genesis. Idempotent: re-run
# to relaunch after a graceful stop (node data persists in $BASE/nodes).
# NEVER SIGKILL a running validator (corrupts the XDPoS state DB → deadlock);
# use stop.sh for a graceful SIGTERM.
#
# Config via env:
#   BASE      working dir                    (default ./onboard-run)
#   XDC       path to the oldxdc XDC binary  [REQUIRED]
#   BOOTNODE  path to the bootnode binary    [REQUIRED]
#   GENESIS   genesis.json                   (default $BASE/genesis.json)
#   IP        advertised extip               (default 127.0.0.1)
#   NETID     networkid                      (default 34093)
#   BPORT     bootnode p2p port              (default 37000)
#   N         number of validators           (default 5)
#   STATS     ethstats "secret@host:port"    (optional; nodes report as $STATS_PREFIX-nodeN)
#   STATS_PREFIX  ethstats name prefix       (default xdclabs-net)
#   KEYS      space-separated validator privkeys (hex, no 0x); must be seated in genesis extraData + 0x88 SMC
#
# Ports: node n uses p2p=BPORT+n, rpc=BPORT+10+n, ws=BPORT+20+n (127.0.0.1 only).
set -u
BASE=${BASE:-./onboard-run}
XDC=${XDC:?set XDC to the oldxdc XDC binary}
BOOTNODE=${BOOTNODE:?set BOOTNODE to the bootnode binary}
GENESIS=${GENESIS:-$BASE/genesis.json}
IP=${IP:-127.0.0.1}
NETID=${NETID:-34093}
BPORT=${BPORT:-37000}
N=${N:-5}
STATS=${STATS:-}
STATS_PREFIX=${STATS_PREFIX:-xdclabs-net}
read -ra KEYS <<< "${KEYS:?set KEYS to space-separated validator privkeys}"

mkdir -p "$BASE/nodes"; echo password > "$BASE/.pwd"

# bootnode (persistent key + fixed port)
[ -f "$BASE/bootnode.key" ] || "$BOOTNODE" -genkey "$BASE/bootnode.key"
BKEY=$("$BOOTNODE" -nodekey "$BASE/bootnode.key" -writeaddress)
ENODE="enode://${BKEY}@${IP}:${BPORT}"; echo "$ENODE" > "$BASE/bootnode.enode"
pgrep -f "bootnode -nodekey $BASE/bootnode.key" >/dev/null || \
  nohup "$BOOTNODE" -nodekey "$BASE/bootnode.key" -addr "${IP}:${BPORT}" -verbosity 4 \
    > "$BASE/bootnode.log" 2>&1 &
echo "bootnode: $ENODE"

for n in $(seq 1 "$N"); do
  key=${KEYS[$((n-1))]}; d=$BASE/nodes/node$n
  p2p=$((BPORT+n)); rpc=$((BPORT+10+n)); ws=$((BPORT+20+n))
  mkdir -p "$d"
  if [ -z "$(ls -A "$d/keystore" 2>/dev/null)" ]; then
    printf '%s' "$key" > "$d/pk.hex"
    "$XDC" account import --datadir "$d" --password "$BASE/.pwd" "$d/pk.hex" >/dev/null 2>&1
  fi
  addr=0x$(ls "$d/keystore"/UTC--* 2>/dev/null | head -1 | grep -oE '[0-9a-fA-F]{40}$')
  "$XDC" --datadir "$d" init "$GENESIS" >/dev/null 2>&1
  if pgrep -f "XDC --datadir $d " >/dev/null; then echo "node$n already running"; continue; fi
  es=""; [ -n "$STATS" ] && es="--ethstats ${STATS_PREFIX}-node${n}:${STATS}"
  nohup "$XDC" --datadir "$d" --networkid "$NETID" --port "$p2p" --bootnodes "$ENODE" \
    --syncmode full --nat "extip:${IP}" \
    --rpc --rpcaddr 127.0.0.1 --rpcport "$rpc" --rpcapi eth,net,web3,debug,XDPoS \
    --ws --wsaddr 127.0.0.1 --wsport "$ws" \
    --mine --unlock "$addr" --etherbase "$addr" --password "$BASE/.pwd" \
    --gasprice 1 $es > "$d/node.log" 2>&1 &
  echo "node$n ($addr) mining  rpc=127.0.0.1:$rpc  p2p=$p2p  pid=$!"
done
echo "ALL STARTED. enode for clients to join: $ENODE  (genesis $GENESIS)"
