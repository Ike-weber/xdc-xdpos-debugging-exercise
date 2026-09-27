#!/bin/bash
# lib-lab.sh - shared helpers for the XDPoS multi-client consensus test lab.
# Sourced by every lab/*.sh script (see docs/lab/DESIGN.md §7 for the full
# contract). Builds on the repo's existing lib.sh/run.sh/join.sh rather than
# reinventing launch or build logic: lab_start_producer wraps run.sh,
# lab_start_follower wraps join.sh.
#
# Client registry / ports (docs/lab/DESIGN.md §3):
#   reference  oldxdc(node1)  rpc 8545  ws 8555  p2p 30303  (run.sh)
#   follower   geth           rpc 8605  ws 8606  p2p 30323  (join.sh --client geth)
#   follower   erigon         rpc 8615  ws 8616  p2p 30333  (join.sh --client erigon)
#   follower   besu           rpc 8625  ws 8626  p2p 30343  (join.sh --client besu)
#   follower   nethermind     rpc 8635  ws 8636  p2p 30353  (join.sh --client nethermind)
#   follower   reth           rpc 8645  ws 8646  p2p 30363  (join.sh --client reth)
#
# HARD CONSTRAINT: nothing in here may depend on XDPoS_* RPC methods for
# cross-client comparisons -- only oldxdc/geth expose that namespace. All
# cross-client reads use eth_getBlockByNumber/eth_getBlockByHash/eth_call
# (to 0x88)/eth_getStorageAt/web3_clientVersion, per docs/lab/DESIGN.md §3.

_LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
LAB_DIR="$_LAB_DIR"
REPO_ROOT="$(cd "$LAB_DIR/.." && pwd)" || exit 1
LAB_RESULTS_DIR="$LAB_DIR/results"
mkdir -p "$LAB_RESULTS_DIR" 2>/dev/null || true

# Reuse the repo's existing helpers (detect_ip, ensure_bins, PLATFORM, ...)
# rather than reimplementing them.
# shellcheck source=../lib.sh
source "$REPO_ROOT/lib.sh"

# Topology-independent RPC/ABI helpers (lab_rpc_call, lab_block_number,
# lab_block_hash, lab_wait_block, lab_abi_encode_*, lab_decode_address_array,
# lab_call_0x88, lab_get_candidates, LAB_MASTERNODE_CONTRACT) live in
# lib-rpc.sh (ItWorksinMyLocal#46 T0.1) so the netlab/ topology engine can
# reuse them without this file's fixed-topology bits. Function names
# unchanged -- every existing caller of lib-lab.sh keeps working as-is.
# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

# ---------------------------------------------------------------------------
# Client registry
# ---------------------------------------------------------------------------

# lab_clients: echoes the client ids configured this run (subset of the 6),
# space-separated. Override with LAB_CLIENTS="geth,erigon" (comma-separated).
# Defaults to the 5 followers (the producer/reference is addressed
# separately via `lab_rpc oldxdc`).
lab_clients() {
  local list="${LAB_CLIENTS:-geth,erigon,besu,nethermind,reth}"
  printf '%s' "$list" | tr ',' ' '
}

# lab_rpc <client>: echoes that client's RPC URL.
lab_rpc() {
  case "$1" in
    oldxdc|ref|reference) printf 'http://127.0.0.1:8545\n' ;;
    geth)                  printf 'http://127.0.0.1:8605\n' ;;
    erigon)                printf 'http://127.0.0.1:8615\n' ;;
    besu)                  printf 'http://127.0.0.1:8625\n' ;;
    nethermind)            printf 'http://127.0.0.1:8635\n' ;;
    reth)                  printf 'http://127.0.0.1:8645\n' ;;
    *) echo "lab_rpc: unknown client '$1'" >&2; return 1 ;;
  esac
}

# _lab_producer_rpc_port <node1|node2|node3|node4>: the run.sh RPC port for
# that individual sealer (used for per-node admin/partition control).
_lab_producer_rpc_port() {
  case "$1" in
    node1) echo 8545 ;; node2) echo 8546 ;; node3) echo 8547 ;; node4) echo 8548 ;;
    *) echo "_lab_producer_rpc_port: unknown node '$1'" >&2; return 1 ;;
  esac
}

# lab_xdc_to_wei_hex <XDC-amount>: 0x-prefixed hex wei value for a decimal
# XDC amount (arbitrary size; XDC has 18 decimals like ether). Stays here
# (not lib-rpc.sh): plain unit-conversion utility used only by
# lab/masternode.sh, not an RPC/ABI transport helper.
lab_xdc_to_wei_hex() {
  python3 -c "
import sys

print('0x%x' % (int(sys.argv[1]) * 10**18))
" "$1"
}

# lab_unlock_key <url> <hex-private-key> [unlock-secs] [known-address]:
# imports a raw private key via personal_importRawKey and unlocks it,
# printing the resulting address. Used by masternode.sh to drive txs from a
# lab candidate-owner account (see lab/lab-accounts.env).
#
# personal_importRawKey on this client errors ("account already exists")
# the second time the SAME key is imported -- which happens routinely, since
# every masternode.sh invocation is a fresh process (e.g. upload-kyc then
# propose for the same account). Pass the address as $4 (masternode.sh does
# this whenever both --key and --from are given) to skip re-import entirely
# for the address and make re-import best-effort; without it, the address
# comes from personal_importRawKey's response, which only works the first
# time a given key is used.
lab_unlock_key() {
  local url="$1" key="${2#0x}" secs="${3:-300}" known_addr="${4:-}" addr
  if [ -n "$known_addr" ]; then
    addr="$known_addr"
    lab_rpc_call "$url" personal_importRawKey "[\"${key}\",\"\"]" >/dev/null 2>&1 || true
  else
    addr=$(lab_rpc_call "$url" personal_importRawKey "[\"${key}\",\"\"]") || return 1
  fi
  lab_rpc_call "$url" personal_unlockAccount "[\"${addr}\",\"\",${secs}]" >/dev/null || return 1
  printf '%s\n' "$addr"
}

# ---------------------------------------------------------------------------
# Node enode / peer helpers (producer sealers only -- they carry the admin
# API; see docs/lab/DESIGN.md §3/§6)
# ---------------------------------------------------------------------------

# _lab_node_enode <node1..node4>: that sealer's own enode (admin_nodeInfo).
_lab_node_enode() {
  local node="$1" port url info
  port=$(_lab_producer_rpc_port "$node") || return 1
  url="http://127.0.0.1:${port}"
  info=$(lab_rpc_call "$url" admin_nodeInfo "[]") || return 1
  python3 -c "
import json
import sys

print(json.loads(sys.argv[1]).get('enode', ''))
" "$info"
}

# _lab_producer_enodes: comma-joined enodes of all 4 producer sealers.
# Needed as --bootnodes for besu/nethermind/reth followers, which (per
# join.sh) never derive a bootnode from ./bootnode.key and instead dial the
# validator enodes directly.
_lab_producer_enodes() {
  local i e out=""
  for i in 1 2 3 4; do
    e=$(_lab_node_enode "node${i}") || return 1
    [ -n "$e" ] || return 1
    if [ -n "$out" ]; then out="${out},${e}"; else out="${e}"; fi
  done
  printf '%s\n' "$out"
}

# lab_peers <node>: comma-joined enode list of that producer node's current
# peers (admin_peers). $1: node1..node4. admin_peers entries carry no
# top-level "enode" field -- just "id" + "network.remoteAddress" -- so this
# reconstructs the enode URL the same way lib.sh's harvest_peers does.
lab_peers() {
  local node="$1" port url peers
  port=$(_lab_producer_rpc_port "$node") || return 1
  url="http://127.0.0.1:${port}"
  peers=$(lab_rpc_call "$url" admin_peers "[]") || return 1
  python3 -c "
import json
import sys

ps = json.loads(sys.argv[1]) or []
out = []
for p in ps:
    pid = p.get('id')
    addr = (p.get('network') or {}).get('remoteAddress')
    if pid and addr:
        out.append('enode://%s@%s' % (pid, addr))
print(','.join(out))
" "$peers"
}

# _lab_toggle_peers <add|remove> <groupA nodes> <groupB nodes>: calls
# admin_<action>Peer across every A<->B pair (full split/heal), using each
# node's own admin RPC.
_lab_toggle_peers() {
  local action="$1" group_a="$2" group_b="$3" a b rpc_a rpc_b enode_a enode_b port_a port_b
  for a in $group_a; do
    port_a=$(_lab_producer_rpc_port "$a") || return 1
    rpc_a="http://127.0.0.1:${port_a}"
    enode_a=$(_lab_node_enode "$a") || return 1
    for b in $group_b; do
      port_b=$(_lab_producer_rpc_port "$b") || return 1
      rpc_b="http://127.0.0.1:${port_b}"
      enode_b=$(_lab_node_enode "$b") || return 1
      lab_rpc_call "$rpc_a" "admin_${action}Peer" "[\"${enode_b}\"]" >/dev/null
      lab_rpc_call "$rpc_b" "admin_${action}Peer" "[\"${enode_a}\"]" >/dev/null
    done
  done
}

# lab_partition <groupA-nodes> <groupB-nodes>: splits the sealer set into
# two groups via admin_removePeer (comma- or space-separated node1..node4
# lists, e.g. "node1,node2" "node3,node4"). Records the split so lab_heal
# knows what to reconnect.
lab_partition() {
  local group_a group_b
  group_a=$(printf '%s' "$1" | tr ',' ' ')
  group_b=$(printf '%s' "$2" | tr ',' ' ')
  mkdir -p "$LAB_RESULTS_DIR" || return 1
  _lab_toggle_peers remove "$group_a" "$group_b" || return 1
  printf '%s|%s\n' "$group_a" "$group_b" > "$LAB_RESULTS_DIR/partition.state"
}

# lab_heal: reconnects the most recent lab_partition split via
# admin_addPeer.
lab_heal() {
  [ -f "$LAB_RESULTS_DIR/partition.state" ] || { echo "lab_heal: no active partition to heal" >&2; return 1; }
  local line group_a group_b
  line=$(cat "$LAB_RESULTS_DIR/partition.state")
  group_a="${line%%|*}"
  group_b="${line#*|}"
  _lab_toggle_peers add "$group_a" "$group_b" || return 1
  rm -f "$LAB_RESULTS_DIR/partition.state"
}

# ---------------------------------------------------------------------------
# Producer / follower lifecycle
# ---------------------------------------------------------------------------

# _lab_pid_on_port <port>: PID of the process LISTENing on that TCP port
# (empty if none). Used for per-node stop/restart, since run.sh manages all
# 4 sealers as one process group with no per-node control of its own.
_lab_pid_on_port() {
  command -v lsof >/dev/null 2>&1 || return 1
  lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null | head -1
}

# _lab_ensure_root_genesis_signers: run.sh's 4 sealers (NUM_NODES=4) are
# whichever accounts .env's PRIVATE_KEY_1..4 import to. genesis/genesis.json
# is a tracked file whose baked-in extraData masternode set comes from
# whatever --signers gen-genesis.sh was last run with (or its own hardcoded
# default) -- nothing keeps it in sync with .env, which is gitignored and
# gets swapped wholesale between environments/sessions. A mismatch there is
# fatal but silent: run.sh happily imports+seals with the .env keys and
# inits every node on the mismatched genesis, then the chain wedges forever
# at block 0 because none of the actual sealers are in the authorized
# masternode set -- exactly the "Blocks stop / stuck at an epoch" failure
# README.md's own FAQ already documents, and reproduced live in
# ItWorksinMyLocal#46 Phase 2 acceptance (node1 spinning forever on "Failed
# to retrieve block author err=recovery failed number=0").
#
# lab_start_producer's own genesis-lab-*.json cache (below) patches FROM
# genesis/genesis.json and preserves its masternode set unchanged, so a
# stale root genesis silently poisons the lab genesis too, indefinitely
# (that cache is only regenerated "if missing"). So fix the root cause
# before touching the cache: verify the 4 addresses baked into
# genesis/genesis.json's extraData match what .env's PRIVATE_KEY_1..4
# actually import to, and regenerate genesis/genesis.json (via the repo's
# own gen-genesis.sh, exactly as setup.sh's dynamic-signer derivation
# already does) if they don't. A fixed --timestamp keeps this
# deterministic/assertable. On a genuine rewrite, any cached lab genesis
# is now equally stale and gets dropped along with the active-config
# marker so the next lab_start_producer call regenerates everything
# against the corrected base.
_lab_ensure_root_genesis_signers() {
  local root_genesis="$REPO_ROOT/genesis/genesis.json"
  [ -f "$root_genesis" ] && [ -f "$REPO_ROOT/.env" ] || return 0

  require_oldxdc
  ensure_bins
  local xdc="$XDC_BIN"

  set -a
  # shellcheck disable=SC1091
  source "$REPO_ROOT/.env"
  set +a

  local tmp i pk_var pk a want=""
  tmp=$(mktemp -d) || return 1
  : > "$tmp/pw"
  for i in 1 2 3 4; do
    pk_var="PRIVATE_KEY_$i"; pk="${!pk_var}"
    if [ -z "$pk" ]; then
      echo "_lab_ensure_root_genesis_signers: $pk_var not set in .env" >&2
      rm -rf "$tmp"; return 1
    fi
    a=$("$xdc" account import --password "$tmp/pw" --datadir "$tmp/k$i" <(printf '%s' "$pk") 2>/dev/null \
          | grep -oE 'xdc[0-9a-fA-F]{40}' | head -1 | sed 's/^xdc//' | tr 'A-F' 'a-f')
    if [ -z "$a" ]; then
      echo "_lab_ensure_root_genesis_signers: could not derive an address for $pk_var" >&2
      rm -rf "$tmp"; return 1
    fi
    want="$want $a"
  done
  rm -rf "$tmp"
  want=$(printf '%s\n' $want | sort)

  local have
  have=$(python3 -c "
import json, sys
d = json.load(open(sys.argv[1]))
hexs = d['extraData'][2:]
rest = hexs[64:]
addrs = rest[:-130]
for i in range(0, len(addrs), 40):
    print(addrs[i:i+40].lower())
" "$root_genesis" 2>/dev/null | sort)

  [ "$want" = "$have" ] && return 0

  echo "_lab_ensure_root_genesis_signers: $root_genesis's masternode set doesn't match .env's PRIVATE_KEY_1..4 -- regenerating via gen-genesis.sh (same derivation setup.sh uses)" >&2
  local signers="" w
  for w in $want; do signers="${signers:+$signers,}$w"; done
  ( cd "$REPO_ROOT" && ./gen-genesis.sh --signers "$signers" --timestamp 0x0 ) || return 1

  rm -f "$LAB_DIR"/genesis-lab-*.json
  rm -f "$LAB_RESULTS_DIR/active-config" "$LAB_RESULTS_DIR/genesis-orig-backup.json"
}

# lab_start_producer: brings up the run.sh 4-node oldxdc net using the lab
# genesis for the current LAB_SWITCH_BLOCK/LAB_PERIOD/LAB_TIMEOUT_PERIOD env
# vars (the pinned contract in docs/lab/DESIGN.md Section 5; scenarios
# `export` these before calling in), generating it first via
# gen-lab-genesis.sh if missing. Reuses run.sh unmodified, which hardcodes
# ./genesis/genesis.json and ./nodes/<i> -- so this stages the lab genesis
# into genesis/genesis.json (backing up the original once) and wipes
# ./nodes/* via reset.sh whenever the active config changes, forcing a fresh
# init against the new genesis.
lab_start_producer() {
  local switch_block period timeout_period config_key genesis
  switch_block="${LAB_SWITCH_BLOCK:-900}"
  period="${LAB_PERIOD:-1}"
  timeout_period="${LAB_TIMEOUT_PERIOD:-10}"
  config_key="sw${switch_block}-p${period}-t${timeout_period}"

  mkdir -p "$LAB_RESULTS_DIR" || return 1

  # Back up the CURRENTLY-TRACKED genesis/genesis.json BEFORE anything else
  # in this function can touch it (ItWorksinMyLocal#46 T3.3 residual fix).
  # _lab_ensure_root_genesis_signers (next) rewrites this exact file in
  # place whenever .env's PRIVATE_KEY_1..4 don't match its baked-in
  # masternode set -- the common case, since .env is gitignored and gets
  # swapped per environment/session while the tracked genesis is committed
  # once. Taking the backup AFTER that call (the previous order here)
  # captured the ALREADY-MUTATED file, not the git-tracked original -- so
  # lab_teardown's restore put back the wrong snapshot, permanently, with
  # no way back except `git checkout`. See lab_teardown's own comment for
  # the other half of this same bug (a stale backup being reapplied to
  # runs that never touched the genesis at all).
  local root_genesis="$REPO_ROOT/genesis/genesis.json"
  local backup="$LAB_RESULTS_DIR/genesis-orig-backup.json"
  local active_marker="$LAB_RESULTS_DIR/active-config"
  # A backup file with NO matching active-config marker is ORPHANED --
  # active-config is set/cleared in lockstep with a live overlay (see
  # below and lab_teardown), so its absence means whatever overlay that
  # backup belonged to was already restored (or never completed -- e.g. an
  # even older, already-fixed version of this same bug), and this backup
  # is leftover debris, not a live snapshot to preserve. Confirmed live
  # (ItWorksinMyLocal#46 T3.3 residual, round 2): an ancient
  # genesis-orig-backup.json that predated this whole session -- with no
  # active-config alongside it -- got silently REUSED here (the `[ ! -f
  # backup ]` guard alone saw it as "already backed up, skip") instead of
  # being recognised as stale, so THIS run's fresh, correctly-ordered
  # backup step was skipped entirely, and teardown later restored that
  # ancient, wrong snapshot over a genuinely clean tracked genesis. Discard
  # an orphan before deciding whether a fresh backup is needed.
  if [ -f "$backup" ] && [ ! -f "$active_marker" ]; then
    echo "lab_start_producer: discarding an orphaned $backup (no matching active-config -- stale, not reusing it)" >&2
    rm -f "$backup"
  fi
  if [ -f "$root_genesis" ] && [ ! -f "$backup" ]; then
    cp "$root_genesis" "$backup" || return 1
  fi

  _lab_ensure_root_genesis_signers || return 1

  genesis="$LAB_DIR/genesis-lab-${config_key}.json"
  if [ ! -f "$genesis" ]; then
    echo "lab_start_producer: $genesis missing; generating it..." >&2
    LAB_SWITCH_BLOCK="$switch_block" LAB_PERIOD="$period" LAB_TIMEOUT_PERIOD="$timeout_period" \
      "$LAB_DIR/gen-lab-genesis.sh" || return 1
  fi

  local prev_config=""
  [ -f "$active_marker" ] && prev_config=$(cat "$active_marker")
  if [ "$prev_config" != "$config_key" ]; then
    echo "lab_start_producer: (re)initialising nodes/ for config '$config_key'" >&2
    ( cd "$REPO_ROOT" && ./reset.sh ) || return 1
    printf '%s\n' "$config_key" > "$active_marker"
  fi

  cp "$genesis" "$root_genesis" || return 1

  ( cd "$REPO_ROOT" && nohup ./run.sh >"$LAB_RESULTS_DIR/producer.log" 2>&1 &
    echo $! > "$LAB_RESULTS_DIR/producer.pid" )

  local waited=0
  while ! lab_rpc_call "$(lab_rpc oldxdc)" eth_blockNumber "[]" >/dev/null 2>&1; do
    waited=$((waited + 2))
    if [ "$waited" -ge 60 ]; then
      echo "lab_start_producer: node1 RPC not reachable after 60s (see $LAB_RESULTS_DIR/producer.log)" >&2
      return 1
    fi
    sleep 2
  done
  echo "lab_start_producer: up (switchBlock=$switch_block period=$period timeoutPeriod=$timeout_period), reference=$(lab_rpc oldxdc)" >&2
}

# lab_start_producer_node <node1..node4>: (re)starts a single stopped
# producer sealer, replicating run.sh's per-node XDC invocation (same
# binary/datadir/ports/unlock+mine flags/bootnode). run.sh itself has no
# single-node restart primitive -- it manages all 4 sealers as one process
# group -- so this is the lab's own minimal equivalent, used for the
# offline-validator-penalty scenario (stop one sealer, restart it later).
lab_start_producer_node() {
  local node="$1" idx port_rpc
  case "$node" in
    node1) idx=1 ;; node2) idx=2 ;; node3) idx=3 ;; node4) idx=4 ;;
    *) echo "lab_start_producer_node: unknown node '$node'" >&2; return 1 ;;
  esac
  port_rpc=$(_lab_producer_rpc_port "$node") || return 1
  if [ -n "$(_lab_pid_on_port "$port_rpc")" ]; then
    echo "lab_start_producer_node: $node already running (port $port_rpc busy)" >&2
    return 1
  fi
  mkdir -p "$LAB_RESULTS_DIR" || return 1

  (
    cd "$REPO_ROOT" || exit 1
    # shellcheck source=../lib.sh
    source ./lib.sh
    require_oldxdc
    ensure_bins
    xdc="$XDC_BIN"
    datadir="./nodes/${idx}"
    wallet=$("$xdc" account list --datadir "$datadir" 2>/dev/null | head -n 1 | awk -v FS='({|})' '{print $2}')
    if [ -z "$wallet" ]; then
      echo "lab_start_producer_node: no account found in $datadir" >&2
      exit 1
    fi
    ip=$(detect_ip)
    bootnode_enode="enode://$("$BOOTNODE_BIN" -nodekey ./bootnode.key -writeaddress)@${ip}:30301"
    p2p_port=$((30302 + idx))
    ws_port=$((8554 + idx))
    nohup "$xdc" --bootnodes "$bootnode_enode" --syncmode full --datadir "$datadir" \
      --networkid 20118 --port "$p2p_port" --nat "extip:${ip}" --identity "lab-restart-${node}" \
      --rpc --rpccorsdomain "*" --ws --wsaddr 0.0.0.0 --wsorigins "*" --wsport "$ws_port" \
      --rpcaddr 0.0.0.0 --rpcport "$port_rpc" --rpcvhosts "*" \
      --unlock "$wallet" --password ./.pwd --mine --gasprice 1 --targetgaslimit 420000000 \
      --verbosity 3 --rpcapi admin,db,eth,debug,miner,net,shh,txpool,personal,web3,XDPoS \
      >"$LAB_RESULTS_DIR/${node}-restart.log" 2>&1 &
    echo $! > "$LAB_RESULTS_DIR/${node}.pid"
  )
}

# lab_start_follower <client>: joins the lab producer network as a
# sync-only follower via join.sh --client <client>. geth/erigon/oldxdc
# derive the bootnode from ./bootnode.key like run.sh does (forced via
# --ip so the derivation always fires); besu/nethermind/reth need the
# validator enodes directly (join.sh never derives one for them), so this
# gathers them from the 4 producer sealers first.
lab_start_follower() {
  local client="$1" config_key genesis datadir name
  [ -n "$client" ] || { echo "lab_start_follower: client required" >&2; return 1; }
  config_key="sw900-p1-t10"
  [ -f "$LAB_RESULTS_DIR/active-config" ] && config_key=$(cat "$LAB_RESULTS_DIR/active-config")
  genesis="$LAB_DIR/genesis-lab-${config_key}.json"
  [ -f "$genesis" ] || { echo "lab_start_follower: $genesis missing; run lab_start_producer first" >&2; return 1; }
  mkdir -p "$LAB_RESULTS_DIR" || return 1

  datadir="$LAB_RESULTS_DIR/${client}-peer"
  name="lab-${client}"

  (
    cd "$REPO_ROOT" || exit 1
    case "$client" in
      besu|nethermind|reth)
        enodes=$(_lab_producer_enodes 2>/dev/null)
        if [ -z "$enodes" ]; then
          echo "lab_start_follower: could not gather producer enodes for $client (is the producer up?)" >&2
          exit 1
        fi
        nohup ./join.sh --genesis "$genesis" --client "$client" --bootnodes "$enodes" \
          --datadir "$datadir" --name "$name" --no-ethstats \
          >"$LAB_RESULTS_DIR/${client}.log" 2>&1 &
        ;;
      *)
        # shellcheck source=../lib.sh
        source ./lib.sh
        ip=$(detect_ip)
        nohup ./join.sh --genesis "$genesis" --client "$client" --ip "$ip" \
          --datadir "$datadir" --name "$name" --no-ethstats \
          >"$LAB_RESULTS_DIR/${client}.log" 2>&1 &
        ;;
    esac
    echo $! > "$LAB_RESULTS_DIR/${client}.pid"
  )
}

# _lab_reap_orphan_producer: find+SIGTERM any run.sh/bootnode process that
# belongs to THIS repo (cwd == REPO_ROOT, or -- for a relative-path
# bootnode invocation -- cwd-scoped the same way) but is no longer
# reachable via producer.pid -- e.g. because an external harness SIGKILLed
# the scenario.sh/run.sh ancestor directly (SIGKILL can't be trapped, so
# run.sh's own "kill child_proc on INT/TERM" cleanup in its own trap never
# runs and its bootnode+sealer children are orphaned, reparented to init),
# or because producer.pid was itself lost/cleared (e.g. a results/ wipe)
# while run.sh was still alive. Confirmed live (ItWorksinMyLocal#46
# T3.3/regression-gate finding): exactly such an orphan -- a run.sh with
# every one of its 4 sealers already dead but its OWN bootnode still
# running -- sat blocked forever in run.sh's bare `wait` (bash's `wait`
# with no args blocks on EVERY backgrounded job of that shell, so one
# still-alive bootnode alone is enough to wedge the wrapper permanently),
# holding the bootnode's UDP/TCP port for good and leaving the NEXT
# lab_start_producer attempt on the same box to contend with a leftover
# process nothing in lab_teardown's pidfile-only bookkeeping could find.
# Best-effort/idempotent like the rest of lab_stop: a no-op when nothing
# matches, always returns success either way (called from lab_stop, whose
# own contract is the same).
_lab_reap_orphan_producer() {
  local pid cwd cmd
  for pid in $(pgrep -f '(^|/)run\.sh([[:space:]]|$)' 2>/dev/null); do
    cwd=$(readlink -f "/proc/$pid/cwd" 2>/dev/null) || continue
    [ "$cwd" = "$REPO_ROOT" ] || continue
    echo "lab_stop: reaping orphaned run.sh (pid $pid -- stale/missing producer.pid, or a hard-killed ancestor left it running)" >&2
    kill -TERM "$pid" 2>/dev/null
  done
  for pid in $(pgrep -f 'bootnode .*-nodekey' 2>/dev/null); do
    cmd=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)
    cwd=$(readlink -f "/proc/$pid/cwd" 2>/dev/null)
    case "$cmd" in
      *"-nodekey $REPO_ROOT/bootnode.key"*) : ;;
      *'-nodekey ./bootnode.key'*) [ "$cwd" = "$REPO_ROOT" ] || continue ;;
      *) continue ;;
    esac
    echo "lab_stop: reaping orphaned bootnode (pid $pid)" >&2
    kill -TERM "$pid" 2>/dev/null
  done
  return 0
}

# lab_stop <client|producer|node1..node4>: SIGTERMs that follower's join.sh
# wrapper (which itself traps and cleans up its child), the whole producer
# wrapper (run.sh, which cleans up bootnode + all 4 sealers), or a single
# producer sealer found by the OS process listening on its RPC port. Also
# reaps any run.sh/bootnode orphan left behind by an earlier hard-killed or
# state-lost run (see _lab_reap_orphan_producer) so a stale pidfile (or a
# missing one) never leaves the box dirty.
lab_stop() {
  local target="$1" pidfile pid port
  case "$target" in
    producer)
      pidfile="$LAB_RESULTS_DIR/producer.pid"
      if [ -f "$pidfile" ]; then
        pid=$(cat "$pidfile")
        kill -TERM "$pid" 2>/dev/null
        rm -f "$pidfile"
      fi
      _lab_reap_orphan_producer
      return 0
      ;;
    node1|node2|node3|node4)
      port=$(_lab_producer_rpc_port "$target") || return 1
      pid=$(_lab_pid_on_port "$port")
      [ -n "$pid" ] || { echo "lab_stop: no process found on port $port for $target" >&2; return 1; }
      kill -TERM "$pid" 2>/dev/null
      rm -f "$LAB_RESULTS_DIR/${target}.pid"
      return 0
      ;;
    *)
      pidfile="$LAB_RESULTS_DIR/${target}.pid"
      ;;
  esac
  [ -f "$pidfile" ] || { echo "lab_stop: no pidfile for '$target' ($pidfile)" >&2; return 1; }
  pid=$(cat "$pidfile")
  kill -TERM "$pid" 2>/dev/null
  rm -f "$pidfile"
}

# lab_teardown: stops the producer and every configured follower
# (best-effort), restores the original genesis/genesis.json ONLY if a
# lab_start_producer run actually left it overlaid, and clears the
# active-config marker. Always succeeds (best-effort cleanup) so callers
# can use it freely between/after scenarios. Leaves results/ in place.
lab_teardown() {
  local c
  for c in $(lab_clients); do lab_stop "$c" >/dev/null 2>&1; done
  local i
  for i in 1 2 3 4; do lab_stop "node${i}" >/dev/null 2>&1; done
  lab_stop producer >/dev/null 2>&1

  # Restore the tracked genesis/genesis.json ONLY when active-config is
  # set -- i.e. lab_start_producer (this run, or an earlier one that
  # crashed before its own teardown ran) actually staged an overlay, which
  # is exactly what active-config tracks (ItWorksinMyLocal#46 T3.3 residual
  # fix). NOT "whenever genesis-orig-backup.json happens to exist on disk":
  # that file is written ONCE ever by lab_start_producer and, before this
  # fix, was never deleted afterward -- so scenario.sh's blanket
  # before/after lab_teardown call (every scenario, including every
  # netlab/ scenario like 12/13/14 that NEVER calls lab_start_producer at
  # all) kept unconditionally overwriting genesis/genesis.json with
  # whatever backup happened to still be sitting in lab/results/ from a
  # completely unrelated, possibly hours-old session. Confirmed live:
  # running scenario 13 alone (which only ever touches
  # nodes/all-clients/genesis.json, never the tracked one) left
  # genesis/genesis.json modified in the working tree afterward. Deleting
  # the backup once consumed here closes the other half of the same bug --
  # without it, that same stale snapshot gets silently reapplied by every
  # SUBSEQUENT teardown too, not just once.
  if [ -f "$LAB_RESULTS_DIR/active-config" ] && [ -f "$LAB_RESULTS_DIR/genesis-orig-backup.json" ]; then
    cp "$LAB_RESULTS_DIR/genesis-orig-backup.json" "$REPO_ROOT/genesis/genesis.json" 2>/dev/null \
      && echo "lab_teardown: restored original genesis/genesis.json" >&2
    rm -f "$LAB_RESULTS_DIR/genesis-orig-backup.json"
  fi
  rm -f "$LAB_RESULTS_DIR/active-config" "$LAB_RESULTS_DIR/partition.state"
  return 0
}
