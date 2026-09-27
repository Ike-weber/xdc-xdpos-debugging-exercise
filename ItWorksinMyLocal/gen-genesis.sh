#!/bin/bash
# Generate genesis/genesis.json via the bundled (bug-fixed) puppeth using flags.
#
# Puppeth has no genesis flags of its own (only --network); this wrapper feeds
# its interactive wizard from the flags below. Addresses may be given with or
# without a 0x / xdc prefix; lists are comma-separated. All flags are optional
# and default to the current local 4-masternode setup.
#
# Usage:
#   ./gen-genesis.sh
#   ./gen-genesis.sh --chainid 12345 --signers 0xAAAA...,0xBBBB... --owner 0xCCCC...
#
# Flags:
#   --network NAME            puppeth network name (default localdpos)
#   --chainid N               chain / network id (default 20118)
#   --owner ADDR              owner of the masternodes
#   --signers a,b,c           masternode/sealer addresses (>=1)
#   --reward N                block reward (default 5000)
#   --v2block N               v2 consensus switch block (default 2700)
#   --epoch N                 blocks per epoch (default 900). WARNING: legacy
#                             oldxdc hardcodes its own internal checkpoint
#                             constant (EpocBlockRandomize=900), decoupled
#                             from this genesis field -- any --epoch other
#                             than 900 freezes the chain at the first
#                             checkpoint attempt ("Block sealing failed:
#                             can't get block validator: this block is not
#                             checkpoint block", repeating forever). Keep it
#                             at 900 (the default) unless/until that legacy
#                             constant is fixed; see lab/gen-lab-genesis.sh's
#                             "finding F1" comment, discovered the same way.
#   --gap N                   blocks before checkpoint (default 450)
#   --period N                block time in seconds (leave unset to keep
#                             puppeth's own default, 2). Post-processed like
#                             --skip-v1-validation; doesn't affect the
#                             genesis hash. Useful for reaching the
#                             (mandatory, see --epoch above) 900-block
#                             checkpoint faster in a test, e.g. --period 1.
#   --foundation ADDR         foundation wallet
#   --foundation-owners a,b   foundation multisig confirmers
#   --team-owners a,b         team multisig confirmers
#   --swap ADDR               swap wallet (55m fund)
#   --prefund a,b             pre-funded accounts
#   --skip-v1-validation B    true|false: post-process config.XDPoS.SkipV1Validation
#                             (default: leave puppeth's own default, false, alone).
#                             Setting true disables the v1 double-validation
#                             "blocksigner" system transactions (calls to the
#                             0x...89 contract each masternode submits per
#                             block). This does not change the genesis hash
#                             (config isn't part of the header).
#
#                             DO NOT set this for a mixed legacy+modern-geth
#                             network. This doc used to say the opposite --
#                             that it was REQUIRED there, to dodge a +3600
#                             gas/tx mismatch on those blocksigner calls. Both
#                             halves of that are now wrong:
#
#                             (a) It BREAKS the mixed topology. The field
#                             exists only in legacy XDPoSChain; the modern XDC
#                             go-ethereum fork has no such config key at all.
#                             Legacy honours it and seals difficulty=1, modern
#                             computes difficulty from the masternode count and
#                             rejects every block: "invalid difficulty have=1
#                             want=4". A modern follower freezes a few blocks
#                             in while peering and the genesis hash look fine.
#
#                             (b) The gas mismatch it dodged is fixed upstream.
#                             It came from the modern client injecting fork
#                             blocks the genesis never declared (istanbul/
#                             berlin/london=0), whose EIP-1884/2200/2929
#                             repricing mispriced BlockSigners.sign(). The fork
#                             now caps inherited forks at the highest one the
#                             genesis actually declares (core/genesis.go
#                             forkPatchGenesisAuthoritative).
#
#                             Still useful for a legacy-only net where you
#                             genuinely want v1 validation off.
#   --timestamp N             post-process the header's "timestamp" field
#                             (0x-hex or decimal; default: leave puppeth's
#                             own default alone, the wall-clock time it ran
#                             at). Unlike --skip-v1-validation/--period,
#                             timestamp IS part of the header, so it DOES
#                             change the genesis hash -- but puppeth stamps
#                             the live clock in unconditionally, so without
#                             this flag no two invocations (even with
#                             identical --signers/--chainid/etc) ever
#                             produce the same genesis. Pass a fixed value
#                             (e.g. --timestamp 0x0) for a reproducible,
#                             assertable-hash genesis -- netlab/fleet.sh
#                             does this for every topology it generates.
#   --out FILE                output path (default genesis/genesis.json)

NETWORK=localdpos
CHAINID=20118
OWNER=518075434a104652c5C469BDC04521EA638008Fb
SIGNERS=Bd29914857fBc1c3fA6EE10cc4708B89534878d5,76fBfFd7ac5D53F7A81d45c41E3036068FF8Ba36,d3d1a36F577a4673d6Eb9A96e46C02b365E29Cb3,20aDa917619f7dEA50C8f44d00E4450336543831
REWARD=5000
V2BLOCK=2700
EPOCH=900
GAP=450
FOUNDATION=65290e6eE7ecD267852832e3D1e1211Be42DF33A
FOUNDATION_OWNERS=F7dC23CF42AE8Bf1225e9D411E566eA1484554dd,5cDdE6F2bE1e5E464749104aF42753D6c8Bff42C
TEAM_OWNERS=5cDdE6F2bE1e5E464749104aF42753D6c8Bff42C,F7dC23CF42AE8Bf1225e9D411E566eA1484554dd
SWAP=b49a187961E310fBEa9071975fAe9F497fD3A938
PREFUND=29E07C0905FECcD3b4a35A0C3Ea86c48cbEa893E
SKIP_V1_VALIDATION=""
PERIOD=""
TIMESTAMP=""
OUT=genesis/genesis.json

while [ $# -gt 0 ]; do
  case "$1" in
    --network) NETWORK=$2; shift 2;;
    --chainid) CHAINID=$2; shift 2;;
    --owner) OWNER=$2; shift 2;;
    --signers) SIGNERS=$2; shift 2;;
    --reward) REWARD=$2; shift 2;;
    --v2block) V2BLOCK=$2; shift 2;;
    --epoch) EPOCH=$2; shift 2;;
    --gap) GAP=$2; shift 2;;
    --foundation) FOUNDATION=$2; shift 2;;
    --foundation-owners) FOUNDATION_OWNERS=$2; shift 2;;
    --team-owners) TEAM_OWNERS=$2; shift 2;;
    --swap) SWAP=$2; shift 2;;
    --prefund) PREFUND=$2; shift 2;;
    --skip-v1-validation) SKIP_V1_VALIDATION=$2; shift 2;;
    --period) PERIOD=$2; shift 2;;
    --timestamp) TIMESTAMP=$2; shift 2;;
    --out) OUT=$2; shift 2;;
    -h|--help) sed -n '2,70p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown flag: $1" >&2; exit 1;;
  esac
done

norm() { echo "$1" | sed -E 's/^0[xX]//; s/^xdc//'; }
emit_list() { local IFS=,; for a in $1; do norm "$a"; done; echo; }  # addresses + blank terminator

cd "$(dirname "$0")" || exit 1
source ./lib.sh
require_oldxdc
ensure_bins
PUPPETH="$PUPPETH_BIN"
rm -rf "$HOME/.puppeth/$NETWORK" "$NETWORK.json"
LOG="$(mktemp "${TMPDIR:-/tmp}/gen-genesis.XXXXXX")" || { echo "gen-genesis.sh: mktemp failed" >&2; exit 1; }

{
  echo "$NETWORK"      # network name
  echo 2               # menu: Configure new genesis
  echo 3               # consensus: XDPoS
  echo                 # block time (default 2)
  echo "$REWARD"       # masternode reward
  echo "$V2BLOCK"      # v2 switch block
  echo; echo; echo     # v2 timeout period / sync threshold / QC proportion (defaults)
  norm "$OWNER"        # masternodes owner
  emit_list "$SIGNERS" # signers + terminator
  echo "$EPOCH"        # blocks per epoch
  echo "$GAP"          # blocks before checkpoint
  norm "$FOUNDATION"   # foundation wallet
  emit_list "$FOUNDATION_OWNERS"
  echo                 # foundation required (default 2)
  emit_list "$TEAM_OWNERS"
  echo                 # team required (default 2)
  norm "$SWAP"         # swap wallet
  emit_list "$PREFUND"
  echo "$CHAINID"      # chain id
  echo 2               # menu: Manage existing genesis
  echo 2               # submenu: Export genesis configuration
  echo                 # file (default <network>.json)
} | "$PUPPETH" >"$LOG" 2>&1

if [ -f "$NETWORK.json" ]; then
  mkdir -p "$(dirname "$OUT")"
  cp "$NETWORK.json" "$OUT"

  # Pin the post-Byzantium EVM fork schedule EXPLICITLY (always, not just for
  # --new networks) instead of leaving constantinopleBlock/petersburgBlock/
  # istanbulBlock/berlinBlock/londonBlock/eip1559Block/cancunBlock absent.
  #
  # Root cause (ItWorksinMyLocal#114 follow-up): puppeth's wizard only prompts
  # for homestead/eip150/eip155/eip158/byzantium, leaving every later fork
  # field nil/absent in the JSON it writes. geth/oldxdc treat "absent" as
  # "this fork never activates" (standard go-ethereum semantics) -- so on an
  # unmodified genesis this network is frozen at Byzantium forever on those
  # clients. erigon-xdc does NOT share that semantics for XDPoS chains: any
  # genesis loaded via SpecFromGenesisFile (i.e. not one of its own hardcoded,
  # registered chainspecs) gets a code-side default profile
  # (execution/chain/spec/config.go:applyDefaultXDPoSForkProfile) that fills
  # every nil fork field with mainnet's own upgrade order -- Constantinople/
  # Petersburg/Istanbul/Berlin/London all at block 0, matching XDC mainnet's
  # historical schedule -- REGARDLESS of what this genesis's JSON says. Since
  # Istanbul raised SLOAD from 200 to 800 gas, this alone was enough to
  # disagree on gasUsed for the very first BlockSigner-contract-write
  # transaction and hard-fork erigon (and besu, same client-side "assume
  # mainnet's own progression" behavior) off this network at whatever block
  # first exercises SLOAD/SSTORE -- diverging even though the genesis HASH
  # itself already matched (chain-config fields are never part of the header,
  # so this was invisible to the genesis-hash gate).
  #
  # Fix: stop relying on ANY client's implicit fallback -- explicitly pin the
  # exact schedule erigon's default profile already assumes, so it becomes a
  # no-op there (defaultForkBlock only touches nil fields) while geth/oldxdc
  # now activate the SAME forks at the SAME blocks instead of never. eip1559Block
  # is computed as v2block-gap (this genesis's own V2 switch minus its own gap)
  # rather than hardcoding erigon's 2250 default, so it stays correct even if
  # --v2block/--gap are customized away from their 2700/450 defaults.
  #
  # londonBlock MUST equal eip1559Block, not 0. They gate the same thing --
  # whether the header carries baseFeePerGas -- but different clients read
  # different keys: oldxdc/geth-xdc honour the XDC-specific eip1559Block,
  # erigon and besu honour the standard londonBlock. With london=0 and
  # eip1559Block=2250 the two disagree AT BLOCK 0: erigon synthesises the
  # initial 1 Gwei baseFee into the genesis header, oldxdc/geth omit it, and
  # that one extra RLP field gives erigon a different genesis hash
  # (0x6c8d0128... vs 0x3857450e...). Every sealer then correctly refuses the
  # handshake and erigon sits at head 0 logging '[p2p] No GoodPeers' forever --
  # which presents as a networking fault, not a genesis one. Do NOT 'fix' this
  # by dropping eip1559Block instead: geth-xdc computes
  # isEIP2929 = c.IsEIP1559(num), falling back to IsLondon when Eip1559Block is
  # nil, so removing it silently moves gas pricing too.
  #
  # TRIED AND REFUTED (ItWorksinMyLocal#128, wuhan/20118, 2026-07-30): the
  # hypothesis that dropping londonBlock/eip1559Block/cancunBlock entirely
  # (leaving them absent, "never activate") would unblock besu/reth without
  # touching the other five clients. It does not hold up -- tested end to end
  # against the live wuhan net and reverted:
  #   - erigon regressed. erigon's applyDefaultXDPoSForkProfile (see above)
  #     fills London=0 REGARDLESS of whether the genesis JSON has the field at
  #     all, same as it does for Constantinople/Petersburg/Istanbul/Berlin --
  #     the original comment already said so ("...Berlin/London all at block
  #     0") but the fix that follows only accounted for it by PINNING london
  #     explicitly, never by testing "absent". With it absent, erigon
  #     synthesised baseFeePerGas into its OWN genesis block (oldxdc/geth do
  #     not), producing a different genesis hash
  #     (0x51b07907...f2b3f vs the sealers' 0xfe463361...5ba93) and permanent
  #     '[p2p] No GoodPeers' -- i.e. exactly the failure mode the paragraph
  #     above already documents for londonBlock=0/eip1559Block=2250, just
  #     triggered by the opposite mistake (absent instead of mismatched).
  #   - nethermind regressed too, for an unrelated reason: its genesis is not
  #     read from this JSON directly but re-derived by netlab/nm-chainspec.sh
  #     into its own chainspec format, and that derivation does not tolerate
  #     london/eip1559/cancun being absent -- nethermind's genesis loader
  #     crashed outright ("Error while generating genesis block",
  #     InvalidStateRoot, expected 0x0c255643... calculated 0x56e81f17...).
  #   - besu was NOT fixed. Its failure moved from block 2251 ("transaction
  #     invalid gasPrice is less than the current BaseFee", i.e. the London/
  #     EIP-1559 boundary) to block 16 ("World State Root does not match
  #     expected value") -- a different, earlier, unrelated state-root bug
  #     that the 2251 failure had simply been masking. Removing the fork
  #     boundary did not fix besu; it just uncovered the next bug in line.
  # Two of the five previously-working (guard) clients broke and neither of
  # the two target clients was fixed, so this was reverted in full -- the
  # schedule below is unchanged from the original #114 fix. Do not re-attempt
  # "drop these fields" without first confirming erigon's default-fill
  # includes londonBlock (it does) and that nethermind reads this file
  # directly (it does not; it goes through nm-chainspec.sh). besu's and
  # reth's remaining divergences need client-side fixes (besu#59, reth's
  # evm.rs work), not a harness change.
  # londonBlock/eip1559Block/cancunBlock: pin to XDC MAINNET's OWN values
  # (unreachable on a private net) rather than to v2block-gap / v2block+gap.
  #
  # Pinning them at a small, REACHABLE block (2250/3150 on the defaults) wedges
  # geth-xdc and xone at the boundary: at eip1559Block, geth-xdc turns on the
  # real 1559 fee market AND EIP-2929 cold-access pricing (it computes
  # isEIP2929 = c.IsEIP1559(num)), while the legacy oldxdc sealers have no such
  # execution path at all regardless of what genesis says. The two then disagree
  # on gasUsed for the BlockSigners sign() tx in the first block past the
  # boundary, so geth/xone reject the sealers' canonical chain with
  # 'invalid merkle root' and sit there forever (measured on wuhan/20118: both
  # frozen at exactly 2250 while every other client tracked the sealers past
  # 2251). besu also failed at 2251 here, on the same boundary, as
  # 'transaction invalid gasPrice is less than the current BaseFee' -- the
  # sealers mine with --gasprice 1, which is below the baseFee once 1559 starts.
  #
  # These three must nonetheless stay PRESENT, not be deleted: erigon's
  # applyDefaultXDPoSForkProfile fills any NIL/absent fork field with mainnet's
  # order -- London at 0 -- so an ABSENT londonBlock makes erigon synthesise a
  # baseFeePerGas into its own genesis header that oldxdc/geth omit, giving
  # erigon a different genesis HASH and permanent '[p2p] No GoodPeers'. That was
  # tried and refuted (see the block above, and #132). A large, non-nil value is
  # what satisfies both constraints at once: non-nil so erigon's defaultForkBlock
  # leaves it alone, unreachable so no client ever activates it mid-chain.
  #
  # Using mainnet's real numbers rather than an arbitrary sentinel keeps this a
  # statement of the actual XDC fork schedule (and matches what XDC's own
  # production configs pin), so a private net is simply a chain that never gets
  # there -- exactly how oldxdc already behaves.
  #
  # Which of Constantinople..Berlin belong at 0 is not a matter of taste -- it is
  # fixed by what the legacy oldxdc SEALERS execute, since they produce the chain
  # every other client must reproduce. Measured on wuhan/20118, on the blocks
  # carrying the 4 BlockSigners sign() txs:
  #
  #   sealers (authoritative)            gasUsed = 354632
  #   nethermind, Byzantium-only         computed  340232   (14400 low)
  #   reth                               computed  347928   ( 6704 low)
  #
  # 14400 = 24 x 600, and 600 is exactly Istanbul's SLOAD repricing (200 -> 800)
  # over the 24 SLOADs those four txs perform. So the sealers run Byzantium PLUS
  # Istanbul's gas schedule, and istanbulBlock therefore MUST stay 0. Moving it out
  # of reach was tried and reverted: nethermind then drops to Byzantium SLOAD
  # pricing, computes 340232, rejects block 16 outright with HeaderGasUsedMismatch,
  # deletes it and stops dead. So "put everything past Byzantium out of reach" is
  # NOT the right shape here, even though netv12/34093 appears to get away with
  # declaring nothing -- there the equivalent values come from each client's own
  # runtime resolution rather than being absent in effect.
  #
  # berlinBlock stays 0 as well. reth's 6704 shortfall looks like EIP-2929
  # (cold 2100 / warm 100 instead of a flat Istanbul 800), which suggests moving
  # berlinBlock would fix it -- it does not. Tested directly: with
  # berlinBlock=76321000 reth still produced the identical 347928/354632 mismatch
  # (at blocks 121/181 instead of 16/91/151/211/271), so reth is not reading this
  # field for its EVM spec at all -- consistent with its hardcoded SpecId in
  # evm_env_for_payload(). reth re-downloads and recovers each time, so it still
  # tracks the sealers to tip, but it re-diverges every ~60 blocks. That is a
  # reth-side bug (reth-xdc evm.rs), not something this schedule can fix, and the
  # same 347928/354632 signature appears on runs predating any change here.
  # berlinBlock=0 is therefore kept as the smaller diff, and because #114 needs it
  # explicit anyway (see above).
  python3 -c "
import json, sys
path = sys.argv[1]
d = json.load(open(path))
cfg = d['config']
cfg.setdefault('constantinopleBlock', 0)
cfg.setdefault('petersburgBlock', 0)
cfg.setdefault('istanbulBlock', 0)
cfg.setdefault('berlinBlock', 0)
cfg.setdefault('londonBlock', 76321000)
cfg.setdefault('eip1559Block', 98800200)
cfg.setdefault('cancunBlock', 98802000)
json.dump(d, open(path, 'w'))
print('pinned post-Byzantium fork schedule (constantinople/petersburg/istanbul/berlin=0; london=%d, eip1559Block=%d, cancunBlock=%d = XDC mainnet values, unreachable on a private net) in %s' % (cfg['londonBlock'], cfg['eip1559Block'], cfg['cancunBlock'], path))
" "$OUT"

  if [ -n "$SKIP_V1_VALIDATION" ]; then
    python3 -c "
import json, sys
path, flag = sys.argv[1], sys.argv[2].strip().lower() in ('1', 'true', 'yes')
d = json.load(open(path))
d['config']['XDPoS']['SkipV1Validation'] = flag
json.dump(d, open(path, 'w'))
print('SkipV1Validation set to', flag, 'in', path)
" "$OUT" "$SKIP_V1_VALIDATION"
  fi
  if [ -n "$PERIOD" ]; then
    python3 -c "
import json, sys
path, period = sys.argv[1], int(sys.argv[2])
d = json.load(open(path))
d['config']['XDPoS']['period'] = period
json.dump(d, open(path, 'w'))
print('period set to', period, 'in', path)
" "$OUT" "$PERIOD"
  fi
  if [ -n "$TIMESTAMP" ]; then
    python3 -c "
import json, sys
path, ts = sys.argv[1], int(sys.argv[2], 0)
d = json.load(open(path))
d['timestamp'] = '0x%x' % ts
json.dump(d, open(path, 'w'))
print('timestamp set to', d['timestamp'], 'in', path)
" "$OUT" "$TIMESTAMP"
  fi
  # Record the role -> address mapping so network-info.sh can label accounts.
  {
    echo "NETWORK=$NETWORK"
    echo "CHAINID=$CHAINID"
    echo "OWNER=$(norm "$OWNER")"
    echo "FOUNDATION=$(norm "$FOUNDATION")"
    echo "FOUNDATION_OWNERS=$FOUNDATION_OWNERS"
    echo "TEAM_OWNERS=$TEAM_OWNERS"
    echo "SWAP=$(norm "$SWAP")"
    echo "PREFUND=$PREFUND"
    echo "SIGNERS=$SIGNERS"
  } > network-roles.env

  # Same mapping as network-roles.env above, but as the per-network JSON that
  # accounts/accounts-<chainid>.json already holds the generated keypairs in --
  # so one file per chain id describes the whole network: every role, its
  # address, its genesis balance, the private key where we have one, the
  # consensus parameters, and the total supply.
  #
  # gen-address.sh writes that file first (signers, with private keys); this
  # merges on top of it and preserves those keys, matching on address. Roles
  # that are plain genesis addresses (owner, foundation, swap, prefund) have no
  # key here and get privateKey: null rather than being silently dropped.
  #
  # Balances are strings, not JSON numbers: the prefunded account alone exceeds
  # 2**53, so a number would be silently rounded by any conformant JSON reader.
  ACCOUNTS_JSON="accounts/accounts-${CHAINID}.json"
  mkdir -p "$(dirname "$ACCOUNTS_JSON")"
  ACCOUNTS_JSON="$ACCOUNTS_JSON" GENESIS_PATH="$OUT" NETWORK="$NETWORK" CHAINID="$CHAINID" \
  OWNER="$(norm "$OWNER")" FOUNDATION="$(norm "$FOUNDATION")" SWAP="$(norm "$SWAP")" \
  FOUNDATION_OWNERS="$FOUNDATION_OWNERS" TEAM_OWNERS="$TEAM_OWNERS" \
  PREFUND="$PREFUND" SIGNERS="$SIGNERS" \
  python3 -c '
import json, os, re

def norm(a):
    return re.sub(r"^(0[xX]|xdc)", "", a.strip()).lower()

def split(v):
    return [norm(x) for x in os.environ.get(v, "").split(",") if x.strip()]

path = os.environ["ACCOUNTS_JSON"]
genesis = json.load(open(os.environ["GENESIS_PATH"]))
alloc = {norm(k): v for k, v in genesis.get("alloc", {}).items()}

# Private keys already generated for this chain id, keyed by address.
keys = {}
if os.path.exists(path):
    try:
        for a in json.load(open(path)).get("accounts", []):
            if a.get("privateKey") and a.get("address"):
                keys[norm(a["address"])] = a["privateKey"]
    except (ValueError, KeyError):
        pass   # unreadable/old format -- rebuild it rather than fail the run

# role -> addresses, in the order they are most useful to a reader.
roles = [("signer", split("SIGNERS")), ("owner", [norm(os.environ["OWNER"])]),
         ("foundation", [norm(os.environ["FOUNDATION"])]),
         ("foundationOwner", split("FOUNDATION_OWNERS")),
         ("teamOwner", split("TEAM_OWNERS")),
         ("swap", [norm(os.environ["SWAP"])]), ("prefund", split("PREFUND"))]

accounts, seen = [], {}
for role, addrs in roles:
    for addr in addrs:
        if not addr:
            continue
        if addr in seen:                      # one address, several roles
            entry = seen[addr]
            if role not in entry["roles"]:
                entry["roles"].append(role)
            continue
        bal = int(alloc.get(addr, {}).get("balance", "0x0"), 16)
        entry = {"roles": [role], "address": "0x" + addr, "xdc": "xdc" + addr,
                 "privateKey": keys.get(addr), "balanceWei": str(bal),
                 "balanceXdc": str(bal // 10**18)}
        seen[addr] = entry
        accounts.append(entry)

# Contracts and any other funded genesis address, so the supply below adds up.
others = []
for addr, v in alloc.items():
    if addr in seen:
        continue
    bal = int(v.get("balance", "0x0"), 16)
    if bal or "code" in v:
        others.append({"address": "0x" + addr, "xdc": "xdc" + addr,
                       "isContract": "code" in v, "balanceWei": str(bal),
                       "balanceXdc": str(bal // 10**18)})
others.sort(key=lambda e: e["address"])

total = sum(int(v.get("balance", "0x0"), 16) for v in alloc.values())
xdpos = genesis.get("config", {}).get("XDPoS", {})
out = {
    "chainId": int(os.environ["CHAINID"]),
    "network": os.environ["NETWORK"],
    "genesis": os.environ["GENESIS_PATH"],
    "consensus": {"period": xdpos.get("period"), "epoch": xdpos.get("epoch"),
                  "gap": xdpos.get("gap"), "reward": xdpos.get("reward"),
                  "v2SwitchBlock": (xdpos.get("v2") or {}).get("switchBlock"),
                  "skipV1Validation": xdpos.get("SkipV1Validation")},
    "totalSupply": {"wei": str(total), "xdc": str(total // 10**18)},
    "accounts": accounts,
    "genesisContractsAndOtherFunded": others,
}
with open(path, "w") as f:
    json.dump(out, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)   # carries private keys -- same as gen-address.sh
n = sum(1 for a in accounts if a["privateKey"])
print("Wrote %d accounts (%d with private keys) + total supply to %s" % (len(accounts), n, path))
'
  # Also keep a per-network-id copy so different chains don't overwrite each
  # other's genesis (matches the genesis/genesis-<id>.json convention). run.sh
  # still boots from $OUT (genesis/genesis.json); this is the archival copy.
  GENESIS_BY_ID="$(dirname "$OUT")/genesis-${CHAINID}.json"
  if [ "$GENESIS_BY_ID" != "$OUT" ]; then
    cp "$OUT" "$GENESIS_BY_ID"
    echo "Saved per-network genesis copy: $GENESIS_BY_ID"
  fi
  echo "OK: $OUT generated (chainId $CHAINID, network '$NETWORK')"
  echo "Reset node data before restart:  rm -rf ./nodes/*/XDC ./nodes/*/XDCx ./nodes/*/keystore"
  rm -f "$LOG"
else
  echo "FAILED: $NETWORK.json not produced. See $LOG" >&2
  exit 1
fi
