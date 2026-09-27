#!/bin/bash
# 06-cap-overflow-tiebreak.sh - cap-overflow-tiebreak
# (docs/lab/DESIGN.md §8, row 06).
#
# uploadKYC + propose() every lab candidate-owner account (25 by default,
# see gen-lab-genesis.sh / lab-accounts.env) with the SAME stake value --
# deliberately forcing a tie across all of them, so which 18 make the
# checkpoint-900 masternode set (maxValidatorNumber, docs/lab/DESIGN.md §4)
# and in what order depends entirely on whatever tie-break rule the engine
# uses (insertion/proposal order, address ordering, ...), not on stake
# differences. Every propose() is preceded by uploadKYC() on that owner,
# per finding F2 (docs/lab/DESIGN.md §5) -- propose() has an
# onlyKYCWhitelisted modifier and reverts silently otherwise.
#
# The checkpoint-900 masternode list is decoded directly from the header's
# extraData (universal, eth_getBlockByNumber only -- no XDPoS_*, see
# docs/lab/DESIGN.md §3), which preserves the engine's own ordering. Two
# assertions:
#   1. the reference's list has exactly maxValidatorNumber entries (no
#      overflow past the cap);
#   2. every follower decodes the SAME list in the SAME order (not just the
#      same set) -- an order mismatch with an otherwise-identical member set
#      is exactly the "sort-stability / top-N" bug this scenario exists to
#      catch, and would be masked by a naive getCandidates()-only compare
#      (getCandidates() is not epoch-gated and returns all 25+ proposers
#      regardless of who wins the tie-break).
# oracle.sh's own hash+getCandidates() check runs on top as a broader parity
# pass (a client that disagrees on order would fail the hash check too,
# since extraData is part of the header -- the explicit decode above just
# gives a sharper classify-on-fail message than a bare hash diff would).
#
# Needs LAB_SWITCH_BLOCK=2700 for the same "settled v1 boundary" reason as
# scenarios 02/03/04.

export SCEN_DESC="cap-overflow-tiebreak: propose 25 tied candidates (>maxValidatorNumber), assert identical top-18 + order at checkpoint 900"
export SCEN_PRIORITY="P0"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S06_SWITCH_BLOCK=2700
_S06_PERIOD=1
_S06_GAP_BLOCK=450
_S06_EPOCH_900=900
_S06_SETTLE_TO=910
_S06_PROPOSE_VALUE=11000000

# _s06_checkpoint_masternodes <url> <block-number>: decodes that checkpoint
# block's extraData into an ORDERED, comma-joined address list (order
# preserved, NOT sorted -- that's the whole point of this scenario).
# Universal (eth_getBlockByNumber only) -- mirrors the XDPoS v1 checkpoint
# layout (ExtraVanity=32B .. N*20B masternode addresses .. ExtraSeal=65B),
# see ../../XDPoSChain/consensus/XDPoS/engines/engine_v1/{engine.go,utils.go}.
_s06_checkpoint_masternodes() {
  local url="$1" n="$2" hexn block extra
  hexn=$(printf '0x%x' "$n")
  block=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]") || return 1
  extra=$(python3 -c "
import json
import sys

b = json.loads(sys.argv[1])
print((b or {}).get('extraData', ''))
" "$block") || return 1
  [ -n "$extra" ] || { echo "_s06_checkpoint_masternodes: no extraData for block $n at $url" >&2; return 1; }
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

# _s06_lc <string>: lowercases (for case-insensitive, order-preserving
# comma-list equality between independently-decoded client responses).
_s06_lc() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

scen_06() {
  local clients c ref rc
  local max_validators candidate_count_before i
  local addr_var key_var addr key
  local ref_list ref_len f fu ck fk

  export LAB_SWITCH_BLOCK="$_S06_SWITCH_BLOCK"
  export LAB_PERIOD="$_S06_PERIOD"

  echo "scen_06: starting producer (switch=$LAB_SWITCH_BLOCK period=$LAB_PERIOD)"
  lab_start_producer real || { echo "scen_06: lab_start_producer failed"; return 1; }

  clients=$(lab_clients)
  for c in $clients; do
    echo "scen_06: starting follower $c"
    lab_start_follower "$c" || { echo "scen_06: lab_start_follower $c failed"; return 1; }
  done

  ref=$(lab_rpc oldxdc)

  [ -f "$LAB_DIR/lab-accounts.env" ] \
    || { echo "SKIP:lab-accounts.env not found (gen-lab-genesis.sh did not run / produce candidate accounts)"; return 2; }
  # shellcheck source=../lab-accounts.env
  source "$LAB_DIR/lab-accounts.env"
  : "${LAB_CANDIDATE_COUNT:=0}"

  max_validators=$("$LAB_DIR/masternode.sh" max-validators --rpc "$ref") \
    || { echo "scen_06: could not read maxValidatorNumber"; return 1; }
  echo "scen_06: maxValidatorNumber=$max_validators, lab candidate accounts=$LAB_CANDIDATE_COUNT"

  if [ "$LAB_CANDIDATE_COUNT" -le "$max_validators" ]; then
    echo "SKIP:need more candidate-owner accounts (${LAB_CANDIDATE_COUNT}) than maxValidatorNumber (${max_validators}) to force overflow/tie-break"
    return 2
  fi

  echo "scen_06: uploadKYC + propose (tied at ${_S06_PROPOSE_VALUE} XDC each) for $LAB_CANDIDATE_COUNT candidate accounts"
  i=1
  while [ "$i" -le "$LAB_CANDIDATE_COUNT" ]; do
    addr_var="LAB_CANDIDATE_${i}_ADDR"
    key_var="LAB_CANDIDATE_${i}_KEY"
    addr="${!addr_var:-}"
    key="${!key_var:-}"
    if [ -z "$addr" ] || [ -z "$key" ]; then
      echo "scen_06: missing address/key for candidate $i in lab-accounts.env"
      return 1
    fi

    if [ "$("$LAB_DIR/masternode.sh" is-candidate "$addr" --rpc "$ref")" = "true" ]; then
      echo "scen_06: candidate $i ($addr) already registered, skipping propose (idempotent re-run)"
      i=$((i + 1))
      continue
    fi

    "$LAB_DIR/masternode.sh" upload-kyc "lab-06-cand-$i" --key "$key" --from "$addr" --rpc "$ref" >/dev/null \
      || { echo "scen_06: uploadKYC failed for candidate $i ($addr)"; return 1; }
    "$LAB_DIR/masternode.sh" propose "$addr" --key "$key" --from "$addr" --rpc "$ref" --value "$_S06_PROPOSE_VALUE" >/dev/null \
      || { echo "scen_06: propose failed for candidate $i ($addr)"; return 1; }
    i=$((i + 1))
  done

  echo "scen_06: waiting for a few blocks for all propose txs to be mined"
  candidate_count_before=$("$LAB_DIR/masternode.sh" candidate-count --rpc "$ref") \
    || { echo "scen_06: could not read candidateCount"; return 1; }
  echo "scen_06: candidateCount now $candidate_count_before"

  echo "scen_06: waiting for a few blocks before the gap ($_S06_GAP_BLOCK - 10), to give the propose txs margin to land"
  lab_wait_block "$ref" $((_S06_GAP_BLOCK - 10)) 600 \
    || { echo "scen_06: producer stalled before the gap block"; return 1; }

  echo "scen_06: waiting for reference to settle past checkpoint $_S06_EPOCH_900 (to $_S06_SETTLE_TO)"
  lab_wait_block "$ref" "$_S06_SETTLE_TO" 1800 \
    || { echo "scen_06: reference did not settle past checkpoint $_S06_EPOCH_900"; return 1; }

  ref_list=$(_s06_checkpoint_masternodes "$ref" "$_S06_EPOCH_900") \
    || { echo "scen_06: could not decode reference checkpoint $_S06_EPOCH_900"; return 1; }
  ref_len=$(printf '%s' "$ref_list" | tr ',' '\n' | grep -c '^0x')
  echo "scen_06: reference checkpoint $_S06_EPOCH_900 masternode count=$ref_len (want <= $max_validators)"
  if [ "$ref_len" -gt "$max_validators" ]; then
    echo "scen_06: FAIL - checkpoint $_S06_EPOCH_900 has $ref_len masternodes, more than maxValidatorNumber ($max_validators): cap overflow"
    return 1
  fi
  if [ "$ref_len" -lt "$max_validators" ]; then
    echo "scen_06: FAIL - checkpoint $_S06_EPOCH_900 has only $ref_len masternodes, expected exactly $max_validators (>= $LAB_CANDIDATE_COUNT tied candidates were proposed, should fill the cap)"
    return 1
  fi

  for f in $clients; do
    fu=$(lab_rpc "$f") || { echo "scen_06: unknown client $f"; return 1; }
    ck=$(_s06_checkpoint_masternodes "$fu" "$_S06_EPOCH_900" 2>/dev/null) || ck=""
    if [ -z "$ck" ]; then
      echo "scen_06: FAIL - could not decode checkpoint $_S06_EPOCH_900 from follower $f (missing/unreachable at that height)"
      return 1
    fi
    fk=$(_s06_lc "$ck")
    if [ "$fk" != "$(_s06_lc "$ref_list")" ]; then
      echo "scen_06: FAIL - follower $f's checkpoint $_S06_EPOCH_900 masternode order/set differs from reference (sort-stability / top-N mismatch)"
      echo "  reference: $ref_list"
      echo "  follower ($f): $ck"
      return 1
    fi
    echo "scen_06: follower $f matches reference top-$max_validators order at checkpoint $_S06_EPOCH_900"
  done

  echo "scen_06: running oracle across the propose/checkpoint range"
  "$LAB_DIR/oracle.sh" --from 0 --to "$_S06_SETTLE_TO" \
    --clients "$(printf '%s' "$clients" | tr ' ' ',')" \
    --ref "$ref" --interval-blocks 30 --timeout-per-block 120
  rc=$?

  if [ "$rc" -eq 0 ]; then
    echo "scen_06: PASS - all clients picked the identical top-$max_validators set in identical order at checkpoint $_S06_EPOCH_900"
    return 0
  fi
  echo "scen_06: FAIL - divergence reported above (client + field identify the culprit)"
  return 1
}
