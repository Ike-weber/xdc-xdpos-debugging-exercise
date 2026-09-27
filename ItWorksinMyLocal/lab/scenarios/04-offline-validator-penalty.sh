#!/bin/bash
# 04-offline-validator-penalty.sh - offline-validator-penalty
# (docs/lab/DESIGN.md §8, row 04 / §6 "Offline validator (penalty)").
#
# Stops one producer sealer (node3) for a full v1 epoch (900 -> 1800),
# restarts it, and asserts the checkpoint-1800 masternode set dropped it via
# the HookPenalty mechanism (a masternode that never appears as a "signer"
# in ANY block across the whole prior epoch gets penalized -- see
# ../../XDPoSChain/eth/hooks/engine_v1_hooks.go AttachConsensusV1Hooks /
# ../../XDPoSChain/consensus/XDPoS/engines/engine_v1/engine.go Prepare()).
#
# This is a *different* removal path from scenario 03's resign(): the
# stopped node never resigns, so it stays a "candidate" in getCandidates()
# (that ABI call is real-time contract state, unaffected by liveness) --
# only the checkpoint header's masternode list (decoded from extraData)
# drops it, because HookPenalty subtracts penalized addresses from the
# signer set *before* it's written into header.Extra. So this scenario's
# assertion has to be made on decoded checkpoint extraData, not on
# getCandidates(); oracle.sh's getCandidates()-based `set=` column is
# expected to stay OK throughout (it is not what's under test here).
#
# Uses LAB_PERIOD=2 (risk R1, docs/lab/DESIGN.md §5): at period=1 the
# sign-tx inclusion window that HookPenalty relies on halves, and a merely
# *slow* (not actually offline) client could look penalized too --
# confounding this scenario's specific, deterministic trigger (a fully
# stopped node for the whole epoch). LAB_SWITCH_BLOCK=2700 for the same
# "settled v1 boundary" reason as scenarios 02/03.

export SCEN_DESC="offline-validator-penalty: stop node3 across epoch 900->1800, assert checkpoint-1800 penalty, all clients agree"
export SCEN_PRIORITY="P0"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S04_SWITCH_BLOCK=2700
_S04_PERIOD=2
_S04_EPOCH_900=900
_S04_EPOCH_1800=1800
_S04_STOP_MARGIN=5

# node3's RPC port, per docs/lab/DESIGN.md §2/§3 (run.sh's fixed node1..4
# allocation). Kept as node3, not node1, so the reference (node1) keeps
# running throughout and stays queryable.
_S04_NODE3_RPC="http://127.0.0.1:8547"

# _s04_checkpoint_masternodes <url> <block-number>: decodes that checkpoint
# block's extraData into an ordered, comma-joined address list. Universal
# (eth_getBlockByNumber only) -- mirrors the XDPoS v1 checkpoint layout
# (ExtraVanity=32B .. N*20B masternode addresses .. ExtraSeal=65B), see
# ../../XDPoSChain/consensus/XDPoS/engines/engine_v1/{engine.go,utils.go}.
_s04_checkpoint_masternodes() {
  local url="$1" n="$2" hexn block extra
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || return 1
  extra=$(python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
print((b or {}).get('extraData', ''))
" "$block") || return 1
  [ -n "$extra" ] || { echo "_s04_checkpoint_masternodes: no extraData for block $n at $url" >&2; return 1; }
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

# _s04_list_has <comma-list> <addr>: case-insensitive membership check.
_s04_list_has() {
  local low_list low_addr
  low_list=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  low_addr=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')
  case ",${low_list}," in
    *",${low_addr},"*) return 0 ;;
    *) return 1 ;;
  esac
}

scen_04() {
  local clients c ref rc
  local target_addr ckpt0 ckpt900 ckpt1800
  local stop_at wait1800

  export LAB_SWITCH_BLOCK="$_S04_SWITCH_BLOCK"
  export LAB_PERIOD="$_S04_PERIOD"

  echo "scen_04: starting producer (switch=$LAB_SWITCH_BLOCK period=$LAB_PERIOD)"
  lab_start_producer real || { echo "scen_04: lab_start_producer failed"; return 1; }

  clients=$(lab_clients)
  for c in $clients; do
    echo "scen_04: starting follower $c"
    lab_start_follower "$c" || { echo "scen_04: lab_start_follower $c failed"; return 1; }
  done

  ref=$(lab_rpc oldxdc)

  target_addr=$(lab_rpc_call "$_S04_NODE3_RPC" eth_coinbase "[]") \
    || { echo "scen_04: could not read node3 coinbase"; return 1; }
  echo "scen_04: penalty target (node3)=$target_addr"

  ckpt0=$(_s04_checkpoint_masternodes "$ref" 0) || { echo "scen_04: could not decode genesis checkpoint"; return 1; }
  if ! _s04_list_has "$ckpt0" "$target_addr"; then
    echo "scen_04: FAIL - node3 ($target_addr) is not in the genesis masternode set; wrong stop target for this test"
    return 1
  fi

  # period=2 -> block N takes >= 2*N seconds; generous margins below account
  # for startup + follower sync overhead on top of that floor.
  stop_at=$((_S04_EPOCH_900 + _S04_STOP_MARGIN))
  echo "scen_04: waiting for reference to reach block $stop_at (period=${LAB_PERIOD}s) before stopping node3"
  lab_wait_block "$ref" "$stop_at" 2400 \
    || { echo "scen_04: reference did not reach block $stop_at within timeout"; return 1; }

  echo "scen_04: stopping node3 for the whole epoch $_S04_EPOCH_900 -> $_S04_EPOCH_1800"
  lab_stop node3 || { echo "scen_04: lab_stop node3 failed"; return 1; }

  wait1800=$((_S04_EPOCH_1800 + _S04_STOP_MARGIN))
  echo "scen_04: waiting for reference to reach block $wait1800 with node3 offline"
  lab_wait_block "$ref" "$wait1800" 4200 \
    || { echo "scen_04: reference stalled before block $wait1800 (network may need >=3 live sealers; that itself would be a finding, not a harness bug)"; return 1; }

  echo "scen_04: restarting node3"
  lab_start_producer_node node3 || echo "scen_04: WARNING - node3 failed to restart (non-fatal to this scenario's assertions)"

  ckpt900=$(_s04_checkpoint_masternodes "$ref" "$_S04_EPOCH_900") \
    || { echo "scen_04: could not decode checkpoint $_S04_EPOCH_900"; return 1; }
  if ! _s04_list_has "$ckpt900" "$target_addr"; then
    echo "scen_04: FAIL - checkpoint $_S04_EPOCH_900 already excludes node3; it was still online through that checkpoint, should not yet be penalized"
    return 1
  fi
  echo "scen_04: checkpoint $_S04_EPOCH_900 still includes node3 (correct, it was online through this checkpoint)"

  ckpt1800=$(_s04_checkpoint_masternodes "$ref" "$_S04_EPOCH_1800") \
    || { echo "scen_04: could not decode checkpoint $_S04_EPOCH_1800"; return 1; }
  if _s04_list_has "$ckpt1800" "$target_addr"; then
    echo "scen_04: FAIL - checkpoint $_S04_EPOCH_1800 still includes node3; a full epoch of missed signing should have penalized it out"
    return 1
  fi
  echo "scen_04: checkpoint $_S04_EPOCH_1800 correctly excludes node3 (penalized after a full offline epoch)"

  echo "scen_04: running oracle across the stop/penalty range (checkpoints $_S04_EPOCH_900 and $_S04_EPOCH_1800, every 100th block so both land exactly on the grid)"
  "$LAB_DIR/oracle.sh" --from "$_S04_EPOCH_900" --to "$_S04_EPOCH_1800" \
    --clients "$(printf '%s' "$clients" | tr ' ' ',')" \
    --ref "$ref" --interval-blocks 100 --timeout-per-block 180
  rc=$?

  if [ "$rc" -eq 0 ]; then
    echo "scen_04: PASS - all clients agree node3 is penalized out at checkpoint $_S04_EPOCH_1800, full hash parity maintained"
    return 0
  fi
  echo "scen_04: FAIL - divergence reported above (client + field identify the culprit)"
  return 1
}
