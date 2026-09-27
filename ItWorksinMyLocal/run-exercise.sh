#!/bin/bash
# run-exercise.sh -- the debugging-exercise topology.
#
#   2 legacy XDPoSChain (oldxdc) sealers  + 2 go-ethereum validators
#   = a 4-member validator set, all four seated from genesis.
#
# WHY THIS TOPOLOGY
#   * All four validators are in the genesis signer set, so NOTHING has to be
#     staked at runtime. Staking works (see lab/propose-masternode.sh) but it
#     needs python3 + eth-account and a manual key import, and none of that is
#     what this exercise is testing.
#   * Quorum is ceil(0.667 * 4) = 3 of 4. The two geth nodes are therefore
#     load-bearing: if they do not vote, 2 votes remain and no certificate can
#     form. Once they do vote the network has 4 and tolerates one node down.
#   * geth cannot seal v1 blocks (they are rejected and fork the chain), so the
#     two legacy nodes carry blocks 1..899 and the geth nodes follow. From the
#     v1->v2 switch at block 900 all four participate.
#
# Usage:
#   ./setup.sh --new 4              # generate 4 keys + genesis, init legacy nodes
#   ./configure-exercise-net.sh     # stamp the exercise consensus parameters
#   GETH_BIN=/path/to/your/geth ./run-exercise.sh
#
# GETH_BIN is REQUIRED and must point at the binary YOU built from the
# go-ethereum source tree you were given. This script refuses to fall back to a
# downloaded release -- running a stock geth here silently defeats the exercise.
set -uo pipefail
cd "$(dirname "$0")" || exit 1
source ./lib.sh

LEGACY=2
NETWORKID="${CHAINID:-20118}"
LOG="./nodes/exercise-logs"; mkdir -p "$LOG"
touch .pwd

# ---- 1. the candidate's geth, and only the candidate's geth ----------------
if [ -z "${GETH_BIN:-}" ]; then
  cat >&2 <<'MSG'
GETH_BIN is not set.

Build the go-ethereum source tree you were given, then point this script at it:

    cd ../go-ethereum && make geth
    GETH_BIN=$(pwd)/build/bin/geth ../ItWorksinMyLocal/run-exercise.sh

This script will not download a release build of geth. The exercise is about
the source you were given; a stock binary would not contain it.
MSG
  exit 1
fi
[ -x "$GETH_BIN" ] || { echo "GETH_BIN=$GETH_BIN is not an executable file" >&2; exit 1; }

# Refuse the harness's own download cache -- that is a stock upstream release.
case "$(cd "$(dirname "$GETH_BIN")" && pwd)" in
  */bin/geth/*)
    echo "GETH_BIN points into $PWD/bin/geth/, which is the harness's DOWNLOAD cache." >&2
    echo "That is a stock upstream release, not the source you were given. Build it yourself." >&2
    exit 1;;
esac
echo "geth binary : $GETH_BIN"
echo "             built $(date -r "$GETH_BIN" '+%Y-%m-%d %H:%M' 2>/dev/null || echo '?')"
"$GETH_BIN" version 2>/dev/null | sed -n '1,3p' | sed 's/^/             /'

# ---- 2. legacy binaries + wallets -----------------------------------------
ensure_bins
XDC="$XDC_BIN"; BOOTNODE="$BOOTNODE_BIN"
IP=$(detect_ip); [ -z "$IP" ] && IP=127.0.0.1

declare -a W
for i in 1 2 3 4; do
  f=$(ls ./nodes/$i/keystore/UTC--* 2>/dev/null | head -1)
  [ -z "$f" ] && { echo "no key in ./nodes/$i -- run: ./setup.sh --new 4" >&2; exit 1; }
  W[$i]=0x$(basename "$f" | grep -oE '[0-9a-fA-F]{40}$')
done

# ---- 3. bootnode -----------------------------------------------------------
PUB=$("$BOOTNODE" -nodekey ./bootnode.key -writeaddress)
ENODE="enode://${PUB}@${IP}:30401"
echo "bootnode    : $ENODE"
setsid "$BOOTNODE" -nodekey ./bootnode.key -addr ${IP}:30401 > "$LOG/bootnode.log" 2>&1 &
sleep 2

# ---- 4. two legacy sealers (validators 1 and 2) ----------------------------
for i in 1 2; do
  P2P=$((30410 + i)); RPC=$((8610 + i)); WS=$((8620 + i))
  setsid "$XDC" --bootnodes "$ENODE" --syncmode full --datadir ./nodes/$i \
    --networkid "$NETWORKID" --port $P2P --nat "extip:${IP}" \
    --rpc --rpcaddr 0.0.0.0 --rpcport $RPC --rpccorsdomain "*" --rpcvhosts "*" \
    --ws --wsaddr 127.0.0.1 --wsport $WS --wsorigins "*" \
    --rpcapi admin,db,eth,debug,miner,net,txpool,personal,web3,XDPoS \
    --unlock "${W[$i]}" --etherbase "${W[$i]}" --password ./.pwd --mine \
    --gasprice 1 --targetgaslimit 420000000 --verbosity 3 \
    > "$LOG/legacy$i.log" 2>&1 &
  echo "legacy$i     : rpc=$RPC validator=${W[$i]}  log=$LOG/legacy$i.log"
done
sleep 3

# ---- 5. two geth validators (validators 3 and 4) ---------------------------
# Each reuses the genesis-seated key from ./nodes/<n>, copied into its own
# datadir so geth can unlock it. Same key = same seated validator identity.
g=0
for i in 3 4; do
  g=$((g + 1))
  DD="./nodes/$((4 + g))-geth"
  P2P=$((30414 + g)); HTTP=$((8614 + g)); AUTH=$((8644 + g))
  mkdir -p "$DD/keystore"
  cp -n ./nodes/$i/keystore/UTC--* "$DD/keystore/" 2>/dev/null
  [ -d "$DD/geth/chaindata" ] || "$GETH_BIN" --datadir "$DD" init ./genesis/genesis.json >/dev/null 2>&1
  setsid "$GETH_BIN" --datadir "$DD" --networkid "$NETWORKID" --port $P2P \
    --bootnodes "$ENODE" --syncmode full --nat "extip:${IP}" \
    --http --http.addr 127.0.0.1 --http.port $HTTP \
    --http.api eth,net,web3,admin,debug,txpool,miner,personal,XDPoS \
    --authrpc.port $AUTH --verbosity 4 \
    --mine --miner.etherbase "${W[$i]}" --unlock "${W[$i]}" \
    --password ./.pwd --allow-insecure-unlock \
    > "$LOG/geth$g.log" 2>&1 &
  echo "geth$g       : http=$HTTP validator=${W[$i]}  log=$LOG/geth$g.log"
done

cat <<EOF

started. validator set = 4 (2 legacy + 2 geth), quorum = 3.
v1 -> v2 switch at block 900 (~15 min at 1s blocks).

  watch progress : ./status.sh
  geth log       : tail -f $LOG/geth1.log
  stop everything: ./reset.sh      (this also DELETES chain data)
EOF
