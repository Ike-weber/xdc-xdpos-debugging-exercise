#!/bin/bash
# topo.sh - parse/validate a netlab topology JSON -> a normalized node table.
#
# See netlab/TOPOLOGY.md for the full schema and every constraint enforced
# below. Two subcommands:
#
#   topo.sh validate <topology.json>   # exit 0 + "OK: ..." summary, or exit
#                                       # 1 + one "ERROR: ..." line per
#                                       # violation on stderr (collects all
#                                       # of them before exiting, not just
#                                       # the first)
#   topo.sh print    <topology.json>   # validates first (same as above),
#                                       # then emits the resolved node table
#                                       # to stdout: a few "# key=value"
#                                       # topology-level comment lines
#                                       # (name/chainId/epoch/v2block/period/
#                                       # reward/gap/gasLimit/skipV1Validation/
#                                       # portBase/staggerSeconds/
#                                       # bootnode_port/node_count/
#                                       # sealer_count), then a TSV header +
#                                       # one row per resolved (post {A..B}
#                                       # expansion) node: id, client, role,
#                                       # keyref, maxpeers, gasLimit, p2p,
#                                       # rpc, ws, authrpc, torrent, mcp,
#                                       # privapi, datadir. This is exactly
#                                       # what fleet.sh writes to
#                                       # nodes/<name>/topo.resolved.tsv.
#                                       # gasLimit (ItWorksinMyLocal#96) is
#                                       # the mint/plateau target every
#                                       # producer is launched with -- NOT
#                                       # the genesis header's own gasLimit,
#                                       # which this framework never
#                                       # generates or changes here.
#
# ONE hard requirement for `print`'s keyRef check: this process's
# environment must already have .env's PRIVATE_KEY_* vars loaded (topo.sh
# does this itself, sourcing REPO_ROOT/.env if present) -- a topology that
# references a keyRef which isn't actually set fails fast here, before any
# process starts.
#
# Uses inline python3 for the JSON parse/validate/resolve (already a hard
# dependency across this repo -- lab/lib-lab.sh, gen-genesis.sh, etc).

_NETLAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
REPO_ROOT="$(cd "$_NETLAB_DIR/.." && pwd)" || exit 1

usage() {
  echo "usage: $(basename "$0") validate <topology.json>" >&2
  echo "       $(basename "$0") print    <topology.json>" >&2
  exit 1
}

[ $# -eq 2 ] || usage
MODE="$1"
TOPO_FILE="$2"
case "$MODE" in
  validate|print) ;;
  *) usage ;;
esac

[ -f "$TOPO_FILE" ] || { echo "topo.sh: no such topology file: $TOPO_FILE" >&2; exit 1; }
# Resolve to an absolute path up front: the python step below needs it, and
# it's about to be printed verbatim into error messages.
TOPO_FILE="$(cd "$(dirname "$TOPO_FILE")" && pwd)/$(basename "$TOPO_FILE")" || exit 1

# Load .env (PRIVATE_KEY_* etc) the same way run.sh/join.sh do, so a
# role=sealer node's keyRef can be checked against the real environment.
( cd "$REPO_ROOT" && set -a && [ -f .env ] && . ./.env; set +a
  exec python3 - "$MODE" "$TOPO_FILE" "$REPO_ROOT" <<'PY'
import json
import os
import re
import sys

mode, topo_path, repo_root = sys.argv[1], sys.argv[2], sys.argv[3]
errors = []


def err(msg):
    errors.append(msg)


# ---------------------------------------------------------------------------
# Load
# ---------------------------------------------------------------------------
try:
    with open(topo_path) as f:
        raw = f.read()
except OSError as e:
    print("topo.sh: ERROR: cannot read %s: %s" % (topo_path, e), file=sys.stderr)
    sys.exit(1)

try:
    topo = json.loads(raw)
except json.JSONDecodeError as e:
    print("topo.sh: ERROR: %s is not valid JSON: %s" % (topo_path, e), file=sys.stderr)
    sys.exit(1)

if not isinstance(topo, dict):
    print("topo.sh: ERROR: %s: top level must be a JSON object" % topo_path, file=sys.stderr)
    sys.exit(1)

# ---------------------------------------------------------------------------
# Top-level fields (with defaults)
# ---------------------------------------------------------------------------
name = topo.get("name")
stem = os.path.splitext(os.path.basename(topo_path))[0]
if not name:
    err("missing required field: name")
elif name != stem:
    err("name '%s' must match the filename stem '%s' (%s)" % (name, stem, os.path.basename(topo_path)))

description = topo.get("description", "")

def require_int(field, required=True, default=None):
    if field not in topo:
        if required:
            err("missing required field: %s" % field)
        return default
    v = topo[field]
    if not isinstance(v, int) or isinstance(v, bool):
        err("field '%s' must be an integer (got %r)" % (field, v))
        return default
    return v

chain_id = require_int("chainId")
epoch = require_int("epoch")
v2block = require_int("v2block")
period = require_int("period", required=False, default=1)
reward = require_int("reward", required=False, default=5000)
gap = require_int("gap", required=False, default=450)
# gasLimit: the MINT/PLATEAU target every producer in this topology is
# launched with (join.sh --gas-limit -> the per-client gas-ceiling flag),
# NOT the genesis header's own gasLimit field -- those are legitimately
# DIFFERENT numbers and must stay that way (ItWorksinMyLocal#94/#96).
# geth-xdc/erigon-xdc hard-code a 420,000,000 "XDC plateau" they hone
# towards at runtime (parent/1024 per block, same mechanism live XDC
# mainnet's own history shows: mainnet genesis is 4,700,000 and its current
# plateau is 420,000,000 -- it got there by ratcheting up over blocks, the
# genesis field was never edited). #94's netv12 wedge was never a
# genesis-vs-target mismatch -- it was the legacy arbiter falling back to
# ITS OWN 50,000,000 default (no --targetgaslimit passed) while geth/erigon
# targeted 420,000,000: TWO DIFFERENT TARGETS on one chain. This field
# forces every producer of every client onto the SAME target so that can
# never happen again; it must never be derived from (or made equal to
# forcing a change of) any genesis file.
gas_limit = require_int("gasLimit", required=False, default=420000000)
if gas_limit is not None and gas_limit < 1:
    err("gasLimit must be a positive integer (got %r)" % gas_limit)
port_base = require_int("portBase")
stagger_seconds = require_int("staggerSeconds", required=False, default=5)
skip_v1_validation = bool(topo.get("skipV1Validation", False))

# peering (ItWorksinMyLocal#46 T3.2): "hub" (default) keeps D5's original
# model -- followers dial ONLY the sealers, never each other. "mesh" lets
# fleet.sh also hand each follower every EARLIER-started follower's own
# enode as an additional bootnode (still staggered, still one at a time),
# so followers can peer with each other too -- useful for exercising a
# follower's own peer-management/eviction behavior under a denser graph,
# not just its sync-from-sealer path. Neither value changes how SEALERS
# peer with each other (always through the topology's own bootnode).
peering = topo.get("peering", "hub")
if peering not in ("hub", "mesh"):
    err("field 'peering' must be 'hub' or 'mesh' (got %r)" % peering)

# docs/lab/DESIGN.md's "Documented risks & forbidden knobs" section is
# explicit: "Forbidden: SkipV1Validation / SkipV2Validation -- they disable
# the validation the lab exists to compare." This lab is a
# validation-parity harness (DESIGN.md section 1) -- a topology that ships
# with this genesis flag set true doesn't demonstrate parity, it just turns
# off whichever check the flag guards. Hard-error here (not just document
# it) so a topology JSON can't silently reintroduce the masked-pass finding
# ItWorksinMyLocal#46 was gated on (scenario 12's mixed-5 PASS resting on
# this exact flag).
if skip_v1_validation:
    err("skipV1Validation: true is forbidden (docs/lab/DESIGN.md, "
        "'Documented risks & forbidden knobs') -- it disables the V1 "
        "blocksigner/difficulty validation this lab exists to exercise; "
        "set it to false (or omit the field -- false is the default) and "
        "let any real V1 divergence surface as a scenario result instead "
        "of being masked out of the genesis")

if epoch is not None and epoch != 900:
    err("epoch must be 900 (legacy oldxdc hardcodes EpocBlockRandomize=900 "
        "independent of this genesis field; any other value freezes the "
        "chain at the first checkpoint) -- got %r" % epoch)

if v2block is not None and epoch is not None and epoch != 0 and v2block % epoch != 0:
    err("v2block (%r) must be a multiple of epoch (%r) -- the V1->V2 switch "
        "only happens at a checkpoint boundary" % (v2block, epoch))

if port_base is not None and port_base < 34000:
    err("portBase must be >= 34000 (the framework's reserved port band -- "
        "the live net5151 stack owns 9645-10245, net5050/postgres own "
        "their own range) -- got %r" % port_base)

nodes_raw = topo.get("nodes")
if not isinstance(nodes_raw, list) or not nodes_raw:
    err("missing or empty required field: nodes (must be a non-empty array)")
    nodes_raw = []

# ---------------------------------------------------------------------------
# Node id {A..B} expansion
# ---------------------------------------------------------------------------
RANGE_RE = re.compile(r'^(?P<prefix>.*?)\{(?P<a>\d+)\.\.(?P<b>\d+)\}(?P<suffix>.*)$')

KNOWN_CLIENTS = ("oldxdc", "geth", "erigon", "besu", "nethermind", "reth", "xone")
KNOWN_ROLES = ("sealer", "follower", "proposer")


def expand_node(entry, entry_idx):
    """Yields one or more (id, entry) pairs from a single JSON node entry,
    expanding "prefix{A..B}suffix" ids into prefixA..prefixB copies of the
    same entry (client/role/keyRef/maxpeers all shared -- see
    netlab/TOPOLOGY.md "Id expansion"). A plain id yields itself once."""
    if not isinstance(entry, dict):
        err("nodes[%d]: must be a JSON object" % entry_idx)
        return
    node_id = entry.get("id")
    if not node_id or not isinstance(node_id, str):
        err("nodes[%d]: missing required field: id" % entry_idx)
        return
    m = RANGE_RE.match(node_id)
    if not m:
        yield node_id, entry
        return
    a, b = int(m.group("a")), int(m.group("b"))
    if a > b:
        err("nodes[%d]: id '%s': range {%d..%d} has start > end" % (entry_idx, node_id, a, b))
        return
    for i in range(a, b + 1):
        yield "%s%d%s" % (m.group("prefix"), i, m.group("suffix")), entry


resolved = []  # list of dicts: id, client, role, keyref, maxpeers
seen_ids = set()
for idx, entry in enumerate(nodes_raw):
    for node_id, entry in expand_node(entry, idx):
        if node_id in seen_ids:
            err("duplicate node id after expansion: '%s'" % node_id)
            continue
        seen_ids.add(node_id)

        client = entry.get("client")
        role = entry.get("role")
        keyref = entry.get("keyRef", "")
        maxpeers = entry.get("maxpeers", "")

        if client not in KNOWN_CLIENTS:
            err("node '%s': unknown client '%s' (must be one of: %s)" % (node_id, client, ", ".join(KNOWN_CLIENTS)))
        if role not in KNOWN_ROLES:
            err("node '%s': unknown role '%s' (must be one of: %s)" % (node_id, role, ", ".join(KNOWN_ROLES)))
        if role == "proposer":
            err("node '%s': role 'proposer' is reserved and not yet supported "
                "(runtime-rejected pending go-ethereum#1341) -- remove this node "
                "or change its role" % node_id)
        if role == "sealer" and client != "oldxdc":
            err("node '%s': role 'sealer' requires client 'oldxdc' (only oldxdc "
                "can seal via join.sh --mine; got client '%s')" % (node_id, client))
        if role == "sealer":
            if not keyref:
                err("node '%s': role 'sealer' requires keyRef (name of an env "
                    "var holding the validator private key)" % node_id)
            elif mode == "print" and not os.environ.get(keyref):
                err("node '%s': keyRef '%s' is not set in the environment "
                    "(check .env)" % (node_id, keyref))
        if client == "nethermind" and chain_id is not None and chain_id != 5151:
            err("node '%s': client 'nethermind' only loads its own bundled "
                "net5151 chainspec, not an arbitrary genesis file -- it cannot "
                "join a custom chainId (%r) topology (ItWorksinMyLocal#46 open "
                "question #2)" % (node_id, chain_id))
        if maxpeers != "" and (not isinstance(maxpeers, int) or isinstance(maxpeers, bool)):
            err("node '%s': maxpeers must be an integer (got %r)" % (node_id, maxpeers))

        resolved.append({
            "id": node_id,
            "client": client or "",
            "role": role or "",
            "keyref": keyref,
            "maxpeers": maxpeers,
            # Every node in a topology shares the SAME mint/plateau target --
            # there is no per-node override (unlike maxpeers) -- precisely so
            # health.sh's preflight can assert "every resolved node agrees"
            # trivially by construction, not by hoping nothing diverges.
            "gasLimit": gas_limit,
        })

sealer_count = sum(1 for n in resolved if n["role"] == "sealer")
if nodes_raw and sealer_count == 0:
    err("topology has no role=sealer node -- nothing to generate/join a chain against")

if errors:
    for e in errors:
        print("topo.sh: ERROR: %s" % e, file=sys.stderr)
    sys.exit(1)

# ---------------------------------------------------------------------------
# Role-based maxpeers defaults (ItWorksinMyLocal#46 T3.1): a node whose JSON
# entry didn't set its own "maxpeers" gets one derived from its ROLE instead
# of silently falling through to whatever the client binary itself defaults
# to (e.g. geth's compiled-in 25) -- which knows nothing about how many
# OTHER nodes this particular topology actually has.
#
# A sealer dials every other sealer through the shared bootnode AND accepts
# a dial from every follower (hub peering, the default) or from every other
# follower too (mesh peering -- see the "peering" field below): its cap must
# hold the whole topology's resolved node count, not a fixed number, or a
# large topology can starve a SEALER's peer slots and reproduce the exact
# peer-starvation regression (scenario 13, all-clients-soak) at the sealer
# end instead of the follower end. MAXPEERS_SLACK leaves room for a human
# operator's own manual admin_addPeer / a debugging RPC client on top of the
# topology's own node count.
#
# A follower, by contrast, only ever needs a small, FIXED number of slots
# (a handful of sealers in hub mode, plus -- in mesh mode -- a handful of
# other followers): capping it independent of N keeps a follower from
# becoming a peer-storm sink candidate itself as the topology grows.
#
# Either default is just that -- a default. An explicit per-node "maxpeers"
# in the topology JSON always wins (checked below).
MAXPEERS_SLACK = 5
FOLLOWER_MAXPEERS_CAP = 15
sealer_maxpeers_default = len(resolved) + MAXPEERS_SLACK
for n in resolved:
    if n["maxpeers"] == "":
        n["maxpeers"] = sealer_maxpeers_default if n["role"] == "sealer" else FOLLOWER_MAXPEERS_CAP

# ---------------------------------------------------------------------------
# Port math (netlab/TOPOLOGY.md "Port math"): portBase + i*20 per resolved
# node (i = zero-based position after expansion, in declaration order); the
# bootnode gets the port just past the last node's block.
# ---------------------------------------------------------------------------
PORT_WIDTH = 20
for i, n in enumerate(resolved):
    base = port_base + i * PORT_WIDTH
    n["p2p"] = base + 0
    n["rpc"] = base + 1
    n["ws"] = base + 2
    n["authrpc"] = base + 3
    n["torrent"] = base + 4
    n["mcp"] = base + 5
    n["privapi"] = "127.0.0.1:%d" % (base + 6)
    n["datadir"] = "nodes/%s/%s" % (name, n["id"])

bootnode_port = port_base + len(resolved) * PORT_WIDTH

if mode == "validate":
    print("OK: %s: %d node(s) (%d sealer, %d follower), chainId=%s portBase=%s bootnode_port=%s"
          % (name, len(resolved), sealer_count, len(resolved) - sealer_count, chain_id, port_base, bootnode_port))
    sys.exit(0)

# mode == "print"
meta = [
    ("name", name),
    ("description", description),
    ("chainId", chain_id),
    ("epoch", epoch),
    ("v2block", v2block),
    ("period", period),
    ("reward", reward),
    ("gap", gap),
    ("gasLimit", gas_limit),
    ("skipV1Validation", "true" if skip_v1_validation else "false"),
    ("peering", peering),
    ("portBase", port_base),
    ("staggerSeconds", stagger_seconds),
    ("bootnode_port", bootnode_port),
    ("node_count", len(resolved)),
    ("sealer_count", sealer_count),
]
for k, v in meta:
    print("# %s=%s" % (k, v))

cols = ["id", "client", "role", "keyref", "maxpeers", "gasLimit", "p2p", "rpc", "ws", "authrpc", "torrent", "mcp", "privapi", "datadir"]
print("\t".join(cols))
for n in resolved:
    print("\t".join(str(n[c]) for c in cols))
PY
)
