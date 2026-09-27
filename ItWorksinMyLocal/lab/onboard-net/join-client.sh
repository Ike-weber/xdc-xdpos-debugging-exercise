#!/bin/bash
# join-client.sh — bring up a modern XDC client (geth-XDC / xone / reth / erigon)
# as a full node that JOINS the running oldxdc net, so it can then be onboarded
# as a masternode via onboard.py (uploadKYC + propose).
#
# This launches the node with mining/producing flags where the client supports
# it; after it syncs to tip and you run onboard.py, it seats at the next epoch
# checkpoint and mints on its round-robin turn.
#
# IMPORTANT (client fork/gas parity): a client must compute the SAME state root
# as the legacy oldxdc validators or it diverges (state-root mismatch) and can't
# sync. Use client builds carrying the XDC default network config (legacy-
# compatible TIPSigning) — see XDCIndia/go-ethereum#1339 (default XDC fork
# profile for unregistered XDPoS chains, no per-chainId hardcoding) and the
# equivalents for xone/reth/erigon. Do NOT rely on a per-net genesis fork ladder.
#
# Config via env:
#   BASE     working dir            (default ./onboard-run)
#   BIN      path to the client binary   [REQUIRED]
#   GENESIS  genesis.json           (default $BASE/genesis.json)
#   NAME     node name (geth|xone|reth|erigon; picks flags + ports)  [REQUIRED]
#   CAND_KEY candidate signer privkey (hex)   [REQUIRED]
#   BOOT     bootnode enode         (default: read $BASE/bootnode.enode)
#   PEERS    extra static enodes, comma-separated (recommend: the validator enodes)
#   IP       extip                  (default 127.0.0.1)
#   NETID    networkid              (default 34093)
#   BASEPORT p2p/rpc base for this client (default 37050)
#   STATS / STATS_PREFIX  ethstats (optional)
set -u
BASE=${BASE:-./onboard-run}
BIN=${BIN:?set BIN to the client binary}
GENESIS=${GENESIS:-$BASE/genesis.json}
NAME=${NAME:?set NAME to geth|xone|reth|erigon}
CKEY=${CAND_KEY:?set CAND_KEY to the candidate signer privkey}
BOOT=${BOOT:-$(cat "$BASE/bootnode.enode" 2>/dev/null)}
PEERS=${PEERS:-}
IP=${IP:-127.0.0.1}
NETID=${NETID:-34093}
BASEPORT=${BASEPORT:-37050}
STATS=${STATS:-}; STATS_PREFIX=${STATS_PREFIX:-xdclabs-net}
D=$BASE/$NAME; mkdir -p "$D"; echo password > "$BASE/.pwd"
P2P=$BASEPORT; HTTP=$((BASEPORT+1)); WS=$((BASEPORT+2)); AUTH=$((BASEPORT+3))
ALLPEERS="$BOOT${PEERS:+,$PEERS}"
CAND=0x$(python3 -c "from eth_account import Account;print(Account.from_key('$CKEY').address[2:])")
es=""; [ -n "$STATS" ] && es="${STATS_PREFIX}-${NAME}:${STATS}"

case "$NAME" in
  geth|xone)  # go-ethereum lineage: import key, init, --mine
    if [ -z "$(ls -A "$D/keystore" 2>/dev/null)" ]; then
      printf '%s' "$CKEY" > "$D/pk.hex"
      "$BIN" account import --datadir "$D" --password "$BASE/.pwd" "$D/pk.hex" >/dev/null 2>&1
    fi
    "$BIN" --datadir "$D" init "$GENESIS" >/dev/null 2>&1
    nohup "$BIN" --datadir "$D" --networkid "$NETID" --port "$P2P" --bootnodes "$ALLPEERS" \
      --syncmode full --nat "extip:${IP}" \
      --http --http.addr 127.0.0.1 --http.port "$HTTP" --http.api eth,net,web3,debug,XDPoS \
      --authrpc.port "$AUTH" \
      --mine --miner.etherbase "$CAND" --unlock "$CAND" --password "$BASE/.pwd" --allow-insecure-unlock \
      ${es:+--ethstats "$es"} > "$D/node.log" 2>&1 &
    ;;
  reth)   # Rust: XDC_PRODUCE=1 + signer key file
    printf '%s' "$CKEY" > "$D/xdc-signer.key"
    XDC_PRODUCE=1 XDC_SIGNER_KEY_FILE="$D/xdc-signer.key" \
    nohup "$BIN" node --datadir "$D" --chain "$GENESIS" \
      --port "$P2P" --http --http.addr 127.0.0.1 --http.port "$HTTP" --authrpc.port "$AUTH" \
      --bootnodes "$ALLPEERS" --trusted-peers "$ALLPEERS" > "$D/node.log" 2>&1 &
    ;;
  erigon) # Go staged-sync: --xdc.v1.produce + signer key
    printf '%s' "$CKEY" > "$D/xdc-signer.key"
    nohup "$BIN" --datadir "$D" --chain "$GENESIS" --xdc.v1.produce --xdc.v1.signerkey "$D/xdc-signer.key" \
      --port "$P2P" --http --http.addr 127.0.0.1 --http.port "$HTTP" --authrpc.port "$AUTH" \
      --bootnodes "$ALLPEERS" --staticpeers "$ALLPEERS" > "$D/node.log" 2>&1 &
    ;;
  *) echo "unknown client NAME=$NAME (geth|xone|reth|erigon)"; exit 1 ;;
esac
echo "$NAME up: cand=$CAND rpc=127.0.0.1:$HTTP p2p=$P2P pid=$!"
echo "next: wait for it to reach tip, then:  RPC=http://127.0.0.1:$HTTP CAND_KEY=$CKEY ./onboard.py both"
