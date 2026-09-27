#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 14-topo-mint-acceptance.sh - modern-node masternode REGISTRATION, via the
# netlab framework (ItWorksinMyLocal#46 T3.4): a fresh throwaway address
# stands in for "the modern node's coinbase" (mixed-5.json's node5 -- a
# modern-geth follower -- is sync-only in this framework, per join.sh's own
# "note: --mine ignored (geth join is sync-only)"; there is no real
# running-node account to propose, so this is exactly the address that
# WOULD be node5's coinbase once client-side sealing/BFT-voting exists --
# go-ethereum#1341). node1 (the reference sealer, already --unlock'd at
# startup) acts as that candidate's OWNER: it uploadKYCs itself, then
# proposes the candidate address, paying the cap -- propose(address) never
# requires the candidate itself to sign anything (see masternode.sh's own
# header: the onlyKYCWhitelisted check is on msg.sender, the OWNER, not the
# candidate), so the candidate address needs neither funding nor its own
# unlocked key. This also sidesteps a real, confirmed environment gap: the
# pinned OLDXDC_BIN (2.7.0-devnet) this framework prefers for netlab
# sealers does not implement the `personal` RPC namespace at all
# (rpc_modules lists no `personal` entry; personal_importRawKey/
# personal_unlockAccount both error "does not exist/is not available") --
# so a design that needed to import+unlock the CANDIDATE's own key (as an
# earlier revision of this file did) would fail here regardless of the
# registration logic being exercised. Using the already-unlocked sealer as
# owner is not a workaround for that gap, though -- it is how masternode
# ownership actually works in production too (an owner sponsors a
# validator address it does not need to hold signing keys for itself).
#
# uploadKYC + propose land BEFORE the gap block, then this scenario asserts
# the candidate address actually appears in the reference producer's
# getCandidates() once the chain has reached the next epoch boundary
# (docs/lab/DESIGN.md §5's gap-block timing rule: a propose landed by the
# gap block affects the set at the NEXT epoch boundary, not the one after).
#
# That registration assertion is the REAL thing this scenario can verify
# TODAY. The actual "mint acceptance" this scenario is named for -- the
# modern node being invited to author/co-sign a v2 block once it holds a
# masternode seat -- needs client-side BFT-vote/block-authoring support no
# modern client in this framework has yet (go-ethereum#1341 is still open
# upstream). That leg is gated behind S14_ATTEMPT_MINT_ACCEPTANCE=1: with
# it unset (the default), the scenario SKIPs (rc=2) once the registration
# assertion passes, with a reason naming go-ethereum#1341 -- never a
# silent/masked PASS for a capability that does not exist yet (the same
# "INJECT scenarios with no patched binary must SKIP" discipline
# docs/lab/DESIGN.md §7/scenario.sh's own contract already requires).
# Setting the flag does not manufacture a fake pass either: there is still
# no patched client to actually attempt authoring/voting against, so it
# SKIPs with the same reason either way -- the flag exists so this file has
# a single, obvious place to wire the real check into once a client lands.
#
# A registration FAILURE (the propose never lands in getCandidates() by the
# epoch boundary) is a real FAIL, not folded into the SKIP -- that would be
# a genuine contract/timing bug, independent of go-ethereum#1341.
#
# KNOWN ENVIRONMENT RISK (observed during T3.4 verification, reproduced 3x
# independently, NOT a bug in this file or the netlab framework): the
# pinned OLDXDC_BIN (2.7.0-devnet) this framework prefers can, once ANY
# real transaction flows through a fresh 4-sealer network (a plain value
# transfer reproduces it identically -- it is not specific to the 0x88
# masternode contract or to this scenario's own calls), have the validator
# that most recently signed a block reject the very next propagated block
# ("invalid merkle root"), then also discard its own competing attempt at
# that height ("Signed recently, must wait for others") -- leaving it
# stuck, and stalling the WHOLE network if it is currently the
# round-robin-eligible proposer. A transaction-free chain was NOT observed
# to hit this across 37+ consecutive blocks in the same environment. If
# this scenario FAILs on an uploadKYC/propose receipt never getting mined,
# check the sealer node logs for exactly this "invalid merkle root" /
# "Signed recently" pair before assuming a netlab or scenario bug -- this
# is an upstream oldxdc binary characteristic, out of this file's scope to
# fix, and reported separately (not filed to a repo this task did not
# authorize; see the session's final report).

export SCEN_DESC="topo-mint-acceptance: uploadKYC+propose the modern node's (mixed-5 node5) coinbase before the gap block (owner = the reference sealer), assert getCandidates() registration at the epoch boundary; mint-acceptance leg SKIPs pending go-ethereum#1341"
export SCEN_PRIORITY="P1"
export SCEN_PROFILE="real"
export SCEN_MODE="OBSERVE"

_S14_TOPO="$REPO_ROOT/topologies/mixed-5.json"
_S14_GAP="${S14_GAP:-450}"
_S14_EPOCH="${S14_EPOCH:-900}"
_S14_SETTLE_TO="${S14_SETTLE_TO:-910}"           # epoch boundary + margin
_S14_PROPOSE_VALUE="${S14_PROPOSE_VALUE:-11000000}"   # XDC, > genesis minCandidateCap (10,000,000)

# _s14_gen_address: a fresh, random 20-byte address -- does NOT need a real
# keypair at all. This address is only ever the CANDIDATE argument to
# propose(), never a signer/caller itself (see the file header): the
# reference sealer, already unlocked at node startup, is the owner that
# pays for and calls uploadKYC/propose on its behalf.
_s14_gen_address() {
  command -v openssl >/dev/null 2>&1 || { echo "_s14_gen_address: openssl is required" >&2; return 1; }
  printf '0x%s\n' "$(openssl rand -hex 20)"
}

# _s14_wait_receipt <rpc> <txHash> [timeout_s]: polls eth_getTransactionReceipt
# until it stops coming back null (i.e. the tx is actually MINED), instead of
# guessing "N blocks should be enough" from the current head height. A
# height-based guess is fragile against a single sealer's own transient
# round-robin stall (XDPoS v1's "signed recently, must wait for others" /
# an occasional propagated-block merkle-root mismatch while its peers
# advance -- both observed live and self-recovering within a couple of
# rounds during T3.4 verification): the specific RPC endpoint this
# scenario submits to and reads back from can legitimately sit a block or
# two behind its own peers for tens of seconds while XDPoS's own recovery
# path catches it up, and a fixed "current height + 2" target computed
# BEFORE that catch-up can therefore wait on a number that was never
# actually reachable that quickly. Waiting on the tx's OWN receipt is
# correct regardless of which specific height it ultimately lands at.
_s14_wait_receipt() {
  local rpc="$1" txh="$2" timeout="${3:-180}" waited=0 receipt
  while :; do
    receipt=$(lab_rpc_call "$rpc" eth_getTransactionReceipt "[\"$txh\"]" 2>/dev/null) || receipt=""
    [ -n "$receipt" ] && { echo "$receipt"; return 0; }
    [ "$waited" -ge "$timeout" ] && return 1
    sleep 2; waited=$((waited + 2))
  done
}

# _s14_masternode <label> <args...>: runs lab/masternode.sh, echoing its
# output to stderr (for the log) AND parsing its own "tx=0x..." line so the
# caller can wait on that exact receipt -- prints the parsed tx hash on
# stdout (nothing else), or returns nonzero if the command itself failed or
# printed no recognizable tx hash.
_s14_masternode() {
  local label="$1"; shift
  local out rc txh
  out=$("$LAB_DIR/masternode.sh" "$@" 2>&1)
  rc=$?
  printf '%s\n' "$out" >&2
  [ "$rc" -eq 0 ] || { echo "_s14_masternode: $label failed (rc=$rc)" >&2; return 1; }
  txh=$(printf '%s\n' "$out" | grep -oE 'tx=0x[0-9a-fA-F]+' | head -1 | cut -d= -f2)
  [ -n "$txh" ] || { echo "_s14_masternode: $label: could not find a tx=0x... hash in its output" >&2; return 1; }
  printf '%s\n' "$txh"
}

scen_14() {
  ( cd "$REPO_ROOT" && ./netlab/fleet.sh wipe "$_S14_TOPO" --force >/dev/null 2>&1 ) || true

  echo "scen_14: bringing up mixed-5 via topo_up --wipe ..." >&2
  topo_up "$_S14_TOPO" --wipe >&2
  local up_rc=$?
  if [ "$up_rc" -ne 0 ]; then
    echo "scen_14: FAIL - topo_up mixed-5 failed (rc=$up_rc) -- see fleet.sh output above"
    return 1
  fi

  local ref node5
  ref=$(topo_rpc "$_S14_TOPO" node1) || { echo "scen_14: FAIL - could not resolve node1 (reference sealer) rpc"; return 1; }
  node5=$(topo_rpc "$_S14_TOPO" node5) || { echo "scen_14: FAIL - could not resolve node5 (modern follower) rpc"; return 1; }
  echo "scen_14: reference(node1)=$ref modern-node(node5)=$node5" >&2

  local owner_addr
  owner_addr=$("$REPO_ROOT/netlab/node.sh" addr "$_S14_TOPO" node1) \
    || { echo "scen_14: FAIL - could not resolve node1's sealer address (acts as the candidate's owner below)"; return 1; }
  echo "scen_14: node1 sealer address (candidate owner, already unlocked at node startup) = $owner_addr" >&2

  local addr
  addr=$(_s14_gen_address) || { echo "scen_14: FAIL - could not generate a candidate address"; return 1; }
  echo "scen_14: generated candidate address (stands in for node5's coinbase) = $addr" >&2

  # ---- uploadKYC + propose, BEFORE the gap block -- owner = node1's
  # sealer account (--from only, no --key: it is already unlocked on the
  # node, exactly masternode.sh's documented "an address already unlocked
  # on the node, e.g. a producer sealer account" path) ----
  local kyc_tx
  kyc_tx=$(_s14_masternode "uploadKYC" upload-kyc "netlab-s14-$addr" --from "$owner_addr" --rpc "$ref") \
    || { echo "scen_14: FAIL - uploadKYC failed for owner $owner_addr"; return 1; }

  # uploadKYC's tx must actually be MINED before propose() is submitted --
  # propose()'s onlyKYCWhitelisted check reads ON-CHAIN state at the block
  # it executes in, not the mempool; submitting propose() back-to-back with
  # no wait risks it landing in an EARLIER block than uploadKYC (or the
  # same block, ahead of it in ordering) and reverting silently with no
  # revert reason surfaced over RPC (masternode.sh's own header note).
  # Waits on the tx's OWN receipt (see _s14_wait_receipt), not a guessed
  # block-height target.
  _s14_wait_receipt "$ref" "$kyc_tx" 180 >/dev/null \
    || { echo "scen_14: FAIL - uploadKYC tx ($kyc_tx) never mined within 180s"; return 1; }

  local propose_tx
  propose_tx=$(_s14_masternode "propose" propose "$addr" --from "$owner_addr" --rpc "$ref" --value "$_S14_PROPOSE_VALUE") \
    || { echo "scen_14: FAIL - propose($addr) failed"; return 1; }

  # Same reasoning: wait for propose()'s own tx to be mined before reading
  # is-candidate below (an eth_sendTransaction response is not a receipt).
  _s14_wait_receipt "$ref" "$propose_tx" 180 >/dev/null \
    || { echo "scen_14: FAIL - propose tx ($propose_tx) never mined within 180s"; return 1; }

  local propose_height
  propose_height=$(lab_block_number "$ref" 2>/dev/null || echo '?')
  echo "scen_14: propose($addr) mined by block $propose_height (gap block is $_S14_GAP -- must be well before it)" >&2
  if [ "$propose_height" != "?" ] && [ "$propose_height" -ge "$_S14_GAP" ] 2>/dev/null; then
    echo "scen_14: FAIL - propose landed at block $propose_height, at or past the gap block ($_S14_GAP) -- too late to affect the NEXT epoch boundary (docs/lab/DESIGN.md section 5's gap-timing rule); rerun (chain moved faster than expected) or raise S14_GAP for a slower box"
    return 1
  fi

  if [ "$("$LAB_DIR/masternode.sh" is-candidate "$addr" --rpc "$ref")" = "true" ]; then
    echo "scen_14: $addr is already a registered candidate (candidateCount reflects it before the gap block)" >&2
  else
    echo "scen_14: FAIL - $addr does not show as a candidate immediately after propose() -- propose() likely reverted silently (KYC/value issue)"
    return 1
  fi

  # ---- wait for the gap block, then the epoch boundary ----
  echo "scen_14: waiting for the gap block ($_S14_GAP) ..." >&2
  lab_wait_block "$ref" "$_S14_GAP" $((_S14_GAP * 2 + 300)) \
    || { echo "scen_14: FAIL - reference never reached the gap block ($_S14_GAP)"; return 1; }

  echo "scen_14: waiting for the epoch boundary + margin ($_S14_SETTLE_TO) ..." >&2
  lab_wait_block "$ref" "$_S14_SETTLE_TO" $((_S14_SETTLE_TO * 2 + 300)) \
    || { echo "scen_14: FAIL - reference never reached the epoch boundary ($_S14_SETTLE_TO)"; return 1; }

  # ---- THE registration assertion: getCandidates() at the epoch boundary ----
  local candidates
  candidates=$(lab_get_candidates "$ref") || { echo "scen_14: FAIL - could not read getCandidates() from reference"; return 1; }
  if printf '%s\n' "$candidates" | tr ',' '\n' | grep -qiF "$addr"; then
    echo "scen_14: PASS (registration) - $addr appears in getCandidates() at block $_S14_SETTLE_TO (proposed before gap block $_S14_GAP, as required by the gap-timing rule)" >&2
  else
    echo "scen_14: FAIL - $addr does NOT appear in getCandidates() at the epoch boundary ($_S14_SETTLE_TO), despite propose() landing before the gap block ($propose_height < $_S14_GAP) -- getCandidates()=$candidates"
    return 1
  fi

  # ---- best-effort, non-fatal: does node5 (the modern follower) observe
  # the SAME registration through its own synced state? Not this
  # scenario's gate (node5's own sync health/known V1 stall is scenario
  # 12's territory, ItWorksinMyLocal#34) -- purely informational here.
  local node5_candidates
  if node5_candidates=$(lab_get_candidates "$node5" 2>/dev/null) && [ -n "$node5_candidates" ]; then
    if printf '%s\n' "$node5_candidates" | tr ',' '\n' | grep -qiF "$addr"; then
      echo "scen_14: (informational) node5 (modern follower) also observes $addr in its own getCandidates()" >&2
    else
      echo "scen_14: (informational) node5 (modern follower) does not yet observe $addr in getCandidates() -- if node5 has stalled at its known V1 boundary (#34), this is expected and is scenario 12's concern, not this one's" >&2
    fi
  else
    echo "scen_14: (informational) could not read getCandidates() from node5 (likely still syncing or already stalled at its known V1 boundary, #34) -- not fatal here" >&2
  fi

  # ---- the actual mint-acceptance leg: gated, pending go-ethereum#1341 ----
  if [ "${S14_ATTEMPT_MINT_ACCEPTANCE:-0}" != 1 ]; then
    echo "SKIP:mint-acceptance leg not attempted (S14_ATTEMPT_MINT_ACCEPTANCE is unset/0) -- no modern client in this framework supports client-side BFT-vote/block-authoring yet (go-ethereum#1341 is still open); the registration assertion above already PASSED"
    return 2
  fi
  # Flag forced on: still nothing to actually check -- no patched client
  # exists yet to attempt authoring/voting against, so this remains an
  # honest SKIP, not a fabricated PASS (see the file header).
  echo "SKIP:S14_ATTEMPT_MINT_ACCEPTANCE=1 was set, but no modern client in this framework yet implements client-side BFT-vote/block-authoring (go-ethereum#1341 still open) -- there is nothing to attempt; wire the real check in here once a patched client lands"
  return 2
}
