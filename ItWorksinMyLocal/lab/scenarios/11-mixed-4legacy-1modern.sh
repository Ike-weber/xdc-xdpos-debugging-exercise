#!/bin/bash
# shellcheck disable=SC2034  # SCEN_* are read by scenario.sh after sourcing, not within this file
# 11-mixed-4legacy-1modern.sh - 5-node mixed topology smoke test (issue #34):
# 4 legacy oldxdc validators + node 5 (modern geth, sync-only follower),
# brought up TOGETHER from genesis via run.sh --mixed -- unlike every other
# lab scenario, node 5 here is part of the topology run.sh launches, not a
# late join.sh follower, so this scenario does NOT use
# lab_start_producer/lab_start_follower (those assume the docs/lab
# differential-oracle genesis/port registry, which this isn't).
#
# Asserts:
#   1. genesis-hash agreement: legacy node1 and modern node5 compute the
#      SAME genesis hash from the SAME genesis.json. This is the documented
#      net5151 failure mode -- a mismatch means node5 is rejected at the
#      eth/XDPoS handshake (0 peers) and never syncs -- so it is checked
#      first and is fatal on mismatch.
#   2. the 4 legacy sealers actually produce blocks from genesis.
#   3. node5 has peers > 0 and syncs 0 -> head in lockstep with a legacy
#      sealer: matching block hashes at several checkpoints, spanning the
#      epoch-900 boundary.
#
# epoch/gap are NOT tunable down for a faster test: legacy oldxdc hardcodes
# its own internal checkpoint constant (EpocBlockRandomize=900), decoupled
# from genesis.json's "epoch" field -- any other value freezes the chain at
# the first checkpoint attempt (confirmed live: "Block sealing failed:
# can't get block validator: this block is not checkpoint block", repeating
# forever; same as lab/gen-lab-genesis.sh's documented "finding F1"). So
# this scenario uses the mandatory epoch=900/gap=450 and only speeds up
# --period (block time) to 1s to make the ~900-block wait tractable.
_S11_EPOCH=900
_S11_GAP=450
_S11_PERIOD=1
_S11_TARGET=920   # > epoch -- past the checkpoint boundary

# Port bases: same knobs run.sh --mixed itself honors (BASE_P2P_PORT /
# BASE_RPC_PORT / BASE_WS_PORT), so this scenario can be moved off a
# conflicting port range exactly the way a real operator would.
_s11_rpc_port() { echo $(( ${BASE_RPC_PORT:-8544} + $1 )); }
_s11_rpc()      { printf 'http://127.0.0.1:%d\n' "$(_s11_rpc_port "$1")"; }

_S11_PIDFILE="$LAB_RESULTS_DIR/s11-run.pid"
_S11_LOG="$LAB_RESULTS_DIR/s11-run.log"
_S11_ENV_BACKUP="$LAB_RESULTS_DIR/s11-env-backup"
_S11_GENESIS_BACKUP="$LAB_RESULTS_DIR/s11-genesis-backup.json"
_S11_ROLES_BACKUP="$LAB_RESULTS_DIR/s11-roles-backup.env"

# net_peerCount (hex) -> decimal, 0 on any failure.
_s11_peer_count() {
  local url="$1" hex
  hex=$(lab_rpc_call "$url" net_peerCount "[]" 2>/dev/null) || { echo 0; return; }
  printf '%d\n' "$hex" 2>/dev/null || echo 0
}

_s11_cleanup() {
  echo "scen_11: cleaning up ..." >&2
  if [ -f "$_S11_PIDFILE" ]; then
    local pid; pid=$(cat "$_S11_PIDFILE" 2>/dev/null)
    [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null
    rm -f "$_S11_PIDFILE"
    sleep 3
    # run.sh's own trap forwards TERM to bootnode + all sealers + node5, but
    # be thorough about anything still bound to our ports before we blow
    # away ./nodes -- a stray process still holding the datadir open would
    # otherwise corrupt the next run.
    local i port
    for i in 1 2 3 4 5; do
      port=$(_s11_rpc_port "$i")
      pid=$(command -v lsof >/dev/null 2>&1 && lsof -ti "tcp:$port" -sTCP:LISTEN 2>/dev/null)
      [ -n "$pid" ] && kill -TERM $pid 2>/dev/null
    done
    sleep 1
  fi
  rm -rf "$REPO_ROOT/nodes"

  if [ -f "$_S11_ENV_BACKUP" ]; then
    cp "$_S11_ENV_BACKUP" "$REPO_ROOT/.env"; rm -f "$_S11_ENV_BACKUP"
  else
    rm -f "$REPO_ROOT/.env"
  fi
  if [ -f "$_S11_GENESIS_BACKUP" ]; then
    cp "$_S11_GENESIS_BACKUP" "$REPO_ROOT/genesis/genesis.json"; rm -f "$_S11_GENESIS_BACKUP"
  fi
  if [ -f "$_S11_ROLES_BACKUP" ]; then
    cp "$_S11_ROLES_BACKUP" "$REPO_ROOT/network-roles.env"; rm -f "$_S11_ROLES_BACKUP"
  else
    rm -f "$REPO_ROOT/network-roles.env"
  fi
}

scen_11() {
  trap _s11_cleanup RETURN

  # ---- preserve any pre-existing developer state before we clobber it ----
  [ -f "$REPO_ROOT/.env" ] && cp "$REPO_ROOT/.env" "$_S11_ENV_BACKUP"
  [ -f "$REPO_ROOT/genesis/genesis.json" ] && cp "$REPO_ROOT/genesis/genesis.json" "$_S11_GENESIS_BACKUP"
  [ -f "$REPO_ROOT/network-roles.env" ] && cp "$REPO_ROOT/network-roles.env" "$_S11_ROLES_BACKUP"

  ( cd "$REPO_ROOT" && bash ./gen-address.sh 4 --env >/dev/null ) \
    || { echo "scen_11: FAIL - gen-address.sh failed"; return 1; }

  # Ensure both binaries exist. oldxdc via the already-sourced lib.sh
  # (source "$REPO_ROOT/lib.sh" happened when scenario.sh sourced
  # lib-lab.sh); geth in a subshell so its CLIENT=geth doesn't clobber the
  # oldxdc-scoped vars (XDC_BIN etc.) used below.
  ensure_bins || { echo "scen_11: FAIL - oldxdc build/download failed"; return 1; }
  local xdc="$XDC_BIN"
  ( CLIENT=geth; source "$REPO_ROOT/lib.sh"; ensure_bins ) \
    || { echo "scen_11: FAIL - modern geth build/download failed"; return 1; }
  local modern_geth="$REPO_ROOT/bin/geth/$PLATFORM/geth"
  [ -x "$modern_geth" ] || { echo "scen_11: FAIL - modern geth binary missing at $modern_geth"; return 1; }

  # ---- derive signer addresses from the fresh .env (same recipe as
  # setup.sh's default flow) ----
  local signers="" i=1 var pk a tmp
  tmp=$(mktemp -d); : > "$tmp/pw"
  # shellcheck disable=SC1091
  set -a; source "$REPO_ROOT/.env"; set +a
  while :; do
    var="PRIVATE_KEY_$i"; pk="${!var}"
    [ -z "$pk" ] && break
    a=$("$xdc" account import --password "$tmp/pw" --datadir "$tmp/k$i" <(printf '%s' "$pk") 2>/dev/null \
          | grep -oE 'xdc[0-9a-fA-F]{40}' | head -1 | sed 's/^xdc//')
    signers="${signers:+$signers,}$a"
    i=$((i+1))
  done
  rm -rf "$tmp"
  [ -n "$signers" ] || { echo "scen_11: FAIL - could not derive signers from .env"; return 1; }
  echo "scen_11: signers=$signers epoch=$_S11_EPOCH gap=$_S11_GAP" >&2

  # --skip-v1-validation true: without it, the legacy sealers' per-block v1
  # double-validation "blocksigner" system transactions desync node5 a few
  # blocks in (a gas-accounting mismatch between modern geth and legacy
  # oldxdc on those specific transactions -- see gen-genesis.sh's doc for
  # --skip-v1-validation). Doesn't affect the genesis hash.
  # --period 1: default is 2s/block: at epoch=900 that's 30 real minutes
  # just to reach the checkpoint. 1s/block halves that.
  ( cd "$REPO_ROOT" && bash ./gen-genesis.sh --signers "$signers" --epoch "$_S11_EPOCH" --gap "$_S11_GAP" \
      --skip-v1-validation true --period "$_S11_PERIOD" >&2 ) \
    || { echo "scen_11: FAIL - gen-genesis.sh failed"; return 1; }
  ( cd "$REPO_ROOT" && bash ./reset.sh >&2 ) \
    || { echo "scen_11: FAIL - reset.sh failed"; return 1; }

  # ---- launch the mixed topology ----
  mkdir -p "$LAB_RESULTS_DIR"
  ( cd "$REPO_ROOT" && nohup ./run.sh --mixed >"$_S11_LOG" 2>&1 &
    echo $! > "$_S11_PIDFILE" )

  local ref="$(_s11_rpc 1)" node5="$(_s11_rpc 5)"
  echo "scen_11: waiting for reference (node1, $ref) RPC ..." >&2
  local waited=0
  while ! lab_rpc_call "$ref" eth_blockNumber "[]" >/dev/null 2>&1; do
    waited=$((waited + 2))
    [ "$waited" -ge 120 ] && { echo "scen_11: FAIL - node1 RPC not reachable after 120s (see $_S11_LOG)"; return 1; }
    sleep 2
  done

  # ---- 1. genesis-hash agreement (fatal on mismatch) ----
  local legacy_hash="" modern_hash=""
  waited=0
  while [ -z "$legacy_hash" ] || [ -z "$modern_hash" ]; do
    legacy_hash=$(grep -m1 'Genesis hash (legacy XDC' "$_S11_LOG" 2>/dev/null | awk '{print $NF}')
    modern_hash=$(grep -m1 'Genesis hash (modern geth' "$_S11_LOG" 2>/dev/null | awk '{print $NF}')
    waited=$((waited + 2))
    [ "$waited" -ge 60 ] && break
    { [ -z "$legacy_hash" ] || [ -z "$modern_hash" ]; } && sleep 2
  done
  echo "scen_11: genesis hash legacy(node1)=${legacy_hash:-<missing>} modern(node5)=${modern_hash:-<missing>}" >&2
  if [ -z "$legacy_hash" ] || [ -z "$modern_hash" ]; then
    echo "scen_11: FAIL - could not read one or both genesis hashes from $_S11_LOG"; return 1
  fi
  if [ "$legacy_hash" != "$modern_hash" ]; then
    echo "scen_11: FAIL - genesis hash MISMATCH legacy=$legacy_hash modern=$modern_hash (node5 will be rejected at handshake)"
    return 1
  fi

  # ---- 2. the 4 legacy sealers actually produce blocks ----
  echo "scen_11: waiting for reference to produce blocks (target=$_S11_TARGET, ~$((_S11_TARGET * _S11_PERIOD))s minimum) ..." >&2
  lab_wait_block "$ref" "$_S11_TARGET" $((_S11_TARGET * _S11_PERIOD * 2 + 300)) \
    || { echo "scen_11: FAIL - reference (node1) never reached block $_S11_TARGET"; return 1; }
  local ref_head; ref_head=$(lab_block_number "$ref")
  echo "scen_11: reference head=$ref_head" >&2

  # ---- 3. node5 peers>0 and syncs in lockstep, spanning the epoch-900
  # boundary. Note: as of the modern go-ethereum XDC fork v1.17.4-xdc.7,
  # node5 is known NOT to reliably reach this target -- its V1 XDPoS
  # difficulty verifier (consensus/XDPoS/engines/engine_v1/engine.go
  # calcDifficulty) can compute a different expected difficulty than a
  # legacy sealer validly produces for an out-of-turn block ("invalid
  # difficulty ... have=1 want=4"), permanently stalling xdcSyncer. This
  # reproduced nondeterministically as early as block 3-19 in repeated live
  # tests here, regardless of --skip-v1-validation or epoch size, so a FAIL
  # at this step is the expected, honest result against that binary -- see
  # the PR description for the full writeup. This scenario still exists to
  # catch it (and to prove it immediately once a fixed modern-geth binary
  # is available).
  echo "scen_11: waiting for node5 ($node5) to reach block $_S11_TARGET ..." >&2
  lab_wait_block "$node5" "$_S11_TARGET" $((_S11_TARGET * _S11_PERIOD * 3 + 300)) \
    || { echo "scen_11: FAIL - node5 never reached block $_S11_TARGET (peers=$(_s11_peer_count "$node5"), head=$(lab_block_number "$node5" 2>/dev/null)) -- see $_S11_LOG"; return 1; }

  local peers; peers=$(_s11_peer_count "$node5")
  echo "scen_11: node5 peers=$peers" >&2
  [ "$peers" -gt 0 ] || { echo "scen_11: FAIL - node5 has 0 peers"; return 1; }

  local node5_head; node5_head=$(lab_block_number "$node5")
  echo "scen_11: node5 head=$node5_head" >&2

  local cp h1 h5 mismatch=0
  for cp in 1 5 50 "$_S11_EPOCH" $((_S11_EPOCH + 1)) "$_S11_TARGET"; do
    h1=$(lab_block_hash "$ref" "$cp") || h1=""
    h5=$(lab_block_hash "$node5" "$cp") || h5=""
    if [ -z "$h1" ] || [ -z "$h5" ] || [ "$h1" != "$h5" ]; then
      echo "scen_11: DIFF at H=$cp legacy=${h1:-<miss>} modern=${h5:-<miss>}" >&2
      mismatch=1
    else
      echo "scen_11: MATCH H=$cp hash=$h1" >&2
    fi
  done
  [ "$mismatch" -eq 0 ] \
    || { echo "scen_11: FAIL - node5 diverged from the reference at one or more checkpoints (see DIFF lines above)"; return 1; }

  echo "scen_11: PASS - genesis hashes matched, 4 legacy sealers produced blocks 0->$ref_head, node5 (peers=$peers) synced 0->$node5_head in lockstep across epoch boundary $_S11_EPOCH"
  return 0
}
