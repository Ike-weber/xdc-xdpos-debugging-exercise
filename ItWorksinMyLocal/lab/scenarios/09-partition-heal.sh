#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 09-partition-heal.sh - a sustained post-switch partition must not corrupt
# consensus: whichever side(s) can make progress do so cleanly, and on heal
# every follower converges on one identical head (docs/lab/DESIGN.md §8 row
# 09; adviser catalog 6.1, adapted from a 6-client 3+3 split down to the
# lab's 4 producer sealers).
#
#   switch  : LAB_SWITCH_BLOCK=900
#   period  : LAB_PERIOD=1
#   trigger : lab_partition the 4 producer sealers 2/2 for ~1-1.5 epochs
#             (epoch is fixed at 900 -- docs/lab/DESIGN.md §5/F1) once past
#             the switch, then lab_heal.
#   expected: the minority partition (no quorum) halts; the majority (if it
#             has quorum) advances; on heal, every follower converges on the
#             identical head.
#   classify-on-fail: refuses reorg / stuck head (a client that keeps its
#             own partition-time view instead of reorging onto the
#             network's actual head once healed).
#
# NOTE: a clean 2/2 split of the 4 v2 masternodes is actually a TIE, not a
# minority/majority split -- with certificateThreshold=0.667 (genesis
# config), neither 2-node half reaches the ceil(4*0.667)=3-signature quorum,
# so in the strict case BOTH halves halt rather than one advancing. That
# doesn't weaken the test: the one thing this scenario can mechanically
# assert without live introspection into which side (if either) kept
# producing is exactly the post-heal invariant above -- one identical head,
# reached by every follower, with nothing left stuck. That's what
# "classify-on-fail: refuses reorg / stuck head" is checking for.

SCEN_DESC="post-switch 2/2 partition for ~1-1.5 epochs, heal, assert every follower converges on one identical head"
SCEN_PRIORITY="P0"
SCEN_PROFILE="real"
SCEN_MODE="OBSERVE"

scen_09() {
  export LAB_SWITCH_BLOCK=900
  export LAB_PERIOD=1

  local epoch=900 ref pre_split_height partition_seconds
  lab_start_producer "$SCEN_PROFILE" || { echo "scen_09: FAIL - producer failed to start"; return 1; }
  ref=$(lab_rpc oldxdc)

  local c started_clients=""
  for c in $(lab_clients); do
    if lab_start_follower "$c"; then
      started_clients="${started_clients:+$started_clients,}$c"
    else
      echo "scen_09: warning - follower $c failed to start, excluding it from this run" >&2
    fi
  done
  [ -n "$started_clients" ] || { echo "scen_09: FAIL - no follower could be started (need at least one for parity checks)"; return 1; }

  # ---- drive: reach v2, split 2/2, hold for ~1-1.5 epochs, heal ----
  pre_split_height=$((LAB_SWITCH_BLOCK + 5))
  lab_wait_block "$ref" "$pre_split_height" $((pre_split_height * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_09: FAIL - reference never reached post-switch height $pre_split_height"; return 1; }

  lab_partition "node1,node2" "node3,node4" \
    || { echo "scen_09: FAIL - partition failed"; return 1; }

  # ~1.5 epochs of wall-clock at the fixed epoch length (docs/lab/DESIGN.md
  # §5/F1), covering the "1-2 epochs" the design calls for.
  partition_seconds=$((epoch * 3 / 2 * LAB_PERIOD))
  sleep "$partition_seconds"

  lab_heal || { echo "scen_09: FAIL - heal failed"; return 1; }

  # ---- observe: the network must resume and every follower must converge
  # on one identical head; no client may stay stuck on its partition-time
  # view (refuses reorg) ----
  local post_heal_target
  post_heal_target=$((pre_split_height + 30))
  lab_wait_block "$ref" "$post_heal_target" $((partition_seconds + post_heal_target * LAB_PERIOD * 2 + 600)) \
    || { echo "scen_09: FAIL - refuses reorg / stuck head (reference never resumed to $post_heal_target after heal)"; return 1; }

  local oracle_out oracle_rc
  oracle_out=$("$LAB_DIR/oracle.sh" --from $((pre_split_height - 5)) --to "$post_heal_target" \
    --clients "$started_clients" --timeout-per-block $((partition_seconds / 4 + 120)))
  oracle_rc=$?
  printf '%s\n' "$oracle_out"

  if [ "$oracle_rc" -ne 0 ]; then
    echo "scen_09: FAIL - refuses reorg / stuck head (oracle reported a divergence after heal, see DIVERGENCE line above)"
    return 1
  fi

  local final_line
  final_line=$(printf '%s\n' "$oracle_out" | grep -m1 "^H=${post_heal_target} ")
  if printf '%s' "$final_line" | grep -qE 'MISS|DIFF'; then
    echo "scen_09: FAIL - refuses reorg / stuck head (not every follower converged by H=$post_heal_target: $final_line)"
    return 1
  fi

  echo "scen_09: PASS - post-switch 2/2 partition resolved cleanly; all followers converged on one head after heal"
  return 0
}
