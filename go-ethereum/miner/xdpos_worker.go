// Copyright 2024 XDC Network
// XDPoS V2 block-production worker.
//
// PR-B of A.97-M: implements the XDPoS V2 minting loop as a parallel,
// standalone worker that coexists with the upstream payload-builder Miner.
// The payload-builder is idle on XDC (Engine API not used), so there is no
// conflict.  This file owns Task 5 of the A.97-M design brief.
//
// CONSENSUS-CRITICAL: see design Q7 — worker sets ONLY header.Coinbase before
// Prepare().  Extra / Validator / Validators / Penalties / Difficulty / Time /
// Root are all engine-owned.  Do NOT mutate them after engine.Prepare returns.
//
// Safety gates (design Q5):
//   1. eth.Synced() — refuse to seal until the local head is caught up.
//   2. core.XdcBulkSyncMode.Load() — refuse during bulk state import.
//   3. engine.Prepare returns ErrNotReadyToMine/ErrNotReadyToPropose — skip round.
//
// Channel rules (design Q2):
//   - Worker is the sole consumer of minePeriodCh and newRoundCh.
//   - Worker drains both channels even when !mining so producers never pile up.
//   - Worker NEVER closes these channels; the engine is a singleton that
//     outlives miner restarts and would panic on a next send to a closed chan.
//
// References:
//   XDPoSChain/miner/worker.go update()      :270-382
//   XDPoSChain/miner/worker.go getResetTime  :384-395
//   XDPoSChain/miner/worker.go commitNewWork :600+
//   A.97-M design Tasks 5-7, Q1-Q7

package miner

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	engine_v2 "github.com/ethereum/go-ethereum/consensus/XDPoS/engines/engine_v2"
	xdposutils "github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/contracts"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// XdcBroadcaster is the subset of the eth handler that the XdcWorker needs.
// Avoids a circular import while still letting the worker gossip its blocks.
type XdcBroadcaster interface {
	BroadcastBlock(block *types.Block)
}

// XdcSynced is the minimal interface to the eth.Ethereum backend needed for
// sync-state querying.  eth.Ethereum itself satisfies this.
type XdcSynced interface {
	Synced() bool
}

// XdcWorker is the XDPoS V2 block-production loop.
// Constructed by eth/backend.go (PR-B Task 6) and started via StartMining.
// Stopped via StopMining.  Never starts without Synced() returning true.
type XdcWorker struct {
	// engine wrapper — used for Prepare, HandleProposedBlock, IsV2Block.
	engine *XDPoS.XDPoS

	// v2engine — used for FindParentBlockToAssign, IsEpochSwitch, Finalize, Seal.
	v2engine *engine_v2.XDPoS_v2

	// miner gives us makeEnv / fillTransactions / config.Recommit / chain access.
	miner *Miner

	// chainHeadCh receives ChainHeadEvent from the blockchain.
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub interface{ Unsubscribe() }

	// BFT-channel endpoints owned by eth.Ethereum (design Q2 — never close).
	minePeriodCh chan int
	newRoundCh   chan types.Round

	// broadcaster wired to handler.BroadcastBlock.
	broadcaster XdcBroadcaster

	// synced reports whether the local head is caught up.
	synced XdcSynced

	// coinbase is set once at start(); never mutated after that.
	coinbase common.Address

	// lastParentBlockCommit holds the hex of the parent we most recently
	// attempted to build on.  Deduplicates rapid double-fires on the same tip
	// (design Q5 double-mint dedupe).
	lastParentBlockCommit string

	// mining is 1 when the loop should seal blocks.
	mining atomic.Int32

	// quit signals update() to exit.
	quit chan struct{}

	// accountManager and txPool are used by broadcastSigningTx to submit
	// the masternode signing transaction to BlockSigners (0x…0089) every
	// MergeSignRange (15) blocks.  Nil-safe: if either is nil, the signing
	// tx is skipped with a warning (e.g. non-mining observer nodes).
	// Ported from XDPoSChain/miner/worker.go:497 + contracts/utils.go:64.
	accountManager *accounts.Manager
	txPool         contracts.TxPoolAdder
}

// NewXdcWorker constructs (but does not start) the XDPoS V2 production worker.
// Called from eth/backend.go New() (Task 6).
//
// accountManager and pool are used to broadcast the masternode signing
// transaction to BlockSigners (0x…0089) every MergeSignRange (15) blocks.
// Pass nil for either to disable signing-tx emission (observer/non-mining nodes).
func NewXdcWorker(
	xdposEngine *XDPoS.XDPoS,
	v2Engine *engine_v2.XDPoS_v2,
	m *Miner,
	minePeriodCh chan int,
	newRoundCh chan types.Round,
	broadcaster XdcBroadcaster,
	synced XdcSynced,
	accountManager *accounts.Manager,
	pool contracts.TxPoolAdder,
) *XdcWorker {
	return &XdcWorker{
		engine:         xdposEngine,
		v2engine:       v2Engine,
		miner:          m,
		minePeriodCh:   minePeriodCh,
		newRoundCh:     newRoundCh,
		broadcaster:    broadcaster,
		synced:         synced,
		quit:           make(chan struct{}),
		accountManager: accountManager,
		txPool:         pool,
	}
}

// Start activates the mining loop with the given coinbase address.
// Safe to call only once per XdcWorker instance.
func (w *XdcWorker) Start(coinbase common.Address) {
	w.coinbase = coinbase
	w.mining.Store(1)
	go w.update()
	log.Info("XDPoS2: block-production worker started", "coinbase", coinbase)
}

// Stop signals the update() loop to exit.  Does NOT close any channels.
func (w *XdcWorker) Stop() {
	w.mining.Store(0)
	close(w.quit)
	log.Info("XDPoS2: block-production worker stopped")
}

// update is the main production loop.  Mirrors XDPoSChain worker.update().
//
// Timer logic (mirrors XDPoSChain miner/worker.go:270-382):
//   - Start with period=2 s (conservative default before minePeriodCh fires).
//   - minePeriodCh updates the period; the timer resets.
//   - On each tick / newRoundCh / chainHeadCh: commitNewWork() if mining.
//   - Channels are drained even when not mining (design Q2 drain rule).
func (w *XdcWorker) update() {
	w.chainHeadCh = make(chan core.ChainHeadEvent, 10)
	w.chainHeadSub = w.miner.chain.SubscribeChainHeadEvent(w.chainHeadCh)
	defer w.chainHeadSub.Unsubscribe()

	minePeriod := 2
	timeout := time.NewTimer(time.Duration(minePeriod) * time.Second)
	defer timeout.Stop()

	// Decouple timer-reset requests from the main select.
	resetCh := make(chan time.Duration, 1)
	// Tick notifications from the inner timer goroutine.
	c := make(chan struct{}, 1)
	finish := make(chan struct{})
	defer close(finish)

	// Timer management goroutine — mirrors XDPoSChain worker.go:291-311.
	go func() {
		for {
			select {
			case d := <-resetCh:
				if !timeout.Stop() {
					select {
					case <-timeout.C:
					default:
					}
				}
				timeout.Reset(d)
			case <-timeout.C:
				select {
				case c <- struct{}{}:
				default:
				}
			case <-finish:
				return
			}
		}
	}()

	for {
		select {
		case v := <-w.minePeriodCh:
			// Engine published a new min-period; update the timer.
			log.Info("XDPoS2: mine period updated", "period", v)
			minePeriod = v
			resetCh <- time.Duration(minePeriod) * time.Second

		case <-c:
			// Periodic tick.
			if w.mining.Load() == 1 {
				w.commitNewWork()
			}
			resetCh <- getResetTimeXDC(w.miner.chain, minePeriod)

		case ev := <-w.chainHeadCh:
			// New canonical head arrived; try our next slot.
			if w.mining.Load() == 1 {
				w.commitNewWork()
			}
			// IMPORT-TIME SIGN-TX (Fable advisory port from V1 fetcher.signHook):
			// Mirror XDPoSChain/eth/backend.go:270-286 — when a new head lands
			// that we DIDN'T mint, opportunistically broadcast a sign-tx for it
			// if our coinbase is a registered V2 candidate (NextEpochCandidates).
			// This is the comeback unblocker: a candidate that's penalized and
			// can't mint still accumulates sign-tx records via imported blocks,
			// proving liveness and clearing the comeback penalty at the next
			// epoch switch. Refs #894.
			if ev.Header != nil && w.mining.Load() == 1 {
				if block := w.miner.chain.GetBlock(ev.Header.Hash(), ev.Header.Number.Uint64()); block != nil {
					w.maybeBroadcastImportedSignTx(block)
				}
			}
			resetCh <- getResetTimeXDC(w.miner.chain, minePeriod)

		case <-w.newRoundCh:
			// Engine advanced to a new BFT round via TC/QC; try to mint.
			// Do NOT reset lastParentBlockCommit here — XDPoSChain does not
			// reset it on NewRoundCh (XDPoSChain miner/worker.go:334-337).
			// The dedupe is intentional: if highestQC hasn't advanced, we
			// should not try to mint on the same parent again.  When the QC
			// or a peer's block advances the chain, FindParentBlockToAssign
			// returns a new parent, naturally clearing the dedupe.
			if w.mining.Load() == 1 {
				w.commitNewWork()
			}
			resetCh <- getResetTimeXDC(w.miner.chain, minePeriod)

		case <-w.quit:
			return
		}
	}
}

// getResetTimeXDC computes how long to wait before the next mining attempt.
// Mirrors XDPoSChain miner/worker.go:384-395.
//
// Deviation from XDPoSChain: header.Time is uint64 (not *big.Int) in this fork.
func getResetTimeXDC(chain *core.BlockChain, minePeriod int) time.Duration {
	minePeriodDuration := time.Duration(minePeriod) * time.Second
	currentBlockTime := int64(chain.CurrentBlock().Time) // uint64 → int64 safe until year 2554
	nowTime := time.Now().UnixMilli()
	resetTime := time.Duration(currentBlockTime)*time.Second + minePeriodDuration - time.Duration(nowTime)*time.Millisecond
	if resetTime > minePeriodDuration || resetTime <= 0 {
		resetTime = minePeriodDuration
	}
	log.Debug("XDPoS2: worker timer reset",
		"resetMs", resetTime.Milliseconds(),
		"periodSec", minePeriod,
		"blockTimeSec", fmt.Sprintf("%d", currentBlockTime),
		"nowSec", fmt.Sprintf("%d.%03d", nowTime/1000, nowTime%1000))
	return resetTime
}

// commitNewWork attempts to build, seal, insert, and broadcast a new block.
// Safety gates are checked in order (design Q5):
//  1. mining flag
//  2. eth.Synced()
//  3. core.XdcBulkSyncMode
//  4. v2engine.FindParentBlockToAssign (derives parent from highest QC)
//  5. lastParentBlockCommit dedupe
//  6. engine.Prepare (yourturn + coinbase==signer + Extra/Difficulty/Time)
//
// CONSENSUS INVARIANT (Q7): worker sets ONLY header.Coinbase pre-Prepare.
// All other header fields are set exclusively by engine Prepare/Finalize/Seal.
// xdcGasCeil returns the CalcGasLimit target for minted blocks. XDC runs at a
// fixed gas-limit plateau of 420,000,000 — verified live on BOTH mainnet
// (block 103,918,257) and apothem (block 83,228,214). This value is HARD-CODED
// (XDCBlockGasLimit) and intentionally IGNORES --miner.gaslimit / config.GasCeil:
// an operator must never be able to start a minter with a wrong ceiling and hone
// the chain off its plateau. That misconfiguration is exactly what halted public
// apothem at block 83,088,791 (#952): the upstream geth default GasCeil
// (60,000,000) made core.CalcGasLimit(parent=420M, 60M) drop the limit one max
// step (~parent/1024) every block until a successor block went out-of-bound.
//
// Safety guard: never target BELOW the parent's established plateau. If a chain
// is somehow already above 420M, hold it there rather than honing DOWN (a down-
// step is what wedged #952). In steady state parent == 420M and this returns
// XDCBlockGasLimit, which core.CalcGasLimit holds flat.
func (w *XdcWorker) xdcGasCeil(parentGasLimit uint64) uint64 {
	if parentGasLimit > params.XDCBlockGasLimit {
		return parentGasLimit
	}
	return params.XDCBlockGasLimit
}

// noHeadStateWarn rate-limits the Gate 3.1 (checkpointSyncNoState) mint-skip
// warning. Gate 3.1 sits upstream of every other V2 mint check, so when it is
// latched the node produces no "yourturn" log lines at all and looks like a
// healthy but perpetually out-of-turn follower. Logging it at Debug made that
// state invisible at the default --verbosity 3 for thousands of blocks.
var noHeadStateWarn struct {
	lastUnixNano atomic.Int64 // time of the last emitted warning
	suppressed   atomic.Int64 // rounds skipped since that warning
}

// noHeadStateWarnInterval is the minimum spacing between Gate 3.1 warnings.
// The XDC import path can call commitNewWork several times per height (netv12
// re-imported the same block 5x), so an unthrottled Warn would multiply.
const noHeadStateWarnInterval = 60 * time.Second

// warnNoHeadStateMintSkip emits a rate-limited Warn that minting is disabled
// because the head state is absent, reporting how many rounds were skipped
// since the previous warning so the magnitude of the wedge is visible.
func warnNoHeadStateMintSkip() {
	now := time.Now().UnixNano()
	last := noHeadStateWarn.lastUnixNano.Load()
	if last != 0 && now-last < int64(noHeadStateWarnInterval) {
		noHeadStateWarn.suppressed.Add(1)
		return
	}
	// Claim the slot; if another goroutine won the race, just count this round.
	if !noHeadStateWarn.lastUnixNano.CompareAndSwap(last, now) {
		noHeadStateWarn.suppressed.Add(1)
		return
	}
	skipped := noHeadStateWarn.suppressed.Swap(0)
	log.Warn("XDPoS2: MINTING DISABLED — head state not available (checkpointSyncNoState latched); "+
		"this node cannot seal and will not log 'yourturn'. The chain DB must be resynced from genesis; "+
		"clearing the flag alone will not rebuild the missing state",
		"roundsSkippedSinceLastWarn", skipped)
}

func (w *XdcWorker) commitNewWork() {
	// Gate 1: mining flag.
	if w.mining.Load() != 1 {
		return
	}

	// Gate 2: network sync (design Q5 gate 1).
	if !w.synced.Synced() {
		log.Info("XDPoS2: network syncing, will mint when at tip")
		return
	}

	// Gate 3: bulk XDC sync mode (design Q5 gate 2).
	if core.XdcBulkSyncMode.Load() {
		log.Debug("XDPoS2: bulk sync mode active, skipping mint")
		return
	}

	// Gate 3.1 (#1037): fast-sync state incomplete. A node that fast-synced without
	// a full trie download (checkpointSyncNoState=true) has no real head state — block
	// headers have real state roots, but the trie DB only holds EmptyRootHash for
	// post-pivot blocks. makeEnv → StateAtForkBoundary would return "missing trie node".
	// Refuse to seal until snap-heal completes and CheckpointSyncNoStateActive is cleared.
	if core.CheckpointSyncNoStateActive.Load() {
		// A miner that has been silently disabled for thousands of blocks must say so
		// at default verbosity. This gate sits UPSTREAM of the YourTurn check, so when
		// it fires there are no "yourturn" lines at all and the node looks merely
		// out-of-turn rather than switched off. Rate-limited because the XDC import
		// path can re-enter commitNewWork several times per height.
		warnNoHeadStateMintSkip()
		return
	}

	// Gate 3.5 (#951): fork-recovery pause. While the xdcSyncer is rolling back
	// off a fork (repeated ErrUnknownAncestor), do NOT seal — minting on the
	// rolled-back tip recreates the fork and deadlocks the rollback loop. The
	// flag clears on the next successful canonical import, after which the miner
	// resumes. This applies to both the V1 and V2 sealing paths below.
	if core.XdcForkRecovering.Load() {
		log.Debug("XDPoS: fork-recovery in progress, skipping mint")
		return
	}

	// Gate 3.6 (#951 oscillation): post-rollback cooldown. Clearing XdcForkRecovering
	// on the first successful import is not enough — the node can still be far behind
	// the network tip and will re-mint its (already-superseded) in-turn block the moment
	// it re-reaches the contested height, recreating the fork (devnet-5151: head bounced
	// 143↔784, never past 785, with 265 rollbacks). While the node is actively catching
	// up, rollbacks recur and keep refreshing XdcLastForkRollbackUnix, so this gate holds
	// minting off until rollbacks STOP for the full cooldown (= caught up to a stable tip).
	// An idle/paused network produces no rollbacks, so the cooldown never engages there.
	if last := core.XdcLastForkRollbackUnix.Load(); last > 0 {
		if since := time.Now().Unix() - last; since < xdcForkRollbackCooldownSecs {
			log.Debug("XDPoS: post-rollback cooldown, skipping mint", "sinceRollbackSec", since, "cooldownSec", xdcForkRollbackCooldownSecs)
			return
		}
	}

	// V1 dispatch (#190): blocks at or before the V2 switch are sealed by the
	// V1 Clique-style PoA round-robin engine, NOT the V2 BFT worker. The V2
	// engine's FindParentBlockToAssign / Initial paths require a V2 QC anchor
	// that does not exist pre-switch, so on a V1-phase chain they fail with
	// "V2 gap header not in chain DB" and the node never produces. Legacy
	// XDPoSChain mints every block (V1 and V2); to match that, route pre-switch
	// blocks to commitV1Work which drives the wrapper's V1 YourTurn→Prepare→
	// Seal path. The switch block itself (num == SwitchBlock) is V1-format
	// (IsV2Block is num > SwitchBlock), so it is included in the V1 range.
	if v2 := w.miner.chainConfig.XDPoS; v2 != nil && v2.V2 != nil && v2.V2.SwitchBlock != nil {
		head := w.miner.chain.CurrentBlock()
		if head != nil {
			nextNum := new(big.Int).Add(head.Number, common.Big1)
			if nextNum.Cmp(v2.V2.SwitchBlock) <= 0 {
				parent := w.miner.chain.GetBlock(head.Hash(), head.Number.Uint64())
				if parent != nil {
					w.commitV1Work(parent)
				}
				return
			}
		}
	}

	// Gate 4: find parent from highest QC (design Q1 port map).
	if w.v2engine == nil {
		log.Warn("XDPoS2: v2engine not wired, cannot mint")
		return
	}
	chainReader := consensus.ChainReader(w.miner.chain)
	parent := w.v2engine.FindParentBlockToAssign(chainReader)
	if parent == nil {
		// HighestQC block not yet in our DB.
		// XDPoSChain consensus/XDPoS/XDPoS.go:482-493 falls back to currentBlock
		// when EngineV2 returns nil.  We do the same.
		curHeader := w.miner.chain.CurrentBlock()
		if curHeader == nil {
			log.Warn("XDPoS2: FindParentBlockToAssign returned nil and canonical head is nil")
			return
		}
		parent = w.miner.chain.GetBlock(curHeader.Hash(), curHeader.Number.Uint64())
		if parent == nil {
			log.Warn("XDPoS2: FindParentBlockToAssign returned nil, canonical head block not in DB",
				"number", curHeader.Number, "hash", curHeader.Hash())
			return
		}
		log.Debug("XDPoS2: FindParentBlockToAssign returned nil, using canonical head",
			"number", parent.NumberU64(), "hash", parent.Hash())
	}

	// Canonical wiring: XDPoSChain guarantees engine.Initial() runs before the
	// first Prepare() via two mechanisms this port dropped:
	//   (1) eth/backend.go:222 — engine.Initial(blockchain, CurrentHeader()) at startup
	//   (2) miner/worker.go:630 — YourTurn() (lazy-init gated) called before Prepare()
	// Without this, highestQuorumCert stays zero-valued {Hash:0x000..} at cold start
	// on V2-from-genesis chains (switchBlock=0).  Prepare()'s parent-hash check then
	// fails because genesis.Hash() != 0x000..., returning ErrNotReadyToPropose.
	//
	// Restoration: mirror canonical XDPoSChain/eth/backend.go:222 + worker.go:630 by
	// calling Initial() here, after parent is resolved.  Initial() is idempotent
	// (guarded by isInitialized flag in engine.go:313-317): real init once
	// (synthesises genesis QC at switchBlock=0), cheap no-op on every subsequent call.
	// On V1-first chains (apothem/mainnet) parent is the V2 tip, so Initial() takes
	// the else-branch and extracts QC from the tip header — same as canonical.
	// Canonical ref: XDPoSChain/consensus/XDPoS/engines/engine_v2/engine.go:188-206
	// (genesis-QC synthesis), engine.go:313-317 (idempotent guard).
	// Fixes: github.com/XDCIndia/go-ethereum/issues/936
	if err := w.v2engine.Initial(chainReader, parent.Header()); err != nil {
		log.Warn("XDPoS2: engine Initial failed, will retry next round",
			"err", err, "parent", parent.NumberU64())
		return
	}

	// Anti-fork guard: if highestQC still points to genesis (block 0) but the
	// canonical chain already has a block from a peer, do NOT mint a competing
	// block-1.  Minting would create a permanent BFT fork in a 3-node devnet
	// where QC requires all 3 votes.  Instead, wait for a QC (which advances
	// highestQC and makes FindParentBlockToAssign return a non-genesis parent)
	// or for chainHeadCh to re-trigger once votes arrive.
	//
	// This matches the implicit safety in XDPoSChain: in production all nodes
	// are wired from genesis so round-1 QC forms before TC fires.  Devnet
	// manual peer wiring introduces latency that this guard compensates for.
	curCanonicalNum := w.miner.chain.CurrentBlock().Number.Uint64()
	if parent.NumberU64() == 0 && curCanonicalNum > 0 {
		// highestQC is genesis, but canonical head is ahead — peers already minted.
		// Skip until QC advances to match the canonical head.
		log.Debug("XDPoS2: skipping mint — canonical head ahead of highestQC, waiting for QC",
			"canonicalNum", curCanonicalNum, "parent", parent.NumberU64())
		return
	}

	// Gate 5: lastParentBlockCommit dedupe (mirrors XDPoSChain worker.go:619-621).
	parentHex := parent.Hash().Hex()
	if parentHex == w.lastParentBlockCommit {
		log.Debug("XDPoS2: already attempted block on this parent, waiting for new round",
			"parent", parent.NumberU64())
		return
	}

	// Build the header skeleton.  Worker sets ONLY Coinbase (Q7.6).
	// Number and ParentHash are structural, not consensus-sensitive.
	// All other fields (Extra, Time, Difficulty, Validators/Penalties) will be
	// set by engine.Prepare below.
	// GasLimit must be set here; engine.Prepare does not touch it.
	// Target the genesis gas limit (XDC pins the limit there — see xdcGasCeil),
	// NOT the upstream miner.GasCeil default, which would hone the limit off the
	// network plateau and wedge the chain (issue #952).
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		Coinbase:   w.coinbase,
		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.xdcGasCeil(parent.GasLimit())),
	}

	// EIP-1559: set BaseFee on every block for which the fee market is active.
	// Mirrors miner/worker.go:294-296 (standard Geth worker), but gated on
	// eip1559.FeeMarketActive rather than IsLondon: XDPoSChain splits the London
	// EVM upgrade (LondonBlock) from the fee market (Eip1559Block), and an XDC
	// private net commonly has London active at block 0 with no Eip1559Block at
	// all. Stamping BaseFee there would put a field in the header that the rest of
	// the net — including the legacy XDPoSChain validators and this node's own
	// IsEIP1559-gated verifyHeader — rejects as "invalid baseFee ... want <nil>",
	// so every block we mint would be thrown away. Conversely, when the fee market
	// IS active, omitting it means InsertChain rejects the block with
	// "header is missing baseFee". Refs ethOne#62.
	if eip1559.FeeMarketActive(w.miner.chainConfig, header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(w.miner.chainConfig, parent.Header())
	}

	// Gate 6: engine.Prepare.
	// Sets: Extra (Round+QC), Difficulty, Time, Validators/Penalties (epoch),
	// MixDigest={}.  Also validates Coinbase==signer and calls yourturnAligned.
	// ErrNotReadyToMine / ErrNotReadyToPropose → skip this round (not fatal).
	if err := w.engine.Prepare(w.miner.chain, header); err != nil {
		if xdcSkipRound(err) {
			log.Debug("XDPoS2: Prepare says skip round", "err", err, "block", header.Number)
			return
		}
		log.Error("XDPoS2: Prepare failed", "err", err, "block", header.Number)
		return
	}

	// Mark this parent as attempted AFTER Prepare succeeds.
	// (If Prepare returned ErrNotReadyToPropose we do NOT mark it, allowing
	// a retry once the QC advances and Prepare might succeed.)
	w.lastParentBlockCommit = parentHex

	// Detect epoch switch to decide whether to include transactions.
	// Mirrors XDPoSChain worker.go:719-727; design Q5.
	isEpochSwitch, _, epochErr := w.v2engine.IsEpochSwitch(header)
	if epochErr != nil {
		log.Error("XDPoS2: IsEpochSwitch failed", "err", epochErr)
		return
	}

	// Build the execution environment on top of the parent state.
	// makeEnv calls chain.StateAtForkBoundary(parent, header) internally — the
	// same state base that InsertChain's Process() will use on re-execution.
	// (Design Q7.9: Finalize is called once here; InsertChain re-executes and
	// must reproduce the same root.  It will, because HookReward is deterministic.)
	env, err := w.miner.makeEnv(parent.Header(), header, w.coinbase, false)
	if err != nil {
		log.Error("XDPoS2: makeEnv failed", "err", err, "block", header.Number)
		return
	}
	defer env.discard()

	// XDC: resolve the TRC21 fee recipient (masternode candidate-owner) ONCE
	// from the pre-tx state, mirroring core/state_processor.go Process. The
	// miner's applyTransaction uses this to route fees and dispatch special txs
	// (signing → ApplySigningTransaction) exactly as InsertChain re-execution
	// will — without it the minted block's state root diverges and the producer
	// rejects its own block (#952 / apothem 83,088,792 "invalid merkle root").
	env.coinbaseOwner = state.GetCandidateOwner(env.state, w.coinbase)

	if !isEpochSwitch {
		// Fill transactions from the txpool (fork's EIP-1559/4844-aware path).
		// This reuses the fork's implementation rather than reimplementing tx
		// selection (design Q1 "reuse fork primitives").
		ctx := context.Background()
		interrupt := new(atomic.Int32)
		recommitTimer := time.AfterFunc(w.miner.config.Recommit, func() {
			interrupt.Store(commitInterruptTimeout)
		})
		defer recommitTimer.Stop()
		if fillErr := w.miner.fillTransactions(ctx, interrupt, env); fillErr != nil {
			if fillErr.Error() != errBlockInterruptedByTimeout.Error() {
				log.Error("XDPoS2: fillTransactions failed", "err", fillErr)
				return
			}
			// Timeout interrupt is normal — block is ready with what we have.
		}
	}

	// Finalize: compute state root, fire HookReward at epoch switch,
	// return assembled *types.Block.  Called ONCE (Q7.8).
	//
	// V2 Finalize signature: (chain, header, state, parentState, txs, uncles, receipts)
	// We pass a fresh parentState for HookReward (needed for correct reward calc).
	parentState, psErr := w.miner.chain.StateAtForkBoundary(parent.Header(), header)
	if psErr != nil {
		log.Error("XDPoS2: StateAtForkBoundary for parentState failed", "err", psErr)
		return
	}
	assembled, finErr := w.v2engine.Finalize(
		chainReader, header, env.state, parentState,
		env.txs, nil, env.receipts,
	)
	if finErr != nil {
		log.Error("XDPoS2: Finalize failed", "err", finErr, "block", header.Number)
		return
	}

	// Seal: synchronous V2 seal writes header.Validator (ECDSA signature).
	// Q7.2: worker does NOT pre-size or touch header.Validator.
	// Pass w.quit as stop channel so we abort cleanly on shutdown.
	sealed, sealErr := w.v2engine.Seal(chainReader, assembled, w.quit)
	if sealErr != nil {
		if xdcSkipRound(sealErr) {
			log.Debug("XDPoS2: Seal says skip round", "err", sealErr)
			return
		}
		log.Error("XDPoS2: Seal failed", "err", sealErr, "block", header.Number)
		return
	}
	if sealed == nil {
		// quit was closed while Seal was running.
		return
	}

	w.commitMinedBlock(sealed)
}

// commitV1Work seals a single V1 (Clique-style PoA round-robin) block on top of
// parent, when the chain is still in the V1 phase (next block <= V2 switch).
// It drives the wrapper engine's V1 path exactly as the upstream geth miner
// would, mirroring XDPoSChain/miner/worker.go's V1 sealing loop:
//
//	YourTurn → Prepare → makeEnv → fillTransactions → FinalizeAndAssemble → Seal
//
// Unlike the V2 path it does NOT touch the V2 engine (no FindParentBlockToAssign,
// no Initial, no QC) — those are meaningless before the switch. The wrapper's
// Prepare sets Coinbase (zero or a vote address), Difficulty (in/out-of-turn),
// Extra (vanity + epoch-checkpoint signer list + seal space) and Time
// (parent.Time + Period); Seal signs with the Authorize'd key and returns the
// sealed block asynchronously on a results channel.
//
// Refs #190 (full legacy parity: modern mints in V1 phase too).
func (w *XdcWorker) commitV1Work(parent *types.Block) {
	chain := w.miner.chain

	// V1 round-robin turn check: (preIndex+1) % len(masternodes) == curIndex.
	// Returns (masternodesLen, preIndex, curIndex, isMyTurn, err).
	_, _, _, myTurn, err := w.engine.YourTurn(chain, parent.Header(), w.coinbase)
	if err != nil {
		log.Debug("XDPoS V1: YourTurn check failed", "err", err, "parent", parent.NumberU64())
		return
	}
	if !myTurn {
		return
	}

	// Dedupe rapid double-fires on the same parent (mirrors V2 gate 5).
	parentHex := parent.Hash().Hex()
	if parentHex == w.lastParentBlockCommit {
		log.Debug("XDPoS V1: already attempted block on this parent", "parent", parent.NumberU64())
		return
	}

	// Header skeleton. Worker sets ONLY structural fields + GasLimit + BaseFee;
	// Coinbase/Difficulty/Extra/Time are set exclusively by engine.Prepare
	// (V1 zeroes Coinbase then optionally sets a vote address — the signer is
	// recovered from the seal, not the coinbase).
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.xdcGasCeil(parent.GasLimit())),
	}
	// Gated on FeeMarketActive, not IsLondon — see the note in commitNewWork:
	// on XDC, London and the EIP-1559 fee market are separate forks, and a V1
	// block minted with a BaseFee the net does not expect is rejected by every
	// other client. Refs ethOne#62.
	if eip1559.FeeMarketActive(w.miner.chainConfig, header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(w.miner.chainConfig, parent.Header())
	}

	// #951 Defect 1c (Part C — legacy mint-gate parity): the M1 creator ALWAYS seals its
	// slot, exactly like XDPoSChain engine_v1/engine.go:933-939 / worker.go. engine.Seal
	// (xdpos.go) fills header.Validator only when this node is also its OWN assigned M2;
	// for slots where the assigned M2 is a different node, Seal leaves header.Validator
	// empty and commitMinedBlock broadcasts that empty block WITHOUT writing it to our
	// own head — the assigned M2 co-signs it in place and our xdcSyncer adopts the
	// resulting co-signed canonical (adoptCoSignedSibling, Part B). The earlier self-M2
	// defer gate here made xdc04 skip nearly every >epoch slot (M1==M2 is rare with N=4),
	// trading the churn for near-zero minting; that is NOT legacy behaviour and is removed.

	// engine.Prepare (wrapper V1 path): Coinbase, Difficulty, Extra(+signers at
	// epoch checkpoints), MixDigest, Time. Routes to V1 because the header is
	// not yet a V2 block.
	if err := w.engine.Prepare(chain, header); err != nil {
		log.Error("XDPoS V1: Prepare failed", "err", err, "block", header.Number)
		return
	}
	w.lastParentBlockCommit = parentHex

	// Build execution env on the parent state. Pass header.Coinbase (set by
	// Prepare) so the COINBASE opcode + any coinbase crediting match what
	// InsertChain's re-execution will see (consensus invariant).
	env, err := w.miner.makeEnv(parent.Header(), header, header.Coinbase, false)
	if err != nil {
		log.Error("XDPoS V1: makeEnv failed", "err", err, "block", header.Number)
		return
	}
	defer env.discard()

	// XDC: resolve the TRC21 fee recipient once before tx execution (mirrors
	// core/state_processor.go Process) so applyTransaction routes fees/special
	// txs identically to InsertChain re-execution (#952).
	env.coinbaseOwner = state.GetCandidateOwner(env.state, w.coinbase)

	// Fill transactions from the pool. V1 includes txs in every block (rewards
	// are applied in Finalize at rCheckpoint blocks, not gated on tx presence).
	ctx := context.Background()
	interrupt := new(atomic.Int32)
	recommitTimer := time.AfterFunc(w.miner.config.Recommit, func() {
		interrupt.Store(commitInterruptTimeout)
	})
	defer recommitTimer.Stop()
	if fillErr := w.miner.fillTransactions(ctx, interrupt, env); fillErr != nil {
		if fillErr.Error() != errBlockInterruptedByTimeout.Error() {
			log.Error("XDPoS V1: fillTransactions failed", "err", fillErr)
			return
		}
		// Timeout is normal — proceed with the txs gathered so far.
	}

	// FinalizeAndAssemble (wrapper V1 path): applies HookReward at reward
	// checkpoints, computes state root, returns the assembled block.
	body := &types.Body{Transactions: env.txs}
	assembled, finErr := w.engine.FinalizeAndAssemble(chain, header, env.state, body, env.receipts)
	if finErr != nil {
		log.Error("XDPoS V1: FinalizeAndAssemble failed", "err", finErr, "block", header.Number)
		return
	}

	// Seal (wrapper V1 path): async — signs and pushes the sealed block onto
	// the results channel. Bound the wait so a missed slot doesn't wedge the loop.
	results := make(chan *types.Block, 1)
	if sealErr := w.engine.Seal(chain, assembled, results, w.quit); sealErr != nil {
		log.Error("XDPoS V1: Seal failed", "err", sealErr, "block", header.Number)
		return
	}
	select {
	case sealed := <-results:
		if sealed != nil {
			log.Info("XDPoS V1: minted block", "number", sealed.NumberU64(), "hash", sealed.Hash(), "coinbase", w.coinbase)
			w.commitMinedBlock(sealed)
		}
	case <-time.After(time.Duration(w.miner.chainConfig.XDPoS.Period+2) * time.Second):
		log.Warn("XDPoS V1: Seal timed out, abandoning slot", "block", header.Number)
	case <-w.quit:
	}
}

// commitMinedBlock inserts the sealed block into the local chain, broadcasts
// it to peers, then triggers the BFT self-vote path.
// Mirrors XDPoSChain worker.wait() :432-476 (adapted for InsertChain).
func (w *XdcWorker) commitMinedBlock(block *types.Block) {
	// #951 Defect 1c (Part C — legacy co-sign handoff): above the first epoch, a block
	// we minted as M1 but whose assigned M2 is a DIFFERENT node carries an EMPTY
	// header.Validator (Seal only fills it when self==M2). Such a block is NOT the
	// canonical form — the assigned M2 must co-sign it. We BROADCAST it so the M2 can
	// co-sign in place, but we do NOT write it to our own head; our xdcSyncer then adopts
	// the resulting co-signed canonical (adoptCoSignedSibling). Writing the empty block
	// to our head is exactly what created the fork-side empty-N tip and the >900 churn.
	// Mirrors XDPoSChain worker.go (broadcast the empty block, let the M2 complete it).
	if w.miner.chainConfig.XDPoS != nil &&
		block.NumberU64() > w.miner.chainConfig.XDPoS.Epoch &&
		len(block.Header().Validator) == 0 {
		log.Info("XDPoS V1: minted empty-Validator block — broadcasting for assigned M2 to co-sign",
			"number", block.NumberU64(), "hash", block.Hash(), "coinbase", block.Coinbase())
		if w.broadcaster != nil {
			w.broadcaster.BroadcastBlock(block)
		}
		return
	}

	// Wait until the block's timestamp is no longer in the future.
	// engine.Prepare sets header.Time = parent.Time + MinePeriod (whole seconds).
	// yourturnAligned fires as soon as waitedTime >= MinePeriod, which can be
	// at sub-second precision — InsertChain's consensus.VerifyHeader rejects
	// blocks whose Time > now.Unix().  Sleep at most ~1 s.
	blockTime := time.Unix(int64(block.Time()), 0)
	if now := time.Now(); blockTime.After(now) {
		delay := blockTime.Sub(now)
		log.Debug("XDPoS2: waiting for block time", "delay", delay, "block", block.NumberU64())
		select {
		case <-time.After(delay):
		case <-w.quit:
			return
		}
	}

	n, err := w.miner.chain.InsertChain(types.Blocks{block})
	if err != nil || n != 1 {
		log.Error("XDPoS2: InsertChain failed for minted block",
			"number", block.NumberU64(), "hash", block.Hash(), "n", n, "err", err)
		return
	}
	log.Info("XDPoS2: minted block",
		"number", block.NumberU64(),
		"hash", block.Hash(),
		"txs", len(block.Transactions()),
		"coinbase", block.Coinbase())

	// Broadcast the masternode signing transaction to the BlockSigners contract
	// (0x0000…0089) every MergeSignRange (15) blocks.  This is required for:
	//   (1) reward accounting — GetSigningTxCount reads signing txs over the last epoch.
	//   (2) penalty comeback — the penalty code scans sign txs in the last 150 blocks.
	// Canonical reference: XDPoSChain/miner/worker.go:495-499.
	// TODO(#922): TIPUpgradePenalty cadence (activates at block 9,999,999,999) uses
	// MinimumSigningTx/LimitPenaltyEpoch — not implemented here; see engine_v2_hooks.go.
	w.broadcastSigningTx(block)

	// M4c: gossip NewBlockMsg to peers so they can vote (design Q4 step 5a).
	if w.broadcaster != nil {
		w.broadcaster.BroadcastBlock(block)
	}

	// M4: proposer self-processes the block via BFT path (design Q4 step 5b).
	// HandleProposedBlock: processQC → allowedToSend → verifyVotingRule →
	// sendVote → broadcastToBftChannel.
	if err := w.engine.HandleProposedBlock(w.miner.chain, block.Header()); err != nil {
		log.Warn("XDPoS2: HandleProposedBlock failed",
			"err", err, "number", block.NumberU64(), "hash", block.Hash())
	}
}

// broadcastSigningTx submits a sign(uint256 blockNumber, bytes32 blockHash)
// transaction to the BlockSigners contract every MergeSignRange (15) blocks.
//
// Canonical gate and call site: XDPoSChain/miner/worker.go:495-499:
//
//	if block.NumberU64()%common.MergeSignRange == 0 || !w.config.IsTIP2019(block.Number()) {
//	    contracts.CreateTransactionSign(w.config, w.eth.TxPool(), w.eth.AccountManager(), block, w.chainDb, w.coinbase)
//
// All mainnet/apothem chains have TIP2019Block=0 so IsTIP2019 is always true;
// the condition simplifies to block.NumberU64() % 15 == 0.
//
// FRESHNESS GATE (fixes #932): the BlockSigners contract requires at execution
// time: block.number <= _blockNumber + epoch*2 (BlockSigner.sol:21).  If this
// node is catching up (importing many blocks rapidly), the signing tx would be
// submitted for block N but by the time it is mined the chain may be at
// N + epoch*2 + k, causing a contract revert.  A reverted tx at the lowest
// pending nonce permanently jams the nonce sequence.
//
// Canonical avoids this via the fetcher.signHook (XDPoSChain/eth/backend.go:270):
// the hook only fires for blocks received via P2P announce (near-tip), not for
// bulk-sync imported blocks.  We mirror the effective behaviour by gating on
// head - block.Number <= 2*MergeSignRange (generous: ~30 blocks ~60s), which is
// well inside the epoch*2 budget and also covers the typical P2P propagation
// window.  Any block outside this window is "stale" and must not be signed.
//
// This method is nil-safe: if accountManager or txPool was not wired (observer
// node), it returns immediately with a warning.
func (w *XdcWorker) broadcastSigningTx(block *types.Block) {
	if block.NumberU64()%common.MergeSignRange != 0 {
		return
	}
	if w.accountManager == nil || w.txPool == nil {
		log.Warn("XDPoS2: signing tx skipped — account manager or txpool not wired",
			"number", block.NumberU64())
		return
	}

	// Freshness gate: only sign blocks that are close to the current chain head.
	// Uses 2*MergeSignRange as the staleness bound (30 blocks ≈ 60 s), which is
	// a strict subset of the contract's epoch*2 window.  Blocks further behind
	// the head are from a catch-up burst and must be skipped.
	chainCfg := w.miner.chain.Config()
	if chainCfg.XDPoS != nil {
		currentHead := w.miner.chain.CurrentBlock()
		if currentHead != nil {
			headNum := currentHead.Number.Uint64()
			blockNum := block.NumberU64()
			if isSigningTxStale(blockNum, headNum) {
				log.Debug("XDPoS2: signing tx skipped — block is stale (catch-up burst)",
					"blockNum", blockNum,
					"headNum", headNum,
					"lag", headNum-blockNum,
					"freshnessBound", uint64(signingFreshnessBound),
				)
				return
			}
		}
	}

	if err := contracts.CreateTransactionSign(chainCfg, w.txPool, w.accountManager, block, w.coinbase,
		chainStateNonces{w.miner.chain}); err != nil {
		log.Error("XDPoS2: failed to broadcast masternode signing tx",
			"number", block.NumberU64(), "error", err)
	}
}

// chainStateNonces adapts *core.BlockChain to contracts.StateNonceReader so the
// sign-tx nonce can be floored at the confirmed chain-head nonce instead of
// trusting a possibly-stale txpool nonce. See contracts.signTxNonce.
type chainStateNonces struct {
	chain *core.BlockChain
}

// ConfirmedNonce returns addr's nonce in the current canonical head state.
// It reports false — never an error and never a block — when that state is not
// available, because the caller holds contracts.TxSignMu and must not stall or
// fail the signing path on a state miss.
func (c chainStateNonces) ConfirmedNonce(addr common.Address) (uint64, bool) {
	if c.chain == nil {
		return 0, false
	}
	head := c.chain.CurrentBlock()
	if head == nil || !c.chain.HasState(head.Root) {
		return 0, false
	}
	statedb, err := c.chain.StateAt(head)
	if err != nil || statedb == nil {
		return 0, false
	}
	return statedb.GetNonce(addr), true
}

// maybeBroadcastImportedSignTx is the import-time signing hook (Fable advisory):
// when a new canonical head arrives that we did NOT mint, opportunistically
// broadcast a sign-tx for it IF our coinbase is in V2 NextEpochCandidates.
// Mirrors XDPoSChain/eth/backend.go:270-286 (the V1 fetcher.signHook) so
// candidates that are penalized and cannot mint can still accumulate sign-tx
// records via imported blocks, satisfying the comeback rule.
//
// Gating intentionally uses NextEpochCandidates (the FULL registered set), NOT
// the active masternodes set or IsAuthorisedAddress. A candidate is allowed to
// sign for liveness even if penalty-excluded from the active set — that's the
// whole point of the comeback path.
func (w *XdcWorker) maybeBroadcastImportedSignTx(block *types.Block) {
	if w.coinbase == (common.Address{}) {
		return
	}
	// Cheap gate first — block number must be a merge block.
	if block.NumberU64()%common.MergeSignRange != 0 {
		return
	}
	if w.engine == nil || w.engine.EngineV2 == nil {
		return
	}
	v2, ok := w.engine.EngineV2.(*engine_v2.XDPoS_v2)
	if !ok {
		return
	}
	snap, err := v2.GetSnapshot(w.miner.chain, block.Header())
	if err != nil || snap == nil {
		return
	}
	inCandidates := false
	// Use CandidatePool() (active set ∪ penalties) rather than NextEpochCandidates
	// alone so a currently-penalized coinbase still self-detects and broadcasts
	// its comeback (0x89) signing tx (matches canonical XDPoSChain).
	for _, c := range snap.CandidatePool() {
		if c == w.coinbase {
			inCandidates = true
			break
		}
	}
	if !inCandidates {
		return
	}
	// Reuse existing broadcastSigningTx — it already handles the freshness gate,
	// nil-safe wiring, and CreateTransactionSign call.
	w.broadcastSigningTx(block)
}

// signingFreshnessBound is the maximum lag (in blocks) between the chain head
// and the block being signed before the signing tx is considered stale.
// Set to 2*MergeSignRange (30 blocks ≈ 60 s), well inside the contract's
// epoch*2 window.  Exported for tests.
const signingFreshnessBound = 2 * common.MergeSignRange

// xdcForkRollbackCooldownSecs is how long the miner suppresses sealing after the
// most recent xdcSyncer fork-rollback (#951 oscillation gate). Sized to comfortably
// exceed a catch-up burst on a fast chain: while the node is still escaping a fork
// the syncer keeps rolling back and refreshing core.XdcLastForkRollbackUnix, so the
// cooldown keeps resetting and minting stays off until rollbacks cease for this full
// window (= the node is genuinely caught up to a stable tip). 60s ≈ 30 V1 periods.
const xdcForkRollbackCooldownSecs = 60

// isSigningTxStale returns true when signing a block at blockNum would produce
// a tx that is already expired (or dangerously close) by the time it is mined.
// Specifically: returns true when headNum > blockNum + signingFreshnessBound.
//
// Exported for unit testing without requiring a full chain.
func isSigningTxStale(blockNum, headNum uint64) bool {
	return headNum > blockNum && headNum-blockNum > signingFreshnessBound
}

// xdcSkipRound returns true for errors that mean "not my turn / not ready"
// and should cause commitNewWork to skip this round silently.
func xdcSkipRound(err error) bool {
	return err == xdposutils.ErrNotReadyToMine ||
		err == xdposutils.ErrNotReadyToPropose ||
		err == xdposutils.ErrAlreadyMined
}
