#!/bin/bash
# 02-reorg-across-epoch.sh - reorg-across-epoch-set-change
# (docs/lab/DESIGN.md §8, row 02 / §6 "Competing branches / reorg").
#
# Splits the 4 oldxdc sealers 2/2 (admin_removePeer, via lab_partition)
# *before* the epoch-900 gap block (450), lets each branch resign a
# *different* validator (self-resign of its own producer account -- no
# KYC/propose plumbing needed, see docs/lab/DESIGN.md §4/finding F2), so
# each branch's gap-450 read of candidate state produces a *different*
# next-epoch (900) masternode set. Heals the partition, lets the network
# reorg onto whichever branch wins, then asserts:
#   1. the reference's checkpoint-900 header dropped exactly the resigning
#      validator from the *winning* branch (not both, not neither -- that
#      would mean the losing branch's precomputed set leaked through, the
#      "stale snapshot cache keyed by epoch-number vs by branch/hash" bug
#      this scenario exists to catch, docs/lab/DESIGN.md §1);
#   2. every follower matches the reference byte-for-byte (block hash) and
#      on decoded getCandidates() across the whole range, i.e. every
#      follower reorged onto the same winning branch and recomputed the
#      same masternode set from it, not from whatever it had cached before
#      the partition/heal.
#
# Followers are brought up *before* the partition (not after) so they are
# actively peered through the split and experience the fork/heal/reorg
# themselves -- cold-start-after-the-fact is scenario 10's job, not this
# one's.
#
# Needs LAB_SWITCH_BLOCK=2700 (not the default 900): the table's rationale
# (docs/lab/DESIGN.md §5 "Only one v1 epoch at switchBlock=900") is that this
# scenario needs the epoch-900 boundary to be a *settled v1* checkpoint, not
# the v1->v2 switch point itself -- those are two different seams and mixing
# them would make a divergence ambiguous (engine-handoff bug vs gap-read bug).

export SCEN_DESC="reorg-across-epoch-set-change: 2/2 partition before gap 450, differing resigns per branch, heal, assert winning-branch set"
export SCEN_PRIORITY="P0"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S02_SWITCH_BLOCK=2700
_S02_PERIOD=1
_S02_GAP_BLOCK=450
_S02_EPOCH_BLOCK=900
_S02_SETTLE_TO=920

# Producer sealer RPC ports, per docs/lab/DESIGN.md §2/§3 (run.sh's fixed
# node1..4 allocation; lab_rpc() only names the reference (node1) and the
# follower clients, not individual sealers). node2 and node4 are used as the
# branch-A/branch-B representatives (rather than node1/node3): each node's
# private key is unlocked ONLY on that node's own process (run.sh gives each
# sealer its own datadir/keystore), so node2's self-resign tx MUST be sent
# via node2's own RPC, not node1's -- node1 does not have node2's key.
_S02_NODE1_RPC="http://127.0.0.1:8545"
_S02_NODE2_RPC="http://127.0.0.1:8546"
_S02_NODE3_RPC="http://127.0.0.1:8547"
_S02_NODE4_RPC="http://127.0.0.1:8548"

# _s02_checkpoint_masternodes <url> <block-number>: decodes that checkpoint
# block's extraData into an ordered, comma-joined address list. Universal
# (eth_getBlockByNumber only) -- mirrors the XDPoS v1 checkpoint layout
# (ExtraVanity=32B .. N*20B masternode addresses .. ExtraSeal=65B), see
# ../../XDPoSChain/consensus/XDPoS/engines/engine_v1/{engine.go,utils.go}.
_s02_checkpoint_masternodes() {
  local url="$1" n="$2" hexn block extra
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || return 1
  extra=$(python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
print((b or {}).get('extraData', ''))
" "$block") || return 1
  [ -n "$extra" ] || { echo "_s02_checkpoint_masternodes: no extraData for block $n at $url" >&2; return 1; }
  python3 -c "
import sys

h = sys.argv[1]
if h.startswith('0x') or h.startswith('0X'):
    h = h[2:]
vanity_hex = 32 * 2
seal_hex = 65 * 2
if len(h) < vanity_hex + seal_hex:
    print('')
    sys.exit(0)
body = h[vanity_hex: len(h) - seal_hex]
addrs = []
i = 0
while i + 40 <= len(body):
    addrs.append('0x' + body[i:i + 40].lower())
    i += 40
print(','.join(addrs))
" "$extra"
}

# _s02_list_has <comma-list> <addr>: case-insensitive membership check.
_s02_list_has() {
  local low_list low_addr
  low_list=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  low_addr=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')
  case ",${low_list}," in
    *",${low_addr},"*) return 0 ;;
    *) return 1 ;;
  esac
}

scen_02() {
  local clients c ref rc
  local node2_addr node4_addr
  local branchA_head branchB_head
  local candA candB
  local ckpt_ref node2_in node4_in

  export LAB_SWITCH_BLOCK="$_S02_SWITCH_BLOCK"
  export LAB_PERIOD="$_S02_PERIOD"

  echo "scen_02: starting producer (switch=$LAB_SWITCH_BLOCK period=$LAB_PERIOD)"
  lab_start_producer real || { echo "scen_02: lab_start_producer failed"; return 1; }

  clients=$(lab_clients)
  for c in $clients; do
    echo "scen_02: starting follower $c (before partition, so it experiences the reorg)"
    lab_start_follower "$c" || { echo "scen_02: lab_start_follower $c failed"; return 1; }
  done

  ref=$(lab_rpc oldxdc)

  # node2/node4 self-resign as the two branches' divergent action -- each is
  # already unlocked+mining on its own node, so --from <addr> needs no key.
  node2_addr=$(lab_rpc_call "http://127.0.0.1:8546" eth_coinbase "[]") \
    || { echo "scen_02: could not read node2 coinbase"; return 1; }
  node4_addr=$(lab_rpc_call "http://127.0.0.1:8548" eth_coinbase "[]") \
    || { echo "scen_02: could not read node4 coinbase"; return 1; }
  echo "scen_02: branch-A resign target=$node2_addr branch-B resign target=$node4_addr"

  echo "scen_02: waiting for a few blocks before the gap ($_S02_GAP_BLOCK - 10)"
  lab_wait_block "$ref" $((_S02_GAP_BLOCK - 10)) 600 \
    || { echo "scen_02: producer stalled before the gap block"; return 1; }

  echo "scen_02: partitioning sealers node1,node2 | node3,node4"
  lab_partition "node1,node2" "node3,node4" || { echo "scen_02: lab_partition failed"; return 1; }

  echo "scen_02: branch A (node1,node2) resigns node2 ($node2_addr)"
  "$LAB_DIR/masternode.sh" resign "$node2_addr" --from "$node2_addr" --rpc "$_S02_NODE2_RPC" \
    || { echo "scen_02: branch-A resign tx failed"; return 1; }

  echo "scen_02: branch B (node3,node4) resigns node4 ($node4_addr)"
  "$LAB_DIR/masternode.sh" resign "$node4_addr" --from "$node4_addr" --rpc "$_S02_NODE4_RPC" \
    || { echo "scen_02: branch-B resign tx failed"; return 1; }

  # Sanity: confirm the two branches actually observe different candidate
  # pools while still split (getCandidates() is real-time, not epoch-gated --
  # only the *masternode-set-for-the-epoch* computed at the gap block is).
  candA=$(lab_get_candidates "$_S02_NODE1_RPC") || { echo "scen_02: could not read branch-A candidates"; return 1; }
  candB=$(lab_get_candidates "$_S02_NODE3_RPC") || { echo "scen_02: could not read branch-B candidates"; return 1; }
  if [ "$candA" = "$candB" ]; then
    echo "scen_02: FAIL - branch-A and branch-B candidate pools are identical; partition/resign did not create divergent state (test setup did not fire)"
    return 1
  fi
  echo "scen_02: confirmed branches hold divergent candidate state while split"

  branchA_head=$((_S02_GAP_BLOCK + 1))
  branchB_head=$((_S02_GAP_BLOCK + 1))
  echo "scen_02: waiting for both branches to pass the gap block ($_S02_GAP_BLOCK) independently"
  lab_wait_block "$_S02_NODE1_RPC" "$branchA_head" 600 || { echo "scen_02: branch A stalled before gap+1"; return 1; }
  lab_wait_block "$_S02_NODE3_RPC" "$branchB_head" 600 || { echo "scen_02: branch B stalled before gap+1"; return 1; }

  echo "scen_02: healing partition"
  lab_heal || { echo "scen_02: lab_heal failed"; return 1; }

  echo "scen_02: waiting for reference to settle past checkpoint $_S02_EPOCH_BLOCK (to $_S02_SETTLE_TO)"
  lab_wait_block "$ref" "$_S02_SETTLE_TO" 900 \
    || { echo "scen_02: reference did not settle past checkpoint $_S02_EPOCH_BLOCK after heal"; return 1; }

  ckpt_ref=$(_s02_checkpoint_masternodes "$ref" "$_S02_EPOCH_BLOCK") \
    || { echo "scen_02: could not decode reference checkpoint $_S02_EPOCH_BLOCK"; return 1; }
  if _s02_list_has "$ckpt_ref" "$node2_addr"; then node2_in=1; else node2_in=0; fi
  if _s02_list_has "$ckpt_ref" "$node4_addr"; then node4_in=1; else node4_in=0; fi

  if [ "$node2_in" -eq 1 ] && [ "$node4_in" -eq 1 ]; then
    echo "scen_02: FAIL - checkpoint $_S02_EPOCH_BLOCK contains BOTH resign targets; neither branch's resign took effect (unexpected)"
    return 1
  fi
  if [ "$node2_in" -eq 0 ] && [ "$node4_in" -eq 0 ]; then
    echo "scen_02: FAIL - checkpoint $_S02_EPOCH_BLOCK is missing BOTH resign targets; losing branch's effect leaked through (stale snapshot cache bug)"
    return 1
  fi
  echo "scen_02: checkpoint $_S02_EPOCH_BLOCK reflects exactly one branch's resign (node2_in=$node2_in node4_in=$node4_in) -- canonical branch recomputed correctly"

  echo "scen_02: running oracle across the partition/heal range on reference"
  "$LAB_DIR/oracle.sh" --from $((_S02_GAP_BLOCK - 20)) --to "$_S02_SETTLE_TO" \
    --clients "$(printf '%s' "$clients" | tr ' ' ',')" \
    --ref "$ref" --timeout-per-block 120
  rc=$?

  if [ "$rc" -eq 0 ]; then
    echo "scen_02: PASS - all followers converged on the winning branch's set (checkpoint $_S02_EPOCH_BLOCK) with full hash parity"
    return 0
  fi
  echo "scen_02: FAIL - divergence reported above (client + field identify the culprit)"
  return 1
}
