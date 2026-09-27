#!/usr/bin/env bash
# snapshot-daily.sh — pause a geth instance, tar+zstd its chaindata,
# then resume. Keeps last 3 snapshots in /mnt/data/snapshots/.
#
# Purpose: guards loophole L2 from docs/port/V117_UPGRADE_LOOPHOLES.md.
# Without daily snapshots, snapshot age drifts and a future restore takes
# longer (sync forward from older block). The xdc-network snapshot at
# May 23 means each subsequent day adds ~43k blocks to re-sync after wipe.
#
# Usage:
#   snapshot-daily.sh --datadir <path> --label <name> [--bin <path>]
#                     [--snapdir /mnt/data/snapshots] [--keep 3]
#
# Suggested cron entry (xdc04, daily at 03:00 UTC):
#   0 3 * * * /root/workspace/go-ethereum/scripts/snapshot-daily.sh \
#               --datadir /mnt/data/workspace/go-ethereum/nodes-local/archivepath \
#               --label archivepath \
#               >> /root/workspace/go-ethereum/logs/snapshot-daily.log 2>&1
#
# IMPORTANT: this script does NOT stop geth. It assumes pathdb's
# state-history journal is flushed periodically and the tar-while-running
# is acceptable (small risk of inconsistent snapshot if a write straddles
# the tar). For perfect consistency, stop geth before invoking — but that
# means downtime. Operator choice.
#
# For archive nodes specifically, tar-while-running is acceptable because:
#   - archive writes all historical state once; no in-place mutation
#   - The state-history journal commits at fixed-block intervals
#   - The risk window is at most one journal commit
# For snap/full mode with active state pruning, prefer stop-before-snap.

set -euo pipefail

DATADIR=""
LABEL=""
GETH="${GETH:-/root/workspace/go-ethereum/build/bin/geth}"
SNAPDIR="${SNAPDIR:-/mnt/data/snapshots}"
KEEP="${KEEP:-3}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --datadir) DATADIR="$2"; shift 2 ;;
    --label) LABEL="$2"; shift 2 ;;
    --bin|--geth) GETH="$2"; shift 2 ;;
    --snapdir) SNAPDIR="$2"; shift 2 ;;
    --keep) KEEP="$2"; shift 2 ;;
    -h|--help) sed -n '2,/^$/p' "$0" | sed 's/^# *//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[[ -z "$DATADIR" ]] && { echo "FATAL: --datadir required" >&2; exit 2; }
[[ -z "$LABEL" ]] && { echo "FATAL: --label required" >&2; exit 2; }
[[ -d "$DATADIR" ]] || { echo "FATAL: datadir missing: $DATADIR" >&2; exit 2; }
[[ -d "$SNAPDIR" ]] || mkdir -p "$SNAPDIR"

# Find IPC for the active geth at this datadir, get current head (for filename)
ipc="$DATADIR/geth.ipc"
head=""
if [[ -S "$ipc" ]] && [[ -x "$GETH" ]]; then
  head=$("$GETH" attach --exec "eth.blockNumber" "$ipc" 2>/dev/null || echo "")
fi
[[ -n "$head" && "$head" =~ ^[0-9]+$ ]] || head="unknown"

git_commit=$("$GETH" version 2>&1 | grep "Git Commit:" | awk '{print $3}' | cut -c1-8 || echo "unknown")
date_tag=$(date -u +%Y%m%dT%H%M%SZ)
out="$SNAPDIR/${LABEL}-${git_commit}-${head}-${date_tag}.tar.zst"

echo "[snapshot-daily] starting"
echo "  datadir:  $DATADIR"
echo "  label:    $LABEL"
echo "  head:     $head"
echo "  out:      $out"
echo "  snapdir:  $SNAPDIR (keep=$KEEP)"

# Pre-flight: enough disk?
need_kb=$(du -sk "$DATADIR" | awk '{print $1}')
have_kb=$(df -k "$SNAPDIR" | awk 'NR==2 {print $4}')
# Need ~70% of source size for compressed snapshot (zstd ratio observed: 0.7×)
need_compressed_kb=$(( need_kb * 7 / 10 ))
if (( have_kb < need_compressed_kb )); then
  echo "FATAL: insufficient disk on $SNAPDIR: need ${need_compressed_kb} KB, have ${have_kb} KB" >&2
  exit 2
fi

# Tar + zstd. Skip the LOCK file (geth's pidfile) and any *.tmp.
echo "[snapshot-daily] taring $(du -sh "$DATADIR" | awk '{print $1}') of source"
tar --exclude='*.tmp' --exclude='LOCK' \
    -cf - -C "$(dirname "$DATADIR")" "$(basename "$DATADIR")" \
    | zstd -3 -T0 -o "$out"

# Verify the snapshot is non-empty
size=$(stat -c%s "$out")
if (( size < 1024 )); then
  echo "FATAL: snapshot too small ($size bytes) — likely corrupt" >&2
  rm -f "$out"
  exit 2
fi
echo "[snapshot-daily] done: $(du -sh "$out" | awk '{print $1}')"

# Prune: keep most recent $KEEP for this label
echo "[snapshot-daily] pruning: keeping last $KEEP for label=$LABEL"
ls -1t "$SNAPDIR/${LABEL}-"*.tar.zst 2>/dev/null \
  | tail -n +"$((KEEP + 1))" \
  | while read -r old; do
      echo "  rm $old"
      rm -f "$old"
    done

echo "[snapshot-daily] complete"
