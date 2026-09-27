# A.97-M.C — XDPoS V2 Devnet Mint Gate (3-node cross-client harness)

Tests that a block minted by our geth (feat/m-xdpos-worker) is consensus-valid
and accepted by XDPoSChain peers, and that XDPoSChain blocks are accepted by us.

## Gate summary

| Gate | Criticality | Test |
|------|-------------|------|
| G1 | P0 reward-root | Chain advances ≥2 epochs; ZERO `invalid merkle root`/`BAD BLOCK` |
| G2 | P0 cross-client | XDPoSChain (node3) accepts our blocks; we accept theirs |
| G3 | P0 BFT closure | Vote dispatch, QC formation, round advancement, blocks from BOTH our nodes |
| G4 | P1 equivocation | No double-mint; node1 stop/start without panic |

## Quick start (on xdc03: 65.21.27.213 port 12141)

```bash
# 1. Build our geth from feat/m-devnet-gate
cd /root/workspace/go-ethereum-a97m
git fetch origin feat/m-devnet-gate
git checkout feat/m-devnet-gate
make geth
OUR_GETH=/root/workspace/go-ethereum-a97m/build/bin/geth

# 2. Build XDPoSChain XDC binary (reference client, v2.7.0)
cd /root/workspace/XDPoSChain
git checkout main
make XDC
XDC_GETH=/root/workspace/XDPoSChain/build/bin/XDC

# 3. Generate keys
export OUT_DIR=/root/devnet-a97m
export GETH=$OUR_GETH
bash scripts/devnet-mint/01-gen-keys.sh

# 4. Generate genesis (V2-at-0, epoch=90, gap=45)
source $OUT_DIR/keys.env
bash scripts/devnet-mint/02-gen-genesis.sh

# 5. Launch 3 nodes
OUR_GETH=$OUR_GETH XDC_GETH=$XDC_GETH OUT_DIR=$OUT_DIR \
    bash scripts/devnet-mint/03-launch-nodes.sh

# 6. Run gates (wait ~400s for 2 epochs)
OUR_GETH=$OUR_GETH XDC_GETH=$XDC_GETH OUT_DIR=$OUT_DIR \
    bash scripts/devnet-mint/04-run-gates.sh
```

## Node topology

| Node | Client | Key | RPC | p2p | IPC |
|------|--------|-----|-----|-----|-----|
| node1 | feat/m-devnet-gate (ours) | ADDR1 | 9551 | 31301 | $OUT_DIR/node1/XDC.ipc |
| node2 | feat/m-devnet-gate (ours) | ADDR2 | 9552 | 31302 | $OUT_DIR/node2/XDC.ipc |
| node3 | XDPoSChain v2.7.0 (reference) | ADDR3 | 9553 | 31303 | $OUT_DIR/node3/XDC.ipc |

## Genesis parameters

- chainId: 551 (maps to devnet551Constants: V2 from genesis, all TIPs=0)
- epoch: 90, gap: 45 (fast epoch switches: every ~180s at 2s/block)
- V2 switchBlock: 0 (V2 from genesis — no V1 phase)
- 3 masternodes in genesis extraData + alloc

## Relaunch commands

```bash
# If nodes die, relaunch:
source /root/devnet-a97m/keys.env
OUR_GETH=/root/workspace/go-ethereum-a97m/build/bin/geth
XDC_GETH=/root/workspace/XDPoSChain/build/bin/geth
OUT_DIR=/root/devnet-a97m
OUR_GETH=$OUR_GETH XDC_GETH=$XDC_GETH OUT_DIR=$OUT_DIR \
    bash scripts/devnet-mint/03-launch-nodes.sh
```

## Files

- `01-gen-keys.sh` — generate 3 throwaway masternode keys
- `02-gen-genesis.sh` — generate V2-at-0 genesis JSON
- `03-launch-nodes.sh` — init datadirs, launch nodes, wire peers
- `04-run-gates.sh` — run G1-G4 gates, capture evidence, exit 0 on P0 pass
