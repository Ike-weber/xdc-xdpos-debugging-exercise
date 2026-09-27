---
name: port-xdc-onto-geth
description: >-
  Use when porting, rebasing, re-applying, or recreating XDC Network (XDPoS)
  consensus on top of a go-ethereum base - e.g. upgrading the XDC geth fork to a
  newer upstream geth version, catching up an XDC fork to upstream, resolving
  consensus-parity divergence after an upstream merge, or onboarding to how the
  XDC overlay is structured. Encodes the reset-and-replay bundle methodology, the
  surgical integration seams, the consensus-parity invariants, and the validation
  gates. Triggers on requests mentioning XDC/XDPoS + geth/go-ethereum + port/
  upgrade/rebase/merge/parity, or "recreate XDC consensus".
---

# Porting XDC (XDPoS) consensus onto any go-ethereum version

You are layering XDC Network consensus (XDPoS) onto an upstream go-ethereum base.
The **canonical XDC clients are the specification** (`XDPoSChain/`, and the
geth-based `XDCIndia/go-ethereum`); the new geth is just the host. When they
disagree, **XDC wins**, and you document why.

> **The exhaustive, verified reference is `docs/port/recreate-guide/README.md`
> (and `index.html`).** Read it before deep work. This skill is the operating
> procedure; that guide is the detailed subsystem map with `file:line` citations.

## Operating rules (non-negotiable)

1. **Consensus parity is sacred.** Never "improve", "optimise", reorder, or
   "clean up" any consensus path. Never change RLP encoding of a block, header,
   receipt, or state account. Never force an upstream default onto an
   XDC-customised path. A post-state-root / receipts-root / block-hash mismatch
   vs canonical XDC is **P0 — stop everything and root-cause it.**
2. **The canonical client is the source of truth for every value.** Fork blocks,
   reward percentages, gas constants, chain IDs, system-contract addresses,
   storage slots — read them out of `XDPoSChain`/`XDCIndia/go-ethereum`. Never
   infer or "modernise" them.
3. **Escalate irreversible architectural calls** (catch-up strategy; any module
   rewrite; the first consensus mismatch since its root cause may be
   architectural; bundle ordering; anything that changes block-hash output).
4. **Verify, don't assume.** When you analyse a seam, confirm it against the
   actual code and cite `file:line`. Upstream geth drifts — a seam's *intent* is
   stable, its *location* is not.
5. **Maintain a trail.** Append every decision to a port log and every conflict
   resolution to a conflict log (file path + before/after + one-line why).

## The mental model

XDC is ~78% **additive** (new files upstream never touches) and ~22% **surgical
seams** cut into core geth files. So the work splits in two:

- **Category A — copy verbatim:** `consensus/XDPoS/**`, `contracts/**`,
  `eth/hooks/**`, `eth/bft/**`, `eth/sync_xdc.go`, `eth/handler_xdc.go`,
  `core/xdc_genesis/** + core/genesis_xdc.go`, `params/xdc_features.go`,
  `common/xdc_contracts.go`, `core/xdc_sync_opts.go`, `node/config_xdc.go`,
  `node/health.go`. These import only stable geth surfaces.
- **Category B — re-cut by hand:** the ~58 modified geth files. All the
  difficulty lives here because the surrounding upstream code drifts.

## Methodology: reset-and-replay in bundles (NOT a linear rebase)

Do **not** cherry-pick hundreds of commits linearly across a major-version gap.
Group the overlay into ~11 logical bundles, replay each onto a pristine baseline,
build+test, and merge as one `--no-ff` commit.

Bundle order (foundation before the things that sit on it):
`B0 config/genesis/contracts → B1 XDPoS-V1 → B3 TIPSigning/precompiles →
B4 header/finality plumbing → B5 P1 bulk-sync → B7 bypass cleanup →
B2 XDPoS-V2 engine → B6 snap-sync → B8 scripts → B9 fixes → B10 docs → B11 rest`.
(V2 lands 7th so it sits on validated plumbing; if it broke earlier you couldn't
tell whether V2 itself was wrong or its substrate.)

**Conflict policy:** on any conflict where the `XDC:` marker is on either side,
XDC behaviour wins; log it. For pure-refactor conflicts, take the upstream
skeleton and re-apply the XDC additions on top.

**Per-bundle gate:** `go build ./... && go vet ./... && go test ./<touched>/...`.
For F-bucket bundles (B4/B5/B7 — they touch `core/blockchain.go` /
`eth/downloader/`), also run the full local sync test and grep the logs for zero
hits of: `BAD BLOCK`, `invalid merkle root`, `state-root bypass`,
`forced trie commit`.

## Process

1. **Phase 0 — baseline & oracle.** Pin the target geth tag; confirm clean
   build; tag `v<ver>-pristine`. Enumerate the overlay
   (`git diff <old-base>..xdc-HEAD --name-status`). Stand up a parity oracle
   (synced canonical node or block/pre-state dumps incl. fork-boundary blocks).
   **Do not skip the oracle.**
2. **Phase 1 — copy Category A verbatim.** It won't compile yet; that's fine —
   the compiler will enumerate the missing seams.
3. **Phase 2 — cut Category B seams in dependency order:** `params/`+`common/`
   → `core/types/` (+ regenerate RLP) → `core/genesis.go` (**assert genesis
   hashes immediately**) → `core/vm/` + `core/state_*` + `eip1559` →
   `eth/ethconfig` + `eth/backend.go` (engine wiring) → `eth/protocols/eth` +
   `eth/handler*` + `eth/sync_xdc.go` + `p2p/discover/v4wire` →
   `internal/ethapi` + `internal/web3ext` → `cmd/utils/flags.go` + bootnodes.
4. **Phase 3 — build & wire.** `go build ./...` walks you through missing seams.
5. **Phase 4 — validate** (three tiers below). Done only when parity is green
   across the **fork boundaries**, not just genesis.

## Find XDC code in any tree (the four markers)

- `grep -rIn "XDC:" --include=*.go` (log/comment prefix on most seams)
- `grep -rIn "IsXDC()\|XDPoS != nil\|config.XDPoS"` (the behavioural-fork predicate)
- `git ls-files | grep -iE "xdc|xdpos"` (filenames carry `xdc_`/`_xdc`)
- `git diff <geth-base-tag>..HEAD --name-status | grep '^M'` (the surgical seams)

## Consensus-parity invariants — what MUST be bit-exact

Ordered by how early/often they bite (full detail in the guide §7–§8):

1. **Genesis hash** (assert first): Mainnet `0x4a9d748b…`, Apothem `0xbdea512b…`.
2. **Header RLP:** `Validators`, `Validator`, `Penalties` as `[]byte`,
   **always-encoded** (no `rlp:"optional"`), at indices 16/17/18, inserted
   **after `Nonce`, before `BaseFee`**. Regenerate `gen_header_rlp.go` via
   `go:generate` (rlpgen). Strip any upstream optional newer than XDC's fork
   point that can't be guaranteed nil (the port stripped EIP-7928/7843 fields).
3. **StateAccount RLP:** add `BigBalance *big.Int rlp:"-"`; hand-written encoder
   must be byte-identical to upstream for balances ≤ 2^256-1.
4. **EVM EIP bundle at `Eip1559Block`** (NOT Berlin/London/Shanghai): EIP-2929 +
   3529 + 2565 + 3860 all defer to `Eip1559Block`; **EIP-2028 permanently OFF**
   (68 gas/non-zero data byte). `IsEIP1559` is **decoupled from `IsLondon`**.
5. **Special txs** to `0x89`/`0x92`/`0x93`: zero gas, **minimal `{Address,
   BlockNumber}` log** (a richer log breaks bloom/receipts root), no EVM.
6. **TIPSigning self-destruct** of `0x89` with the pathdb-correct
   `IntermediateRoot(false)` first.
7. **`parentState` reward seam:** `statedb.Copy()` before the tx loop, pushed via
   `SetParentState` so rewards read pre-tx balances.
8. **TRC21 fee routing & no base-fee burn:** full `gasUsed*gasPrice` to the
   candidate owner past `TIPTRC21Fee`.
9. **Fixed base fee = 12500000000** (12.5 gwei), never market-driven.
10. **TIP fork-block constants & reward % (90/10)** from `common/types.go`.
11. **StaticCall zero-value touch gated to `IsIstanbul`** (else single-leaf
    divergence on ecrecover STATICCALL).
12. **PREVRANDAO = `Keccak256(blockNumber)`.**

## The #1 silent failure: V2 reward-hook wiring

In `eth/backend.go New()`, after the blockchain exists, the order is load-bearing:
`AttachConsensusV1Hooks` → `AttachConsensusV2Hooks` → `engine_v2.New(cfg,db,nil,nil)`
→ set `HookCommitBlock = blockchain.SetFinalized` → `SetEngineV2(v2)` →
`v2.SetHookReward(forward to wrapper.HookReward)`. Because `AttachConsensusV2Hooks`
runs *before* `SetEngineV2`, its internal `SetHookReward` is dead — the explicit
`v2.SetHookReward(...)` **after** `SetEngineV2` is the ONLY effective wiring of the
V2 engine's reward callback. Miss or misorder it and V2 epoch-switch rewards never
fire: zero compile/verify errors, state-root divergence at the first V2 epoch.
Also: upstream removed `APIs()` from `consensus.Engine` (#814) — re-register the
`XDPoS` namespace manually in `Ethereum.APIs()`.

## Validation tiers

- **Tier 1:** `go build/vet/test`. **Run the engine's own parity vectors:**
  `consensus/XDPoS/{cross_client_vectors,state_root_vectors,header_rlp_compat}_test.go`
  + `testvectors/*.json` — they catch RLP/hash drift directly.
- **Tier 2:** `scripts/local-sync-test.sh wipe-stateful && start all`; grep logs
  for zero bad hits.
- **Tier 3 (the real gate):** canonical parity — genesis→head sync against the
  production fleet (head hash == public explorer) **and** the fork-boundary
  ranges (V2 switch, EIP-1559). `scripts/validate-canonical-parity.sh`.

## Known gaps to be aware of (don't mistake for working features)

`gen_header_json.go` may be stale (so `eth_getBlockByNumber` omits
`validators/validator/penalties` — re-run `go generate ./core/types/`); the
`web3ext` console namespace casing (`xdpos` vs `XDPoS`) can mismatch the service;
`EventAPI` (`xdpos_subscribe`) may be defined but unregistered; the `xdc_*`
console namespace may be dead. Masternode block production (the full BFT
dispatcher, `miner/xdc_agent.go`) is a separate workstream (M8) — a sync node
doesn't need it; escalate before implementing it.

## To peer with the production fleet (sync subsystem)

`UseXDCPing=true` (discovery ping = packet **type 5**); advertise `eth` with
version **XDPOS2=100 first**, `protocolLengths[100]=227`; handshake via
`StatusPacket62` with **no ForkID** (match NetworkID + Genesis only). The
handshake `TD` is **not a block number** — it's a peer-ranking metric; caught-up
is tested by **hash equality**. The downloader state pivot is *always*
`head − fsMinFullBlocks (=64)` — pass the peer's **true tip** as `BeaconSync(head)`
and the QC-trusted ancestor (`peer_tip − 1024`) as `final`, never the reverse.
`xdcSyncer` (a continuous ticker) must run, or post-merge `BeaconSync` idles
forever after one batch.
