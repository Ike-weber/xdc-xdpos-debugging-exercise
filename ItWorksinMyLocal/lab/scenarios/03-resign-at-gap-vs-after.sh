#!/bin/bash
# 03-resign-at-gap-vs-after.sh - resign-at-gap-vs-after
# (docs/lab/DESIGN.md §8, row 03 / §5 "the sharpest test tool").
#
# The gap-block rule: the masternode set for the *next* epoch boundary is
# computed by reading candidate state exactly at the gap block
# (block % Epoch == Epoch - Gap, i.e. 450 for the epoch-900 boundary), and
# takes effect at the epoch boundary itself (900). A resign() mined *in*
# block 450 must therefore be gone from the checkpoint-900 masternode set;
# a resign() mined one block later (451) is invisible until the *next*
# recompute (gap 1350) and so still appears at checkpoint 900, only
# dropping at checkpoint 1800. This is exactly the off-by-one every
# reimplementation risks.
#
# Two full producer lifecycles ("run A", "run B"), torn down between them:
#   run A: resign lands in block 450 (the gap block itself)
#          -> assert checkpoint 900 does NOT contain the resigned validator.
#   run B: resign lands in block 451 (one block after the gap)
#          -> assert checkpoint 900 STILL contains it, and checkpoint 1800
#             does not.
# After each run's functional assertion on the reference, oracle.sh checks
# hash+candidate parity across every follower over the relevant range: since
# extraData is part of the header, a follower that gets the gap-boundary
# read wrong computes a different checkpoint header (different hash), so
# hash divergence at exactly the checkpoint height is the direct signal for
# "off-by-one in gap read" (this scenario's classify-on-fail column).
#
# Needs LAB_SWITCH_BLOCK=2700 (settled v1 epoch boundaries at 900/1800; see
# docs/lab/DESIGN.md §5 "Only one v1 epoch at switchBlock=900").

export SCEN_DESC="resign-at-gap-vs-after: resign in block 450 vs 451, assert checkpoint-900/1800 off-by-one behaviour"
export SCEN_PRIORITY="P0"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S03_SWITCH_BLOCK=2700
_S03_PERIOD=1
_S03_GAP_BLOCK=450
_S03_EPOCH_900=900
_S03_EPOCH_1800=1800

# _s03_checkpoint_masternodes <url> <block-number>: decodes that checkpoint
# block's extraData into an ordered, comma-joined address list. Universal
# (eth_getBlockByNumber only) -- mirrors the XDPoS v1 checkpoint layout
# (ExtraVanity=32B .. N*20B masternode addresses .. ExtraSeal=65B), see
# ../../XDPoSChain/consensus/XDPoS/engines/engine_v1/{engine.go,utils.go}.
_s03_checkpoint_masternodes() {
  local url="$1" n="$2" hexn block extra
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || return 1
  extra=$(python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
print((b or {}).get('extraData', ''))
" "$block") || return 1
  [ -n "$extra" ] || { echo "_s03_checkpoint_masternodes: no extraData for block $n at $url" >&2; return 1; }
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

# _s03_list_has <comma-list> <addr>: case-insensitive membership check.
_s03_list_has() {
  local low_list low_addr
  low_list=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  low_addr=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')
  case ",${low_list}," in
    *",${low_addr},"*) return 0 ;;
    *) return 1 ;;
  esac
}

# _s03_wait_block_eq <url> <N> <timeout>: busy-waits until the head is
# *exactly* N (not just >= N), for landing a tx in a precise block. Polls
# fast (1s) since period=1 leaves no slack.
_s03_wait_block_eq() {
  local url="$1" target="$2" timeout="${3:-300}" waited=0 cur
  while :; do
    cur=$(lab_block_number "$url" 2>/dev/null) || cur=0
    [ -n "$cur" ] && [ "$cur" -ge "$target" ] 2>/dev/null && { printf '%s\n' "$cur"; return 0; }
    [ "$waited" -ge "$timeout" ] && { printf '%s\n' "$cur"; return 1; }
    sleep 1
    waited=$((waited + 1))
  done
}

# _s03_run <label> <resign_target_block>: brings up a fresh producer+
# followers, resigns the initial checkpoint-0 masternode #1 in exactly $2,
# and on success prints ONLY a clean "target_addr|clients" line to stdout
# (all diagnostics go to stderr) since the caller captures this function's
# stdout via command substitution.
_s03_run() {
  local label="$1" resign_block="$2"
  local ref clients c target_addr landed txh receipt landed_at

  export LAB_SWITCH_BLOCK="$_S03_SWITCH_BLOCK"
  export LAB_PERIOD="$_S03_PERIOD"

  echo "scen_03[$label]: starting producer (switch=$LAB_SWITCH_BLOCK period=$LAB_PERIOD)" >&2
  lab_start_producer real || { echo "scen_03[$label]: lab_start_producer failed" >&2; return 1; }

  clients=$(lab_clients)
  for c in $clients; do
    echo "scen_03[$label]: starting follower $c" >&2
    lab_start_follower "$c" || { echo "scen_03[$label]: lab_start_follower $c failed" >&2; return 1; }
  done

  ref=$(lab_rpc oldxdc)

  target_addr=$(_s03_checkpoint_masternodes "$ref" 0 | cut -d',' -f1) \
    || { echo "scen_03[$label]: could not decode genesis checkpoint masternodes" >&2; return 1; }
  [ -n "$target_addr" ] || { echo "scen_03[$label]: genesis checkpoint has no masternodes" >&2; return 1; }
  echo "scen_03[$label]: resign target=$target_addr, must land in block $resign_block" >&2

  echo "scen_03[$label]: waiting for block $((resign_block - 1))" >&2
  _s03_wait_block_eq "$ref" $((resign_block - 1)) 600 >/dev/null \
    || { echo "scen_03[$label]: producer stalled before block $((resign_block - 1))" >&2; return 1; }

  txh=$("$LAB_DIR/masternode.sh" resign "$target_addr" --from "$target_addr" --rpc "$ref" | grep -oE 'tx=0x[0-9a-fA-F]+' | cut -d= -f2) \
    || { echo "scen_03[$label]: resign tx submission failed" >&2; return 1; }
  [ -n "$txh" ] || { echo "scen_03[$label]: resign tx submission returned no tx hash" >&2; return 1; }

  landed=""
  local waited=0
  while [ -z "$landed" ] && [ "$waited" -lt 60 ]; do
    receipt=$(lab_rpc_call "$ref" eth_getTransactionReceipt "[\"$txh\"]" 2>/dev/null) || receipt=""
    if [ -n "$receipt" ]; then
      landed=$(python3 -c "
import json
import sys

r = json.loads(sys.argv[1])
bn = (r or {}).get('blockNumber')
print(int(bn, 16) if bn else '')
" "$receipt")
    fi
    [ -n "$landed" ] && break
    sleep 1
    waited=$((waited + 1))
  done
  [ -n "$landed" ] || { echo "scen_03[$label]: resign tx $txh never confirmed" >&2; return 1; }
  landed_at="$landed"
  echo "scen_03[$label]: resign tx landed in block $landed_at (wanted $resign_block)" >&2
  if [ "$landed_at" -ne "$resign_block" ]; then
    echo "scen_03[$label]: FAIL - could not land the resign precisely in block $resign_block (test-harness timing precision not achieved, landed $landed_at)" >&2
    return 1
  fi

  printf '%s|%s\n' "$target_addr" "$clients"
  return 0
}

scen_03() {
  local runA_out runA_target runA_clients ref
  local runB_out runB_target runB_clients
  local ckptA900 ckptB900 ckptB1800
  local rc

  # ---- run A: resign lands exactly at the gap block (450) ----
  echo "scen_03: === run A: resign at gap block $_S03_GAP_BLOCK ==="
  runA_out=$(_s03_run "A" "$_S03_GAP_BLOCK")
  rc=$?
  [ "$rc" -eq 0 ] || { echo "scen_03: run A setup failed"; return 1; }
  runA_target="${runA_out%%|*}"
  runA_clients="${runA_out#*|}"

  ref=$(lab_rpc oldxdc)
  echo "scen_03[A]: waiting for reference to settle past checkpoint $_S03_EPOCH_900"
  lab_wait_block "$ref" $((_S03_EPOCH_900 + 5)) 900 \
    || { echo "scen_03[A]: reference did not reach checkpoint $_S03_EPOCH_900 + margin"; return 1; }

  ckptA900=$(_s03_checkpoint_masternodes "$ref" "$_S03_EPOCH_900") \
    || { echo "scen_03[A]: could not decode checkpoint $_S03_EPOCH_900"; return 1; }
  if _s03_list_has "$ckptA900" "$runA_target"; then
    echo "scen_03[A]: FAIL - checkpoint $_S03_EPOCH_900 still contains $runA_target; a resign mined IN the gap block should already be excluded (off-by-one)"
    return 1
  fi
  echo "scen_03[A]: checkpoint $_S03_EPOCH_900 correctly excludes $runA_target (resign-at-gap took effect immediately)"

  echo "scen_03[A]: running oracle around checkpoint $_S03_EPOCH_900"
  "$LAB_DIR/oracle.sh" --from $((_S03_GAP_BLOCK - 10)) --to $((_S03_EPOCH_900 + 5)) \
    --clients "$(printf '%s' "$runA_clients" | tr ' ' ',')" --ref "$ref" --timeout-per-block 120
  rc=$?
  [ "$rc" -eq 0 ] || { echo "scen_03[A]: FAIL - divergence reported above"; return 1; }

  echo "scen_03: tearing down run A before run B"
  lab_teardown >/dev/null 2>&1

  # ---- run B: resign lands one block after the gap (451) ----
  echo "scen_03: === run B: resign at gap block + 1 ($((_S03_GAP_BLOCK + 1))) ==="
  runB_out=$(_s03_run "B" $((_S03_GAP_BLOCK + 1)))
  rc=$?
  [ "$rc" -eq 0 ] || { echo "scen_03: run B setup failed"; return 1; }
  runB_target="${runB_out%%|*}"
  runB_clients="${runB_out#*|}"

  ref=$(lab_rpc oldxdc)
  echo "scen_03[B]: waiting for reference to settle past checkpoint $_S03_EPOCH_900"
  lab_wait_block "$ref" $((_S03_EPOCH_900 + 5)) 900 \
    || { echo "scen_03[B]: reference did not reach checkpoint $_S03_EPOCH_900 + margin"; return 1; }

  ckptB900=$(_s03_checkpoint_masternodes "$ref" "$_S03_EPOCH_900") \
    || { echo "scen_03[B]: could not decode checkpoint $_S03_EPOCH_900"; return 1; }
  if ! _s03_list_has "$ckptB900" "$runB_target"; then
    echo "scen_03[B]: FAIL - checkpoint $_S03_EPOCH_900 already excludes $runB_target; a resign mined ONE block after the gap should not be visible until the next epoch (off-by-one, dropped too early)"
    return 1
  fi
  echo "scen_03[B]: checkpoint $_S03_EPOCH_900 correctly still contains $runB_target (resign-after-gap not yet visible)"

  echo "scen_03[B]: waiting for reference to settle past checkpoint $_S03_EPOCH_1800"
  lab_wait_block "$ref" $((_S03_EPOCH_1800 + 5)) 1800 \
    || { echo "scen_03[B]: reference did not reach checkpoint $_S03_EPOCH_1800 + margin"; return 1; }

  ckptB1800=$(_s03_checkpoint_masternodes "$ref" "$_S03_EPOCH_1800") \
    || { echo "scen_03[B]: could not decode checkpoint $_S03_EPOCH_1800"; return 1; }
  if _s03_list_has "$ckptB1800" "$runB_target"; then
    echo "scen_03[B]: FAIL - checkpoint $_S03_EPOCH_1800 still contains $runB_target; the resign should have taken effect by the second recompute"
    return 1
  fi
  echo "scen_03[B]: checkpoint $_S03_EPOCH_1800 correctly excludes $runB_target (resign-after-gap took effect one epoch late, as expected)"

  echo "scen_03[B]: running oracle across checkpoints $_S03_EPOCH_900 and $_S03_EPOCH_1800"
  "$LAB_DIR/oracle.sh" --from $((_S03_GAP_BLOCK - 10)) --to $((_S03_EPOCH_1800 + 5)) \
    --clients "$(printf '%s' "$runB_clients" | tr ' ' ',')" --ref "$ref" --interval-blocks 10 --timeout-per-block 120
  rc=$?
  [ "$rc" -eq 0 ] || { echo "scen_03[B]: FAIL - divergence reported above"; return 1; }

  echo "scen_03: PASS - both run A (drop-at-900) and run B (drop-at-1800) matched expected gap-off-by-one behaviour, all followers agreed"
  return 0
}
