#!/usr/bin/env bash
# sync-bench.sh — measure bulk-sync throughput of a v1.17.3 XDC binary
# against canonical Apothem.
#
# Purpose: guards loophole L7 from docs/port/V117_UPGRADE_LOOPHOLES.md.
# Without an objective bench, a future commit can silently regress sync
# speed from the validated peak of 500 bps (commit 00868f55 multipeer fix)
# down to 6-8 bps (pre-fix baseline) and we won't notice until the next
# wipe+restore takes 24h instead of 3 minutes.
#
# Usage:
#   sync-bench.sh [--ipc PATH] [--samples N] [--interval SEC] [--bin PATH]
#
# Reads block-height samples at fixed intervals, computes bps over each
# window, reports min/median/p95/max + overall avg. Suitable for CI on
# each merged commit OR ad-hoc check after deployment.
#
# Exit codes:
#   0 — measurement complete (bench data emitted; doesn't judge pass/fail)
#   2 — config error (no IPC, no geth)
#
# CI thresholds: a calling harness should compare median bps to a known
# baseline (see HISTORICAL_BASELINES below) and fail if regression > 30%.

set -euo pipefail

IPC="${IPC:-/mnt/data/workspace/go-ethereum/nodes-local/snapfullpath-test/geth.ipc}"
GETH="${GETH:-/root/workspace/go-ethereum/build/bin/geth}"
SAMPLES="${SAMPLES:-12}"
INTERVAL="${INTERVAL:-10}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ipc) IPC="$2"; shift 2 ;;
    --bin|--geth) GETH="$2"; shift 2 ;;
    --samples) SAMPLES="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    -h|--help) sed -n '2,/^$/p' "$0" | sed 's/^# *//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[[ -S "$IPC" ]] || { echo "FATAL: IPC missing: $IPC" >&2; exit 2; }
[[ -x "$GETH" ]] || { echo "FATAL: geth not executable: $GETH" >&2; exit 2; }

commit=$("$GETH" version 2>&1 | grep "Git Commit:" | awk '{print $3}' | cut -c1-12 || echo "unknown")
echo "=== Sync Bench ==="
echo "  binary commit: $commit"
echo "  IPC:           $IPC"
echo "  samples:       $SAMPLES × ${INTERVAL}s = $((SAMPLES * INTERVAL))s total"
echo

# Collect samples
rates_file=$(mktemp)
trap 'rm -f "$rates_file"' EXIT

prev_head=""
prev_t=""
echo "  time      head             delta    bps"
for ((i=1; i<=SAMPLES; i++)); do
  head=$("$GETH" attach --exec "eth.blockNumber" "$IPC" 2>/dev/null)
  now=$(date +%s)
  if [[ "$head" =~ ^[0-9]+$ ]]; then
    if [[ -n "$prev_head" ]]; then
      dt=$((now - prev_t))
      dh=$((head - prev_head))
      rate=$(( dh / (dt > 0 ? dt : 1) ))
      printf "  %s  %-15s  +%-7d %d\n" "$(date -u +%H:%M:%S)" "$head" "$dh" "$rate"
      echo "$rate" >> "$rates_file"
    else
      printf "  %s  %-15s  (baseline)\n" "$(date -u +%H:%M:%S)" "$head"
    fi
    prev_head=$head
    prev_t=$now
  fi
  sleep "$INTERVAL"
done

echo
echo "=== Bench Summary ==="
if [[ ! -s "$rates_file" ]]; then
  echo "  no rate samples collected"; exit 0
fi
sorted=$(sort -n "$rates_file")
n=$(echo "$sorted" | wc -l)
min=$(echo "$sorted" | head -1)
max=$(echo "$sorted" | tail -1)
median=$(echo "$sorted" | sed -n "$(( (n + 1) / 2 ))p")
p95_idx=$(( (n * 95 + 99) / 100 ))
p95=$(echo "$sorted" | sed -n "${p95_idx}p")
sum=$(echo "$sorted" | awk '{s+=$1} END {print s+0}')
avg=$(( sum / n ))

echo "  samples:   $n"
echo "  min bps:   $min"
echo "  median bps: $median"
echo "  p95 bps:   $p95"
echo "  max bps:   $max"
echo "  avg bps:   $avg"

cat <<'HISTORICAL_BASELINES'

=== Historical baselines (for reference) ===
  Pre-multipeer fix       ~6 bps    (pre-commit 00868f55)
  Post-multipeer + warm-up ~275 bps (commit 00868f55 sustained)
  Peak measured (10s)     ~661 bps  (commit 00868f55 bulk catch-up)
  At-tip natural rate     ~0.5 bps  (Apothem 2s/block)
HISTORICAL_BASELINES

# Emit JSON-like one-liner for CI parsing
echo
echo "BENCH_RESULT: commit=$commit min=$min median=$median p95=$p95 max=$max avg=$avg"
