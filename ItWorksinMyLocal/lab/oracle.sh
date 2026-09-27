#!/bin/bash
# oracle.sh - differential oracle for the XDPoS multi-client consensus lab
# (docs/lab/DESIGN.md §2/§7).
#
# For each checked block height, compares every follower client against the
# producer reference (oldxdc node1) using UNIVERSAL RPC calls only
# (eth_getBlockByNumber, eth_call to 0x88) -- never XDPoS_*, which only
# oldxdc/geth expose (docs/lab/DESIGN.md §3). Checks two things per height:
# the block hash (divergence detector) and the decoded getCandidates() set
# (validator-set oracle).
#
# Usage:
#   ./oracle.sh --from 0 --to 400
#   ./oracle.sh --from 0 --to 400 --clients "geth,erigon" --interval-blocks 10
#   ./oracle.sh --targets "node1=http://127.0.0.1:34101,node5=http://127.0.0.1:34121" --ref http://127.0.0.1:34101
#   ./oracle.sh --self-test        # pure ABI-decode check, no node required
#
# Flags:
#   --from N                first block height to check (default 0)
#   --to N                   last block height to check (default: the
#                           reference's current head at start)
#   --clients "a,b,..."      follower client ids to check (default: lab_clients)
#                           -- looked up via lab_rpc's fixed lab/ port
#                           registry; mutually exclusive with --targets.
#   --targets "name=url,..."  explicit name=url endpoints (ItWorksinMyLocal#46
#                           T2.2) instead of the fixed lab/ registry -- for a
#                           netlab topology (see lab/lib-topo.sh's topo_rpc),
#                           whose ports are NOT the lab/ fixed ones. Requires
#                           --ref (no implicit producer/node1 reference in
#                           target mode -- a topology may not even HAVE a
#                           node named "node1"). Mutually exclusive with
#                           --clients.
#   --interval-blocks K      check every Kth height (default 1)
#   --ref URL                reference RPC URL (default the producer, node1;
#                           REQUIRED when --targets is given)
#   --timeout-per-block S    how long to wait for a follower to reach a
#                           height before marking it MISS (default 60s)
#   --gas-plateau N          the network's single mint/plateau target (also
#                           env GAS_LIMIT_PLATEAU) -- when given, every
#                           checked height's gasLimit-per-block is watched
#                           as a first-class metric (ItWorksinMyLocal#94/#96)
#                           on the reference AND every client: it must never
#                           decrease block-over-block, never exceed
#                           <plateau>, and once it reaches <plateau> it must
#                           hold flat there for the rest of the run. Any
#                           violation is a DIVERGENCE (field=gaslimit), same
#                           severity as a hash/candidates mismatch -- see
#                           lab/lib-assert.sh's assert_gas_limit_plateau for
#                           the single-node version of this same check.
#                           Omitted (or 0): the gas= column still prints but
#                           the check is skipped (SKIP, never a false PASS).
#   --self-test              decode a canned getCandidates() hex blob with
#                           no live node, assert the expected address list
#
# Output contract (docs/lab/DESIGN.md §7):
#   one line per checked height:
#     H=<n> ref=<hash8> geth=<hash8|MISS|DIFF> erigon=... set=<OK|DIFF> gas=<OK|SKIP|DOWN|OVER|OFF>
#   on first divergence: "DIVERGENCE at H=<n> field=<hash|candidates|head|gaslimit>"
#   plus a diff block, then exits nonzero.
#   on a clean run: "PARITY OK <from>..<to> across <k> clients", exits 0.

_SELF_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 1
_SELF="$_SELF_DIR/$(basename "$0")"
cd "$_SELF_DIR" || exit 1
# shellcheck source=lib-lab.sh
source ./lib-lab.sh

_usage() { sed -n '2,29p' "$_SELF" | sed 's/^# \{0,1\}//'; }

# Canned getCandidates() ABI return (offset word 0x20, length word 3, then 3
# left-padded 20-byte addresses) -- decoded with NO network access, so
# --self-test never needs a live node.
_self_test() {
  local canned expected got
  canned="0000000000000000000000000000000000000000000000000000000000000020"
  canned="${canned}0000000000000000000000000000000000000000000000000000000000000003"
  canned="${canned}0000000000000000000000001111111111111111111111111111111111111111"
  canned="${canned}0000000000000000000000002222222222222222222222222222222222222222"
  canned="${canned}0000000000000000000000003333333333333333333333333333333333333333"
  expected="0x1111111111111111111111111111111111111111,0x2222222222222222222222222222222222222222,0x3333333333333333333333333333333333333333"
  got=$(lab_decode_address_array "$canned")
  if [ "$got" = "$expected" ]; then
    echo "self-test PASS: decoded $got"
    return 0
  fi
  echo "self-test FAIL: expected [$expected] got [$got]" >&2
  return 1
}

FROM=0
TO=""
CLIENTS=""
TARGETS=""
INTERVAL=1
REF=""
TIMEOUT=60
SELFTEST=0
GAS_PLATEAU="${GAS_LIMIT_PLATEAU:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --from)              FROM=$2; shift 2 ;;
    --to)                TO=$2; shift 2 ;;
    --clients)           CLIENTS=$2; shift 2 ;;
    --targets)           TARGETS=$2; shift 2 ;;
    --interval-blocks)   INTERVAL=$2; shift 2 ;;
    --ref)               REF=$2; shift 2 ;;
    --timeout-per-block) TIMEOUT=$2; shift 2 ;;
    --gas-plateau)       GAS_PLATEAU=$2; shift 2 ;;
    --self-test)         SELFTEST=1; shift ;;
    -h|--help) _usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [ "$SELFTEST" = 1 ]; then
  _self_test
  exit $?
fi

if [ -n "$TARGETS" ] && [ -n "$CLIENTS" ]; then
  echo "oracle: --targets and --clients are mutually exclusive" >&2
  exit 1
fi

client_urls=""
if [ -n "$TARGETS" ]; then
  [ -n "$REF" ] || { echo "oracle: --targets requires --ref <url> (no implicit producer/node1 reference in target mode)" >&2; exit 1; }
  CLIENTS=""
  for pair in $(printf '%s' "$TARGETS" | tr ',' ' '); do
    c="${pair%%=*}"; u="${pair#*=}"
    [ -n "$c" ] && [ -n "$u" ] && [ "$c" != "$pair" ] || { echo "oracle: invalid --targets entry '$pair' (want name=url)" >&2; exit 1; }
    client_urls="$client_urls $c=$u"
    CLIENTS="$CLIENTS $c"
  done
  CLIENTS=$(printf '%s' "$CLIENTS" | sed 's/^ //')
else
  [ -n "$REF" ] || REF=$(lab_rpc oldxdc)
  if [ -z "$CLIENTS" ]; then
    CLIENTS=$(lab_clients)
  else
    CLIENTS=$(printf '%s' "$CLIENTS" | tr ',' ' ')
  fi
  for c in $CLIENTS; do
    u=$(lab_rpc "$c") || { echo "oracle: unknown client '$c'" >&2; exit 1; }
    client_urls="$client_urls $c=$u"
  done
fi

if [ -z "$TO" ]; then
  TO=$(lab_block_number "$REF") || { echo "oracle: cannot reach reference $REF" >&2; exit 1; }
fi

# A malformed --gas-plateau (non-numeric, e.g. a hex string like 0x1908B100,
# or an explicit empty value) used to be silently coerced to 0 here, which
# is the SAME VALUE as "flag not given" (see GAS_PLATEAU's own default just
# above) -- so a typo'd/malformed invariant silently disabled the whole
# gasLimit-plateau check instead of failing loudly: the check would print
# gas=SKIP and the run would report PARITY OK, a silent pass on a malformed
# invariant. Hard-error instead; 0 (or simply omitting the flag) remains
# the explicit, intentional way to disable the check.
case "$GAS_PLATEAU" in
  ''|*[!0-9]*)
    echo "oracle: malformed --gas-plateau '$GAS_PLATEAU' (expected a non-negative integer, e.g. 42000000 -- omit the flag, or pass 0, to disable the gasLimit-plateau check)" >&2
    exit 1 ;;
esac

echo "oracle: ref=$REF clients=[$CLIENTS] range=${FROM}..${TO} step=$INTERVAL gas-plateau=${GAS_PLATEAU:-<unset, skipped>}" >&2

# ---------------------------------------------------------------------------
# _oracle_check_gas <name> <url> <height>: the gasLimit-per-block metric
# (ItWorksinMyLocal#94/#96) -- see lab/lib-assert.sh's
# assert_gas_limit_plateau for the same invariant as a standalone assertion.
# Tracks per-name state across calls via the GAS_PREV/GAS_ATPLATEAU
# associative arrays (declared just below), so a single pass over the
# height range is enough. Sets GAS_CHECK_RESULT to one of: SKIP (no
# --gas-plateau given), MISS (node doesn't have this block), OK, DOWN
# (decreased -- the #94 signature), OVER (exceeded the plateau), OFF
# (moved away after reaching it) -- called DIRECTLY (never via `$(...)`):
# a command substitution runs the function in a SUBSHELL, and bash does
# not propagate associative-array mutations back out of a subshell, which
# would silently reset GAS_PREV/GAS_ATPLATEAU to empty on every single
# call and defeat the whole monotonic check (confirmed with a standalone
# repro while building this).
# ---------------------------------------------------------------------------
declare -A GAS_PREV=() GAS_ATPLATEAU=()
GAS_CHECK_RESULT=""
_oracle_check_gas() {
  local name="$1" url="$2" height="$3" gl
  if ! [ "$GAS_PLATEAU" -gt 0 ]; then GAS_CHECK_RESULT="SKIP"; return 0; fi
  gl=$(lab_block_gas_limit "$url" "$height" 2>/dev/null) || { GAS_CHECK_RESULT="MISS"; return 0; }
  if [ -n "${GAS_PREV[$name]:-}" ] && [ "$gl" -lt "${GAS_PREV[$name]}" ] 2>/dev/null; then
    GAS_CHECK_RESULT="DOWN|${GAS_PREV[$name]}|$gl"; return 0
  fi
  if [ "$gl" -gt "$GAS_PLATEAU" ]; then
    GAS_CHECK_RESULT="OVER|$GAS_PLATEAU|$gl"; return 0
  fi
  if [ "${GAS_ATPLATEAU[$name]:-0}" = 1 ] && [ "$gl" != "$GAS_PLATEAU" ]; then
    GAS_CHECK_RESULT="OFF|$GAS_PLATEAU|$gl"; return 0
  fi
  [ "$gl" = "$GAS_PLATEAU" ] && GAS_ATPLATEAU[$name]=1
  GAS_PREV[$name]="$gl"
  GAS_CHECK_RESULT="OK"
}

h=$FROM
while [ "$h" -le "$TO" ]; do
  ref_hash=$(lab_block_hash "$REF" "$h") || ref_hash=""
  if [ -z "$ref_hash" ]; then
    echo "oracle: reference $REF is missing block $h; aborting" >&2
    exit 1
  fi
  ref_set=$(lab_get_candidates "$REF" 2>/dev/null) || ref_set=""

  line="H=$h ref=${ref_hash:2:8}"
  divergence=""
  for pair in $client_urls; do
    c="${pair%%=*}"
    u="${pair#*=}"
    lab_wait_block "$u" "$h" "$TIMEOUT" || true
    c_hash=$(lab_block_hash "$u" "$h") || c_hash=""
    if [ -z "$c_hash" ]; then
      line="$line $c=MISS"
    elif [ "$c_hash" = "$ref_hash" ]; then
      line="$line $c=${c_hash:2:8}"
    else
      line="$line $c=DIFF"
      [ -n "$divergence" ] || divergence="hash|$c|$ref_hash|$c_hash"
    fi
  done

  set_status="OK"
  if [ -z "$divergence" ]; then
    for pair in $client_urls; do
      c="${pair%%=*}"
      u="${pair#*=}"
      c_set=$(lab_get_candidates "$u" 2>/dev/null) || c_set=""
      if [ -n "$c_set" ] && [ "$c_set" != "$ref_set" ]; then
        set_status="DIFF"
        divergence="candidates|$c|$ref_set|$c_set"
        break
      fi
    done
  fi
  line="$line set=$set_status"

  # gas=<OK|SKIP|MISS|DOWN|OVER|OFF> (ItWorksinMyLocal#94/#96): checked on
  # the reference too, not just the followers -- the reference IS a
  # producer, and #94's wedge was a producer-vs-producer target mismatch,
  # not a follower-side bug. _oracle_check_gas is called directly (not via
  # `$(...)`) so its GAS_PREV/GAS_ATPLATEAU updates actually persist --
  # see its own header comment.
  _oracle_check_gas "ref" "$REF" "$h"; gas_status="$GAS_CHECK_RESULT"
  if [ -z "$divergence" ] && [ "$gas_status" != OK ] && [ "$gas_status" != SKIP ] && [ "$gas_status" != MISS ]; then
    divergence="gaslimit|ref|${gas_status#*|}"
  fi
  if [ -z "$divergence" ]; then
    for pair in $client_urls; do
      c="${pair%%=*}"
      u="${pair#*=}"
      _oracle_check_gas "$c" "$u" "$h"; c_gas_status="$GAS_CHECK_RESULT"
      if [ "$c_gas_status" != OK ] && [ "$c_gas_status" != SKIP ] && [ "$c_gas_status" != MISS ]; then
        gas_status="$c_gas_status"
        divergence="gaslimit|$c|${c_gas_status#*|}"
        break
      fi
    done
  fi
  line="$line gas=${gas_status%%|*}"
  echo "$line"

  if [ -n "$divergence" ]; then
    field="${divergence%%|*}"
    rest="${divergence#*|}"
    who="${rest%%|*}"
    rest="${rest#*|}"
    refval="${rest%%|*}"
    followerval="${rest#*|}"
    echo "DIVERGENCE at H=$h field=$field"
    echo "  client   : $who"
    echo "  reference: $refval"
    echo "  follower : $followerval"
    exit 1
  fi

  h=$((h + INTERVAL))
done

client_count=$(printf '%s\n' "$CLIENTS" | wc -w | tr -d ' ')
echo "PARITY OK ${FROM}..${TO} across ${client_count} clients"
exit 0
