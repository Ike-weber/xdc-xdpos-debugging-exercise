#!/bin/bash
# Wipe all node data so the next ./run.sh re-imports keys and re-inits on the
# current genesis. Run this after regenerating genesis or changing .env keys.
for i in 1 2 3 4; do
  rm -rf "./nodes/$i/XDC" "./nodes/$i/XDCx" "./nodes/$i/keystore"
done
# Node 5 (--mixed only): modern geth, no keys of its own -- just re-init on
# the current genesis.
rm -rf "./nodes/5/XDC"
# --all followers live in ./nodes/<idx>-<client> (5-geth, 6-erigon, ...), one
# datadir per client, plus a sibling .log. Wiped whole rather than per-subdir:
# unlike nodes/1-4 they hold no keystore worth preserving, and each client lays
# its chaindata out differently (geth/xone XDC/, erigon chaindata/, besu
# database/, reth db/), so naming the subdirs would silently miss one and leave
# a follower inited on a stale genesis -- the exact failure this script exists
# to prevent.
for d in ./nodes/[0-9]*-*; do
  [ -e "$d" ] || continue          # unexpanded glob when no followers exist
  rm -rf "$d"
done
rm -f ./nodes/[0-9]*-*.log
echo "Node data wiped. Run ./run.sh to start fresh."
