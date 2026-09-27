#!/bin/bash
# configure-exercise-net.sh -- stamp the debugging-exercise consensus parameters
# onto the genesis that ./setup.sh --new has just generated, seat the v1 signer
# set, import the validator keys and initialise the legacy nodes.
#
# WHY THIS EXISTS
# setup.sh --new regenerates genesis/genesis.json from fresh keys via puppeth,
# so any parameter set by hand beforehand is overwritten. This runs AFTER setup
# and stamps the values the exercise depends on, so the network is the same on
# every candidate's machine regardless of puppeth's built-in defaults.
#
# The values below are the CORRECT, self-consistent ones. Nothing here is a
# deliberate defect -- the exercise's defects are in the client source, not the
# chain configuration.
#
#   period      1    1-second blocks. The stock value is 2; at 2s the v1->v2
#                    switch is 30 minutes away instead of 15.
#   minePeriod  1    the v2 engine's block time. MUST equal period, otherwise
#                    block time silently changes at the switch.
#   switchBlock 900  v1->v2 switch. 900 is the floor: it must be a whole number
#                    of epochs (epoch=900) so the validator set seated at the
#                    epoch boundary is the one v2 starts from.
#   gap         450  epoch/2. Where the validator set is snapshotted before each
#                    epoch switch. Left at its correct value.
#
# THE V1 SIGNER SET (extraData)
# puppeth seats every --signers address in genesis extraData, which is the v1
# round-robin. The modern geth client does NOT seal v1 blocks -- it defers its
# at-tip flip while localHead < switchBlock and skips minting ("bulk sync mode
# active, skipping mint"). XDPoS v1 is a strict rotation with no out-of-turn
# fallback, so a seated validator that cannot seal HALTS the chain at the first
# turn it owns. With all four seated, turn index 0 belongs to a geth node and
# the chain never produces block 1.
#
# So extraData is rewritten here to hold ONLY the two legacy validators
# (nodes/1 and nodes/2, the ones run-exercise.sh starts as oldxdc sealers).
# The four-member set the exercise needs in v2 is NOT affected: the v2 engine
# reads its validators from the epoch-switch header, which the legacy client
# fills from the masternode candidate list in the validator contract
# (0x...88) -- and puppeth seeded all four addresses there. So:
#
#   blocks 1..899  v1, rotation over the 2 legacy sealers, geth follows
#   block 900      epoch switch, header carries all 4 candidates
#   blocks 900+    v2, 4 validators, quorum ceil(0.667*4) = 3, geth load-bearing
#
# Usage:  ./configure-exercise-net.sh [--genesis PATH]
set -euo pipefail
cd "$(dirname "$0")" || exit 1

GENESIS="genesis/genesis.json"
while [ $# -gt 0 ]; do
  case "$1" in
    --genesis) GENESIS=$2; shift 2;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "configure-exercise-net.sh: unknown flag $1" >&2; exit 1;;
  esac
done

[ -f "$GENESIS" ] || { echo "no genesis at $GENESIS -- run ./setup.sh --new 4 --no-run first" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required (the harness already needs it for setup.sh/status.sh)" >&2; exit 1; }
[ -f .env ] || { echo "no .env -- run ./setup.sh --new 4 --no-run first" >&2; exit 1; }

PLATFORM="$(uname -s)-$(uname -m)"
XDC="./bin/oldxdc/$PLATFORM/XDC"
[ -x "$XDC" ] || { echo "legacy XDC binary not found at $XDC -- run ./setup.sh --new 4 --no-run first" >&2; exit 1; }

# ---- 1. consensus parameters -----------------------------------------------
python3 - "$GENESIS" <<'PY'
import json, sys

path = sys.argv[1]
with open(path) as fh:
    d = json.load(fh)

x = d["config"]["XDPoS"]
x["period"] = 1
x["epoch"]  = 900
x["gap"]    = 450

v2 = x.setdefault("v2", {})
v2["switchBlock"] = 900

# Stamp every v2 config block: `config` is the live one, and each entry of
# `allConfigs` is the one used from its switchRound onward. Setting only the
# first leaves the config internally inconsistent the moment a round advances.
targets = [v2.get("config")] + list((v2.get("allConfigs") or {}).values())
for cfg in targets:
    if not isinstance(cfg, dict):
        continue
    cfg["minePeriod"] = 1                 # == period, by rule
    cfg["certificateThreshold"] = 0.667   # 2/3 BFT quorum

with open(path, "w") as fh:
    json.dump(d, fh, indent=2)

print("exercise parameters stamped on %s:" % path)
print("  period=%s epoch=%s gap=%s switchBlock=%s" % (x["period"], x["epoch"], x["gap"], v2["switchBlock"]))
for i, cfg in enumerate(t for t in targets if isinstance(t, dict)):
    print("  v2 cfg[%d]: minePeriod=%s certificateThreshold=%s" % (i, cfg["minePeriod"], cfg["certificateThreshold"]))
PY

# ---- 2. import the validator keys in node order -----------------------------
# setup.sh derives addresses in a temp dir; the real keystores live here. Node
# order matters: run-exercise.sh starts nodes/1-2 as legacy sealers and uses
# nodes/3-4 as the geth validators' key source.
: > .pwd
while IFS= read -r _kv; do export "$_kv"; done < <(grep -E '^PRIVATE_KEY_[0-9]+=' .env)
ADDRS=""
i=1
while :; do
  var="PRIVATE_KEY_$i"; pk="${!var:-}"
  [ -z "$pk" ] && break
  if ! ls ./nodes/$i/keystore/UTC--* >/dev/null 2>&1; then
    "$XDC" account import --password .pwd --datadir "./nodes/$i" <(printf '%s' "$pk") >/dev/null 2>&1 || true
  fi
  f=$(ls ./nodes/$i/keystore/UTC--* 2>/dev/null | head -1)
  [ -n "$f" ] || { echo "configure-exercise-net.sh: could not import PRIVATE_KEY_$i into ./nodes/$i" >&2; exit 1; }
  a=$(basename "$f" | grep -oE '[0-9a-fA-F]{40}$')
  ADDRS="${ADDRS:+$ADDRS }$a"
  i=$((i+1))
done
echo "  validator keys imported: $(printf '%s' "$ADDRS" | wc -w)"

# ---- 3. seat ONLY the legacy pair in the v1 signer set ----------------------
LEGACY1=$(printf '%s' "$ADDRS" | awk '{print $1}')
LEGACY2=$(printf '%s' "$ADDRS" | awk '{print $2}')
python3 - "$GENESIS" "$LEGACY1" "$LEGACY2" <<'PY'
import json, sys
path, a, b = sys.argv[1], sys.argv[2].lower(), sys.argv[3].lower()
with open(path) as fh:
    d = json.load(fh)
ex = d["extraData"]
raw = ex[2:] if ex.startswith("0x") else ex
blob = bytes.fromhex(raw)
vanity, seal = blob[:32], blob[-65:]
old = (len(blob) - 32 - 65) // 20
# XDPoS/clique require the signer list ascending.
signers = sorted({a, b})
new = vanity + b"".join(bytes.fromhex(s) for s in signers) + seal
d["extraData"] = "0x" + new.hex()
with open(path, "w") as fh:
    json.dump(d, fh, indent=2)
print("  v1 signer set (extraData): %d -> %d" % (old, len(signers)))
for s in signers:
    print("    0x" + s)
PY

# ---- 4. re-initialise the legacy nodes on THIS genesis ----------------------
# extraData changed, so the genesis hash changed. Any chaindata initialised
# before this point is on a different chain; wipe and re-init so the legacy
# sealers and the geth validators agree. run-exercise.sh inits the geth
# datadirs itself, so removing them here is enough.
for i in 1 2 3 4; do rm -rf "./nodes/$i/XDC" "./nodes/$i/XDCx"; done
for d in ./nodes/[0-9]*-*; do [ -e "$d" ] && rm -rf "$d"; done
for i in 1 2; do
  "$XDC" --datadir "./nodes/$i" init "$GENESIS" >/dev/null 2>&1 \
    || { echo "configure-exercise-net.sh: init failed for ./nodes/$i" >&2; exit 1; }
done
echo "  legacy nodes 1,2 initialised on $GENESIS"
echo ""
echo "Next:  GETH_BIN=<your build> ./run-exercise.sh"
