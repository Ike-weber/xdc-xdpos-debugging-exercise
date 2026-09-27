# XDPoS Multi-Client Consensus Test Lab — Worst-Case Scenario Catalog

> Full scenario reference — feeds `docs/lab/DESIGN.md` §8 (broader than the P0-10 shortlist there); source for Phase-B and secondary scenarios.

**Grounding facts** (from `/Users/anilchinchawale/github/XDCNetwork/deVTest/XDPoSChain`): epoch = 900 blocks, gap = 450 (masternode list for epoch N+1 is computed at checkpoint − 450), `MaxMasternodes = 18` (`common/constants.go:17`), v2 switch at `SwitchBlock` (lab: 2700 = start of epoch 3), propose/resign via the candidate contract (0x...88, `TomoValidator`/`XDCValidator`), penalties via `MinimumMinerBlockPerEpoch` + `LimitPenaltyEpoch` (`params/config.go:505-506`), v1 checkpoint validator list in header extra-data (`engine_v1/snapshot.go`), v2 QC/TC/timeout in `engine_v2/`.

**Universal failure signature** (applies to every scenario below): any client whose `eth_getBlockByNumber(N).hash`, computed masternode list (`XDPoS_getMasternodesByNumber` / snapshot API), or canonical head after reorg differs from the other five. The comparator loop should poll all six every block.

Clients under test: **oldxdc, geth, erigon, besu, nethermind, reth**. oldxdc/geth-XDC are the reference oracle.

---

## Category 1 — Masternode lifecycle (propose / resign)

### 1.1 Propose a new candidate mid-epoch — P0
- **(a) Trigger:** Send `propose(addr)` tx with ≥ minCandidateCap stake to the 0x88 contract at, say, block ~1000 (mid-epoch-2). Wait through gap block 1350 and checkpoint 1800.
- **(b) Expected:** Candidate is NOT active in current epoch; appears in the masternode list computed at the gap block (1350) and becomes active at checkpoint 1800. All clients agree on the exact activation epoch.
- **(c) Failure signature:** One client includes the candidate one epoch early/late; checkpoint-block extra-data validator list (v1) or epoch-switch masternode set (v2) differs → checkpoint block hash divergence.
- **(d) Likely breakers:** besu/nethermind/reth — their XDPoS ports must re-implement the "read contract state at gap block, sort candidates by stake" logic; any difference in the state-read block (gap vs gap−1) or sort tiebreak (stake-equal candidates sorted by address?) diverges. erigon — its flat state model may read candidate state at a subtly different state root.
- **(e) Priority:** P0

### 1.2 Resign an ACTIVE validator — P0
- **(a) Trigger:** From an address currently in the active set, call `resign(addr)` mid-epoch.
- **(b) Expected:** Validator keeps producing for the remainder of the current epoch (list is frozen per epoch); dropped from the list computed at next gap block; stake locked for the withdrawal delay (`candidateWithdrawDelay`).
- **(c) Failure signature:** A client drops the validator immediately (rejects its blocks mid-epoch → fork), or keeps it one epoch too long.
- **(d) Likely breakers:** New ports (besu/reth/nethermind) that recompute the set live from contract state instead of freezing it per-epoch snapshot. This is a classic "reads contract at wrong time" bug.
- **(e) Priority:** P0

### 1.3 Propose beyond MaxMasternodes cap (>18 candidates) — P0
- **(a) Trigger:** Propose 25 funded candidates so >18 have valid stake before a gap block.
- **(b) Expected:** Exactly top-18 by stake selected; deterministic tiebreak for equal stakes. All clients pick the identical 18 and identical ordering (ordering affects round-robin mining order and v1 difficulty).
- **(c) Failure signature:** Different 18th member, or same members in different ORDER → different expected leader per slot → mass "wrong difficulty/leader" rejections.
- **(d) Likely breakers:** Everyone except oldxdc/geth. Sort-stability bugs (Go's `sort.Slice` non-stable vs a stable sort in C#/Java/Rust) are highly likely to surface here. Also check whether the cap is 18 vs a config-driven value clients read differently.
- **(e) Priority:** P0

### 1.4 Resign then re-propose the same address — P1
- **(a) Trigger:** Resign validator X at block ~950; re-propose X (new stake) at ~1100, both within/near one epoch; also a variant where re-propose happens before withdrawal delay expires.
- **(b) Expected:** Contract-level rules decide acceptance (re-propose of a resigned-but-not-withdrawn candidate may be rejected by the contract); consensus set follows contract state at the gap block. All clients agree because it's EVM-level — unless a client caches candidate lists.
- **(c) Failure signature:** A client's cached masternode/candidate snapshot shows X absent while others include X.
- **(d) Likely breakers:** erigon and nethermind — both are prone to caching validator snapshots keyed by epoch and not invalidating on rebuilt state; oldxdc's `snapshot.go` cache also has known staleness corners.
- **(e) Priority:** P1

### 1.5 Propose with insufficient stake — P1
- **(a) Trigger:** `propose()` with stake just below minCandidateCap (boundary: cap − 1 wei, exactly cap, cap + 1).
- **(b) Expected:** Below-cap tx reverts at contract level OR candidate exists but never selected (depending on contract version); exactly-at-cap is included. All clients identical since it's EVM execution.
- **(c) Failure signature:** Divergence here means EVM-level divergence (gas, revert semantics) — very serious; shows up as receipts-root mismatch on the propose tx block.
- **(d) Likely breakers:** reth/besu XDC forks if XDC-specific gas/fee changes (e.g., XDC's zero-fee special contracts / TIPSigning gas rules) aren't ported.
- **(e) Priority:** P1

### 1.6 Resign at the exact epoch boundary blocks (checkpoint 900/1800) and at gap block (1350) — P0
- **(a) Trigger:** Time three resign txs to land in blocks 899, 900 (a checkpoint), and exactly the gap block 1350 (and 1349/1351 controls).
- **(b) Expected:** The set for next epoch is computed from state at the gap-block boundary. The critical question every client must answer identically: does a resign tx *in* the gap block itself count for the list computed *at* that block? Canonical answer comes from oldxdc's behavior (state at end of gap block processing vs beginning).
- **(c) Failure signature:** Off-by-one-block divergence → checkpoint 1800 validator list differs on one client only.
- **(d) Likely breakers:** ALL new clients. This is the single most likely off-by-one in any reimplementation. erigon's staged-sync computes state at block boundaries differently and is a prime suspect.
- **(e) Priority:** P0 — build first.

### 1.7 Propose + resign the same address within one epoch (before the gap block) — P1
- **(a) Trigger:** Propose X at block 910, resign X at block 1200; gap block 1350 sees X as resigned.
- **(b) Expected:** X never becomes active. All clients agree.
- **(c) Failure signature:** A client that processes propose events incrementally (event-driven candidate tracking instead of state-read at gap) shows X active for one epoch.
- **(d) Likely breakers:** nethermind/besu if they used log/event subscription to track candidates rather than direct state reads.
- **(e) Priority:** P1

### 1.8 Many lifecycle ops in a single block (batch) — P2
- **(a) Trigger:** Pack 10+ propose/resign/vote/unvote txs into one block near the gap block, some interdependent (vote for candidate proposed earlier in same block).
- **(b) Expected:** Deterministic intra-block ordering by tx index; final state identical everywhere.
- **(c) Failure signature:** Receipts-root or state-root mismatch on that block.
- **(d) Likely breakers:** Parallel-EVM clients (reth with parallel execution, nethermind) if XDC's special-address rules break dependency detection.
- **(e) Priority:** P2

---

## Category 2 — Epoch & gap boundary edge cases

### 2.1 Checkpoint block correctness (v1) — P0
- **(a) Trigger:** Just run v1 epochs (blocks 900, 1800) with a changing candidate set and compare checkpoint headers.
- **(b) Expected:** Checkpoint header extra-data contains the sorted new masternode list; penalties field matches; all clients produce/accept identical checkpoint hash.
- **(c) Failure signature:** `ErrInvalidCheckpointPenalties` / `ErrValidatorsNotLegit` (in `utils/errors.go:50-52`) logged on one client; that client stalls at the checkpoint while others advance.
- **(d) Likely breakers:** Everyone except oldxdc. Extra-data byte layout (32-byte vanity + 20-byte addresses + 65-byte seal) is easy to mis-encode.
- **(e) Priority:** P0

### 2.2 Gap-block state read at 450/1350/2250 — P0
- **(a) Trigger:** Change stake orderings via `vote()`/`unvote()` txs in blocks (gap−2 .. gap+2); check which changes are reflected in the next epoch's set.
- **(b) Expected:** Identical cut-off semantics across clients (canonical = oldxdc).
- **(c) Failure signature:** Next-epoch set differs by one address or by order.
- **(d) Likely breakers:** erigon (state access model), reth (state provider abstraction). Same off-by-one family as 1.6 but via stake-weight changes, not membership.
- **(e) Priority:** P0 (can be merged with 1.6 into one harness)

### 2.3 Empty/degenerate candidate set at gap block — P1
- **(a) Trigger:** Resign all but 1 (and in an extreme run, ALL) candidates before a gap block.
- **(b) Expected:** Canonical behavior per oldxdc: fall back to previous set or minimum set; chain must not halt (or halts identically!). Must be verified against reference — even "all clients halt the same way" is the pass criterion.
- **(c) Failure signature:** One client falls back to genesis signers, another keeps the previous epoch's set, another panics.
- **(d) Likely breakers:** All new clients — degenerate paths are rarely ported. Even oldxdc may panic; that's still the reference behavior.
- **(e) Priority:** P1

### 2.4 Epoch boundary + non-consecutive checkpoint parents (v1 skip) — P2
- **(a) Trigger:** Kill the scheduled producer of block 900 so a different masternode seals the checkpoint at a later timestamp.
- **(b) Expected:** Backup-producer rules (v1 difficulty/turn-ness) apply at checkpoints identically as at normal blocks.
- **(c) Failure signature:** Divergent difficulty computation at checkpoint → competing checkpoint blocks with different total difficulty → split canonical chains.
- **(d) Likely breakers:** besu (its default fork-choice is TD-based but v1 XDPoS difficulty logic is bespoke), geth-XDC vs oldxdc drift.
- **(e) Priority:** P2

### 2.5 Reward calculation epoch (rewards applied at checkpoint) — P1
- **(a) Trigger:** Run through 2+ full epochs with unequal block production per signer; inspect checkpoint state root (reward distribution to masternode owners + foundation).
- **(b) Expected:** Identical reward amounts → identical state root at 900/1800.
- **(c) Failure signature:** State-root mismatch at exactly checkpoint blocks and nowhere else — a very recognizable fingerprint.
- **(d) Likely breakers:** All ports; reward code reads signing txs from the *previous* epoch range and involves integer division ordering. erigon especially (must replicate the "scan blocks for signing txs" against its own DB layout).
- **(e) Priority:** P1

---

## Category 3 — v1→v2 switch (block 2700) interacting with pending propose/resign

### 3.1 Propose/resign in the last v1 epoch, activating exactly at the switch — P0
- **(a) Trigger:** Propose candidate A and resign active validator B in the last v1 epoch, with txs straddling the epoch's gap block 2250 (e.g., blocks 2200–2260). Chain switches to v2 at 2700 with a changed set.
- **(b) Expected:** The first v2 epoch's masternode set = list computed under v1 rules at gap 2250; v2 `epochSwitch.go` picks it up identically. Round 0 leader identical everywhere.
- **(c) Failure signature:** v2 clients disagree on the first epoch-switch block's validator set → the very first v2 block is rejected by some clients; total network split at 2700.
- **(d) Likely breakers:** ALL clients — this is the highest-risk composite event in the whole system. besu/nethermind/reth must implement both engines AND the handoff.
- **(e) Priority:** P0 — build first.

### 3.2 Switch block header format transition — P0
- **(a) Trigger:** Just cross 2700 and diff headers 2699/2700/2701 across clients (extra-data schema changes: v2 uses round number + QC in extra fields; validators/penalties encoding changes).
- **(b) Expected:** All clients encode/parse v2 extra fields identically; block hash of 2700 identical.
- **(c) Failure signature:** RLP decode errors in logs (`extra-data` parse), or a client accepting a malformed v2 header others reject.
- **(d) Likely breakers:** reth (Rust RLP for bespoke nested structs), besu (Java). oldxdc is the reference.
- **(e) Priority:** P0

### 3.3 v1-style block arriving after the switch (adversarial) — P1
- **(a) Trigger:** Modified oldxdc node mines a v1-format block at height 2700+ and gossips it.
- **(b) Expected:** All clients reject it with the same class of error; no client stores it as a side-chain that later confuses fork choice.
- **(c) Failure signature:** One client imports it as valid → instant divergence; or crashes on parse.
- **(d) Likely breakers:** Clients with lenient header validation ordering (parse-then-verify vs verify-then-parse); nethermind historically permissive on extra-data.
- **(e) Priority:** P1

### 3.4 Timeout/QC referencing pre-switch blocks — P1
- **(a) Trigger:** Immediately after 2700, force a timeout in round 1 so the first QC/TC ever built must reference the switch block (whose parent is v1).
- **(b) Expected:** v2 treats switch block as round-0/genesis-of-v2 anchor (per `engine_v2/utils.go` special-casing); TC/QC verification succeeds identically.
- **(c) Failure signature:** One client can't verify the first QC (missing round info for v1 parent) and stalls at 2701 forever.
- **(d) Likely breakers:** All new v2 implementations — the "first QC" special case is a classic.
- **(e) Priority:** P1

---

## Category 4 — Penalties (offline, comeback, equivocation)

### 4.1 Validator misses its slots for a full epoch → penalized — P0
- **(a) Trigger:** Stop one masternode's process for blocks 900–1800 (falls below `MinimumMinerBlockPerEpoch`).
- **(b) Expected:** Address appears in the penalty list of checkpoint 1800 (v1) / epoch-switch block (v2); excluded from active set for `LimitPenaltyEpoch` epochs; ALL clients compute the identical penalty list (order matters — it's in the header).
- **(c) Failure signature:** `ErrInvalidCheckpointPenalties` on a subset of clients; checkpoint hash divergence.
- **(d) Likely breakers:** Everyone — penalty computation scans block signers over an epoch range; off-by-one on range endpoints (900..1799 vs 901..1800) differs per implementation. erigon's snapshot-based signer lookup is a suspect.
- **(e) Priority:** P0 — build first.

### 4.2 Penalized validator comeback — P1
- **(a) Trigger:** Restart the stopped node; keep it online during its penalty epochs; verify it rejoins after exactly `LimitPenaltyEpoch` epochs.
- **(b) Expected:** Deterministic re-admission epoch; identical across clients.
- **(c) Failure signature:** One client readmits an epoch early → its epoch-switch validator set differs.
- **(d) Likely breakers:** Clients that store penalty state in their own DB format (nethermind/besu) and reload it wrong after restart.
- **(e) Priority:** P1

### 4.3 Double-sign / equivocation (same round, two blocks) — P0
- **(a) Trigger:** Patch one oldxdc/geth miner to sign two different blocks for the same height (v1) or two votes / two proposals for the same round (v2). Gossip both to different halves of the client set.
- **(b) Expected:** v2: `forensics.go` detects and reports; consensus continues; all clients converge on the QC'd branch. No client should follow different branches permanently.
- **(c) Failure signature:** Clients split by which block they saw first and never converge (missing "prefer QC'd chain" rule); or a client crashes in forensics handling.
- **(d) Likely breakers:** Clients that ported fork-choice as "first seen wins" or TD-based (besu) instead of v2 round/QC-based. Also erigon — forensics is likely unported.
- **(e) Priority:** P0

### 4.4 Penalty at the v1→v2 boundary — P1
- **(a) Trigger:** Make a validator miss the last v1 epoch (1800–2700) so the penalty list is computed for the first v2 epoch.
- **(b) Expected:** v2 epoch-switch honors v1-computed penalties identically across clients.
- **(c) Failure signature:** First v2 set differs by the penalized node on one client.
- **(d) Likely breakers:** All — combines 3.1 and 4.1 risks.
- **(e) Priority:** P1

### 4.5 All-but-quorum penalized — P2
- **(a) Trigger:** Take down enough masternodes that the surviving set is smaller than the v2 vote quorum (2/3 of set + 1) — e.g., 3 of 5 offline.
- **(b) Expected:** Chain halts (no QC possible) but does NOT diverge; on restart of nodes, all clients resume from the same head via TC path.
- **(c) Failure signature:** A client mints blocks without quorum, or resumes on a different branch.
- **(d) Likely breakers:** Any client with quorum computed on the wrong denominator (total-configured vs currently-active set; ceil vs floor of 2/3).
- **(e) Priority:** P2

---

## Category 5 — v2 timeout / QC / TC

### 5.1 Single-round timeout and recovery — P0
- **(a) Trigger:** Kill the round-r leader just before it proposes; wait for timeout period; nodes exchange timeout messages → TC → round r+1.
- **(b) Expected:** All clients emit timeout votes, assemble identical TC, and agree the next leader is the round-(r+1) leader; block numbering continues with a gap in rounds but not in heights.
- **(c) Failure signature:** One client stuck in round r (didn't accept TC), falls behind, then re-syncs on a "different" view; or computes a different r+1 leader (leader = masternodes[round % len] — off-by-one or different modulo base).
- **(d) Likely breakers:** ALL new v2 ports. Leader-rotation formula mismatch is the most common porting bug in HotStuff-family engines. Also timeout-period config drift (exponential backoff parameters).
- **(e) Priority:** P0 — build first.

### 5.2 Competing QCs / fork at same height — P0
- **(a) Trigger:** Partition the network briefly during a proposal so half sees block B(r), half times out to r+1 and gets B'(r+1) at the same height; heal.
- **(b) Expected:** All clients follow the branch with the higher-round QC (v2 fork choice = highest QC round, not TD/length); identical final head.
- **(c) Failure signature:** Divergent heads after heal; a client using longest-chain/TD keeps the wrong branch — a permanent split.
- **(d) Likely breakers:** besu (native TD fork choice), erigon (staged sync fork choice assumptions), reth (needs custom fork-choice hook).
- **(e) Priority:** P0 — build first.

### 5.3 TC with mixed/gappy round numbers — P1
- **(a) Trigger:** Multiple consecutive leader failures → several timeouts in a row (rounds r, r+1, r+2 all fail) before recovery.
- **(b) Expected:** Round advances by exponential/linear schedule; all clients agree on the final round of the next successful block; QC/TC chain verifies.
- **(c) Failure signature:** Round-number mismatch in the next block's header → header rejected by a subset.
- **(d) Likely breakers:** Clients that persist "current round" and reload badly on restart mid-timeout-cascade (nethermind, erigon).
- **(e) Priority:** P1

### 5.4 QC signature verification edge: exactly-quorum vs quorum+1 signatures — P1
- **(a) Trigger:** Craft a QC with exactly ⌈2n/3⌉ signatures, one with fewer (adversarial), one with duplicate signatures from the same validator.
- **(b) Expected:** Exact-quorum accepted, fewer rejected, duplicates counted ONCE (so dup-padded below-quorum QC rejected) — identically on all clients.
- **(c) Failure signature:** A client accepting a duplicate-padded QC → accepts a block others reject → split.
- **(d) Likely breakers:** Any client counting signatures by length instead of unique-signer set. High-value adversarial test.
- **(e) Priority:** P1

### 5.5 Vote/timeout message replay across epochs — P2
- **(a) Trigger:** Record round-r vote messages from epoch E; replay them in epoch E+1 (same round numbers repeat if rounds re-baseline per epoch).
- **(b) Expected:** Messages bound to epoch/parent-hash; replays rejected identically.
- **(c) Failure signature:** A client double-counts a replayed vote toward a current QC.
- **(d) Likely breakers:** Clients missing epoch binding in vote hash.
- **(e) Priority:** P2

---

## Category 6 — Network faults

### 6.1 Partition then heal (clean 3/3 split) — P0
- **(a) Trigger:** iptables/network-namespace split the 6 clients 3+3 for ~2 epochs, then reconnect.
- **(b) Expected:** Minority partition (no quorum) halts; majority (if it has quorum) advances; on heal, minority reorgs to majority chain. All clients end on the identical head.
- **(c) Failure signature:** Two clients keep different heads post-heal; or a client refuses to reorg (stuck).
- **(d) Likely breakers:** erigon (staged sync reorg/unwind across an epoch boundary is fragile), besu (fork-choice), reth.
- **(e) Priority:** P0

### 6.2 Reorg across an epoch boundary with a set change — P0
- **(a) Trigger:** Force a reorg whose fork point is before a gap block, where the two branches computed *different* masternode sets for the next epoch.
- **(b) Expected:** On reorg, clients recompute the epoch set from the winning branch's state and re-validate subsequent blocks; unwind is clean.
- **(c) Failure signature:** A client keeps the losing branch's cached masternode snapshot after reorg → validates new blocks against the wrong set → rejects the canonical chain.
- **(d) Likely breakers:** ALL clients that cache validator snapshots by epoch number rather than by (epoch, branch/hash). erigon and oldxdc snapshot caches are prime suspects (`engine_v1/snapshot.go` caching).
- **(e) Priority:** P0 — build first. This is the highest-value reorg test.

### 6.3 Client joins mid-epoch and must derive the current set — P1
- **(a) Trigger:** Start a fresh client at, e.g., block 1350+ (past a checkpoint) with only P2P sync; it must derive the active set without having produced it live.
- **(b) Expected:** Joining client computes the same active set from synced headers/state as the running network; validates the current epoch's blocks.
- **(c) Failure signature:** Joining client computes a different set (e.g., reads latest state instead of gap-block state) → can't validate incoming blocks → never syncs.
- **(d) Likely breakers:** besu/nethermind/reth — "cold start set derivation" is a distinct code path from "live tracking" and often diverges.
- **(e) Priority:** P1

### 6.4 Snapshot / fast-sync across a set change — P1
- **(a) Trigger:** Snap-sync (or erigon staged snapshot sync) a new node to head where the pivot lands mid-epoch and multiple set changes occurred in history.
- **(b) Expected:** Post-sync, the node reconstructs correct penalty/candidate/snapshot state at the pivot and matches the network head.
- **(c) Failure signature:** Fast-synced node has correct state root but wrong in-memory XDPoS snapshot (penalties/round) → first self-validated block after pivot diverges.
- **(d) Likely breakers:** erigon (its snapshot format is unique and must serialize XDPoS auxiliary state), reth (snap sync + custom consensus state). Classic "state synced, consensus metadata not" bug.
- **(e) Priority:** P1

### 6.5 Deep reorg spanning the v1→v2 switch — P2
- **(a) Trigger:** Force a reorg whose common ancestor is before 2700 and competing branches extend past it.
- **(b) Expected:** Clients unwind through the engine switch cleanly and re-run v2 from 2700 on the winning branch.
- **(c) Failure signature:** Crash/stall during cross-engine unwind; wrong engine used for revalidation.
- **(d) Likely breakers:** All — engine selection keyed on height must be re-evaluated per branch during unwind.
- **(e) Priority:** P2

### 6.6 Time-skew / clock drift between clients — P2
- **(a) Trigger:** Skew system clocks ±5–15s across clients (v2 timeouts and v1 block timestamps are time-sensitive).
- **(b) Expected:** Blocks with future timestamps within allowed drift accepted; beyond drift rejected — identical threshold everywhere.
- **(c) Failure signature:** One client rejects a valid block as "too far in future" (different `allowedFutureBlockTime`) → temporary desync.
- **(d) Likely breakers:** besu/nethermind if they use Ethereum-default drift constants instead of XDC's.
- **(e) Priority:** P2

---

## Category 7 — Cross-client divergence (the meta-oracle)

These are not separate triggers but continuous differential-oracle checks layered over every scenario above.

### 7.1 Identical active validator set every block — P0
- **(a) Trigger:** Poll `XDPoS_getMasternodes`/snapshot API + the masternode-order used for the next leader on all 6, every block.
- **(b) Expected:** All 6 return byte-identical ordered lists.
- **(c) Failure signature:** Any mismatch, especially in ordering.
- **(d) Likely breakers:** N/A (this is the oracle). Set-computation and sort-stability bugs surface here first.
- **(e) Priority:** P0

### 7.2 Identical block hash at every height — P0
- **(a) Trigger:** Compare `eth_getBlockByNumber(N).hash` across all 6 continuously.
- **(b) Expected:** Identical.
- **(c) Failure signature:** First diverging height pinpoints the offending block; diff header fields (extra-data, penalties, difficulty/round, receipts root, state root) to classify.
- **(d) Likely breakers:** N/A (oracle).
- **(e) Priority:** P0

### 7.3 Identical reorg resolution — P0
- **(a) Trigger:** After every injected fork/partition, compare canonical head hash across all 6.
- **(b) Expected:** Identical head; identical set of orphaned blocks.
- **(c) Failure signature:** Divergent heads = fork-choice rule mismatch (the single most dangerous multi-client bug class).
- **(d) Likely breakers:** N/A (oracle).
- **(e) Priority:** P0

### 7.4 Identical RPC-reported epoch/round/penalty metadata — P1
- **(a) Trigger:** Compare `XDPoS_getEpoch`, current round, penalty lists, `getRewards` across clients.
- **(b) Expected:** Identical values.
- **(c) Failure signature:** Metadata mismatch even when block hashes match → latent bug that will surface under stress later.
- **(d) Likely breakers:** N/A (oracle).
- **(e) Priority:** P1

### 7.5 Identical rejection behavior on invalid input — P1
- **(a) Trigger:** Feed each adversarial block (from 3.3, 4.3, 5.4) to all clients.
- **(b) Expected:** All reject (or all accept) — never a split decision.
- **(c) Failure signature:** Split accept/reject = consensus-critical validation gap.
- **(d) Likely breakers:** N/A (oracle).
- **(e) Priority:** P1

---

## Top 10 to build first (ranked)

1. **7.2 + 7.1 + 7.3 differential oracle** — the harness itself: per-block hash, ordered validator set, and post-fork head comparison across all 6. Nothing else is measurable without this. **P0**
2. **6.2 Reorg across an epoch boundary with a set change** — highest-value single bug trap; hits snapshot-cache-by-epoch bugs in every client. **P0**
3. **1.6 / 2.2 Resign & stake-change at the exact gap block (1350) and checkpoint (900/1800)** — the off-by-one boundary that every reimplementation gets wrong. **P0**
4. **4.1 Offline validator → penalty list at checkpoint** — penalty range off-by-one; directly divergent checkpoint hashes. **P0**
5. **5.1 v2 single-round timeout & leader rotation** — leader-rotation-modulo bug is the canonical v2 porting failure. **P0**
6. **5.2 Competing QCs / same-height fork resolution** — verifies v2 highest-QC fork choice vs TD; catches besu/erigon/reth fork-choice ports. **P0**
7. **3.1 Propose/resign in last v1 epoch activating at the v1→v2 switch (2700)** — the highest-risk composite event; engine handoff + set change together. **P0**
8. **1.3 Propose beyond MaxMasternodes cap (>18) with tie-break** — sort-stability / top-N selection divergence. **P0**
9. **4.3 Double-sign / equivocation + forensics** — partitions clients by first-seen; tests convergence on the QC'd branch. **P0**
10. **6.1 Partition then heal (3/3)** — end-to-end liveness + safety under the most realistic production fault. **P0**

**Runners-up to stage next:** 2.1 (checkpoint extra-data encoding), 6.4 (snapshot/fast-sync consensus-metadata gap), 5.4 (duplicate-signature QC), 6.3 (mid-epoch cold-start set derivation).

---

## Rationale notes for the orchestrator

- The recurring root-cause theme is **"when and against which branch is the masternode set computed"** — freeze-per-epoch vs live-read, gap-block state timing, and cache-keying by epoch-number vs by-branch. Scenarios 1.1, 1.2, 1.6, 2.2, 6.2, 6.3, 6.4 all probe this one seam and should share harness plumbing.
- The second theme is **v2 fork choice = highest-QC-round**, not TD/longest-chain. besu, erigon, and reth inherit Ethereum-default fork choice and are the top suspects for 5.2, 6.1, 6.2, 4.3.
- oldxdc/geth-XDC are the **reference oracle**; a divergence where only oldxdc and geth agree and the other four differ points at a porting bug; a divergence where oldxdc and geth *disagree with each other* points at a geth-XDC drift bug and should be escalated.
