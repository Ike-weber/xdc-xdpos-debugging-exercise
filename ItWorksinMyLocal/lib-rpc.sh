#!/bin/bash
# lib-rpc.sh - topology-independent JSON-RPC / ABI helpers.
#
# Factored OUT of lab/lib-lab.sh (see ItWorksinMyLocal#46 T0.1) so the
# netlab/ topology engine (health.sh, node.sh, fleet.sh) can use the same
# battle-tested RPC transport and 0x88-masternode-contract ABI helpers as
# lab/ without depending on lab/lib-lab.sh's TOPOLOGY-SPECIFIC bits (the
# fixed node1..node4 / 5-client port registry, lab_start_producer, etc --
# those stay in lab/lib-lab.sh since they hardcode run.sh's topology).
#
# Function names are UNCHANGED from lib-lab.sh (T0.1 acceptance): any caller
# that already does `source lib-lab.sh` keeps working exactly as before,
# because lib-lab.sh now sources this file. New callers (netlab/*) can
# `source lib-rpc.sh` directly without pulling in the fixed-topology helpers.
#
# No dependency on lib.sh / LAB_DIR / REPO_ROOT here on purpose: this file
# must be sourceable standalone from any directory.

# The XDPoS masternode/validator contract, always at this address on every
# XDC-family client.
LAB_MASTERNODE_CONTRACT="0x0000000000000000000000000000000000000088"

# ---------------------------------------------------------------------------
# JSON-RPC transport
# ---------------------------------------------------------------------------

# lab_rpc_call <url> <method> <json-params>: POSTs a JSON-RPC request and
# prints the raw "result" (a bare string as-is, anything else JSON-encoded).
# Returns nonzero on a transport error, an RPC error, or a null result.
lab_rpc_call() {
  local url="$1" method="$2" params="${3:-[]}" resp
  resp=$(curl -s -m "${LAB_RPC_TIMEOUT:-10}" -X POST "$url" \
    -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"${method}\",\"params\":${params},\"id\":1}") || return 1
  [ -n "$resp" ] || return 1
  python3 - "$resp" <<'PY'
import json
import sys

try:
    obj = json.loads(sys.argv[1])
except Exception:
    sys.exit(1)
if "error" in obj:
    sys.stderr.write(json.dumps(obj["error"]) + "\n")
    sys.exit(1)
result = obj.get("result")
if result is None:
    sys.exit(1)
if isinstance(result, str):
    print(result)
else:
    print(json.dumps(result))
PY
}

# lab_block_number <url>: decimal head height.
lab_block_number() {
  local url="$1" hex
  hex=$(lab_rpc_call "$url" eth_blockNumber "[]") || return 1
  printf '%d\n' "$hex"
}

# lab_block_hash <url> <N>: 0x... hash of block N, or "" (+ nonzero) if the
# node doesn't have that block yet.
lab_block_hash() {
  local url="$1" n="$2" hexn block
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || { printf ''; return 1; }
  python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
print(b.get('hash', '') if b else '')
" "$block"
}

# lab_block_gas_limit <url> <N>: decimal gasLimit of block N, or "" (+
# nonzero) if the node doesn't have that block yet. The first-class metric
# ItWorksinMyLocal#94/#96 exists to watch: this must MOVE ONLY TOWARD, then
# HOLD FLAT AT, the network's single mint/plateau target (never step away
# from it once reached, and never step DOWN when it hasn't -- a down-step
# is the exact signature of a second producer honing toward a LOWER target
# than the one that just raised it, i.e. two different targets on one
# chain). See lab/lib-assert.sh's assert_gas_limit_plateau.
lab_block_gas_limit() {
  local url="$1" n="$2" hexn block
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || { printf ''; return 1; }
  python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
if not b or 'gasLimit' not in b:
    sys.exit(1)
print(int(b['gasLimit'], 16))
" "$block"
}

# lab_peer_count <url>: decimal peer count via net_peerCount -- UNIVERSAL
# across every client this framework can bring up, including erigon, whose
# join.sh profile never enables the `admin` RPC namespace (--http.api
# eth,net,web3,erigon,debug,txpool -- no admin) and so never answers
# admin_peers at all (docs/lab/DESIGN.md §3's client registry). Anything
# that needs a peer-count check that must work identically across EVERY
# follower client -- not just the admin-capable ones (oldxdc/geth/besu/
# reth/xone; see lab/lib-topo.sh's topo_partition) -- should call this
# instead of admin_peers (ItWorksinMyLocal#46 T3.3: an admin_peers-based
# check would misreport a perfectly healthy erigon follower as
# peer-starved, which is exactly the kind of false-FAIL this lab's honesty
# rule exists to prevent).
lab_peer_count() {
  local url="$1" hex
  hex=$(lab_rpc_call "$url" net_peerCount "[]") || return 1
  printf '%d\n' "$hex"
}

# lab_wait_block <url> <N> [timeout_s]: blocks until head >= N, or returns
# nonzero after timeout_s (default 120).
lab_wait_block() {
  local url="$1" target="$2" timeout="${3:-120}" waited=0 cur
  while :; do
    cur=$(lab_block_number "$url" 2>/dev/null) || cur=0
    [ -n "$cur" ] && [ "$cur" -ge "$target" ] 2>/dev/null && return 0
    [ "$waited" -ge "$timeout" ] && return 1
    sleep 2
    waited=$((waited + 2))
  done
}

# ---------------------------------------------------------------------------
# ABI helpers (0x88 masternode contract)
# ---------------------------------------------------------------------------

# lab_abi_encode_address <0xAddr>: 32-byte left-padded word (64 hex chars,
# no 0x prefix), suitable for concatenating after a selector.
lab_abi_encode_address() {
  python3 -c "
import sys

a = sys.argv[1].lower()
if a.startswith('0x'):
    a = a[2:]
if len(a) != 40:
    sys.stderr.write('lab_abi_encode_address: not a 20-byte address: %s\n' % sys.argv[1])
    sys.exit(1)
print(a.rjust(64, '0'))
" "$1"
}

# lab_abi_encode_uint256 <decimal>: 32-byte left-padded word (64 hex chars,
# no 0x prefix). Accepts arbitrary-size integers (wei amounts).
lab_abi_encode_uint256() {
  python3 -c "
import sys

n = int(sys.argv[1])
if n < 0:
    sys.stderr.write('lab_abi_encode_uint256: negative value\n')
    sys.exit(1)
print(format(n, '064x'))
" "$1"
}

# lab_abi_encode_string <string>: ABI encoding of a single dynamic `string`
# argument (offset word + length word + data, right-padded to a 32-byte
# boundary), no leading selector. Needed for uploadKYC(string) -- see the
# note on cmd_upload_kyc in lab/masternode.sh for why that call exists at all.
lab_abi_encode_string() {
  python3 -c "
import sys

s = sys.argv[1].encode('utf-8')
length = len(s)
padded = ((length + 31) // 32) * 32
data_hex = s.hex().ljust(padded * 2, '0')
print(format(0x20, '064x') + format(length, '064x') + data_hex)
" "$1"
}

# lab_decode_address_array <hex>: decodes an ABI dynamic address[] return
# value (offset word, length word, then N left-padded address words) into a
# sorted, comma-joined, lowercase 0x-prefixed address list. Pure decode, no
# network access -- this is what oracle.sh --self-test exercises directly.
lab_decode_address_array() {
  python3 -c "
import sys

h = sys.argv[1]
if h.startswith('0x'):
    h = h[2:]
if len(h) < 128:
    print('')
    sys.exit(0)
length = int(h[64:128], 16)
addrs = []
pos = 128
for _ in range(length):
    word = h[pos:pos + 64]
    if len(word) < 64:
        break
    addrs.append('0x' + word[-40:].lower())
    pos += 64
addrs.sort()
print(','.join(addrs))
" "$1"
}

# lab_call_0x88 <url> <selector-with-args-hex>: eth_call to the masternode
# contract at 'latest', returns the raw hex result.
lab_call_0x88() {
  local url="$1" data="$2"
  lab_rpc_call "$url" eth_call "[{\"to\":\"${LAB_MASTERNODE_CONTRACT}\",\"data\":\"0x${data#0x}\"},\"latest\"]"
}

# lab_get_candidates <url>: decoded, sorted, comma-joined getCandidates()
# address list. Universal (eth_call only) -- works against every client.
lab_get_candidates() {
  local url="$1" raw
  raw=$(lab_call_0x88 "$url" "06a49fce") || return 1
  lab_decode_address_array "$raw"
}
