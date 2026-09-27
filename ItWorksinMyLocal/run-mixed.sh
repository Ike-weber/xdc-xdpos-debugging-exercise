#!/bin/bash
# run-mixed.sh -- issue #34: 5-node mixed-client topology.
#   N legacy XDPoSChain (oldxdc) validators (--mine) + 1 modern client follower.
# Mirrors the live net5151 topology (legacy producers + modern follower) locally,
# from genesis, so cross-client interop can be tested before touching the devnet.
#
# Usage:
#   ./setup.sh --new 4            # first: generate keys + genesis + init legacy nodes
#   ./run-mixed.sh                # 4 legacy validators + 1 geth follower, on ethstats
#   ./run-mixed.sh --modern-client geth        # pick the modern client (geth default)
#   ./run-mixed.sh --legacy 4 --modern-client reth
#   ./run-mixed.sh --no-ethstats  # don't report to ethstats
#
# Binaries: legacy = bin/oldxdc/<plat>/{XDC,bootnode} (ensure_bins builds them).
# Modern client binary is resolved from an env override (GETH_BIN / XONE_BIN /
# RETH_BIN / BESU_BIN / NETHERMIND_DIST) or a sensible default on this host.
cd "$(dirname "$0")" || exit 1
source ./lib.sh
export CLIENT=oldxdc

LEGACY=4
MODERN=geth
NETWORKID=20250          # distinct from mainnet(50)/net5151(5151) to avoid peer conflicts
MODERN_MINE=0            # geth mints only if =1; minting forks (its V1 blocks are rejected), following works
STATS_SECRET="xdc_openscan_stats_2026"
ETHSTATS_HOST="stats.xdcindia.com:443"
USE_STATS=1
while [ $# -gt 0 ]; do
  case "$1" in
    --legacy) LEGACY=$2; shift 2;;
    --modern-client) MODERN=$2; shift 2;;
    --no-ethstats) USE_STATS=0; shift;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "usage: ./run-mixed.sh [--legacy N] [--modern-client geth|xone|reth|besu|nethermind] [--no-ethstats]" >&2; exit 1;;
  esac
done

ensure_bins
XDC="$XDC_BIN"; BOOTNODE="$BOOTNODE_BIN"
IP=$(detect_ip); [ -z "$IP" ] && IP=127.0.0.1
HOST=$(hostname -s)
LOG="${MIXED_LOG:-./nodes/mixed-logs}"; mkdir -p "$LOG"   # under nodes/ so .gitignore covers it
touch .pwd

# Resolve the modern client binary. Prefer an explicit env override, else the
# repo's bin/<client>/<platform>/<binary> (build it with ./setup.sh --client <c>).
# mbin_var holds the env var name quoted in the error below -- it replaces a
# bash-4-only upper-casing parameter expansion that macOS bash 3.2 cannot
# parse. Set alongside MBIN rather than in a second switch over the same
# value: two switches drift apart the moment a client is added to one and not
# the other, and it fails silently (the name renders empty: "set =/path").
case "$MODERN" in
  geth) MBIN="${GETH_BIN:-bin/geth/$PLATFORM/geth}"; mbin_var=GETH_BIN;;
  xone) MBIN="${XONE_BIN:-bin/xone/$PLATFORM/xone}"; mbin_var=XONE_BIN;;
  reth) MBIN="${RETH_BIN:-bin/reth/$PLATFORM/xdc-reth}"; mbin_var=RETH_BIN;;
  *) echo "modern client '$MODERN' not wired in run-mixed.sh yet" >&2; exit 1;;
esac
[ -x "$MBIN" ] || { echo "modern binary not found: $MBIN (set ${mbin_var}=/path, or ./setup.sh --client $MODERN to build it)" >&2; exit 1; }

# legacy wallets from keystores (setup.sh must have imported them)
declare -a W
for i in $(seq 1 "$LEGACY"); do
  f=$(ls ./nodes/$i/keystore/UTC--* 2>/dev/null | head -1)
  [ -z "$f" ] && { echo "no key in ./nodes/$i -- run: ./setup.sh --new $LEGACY" >&2; exit 1; }
  W[$i]=0x$(basename "$f" | grep -oE '[0-9a-fA-F]{40}$')
done

# bootnode
PUB=$("$BOOTNODE" -nodekey ./bootnode.key -writeaddress)
ENODE="enode://${PUB}@${IP}:30401"
echo "bootnode: $ENODE"
setsid "$BOOTNODE" -nodekey ./bootnode.key -addr ${IP}:30401 > "$LOG/bootnode.log" 2>&1 &
sleep 2

# N legacy validators (mine, ethstats)
for i in $(seq 1 "$LEGACY"); do
  P2P=$((30410 + i)); RPC=$((8610 + i)); WS=$((8620 + i))
  ES=""; [ "$USE_STATS" = 1 ] && ES="--ethstats mixed5-legacy-${i}-${HOST}:${STATS_SECRET}@${ETHSTATS_HOST}"
  setsid "$XDC" --bootnodes "$ENODE" --syncmode full --datadir ./nodes/$i \
    --networkid "$NETWORKID" --port $P2P --nat "extip:${IP}" \
    --rpc --rpcaddr 0.0.0.0 --rpcport $RPC --rpccorsdomain "*" --rpcvhosts "*" \
    --ws --wsaddr 127.0.0.1 --wsport $WS --wsorigins "*" \
    --rpcapi admin,db,eth,debug,miner,net,txpool,personal,web3,XDPoS \
    --unlock "${W[$i]}" --etherbase "${W[$i]}" --password ./.pwd --mine \
    --gasprice 1 --targetgaslimit 420000000 --verbosity 3 $ES \
    > "$LOG/node$i.log" 2>&1 &
  echo "legacy node$i (mine) rpc=$RPC etherbase=${W[$i]} stats=${ES:+mixed5-legacy-$i-$HOST}"
done
sleep 3

# modern node (validator/proposer when MODERN_MINE=1, else follower), ethstats
DD="./nodes/5-${MODERN}"
ES5=""; [ "$USE_STATS" = 1 ] && ES5="--ethstats mixed5-${MODERN}-${HOST}:${STATS_SECRET}@${ETHSTATS_HOST}"
if [ "$MODERN" = geth ] || [ "$MODERN" = xone ]; then
  [ -d "$DD/geth/chaindata" ] || [ -d "$DD/XDC/chaindata" ] || "$MBIN" --datadir "$DD" init ./genesis/genesis.json >/dev/null 2>&1
  MINE=""
  if [ "$MODERN_MINE" = 1 ]; then
    A5=0x$(basename "$(ls $DD/keystore/UTC--* 2>/dev/null | head -1)" | grep -oE '[0-9a-fA-F]{40}$')
    MINE="--mine --miner.etherbase $A5 --unlock $A5 --password ./.pwd --allow-insecure-unlock"
    echo "  modern node mints as validator $A5"
  fi
  setsid "$MBIN" --datadir "$DD" --networkid "$NETWORKID" --port 30415 \
    --bootnodes "$ENODE" --syncmode full --nat "extip:${IP}" \
    --http --http.addr 127.0.0.1 --http.port 8615 \
    --http.api eth,net,web3,admin,debug,txpool,miner,personal,XDPoS \
    --authrpc.port 8645 --verbosity 3 $MINE $ES5 \
    > "$LOG/node5-${MODERN}.log" 2>&1 &
  echo "modern $MODERN node5 ($([ "$MODERN_MINE" = 1 ] && echo validator/mint || echo follower)) http=8615 stats=mixed5-${MODERN}-${HOST}"
else
  echo "note: $MODERN launch not scripted here; attach with join.sh instead" >&2
fi
echo "ALL STARTED (logs: $LOG)"
