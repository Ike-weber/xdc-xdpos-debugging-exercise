#!/bin/bash
# lib-topo.sh - scenario <-> netlab fleet bridge (ItWorksinMyLocal#46 T2.1).
#
# Thin wrappers around netlab/fleet.sh + netlab/node.sh + netlab/topo.sh so
# lab/ scenarios can drive a declarative topology (topologies/*.json)
# without reaching into netlab/ paths themselves, and so scenario.sh's
# generic per-scenario teardown can tear down ANY topology a scenario
# brought up without needing to know which one:
#
#   topo_up    <topology.json> [--wipe]           # netlab/fleet.sh up, records it
#   topo_down  [<topology.json>] [--force]        # netlab/fleet.sh down; no-arg form is
#                                                  # idempotent teardown-of-everything
#   topo_rpc   <topology.json> <nodeId>            # -> http://127.0.0.1:<rpc> for that node
#   topo_client <topology.json> <nodeId>           # -> that node's client id (e.g. "geth")
#   topo_nodes <topology.json> [role]              # -> node ids, one per line, optionally
#                                                   #    filtered to role=sealer|follower
#   topo_stop  <topology.json> <nodeId> [--force]  # netlab/node.sh stop for one node
#   topo_partition <topology.json> <groupA-ids> <groupB-ids>  # T3.2: admin_removePeer
#                                                  # split, admin-RPC-capable clients only
#   topo_heal  <topology.json>                     # T3.2: admin_addPeer, reverses the
#                                                  # most recent topo_partition for this topology
#
# State: lab/results/active-topologies -- one absolute topology.json path
# per line, appended to by a successful topo_up, removed by topo_down.
# scenario.sh's _run_one calls the no-arg `topo_down` both BEFORE and AFTER
# every scenario, alongside the existing lab_teardown call (same "always
# safe, best-effort, never fails the run" contract) -- so a topology left
# running by a crashed/killed scenario still gets torn down on the very
# next scenario.sh invocation, and a scenario that never touched netlab at
# all (01-11) pays a no-op (missing/empty marker file, nothing to do).
#
# Deliberately does NOT try to guess "the" topology a scenario is using --
# scenario 12 (and anything using this file) must pass the topology.json
# path explicitly to topo_up/topo_rpc/topo_nodes/topo_stop; only the
# teardown-time topo_down gets to omit it, and only because at that point
# "everything this file marked as up" is exactly what needs to go down.

_LIBTOPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_LIBTOPO_DIR/.." && pwd)" || exit 1
LAB_RESULTS_DIR="$_LIBTOPO_DIR/results"
mkdir -p "$LAB_RESULTS_DIR" 2>/dev/null || true

# topo_partition/topo_heal (below) drive admin_nodeInfo/admin_addPeer/
# admin_removePeer directly, so this file needs lab_rpc_call -- standalone
# source, same posture as netlab/health.sh (T1.4): don't depend on
# lab/lib-lab.sh's fixed-topology bits just for the RPC transport.
# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

_TOPO_FLEET_SH="$REPO_ROOT/netlab/fleet.sh"
_TOPO_NODE_SH="$REPO_ROOT/netlab/node.sh"
_TOPO_TOPO_SH="$REPO_ROOT/netlab/topo.sh"
_TOPO_ACTIVE_FILE="$LAB_RESULTS_DIR/active-topologies"

# _topo_abspath <file>: absolute path (file must exist).
_topo_abspath() {
  local f="$1"
  [ -f "$f" ] || { echo "lib-topo.sh: no such topology file: $f" >&2; return 1; }
  (cd "$(dirname "$f")" && printf '%s/%s\n' "$(pwd)" "$(basename "$f")")
}

# _topo_mark_up/_topo_mark_down: maintain the active-topologies marker,
# deduped, one absolute path per line.
_topo_mark_up() {
  local abs="$1"
  touch "$_TOPO_ACTIVE_FILE"
  grep -qxF "$abs" "$_TOPO_ACTIVE_FILE" 2>/dev/null || printf '%s\n' "$abs" >> "$_TOPO_ACTIVE_FILE"
}

_topo_mark_down() {
  local abs="$1"
  [ -f "$_TOPO_ACTIVE_FILE" ] || return 0
  grep -vxF "$abs" "$_TOPO_ACTIVE_FILE" > "${_TOPO_ACTIVE_FILE}.tmp" 2>/dev/null
  mv "${_TOPO_ACTIVE_FILE}.tmp" "$_TOPO_ACTIVE_FILE" 2>/dev/null || rm -f "${_TOPO_ACTIVE_FILE}.tmp"
}

# ---------------------------------------------------------------------------
# topo_up <topology.json> [--wipe]
# ---------------------------------------------------------------------------
topo_up() {
  [ $# -ge 1 ] || { echo "topo_up: usage: topo_up <topology.json> [--wipe]" >&2; return 1; }
  local topo_file="$1"; shift
  local abs; abs=$(_topo_abspath "$topo_file") || return 1
  "$_TOPO_FLEET_SH" up "$abs" "$@"
  local rc=$?
  [ "$rc" -eq 0 ] && _topo_mark_up "$abs"
  return $rc
}

# ---------------------------------------------------------------------------
# topo_down [<topology.json>] [--force]
#
# No-arg (or --force-only) form: best-effort teardown of every topology
# recorded in active-topologies -- ALWAYS returns 0 (identical contract to
# lab_teardown), since this is what scenario.sh's generic per-scenario
# teardown calls unconditionally and must never itself fail a run. Entries
# are only cleared from the marker once fleet.sh down actually succeeds on
# them; a failed graceful stop is left in place (with fleet.sh's own
# warning already on stderr) so the NEXT teardown call retries it instead
# of silently forgetting about a still-running node.
#
# Single-topology form: propagates fleet.sh's real exit code, for callers
# (scenario code, an operator) that want to know whether teardown actually
# succeeded.
# ---------------------------------------------------------------------------
topo_down() {
  if [ $# -eq 0 ] || [ "$1" = "--force" ]; then
    local force_args=(); [ "${1:-}" = "--force" ] && force_args=(--force)
    [ -f "$_TOPO_ACTIVE_FILE" ] || return 0
    local abs
    while IFS= read -r abs; do
      [ -n "$abs" ] || continue
      if [ ! -f "$abs" ]; then
        echo "lib-topo.sh: topo_down: '$abs' no longer exists; dropping it from active-topologies" >&2
        _topo_mark_down "$abs"
        continue
      fi
      if "$_TOPO_FLEET_SH" down "$abs" "${force_args[@]}" >&2; then
        _topo_mark_down "$abs"
      else
        echo "lib-topo.sh: topo_down: fleet.sh down failed for '$abs' -- left in active-topologies for the next retry" >&2
      fi
    done < "$_TOPO_ACTIVE_FILE"
    return 0
  fi

  local topo_file="$1"; shift
  local abs; abs=$(_topo_abspath "$topo_file") || return 1
  "$_TOPO_FLEET_SH" down "$abs" "$@"
  local rc=$?
  [ "$rc" -eq 0 ] && _topo_mark_down "$abs"
  return $rc
}

# ---------------------------------------------------------------------------
# topo_rpc <topology.json> <nodeId> -> http://127.0.0.1:<rpc>
# ---------------------------------------------------------------------------
topo_rpc() {
  [ $# -ge 2 ] || { echo "topo_rpc: usage: topo_rpc <topology.json> <nodeId>" >&2; return 1; }
  local topo_file="$1" node_id="$2" abs out
  abs=$(_topo_abspath "$topo_file") || return 1
  out=$("$_TOPO_TOPO_SH" print "$abs") || return 1
  printf '%s\n' "$out" | python3 -c "
import sys

node_id = sys.argv[1]
header = None
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('# ') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    row = dict(zip(header, line.split('\t')))
    if row.get('id') == node_id:
        print('http://127.0.0.1:%s' % row['rpc'])
        sys.exit(0)
sys.exit(1)
" "$node_id" || { echo "topo_rpc: no such node id '$node_id' in $abs" >&2; return 1; }
}

# ---------------------------------------------------------------------------
# topo_client <topology.json> <nodeId> -> that node's client id (e.g. "geth")
# -- lets scenario code (e.g. an all-clients matrix, T3.3) key its own
# per-node verdicts by client without reaching into topo.sh's TSV itself.
# ---------------------------------------------------------------------------
topo_client() {
  [ $# -ge 2 ] || { echo "topo_client: usage: topo_client <topology.json> <nodeId>" >&2; return 1; }
  local topo_file="$1" node_id="$2"
  _topo_row_field "$topo_file" "$node_id" client \
    || { echo "topo_client: no such node id '$node_id' in $topo_file" >&2; return 1; }
}

# ---------------------------------------------------------------------------
# topo_log <topology.json> <nodeId> -> absolute path to that node's
# node.log (nodes/<name>/<id>/node.log -- see netlab/node.sh's
# NODE_LOGFILE). Lets scenario code inspect a node's own log for signals
# no RPC/hash comparison can see (e.g. lib-assert.sh's
# assert_no_validation_bypass, added for ItWorksinMyLocal#46's gate
# finding that a follower can silently auto-recover from a genuine
# consensus mismatch and still hash-agree afterward).
# ---------------------------------------------------------------------------
topo_log() {
  [ $# -ge 2 ] || { echo "topo_log: usage: topo_log <topology.json> <nodeId>" >&2; return 1; }
  local topo_file="$1" node_id="$2" abs out
  abs=$(_topo_abspath "$topo_file") || return 1
  out=$("$_TOPO_TOPO_SH" print "$abs") || return 1
  printf '%s\n' "$out" | python3 -c "
import sys

node_id = sys.argv[1]
header = None
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('# ') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    row = dict(zip(header, line.split('\t')))
    if row.get('id') == node_id:
        print(row['datadir'])
        sys.exit(0)
sys.exit(1)
" "$node_id" | { read -r datadir; [ -n "$datadir" ] || { echo "topo_log: no such node id '$node_id' in $abs" >&2; return 1; }; printf '%s/%s/node.log\n' "$REPO_ROOT" "$datadir"; }
}

# ---------------------------------------------------------------------------
# topo_nodes <topology.json> [role] -> node ids, one per line, declaration
# order, optionally filtered to role=sealer|follower.
# ---------------------------------------------------------------------------
topo_nodes() {
  [ $# -ge 1 ] || { echo "topo_nodes: usage: topo_nodes <topology.json> [role]" >&2; return 1; }
  local topo_file="$1" role="${2:-}" abs out
  abs=$(_topo_abspath "$topo_file") || return 1
  out=$("$_TOPO_TOPO_SH" print "$abs") || return 1
  printf '%s\n' "$out" | python3 -c "
import sys

role = sys.argv[1]
header = None
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('# ') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    row = dict(zip(header, line.split('\t')))
    if role == '' or row.get('role') == role:
        print(row['id'])
" "$role"
}

# ---------------------------------------------------------------------------
# topo_stop <topology.json> <nodeId> [--force]: graceful-only (D3),
# delegates entirely to node.sh stop.
# ---------------------------------------------------------------------------
topo_stop() {
  [ $# -ge 2 ] || { echo "topo_stop: usage: topo_stop <topology.json> <nodeId> [--force]" >&2; return 1; }
  local topo_file="$1" node_id="$2"; shift 2
  local abs; abs=$(_topo_abspath "$topo_file") || return 1
  "$_TOPO_NODE_SH" stop "$abs" "$node_id" "$@"
}

# ---------------------------------------------------------------------------
# topo_partition / topo_heal (ItWorksinMyLocal#46 T3.2)
#
# Generalizes lab/lib-lab.sh's lab_partition/lab_heal (which hardcode the
# fixed node1..node4 producer set and its own port registry) to ANY node id
# in ANY netlab topology, via admin_removePeer/admin_addPeer -- same
# mechanism, same docs/lab/DESIGN.md §6 rationale (peer manipulation on the
# admin API, not iptables/pfctl -- deterministic and portable).
#
# ADMIN-API-CAPABLE CLIENTS ONLY: not every client this framework can bring
# up actually exposes admin_removePeer/admin_addPeer/admin_nodeInfo. Per
# join.sh's own per-client flag lists (--http.api / --rpcapi) and
# docs/lab/DESIGN.md §3's client registry:
#   oldxdc, geth, besu, reth, xone   -- all enable `admin` -- SUPPORTED
#   erigon                          -- join.sh never passes `admin` in its
#                                      --http.api list -- EXCLUDED
#   nethermind                      -- its dist config doesn't expose the
#                                      peer-control admin methods either
#                                      (DESIGN.md §3: "via config", no
#                                      confirmed admin_removePeer/addPeer) --
#                                      EXCLUDED
# topo_partition refuses outright (fails fast, before touching any peer) if
# EITHER group contains a node running an excluded client -- this is an
# honest capability gap, not something to silently skip past.
_TOPO_ADMIN_CAPABLE_CLIENTS="oldxdc geth besu reth xone"

# _topo_row_field <topology.json> <nodeId> <column> -> that column's value
# from topo.sh print's resolved row (e.g. "client"), or empty + nonzero if
# no such node id exists in the topology.
_topo_row_field() {
  local topo_file="$1" node_id="$2" col="$3" abs out
  abs=$(_topo_abspath "$topo_file") || return 1
  out=$("$_TOPO_TOPO_SH" print "$abs") || return 1
  printf '%s\n' "$out" | python3 -c "
import sys

node_id, col = sys.argv[1], sys.argv[2]
header = None
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('# ') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    row = dict(zip(header, line.split('\t')))
    if row.get('id') == node_id:
        print(row.get(col, ''))
        sys.exit(0)
sys.exit(1)
" "$node_id" "$col"
}

# _topo_node_enode <topology.json> <nodeId>: that node's own live enode
# (admin_nodeInfo) -- caller must have already confirmed it's admin-capable.
_topo_node_enode() {
  local topo_file="$1" node_id="$2" url info
  url=$(topo_rpc "$topo_file" "$node_id") || return 1
  info=$(lab_rpc_call "$url" admin_nodeInfo "[]") || return 1
  python3 -c "
import json
import sys

print(json.loads(sys.argv[1]).get('enode', ''))
" "$info"
}

# _topo_toggle_peers <topology.json> <add|remove> <groupA ids> <groupB ids>:
# calls admin_<action>Peer across every A<->B pair, at each node's own admin
# RPC (mirrors lab/lib-lab.sh's _lab_toggle_peers, generalized to arbitrary
# topology node ids instead of the fixed node1..node4).
_topo_toggle_peers() {
  local topo_file="$1" action="$2" group_a="$3" group_b="$4"
  local a b url_a url_b enode_a enode_b
  for a in $group_a; do
    url_a=$(topo_rpc "$topo_file" "$a") || return 1
    enode_a=$(_topo_node_enode "$topo_file" "$a") || { echo "topo_partition: could not read enode for $a" >&2; return 1; }
    for b in $group_b; do
      url_b=$(topo_rpc "$topo_file" "$b") || return 1
      enode_b=$(_topo_node_enode "$topo_file" "$b") || { echo "topo_partition: could not read enode for $b" >&2; return 1; }
      lab_rpc_call "$url_a" "admin_${action}Peer" "[\"${enode_b}\"]" >/dev/null
      lab_rpc_call "$url_b" "admin_${action}Peer" "[\"${enode_a}\"]" >/dev/null
    done
  done
}

# topo_partition <topology.json> <groupA-ids> <groupB-ids>: splits the two
# (comma- or space-separated) node-id groups via admin_removePeer, recording
# the split (scoped to THIS topology, so partitioning two different
# topologies at once doesn't clobber each other's state) so topo_heal knows
# what to reconnect.
topo_partition() {
  [ $# -ge 3 ] || { echo "topo_partition: usage: topo_partition <topology.json> <groupA-ids> <groupB-ids>" >&2; return 1; }
  local topo_file="$1" group_a group_b
  group_a=$(printf '%s' "$2" | tr ',' ' ')
  group_b=$(printf '%s' "$3" | tr ',' ' ')
  [ -n "$(printf '%s' "$group_a" | tr -d '[:space:]')" ] || { echo "topo_partition: groupA is empty" >&2; return 1; }
  [ -n "$(printf '%s' "$group_b" | tr -d '[:space:]')" ] || { echo "topo_partition: groupB is empty" >&2; return 1; }

  # Validate EVERY node's client is admin-capable BEFORE touching a single
  # peer connection -- an honest refusal, not a partial partition.
  local id client bad=""
  for id in $group_a $group_b; do
    client=$(_topo_row_field "$topo_file" "$id" client) || { echo "topo_partition: no such node id '$id' in $topo_file" >&2; return 1; }
    case " $_TOPO_ADMIN_CAPABLE_CLIENTS " in
      *" $client "*) ;;
      *) bad="${bad}${bad:+, }${id}(${client})" ;;
    esac
  done
  if [ -n "$bad" ]; then
    echo "topo_partition: refusing -- these node(s) run a client with no admin peer-control API (erigon/nethermind excluded, see this function's header): $bad" >&2
    return 1
  fi

  local abs; abs=$(_topo_abspath "$topo_file") || return 1
  local state_file; state_file="$LAB_RESULTS_DIR/partition-$(basename "$abs" .json).state"
  _topo_toggle_peers "$topo_file" remove "$group_a" "$group_b" || return 1
  printf '%s|%s\n' "$group_a" "$group_b" > "$state_file"
  echo "topo_partition: split [$group_a] | [$group_b] for $(basename "$abs")" >&2
}

# topo_heal <topology.json>: reconnects the most recent topo_partition split
# for THIS topology via admin_addPeer.
topo_heal() {
  [ $# -ge 1 ] || { echo "topo_heal: usage: topo_heal <topology.json>" >&2; return 1; }
  local topo_file="$1" abs state_file
  abs=$(_topo_abspath "$topo_file") || return 1
  state_file="$LAB_RESULTS_DIR/partition-$(basename "$abs" .json).state"
  [ -f "$state_file" ] || { echo "topo_heal: no active partition for $(basename "$abs") (no $state_file)" >&2; return 1; }
  local line group_a group_b
  line=$(cat "$state_file")
  group_a="${line%%|*}"
  group_b="${line#*|}"
  _topo_toggle_peers "$topo_file" add "$group_a" "$group_b" || return 1
  rm -f "$state_file"
  echo "topo_heal: reconnected [$group_a] | [$group_b] for $(basename "$abs")" >&2
}
