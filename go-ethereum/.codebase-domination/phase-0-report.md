# Phase 0 — Bootstrap (CLOSED)

**Orchestrator:** all preconditions captured. No hedging.

## Environment
- **CWD:** `/Users/anilchinchawale/github/XDCNetwork/deVTest/go-ethereum`
- **Branch:** `fix/pathdb-rlp-import`
- **Head:** `0ec7aec12 docs: audit of v1.17.3-new files — none contain the issue #798 bug`
- **Remote:** `origin=https://github.com/XDCIndia/go-ethereum/` (gh authenticated), `upstream=ethereum/go-ethereum`
- **Working tree:** clean
- **Git user:** AnilChinchawale (anil@xinfin.org)

## Stack (Codebase Onboarding Engineer)
- Go 1.24.5 darwin/arm64 (module declares 1.24.0)
- 18 main packages under `cmd/`: geth, devp2p, ethkey, rlpdump, clef, ethereum, era, evm, fetchpayload, extract-checkpoint-state, clearxdpos, p2psim, faucet, abigen, blsync, signify, utils, plus miner/stress
- 444 `*_test.go` files
- Build: `make geth` (writes `build/bin/geth`) via `build/ci.go`
- Test runner: `go test`
- Dependencies: 3 direct `require` clauses in `go.mod`, 555 lines in `go.sum`
- Genesis configs: Apothem (chain 51), XDC mainnet (chain 50), plus Ethereum mainnets in `params/config.go`

## Test baseline (LSP/Index Engineer)
Touched-package suite (`./common/... ./params/ ./consensus/misc/eip1559/ ./core/vm/ ./core/txpool/{,legacypool,blobpool} ./internal/web3ext/`):
- **Result:** all PASS
- **Wall-clock:** 22 seconds
- **Slowest:** `core/txpool/blobpool` 18.6s, `core/txpool/legacypool` 10.3s, `core/vm` 7.2s

**Pre-existing test failures** (logged for Phase 6, not blocking):
- `core/bintrie_witness_test.go:58` and `core/genesis_test.go:297` reference `params.BlobScheduleConfig.UBT` field which doesn't exist on the struct — upstream rebase fallout from commits `aaa2b6628` (EIP-7981) + `a15778c52` (verkle trie node grouping). Already documented in `docs/port/HANDOFF.md`.
- `core/genesis_test.go::TestGenesisHashes` fails on upstream mainnet/sepolia/holesky/hoodi genesis hashes. XDC's `core/genesis.go patchMissingForkBlocks` is gated to chain IDs 50/51, so the cause is something else and these are not blocking XDC operation. Block 1 of Apothem hash matches canonical 38/38 in parity validation.

## Tracking infrastructure
- **GitHub:** 18+ open issues. Master: **#799** — "Operation: Codebase Domination — Master" (created this phase)
- **ADR dir:** `docs/03-architecture-decisions/` created; existing `docs/adr/001-genesis-config-merge.md`
- **Runbook dir:** `docs/08-runbooks/` created
- **Swarm scratchpad:** `.codebase-domination/` created
- **`ISSUES.md` template:** unnecessary — GitHub tracker active

## Existing docs (recon)
14 files under `docs/port/`. Notable:
- `HANDOFF.md` — entry point
- `PENDING_PORTS_FROM_XDC_NETWORK.md` — file audit
- `BUG_HUNT_PLAN.md` — issue #798 investigation record (8 hypotheses ruled out)
- `V117_FILE_AUDIT.md` — 14 v1.17.3-new files audited, none contain the bug
- 4 interactive HTMLs: `XDC_CONSENSUS_CHANGES.html`, `XDC_PORT_STATUS.html`, `XDC_PORTING_PLAYBOOK.html`, `XDC_DEEP_DIFF.html`

## Active P0 (carried forward, not from this swarm session)
**Issue #798** — block 82,219,687 stateRoot divergence on Apothem. Branch produces `0x5d4bf47a...`, canonical produces `0x17e9ce53...`. Multiple instrumented binaries narrowed but did not isolate. **Production deploy is blocked on this.**

## Phase 0 exit criteria — VERIFIED
- [x] `pwd`, `git status`, `git log -20`, `git remote -v` run
- [x] Stack identified with evidence
- [x] Test suite baseline recorded (22s on touched packages; full suite not run — go-ethereum full test is 30-60+ min, would block this session)
- [x] `docs/` exists, `.codebase-domination/` created
- [x] Master tracking issue opened: **#799**

## Orchestrator notes for Phase 1
- The codebase is mid-port: v1.17.3 upstream base + XDC overlay. Real architecture has been documented in `docs/port/XDC_CONSENSUS_CHANGES.html`, `XDC_PORTING_PLAYBOOK.html`, `XDC_PORT_STATUS.html`. Phase 1 should LEVERAGE these, not duplicate them, and audit for gaps.
- The active P0 bug (#798) is the highest-leverage item for Phase 9 (Execution). Skipping ahead to attack it would short-circuit the swarm spec, but it must remain the orchestrator's #1 priority.
- TODO/FIXME/HACK grep is queued for Phase 1 step 5.
