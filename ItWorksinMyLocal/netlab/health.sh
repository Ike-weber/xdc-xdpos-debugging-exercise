#!/bin/bash
# health.sh - readiness gates + a preflight check for a netlab topology.
#
#   health.sh wait-rpc    <url> [timeout_s=60]
#   health.sh wait-peers  <url> <minPeers> [timeout_s=60]
#   health.sh wait-block  <url> <N> [timeout_s=120]
#   health.sh seated      <url> <expectedCount> [lookbackBlocks=50]
#   health.sh preflight   <topology.json> [--min-disk-gb N] [--min-ram-gb N]
#                                          [--min-cores N] [--force]
#
# wait-* are the gates fleet.sh (T1.5) chains between bring-up steps: "wait
# for a sealer's RPC to answer", "wait for a follower to reach peers>=1
# before starting the next one" (D5's staggered bring-up), etc. Each prints
# a one-line OK/TIMEOUT verdict and exits 0/1 accordingly -- safe to use
# directly in a shell `if` without capturing output.
#
# seated (ItWorksinMyLocal#97 item 4): reports a seated-but-absent
# masternode as a first-class FAILURE, not a quiet gap. The concrete
# incident: netv12 ran on 4 of 5 seated validators for ~40 minutes (a stale
# node3 the old teardown couldn't see -- see node.sh/fleet.sh's own #97
# comments) with every OTHER health signal green (peers up, blocks
# advancing) -- and every fair-share number measured in that window was
# meaningless, because a dead seat changes the denominator. `seated` scans
# the last `lookbackBlocks` blocks' `miner` field for distinct producers
# and compares the count against `expectedCount` (the caller's own
# masternode-set size, e.g. fleet.sh's TOPO_SEALER_COUNT):
#   OK      producing count >= expectedCount
#   FAIL    producing count <  expectedCount -- exit 1, loud
#   UNKNOWN every block in the window had a zero `miner` (exit 2) -- this
#           build's V1 header format doesn't populate `miner` as the actual
#           signer (only V2/BFT blocks reliably do; verified live against
#           netv12: block 10 and block 100, pre-V2, both read
#           0x000...000, while a recent V2 block read a real address).
#           Per-signer attribution for V1 needs a seal ecrecover this
#           script does NOT implement -- reporting a fabricated 0/N would
#           be worse than refusing to guess, so this returns a visibly
#           distinct UNKNOWN instead of a false OK or a false FAIL.
#
# preflight resolves the topology (via topo.sh print) and hard-checks, all
# BEFORE any process starts:
#   - disk:  free space on the filesystem holding the repo, against
#            --min-disk-gb (default 5)
#   - ram:   available memory, against --min-ram-gb (default 2)
#   - cores: online CPU count, against --min-cores (default 2) -- a
#            topology's sealers+followers all end up runnable on the SAME
#            box; too few cores is exactly how a shared dev host starves
#            block production under a heavy follower's sync load (T3.1;
#            the same class of problem 5551's mainnet-fast-follower
#            incident diagnosed at the OS level, see
#            xone-5551-recovery-playbook)
#   - ports: every resolved port (p2p/rpc/ws/authrpc/torrent/mcp/privapi
#            for every node, plus the topology's bootnode port) is checked
#            with a REAL bind() attempt, not just a passive listening-socket
#            scan -- this sandbox's ephemeral port range (32768-60999, see
#            /proc/sys/net/ipv4/ip_local_port_range) overlaps the
#            framework's 34000+ band, so a long-lived process (postgres,
#            a dev server, ...) can transiently hold one of our ports as
#            its own OUTBOUND connection's ephemeral port; `ss -ltn` /
#            `lsof` won't show that as a listener, but bind() still fails.
#            Verified during T1.3's integration test: a fresh oldxdc join
#            crashed twice with "address already in use" on ports `ss`
#            reported as completely free. Because that borrow is
#            split-second, a single bind() sample can itself land in the
#            same transient window and misreport a genuinely-free port as
#            busy (seen live during T2.4/T2.5 acceptance: port 34104 read
#            busy once, free on every immediate retry) -- so the port
#            check is debounced (_port_free_debounced: a few retries with
#            a short backoff before declaring FAIL), which still catches
#            a REAL listener (busy on every attempt) but rides out a
#            one-sample ephemeral-port collision.
#   - gas-limit: every resolved node's mint/plateau target (topo.sh print's
#            "gasLimit" column -- topologies/*.json's `gasLimit` field,
#            threaded to join.sh --gas-limit) must be IDENTICAL across the
#            whole topology. This is NOT a check against the genesis
#            header's own gasLimit field (that field legitimately differs
#            -- see topologies/*.json's gasLimit doc, ItWorksinMyLocal#96);
#            it is the guard that would have caught #94's netv12 wedge
#            before a single process started: node.sh used to pass NO gas
#            flag at all, so the legacy oldxdc arbiter fell back to its
#            own compiled-in 50,000,000 default while geth/erigon
#            elsewhere targeted a hard-coded 420,000,000 -- two different
#            mint targets on one chain. A node whose resolved gasLimit
#            column is empty/non-positive (meaning it would silently fall
#            back to ITS OWN client default) is a WARN, not a hard FAIL --
#            topo.sh's own default (420000000) already makes this
#            practically unreachable today, but it is a real signal if a
#            future schema/regression ever lets it happen.
#
# ENFORCEMENT (T3.1): any real shortfall above is a hard FAIL and preflight
# exits nonzero -- fleet.sh's `up` treats that as fatal and aborts before
# starting a single process (see fleet.sh's own header). `--force` does NOT
# turn a FAIL into a PASS or hide it: every FAIL: line above still prints
# exactly as measured, but preflight exits 0 anyway so an operator who has
# deliberately decided a shortfall is acceptable (e.g. a resource-capped dev
# box that's fine running a 2-node smoke topology) can proceed. This is an
# explicit, visible operator override of a resource check, not a masked
# scenario verdict -- it never touches disk/ram/cores/port THRESHOLDS
# themselves and never silently downgrades a FAIL to a PASS/SKIP.
#
# Uses lib-rpc.sh directly (topology-independent -- see T0.1) rather than
# lab/lib-lab.sh, so this has no dependency on the fixed lab/ port registry.

_NETLAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_NETLAB_DIR/.." && pwd)" || exit 1
TOPO_SH="$_NETLAB_DIR/topo.sh"

# shellcheck source=../lib-rpc.sh
source "$REPO_ROOT/lib-rpc.sh"

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") wait-rpc    <url> [timeout_s=60]
       $(basename "$0") wait-peers  <url> <minPeers> [timeout_s=60]
       $(basename "$0") wait-block  <url> <N> [timeout_s=120]
       $(basename "$0") seated      <url> <expectedCount> [lookbackBlocks=50]
       $(basename "$0") preflight   <topology.json> [--min-disk-gb N] [--min-ram-gb N]
EOF
  exit 1
}

[ $# -ge 1 ] || usage
CMD="$1"; shift

# ---------------------------------------------------------------------------
# wait-rpc
# ---------------------------------------------------------------------------
cmd_wait_rpc() {
  [ $# -ge 1 ] || usage
  local url="$1" timeout="${2:-60}" waited=0
  while :; do
    if lab_rpc_call "$url" eth_blockNumber "[]" >/dev/null 2>&1; then
      echo "OK: rpc up at $url (waited ${waited}s)"
      return 0
    fi
    [ "$waited" -ge "$timeout" ] && { echo "TIMEOUT: rpc never came up at $url after ${timeout}s" >&2; return 1; }
    sleep 2; waited=$((waited + 2))
  done
}

# ---------------------------------------------------------------------------
# wait-peers -- uses lab_peer_count (net_peerCount), NOT admin_peers: erigon
# never enables the `admin` RPC namespace (see lib-rpc.sh's lab_peer_count
# header), so an admin_peers-based gate would misreport a perfectly healthy
# erigon follower as stuck at 0 peers forever -- exactly the false-FAIL
# ItWorksinMyLocal#46 T3.3's all-clients-soak scenario exists to NOT
# reproduce. net_peerCount is answered by every client this framework can
# bring up (the "net" RPC namespace, unlike "admin", is universal).
# ---------------------------------------------------------------------------
cmd_wait_peers() {
  [ $# -ge 2 ] || usage
  local url="$1" min="$2" timeout="${3:-60}" waited=0 n
  while :; do
    n=$(lab_peer_count "$url" 2>/dev/null) || n=0
    [ -n "$n" ] || n=0
    if [ "$n" -ge "$min" ] 2>/dev/null; then
      echo "OK: $url has $n peer(s) (>= $min, waited ${waited}s)"
      return 0
    fi
    [ "$waited" -ge "$timeout" ] && { echo "TIMEOUT: $url only has $n peer(s) (< $min) after ${timeout}s" >&2; return 1; }
    sleep 2; waited=$((waited + 2))
  done
}

# ---------------------------------------------------------------------------
# wait-block (thin CLI wrapper around lib-rpc.sh's lab_wait_block)
# ---------------------------------------------------------------------------
cmd_wait_block() {
  [ $# -ge 2 ] || usage
  local url="$1" target="$2" timeout="${3:-120}"
  if lab_wait_block "$url" "$target" "$timeout"; then
    echo "OK: $url reached block >= $target"
    return 0
  else
    echo "TIMEOUT: $url did not reach block >= $target after ${timeout}s (at $(lab_block_number "$url" 2>/dev/null || echo '?'))" >&2
    return 1
  fi
}

# ---------------------------------------------------------------------------
# seated -- see the file header for the full rationale (ItWorksinMyLocal#97
# item 4). Scans the last <lookbackBlocks> blocks' `miner` field for
# distinct nonzero producers and compares against <expectedCount>.
# ---------------------------------------------------------------------------
cmd_seated() {
  [ $# -ge 2 ] || usage
  local url="$1" expected="$2" lookback="${3:-50}"
  [ "$expected" -gt 0 ] 2>/dev/null || { echo "health.sh seated: expectedCount must be a positive integer" >&2; return 1; }

  local head
  head=$(lab_block_number "$url" 2>/dev/null) || { echo "FAIL: seated: could not read block number at $url" >&2; return 1; }

  local start=$((head - lookback + 1))
  [ "$start" -lt 0 ] && start=0

  local -A seen=()
  local sawnonzero=0 n hexn blk miner
  for ((n = start; n <= head; n++)); do
    hexn=$(printf '0x%x' "$n")
    blk=$(lab_rpc_call "$url" eth_getBlockByNumber "[\"${hexn}\",false]" 2>/dev/null) || continue
    # int(miner,16)==0 (not a string-literal compare): this repo has seen
    # the 0x-prefixed hex render with an odd digit count on some builds, so
    # comparing the numeric value is the robust "is this the zero address"
    # check rather than matching one specific zero-padding width.
    miner=$(printf '%s' "$blk" | python3 -c "
import json
import sys

try:
    b = json.load(sys.stdin) or {}
except Exception:
    b = {}
m = (b.get('miner') or '').lower()
try:
    zero = int(m, 16) == 0
except Exception:
    zero = True
print('' if zero else m)
" 2>/dev/null)
    [ -n "$miner" ] || continue
    sawnonzero=1
    seen["$miner"]=1
  done

  local producing=${#seen[@]}
  local window=$((head - start + 1))

  if [ "$sawnonzero" -eq 0 ]; then
    echo "UNKNOWN: seated: every block's miner/coinbase in the last $window blocks (#$start..#$head) at $url was the zero address" >&2
    echo "  -- this build's V1 header format doesn't populate miner as the actual signer" >&2
    echo "  (only V2/BFT blocks reliably do); per-signer attribution for V1 needs a seal" >&2
    echo "  ecrecover this script does not implement. Treat this as UNKNOWN, not 0/$expected" >&2
    echo "  -- that would be a fabricated number, not a measurement (ItWorksinMyLocal#97)." >&2
    return 2
  fi

  if [ "$producing" -lt "$expected" ]; then
    echo "FAIL: seated: only $producing of $expected masternode(s) produced a block in the last $window (#$start..#$head): ${!seen[*]}" >&2
    echo "  every fair-share number measured while the set is short a seat is INVALID --" >&2
    echo "  find and fix the absent validator(s) before trusting any measurement taken now." >&2
    return 1
  fi

  echo "OK: seated: $producing of $expected masternode(s) produced a block in the last $window (#$start..#$head)"
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
_port_free() {
  # $1: bare port number. Real bind() test (0.0.0.0), not a passive scan --
  # see the file header for why that matters on this sandbox.
  python3 -c "
import socket
import sys

s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(('0.0.0.0', int(sys.argv[1])))
    ok = True
except OSError:
    ok = False
s.close()
sys.exit(0 if ok else 1)
" "$1"
}

# _port_free_debounced <port>: the file header already documents that this
# sandbox's ephemeral port range (32768-60999) overlaps our 34000+ band, so
# an unrelated process's OUTBOUND connection can transiently borrow one of
# our ports for a few hundred ms -- a single bind() sample can catch that
# split-second window and misreport a port that is not actually reserved
# by anything as "in use". A GENUINELY held port (another listener bound
# to it) fails every attempt; a transient ephemeral borrow clears within
# a couple of retries. So retry a few times with a short backoff and only
# report busy if every attempt failed.
_port_free_debounced() {
  local port="$1" tries="${2:-3}" delay="${3:-0.2}" i
  for ((i = 0; i < tries; i++)); do
    _port_free "$port" && return 0
    sleep "$delay"
  done
  return 1
}

cmd_preflight() {
  [ $# -ge 1 ] || usage
  local topo_file="$1"; shift
  local min_disk_gb=5 min_ram_gb=2 min_cores=2 force=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --min-disk-gb) min_disk_gb=$2; shift 2;;
      --min-ram-gb)  min_ram_gb=$2; shift 2;;
      --min-cores)   min_cores=$2; shift 2;;
      --force)       force=1; shift;;
      *) echo "health.sh preflight: unknown flag: $1" >&2; return 1;;
    esac
  done
  [ -f "$topo_file" ] || { echo "health.sh: no such topology file: $topo_file" >&2; return 1; }

  local fail=0

  # -- disk --
  local avail_kb avail_gb
  avail_kb=$(df -Pk "$REPO_ROOT" 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$avail_kb" ]; then
    avail_gb=$((avail_kb / 1024 / 1024))
    if [ "$avail_gb" -ge "$min_disk_gb" ]; then
      echo "PASS: disk: ${avail_gb}GB free on $(df -P "$REPO_ROOT" | awk 'NR==2{print $NF}') (>= ${min_disk_gb}GB)"
    else
      echo "FAIL: disk: only ${avail_gb}GB free (need >= ${min_disk_gb}GB)" >&2
      fail=1
    fi
  else
    echo "FAIL: disk: could not read free space for $REPO_ROOT" >&2
    fail=1
  fi

  # -- ram --
  local avail_mem_kb avail_mem_gb
  avail_mem_kb=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo 2>/dev/null)
  if [ -n "$avail_mem_kb" ]; then
    avail_mem_gb=$((avail_mem_kb / 1024 / 1024))
    if [ "$avail_mem_gb" -ge "$min_ram_gb" ]; then
      echo "PASS: ram: ${avail_mem_gb}GB available (>= ${min_ram_gb}GB)"
    else
      echo "FAIL: ram: only ${avail_mem_gb}GB available (need >= ${min_ram_gb}GB)" >&2
      fail=1
    fi
  else
    echo "FAIL: ram: could not read /proc/meminfo MemAvailable" >&2
    fail=1
  fi

  # -- cores --
  local cores
  cores=$(nproc 2>/dev/null) || cores=""
  [ -n "$cores" ] || cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null) || cores=""
  [ -n "$cores" ] || cores=$(awk '/^processor/{c++} END{print c+0}' /proc/cpuinfo 2>/dev/null)
  if [ -n "$cores" ] && [ "$cores" -gt 0 ] 2>/dev/null; then
    if [ "$cores" -ge "$min_cores" ]; then
      echo "PASS: cores: $cores online (>= $min_cores)"
    else
      echo "FAIL: cores: only $cores online (need >= $min_cores)" >&2
      fail=1
    fi
  else
    echo "FAIL: cores: could not determine the online CPU count" >&2
    fail=1
  fi

  # -- ports (resolve the topology first) --
  local out
  out="$("$TOPO_SH" print "$topo_file")"
  if [ $? -ne 0 ]; then
    echo "FAIL: topology did not validate/resolve -- see errors above" >&2
    return 1
  fi
  local bootnode_port
  bootnode_port=$(printf '%s\n' "$out" | sed -n 's/^# bootnode_port=//p')
  local ports
  ports=$(printf '%s\n' "$out" | python3 -c "
import sys

header = None
ports = set()
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('#') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    row = dict(zip(header, line.split('\t')))
    for col in ('p2p', 'rpc', 'ws', 'authrpc', 'torrent', 'mcp'):
        v = row.get(col, '')
        if v:
            ports.add(int(v))
    priv = row.get('privapi', '')
    if ':' in priv:
        ports.add(int(priv.rsplit(':', 1)[1]))
print(' '.join(str(p) for p in sorted(ports)))
")
  [ -n "$bootnode_port" ] && ports="$ports $bootnode_port"

  # Re-scan only the still-busy ports across several passes: an unrelated
  # process's ephemeral-port borrow (see header) can hold a given port of
  # ours for a few seconds at a time (an established connection, not just
  # a one-sample blip -- confirmed live during T2.4/T2.5 acceptance: port
  # 34100 read busy on 20 consecutive single-shot samples ~200ms apart,
  # then cleared), so a handful of quick per-port retries (_port_free_debounced)
  # isn't always enough on its own. Re-running the FULL scan (not
  # per-port-sequential debouncing, which multiplies cost across every
  # port) over up to $port_retry_seconds bounds the worst case while still
  # riding out a multi-second borrow. A port held by a REAL listener stays
  # busy across every pass and correctly still fails at the end.
  local port_retry_seconds="${NETLAB_PREFLIGHT_PORT_RETRY_SECONDS:-20}"
  local pass_sleep=2
  local busy="" p elapsed=0
  for p in $ports; do busy="$busy $p"; done
  while :; do
    local still_busy=""
    for p in $busy; do
      _port_free_debounced "$p" || still_busy="$still_busy $p"
    done
    busy="$still_busy"
    [ -z "$busy" ] && break
    [ "$elapsed" -ge "$port_retry_seconds" ] && break
    sleep "$pass_sleep"
    elapsed=$((elapsed + pass_sleep))
  done
  if [ -z "$busy" ]; then
    echo "PASS: ports: all $(echo "$ports" | wc -w) resolved ports are free"
  else
    echo "FAIL: ports: already in use (bind test):$busy" >&2
    fail=1
  fi

  # -- gas-limit consistency (ItWorksinMyLocal#94/#96) --
  # Every resolved node's mint/plateau target (topo.sh print's "gasLimit"
  # column) must be the SAME number across the whole topology -- see this
  # file's header for why this is deliberately NOT a check against the
  # genesis header's own gasLimit field (those are legitimately different).
  local gl_report
  gl_report=$(printf '%s\n' "$out" | python3 -c "
import sys

header = None
rows = []
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('#') or not line:
        continue
    if header is None:
        header = line.split('\t')
        continue
    rows.append(dict(zip(header, line.split('\t'))))

missing = [r['id'] for r in rows if not r.get('gasLimit') or not r['gasLimit'].lstrip('-').isdigit() or int(r['gasLimit']) <= 0]
present = [(r['id'], r['gasLimit']) for r in rows if r.get('gasLimit') and r['gasLimit'].lstrip('-').isdigit() and int(r['gasLimit']) > 0]
targets = sorted(set(v for _, v in present))

if missing:
    print('WARN missing=%s' % ','.join(missing))
if len(targets) > 1:
    detail = ', '.join('%s=%s' % (i, v) for i, v in present)
    print('FAIL multiple=%s detail=%s' % (','.join(targets), detail))
if not missing and len(targets) <= 1:
    print('OK target=%s' % (targets[0] if targets else 'n/a'))
")
  while IFS= read -r gl_line; do
    [ -n "$gl_line" ] || continue
    case "$gl_line" in
      OK\ *)
        echo "PASS: gas-limit: every resolved node targets the same mint ceiling (${gl_line#OK target=})" ;;
      WARN\ missing=*)
        echo "WARN: gas-limit: node(s) [${gl_line#WARN missing=}] resolved no explicit gasLimit -- they would silently fall back to their OWN client default (this is exactly ItWorksinMyLocal#94's netv12 wedge: no flag passed, arbiter defaulted to 50,000,000 while geth/erigon elsewhere targeted 420,000,000)" >&2 ;;
      FAIL\ multiple=*)
        echo "FAIL: gas-limit: topology resolves DIFFERENT mint ceilings across its nodes (${gl_line#FAIL multiple=}) -- every node must target the SAME plateau or a client-sealed block ratchets the gas limit until the legacy arbiter's misc.VerifyGaslimit permanently locks every validator out of proposing (ItWorksinMyLocal#94)" >&2
        fail=1 ;;
    esac
  done <<<"$gl_report"

  if [ "$fail" -eq 0 ]; then
    echo "PREFLIGHT OK"
    return 0
  fi
  if [ "$force" -eq 1 ]; then
    # --force: every FAIL: line above already printed the real measured
    # shortfall -- this does not change any of them, it only decides that a
    # shortfall isn't fatal. Still exits 0 so `fleet.sh up --force` proceeds.
    echo "PREFLIGHT FAILED but continuing: --force overrode the FAIL(s) above" >&2
    return 0
  fi
  echo "PREFLIGHT FAILED" >&2
  return 1
}

case "$CMD" in
  wait-rpc)   cmd_wait_rpc "$@" ;;
  wait-peers) cmd_wait_peers "$@" ;;
  wait-block) cmd_wait_block "$@" ;;
  seated)     cmd_seated "$@" ;;
  preflight)  cmd_preflight "$@" ;;
  *) usage ;;
esac
