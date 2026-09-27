#!/bin/bash
# lib-assert.sh - named, reusable assertions for lab scenarios
# (ItWorksinMyLocal#46 T2.3).
#
# Each assert_* function:
#   - takes plain positional args (no --flags), so scenario code can call
#     it directly without an argument-parsing dance
#   - echoes EXACTLY ONE line to stdout: "PASS: <reason>" or
#     "FAIL: <reason>" -- same one-line-verdict convention as
#     netlab/health.sh's OK:/FAIL: and oracle.sh's PARITY OK/DIVERGENCE
#   - returns 0 (PASS) or 1 (FAIL) -- never anything else; a malformed call
#     (wrong arg count) is a hard FAIL with a usage reason, not a crash, so
#     a scenario can do `assert_foo ... || { echo "..."; return 1; }`
#     unconditionally without first checking arity itself
#
# Node references are passed as "name=url" pairs throughout (e.g.
# "node1=http://127.0.0.1:34101") so a FAIL's one-line reason can name the
# offending node instead of just its bare RPC URL -- matches
# lib-topo.sh's topo_rpc/topo_nodes output shape, which is what scenario 12
# feeds these functions.
#
# Uses lib-rpc.sh directly (lab_block_hash/lab_wait_block/lab_block_number)
# -- standalone-sourceable, no dependency on lab/lib-lab.sh's fixed-topology
# client registry, same posture as netlab/health.sh (T1.4).

_LIBASSERT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_LIBASSERT_DIR/.." && pwd)" || exit 1
# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

# _assert_pass/_assert_fail: the one-line PASS:/FAIL: contract every
# assert_* function below funnels through.
_assert_pass() { printf 'PASS: %s\n' "$1"; return 0; }
_assert_fail() { printf 'FAIL: %s\n' "$1"; return 1; }

# _pair_name "name=url" -> name ; _pair_url "name=url" -> url
_pair_name() { printf '%s' "${1%%=*}"; }
_pair_url()  { printf '%s' "${1#*=}"; }

# ---------------------------------------------------------------------------
# assert_genesis_parity <name1=url1> <name2=url2> [more...]
#
# All given nodes must report the SAME block-0 (genesis) hash. This is the
# documented net5151/#34 failure mode made explicit: a genesis-hash
# mismatch means the affected node was (or will be) rejected at the
# eth/XDPoS handshake and can never sync, no matter what happens
# afterwards -- scenarios should check this FIRST, before anything else.
# ---------------------------------------------------------------------------
assert_genesis_parity() {
  [ $# -ge 2 ] || { _assert_fail "assert_genesis_parity: need >= 2 name=url pairs, got $#"; return 1; }
  local first="$1" first_name first_hash pair name url hash mismatches=""
  first_name=$(_pair_name "$first")
  first_hash=$(lab_block_hash "$(_pair_url "$first")" 0) || first_hash=""
  [ -n "$first_hash" ] || { _assert_fail "genesis parity: could not read block 0 from $first_name"; return 1; }
  shift
  for pair in "$@"; do
    name=$(_pair_name "$pair"); url=$(_pair_url "$pair")
    hash=$(lab_block_hash "$url" 0) || hash=""
    if [ -z "$hash" ]; then
      mismatches="${mismatches}${mismatches:+, }${name}=<unreachable>"
    elif [ "$hash" != "$first_hash" ]; then
      mismatches="${mismatches}${mismatches:+, }${name}=${hash:0:10}"
    fi
  done
  if [ -z "$mismatches" ]; then
    _assert_pass "genesis parity: $first_name + $# more node(s) all agree on ${first_hash:0:10}"
  else
    _assert_fail "genesis mismatch: $first_name=${first_hash:0:10} vs $mismatches"
  fi
}

# ---------------------------------------------------------------------------
# assert_lockstep <refName=refUrl> <peerName=peerUrl> <height> [timeout_s=120]
#
# Waits (up to timeout_s, for EACH of the two) for both the reference and
# the peer to reach <height>, then asserts they compute the SAME block
# hash there -- the reusable form of the pairwise "is this follower still
# in lockstep with the reference" check scenario 11 open-coded per
# checkpoint.
# ---------------------------------------------------------------------------
assert_lockstep() {
  [ $# -ge 3 ] || { _assert_fail "assert_lockstep: usage: assert_lockstep <ref=url> <peer=url> <height> [timeout_s]"; return 1; }
  local ref="$1" peer="$2" height="$3" timeout="${4:-120}"
  local ref_name peer_name ref_url peer_url ref_hash peer_hash
  ref_name=$(_pair_name "$ref"); ref_url=$(_pair_url "$ref")
  peer_name=$(_pair_name "$peer"); peer_url=$(_pair_url "$peer")

  lab_wait_block "$ref_url" "$height" "$timeout" \
    || { _assert_fail "lockstep H=$height: reference $ref_name never reached it (head=$(lab_block_number "$ref_url" 2>/dev/null || echo '?'))"; return 1; }
  lab_wait_block "$peer_url" "$height" "$timeout" \
    || { _assert_fail "lockstep H=$height: $peer_name never reached it (head=$(lab_block_number "$peer_url" 2>/dev/null || echo '?'))"; return 1; }

  ref_hash=$(lab_block_hash "$ref_url" "$height") || ref_hash=""
  peer_hash=$(lab_block_hash "$peer_url" "$height") || peer_hash=""
  if [ -n "$ref_hash" ] && [ "$ref_hash" = "$peer_hash" ]; then
    _assert_pass "lockstep H=$height: $ref_name and $peer_name agree on ${ref_hash:0:10}"
  else
    _assert_fail "lockstep H=$height: $ref_name=${ref_hash:-<miss>} $peer_name=${peer_hash:-<miss>}"
  fi
}

# ---------------------------------------------------------------------------
# assert_hash_agreement <height> <name1=url1> <name2=url2> [more...]
#
# N-way (not reference-vs-one) hash agreement at a single, already-reached
# height. Does NOT wait for anyone to catch up -- callers that need that
# should assert_lockstep or lab_wait_block first. A node missing the block
# yet is a FAIL, not a skip: this is meant to be called only at checkpoints
# the caller has already gated on.
# ---------------------------------------------------------------------------
assert_hash_agreement() {
  [ $# -ge 3 ] || { _assert_fail "assert_hash_agreement: usage: assert_hash_agreement <height> <name1=url1> <name2=url2> [...]"; return 1; }
  local height="$1"; shift
  local first="$1" first_name first_hash pair name url hash mismatches=""
  first_name=$(_pair_name "$first")
  first_hash=$(lab_block_hash "$(_pair_url "$first")" "$height") || first_hash=""
  [ -n "$first_hash" ] || { _assert_fail "hash agreement H=$height: $first_name is missing this block"; return 1; }
  shift
  for pair in "$@"; do
    name=$(_pair_name "$pair"); url=$(_pair_url "$pair")
    hash=$(lab_block_hash "$url" "$height") || hash=""
    if [ -z "$hash" ]; then
      mismatches="${mismatches}${mismatches:+, }${name}=<miss>"
    elif [ "$hash" != "$first_hash" ]; then
      mismatches="${mismatches}${mismatches:+, }${name}=${hash:0:10}"
    fi
  done
  if [ -z "$mismatches" ]; then
    _assert_pass "hash agreement H=$height: all agree on ${first_hash:0:10}"
  else
    _assert_fail "hash agreement H=$height: $first_name=${first_hash:0:10} vs $mismatches"
  fi
}

# ---------------------------------------------------------------------------
# assert_survives_epoch <name=url> [epoch=900] [margin=1] [timeout_s]
#
# Waits for the given node to reach block (epoch+margin) -- i.e. that it is
# still alive and syncing/sealing PAST the checkpoint boundary, not
# permanently stalled at or before it (the modern-geth V1 difficulty stall
# this whole issue is scoped around -- #34, scenario 11). Default timeout
# is a generous 3x(epoch+margin) seconds (period=1s/block assumption) +
# 300s slack; pass timeout_s explicitly for a non-default period.
# ---------------------------------------------------------------------------
assert_survives_epoch() {
  [ $# -ge 1 ] || { _assert_fail "assert_survives_epoch: usage: assert_survives_epoch <name=url> [epoch=900] [margin=1] [timeout_s]"; return 1; }
  local target_pair="$1" epoch="${2:-900}" margin="${3:-1}" timeout="$4"
  local name url target
  name=$(_pair_name "$target_pair"); url=$(_pair_url "$target_pair")
  target=$((epoch + margin))
  [ -n "$timeout" ] || timeout=$((target * 3 + 300))

  if lab_wait_block "$url" "$target" "$timeout"; then
    _assert_pass "survives-epoch: $name reached block $target (epoch=$epoch, margin=$margin) within ${timeout}s"
  else
    _assert_fail "survives-epoch: $name never reached block $target (epoch=$epoch, margin=$margin, head=$(lab_block_number "$url" 2>/dev/null || echo '?')) within ${timeout}s"
  fi
}

# ---------------------------------------------------------------------------
# assert_no_validation_bypass <name> <logfile>
#
# Scans a node's own log for known "swallowed consensus mismatch" markers
# instead of trusting a final hash-agreement PASS at face value.
# ItWorksinMyLocal#46's gate review found scenario 12's mixed-5 node5
# (modern-geth) logging "BlockChain: A.94 auto-recover on ValidateState
# mismatch during bulk sync block=N err=\"invalid gas used ...\"" -- i.e.
# the follower DID detect a genuine per-block consensus divergence from
# the oldxdc producer (the v1 blocksigner gas-cost mismatch #34/this
# scenario exists to surface) and silently patched over it instead of
# rejecting the block, then went on to agree with the producer's hash at
# every later checkpoint anyway. Every assertion above (genesis parity,
# lockstep, hash agreement, survives-epoch) is blind to this: they only
# ever look at CURRENT chain state, never at whether the node arrived at
# that state by validating every block or by quietly recovering from a
# rejected one. DESIGN.md section 1 defines this lab's whole purpose as
# "a follower that rejects a valid block... = a porting bug" -- a follower
# that silently ACCEPTS an invalid one via an internal auto-recover path
# is the same bug wearing a PASS. Call this for every follower's log
# alongside (not instead of) the hash-based assertions; a FAIL here means
# the run's PASS, if any, is not real parity and must not be reported as
# one. Pattern is the exact upstream wording seen in the field; extend it
# if a client's phrasing differs.
# ---------------------------------------------------------------------------
assert_no_validation_bypass() {
  [ $# -ge 2 ] || { _assert_fail "assert_no_validation_bypass: usage: assert_no_validation_bypass <name> <logfile>"; return 1; }
  local name="$1" logfile="$2" hit
  if [ ! -f "$logfile" ]; then
    _assert_fail "no-validation-bypass: $name -- log file not found ($logfile); cannot confirm no mismatch was swallowed"
    return 1
  fi
  hit=$(grep -m1 -iE 'auto-recover on ValidateState mismatch|ValidateState mismatch' "$logfile" 2>/dev/null)
  if [ -n "$hit" ]; then
    _assert_fail "no-validation-bypass: $name's log shows a swallowed consensus mismatch instead of a rejected block: ${hit}"
  else
    _assert_pass "no-validation-bypass: $name's log shows no swallowed ValidateState mismatch"
  fi
}

# ---------------------------------------------------------------------------
# assert_gas_limit_plateau <name=url> <plateau> <from_height> <to_height>
#
# The gasLimit-per-block health metric ItWorksinMyLocal#94/#96 exists to
# make first-class. genesis gasLimit is only ever the RATCHET'S STARTING
# POINT (mainnet parity: mainnet genesis is 4,700,000, its live plateau is
# 420,000,000 -- it climbed there over blocks, the genesis field was never
# touched), not a value this lab compares against. <plateau> is the single
# mint/plateau target every producer in the run was launched with
# (join.sh --gas-limit / topologies/*.json's gasLimit field).
#
# Over [from_height, to_height] this asserts, per block on the named node:
#   - gasLimit never DECREASES block-over-block. A down-step is the exact
#     signature of #94's wedge: a producer honing toward a LOWER target
#     than the one that just raised it -- i.e. two different mint targets
#     live on the same chain. (The legal per-block step is at most
#     parent/1024 in EITHER direction per XDPoSChain's misc.VerifyGaslimit,
#     but with every producer sharing one target the chain only ever climbs
#     towards it, never away.)
#   - gasLimit never exceeds <plateau> (a producer targeting something
#     ABOVE the shared plateau is the same class of bug from the other
#     side).
#   - once a block's gasLimit reaches <plateau> exactly, EVERY later block
#     in range must hold flat at <plateau> -- moving away from it after
#     arriving is still evidence of an inconsistent target somewhere.
# A single node missing a block in range is a FAIL, not a skip (same
# posture as assert_hash_agreement) -- call this only over a range the
# caller has already gated on via lab_wait_block.
# ---------------------------------------------------------------------------
assert_gas_limit_plateau() {
  [ $# -ge 4 ] || { _assert_fail "assert_gas_limit_plateau: usage: assert_gas_limit_plateau <name=url> <plateau> <from_height> <to_height>"; return 1; }
  local pair="$1" plateau="$2" from="$3" to="$4"
  local name url h gl prev="" reached_plateau=0
  name=$(_pair_name "$pair"); url=$(_pair_url "$pair")
  h="$from"
  while [ "$h" -le "$to" ]; do
    gl=$(lab_block_gas_limit "$url" "$h") || gl=""
    if [ -z "$gl" ]; then
      _assert_fail "gas-limit plateau: $name is missing block $h"
      return 1
    fi
    if [ -n "$prev" ] && [ "$gl" -lt "$prev" ]; then
      _assert_fail "gas-limit plateau: $name down-stepped at H=$h: $prev -> $gl (gasLimit must never decrease -- two different mint targets on one chain, ItWorksinMyLocal#94/#96)"
      return 1
    fi
    if [ "$gl" -gt "$plateau" ]; then
      _assert_fail "gas-limit plateau: $name exceeded the plateau at H=$h: $gl > $plateau (some producer is targeting a HIGHER ceiling than the network's shared plateau)"
      return 1
    fi
    if [ "$reached_plateau" = 1 ] && [ "$gl" -ne "$plateau" ]; then
      _assert_fail "gas-limit plateau: $name moved off the plateau after reaching it: H=$h is $gl, expected flat at $plateau"
      return 1
    fi
    [ "$gl" -eq "$plateau" ] && reached_plateau=1
    prev="$gl"
    h=$((h + 1))
  done
  _assert_pass "gas-limit plateau: $name H=$from..$to monotonic non-decreasing, converges to/holds flat at $plateau"
}
