#!/bin/bash
# 08-doublesign-forensics.sh - double-sign/equivocation forensics: a v1 case
# (two blocks signed by the same masternode at the same height, pre-switch)
# and a v2 case (two votes/proposals for the same round, post-switch) must
# be rejected/converged on identically by every follower
# (docs/lab/DESIGN.md §8 row 08; adviser catalog 4.3). MODE=INJECT.
#
#   switch  : LAB_SWITCH_BLOCK=900
#   period  : LAB_PERIOD=1
#   trigger : inject two competing blocks/votes at the same height/round --
#             once pre-900 (v1) and once post-901 (v2) -- gossiped to
#             different halves of the client set.
#   expected: all followers reject/converge identically (v2: forensics.go
#             detects the equivocation and consensus continues on the QC'd
#             branch; v1: all followers pick the same canonical block).
#   classify-on-fail: first-seen-wins fork choice (a client that kept
#             whichever competing block it happened to see first, instead
#             of the canonical v1/v2 selection rule).
#
# This requires an actual double-signing producer (a patched oldxdc/geth
# miner, or a hand-crafted-RLP helper -- docs/lab/DESIGN.md §6) that this
# repo does not ship. Per the lab's INJECT contract, that capability is
# checked FIRST, before any network is started, and the scenario SKIPs
# (never silently PASSes) when it's unavailable -- which is the only
# reachable path today.
#
# If/when such a tool exists, point LAB_DOUBLESIGN_BIN at its executable,
# or drop one at lab/inject/doublesign-producer. The (documented, not yet
# implemented) contract this scenario drives it with:
#   "$bin" v1 --rpc <reference-rpc-url> --at <height>
#   "$bin" v2 --rpc <reference-rpc-url> --at <round>
# each call injecting two competing blocks/votes at <height>/<round>,
# gossiping them to different halves of the running client set, and
# returning once both are injected (the tool owns how it reaches the
# network and how it splits the gossip -- this scenario only supplies where
# and when).

# SCEN_DESC/SCEN_PRIORITY/SCEN_MODE are read by scenario.sh after sourcing
# this file (docs/lab/DESIGN.md §7's scenario.sh contract) -- not dead code.
# shellcheck disable=SC2034
SCEN_DESC="double-sign/equivocation forensics: v1 pre-900 + v2 post-901 same-height/round injection (INJECT, SKIPs without a patched producer)"
SCEN_PRIORITY="P0"
SCEN_PROFILE="real"
SCEN_MODE="INJECT"

# _s08_doublesign_bin: echoes the path to a usable double-sign injector, or
# returns nonzero if none is configured/found. Checked before any network is
# started so an unavailable capability SKIPs cheaply.
_s08_doublesign_bin() {
  if [ -n "${LAB_DOUBLESIGN_BIN:-}" ] && [ -x "${LAB_DOUBLESIGN_BIN}" ]; then
    printf '%s\n' "$LAB_DOUBLESIGN_BIN"
    return 0
  fi
  local conventional="$LAB_DIR/inject/doublesign-producer"
  if [ -x "$conventional" ]; then
    printf '%s\n' "$conventional"
    return 0
  fi
  return 1
}

# _s08_inject_and_check <bin> <label> <mode> <n> <window_from> <window_to>
#   <clients>: runs one injection ($mode=v1|v2 at height/round $n against
# the reference), then checks convergence with oracle.sh over
# [$window_from..$window_to] across the comma-joined <clients>. Returns 0 on
# convergence; on FAIL, echoes a single reason line (the last line of
# output, per scenario.sh's summary-reason contract) and returns 1.
_s08_inject_and_check() {
  local bin="$1" label="$2" mode="$3" n="$4" window_from="$5" window_to="$6" clients="$7"
  local ref oracle_out oracle_rc
  ref=$(lab_rpc oldxdc)

  "$bin" "$mode" --rpc "$ref" --at "$n" \
    || { echo "scen_08: FAIL - first-seen-wins fork choice ($label injection via $bin failed/errored)"; return 1; }

  lab_wait_block "$ref" "$window_to" $((window_to * LAB_PERIOD * 2 + 300)) \
    || { echo "scen_08: FAIL - first-seen-wins fork choice (reference never reached $window_to after $label injection)"; return 1; }

  oracle_out=$("$LAB_DIR/oracle.sh" --from "$window_from" --to "$window_to" --clients "$clients")
  oracle_rc=$?
  printf '%s\n' "$oracle_out"

  if [ "$oracle_rc" -ne 0 ]; then
    echo "scen_08: FAIL - first-seen-wins fork choice ($label injection caused a divergence, see DIVERGENCE line above)"
    return 1
  fi
  return 0
}

scen_08() {
  local bin
  if ! bin=$(_s08_doublesign_bin); then
    echo "SKIP:no double-sign/equivocation injection binary available (set LAB_DOUBLESIGN_BIN or provide an executable at $LAB_DIR/inject/doublesign-producer -- docs/lab/DESIGN.md §6 requires a patched oldxdc/geth miner or hand-crafted-RLP helper that this repo does not ship)"
    return 2
  fi

  export LAB_SWITCH_BLOCK=900
  export LAB_PERIOD=1

  lab_start_producer "$SCEN_PROFILE" || { echo "scen_08: FAIL - producer failed to start"; return 1; }

  local ref
  ref=$(lab_rpc oldxdc)

  local c started_clients=""
  for c in $(lab_clients); do
    if lab_start_follower "$c"; then
      started_clients="${started_clients:+$started_clients,}$c"
    else
      echo "scen_08: warning - follower $c failed to start, excluding it from this run" >&2
    fi
  done
  [ -n "$started_clients" ] || { echo "scen_08: FAIL - no follower could be started (need at least one for parity checks)"; return 1; }

  # ---- v1 case: double-sign well before the switch ----
  lab_wait_block "$ref" 50 200 || { echo "scen_08: FAIL - reference never reached block 50 (v1 injection window)"; return 1; }
  _s08_inject_and_check "$bin" "v1 pre-switch" v1 60 50 70 "$started_clients" || return 1

  # ---- v2 case: double-sign/equivocation once safely past the switch ----
  local v2_at=$((LAB_SWITCH_BLOCK + 20))
  lab_wait_block "$ref" $((LAB_SWITCH_BLOCK + 10)) $(( (LAB_SWITCH_BLOCK + 10) * LAB_PERIOD * 2 + 300 )) \
    || { echo "scen_08: FAIL - reference never reached v2 (block $((LAB_SWITCH_BLOCK + 10)))"; return 1; }
  _s08_inject_and_check "$bin" "v2 post-switch" v2 "$v2_at" $((LAB_SWITCH_BLOCK + 10)) $((LAB_SWITCH_BLOCK + 30)) "$started_clients" || return 1

  echo "scen_08: PASS - v1 and v2 double-sign/equivocation injections converged identically across all followers"
  return 0
}
