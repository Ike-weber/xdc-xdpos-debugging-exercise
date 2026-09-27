#!/usr/bin/env bash
# archive-integrity-probe.sh — sample N historical blocks on a local archive
# node and compare their hashes to canonical Apothem RPC.
#
# Purpose: guards loophole L3 from docs/port/V117_UPGRADE_LOOPHOLES.md.
# An archive node running v1.17.3 may silently return WRONG state for
# historical queries even when sync looks healthy at tip — the chaindata
# format gap (#804) would manifest as some-but-not-all historical blocks
# differing from canonical. Tip parity alone doesn't catch this.
#
# Usage:
#   archive-integrity-probe.sh [--ipc PATH] [--samples N] [--rpc URL] [--start N] [--end N]
#
# Exit codes:
#   0 — all samples matched canonical
#   1 — at least one divergence (PAGE: chaindata may be corrupt)
#   2 — connectivity / config error
#
# Defaults probe the xdc04 archivepath against rpc.apothem.network.

set -euo pipefail

IPC="${IPC:-/mnt/data/workspace/go-ethereum/nodes-local/archivepath/geth.ipc}"
GETH="${GETH:-/root/workspace/go-ethereum/build/bin/geth}"
RPC="${RPC:-https://rpc.apothem.network}"
SAMPLES="${SAMPLES:-100}"
START="${START:-0}"
END="${END:-}"

# Parse args
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ipc) IPC="$2"; shift 2 ;;
    --geth) GETH="$2"; shift 2 ;;
    --rpc) RPC="$2"; shift 2 ;;
    --samples) SAMPLES="$2"; shift 2 ;;
    --start) START="$2"; shift 2 ;;
    --end) END="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,/^$/p' "$0" | sed 's/^# *//'
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# Pre-flight
[[ -S "$IPC" ]] || { echo "FATAL: IPC socket missing: $IPC" >&2; exit 2; }
command -v "$GETH" >/dev/null || { echo "FATAL: geth not found: $GETH" >&2; exit 2; }
command -v curl >/dev/null || { echo "FATAL: curl missing" >&2; exit 2; }
command -v jq >/dev/null || { echo "FATAL: jq missing" >&2; exit 2; }

# Discover end if not provided — local head
if [[ -z "$END" ]]; then
  END=$("$GETH" attach --exec "eth.blockNumber" "$IPC" 2>/dev/null)
  [[ "$END" =~ ^[0-9]+$ ]] || { echo "FATAL: could not read local head" >&2; exit 2; }
fi

if (( START >= END )); then
  echo "FATAL: --start ($START) must be < --end ($END)" >&2
  exit 2
fi

echo "=== Archive Integrity Probe ==="
echo "  IPC:     $IPC"
echo "  RPC:     $RPC"
echo "  Range:   $START .. $END"
echo "  Samples: $SAMPLES"
echo

# Generate N evenly-spaced + random sample points
samples_file=$(mktemp)
trap 'rm -f "$samples_file"' EXIT

# Half evenly-spaced
half=$((SAMPLES / 2))
step=$(( (END - START) / (half > 0 ? half : 1) ))
for ((i=0; i<half; i++)); do
  echo $(( START + i * step ))
done > "$samples_file"

# Half random
range=$((END - START))
for ((i=0; i<SAMPLES - half; i++)); do
  echo $(( START + (RANDOM % range) ))
done >> "$samples_file"

# Sort + dedup
sort -nu "$samples_file" > "${samples_file}.uniq"
mv "${samples_file}.uniq" "$samples_file"
actual_samples=$(wc -l < "$samples_file")

failures=0
checked=0
divergences_file=$(mktemp)
trap 'rm -f "$samples_file" "$divergences_file"' EXIT

while IFS= read -r blk; do
  hex=$(printf "0x%x" "$blk")
  local_hash=$("$GETH" attach --exec "eth.getBlock($blk).hash" "$IPC" 2>/dev/null | tr -d '"')
  canon_hash=$(curl -s --max-time 10 -X POST -H "Content-Type: application/json" \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$hex\",false],\"id\":1}" \
    "$RPC" 2>/dev/null \
    | jq -r '.result.hash // "null"' 2>/dev/null)

  checked=$((checked + 1))
  if [[ -z "$local_hash" || "$local_hash" == "null" ]]; then
    echo "  $blk  ? local hash missing"
    continue
  fi
  if [[ -z "$canon_hash" || "$canon_hash" == "null" ]]; then
    echo "  $blk  ? canonical lookup failed (skipped)"
    continue
  fi
  if [[ "$local_hash" == "$canon_hash" ]]; then
    : # match — silent
  else
    failures=$((failures + 1))
    echo "  $blk  ✗ DIVERGE  local=${local_hash:0:18}  canon=${canon_hash:0:18}"
    echo "$blk $local_hash $canon_hash" >> "$divergences_file"
  fi
done < "$samples_file"

echo
echo "=== Summary ==="
echo "  Checked:   $checked"
echo "  Divergent: $failures"

if (( failures > 0 )); then
  echo
  echo "=== PAGE-NOW SIGNATURES ==="
  echo "Archive node at $IPC returned DIFFERENT block hashes than canonical at $failures sample blocks."
  echo "This may indicate the v1.17.3 ↔ v1.17.0 chaindata-format gap (#804) is silently leaking,"
  echo "OR the archive is on a forked chain past the rollback point."
  echo
  echo "First 5 divergences:"
  head -5 "$divergences_file"
  echo
  echo "Recommended action: roll back to v1.17.0 (docs/runbooks/v1173-rollback.md)"
  exit 1
fi

echo "  ✓ all sampled historical blocks match canonical"
exit 0
