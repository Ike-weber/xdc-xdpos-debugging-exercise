#!/bin/bash
# 07-competing-qc-fork-choice.sh - v2 fork choice must follow the branch
# with the higher QC round, not total-difficulty/longest-chain
# (docs/lab/DESIGN.md §8 row 07; adviser catalog 5.2).
#
#   switch  : LAB_SWITCH_BLOCK=900
#   period  : LAB_PERIOD=1
#   trigger : once past 901 (v2 active), split the 4 producer sealers 2/2
#             via lab_partition (admin_removePeer, docs/lab/DESIGN.md §6) so
#             each half can propose competing blocks at the same height/
#             round, then lab_heal.
#   expected: every follower ends up following the branch with the higher
#             QC round, not whichever branch is longer/heavier.
#   classify-on-fail: TD/longest-chain fork choice (a client that ported
#             Ethereum-default fork choice instead of v2's highest-QC-round
#             rule -- R2: this scenario drives the divergence with a real
#             partition, never by tuning timeoutPeriod).
#
# NOTE on quorum: with only 4 v2 masternodes total and certificateThreshold
# 0.667 (genesis config), a QC needs ceil(4*0.667)=3 signatures -- so a
# clean 2/2 split means neither half can finalize a QC alone. What each half
# *can* still do is PROPOSE a block for its own round (proposing needs no
# quorum, only finalizing does), so followers still see two competing
# proposals contending for the same height while the network is split. The
# assertion this scenario makes is the one thing that's mechanically
# checkable without live introspection into the round/QC state: after heal,
# every follower converges on an identical head with nothing left
# disagreeing or stuck (a client on the wrong fork-choice rule is the one
# likely to disagree, or to stall, once quorum resumes).

# SCEN_DESC/SCEN_PRIORITY/SCEN_MODE are read by scenario.sh after sourcing
# this file (docs/lab/DESIGN.md §7's scenario.sh contract) -- not dead code.
# shellcheck disable=SC2034
SCEN_DESC="post-switch v2: partition 2/2 to create competing same-height proposals, heal, assert all followers converge on the higher-QC-round branch"
SCEN_PRIORITY="P0"
SCEN_PROFILE="real"
SCEN_MODE="OBSERVE"

scen_07() {
  export LAB_SWITCH_BLOCK=900
  export LAB_PERIOD=1
  export LAB_TIMEOUT_PERIOD=10

  local ref pre_split_height post_heal_target
  lab_start_producer "$SCEN_PROFILE" || { echo "scen_07: FAIL - producer failed to start"; return 1; }
  ref=$(lab_rpc oldxdc)

  local c started_clients=""
  for c in $(lab_clients); do
    if lab_start_follower "$c"; then
      started_clients="${started_clients:+$started_clients,}$c"
    else
      echo "scen_07: warning - follower $c failed to start, excluding it from this run" >&2
    fi
  done
  [ -n "$started_clients" ] || { echo "scen_07: FAIL - no follower could be started (need at least one for parity checks)"; return 1; }

  # ---- drive: reach v2, split, let both halves contend, heal ----
  pre_split_height=$((LAB_SWITCH_BLOCK + 5))
  lab_wait_block "$ref" "$pre_split_height" $((pre_split_height * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_07: FAIL - reference never reached v2 height $pre_split_height"; return 1; }

  lab_partition "node1,node2" "node3,node4" \
    || { echo "scen_07: FAIL - partition failed"; return 1; }

  # Let both halves attempt several rounds (proposals, timeouts) while split.
  sleep $((LAB_TIMEOUT_PERIOD * 6))

  lab_heal || { echo "scen_07: FAIL - heal failed"; return 1; }

  # ---- observe: the network must resume and every follower must converge ----
  post_heal_target=$((pre_split_height + 20))
  lab_wait_block "$ref" "$post_heal_target" $((LAB_TIMEOUT_PERIOD * 20 + post_heal_target * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_07: FAIL - TD/longest-chain fork choice (reference never resumed to $post_heal_target after heal -- network may be stuck disagreeing on which branch to extend)"; return 1; }

  local oracle_out oracle_rc
  oracle_out=$("$LAB_DIR/oracle.sh" --from $((pre_split_height - 5)) --to "$post_heal_target" \
    --clients "$started_clients" --timeout-per-block $((LAB_TIMEOUT_PERIOD * 4 + 60)))
  oracle_rc=$?
  printf '%s\n' "$oracle_out"

  if [ "$oracle_rc" -ne 0 ]; then
    echo "scen_07: FAIL - TD/longest-chain fork choice (oracle reported a divergence after heal, see DIVERGENCE line above)"
    return 1
  fi

  local final_line
  final_line=$(printf '%s\n' "$oracle_out" | grep -m1 "^H=${post_heal_target} ")
  if printf '%s' "$final_line" | grep -qE 'MISS|DIFF'; then
    echo "scen_07: FAIL - TD/longest-chain fork choice (not every follower converged by H=$post_heal_target: $final_line)"
    return 1
  fi

  echo "scen_07: PASS - all followers converged on the same head after the post-switch 2/2 partition and heal"
  return 0
}
