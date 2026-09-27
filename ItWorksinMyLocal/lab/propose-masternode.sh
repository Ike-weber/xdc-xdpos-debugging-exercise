#!/bin/bash
# propose-masternode.sh -- onboard a NEW masternode candidate using the keys
# this repo already stores, and record the new accounts back into that store.
#
# WHY THIS EXISTS, next to lab/onboard-masternode.sh
#
# onboard-masternode.sh drives the same 0x88 flow, but every write goes through
# lab/masternode.sh, which sends eth_sendTransaction after unlocking the sender
# with personal_importRawKey + personal_unlockAccount. The published oldxdc
# 2.7.1-stable build does NOT register the `personal` namespace at all -- not
# over HTTP, not over IPC, and not in the console (the JS object exists but the
# method behind it 404s: "the method personal_importRawKey does not exist").
# So that flow cannot run against a stock local net (ItWorksinMyLocal#188).
#
# This script signs LOCALLY and submits eth_sendRawTransaction, which needs no
# unlocked account and works against every client. It also closes the two gaps
# that made the old flow awkward to use:
#
#   - the funder key is read from accounts/accounts-<chainId>.json, the file
#     setup.sh already writes, instead of being pasted on stdin;
#   - the generated owner+coinbase are written BACK into that same file, so the
#     network's key store stays the single source of truth. Previously they
#     landed in a separate --keyfile that nothing else knew about.
#
# Each run generates a FRESH owner and a FRESH coinbase -- never reused across
# masternodes. The owner is the account that stakes and controls the candidate;
# the coinbase is the address seated into the validator set. Keeping them
# distinct (and distinct per node) is what lets one owner be slashed/resigned
# without touching another node's identity.
#
# Usage
#   ./propose-masternode.sh --rpc URL [opts]
#   ./propose-masternode.sh --rpc URL --dry-run
#
#   --rpc URL            JSON-RPC endpoint (default http://localhost:8545)
#   --accounts FILE      key store (default: accounts/accounts-<chainId>.json,
#                        chainId read from the chain)
#   --funder ADDR        which stored account pays. Default: the account with
#                        role "prefund", else the richest one holding a key.
#   --label NAME         suffix for the recorded roles, e.g. --label mn2 records
#                        roles ["masternodeOwner","mn2"]. Default: mn<N>.
#   --stake XDC          stake sent with propose(). Default: live
#                        minCandidateCap() from the contract.
#   --headroom XDC       extra funding on top of the stake, for gas (default 10)
#   --dry-run            show what would happen; no keys generated, no tx sent,
#                        no file written.
#
# Requires python3 with `eth_account` (pip install eth-account) for local
# signing. Everything else is stdlib. The script checks up front and says so.

set -euo pipefail
cd "$(dirname "$0")/.."

RPC="http://localhost:8545"; ACCOUNTS=""; FUNDER=""; LABEL=""; STAKE=""; HEADROOM="10"; DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --rpc) RPC=$2; shift 2;;
    --accounts) ACCOUNTS=$2; shift 2;;
    --funder) FUNDER=$2; shift 2;;
    --label) LABEL=$2; shift 2;;
    --stake) STAKE=$2; shift 2;;
    --headroom) HEADROOM=$2; shift 2;;
    --dry-run) DRY=1; shift;;
    -h|--help) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "propose-masternode.sh: unknown flag $1" >&2; exit 1;;
  esac
done

python3 - "$RPC" "$ACCOUNTS" "$FUNDER" "$LABEL" "$STAKE" "$HEADROOM" "$DRY" <<'PY'
import json, sys, os, time, urllib.request, secrets

RPC, ACCOUNTS, FUNDER, LABEL, STAKE, HEADROOM, DRY = sys.argv[1:8]
DRY = DRY == "1"

try:
    from eth_account import Account
except ImportError:
    sys.exit("propose-masternode.sh: needs python3 `eth_account` for local signing.\n"
             "  pip3 install eth-account\n"
             "  (the node's `personal` namespace is not available -- see #188 --\n"
             "   so transactions must be signed here rather than on the node.)")

VALIDATOR = "0x0000000000000000000000000000000000000088"
SEL_PROPOSE, SEL_UPLOAD_KYC, SEL_IS_CANDIDATE = "01267951", "f5c95125", "d51b9e93"
SEL_MIN_CAP = "d51b9e93"  # placeholder, real min cap read via minCandidateCap below

def rpc(method, params):
    req = urllib.request.Request(RPC,
        data=json.dumps({"jsonrpc": "2.0", "method": method, "params": params, "id": 1}).encode(),
        headers={"Content-Type": "application/json"})
    d = json.load(urllib.request.urlopen(req, timeout=20))
    if "error" in d:
        raise SystemExit(f"propose-masternode.sh: RPC {method} failed: {d['error']}")
    return d["result"]

def call088(data):
    return rpc("eth_call", [{"to": VALIDATOR, "data": "0x" + data}, "latest"])

chain_id = int(rpc("eth_chainId", []), 16)
tip = int(rpc("eth_blockNumber", []), 16)

if not ACCOUNTS:
    ACCOUNTS = f"accounts/accounts-{chain_id}.json"
if not os.path.exists(ACCOUNTS):
    raise SystemExit(f"propose-masternode.sh: no key store at {ACCOUNTS} "
                     f"(setup.sh writes it; pass --accounts to point elsewhere)")

store = json.load(open(ACCOUNTS))
accounts = store.get("accounts", [])
have_key = [a for a in accounts if a.get("privateKey")]
if not have_key:
    raise SystemExit(f"propose-masternode.sh: {ACCOUNTS} holds no private keys")

# --- pick the funder -------------------------------------------------------
def balance_of(addr):
    return int(rpc("eth_getBalance", [addr, "latest"]), 16)

if FUNDER:
    cand = [a for a in have_key if a["address"].lower() == FUNDER.lower()]
    if not cand:
        raise SystemExit(f"propose-masternode.sh: --funder {FUNDER} has no stored key in {ACCOUNTS}")
    funder = cand[0]
else:
    prefund = [a for a in have_key if "prefund" in (a.get("roles") or [])]
    pool = prefund or have_key
    funder = max(pool, key=lambda a: balance_of(a["address"]))

fbal = balance_of(funder["address"])

# --- stake + funding math --------------------------------------------------
if STAKE:
    stake_wei = int(float(STAKE) * 10**18)
else:
    # minCandidateCap() -> uint256
    raw = call088("d55b7dff")
    stake_wei = int(raw, 16) if raw and raw != "0x" else 10**25
head_wei = int(float(HEADROOM) * 10**18)
need_wei = stake_wei + head_wei

# --- label / roles ---------------------------------------------------------
existing_mn = [a for a in accounts if "masternodeOwner" in (a.get("roles") or [])]
label = LABEL or f"mn{len(existing_mn) + 1}"
if any(label in (a.get("roles") or []) for a in accounts):
    raise SystemExit(f"propose-masternode.sh: label '{label}' already recorded in {ACCOUNTS}; pass a different --label")

print(f"== propose-masternode ==")
print(f"  chainId    : {chain_id}   tip {tip}")
print(f"  key store  : {ACCOUNTS}   ({len(accounts)} accounts, {len(have_key)} with keys)")
print(f"  funder     : {funder['address']}  roles={funder.get('roles')}  balance={fbal/1e18:,.0f} XDC")
print(f"  stake      : {stake_wei/1e18:,.0f} XDC  (+{head_wei/1e18:,.0f} gas headroom)")
print(f"  label      : {label}")

if fbal < need_wei:
    raise SystemExit(f"propose-masternode.sh: funder holds {fbal/1e18:,.0f} XDC, needs {need_wei/1e18:,.0f}")

if DRY:
    print("  DRY RUN -- no keys generated, no transactions sent, no file written.")
    raise SystemExit(0)

# --- generate the new identities ------------------------------------------
owner = Account.create()
coinbase = Account.create()
print(f"  new owner    : {owner.address}")
print(f"  new coinbase : {coinbase.address}")

fkey = funder["privateKey"]
fkey = fkey if fkey.startswith("0x") else "0x" + fkey
facct = Account.from_key(fkey)
if facct.address.lower() != funder["address"].lower():
    raise SystemExit("propose-masternode.sh: stored privateKey does not derive the stored address -- refusing to send")

gas_price = max(int(rpc("eth_gasPrice", []), 16), 1)

def send(acct, to, value=0, data="", gas=None):
    nonce = int(rpc("eth_getTransactionCount", [acct.address, "pending"]), 16)
    tx = {"nonce": nonce, "gasPrice": gas_price, "gas": gas or 500000,
          "to": to, "value": value, "chainId": chain_id}
    if data:
        tx["data"] = data if data.startswith("0x") else "0x" + data
    signed = acct.sign_transaction(tx)
    raw = signed.raw_transaction if hasattr(signed, "raw_transaction") else signed.rawTransaction
    return rpc("eth_sendRawTransaction", ["0x" + raw.hex().lstrip("0x")])

def wait(txh, what):
    for _ in range(40):
        r = rpc("eth_getTransactionReceipt", [txh])
        if r:
            blk, st = int(r["blockNumber"], 16), r["status"]
            if st != "0x1":
                raise SystemExit(f"propose-masternode.sh: {what} REVERTED in block {blk} (tx {txh})")
            print(f"  {what}: mined block {blk}")
            return r
        time.sleep(3)
    raise SystemExit(f"propose-masternode.sh: {what} not mined within timeout (tx {txh})")

# 1. fund the owner (stake + gas)
print("  funding the owner ...")
wait(send(facct, owner.address, value=need_wei, gas=21000), "fund owner")

# 2. uploadKYC -- propose() carries onlyKYCWhitelisted and reverts, with no
#    revert reason over RPC, if the owner has never called this.
kyc = f"kyc-{label}"
kb = kyc.encode()
payload = (SEL_UPLOAD_KYC
           + f"{32:064x}"
           + f"{len(kb):064x}"
           + kb.hex() + "00" * ((32 - len(kb) % 32) % 32))
print("  uploadKYC ...")
wait(send(owner, VALIDATOR, data=payload, gas=1000000), "uploadKYC")

# 3. propose(coinbase) with the stake attached
print("  propose ...")
wait(send(owner, VALIDATOR, value=stake_wei,
          data=SEL_PROPOSE + coinbase.address[2:].lower().rjust(64, "0"), gas=1000000), "propose")

# 4. verify on-chain
is_cand = call088(SEL_IS_CANDIDATE + coinbase.address[2:].lower().rjust(64, "0"))
seated = int(is_cand, 16) == 1 if is_cand and is_cand != "0x" else False
print(f"  isCandidate({coinbase.address}) = {seated}")
if not seated:
    raise SystemExit("propose-masternode.sh: propose() mined but the coinbase is not a candidate -- not recording it")

# 5. record BOTH new accounts back into the key store
def entry(acct, role):
    return {"roles": [role, label],
            "address": acct.address.lower(),
            "xdc": "xdc" + acct.address[2:].lower(),
            "privateKey": acct.key.hex(),
            "balanceWei": "0", "balanceXdc": "0",
            "note": f"masternode {label}, proposed at block {tip} on chainId {chain_id}"}

store["accounts"].append(entry(owner, "masternodeOwner"))
store["accounts"].append(entry(coinbase, "masternodeCoinbase"))

tmp = ACCOUNTS + ".tmp"
with open(tmp, "w") as f:
    json.dump(store, f, indent=2)
    f.write("\n")
os.chmod(tmp, 0o600)
os.replace(tmp, ACCOUNTS)
print(f"  recorded owner + coinbase in {ACCOUNTS} (roles: masternodeOwner/{label}, masternodeCoinbase/{label})")

# 6. seating math -- a candidate is only seated at an epoch switch, and the set
#    is snapshotted Gap blocks before the START of that epoch.
try:
    info = rpc("XDPoS_networkInformation", [])
    epoch = int(info.get("Epoch", 900)); gap = int(info.get("Gap", 450))
except Exception:
    epoch, gap = 900, 450
tip2 = int(rpc("eth_blockNumber", []), 16)
nxt = ((tip2 // epoch) + 1) * epoch
deadline = nxt - gap
if tip2 > deadline:
    nxt += epoch; deadline = nxt - gap
print(f"  seating: Epoch={epoch} Gap={gap}; proposed at ~{tip2}, deadline {deadline}, "
      f"seated at switch {nxt} ({nxt - tip2} blocks away)")
print(f"  watch:  ./lab/masternode.sh list --rpc {RPC}")
PY
