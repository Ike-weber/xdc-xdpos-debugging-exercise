# xdc-conformance — XDC cross-client conformance harness

A runnable companion to [`docs/XDC_CROSS_CLIENT_CONFORMANCE_SPEC.md`](../../docs/XDC_CROSS_CLIENT_CONFORMANCE_SPEC.md).
It validates the spec's consensus claims against a **live XDC node over JSON-RPC**
and acts as a **known-good reference oracle** that a reth / erigon / besu / nethermind
port can diff its own outputs against. It reuses this repo's own `core/types` so the
hashing / QC structures are exactly the canonical ones.

## Build
```
go build -o xdc-conformance ./tools/xdc-conformance/
```

## Gates

| Gate | Spec | What it checks | Status |
|---|---|---|---|
| **b** | Part 4, Part 12-B | Re-derives `keccak256(RLP(header))` and compares to the node-reported `blockHash`. Proves the XDC header field order (`Validators`/`Validator`/`Penalties`) is exact. | ✅ implemented + live-validated |
| **d** | Part 6, Part 12-D | Decodes v2 `extraData` (`0x02` + RLP(round, QC)), recovers every QC signer over `VoteSigHash{ProposedBlockInfo, GapNumber}`, and confirms the in-set signer count meets the **round-keyed `CertThreshold`** from the reference config (`V2.Config(qcRound)` — apothem 0.45 for rounds 0–899,999 then 0.667; mainnet 0.667 throughout). Round-0 QCs are the epoch-switch bootstrap marker and skip the quorum check, matching the engine. | ✅ implemented + live-validated |
| **c** | Part 5, Part 12-C | **Full independent v1 seal recovery.** Re-builds the v1 `sigHash` (`keccak256(RLP(header with the trailing 65-byte seal stripped from Extra))`, replicating `consensus/XDPoS.encodeSigHeader`), `ecrecover`s **M1** (the creator) from `Extra[len-65:]`, and confirms it is in the active masternode set — which is decoded directly from the epoch checkpoint header's `Extra` (`32 vanity + N·20 addrs + 65 seal`), so **no `XDPoS_` RPC namespace is required**. Also checks checkpoint coinbase `0x0` and reports the in-turn slot. **M2** (#951 double-validation, `header.Validator`) is recovered over `HashNoValidator()` and reported informationally only — legacy mainnet producers sign it over a construction `HashNoValidator()` does not reproduce and it is a non-enforced diagnostic. | ✅ implemented + live-validated (6/6 mainnet v1, incl. 2 checkpoints) |
| **e** | Part 12-E | Diffs `stateRoot` / `receiptsRoot` / `gasUsed` of block N between a canonical archive RPC and the client-under-test RPC. | ◐ RPC-diff (full re-execution from pre-state @N-1 is TODO; needs an archive datadir + `core/state`) |
| **f** | Part 8, Part 12-J | **EIP-2124 fork ID (peering parity).** Computes the canonical XDC `ForkID` (`forkHash` + `forkNext`) from the *reference* chain config (`params.XDCMainnetChainConfig` / `XDCApothemChainConfig`) seeded with the live genesis hash + head, via `core/forkid.NewID`. This is the exact `ForkID` a porting client must advertise in its eth `Status` handshake to peer with the XDC fleet — a mismatch is rejected before the XDPOS2/100 subprotocol even negotiates (the literal cause of a devnet node stuck at `peers=0` with an identical genesis). | ✅ implemented + live-validated (mainnet + apothem) |
| **g** | Part 12-E | **State-transition output re-derivation** (self-contained form of gate E). Rebuilds the consensus execution-output commitments from the block's own data instead of trusting the node: `txRoot = DeriveSha(transactions)`, `receiptsRoot = DeriveSha(eth_getBlockReceipts)`, and verifies an `eth_getProof` account proof roots to `header.Root` via `trie.VerifyProof`. txRoot/receiptsRoot work on any full node at any depth; the stateRoot proof works at the tip on a full node and at any depth on an archive node (reports `n/a` where state is pruned). | ✅ implemented + live-validated (apothem full node incl. 31-receipt block; deep mainnet @80M/50M) |

## Usage
```
xdc-conformance b <rpc> <block...>
xdc-conformance d <rpc> <block...>
xdc-conformance c <rpc> <block...>
xdc-conformance e <rpcCanonical> <rpcUnderTest> <block...>
xdc-conformance f <rpc>
```
`<block>` = decimal, `0x`-hex, or `latest`. Exit code is non-zero if any checked block fails.

## Example (live, 2026-06-21)
```
# Gate B — mainnet (v1, v1-checkpoint w/ 21 validators, two v2):
$ xdc-conformance b https://rpc.xinfin.network 80000000 80369100 80370321 83886080
80000000  num=80000000  PASS derived=0x5f64…f950 canonical=0x5f64…f950 |v=0B  val=65B p=0B|
80369100  num=80369100  PASS derived=0xa30d…0e99 canonical=0xa30d…0e99 |v=420B val=65B p=0B|   # checkpoint
80370321  num=80370321  PASS …                                                                 # first v2 blocks
83886080  num=83886080  PASS …

# Gate D — apothem v2 QC quorum:
$ xdc-conformance d https://rpc.apothem.network 83356685 83356015 83349535
83356685  round=27129141 PASS qcSigs=22 distinctSigners=22 inMasternodeSet=22/32 threshold=22
83356015  round=27128432 PASS qcSigs=21 distinctSigners=21 inMasternodeSet=21/31 threshold=21
83349535  round=27121758 PASS qcSigs=23 distinctSigners=23 inMasternodeSet=23/33 threshold=23

# Gate C — mainnet v1 independent seal recovery (M1 creator ∈ masternode set):
$ xdc-conformance c https://rpc.xinfin.network 79999200 80369100 80000000 12345 5000001
79999200  num=79999200  PASS ckpt=true  diff=105 M1=0x63443fFd(in=true,inturn=false) M2=0x71aCFC75(in=false,info) mns=105@cp79999200
80369100  num=80369100  PASS ckpt=true  diff=105 M1=0x450d714e(in=true,inturn=false) M2=0x989d4128(in=false,info) mns=105@cp80369100
80000000  num=80000000  PASS ckpt=false diff=105 M1=0x360B9E58(in=true,inturn=false) M2=0x6086BC24(in=false,info) mns=105@cp79999200
12345     num=12345     PASS ckpt=false diff=9   M1=0x9a378768(in=true,inturn=false) M2=0xD868c20b(in=false,info) mns=9@cp11700
5000001   num=5000001   PASS ckpt=false diff=18  M1=0x065551F0(in=true,inturn=false) M2=0x449533e8(in=false,info) mns=18@cp4999500

# Gate F — canonical fork ID a porting client must advertise to peer:
$ xdc-conformance f https://rpc.xinfin.network
xdc-mainnet(50)  genesis=0x4a9d748b…42d6b1
  forkID @head = forkHash=0x44f52682 forkNext=0

# Gate G — re-derive txRoot/receiptsRoot from block data + verify stateRoot via eth_getProof:
$ xdc-conformance g <apothem-full-node> 83362981          # tip-window block, state available
83362981  num=83362981  PASS txRoot[true] receiptsRoot[true](n=31) stateRoot-proof[OK]
$ xdc-conformance g https://rpc.xinfin.network 80000000   # deep history: tx/receipts re-derive; state pruned
80000000  num=80000000  PASS txRoot[true] receiptsRoot[true](n=3) stateRoot-proof[n/a]
```

## Endpoint notes
- **Gate D needs the `XDPoS` RPC namespace.** Canonical fleet nodes expose `XDPoS_getMasternodes`; some public gateways expose only `XDPoS_getMasternodesByNumber` — the harness tries both.
- **Gate E** needs an archive node for the canonical side (historical state). Public gateways are pruned for deep mid-history.
- **Gate G** `txRoot`/`receiptsRoot` checks need `eth_getBlockReceipts` (any full node, any depth). The `stateRoot` proof needs `eth_getProof`: served by the fleet full/archive nodes (e.g. apothem `XDPoS`-enabled full nodes serve it at the tip), **not** by the public mainnet gateway. For deep-history stateRoot proofs, point gate G at an archive node that exposes `eth_getProof`.

## Known caveat (`#978`)
`core/types/gen_header_json.go` omits the `Validators`/`Validator`/`Penalties` JSON fields, so unmarshalling an `eth_getBlockByNumber` result straight into `types.Header` zeroes them and gate B fails for the wrong reason. This harness re-populates those three fields from the raw JSON as a workaround. Fixing the generated codec is tracked separately.

## How a porting team uses this
1. Run gates b/c/d/e against the live network to see the **expected** (canonical) values.
2. Point gate **e** at your client's RPC (`rpcUnderTest`) to diff state roots/receipts/gasUsed block-by-block.
3. Reproduce the gate logic inside your client's test suite (header hashing, QC quorum, reward/penalty) per the spec's Part 12 PASS criteria — this binary is the oracle.
