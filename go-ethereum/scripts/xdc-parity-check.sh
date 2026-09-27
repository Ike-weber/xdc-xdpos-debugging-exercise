#!/usr/bin/env bash
# xdc-parity-check.sh — verify consensus parity between two XDC RPC endpoints.
#
# Compares block headers + per-transaction receipts (normalized for
# non-consensus RPC enrichment fields) for a given list of block numbers.
# Used to certify that a Phase 14.4 v1.17.3-based binary produces
# byte-identical block output to the reference XDC v2.6.8-stable client.
#
# Usage:
#   scripts/xdc-parity-check.sh <ref_url> <test_url> <block> [<block> ...]
#
# Example (mainnet):
#   scripts/xdc-parity-check.sh \
#       https://01.xdcrpc.com http://127.0.0.1:8650 \
#       100000 1000000 50000000 80000000 100000000 102000000 102500000 102800000
#
# Output: PASS/FAIL per check + final tally. Exit code 0 if all pass, 1 otherwise.
#
# Notes:
#   • Compares hash, stateRoot, receiptsRoot, transactionsRoot in the block header.
#   • Compares per-transaction receipts after stripping the `blockTimestamp`
#     field from log entries (newer geth adds it; v2.6.8 doesn't) and the
#     `blobGasUsed` / `blobGasPrice` fields (Cancun additions). These fields
#     are NOT part of the receipts trie, so removing them is consensus-safe.
#   • eth_getBalance is sampled at the given blocks for XDPoS precompile
#     addresses. Skipped (not failed) when both sides return empty (typical
#     for --gcmode=full nodes where historical state is pruned).
#   • eth_getProof is intentionally NOT checked because XDC v2.6.8-stable
#     does not implement that RPC method.
#
# Refs: issue #807 (snap-sync overlay), #740 (v1.17.3 reset).

set -e

if [[ "$#" -lt 3 ]]; then
    echo "Usage: $0 <ref_url> <test_url> <block> [<block> ...]" >&2
    exit 64
fi

REF="$1"
TEST="$2"
shift 2

# XDPoS precompile addresses — always allocated, balance comparison is a
# cheap sanity check when state is still available.
ACCOUNTS=(
    "0x0000000000000000000000000000000000000088"  # BlockSigners
    "0x0000000000000000000000000000000000000089"  # Masternodes
    "0x0000000000000000000000000000000000000092"  # RandomizeSMC
)

rpc() {
    curl -sS -m 12 -X POST -H 'Content-Type: application/json' --data "$2" "$1"
}

# Strip non-consensus RPC enrichment fields from a receipts response and
# emit "<count> <16-hex digest>" so two clients can be compared by content.
normalize_receipts() {
    python3 -c '
import sys, json, hashlib
d = json.load(sys.stdin)
r = d.get("result") or []
for rcpt in r:
    for log in rcpt.get("logs", []):
        log.pop("blockTimestamp", None)
    rcpt.pop("blobGasUsed", None)
    rcpt.pop("blobGasPrice", None)
canonical = json.dumps(r, sort_keys=True, separators=(",", ":"))
print(len(r), hashlib.sha256(canonical.encode()).hexdigest()[:16])
'
}

pass=0
fail=0

for b in "$@"; do
    bx=$(printf '0x%x' "$b")
    echo "=== block $b ($bx) ==="

    # 1. Header parity
    ref=$(rpc "$REF"  "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$bx\",false],\"id\":1}")
    tst=$(rpc "$TEST" "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$bx\",false],\"id\":1}")
    read ref_h ref_sr ref_rr ref_th < <(echo "$ref" | python3 -c '
import sys, json
r = (json.load(sys.stdin).get("result") or {})
print(r.get("hash",""), r.get("stateRoot",""), r.get("receiptsRoot",""), r.get("transactionsRoot",""))')
    read tst_h tst_sr tst_rr tst_th < <(echo "$tst" | python3 -c '
import sys, json
r = (json.load(sys.stdin).get("result") or {})
print(r.get("hash",""), r.get("stateRoot",""), r.get("receiptsRoot",""), r.get("transactionsRoot",""))')

    if [[ "$ref_h" == "$tst_h" && "$ref_sr" == "$tst_sr" && "$ref_rr" == "$tst_rr" && "$ref_th" == "$tst_th" && -n "$ref_h" ]]; then
        echo "  header:   PASS hash=$ref_h"
        pass=$((pass+1))
    else
        echo "  header:   FAIL"
        echo "    ref: h=$ref_h sr=$ref_sr rr=$ref_rr th=$ref_th"
        echo "    tst: h=$tst_h sr=$tst_sr rr=$tst_rr th=$tst_th"
        fail=$((fail+1))
    fi

    # 2. Receipts parity (normalized)
    ref_r=$(rpc "$REF"  "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockReceipts\",\"params\":[\"$bx\"],\"id\":1}")
    tst_r=$(rpc "$TEST" "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockReceipts\",\"params\":[\"$bx\"],\"id\":1}")
    read ref_n ref_dg < <(echo "$ref_r" | normalize_receipts)
    read tst_n tst_dg < <(echo "$tst_r" | normalize_receipts)

    if [[ "$ref_n" == "$tst_n" && "$ref_dg" == "$tst_dg" ]]; then
        echo "  receipts: PASS count=$ref_n digest=$ref_dg"
        pass=$((pass+1))
    else
        echo "  receipts: FAIL ref=$ref_n/$ref_dg tst=$tst_n/$tst_dg"
        fail=$((fail+1))
    fi

    # 3. Balance check for XDPoS precompile addresses
    for acct in "${ACCOUNTS[@]}"; do
        ref_bal=$(rpc "$REF"  "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$acct\",\"$bx\"],\"id\":1}" | python3 -c '
import sys, json
print((json.load(sys.stdin).get("result") or ""))')
        tst_bal=$(rpc "$TEST" "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$acct\",\"$bx\"],\"id\":1}" | python3 -c '
import sys, json
print((json.load(sys.stdin).get("result") or ""))')

        if [[ "$ref_bal" == "$tst_bal" && -n "$ref_bal" ]]; then
            echo "  balance($acct): PASS = $ref_bal"
            pass=$((pass+1))
        elif [[ -z "$ref_bal" && -z "$tst_bal" ]]; then
            echo "  balance($acct): SKIP (state pruned both sides)"
        else
            echo "  balance($acct): FAIL ref=$ref_bal tst=$tst_bal"
            fail=$((fail+1))
        fi
    done
done

echo "===================="
echo "TOTAL: $pass passed, $fail failed"

if (( fail > 0 )); then
    exit 1
fi
