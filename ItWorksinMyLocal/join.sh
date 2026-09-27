#!/bin/bash
# Connect to and sync an EXISTING network as a (non-mining) peer.
# You supply that network's genesis file and one or more bootnode enodes.
#
# Usage:
#   ./join.sh --genesis path/to/genesis.json         # uses default host + peers
#   ./join.sh --genesis g.json --ip 10.0.0.5         # override the host
#   ./join.sh --genesis g.json --bootnodes "enode://KEY@HOST:30301"
#
# Defaults (no --ip / --staticpeers needed): the target host comes from
# network.env (DEFAULT_HOST) and the peers from peers.list -- edit those to
# point at your own network. --ip / --bootnodes / --staticpeers still override.
#
# Flags:
#   --network NAME       public XDC network to join: mainnet (chainId 50) |
#                        testnet/apothem (chainId 51) | devnet5151 (default).
#                        mainnet/testnet use the vendored public genesis
#                        (genesis/genesis-<net>.json) + public bootnodes
#                        (bootnodes/<net>.list, from XinFinOrg/XinFin-Node) by
#                        default. Works with --client <c> or --client all.
#   --syncmode MODE      snap | fast | full. Default: snap for --network
#                        mainnet/testnet (bounds disk to recent state), full for
#                        devnet5151. Mapped per client (geth/xone --syncmode, besu
#                        --sync-mode, nethermind --Sync.SnapSync/FastSync; erigon +
#                        reth snap/stage-sync by default so they ignore it).
#   --genesis FILE       the target network's genesis.json (overrides --network;
#                        defaults to DEFAULT_GENESIS in network.env -- devnet 5151)
#   --ip IP[:PORT]       just give the target machine's IP -- connects to its
#                        bootnode (shared bootnode.key) at PORT (default 30301).
#                        Simplest way to join a network on another host.
#   --bootnodes LIST     full enode URL(s) to dial, if you have them
#                        (overrides --ip; default: derived from ./bootnode.key)
#   --host IP[:PORT]     alias of --ip
#   --client C           node client: oldxdc (default) | geth | geth4 | erigon |
#                        besu | nethermind | reth | xone | all. geth4 is the
#                        1.17.4 pin (linux-amd64 only, so it is skipped on
#                        darwin); it was accepted here long before this line
#                        listed it. xone is the go-ethereum-
#                        derived XDPoS reference client (XDCIndia/xOneGo). "all"
#                        boots every client at once, each in its own datadir
#                        (nodes/<client>-<type>) with a distinct 100-wide port
#                        block, all joining the same network -- a one-command
#                        cross-client bring-up. Only oldxdc can seal
#                        (--mine); all others join sync-only. All clients default
#                        to a
#                        prebuilt-binary download (binaries.json) for this
#                        platform, falling back to a source build for
#                        oldxdc/geth/erigon if unavailable; BIN_SOURCE=build
#                        (or per-client BIN_SOURCE_<client>=build) forces a
#                        build / skips the download. besu/nethermind/reth also
#                        honor OLDXDC_BIN / BESU_BIN / RETH_BIN / NETHERMIND_DIST (or PATH)
#                        ahead of both. besu & reth read the geth-format
#                        --genesis natively; erigon loads it directly (v3.5.0+); nethermind
#                        uses its own chainspec.
#   --chain NAME         erigon only: built-in chainspec (xdc | xdc-apothem). Optional
#                        -- erigon v3.5.0+ loads the --genesis file directly.
#   --nm-config NAME     nethermind only: chainspec name to load from
#                        NETHERMIND_DIST/chainspec/NAME.json via --config
#                        (default: xdc-devnet5151; env override: NM_CONFIG)
#   --staticpeers LIST   comma-separated enode URLs to connect directly (erigon
#                        needs these to peer with an XDPoS net; its discv4 doesn't
#                        discover peers through an XDPoSChain bootnode)
#   --chainid N          chain / network id (default: chainId from the genesis).
#                        (--networkid is accepted as a deprecated alias)
#   --datadir DIR        where to store this peer's data (default
#                        nodes/<client>-<nodetype>, e.g. nodes/geth-sync)
#   --name NAME          machine/deployment label -- the FIRST part of this
#                        node's identity, not the whole name. The full identity
#                        is always
#                          <machine>-<client>-v<version>-<commit>-<nodetype>-<ip>
#                        so client, build and location are never lost. Omit it
#                        and <machine> falls back to MACHINE from
#                        network.env/env, else this host's short hostname.
#                        Sealers additionally get their coinbase appended.
#   --port N             p2p port (per-client default: oldxdc 31413 / geth 31513
#                        / erigon 31613 / … / xone 32013 -- 100 apart, clear of
#                        run.sh and validator ports, no cross-client clash)
#   --rpcport N          JSON-RPC port (oldxdc 9645 / geth 9745 / … / xone 10245)
#   --wsport N           WebSocket port (JSON-RPC port + 1)
#   --authrpc-port N     authenticated-RPC port (erigon/reth/xone/geth; also
#                        env AUTHRPC_PORT). Per-client default as above.
#                        geth: unlike the others this is a small default
#                        CHANGE, not just an override knob -- geth always
#                        starts an (unused-by-XDPoS) Engine API listener
#                        with no flag to move it off its own hardcoded
#                        127.0.0.1:8551, a collision landmine on any host
#                        already running another geth-family process; it is
#                        now pinned to this per-client-offset default
#                        (10745) unconditionally, same as every other client.
#   --torrent-port N     erigon BitTorrent port (also env TORRENT_PORT).
#   --privapi ADDR:PORT  erigon private API address (also env PRIVAPI).
#   --mcp-port N         erigon MCP port (also env MCP_PORT).
#                        The four flags above (and their env vars) let two
#                        instances of the SAME client run side by side --
#                        otherwise they'd collide on the per-client default.
#   --maxpeers N         cap the max peer count (also env MAXPEERS). Passed
#                        through per client: oldxdc/geth/erigon --maxpeers,
#                        besu --max-peers, reth --max-peers, nethermind
#                        --Network.MaxActivePeers, xone -maxpeers. Omitted
#                        (client's own default applies) unless given --
#                        geth's own default of 25 is unchanged either way.
#   --gas-limit N        the mint/plateau target this producer is launched
#                        with (also env GAS_LIMIT). UNLIKE --maxpeers above,
#                        this is NEVER omitted -- a missing gas-ceiling flag
#                        is exactly ItWorksinMyLocal#94's netv12 wedge: no
#                        flag was passed, so the legacy oldxdc arbiter fell
#                        back to its own compiled-in 50,000,000 default
#                        (cmd/utils/flags.go MinerGasLimitFlag) while
#                        geth-xdc/erigon-xdc elsewhere targeted a hard-coded
#                        420,000,000 "XDC plateau" -- two different targets
#                        on one chain, which the legacy arbiter's
#                        misc.VerifyGaslimit hard-errors on (instead of
#                        clamping) once they drift more than one legal
#                        parent/1024 step apart, permanently locking every
#                        validator out of proposing.
#                          THE PLATEAU IS PER-NETWORK, NOT ONE GLOBAL NUMBER
#                        (ItWorksinMyLocal#96/#98): an earlier version of
#                        this flag defaulted EVERY network to 420,000,000,
#                        which is itself a #94-shaped hazard against a chain
#                        whose live plateau is lower -- driving net5151's
#                        producers from its measured 42,000,000 plateau
#                        toward 420,000,000 reproduces the exact same wedge
#                        the flag exists to prevent. Measured live (2026):
#                        net5151 on 127.0.0.1:9551 -- block 0 (genesis) =
#                        4,700,000, block 1 = 42,000,000, still 42,000,000
#                        at block 213,000+; net5050/net5152 share that same
#                        42,000,000 plateau. mainnet/testnet(apothem) and
#                        netv12 (chainId 34093, confirmed live on
#                        127.0.0.1:37422) sit at 420,000,000. The default is
#                        therefore resolved from --network below (see the
#                        --network case ~line 245): devnet5151/5050/5152 ->
#                        42,000,000; mainnet/testnet/apothem -> 420,000,000.
#                        DO NOT re-globalise this to a single number again --
#                        a network with no mapped plateau hard-errors on
#                        --mine (and on any join at all -- see the guard
#                        right after that --network case) rather than
#                        silently picking one of the two numbers above; use
#                        an explicit --gas-limit to override. This is NOT
#                        the genesis header's own gasLimit field, and must
#                        never be set to force the two to match -- they are
#                        legitimately different (see netlab/TOPOLOGY.md's
#                        gasLimit field, ItWorksinMyLocal#96). Passed as
#                        --targetgaslimit for oldxdc, --miner.gaslimit for
#                        geth/erigon (both geth-derived: erigon inherited
#                        the same miner flag set, even though it currently
#                        only mints via the genesis-derived path on its own
#                        fix/94-xdc-gas-limit-genesis-plateau branch), and
#                        -miner.gaslimit for xone (also go-ethereum-derived,
#                        single-dash flags). besu/reth/nethermind are
#                        deliberately NOT given a flag here: join.sh never
#                        offers a --mine path for any of them (always
#                        sync-only in this harness -- see their sections
#                        below), so there is no local mint target for them
#                        to silently default on, and none of the three has
#                        a confirmed-safe equivalent CLI flag to guess at.
#   --etherbase ADDR     oldxdc --mine only: block-reward address (also env
#                        ETHERBASE). Omitted (client default) unless given.
#   --mine KEY           also mine, unlocking this address (oldxdc only; needs a
#                        validator key in .env). Ignored by geth/erigon.
#   --ethstats SPEC      ethstats target (ON by default, same as run.sh). SPEC
#                        is SECRET@HOST:PORT (node name prepended) or a full
#                        NAME:SECRET@HOST:PORT used verbatim.
#   --no-ethstats        disable ethstats reporting
#
# This joins as a read/sync node by default (no mining). It does NOT touch your
# local 4-node network in ./nodes.

_interupt() { echo "Shutdown $child_proc"; kill -TERM $child_proc 2>/dev/null; exit; }
trap _interupt INT TERM

cd "$(dirname "$0")" || exit 1
# Absolute path to THIS script, resolved after the cd above, so the --client all
# fan-out can re-invoke it reliably however it was started -- `./join.sh`,
# `bash join.sh` (where $0 is a bare "join.sh" not on PATH), or an absolute path.
SELF="$PWD/$(basename "$0")"

# DATADIR empty here means "use the default convention" (nodes/<client>-<type>),
# computed once the client is known; an explicit --datadir always wins.
GENESIS=""; BOOTNODES=""; NETWORKID=""; DATADIR=""; NAME=""; HOST_IP=""; CHAIN=""
MINEADDR=""; STATICPEERS=""; NETWORK=""; SYNCMODE=""
# STATICPEERS_EXPLICIT: set when the caller passes --staticpeers directly, so
# the local-sealer harvest below (LOCAL_BOOTNODE=1) knows to never clobber an
# explicit choice -- it only overrides STATICPEERS it (or peers.list) derived.
# It does NOT skip the harvest: the explicit peers are merged into $BOOTNODES so
# the six clients that never read $STATICPEERS still get them. See the harvest.
STATICPEERS_EXPLICIT=0
# LOCAL_BOOTNODE: set to 1 when BOOTNODES is derived from ./bootnode.key below
# (i.e. we're joining THIS machine's own local run.sh network), so the
# local-sealer peer harvest after it knows to fire.
LOCAL_BOOTNODE=0
# Ports are left empty here and defaulted PER CLIENT after --client is known, so
# oldxdc/geth/erigon joins each get a distinct set (clear of run.sh) and can run
# side by side without conflict. Any explicit flag below wins.
PORT=""; RPCPORT=""; WSPORT=""
# AUTHRPC/TORRENT/PRIVAPI/MCP default PER CLIENT below too, but (unlike PORT/
# RPCPORT/WSPORT above) they also honor a pre-set env var of the same name --
# needed so two instances of the SAME client (e.g. netlab/ running N oldxdc
# nodes) can each get a distinct authrpc/torrent/privapi/mcp port without a
# --flag per instance. A bare `AUTHRPC_PORT=""` here would silently discard
# that env value, so use the "${VAR:-}" no-op-if-set form instead.
AUTHRPC_PORT="${AUTHRPC_PORT:-}"; TORRENT_PORT="${TORRENT_PORT:-}"
PRIVAPI="${PRIVAPI:-}"; MCP_PORT="${MCP_PORT:-}"
# MAXPEERS/ETHERBASE: unset by default (client's own default applies / no
# --etherbase passed) unless a flag or env var provides one.
MAXPEERS="${MAXPEERS:-}"; ETHERBASE="${ETHERBASE:-}"
# GAS_LIMIT: UNLIKE MAXPEERS/ETHERBASE above, this is NEVER left empty by the
# time the per-client launch sections run -- see the --gas-limit flag
# comment above (ItWorksinMyLocal#94/#96/#98). Left unresolved here (just
# carrying a pre-set env var through, same "${VAR:-}" pattern as
# AUTHRPC_PORT etc, so netlab/node.sh's per-node --gas-limit flag still
# wins) because the correct default depends on --network, which isn't
# parsed yet -- resolved per-network right after the --network case below
# (~line 245), NOT to a single global number here (that single-default was
# the #98 bug: see the header comment above for why a global default is
# itself a #94-shaped hazard).
GAS_LIMIT="${GAS_LIMIT:-}"
# ethstats reporting is ON by default (same endpoint as run.sh); --no-ethstats
# disables it, --ethstats SPEC overrides the target.
ETHSTATS="xdc_openscan_stats_2026@stats.xdcindia.com:443"
# nethermind's chainspec name (NETHERMIND_DIST/chainspec/$NM_CONFIG.json via
# --config); env-overridable, defaults to devnet 5151. --nm-config wins over both.
# NM_CONFIG_EXPLICIT tracks whether NM_CONFIG was actually chosen (env or
# --nm-config) vs left at this default -- see the auto-convert block below
# (ItWorksinMyLocal#114 / xdc-nethermind-private#250): only auto-convert a
# genesis->chainspec when the caller did NOT ask for a specific chainspec by
# name, so an explicit --nm-config always wins untouched.
NM_CONFIG_EXPLICIT=""
[ -n "${NM_CONFIG:-}" ] && NM_CONFIG_EXPLICIT=1
NM_CONFIG="${NM_CONFIG:-xdc-devnet5151}"
while [ $# -gt 0 ]; do
  case "$1" in
    --network)     NETWORK=$2; shift 2;;
    --syncmode)    SYNCMODE=$2; shift 2;;
    --genesis)     GENESIS=$2; shift 2;;
    --chain)       CHAIN=$2; shift 2;;
    --nm-config)   NM_CONFIG=$2; NM_CONFIG_EXPLICIT=1; shift 2;;
    --bootnodes)   BOOTNODES=$2; shift 2;;
    --ip)          HOST_IP=$2; shift 2;;
    --host)        HOST_IP=$2; shift 2;;
    --client)      export CLIENT=$2; shift 2;;
    --chainid)     NETWORKID=$2; shift 2;;
    # Deprecated spelling of --chainid, kept so existing command lines and
    # scripts don't break. Every script here now takes --chainid.
    --networkid)   NETWORKID=$2; shift 2
                   echo "join.sh: --networkid is deprecated; use --chainid" >&2;;
    --datadir)     DATADIR=$2; shift 2;;
    --name)        NAME=$2; shift 2;;
    --port)        PORT=$2; shift 2;;
    --rpcport)     RPCPORT=$2; shift 2;;
    --wsport)      WSPORT=$2; shift 2;;
    --authrpc-port) AUTHRPC_PORT=$2; shift 2;;
    --torrent-port) TORRENT_PORT=$2; shift 2;;
    --privapi)     PRIVAPI=$2; shift 2;;
    --mcp-port)    MCP_PORT=$2; shift 2;;
    --maxpeers)    MAXPEERS=$2; shift 2;;
    --gas-limit)   GAS_LIMIT=$2; shift 2;;
    --etherbase)   ETHERBASE=$2; shift 2;;
    --mine)        MINEADDR=$2; shift 2;;
    --staticpeers) STATICPEERS=$2; STATICPEERS_EXPLICIT=1; shift 2;;
    --ethstats)    ETHSTATS=$2; shift 2;;
    --no-ethstats) ETHSTATS=""; shift;;
    -h|--help)     sed -n '2,135p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown flag: $1" >&2; exit 1;;
  esac
done

# Sync mode. Default: snap for the PUBLIC networks (mainnet/testnet) so a node
# bounds its disk to recent state (~10x smaller than full/archive) and syncs in
# hours; full for devnet5151 (its legacy flat-diff base is validated in full, and
# this preserves the pre-existing devnet behaviour exactly). Override anytime with
# --syncmode snap|fast|full. Applied per-client at launch below: geth/xone
# --syncmode, besu --sync-mode (upper-cased), nethermind --Sync.SnapSync/FastSync;
# erigon and reth already snap/stage-sync by default so they take no extra flag.
if [ -z "$SYNCMODE" ]; then
  case "${NETWORK:-}" in
    mainnet|xinfin|xdc|testnet|apothem) SYNCMODE=snap ;;
    *)                                  SYNCMODE=full ;;
  esac
fi

source ./lib.sh

# Default network target so neither --ip nor --staticpeers is required.
# network.env -> DEFAULT_HOST/DEFAULT_BOOTNODE_PORT/DEFAULT_BOOTNODES/DEFAULT_GENESIS;
# peers.list -> default peers.
[ -f network.env ] && . ./network.env

# --network mainnet|testnet: sync a PUBLIC XDC network using the genesis +
# bootnodes vendored from XinFinOrg/XinFin-Node (genesis/genesis-<net>.json,
# bootnodes/<net>.list). Unset (or devnet5151) keeps the network.env default
# (devnet 5151). Explicit --genesis/--bootnodes/--chainid still win; public
# bootnodes are used by default for mainnet/testnet.
PUB_BOOTNODES_FILE=""
# GAS_LIMIT_DEFAULT: the per-network mint/plateau default (see the
# --gas-limit flag doc up top, ItWorksinMyLocal#94/#96/#98) -- set in the
# SAME case branch as GENESIS/NETWORKID/NM_CONFIG above so the network list
# and the gas-plateau list can never drift apart the way two independent
# lists would (exactly the failure mode #99's onboard-masternode.sh hit
# with its own duplicated epoch-boundary arithmetic). Left unset in the
# unreachable `*` branch below -- that branch already exits before it
# matters; the guard right after this esac exists for defense in depth,
# should a future branch ever get added here without a plateau.
GAS_LIMIT_DEFAULT=""
case "${NETWORK:-}" in
  mainnet|xinfin|xdc)
    [ -z "$GENESIS" ]   && GENESIS="genesis/genesis-mainnet.json"
    [ -z "$NETWORKID" ] && NETWORKID=50
    # nethermind's dist ships chainspec/config named "xdc" for mainnet (chainId 50)
    # -- there is NO xdc-mainnet.json, so mapping to xdc-mainnet made
    # `--client nethermind --network mainnet` abort ("missing chainspec/xdc-mainnet.json")
    # and crash-loop. Map to the actual bundled name.
    NM_CONFIG="${NM_CONFIG/xdc-devnet5151/xdc}"
    NM_CONFIG_EXPLICIT=1   # a curated bundled chainspec, never auto-convert over it
    PUB_BOOTNODES_FILE="bootnodes/mainnet.list"
    GAS_LIMIT_DEFAULT=420000000 ;;
  testnet|apothem)
    [ -z "$GENESIS" ]   && GENESIS="genesis/genesis-testnet.json"
    [ -z "$NETWORKID" ] && NETWORKID=51
    NM_CONFIG="${NM_CONFIG/xdc-devnet5151/xdc-testnet}"
    NM_CONFIG_EXPLICIT=1   # a curated bundled chainspec, never auto-convert over it
    PUB_BOOTNODES_FILE="bootnodes/testnet.list"
    GAS_LIMIT_DEFAULT=420000000 ;;
  devnet5551|5551)
    # XDC public devnet, chainId 5551. Genesis and bootnodes are vendored
    # verbatim from XinFinOrg/XinFin-Node@devnet (devnet/genesis.json and
    # devnet/bootnodes.list) so this joins the REAL network rather than a
    # locally generated look-alike.
    #
    # nethermind: no bundled chainspec ships for 5551, so NM_CONFIG_EXPLICIT is
    # deliberately NOT set -- join.sh auto-converts one from the genesis, the
    # same path it uses for any custom chain.
    #
    # GAS_LIMIT_DEFAULT matches the 420,000,000 plateau the other two PUBLIC
    # networks use. UNVERIFIED for 5551: not measured from a synced header,
    # since that requires joining first. It only affects a --mine producer (a
    # sync-only follower never uses it), but per #94 a producer given the wrong
    # plateau can lock the validator set out permanently -- so confirm it
    # against a synced 5551 header before ever sealing here.
    [ -z "$GENESIS" ]   && GENESIS="genesis/genesis-devnet5551.json"
    [ -z "$NETWORKID" ] && NETWORKID=5551
    NM_CONFIG="${NM_CONFIG/xdc-devnet5151/xdc-devnet5551}"
    PUB_BOOTNODES_FILE="bootnodes/devnet5551.list"
    GAS_LIMIT_DEFAULT=420000000 ;;
  ""|devnet5151|5151|devnet|5050|net5050|5152|net5152)
    # net5151/net5050/net5152 all measured live at a 42,000,000 plateau
    # (see header comment); net5050/net5152 aren't otherwise wired up as
    # connectable --network targets here (no vendored genesis/bootnodes --
    # pass --genesis/--bootnodes explicitly), but they still need the
    # correct gas-limit default when a caller does target them that way.
    GAS_LIMIT_DEFAULT=42000000 ;;
  *) echo "unknown --network '$NETWORK' (expected: mainnet | testnet | devnet5551 | devnet5151 | 5050 | 5152)" >&2; exit 1 ;;
esac
# Resolve GAS_LIMIT now that --network is known and validated: an explicit
# --gas-limit/GAS_LIMIT always wins (GAS_LIMIT is already non-empty in that
# case, so this is a no-op); otherwise use this network's plateau.
GAS_LIMIT="${GAS_LIMIT:-$GAS_LIMIT_DEFAULT}"
# Every case branch above that doesn't exit sets GAS_LIMIT_DEFAULT, so this
# should be unreachable -- but GAS_LIMIT is NEVER omitted from a client
# launch (--mine or not, see the flag doc up top), and a silent fallback
# here is exactly ItWorksinMyLocal#94's netv12 wedge (mismatched/missing
# targets across producers). Hard-error instead of guessing.
if [ -z "$GAS_LIMIT" ]; then
  echo "error: no gas-limit plateau mapped for --network '${NETWORK:-devnet5151}' (see the GAS_LIMIT_DEFAULT cases above, ItWorksinMyLocal#94/#96/#98); refusing to silently default -- pass an explicit --gas-limit N (or GAS_LIMIT env var) instead. This matters most before --mine: a mismatched or missing plateau on a sealing/proposing join is exactly how #94's netv12 wedge happened." >&2
  exit 1
fi
if [ -n "$PUB_BOOTNODES_FILE" ] && [ -z "$BOOTNODES" ] && [ -f "$PUB_BOOTNODES_FILE" ]; then
  BOOTNODES=$(grep -v '^[[:space:]]*#' "$PUB_BOOTNODES_FILE" | grep enode | paste -sd, -)
  echo "No --bootnodes given; using $(echo "$BOOTNODES" | tr ',' '\n' | grep -c enode) public $NETWORK bootnode(s)"
fi
# The vendored public genesis uses XDC's "xdc"-prefixed address display format in
# its coinbase / foudationWalletAddr / alloc keys. geth-xdc (chainId in code) and
# erigon (built-in --chain) don't read the file, but the FILE-reading clients --
# besu (--genesis-file), reth (--chain <file>), xone (-override.genesis) -- run a
# strict address decoder that rejects the "xdc" prefix ("Invalid coinbase ...",
# "invalid address", "odd number of digits"). "xdc" and "0x" are the same 20-byte
# address, so normalise a cached copy to 0x for those clients. Verified: the
# normalised genesis computes the IDENTICAL genesis hash (mainnet 0x4a9d74...),
# i.e. same chain, and unblocks besu/reth/xone on the public networks.
case "${NETWORK:-}" in
  mainnet|xinfin|xdc|testnet|apothem)
    if [ -n "$GENESIS" ] && [ -f "$GENESIS" ] && grep -q '"xdc[0-9a-fA-F]\{40\}"' "$GENESIS" 2>/dev/null; then
      _g0x="${GENESIS%.json}-0x.json"
      sed -E 's/"xdc([0-9a-fA-F]{40})"/"0x\1"/g' "$GENESIS" > "$_g0x"
      if python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$_g0x" 2>/dev/null; then
        echo "  normalised xdc-prefixed genesis addresses -> 0x for file-reading clients ($_g0x)"
        GENESIS="$_g0x"
      else
        rm -f "$_g0x"
        echo "  warning: genesis xdc->0x normalisation produced invalid JSON; using original $GENESIS" >&2
      fi
    fi ;;
esac
[ -n "$NETWORK" ] && [ "$NETWORK" != devnet5151 ] && echo "Target network: $NETWORK (chainId ${NETWORKID:-?}, genesis ${GENESIS:-?})"

# Default the genesis from network.env (DEFAULT_GENESIS) when --genesis is omitted,
# so a plain `./join.sh` joins the configured network (devnet 5151) with no flags.
if [ -z "$GENESIS" ] && [ -n "$DEFAULT_GENESIS" ] && [ -f "$DEFAULT_GENESIS" ]; then
  GENESIS="$DEFAULT_GENESIS"
  echo "No --genesis given; using default genesis $GENESIS (from network.env)"
  # If a LOCAL network exists in this checkout, silently joining the configured
  # PUBLIC one is almost certainly not what was wanted. This is how a newcomer who
  # ran ./setup.sh and then `./join.sh --client all` ended up syncing devnet 5151
  # instead of their own chain -- it succeeds, so nothing looks wrong. Warn loudly
  # rather than change the default, which other callers rely on.
  if [ -f ./genesis/genesis.json ] && [ "$DEFAULT_GENESIS" != "./genesis/genesis.json" ]; then
    echo "" >&2
    echo "  WARNING: this checkout also has a LOCAL genesis at ./genesis/genesis.json," >&2
    echo "  but you are about to join the network in network.env instead." >&2
    echo "  If you meant to join the net you just created with ./setup.sh, pass it:" >&2
    echo "      --genesis ./genesis/genesis.json --ip <IP>:<BOOTNODE_PORT> --chainid <N>" >&2
    echo "" >&2
  fi
fi

# Prefer explicit DEFAULT_BOOTNODES (real masternode enodes) over deriving one from
# bootnode.key: devnet 5151's masternodes live on four separate hosts with their own
# node keys, so a key-derived enode would carry the wrong node id and fail to dial.
if [ -z "$BOOTNODES" ] && [ -z "$HOST_IP" ] && [ -n "$DEFAULT_BOOTNODES" ]; then
  BOOTNODES="$DEFAULT_BOOTNODES"
  echo "No --ip/--bootnodes given; using $(echo "$BOOTNODES" | tr ',' '\n' | grep -c enode) default bootnode(s) from network.env"
fi

if [ -z "$HOST_IP" ] && [ -z "$BOOTNODES" ] && [ -n "$DEFAULT_HOST" ]; then
  HOST_IP="${DEFAULT_HOST}:${DEFAULT_BOOTNODE_PORT:-30301}"
  echo "No --ip/--bootnodes given; using default host $HOST_IP (from network.env)"
fi
if [ -z "$STATICPEERS" ] && [ -f peers.list ]; then
  STATICPEERS=$(grep -v '^[[:space:]]*#' peers.list | grep enode | paste -sd, -)
  [ -n "$STATICPEERS" ] && echo "Using $(echo "$STATICPEERS" | tr ',' '\n' | grep -c enode) default peer(s) from peers.list"
fi

# If no explicit --bootnodes, derive the enode from ./bootnode.key at the given
# IP (--ip/--host, may include :PORT; defaults to this machine's IP:30301).
# erigon joining a built-in --chain (no --ip) uses that chain's own bootnodes,
# so skip derivation there.
# besu/nethermind/reth take the validator enodes directly (network.env
# DEFAULT_BOOTNODES / --bootnodes); a key-derived enode would carry the wrong
# node id, so never derive one for them. Resolved HERE (before the --client all
# fan-out below) so it runs ONCE and is forwarded to every child via `shared`
# -- besu/nethermind/reth can't self-derive, so under --client all they'd never
# get a bootnode otherwise (this used to sit after the fan-out and never ran
# for CLIENT=all, leaving those three childrens' BOOTNODES empty).
if [ -z "$BOOTNODES" ] \
   && [ "$CLIENT" != besu ] && [ "$CLIENT" != nethermind ] && [ "$CLIENT" != reth ] \
   && { [ "$CLIENT" != erigon ] || [ -n "$HOST_IP" ]; }; then
  [ -f ./bootnode.key ] || { echo "need --bootnodes or --ip, or a ./bootnode.key to derive one" >&2; exit 1; }
  # The enode pubkey comes from bootnode.key via any bootnode binary. The geth
  # client has none, so fall back to whichever bootnode binary is built.
  bn="$BOOTNODE_BIN"
  [ -x "$bn" ] || bn=$(ls bin/*/*/bootnode 2>/dev/null | head -1)
  [ -x "$bn" ] || { echo "no bootnode binary to derive the enode; pass --bootnodes, or build the oldxdc client once" >&2; exit 1; }
  target="${HOST_IP:-$(detect_ip)}"
  case "$target" in
    *:*) bhost="${target%:*}"; bport="${target##*:}";;
    *)   bhost="$target";      bport=30301;;
  esac
  # Derive AND guarantee it is actually running. This block used to only compute
  # the enode from the static key; if run.sh was not up on this host, every client
  # was then pointed at an address nothing was bound to and discovery silently
  # never completed. ensure_bootnode() is idempotent (it reuses an already-bound
  # one) and non-fatal, so a host without the oldxdc bootnode binary still joins
  # on whatever static/trusted peers the harvest below finds.
  BOOTNODES=$(ensure_bootnode "$bhost" "$bport") \
    || BOOTNODES="enode://$("$bn" -nodekey ./bootnode.key -writeaddress)@${bhost}:${bport}"
  echo "Using key-based bootnode: $BOOTNODES"
  LOCAL_BOOTNODE=1
fi

# LOCAL_BOOTNODE=1 means we're joining THIS machine's own local run.sh network
# (the bootnode-derivation above only fires for a key-based, same-host bootnode).
# That bootnode serves DISCOVERY only, not blocks -- and reth/besu can't even
# discover through it (trusted-only / static-nodes designs) -- so without a
# real block-serving peer every client sits at peers=0/1 and never advances.
# Harvest the LIVE local sealers' (run.sh nodes/1..4, RPC 8545-8548) own enodes
# via admin_nodeInfo -- these ARE real, block-serving peers on the SAME chain.
# Runs here (before the --client all fan-out) so it executes ONCE in the
# parent and is forwarded to every child via `shared`. This overrides any
# peers.list-derived STATICPEERS set above: peers.list holds the PUBLIC devnet
# 5151 masternode enodes, the wrong chain for a local join. An explicit
# --staticpeers (STATICPEERS_EXPLICIT=1) always wins over this.
#
# Set on BOTH STATICPEERS and BOOTNODES: only erigon's launch section reads
# STATICPEERS (its discv4 can't discover XDPoS peers through a bootnode at
# all, hence a dedicated --staticpeers flag); oldxdc/geth/xone (--bootnodes),
# besu (--bootnodes + a static-nodes-file built FROM --bootnodes), reth
# (--trusted-peers) and nethermind (--Network.StaticPeers) all wire their peer
# connections off $BOOTNODES instead. Verified live: with BOOTNODES holding
# only the bootnode-only enode, besu logs "Looking for peers ... static=0" and
# oldxdc/geth cross-connect with each other (both stuck at genesis) rather
# than finding the real sealers -- appending the harvested sealers onto
# BOOTNODES (kept alongside the discovery bootnode) gives every client an
# immediate, real, block-serving peer to dial instead of waiting on (or never
# completing) Kademlia propagation through the bootnode alone.
#
# An EXPLICIT --bootnodes that points at THIS machine (127.0.0.1/localhost or
# our own detected IP) is just as much a local join as a key-derived one, but
# previously only the key-derived path set LOCAL_BOOTNODE=1 -- so
# `join.sh --bootnodes <local-enode>` skipped this harvest entirely and left
# modern clients pinned to the PUBLIC peers.list (wrong chain) -> 0 peers on
# the local network. Promote such a bootnode to a local join so the harvest
# runs for it too.
if [ "$LOCAL_BOOTNODE" != 1 ] && [ -n "$BOOTNODES" ]; then
  _myip=$(detect_ip 2>/dev/null)
  case ",$BOOTNODES," in
    *@127.0.0.1:*|*@localhost:*) LOCAL_BOOTNODE=1 ;;
    *) [ -n "$_myip" ] && case "$BOOTNODES" in *"@${_myip}:"*) LOCAL_BOOTNODE=1 ;; esac ;;
  esac
fi
# Runs on EVERY local join, including one that passed --staticpeers explicitly.
#
# It used to be gated on `[ "$STATICPEERS_EXPLICIT" != 1 ]`, which looked like it
# was only protecting an explicit choice from being clobbered. In practice it
# threw the peers away for six of the seven clients: ONLY erigon's launch section
# reads $STATICPEERS. oldxdc/geth/xone use --bootnodes, besu builds its
# static-nodes file FROM --bootnodes, nethermind uses --Network.StaticPeers, and
# reth passes `--trusted-peers "$BOOTNODES"` (see its launch below) -- all of them
# off $BOOTNODES. So `join.sh --client reth --staticpeers <4 sealer enodes>`
# skipped this block, left BOOTNODES holding only the discovery bootnode, and reth
# sat at connected_peers=0 latest_block=0 forever while the sealers climbed.
# Measured, then fixed by folding the same enodes into --bootnodes instead.
#
# The invariant now: a local join ALWAYS ends up with real, dialable, block-serving
# sealer enodes in $BOOTNODES, so every client peers immediately instead of waiting
# on Kademlia propagation through a discovery-only bootnode (which erigon's discv4
# cannot do through an XDPoSChain bootnode at all). An explicit --staticpeers is
# still never overwritten -- it is now ADDITIVE: it seeds $BOOTNODES too, and is
# kept as-is for erigon.
if [ "$LOCAL_BOOTNODE" = 1 ]; then
  _local_nid="$NETWORKID"
  if [ -z "$_local_nid" ] && [ -n "$GENESIS" ] && [ -f "$GENESIS" ]; then
    _local_nid=$(python3 -c "import json,sys;print(json.load(open('$GENESIS'))['config']['chainId'])" 2>/dev/null)
  fi
  _local_peers=""
  # Probe the local sealers' JSON-RPC. run.sh/setup.sh honor BASE_RPC_PORT
  # (default 8545) and assign nodes BASE_RPC_PORT, +1, +2, +3 -- so hardcoding
  # 8545-8548 missed the sealers whenever the network was launched on a custom
  # BASE_RPC_PORT (e.g. when 8545 was already taken), leaving _local_peers empty
  # and modern clients unable to find a real peer. Derive the scan from the same
  # env instead.
  # run.sh assigns sealer i the RPC port BASE_RPC_PORT+i, i=1..4 -- so probe +1..+4,
  # NOT +0..+3. The old form was correct only by accident when BASE_RPC_PORT was
  # left unset: the inline fallback 8545 happens to equal sealer 1's port (8544+1).
  # Export BASE_RPC_PORT -- exactly as run.sh --help's own example suggests -- and
  # the probe shifted down one, wasting a scan on BASE_RPC_PORT itself (nothing
  # listens there) and silently MISSING sealer 4. It then reported "Using 3 local
  # sealer peer(s)" and looked fine.
  _brp="${BASE_RPC_PORT:-8544}"
  for _lp in $((_brp + 1)) $((_brp + 2)) $((_brp + 3)) $((_brp + 4)); do
    _lenode=$(curl -s -m3 "http://127.0.0.1:${_lp}" -X POST -H 'Content-Type: application/json' \
      --data '{"jsonrpc":"2.0","method":"admin_nodeInfo","params":[],"id":1}' 2>/dev/null \
      | python3 -c "
import sys, json
try:
    print((json.load(sys.stdin).get('result') or {}).get('enode', ''))
except Exception:
    pass" 2>/dev/null)
    [ -n "$_lenode" ] || continue
    # Strip any ?discport=... query and normalise the host so it's a clean,
    # dialable enode://<id>@<ip>:<tcpport>. Prefer $bhost (the --ip / resolved
    # bootnode host) but fall back to the sealer's OWN self-reported host when
    # that is already a real address -- $bhost is only set inside the
    # bootnode-key-derivation block, so an explicit local --bootnodes join
    # leaves it empty, which previously produced malformed `...@:<port>` enodes
    # that geth rejects as a "bad bootstrap node". Only override a placeholder
    # host ([::], 0.0.0.0, 127.0.0.1, localhost, empty).
    _lenode=$(printf '%s' "$_lenode" | BHOST="$bhost" python3 -c "
import sys, os
e = sys.stdin.read().strip().split('?', 1)[0]
try:
    head, hostport = e.split('@', 1)
    host, port = hostport.rsplit(':', 1)
    bhost = os.environ.get('BHOST', '').strip()
    placeholder = host.strip('[]') in ('', '::', '0.0.0.0', '127.0.0.1', 'localhost')
    if placeholder and bhost:
        host = bhost
    print('%s@%s:%s' % (head, host, port))
except Exception:
    print(e)
")
    _local_peers="${_local_peers:+$_local_peers,}$_lenode"
  done
  if [ -z "$_local_peers" ]; then
    echo "  no local sealer RPC reachable on 8545-8548; falling back to bootnode-discovery harvest" >&2
    _local_peers=$(harvest_peers "$GENESIS" "$BOOTNODES" "${_local_nid:-0}")
  fi
  # An explicit --staticpeers is authoritative for STATICPEERS but must ALSO reach
  # the $BOOTNODES-reading clients, so merge it in rather than letting it bypass
  # this block entirely (which is what used to strand reth at 0 peers).
  #
  # Dedup the MERGED list, not just $BOOTNODES. Under `--client all` the parent
  # harvests the sealers and forwards them to each child via --staticpeers; the
  # child then harvests the very same sealers itself, so without this the two
  # copies concatenate and the line below reports "8 local sealer peer(s)" on a
  # 4-sealer net. The wiring was still correct (BOOTNODES was deduped anyway),
  # but a peer count that double-counts is exactly the kind of number someone
  # later trusts while debugging a peering problem.
  if [ "$STATICPEERS_EXPLICIT" = 1 ]; then
    _local_peers="${STATICPEERS:+$STATICPEERS}${STATICPEERS:+${_local_peers:+,}}$_local_peers"
  fi
  _local_peers=$(dedupe_csv "$_local_peers")
  if [ -n "$_local_peers" ]; then
    [ "$STATICPEERS_EXPLICIT" = 1 ] || STATICPEERS="$_local_peers"
    # An enode repeated in the bootnode list makes geth log "bad bootstrap node".
    BOOTNODES=$(dedupe_csv "${BOOTNODES:+$BOOTNODES,}$_local_peers")
    echo "Using $(echo "$_local_peers" | tr ',' '\n' | grep -c enode) local sealer peer(s) as staticpeers/bootnodes (overrides peers.list)"
  else
    echo "  warning: could not find any local sealer peers; clients may not sync (pass --staticpeers)" >&2
  fi
fi

# --client all: boot EVERY client at once, each in its own datadir with its
# per-client port offset (already distinct below), all sharing the same genesis +
# bootnodes resolved above. Implemented as a fan-out: re-invoke this same script
# once per client with --client <c>, so each child reuses the full per-client
# setup (binary resolution, genesis init, ethstats, peer harvest) with zero
# duplicated logic. Children run concurrently; Ctrl-C / TERM stops them all.
# A client whose extra inputs are missing (erigon needs --chain; nethermind needs
# a dist) exits with its own hint and is skipped -- the rest keep syncing.
if [ "$CLIENT" = all ]; then
  ALL_CLIENTS="$JOIN_ALL_CLIENTS"
  # Forward the shared, already-resolved network inputs to every child so they all
  # dial the SAME network (and a child doesn't re-run default resolution).
  shared=()
  [ -n "$GENESIS" ]     && shared+=(--genesis "$GENESIS")
  [ -n "$BOOTNODES" ]   && shared+=(--bootnodes "$BOOTNODES")
  [ -n "$HOST_IP" ]     && shared+=(--ip "$HOST_IP")
  [ -n "$STATICPEERS" ] && shared+=(--staticpeers "$STATICPEERS")
  [ -n "$NETWORKID" ]   && shared+=(--chainid "$NETWORKID")
  [ -n "$CHAIN" ]       && shared+=(--chain "$CHAIN")
  [ -n "$MINEADDR" ]    && shared+=(--mine "$MINEADDR")   # only oldxdc seals; others print "ignored"
  [ -n "$MAXPEERS" ]    && shared+=(--maxpeers "$MAXPEERS")
  [ -n "$ETHERBASE" ]   && shared+=(--etherbase "$ETHERBASE")   # only the oldxdc --mine child uses it
  # --gas-limit is ALWAYS forwarded (unlike the optional flags above): GAS_LIMIT
  # is never empty (see its default above), and each `bash "$SELF" ...` child
  # below is a fresh process that would otherwise re-default it independently
  # instead of inheriting an explicit --gas-limit override from the parent
  # invocation -- the same reasoning --etherbase/--maxpeers already document.
  shared+=(--gas-limit "$GAS_LIMIT")
  if [ -n "$ETHSTATS" ]; then shared+=(--ethstats "$ETHSTATS"); else shared+=(--no-ethstats); fi
  # Base directory for the per-client chaindata: a custom --datadir wins, else the
  # shared ./nodes tree (clear of run.sh's numbered nodes/1..N local net).
  base="${DATADIR:-nodes}"
  all_pids=()
  # Named function (not an inline single-quoted trap body): shellcheck's
  # dataflow analysis doesn't associate a `for p in ...` loop assignment
  # inside a trap's quoted argument string, so it (falsely) flags `p` as
  # referenced-but-unassigned (SC2154) there. A real function has no such
  # blind spot -- same behavior, clean lint.
  _join_stop_all_clients() {
    echo "[all] stopping ${#all_pids[@]} client(s)..."
    local p
    for p in "${all_pids[@]}"; do kill -TERM "$p" 2>/dev/null; done
    exit 0
  }
  trap _join_stop_all_clients INT TERM
  for c in $ALL_CLIENTS; do
    # Each client's chaindata lands in nodes/<client>-<nodetype> so the six chains
    # never share a datadir. Only oldxdc can seal (--mine); the rest join sync-only.
    nt=sync; [ -n "$MINEADDR" ] && [ "$c" = oldxdc ] && nt=seal
    dd="${base}/${c}-${nt}"
    args=(--client "$c" --datadir "$dd" "${shared[@]}")
    # erigon-xdc v3.5.0+ loads a genesis FILE directly via --chain <path> (see the
    # erigon-only validation below), so only fall back to the net5151 built-in
    # chainspec when the caller passed neither --chain nor a usable --genesis --
    # otherwise `--client all` would silently override a real --genesis (e.g. a
    # LOCAL network's genesis) with the public devnet 5151 chainspec, putting
    # erigon on the wrong chain while every other client joins the intended one.
    # Harmless for the other clients (they ignore --chain).
    [ "$c" = erigon ] && [ -z "$CHAIN" ] && [ ! -f "$GENESIS" ] && args+=(--chain "${ERIGON_CHAIN:-xdc-net5151}")
    # Pass the operator's label through UNCHANGED. It is only the machine
    # component now, and each child appends its own client, version, commit,
    # nodetype and ip -- so appending "-$c" here would duplicate the client token
    # in every name (mylab-geth-geth-v1.17...).
    [ -n "$NAME" ] && args+=(--name "$NAME")
    # Each child gets its OWN log file, named to match its datadir
    # (nodes/<client>-<nodetype> -> nodes/<client>-<nodetype>.log). Without this
    # every client's output landed in the shared parent stream, so when one of
    # eight misbehaved there was nothing to read for it -- "besu has 0 peers"
    # could not be told apart from a fork-id mismatch, a bad chainspec, a failed
    # handshake or a crash loop (#186). run.sh --all already does exactly this for
    # its followers, which is why local-net failures were diagnosable this week
    # and public-net ones were not.
    #
    # stdout still gets the launch line above, so the aggregate view is unchanged.
    # The log lives beside the datadir, so its parent must exist BEFORE the
    # redirect is set up. join.sh normally creates the datadir inside each child,
    # which is too late: bash evaluates `> "$_all_log"` in the parent and the
    # child dies instantly with "No such file or directory". run.sh does not hit
    # this because nodes/ already exists by the time it forks its followers.
    _all_log="${dd}.log"
    mkdir -p "$(dirname "$_all_log")" 2>/dev/null || true
    echo "=== [all] launching $c  (datadir $dd, log $_all_log) ==="
    bash "$SELF" "${args[@]}" > "$_all_log" 2>&1 &
    all_pids+=($!)
  done
  echo "[all] launched ${#all_pids[@]} client(s): $ALL_CLIENTS"
  echo "[all] each syncs in ${base}/<client>-<nodetype>; Ctrl-C stops all"
  wait
  exit 0
fi

# Node role: "seal" when this client will mine (oldxdc + --mine), else "sync".
# Drives both the default datadir and the default identity/ethstats name so they
# read consistently (nodes/<client>-<nodetype> <-> <client>-v<ver>-<host>-<nodetype>).
NODETYPE=sync
[ -n "$MINEADDR" ] && case "$CLIENT" in oldxdc) NODETYPE=seal;; esac

# Default datadir: nodes/<client>-<nodetype>, so every client's chaindata lives
# under ./nodes (clear of run.sh's numbered nodes/1..N local net) and different
# clients never share a datadir. An explicit --datadir always wins.
[ -z "$DATADIR" ] && DATADIR="nodes/${CLIENT}-${NODETYPE}"

# Per-client default ports: a 100-wide block per client so all seven join clients
# run side by side with no cross-client clash, in a high band kept clear of
# run.sh's local net (30301, 30303-6, 8545-58), any XDPoS validator ports
# (85xx / 91xx / 99xx / 310xx), and the previous join defaults (83xx/85-86xx/
# 30313+) -- so a `--client all` fan-out won't collide with those or a second run.
O=$(join_client_port_offset "$CLIENT")
PORT="${PORT:-$((31413 + O))}"                  # p2p     31413..32013
RPCPORT="${RPCPORT:-$((9645 + O))}"             # http    9645..10245
WSPORT="${WSPORT:-$((RPCPORT + 1))}"            # ws      http+1
AUTHRPC_PORT="${AUTHRPC_PORT:-$((10645 + O))}"  # authrpc 10645..11245
TORRENT_PORT="${TORRENT_PORT:-$((42579 + O))}"  # erigon torrent
MCP_PORT="${MCP_PORT:-$((10653 + O))}"          # erigon mcp
PRIVAPI="${PRIVAPI:-127.0.0.1:$((11645 + O))}"  # erigon private api

# Validate required inputs BEFORE building (a build can take minutes).
case "$CLIENT" in
  erigon)
    # erigon-xdc v3.5.0+ CAN load a genesis FILE via --chain <path> (verified: it
    # writes the custom genesis and computes the correct hash, e.g. net5151
    # 0x96f121). So --chain is optional now: with no --chain we hand erigon the
    # --genesis file directly (issue #9 was an older build that only resolved
    # built-in names). A built-in --chain name (xdc | xdc-apothem) still wins; but
    # do NOT use the built-in xdc-net5151/xdc-devnet5151 names -- their compiled-in
    # genesis is stale (0x11b449 != live 0x96f121) and won't peer. Just need a
    # genesis file when no built-in name is given.
    if [ -z "$CHAIN" ] && [ ! -f "$GENESIS" ]; then
      echo "erigon join needs a genesis: pass --genesis <file> (or a built-in --chain xdc | xdc-apothem)." >&2
      exit 1
    fi ;;
  nethermind)
    : ;;  # nethermind loads its own chainspec from the dist, not --genesis
  *)
    # oldxdc/geth/besu/reth all consume the geth-format genesis natively.
    [ -f "$GENESIS" ] || { echo "need --genesis <file> (got '$GENESIS')" >&2; exit 1; } ;;
esac

# besu/nethermind/reth are separate toolchains (Java/.NET/Rust) that ensure_bins
# does not build; locate a prebuilt binary (env override, then PATH, then a
# binaries.json download, then a build hint). oldxdc/geth/erigon build from the
# XDPoS source via ensure_bins as before. BIN_SOURCE=build (or the per-client
# BIN_SOURCE_besu/BIN_SOURCE_reth/BIN_SOURCE_nethermind) skips the download and
# goes straight to the "not found" hint below.
case "$CLIENT" in
  besu)
    CLIENT_BIN="${BESU_BIN:-$(command -v besu 2>/dev/null)}"
    if [ ! -x "$CLIENT_BIN" ] && [ "${BIN_SOURCE_besu:-$BIN_SOURCE}" != build ]; then
      download_bins && CLIENT_BIN="$BIN_DIR/besu"
    fi
    [ -x "$CLIENT_BIN" ] || { echo "besu binary not found. Build besu-xdc (XDCIndia/besu-xdc) and set BESU_BIN=/path/to/besu, put 'besu' on PATH, or let this script download it (unset BIN_SOURCE/BIN_SOURCE_besu if you set either to 'build')." >&2; exit 1; }
    # besu's launcher is a Gradle start script: it needs a JVM via JAVA_HOME or
    # `java` on PATH, and exits with Gradle's own generic "JAVA_HOME is not set
    # and no 'java' command could be found" if it has neither. The DOWNLOADED
    # besu dist bundles a JRE, but a LOCAL build (gradle installDist/distTar --
    # i.e. exactly what BESU_BIN normally points at, and the only way to run a
    # besu carrying an unreleased fix) does NOT. On a host whose only JVM is the
    # one inside a previously-downloaded besu dist, `BESU_BIN=... ./join.sh
    # --client besu` therefore died before printing anything besu-specific,
    # which reads as a broken local build rather than a missing-JVM wiring gap
    # (ItWorksinMyLocal#133 -- this masked besu's real behaviour during #128/#132
    # testing, because the silent fallback to the downloaded dist ran a DIFFERENT
    # besu than the one intended).
    #
    # So: if no usable JVM is visible, borrow the JRE from a besu dist we already
    # have on disk and export JAVA_HOME for the child. Only fills a gap -- an
    # existing, working JAVA_HOME/`java` is always left alone.
    if ! { [ -n "$JAVA_HOME" ] && [ -x "$JAVA_HOME/bin/java" ]; } && ! command -v java >/dev/null 2>&1; then
      for _besu_jre in \
        "$(dirname "$CLIENT_BIN")/../jre" \
        "$BIN_DIR/.dist-besu/jre" \
        "$(dirname "$0")/bin/besu/$PLATFORM/.dist-besu/jre"; do
        if [ -x "$_besu_jre/bin/java" ]; then
          JAVA_HOME=$(cd "$_besu_jre" && pwd); export JAVA_HOME
          PATH="$JAVA_HOME/bin:$PATH"; export PATH
          echo "  besu: no JVM on PATH/JAVA_HOME -- using bundled JRE $JAVA_HOME"
          break
        fi
      done
      command -v java >/dev/null 2>&1 || { echo "besu needs a JVM and none was found. Set JAVA_HOME=/path/to/jdk (or install java); a besu dist downloaded by this script bundles one at bin/besu/$PLATFORM/.dist-besu/jre." >&2; exit 1; }
    fi ;;
  reth)
    CLIENT_BIN="${RETH_BIN:-$(command -v xdc-reth 2>/dev/null)}"
    if [ ! -x "$CLIENT_BIN" ] && [ "${BIN_SOURCE_reth:-$BIN_SOURCE}" != build ]; then
      download_bins && CLIENT_BIN="$BIN_DIR/xdc-reth"
    fi
    [ -x "$CLIENT_BIN" ] || { echo "reth binary not found. Build reth-xdc (XDCIndia/reth-xdc) and set RETH_BIN=/path/to/xdc-reth, put 'xdc-reth' on PATH, or let this script download it (unset BIN_SOURCE/BIN_SOURCE_reth if you set either to 'build')." >&2; exit 1; } ;;
  nethermind)
    NM_ON_PATH=$(command -v nethermind 2>/dev/null)
    NETHERMIND_DIST="${NETHERMIND_DIST:-${NM_ON_PATH:+$(dirname "$NM_ON_PATH")}}"
    CLIENT_BIN="$NETHERMIND_DIST/nethermind"
    if [ ! -x "$CLIENT_BIN" ] && [ "${BIN_SOURCE_nethermind:-$BIN_SOURCE}" != build ]; then
      if download_bins && [ -n "$DOWNLOADED_DIST" ]; then
        NETHERMIND_DIST="$DOWNLOADED_DIST"
        CLIENT_BIN="$NETHERMIND_DIST/nethermind"
      fi
    fi
    [ -x "$CLIENT_BIN" ] || { echo "nethermind not found. Build the xdc nethermind dist (XDCIndia/nethermind) and set NETHERMIND_DIST=/path/to/dist (containing ./nethermind and chainspec/$NM_CONFIG.json), put it on PATH, or let this script download it (unset BIN_SOURCE/BIN_SOURCE_nethermind if you set either to 'build')." >&2; exit 1; }
    # ItWorksinMyLocal#114 / xdc-nethermind-private#250: nethermind ships only a
    # handful of hand-authored chainspecs (5151/mainnet/testnet) and has no
    # --genesis path of its own. When the caller didn't ask for a specific
    # --nm-config/NM_CONFIG (NM_CONFIG_EXPLICIT unset), a geth-format --genesis
    # WAS given, and that genesis's chainId does NOT match the chainspec
    # NM_CONFIG currently resolves to (whether that chainspec is missing
    # entirely, or just targets a different chain), auto-convert the genesis
    # via netlab/nm-chainspec.sh into a fresh chainspec+config keyed off its
    # own chainId ("xdc-custom-<chainId>") so nethermind can at least ATTEMPT
    # to join a private/per-run network setup.sh generated. KNOWN LIMITATION
    # (see nm-chainspec.sh's own header, xdc-nethermind-private#250): the
    # converted chainspec loads and a genesis block validation actually runs,
    # but nethermind's genesis-block loader deletes the 0x89 BlockSigners
    # contract ("TIPSigning fork") unconditionally at block 0 for any
    # non-hardcoded chainId, producing a genesis state-root/hash mismatch
    # against every other client -- a client-side gap, not fixable from this
    # script; still tracked as an open blocker, not silently papered over.
    if [ -z "$NM_CONFIG_EXPLICIT" ] && [ -f "$GENESIS" ]; then
      AUTO_NM_CHAINID=$(python3 -c "import json,sys
try:
    print(json.load(open(sys.argv[1]))['config']['chainId'])
except Exception:
    pass" "$GENESIS" 2>/dev/null)
      CURRENT_NM_CHAINID=$(python3 -c "import json,sys
try:
    print(json.load(open(sys.argv[1]))['params']['chainId'])
except Exception:
    pass" "$NETHERMIND_DIST/chainspec/$NM_CONFIG.json" 2>/dev/null)
      if [ -n "$AUTO_NM_CHAINID" ] && [ "$AUTO_NM_CHAINID" != "$CURRENT_NM_CHAINID" ]; then
        AUTO_NM_CONFIG="xdc-custom-${AUTO_NM_CHAINID}"
        echo "nethermind: no explicit --nm-config, and the resolved chainspec '$NM_CONFIG' (chainId ${CURRENT_NM_CHAINID:-<missing>}) doesn't match --genesis's chainId $AUTO_NM_CHAINID -- auto-converting $GENESIS into $NETHERMIND_DIST/chainspec/$AUTO_NM_CONFIG.json via netlab/nm-chainspec.sh" >&2
        if "$(dirname "$0")/netlab/nm-chainspec.sh" --genesis "$GENESIS" --name "$AUTO_NM_CONFIG" --dist "$NETHERMIND_DIST"; then
          NM_CONFIG="$AUTO_NM_CONFIG"
        else
          echo "nethermind: genesis->chainspec auto-conversion failed; falling back to the original chainspec-missing error below" >&2
        fi
      fi
    fi
    if [ ! -f "$NETHERMIND_DIST/chainspec/$NM_CONFIG.json" ]; then
      echo "nethermind dist '$NETHERMIND_DIST' is missing chainspec/$NM_CONFIG.json." >&2
      echo "nethermind loads its own Parity-style chainspec matching the target chainId" >&2
      echo "-- it does NOT read the geth-format --genesis -- so joining a custom network" >&2
      echo "needs either a NETHERMIND_DIST that ships a chainspec for it, or --nm-config" >&2
      echo "(env NM_CONFIG) set to a name that's already present, or a --genesis file this" >&2
      echo "script can auto-convert via netlab/nm-chainspec.sh (see above for why that" >&2
      echo "didn't happen/succeed here). Chainspecs present in" >&2
      echo "'$NETHERMIND_DIST/chainspec/':" >&2
      { ls "$NETHERMIND_DIST/chainspec/" 2>/dev/null || echo "  (directory not found)"; } >&2
      exit 1
    fi ;;
  *)
    # oldxdc/geth/erigon build from the XDPoS source via ensure_bins. BUT the
    # current OLDXDC-master build produces an INCOMPATIBLE genesis for net5151
    # (0x3b622c) vs the live chain (0x96f121), so oldxdc gets 0 peers and never
    # leaves block 0 (issue #33). The
    # masternodes run XDPoSChain v2.7.0-devnet, which computes 0x96f121 from the
    # SAME genesis-5151.json and syncs. So let an operator point --client oldxdc at
    # that binary via OLDXDC_BIN (mirrors BESU_BIN/RETH_BIN); otherwise fall back to
    # the source build (works for geth/erigon; oldxdc needs a v2.7.0-devnet build).
    if [ "$CLIENT" = oldxdc ] && [ -n "$OLDXDC_BIN" ] && [ -x "$OLDXDC_BIN" ]; then
      XDC="$OLDXDC_BIN"
      echo "Using OLDXDC_BIN=$OLDXDC_BIN ($("$XDC" version 2>/dev/null | awk '/^Version/{print $2; exit}'))"
    else
      ensure_bins
      XDC="$XDC_BIN"
    fi ;;
esac

# Default the network id to the genesis chainId (erigon --chain <name> needn't).
if [ -z "$NETWORKID" ] && [ -f "$GENESIS" ]; then
  NETWORKID=$(python3 -c "import json,sys;print(json.load(open('$GENESIS'))['config']['chainId'])" 2>/dev/null)
fi
if [ "$CLIENT" != erigon ] && [ -z "$NETWORKID" ]; then
  echo "could not read chainId from $GENESIS; pass --chainid" >&2; exit 1
fi

# The networkid the node runs with and the chainId inside --genesis must agree.
# Nothing downstream fails loudly when they don't: the node starts, reports
# healthy, and simply never peers -- the eth handshake rejects a mismatched
# network -- so it surfaces much later as an unexplained "peers=0, block=0".
#
# The trap this closes: `--chainid N` WITHOUT `--genesis` is not an error today.
# GENESIS silently falls back to DEFAULT_GENESIS from network.env (the public
# devnet 5151 file), so you get networkid=N on a 5151 genesis -- a chain that
# exists nowhere and can never peer with anything. Observed live: five
# followers launched as `--chainid 919` sat at block 0 / peers 0 while
# eth_chainId reported 5151, and ethstats made it look fine because it reports
# `net` from the NETWORKID, so the dashboard grouped them under 919 alongside
# the healthy sealers.
#
# run.sh has asserted this for its own nodes since the --chainid work; join.sh
# did not, which is why the same class of bug reappeared here.
if [ -f "$GENESIS" ] && [ -n "$NETWORKID" ]; then
  _g_chainid=$(python3 -c "import json,sys;print(json.load(open('$GENESIS'))['config']['chainId'])" 2>/dev/null)
  if [ -n "$_g_chainid" ] && [ "$_g_chainid" != "$NETWORKID" ]; then
    echo "join.sh: --chainid $NETWORKID does not match the chainId in $GENESIS ($_g_chainid)." >&2
    echo "  This node would start but never peer. Either:" >&2
    echo "    - pass the matching genesis:  --genesis <file with chainId $NETWORKID>" >&2
    echo "    - or drop --chainid and let it default to the genesis's own id ($_g_chainid)" >&2
    if [ "$GENESIS" = "${DEFAULT_GENESIS:-}" ]; then
      echo "  NOTE: no --genesis was given, so this is network.env's DEFAULT_GENESIS" >&2
      echo "  ($DEFAULT_GENESIS) -- almost certainly not the network you meant to join." >&2
      echo "  For a LOCAL net, pass --genesis genesis/genesis-$NETWORKID.json (and the" >&2
      echo "  local --bootnodes), or use ./run.sh --all which wires all of that up." >&2
    fi
    exit 1
  fi
fi

# erigon can't discover XDPoS peers through the bootnode (its discv4 doesn't
# traverse it), so when joining by --ip/--genesis without explicit --staticpeers
# we borrow a discovering client (oldxdc/geth) to harvest the live peer enodes
# -- the same discovery geth uses -- and hand them to erigon. Gives a simple
# "just an --ip" erigon join.
if [ "$CLIENT" = erigon ] && [ -z "$STATICPEERS" ] && [ -n "$BOOTNODES" ] && [ -f "$GENESIS" ]; then
  STATICPEERS=$(harvest_peers "$GENESIS" "$BOOTNODES" "${NETWORKID:-0}")
  if [ -n "$STATICPEERS" ]; then
    echo "  harvested $(echo "$STATICPEERS" | tr ',' '\n' | grep -c enode) peer(s) from the network"
  else
    echo "  warning: could not harvest peers; erigon may stay at peers=0 (pass --staticpeers)" >&2
  fi
fi

IP=$(detect_ip)
# --name gives the MACHINE component of the identity (it is a label for "which
# deployment/box is this", not the whole name). Feed it to MACHINE so both
# sources land in one place; machine_name() then falls back to this host's short
# hostname when neither was given. HOST stays defined because --identity and the
# datadir echo below still reference it.
[ -n "$NAME" ] && MACHINE="$NAME"
HOST=$(machine_name)
# Node identity: ONE canonical shape for every client and every launch path,
# composed by node_name() in lib.sh --
#
#   <machine>-<client>-v<version>-<commit>-<nodetype>-<ip>
#
# --name now supplies only the FIRST component instead of replacing the whole
# name. It used to override everything, so an operator who labelled a deployment
# ("--name xdclabs-wuhan") lost the client, version, commit and IP from the
# dashboard -- precisely the fields you need when one node out of eight
# misbehaves and you have to tell a stale build from a current one.
_vc=$(client_version_commit "${CLIENT_BIN:-$XDC}")
VER=${_vc%% *}; COMMIT=${_vc##* }
NAME=$(node_name "$HOST" "$CLIENT" "$VER" "$COMMIT" "$NODETYPE" "$IP")
# A mining node also carries its coinbase (0x + 8 hex) so a dashboard row maps to
# the address it mints to. Sealers only -- it is noise on a follower.
[ -n "$MINEADDR" ] && NAME="${NAME}-$(name_part "${MINEADDR:0:10}")"

# Initialise the datadir against the genesis (first run only). oldxdc and the
# GP5 geth fork (both `geth` and the opt-in `geth4`) store chaindata under
# <datadir>/XDC and need an explicit init -- their launch arm passes only
# --networkid, and network id does NOT select a genesis. Omitting a geth-family
# client here means it silently builds ETHEREUM MAINNET's genesis instead: it logs
# "Chain ID: 1 (mainnet)", its block 0 hash differs from the sealers', and the
# handshake can never complete, so it sits at 0 peers no matter what --bootnodes
# says. That is exactly how geth4 failed before it was added to this list.
# erigon (derives from --chain), besu/reth (write genesis on first launch), and
# nethermind (loads its own chainspec) do their own thing, so skip init there.
case "$CLIENT" in
  oldxdc|geth|geth4|"")
    if [ ! -d "$DATADIR/XDC/chaindata" ]; then
      echo "Initialising $DATADIR from $GENESIS ..."
      "$XDC" --datadir "$DATADIR" init "$GENESIS" || exit 1
    fi ;;
esac

# ethstats: prepend the node name unless SPEC already includes one (name:secret@…)
ETHSTATS_ARG=""; full=""; ES_NAME=""; ES_SECRET=""; ES_HOSTPORT=""
if [ -n "$ETHSTATS" ]; then
  case "${ETHSTATS%%@*}" in
    *:*) full="$ETHSTATS";;            # already NAME:SECRET
    *)   full="${NAME}:${ETHSTATS}";;  # just SECRET -> prepend node name
  esac
  ETHSTATS_ARG="--ethstats $full"
  # Split NAME:SECRET@HOST:PORT into pieces -- reth and nethermind want the parts
  # separately and a wss:// scheme rather than geth's bare NAME:SECRET@host:port.
  ES_NAME="${full%%:*}"; _es_rest="${full#*:}"; ES_SECRET="${_es_rest%%@*}"; ES_HOSTPORT="${_es_rest#*@}"
  echo "  ethstats: $full"
fi

echo "Joining ($CLIENT) network=${CHAIN:-id $NETWORKID}  bootnodes=$BOOTNODES"
echo "  datadir=$DATADIR  identity=$NAME  ip=$IP  ports p2p:$PORT rpc:$RPCPORT ws:$WSPORT"

if [ "$CLIENT" = erigon ]; then
  # Erigon (sync-only). --chain takes a built-in name OR a genesis file (the
  # merged genesis-file chainspec support). XDPoS peers via discv4; discovery
  # through an XDPoSChain bootnode doesn't populate erigon, so masternode enodes
  # are usually needed via --staticpeers.
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (erigon join is sync-only)" >&2
  # erigon-xdc v3.5.0+ reads the geth-format --genesis directly via --chain <file>
  # (verified: net5151 -> 0x96f121, full fork schedule, synced to tip). So hand it
  # the ORIGINAL genesis file, not a stripped conversion (the old conversion
  # dropped london/eip1559/cancun fork blocks -> wrong schedule). A built-in
  # --chain NAME still wins, for xdc | xdc-apothem.
  ERC="${CHAIN:-$GENESIS}"
  [ -n "$CHAIN" ] && echo "  erigon built-in chain=$CHAIN" || echo "  erigon genesis-file chain=$ERC (loads directly)"
  echo "  erigon chain=$ERC ports authrpc:$AUTHRPC_PORT torrent:$TORRENT_PORT privapi:$PRIVAPI mcp:$MCP_PORT"
  BN_ARG=""; [ -n "$BOOTNODES" ] && BN_ARG="--bootnodes $BOOTNODES"
  SP_ARG=""; [ -n "$STATICPEERS" ] && SP_ARG="--staticpeers $STATICPEERS"
  [ -z "$STATICPEERS" ] && echo "  note: no --staticpeers given; erigon may not discover XDPoS peers via the bootnode alone" >&2
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="--maxpeers $MAXPEERS"
  # --miner.gaslimit: erigon inherited the same geth-derived miner flag set
  # (cmd/utils/flags.go's MinerGasLimitFlag), so it's accepted here even
  # though a plain sync-only join (no --mine) never actually mints -- it
  # ensures erigon never silently carries a stale/default target if it's
  # later seated as a proposer (ItWorksinMyLocal#62/#94/#96). Its own
  # fix/94-xdc-gas-limit-genesis-plateau branch instead derives the
  # plateau from the chain's genesis when it mines; passing this flag too
  # is a harmless, defence-in-depth belt-and-braces default, never a
  # requirement erigon's mint path relies on.
  GL_ARG="--miner.gaslimit $GAS_LIMIT"
  # erigon's default --p2p.allowed-ports is a HARDCODED [30303,30304,30305,30306,
  # 30307] (from its own flag default, independent of --port) with our --port only
  # prepended, not substituted -- so it still tries to bind that classic range and
  # fails ("run out of allowed ports") whenever those ports are already taken by a
  # local net or another join.sh client. Give it its own private range based on
  # $PORT instead (comma-separated per erigon's own --p2p.allowed-ports syntax);
  # 5 ports comfortably covers its eth-protocol version count (seen up to 4).
  ALLOWED_PORTS="$PORT,$((PORT+1)),$((PORT+2)),$((PORT+3)),$((PORT+4))"
  # ...but only if this erigon still HAS that flag. It is gone in erigon 3.5.2
  # ("flag provided but not defined: -p2p.allowed-ports", which kills the node
  # at startup), so probe instead of assuming. Without the flag erigon picks its
  # own ports upward from --port, one per --p2p.protocol version (default
  # 69,70,71), so callers must leave a gap after $PORT either way -- run.sh
  # --all strides its followers by 10 for exactly this reason.
  AP_ARG=""
  if "$XDC" --help 2>&1 | grep -q 'p2p\.allowed-ports'; then
    AP_ARG="--p2p.allowed-ports $ALLOWED_PORTS"
  else
    echo "  erigon: no --p2p.allowed-ports in this build; it will choose ports upward from $PORT"
  fi
  # shellcheck disable=SC2086  # $AP_ARG is an intentionally-splitting flag pair
  "$XDC" --datadir "$DATADIR" --chain "$ERC" --discv4 \
    --port "$PORT" $AP_ARG --nat "extip:${IP}" $BN_ARG $SP_ARG $MP_ARG $GL_ARG \
    --http --http.addr 0.0.0.0 --http.port "$RPCPORT" --http.vhosts "*" \
    --http.corsdomain "*" --http.api eth,net,web3,erigon,debug,txpool \
    --ws --ws.port "$WSPORT" \
    --authrpc.port "$AUTHRPC_PORT" --torrent.port "$TORRENT_PORT" \
    --private.api.addr "$PRIVAPI" --mcp.port "$MCP_PORT" \
    $ETHSTATS_ARG &
elif [ "$CLIENT" = geth ] || [ "$CLIENT" = geth4 ]; then
  # Modern geth: dot-notation flags, sync-only (it can't seal here). --maxpeers
  # keeps its long-standing default of 25 unless --maxpeers/env MAXPEERS overrides it.
  #
  # --authrpc.port is a DELIBERATE exception to "defaults unchanged": geth
  # always starts an Engine API (authrpc) listener regardless -- unused by
  # XDPoS ("Engine API started but chain not configured for merge yet") --
  # defaulting to 127.0.0.1:8551 with NO flag to move it. That's a
  # collision landmine on any shared host running more than one geth-family
  # process (run.sh's own --mixed node5 already had to work around exactly
  # this: "commonly already taken by another node ... would otherwise crash
  # node 5 at startup"). Pinning it to AUTHRPC_PORT's already-computed
  # per-client-offset default (here: 10745) just brings geth in line with
  # every OTHER join.sh client, which already gets an explicit, non-default
  # authrpc port unconditionally.
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (geth join is sync-only)" >&2
  # --miner.gaslimit: geth-xdc hard-codes params.XDCBlockGasLimit and
  # DELIBERATELY IGNORES this flag (miner/xdpos_worker.go's xdcGasCeil) --
  # passed anyway so the invocation is never silently missing it (an
  # operator diffing invocations for #94/#96 compliance should see the
  # same --gas-limit-derived flag on every client, even a client that
  # happens to disregard it).
  "$XDC" --datadir "$DATADIR" --networkid "$NETWORKID" --syncmode "$SYNCMODE" \
    --bootnodes "$BOOTNODES" --port "$PORT" --nat "extip:${IP}" --identity "$NAME" --maxpeers "${MAXPEERS:-25}" \
    --miner.gaslimit "$GAS_LIMIT" \
    --http --http.corsdomain "*" --http.addr 0.0.0.0 --http.port "$RPCPORT" --http.vhosts "*" \
    --http.api eth,net,web3,txpool,debug,admin,XDPoS \
    --ws --ws.addr 0.0.0.0 --ws.origins "*" --ws.port "$WSPORT" --ws.api eth,net,web3,XDPoS \
    --authrpc.port "$AUTHRPC_PORT" \
    $ETHSTATS_ARG &
elif [ "$CLIENT" = besu ]; then
  # besu-xdc (sync-only). Reads the geth-format genesis natively via --genesis-file
  # (no translation). Needs BOTH --bootnodes and --static-nodes-file: bootnodes are
  # only used for initial discovery, and besu drops the validators as "UselessPeer"
  # after capability negotiation unless they are pinned via static-nodes.
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (besu join is sync-only)" >&2
  [ -n "$BOOTNODES" ] || { echo "besu join needs peers: set DEFAULT_BOOTNODES in network.env or pass --bootnodes" >&2; exit 1; }
  mkdir -p "$DATADIR"
  python3 -c "import json,sys;print(json.dumps([e for e in sys.argv[1].split(',') if e]))" "$BOOTNODES" > "$DATADIR/static-nodes.json"
  # besu's --ethstats is <[ws://|wss://]nodename:secret@host:[port]> and the
  # scheme is OPTIONAL -- omit it and besu dials plaintext ws://. The collector
  # is TLS-only on :443, so it rejected every besu report with
  #   ERROR EthStatsService | Failed to reach the ethstats server due to:
  #   WebSocket upgrade failure: 400
  # and besu stayed invisible on the dashboard while syncing perfectly at tip --
  # a silent REPORTING failure, not a sync failure, which is why it looked like
  # "besu is not syncing". Every other client either takes the scheme explicitly
  # (reth passes @wss://<host>) or infers it (xone), so besu was the only one
  # missing from the network's page. Only prepend when no scheme was supplied.
  BESU_ES=""
  if [ -n "$full" ]; then
    case "$full" in
      ws://*|wss://*) BESU_ES="--ethstats=$full" ;;
      *)              BESU_ES="--ethstats=wss://$full" ;;
    esac
  fi
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="--max-peers=$MAXPEERS"
  # besu expects an upper-case sync mode (SNAP|FAST|FULL); map from $SYNCMODE.
  BESU_SYNC=$(printf '%s' "$SYNCMODE" | tr '[:lower:]' '[:upper:]')
  # besu will not begin syncing until it has --sync-min-peers peers, and the
  # default is 5. A private XDPoS net here runs FOUR sealers, so besu can never
  # reach that threshold and waits forever, logging
  #   "Unable to find sync target. Waiting for 5 peers minimum."
  # while sitting at block 0 with a genesis hash that matches the sealers
  # exactly. It looks like a consensus or peering fault and is neither -- it is
  # a threshold that cannot be met on a small network. Default to 1 and let the
  # operator raise it (BESU_SYNC_MIN_PEERS) on a net large enough to warrant it.
  "$CLIENT_BIN" \
    --genesis-file="$GENESIS" --data-path="$DATADIR" --network-id="$NETWORKID" \
    --sync-mode="$BESU_SYNC" --data-storage-format=BONSAI \
    --sync-min-peers="${BESU_SYNC_MIN_PEERS:-1}" \
    --p2p-port="$PORT" --p2p-host="$IP" --nat-method=NONE \
    --bootnodes="$BOOTNODES" --static-nodes-file="$DATADIR/static-nodes.json" \
    --rpc-http-enabled=true --rpc-http-host=127.0.0.1 --rpc-http-port="$RPCPORT" \
    --rpc-http-apis=ETH,NET,WEB3,ADMIN,DEBUG,TXPOOL \
    --engine-rpc-enabled=false --min-gas-price=0 --auto-log-bloom-caching-enabled=false \
    $MP_ARG $BESU_ES &
elif [ "$CLIENT" = reth ]; then
  # reth-xdc (sync-only). Reads the geth-format genesis natively via --chain <file>.
  # Peers via trusted-only (its discv4/v5 doesn't traverse an XDPoS bootnode), and
  # its --ethstats wants a wss:// URL, not geth's bare host:port.
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (reth join is sync-only)" >&2
  [ -n "$BOOTNODES" ] || { echo "reth join needs peers: set DEFAULT_BOOTNODES in network.env or pass --bootnodes" >&2; exit 1; }
  mkdir -p "$DATADIR"
  RETH_ES=""; [ -n "$full" ] && RETH_ES="--ethstats ${ES_NAME}:${ES_SECRET}@wss://${ES_HOSTPORT}"
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="--max-peers $MAXPEERS"
  # reth now achieves strict net5151 state parity natively: it applies the
  # TIPSigning 0x89 wipe and the net5151 calldata-gas / basefee-floor corrections,
  # so its own state root matches the header across the V1->V2 switch at 1800 with
  # NO state-root masking. (XDCIndia/reth main >= 22adb15, PRs #247/#249/#242 --
  # the earlier XDC_ENABLE_STATE_ROOT_BYPASS is gone.) Point RETH_BIN at a build
  # of main; older bypass-based builds are no longer needed.
  "$CLIENT_BIN" node --chain "$GENESIS" --datadir "$DATADIR" \
    --port "$PORT" --discovery.port "$PORT" \
    --disable-discv5-discovery --disable-dns-discovery --trusted-only \
    --nat "extip:${IP}" --trusted-peers "$BOOTNODES" \
    --http --http.addr 127.0.0.1 --http.port "$RPCPORT" --http.api eth,net,web3,admin,debug \
    --authrpc.addr 127.0.0.1 --authrpc.port "$AUTHRPC_PORT" \
    $MP_ARG $RETH_ES &
elif [ "$CLIENT" = nethermind ]; then
  # nethermind-xdc (sync-only). Loads its own Parity-style chainspec that ships in
  # the dist ($NETHERMIND_DIST/chainspec/$NM_CONFIG.json) via --config; it does
  # NOT read the geth-format genesis. nethermind resolves configs/ and chainspec/
  # relative to CWD, so cd into the dist first. WebSocketsPort must be set explicitly
  # (the config hardcodes 8549; a clash aborts the whole JSON-RPC stack silently).
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (nethermind join is sync-only)" >&2
  [ -n "$BOOTNODES" ] || { echo "nethermind join needs peers: set DEFAULT_BOOTNODES in network.env or pass --bootnodes" >&2; exit 1; }
  mkdir -p "$DATADIR"
  # Resolve the datadir to an ABSOLUTE path BEFORE the `cd "$NETHERMIND_DIST"`
  # below: nethermind is launched from inside the dist (to resolve configs/ and
  # chainspec/), so a relative --Init.BaseDbPath would land inside the dist dir
  # (e.g. <dist>/nodes/<client>-sync) instead of here -- and silently reuse a
  # stale DB there, ignoring the intended (and chainspec-fresh) datadir.
  NM_DATADIR="$(cd "$DATADIR" && pwd)"
  WS_PORT=$((RPCPORT + 1))
  NM_ES=""; [ -n "$full" ] && NM_ES="--EthStats.Enabled true --EthStats.Name $ES_NAME --EthStats.Secret $ES_SECRET --EthStats.Server wss://${ES_HOSTPORT}/api"
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="--Network.MaxActivePeers $MAXPEERS"
  # nethermind sync mode is config-driven (no --syncmode). Map $SYNCMODE to its
  # Sync.* toggles: snap => SnapSync (+FastSync), fast => FastSync; full => neither
  # (its default, which is what devnet5151 has always used -> unchanged there).
  NM_SYNC=""
  case "$SYNCMODE" in
    snap) NM_SYNC="--Sync.SnapSync true --Sync.FastSync true" ;;
    fast) NM_SYNC="--Sync.FastSync true" ;;
  esac
  # net5151: disable nethermind peer persistence. With it ON, the DB-loaded copies
  # of the validator node IDs register in the peer pool BEFORE the static-config
  # entries, so those nodes never land in _staticPeers; once the validators drop
  # nethermind as UselessPeer (they do, on the legacy XDPoS base), nethermind never
  # reconnects -- 0 peers, "Waiting for peers..." forever, stalled behind tip.
  # IsPeersPersistenceOn is NOT exposed as a CLI arg in this build, so patch it into
  # the dist config before launch (idempotent). Verified live: nethermind then holds
  # the validators and stays on-tip. Underlying nethermind fix: xdc-nethermind #249.
  python3 - "$NETHERMIND_DIST/configs/$NM_CONFIG.json" <<'PY'
import json, sys
p = sys.argv[1]
try:
    cfg = json.load(open(p))
    cfg.setdefault("Network", {})["IsPeersPersistenceOn"] = False
    json.dump(cfg, open(p, "w"), indent=2)
    print("  nethermind: set Network.IsPeersPersistenceOn=false in %s" % p)
except Exception as e:
    sys.stderr.write("  warning: could not patch nethermind config %s: %s\n" % (p, e))
PY
  ( cd "$NETHERMIND_DIST" && ./nethermind \
    --config "$NM_CONFIG" \
    --Init.BaseDbPath "$NM_DATADIR" \
    --Network.P2PPort "$PORT" --Network.DiscoveryPort "$PORT" \
    --JsonRpc.Port "$RPCPORT" --JsonRpc.WebSocketsPort "$WS_PORT" --JsonRpc.Host 127.0.0.1 \
    --Network.StaticPeers "$BOOTNODES" \
    $NM_SYNC $MP_ARG $NM_ES ) &
elif [ "$CLIENT" = xone ]; then
  # xone-native (XDCIndia/xOneGo): go-ethereum-derived XDPoS client with
  # single-dash flags. Sync-only join here -- it recognises XDPoS from the genesis
  # (config-driven) and inits from -override.genesis on a fresh datadir. Sealing
  # needs a raw -miner.key, which a sync join doesn't carry, so --mine is noted.
  [ -n "$MINEADDR" ] && echo "note: --mine ignored (xone join is sync-only; sealing needs -miner.key)" >&2
  mkdir -p "$DATADIR"
  XONE_ES=""; [ -n "$full" ] && XONE_ES="-ethstats $full"
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="-maxpeers $MAXPEERS"
  "$XDC" -override.genesis "$GENESIS" -networkid "$NETWORKID" -syncmode "$SYNCMODE" \
    -datadir "$DATADIR" -port "$PORT" -discovery.port "$PORT" -nat "extip:${IP}" \
    -bootnodes "$BOOTNODES" $MP_ARG -miner.gaslimit "$GAS_LIMIT" \
    -http -http.addr 0.0.0.0 -http.port "$RPCPORT" -http.vhosts "*" \
    -http.api eth,net,web3,admin,debug,txpool,XDPoS \
    -authrpc.port "$AUTHRPC_PORT" \
    $XONE_ES &
else
  # oldxdc (XDPoSChain): classic flags; --mine unlocks a validator to also seal.
  # --maxpeers/--etherbase are additive: omitted (client default) unless given.
  MINE_FLAGS=""
  if [ -n "$MINEADDR" ]; then
    touch .pwd
    MINE_FLAGS="--unlock $MINEADDR --password ./.pwd --mine --gasprice 1"
    [ -n "$ETHERBASE" ] && MINE_FLAGS="$MINE_FLAGS --etherbase $ETHERBASE"
  fi
  MP_ARG=""; [ -n "$MAXPEERS" ] && MP_ARG="--maxpeers $MAXPEERS"
  # --targetgaslimit: THE flag that was missing entirely before #96 -- with
  # no flag passed at all, this legacy arbiter falls back to its own
  # compiled-in 50,000,000 default (cmd/utils/flags.go MinerGasLimitFlag),
  # silently different from whatever geth-xdc/erigon-xdc target elsewhere
  # on the same chain (their own hard-coded 420,000,000 "XDC plateau").
  # That mismatch is exactly netv12's #94 wedge. Passed unconditionally
  # (not just inside MINE_FLAGS) since even a non-mining oldxdc join
  # should never silently carry a different ceiling than every other node
  # in the same topology.
  "$XDC" --datadir "$DATADIR" --networkid "$NETWORKID" --syncmode full \
    --bootnodes "$BOOTNODES" --port "$PORT" --nat "extip:${IP}" --identity "$NAME" $MP_ARG \
    --targetgaslimit "$GAS_LIMIT" \
    --rpc --rpccorsdomain "*" --rpcaddr 0.0.0.0 --rpcport "$RPCPORT" --rpcvhosts "*" \
    --ws --wsaddr 0.0.0.0 --wsorigins "*" --wsport "$WSPORT" \
    --rpcapi admin,db,eth,debug,miner,net,shh,txpool,personal,web3,XDPoS \
    $MINE_FLAGS $ETHSTATS_ARG &
fi
child_proc=$!
wait
