#!/bin/bash
# 05-v1v2-switch-pending-lifecycle.sh - the v1->v2 engine handoff must carry
# a pending masternode-set change across the switch boundary as one
# consistent set (docs/lab/DESIGN.md §8, row 05).
#
#   switch  : LAB_SWITCH_BLOCK=1800 (the last v1 block; 1801 is the first v2
#             block -- docs/lab/DESIGN.md §5)
#   period  : LAB_PERIOD=1
#   trigger : uploadKYC+propose a fresh candidate and resign an existing one,
#             both mined and committed before gap block 1350 (= 1800 - gap
#             450; epoch=900/gap=450 are fixed per docs/lab/DESIGN.md §5/F1),
#             so the set change lands exactly at the v1/v2 switch boundary
#             (1800) instead of a full epoch later.
#   expected: the first v2 masternode set (post-1800) equals the set the v1
#             engine computed at gap block 1350; block 1801 is accepted by
#             every follower.
#   classify-on-fail: engine-handoff set mismatch (v1's pending computation
#             at the gap block didn't carry cleanly into the v2 engine that
#             takes over at the switch).
#
# Drive uses masternode.sh (0x88 propose/resign + uploadKYC -- finding F2:
# propose() reverts for an owner that has never uploadKYC'd). Observe uses
# oracle.sh (universal eth_getBlockByNumber/eth_call checks only, per
# docs/lab/DESIGN.md §3 -- never XDPoS_*).
#
# The "existing validator to resign" is discovered live via eth_accounts on
# the reference (its own genesis-sealer wallet -- run.sh unlocks it on
# itself), rather than hardcoded, since keystores are regenerated per lab
# run. This assumes that account is also its own candidate owner (the usual
# self-owned genesis-masternode convention); if that assumption is wrong the
# resign() call will simply not take effect, which
# _s05_wait_candidate_state below catches explicitly rather than assuming
# success.

# SCEN_DESC/SCEN_PRIORITY/SCEN_MODE are read by scenario.sh after sourcing
# this file (docs/lab/DESIGN.md §7's scenario.sh contract) -- not dead code.
# shellcheck disable=SC2034
SCEN_DESC="v1->v2 switch: propose+resign straddling gap 1350 must hand off to v2 as one consistent set at switch 1800"
SCEN_PRIORITY="P0"
SCEN_PROFILE="real"
SCEN_MODE="OBSERVE"

# _s05_wait_candidate_state <ref> <addr> <want_present:0|1> <timeout_s>:
# polls lab_get_candidates(ref) until $addr's presence matches $want_present
# (1=must be present, 0=must be absent), or returns nonzero after
# timeout_s. Confirms a propose/resign tx actually took effect on-chain
# instead of assuming the eth_sendTransaction call alone means it landed.
_s05_wait_candidate_state() {
  local ref="$1" addr="$2" want_present="$3" timeout="$4" waited=0 set_str present
  addr=$(printf '%s' "$addr" | tr '[:upper:]' '[:lower:]')
  while :; do
    set_str=$(lab_get_candidates "$ref" 2>/dev/null) || set_str=""
    case ",$set_str," in
      *",$addr,"*) present=1 ;;
      *)           present=0 ;;
    esac
    [ "$present" = "$want_present" ] && return 0
    [ "$waited" -ge "$timeout" ] && return 1
    sleep 2
    waited=$((waited + 2))
  done
}

scen_05() {
  export LAB_SWITCH_BLOCK=1800
  export LAB_PERIOD=1

  local gap=450 gap_block first_v2_block ref accounts_env
  gap_block=$((LAB_SWITCH_BLOCK - gap))
  first_v2_block=$((LAB_SWITCH_BLOCK + 1))

  lab_start_producer "$SCEN_PROFILE" || { echo "scen_05: FAIL - producer failed to start"; return 1; }
  ref=$(lab_rpc oldxdc)

  local c started_clients=""
  for c in $(lab_clients); do
    if lab_start_follower "$c"; then
      started_clients="${started_clients:+$started_clients,}$c"
    else
      echo "scen_05: warning - follower $c failed to start, excluding it from this run" >&2
    fi
  done
  [ -n "$started_clients" ] || { echo "scen_05: FAIL - no follower could be started (need at least one for parity checks)"; return 1; }

  # ---- drive: propose a fresh candidate, resign an existing masternode,
  # both before gap block 1350 ----
  accounts_env="$LAB_DIR/lab-accounts.env"
  [ -f "$accounts_env" ] || { echo "scen_05: FAIL - $accounts_env missing (expected gen-lab-genesis.sh to have written it via lab_start_producer)"; return 1; }
  # shellcheck disable=SC1090
  source "$accounts_env"
  [ -n "${LAB_CANDIDATE_1_ADDR:-}" ] && [ -n "${LAB_CANDIDATE_1_KEY:-}" ] \
    || { echo "scen_05: FAIL - no candidate-owner account (LAB_CANDIDATE_1_*) in $accounts_env"; return 1; }

  local incoming="$LAB_CANDIDATE_1_ADDR" incoming_key="$LAB_CANDIDATE_1_KEY"
  local outgoing_json outgoing
  outgoing_json=$(lab_rpc_call "$ref" eth_accounts "[]") || { echo "scen_05: FAIL - eth_accounts failed on reference"; return 1; }
  outgoing=$(python3 -c "
import json
import sys

a = json.loads(sys.argv[1])
print(a[0] if a else '')
" "$outgoing_json")
  outgoing=$(printf '%s' "$outgoing" | tr '[:upper:]' '[:lower:]')
  [ -n "$outgoing" ] || { echo "scen_05: FAIL - reference has no unlocked account to resign"; return 1; }

  "$LAB_DIR/masternode.sh" upload-kyc --rpc "$ref" --key "$incoming_key" --from "$incoming" \
    || { echo "scen_05: FAIL - uploadKYC failed for incoming candidate $incoming"; return 1; }
  "$LAB_DIR/masternode.sh" propose "$incoming" --rpc "$ref" --key "$incoming_key" --from "$incoming" \
    || { echo "scen_05: FAIL - propose($incoming) failed"; return 1; }
  "$LAB_DIR/masternode.sh" resign "$outgoing" --rpc "$ref" --from "$outgoing" \
    || { echo "scen_05: FAIL - resign($outgoing) failed"; return 1; }

  _s05_wait_candidate_state "$ref" "$incoming" 1 120 \
    || { echo "scen_05: FAIL - propose($incoming) never reflected in getCandidates()"; return 1; }
  _s05_wait_candidate_state "$ref" "$outgoing" 0 120 \
    || { echo "scen_05: FAIL - resign($outgoing) never reflected in getCandidates() (self-owned-masternode assumption may be wrong -- see header note)"; return 1; }

  local height_after_drive
  height_after_drive=$(lab_block_number "$ref") || height_after_drive=0
  if [ "$height_after_drive" -ge "$gap_block" ]; then
    echo "scen_05: FAIL - propose/resign confirmed too late (already at block $height_after_drive, gap block $gap_block already passed)"
    return 1
  fi

  lab_wait_block "$ref" "$gap_block" $((gap_block * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_05: FAIL - reference never reached gap block $gap_block"; return 1; }

  # ---- capture: the set the v1 engine computes at the gap block ----
  local gap_set
  gap_set=$(lab_get_candidates "$ref") || { echo "scen_05: FAIL - getCandidates() failed at the gap block"; return 1; }

  lab_wait_block "$ref" $((first_v2_block + 5)) $(((first_v2_block + 5) * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_05: FAIL - reference never reached $((first_v2_block + 5)) (past the v1/v2 switch)"; return 1; }

  # ---- observe: parity across the switch boundary ----
  local oracle_out oracle_rc
  oracle_out=$("$LAB_DIR/oracle.sh" --from $((LAB_SWITCH_BLOCK - 5)) --to $((first_v2_block + 5)) \
    --clients "$started_clients")
  oracle_rc=$?
  printf '%s\n' "$oracle_out"

  if [ "$oracle_rc" -ne 0 ]; then
    echo "scen_05: FAIL - engine-handoff set mismatch (oracle reported a divergence across the switch boundary, see DIVERGENCE line above)"
    return 1
  fi

  local handoff_line
  handoff_line=$(printf '%s\n' "$oracle_out" | grep -m1 "^H=${first_v2_block} ")
  if printf '%s' "$handoff_line" | grep -qE 'MISS|DIFF'; then
    echo "scen_05: FAIL - engine-handoff set mismatch (block $first_v2_block not accepted identically by all followers: $handoff_line)"
    return 1
  fi

  # ---- verdict: first v2 set must equal the v1-computed set at the gap ----
  local v2_set
  v2_set=$(lab_get_candidates "$ref") || { echo "scen_05: FAIL - getCandidates() failed post-switch"; return 1; }
  if [ "$v2_set" != "$gap_set" ]; then
    echo "scen_05: FAIL - engine-handoff set mismatch (gap-$gap_block set [$gap_set] != post-switch set [$v2_set])"
    return 1
  fi

  echo "scen_05: PASS - v1-computed set at gap $gap_block carried into v2 unchanged at switch $LAB_SWITCH_BLOCK; block $first_v2_block accepted by all"
  return 0
}
