#!/bin/bash
# node.sh - per-node lifecycle for a netlab topology instance.
#
#   node.sh start  <topology.json> <nodeId> [--bootnodes LIST] [--genesis FILE]
#   node.sh stop   <topology.json> <nodeId> [--force]
#   node.sh status <topology.json> <nodeId>
#   node.sh logs   <topology.json> <nodeId> [-f|--follow] [-n N]
#   node.sh wipe   <topology.json> <nodeId> [--force]
#   node.sh addr   <topology.json> <nodeId>     # utility: sealer-only, see below
#
# One node = one join.sh invocation, launched/tracked exactly like every
# other wrapper in this repo (nohup + a pidfile), under its own
# nodes/<topoName>/<nodeId>/ datadir (never touches ./nodes/1..4 or
# ./nodes/<client>-<type> -- see netlab/TOPOLOGY.md "Run-state layout").
#
# Graceful-only teardown (D3, hardened per ItWorksinMyLocal#97): `stop`
# resolves every live pid for this node (pidfile, primary -- see
# _node_target_pids below for why that alone isn't enough with this repo's
# join.sh wrapper -- unioned with a /proc/*/cmdline+exe fallback scan, see
# lib.sh's proc_pids_for_datadir), then sends REPEATED SIGTERM, polling
# between rounds, up to 10 rounds -- ported from the proven
# /data/mint-r3/netv12/stop.sh: XDPoSChain's shutdown can hang indefinitely
# (observed live: 40+ minutes stuck after "Got interrupt, shutting down..."
# while the V2 consensus loop kept spinning "[sendTimeout] Timeout message
# generated" every 10s, with SIGTERM confirmed caught via
# /proc/<pid>/status, not blocked/ignored), and the client itself counts
# repeated interrupts and force-exits via its OWN panic path at 10
# ("Already shutting down, interrupt more to panic. times=N" counting
# down). SIGKILL is NEVER sent here -- not even behind --force -- because
# SIGKILL on a live XDPoS validator corrupts its state DB and deadlocks the
# node on restart (the whole class of incident the 5551 recovery playbook
# exists for). If 10 SIGTERMs don't clear it, `stop` reports failure and
# tells the operator to investigate by hand; --force no longer changes
# that (kept as an accepted, now-inert flag so existing callers don't
# break).
#
# --bootnodes: a `sealer` node derives its own bootnode enode from the
# shared ./bootnode.key (same derivation run.sh uses) unless --bootnodes
# overrides it -- sealers peer with each other through the topology's own
# local bootnode. A `follower` node has no such fallback: fleet.sh gathers
# the live sealer enodes (admin_nodeInfo) and MUST pass them via
# --bootnodes -- followers dial sealers directly, never each other (D5).
#
# `addr`: sealer-only utility (not part of the five lifecycle verbs above).
# Ensures this node's validator key (topology's keyRef, an env var out of
# .env) is imported into its keystore -- WITHOUT starting anything -- and
# prints the resulting address on stdout, nothing else. This is what
# fleet.sh calls, once per sealer, to learn the genesis --signers list
# *before* any genesis exists (the same account-import step `start` performs
# lazily is idempotent, so calling `addr` first and then `start` later never
# double-imports).

_NETLAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_NETLAB_DIR/.." && pwd)" || exit 1
TOPO_SH="$_NETLAB_DIR/topo.sh"

# shellcheck source=../lib.sh
source "$REPO_ROOT/lib.sh"
# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

# .env (PRIVATE_KEY_* etc), same convention as run.sh: exported so a
# sealer's keyRef (e.g. PRIVATE_KEY_1) is readable via ${!NODE_KEYREF}.
set -a
[ -f "$REPO_ROOT/.env" ] && . "$REPO_ROOT/.env"
set +a

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") start  <topology.json> <nodeId> [--bootnodes LIST] [--genesis FILE]
       $(basename "$0") stop   <topology.json> <nodeId> [--force]
       $(basename "$0") status <topology.json> <nodeId>
       $(basename "$0") logs   <topology.json> <nodeId> [-f|--follow] [-n N]
       $(basename "$0") wipe   <topology.json> <nodeId> [--force]
       $(basename "$0") addr   <topology.json> <nodeId>
EOF
  exit 1
}

[ $# -ge 3 ] || usage
CMD="$1"; TOPO_FILE="$2"; NODE_ID="$3"; shift 3
case "$CMD" in
  start|stop|status|logs|wipe|addr) ;;
  *) usage ;;
esac
[ -f "$TOPO_FILE" ] || { echo "node.sh: no such topology file: $TOPO_FILE" >&2; exit 1; }

OPT_BOOTNODES=""; OPT_GENESIS=""; OPT_FORCE=0; OPT_FOLLOW=0; OPT_LINES=50
while [ $# -gt 0 ]; do
  case "$1" in
    --bootnodes) OPT_BOOTNODES=$2; shift 2;;
    --genesis)   OPT_GENESIS=$2; shift 2;;
    --force)     OPT_FORCE=1; shift;;
    -f|--follow) OPT_FOLLOW=1; shift;;
    -n)          OPT_LINES=$2; shift 2;;
    *) echo "node.sh: unknown flag: $1" >&2; exit 1;;
  esac
done

# ---------------------------------------------------------------------------
# _node_resolve: runs topo.sh print once, extracts this node's row + the
# topology-level meta, and shell-`eval`s them into TOPO_*/NODE_* variables.
# Single source of truth for ports/paths -- nothing here re-derives the
# portBase+i*20 math itself.
# ---------------------------------------------------------------------------
_node_resolve() {
  local out
  out="$("$TOPO_SH" print "$TOPO_FILE")" || return 1
  local assigns
  assigns="$(printf '%s\n' "$out" | python3 -c "
import sys

node_id = sys.argv[1]
meta = {}
rows = []
header = None
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('# '):
        k, _, v = line[2:].partition('=')
        meta[k] = v
    elif header is None:
        header = line.split('\t')
    else:
        rows.append(dict(zip(header, line.split('\t'))))
row = next((r for r in rows if r.get('id') == node_id), None)
if row is None:
    sys.stderr.write('node.sh: no such node id in topology: %s\n' % node_id)
    sys.exit(1)


def shq(s):
    return \"'\" + s.replace(\"'\", \"'\\\\''\") + \"'\"


for k, v in meta.items():
    print('TOPO_%s=%s' % (k.upper(), shq(v)))
for k, v in row.items():
    print('NODE_%s=%s' % (k.upper(), shq(v)))
" "$NODE_ID")" || return 1
  eval "$assigns"
  NODE_DIR="$REPO_ROOT/$NODE_DATADIR"
  NODE_PIDFILE="$NODE_DIR/node.pid"
  NODE_LOGFILE="$NODE_DIR/node.log"
  NODE_METAFILE="$NODE_DIR/meta.env"
  GENESIS_FILE="${OPT_GENESIS:-$REPO_ROOT/nodes/$TOPO_NAME/genesis.json}"
}

_node_resolve || exit 1

# ---------------------------------------------------------------------------
# _pid_alive <pid>: true if that pid exists.
# ---------------------------------------------------------------------------
_pid_alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

_read_pid() { [ -f "$NODE_PIDFILE" ] && cat "$NODE_PIDFILE" 2>/dev/null; }

# ---------------------------------------------------------------------------
# _node_target_pids: prints every LIVE pid that is actually this node's
# client process right now, one per line, deduplicated (ItWorksinMyLocal#97).
#
# cmd_start backgrounds join.sh (NOT the client binary directly), so the
# pidfile holds join.sh's own pid. join.sh traps INT/TERM with
# `kill -TERM $child_proc; exit` -- it forwards the signal to the real
# client exactly ONCE and exits ITSELF immediately, without waiting for the
# child. That means the pidfile pid can vanish within about a second of a
# SIGTERM while the real client -- which is what can hang for 40+ minutes
# per #97's defect B -- is still running, now reparented to init and
# invisible to a naive `pgrep -P <pidfile pid>` (its parent is already
# gone). Trusting the pidfile pid alone would make `stop` declare victory
# the instant join.sh exits, while an actually-wedged validator keeps
# holding its datadir's LOCK files open -- defect A's failure mode, reached
# by a different route.
#
# So resolution is: pidfile (primary) -> its still-live child, found via
# `pgrep -P` WHILE join.sh is still around -- UNIONED WITH a
# /proc/*/cmdline+exe scan for this node's own datadir (lib.sh's
# proc_pids_for_datadir), which independently rediscovers that same child
# whether join.sh has already exited (orphaned) or the pidfile is missing
# or stale entirely. NEVER `pgrep -f <datadir path>` -- see
# proc_pids_for_datadir's own header in lib.sh for why (it kills its own
# caller).
# ---------------------------------------------------------------------------
_node_target_pids() {
  local pid; pid="$(_read_pid)"
  local -a out=()
  if _pid_alive "$pid"; then
    out+=("$pid")
    local c
    for c in $(pgrep -P "$pid" 2>/dev/null); do out+=("$c"); done
  fi
  local scanned s
  scanned=$(proc_pids_for_datadir "$NODE_DIR" $(client_exe_names "$NODE_CLIENT"))
  for s in $scanned; do out+=("$s"); done
  [ "${#out[@]}" -eq 0 ] && return 0
  printf '%s\n' "${out[@]}" | sort -un
}

# ---------------------------------------------------------------------------
# _ensure_sealer_account: idempotent. Imports NODE_KEYREF's env value into
# this node's keystore if it doesn't already hold an account, else reads
# the existing one back. Prints the resulting 0x-prefixed address on stdout
# (only), everything else (including ensure_bins' own progress messages)
# is routed to stderr so callers can safely capture stdout.
# ---------------------------------------------------------------------------
_ensure_sealer_account() {
  [ "$NODE_ROLE" = sealer ] || { echo "node.sh: ${NODE_ID}: not a sealer (role=$NODE_ROLE)" >&2; return 1; }
  local key="${!NODE_KEYREF:-}"
  [ -n "$key" ] || { echo "node.sh: ${NODE_ID}: keyRef '$NODE_KEYREF' is not set in the environment (check .env)" >&2; return 1; }

  local xdc
  if [ -n "${OLDXDC_BIN:-}" ] && [ -x "$OLDXDC_BIN" ]; then
    xdc="$OLDXDC_BIN"
  else
    ensure_bins >&2
    xdc="$XDC_BIN"
  fi

  mkdir -p "$NODE_DIR" || return 1
  touch "$REPO_ROOT/.pwd"

  local addr
  if [ -d "$NODE_DIR/keystore" ] && [ -n "$(ls -A "$NODE_DIR/keystore" 2>/dev/null)" ]; then
    addr=$("$xdc" account list --datadir "$NODE_DIR" 2>/dev/null | head -n 1 | grep -oE '\{[0-9a-fA-Fxdc]+\}' | tr -d '{}')
  else
    addr=$("$xdc" account import --password "$REPO_ROOT/.pwd" --datadir "$NODE_DIR" <(echo "$key") 2>/dev/null \
      | grep -oE '\{[0-9a-fA-Fxdc]+\}' | tr -d '{}')
  fi
  [ -n "$addr" ] || { echo "node.sh: ${NODE_ID}: could not determine a wallet address (check keyRef '$NODE_KEYREF')" >&2; return 1; }
  # Normalise to a canonical 0x-prefixed address regardless of which branch
  # above produced it: `account import`'s "Address: {xdc...}" and `account
  # list`'s "Account #0: {...}" (no prefix at all) format the hex
  # differently, so strip either an "xdc" or "0x" prefix (if present) and
  # re-add "0x" uniformly.
  addr=$(printf '%s' "$addr" | sed -E 's/^0[xX]//; s/^xdc//')
  addr="0x${addr}"
  printf '%s\n' "$addr"
}

# ---------------------------------------------------------------------------
# cmd_start
# ---------------------------------------------------------------------------
cmd_start() {
  local existing_pid
  existing_pid="$(_read_pid)"
  if _pid_alive "$existing_pid"; then
    echo "node.sh: ${NODE_ID}: already running (pid $existing_pid)" >&2
    return 0
  fi

  [ -f "$GENESIS_FILE" ] || {
    echo "node.sh: ${NODE_ID}: genesis not found at $GENESIS_FILE" >&2
    echo "  run 'netlab/fleet.sh up $TOPO_FILE' first, or generate it directly:" >&2
    echo "  ./gen-genesis.sh --chainid $TOPO_CHAINID --out nodes/$TOPO_NAME/genesis.json ..." >&2
    return 1
  }

  mkdir -p "$NODE_DIR" || return 1

  local bootnodes="$OPT_BOOTNODES"
  local seal_addr=""
  if [ "$NODE_ROLE" = sealer ]; then
    seal_addr="$(_ensure_sealer_account)" || return 1
    if [ -z "$bootnodes" ]; then
      # Sealers peer with each other through this topology's own local
      # bootnode (./bootnode.key, same derivation run.sh uses), at the
      # dedicated bootnode_port topo.sh reserved just past the last node's
      # port block -- fleet.sh must have started it (cmd_status below will
      # just report a connection failure if not; this does not start one).
      local ip pubkey
      ip=$(detect_ip)
      pubkey=$("$BOOTNODE_BIN" -nodekey "$REPO_ROOT/bootnode.key" -writeaddress)
      bootnodes="enode://${pubkey}@${ip}:${TOPO_BOOTNODE_PORT}"
    fi
  else
    [ -n "$bootnodes" ] || {
      echo "node.sh: ${NODE_ID}: follower nodes need --bootnodes (fleet.sh gathers the" >&2
      echo "  live sealer enodes via admin_nodeInfo and passes them here -- see D5 in" >&2
      echo "  ItWorksinMyLocal#46)." >&2
      return 1
    }
  fi

  local args=(--genesis "$GENESIS_FILE" --client "$NODE_CLIENT" --datadir "$NODE_DIR"
    --chainid "$TOPO_CHAINID" --port "$NODE_P2P" --rpcport "$NODE_RPC" --wsport "$NODE_WS"
    --authrpc-port "$NODE_AUTHRPC" --torrent-port "$NODE_TORRENT" --mcp-port "$NODE_MCP" --privapi "$NODE_PRIVAPI"
    --bootnodes "$bootnodes" --name "${TOPO_NAME}-${NODE_ID}" --no-ethstats)
  [ -n "$NODE_MAXPEERS" ] && args+=(--maxpeers "$NODE_MAXPEERS")
  # --gas-limit: NODE_GASLIMIT comes straight out of topo.sh print's resolved
  # "gasLimit" column (picked up generically by _node_resolve's NODE_* eval
  # above -- no per-column code needed here), always non-empty (topo.sh
  # defaults it to 420000000). Passed unconditionally, unlike --maxpeers
  # above, because leaving it off is exactly the #94 bug: node.sh used to
  # pass NO gas flag at all, so join.sh's oldxdc branch fell back to the
  # legacy arbiter's own compiled-in 50,000,000 default while geth/erigon
  # elsewhere targeted 420,000,000 -- two different mint targets on one
  # chain. Every node in a topology -- sealer or follower, every client --
  # now gets the SAME explicit target (ItWorksinMyLocal#96).
  args+=(--gas-limit "$NODE_GASLIMIT")
  if [ "$NODE_ROLE" = sealer ]; then
    args+=(--mine "$seal_addr" --etherbase "$seal_addr")
  fi

  echo "node.sh: starting ${NODE_ID} (${NODE_CLIENT}, ${NODE_ROLE}) rpc=http://127.0.0.1:${NODE_RPC} p2p=${NODE_P2P}" >&2
  # Invoke join.sh by ABSOLUTE path as a single simple command (no `cd &&
  # nohup ... &` subshell chaining, no nested subshell at all) so `$!`
  # unambiguously names join.sh's own pid -- join.sh itself does `cd
  # "$(dirname "$0")"` as its first line, so the absolute path here is all
  # it needs to resolve everything relative to REPO_ROOT correctly. (An
  # earlier `( cd ... && nohup ... & echo $! )` version recorded the WRONG
  # pid -- a parent of the real join.sh process, already reaped by the time
  # `stop` used it -- exactly the kind of silent-no-op-teardown bug D3
  # exists to prevent; verified fixed by an actual start/status/stop cycle,
  # not just reasoning about it.)
  nohup "$REPO_ROOT/join.sh" "${args[@]}" >"$NODE_LOGFILE" 2>&1 &
  echo $! > "$NODE_PIDFILE"

  {
    echo "NODE_ID=$NODE_ID"
    echo "CLIENT=$NODE_CLIENT"
    echo "ROLE=$NODE_ROLE"
    echo "TOPOLOGY=$TOPO_NAME"
    echo "BOOTNODES=$bootnodes"
    echo "GENESIS=$GENESIS_FILE"
    echo "RPC=http://127.0.0.1:${NODE_RPC}"
    echo "STARTED_AT=$(date -u +%FT%TZ)"
  } > "$NODE_METAFILE"

  echo "node.sh: ${NODE_ID}: started (pid $(cat "$NODE_PIDFILE"))" >&2
}

# ---------------------------------------------------------------------------
# cmd_stop: resolve every live pid for this node (_node_target_pids above),
# then repeated SIGTERM -- poll up to 10s -- repeat, up to 10 rounds (the
# client's own documented forced-exit threshold; see the header comment).
# SIGKILL is NEVER sent here, not even behind --force (see header).
# ---------------------------------------------------------------------------
cmd_stop() {
  local -a targets=()
  local t; while IFS= read -r t; do [ -n "$t" ] && targets+=("$t"); done < <(_node_target_pids)

  if [ "${#targets[@]}" -eq 0 ]; then
    echo "node.sh: ${NODE_ID}: not running" >&2
    rm -f "$NODE_PIDFILE"
    return 0
  fi

  if [ "$OPT_FORCE" = 1 ]; then
    echo "node.sh: note: --force no longer escalates to SIGKILL (ItWorksinMyLocal#97) -- 'stop' only ever sends repeated SIGTERM" >&2
  fi
  echo "node.sh: stopping ${NODE_ID} (pid(s): ${targets[*]})" >&2

  local round
  for round in $(seq 1 10); do
    local -a alive=()
    for t in "${targets[@]}"; do _pid_alive "$t" && alive+=("$t"); done
    if [ "${#alive[@]}" -eq 0 ]; then
      rm -f "$NODE_PIDFILE"
      echo "node.sh: ${NODE_ID}: stopped cleanly (round $round)" >&2
      return 0
    fi
    for t in "${alive[@]}"; do kill -TERM "$t" 2>/dev/null; done
    [ "$round" -gt 1 ] && echo "node.sh: ${NODE_ID}: round $round: still up: ${alive[*]}" >&2
    local waited=0
    while [ "$waited" -lt 10 ]; do
      sleep 1
      local -a still=()
      for t in "${alive[@]}"; do _pid_alive "$t" && still+=("$t"); done
      alive=("${still[@]}")
      [ "${#alive[@]}" -eq 0 ] && break
      waited=$((waited + 1))
    done
    targets=("${alive[@]}")
  done

  if [ "${#targets[@]}" -eq 0 ]; then
    rm -f "$NODE_PIDFILE"
    echo "node.sh: ${NODE_ID}: stopped" >&2
    return 0
  fi

  echo "node.sh: WARNING: ${NODE_ID} still running after 10 SIGTERMs: ${targets[*]}" >&2
  echo "  do NOT SIGKILL -- that corrupts the XDPoS state DB and deadlocks the node on" >&2
  echo "  restart (ItWorksinMyLocal#97). Inspect the logs for a hung shutdown (repeated" >&2
  echo "  '[sendTimeout] Timeout message generated' is the known symptom) before doing" >&2
  echo "  anything else -- this script never sends SIGKILL itself, --force or not." >&2
  return 1
}

# ---------------------------------------------------------------------------
# cmd_status
# ---------------------------------------------------------------------------
cmd_status() {
  local pid; pid="$(_read_pid)"
  if [ -n "$pid" ] && _pid_alive "$pid"; then
    local url="http://127.0.0.1:${NODE_RPC}" block
    if block=$(LAB_RPC_TIMEOUT=2 lab_block_number "$url" 2>/dev/null); then
      echo "RUNNING  ${NODE_ID}  (${NODE_CLIENT}/${NODE_ROLE})  pid=$pid  rpc=$url  block=$block"
    else
      echo "RUNNING  ${NODE_ID}  (${NODE_CLIENT}/${NODE_ROLE})  pid=$pid  rpc=$url  block=<not up yet>"
    fi
    return 0
  fi
  [ -n "$pid" ] && rm -f "$NODE_PIDFILE"   # stale pidfile -- clean it up

  # ItWorksinMyLocal#97: a missing/stale pidfile does NOT mean the node is
  # actually stopped -- fall back to the same /proc scan `stop`/`wipe` use
  # (_node_target_pids), so an orphaned or wedged process (mid-shutdown, RPC
  # never bound, whatever) is reported RUNNING instead of silently looking
  # STOPPED to an operator deciding whether it's safe to wipe or restart.
  local orphan; orphan="$(_node_target_pids | head -1)"
  if [ -n "$orphan" ]; then
    echo "RUNNING  ${NODE_ID}  (${NODE_CLIENT}/${NODE_ROLE})  pid=$orphan  (orphaned -- no valid pidfile, found via /proc scan)"
    return 0
  fi

  echo "STOPPED  ${NODE_ID}  (${NODE_CLIENT}/${NODE_ROLE})"
  return 1
}

# ---------------------------------------------------------------------------
# cmd_logs
# ---------------------------------------------------------------------------
cmd_logs() {
  [ -f "$NODE_LOGFILE" ] || { echo "node.sh: no log yet at $NODE_LOGFILE" >&2; return 1; }
  if [ "$OPT_FOLLOW" = 1 ]; then
    tail -n "$OPT_LINES" -f "$NODE_LOGFILE"
  else
    tail -n "$OPT_LINES" "$NODE_LOGFILE"
  fi
}

# ---------------------------------------------------------------------------
# cmd_wipe: refuses on a running node unless --force (which stops it first,
# via the same graceful cmd_stop path -- which never SIGKILLs, --force or
# not; see cmd_stop's header). Uses _node_target_pids (not just the
# pidfile) for the running-check itself (ItWorksinMyLocal#97): a stale or
# missing pidfile must NOT let `wipe` sail past this guard and rm -rf a
# datadir a live/orphaned process still has open.
# ---------------------------------------------------------------------------
cmd_wipe() {
  local -a targets=()
  local t; while IFS= read -r t; do [ -n "$t" ] && targets+=("$t"); done < <(_node_target_pids)

  if [ "${#targets[@]}" -gt 0 ]; then
    if [ "$OPT_FORCE" != 1 ]; then
      echo "node.sh: ${NODE_ID}: refusing to wipe a running node (pid(s): ${targets[*]}); stop it first or pass --force" >&2
      return 1
    fi
    cmd_stop || return 1
  fi
  rm -rf "$NODE_DIR"
  echo "node.sh: ${NODE_ID}: wiped ($NODE_DIR)" >&2
}

# ---------------------------------------------------------------------------
# cmd_addr
# ---------------------------------------------------------------------------
cmd_addr() { _ensure_sealer_account; }

case "$CMD" in
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  logs)   cmd_logs ;;
  wipe)   cmd_wipe ;;
  addr)   cmd_addr ;;
esac
