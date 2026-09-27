#!/usr/bin/env python3
"""
onboard.py — enroll a client as an XDPoS masternode candidate at RUNTIME via the
0x88 XDCValidator smart contract: uploadKYC(string) + propose(address).

Why raw txs: the legacy XDPoSChain (oldxdc) RPC has the `personal` namespace
disabled, so we sign OFFLINE with the candidate's private key and submit via
eth_sendRawTransaction. Works against any XDPoS node (oldxdc / geth-XDC / xone /
reth / erigon) that exposes eth_* JSON-RPC.

Usage:
  onboard.py kyc                 # send uploadKYC(<KYC_HASH>)
  onboard.py propose             # send propose(<CAND>) with <DEPOSIT_WEI> value
  onboard.py both                # kyc, wait for receipt, then propose
  onboard.py receipt <txhash>    # fetch a receipt
  onboard.py iscandidate         # eth_call isCandidate(<CAND>) -> bool

Config via env (all optional except CAND_KEY):
  RPC          JSON-RPC endpoint            (default http://127.0.0.1:8545)
  CHAIN_ID     numeric chain id             (default 34093)
  CAND_KEY     candidate private key (hex, no 0x)   [REQUIRED]
  CAND         candidate address (checked against CAND_KEY if given)
  SMC          validator contract           (default 0x...0088)
  DEPOSIT_WEI  propose() stake in wei       (default 10000e18 = 10,000 XDC)
  KYC_HASH     KYC document ref string      (default "kyc://<CAND>")
  GAS_PRICE    wei                          (default 250000000)

Deps: eth_account  (pip install eth-account)
"""
import json, os, sys, time, urllib.request
from eth_account import Account

RPC        = os.environ.get("RPC", "http://127.0.0.1:8545")
CHAIN_ID   = int(os.environ.get("CHAIN_ID", "34093"))
KEY        = os.environ["CAND_KEY"].replace("0x", "")
SMC        = os.environ.get("SMC", "0x0000000000000000000000000000000000000088")
DEPOSIT    = int(float(os.environ.get("DEPOSIT_WEI", str(10000 * 10**18))))
GAS_PRICE  = int(os.environ.get("GAS_PRICE", "250000000"))

# XDCValidator (0x88) 4-byte selectors
SEL_UPLOAD_KYC  = "f5c95125"   # uploadKYC(string)
SEL_PROPOSE     = "01267951"   # propose(address)
SEL_ISCANDIDATE = "d51b9e93"   # isCandidate(address) -> bool

acct = Account.from_key(KEY)
CAND = os.environ.get("CAND", acct.address).lower()
assert acct.address.lower() == CAND, f"CAND {CAND} != key address {acct.address}"
KYC_HASH = os.environ.get("KYC_HASH", f"kyc://{CAND}")


def rpc(method, params):
    req = {"jsonrpc": "2.0", "method": method, "params": params, "id": 1}
    r = urllib.request.urlopen(urllib.request.Request(
        RPC, data=json.dumps(req).encode(),
        headers={"Content-Type": "application/json"}), timeout=15)
    out = json.loads(r.read())
    if "error" in out:
        raise RuntimeError(f"{method}: {out['error']}")
    return out["result"]


def enc_addr(a):
    return a.lower().replace("0x", "").rjust(64, "0")


def enc_string(s):
    b = s.encode()
    pad = ((len(b) + 31) // 32) * 32
    return format(0x20, "064x") + format(len(b), "064x") + b.hex().ljust(pad * 2, "0")


def send(data_hex, value, gas):
    nonce = int(rpc("eth_getTransactionCount", [CAND, "pending"]), 16)
    tx = {"nonce": nonce, "to": SMC, "value": value, "gas": gas,
          "gasPrice": GAS_PRICE, "data": "0x" + data_hex, "chainId": CHAIN_ID}
    signed = Account.sign_transaction(tx, KEY)
    raw = signed.raw_transaction.hex()
    if not raw.startswith("0x"):
        raw = "0x" + raw
    return rpc("eth_sendRawTransaction", [raw])


def wait_receipt(txh, timeout=120):
    end = time.time() + timeout
    while time.time() < end:
        r = rpc("eth_getTransactionReceipt", [txh])
        if r:
            ok = int(r.get("status", "0x0"), 16) == 1
            print(f"  receipt {txh} status={'OK' if ok else 'FAIL'} block={int(r['blockNumber'],16)}")
            return ok
        time.sleep(2)
    raise TimeoutError(f"no receipt for {txh}")


def kyc():
    txh = send(SEL_UPLOAD_KYC + enc_string(KYC_HASH), 0, 300000)
    print(f"uploadKYC({KYC_HASH!r}) -> {txh}")
    return txh


def propose():
    txh = send(SEL_PROPOSE + enc_addr(CAND), DEPOSIT, 3000000)
    print(f"propose({CAND}) value={DEPOSIT} -> {txh}")
    return txh


def iscandidate():
    res = rpc("eth_call", [{"to": SMC, "data": "0x" + SEL_ISCANDIDATE + enc_addr(CAND)}, "latest"])
    val = int(res, 16) == 1
    print(f"isCandidate({CAND}) = {val}")
    return val


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "both"
    if cmd == "kyc":
        kyc()
    elif cmd == "propose":
        propose()
    elif cmd == "both":
        wait_receipt(kyc())
        wait_receipt(propose())
        iscandidate()
    elif cmd == "receipt":
        print(json.dumps(rpc("eth_getTransactionReceipt", [sys.argv[2]]), indent=2))
    elif cmd == "iscandidate":
        iscandidate()
    else:
        sys.exit(__doc__)
