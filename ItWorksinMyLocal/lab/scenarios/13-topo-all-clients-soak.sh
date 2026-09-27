#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 13-topo-all-clients-soak.sh - all-clients cross-client bring-up + bounded
# soak (ItWorksinMyLocal#46 T3.3): brings up topologies/all-clients.json (4
# oldxdc sealers + every OTHER client that can join a custom-chainId genesis
# -- geth, erigon, besu, reth, xone; nethermind is excluded by the topology
# itself, per its own description and topo.sh's client=nethermind
# constraint -- it only loads its own bundled net5151 chainspec, not an
# arbitrary --genesis file, ItWorksinMyLocal#46 open question #2), then for
# EACH follower, independently, asserts:
#   - peers>=1, via lab_peer_count (net_peerCount) -- deliberately NOT
#     admin_peers: erigon's join.sh profile never enables the `admin` RPC
#     namespace, so an admin_peers-based check would misreport a perfectly
#     healthy erigon follower as peer-starved forever (see lib-rpc.sh's
#     lab_peer_count / netlab/health.sh's wait-peers, both fixed under this
#     same issue for exactly this reason).
#   - a MOVING TIP across one shared, bounded soak window (all 5 followers
#     syncing CONCURRENTLY against the same 4 sealers for the same window --
#     the whole point of this topology; running one follower at a time,
#     like scenario.sh --matrix would, can never reproduce a peer-starvation
#     regression that only shows up under concurrent load) -- not just "has
#     some block" once: a follower that synced 3 blocks and then silently
#     stalled must not read as a pass just because head>0.
#
# Verdict per follower is reported as its own matrix row (client | verdict |
# reason), written to lab/results/matrix-13.md via the same atomic
# scratch-then-rename pattern scenario.sh's own --matrix uses (never
# truncate-then-fill the committed file directly -- an interrupted run must
# leave the LAST GOOD matrix in place, not a corrupted partial one; this is
# the exact bug class ItWorksinMyLocal#46's gate review already fixed once
# in scenario.sh's _run_matrix). A follower that can't reach peers>=1, or
# whose head never advances during the soak window, is an HONEST per-client
# FAIL row -- this IS the peer-starvation regression this scenario exists
# to catch, not a framework bug; the scenario's own verdict is PASS only if
# every follower row PASSes, and any single FAIL row fails scen_13 as a
# whole (loud, not buried in a sub-table a human has to go find).

export SCEN_DESC="topo-all-clients-soak: all-clients.json (4 sealers + geth/erigon/besu/reth/xone) bounded concurrent soak -- per-client peers>=1 + moving-tip matrix; HONEST per-client FAIL on peer starvation or a stalled follower"
export SCEN_PRIORITY="P1"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S13_TOPO="$REPO_ROOT/topologies/all-clients.json"
_S13_SEALER_TARGET="${S13_SEALER_TARGET:-10}"     # sealers must reach this block first (something to sync from)
_S13_SEALER_TIMEOUT="${S13_SEALER_TIMEOUT:-180}"
_S13_PEER_TIMEOUT="${S13_PEER_TIMEOUT:-90}"       # per-follower peers>=1 gate, seconds
_S13_SOAK_SECONDS="${S13_SOAK_SECONDS:-120}"      # bounded shared soak window
_S13_MIN_ADVANCE="${S13_MIN_ADVANCE:-3}"          # min blocks a follower's head must gain over the window

scen_13() {
  ( cd "$REPO_ROOT" && ./netlab/fleet.sh wipe "$_S13_TOPO" --force >/dev/null 2>&1 ) || true

  echo "scen_13: bringing up all-clients via topo_up --wipe ..." >&2
  topo_up "$_S13_TOPO" --wipe >&2
  local up_rc=$?
  if [ "$up_rc" -ne 0 ]; then
    echo "scen_13: FAIL - topo_up all-clients failed (rc=$up_rc) -- see fleet.sh output above"
    return 1
  fi

  local sealer1
  sealer1=$(topo_rpc "$_S13_TOPO" node1) || { echo "scen_13: FAIL - could not resolve node1 rpc"; return 1; }

  echo "scen_13: waiting for sealers to reach block $_S13_SEALER_TARGET (something for followers to sync from) ..." >&2
  lab_wait_block "$sealer1" "$_S13_SEALER_TARGET" "$_S13_SEALER_TIMEOUT" \
    || { echo "scen_13: FAIL - sealers never reached block $_S13_SEALER_TARGET within ${_S13_SEALER_TIMEOUT}s -- nothing for followers to sync from (sealer-side problem, not a follower one)"; return 1; }
  echo "scen_13: sealer node1 head=$(lab_block_number "$sealer1")" >&2

  local followers id
  followers=$(topo_nodes "$_S13_TOPO" follower) || { echo "scen_13: FAIL - topo_nodes failed"; return 1; }
  [ -n "$followers" ] || { echo "scen_13: FAIL - all-clients.json has no follower nodes (topology changed?)"; return 1; }

  # ---- resolve url/client per follower up front ----
  declare -A F_URL F_CLIENT
  for id in $followers; do
    F_URL[$id]=$(topo_rpc "$_S13_TOPO" "$id") || { echo "scen_13: FAIL - could not resolve rpc for $id"; return 1; }
    F_CLIENT[$id]=$(topo_client "$_S13_TOPO" "$id") || { echo "scen_13: FAIL - could not resolve client for $id"; return 1; }
  done
  echo "scen_13: followers: $(for id in $followers; do printf '%s(%s) ' "$id" "${F_CLIENT[$id]}"; done)" >&2

  # ---- per-follower: peers>=1 gate (independent timeout per node -- one
  # slow/starved follower must not shrink another's own gate window) ----
  declare -A F_PEER_OK F_PEER_N
  for id in $followers; do
    echo "scen_13: [$id/${F_CLIENT[$id]}] waiting for peers>=1 (timeout ${_S13_PEER_TIMEOUT}s) ..." >&2
    if "$REPO_ROOT/netlab/health.sh" wait-peers "${F_URL[$id]}" 1 "$_S13_PEER_TIMEOUT" >&2; then
      F_PEER_OK[$id]=1
    else
      F_PEER_OK[$id]=0
    fi
    F_PEER_N[$id]=$(lab_peer_count "${F_URL[$id]}" 2>/dev/null) || F_PEER_N[$id]=0
  done

  # ---- moving-tip: one SHARED soak window, all followers syncing
  # concurrently against the same 4 sealers (the actual regression this
  # scenario exists to catch only shows up under concurrent load) ----
  declare -A F_START F_END
  for id in $followers; do
    F_START[$id]=$(lab_block_number "${F_URL[$id]}" 2>/dev/null) || F_START[$id]=0
  done
  local follower_count; follower_count=$(printf '%s\n' "$followers" | wc -w | tr -d ' ')
  echo "scen_13: soaking ${_S13_SOAK_SECONDS}s with all $follower_count follower(s) syncing concurrently ..." >&2
  sleep "$_S13_SOAK_SECONDS"
  local sealer1_end; sealer1_end=$(lab_block_number "$sealer1" 2>/dev/null) || sealer1_end="?"
  echo "scen_13: soak window done; sealer node1 head now=$sealer1_end" >&2
  for id in $followers; do
    F_END[$id]=$(lab_block_number "${F_URL[$id]}" 2>/dev/null) || F_END[$id]=0
  done

  # ---- per-follower verdict + matrix ----
  local scratch="$LAB_RESULTS_DIR/.matrix-13.md.building.$$"
  local matrix="$LAB_RESULTS_DIR/matrix-13.md"
  {
    echo "# Lab scenario matrix: id=13 (all-clients-soak)"
    echo
    echo "Generated: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    echo
    echo "| client | node | peers(end) | head start->end | verdict | reason |"
    echo "|--------|------|------------|------------------|---------|--------|"
  } > "$scratch"

  local any_fail=0 advance verdict reason end_peers
  for id in $followers; do
    end_peers=$(lab_peer_count "${F_URL[$id]}" 2>/dev/null) || end_peers=0
    advance=$(( F_END[$id] - F_START[$id] ))
    if [ "${F_PEER_OK[$id]}" != 1 ] && [ "$end_peers" -lt 1 ] 2>/dev/null; then
      verdict=FAIL; reason="peer-starved (0 peers throughout the ${_S13_PEER_TIMEOUT}s gate and at soak end)"
    elif [ "$advance" -lt "$_S13_MIN_ADVANCE" ] 2>/dev/null; then
      verdict=FAIL; reason="stalled tip (head ${F_START[$id]}->${F_END[$id]}, advanced $advance block(s), need >= $_S13_MIN_ADVANCE over ${_S13_SOAK_SECONDS}s)"
    else
      verdict=PASS; reason="peers=$end_peers, head ${F_START[$id]}->${F_END[$id]} (+$advance)"
    fi
    [ "$verdict" = FAIL ] && any_fail=1
    echo "scen_13: [$id/${F_CLIENT[$id]}] $verdict - $reason" >&2
    echo "| ${F_CLIENT[$id]} | $id | $end_peers | ${F_START[$id]}->${F_END[$id]} | $verdict | $reason |" >> "$scratch"
  done

  mv "$scratch" "$matrix"
  echo "scen_13: wrote $matrix" >&2

  if [ "$any_fail" -eq 1 ]; then
    echo "scen_13: FAIL - one or more followers peer-starved or stalled during the soak window (see matrix-13.md / PASS-FAIL lines above) -- this is the peer-starvation regression this scenario exists to catch, not a framework bug"
    return 1
  fi

  echo "scen_13: PASS - every follower (${followers//$'\n'/, }) reached peers>=1 and advanced its tip by >= $_S13_MIN_ADVANCE block(s) over the ${_S13_SOAK_SECONDS}s soak window"
  return 0
}
