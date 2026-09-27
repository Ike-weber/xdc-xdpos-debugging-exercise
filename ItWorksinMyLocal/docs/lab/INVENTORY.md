# XDPoS Consensus Lab — Worker Inventory

> Worker inventory feeding `docs/lab/DESIGN.md`. Read-only findings from `ItWorksinMyLocal`, `XDPoSChain`, and `go-ethereum` — no source was modified to produce this document.

Repos referenced (confirmed present under `/Users/anilchinchawale/github/XDCNetwork/deVTest/`): `XDPoSChain`, `go-ethereum`, plus sibling client repos (`erigon-xdc`, `besu-xdc`, `reth`, `xdc-nethermind-private`) referenced by the toolkit's `--client` flag.

---

## A) Current masternode setup in `ItWorksinMyLocal`

**Genesis generation** (`gen-genesis.sh:1-121`): drives a bundled, bug-fixed `puppeth` binary non-interactively via piped menu answers. Flags → puppeth wizard prompts: `--signers` (masternode addresses), `--owner`, `--reward` (default 5000), `--v2block` (default **2700**), `--epoch` (default **900**), `--gap` (default **450**), foundation/team multisig, swap wallet, prefund list, chain id (default 20118). Output copied to `genesis/genesis.json`, plus `network-roles.env` recording the role→address map.

**How signers map into `extraData`**: standard XDPoS clique-style layout — 32 bytes vanity + N×20-byte signer addresses + 65-byte seal, e.g. `genesis/genesis.json` extraData decodes to exactly 4 addresses (`20ada9...`, `76fbff...`, `bd2991...`, `d3d1a3...`), matching `SIGNERS` in `gen-genesis.sh:32`. `network-info.sh:47-49` parses this the same way (`body[i:i+40]` chunks).

**How the `0x88` XDCValidator contract is seeded**: puppeth deploys the contract into `alloc["0000...0088"]` with `code` + 39 pre-set `storage` slots (verified directly in `genesis/genesis.json`). Key slots decoded:
- slot `0x7`=1, `0x8`=4, `0x9`=4, `0xa`=1 (owner/candidate counters)
- slot `0xb` (`minCandidateCap`) = `0x84595161401484a000000` = **10,000,000 XDC**
- slot `0xc` (`minVoterCap`) = `0x54b40b1f852bda00000` = **25,000 XDC**
- slot `0xd` (`maxValidatorNumber`) = `0x12` = **18**
- slot `0xe` (`candidateWithdrawDelay`) = `0x13c680` = **1,296,000 blocks**
- slot `0xf` (`voterWithdrawDelay`) = `0x69780` = **432,000 blocks**
- per-candidate `ValidatorState` / `voters` mapping slots give each of the 4 genesis signers a cap of `0xa968163f0a57b400000` = **50,000 XDC**, owned by `OWNER` (`518075434a...`).

This confirms genesis-seeded signers get a flat 50,000 XDC cap directly written to storage (bypassing `propose()`), while the on-chain `propose()` path itself requires `minCandidateCap` = 10,000,000 XDC (see B).

**Bug note** (`README.md:285-293`): stock `puppeth`'s storage RLP-decodes each already-decoded storage word, corrupting `candidates[]` addresses (`0xbd29…`→`0x00`), causing `getCandidates()` garbage and a chain stall at the first epoch boundary (block 900). The toolkit's bundled `puppeth` fixes this (`cmd/puppeth/wizard_genesis.go` in XDPoSChain upstream).

**Existing propose/resign/vote tooling**: **none**. `grep -RIn -i "propose|resign|vote|unvote|withdraw|candidate"` across all `.sh`/`.md`/`.env*` files in the toolkit returns only the two README lines above (the puppeth-bug description). There is no script that calls `propose`/`resign`/`vote`/`unvote`/`withdraw` on `0x88`.

**RPC ports/namespaces exposed**:

| Source | Ports | `--rpcapi`/`--http.api` |
|---|---|---|
| `run.sh:65-76` (oldxdc, local 4-node net) | rpc 8545-8548, ws 8555-8558 | `admin,db,eth,debug,miner,net,shh,txpool,personal,web3,XDPoS` |
| `join.sh:376-387` (oldxdc join) | rpc 8595(+offset), ws 8596 | same, includes `XDPoS` |
| `join.sh:314-322` (geth join) | rpc 8605, ws 8606 | http: `eth,net,web3,txpool,debug,admin,XDPoS`; ws: `eth,net,web3,XDPoS` |
| `join.sh:306-313` (erigon join) | rpc 8615, authrpc 8651, torrent 42079, privapi 9095, mcp 8653 | `eth,net,web3,erigon,debug,txpool` — **no `XDPoS` namespace** |
| `join.sh:333-341` (besu) | via `RPCPORT` | `ETH,NET,WEB3,ADMIN,DEBUG,TXPOOL` — no XDPoS |
| `join.sh:350-356` (reth) | via `RPCPORT`/authrpc | `eth,net,web3,admin,debug` — no XDPoS |
| `join.sh:368-374` (nethermind) | `JsonRpc.Port`/`WebSocketsPort` | own chainspec/config, not flag-driven here |

**Important for the lab**: only `oldxdc` and `geth` expose the `XDPoS_*` namespace; erigon/besu/reth/nethermind currently do not (a gap the lab will need to work around when observing masternode/epoch state cross-client — likely via `eth_getBlockByNumber`'s extraData/header fields or direct state reads of `0x88` instead).

---

## B) XDCValidator (`0x88`) contract interface

**Source**: `/Users/anilchinchawale/github/XDCNetwork/deVTest/XDPoSChain/contracts/validator/contract/XDCValidator.sol` (Solidity `^0.4.21`).
**Generated ABI/bindings**: `contracts/validator/contract/validator.go` (ABI string at `validator.go:180`).
**Go deploy wrapper w/ production defaults**: `contracts/validator/validator.go:46-67`.

### Method signatures, selectors, and locations (`XDCValidator.sol`)

| Method | Signature | Selector | `.sol` line |
|---|---|---|---|
| `propose` | `propose(address)` | `0x01267951` | `XDCValidator.sol:144` |
| `resign` | `resign(address)` | `0xae6e43f5` | `XDCValidator.sol:216` |
| `vote` | `vote(address)` | `0x6dd7d8ea` | `XDCValidator.sol:163` |
| `unvote` | `unvote(address,uint256)` | `0x02aa9be2` | `XDCValidator.sol:204` |
| `withdraw` | `withdraw(uint256,uint256)` | `0x441a3e70` | `XDCValidator.sol:296` |
| `uploadKYC` | `uploadKYC(string)` | — | `XDCValidator.sol:138` |
| `voteInvalidKYC` | `voteInvalidKYC(address)` | — | `XDCValidator.sol:237` |

### Read methods

| Method | Signature | Selector | `.sol` line |
|---|---|---|---|
| `getCandidates` | `getCandidates()` returns `address[]` | `0x06a49fce` | `172` |
| `getCandidateCap` | `getCandidateCap(address)` returns `uint256` | `0x58e7525f` | `176` |
| `getCandidateOwner` | `getCandidateOwner(address)` | `0xb642facd` | `180` |
| `getVoterCap` | `getVoterCap(address,address)` | `0x302b6872` | `184` |
| `getVoters` | `getVoters(address)` returns `address[]` | `0x2d15cc04` | `188` |
| `isCandidate` | `isCandidate(address)` returns `bool` | `0xd51b9e93` | `192` |
| `candidateCount` | (public var) | `0xa9a981a3` | `44` |
| `ownerCount` / `getOwnerCount` | | `0x0db02622` / `0xef18374a` | `45` / `278` |
| `minCandidateCap` | | `0xd55b7dff` | `46` |
| `minVoterCap` | | `0xf8ac9dd5` | `47` |
| `maxValidatorNumber` | | `0xd09f1ab4` | `48` |
| `candidateWithdrawDelay` | | `0xd161c767` | `49` |
| `voterWithdrawDelay` | | `0xa9ff959e` | `50` |
| `getWithdrawBlockNumbers`/`getWithdrawCap` | | `0x2f9c4bba` / `0x15febd68` | `196`/`200` |

(All selectors pulled directly from the generated-binding doc-comments in `contracts/validator/contract/validator.go`, e.g. lines 396, 844, 872, 900, 1012, 1124, 1166.)

### Minimum stake / delay constants

**Contract itself has no hardcoded constants** — `minCandidateCap`, `minVoterCap`, `maxValidatorNumber`, `candidateWithdrawDelay`, `voterWithdrawDelay` are constructor params (`XDCValidator.sol:105-134`). The production values are set at deploy time by `contracts/validator/validator.go:46-56` (`DeployValidator`, used by puppeth genesis generation):

```go
minDeposit.SetString("10000000000000000000000000", 10)   // 10,000,000 XDC — minCandidateCap
minVoterCap.SetString("25000000000000000000000", 10)     //     25,000 XDC
... DeployXDCValidator(..., minDeposit, minVoterCap, big.NewInt(18), big.NewInt(1296000), big.NewInt(432000))
```

- **Min stake to `propose`**: **10,000,000 XDC** (`minCandidateCap`).
- **Min voter stake**: 25,000 XDC.
- **`maxValidatorNumber`**: 18 — note the adjacent code comments say "150 masternodes" (stale comment vs. actual hardcoded `18`; flagging as a discrepancy worth double-checking against the real mainnet-deployed contract, since these values only apply to genesis freshly generated by this puppeth path, not necessarily to the already-deployed XDC mainnet/Apothem `0x88`).
- **`candidateWithdrawDelay`** (resign lock): **1,296,000 blocks** = 30 days at 2s blocks.
- **`voterWithdrawDelay`** (unvote lock): **432,000 blocks** = 10 days at 2s blocks.

All five values are independently confirmed by decoding `genesis/genesis.json`'s live storage slots 0xb-0xf (see section A) — they match exactly.

Storage slot layout for state-side (non-contract-call) reads: `core/state/statedb_utils.go:71-108` (`slotValidatorMapping`), used by fast-path reads like `state.GetCandidates`/`GetCandidateCap` (avoids an EVM call).

---

## C) XDPoS RPC methods (`XDPoS_*` namespace)

Registered at `consensus/XDPoS/XDPoS.go:176` (`Namespace: "XDPoS"`); implemented in `consensus/XDPoS/api.go`.

| RPC method | Returns | file:line |
|---|---|---|
| `XDPoS_getSnapshot` | `PublicApiSnapshot` at a block (signer rotation state) | `api.go:124-137` |
| `XDPoS_getSnapshotAtHash` | same, by block hash | `api.go:140-146` |
| `XDPoS_getSigners` | `[]common.Address` — authorized signers at block | `api.go:149-163` |
| `XDPoS_getSignersAtHash` | same, by hash | `api.go:166-172` |
| `XDPoS_getMasternodesByNumber` | `MasternodesStatus{Epoch, Round, Masternodes, Penalty, Standbynodes,...}` | `api.go:174-209` |
| `XDPoS_getLatestPoolStatus` | `MessageStatus` — current vote/timeout pool signers & missing signers | `api.go:212-226`, helper `calculateSigners` at `337-360` |
| `XDPoS_getV2BlockByNumber` | `V2BlockInfo{Hash, Round, Committed, Miner, EncodedRLP,...}` | `api.go:271-282` |
| `XDPoS_getV2BlockByHash` | same, by hash (also flags `uncle` if off canonical chain) | `api.go:285-302` |
| `XDPoS_networkInformation` | `NetworkInformation{NetworkId, XDCValidatorAddress(0x88), Relayer/XDCX/TRC21 addrs, ConsensusConfigs}` | `api.go:304-315` |
| `XDPoS_getMissedRoundsInEpochByBlockNum` | V2-only: who missed their mining slot in the round's epoch | `api.go:320-322`, `utils.PublicApiMissedRoundsMetadata` |
| `XDPoS_getRewardByAccount` | Epoch-by-epoch masternode/delegator reward breakdown for an address over a block range (reads `common.StoreRewardFolder` JSON files) | `api.go:362-413` |
| `XDPoS_getEpochNumbersBetween` | `[]uint64` epoch-switch block numbers in a range | `api.go:576-601` |
| `XDPoS_getBlockInfoByEpochNum` | `EpochNumInfo{EpochBlockHash, EpochRound, First/LastBlockNumber}` for a given epoch number | `api.go:607-623` |

`GetV2BlockByHeader` (`api.go:228-269`) is an internal helper, not itself RPC-exposed (only the ByNumber/ByHash wrappers are). As noted in A, this whole namespace is oldxdc/geth-only in the current toolkit wiring — erigon/besu/reth/nethermind joins don't register `XDPoS` in their `--http.api`/equivalent flags.

---

## D) Epoch mechanics (epoch=900, gap=450) — when propose/resign takes effect

The candidate-set recomputation is **engine-agnostic** and lives in `core/blockchain.go`, shared by both V1 and V2:

1. **Trigger** (`core/blockchain.go:1547-1556`): after any block is canonicalized, if `block.NumberU64() % Epoch == Epoch - Gap` (i.e., blocks **450, 1350, 2250, …** — the "gap block", `Gap` blocks before each epoch boundary), `bc.UpdateM1()` runs.
2. **`UpdateM1()`** (`core/blockchain.go:2671-2738`): reads the **current state at that gap block** — `state.GetCandidates(stateDB)` (fast-path slot read, `core/state/statedb_utils.go:92-108`), then `GetCandidateCap` per candidate, sorts descending by stake, and calls `engine.UpdateMasternodes(bc, header, ms)`.
3. **V1 consumption** (`consensus/XDPoS/engines/engine_v1/engine.go:1020-1031` `getSignersFromContract`, called from `VerifyHeader` at `engine.go:241-271` when `number % Epoch == 0`): walks back `Gap` blocks from the epoch-boundary header and calls `HookGetSignersFromContract` (`eth/hooks/engine_v1_hooks.go:211-255`) which re-derives the same top-stake list, **capped at top 150** (`engine_v1_hooks.go:247-249` — note: a *third*, different cap from the contract's `maxValidatorNumber=18` and the genesis V2 `maxMasternodes=108`; three different caps exist in this codebase and only the actually-effective one for a given path should be trusted per-engine).
4. **V2 consumption**: same gap arithmetic, `gapNumber = epochSwitchNumber - epochSwitchNumber%Epoch - Gap` (`consensus/XDPoS/engines/engine_v2/engine.go:861`), stored as `SnapshotV2.NextEpochCandidates` (`engines/engine_v2/snapshot.go:75-105` `getSnapshot`, populated via `UpdateMasternodes` at `engine.go:506-524`).

**Practical answer**: a `propose`/`resign`/`vote`/`unvote` transaction must be **mined and its state committed by the gap block** (block number ≡ `Epoch - Gap` mod `Epoch`, e.g. block 450, 1350, 2250…) to affect the masternode set that takes effect at the **next** epoch boundary (900, 1800, 2700…). A transaction landing after the gap block but before the epoch boundary is invisible to that computation and only takes effect one full epoch later (next gap block → epoch boundary after that). This is a hard timing window the lab must respect when scripting worst-case propose/resign scenarios (e.g., "propose exactly at the gap block" vs. "propose one block after the gap block" are meaningfully different test cases).

---

## E) V1→V2 switch (local default block 2700; bundled devnet 5151 genesis uses 1800)

**Selection logic** (`params/config.go:577-582`, `BlockConsensusVersion`): `if V2.SwitchBlock != nil && num.Cmp(SwitchBlock) > 0 { return V2 } else V1`. So block `SwitchBlock` itself is still V1; block `SwitchBlock+1` is the **first V2 block**. `XDPoS.go` dispatches essentially every consensus method (`VerifyHeader`, `Prepare`, `Finalize`, `Seal`, `CalcDifficulty`, `IsAuthorisedAddress`, `GetMasternodes`, `UpdateMasternodes`, `IsEpochSwitch`, etc. — `XDPoS.go:193-485`) through this same switch. Config requires `SwitchBlock % Epoch == 0` or it panics (`params/config.go` — enforced in `XDPoS.go:102-103`).

**Masternode-set bootstrap for V2** (`engines/engine_v2/engine.go:220-252`, in `Initial()`): on first V2 startup, it looks for a `SnapshotV2` at `lastGapNum = SwitchBlock - Gap` (the *last V1-era gap block*); if none is cached on disk, it reconstructs it directly from the V1 checkpoint header's embedded masternode list at `SwitchBlock` (`getExtraFields(checkpointHeader)`, `engine.go:231-238`) rather than re-deriving from contract state — i.e., V2 inherits the exact masternode set V1 had already computed/embedded, then takes over recomputation via its own gap-based mechanism from there on (`gapNumber` arithmetic at `engine.go:861`, `snapshot.go:75-85`).

**What changes for block sealing**:

- **V1**: clique-style — fixed round-robin among the epoch's precomputed M1 (stake-ranked, capped at 150)/M2 (randomized-order) signer list; single signature per block; longest-chain fork choice. Randomize/M2 shuffle: `getValidators`/`BuildValidatorFromM2` in `eth/hooks/engine_v1_hooks.go:309-338`.
- **V2**: HotStuff-style BFT with **Rounds**, **QuorumCert (QC)**, and **TimeoutCert (TC)**:
  - Leader-per-round mining, `YourTurn`/`calcMasternodes` (`engines/engine_v2/engine.go:1041-...`, `mining.go:28`).
  - Votes aggregate into a QC once signatures ≥ `MasternodesLen * certThreshold` (genesis default `certificateThreshold = 0.667`) — `engines/engine_v2/vote.go:176-208` (`onVotePoolThresholdReached`), HotStuff voting rule at `vote.go:210+` (`verifyVotingRule`).
  - Round timeouts aggregate into a TC (`engines/engine_v2/timeout.go`), governed by genesis `timeoutPeriod=10`, `timeoutSyncThreshold=3`.
  - QC/TC each carry a `GapNumber` used to look up the correct `SnapshotV2.NextEpochCandidates` for signature verification (`engine.go:632,677`, `verifyQC` at `engine.go:861-868`).
  - Byzantine/fork detection lives in `engines/engine_v2/forensics.go`.
  - Difficulty calc becomes round-based: `engines/engine_v2/difficulty.go`.
  - Reward/penalty hooks split into masternode/protector/observer tiers post a later "TIPUpgradeReward" (`eth/hooks/engine_v2_hooks.go:283-367`), and standby-node concept appears (candidates beyond the active `maxMasternodes` cut become `Standbynodes`, exposed via `XDPoS_getMasternodesByNumber`).
  - `IsEpochSwitch` in V2 is **round**-based, not raw-block-based (`engines/engine_v2/epochSwitch.go:47+`), since round numbers can skip blocks on timeout — meaning epoch boundaries can occur at different block heights than a naive `block % Epoch == 0` would predict once timeouts have occurred.

---

## Flags for the lab design (open questions / things not fully resolved)

1. **RPC namespace gap**: only oldxdc/geth expose `XDPoS_*`; erigon/besu/reth/nethermind don't in the current `join.sh` wiring — the lab will need an alternate cross-client observation method (raw storage reads of `0x88`, or `eth_getBlockByNumber` header fields) for full 6-client parity checks.
2. **Cap-value discrepancy**: three different "how many masternodes" numbers exist in-repo — contract `maxValidatorNumber=18` (constructor param), V1's hardcoded top-150 cutoff (`engine_v1_hooks.go:247`), and genesis V2 `maxMasternodes=108`. Not verified which is authoritative on real Apothem/mainnet (only that this puppeth-driven local genesis path always deploys `maxValidatorNumber=18`, which looks stale/inconsistent with the "150" code comment beside it) — worth a quick sanity check against a live Apothem `0x88.maxValidatorNumber()` call before designing worst-case propose/resign tests around it.
3. **Local vs. devnet 5151 V2 switch block differ**: local default genesis (`gen-genesis.sh`) uses `v2block=2700`; the bundled `genesis/genesis-5151.json` uses `switchBlock=1800`. Both use `epoch=900`/`gap=450`. The original task brief's "devnet 2700" matches the local default, not devnet 5151 — worth confirming which network the lab is meant to target.
4. No propose/resign/vote/unvote/withdraw tooling exists anywhere in the toolkit today — this will need to be built from scratch (Go binding calls via `contracts/validator/contract/validator.go`, or raw `eth_sendTransaction` with the selectors/ABI above).
