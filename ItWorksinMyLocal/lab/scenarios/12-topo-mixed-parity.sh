#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 12-topo-mixed-parity.sh - framework-based mixed-5 topology scenario
# (ItWorksinMyLocal#46 T2.4): brings up topologies/mixed-5.json (4 oldxdc
# sealers + 1 modern-geth follower) via the netlab framework (topo_up,
# lab/lib-topo.sh), then asserts genesis parity, sealer block production,
# follower lockstep/survival past the epoch-900 boundary, and multi-way
# hash agreement at a handful of checkpoints -- using ONLY lib-assert.sh's
# named assertions, no scenario-local ad hoc checks. Unlike scenario 11
# (which predates lib-topo.sh/lib-assert.sh and drives run.sh --mixed
# directly, kept untouched as the regression baseline), this scenario
# exercises the declarative topology engine end to end: topo.sh validate,
# fleet.sh's staggered/health-gated bring-up, and graceful topo_down
# teardown (handled generically by scenario.sh's per-run teardown, not by
# this file -- see lib-topo.sh's header).
#
# Expected result: an HONEST FAIL. Modern-geth's V1 XDPoS difficulty
# verifier (consensus/XDPoS/engines/engine_v1/engine.go calcDifficulty;
# see scenario 11's own note and the ItWorksinMyLocal#34 writeup)
# permanently stalls node5's xdcSyncer a few blocks in, well short of the
# epoch-900 checkpoint. That is the CORRECT and expected outcome here: this
# scenario exists to prove the netlab framework itself -- topo_up
# bring-up, health-gated staggered start, graceful topo_down teardown --
# runs end to end against a real declarative topology, not to mask node5's
# known stall. A PASS would actually be suspicious here (it would mean
# either the modern-geth binary got fixed -- update this comment and #34 --
# or an assertion silently isn't exercising the real sync path).
#
# Gate-review addendum (ItWorksinMyLocal#46 re-run, post gate-flagged
# masked-pass finding): topologies/mixed-5.json used to set
# skipV1Validation:true -- forbidden by docs/lab/DESIGN.md and now a hard
# error in topo.sh validate (see netlab/topo.sh) -- which suppressed the
# #34 stall at the genesis-config level. With that removed, node5 does
# NOT actually halt: its xdcSyncer detects the exact #34 gas-cost mismatch
# on real blocks and logs "BlockChain: A.94 auto-recover on ValidateState
# mismatch during bulk sync ... invalid gas used" -- then silently
# recovers and keeps going, still ending up hash-agreeing with the
# producer at every later checkpoint. A verdict built only on
# assert_lockstep/assert_hash_agreement would call that a clean PASS; it
# would be wrong, because the follower demonstrably did NOT validate that
# block the way the producer did -- it detected the divergence and
# swallowed it. assert_no_validation_bypass (lab/lib-assert.sh) makes that
# swallowed mismatch part of the verdict instead of a footnote a human has
# to grep node5/node.log for: any occurrence FAILs the run regardless of
# what the final hashes say. Given this, the honest expectation for this
# scenario stays a FAIL -- either the "stalls a few blocks in" kind (if a
# future modern-geth build stops auto-recovering and genuinely halts
# instead) or the "auto-recovered instead of rejecting" kind this run
# actually hits. A PASS is still suspicious for exactly the reason above:
# it would mean node5 validated the v1 blocksigner tx correctly with no
# auto-recover log line anywhere, i.e. the underlying #34 mismatch is
# actually gone -- update this comment and #34 if that is ever what
# happens.

export SCEN_DESC="topo-mixed-parity: mixed-5 via the netlab framework (topo_up/lib-assert) -- honest FAIL expected against modern-geth's known V1 stall / auto-recover masking (#34)"
export SCEN_PRIORITY="P1"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S12_TOPO="$REPO_ROOT/topologies/mixed-5.json"
_S12_EPOCH=900
_S12_MARGIN=20
_S12_TARGET=920   # > epoch -- past the checkpoint boundary

scen_12() {
  # Belt-and-braces: topo_down/fleet wipe are both idempotent, but make
  # sure we start from a clean nodes/mixed-5/ regardless of any prior run's
  # leftovers (scenario.sh's generic teardown already ran topo_down before
  # sourcing this file, but a previous --wipe-less run could have left
  # state behind under a DIFFERENT invocation of the harness entirely).
  ( cd "$REPO_ROOT" && ./netlab/fleet.sh wipe "$_S12_TOPO" --force >/dev/null 2>&1 ) || true

  echo "scen_12: bringing up mixed-5 via topo_up --wipe ..." >&2
  topo_up "$_S12_TOPO" --wipe >&2
  local up_rc=$?
  if [ "$up_rc" -ne 0 ]; then
    echo "scen_12: FAIL - topo_up mixed-5 failed (rc=$up_rc) -- see fleet.sh output above"
    return 1
  fi

  local node1 node5 node5_log
  node1=$(topo_rpc "$_S12_TOPO" node1) || { echo "scen_12: FAIL - could not resolve node1 rpc"; return 1; }
  node5=$(topo_rpc "$_S12_TOPO" node5) || { echo "scen_12: FAIL - could not resolve node5 rpc"; return 1; }
  node5_log=$(topo_log "$_S12_TOPO" node5) || { echo "scen_12: FAIL - could not resolve node5 log path"; return 1; }
  echo "scen_12: node1(sealer, reference)=$node1 node5(modern-geth follower)=$node5 node5_log=$node5_log" >&2

  # ---- 1. genesis parity across every resolved node in the topology ----
  # fleet.sh already generates ONE genesis for every node, but this is
  # exactly what actually gets a node rejected at handshake if it ever
  # diverges -- assert it explicitly, and first, per lib-assert.sh's own
  # doc on assert_genesis_parity.
  local ids id pairs=() url
  ids=$(topo_nodes "$_S12_TOPO") || { echo "scen_12: FAIL - topo_nodes failed"; return 1; }
  for id in $ids; do
    url=$(topo_rpc "$_S12_TOPO" "$id") || { echo "scen_12: FAIL - could not resolve rpc for $id"; return 1; }
    pairs+=("$id=$url")
  done
  assert_genesis_parity "${pairs[@]}" \
    || { echo "scen_12: FAIL - genesis parity assertion failed (see PASS/FAIL line above)"; return 1; }

  # ---- 2. the 4 sealers actually produce blocks ----
  # node1 is as good a witness as any -- all 4 sealers are sealing the same
  # chain through the shared bootnode.
  echo "scen_12: waiting for node1 to reach block $_S12_TARGET (sealers producing) ..." >&2
  lab_wait_block "$node1" "$_S12_TARGET" $((_S12_TARGET * 2 + 300)) \
    || { echo "scen_12: FAIL - node1 (sealer) never reached block $_S12_TARGET -- sealers are not producing"; return 1; }
  echo "scen_12: node1 head=$(lab_block_number "$node1")" >&2

  # ---- 3. node5 survival past the epoch boundary ----
  # THIS is expected to FAIL (see the file header) -- node5's V1
  # difficulty verifier stalls its xdcSyncer a few blocks in, well before
  # it ever reaches epoch+margin.
  assert_survives_epoch "node5=$node5" "$_S12_EPOCH" "$_S12_MARGIN" $((_S12_TARGET * 3 + 300))
  local survive_rc=$?
  if [ "$survive_rc" -ne 0 ]; then
    assert_no_validation_bypass "node5" "$node5_log" >&2 || true
    echo "scen_12: FAIL (EXPECTED) - node5 did not survive past epoch $_S12_EPOCH -- known modern-geth V1 difficulty stall (#34); this is the harness working correctly, not a framework bug"
    return 1
  fi

  # node5 reached the checkpoint -- but reaching it is NOT the same as
  # having validated every block honestly along the way (see the file
  # header's gate-review addendum). Check node5's own log for a swallowed
  # ValidateState mismatch BEFORE trusting the hash-based assertions
  # below: a follower that auto-recovered from a genuine divergence and
  # then hash-agrees at every checkpoint anyway is exactly the masked-pass
  # failure mode this file exists to not reproduce.
  local cp all_ok=0
  assert_no_validation_bypass "node5" "$node5_log" || all_ok=1
  for cp in 1 5 50 "$_S12_EPOCH" $((_S12_EPOCH + 1)) "$_S12_TARGET"; do
    assert_lockstep "node1=$node1" "node5=$node5" "$cp" 60 || all_ok=1
    assert_hash_agreement "$cp" "node1=$node1" "node5=$node5" || all_ok=1
  done
  [ "$all_ok" -eq 0 ] \
    || { echo "scen_12: FAIL - lockstep/hash-agreement/validation-bypass check failed at one or more checkpoints (see PASS/FAIL lines above)"; return 1; }

  echo "scen_12: PASS - mixed-5 topology fully synced in lockstep past epoch $_S12_EPOCH with no swallowed validation mismatch (unexpected but genuine: update the #34 stall note)"
  return 0
}
