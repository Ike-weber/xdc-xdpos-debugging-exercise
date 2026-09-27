# XDPoS Multi-Client Consensus Test Lab — Design & Build Contract

**Status:** Phase A (validation-parity). Authoritative spec — all lab scripts build against
this. Authored by the Opus orchestrator from the worker inventory (`INVENTORY.md`) and the
adviser scenario catalog (`SCENARIOS.md`).

---

## 1. What this lab is (and is not)

**Is:** a *validation-parity* harness. One `oldxdc` network is the sole block **producer**
and **adversary-injector**; the other clients join **sync-only** as **followers**; a
**differential oracle** checks, every block, that all followers agree with the producer on
block hash, validator set, and reorg resolution. A follower that rejects a valid block,
derives a different masternode set, or resolves a reorg differently = a porting bug.

**Is not (Phase B, deferred):** block-production parity. Only `oldxdc` can seal in this
toolkit (geth seal is TODO; besu/nethermind/reth/erigon are sync-only). Scenarios that
require a *follower to lead a round / build a QC* (adviser 5.1/5.2 producer-side, 4.3
convergence) can only be tested here in their **injected** form: a patched producer emits the
adversarial artifact and we assert every follower accepts/rejects it **identically**.

**Root-cause seams the lab targets** (from both agents):
1. *When, and against which branch, is the masternode set computed* — freeze-per-epoch vs
   live read; gap-block state timing; snapshot caches keyed by epoch-number vs by branch/hash.
2. *v2 fork choice = highest-QC-round*, not total-difficulty/longest-chain — the newer ports
   inherit Ethereum-default fork choice.

---

## 2. Architecture

```
        PRODUCER (canonical + adversary)                 FOLLOWERS (sync-only, under test)
   ┌───────────────────────────────────────┐     ┌──────────────────────────────────────────┐
   │ oldxdc 4-node net  (run.sh)            │     │ geth  erigon  besu  nethermind  reth       │
   │  node1..4 seal; bootnode :30301        │◀────│ each: ./join.sh --client <c> ...           │
   │  admin API on each (partition control) │ p2p │ (+ optional oldxdc follower for self-test) │
   └───────────────┬───────────────────────┘     └───────────────────┬──────────────────────┘
                   │ reference = node1 (:8545)                        │
                   ▼                                                  ▼
             ┌──────────────────────────────────────────────────────────────┐
             │ lab/oracle.sh — every block, for the REFERENCE + each FOLLOWER: │
             │   • eth_getBlockByNumber(N).hash        (divergence detector)   │
             │   • eth_call 0x88 getCandidates()       (validator-set oracle)  │
             │   → first diverging height → classified header diff → verdict   │
             └──────────────────────────────────────────────────────────────┘
```

- **Reference oracle:** producer `node1` at `http://127.0.0.1:8545` is canonical by
  definition. Followers must match it. If producer nodes disagree *with each other*, that is a
  geth/oldxdc drift bug → escalate (do not treat any single one as reference in that case).

---

## 3. Client registry (endpoints the oracle polls)

Producer net from `run.sh` (oldxdc sealers): rpc `8545-8548`, ws `8555-8558`, p2p
`30303-30306`, bootnode `30301`. Followers from `join.sh` (its per-client offset `O`: geth
10, erigon 20, besu 30, nethermind 40, reth 50):

| Role      | client     | rpc  | ws   | p2p   | admin API? | `XDPoS_*` RPC? | launch |
|-----------|------------|------|------|-------|-----------|----------------|--------|
| reference | oldxdc(n1) | 8545 | 8555 | 30303 | yes       | yes            | run.sh |
| follower  | geth       | 8605 | 8606 | 30323 | yes       | yes            | join.sh --client geth |
| follower  | erigon     | 8615 | 8616 | 30333 | **no**    | **no**         | join.sh --client erigon |
| follower  | besu       | 8625 | 8626 | 30343 | yes(ADMIN)| **no**         | join.sh --client besu |
| follower  | nethermind | 8635 | 8636 | 30353 | via config| **no**         | join.sh --client nethermind |
| follower  | reth       | 8645 | 8646 | 30363 | yes       | **no**         | join.sh --client reth |

Note: erigon can only join with a built-in chainspec (`--chain xdc|xdc-apothem`) — it
can't yet load the lab's custom genesis file (issue #9), so it's excluded from the
lab's followers until erigon-xdc gains genesis-file support.

**Consequence (hard constraint):** the oracle MUST NOT depend on `XDPoS_*` — only oldxdc/geth
expose it. Cross-client observation uses **universal** calls only:
`eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_call` (to `0x88`), `eth_getStorageAt`,
`web3_clientVersion`. `XDPoS_*` may be used **only** as an *enrichment* on oldxdc/geth for
diagnostics, never as a parity gate.

---

## 4. Contract interface (0x0000…0088) — verified selectors & constants

Write methods (send via `eth_sendTransaction` from a prefunded owner, or raw signed tx):

| method | signature | selector |
|--------|-----------|----------|
| propose | `propose(address)` | `0x01267951` |
| resign  | `resign(address)`  | `0xae6e43f5` |
| vote    | `vote(address)`    | `0x6dd7d8ea` |
| unvote  | `unvote(address,uint256)` | `0x02aa9be2` |
| withdraw| `withdraw(uint256,uint256)` | `0x441a3e70` |

Read methods (`eth_call`, universal across all clients):

| method | signature | selector | returns |
|--------|-----------|----------|---------|
| getCandidates | `getCandidates()` | `0x06a49fce` | `address[]` |
| getCandidateCap | `getCandidateCap(address)` | `0x58e7525f` | `uint256` |
| isCandidate | `isCandidate(address)` | `0xd51b9e93` | `bool` |
| candidateCount | `candidateCount()` | `0xa9a981a3` | `uint256` |
| maxValidatorNumber | `maxValidatorNumber()` | `0xd09f1ab4` | `uint256` |
| candidateWithdrawDelay | `candidateWithdrawDelay()` | `0xd161c767` | `uint256` |

ABI-encode args as 32-byte left-padded words. `getCandidates()` returns ABI dynamic array
(offset word, length word, then addresses) — the oracle must decode this, not string-compare.

Genesis-deployed constants (storage slots, confirmed in `genesis/genesis.json`):
`minCandidateCap`=10,000,000 XDC (slot 0xb), `minVoterCap`=25,000 XDC (0xc),
`maxValidatorNumber`=18 (0xd), `candidateWithdrawDelay`=1,296,000 blocks (0xe),
`voterWithdrawDelay`=432,000 blocks (0xf).

---

## 5. Epoch timing — the sharpest test tool

Set recompute is engine-agnostic (`core/blockchain.go` `UpdateM1`): it runs at the **gap
block** `block % Epoch == Epoch - Gap`, reading candidate state **at that block**, and the
result takes effect at the **next epoch boundary** `block % Epoch == 0`.

> A propose/resign/vote tx must be **mined and committed by the gap block** to affect the set
> at the next epoch boundary. Landing one block later ⇒ invisible until a full epoch later.
> `resign at gap-block` vs `resign at gap-block+1` are therefore *distinct* tests (the
> off-by-one every reimplementation risks).

v1→v2 switch: `block > SwitchBlock ⇒ v2`; `SwitchBlock` itself is the last v1 block,
`SwitchBlock+1` the first v2 block. Requires `SwitchBlock % Epoch == 0`.

### Lab genesis parameters — epoch is FIXED (finding F1)

The lab generates its **own** genesis (not devnet 5151). **`epoch=900, gap=450` are FORCED.**
`common.EpocBlockRandomize` is hardcoded to 900 in XDPoSChain (`common/constants.go`) and is
decoupled from the genesis `epoch`; any other epoch freezes the chain at the first checkpoint
(finding F1, verified live). This is exactly upstream's own `TestXDPoSMockChainConfig`
(`params/config.go`), so it is the blessed config — not a workaround. **The originally-planned
`fast`/`180` profile is dead; do not reintroduce it.**

Wall-clock is recovered two non-contaminating ways instead of shrinking the epoch:
- **Block period = 1s** (`period:1`, `minePeriod:1`). `1` is the hard floor: `period:0` is a
  trap (won't seal empty non-checkpoint blocks) and `minePeriod:0` allows duplicate
  timestamps. Keep top-level `period == minePeriod`.
- **`switchBlock = 900`** by default (earliest legal v1→v2 boundary; `switchBlock % 900 == 0`),
  so v2 scenarios reach the switch in ~1 epoch (~15 min at 1s) instead of 3.

**Mandatory genesis fields the generator MUST set** (Fable-validated against source):
- `epoch:900`, `gap:450`
- `period:1`, `minePeriod:1`, `timeoutPeriod:10` — **timeoutPeriod is SECONDS** (the struct
  comment saying ms is stale); use `5` for timeout-focused runs.
- `switchBlock:<N>` **AND** `switchEpoch:<N/900>` — `switchEpoch` is **NOT derived** from a
  genesis file; if omitted it defaults to `0` and breaks v2 TC epoch resolution + epoch RPCs.
  So `900→switchEpoch:1`, `1800→2`, `2700→3`. (Base `genesis-5151.json` already pairs
  `switchEpoch:2` with `switchBlock:1800` — follow that pattern.)
- `candidateWithdrawDelay` (slot 0xe) → `20` blocks so `withdraw` is testable in-run.
- ≥ 25 candidate-owner accounts prefunded > 10,000,000 XDC each in `alloc` (for the cap /
  tie-break scenario). Keys → `lab/lab-accounts.env` (throwaway; git-ignored).

### Per-scenario overrides — the env-var contract (pinned)

Scenarios `export` these **before** calling `lab_start_producer`; both `gen-lab-genesis.sh`
and `lib-lab.sh` read them. This is the seam that lets scenario files and the genesis
generator be built in parallel — do not change these names.
- `LAB_SWITCH_BLOCK` (default `900`) — the generator derives `switchEpoch = LAB_SWITCH_BLOCK/900`.
- `LAB_PERIOD` (default `1`) — set `2` for penalty-sensitive scenarios (risk R1).
- `LAB_TIMEOUT_PERIOD` (default `10`).

### Documented risks & forbidden knobs
- **R1 — penalty noise at 1s:** the M2 sign-tx inclusion window halves at 1s; a slow client
  may land fewer sign-txs → spurious penalties at checkpoints. Deterministic on-chain (parity
  unaffected) but it changes *what* scenario 04 observes → **scenario 04 runs `LAB_PERIOD=2`.**
- **R2 — round/wallclock:** never manufacture v2 timeouts by tuning timers; drive them by
  killing the round leader (scenarios 07/09).
- **Forbidden:** `SkipV1Validation` / `SkipV2Validation` — they disable the validation the lab
  exists to compare.
- **Only one v1 epoch at `switchBlock=900`:** scenarios needing a settled multi-epoch v1 chain
  (v1 epoch boundaries, penalty carry-over via `LimitPenaltyEpoch`, reward cycles) set
  `LAB_SWITCH_BLOCK=2700` (v1 boundaries at 900/1800, switch at 2700).

### Gas-limit plateau — a network policy constant, not a genesis field (ItWorksinMyLocal#94/#96)
The block gas limit is **not** part of this lab's genesis contract above, and must never be added
to it. It has two, deliberately different, meanings:
- **Genesis `gasLimit`** is only ever the ratchet's *starting point*. It is FIXED at whatever
  `gen-genesis.sh`/puppeth already produces (mainnet parity: real XDC mainnet's own genesis is
  `0x47b760` = 4,700,000) and this lab never edits it.
- **The mint/plateau target** is the number every producer client is actually launched with
  (`join.sh --gas-limit`, `topologies/*.json`'s `gasLimit` field — default 420,000,000 =
  `0x1908B100`). geth-xdc/erigon-xdc hard-code this as an "XDC plateau" they hone towards at
  runtime, one legal `parent/1024` step per block — exactly how live XDC mainnet itself climbed
  from its own 4,700,000 genesis to its current 420,000,000 plateau, without the genesis file ever
  changing.

**Never make these equal on purpose, and never derive the plateau from genesis** — a client that
did (`erigon-xdc`'s `fix/94-xdc-gas-limit-genesis-plateau` branch; the geth proposal
`XDCIndia/go-ethereum#1351`) would target 4,700,000 forever on a mainnet-parity net and hone the
chain *down* off its real plateau, which is the wrong direction entirely.

`ItWorksinMyLocal#94`'s netv12 wedge was never a genesis-vs-target mismatch. `netlab/node.sh` used
to pass **no** gas flag to `join.sh` at all, so the legacy oldxdc arbiter fell back to its own
compiled-in 50,000,000 default (`cmd/utils/flags.go`'s `MinerGasLimitFlag`) while geth/erigon
elsewhere targeted a hard-coded 420,000,000 — **two different plateau targets on one chain**. The
legacy arbiter's `misc.VerifyGaslimit` hard-errors (instead of clamping) once two producers' targets
drift the parent gas limit more than one legal `parent/1024` step apart, permanently locking every
validator out of proposing. The fix is to give **every** producer of **every** client the exact
same explicit target (`join.sh --gas-limit`, threaded from `topologies/*.json`'s `gasLimit` →
`netlab/topo.sh` → `netlab/node.sh`), never to touch the genesis.

The gasLimit-per-block metric this invariant implies (`lab/lib-rpc.sh`'s `lab_block_gas_limit`,
`lab/lib-assert.sh`'s `assert_gas_limit_plateau`, `oracle.sh`'s `gas=` column): across any run,
gasLimit must be **monotonic non-decreasing** and must **converge on, then hold flat at, the single
plateau**. A DOWN-step (a value LOWER than the previous block) is the bug signature — it is exactly
what a second producer honing toward a lower target than the one that just raised it looks like.

### uploadKYC gotcha (finding F2)
`propose()` has an `onlyKYCWhitelisted` modifier — a fresh owner account MUST call
`uploadKYC(string)` once before its first `propose()`, or every propose reverts. `masternode.sh`
supports `uploadKYC`; every propose-driven scenario must `uploadKYC` the owner first.

### Future optimization (NOT first pass)
Datadir snapshot/restore: mine once to ~block 904 (past the switch + the v2 3-chain commit
depth), snapshot each client's datadir, restore per scenario to skip warm-up. Tracked separately.

---

## 6. Partition & fault injection — cross-platform (host is macOS)

No `iptables`/`pfctl`. Partition by **peer manipulation on the producer nodes** (they have
admin API): `admin_removePeer(enode)` to split the 4 sealers into two groups; let each build a
branch; `admin_addPeer(enode)` to heal. Node stop/start (SIGTERM / relaunch) for
offline-validator and comeback. This is deterministic and portable.

- **Offline validator (penalty):** stop one producer node for a full epoch, restart.
- **Competing branches / reorg:** split 2/2 via removePeer, let both extend, heal, assert all
  followers converge to the higher-total-difficulty (v1) / higher-QC-round (v2) branch.
- **Double-sign / v1-after-switch / bad-QC (injected):** a small patched-producer or a
  hand-crafted RLP block fed via `eth_sendRawTransaction`/`admin_addPeer` from a helper; assert
  every follower's accept/reject is identical (adviser 7.5). Mark these `INJECT` and, if the
  patched binary isn't available, `SKIP` with a logged reason (never silently pass).

---

## 7. File layout & interface contracts

```
lab/
├── lib-lab.sh            # sourced by all lab scripts. Defines the contract below.
├── gen-lab-genesis.sh    # builds a lab genesis (fast|real profile) + patches + lab-accounts.env
├── masternode.sh         # propose|resign|vote|unvote|withdraw|list  (0x88 tx + read helpers)
├── oracle.sh             # differential oracle (per-block parity across running clients)
├── scenario.sh           # runner: `./scenario.sh <id>` or `--all`; up→drive→observe→verdict
├── scenarios/            # one file per scenario, each defining scen_<id>() using lib-lab API
│   ├── 01-diff-oracle-baseline.sh
│   ├── 02-reorg-across-epoch.sh
│   └── ... (see §8)
└── results/              # per-run logs + PASS/DIVERGENCE reports (git-ignored)
```

### `lib-lab.sh` API contract (function signatures every scenario relies on)
```
lab_clients                 # echoes the client ids configured this run (subset of the 6)
lab_rpc <client>            # echoes that client's rpc URL (e.g. http://127.0.0.1:8615)
lab_rpc_call <url> <method> <json-params>      # returns raw JSON-RPC result (curl+parse)
lab_block_number <url>                          # decimal head height
lab_block_hash <url> <N>                        # 0x… hash of block N (or "" if absent)
lab_block_gas_limit <url> <N>                   # decimal gasLimit of block N (or "" if absent) --
                                                 # ItWorksinMyLocal#94/#96's first-class metric
lab_call_0x88 <url> <selector-with-args>        # eth_call to 0x88, returns hex
lab_get_candidates <url>                         # decoded, sorted, comma-joined address list
lab_wait_block <url> <N> [timeout_s]            # block until head ≥ N (or timeout → nonzero)
lab_start_producer                               # bring up run.sh net (reads LAB_SWITCH_BLOCK/LAB_PERIOD/LAB_TIMEOUT_PERIOD env)
lab_start_follower <client>                      # join.sh a follower against the producer
lab_stop <client|node>                           # SIGTERM a node/follower
lab_peers <node>                                 # enode list of a producer node (admin_peers)
lab_partition <groupA-nodes> <groupB-nodes>      # removePeer across the split
lab_heal                                          # addPeer to restore full mesh
lab_teardown                                      # stop everything, keep results/
```
Every function: `set -euo pipefail`-safe, no bare `cd` without `|| exit`, `shellcheck -S
warning` clean (respect the conventions already in the repo — see the shellcheck fix commit).

### `oracle.sh` output contract (scenario.sh parses this)
- Streams one line per checked height:
  `H=<n> ref=<hash8> geth=<hash8|MISS|DIFF> erigon=… …  set=<OK|DIFF> gas=<OK|SKIP|MISS|DOWN|OVER|OFF>`
- On first divergence: prints `DIVERGENCE at H=<n> field=<hash|candidates|head|gaslimit>` plus a
  diff block (differing header fields decoded), then exits nonzero.
- Clean run to target height: prints `PARITY OK 0..<n> across <k> clients` and exits 0.
- Flags `--from N --to M --clients "geth,erigon,…" --interval-blocks K --gas-plateau N`.
- `gas=` (ItWorksinMyLocal#94/#96) is checked on the reference AND every client when
  `--gas-plateau N` (or env `GAS_LIMIT_PLATEAU`) is given: `SKIP` if no plateau was given, `MISS`
  if the node doesn't have the block yet, `DOWN` if its gasLimit decreased since the last checked
  height (the #94 signature -- two different mint targets on one chain), `OVER` if it exceeds the
  given plateau, `OFF` if it moved away from the plateau after reaching it exactly. `DOWN`/`OVER`/
  `OFF` are DIVERGENCEs, same severity as a hash/candidates mismatch. This is **not** a check
  against the genesis header's own `gasLimit` -- see topologies/*.json's `gasLimit` field.

### `scenario.sh` contract
- `scen_<id>()` returns 0 = PASS (parity held as expected / expected-divergence caught),
  nonzero = FAIL. Each scenario declares `SCEN_DESC`, `SCEN_PRIORITY`, `SCEN_PROFILE`
  (fast|real), and `SCEN_MODE` (OBSERVE|INJECT).
- `--all` runs every scenario, writes `results/summary.md` (id, priority, verdict, first-diverging
  height if any, duration). INJECT scenarios with no patched binary ⇒ `SKIP (reason)`, never PASS.

---

## 8. P0 scenario set (build all; ids are file stems)

**All scenarios use epoch=900, gap=450** (finding F1). Each declares `LAB_SWITCH_BLOCK` and
`LAB_PERIOD` (env-var contract, §5). Gap blocks are 450/1350/2250; epoch boundaries 900/1800;
switch at `LAB_SWITCH_BLOCK`. Universal failure signature: any follower's block hash or decoded
`getCandidates()` set differs from the reference at a height.

| id | name | switch | period | trigger blocks | expected | classify-on-fail |
|----|------|:------:|:------:|----------------|----------|------------------|
| 01 | diff-oracle-baseline | 900 | 1 | sync 0→~950 (crosses into v2) | all followers match ref every block | which client, which field |
| 02 | reorg-across-epoch-set-change | 2700 | 1 | split 2/2 before gap 450 so branches compute different next-epoch sets; heal after boundary 900 | all converge to canonical; followers recompute set from winning branch | stale snapshot cache by epoch |
| 03 | resign-at-gap-vs-after | 2700 | 1 | resign active validator in block 450 (run A) vs 451 (run B) | A drops it at epoch 900; B not until 1800 | off-by-one in gap read |
| 04 | offline-validator-penalty | 2700 | **2** (R1) | stop 1 sealer across epoch 900→1800; restart | penalized in checkpoint 1800 set; all agree | penalty range off-by-one |
| 05 | v1v2-switch-pending-lifecycle | 1800 | 1 | propose+resign straddling gap 1350 so set changes exactly at switch 1800 | first v2 set = v1-computed set at gap 1350; block 1801 accepted by all | engine-handoff set mismatch |
| 06 | cap-overflow-tiebreak | 2700 | 1 | uploadKYC+propose 25 candidates (> maxValidatorNumber 18) before gap 450 | all pick identical top-18 and identical ORDER at boundary 900 | sort-stability / top-N |
| 07 | competing-qc-fork-choice | 900 | 1 | post-901 (v2) split, competing same-height blocks, heal | all follow higher-QC-round branch | TD/longest-chain fork choice |
| 08 | doublesign-forensics (INJECT) | 900 | 1 | inject two blocks/votes same height/round (v1 pre-900 and v2 post-901) | all followers reject/converge identically | first-seen-wins fork choice |
| 09 | partition-heal | 900 | 1 | 2/2 partition for ~1–2 epochs post-switch, heal | minority halts, majority advances, all converge on heal | refuses reorg / stuck head |
| 10 | join-mid-epoch-coldstart | 900 | 1 | start each follower fresh at block > 900 (past checkpoint + switch) | follower derives current set from sync, validates head | cold-start set derivation |

Scenarios 02/03/04/06 use `LAB_SWITCH_BLOCK=2700` (need settled v1 epoch boundaries at
900/1800). Scenario 04 uses `LAB_PERIOD=2` (risk R1). Propose-driven scenarios (06) `uploadKYC`
the owner first (finding F2).

Secondary (build if cheap after P0): checkpoint extra-data encoding (adviser 2.1),
snapshot/fast-sync consensus-metadata gap (6.4), duplicate-signature QC (5.4).

---

## 9. Acceptance criteria for the build
- `shellcheck -S warning lab/*.sh lab/scenarios/*.sh` → **zero**; `bash -n` clean on all.
- `lab/gen-lab-genesis.sh` (default, or `--switch-block 900|2700`) produces a valid genesis +
  `lab-accounts.env` with an explicit `switchEpoch`; `oldxdc` `init` against it returns 0.
- `masternode.sh list` decodes `getCandidates()` correctly against a live producer node.
- `oracle.sh` runs against the producer alone (ref vs ref) and reports `PARITY OK` (self-check).
- Each `scen_<id>()` is runnable and self-reports PASS/FAIL/SKIP; `--all` writes `summary.md`.
- **Not required at build time:** that every follower actually passes — discovering real
  divergences is the *point*. Building the harness that can *detect* them is the deliverable.
  Actual multi-client runs need the client binaries built/supplied and are driven by the user.

## 10. `.gitignore` additions
`lab/results/`, `lab/lab-accounts.env`, `lab/*/peer/`, any `lab/**/datadir`.
```
