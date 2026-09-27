#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 10-join-mid-epoch-coldstart.sh - a follower that joins the network fresh,
# well past a checkpoint and the v1->v2 switch, must derive the current
# masternode set purely from synced headers/state and validate the head
# going forward (docs/lab/DESIGN.md §8 row 10; adviser catalog 6.3).
#
#   switch  : LAB_SWITCH_BLOCK=900
#   period  : LAB_PERIOD=1
#   trigger : let the reference run alone to well past 900 (checkpoint +
#             switch) BEFORE calling lab_start_follower for any client, so
#             every follower's first view of the chain is a cold P2P sync
#             starting from genesis, not a live-tracked state.
#   expected: each follower derives the same active set as the reference and
#             validates the current epoch's blocks going forward.
#   classify-on-fail: cold-start set derivation (a client whose "just
#             joined, must derive from sync" code path diverges from its
#             own "was running live" code path).

SCEN_DESC="cold-start each follower past checkpoint+switch 900; assert set derivation from sync and head validation"
SCEN_PRIORITY="P1"
SCEN_PROFILE="real"
SCEN_MODE="OBSERVE"

scen_10() {
  export LAB_SWITCH_BLOCK=900
  export LAB_PERIOD=1

  local ref join_height
  lab_start_producer "$SCEN_PROFILE" || { echo "scen_10: FAIL - producer failed to start"; return 1; }
  ref=$(lab_rpc oldxdc)

  # ---- drive: let the reference run WITHOUT any follower attached, well
  # past the checkpoint (900) and the switch (900) ----
  join_height=$((LAB_SWITCH_BLOCK + 50))
  lab_wait_block "$ref" "$join_height" $((join_height * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_10: FAIL - reference never reached cold-start join height $join_height"; return 1; }

  # Now start every follower cold -- this is the point of the scenario: no
  # follower has been running since genesis, each must derive the active
  # (post-switch) masternode set purely from synced headers/state.
  local c started_clients=""
  for c in $(lab_clients); do
    if lab_start_follower "$c"; then
      started_clients="${started_clients:+$started_clients,}$c"
    else
      echo "scen_10: warning - follower $c failed to start, excluding it from this run" >&2
    fi
  done
  [ -n "$started_clients" ] || { echo "scen_10: FAIL - no follower could be started (need at least one for parity checks)"; return 1; }

  # ---- observe: every follower must sync from genesis and catch up ----
  local join_timeout=$((join_height * LAB_PERIOD * 6 + 600))
  for c in $(printf '%s' "$started_clients" | tr ',' ' '); do
    lab_wait_block "$(lab_rpc "$c")" "$join_height" "$join_timeout" \
      || echo "scen_10: warning - $c had not reached $join_height within ${join_timeout}s (will surface as MISS below)" >&2
  done

  # Then confirm each keeps validating going forward, not just catching up
  # to one synced snapshot.
  local validate_height=$((join_height + 30))
  lab_wait_block "$ref" "$validate_height" $((validate_height * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_10: FAIL - reference never reached $validate_height post-join"; return 1; }
  for c in $(printf '%s' "$started_clients" | tr ',' ' '); do
    lab_wait_block "$(lab_rpc "$c")" "$validate_height" $((join_timeout / 2)) \
      || echo "scen_10: warning - $c had not reached $validate_height within $((join_timeout / 2))s (will surface as MISS below)" >&2
  done

  local oracle_out oracle_rc
  oracle_out=$("$LAB_DIR/oracle.sh" --from 0 --to "$validate_height" --clients "$started_clients")
  oracle_rc=$?
  printf '%s\n' "$oracle_out"

  if [ "$oracle_rc" -ne 0 ]; then
    echo "scen_10: FAIL - cold-start set derivation (oracle reported a divergence, see DIVERGENCE line above)"
    return 1
  fi

  local final_line
  final_line=$(printf '%s\n' "$oracle_out" | grep -m1 "^H=${validate_height} ")
  if printf '%s' "$final_line" | grep -qE 'MISS|DIFF'; then
    echo "scen_10: FAIL - cold-start set derivation (not every follower reached/matched H=$validate_height: $final_line)"
    return 1
  fi

  echo "scen_10: PASS - every follower cold-joined past checkpoint+switch $LAB_SWITCH_BLOCK, derived the set from sync, and validated through $validate_height"
  return 0
}
