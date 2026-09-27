# onboard-net — persistent XDPoS net + runtime masternode onboarding

Reusable scripts to (1) stand up a **persistent oldxdc validator network**, then
(2) **push clients in at runtime** as masternodes via the real XDPoS validator
flow — `uploadKYC()` + `propose()` on the `0x88` XDCValidator contract — so they
seat at the next epoch checkpoint and mint. Extracted from the 7-client mint
program (#62).

## Files
| script | what it does |
|---|---|
| `start-oldxdc.sh` | bootnode + N oldxdc (XDPoSChain) validators from a genesis, `--mine`, on stats. Idempotent. |
| `join-client.sh`  | bring up a modern client (geth-XDC / xone / reth / erigon) that joins the net with producing flags. |
| **`onboard.py`**  | **the onboarding call: `uploadKYC(string)` + `propose(address)` on `0x88`**, offline-signed → `eth_sendRawTransaction` (works when `personal` is disabled). |
| `stop.sh`         | graceful SIGTERM (never SIGKILL an XDPoS validator — it corrupts the state DB). |

## Flow
```bash
export BASE=./onboard-run XDC=/path/oldxdc/XDC BOOTNODE=/path/bootnode \
       GENESIS=$BASE/genesis.json IP=<extip> NETID=34093 N=5 \
       KEYS="<privkey1> ... <privkey5>" \
       STATS="xdc_openscan_stats_2026@stats.xdcindia.com:443"

# 1. start the oldxdc-only net (validators must be seated in genesis extraData + 0x88 SMC)
./start-oldxdc.sh

# 2. bring a client up so it syncs to tip
BIN=/path/geth NAME=geth CAND_KEY=<candidate_privkey> BASEPORT=37060 \
PEERS="<validator enodes,comma-separated>" ./join-client.sh

# 3. onboard it as a masternode (uploadKYC + propose) once it's at tip
RPC=http://127.0.0.1:<node1-rpc> CHAIN_ID=34093 \
CAND_KEY=<candidate_privkey> DEPOSIT_WEI=10000000000000000000000 \
./onboard.py both        # -> uploadKYC, propose(10,000 XDC), isCandidate

# it seats at the next epoch (900) checkpoint and mints on its round-robin turn.
```

## `onboard.py` reference
`0x88` XDCValidator selectors used: `uploadKYC(string)=f5c95125`,
`propose(address)=01267951`, `isCandidate(address)=d51b9e93`.
Commands: `kyc` | `propose` | `both` | `receipt <txhash>` | `iscandidate`.
Config is env-driven (`RPC`, `CHAIN_ID`, `CAND_KEY`, `SMC`, `DEPOSIT_WEI`,
`KYC_HASH`, `GAS_PRICE`). Deps: `pip install eth-account`.

## Client fork/gas parity (important)
A client must compute the **same state root** as the legacy oldxdc validators or
it diverges (`invalid state root`) and can't sync. Use client builds that carry a
**default XDC network config** (legacy-compatible `TIPSigning`), not a per-net
genesis fork ladder and **no per-chainId hardcoding** — see
[XDCIndia/go-ethereum#1339](https://github.com/XDCIndia/go-ethereum/pull/1339)
(default XDC fork profile for unregistered XDPoS chains) and the xone/reth/erigon
equivalents. "Synced to tip" is necessary but not sufficient for minting — some
clients also need producer/sealer fixes (tracked per client).

## Genesis
Provide a `GENESIS` with: chainId, XDPoS `epoch`/`gap`/`period`/`reward`, the
`0x88` XDCValidator contract pre-embedded with the N validators as candidates
(and a modest `minCandidateCap` so clients can `propose`), the validators in
`extraData`, and `SkipV1Validation=false`. For a "keep running" net, prefer
keeping the V2 switch high (V1-only) unless every seated client can produce V2 —
a seated non-V2 producer can trigger the V2 dead-leader round-change halt.
