#!/bin/bash
# 01-diff-oracle-baseline.sh - the lab's smoke test (docs/lab/DESIGN.md §8,
# row 01). Brings up the oldxdc producer + every configured follower on the
# default v1->v2 timing (switchBlock=900, i.e. v2 begins at block 901),
# lets everyone sync from genesis past the switch, then runs the
# differential oracle (oracle.sh) over the whole range and requires every
# follower to match the reference (oldxdc node1) on both block hash and
# decoded getCandidates() at every block.
#
# No fault injection here -- this is the "does the harness even work, and
# do the ports/basics line up" baseline that every other scenario builds on.
# PASS = oracle.sh reports "PARITY OK" for 0..~950. FAIL = any divergence
# (oracle.sh already reports which client + which field on first mismatch).

export SCEN_DESC="diff-oracle-baseline: sync 0->~950 across the v1->v2 switch, full parity every block"
export SCEN_PRIORITY="P0"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

# Target height: comfortably past LAB_SWITCH_BLOCK (900) so the run crosses
# into v2 (first v2 block is switchBlock+1 = 901), per docs/lab/DESIGN.md §5.
_S01_SWITCH_BLOCK=900
_S01_PERIOD=1
_S01_TO=950

scen_01() {
  local clients c ref rc

  export LAB_SWITCH_BLOCK="$_S01_SWITCH_BLOCK"
  export LAB_PERIOD="$_S01_PERIOD"

  echo "scen_01: starting producer (switch=$LAB_SWITCH_BLOCK period=$LAB_PERIOD)"
  lab_start_producer real || { echo "scen_01: lab_start_producer failed"; return 1; }

  clients=$(lab_clients)
  for c in $clients; do
    echo "scen_01: starting follower $c"
    lab_start_follower "$c" || { echo "scen_01: lab_start_follower $c failed"; return 1; }
  done

  ref=$(lab_rpc oldxdc)
  echo "scen_01: waiting for reference $ref to reach block $_S01_TO (period=${LAB_PERIOD}s -> ~${_S01_TO}s minimum)"
  lab_wait_block "$ref" "$_S01_TO" 1800 \
    || { echo "scen_01: reference did not reach block $_S01_TO within timeout"; return 1; }

  echo "scen_01: running oracle 0..$_S01_TO across [$clients] (every block)"
  "$LAB_DIR/oracle.sh" --from 0 --to "$_S01_TO" \
    --clients "$(printf '%s' "$clients" | tr ' ' ',')" \
    --ref "$ref" --timeout-per-block 120
  rc=$?

  if [ "$rc" -eq 0 ]; then
    echo "scen_01: PASS - all followers matched reference 0..$_S01_TO (v1->v2 switch at $LAB_SWITCH_BLOCK crossed cleanly)"
    return 0
  fi
  echo "scen_01: FAIL - divergence reported above (client + field identify the culprit)"
  return 1
}
