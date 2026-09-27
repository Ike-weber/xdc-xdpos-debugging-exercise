#!/bin/bash
# stop.sh — GRACEFULLY stop the onboard-net validators/clients (SIGTERM only).
# NEVER SIGKILL an XDPoS validator: a hard kill corrupts the state DB and the
# node deadlocks on next start. SIGTERM lets it flush cleanly; start-oldxdc.sh /
# join-client.sh are idempotent, so you can relaunch afterwards.
#
#   BASE  working dir (default ./onboard-run)
set -u
BASE=${BASE:-./onboard-run}
pids=$(pgrep -f "datadir $BASE" ; pgrep -f "bootnode -nodekey $BASE/bootnode.key")
[ -z "$pids" ] && { echo "nothing running under $BASE"; exit 0; }
echo "SIGTERM: $pids"
kill -TERM $pids 2>/dev/null
for _ in $(seq 1 20); do
  sleep 1
  pgrep -f "datadir $BASE" >/dev/null || { echo "stopped cleanly"; exit 0; }
done
echo "WARNING: some procs still up after 20s (still flushing) — do NOT SIGKILL; wait." >&2
