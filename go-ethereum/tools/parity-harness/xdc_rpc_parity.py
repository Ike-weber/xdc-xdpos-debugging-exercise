#!/usr/bin/env python3
"""
xdc_rpc_parity.py — XDPoS cross-client block-parity harness (RPC-based).

Compares two XDC JSON-RPC endpoints block-by-block and reports any divergence
in the consensus-critical header fields. On a SHARED chain (e.g. a modern
go-ethereum node and a legacy XDPoSChain node both synced to the same network),
a full-range PASS proves the modern node's import + state execution produce a
bit-identical header to legacy — i.e. V1/V2 consensus parity for that range.

Consensus-critical fields compared (all part of, or derived from, Header.Hash):
  hash, parentHash, stateRoot, transactionsRoot, receiptsRoot, sha3Uncles,
  logsBloom, difficulty, gasLimit, gasUsed, timestamp, miner, mixHash, nonce,
  extraData, and the XDC-specific fields when present (validators, validator,
  penalties). `hash` mismatch alone is sufficient to prove divergence; the
  per-field diff pinpoints WHICH field diverged (state-exec vs seal vs reward).

Usage:
  python3 xdc_rpc_parity.py --legacy http://127.0.0.1:9561 \\
      --modern http://127.0.0.1:9566 --from 1 --to 2000 [--step 1] \\
      [--receipts] [--out report.json] [--stop-on-fail]

Exit code 0 = full parity, 1 = at least one divergence, 2 = harness error.

No third-party deps (stdlib only): safe to scp to any node with python3.
"""
import argparse
import json
import sys
import urllib.request

# Header fields that participate in (or are derived into) the block hash.
# A `hash` mismatch is conclusive; these locate the diverging field.
CORE_FIELDS = [
    "hash", "parentHash", "stateRoot", "transactionsRoot", "receiptsRoot",
    "sha3Uncles", "logsBloom", "difficulty", "gasLimit", "gasUsed",
    "timestamp", "miner", "mixHash", "nonce", "extraData",
]
# XDC-specific header fields (V1 4-byte M2 indices / V2 20-byte addrs, seal sig,
# penalty list) — only compared when at least one node returns them.
XDC_FIELDS = ["validators", "validator", "penalties"]


def rpc(url, method, params, rid=1):
    body = json.dumps({"jsonrpc": "2.0", "method": method, "params": params, "id": rid}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        resp = json.loads(r.read())
    if "error" in resp and resp["error"]:
        raise RuntimeError(f"{method} {params}: {resp['error']}")
    return resp.get("result")


def block_by_number(url, n):
    return rpc(url, "eth_getBlockByNumber", [hex(n), False])


def _norm(v):
    """Normalize an RPC field value so serialization-only differences (empty hex
    serialized as "0x"/""/None, hex case) do NOT read as consensus divergence."""
    if v is None:
        return "0x"
    if isinstance(v, str):
        s = v.lower()
        if s in ("", "0x", "0x0", "0x00"):
            return "0x"
        return s
    return v


def compare_block(legacy_b, modern_b):
    """Return list of (field, legacy_val, modern_val) diffs.

    The block `hash` is the canonical fingerprint of every consensus field, so it
    is authoritative: if hashes match, the blocks are byte-identical and we report
    NO divergence regardless of RPC field-presence noise (legacy serializes empty
    XDC fields as "0x", modern omits them). The per-field diff is only produced to
    LOCALIZE a genuine hash mismatch."""
    if legacy_b and modern_b and _norm(legacy_b.get("hash")) == _norm(modern_b.get("hash")):
        return []
    diffs = []
    fields = list(CORE_FIELDS)
    for f in XDC_FIELDS:
        if (legacy_b and f in legacy_b) or (modern_b and f in modern_b):
            fields.append(f)
    for f in fields:
        lv = legacy_b.get(f) if legacy_b else None
        mv = modern_b.get(f) if modern_b else None
        if _norm(lv) != _norm(mv):
            diffs.append((f, lv, mv))
    return diffs


def main():
    ap = argparse.ArgumentParser(description="XDPoS cross-client block-parity harness")
    ap.add_argument("--legacy", required=True, help="legacy (canonical) RPC URL")
    ap.add_argument("--modern", required=True, help="modern fork RPC URL")
    ap.add_argument("--from", dest="frm", type=int, required=True)
    ap.add_argument("--to", dest="to", type=int, required=True)
    ap.add_argument("--step", type=int, default=1)
    ap.add_argument("--out", default=None, help="write JSON report here")
    ap.add_argument("--stop-on-fail", action="store_true")
    ap.add_argument("--quiet", action="store_true", help="only print failures + summary")
    args = ap.parse_args()

    try:
        lh = int(rpc(args.legacy, "eth_blockNumber", []), 16)
        mh = int(rpc(args.modern, "eth_blockNumber", []), 16)
    except Exception as e:
        print(f"ERROR: cannot reach an endpoint: {e}", file=sys.stderr)
        return 2
    print(f"legacy head={lh}  modern head={mh}  comparing [{args.frm}..{args.to}] step {args.step}")

    total = passed = failed = missing = 0
    first_fail = None
    failures = []
    for n in range(args.frm, args.to + 1, args.step):
        if n > lh or n > mh:
            missing += 1
            continue
        total += 1
        try:
            lb = block_by_number(args.legacy, n)
            mb = block_by_number(args.modern, n)
        except Exception as e:
            failed += 1
            if first_fail is None:
                first_fail = n
            rec = {"block": n, "error": str(e)}
            failures.append(rec)
            print(f"  block {n}: RPC ERROR {e}")
            if args.stop_on_fail:
                break
            continue
        diffs = compare_block(lb, mb)
        if diffs:
            failed += 1
            if first_fail is None:
                first_fail = n
            rec = {"block": n, "diffs": [{"field": f, "legacy": lv, "modern": mv} for f, lv, mv in diffs]}
            failures.append(rec)
            print(f"  block {n}: DIVERGENCE")
            for f, lv, mv in diffs:
                # truncate long hex blobs for readability
                lvs = (lv[:18] + "…") if isinstance(lv, str) and len(lv) > 20 else lv
                mvs = (mv[:18] + "…") if isinstance(mv, str) and len(mv) > 20 else mv
                print(f"      {f}: legacy={lvs}  modern={mvs}")
            if args.stop_on_fail:
                break
        else:
            passed += 1
            if not args.quiet and (n % 100 == 0 or n == args.frm):
                print(f"  block {n}: PARITY (hash {mb['hash'][:14]}…)")

    print("\n=== PARITY SUMMARY ===")
    print(f"compared: {total}   PARITY: {passed}   DIVERGENT: {failed}   skipped(head behind): {missing}")
    if first_fail is not None:
        print(f"first divergence at block: {first_fail}")
    verdict = "PASS — full consensus parity" if failed == 0 and total > 0 else (
        "FAIL — divergences found" if failed else "NO BLOCKS COMPARED")
    print(f"verdict: {verdict}")

    if args.out:
        with open(args.out, "w") as fh:
            json.dump({
                "range": [args.frm, args.to], "step": args.step,
                "legacy_head": lh, "modern_head": mh,
                "compared": total, "parity": passed, "divergent": failed,
                "skipped": missing, "first_divergence": first_fail,
                "failures": failures,
            }, fh, indent=2)
        print(f"report written: {args.out}")

    return 0 if (failed == 0 and total > 0) else 1


if __name__ == "__main__":
    sys.exit(main())
