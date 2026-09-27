#!/bin/bash
# nm-chainspec.sh - convert a geth-format XDPoS genesis.json into a Parity-
# style chainspec + matching configs/<name>.json that the nethermind dist can
# load via `--config <name>` (ItWorksinMyLocal#62 harness feature, wired into
# join.sh's setup.sh-network path by #114; xdc-nethermind-private#250: "no
# genesis->chainspec converter").
#
# Usage:
#   nm-chainspec.sh --genesis FILE --name NAME [--dist DIR] [--force]
#
# Flags:
#   --genesis FILE   the target network's geth-format genesis.json (the same
#                     file every other join.sh client consumes natively --
#                     see gen-genesis.sh / fleet.sh). Must have config.XDPoS.
#   --name NAME      chainspec/config basename to write, e.g. "xdc-custom-34093".
#                     Written to <dist>/chainspec/<name>.json and
#                     <dist>/configs/<name>.json -- exactly where join.sh's
#                     nethermind branch (`--config NAME`) already looks.
#   --dist DIR       nethermind dist root (default: env NETHERMIND_DIST).
#                     Must contain (or be creatable with) chainspec/ and
#                     configs/ subdirectories.
#   --force          overwrite an existing chainspec/config at that name
#                     (default: skip re-conversion if both already exist --
#                     idempotent for repeated join.sh/fleet.sh invocations).
#
# What this DOES map (mechanical, field-for-field, matching the one proven
# hand-authored working chainspec this dist ships, chainspec/xdc-devnet5151.json):
#   config.chainId              -> params.chainId, params.networkID
#   config.XDPoS.period/epoch/reward/rewardCheckpoint/gap
#                                -> engine.XDPoS.params.{period,epoch,reward,
#                                   rewardCheckpoint,gap}
#   config.XDPoS.foudationWalletAddr (sic -- the geth-side genesis format's
#     own typo; carried through XDCIndia/XDPoSChain's puppeth fork)
#                                -> engine.XDPoS.params.foundationWalletAddr
#                                   (correctly spelled -- matches xdc-devnet5151.json)
#   config.XDPoS.v2.SwitchEpoch/switchBlock
#                                -> engine.XDPoS.params.switchEpoch/switchBlock
#   config.XDPoS.v2.allConfigs  -> engine.XDPoS.params.v2Configs[] (each entry:
#                                   maxMasternodes->MaxMasternodes, switchRound->
#                                   SwitchRound, certificateThreshold->CertThreshold,
#                                   timeoutSyncThreshold->TimeoutSyncThreshold,
#                                   timeoutPeriod->TimeoutPeriod, minePeriod->MinePeriod),
#                                   sorted by round key ascending
#   nonce/mixHash/difficulty/coinbase/timestamp/extraData/gasLimit
#                                -> genesis.seal.ethereum.{nonce,mixHash},
#                                   genesis.{difficulty,author,timestamp,extraData,gasLimit}
#                                   (author <- coinbase; parentHash synthesized
#                                   as 32 zero bytes, same as every genesis block)
#   alloc[addr] {balance,code,storage,nonce}
#                                -> accounts["0x"+addr] (0x-prefixed; fields
#                                   passed through verbatim)
#
# What this DOES map for the post-Byzantium hardfork schedule (fixed
# xdc-nethermind-private#254 -- see below): early Homestead-family EIPs are
# still bundled at fixed low blocks 2-4 (matching the one hand-authored
# reference, xdc-devnet5151.json, exactly -- genesis has no field-by-field
# equivalent of its own for these). But the Berlin/London-style bundle
# (eip2929/eip3529/eip3198/Eip1559) is now read DIRECTLY from the genesis's
# own config.eip1559Block (falling back to switchBlock + gap only if that key
# is absent, for old-style genesis files predating gen-genesis.sh pinning it) --
# NOT reverse-engineered from switchBlock + gap unconditionally.
#
# Why the switch: `switch_block + gap` and the real eip1559Block are NOT the
# same formula on every network. On net5151 (switchBlock=1800, gap=450) they
# happen to coincide (1800+450=2250, its actual eip1559Block) -- that
# coincidence is what got baked into the old formula. But gen-genesis.sh (this
# repo's own generator for setup.sh networks) computes eip1559Block as
# switchBlock - gap (the XDPoS gap-block formula, S - S%Epoch - Gap), and pins
# it explicitly into config.eip1559Block/config.londonBlock since #129. On
# chainId 20118 (switchBlock=2700, gap=450) that is 2250 -- but the old
# `switch_block + gap` formula computed 3150 (= this genesis's cancunBlock,
# switchBlock + gap, by coincidence of the same two inputs), 900 blocks late.
# nethermind's ChainSpecBasedSpecProvider gates BerlinBlockNumber/
# IsEip2929Enabled/IsEip1559Enabled generically off exactly these transition
# values (Nethermind.Specs.ChainSpecStyle.ChainSpecLoader.cs:254-255,
# ChainSpecBasedSpecProvider.cs:221-226) -- there is no XDC-specific override,
# so a wrong transition value is a silent, deterministic gas-schedule
# divergence, not a client bug. It surfaced as block 2251 (first block after
# the real eip1559Block=2250) rejected with gasUsed off by exactly the
# cold-access repricing delta (EIP-2929 not yet active in nethermind's
# calculation, already active in the sealers'): expected 465654, got 450854,
# diff -14800. Fixed by reading the genesis's own value instead of
# re-deriving a formula that only worked by coincidence for one sample size
# of one. See xdc-nethermind-private#254 for the full block-level evidence.
#
# KNOWN LIVE WALL (verified 2026-07, xdc-nethermind-private#250): a converted
# chainspec loads cleanly (engine params + accounts parse, genesis-block
# processing runs for real, comparably to the bundled xdc-devnet5151.json)
# but nethermind's own genesis-block loader deletes the 0x89 BlockSigners
# contract ("[XDC] Block 0: TIPSigning fork") DURING genesis processing for
# ANY chainId this converter targets, producing a state-root/hash mismatch
# against oldxdc/geth (who keep 0x89 live pre-TIPSigning). The bundled
# xdc-devnet5151.json does NOT hit this at genesis -- there, the same
# deletion only fires at a later real block (4) once actually syncing --
# strongly suggesting nethermind's Xdc plugin keys "TIPSigning active since
# genesis" off a hardcoded/whitelisted-chainId table (5151/mainnet/testnet)
# rather than any chainspec-exposed field: tried params keys tipSigningBlock,
# TipSigningBlock, tipSigning, TIPSigningBlock, tipsigning, TipSigning,
# tipSigningSwitchBlock, TipSigningSwitchBlock (engine.XDPoS.params) -- none
# changed the behavior. This blocks G1 (genesis parity) for nethermind on
# ANY custom/private chainId today, independent of this script -- a
# client-side gap (no exposed TipSigningBlock config), not a mapping bug
# here. See xdc-nethermind-private#250 for the live repro/evidence.

set -euo pipefail

GENESIS=""; NAME=""; DIST="${NETHERMIND_DIST:-}"; FORCE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --genesis) GENESIS=$2; shift 2;;
    --name)    NAME=$2; shift 2;;
    --dist)    DIST=$2; shift 2;;
    --force)   FORCE=1; shift;;
    -h|--help) sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "nm-chainspec.sh: unknown flag: $1" >&2; exit 1;;
  esac
done

[ -n "$GENESIS" ] || { echo "nm-chainspec.sh: --genesis FILE is required" >&2; exit 1; }
[ -f "$GENESIS" ] || { echo "nm-chainspec.sh: no such genesis file: $GENESIS" >&2; exit 1; }
[ -n "$NAME" ] || { echo "nm-chainspec.sh: --name NAME is required" >&2; exit 1; }
[ -n "$DIST" ] || { echo "nm-chainspec.sh: --dist DIR is required (or set NETHERMIND_DIST)" >&2; exit 1; }

CHAINSPEC_OUT="$DIST/chainspec/$NAME.json"
CONFIG_OUT="$DIST/configs/$NAME.json"

if [ -z "$FORCE" ] && [ -f "$CHAINSPEC_OUT" ] && [ -f "$CONFIG_OUT" ]; then
  echo "nm-chainspec.sh: $CHAINSPEC_OUT and $CONFIG_OUT already exist (pass --force to regenerate)"
  exit 0
fi

mkdir -p "$(dirname "$CHAINSPEC_OUT")" "$(dirname "$CONFIG_OUT")"

python3 - "$GENESIS" "$NAME" "$CHAINSPEC_OUT" "$CONFIG_OUT" <<'PY'
import json
import sys

genesis_path, name, chainspec_out, config_out = sys.argv[1:5]

g = json.load(open(genesis_path))
cfg = g.get("config", {})
xdpos = cfg.get("XDPoS")
if xdpos is None:
    sys.stderr.write("nm-chainspec.sh: %s has no config.XDPoS -- not an XDPoS genesis\n" % genesis_path)
    sys.exit(1)

chain_id = cfg["chainId"]
v2 = xdpos.get("v2", {})
switch_block = v2.get("switchBlock", 0)
gap = xdpos.get("gap", 450)
# eip1559Block is the real fork-activation block every other client (oldxdc,
# geth-xdc, erigon, xone, besu) actually reads. gen-genesis.sh pins it
# explicitly (config.eip1559Block, == config.londonBlock -- see #129) as of
# xdc-nethermind-private#254; only fall back to the old switch_block + gap
# guess for a genesis that predates that pinning and has no such key at all.
post_v2 = cfg.get("eip1559Block", switch_block + gap)

def norm_addr(a):
    a = a.lower()
    if not a.startswith("0x"):
        a = "0x" + a
    return a

all_configs = v2.get("allConfigs", {}) or {}
v2_configs = []
for round_key in sorted(all_configs, key=lambda k: int(k)):
    c = all_configs[round_key]
    v2_configs.append({
        "MaxMasternodes": c.get("maxMasternodes", 108),
        "SwitchRound": c.get("switchRound", int(round_key)),
        "CertThreshold": c.get("certificateThreshold", 0.667),
        "TimeoutSyncThreshold": c.get("timeoutSyncThreshold", 3),
        "TimeoutPeriod": c.get("timeoutPeriod", 30),
        "MinePeriod": c.get("minePeriod", xdpos.get("period", 2)),
    })
if not v2_configs:
    # v2.config (singular, the "current" config) is always present even if
    # allConfigs is empty -- fall back to it so a converted chainspec is
    # never missing v2Configs entirely.
    c = v2.get("config", {})
    v2_configs = [{
        "MaxMasternodes": c.get("maxMasternodes", 108),
        "SwitchRound": c.get("switchRound", 0),
        "CertThreshold": c.get("certificateThreshold", 0.667),
        "TimeoutSyncThreshold": c.get("timeoutSyncThreshold", 3),
        "TimeoutPeriod": c.get("timeoutPeriod", 30),
        "MinePeriod": c.get("minePeriod", xdpos.get("period", 2)),
    }]

chainspec = {
    "name": name,
    "engine": {
        "XDPoS": {
            "params": {
                "period": xdpos.get("period", 2),
                "epoch": xdpos.get("epoch", 900),
                "reward": xdpos.get("reward", 5000),
                "rewardCheckpoint": xdpos.get("rewardCheckpoint", xdpos.get("epoch", 900)),
                "gap": gap,
                "foundationWalletAddr": norm_addr(xdpos.get("foudationWalletAddr", xdpos.get("foundationWalletAddr", "0x0000000000000000000000000000000000000000"))),
                "switchEpoch": v2.get("SwitchEpoch", v2.get("switchEpoch", 0)),
                "switchBlock": switch_block,
                "v2Configs": v2_configs,
            }
        }
    },
    "params": {
        "chainId": chain_id,
        "maximumExtraDataSize": "0x400",
        "Eip150Transition": 2,
        "Eip155Transition": 3,
        "Eip160Transition": 3,
        "Eip161abcTransition": 3,
        "Eip161dTransition": 3,
        "Eip140Transition": 4,
        "Eip211Transition": 4,
        "Eip214Transition": 4,
        "Eip658Transition": 4,
        "Eip198Transition": 4,
        "Eip212Transition": 4,
        "Eip213Transition": 4,
        "eip1344Transition": 4,
        "eip1884Transition": 4,
        "eip2028Transition": 100000000,
        "eip2200Transition": 4,
        "eip152Transition": 4,
        "eip1108Transition": 4,
        "eip2565Transition": 100000000,
        "eip2929Transition": post_v2,
        "eip2930Transition": 100000000,
        "eip2718Transition": 100000000,
        "eip3529Transition": post_v2,
        "eip3541Transition": 100000000,
        "eip3198Transition": post_v2,
        "networkID": chain_id,
        "Eip158Transition": 3,
        "Eip1559Transition": post_v2,
    },
    "genesis": {
        "seal": {
            "ethereum": {
                "nonce": g.get("nonce", "0x0"),
                "mixHash": g.get("mixHash", "0x" + "00" * 32),
            }
        },
        "difficulty": g.get("difficulty", "0x1"),
        "author": g.get("coinbase", "0x0000000000000000000000000000000000000000"),
        "timestamp": g.get("timestamp", "0x0"),
        "parentHash": "0x" + "00" * 32,
        "extraData": g["extraData"],
        "gasLimit": g.get("gasLimit", "0x47b760"),
    },
    "nodes": [],
    "accounts": {},
}

for addr, acct in g.get("alloc", {}).items():
    out_acct = {"balance": acct.get("balance", "0x0")}
    if "nonce" in acct:
        out_acct["nonce"] = acct["nonce"]
    if "code" in acct:
        out_acct["code"] = acct["code"]
    if "storage" in acct:
        out_acct["storage"] = acct["storage"]
    chainspec["accounts"][norm_addr(addr)] = out_acct

json.dump(chainspec, open(chainspec_out, "w"), indent=2)

nm_config = {
    "Init": {
        "ChainSpecPath": "chainspec/%s.json" % name,
        "BaseDbPath": "nethermind_db/%s" % name,
        "LogFileName": "%s.log" % name,
        "MemoryHint": 2048000000,
    },
    "Sync": {
        "FastSync": False,
        "SnapSync": False,
        "DownloadReceiptsInFastSync": False,
        "DownloadBodiesInFastSync": False,
        "NonValidatorNode": True,
    },
    "Metrics": {"NodeName": "XDC-Nethermind-%s" % name},
    "JsonRpc": {
        "Enabled": True,
        "Timeout": 20000,
        "Host": "127.0.0.1",
        "Port": 8695,
        "EnabledModules": ["Admin", "Debug", "Eth", "Net", "Trace", "Web3"],
        "WebSocketsPort": 8696,
    },
    "Merge": {"Enabled": False},
    "Pruning": {"Mode": "None"},
    "Network": {
        "P2PPort": 30393,
        "DiscoveryPort": 30393,
        "MaxActivePeers": 50,
        "IsPeersPersistenceOn": False,
    },
    "EthStats": {
        "Enabled": False,
        "Name": "xdclabs-nethermind-%s" % name,
        "Secret": "xdc_openscan_stats_2026",
        "Server": "wss://stats.xdcindia.com:443/api",
    },
}
json.dump(nm_config, open(config_out, "w"), indent=2)

print("OK: wrote %s and %s (chainId %s, switchBlock %s, gap %s, %d account(s))" % (
    chainspec_out, config_out, chain_id, switch_block, gap, len(chainspec["accounts"])))
PY
