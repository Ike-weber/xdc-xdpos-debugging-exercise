#!/bin/bash
# gen-lab-genesis.sh - build a lab genesis for the XDPoS multi-client
# consensus test lab (docs/lab/DESIGN.md Section 5).
#
# Starts from the repo's existing genesis/genesis.json (same masternodes /
# owner / foundation as run.sh's .env sealer keys, same 0x68/0x88 contract
# bytecode+storage) and patches only what the lab needs:
#   - epoch/gap: FORCED to epoch=900 gap=450 (finding F1 -- XDPoSChain
#     hardcodes common.EpocBlockRandomize=900, decoupled from the genesis
#     "epoch" field; any other epoch freezes the chain at the first
#     checkpoint). The originally-planned fast/epoch=180 profile is dead;
#     do not reintroduce it.
#   - period/minePeriod/timeoutPeriod and the v1->v2 switchBlock/switchEpoch,
#     all driven by the LAB_SWITCH_BLOCK / LAB_PERIOD / LAB_TIMEOUT_PERIOD
#     env vars -- the pinned contract in docs/lab/DESIGN.md Section 5.
#     Scenario files and lib-lab.sh both read these same three names, so
#     don't rename them.
#   - candidateWithdrawDelay (0x88 storage slot 0xe) -> a small value, so
#     `withdraw` is testable inside a lab run instead of needing ~30 real
#     days of blocks
#   - N freshly-generated candidate-owner accounts (default 25), each
#     prefunded with more than minCandidateCap (10,000,000 XDC), so scenario
#     06 (cap-overflow-tiebreak) has enough distinct proposers
#
# We patch a copy of the existing genesis rather than re-running puppeth's
# interactive wizard (as gen-genesis.sh does): it's simpler, fully
# deterministic (no feeding a fragile positional prompt sequence), and it
# avoids gen-genesis.sh's side effects outside lab/ (it writes
# network-roles.env and a stray NETWORK.json at the repo root). Genesis
# patching itself needs no XDC binary; deriving the candidate accounts'
# addresses from their random private keys does (same method as
# gen-address.sh), so ensure_bins still runs once.
#
# The chain id is deliberately left untouched (stays whatever
# genesis/genesis.json already has): run.sh defaults its chain id to 20118 for
# its p2p handshake regardless of the genesis file, and join.sh derives a
# follower's --networkid from the genesis chainId -- so changing it here
# would desync followers from the producer's p2p network id.
#
# Env vars (the pinned contract -- docs/lab/DESIGN.md Section 5):
#   LAB_SWITCH_BLOCK    v1->v2 switch block (default 900). Must be a
#                       multiple of 900 (the epoch length). switchEpoch is
#                       derived as LAB_SWITCH_BLOCK/900 and always written
#                       explicitly to the genesis -- it is NOT derived from
#                       a genesis file at runtime, so omitting it defaults
#                       to 0 and breaks v2 TC epoch resolution.
#   LAB_PERIOD          top-level XDPoS `period` AND v2 `minePeriod`
#                       (default 1). Must be >= 1: period:0 won't seal
#                       empty non-checkpoint blocks, and minePeriod:0 allows
#                       duplicate timestamps. Kept equal by construction.
#   LAB_TIMEOUT_PERIOD  v2 `timeoutPeriod`, in SECONDS (default 10).
#
# Usage:
#   ./gen-lab-genesis.sh
#   LAB_SWITCH_BLOCK=2700 ./gen-lab-genesis.sh --candidates 30 --out /tmp/g.json
#
# Flags:
#   --switch-block N        shorthand for setting LAB_SWITCH_BLOCK
#   --period N              shorthand for setting LAB_PERIOD
#   --timeout-period N      shorthand for setting LAB_TIMEOUT_PERIOD
#   --candidates N          number of candidate-owner accounts to prefund (default 25)
#   --withdraw-delay N      candidateWithdrawDelay in blocks (default 20)
#   --base FILE             base genesis to patch (default genesis/genesis.json)
#   --out FILE              output path (default lab/genesis-lab-sw<N>-p<N>-t<N>.json)
#   --accounts-env FILE     where to write the candidate keys (default lab/lab-accounts.env)
#
# --base/--out/--accounts-env are resolved relative to the directory this
# script was invoked from (not relative to lab/), so a plain relative path
# behaves the way you'd expect when running `./lab/gen-lab-genesis.sh ...`
# from the repo root.

SWITCH_BLOCK_ARG=""
PERIOD_ARG=""
TIMEOUT_PERIOD_ARG=""
CANDIDATES=25
WITHDRAW_DELAY=20
BASE_ARG=""
OUT_ARG=""
ACCOUNTS_ENV_ARG=""

while [ $# -gt 0 ]; do
  case "$1" in
    --switch-block)    SWITCH_BLOCK_ARG=$2; shift 2 ;;
    --period)          PERIOD_ARG=$2; shift 2 ;;
    --timeout-period)  TIMEOUT_PERIOD_ARG=$2; shift 2 ;;
    --candidates)      CANDIDATES=$2; shift 2 ;;
    --withdraw-delay)  WITHDRAW_DELAY=$2; shift 2 ;;
    --base)            BASE_ARG=$2; shift 2 ;;
    --out)             OUT_ARG=$2; shift 2 ;;
    --accounts-env)    ACCOUNTS_ENV_ARG=$2; shift 2 ;;
    -h|--help) sed -n '2,70p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

# CLI flags (if given) set the env var; otherwise an already-exported value
# (e.g. from a scenario file) or the DESIGN.md Section 5 default wins.
[ -n "$SWITCH_BLOCK_ARG" ] && LAB_SWITCH_BLOCK="$SWITCH_BLOCK_ARG"
[ -n "$PERIOD_ARG" ] && LAB_PERIOD="$PERIOD_ARG"
[ -n "$TIMEOUT_PERIOD_ARG" ] && LAB_TIMEOUT_PERIOD="$TIMEOUT_PERIOD_ARG"

LAB_SWITCH_BLOCK="${LAB_SWITCH_BLOCK:-900}"
LAB_PERIOD="${LAB_PERIOD:-1}"
LAB_TIMEOUT_PERIOD="${LAB_TIMEOUT_PERIOD:-10}"

case "$LAB_SWITCH_BLOCK" in
  ''|*[!0-9]*) echo "LAB_SWITCH_BLOCK must be a positive integer, got '$LAB_SWITCH_BLOCK'" >&2; exit 1 ;;
esac
[ "$LAB_SWITCH_BLOCK" -ge 900 ] || { echo "LAB_SWITCH_BLOCK must be >= 900 (the epoch length), got $LAB_SWITCH_BLOCK" >&2; exit 1; }
[ $((LAB_SWITCH_BLOCK % 900)) -eq 0 ] || { echo "LAB_SWITCH_BLOCK must be a multiple of 900 (epoch), got $LAB_SWITCH_BLOCK" >&2; exit 1; }

case "$LAB_PERIOD" in
  ''|*[!0-9]*) echo "LAB_PERIOD must be a positive integer, got '$LAB_PERIOD'" >&2; exit 1 ;;
esac
[ "$LAB_PERIOD" -ge 1 ] || { echo "LAB_PERIOD must be >= 1 (period:0 is a trap -- see docs/lab/DESIGN.md Section 5), got $LAB_PERIOD" >&2; exit 1; }

case "$LAB_TIMEOUT_PERIOD" in
  ''|*[!0-9]*) echo "LAB_TIMEOUT_PERIOD must be a positive integer, got '$LAB_TIMEOUT_PERIOD'" >&2; exit 1 ;;
esac
[ "$LAB_TIMEOUT_PERIOD" -ge 1 ] || { echo "LAB_TIMEOUT_PERIOD must be >= 1, got $LAB_TIMEOUT_PERIOD" >&2; exit 1; }

SWITCH_EPOCH=$((LAB_SWITCH_BLOCK / 900))

# epoch/gap are FORCED -- finding F1 (docs/lab/DESIGN.md Section 5).
EPOCH=900
GAP=450

case "$CANDIDATES" in
  ''|*[!0-9]*) echo "--candidates must be a positive integer, got '$CANDIDATES'" >&2; exit 1 ;;
esac
[ "$CANDIDATES" -ge 1 ] || { echo "--candidates must be >= 1" >&2; exit 1; }

cd "$(dirname "$0")" || exit 1
# shellcheck source=../lib.sh
source ../lib.sh
require_oldxdc
ensure_bins
XDC="$XDC_BIN"

# Resolve any user-supplied path against the caller's original directory
# (captured via $OLDPWD, set by the cd above) since this script lives in
# lab/, not the repo root.
_resolve() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *)  printf '%s/%s' "${OLDPWD:-$PWD}" "$1" ;;
  esac
}

if [ -n "$BASE_ARG" ]; then BASE=$(_resolve "$BASE_ARG"); else BASE="../genesis/genesis.json"; fi
if [ -n "$OUT_ARG" ]; then
  OUT=$(_resolve "$OUT_ARG")
else
  OUT="genesis-lab-sw${LAB_SWITCH_BLOCK}-p${LAB_PERIOD}-t${LAB_TIMEOUT_PERIOD}.json"
fi
if [ -n "$ACCOUNTS_ENV_ARG" ]; then ACCOUNTS_ENV=$(_resolve "$ACCOUNTS_ENV_ARG"); else ACCOUNTS_ENV="lab-accounts.env"; fi

[ -f "$BASE" ] || { echo "base genesis not found: $BASE" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }

echo "Building lab genesis: epoch=$EPOCH gap=$GAP switchBlock=$LAB_SWITCH_BLOCK switchEpoch=$SWITCH_EPOCH period=$LAB_PERIOD minePeriod=$LAB_PERIOD timeoutPeriod=$LAB_TIMEOUT_PERIOD withdrawDelay=$WITHDRAW_DELAY candidates=$CANDIDATES"
echo "  base=$BASE out=$OUT accounts-env=$ACCOUNTS_ENV"

TMPD=$(mktemp -d) || exit 1
trap 'rm -rf "$TMPD"' EXIT
PW="$TMPD/pw"
: > "$PW"

# ---- generate the candidate-owner accounts (same derivation as gen-address.sh) ----
ACCOUNTS_JSON="$TMPD/accounts.json"
{
  echo "["
  i=1
  first=1
  while [ "$i" -le "$CANDIDATES" ]; do
    priv=$(openssl rand -hex 32)
    addr=$("$XDC" account import --password "$PW" --datadir "$TMPD/k$i" <(printf '%s' "$priv") 2>/dev/null \
            | grep -oE 'xdc[0-9a-fA-F]{40}' | head -1 | sed 's/^xdc//' | tr '[:upper:]' '[:lower:]')
    if [ -z "$addr" ]; then
      echo "gen-lab-genesis: failed to derive an address for candidate $i" >&2
      exit 1
    fi
    [ "$first" = 1 ] || echo ","
    first=0
    printf '{"index":%d,"address":"%s","private_key":"%s"}' "$i" "$addr" "$priv"
    i=$((i + 1))
  done
  echo
  echo "]"
} > "$ACCOUNTS_JSON"

# ---- patch the genesis JSON ----
python3 - "$BASE" "$OUT" "$EPOCH" "$GAP" "$LAB_SWITCH_BLOCK" "$SWITCH_EPOCH" "$LAB_PERIOD" "$LAB_TIMEOUT_PERIOD" "$WITHDRAW_DELAY" "$ACCOUNTS_JSON" <<'PY'
import json
import sys
import time

(base, out, epoch, gap, switch_block, switch_epoch, period, timeout_period,
 withdraw_delay, accounts_json) = sys.argv[1:11]
epoch = int(epoch)
gap = int(gap)
switch_block = int(switch_block)
switch_epoch = int(switch_epoch)
period = int(period)
timeout_period = int(timeout_period)
withdraw_delay = int(withdraw_delay)

with open(base) as f:
    genesis = json.load(f)

xdpos = genesis["config"]["XDPoS"]
xdpos["period"] = period
xdpos["epoch"] = epoch
xdpos["gap"] = gap
xdpos["rewardCheckpoint"] = epoch

v2 = xdpos["v2"]
# V2.SwitchEpoch has no explicit `json:"..."` tag in XDPoSChain's params.V2
# struct, so Go's encoding/json matches it case-insensitively on decode.
# Drop any pre-existing case-variant key first so exactly one survives --
# lowercase "switchEpoch", matching genesis/genesis-5151.json and
# docs/lab/DESIGN.md Section 5 -- instead of leaving an ambiguous duplicate.
for k in list(v2.keys()):
    if k.lower() == "switchepoch":
        del v2[k]
v2["switchEpoch"] = switch_epoch
v2["switchBlock"] = switch_block
v2["config"]["minePeriod"] = period
v2["config"]["timeoutPeriod"] = timeout_period
for cfg in v2.get("allConfigs", {}).values():
    cfg["minePeriod"] = period
    cfg["timeoutPeriod"] = timeout_period

# candidateWithdrawDelay lives in the 0x88 contract's storage, slot 0xe (a
# full 32-byte left-padded word, per genesis/genesis.json's existing layout).
addr_88 = "0000000000000000000000000000000000000088"
slot_e = "0x000000000000000000000000000000000000000000000000000000000000000e"
storage = genesis["alloc"][addr_88]["storage"]
if slot_e not in storage:
    sys.stderr.write(
        "gen-lab-genesis: base genesis has no 0x88 storage slot 0xe; "
        "layout has changed, refusing to patch blind\n")
    sys.exit(1)
storage[slot_e] = "0x" + format(withdraw_delay, "064x")

# Fresh timestamp so repeated regenerations don't collide.
genesis["timestamp"] = "0x%x" % int(time.time())

# Prefund the candidate-owner accounts: > minCandidateCap (10,000,000 XDC)
# each, with headroom for gas plus a subsequent vote() call.
balance_xdc = 20_000_000
balance_hex = "0x%x" % (balance_xdc * 10**18)
with open(accounts_json) as f:
    accounts = json.load(f)
for acc in accounts:
    genesis["alloc"][acc["address"]] = {"balance": balance_hex}

with open(out, "w") as f:
    json.dump(genesis, f, indent=2)

sys.stderr.write("gen-lab-genesis: wrote %s (%d candidate accounts, %s XDC each)\n" %
                  (out, len(accounts), format(balance_xdc, ",")))
PY
[ -f "$OUT" ] || { echo "gen-lab-genesis: failed to write $OUT" >&2; exit 1; }

# ---- write lab-accounts.env ----
{
  echo "# Lab candidate-owner accounts (throwaway, generated by gen-lab-genesis.sh)."
  echo "# Each account self-proposes as both candidate and owner. Git-ignored."
  echo "LAB_CANDIDATE_COUNT=$CANDIDATES"
  python3 -c "
import json
import sys

with open(sys.argv[1]) as f:
    accs = json.load(f)
for a in accs:
    print('LAB_CANDIDATE_%d_ADDR=0x%s' % (a['index'], a['address']))
    print('LAB_CANDIDATE_%d_KEY=%s' % (a['index'], a['private_key']))
print('LAB_CANDIDATE_ADDRS=' + ','.join('0x' + a['address'] for a in accs))
" "$ACCOUNTS_JSON"
} > "$ACCOUNTS_ENV"
chmod 600 "$ACCOUNTS_ENV" 2>/dev/null || true

echo "OK: wrote $OUT"
echo "OK: wrote $ACCOUNTS_ENV ($CANDIDATES candidate-owner accounts)"
