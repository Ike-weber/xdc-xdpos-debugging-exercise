#!/bin/bash
# fleet.sh - whole-topology lifecycle: up|down|status|wipe|logs.
#
#   fleet.sh up     <topology.json> [--wipe] [--force]
#   fleet.sh down   <topology.json> [--force]
#   fleet.sh status <topology.json>
#   fleet.sh wipe   <topology.json> [--force]
#   fleet.sh logs   <topology.json> [nodeId|bootnode] [-f|--follow] [-n N]
#
# `up` sequence (D5's "hub + staggered + capped" peering model):
#   1. topo.sh print  -> nodes/<name>/topo.resolved.tsv (validates first;
#      hard-errors on any violation -- see netlab/TOPOLOGY.md; this is also
#      where each node's role-based maxpeers default is resolved -- T3.1)
#   2. health.sh preflight (disk/RAM/cores/every resolved port, real bind
#      test) -- ENFORCED: a real shortfall aborts `up` before any process
#      starts, unless `--force` (T3.1) tells preflight to proceed anyway
#      (still printing every FAIL line first -- never silently masked)
#   3. node.sh addr for every sealer -> derives (or reuses) each one's
#      keystore address BEFORE any genesis exists
#   4. gen-genesis.sh --signers <those addresses> --out
#      nodes/<name>/genesis.json -- this topology's OWN genesis, never
#      ./genesis/genesis.json
#   5. start this topology's own local bootnode (shared ./bootnode.key,
#      same derivation run.sh uses), at the bootnode_port topo.sh reserved
#      just past the last node's port block
#   6. node.sh start every sealer (they peer with each other through that
#      bootnode); health.sh wait-rpc each
#   7. gather every sealer's live enode via admin_nodeInfo
#   8. node.sh start each follower ONE AT A TIME, in declaration order, under
#      `nice -n 10` (T3.1 -- a follower is sync-only load on the box, not
#      the chain's liveness, so it never competes with a sealer for CPU at
#      the OS scheduling level), --bootnodes = the gathered sealer enodes
#      (comma-joined), plus every earlier-started follower's own enode too
#      when the topology's "peering" field is "mesh" instead of the default
#      "hub" (T3.2 -- see netlab/TOPOLOGY.md) -- gated on health.sh wait-rpc
#      + wait-peers>=1 before starting the next one, with an extra
#      staggerSeconds settle pause from the topology (default 5)
#
# `down` stops in REVERSE declaration order (so followers stop before the
# sealers they depend on), then the bootnode -- all via node.sh's graceful
# stop (repeated SIGTERM; see node.sh's own header -- SIGKILL is never
# automatic there, --force or not). `wipe` runs `down` first (refusing on
# a still-running node unless --force, exactly like node.sh wipe) then
# removes nodes/<name>/ entirely.
#
# Both `down` and `wipe` finish with `_fleet_assert_torn_down`
# (ItWorksinMyLocal#97): a fresh /proc scan of EVERY node's own datadir,
# regardless of what each node.sh stop believed happened, plus a check for
# any process still holding one of its embedded-DB LOCK files open. Either
# hit is a hard FAIL -- `wipe` refuses to rm -rf, `down` refuses to report
# success, and `up --wipe` refuses to restart on top of it. This is exactly
# the check that would have caught netv12's stale node3 before it wasted a
# whole net run (see node.sh's own header for the incident).
#
# Concurrency: up/down/wipe for the SAME topology take an exclusive
# per-topology flock (nodes/.fleet-locks/<name>.lock) held for the whole
# command. Without this, two concurrent invocations against the same
# topology (e.g. a stray `up` left running from an earlier session plus a
# fresh one) can interleave their mkdir/rm -rf on the shared nodes/<name>/
# tree -- confirmed live in ItWorksinMyLocal#46 Phase 2 acceptance: an
# orphaned `scenario.sh --matrix 12` run's retry loop wiped a second,
# healthy, in-progress run's live node data out from under it. A second
# invocation that can't get the lock fails fast with an actionable message
# instead of silently racing. status/logs stay lock-free (read-only).

_NETLAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_NETLAB_DIR/.." && pwd)" || exit 1
TOPO_SH="$_NETLAB_DIR/topo.sh"
NODE_SH="$_NETLAB_DIR/node.sh"
HEALTH_SH="$_NETLAB_DIR/health.sh"

# shellcheck source=../lib.sh
source "$REPO_ROOT/lib.sh"
# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") up     <topology.json> [--wipe] [--force]
       $(basename "$0") down   <topology.json> [--force]
       $(basename "$0") status <topology.json>
       $(basename "$0") wipe   <topology.json> [--force]
       $(basename "$0") logs   <topology.json> [nodeId|bootnode] [-f|--follow] [-n N]
EOF
  exit 1
}

[ $# -ge 2 ] || usage
CMD="$1"; TOPO_FILE="$2"; shift 2
case "$CMD" in
  up|down|status|wipe|logs) ;;
  *) usage ;;
esac
[ -f "$TOPO_FILE" ] || { echo "fleet.sh: no such topology file: $TOPO_FILE" >&2; exit 1; }
# Absolute path: every helper below (topo.sh/node.sh/health.sh) is invoked
# from various cwds, and this gets embedded in meta files.
TOPO_FILE="$(cd "$(dirname "$TOPO_FILE")" && pwd)/$(basename "$TOPO_FILE")" || exit 1

OPT_WIPE=0; OPT_FORCE=0; OPT_FOLLOW=0; OPT_LINES=50; OPT_TARGET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --wipe)      OPT_WIPE=1; shift;;
    --force)     OPT_FORCE=1; shift;;
    -f|--follow) OPT_FOLLOW=1; shift;;
    -n)          OPT_LINES=$2; shift 2;;
    *)
      if [ -z "$OPT_TARGET" ] && [ "$CMD" = logs ]; then OPT_TARGET="$1"; shift
      else echo "fleet.sh: unknown argument: $1" >&2; exit 1; fi
      ;;
  esac
done

# ---------------------------------------------------------------------------
# _fleet_resolve: topo.sh print once -> TOPO_* meta vars, NODE_IDS (ordered
# array), and per-id lookups (ROLE_OF/RPC_OF associative arrays). node.sh
# reads client (NODE_CLIENT) itself when reporting per-node status/logs, so
# fleet.sh -- which only ever branches on role and dials by rpc port -- has
# no need for its own client lookup.
# Single source of truth -- nothing here re-derives port math or re-checks
# constraints; topo.sh already did both.
# ---------------------------------------------------------------------------
declare -a NODE_IDS
declare -A ROLE_OF RPC_OF CLIENT_OF DATADIR_OF

_fleet_resolve() {
  local out
  out="$("$TOPO_SH" print "$TOPO_FILE")" || return 1
  NODE_IDS=(); ROLE_OF=(); RPC_OF=(); CLIENT_OF=(); DATADIR_OF=()

  local assigns
  assigns="$(printf '%s\n' "$out" | python3 -c "
import sys

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


def shq(s):
    return \"'\" + s.replace(\"'\", \"'\\\\''\") + \"'\"


for k, v in meta.items():
    print('TOPO_%s=%s' % (k.upper(), shq(v)))
print('NODE_IDS=(%s)' % ' '.join(shq(r['id']) for r in rows))
for r in rows:
    print('ROLE_OF[%s]=%s' % (shq(r['id']), shq(r['role'])))
    print('RPC_OF[%s]=%s' % (shq(r['id']), shq(r['rpc'])))
    print('CLIENT_OF[%s]=%s' % (shq(r['id']), shq(r['client'])))
    print('DATADIR_OF[%s]=%s' % (shq(r['id']), shq(r['datadir'])))
")" || return 1
  eval "$assigns"

  RUN_DIR="$REPO_ROOT/nodes/$TOPO_NAME"
  RESOLVED_TSV="$RUN_DIR/topo.resolved.tsv"
  GENESIS_FILE="$RUN_DIR/genesis.json"
  BOOTNODE_PIDFILE="$RUN_DIR/bootnode.pid"
  BOOTNODE_LOGFILE="$RUN_DIR/bootnode.log"
}

_pid_alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

_fleet_node_enode() {
  local url="$1" info
  info=$(lab_rpc_call "$url" admin_nodeInfo "[]") || return 1
  python3 -c "
import json
import sys

print(json.loads(sys.argv[1]).get('enode', ''))
" "$info"
}

# ---------------------------------------------------------------------------
# _fleet_up_abort: best-effort teardown of whatever THIS cmd_up call already
# started (bootnode/sealers/followers), used on every failure path from the
# first node-start onward (ItWorksinMyLocal#46 T3.3 gate finding). cmd_up
# has several "some node/health-check didn't come up" exits deep into
# bring-up; before this, every one of them just `return 1`ed with sealers
# (and possibly some followers) already running, live processes on real
# ports. Nothing tears those down: topo_up (lib-topo.sh) only appends to
# active-topologies on a SUCCESSFUL `up` (rc=0), so a failed `up` leaves its
# already-started nodes both running AND untracked -- no active-topologies
# entry for the next `topo_down` (scenario.sh's own before/after-scenario
# cleanup) to find. Confirmed live: an erigon follower that never came up
# within its wait-rpc window left 4 sealers + an earlier geth follower
# running with 16 leaked 34000+ listeners after `up` reported failure and
# exited. Calling cmd_down here, using the SAME already-resolved topology
# globals (NODE_IDS/RPC_OF/RUN_DIR/BOOTNODE_PIDFILE etc, set once by
# _fleet_resolve near the top of cmd_up and never re-resolved for this
# invocation), stops exactly what this attempt brought up -- gracefully
# (no --force; a partial bring-up deserves the same SIGTERM->45s-poll
# discipline as a normal `down`), then cmd_up still returns its own real
# failure to the caller.
_fleet_up_abort() {
  echo "fleet.sh: up failed -- tearing down whatever this attempt already started (nothing leaked running/untracked)" >&2
  cmd_down --best-effort >&2
}

# ---------------------------------------------------------------------------
# _fleet_assert_torn_down: the hard gate ItWorksinMyLocal#97 asked for.
# node.sh's own stop/wipe are now robust per-node (see node.sh's
# _node_target_pids), but the ORIGINAL live incident was at the fleet
# level: `up --wipe` tore down by (then-pidfile-only) belief, then
# unconditionally rm -rf'd the run dir and started fresh nodes on top of a
# stale node3 that was still very much alive -- surviving a full wipe for
# ~40 minutes, holding its XDCx LOCK, so the "fresh" node3 never actually
# started and the net silently ran on 4 of 5 seated validators.
#
# So after any teardown, before anything destructive or a restart, re-scan
# EVERY node's own datadir -- regardless of what node.sh's own stop
# believed -- for (a) a live process whose cmdline names that datadir and
# whose exe is that node's client binary (lib.sh's proc_pids_for_datadir),
# and (b) any process still holding one of its LOCK files open (lib.sh's
# proc_pids_holding_lockfiles -- the literal proximate cause of the "Can't
# create new DB ... resource temporarily unavailable" that kept the real
# node3 from starting). Either hit is a hard FAIL: callers must NOT proceed
# to wipe or restart, they must surface this to the operator instead.
# ---------------------------------------------------------------------------
_fleet_assert_torn_down() {
  local id fail=0
  for id in "${NODE_IDS[@]}"; do
    local node_dir="$REPO_ROOT/${DATADIR_OF[$id]}"
    [ -d "$node_dir" ] || continue

    local hits; hits=$(proc_pids_for_datadir "$node_dir" $(client_exe_names "${CLIENT_OF[$id]}"))
    if [ -n "$hits" ]; then
      echo "fleet.sh: FAIL: ${id}: still-live process(es) holding $node_dir: $(echo "$hits" | tr '\n' ' ')" >&2
      fail=1
    fi

    local locks; locks=$(proc_pids_holding_lockfiles "$node_dir")
    if [ -n "$locks" ]; then
      echo "fleet.sh: FAIL: ${id}: LOCK file(s) still held open: $(echo "$locks" | tr '\n' '; ')" >&2
      fail=1
    fi
  done

  if [ "$fail" -eq 0 ]; then
    echo "fleet.sh: teardown assertion OK -- no process anywhere still holds a node's datadir" >&2
    return 0
  fi
  echo "fleet.sh: refusing to proceed -- a stale process is still holding a node's datadir" >&2
  echo "  (see FAIL lines above). Do NOT wipe or restart over this: inspect and stop it by" >&2
  echo "  hand ('node.sh status'/'stop' can help) before trying again (ItWorksinMyLocal#97)." >&2
  return 1
}

# ---------------------------------------------------------------------------
# cmd_up
# ---------------------------------------------------------------------------
cmd_up() {
  if [ "$OPT_WIPE" = 1 ]; then
    echo "fleet.sh: --wipe: tearing down and clearing any existing state first..." >&2
    cmd_down --best-effort
    _fleet_resolve || return 1
    # ItWorksinMyLocal#97: this is the EXACT incident path -- best-effort
    # teardown, then an unconditional rm -rf and a restart on top. Hard-gate
    # it: refuse to wipe/restart if anything still holds a node's datadir.
    _fleet_assert_torn_down || {
      echo "fleet.sh: refusing --wipe for '$TOPO_NAME' -- see FAIL lines above (ItWorksinMyLocal#97)" >&2
      return 1
    }
    rm -rf "$RUN_DIR"
  fi

  _fleet_resolve || return 1
  mkdir -p "$RUN_DIR" || return 1

  echo "fleet.sh: resolving topology '$TOPO_NAME' ($TOPO_NODE_COUNT nodes, $TOPO_SEALER_COUNT sealer)..." >&2
  "$TOPO_SH" print "$TOPO_FILE" > "$RESOLVED_TSV" || return 1
  echo "fleet.sh: wrote $RESOLVED_TSV" >&2

  echo "fleet.sh: preflight..." >&2
  # --force (T3.1): forwarded from `up`'s own --force so an operator who
  # knows a resource shortfall is acceptable (e.g. a deliberately
  # resource-constrained dev box) can still bring the topology up; preflight
  # itself still prints every real FAIL line first (never silently masked --
  # see health.sh's own header), --force just decides whether that's fatal.
  local preflight_args=(); [ "$OPT_FORCE" = 1 ] && preflight_args=(--force)
  "$HEALTH_SH" preflight "$TOPO_FILE" "${preflight_args[@]}" || { echo "fleet.sh: preflight failed, aborting up" >&2; return 1; }

  # -- sealer addresses, BEFORE any genesis exists --
  local id addrs="" addr
  for id in "${NODE_IDS[@]}"; do
    [ "${ROLE_OF[$id]}" = sealer ] || continue
    addr=$("$NODE_SH" addr "$TOPO_FILE" "$id") || return 1
    echo "fleet.sh: sealer $id -> $addr" >&2
    [ -n "$addrs" ] && addrs="$addrs,$addr"
    addrs="${addrs:-$addr}"
  done
  [ -n "$addrs" ] || { echo "fleet.sh: no sealer addresses resolved" >&2; return 1; }

  # -- genesis (this topology's own, at nodes/<name>/genesis.json) --
  if [ -f "$GENESIS_FILE" ]; then
    echo "fleet.sh: reusing existing $GENESIS_FILE (pass --wipe for a fresh one)" >&2
  else
    echo "fleet.sh: generating genesis (chainId=$TOPO_CHAINID signers=$addrs)..." >&2
    local skip_v1_arg="false"; [ "$TOPO_SKIPV1VALIDATION" = true ] && skip_v1_arg="true"
    # puppeth (via gen-genesis.sh) rejects "--network" names containing a
    # hyphen ("no spaces or hyphens, please") -- every shipped topology name
    # has one (legacy-4, mixed-5, all-clients), so sanitise it here rather
    # than rename the topologies. Confirmed live: an unsanitised hyphenated
    # --network desyncs puppeth's scripted-input wizard entirely (it prints
    # its "no hyphens" rejection, which consumes one of the piped answer
    # lines meant for a later prompt, cascading into unrelated menu errors
    # and eventually an EOF crash before a single genesis field is set).
    local puppeth_network; puppeth_network=$(printf '%s' "netlab_$TOPO_NAME" | tr '-' '_')
    # --timestamp 0x0: puppeth otherwise stamps the live wall-clock time
    # into the header (part of the genesis hash, unlike --period/
    # --skip-v1-validation), which would make two `up --wipe` runs for the
    # SAME topology hash differently every time -- breaking D6's
    # determinism goal (confirmed live: an --skip-v1-validation/--period-only
    # regeneration diffed identical in every field except `timestamp`).
    ( cd "$REPO_ROOT" && ./gen-genesis.sh \
        --network "$puppeth_network" --chainid "$TOPO_CHAINID" --signers "$addrs" \
        --owner "${addrs%%,*}" --foundation "${addrs%%,*}" \
        --foundation-owners "$addrs" --team-owners "$addrs" \
        --swap "${addrs%%,*}" --prefund "${addrs%%,*}" \
        --reward "$TOPO_REWARD" --v2block "$TOPO_V2BLOCK" --epoch "$TOPO_EPOCH" \
        --gap "$TOPO_GAP" --period "$TOPO_PERIOD" --skip-v1-validation "$skip_v1_arg" \
        --timestamp 0x0 \
        --out "$GENESIS_FILE" )
    local gg_rc=$?
    # gen-genesis.sh's underlying puppeth wizard always drops its own
    # "$puppeth_network.json" export next to gen-genesis.sh (i.e. at
    # $REPO_ROOT, regardless of --out) -- a known side effect
    # lab/gen-lab-genesis.sh's own header explicitly documents and avoids
    # by not using puppeth at all. fleet.sh DOES need gen-genesis.sh (for
    # arbitrary per-topology signers/chainId), so it inherits that stray
    # file; clean it up here rather than leaving repo-root state mutated
    # by a topology run (D6 -- instance-scoped state only).
    rm -f "$REPO_ROOT/${puppeth_network}.json"
    [ "$gg_rc" -eq 0 ] || return 1
  fi

  # -- bootnode --
  if _pid_alive "$(cat "$BOOTNODE_PIDFILE" 2>/dev/null)"; then
    echo "fleet.sh: bootnode already running" >&2
  else
    local ip; ip=$(detect_ip)
    echo "fleet.sh: starting bootnode on ${ip}:${TOPO_BOOTNODE_PORT}..." >&2
    # {FLEET_LOCK_FD}>&-: this backgrounded, nohup'd bootnode outlives this
    # script -- without explicitly closing our lock fd in it (bash
    # redirections don't set close-on-exec, so a plain `&` would otherwise
    # leak the open fd, and the flock it holds, into every long-running
    # child forever), the per-topology lock (see the dispatch below) would
    # never actually release once this process exits, wedging every future
    # up/down/wipe for this topology behind a lock nothing is still using.
    # Confirmed live: without this, a completed `up` left the bootnode (and
    # every node -- see the matching close on node.sh below) holding the
    # fd, and the very next `up` for the same topology refused to run.
    nohup "$BOOTNODE_BIN" -nodekey "$REPO_ROOT/bootnode.key" -addr "${ip}:${TOPO_BOOTNODE_PORT}" \
      >"$BOOTNODE_LOGFILE" 2>&1 {FLEET_LOCK_FD}>&- &
    echo $! > "$BOOTNODE_PIDFILE"
  fi

  # -- sealers --
  for id in "${NODE_IDS[@]}"; do
    [ "${ROLE_OF[$id]}" = sealer ] || continue
    # {FLEET_LOCK_FD}>&- (see the bootnode comment above): node.sh's own
    # start backgrounds the actual client process, which would otherwise
    # inherit our lock fd across node.sh's exec and hold it open forever.
    "$NODE_SH" start "$TOPO_FILE" "$id" --genesis "$GENESIS_FILE" {FLEET_LOCK_FD}>&- || { _fleet_up_abort; return 1; }
  done
  for id in "${NODE_IDS[@]}"; do
    [ "${ROLE_OF[$id]}" = sealer ] || continue
    "$HEALTH_SH" wait-rpc "http://127.0.0.1:${RPC_OF[$id]}" 60 || { echo "fleet.sh: sealer $id never came up" >&2; _fleet_up_abort; return 1; }
  done

  # -- gather sealer enodes for the followers --
  local sealer_enodes="" e
  for id in "${NODE_IDS[@]}"; do
    [ "${ROLE_OF[$id]}" = sealer ] || continue
    e=$(_fleet_node_enode "http://127.0.0.1:${RPC_OF[$id]}") || { echo "fleet.sh: could not read enode for $id" >&2; _fleet_up_abort; return 1; }
    [ -n "$e" ] || { echo "fleet.sh: empty enode for $id" >&2; _fleet_up_abort; return 1; }
    [ -n "$sealer_enodes" ] && sealer_enodes="$sealer_enodes,"
    sealer_enodes="${sealer_enodes}${e}"
  done
  echo "fleet.sh: gathered $(echo "$sealer_enodes" | tr ',' '\n' | grep -c enode) sealer enode(s)" >&2

  # -- followers, staggered, gated on peers>=1 --
  # nice -n 10 (T3.1): followers are sync-only load on the box, not the
  # chain's liveness -- a heavy follower (a full-sync erigon/besu/nethermind-
  # class build) competing for CPU with the SEALERS is exactly how a shared
  # dev box starves consensus (net5551 recovery already learned this lesson
  # the hard way at the OS level: a mainnet geth follower left running
  # alongside V2 validators starved sealing outright). node.sh's own start
  # backgrounds the real client process via a plain `&`, which inherits
  # whatever niceness the shell that forked it was running at -- so simply
  # running node.sh itself (and everything it forks) under `nice -n 10` here
  # lowers the WHOLE follower chain's scheduling priority in one place,
  # without touching node.sh/join.sh. Sealers are never niced -- they ARE
  # the liveness this framework exists to protect.
  # mesh peering (T3.2, see TOPOLOGY.md's "peering" field): accumulated as
  # each follower comes up, comma-joined and appended to the NEXT follower's
  # --bootnodes -- so with peering=mesh a follower dials every sealer AND
  # every follower started before it (still one at a time; still gated on
  # peers>=1 below). Stays empty (a no-op) for the default peering=hub.
  local follower_enodes=""
  for id in "${NODE_IDS[@]}"; do
    [ "${ROLE_OF[$id]}" = sealer ] && continue
    local bootnodes_for_id="$sealer_enodes"
    if [ "$TOPO_PEERING" = mesh ] && [ -n "$follower_enodes" ]; then
      bootnodes_for_id="${bootnodes_for_id},${follower_enodes}"
    fi
    nice -n 10 "$NODE_SH" start "$TOPO_FILE" "$id" --genesis "$GENESIS_FILE" --bootnodes "$bootnodes_for_id" {FLEET_LOCK_FD}>&- || { _fleet_up_abort; return 1; }
    "$HEALTH_SH" wait-rpc "http://127.0.0.1:${RPC_OF[$id]}" 90 || { echo "fleet.sh: follower $id never came up" >&2; _fleet_up_abort; return 1; }
    "$HEALTH_SH" wait-peers "http://127.0.0.1:${RPC_OF[$id]}" 1 60 || echo "fleet.sh: WARNING: follower $id still has 0 peers -- continuing anyway" >&2
    if [ "$TOPO_PEERING" = mesh ]; then
      local this_enode; this_enode=$(_fleet_node_enode "http://127.0.0.1:${RPC_OF[$id]}" 2>/dev/null)
      if [ -n "$this_enode" ]; then
        [ -n "$follower_enodes" ] && follower_enodes="$follower_enodes,"
        follower_enodes="${follower_enodes}${this_enode}"
      else
        echo "fleet.sh: WARNING: could not read $id's own enode for mesh peering -- later followers won't dial it directly" >&2
      fi
    fi
    sleep "${TOPO_STAGGERSECONDS:-5}"
  done

  echo "fleet.sh: '$TOPO_NAME' is up ($TOPO_NODE_COUNT nodes)" >&2
  cmd_status
}

# ---------------------------------------------------------------------------
# cmd_down: reverse declaration order (followers before the sealers they
# depend on), then the bootnode. --best-effort (internal, used by `up
# --wipe`) never returns nonzero -- best-effort cleanup before a fresh up.
# ---------------------------------------------------------------------------
cmd_down() {
  local best_effort=0
  [ "${1:-}" = "--best-effort" ] && best_effort=1
  _fleet_resolve || { [ "$best_effort" = 1 ] && return 0 || return 1; }

  local fail=0 id force_args=()
  [ "$OPT_FORCE" = 1 ] && force_args=(--force)

  local -a reversed=()
  for ((i = ${#NODE_IDS[@]} - 1; i >= 0; i--)); do reversed+=("${NODE_IDS[$i]}"); done

  for id in "${reversed[@]}"; do
    "$NODE_SH" stop "$TOPO_FILE" "$id" "${force_args[@]}" || fail=1
  done

  if [ -f "$BOOTNODE_PIDFILE" ]; then
    local pid; pid=$(cat "$BOOTNODE_PIDFILE")
    if _pid_alive "$pid"; then
      echo "fleet.sh: stopping bootnode (pid $pid, SIGTERM)..." >&2
      kill -TERM "$pid" 2>/dev/null
      local waited=0
      while _pid_alive "$pid" && [ "$waited" -lt 45 ]; do sleep 1; waited=$((waited + 1)); done
      if _pid_alive "$pid"; then
        if [ "$OPT_FORCE" = 1 ]; then
          echo "fleet.sh: WARNING: bootnode (pid $pid) did not stop gracefully -- SIGKILL per --force" >&2
          kill -KILL "$pid" 2>/dev/null
        else
          echo "fleet.sh: bootnode (pid $pid) did not stop within 45s; rerun with --force to SIGKILL" >&2
          fail=1
        fi
      fi
    fi
    rm -f "$BOOTNODE_PIDFILE"
  fi

  # ItWorksinMyLocal#97: always run the assertion (even --best-effort, so
  # its FAIL lines are visible either way) but only let it fail a
  # non-best-effort `down` -- best-effort's own contract (never returns
  # nonzero; used by _fleet_up_abort and cmd_up's --wipe teardown step,
  # which re-asserts explicitly itself right before its rm -rf) is
  # unchanged by this.
  _fleet_assert_torn_down || { [ "$best_effort" = 1 ] || fail=1; }

  [ "$best_effort" = 1 ] && return 0
  return $fail
}

# ---------------------------------------------------------------------------
# cmd_status
# ---------------------------------------------------------------------------
cmd_status() {
  _fleet_resolve || return 1
  local id
  for id in "${NODE_IDS[@]}"; do
    "$NODE_SH" status "$TOPO_FILE" "$id"
  done
  if [ -f "$BOOTNODE_PIDFILE" ] && _pid_alive "$(cat "$BOOTNODE_PIDFILE")"; then
    echo "RUNNING  bootnode  pid=$(cat "$BOOTNODE_PIDFILE")"
  else
    echo "STOPPED  bootnode"
  fi
}

# ---------------------------------------------------------------------------
# cmd_wipe
# ---------------------------------------------------------------------------
cmd_wipe() {
  _fleet_resolve || return 1
  if [ "$OPT_FORCE" != 1 ]; then
    # Refuse if anything is still running -- same contract as node.sh wipe.
    local id running=0
    for id in "${NODE_IDS[@]}"; do
      "$NODE_SH" status "$TOPO_FILE" "$id" >/dev/null 2>&1 && running=1
    done
    if [ -f "$BOOTNODE_PIDFILE" ] && _pid_alive "$(cat "$BOOTNODE_PIDFILE")"; then running=1; fi
    if [ "$running" = 1 ]; then
      echo "fleet.sh: refusing to wipe '$TOPO_NAME' while something is still running; run 'down' first or pass --force" >&2
      return 1
    fi
  else
    cmd_down || return 1
  fi
  # Independently re-assert right before the destructive rm -rf
  # (ItWorksinMyLocal#97): the non-force branch above only checked node.sh's
  # own belief of "running" via `status`, which is itself pidfile-based --
  # re-running the full /proc+LOCK assertion here, unconditionally, closes
  # that gap: a node can look STOPPED to node.sh's status while still very
  # much alive and holding its LOCK files, and wiping over that is exactly
  # how a "fresh" restart silently loses a seat. (The --force branch above
  # already ran this same check inside cmd_down; redoing it here is cheap
  # and read-only, and keeps this guard unconditional either way.)
  _fleet_assert_torn_down || return 1
  rm -rf "$RUN_DIR"
  echo "fleet.sh: wiped $RUN_DIR" >&2
}

# ---------------------------------------------------------------------------
# cmd_logs
# ---------------------------------------------------------------------------
cmd_logs() {
  _fleet_resolve || return 1
  if [ -z "$OPT_TARGET" ]; then
    echo "nodes: ${NODE_IDS[*]}" >&2
    echo "-- bootnode ($BOOTNODE_LOGFILE) --" >&2
    [ -f "$BOOTNODE_LOGFILE" ] && tail -n "$OPT_LINES" "$BOOTNODE_LOGFILE"
    return 0
  fi
  if [ "$OPT_TARGET" = bootnode ]; then
    [ -f "$BOOTNODE_LOGFILE" ] || { echo "fleet.sh: no bootnode log yet" >&2; return 1; }
    if [ "$OPT_FOLLOW" = 1 ]; then tail -n "$OPT_LINES" -f "$BOOTNODE_LOGFILE"; else tail -n "$OPT_LINES" "$BOOTNODE_LOGFILE"; fi
    return 0
  fi
  local follow_args=(); [ "$OPT_FOLLOW" = 1 ] && follow_args=(-f)
  "$NODE_SH" logs "$TOPO_FILE" "$OPT_TARGET" -n "$OPT_LINES" "${follow_args[@]}"
}

# ---------------------------------------------------------------------------
# Per-topology exclusive lock for the mutating commands (see header comment).
# Held for the process's remaining lifetime (released automatically when it
# exits and the fd closes) -- cmd_up's internal `cmd_down --best-effort`
# (for --wipe) runs as a plain function call in this same process, so it
# doesn't need (and must not attempt) its own nested acquisition.
# ---------------------------------------------------------------------------
case "$CMD" in
  up|down|wipe)
    _fleet_resolve || exit 1
    LOCK_DIR="$REPO_ROOT/nodes/.fleet-locks"
    mkdir -p "$LOCK_DIR" || exit 1
    FLEET_LOCKFILE="$LOCK_DIR/${TOPO_NAME}.lock"
    # Append (>>), not truncate: a losing contender still opens this same
    # path before it knows it lost (see below), and a truncating open would
    # blow away the winner's diagnostic line out from under it. Only the
    # winner (post-flock) actually rewrites the content, via /dev/fd below.
    exec {FLEET_LOCK_FD}>>"$FLEET_LOCKFILE" || exit 1
    if ! flock -n "$FLEET_LOCK_FD"; then
      echo "fleet.sh: another up/down/wipe for topology '$TOPO_NAME' is already in progress (lock: $FLEET_LOCKFILE); refusing to run concurrently" >&2
      exit 1
    fi
    : >"$FLEET_LOCKFILE"
    printf 'pid=%s locked=%s\n' "$$" "$(date -u +%FT%TZ)" 1>&"$FLEET_LOCK_FD" 2>/dev/null
    ;;
esac

case "$CMD" in
  up)     cmd_up ;;
  down)   cmd_down ;;
  status) cmd_status ;;
  wipe)   cmd_wipe ;;
  logs)   cmd_logs ;;
esac
