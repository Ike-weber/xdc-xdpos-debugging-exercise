#!/bin/bash
# onboard-masternode.sh - end-to-end RUNTIME onboarding of a brand-new XDPoS
# masternode candidate on a LIVE net: generate owner+coinbase keys, fund the
# owner, uploadKYC, propose, verify, and report/poll the actual seating
# block. This is the missing glue around lab/masternode.sh (ItWorksinMyLocal
# #62) -- masternode.sh already implements every individual 0x88 call
# (propose/upload-kyc/vote/list/is-candidate/candidate-cap/...) but has no
# end-to-end flow and, critically, no TIMING logic. This script calls
# masternode.sh for every contract write (it does NOT reimplement any
# selector) and adds exactly the two things masternode.sh does not have:
# the multi-step flow, and the gap-deadline math below.
#
# ---------------------------------------------------------------------------
# THE TIMING MATH -- read this before touching any of the arithmetic below.
# ---------------------------------------------------------------------------
# A candidate proposed via propose() is only ever SEATED into the active
# masternode set at an EPOCH SWITCH block. The set that takes effect AT that
# switch is snapshotted at a GAP BLOCK that sits `Gap` blocks before the
# START of the epoch the switch belongs to -- NOT `Gap` blocks before the
# switch block itself. Confusing "before the switch" with "before the epoch
# start" is exactly the off-by-one-epoch mistake that has already happened
# once on this program and silently wasted a full epoch (propose() was
# mined AFTER the real snapshot point but still looked "early enough" if you
# measured from the switch block instead of the epoch start).
#
# V1 regime (current block is BEFORE the net's V1->V2 consensus switch
# block): checkpoints -- and therefore epoch starts -- sit at plain
# multiples of Epoch (900, 1800, 2700, ...), so:
#     gapNumber = checkpoint - Gap
#
# V2 (HotStuff) regime (current block is AT/AFTER the V1->V2 switch block):
# epoch-switch blocks do NOT sit at multiples of Epoch. They sit at
#     S ≡ (Epoch-1) mod Epoch
# e.g. with Epoch=900 the switch blocks are 899, 1799, 2699, 3599, 4499,
# 5399, 6299, ... -- NOT 900, 1800, 2700, .... The epoch that switch S
# belongs to STARTS at `S - (S % Epoch)` (the previous multiple of Epoch),
# so the deadline is:
#     gapNumber = S - (S % Epoch) - Gap
#
# Worked example (verified live on netv12, Epoch=900, Gap=450):
#     S = 4499  =>  S % Epoch = 4499 % 900 = 899
#                   gapNumber = 4499 - 899 - 450 = 3150
# Note gapNumber (3150) is 1349 blocks BEFORE the switch (4499), and sits
# inside the PREVIOUS epoch entirely. "Gap blocks before the switch" would
# have given 4049 -- 899 blocks too late, i.e. an entire epoch missed.
#
# The propose() transaction must be MINED at or before gapNumber -- merely
# SUBMITTED (in the mempool) is not enough; if it lands in a block after
# gapNumber it simply seats one epoch later than expected, silently.
#
# For a V1-era net, the checkpoint itself is at a multiple of Epoch and the
# deadline is `checkpoint - Gap` (no S-sequence involved -- see above).
#
# This script sources Epoch/Gap/the V1->V2 switch block from the chain
# itself (XDPoS_networkInformation -> ConsensusConfigs.{epoch,gap,v2.
# switchBlock}) wherever the RPC endpoint supports it, falling back to
# --epoch/--gap/--v2-switch-block flags (default 900/450/unknown) with a
# loud "DEFAULT, not chain-verified" warning -- never silently assumes
# 900/450 the way an off-by-one script might. It always prints exactly
# which source (chain vs flag vs default) was used for each value.
#
# ---------------------------------------------------------------------------
# The flow
# ---------------------------------------------------------------------------
#   1. Generate a NEW owner key and a NEW coinbase key (must be different
#      accounts -- a hard requirement of the masternode program: owner !=
#      coinbase). Both go into a LOCAL keyfile, chmod 600, never printed to
#      stdout/argv, never committed (see .gitignore). Re-running `run`
#      against an existing --keyfile reuses the same owner/coinbase instead
#      of generating fresh ones each time (idempotent retries).
#   2. Fund the owner from a prefunded account (--funder-from, already
#      unlocked on the node, e.g. a producer sealer -- or --funder-key-stdin)
#      with minCandidateCap() (read live via selector d55b7dff -- NEVER
#      hardcoded; it's 10,000 XDC on netv12 but this script does not assume
#      that on any other net) plus a small gas headroom.
#   3. uploadKYC(string) from the owner, BEFORE propose -- masternode.sh's
#      own header documents that propose() carries an onlyKYCWhitelisted
#      modifier and reverts with status 0x0 and NO revert reason if the
#      owner has never uploaded KYC. Receipt status is checked and this
#      script dies loudly (does not proceed to propose) if it isn't 0x1.
#   4. propose(coinbase) payable from the owner, value = the live-queried
#      minCandidateCap. Receipt status checked (0x1 or die), then verified
#      independently two ways: is-candidate(coinbase) and presence in
#      getCandidates() (both via masternode.sh -- read-only, no selectors
#      reimplemented here).
#   5. The block the propose tx actually MINED in (not the block it was
#      submitted in) is run back through the timing math above to name the
#      exact switch block this candidate is on track to be seated at, and
#      whether that mining block landed at-or-before that switch's
#      gapNumber deadline. The candidate's actual presence in the live
#      masternode set (XDPoS_getMasternodesByNumber) is then polled until
#      it appears or --poll-timeout elapses (seating can be very far in the
#      future -- see gapNumber's typical ~1.5-epoch lead time above; the
#      default poll timeout is a short safety valve for THIS process, not a
#      promise to block for a whole epoch -- re-run `status --follow` later
#      to keep checking a pending seat).
#
# ---------------------------------------------------------------------------
# masternode.sh --key-stdin
# ---------------------------------------------------------------------------
# masternode.sh's own CLI only accepted `--key HEXPRIVKEY` on argv. That is
# incompatible with this script's hard requirement to NEVER put a private
# key on a command line (visible to any other user on the box via `ps` /
# /proc/<pid>/cmdline, and to shell history) -- brand-new owner/coinbase
# keys generated here are exactly the kind of secret that rule protects.
# Rather than reimplementing propose/upload-kyc's selector logic here to
# avoid the conflict, masternode.sh gained one small, additive, backward-
# compatible flag: `--key-stdin`, which reads the raw key from stdin (one
# line) instead of argv. Every call this script makes into masternode.sh
# for a write (upload-kyc, propose) uses `--key-stdin`, piping the key in;
# `--key` on argv is never used from here.
#
# ---------------------------------------------------------------------------
# Exit codes
# ---------------------------------------------------------------------------
#   0  success -- for `plan`: table printed; for `run`/`status`: candidate
#      confirmed present in the live masternode set.
#   1  hard failure -- bad args, an RPC call failed, a receipt was missing
#      or had status != 0x1, or a post-propose verification failed.
#   2  pending, not a failure -- `run`/`status` completed every on-chain
#      step successfully (candidate confirmed via is-candidate/
#      getCandidates) but the poll window elapsed before the candidate
#      appeared in the live masternode set. Re-run `status --keyfile FILE
#      --follow` to keep waiting.
#
# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
#   ./onboard-masternode.sh plan [opts]
#       Read-only: prints the sourced Epoch/Gap/regime, the timing table of
#       upcoming switch blocks with deadlines and blocks-remaining, the
#       live minCandidateCap, and a PREVIEW owner/coinbase address pair
#       (freshly generated in-memory for illustration only -- NOT persisted
#       to any keyfile, NOT reused by a later `run`). Sends zero
#       transactions, never unlocks/imports any key on the node. This is
#       the mode you are meant to run first.
#
#   ./onboard-masternode.sh run [opts]
#       The full flow (steps 1-5 above). `run --dry-run` is an alias for
#       `plan` (same opts apply) -- useful so a single command line can be
#       flipped from preview to real by adding/removing one flag.
#
#   ./onboard-masternode.sh status --keyfile FILE [--follow] [opts]
#       Re-checks an existing keyfile's coinbase: is-candidate, presence in
#       getCandidates(), and presence in the live masternode set, plus the
#       recorded target switch/deadline from that keyfile's .state sidecar
#       (written by `run`) if present. `--follow` polls instead of a single
#       check (same semantics as `run`'s own poll tail).
#
#   ./onboard-masternode.sh self-test
#       Pure arithmetic regression check for _target_switch_for_block (the
#       switch-attribution helper `run`'s post-mine step and `plan`'s timing
#       table both share) -- no RPC, no node, no transactions. Covers one
#       block % Epoch > Gap case and one block % Epoch <= Gap case against
#       live-verified values (ItWorksinMyLocal#99); exits non-zero on any
#       mismatch.
#
# Common options:
#   --rpc URL               JSON-RPC endpoint (default: the lab producer,
#                           node1 -- same default as masternode.sh)
#   --epoch N                 override Epoch (else sourced from the chain)
#   --gap N                   override Gap (else sourced from the chain)
#   --v2-switch-block N       override the V1->V2 consensus switch block
#                           (else sourced from the chain; if genuinely
#                           unknown, pass 0 to force "always V2 regime" or a
#                           huge number to force "always V1 regime")
#   --count N                 how many upcoming switch/checkpoint rows to
#                           print in the timing table (default 5)
#
# run/status-only options:
#   --keyfile FILE            local owner+coinbase keyfile (chmod 600).
#                           Default: lab/results/onboard-masternode-keys.env
#                           `run` reuses it if present (idempotent retries);
#                           `status` requires it (or --coinbase, see below).
#   --coinbase ADDR            (status only, in place of --keyfile) check an
#                           arbitrary already-known coinbase address instead
#                           of reading one from a keyfile.
#   --funder-from ADDR         an address already unlocked on the node (e.g.
#                           a producer sealer) to fund the owner from.
#   --funder-key-stdin         read the funder's raw private key from stdin
#                           (one line) instead of --funder-from.
#   --value-headroom-xdc N     extra XDC funded to the owner atop
#                           minCandidateCap, for gas (default 50).
#   --propose-value-xdc N      XDC value sent WITH propose() (default: the
#                           live-queried minCandidateCap, rounded up to a
#                           whole XDC if the on-chain value ever isn't one).
#   --gas N                    gas limit passed through to masternode.sh
#                           (default 2000000, same default it uses).
#   --unlock-secs N            personal_unlockAccount duration passed
#                           through to masternode.sh (default 300).
#   --poll-interval N          seconds between masternode-set polls
#                           (default 5).
#   --poll-timeout N           seconds to poll before giving up (default
#                           120 -- a safety valve for this process, NOT the
#                           expected real wait; see exit code 2 above).
#   --follow                   (status only) poll instead of a single check.
#   --kyc-hash STRING          KYC placeholder string (default:
#                           "onboard-masternode-lab-kyc", same idea as
#                           masternode.sh's own upload-kyc default).

set -u

_SELF_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 1
_SELF="$_SELF_DIR/$(basename "$0")"
cd "$_SELF_DIR" || exit 1
# shellcheck source=lib-lab.sh
source ./lib-lab.sh

# minCandidateCap() -- NOT in masternode.sh (which only has getCandidateCap
# (address), selector 58e7525f, a PER-CANDIDATE cap read). This is the
# global minimum, queried live so this script never assumes a value (it is
# 10,000 XDC on netv12, but that is net-specific config, not a constant).
SEL_MIN_CANDIDATE_CAP=d55b7dff
# getLatestKYC(address) -- also not in masternode.sh. Used only as a
# best-effort idempotency check (skip a redundant uploadKYC on a `run`
# retry against a keyfile that already has KYC on-chain); reverts (INVALID
# opcode, on this contract's older Solidity) for an address with no KYC
# history yet, which this script treats as "needs KYC", not as a hard
# error -- see _owner_has_kyc.
SEL_GET_LATEST_KYC=32658652

RPC=""
EPOCH_OVERRIDE=""
GAP_OVERRIDE=""
SWITCH_BLOCK_OVERRIDE=""
COUNT=5
KEYFILE=""
COINBASE_ARG=""
FUNDER_FROM=""
FUNDER_KEY_STDIN=""
HEADROOM_XDC=50
PROPOSE_VALUE_XDC=""
GAS=2000000
UNLOCK_SECS=300
POLL_INTERVAL=5
POLL_TIMEOUT=120
FOLLOW=""
KYC_HASH="onboard-masternode-lab-kyc"

_usage() { sed -n '2,191p' "$_SELF" | sed 's/^# \{0,1\}//'; }

_die() { echo "onboard-masternode.sh: $*" >&2; exit 1; }

_default_keyfile() { printf '%s/onboard-masternode-keys.env\n' "$LAB_RESULTS_DIR"; }

_parse_opts() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --rpc)                 RPC=$2; shift 2 ;;
      --epoch)               EPOCH_OVERRIDE=$2; shift 2 ;;
      --gap)                 GAP_OVERRIDE=$2; shift 2 ;;
      --v2-switch-block)     SWITCH_BLOCK_OVERRIDE=$2; shift 2 ;;
      --count)               COUNT=$2; shift 2 ;;
      --keyfile)             KEYFILE=$2; shift 2 ;;
      --coinbase)             COINBASE_ARG=$2; shift 2 ;;
      --funder-from)          FUNDER_FROM=$2; shift 2 ;;
      --funder-key-stdin)     FUNDER_KEY_STDIN=1; shift 1 ;;
      --value-headroom-xdc)   HEADROOM_XDC=$2; shift 2 ;;
      --propose-value-xdc)    PROPOSE_VALUE_XDC=$2; shift 2 ;;
      --gas)                  GAS=$2; shift 2 ;;
      --unlock-secs)          UNLOCK_SECS=$2; shift 2 ;;
      --poll-interval)        POLL_INTERVAL=$2; shift 2 ;;
      --poll-timeout)         POLL_TIMEOUT=$2; shift 2 ;;
      --follow)               FOLLOW=1; shift 1 ;;
      --kyc-hash)             KYC_HASH=$2; shift 2 ;;
      --dry-run)              DRY_RUN=1; shift 1 ;;
      *) echo "onboard-masternode.sh: unknown option: $1" >&2; return 1 ;;
    esac
  done
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  [ -n "$KEYFILE" ] || KEYFILE=$(_default_keyfile)
}

# ---------------------------------------------------------------------------
# Chain config sourcing (Epoch/Gap/V1->V2 switch block)
# ---------------------------------------------------------------------------

# _read_chain_config <rpc>: prints "EPOCH GAP SWITCH_BLOCK SOURCE" on one
# line via XDPoS_networkInformation, or returns nonzero if that RPC method
# isn't available/parseable (e.g. a non-XDC-family client, or a net with no
# V2 config at all -- the latter still succeeds, with SWITCH_BLOCK reported
# as a sentinel meaning "never/always-V1", see below).
_read_chain_config() {
  local rpc="$1" raw
  raw=$(lab_rpc_call "$rpc" XDPoS_networkInformation "[]" 2>/dev/null) || return 1
  python3 -c "
import json
import sys

try:
    d = json.loads(sys.argv[1])
except Exception:
    sys.exit(1)
cc = d.get('ConsensusConfigs') or {}
epoch = cc.get('epoch')
gap = cc.get('gap')
if epoch is None or gap is None:
    sys.exit(1)
v2 = cc.get('v2') or {}
switch_block = v2.get('switchBlock')
if switch_block is None:
    # No V2 config at all on this net -- treat as 'never reaches V2',
    # i.e. always V1 regime, via a sentinel far beyond any real chain height.
    switch_block = 2**63 - 1
print('%d %d %d' % (int(epoch), int(gap), int(switch_block)))
" "$raw"
}

# _resolve_chain_config <rpc>: fills EPOCH/GAP/SWITCH_BLOCK/{EPOCH,GAP,
# SWITCH_BLOCK}_SOURCE globals. Precedence: explicit --epoch/--gap/
# --v2-switch-block flag > chain (XDPoS_networkInformation) > hardcoded
# default (900/450/sentinel) -- and ALWAYS prints which source won, so a
# silent 900/450 assumption is never mistaken for a chain-verified one.
EPOCH=900; EPOCH_SOURCE="default"
GAP=450; GAP_SOURCE="default"
SWITCH_BLOCK=9223372036854775807; SWITCH_BLOCK_SOURCE="default (assumed always-V1)"

_resolve_chain_config() {
  local rpc="$1" line chain_epoch chain_gap chain_switch
  if line=$(_read_chain_config "$rpc"); then
    read -r chain_epoch chain_gap chain_switch <<<"$line"
    EPOCH="$chain_epoch"; EPOCH_SOURCE="chain (XDPoS_networkInformation)"
    GAP="$chain_gap"; GAP_SOURCE="chain (XDPoS_networkInformation)"
    SWITCH_BLOCK="$chain_switch"; SWITCH_BLOCK_SOURCE="chain (XDPoS_networkInformation)"
  else
    echo "onboard-masternode.sh: WARNING -- could not read Epoch/Gap/V2-switch-block from $rpc via XDPoS_networkInformation (non-XDC-family client? unreachable?); falling back to defaults (Epoch=900 Gap=450, assumed-always-V1) unless overridden by flags -- these are NOT chain-verified" >&2
  fi
  if [ -n "$EPOCH_OVERRIDE" ]; then EPOCH="$EPOCH_OVERRIDE"; EPOCH_SOURCE="flag --epoch"; fi
  if [ -n "$GAP_OVERRIDE" ]; then GAP="$GAP_OVERRIDE"; GAP_SOURCE="flag --gap"; fi
  if [ -n "$SWITCH_BLOCK_OVERRIDE" ]; then SWITCH_BLOCK="$SWITCH_BLOCK_OVERRIDE"; SWITCH_BLOCK_SOURCE="flag --v2-switch-block"; fi
  echo "onboard-masternode.sh: using Epoch=$EPOCH (source: $EPOCH_SOURCE), Gap=$GAP (source: $GAP_SOURCE), V1->V2 switch block=$SWITCH_BLOCK (source: $SWITCH_BLOCK_SOURCE)" >&2
}

# ---------------------------------------------------------------------------
# Timing table (the math from the header comment, in one place)
# ---------------------------------------------------------------------------

# _timing_table <tip> <epoch> <gap> <switch_block> <count>: prints
#   REGIME V1|V2
#   ROW <switchOrCheckpoint> <gapNumber> PASSED|OK <blocksRemaining>
#     (one line per row, oldest-first, blocksRemaining = gapNumber - tip;
#     negative/zero means PASSED)
#   RECOMMENDED <switchOrCheckpoint> <gapNumber> <blocksRemaining>
#     (the first S/checkpoint, scanning forward from `tip`, whose gapNumber
#     deadline has NOT yet passed -- extends past `count` rows if every
#     displayed row has already passed, so a recommendation is always
#     produced) -- or "RECOMMENDED NONE" only if none could be found within
#     a generous search bound (should not happen in practice).
_timing_table() {
  local tip="$1" epoch="$2" gap="$3" switch_block="$4" count="$5"
  python3 -c "
import sys

tip = int(sys.argv[1])
epoch = int(sys.argv[2])
gap = int(sys.argv[3])
switch_block = int(sys.argv[4])
count = int(sys.argv[5])


def v2_rows(start_k, n):
    out = []
    for i in range(n):
        k = start_k + i
        s = k * epoch - 1
        gap_number = s + 1 - epoch - gap  # == (s - s % epoch) - gap
        out.append((s, gap_number))
    return out


def v1_rows(start_k, n):
    out = []
    for i in range(n):
        k = start_k + i
        checkpoint = k * epoch
        gap_number = checkpoint - gap
        out.append((checkpoint, gap_number))
    return out


if tip >= switch_block:
    regime = 'V2'
    k0 = tip // epoch + 1
    while k0 * epoch - 1 <= tip:
        k0 += 1
    gen = v2_rows
else:
    regime = 'V1'
    k0 = tip // epoch + 1
    while k0 * epoch <= tip:
        k0 += 1
    gen = v1_rows

print('REGIME %s' % regime)

rows = gen(k0, count)
for (mark, deadline) in rows:
    passed = deadline <= tip
    remaining = deadline - tip
    print('ROW %d %d %s %d' % (mark, deadline, 'PASSED' if passed else 'OK', remaining))

recommended = None
all_rows = list(rows)
extra_k = k0 + count
tries = 0
while recommended is None and tries < 50:
    for (mark, deadline) in all_rows:
        if deadline > tip:
            recommended = (mark, deadline)
            break
    if recommended is None:
        all_rows = gen(extra_k, count)
        extra_k += count
        tries += 1

if recommended:
    mark, deadline = recommended
    print('RECOMMENDED %d %d %d' % (mark, deadline, deadline - tip))
else:
    print('RECOMMENDED NONE')
" "$tip" "$epoch" "$gap" "$switch_block" "$count"
}

# _print_timing_table <rpc> <epoch> <gap> <switch_block> <count>: fetches
# the live tip, runs _timing_table, prints a human-readable table, and sets
# REGIME/RECO_MARK/RECO_DEADLINE/RECO_REMAINING globals for callers that
# need the recommended target programmatically.
REGIME=""; RECO_MARK=""; RECO_DEADLINE=""; RECO_REMAINING=""

_print_timing_table() {
  local rpc="$1" epoch="$2" gap="$3" switch_block="$4" count="$5" tip out line
  tip=$(lab_block_number "$rpc") || _die "could not read current block number from $rpc"
  out=$(_timing_table "$tip" "$epoch" "$gap" "$switch_block" "$count") || _die "timing table computation failed"

  echo "onboard-masternode.sh: current tip = $tip" >&2
  while IFS= read -r line; do
    case "$line" in
      REGIME\ *)
        REGIME="${line#REGIME }"
        if [ "$REGIME" = "V2" ]; then
          echo "onboard-masternode.sh: regime = V2 (HotStuff) -- switch blocks satisfy (S mod Epoch) == (Epoch-1), NOT multiples of Epoch" >&2
        else
          echo "onboard-masternode.sh: regime = V1 -- checkpoints sit at multiples of Epoch" >&2
        fi
        printf '%-12s %-12s %-10s %s\n' "SWITCH/CKPT" "DEADLINE" "STATUS" "BLOCKS_REMAINING"
        ;;
      ROW\ *)
        # shellcheck disable=SC2086  # intentional word-splitting: $line is
        # our own space-separated "ROW mark deadline status remaining" output
        set -- $line
        # $1=ROW $2=mark $3=deadline $4=status $5=remaining
        printf '%-12s %-12s %-10s %s\n' "$2" "$3" "$4" "$5"
        ;;
      RECOMMENDED\ NONE)
        echo "onboard-masternode.sh: WARNING -- no reachable switch/checkpoint found in a generous search window" >&2
        ;;
      RECOMMENDED\ *)
        # shellcheck disable=SC2086  # intentional word-splitting, see above
        set -- $line
        RECO_MARK="$2"; RECO_DEADLINE="$3"; RECO_REMAINING="$4"
        ;;
    esac
  done <<<"$out"

  if [ -n "$RECO_MARK" ]; then
    echo "onboard-masternode.sh: RECOMMENDED TARGET -- switch/checkpoint $RECO_MARK, propose must be MINED at or before block $RECO_DEADLINE ($RECO_REMAINING blocks from now)" >&2
  fi
  # Explicitly call out passed deadlines per the spec's own worked example
  # ("say so explicitly and name the first one that is still reachable").
  if printf '%s\n' "$out" | grep -q '^ROW .* PASSED '; then
    echo "onboard-masternode.sh: note -- one or more of the switches/checkpoints listed above already have a PASSED deadline (their gapNumber is behind the current tip); the RECOMMENDED target above is the first one still reachable." >&2
  fi
}

# _target_switch_for_block <block> <epoch> <gap> <switch_block> [count]:
# "which switch/checkpoint does a propose() MINED at <block> attribute to?"
# -- answered by running _timing_table's OWN "smallest still-reachable
# switch" search (the same RECOMMENDED-line logic `plan`/`_print_timing_table`
# already use against the LIVE tip) as-of <block> instead of the live tip.
#
# THIS IS THE ONLY PLACE THAT SEARCH MAY BE IMPLEMENTED. `run`'s post-mine
# attribution used to recompute it inline instead of calling here: it took
# only the FIRST row _timing_table's k0 guess produced for <block> and, if
# that row's gapNumber deadline had already passed <block>, bumped the mark
# by exactly one Epoch and stopped -- never re-checking whether even the
# bumped switch's OWN deadline had passed too. That one-Epoch bump is only
# ever correct when <block> lands in the tail Gap blocks of its epoch
# (block % Epoch <= Gap); for every other block (block % Epoch > Gap --
# measured ~49.7% of the time live) the bumped switch's deadline has ALSO
# already passed by <block>, and the true target is a second Epoch later
# still. That inline reimplementation, independent of the search below, is
# exactly how it drifted from `plan`'s (correct) answer -- see
# ItWorksinMyLocal#99.
#
# Verified live (Epoch=900, Gap=450): S=4499 -> gapNumber 3150; S=5399 ->
# gapNumber 4050; S=6299 -> gapNumber 4950. A propose MINED at block 4276
# (4276 % 900 = 676 > 450 = Gap) must attribute to S=6299 (gapNumber 4950
# is the first one still >= 4276), NOT S=5399 (whose gapNumber 4050 had
# already passed by block 4276) -- see `self-test` below for this exact
# case, plus a block % Epoch <= Gap case, as regression coverage.
#
# Sets TARGET_SWITCH_MARK / TARGET_SWITCH_DEADLINE; returns non-zero (both
# left empty) only if _timing_table's own bounded search gave up (should
# not happen in practice -- see its own comment).
TARGET_SWITCH_MARK=""; TARGET_SWITCH_DEADLINE=""
_target_switch_for_block() {
  local block="$1" epoch="$2" gap="$3" switch_block="$4" count="${5:-5}" out
  out=$(_timing_table "$block" "$epoch" "$gap" "$switch_block" "$count") || return 1
  # Match only a NUMERIC mark ("RECOMMENDED 6299 4950 ..."), never the
  # "RECOMMENDED NONE" line (also starts with "RECOMMENDED ").
  TARGET_SWITCH_MARK=$(printf '%s\n' "$out" | awk '/^RECOMMENDED [0-9]/{print $2; exit}')
  TARGET_SWITCH_DEADLINE=$(printf '%s\n' "$out" | awk '/^RECOMMENDED [0-9]/{print $3; exit}')
  [ -n "$TARGET_SWITCH_MARK" ] && [ -n "$TARGET_SWITCH_DEADLINE" ]
}

# _assert_target_switch <label> <block> <epoch> <gap> <switch_block>
#   <want_switch> <want_deadline>: regression coverage for
# _target_switch_for_block, used by the `self-test` command below. Prints
# PASS/FAIL and increments the global _SELFTEST_FAILURES counter on a
# mismatch -- never exits by itself, so a caller can run several assertions
# and report a single pass/fail summary.
_SELFTEST_FAILURES=0
_assert_target_switch() {
  local label="$1" block="$2" epoch="$3" gap="$4" switch_block="$5" want_switch="$6" want_deadline="$7"
  if ! _target_switch_for_block "$block" "$epoch" "$gap" "$switch_block" 5; then
    echo "FAIL $label: _target_switch_for_block returned no result for block $block" >&2
    _SELFTEST_FAILURES=$((_SELFTEST_FAILURES + 1))
    return
  fi
  if [ "$TARGET_SWITCH_MARK" = "$want_switch" ] && [ "$TARGET_SWITCH_DEADLINE" = "$want_deadline" ]; then
    echo "PASS $label: block=$block (block % epoch = $((block % epoch))) -> switch=$TARGET_SWITCH_MARK deadline=$TARGET_SWITCH_DEADLINE" >&2
  else
    echo "FAIL $label: block=$block -> got switch=$TARGET_SWITCH_MARK deadline=$TARGET_SWITCH_DEADLINE, want switch=$want_switch deadline=$want_deadline" >&2
    _SELFTEST_FAILURES=$((_SELFTEST_FAILURES + 1))
  fi
}

# ---------------------------------------------------------------------------
# Key generation (owner + coinbase; must be two DIFFERENT accounts)
# ---------------------------------------------------------------------------

# _gen_keypair: prints "PRIVKEY ADDR" (space-separated, one line) for one
# fresh secp256k1 key. Uses eth_keys (pure-Python fallback backend, no
# `coincurve` required) -- pycryptodome's ECC module (already used
# elsewhere in this lab for keccak256 cross-checks) does NOT support
# secp256k1, only NIST/Ed curves, so it cannot do this job.
_gen_keypair() {
  python3 -c "
import os
import sys

try:
    from eth_keys import keys
except ImportError:
    sys.stderr.write('onboard-masternode.sh: python3 module eth_keys is required to generate keys (pip install eth-keys)\n')
    sys.exit(1)

pk = keys.PrivateKey(os.urandom(32))
addr = pk.public_key.to_checksum_address().lower()
print('%s %s' % (pk.to_hex(), addr))
"
}

# _load_or_generate_keys <keyfile>: sources OWNER_ADDR/OWNER_KEY/
# COINBASE_ADDR/COINBASE_KEY from an existing keyfile if present (chmod 600
# enforced/warned), else generates a fresh owner+coinbase pair (asserting
# they differ -- a hard program requirement: owner != coinbase) and writes
# it. Never prints a private key. Sets OWNER_ADDR/OWNER_KEY/COINBASE_ADDR/
# COINBASE_KEY.
OWNER_ADDR=""; OWNER_KEY=""; COINBASE_ADDR=""; COINBASE_KEY=""

_load_or_generate_keys() {
  local keyfile="$1" perms
  if [ -f "$keyfile" ]; then
    perms=$(stat -c '%a' "$keyfile" 2>/dev/null || stat -f '%Lp' "$keyfile" 2>/dev/null)
    if [ "$perms" != "600" ]; then
      echo "onboard-masternode.sh: WARNING -- $keyfile is not chmod 600 (perms=$perms); tightening it now" >&2
      chmod 600 "$keyfile" 2>/dev/null || true
    fi
    # shellcheck disable=SC1090
    source "$keyfile"
    [ -n "${OWNER_ADDR:-}" ] && [ -n "${OWNER_KEY:-}" ] && [ -n "${COINBASE_ADDR:-}" ] && [ -n "${COINBASE_KEY:-}" ] \
      || _die "$keyfile exists but is missing OWNER_ADDR/OWNER_KEY/COINBASE_ADDR/COINBASE_KEY"
    echo "onboard-masternode.sh: reusing existing keyfile $keyfile (owner=$OWNER_ADDR coinbase=$COINBASE_ADDR)" >&2
    return 0
  fi

  local owner_line coinbase_line
  owner_line=$(_gen_keypair) || _die "key generation failed (owner)"
  coinbase_line=$(_gen_keypair) || _die "key generation failed (coinbase)"
  OWNER_KEY="${owner_line%% *}"; OWNER_ADDR="${owner_line##* }"
  COINBASE_KEY="${coinbase_line%% *}"; COINBASE_ADDR="${coinbase_line##* }"

  if [ "$OWNER_ADDR" = "$COINBASE_ADDR" ]; then
    # Astronomically unlikely (two independent 256-bit draws colliding) but
    # the program HARD REQUIRES owner != coinbase, so this is a fail-closed
    # check, not decoration.
    _die "generated owner and coinbase addresses are identical ($OWNER_ADDR) -- refusing to proceed; re-run"
  fi

  mkdir -p "$(dirname "$keyfile")" || _die "could not create directory for $keyfile"
  ( umask 077
    {
      echo "# onboard-masternode.sh keyfile -- generated $(date -u +%FT%TZ). chmod 600."
      echo "# Owner and coinbase are DIFFERENT accounts on purpose (program requirement:"
      echo "# owner != coinbase). NEVER commit this file -- see .gitignore."
      echo "OWNER_ADDR=$OWNER_ADDR"
      echo "OWNER_KEY=$OWNER_KEY"
      echo "COINBASE_ADDR=$COINBASE_ADDR"
      echo "COINBASE_KEY=$COINBASE_KEY"
    } > "$keyfile"
  )
  chmod 600 "$keyfile" || _die "could not chmod 600 $keyfile"
  echo "onboard-masternode.sh: generated new owner=$OWNER_ADDR coinbase=$COINBASE_ADDR, wrote $keyfile (chmod 600)" >&2
}

# _gen_preview_keypair: like _load_or_generate_keys but for `plan` only --
# generates a throwaway pair purely for display, persists NOTHING, and is
# never reused by a later `run` (plan performs no transactions and touches
# no file, per spec).
_print_preview_keys() {
  local owner_line coinbase_line
  owner_line=$(_gen_keypair) || _die "key generation failed (preview owner)"
  coinbase_line=$(_gen_keypair) || _die "key generation failed (preview coinbase)"
  echo "onboard-masternode.sh: PREVIEW owner   = ${owner_line##* } (not persisted -- 'run' will generate its own and write $KEYFILE)" >&2
  echo "onboard-masternode.sh: PREVIEW coinbase = ${coinbase_line##* } (not persisted)" >&2
}

# ---------------------------------------------------------------------------
# minCandidateCap()
# ---------------------------------------------------------------------------

# _min_candidate_cap_wei <rpc>: live value, decimal wei. NEVER hardcoded --
# see the header note (10,000 XDC on netv12 is this net's config, not a
# constant of the contract).
_min_candidate_cap_wei() {
  local rpc="$1" raw
  raw=$(lab_call_0x88 "$rpc" "$SEL_MIN_CANDIDATE_CAP") || return 1
  python3 -c "
import sys

print(int(sys.argv[1], 16))
" "$raw"
}

# _wei_to_whole_xdc_ceil <wei>: integer XDC, rounded UP if not exactly whole
# (masternode.sh's --value / lab_xdc_to_wei_hex take whole-XDC integers).
_wei_to_whole_xdc_ceil() {
  python3 -c "
import sys

wei = int(sys.argv[1])
whole, rem = divmod(wei, 10**18)
print(whole + (1 if rem else 0))
" "$1"
}

# ---------------------------------------------------------------------------
# Receipt waiting / status checks
# ---------------------------------------------------------------------------

# _wait_receipt <rpc> <txhash> <timeout_s>: prints "STATUS BLOCKNUM" once
# mined, or returns nonzero after timeout_s with nothing printed.
_wait_receipt() {
  local rpc="$1" txh="$2" timeout="${3:-120}" waited=0 receipt status blockno
  while :; do
    receipt=$(lab_rpc_call "$rpc" eth_getTransactionReceipt "[\"$txh\"]" 2>/dev/null)
    if [ -n "$receipt" ]; then
      status=$(python3 -c "
import json
import sys

r = json.loads(sys.argv[1])
print(r.get('status', ''))
" "$receipt" 2>/dev/null)
      blockno=$(python3 -c "
import json
import sys

r = json.loads(sys.argv[1])
print(int(r.get('blockNumber', '0x0'), 16))
" "$receipt" 2>/dev/null)
      printf '%s %s\n' "$status" "$blockno"
      return 0
    fi
    [ "$waited" -ge "$timeout" ] && return 1
    sleep 2
    waited=$((waited + 2))
  done
}

# _owner_has_kyc <rpc> <ownerAddr>: best-effort idempotency check for `run`
# retries. Returns 0 (has KYC) if getLatestKYC(owner) does NOT revert,
# nonzero otherwise (no KYC yet, OR the check itself failed for any other
# reason -- either way the caller's fallback is simply to call upload-kyc,
# which is harmless to repeat).
_owner_has_kyc() {
  local rpc="$1" addr="$2" word
  word=$(lab_abi_encode_address "$addr") || return 1
  lab_call_0x88 "$rpc" "${SEL_GET_LATEST_KYC}${word}" >/dev/null 2>&1
}

# _in_masternode_set <rpc> <addr>: 0 if addr is in the LIVE masternode set
# (XDPoS_getMasternodesByNumber "latest"), 1 otherwise/on error.
_in_masternode_set() {
  local rpc="$1" addr="$2" raw
  raw=$(lab_rpc_call "$rpc" XDPoS_getMasternodesByNumber "[\"latest\"]" 2>/dev/null) || return 1
  python3 -c "
import json
import sys

d = json.loads(sys.argv[1])
addr = sys.argv[2].lower()
nodes = [a.lower() for a in (d.get('Masternodes') or [])]
sys.exit(0 if addr in nodes else 1)
" "$raw" "$addr"
}

# ---------------------------------------------------------------------------
# Subcommands
# ---------------------------------------------------------------------------

cmd_plan() {
  _parse_opts "$@" || return 1
  echo "== onboard-masternode.sh plan == (read-only: no transactions, no node mutation, nothing written to disk)" >&2
  _resolve_chain_config "$RPC"
  _print_timing_table "$RPC" "$EPOCH" "$GAP" "$SWITCH_BLOCK" "$COUNT"

  local cap_wei cap_xdc
  if cap_wei=$(_min_candidate_cap_wei "$RPC"); then
    cap_xdc=$(python3 -c "import sys; print(int(sys.argv[1]) / 10**18)" "$cap_wei")
    echo "onboard-masternode.sh: live minCandidateCap() = $cap_wei wei ($cap_xdc XDC) -- this is what 'run' will fund the owner for + headroom, and the default propose() value" >&2
  else
    echo "onboard-masternode.sh: WARNING -- could not read minCandidateCap() from $RPC (eth_call to 0x88 selector $SEL_MIN_CANDIDATE_CAP failed)" >&2
  fi

  _print_preview_keys
  echo "onboard-masternode.sh: plan complete. Run './onboard-masternode.sh run --funder-from <addr> [...]' to actually onboard (or 'run --dry-run' for this same preview)." >&2
}

cmd_run() {
  DRY_RUN=""
  _parse_opts "$@" || return 1
  if [ -n "$DRY_RUN" ]; then
    cmd_plan "$@"
    return $?
  fi

  echo "== onboard-masternode.sh run ==" >&2
  _resolve_chain_config "$RPC"
  _print_timing_table "$RPC" "$EPOCH" "$GAP" "$SWITCH_BLOCK" "$COUNT"

  local cap_wei cap_xdc
  cap_wei=$(_min_candidate_cap_wei "$RPC") || _die "could not read live minCandidateCap() from $RPC -- refusing to guess a value"
  cap_xdc=$(_wei_to_whole_xdc_ceil "$cap_wei")
  echo "onboard-masternode.sh: live minCandidateCap() = $cap_wei wei ($cap_xdc whole XDC, rounded up)" >&2
  [ -n "$PROPOSE_VALUE_XDC" ] || PROPOSE_VALUE_XDC="$cap_xdc"

  # ---- 1. keys ----
  _load_or_generate_keys "$KEYFILE"

  # ---- 2. fund the owner ----
  local funder_addr funder_key required_wei owner_bal_wei top_up_wei
  if [ -n "$FUNDER_KEY_STDIN" ]; then
    IFS= read -r funder_key || _die "--funder-key-stdin given but stdin had no line"
    funder_addr=$(lab_unlock_key "$RPC" "$funder_key" "$UNLOCK_SECS" "$FUNDER_FROM") || _die "could not import/unlock funder key"
    unset funder_key
  elif [ -n "$FUNDER_FROM" ]; then
    funder_addr="$FUNDER_FROM"
  else
    _die "need --funder-from ADDR (already unlocked on the node) or --funder-key-stdin (reads a raw private key from stdin)"
  fi

  required_wei=$(python3 -c "import sys; print(int(sys.argv[1]) + int(sys.argv[2]) * 10**18)" "$cap_wei" "$HEADROOM_XDC")
  owner_bal_wei=$(lab_rpc_call "$RPC" eth_getBalance "[\"$OWNER_ADDR\",\"latest\"]" 2>/dev/null)
  owner_bal_wei=$(python3 -c "import sys; print(int(sys.argv[1], 16) if sys.argv[1] else 0)" "${owner_bal_wei:-0x0}")

  if [ "$owner_bal_wei" -ge "$required_wei" ] 2>/dev/null; then
    echo "onboard-masternode.sh: owner already funded ($owner_bal_wei wei >= required $required_wei wei) -- skipping funding" >&2
  else
    top_up_wei=$(python3 -c "import sys; print(int(sys.argv[1]) - int(sys.argv[2]))" "$required_wei" "$owner_bal_wei")
    local value_hex gas_hex txobj fund_txh result status blockno
    value_hex=$(python3 -c "import sys; print('0x%x' % int(sys.argv[1]))" "$top_up_wei")
    gas_hex="0x5208"
    txobj=$(printf '{"from":"%s","to":"%s","value":"%s","gas":"%s"}' "$funder_addr" "$OWNER_ADDR" "$value_hex" "$gas_hex")
    fund_txh=$(lab_rpc_call "$RPC" eth_sendTransaction "[${txobj}]") || _die "funding eth_sendTransaction failed (from=$funder_addr to=$OWNER_ADDR value=$top_up_wei wei)"
    echo "onboard-masternode.sh: funding tx=$fund_txh (top-up $top_up_wei wei from $funder_addr to owner $OWNER_ADDR)" >&2
    result=$(_wait_receipt "$RPC" "$fund_txh" 120) || _die "funding tx $fund_txh did not get mined within 120s"
    read -r status blockno <<<"$result"
    [ "$status" = "0x1" ] || _die "funding tx $fund_txh FAILED (status=$status) -- owner is not funded, refusing to continue"
    echo "onboard-masternode.sh: funding tx $fund_txh confirmed status=0x1 in block $blockno" >&2
  fi

  # ---- 3. uploadKYC (BEFORE propose -- see header) ----
  if _owner_has_kyc "$RPC" "$OWNER_ADDR"; then
    echo "onboard-masternode.sh: owner $OWNER_ADDR already has KYC on-chain (getLatestKYC did not revert) -- skipping uploadKYC" >&2
  else
    local kyc_out kyc_txh result status blockno
    kyc_out=$(printf '%s\n' "$OWNER_KEY" | ./masternode.sh upload-kyc "$KYC_HASH" --key-stdin --from "$OWNER_ADDR" --rpc "$RPC" --gas "$GAS" --unlock-secs "$UNLOCK_SECS") \
      || _die "masternode.sh upload-kyc failed"
    echo "onboard-masternode.sh: $kyc_out" >&2
    kyc_txh=$(printf '%s\n' "$kyc_out" | grep -oE 'tx=0x[0-9a-fA-F]+' | sed 's/tx=//')
    [ -n "$kyc_txh" ] || _die "could not parse a tx hash out of masternode.sh upload-kyc output: $kyc_out"
    result=$(_wait_receipt "$RPC" "$kyc_txh" 120) || _die "uploadKYC tx $kyc_txh did not get mined within 120s"
    read -r status blockno <<<"$result"
    [ "$status" = "0x1" ] || _die "uploadKYC tx $kyc_txh FAILED (status=$status, block=$blockno) -- refusing to propose without confirmed KYC (propose's onlyKYCWhitelisted modifier will just revert with status 0x0 and no reason otherwise -- see masternode.sh's own header note)"
    echo "onboard-masternode.sh: uploadKYC tx $kyc_txh confirmed status=0x1 in block $blockno" >&2
  fi

  # ---- 4. propose ----
  local propose_out propose_txh result status propose_block
  propose_out=$(printf '%s\n' "$OWNER_KEY" | ./masternode.sh propose "$COINBASE_ADDR" --key-stdin --from "$OWNER_ADDR" --rpc "$RPC" --value "$PROPOSE_VALUE_XDC" --gas "$GAS" --unlock-secs "$UNLOCK_SECS") \
    || _die "masternode.sh propose failed"
  echo "onboard-masternode.sh: $propose_out" >&2
  propose_txh=$(printf '%s\n' "$propose_out" | grep -oE 'tx=0x[0-9a-fA-F]+' | sed 's/tx=//')
  [ -n "$propose_txh" ] || _die "could not parse a tx hash out of masternode.sh propose output: $propose_out"
  result=$(_wait_receipt "$RPC" "$propose_txh" 120) || _die "propose tx $propose_txh did not get mined within 120s"
  read -r status propose_block <<<"$result"
  [ "$status" = "0x1" ] || _die "propose tx $propose_txh FAILED (status=$status, block=$propose_block) -- owner is likely missing KYC or under-funded; NOT seated, do not expect it to appear in any masternode set"
  echo "onboard-masternode.sh: propose tx $propose_txh confirmed status=0x1, MINED in block $propose_block" >&2

  # ---- verify: is-candidate + getCandidates (both via masternode.sh) ----
  local is_cand list_out
  is_cand=$(./masternode.sh is-candidate "$COINBASE_ADDR" --rpc "$RPC") || _die "masternode.sh is-candidate check failed"
  [ "$is_cand" = "true" ] || _die "isCandidate($COINBASE_ADDR) returned '$is_cand' after a status=0x1 propose receipt -- something is inconsistent, refusing to report success"
  list_out=$(./masternode.sh list --rpc "$RPC") || _die "masternode.sh list (getCandidates) failed"
  printf '%s\n' "$list_out" | grep -qi "^${COINBASE_ADDR}\$" \
    || _die "coinbase $COINBASE_ADDR not found in getCandidates() output after a status=0x1 propose receipt: $list_out"
  echo "onboard-masternode.sh: verified -- isCandidate($COINBASE_ADDR)=true AND present in getCandidates()" >&2

  # ---- 5. seating block (recompute from the block propose was ACTUALLY
  # MINED in, not the tip at plan time) + poll ----
  echo "onboard-masternode.sh: recomputing target switch as of the block propose was mined in ($propose_block), not the tip seen at plan time above" >&2
  _print_timing_table "$RPC" "$EPOCH" "$GAP" "$SWITCH_BLOCK" "$COUNT"
  # _print_timing_table re-reads the LIVE tip, which has moved forward
  # since propose_block; but the deadline check that matters is against
  # propose_block itself (was propose mined before ITS relevant deadline).
  # Reuse _target_switch_for_block -- the SAME search `plan` uses -- run
  # as-of propose_block instead of the live tip (see its own header comment,
  # ItWorksinMyLocal#99, for why this must not be reimplemented inline).
  local target_switch mined_deadline
  _target_switch_for_block "$propose_block" "$EPOCH" "$GAP" "$SWITCH_BLOCK" "$COUNT" \
    || _die "could not compute a target switch for propose mined in block $propose_block (see the timing table above)"
  target_switch="$TARGET_SWITCH_MARK"
  mined_deadline="$TARGET_SWITCH_DEADLINE"
  echo "onboard-masternode.sh: propose was mined in block $propose_block -- on track to seat at switch/checkpoint $target_switch, whose gapNumber deadline ($mined_deadline) is the first still-reachable one as of block $propose_block" >&2

  local state_file="${KEYFILE}.state"
  {
    echo "OWNER_ADDR=$OWNER_ADDR"
    echo "COINBASE_ADDR=$COINBASE_ADDR"
    echo "PROPOSE_TX=$propose_txh"
    echo "PROPOSE_BLOCK=$propose_block"
    echo "TARGET_SWITCH=$target_switch"
    echo "RPC=$RPC"
  } > "$state_file"
  echo "onboard-masternode.sh: recorded target state in $state_file" >&2

  echo "onboard-masternode.sh: polling live masternode set for $COINBASE_ADDR every ${POLL_INTERVAL}s (timeout ${POLL_TIMEOUT}s -- a safety valve for THIS process; seating can be far in the future, see the timing table above -- use 'status --keyfile $KEYFILE --follow' to keep checking after this returns)..." >&2
  local waited=0
  while :; do
    if _in_masternode_set "$RPC" "$COINBASE_ADDR"; then
      echo "onboard-masternode.sh: SEATED -- $COINBASE_ADDR is present in the live masternode set (XDPoS_getMasternodesByNumber)" >&2
      return 0
    fi
    [ "$waited" -ge "$POLL_TIMEOUT" ] && break
    sleep "$POLL_INTERVAL"
    waited=$((waited + POLL_INTERVAL))
  done
  echo "onboard-masternode.sh: PENDING -- not yet seated after ${POLL_TIMEOUT}s of polling. Candidate is confirmed proposed (isCandidate/getCandidates both true); target switch/checkpoint is $target_switch. Re-run: './onboard-masternode.sh status --keyfile $KEYFILE --follow'" >&2
  return 2
}

cmd_self_test() {
  echo "== onboard-masternode.sh self-test == (pure arithmetic, no RPC/node/transactions)" >&2
  _SELFTEST_FAILURES=0

  # Epoch=900, Gap=450, switch_block=0 (always V2 regime for these
  # positive block numbers) -- the exact worked example from the header
  # comment / ItWorksinMyLocal#99, verified live: S=4499 -> gapNumber 3150,
  # S=5399 -> gapNumber 4050, S=6299 -> gapNumber 4950.

  # Case 1: block % Epoch (676) > Gap (450) -- the class of block the #99
  # bug got wrong. The old inline recompute answered S=5399 (one Epoch too
  # early: its gapNumber, 4050, had already passed by block 4276); correct
  # is S=6299 (gapNumber 4950 is the first still >= 4276).
  _assert_target_switch "block % Epoch > Gap" 4276 900 450 0 6299 4950

  # Case 2: block % Epoch (0) <= Gap (450) -- the class the old inline
  # recompute happened to already get right (a single +Epoch bump landed
  # on the correct answer); must still hold after reusing the shared helper.
  _assert_target_switch "block % Epoch <= Gap" 3600 900 450 0 5399 4050

  if [ "$_SELFTEST_FAILURES" -eq 0 ]; then
    echo "onboard-masternode.sh: self-test PASS (2/2)" >&2
    return 0
  fi
  echo "onboard-masternode.sh: self-test FAILED ($_SELFTEST_FAILURES failure(s))" >&2
  return 1
}

cmd_status() {
  _parse_opts "$@" || return 1
  local coinbase owner state_file target_switch=""
  if [ -n "$COINBASE_ARG" ]; then
    coinbase="$COINBASE_ARG"
  else
    [ -f "$KEYFILE" ] || _die "no --coinbase given and $KEYFILE does not exist -- pass --keyfile FILE (from a prior 'run') or --coinbase ADDR"
    # shellcheck disable=SC1090
    source "$KEYFILE"
    coinbase="$COINBASE_ADDR"
    owner="$OWNER_ADDR"
    echo "onboard-masternode.sh: from keyfile $KEYFILE -- owner=$owner coinbase=$coinbase" >&2
  fi
  [ -n "$coinbase" ] || _die "could not determine a coinbase address to check"

  state_file="${KEYFILE}.state"
  if [ -f "$state_file" ]; then
    # shellcheck disable=SC1090
    source "$state_file"
    target_switch="${TARGET_SWITCH:-}"
    echo "onboard-masternode.sh: recorded target switch/checkpoint = ${target_switch:-unknown} (propose tx ${PROPOSE_TX:-unknown} mined in block ${PROPOSE_BLOCK:-unknown})" >&2
  fi

  local waited=0
  while :; do
    local tip is_cand in_list in_set
    tip=$(lab_block_number "$RPC") || _die "could not read current block number from $RPC"
    is_cand=$(./masternode.sh is-candidate "$coinbase" --rpc "$RPC" 2>/dev/null)
    in_list="no"
    ./masternode.sh list --rpc "$RPC" 2>/dev/null | grep -qi "^${coinbase}\$" && in_list="yes"
    in_set="no"
    _in_masternode_set "$RPC" "$coinbase" && in_set="yes"
    echo "onboard-masternode.sh: tip=$tip isCandidate=${is_cand:-unknown} inGetCandidates=$in_list inLiveMasternodeSet=$in_set" >&2
    if [ "$in_set" = "yes" ]; then
      echo "onboard-masternode.sh: SEATED -- $coinbase is present in the live masternode set" >&2
      return 0
    fi
    [ -n "$target_switch" ] && [ "$tip" -gt "$target_switch" ] 2>/dev/null \
      && echo "onboard-masternode.sh: note -- tip ($tip) is already past the recorded target switch ($target_switch) and $coinbase is still not seated; either the block-production side hasn't caught up yet, or this candidate missed its window (see 'run's own MISSED-deadline check)" >&2
    [ -n "$FOLLOW" ] || { [ "$in_set" = "yes" ] && return 0 || return 2; }
    [ "$waited" -ge "$POLL_TIMEOUT" ] && { echo "onboard-masternode.sh: PENDING -- --follow timed out after ${POLL_TIMEOUT}s, still not seated" >&2; return 2; }
    sleep "$POLL_INTERVAL"
    waited=$((waited + POLL_INTERVAL))
  done
}

main() {
  local cmd="${1:-}"
  case "$cmd" in
    ""|-h|--help) _usage; exit 0 ;;
  esac
  shift
  case "$cmd" in
    plan)       cmd_plan "$@" ;;
    run)        cmd_run "$@" ;;
    status)     cmd_status "$@" ;;
    self-test)  cmd_self_test "$@" ;;
    *) echo "unknown command: $cmd" >&2; _usage >&2; exit 1 ;;
  esac
}

main "$@"
