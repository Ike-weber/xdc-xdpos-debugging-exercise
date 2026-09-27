// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package eth

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/engines/engine_v2"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// snapsyncExperimentalEnv is the double-opt-in env var. Even with
// --syncmode snap set, BeaconSync is invoked only when this env var
// is exactly "1". Without it, SNAP_RUNNING logs + falls back to
// FULL_SYNC (Phase 3b.2 behavior). Refs #807 Phase 3b.3.
//
// This guards against accidental engagement in production: snap-sync
// writes chaindata via the downloader, and the gossip-handler race
// (NewBlockMsg arriving mid-snap) has not been fully tested. Operators
// who want to try snap-sync must explicitly set both flags.
const snapsyncExperimentalEnv = "XDC_EXPERIMENTAL_SNAPSYNC"

func snapsyncExperimentalEnabled() bool {
	return os.Getenv(snapsyncExperimentalEnv) == "1"
}

// snapHealEnv is the opt-in env var for A.97.2 snap-heal-at-tip. Default off
// so first-rollout operators are unaffected. Set XDC_SNAP_HEAL=1 to enable.
// Distinct from XDC_EXPERIMENTAL_SNAPSYNC which gates the (dead) range path.
// Refs #894.
const snapHealEnv = "XDC_SNAP_HEAL"

func snapHealEnabled() bool {
	return os.Getenv(snapHealEnv) == "1"
}

// snapHealTrustedChainIDs is the set of chain IDs for which the A.97.2/A.97.3
// snap-heal-at-tip path (real snap/1 trie download, hash-verified against
// block.Root before checkpointSyncNoState is cleared — see gate B in
// maybeStartSnapHeal) is considered safe to invoke. Originally restricted to
// mainnet(50)/apothem(51) only, out of caution after devnet-5151's unresolved
// #1800 cross-client parity divergence made a NAIVE (unverified) clear
// unsafe there. Gate B's hash verification is unrelated to #1800 and equally
// safe on any XDC chain, so this list is widened to the same chain-ID
// enumeration used elsewhere for XDC network selection (see
// common.CopyXDCConstants / XDPoS.SetNetworkConstants): mainnet, apothem, the
// XDPoSChain-main devnet (551), the XDCIndia stress devnet (5050), the local
// devnet (5151), and the legacy devnet (5551). This does not weaken any
// validation — maybeStartSnapHeal only clears the flag after downloading and
// hash-verifying the healed trie against the canonical block root.
func isSnapHealTrustedChain(chainID uint64) bool {
	switch chainID {
	case 50, 51, 551, 5050, 5151, 5551:
		return true
	default:
		return false
	}
}

// XDC pre-merge sync (refs #740).
//
// v1.17.3's BeaconSync is post-merge — after one round it stops and waits
// for a fresh forkchoiceUpdated from a consensus layer. XDC has no CL, so
// after the first batch imports the downloader goes idle forever.
//
// xdcSyncer drives sync continuously: every xdcForceSyncCycle it picks the
// best peer, fetches a batch of headers + bodies starting from our current
// chain head, combines them into types.Blocks, and calls
// blockchain.InsertChain to execute + import. This mirrors xdc-network's
// syncWithPeerXDC pre-merge sync loop in spirit (we use the existing
// eth.Peer.Request* primitives we already adapted for XDPOS2 legacy).

const (
	xdcForceSyncCycle      = 500 * time.Millisecond // dropped from 2s — bulk sync was bottlenecked by tick cadence
	xdcHeartbeat           = 30 * time.Second       // periodic head/peer log
	xdcBatchSize           = 4096                   // headers/bodies per round (XDC bulk-sync default)
	xdpos2MaxHeaderFetch   = 192                    // #971: legacy XDPOS2 peers cap GetBlockHeaders at MaxHeaderFetch; requesting more returns an empty reply (count=0). Clamp header requests to this.
	xdcReqTimeout          = 8 * time.Second        // kept for non-bulk callers: anchor repair, snap pivot, head probe
	xdcBulkReqTimeout      = 4 * time.Second        // A.75: tighter per-peer budget in parallel fan-out; fully-synced peers respond <1s
	xdcMaxPeerTriesPerSync = 8                      // A.96: raised 3→8 to compensate for ~3% XDPOS2 GetBlockHeaders serve rate on mainnet (1726 count=0 vs 55 count=192 in 2h); fan-out=3 gave 9% per-round hit rate, fan-out=8 gives ~26%. Was A.79: 4→3 to free a peer for lookahead.
	xdcMaxBackToBackRounds = 8                      // A.75: max consecutive rounds per maybeSync invocation; avoids starving heartbeat
	xdcLookaheadStreams    = 3                      // A.77: extra ranges pre-fetched concurrently from other peers while range-0 body+IC runs
	xdcLookaheadRangeSize  = 128                    // A.77: assumed peer batch size per range (protocol cap observed in practice)

	// Self-healing rollback when the local tip is stuck on a V2 BFT
	// competing-slot fork. See issue #801. The xdc-network downloader has
	// a full findAncestor binary search; until that's ported, we use a
	// fixed exponential-backoff rollback window.
	//
	// Threshold counts CONSECUTIVE failures for the same fromBlock —
	// peer ID does not matter. Empirically a single bestPeer persistently
	// serves fork-side N+1 blocks, so a "distinct peers" requirement (as
	// in the initial implementation) never fires. Use failure count.
	xdcUnknownAncestorThreshold = 8    // consecutive failures for same fromBlock before triggering rollback
	xdcRollbackWindowInit       = 128  // initial rollback (one batch); doubles each consecutive trigger
	xdcRollbackWindowMax        = 8192 // hard cap on cumulative rollback per stuck-period

	// When a peer delivers blocks that fail InsertChain with ErrUnknownAncestor
	// repeatedly, mark them as fork-tainted and exclude from bestPeer/rankedPeers
	// for xdcForkTaintTTL. Without this, bestPeer keeps re-picking the same peer
	// that put us on the fork, post-rollback sync immediately re-imports the
	// same fork-side block. Issue #802 task 2 — geth-aligned peer rotation.
	xdcForkTaintTTL = 60 * time.Second

	// Snap-sync state machine thresholds (refs #807 Phase 3b).
	//
	// Gate: only engage SNAP_SELECTING when local is behind peer by at
	// least this many blocks. Smaller gaps full-sync faster than snap
	// would (snap has fixed setup overhead). Tuned to be conservative —
	// users at-tip should never enter the snap path even with
	// --syncmode snap set.
	xdcSnapEngageMinGap = 5000

	// Fresh-node threshold: engage snap-sync only if local head is below
	// this value. XDPOS2 wire does not carry peer block number (only TD),
	// so we approximate "behind by a lot" with "still at/near genesis".
	// This guarantees no at-tip race: an at-tip node has localHead well
	// above this threshold.
	xdcSnapEngageFreshThreshold = 1000

	// Fast-sync peer-availability gating + retry constants (refs #844 A.11).
	//
	// Startup race: xdcSyncer ticks every 500ms; trusted peers can take
	// 20-60s to dial after node bootstrap. Without gating, maybeEngageFast
	// transitioned the moment a SINGLE peer connected, BeaconSync invoked
	// state download against that single peer, and faststatesync returned
	// "trie node X failed with all peers (0 tries, 0 peers)" if the peer
	// disconnected mid-flight (or wasn't actually NodeData-eligible). The
	// snapEventLoop saw SyncFailed and locked us into FULL_SYNC permanently.
	//
	// xdcFastSyncPeerWaitTimeout: max wall-clock to block fastRunningCheck
	// waiting for the minimum peer count. After timeout we proceed anyway
	// (retry loop below covers the rest).
	// A.28: reduced from 120s -> 30s. The 2-minute cold-start budget isn't
	// needed on the A.24 cooldown re-arm path since peers are already
	// connected; tightening it makes the visible log cadence match the 30s
	// A.24 cooldown.
	// xdcFastSyncMinPeers: minimum connected peers before invoking BeaconSync.
	// xdcFastSyncMaxRetries: number of BeaconSync attempts on transient
	// "0 peers" errors before giving up and falling back to FULL_SYNC.
	// xdcFastSyncRetryBackoff: sleep between BeaconSync retries.
	// xdcFastSyncPeerWaitLogEvery: throttle for the "waiting for peers" log.
	xdcFastSyncPeerWaitTimeout = 30 * time.Second
	xdcFastSyncMinPeers        = 2
	// A.20: bumped from 5 -> 10. Combined with 5s backoff the total retry window
	// stays similar (~50s vs prior ~75s) but each attempt uses a fresher pivot.
	xdcFastSyncMaxRetries = 10
	// A.20: reduced from 15s -> 5s. State-sync failures from a stale operator-pinned
	// pivot benefit from quicker retries: by the time we backoff 15s the peer's
	// head has advanced ~7-8 blocks, leaving the next pivot stale too.
	xdcFastSyncRetryBackoff     = 5 * time.Second
	xdcFastSyncPeerWaitLogEvery = 10 * time.Second

	// xdcSnapSelectMaxRetries: after this many consecutive CONCLUSIVE pivot-
	// selection failures (not transient "no peers" returns), give up on snap
	// and permanently stay in FULL_SYNC for the process lifetime. Fixes #1045.
	xdcSnapSelectMaxRetries = 10
)

// xdcSnapPhase enumerates the snap-sync state machine. Refs #807 Phase 3.
// Stored as int32 in an atomic so the heartbeat goroutine can read
// without locking. The transitions are documented in
// docs/03-architecture-decisions/807-snap-sync-phase-3-gating-design.md
type xdcSnapPhase int32

const (
	phaseFullSync xdcSnapPhase = iota
	phaseSnapSelecting
	phaseSnapRunning
	phaseFastSyncRunning // BeaconSync kicked for FastSync; waiting for completion. Refs #844 A.7.
	phaseTipCatch
	phaseAtTip
)

func (p xdcSnapPhase) String() string {
	switch p {
	case phaseFullSync:
		return "FULL_SYNC"
	case phaseSnapSelecting:
		return "SNAP_SELECTING"
	case phaseSnapRunning:
		return "SNAP_RUNNING"
	case phaseFastSyncRunning:
		return "FAST_SYNC_RUNNING"
	case phaseTipCatch:
		return "TIP_CATCH"
	case phaseAtTip:
		return "AT_TIP"
	}
	return "UNKNOWN"
}

type xdcSyncer struct {
	handler *handler
	syncing atomic.Bool
	quitCh  chan struct{}

	// Self-healing state. When InsertChain returns consensus.ErrUnknownAncestor
	// for the same fromBlock xdcUnknownAncestorThreshold times in a row, we
	// infer the local tip is on a non-canonical competing-slot block and
	// roll back. Tracked here so syncOnce stays stateless except for failures.
	stuckFrom       uint64 // fromBlock that's been failing
	stuckFailures   int    // consecutive InsertChain failures at stuckFrom
	stuckRolledBack uint64 // cumulative rollback in current stuck-period

	// Fork-tainted peers: those that delivered the block leading to a rollback.
	// Excluded from rankedPeers() for xdcForkTaintTTL so post-rollback bulk-sync
	// picks a different peer instead of re-importing the same fork.
	forkTaintedMu sync.Mutex
	forkTainted   map[string]time.Time

	// Snap-sync phase state (refs #807 Phase 3b). Default phaseFullSync;
	// transitions only when ConfigSyncMode == SnapSync AND a sizeable
	// peer-vs-local gap exists. Atomic so the heartbeat can read safely.
	snapPhase atomic.Int32

	// snapPivot is populated by snapSelectOnce when SelectSnapPivot
	// returns a safe pivot. Phase 3b.3 hands it to downloader.BeaconSync.
	// Guarded by snapPivotMu so snapPivotBlock() can read concurrently
	// from heartbeat goroutine. Refs #807 Phase 3b.2-3b.3.
	snapPivotMu sync.Mutex
	snapPivot   *engine_v2.PivotResult

	// snapBeaconStarted indicates we've invoked BeaconSync for the
	// current pivot. Prevents re-entry on every tick while snap is
	// still running in the downloader's worker goroutines.
	// Refs #807 Phase 3b.3.
	snapBeaconStarted atomic.Bool

	// fastBeaconStarted mirrors snapBeaconStarted for the FastSync path.
	// Set to true the first time BeaconSync is invoked for FastSync mode,
	// preventing re-entry while the downloader's worker goroutines are
	// running. Refs #844 A.7.
	fastBeaconStarted atomic.Bool

	// snapBeaconStartedAt is the wall-clock time BeaconSync was invoked,
	// stored as Unix nanoseconds. Used by phase 14.4 stall detection to
	// fall back to FULL_SYNC when the XDC peer fleet hasn't generated a
	// snap-protocol snapshot index (peers receive GetAccountRange and
	// respond empty, the downloader logs "Peer rejected account range",
	// and snap.Syncer.Progress().AccountSynced stays at 0).
	snapBeaconStartedAt atomic.Int64

	// Fast-sync peer-wait bookkeeping (refs #844 A.11).
	// fastWaitStartedAt is the time we first noticed insufficient peers
	// while trying to engage FastSync, stored as Unix nanoseconds. Zero
	// before the first observation. Used to enforce
	// xdcFastSyncPeerWaitTimeout without blocking the sync loop.
	// fastWaitLastLogAt throttles the "waiting for peers" log to once per
	// xdcFastSyncPeerWaitLogEvery to avoid spam at the 500ms tick cadence.
	fastWaitStartedAt atomic.Int64
	fastWaitLastLogAt atomic.Int64

	// fastEngagementResolved is set to true when fast-sync has reached a terminal
	// outcome (SyncCompleted, or unpinned timeout). Until then, fullSyncOnce is
	// gated and bulk-import is forbidden — enforces the operator's --syncmode=fast
	// contract. Refs A.15.
	fastEngagementResolved atomic.Bool

	// fastRetryArmed gates the post-SyncFailed cooldown goroutine. Set true
	// when a retry is scheduled, false when the cooldown elapses. Refs A.24.
	fastRetryArmed atomic.Bool

	// A.70 (refs #844 #859): cache of the V2 masternode-snapshot seeds emitted
	// during the initial fast-sync prefetch. fast-sync block import calls
	// engine.UpdateMasternodesFromHeader at every gap block it imports, which
	// reads contract state and overwrites our seeded snapshot with a
	// candidates-by-stake list that may diverge from header.Validators near the
	// top-N truncation boundary (sort tie-breaking). Re-applying our seed AFTER
	// SyncCompleted's overwrite restores header.Validators as authoritative.
	a70SeedMu    sync.Mutex
	a70SeedCache []a70SeedRecord

	// A.73.1: stuck-TIP_CATCH watchdog. Counts consecutive heartbeats where
	// localHead >= maxKnownPeerHead while phase == phaseTipCatch. If the
	// hash-equality check in maybeTransitionToAtTip never fires (peer churn:
	// peers disconnect / reconnect with an advanced tip before we can match
	// their hash), this counter forces the AT_TIP transition after 3 heartbeats
	// (~90 s) so XdcBulkSyncMode doesn't stay true indefinitely.
	tipCatchAtPeerTicks int

	// #199 (devnet-5151 halted-tip deadlock): on a HALTED V2 chain where every peer
	// speaks xpos2/100 (no eth/69 BlockRangeUpdate -> maxPeerHead==0) AND the local
	// head is stale (chain not producing -> headFresh==false), neither existing
	// at-tip signal fires, so a FULL_SYNC producer's miner stays gated forever and
	// the chain cannot recover (the gated validators are exactly the quorum it needs).
	// These two fields detect a head that has not advanced for haltedTipStallTicks
	// consecutive heartbeats while connected to peers and without any eth/69
	// BlockRange info — i.e. we are at the tip of an idle/halted chain — and clear
	// XdcBulkSyncMode so the miner can resume and help the network make progress.
	// Liveness-only: clearing the gate lets the miner attempt a block; it does not
	// change block contents. Cannot false-trigger during genuine bulk sync because
	// the head advances every heartbeat then (resetting the counter).
	lastFullSyncHead   uint64
	fullSyncStallTicks int

	// A.74: one-shot startup repair for nodes synced with geth-a73 or earlier.
	// PrimeFastSyncAnchor writes empty placeholder bodies for [primeBase..pivot-1];
	// the old PrimeAncestorBodies skipped overwriting them. Set true once the
	// repair completes or no repair is needed.
	anchorGapRepaired atomic.Bool

	// A.76: consecutive rounds where fetchHeadersParallel returned nil (no
	// progress). Used to add progressive backoff so we don't spam peers with
	// header requests during rate-limiting windows, which would extend the
	// rate-limit duration. Reset to 0 on any successful import.
	emptyRounds atomic.Int64

	// A.97.2 snap-heal state. healStarted is a one-shot guard so
	// maybeStartSnapHeal spawns at most one goroutine per AT_TIP transition.
	// healStatusMu protects healStatus for concurrent RPC reads. Refs #894.
	healStarted  atomic.Bool
	healStatusMu sync.Mutex
	healStatus   xdcHealStatus

	// Issue #1045: snap-sync live-lock prevention.
	//
	// snapSelectFailures counts consecutive CONCLUSIVE pivot-selection failures
	// (not transient "no peers" returns). Accessed only from the sync goroutine
	// (no atomic needed). Reset to 0 on any successful snap pivot selection.
	// When it reaches xdcSnapSelectMaxRetries, snapPermanentFullFallback is set.
	snapSelectFailures int

	// snapPermanentFullFallback, when true, causes maybeEngageSnap() to
	// return immediately so fullSyncOnce runs. Set after xdcSnapSelectMaxRetries
	// conclusive failures or when structural conditions (V1-only chain, V2 not
	// initialised) make snap permanently impossible.
	snapPermanentFullFallback atomic.Bool

	// snapExpWarnOnce gates the one-shot WARN emitted when --syncmode snap is
	// set but XDC_EXPERIMENTAL_SNAPSYNC=1 is not. Prevents log spam at the
	// 500ms tick cadence.
	snapExpWarnOnce atomic.Bool
}

// a70SeedRecord captures one V2 masternode-snapshot seed:
// (gap-block header, masternodes from epoch-switch.Validators, penalties).
// Persisted by seedMasternodeSnapshotsFromPrefetch; re-applied by
// reapplyA70SeedsPostSync after SyncCompleted.
type a70SeedRecord struct {
	gapHeader      *types.Header
	anchorHash     common.Hash // zero for gap-keyed seeds; non-zero for anchor-hash seed (operator pivot)
	anchorNumber   uint64      // only meaningful when anchorHash != zero
	masternodes    []common.Address
	penalties      []common.Address
	epochSwitchNum uint64
}

// xdcHealStatus carries the live state of the A.97.2 snap-heal pass.
// Exported via xdc_getStateHealStatus RPC. Refs #894.
type xdcHealStatus struct {
	Enabled         bool   // mirrors bc.checkpointSyncNoState (false = healed/not needed)
	InProgress      bool   // heal goroutine is running
	TargetRoot      string // hex root Sync is/was called with
	TrienodePending uint64 // scheduler.Pending() snapshot at last poll
	BytecodePending uint64 // bytecode tasks pending (informational; 0 until wired)
	HealStartUnix   int64  // Unix timestamp of heal-start (0 if not started)
	LastError       string // last non-nil error from Sync() or gate B; "" on success
}

func newXDCSyncer(h *handler) *xdcSyncer {
	return &xdcSyncer{
		handler: h,
		quitCh:  make(chan struct{}),
	}
}

func (s *xdcSyncer) start() {
	// XDC: enable bulk-sync optimization for the duration of initial sync.
	// When true: signing-tx ECDSA recovery skipped, larger TriesInMemory
	// (4096 vs 128), chainHeadEvent throttled, sender cache skipped.
	core.XdcBulkSyncMode.Store(true)
	go s.loop()
	go s.snapEventLoop() // refs #807 Phase 3b.3
}

func (s *xdcSyncer) stop() {
	core.XdcBulkSyncMode.Store(false)
	close(s.quitCh)
}

func (s *xdcSyncer) loop() {
	forceSync := time.NewTicker(xdcForceSyncCycle)
	defer forceSync.Stop()
	heartbeat := time.NewTicker(xdcHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-s.quitCh:
			return

		case <-forceSync.C:
			s.maybeSync()

		case <-heartbeat.C:
			cur := uint64(0)
			if head := s.handler.chain.CurrentBlock(); head != nil {
				cur = head.Number.Uint64()
			}
			log.Info("xdcSyncer heartbeat",
				"head", cur,
				"peers", s.handler.peers.len(),
				"syncing", s.syncing.Load(),
				"phase", xdcSnapPhase(s.snapPhase.Load()))

			// A.73.1: stuck-TIP_CATCH watchdog (refs #844 #859).
			// maybeTransitionToAtTip requires localHash == rng.LatestBlockHash
			// from a connected peer. On peer churn (peers disconnect after
			// advertising a stale tip, or reconnect with an advanced tip),
			// that equality may never fire. Guard: if localHead has reached or
			// exceeded every known peer's LatestBlock for 3 consecutive
			// heartbeats (~90 s), force the AT_TIP transition.
			//
			// #188 extension (default-flow miners): the same head-matches-peers
			// condition also clears XdcBulkSyncMode for nodes that never enter
			// the snap-sync phase machine (phaseFullSync stays put forever).
			// Without this, `--syncmode full --mine` validators are blocked at
			// Gate 3 of miner/xdpos_worker.commitNewWork — the miner sees its
			// turn but never reaches engine.Prepare. Observed on xdc04 modern
			// (devnet-5151, 0xe20fdc70): V1 nodes timed out every 4th round
			// waiting for xdc04's mint, xdc04 logged setNewRound but no
			// "Preparing new block!".
			phase := xdcSnapPhase(s.snapPhase.Load())
			if phase == phaseTipCatch || phase == phaseFullSync {
				maxPeerHead := uint64(0)
				for _, p := range s.handler.peers.peers {
					rng := p.BlockRange()
					if rng != nil && rng.LatestBlock > maxPeerHead {
						maxPeerHead = rng.LatestBlock
					}
				}
				// #190: tolerate being a few blocks behind the peer-advertised max.
				// On a LIVE chain (peers minting every ~2s) a synced node is
				// perpetually 1-2 blocks behind the advertised tip at the 30s
				// heartbeat sampling instant, so the strict `cur >= maxPeerHead`
				// never accumulates 3 consecutive ticks and XdcBulkSyncMode never
				// clears — which keeps a producer's miner gate 3 closed forever.
				const tipAtPeerTolerance = 3
				peerAtTip := maxPeerHead > 0 && cur+tipAtPeerTolerance >= maxPeerHead

				// #190: peer-independent at-tip signal via head-timestamp freshness.
				// Legacy XDPoSChain peers speak xpos2/100 and do NOT send the
				// eth/69 BlockRangeUpdate, so maxPeerHead is 0 when all peers are
				// legacy V1 nodes — peerAtTip can never fire. But if the local head
				// block's timestamp is within headFreshnessSecs of now, we ARE
				// importing current blocks (at tip); during genuine bulk historical
				// sync the head timestamp is hours/days old, so this cannot
				// false-trigger. This is what lets a modern producer peering only
				// with legacy nodes release bulk-mode and seal.
				const headFreshnessSecs = 30
				headFresh := false
				if head := s.handler.chain.CurrentBlock(); head != nil {
					age := time.Now().Unix() - int64(head.Time)
					headFresh = age >= 0 && age <= headFreshnessSecs
				}

				atTip := peerAtTip || headFresh
				if atTip {
					s.tipCatchAtPeerTicks++
					if s.tipCatchAtPeerTicks >= 3 {
						if phase == phaseTipCatch {
							log.Info("xdcSyncer: TIP_CATCH watchdog forcing AT_TIP transition",
								"head", cur, "maxPeerHead", maxPeerHead, "ticks", s.tipCatchAtPeerTicks)
							s.reapplyA70Seeds()
							s.snapPhase.Store(int32(phaseAtTip))
						} else {
							log.Info("xdcSyncer: FULL_SYNC at-tip detected — clearing XdcBulkSyncMode for miner",
								"head", cur, "maxPeerHead", maxPeerHead, "ticks", s.tipCatchAtPeerTicks)
						}
						core.XdcBulkSyncMode.Store(false)
						// A.97.3c: this heartbeat at-tip detector (peerAtTip/headFresh) is
						// the RELIABLE trigger on a live chain — the res==nil at-tip flip in
						// syncOnce never fires when the tip steadily produces (every fetch
						// round returns the 1 newest block, res != nil), so a snapshot-
						// restored node never heals via that path. Fire the snap-heal here
						// too: no-op unless XDC_SNAP_HEAL=1 + checkpointSyncNoState set + snap
						// peers; it downloads the head trie and clears the sticky flag (gate
						// B validates). Gated to isSnapHealTrustedChain (mainnet/apothem/
						// known devnets) — gate B's hash verification makes this safe
						// everywhere in that list, unlike the removed naive clear below.
						if hcfg := s.handler.chain.Config(); hcfg != nil && hcfg.ChainID != nil &&
							isSnapHealTrustedChain(hcfg.ChainID.Uint64()) {
							s.maybeStartSnapHeal()
						}
						// Same authoritative at-tip signal as the syncOnce flip: bulk-sync is over,
						// so state validation must come back on. State-guarded and idempotent.
						s.handler.chain.ClearCheckpointSyncNoState()
						s.tipCatchAtPeerTicks = 0
					}
				} else {
					s.tipCatchAtPeerTicks = 0
				}

				// #199 halted-at-tip fallback (peer-independent). On an idle/halted V2
				// chain the head stops advancing while syncing=true keeps the
				// FULL_SYNC miner gated — a self-sustaining deadlock since the gated
				// nodes are the quorum the chain needs to resume. Neither existing
				// at-tip signal rescues this: peerAtTip needs cur+tol >= maxPeerHead
				// (fails if any peer advertises a stale/bogus head above ours, which
				// is common on a halted chain) and headFresh needs a recent head
				// (fails — chain not producing). So: if the head has NOT advanced for
				// haltedTipStallTicks heartbeats while connected to peers, clear
				// XdcBulkSyncMode so the miner can resume. Once even quorum-many nodes
				// ungate and seal one block, the head becomes fresh and headFresh
				// keeps the rest ungated. Liveness-only (clearing the gate only lets
				// the miner attempt a block; it does not change block contents) and
				// cannot fire on a healthy chain — there the head advances every block
				// (~2s) so the stall counter resets long before haltedTipStallTicks.
				const haltedTipStallTicks = 4
				if phase == phaseFullSync && cur > 0 &&
					s.handler.peers.len() > 0 && cur == s.lastFullSyncHead {
					s.fullSyncStallTicks++
					if s.fullSyncStallTicks >= haltedTipStallTicks {
						if core.XdcBulkSyncMode.Load() {
							log.Info("xdcSyncer: FULL_SYNC head stalled at tip with peers — clearing XdcBulkSyncMode (halted-chain miner recovery #199)",
								"head", cur, "peers", s.handler.peers.len(), "maxPeerHead", maxPeerHead, "stallTicks", s.fullSyncStallTicks)
							core.XdcBulkSyncMode.Store(false)
						}
						s.fullSyncStallTicks = 0
					}
				} else {
					s.fullSyncStallTicks = 0
				}
				s.lastFullSyncHead = cur
			} else {
				s.tipCatchAtPeerTicks = 0
				s.fullSyncStallTicks = 0
			}
		}
	}
}

// maybeSync runs syncOnce single-flight, with panic recovery so a BigBalance
// overflow or other consensus-path panic doesn't kill the entire node — log
// and continue so the next tick can try a different block range.
func (s *xdcSyncer) maybeSync() {
	if !s.syncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.syncing.Store(false)
		defer func() {
			if r := recover(); r != nil {
				log.Error("xdcSyncer: panic in syncOnce recovered", "panic", r)
			}
		}()
		s.syncOnce()
	}()
}

// syncOnce dispatches to the phase-specific action. Default is
// fullSyncOnce (today's behavior). Refs #807 Phase 3b, #844 A.7.
//
// The state machine is a no-op when ConfigSyncMode != SnapSync and != FastSync
// (default users see zero change). When --syncmode snap is set AND local is
// significantly behind a peer (> xdcSnapEngageMinGap), we transition to
// phaseSnapSelecting. When --syncmode fast is set, we transition to
// phaseFastSyncRunning on the first tick that has a connected peer.
func (s *xdcSyncer) syncOnce() {
	// Phase-machine entry: decide whether to transition to snap or fast pipeline,
	// then whether TIP_CATCH has caught up enough to advance to AT_TIP.
	s.maybeEngageSnap()
	s.maybeEngageFast()
	s.maybeTransitionToAtTip()
	switch xdcSnapPhase(s.snapPhase.Load()) {
	case phaseSnapSelecting:
		s.snapSelectOnce()
		return
	case phaseSnapRunning:
		s.snapRunningCheck()
		return
	case phaseFastSyncRunning:
		s.fastRunningCheck()
		return
	case phaseFullSync, phaseTipCatch, phaseAtTip:
		// A.75: back-to-back rounds — after a successful import, immediately
		// attempt the next batch without waiting for the 500ms forceSync tick.
		// Capped at xdcMaxBackToBackRounds to let the heartbeat tick observe
		// progress and prevent goroutine hogging.
		//
		// A.76: progressive backoff when peers return empty headers repeatedly.
		// XDPOS2 peers rate-limit after serving headers; spamming them at 500ms
		// extends the rate-limit window. After 10 consecutive empty rounds (~5s),
		// sleep 2s between attempts. After 60 consecutive empty rounds, sleep 8s.
		for i := 0; i < xdcMaxBackToBackRounds; i++ {
			var before uint64
			if cur := s.handler.chain.CurrentBlock(); cur != nil {
				before = cur.Number.Uint64()
			}
			s.fullSyncOnce()
			var after uint64
			if cur := s.handler.chain.CurrentBlock(); cur != nil {
				after = cur.Number.Uint64()
			}
			if after == before {
				// No progress — apply progressive backoff before releasing CAS.
				empty := s.emptyRounds.Add(1)
				var wait time.Duration
				switch {
				case empty > 60:
					wait = 8 * time.Second
				case empty > 10:
					wait = 2 * time.Second
				}
				if wait > 0 {
					select {
					case <-time.After(wait):
					case <-s.quitCh:
					}
				}
				return
			}
			// Progress made — reset backoff counter.
			s.emptyRounds.Store(0)
			select {
			case <-s.quitCh:
				return
			default:
			}
		}
		return
	}
}

// maybeEngageSnap looks at ConfigSyncMode + peer-vs-local gap and
// transitions to phaseSnapSelecting if conditions are right. Idempotent.
// Refs #807 Phase 3b.
func (s *xdcSyncer) maybeEngageSnap() {
	// Only engage from phaseFullSync — once we've moved on, the phase
	// machine drives the rest.
	if xdcSnapPhase(s.snapPhase.Load()) != phaseFullSync {
		return
	}
	if s.handler.downloader.ConfigSyncMode() != ethconfig.SnapSync {
		return // operator did not opt in
	}

	// Issue #1045 (primary fix): if the experimental env var is not set,
	// snap-sync cannot run — snapRunningCheck() will immediately fall back to
	// phaseFullSync, re-triggering this function every 500ms and preventing
	// fullSyncOnce from ever being dispatched. Return early and let
	// fullSyncOnce handle the chain. One-shot WARN on first suppression.
	if !snapsyncExperimentalEnabled() {
		if s.snapExpWarnOnce.CompareAndSwap(false, true) {
			log.Warn("xdcSyncer: --syncmode snap requested but XDC_EXPERIMENTAL_SNAPSYNC=1 not set; running full-sync instead",
				"hint", "set XDC_EXPERIMENTAL_SNAPSYNC=1 to enable experimental snap sync")
		}
		return // stay in FULL_SYNC
	}

	// Issue #1045 (secondary fix): after xdcSnapSelectMaxRetries consecutive
	// conclusive pivot-selection failures, give up on snap for the process
	// lifetime so fullSyncOnce gets to run (e.g. no snap/1-capable peers).
	if s.snapPermanentFullFallback.Load() {
		return // snap gave up; stay in FULL_SYNC
	}

	cur := s.handler.chain.CurrentBlock()
	if cur == nil {
		return
	}
	localHead := cur.Number.Uint64()

	// XDPOS2 wire carries peer TD in BlockRange.LatestBlock, not a block
	// number, so we cannot compute the exact gap. Conservative engagement
	// heuristic: if local head is below xdcSnapEngageFreshThreshold AND
	// at least one peer is connected with a non-empty head hash, treat
	// the node as fresh-from-genesis and engage snap. This guarantees we
	// never engage when at-tip (which would race with normal gossip
	// inserts), while still letting fresh-sync benefit from snap mode.
	//
	// A future commit can replace this with an XDPOS2-side request for
	// the peer's block number directly; until then, the heuristic is the
	// safe fallback.
	if localHead >= xdcSnapEngageFreshThreshold {
		return // not a fresh node; stay in FULL_SYNC
	}

	// A.97 short-chain guard (refs #912): on short private networks /
	// devnets the chain may have fewer than xdcSnapEngageMinGap (5000)
	// blocks. If we engage snap here, snapSelectOnce computes a synthetic
	// pivot target of localHead+5000 that doesn't exist, SelectSnapPivot
	// returns nil, we fall back to phaseFullSync — and on the NEXT 500ms
	// tick maybeEngageSnap re-engages, creating a loop that blocks
	// fullSyncOnce from importing any blocks until the chain grows past
	// ~5000.
	//
	// Guard: fetch the actual head block number from the best peer (one
	// GetBlockHeaders round-trip). If the peer head is below
	// xdcSnapEngageMinGap, skip snap-engage and let fullSyncOnce handle
	// the short chain normally. For production chains (mainnet > 73M,
	// Apothem > 50M) this path is never hit.
	//
	// If the fetch fails (peer not ready / XDPOS2 wire doesn't carry block
	// number), fall back to checking whether any peer is connected at all.
	var bestPeer *eth.Peer
	for _, p := range s.handler.peers.peers {
		rng := p.BlockRange()
		if rng != nil && rng.LatestBlockHash != (common.Hash{}) {
			bestPeer = p.Peer
			break
		}
	}
	if bestPeer == nil {
		return
	}
	// Try to learn the peer's real head block number. fetchHeaderByHash is
	// a single request; 8s timeout. On XDPOS2 peers the response contains
	// the actual Number field. If this returns nil (timeout / peer
	// disconnected), proceed without the guard — snap will fall back to
	// FULL_SYNC via SelectSnapPivot returning nil anyway.
	var actualPeerHead uint64
	for _, p := range s.handler.peers.peers {
		rng := p.BlockRange()
		if rng == nil || rng.LatestBlockHash == (common.Hash{}) {
			continue
		}
		if hdr := s.fetchHeaderByHash(p.Peer, rng.LatestBlockHash); hdr != nil {
			actualPeerHead = hdr.Number.Uint64()
			break
		}
	}
	if actualPeerHead > 0 && actualPeerHead < xdcSnapEngageMinGap {
		// Chain is shorter than the minimum snap-engage gap. Skip snap-sync
		// engagement; fullSyncOnce will import blocks normally.
		// We do NOT fall through: returning here keeps the phase in FULL_SYNC
		// and avoids the re-engage loop on short chains. Once the chain grows
		// above xdcSnapEngageMinGap, actualPeerHead will pass the gate and
		// snap can engage (if the node is still below xdcSnapEngageFreshThreshold).
		log.Debug("xdcSyncer: snap-engage skipped — chain shorter than xdcSnapEngageMinGap",
			"actualPeerHead", actualPeerHead, "minGap", xdcSnapEngageMinGap,
			"localHead", localHead)
		return
	}

	peerHead := actualPeerHead
	if peerHead == 0 {
		// Could not fetch peer head; use synthetic value for logging only.
		peerHead = localHead + xdcSnapEngageMinGap
	}
	gap := peerHead - localHead
	log.Info("xdcSyncer: engaging snap-sync state machine",
		"localHead", localHead, "peerHead", peerHead, "gap", gap)
	s.snapPhase.Store(int32(phaseSnapSelecting))
}

// maybeEngageFast transitions the phase machine from phaseFullSync →
// phaseFastSyncRunning when --syncmode fast is configured and at least one
// peer is connected. Unlike the snap path (which has a pivot-selection step),
// fast sync delegates pivot selection to the downloader's skeleton — xdcSyncer
// only needs to kick BeaconSync with the peer's announced head and let the
// downloader run processFastSyncContent.
//
// Engagement gate: phaseFullSync only (idempotent — once we've transitioned,
// the phase machine drives the rest). Refs #844 A.7.
func (s *xdcSyncer) maybeEngageFast() {
	// Only engage from phaseFullSync.
	if xdcSnapPhase(s.snapPhase.Load()) != phaseFullSync {
		return
	}
	if s.handler.downloader.ConfigSyncMode() != ethconfig.FastSync {
		return // operator did not opt in
	}
	cur := s.handler.chain.CurrentBlock()
	if cur == nil {
		return
	}
	// Only engage for fresh/behind nodes — if we somehow end up with state at
	// the current head, the syncModer will have promoted us to FullSync already.
	// Use the same fresh-threshold heuristic as snap for consistency.
	localHead := cur.Number.Uint64()
	if localHead >= xdcSnapEngageFreshThreshold {
		// #1077: a node already past the fresh threshold has no fast-sync to
		// engage (fast-sync, if any, has already completed — it is now merely
		// following the tip). Mark fast-engagement resolved so fullSyncOnce's
		// A.15 bulk-import suppression releases. Without this, a --syncmode=fast
		// node resumed at a high localHead deadlocks: maybeEngageFast returns
		// here without resolving, while fullSyncOnce suppresses all bulk-import
		// pending fast engagement → silent stall (no import, eth_syncing=false,
		// no error). This does not bypass a needed fast-sync: the >= threshold
		// branch already declines to fast-sync; it only unblocks normal import.
		s.fastEngagementResolved.Store(true)
		return // not a fresh node; full sync or already post-fast
	}
	// Need at least one peer with an announced head to drive the skeleton.
	best := s.bestPeer()
	if best == nil {
		return // wait for peers
	}

	// #844 A.11: peer-availability gate. Live testing on mainnet showed
	// xdcSyncer engaging FastSync ~5s after node start while only one
	// transient peer was connected; BeaconSync invoked state-sync against
	// that single peer, peer disconnected, faststatesync returned
	// "trie node X failed with all peers (0 tries, 0 peers)" and the
	// snapEventLoop locked us into FULL_SYNC permanently. Wait up to
	// xdcFastSyncPeerWaitTimeout for at least xdcFastSyncMinPeers connected
	// peers. We do NOT block the sync loop — instead, we delay the phase
	// transition while the 500ms tick re-enters us here. After timeout we
	// engage anyway (the retry loop in fastRunningCheck will cover the
	// remaining peer-dial races).
	peerCount := s.handler.peers.len()
	if peerCount < xdcFastSyncMinPeers {
		now := time.Now()
		startedNS := s.fastWaitStartedAt.Load()
		if startedNS == 0 {
			s.fastWaitStartedAt.Store(now.UnixNano())
			startedNS = now.UnixNano()
		}
		elapsed := now.Sub(time.Unix(0, startedNS))
		if elapsed < xdcFastSyncPeerWaitTimeout {
			lastLogNS := s.fastWaitLastLogAt.Load()
			if lastLogNS == 0 || now.Sub(time.Unix(0, lastLogNS)) >= xdcFastSyncPeerWaitLogEvery {
				log.Info("xdcSyncer: waiting for peers before fast-sync",
					"have", peerCount,
					"need", xdcFastSyncMinPeers,
					"elapsed", elapsed.Round(time.Second),
					"timeout", xdcFastSyncPeerWaitTimeout)
				s.fastWaitLastLogAt.Store(now.UnixNano())
			}
			return // stay in phaseFullSync; retry on next tick
		}
		log.Warn("xdcSyncer: peer-wait timeout — engaging fast-sync anyway",
			"have", peerCount,
			"need", xdcFastSyncMinPeers,
			"timeout", xdcFastSyncPeerWaitTimeout)
		// A.15 escape valve: for non-pinned --syncmode, releasing the bulk-import gate
		// after peer-wait timeout matches the pre-A.15 fallback contract. Pinned mode
		// (A.12) still suppresses bulk-import indefinitely — that's the user's explicit
		// intent of "only sync with fast sync".
		if s.handler.downloader.ConfigSyncMode() != ethconfig.FastSync {
			s.fastEngagementResolved.Store(true)
		}
	} else if s.fastWaitStartedAt.Load() != 0 {
		// Peer threshold met after a previous wait — log once.
		log.Info("xdcSyncer: peer threshold met — engaging fast-sync",
			"peers", peerCount,
			"need", xdcFastSyncMinPeers)
	}

	log.Info("xdcSyncer: engaging fast-sync state machine (FastSync mode)",
		"localHead", localHead, "configMode", "fast", "peers", peerCount)
	s.snapPhase.Store(int32(phaseFastSyncRunning))
}

// fastRunningCheck handles the phaseFastSyncRunning state. On first entry it
// kicks BeaconSync (which drives the skeleton + processFastSyncContent path);
// on subsequent ticks it monitors the SyncEvent stream (via snapEventLoop,
// which also handles FastSync events) and is a near-no-op. Refs #844 A.7.
func (s *xdcSyncer) fastRunningCheck() {
	if s.fastBeaconStarted.Load() {
		// BeaconSync already kicked; snapEventLoop will transition phase on
		// completion (SyncCompleted → phaseTipCatch / SyncFailed → phaseFullSync).
		log.Debug("xdcSyncer: FAST_SYNC_RUNNING — BeaconSync in progress")
		return
	}
	// First entry: select head header from best peer and kick BeaconSync.
	// Fast sync doesn't need a pre-selected pivot from xdcSyncer — the
	// downloader picks the pivot as head.Number - fsMinFullBlocks (same
	// skeleton logic as snap). xdcSyncer just supplies the chain tip.
	const fastHeadFetchMaxAttempts = 3
	var headHdr *types.Header

	// A.13: if operator pinned a fast-sync pivot, use the pinned header as the
	// BeaconSync head instead of the peer-advertised tip. This terminates the
	// skeleton walk-back at the pinned height — needed when the v1.17.3 trusted
	// peer fleet lacks continuous history deeper than the pinned anchor while
	// only the pivot-bearing peer holds it. On lookup failure, fall back to the
	// peer-tip path (do NOT block fast-sync engagement). Refs A.13.
	opNumber, opHash, _ := s.handler.downloader.OperatorPivot()
	// A.14: track the peer that supplied the pivot so we can re-use it to
	// fetch the parent header for PrimeFastSyncAnchor below.
	var pivotPeer *eth.Peer
	if opNumber != 0 && opHash != (common.Hash{}) {
		opAttempts := 0
		for _, p := range s.rankedPeers() {
			if opAttempts >= fastHeadFetchMaxAttempts {
				break
			}
			opAttempts++
			if h := s.fetchHeaderByHash(p, opHash); h != nil {
				headHdr = h
				pivotPeer = p
				log.Info("xdcSyncer: using operator-pinned head for BeaconSync",
					"number", h.Number.Uint64(),
					"hash", h.Hash().Hex(),
					"attempts", opAttempts)
				break
			}
		}
		if headHdr == nil {
			log.Warn("xdcSyncer: operator-pinned head lookup failed; falling back to peer-tip",
				"pinned_number", opNumber,
				"pinned_hash", opHash.Hex(),
				"attempts", opAttempts)
		}
	}

	if headHdr == nil {
		attempts := 0
		for _, p := range s.rankedPeers() {
			if attempts >= fastHeadFetchMaxAttempts {
				break
			}
			rng := p.BlockRange()
			if rng == nil || rng.LatestBlockHash == (common.Hash{}) {
				continue
			}
			attempts++
			if h := s.fetchHeaderByHash(p, rng.LatestBlockHash); h != nil {
				// A.71 (refs #844 #859): when the operator pinned a fast-sync
				// pivot, refuse to engage BeaconSync with a peer-tip below
				// the pivot. The peer is either misreporting its head (some
				// XDC v2.7.0 peers transiently advertise local-state heights
				// on cold start) or genuinely sits on a forked / lagging
				// chain. Engaging anyway with such a head causes the
				// skeleton to initSync at a low number, leaving a stub
				// subchain that poisons subsequent retries with the correct
				// op-pinned head (initSync only drops subchains whose Tail
				// >= the new headchain's Tail; with old Tail ≈ 64.9M and
				// new Tail = 103.3M the predicate fails, len(Subchains)
				// stays at 2, and linked() never short-circuits true on the
				// re-entry path → block-import never starts). Skip this
				// candidate and continue scanning; if every peer-tip is
				// below the pivot, fall through and retry next tick.
				if opNumber != 0 && h.Number.Uint64() < opNumber {
					log.Warn("xdcSyncer: rejecting peer-tip below operator pivot for BeaconSync head",
						"peer", p.ID(),
						"peer_tip", h.Number.Uint64(),
						"peer_tip_hash", h.Hash().Hex(),
						"operator_pivot", opNumber)
					continue
				}
				headHdr = h
				break
			}
		}
		if headHdr == nil {
			log.Warn("xdcSyncer: FAST_SYNC_RUNNING — failed to fetch peer head; retrying next tick",
				"attempts", attempts)
			// Don't fall back — stay in phaseFastSyncRunning and retry.
			return
		}
	}
	s.fastBeaconStarted.Store(true)

	// A.14/A.16: prime the operator-pinned anchor window so findBeaconAncestor's
	// HasFastBlock(pivot) linkup check passes when the skeleton walk-back
	// terminates at the pinned pivot (see skeleton.terminateFloor wired in
	// SetFastSyncPivot). The downloader derives its true snap-sync pivot as
	// `head - pivotDistance` (= head - 1800 for XDPoS). Walk back via
	// fetchHeaderByHash from the pivot-bearing peer.
	//
	// A.90.1: for the dynamic pivot path (A.89/A.90, no operator flags), use
	// the same priming so findBeaconAncestor's HasFastBlock(head-1800) passes.
	// When pivotPeer == nil (peer-tip path), use the best peer available.
	dl := s.handler.downloader
	pivotDist := dl.FastSyncPivotDistance()
	primeHeadersNeeded := int(pivotDist) + 1
	_, _, opRoot := dl.OperatorPivot()
	isDynamic := opRoot == (common.Hash{}) // no operator pivot root → dynamic path
	primeSourcePeer := pivotPeer
	if primeSourcePeer == nil && isDynamic {
		// Dynamic path: pick best available peer for header walk-back.
		for _, p := range s.rankedPeers() {
			primeSourcePeer = p
			break
		}
	}
	if primeSourcePeer != nil && headHdr.Number.Uint64() >= uint64(primeHeadersNeeded-1) {
		primeHeaders := make([]*types.Header, 0, primeHeadersNeeded)
		primeHeaders = append(primeHeaders, headHdr) // tip
		currentHdr := headHdr
		for i := 1; i < primeHeadersNeeded; i++ {
			parentHdr := s.fetchHeaderByHash(primeSourcePeer, currentHdr.ParentHash)
			if parentHdr == nil {
				log.Warn("xdcSyncer: prime-anchor header walk-back failed",
					"at_block", currentHdr.Number.Uint64()-1,
					"parent_hash", currentHdr.ParentHash.Hex(),
					"fetched_so_far", len(primeHeaders))
				// A.91: peer dropped mid-walk — reset flag so next tick retries with fresh peer
				// instead of calling BeaconSync with incomplete anchor (causes errTerminated crash).
				log.Warn("xdcSyncer: prime walk-back peer dropped — will retry next tick with fresh peer",
					"at_block", currentHdr.Number.Uint64()-1,
					"fetched", len(primeHeaders),
					"needed", primeHeadersNeeded)
				s.fastBeaconStarted.Store(false)
				return
			}
			// Prepend so the slice ends up ascending: [head-pivotDist, ..., head].
			primeHeaders = append([]*types.Header{parentHdr}, primeHeaders...)
			currentHdr = parentHdr
		}
		if len(primeHeaders) >= primeHeadersNeeded {
			if err := dl.PrimeFastSyncAnchor(primeHeaders); err != nil {
				log.Warn("xdcSyncer: prime anchor failed — fast-sync will rely on full walk-back",
					"err", err)
			} else {
				// A.68.2 (refs #844 #859): pre-fetch ~2*Epoch worth of ancestor
				// headers immediately below the prime window so the XDPoS V2
				// engine's `getEpochSwitchInfo` recursion can resolve masternodes
				// from the most recent V2 epoch-switch header (header.Validators)
				// when verifyHeader fires on pre-pivot blocks delivered by
				// BeaconSync's skeleton walk-back. Without this, A.68.1's
				// catchup-window gate correctly skips strict verifyQC but the
				// SIGNATURE-verification path further down in verifyHeader still
				// calls GetMasternodesWithParents which fails with "empty
				// masternode list" — the in-batch parents slice never spans
				// back to the previous epoch switch (~900 blocks below the
				// pivot for blocks just under it).
				//
				// Walk back from primeHeaders[0] (= pivot-64) by 2*Epoch =
				// 1800 headers using fetchHeaderBatch (XDPOS2 192-header cap
				// per request → ~10 sequential requests). Best-effort: a
				// partial walk is still useful because it covers some pre-
				// pivot history; full walk failure (peer dropped mid-walk)
				// logs a warning and continues — V2 engine fallbacks may
				// still resolve via header.Validators when in-batch parents
				// reach back far enough.
				s.prefetchAncestorEpochs(primeSourcePeer, primeHeaders[0])
			}
		}
	}

	log.Info("xdcSyncer: invoking BeaconSync for fast-sync",
		"head", headHdr.Number.Uint64(),
		"head_hash", headHdr.Hash().Hex(),
		"head_root", headHdr.Root.Hex())
	go func() {
		// BeaconSync(head, final=nil): final=nil means no trusted finalized
		// point — the downloader will use its pivot heuristic (head-64).
		// For fast sync we rely on processFastSyncContent + NodeData for
		// state; no snap.Syncer is needed, so nil final is correct.
		//
		// #844 A.11: retry transient "0 peers" failures up to
		// xdcFastSyncMaxRetries times with xdcFastSyncRetryBackoff between
		// attempts. faststatesync.go returns errors of the form
		// `trie node <h> failed with all peers (<N> tries, <M> peers)` or
		// `byte code <h> failed with all peers (<N> tries, <M> peers)`.
		// On startup it's common to see (0 tries, 0 peers) because trusted
		// peers haven't finished dialing yet, or because the only connected
		// peer was not NodeData-eligible. Treat these as transient and let
		// the legacy XDC peers complete their dial before giving up.
		for attempt := 1; attempt <= xdcFastSyncMaxRetries; attempt++ {
			// A.20: on retry attempts (attempt > 1) when the operator pinned a
			// fast-sync pivot, the CLI-supplied head is likely stale — v2.7.0
			// peers have advanced their tip by ~7-8 blocks/retry and the trie
			// at the pinned depth gets pruned. Re-query the best peer's actual
			// current head and use THAT as the new pinned head for this retry,
			// so state-sync targets a recent (unpruned) root.
			if attempt > 1 && opNumber != 0 && opHash != (common.Hash{}) {
				if freshHdr := s.refreshOperatorPivotHead(); freshHdr != nil {
					if freshHdr.Hash() != headHdr.Hash() {
						log.Info("xdcSyncer: pivot refreshed for retry",
							"attempt", attempt,
							"old_number", headHdr.Number.Uint64(),
							"old_hash", headHdr.Hash().Hex(),
							"new_number", freshHdr.Number.Uint64(),
							"new_hash", freshHdr.Hash().Hex(),
							"new_root", freshHdr.Root.Hex())
						headHdr = freshHdr
					}
				}
			}
			err := s.handler.downloader.BeaconSync(headHdr, nil)
			if err == nil {
				log.Info("xdcSyncer: fast-sync BeaconSync kicked; awaiting completion via SyncEvent",
					"head", headHdr.Number.Uint64(), "attempt", attempt)
				return
			}
			fastPinned := s.handler.downloader.ConfigSyncMode() == ethconfig.FastSync
			if !isFastSyncTransientPeerErr(err) {
				if fastPinned {
					log.Error("xdcSyncer: BeaconSync for fast-sync failed (non-transient) — staying in fast-sync (--syncmode=fast pinned, will retry)",
						"err", err, "head", headHdr.Number.Uint64(), "attempt", attempt)
					s.fastBeaconStarted.Store(false)
					select {
					case <-time.After(60 * time.Second):
					case <-s.quitCh:
						return
					}
					return
				}
				log.Error("xdcSyncer: BeaconSync for fast-sync failed (non-transient); falling back to FULL_SYNC",
					"err", err, "head", headHdr.Number.Uint64(), "attempt", attempt)
				s.snapPhase.Store(int32(phaseFullSync))
				s.fastBeaconStarted.Store(false)
				return
			}
			if attempt == xdcFastSyncMaxRetries {
				if fastPinned {
					log.Warn("xdcSyncer: fast-sync transient retries exhausted — resetting + retrying (--syncmode=fast pinned)",
						"err", err, "head", headHdr.Number.Uint64(), "attempts", xdcFastSyncMaxRetries)
					s.fastBeaconStarted.Store(false)
					select {
					case <-time.After(60 * time.Second):
					case <-s.quitCh:
						return
					}
					return
				}
				log.Error("xdcSyncer: fast-sync failed after retries — falling back to FULL_SYNC",
					"err", err,
					"head", headHdr.Number.Uint64(),
					"attempts", xdcFastSyncMaxRetries)
				s.snapPhase.Store(int32(phaseFullSync))
				s.fastBeaconStarted.Store(false)
				return
			}
			log.Warn("xdcSyncer: fast-sync transient error — retrying",
				"attempt", attempt,
				"max", xdcFastSyncMaxRetries,
				"backoff", xdcFastSyncRetryBackoff,
				"peers", s.handler.peers.len(),
				"err", err)
			select {
			case <-time.After(xdcFastSyncRetryBackoff):
			case <-s.quitCh:
				return
			}
		}
	}()
}

// refreshOperatorPivotHead re-queries the best-ranked peer for its current
// chain tip and fetches that header by hash. Used by the fast-sync retry loop
// (A.20) when the operator-pinned head has gone stale relative to peers' tip:
// each retry would otherwise target the same pruned trie root.
//
// Returns nil if no peer advertises a usable LatestBlockHash or none of the
// top-ranked peers serves the header within fastHeadFetchMaxAttempts attempts;
// the caller falls back to the existing (stale) headHdr in that case.
func (s *xdcSyncer) refreshOperatorPivotHead() *types.Header {
	const fastHeadFetchMaxAttempts = 3
	attempts := 0
	for _, p := range s.rankedPeers() {
		if attempts >= fastHeadFetchMaxAttempts {
			break
		}
		rng := p.BlockRange()
		if rng == nil || rng.LatestBlockHash == (common.Hash{}) {
			continue
		}
		attempts++
		if h := s.fetchHeaderByHash(p, rng.LatestBlockHash); h != nil {
			return h
		}
	}
	return nil
}

// isFastSyncTransientPeerErr reports whether err matches the "no eligible
// peer" transient failure pattern emitted by eth/downloader/faststatesync.go
// when the downloader runs out of peers willing/able to serve NodeData
// (`trie node <h> failed with all peers (N tries, M peers)` /
//
//	`byte code <h> failed with all peers (...)`).
//
// Trusted peers can take 20-60s to dial on cold start, so a node that
// engaged FastSync early may briefly see 0 NodeData-eligible peers; we
// retry rather than locking into FULL_SYNC permanently. Refs #844 A.11.
func isFastSyncTransientPeerErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Match the canonical failure phrasing from faststatesync.go.
	if !strings.Contains(msg, "failed with all peers") {
		return false
	}
	// Only treat it as transient if peer-pool truly empty / nearly empty —
	// otherwise the downloader exhausted real peers on a real bad root and
	// retrying won't help. The "(N tries, M peers)" suffix carries that.
	// "0 peers" is the unambiguous startup race signature; "1 peers" can
	// also be a transient single-peer-dropped race, so we accept both.
	if strings.Contains(msg, "0 peers)") || strings.Contains(msg, "1 peers)") {
		return true
	}
	return false
}

// healMaxRootRetargets is the maximum number of times maybeStartSnapHeal
// will re-call Sync(newRoot) when the chain head advances during healing.
// After this many retargets we WARN and leave checkpointSyncNoState as-is.
const healMaxRootRetargets = 3

// healTimeout is the wall-clock budget for the entire snap-heal pass
// (initial Sync + retargets). 3× snapStallGrace (10 min) = 30 min.
const healTimeout = 30 * time.Minute

// healGateBBlocks is the number of recent blocks re-executed for gate B
// (read-only, no InsertChain/SetHead). Equals state.TriesInMemory (128).
// Must be > downloaderFsMinFullBlocks (64) so the window spans the
// pivot→head delta. Refs #894 Phase 2.
const healGateBBlocks = 128

// maybeStartSnapHeal attempts to start a background snap-heal goroutine that
// runs Sync(headRoot) against the snap syncer so that only the trie-node heal
// branch fires (account tasks are already satisfied or will time out).
//
// Guards:
//   - XDC_SNAP_HEAL=1 env var (default off)
//   - XDC chain only (chainConfig.XDPoS != nil)
//   - checkpointSyncNoState must be true (nothing to heal otherwise)
//   - healStarted one-shot: only one goroutine spawned per AT_TIP flip
//   - PeerCount()==0 → WARN + return immediately
//
// On Sync() == nil: runs gate B (128-block read-only re-execution with
// XdcBulkSyncMode already false) then calls SetCheckpointSyncNoState(false).
// On timeout or failure: leaves flag as-is, WARNs, exposes lastError via RPC.
//
// Called from maybeTransitionToAtTip AFTER core.XdcBulkSyncMode.Store(false).
// Refs #894.
func (s *xdcSyncer) maybeStartSnapHeal() {
	// Env-gate first: cheapest check, avoids dereferencing handler in tests.
	if !snapHealEnabled() {
		s.healStatusMu.Lock()
		s.healStatus = xdcHealStatus{Enabled: false}
		s.healStatusMu.Unlock()
		return
	}
	cfg := s.handler.chain.Config()
	if cfg.XDPoS == nil {
		return // non-XDC chain
	}
	bc := s.handler.chain
	if !bc.CheckpointSyncNoState() {
		// A.96 / default path: flag is false → nothing to heal.
		log.Debug("A.97.2: snap-heal skipped — checkpointSyncNoState already false")
		s.healStatusMu.Lock()
		s.healStatus = xdcHealStatus{Enabled: false}
		s.healStatusMu.Unlock()
		return
	}
	if !s.healStarted.CompareAndSwap(false, true) {
		return // already started
	}

	// Check snap peers before spawning goroutine.
	snapSyncer := s.handler.downloader.SnapSyncer
	if snapSyncer.PeerCount() == 0 {
		log.Warn("A.97.2: snap-heal skipped — no-snap-peers",
			"hint", "XDC fleet may not advertise snap/1 capability; set XDC_SNAP_HEAL=1 again after peer connects")
		s.healStarted.Store(false) // Store(false) is harmless / future-proof, but note: no
		// phaseTipCatch→phaseAtTip re-entry exists — maybeTransitionToAtTip
		// early-returns unless phase==phaseTipCatch, and it sets phaseAtTip
		// BEFORE calling maybeStartSnapHeal. Practically, with zero snap
		// peers, heal is skipped for this process lifetime. A node restart
		// re-reads the persisted checkpointSyncNoState flag and retries.
		s.healStatusMu.Lock()
		s.healStatus = xdcHealStatus{
			Enabled:   true,
			LastError: "no snap peers at heal-start",
		}
		s.healStatusMu.Unlock()
		return
	}

	go func() {
		now := time.Now()
		s.healStatusMu.Lock()
		s.healStatus = xdcHealStatus{
			Enabled:       true,
			InProgress:    true,
			HealStartUnix: now.Unix(),
		}
		s.healStatusMu.Unlock()

		// 30-minute overall timeout.
		timeoutCh := make(chan struct{})
		timer := time.AfterFunc(healTimeout, func() { close(timeoutCh) })
		defer timer.Stop()

		var syncErr error
		var lastRoot common.Hash

		for retarget := 0; retarget <= healMaxRootRetargets; retarget++ {
			cur := bc.CurrentBlock()
			if cur == nil {
				syncErr = fmt.Errorf("nil current block at retarget %d", retarget)
				break
			}
			targetRoot := cur.Root
			if targetRoot == lastRoot && retarget > 0 {
				// Head hasn't advanced; heal is done.
				break
			}
			lastRoot = targetRoot

			if retarget > 0 {
				log.Info("A.97.2: snap-heal re-targeting root",
					"attempt", retarget,
					"root", targetRoot.Hex(),
					"head", cur.Number.Uint64())
			} else {
				log.Info("A.97.2: snap-heal started",
					"root", targetRoot.Hex(),
					"head", cur.Number.Uint64(),
					"peers", snapSyncer.PeerCount())
			}

			s.healStatusMu.Lock()
			s.healStatus.TargetRoot = targetRoot.Hex()
			s.healStatusMu.Unlock()

			// cancel channel: closed on timeout or manual stop (via watchdog goroutine).
			// done channel: signals the watchdog to exit after Sync returns.
			cancel := make(chan struct{})
			done := make(chan struct{})
			go func() {
				select {
				case <-timeoutCh:
					// Close cancel only once (idempotent via sync.Once).
					select {
					case <-cancel:
					default:
						close(cancel)
					}
				case <-s.quitCh:
					select {
					case <-cancel:
					default:
						close(cancel)
					}
				case <-done:
					// Sync returned naturally; watchdog exits.
				}
			}()

			// snap.Syncer.Sync takes *types.Header (v1.17.4 API). cur is the
			// *types.Header from bc.CurrentBlock(), so pass it directly.
			syncErr = snapSyncer.Sync(cur, cancel)
			close(done) // signal watchdog to exit if still alive

			// Update status with pending counts.
			{
				prog := snapSyncer.Progress()
				s.healStatusMu.Lock()
				s.healStatus.TrienodePending = prog.HealingTrienodes
				s.healStatus.BytecodePending = prog.HealingBytecode
				s.healStatusMu.Unlock()
			}

			if syncErr != nil {
				// ErrCancelled = timeout or shutdown; treat as incomplete.
				break
			}

			// Sync returned nil; check if head advanced (re-target).
			newCur := bc.CurrentBlock()
			if newCur != nil && newCur.Root != targetRoot {
				continue // retarget
			}
			break // targetRoot still matches — proceed to gate B
		}

		if syncErr != nil {
			errStr := syncErr.Error()
			log.Warn("A.97.2: snap-heal incomplete",
				"err", errStr,
				"root", lastRoot.Hex())
			s.healStatusMu.Lock()
			s.healStatus.InProgress = false
			s.healStatus.LastError = errStr
			s.healStatusMu.Unlock()
			// Do NOT clear checkpointSyncNoState — state is still partial.
			return
		}

		// Gate B (A.97.3 fix): the snap heal downloads a SINGLE state root
		// (lastRoot, the healed near-tip root). Re-executing the recent
		// healGateBBlocks blocks is STRUCTURALLY IMPOSSIBLE here:
		// ReexecuteAndValidateRange opens StateAt(parent) for each block, but
		// those parent states were never built (checkpointSyncNoState skipped
		// execution during catch-up) and the heal restores only lastRoot — so
		// StateAt misses on the very first pre-head block, gate B ALWAYS errors,
		// SetCheckpointSyncNoState(false) is never reached, and the rewind never
		// clears. Instead validate that the healed trie is COMPLETE on disk:
		// snapSyncer.Sync returns nil only after downloading AND hash-verifying
		// the full trie against the canonical block.Root, so a present+complete
		// healed state is proof of correct canonical state. Forward execution
		// from the healed root is still protected by normal block-import
		// ValidateState (a divergent block would be rejected). Refs #894 #165.
		log.Info("A.97.3: snap-heal Sync complete — verifying healed state is complete on disk",
			"root", lastRoot.Hex())

		if (lastRoot == common.Hash{}) || !bc.HasState(lastRoot) {
			errStr := fmt.Sprintf("healed state incomplete for root %s after Sync", lastRoot.Hex())
			log.Warn("A.97.3: snap-heal gate-B (head-state completeness) failed — leaving checkpointSyncNoState set",
				"err", errStr)
			s.healStatusMu.Lock()
			s.healStatus.InProgress = false
			s.healStatus.LastError = errStr
			s.healStatusMu.Unlock()
			return
		}

		// Gate B passed: healed head state is provably the canonical state on
		// disk — clear the flag so normal execution + state commit resumes.
		bc.SetCheckpointSyncNoState(false)
		log.Info("A.97.2: snap-heal completed",
			"root", lastRoot.Hex(),
			"head", func() uint64 {
				if cur := bc.CurrentBlock(); cur != nil {
					return cur.Number.Uint64()
				}
				return 0
			}())
		s.healStatusMu.Lock()
		s.healStatus.Enabled = false
		s.healStatus.InProgress = false
		s.healStatus.LastError = ""
		s.healStatusMu.Unlock()
	}()
}

// maybeTransitionToAtTip advances phaseTipCatch → phaseAtTip when the
// local head matches a peer's claimed head hash (i.e. we've caught up
// post-snap-sync). Also flips core.XdcBulkSyncMode off so the at-tip
// behaviors (full ECDSA tx recovery, eager chainHeadEvent dispatch)
// re-engage now that bulk import is done.
//
// Only acts from phaseTipCatch — default-flow operators (always FULL_SYNC)
// are not affected.
//
// Refs #807 Phase 4.
func (s *xdcSyncer) maybeTransitionToAtTip() {
	if xdcSnapPhase(s.snapPhase.Load()) != phaseTipCatch {
		return
	}
	cur := s.handler.chain.CurrentBlock()
	if cur == nil {
		return
	}
	localHash := cur.Hash()
	for _, p := range s.handler.peers.peers {
		rng := p.BlockRange()
		if rng == nil {
			continue
		}
		if rng.LatestBlockHash == localHash {
			log.Info("xdcSyncer: TIP_CATCH complete; transitioning to AT_TIP",
				"head", cur.Number.Uint64(),
				"hash", localHash.Hex())
			s.snapPhase.Store(int32(phaseAtTip))
			// A.73 (refs #844 #859): re-apply A.70 seeds one final time before
			// flipping to strict mode. During TIP_CATCH, block-import of each
			// post-pivot gap block calls UpdateMasternodesFromHeader (reads
			// contract state, sorts candidates by stake) and can overwrite the
			// SyncCompleted-time seed. Re-seeding here restores authoritative
			// header.Validators as the active set so the first fullVerify=true
			// epoch-switch verify after this flip sees the canonical 108-of-N
			// truncation rather than the contract-state-sorted divergent one.
			s.reapplyA70Seeds()
			// A.97.2: flip bulk-mode first (gate B must run with bulk=false),
			// THEN start the heal goroutine. Ordering is critical: gate B
			// checks that XdcBulkSyncMode is false so A.94 cannot fire and
			// any mismatch surfaces as a real error. Refs #894.
			core.XdcBulkSyncMode.Store(false)
			s.maybeStartSnapHeal()
			// Authoritative at-tip transition: re-enable state validation now that
			// bulk-sync is over, so the miner opens the real head state instead of
			// EmptyRootHash. No-op unless the flag is set, and refuses (loudly) if the
			// head state is missing — that DB needs a resync, not a flag flip.
			s.handler.chain.ClearCheckpointSyncNoState()
			return
		}
	}
}

// snapSelectOnce selects a snap-sync pivot. Strategy:
//
// Primary path: use the highest V2-era TrustedSyncCheckpoint with
// embedded Masternodes as the pivot directly. The trust comes from the
// hardcoded checkpoint (same trust model as upstream Geth snap-sync
// where the user trusts the binary's checkpoint data). No QC
// verification against peer headers is needed for the pivot itself —
// the checkpoint IS the trusted anchor.
//
// Fallback path: if no V2 checkpoint with Masternodes exists, fall
// back to the ADR-807 QC-verification helper (SelectSnapPivot). This
// path requires the local node to already have the masternode set
// for the candidate's epoch, which a fresh-genesis node lacks — so
// it will usually return nil and we fall back to FULL_SYNC.
//
// Refs #807 Phase 3b.2 + 3b.4 (checkpoint pivot) + ADR-807 Phase 1.
func (s *xdcSyncer) snapSelectOnce() {
	defer func() {
		log.Info("xdcSyncer: snapSelectOnce returned",
			"phase", xdcSnapPhase(s.snapPhase.Load()),
			"pivot", s.snapPivotBlock())
	}()

	// Engine must be XDPoS + V2 to support snap pivot. V1-only chains
	// fall back to FULL_SYNC (snap is undefined pre-V2).
	xdposEngine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS)
	if !ok {
		log.Info("xdcSyncer: snap-sync requires XDPoS engine; falling back to FULL_SYNC permanently")
		s.snapPhase.Store(int32(phaseFullSync))
		s.snapPermanentFullFallback.Store(true)
		return
	}
	v2Engine, ok := xdposEngine.EngineV2.(*engine_v2.XDPoS_v2)
	if !ok || v2Engine == nil {
		log.Info("xdcSyncer: V2 engine not initialised; falling back to FULL_SYNC permanently")
		s.snapPhase.Store(int32(phaseFullSync))
		s.snapPermanentFullFallback.Store(true)
		return
	}

	cur := s.handler.chain.CurrentBlock()
	if cur == nil {
		return
	}
	cfg := s.handler.chain.Config()

	// Primary path: pick highest V2-era TrustedSyncCheckpoint with
	// embedded Masternodes.
	if cfg != nil && cfg.XDPoS != nil && cfg.XDPoS.V2 != nil && cfg.XDPoS.V2.SwitchBlock != nil {
		v2Switch := cfg.XDPoS.V2.SwitchBlock.Uint64()
		var best *params.TrustedSyncCheckpoint
		for _, cp := range cfg.TrustedSyncCheckpoints {
			if cp.Number >= v2Switch && len(cp.Masternodes) > 0 {
				if best == nil || cp.Number > best.Number {
					best = cp
				}
			}
		}
		if best != nil {
			// Fetch the REAL header at best.Number from a peer. We need
			// the full header (not just Number+Root) because
			// types.Header.Hash() recomputes from all fields — passing
			// a synthesised header with only Number+Root set would
			// produce a hash that doesn't match best.Hash, causing
			// BeaconSync's skeleton-sync requests to peers to fail
			// (peer has no block with the synthesised hash).
			//
			// Refs #807 Phase 7+. Fetch + verify the header matches
			// the checkpoint Hash before using it as a pivot.
			candidates := s.rankedPeers()
			if len(candidates) == 0 {
				return // stay in SNAP_SELECTING; retry next tick
			}
			var hdr *types.Header
			for _, p := range candidates {
				hdrs := s.fetchHeaderBatch(p, best.Number, 1)
				if len(hdrs) == 1 && hdrs[0] != nil && hdrs[0].Hash() == best.Hash {
					hdr = hdrs[0]
					break
				}
			}
			if hdr == nil {
				log.Info("xdcSyncer: no peer served a header matching the checkpoint hash; retrying next tick",
					"block", best.Number, "expectedHash", best.Hash.Hex())
				return
			}
			// Phase 8: state at the checkpoint Number (e.g. 82,293,750)
			// may already be outside peers' state-history retention
			// window (~90k blocks on path-scheme). If we pin the pivot
			// to the checkpoint, snap state download fails because no
			// peer has that root. Instead, prefer the peer's CURRENT
			// head as the pivot — peers always have state at their
			// own head (under both path and hash scheme). The
			// checkpoint hdr we already fetched anchors the
			// masternodes for QC verification (future ADR-807 use).
			//
			// Fetch peer's current head header by their announced
			// BlockRange.LatestBlockHash. Use that header as the
			// snap pivot.
			if peerHeadHdr := s.fetchPeerHeadHeader(); peerHeadHdr != nil {
				s.snapPivotMu.Lock()
				s.snapPivot = &engine_v2.PivotResult{
					Block: peerHeadHdr,
					Root:  peerHeadHdr.Root,
					Depth: peerHeadHdr.Number.Uint64() - cur.Number.Uint64(),
				}
				s.snapPivotMu.Unlock()
				log.Info("xdcSyncer: snap pivot selected from PEER HEAD (#807 Phase 8)",
					"block", peerHeadHdr.Number.Uint64(),
					"hash", peerHeadHdr.Hash().Hex(),
					"root", peerHeadHdr.Root.Hex(),
					"anchor_checkpoint", best.Number)
				// DEPRECATION (A.97 / #894): this pivot was anchored to a baked-in
				// TrustedSyncCheckpoint to source the V2 masternode set. Trusted
				// checkpoints are deprecated and will be removed once the dynamic,
				// header-derived QC-verified pivot (SelectSnapPivot bootstrapped from
				// the epoch-switch header) is validated on mainnet. Logged once per
				// engagement for operator visibility.
				log.Warn("xdcSyncer: snap pivot used a DEPRECATED TrustedSyncCheckpoint anchor — migration tracked in #894",
					"anchor_checkpoint", best.Number)
				s.snapSelectFailures = 0 // success — reset failure counter
				s.snapPhase.Store(int32(phaseSnapRunning))
				return
			}
			// Peer-head fetch failed; fall back to the (stale) checkpoint pivot.
			// State download will likely fail, but we get further in the pipeline.
			s.snapPivotMu.Lock()
			s.snapPivot = &engine_v2.PivotResult{
				Block: hdr,
				Root:  best.Root,
				Depth: best.Number - cur.Number.Uint64(),
			}
			s.snapPivotMu.Unlock()
			log.Info("xdcSyncer: snap pivot fell back to TrustedSyncCheckpoint (peer-head fetch failed)",
				"block", best.Number,
				"hash", hdr.Hash().Hex(),
				"root", best.Root.Hex(),
				"masternodes", len(best.Masternodes))
			s.snapSelectFailures = 0 // success — reset failure counter
			s.snapPhase.Store(int32(phaseSnapRunning))
			return
		}
	}

	// Fallback path: ADR-807 QC-verification helper.
	candidates := s.rankedPeers()
	if len(candidates) == 0 {
		return
	}
	peer := candidates[0]
	if peer.BlockRange() == nil {
		return
	}
	var peerHead uint64
	if cfg != nil {
		for _, cp := range cfg.TrustedSyncCheckpoints {
			if cp.Number > peerHead {
				peerHead = cp.Number
			}
		}
	}
	if peerHead == 0 {
		// A.97 short-chain guard (refs #912): clamp the synthetic peerHead to the
		// peer's actual head block number. On short chains (devnet < 5000 blocks)
		// the synthetic localHead+5000 target doesn't exist, SelectSnapPivot
		// returns nil, and we fall back to FULL_SYNC — but maybeEngageSnap
		// re-engages on the next tick, creating a loop.
		//
		// Fetch the peer's real head block number. If it is below
		// xdcSnapEngageMinGap, fall back to FULL_SYNC immediately rather than
		// passing an unreachable target to SelectSnapPivot. Production chains
		// (mainnet > 73M, Apothem > 50M) are never affected by this guard.
		rng := peer.BlockRange()
		if rng != nil && rng.LatestBlockHash != (common.Hash{}) {
			if hdr := s.fetchHeaderByHash(peer, rng.LatestBlockHash); hdr != nil {
				actualPeer := hdr.Number.Uint64()
				if actualPeer < xdcSnapEngageMinGap {
					log.Info("xdcSyncer: snap pivot fallback skipped — peer head below xdcSnapEngageMinGap; reverting to FULL_SYNC",
						"peerHead", actualPeer, "minGap", xdcSnapEngageMinGap,
						"localHead", cur.Number.Uint64())
					s.snapSelectFailures++
					if s.snapSelectFailures >= xdcSnapSelectMaxRetries {
						s.snapPermanentFullFallback.Store(true)
						log.Warn("xdcSyncer: snap pivot selection failed repeatedly — permanently falling back to full-sync (no snap-capable peers)",
							"attempts", s.snapSelectFailures)
					}
					s.snapPhase.Store(int32(phaseFullSync))
					return
				}
				peerHead = actualPeer
			}
		}
		if peerHead == 0 {
			// Could not fetch real head; use synthetic but cap at xdcSnapEngageMinGap
			// minimum so SelectSnapPivot always has a meaningful search space.
			peerHead = cur.Number.Uint64() + xdcSnapEngageMinGap
		}
	}
	fetchHeader := func(n uint64) *types.Header {
		hdrs := s.fetchHeaderBatch(peer, n, 1)
		if len(hdrs) == 0 {
			return nil
		}
		return hdrs[0]
	}
	pivot := v2Engine.SelectSnapPivot(s.handler.chain, peerHead, fetchHeader, []uint64{})
	if pivot == nil {
		s.snapSelectFailures++
		if s.snapSelectFailures >= xdcSnapSelectMaxRetries {
			s.snapPermanentFullFallback.Store(true)
			log.Warn("xdcSyncer: snap pivot selection failed repeatedly — permanently falling back to full-sync (no snap-capable peers)",
				"attempts", s.snapSelectFailures)
		}
		log.Info("xdcSyncer: no safe pivot returned (both paths exhausted); falling back to FULL_SYNC",
			"peerHead", peerHead, "localHead", cur.Number.Uint64(),
			"consecutiveFailures", s.snapSelectFailures)
		s.snapPhase.Store(int32(phaseFullSync))
		return
	}
	s.snapPivotMu.Lock()
	s.snapPivot = pivot
	s.snapPivotMu.Unlock()
	log.Info("xdcSyncer: snap pivot selected (QC-verify path)",
		"block", pivot.Block.Number.Uint64(),
		"root", pivot.Root.Hex(),
		"depth", pivot.Depth,
		"peerID", peer.ID()[:12])
	s.snapSelectFailures = 0 // success — reset failure counter
	s.snapPhase.Store(int32(phaseSnapRunning))
}

// snapPivotBlock returns the selected pivot block number, or 0 if
// SelectSnapPivot has not run / returned nil. Used by heartbeat logs.
func (s *xdcSyncer) snapPivotBlock() uint64 {
	s.snapPivotMu.Lock()
	defer s.snapPivotMu.Unlock()
	if s.snapPivot == nil || s.snapPivot.Block == nil {
		return 0
	}
	return s.snapPivot.Block.Number.Uint64()
}

// snapRunningCheck dispatches the SNAP_RUNNING phase action. With the
// experimental env var set: invokes downloader.BeaconSync for the
// stored pivot exactly once, then waits for SyncEvent completion to
// transition the phase. Without the env var: logs + falls back to
// FULL_SYNC (Phase 3b.2 behavior).
//
// Refs #807 Phase 3b.3 + Phase 7 (keep-alive). The double-opt-in
// guards against accidental engagement.
//
// Phase 7: each tick (after BeaconSync is kicked off) we also issue a
// xdcSyncer-style fetchHeaderBatch to all peers. This keeps the eth
// subprotocol busy on the request format we KNOW peers respond to
// (the Phase 5 bench confirmed peers serve XDPOS2 BlockHeaders count=512
// when xdcSyncer asks; the downloader's own request format times out
// after 60s). Without these keep-alive pings the downloader's request
// timeout fires and the skeleton can't extend.
func (s *xdcSyncer) snapRunningCheck() {
	if !snapsyncExperimentalEnabled() {
		log.Info("xdcSyncer: SNAP_RUNNING — BeaconSync gated by XDC_EXPERIMENTAL_SNAPSYNC=1; falling back to FULL_SYNC",
			"pivot", s.snapPivotBlock())
		s.snapPhase.Store(int32(phaseFullSync))
		return
	}

	if s.snapBeaconStarted.Load() {
		// Already invoked; waiting for SyncEvent completion handler
		// to transition phase. The Phase 9 bridge routes XDPOS2
		// BlockHeaders responses into the downloader's dispatcher
		// (via peer.legacyHeaderReqID), so the downloader's own
		// skeleton-sync requests stay fulfilled and peers don't time
		// out. Phase 7's keep-alive header requests were removed:
		// they overwrote legacyHeaderReqID with their own ID, stealing
		// the response routing from the downloader's actual requests.
		log.Debug("xdcSyncer: SNAP_RUNNING — BeaconSync in progress",
			"pivot", s.snapPivotBlock())

		// #807 Phase 14.4: stall detection. If no account ranges have been
		// synced after the grace window, snap-sync is not progressing — either
		// state download was never reached (the B1 skeleton-floor bug fixed in
		// this file forced a ~104M-header genesis walk first) or peers served
		// empty GetAccountRange. Detect this and fall back to FULL_SYNC so the
		// node still catches up.
		//
		// NOTE: the correct cross-refs are #810 (stand up a snap-serving peer)
		// and #836 (real snap-sync acceptance / pivot-root coherence). The
		// earlier "#96" cite was a misdiagnosis — repo #96 is an unrelated,
		// closed genesis JSON-tag bug. The A2 audit (2026-06-18) further showed
		// default PBSS XDC nodes DO serve snap state (snap/1 registered at
		// SnapshotCache>0; handlers.go serves from TrieDB iterators), so an
		// empty AccountSynced after the B1 fix most likely means state download
		// was reached but the requested pivot root has no serving peer / drifted.
		const snapStallGrace = 10 * time.Minute
		startNS := s.snapBeaconStartedAt.Load()
		if startNS > 0 && time.Since(time.Unix(0, startNS)) > snapStallGrace {
			progress := s.handler.downloader.SnapSyncer.Progress()
			if progress.AccountSynced == 0 {
				log.Warn("xdcSyncer: snap-sync stalled — no AccountSynced after grace; falling back to FULL_SYNC",
					"grace", snapStallGrace,
					"hint", "state download not reached or peer served empty AccountRange (refs #810/#836)")
				s.snapPhase.Store(int32(phaseFullSync))
				s.snapBeaconStarted.Store(false)
				s.snapBeaconStartedAt.Store(0)
			}
		}
		return
	}
	// First time entering SNAP_RUNNING: kick BeaconSync.
	s.snapBeaconStarted.Store(true)
	s.snapBeaconStartedAt.Store(time.Now().UnixNano())

	s.snapPivotMu.Lock()
	pivot := s.snapPivot
	s.snapPivotMu.Unlock()
	if pivot == nil || pivot.Block == nil {
		log.Warn("xdcSyncer: SNAP_RUNNING entered with nil pivot — programming error; falling back to FULL_SYNC")
		s.snapPhase.Store(int32(phaseFullSync))
		s.snapBeaconStarted.Store(false)
		return
	}

	// #807 Phase 14.2: fetch the peer's CURRENT head header to pass as
	// BeaconSync's `head`. The downloader internally computes its
	// state-download pivot as `head.Number - fsMinFullBlocks` (=64) and
	// uses THAT block's stateRoot for snap.Sync — not our `pivot.Block`.
	// To land the downloader's pivot inside peer state-history retention
	// (path-scheme keeps ~90k blocks), `head` must be the peer's actual
	// chain tip. Passing pivot.Block as head would make the downloader
	// pick pivot.Block - 64 (correct), only if pivot.Block IS the tip;
	// since our pivot.Block is peer_head - snapPivotBackoff (=1024), we
	// want head = peer_head so downloader's pivot lands near peer_head.
	// #807 Phase 14.3: cap header-fetch attempts so a stalled fleet
	// doesn't hold the state machine in SNAP_RUNNING indefinitely.
	// fetchHeaderByHash has a 5s per-call timeout; cap at 3 candidates
	// → worst-case 15s before falling back to FULL_SYNC.
	const headFetchMaxAttempts = 3
	var headHdr *types.Header
	attempts := 0
	for _, p := range s.rankedPeers() {
		if attempts >= headFetchMaxAttempts {
			break
		}
		rng := p.BlockRange()
		if rng == nil || rng.LatestBlockHash == (common.Hash{}) {
			continue
		}
		attempts++
		if h := s.fetchHeaderByHash(p, rng.LatestBlockHash); h != nil {
			headHdr = h
			break
		}
	}
	if headHdr == nil {
		log.Warn("xdcSyncer: failed to fetch peer head for BeaconSync; falling back to FULL_SYNC",
			"attempts", attempts)
		s.snapPhase.Store(int32(phaseFullSync))
		s.snapBeaconStarted.Store(false)
		return
	}

	// #807 Phase 14.3: invalidate stale snap-sync persisted state when the
	// downloader's computed pivot has changed from the last run. Without
	// this, snap.Syncer.loadSyncStatus restores tasks/counters from the
	// previous (possibly wrong-root) sync attempt; root-agnostic hash
	// ranges replay correctly only when the previous run was healthy.
	// A pivot delta is the cheapest cross-run signal we have.
	// fsMinFullBlocks is unexported in eth/downloader; mirror its value
	// here. If upstream changes the constant, update this side too — the
	// invalidation logic depends on the offset matching.
	const downloaderFsMinFullBlocks = 64
	newDownloaderPivot := headHdr.Number.Uint64() - downloaderFsMinFullBlocks
	if lastPivot := rawdb.ReadLastPivotNumber(s.handler.database); lastPivot != nil && *lastPivot != newDownloaderPivot {
		log.Warn("xdcSyncer: invalidating stale snap-sync state (pivot changed)",
			"old_pivot", *lastPivot, "new_pivot", newDownloaderPivot)
		// Write garbage that fails JSON decode → snap.Syncer.loadSyncStatus
		// logs a decode error and falls through to fresh init.
		rawdb.WriteSnapshotSyncStatus(s.handler.database, []byte("xdc-pivot-changed-invalidated"))
	}

	log.Warn("xdcSyncer: invoking experimental snap-sync via downloader.BeaconSync",
		"head", headHdr.Number.Uint64(),
		"head_root", headHdr.Root.Hex(),
		"final_pivot", pivot.Block.Number.Uint64(),
		"final_root", pivot.Root.Hex(),
		"depth", pivot.Depth,
		"warning", "experimental — chaindata writes; rollback may be needed if BeaconSync mishandles XDPoS V2 finality semantics")

	// #807/#810 B1 fix: install the skeleton terminate-floor for the snap path
	// BEFORE kicking BeaconSync. Without it the snap skeleton has no floor
	// (operatorPivotNumber stays 0 here, and the A.90 dynamic floor is dormant
	// because d.fastSyncAnchorFn is always nil — its only wiring lives in a
	// logically-unreachable else-if in eth/backend.go), so the reverse walk-back
	// can only link() at genesis on a cold chain and the node reverse-walks
	// ~104M headers to genesis (64-87 min) before state download begins. That
	// long walk is also what lets the downloader's auto-advanced pivot drift
	// beyond peers' state-history retention (#807 Phase 14). Flooring at
	// newDownloaderPivot-1 (= head-64-1) mirrors SetFastSyncPivot's
	// SetTerminateFloor(number-1): it stops the walk at the state-download pivot
	// while retaining the pivot header [pivot..head] the downloader needs for the
	// state root. Reached only under XDC_EXPERIMENTAL_SNAPSYNC=1 (checked at the
	// top of this function), so default/full/fast/archive sync are unaffected.
	// #807/#810 B1+linkup fix: install the skeleton terminate-floor AND prime the
	// anchor window before BeaconSync — mirroring the dynamic Fast Sync path
	// (fastRunningCheck). Two things are required to make a fresh snap node reach
	// state download instead of reverse-walking ~104M headers to genesis:
	//
	//   1. Terminate floor at pivotNum (= head - pivotDistance, the downloader's
	//      snap state pivot). Floor AT pivotNum (not pivotNum-1): the skeleton
	//      exits walk-back when Tail <= floor+1, so floor=pivotNum stops the tail
	//      at pivotNum+1 and findBeaconAncestor's HasFastBlock probe checks
	//      beaconTail-1 == pivotNum — exactly the lowest header in the primed
	//      window below (A.90.1 rationale, beaconsync.go).
	//   2. PrimeFastSyncAnchor([head-pivotDist..head]): without the primed window,
	//      findBeaconAncestor's HasFastBlock(pivotNum) linkup probe fails on a cold
	//      chain with "beacon linkup unavailable locally" and snap reverts to
	//      FULL_SYNC. This is the missing companion to the floor — the FastSync
	//      path has it (fastRunningCheck), the snap path did not.
	//
	// All reached only under XDC_EXPERIMENTAL_SNAPSYNC=1, so default/full/fast/
	// archive sync are unaffected.
	dl := s.handler.downloader
	pivotDist := dl.FastSyncPivotDistance()
	if headHdr.Number.Uint64() > pivotDist {
		pivotNum := headHdr.Number.Uint64() - pivotDist
		dl.SetSnapTerminateFloor(pivotNum)
		log.Info("xdcSyncer: snap skeleton terminate-floor set — skeleton stops at pivot, no genesis walk",
			"floor", pivotNum, "downloader_pivot", pivotNum, "head", headHdr.Number.Uint64())

		// Prime the anchor window [head-pivotDist..head] so the linkup probe passes.
		// Walk back from headHdr one parent at a time via a peer (XDPOS2 wire serves
		// single headers by hash). primeHeadersNeeded = pivotDist+1 ascending headers.
		primeHeadersNeeded := int(pivotDist) + 1
		var primeSourcePeer *eth.Peer
		for _, p := range s.rankedPeers() {
			primeSourcePeer = p
			break
		}
		if primeSourcePeer != nil {
			primeHeaders := make([]*types.Header, 0, primeHeadersNeeded)
			primeHeaders = append(primeHeaders, headHdr)
			currentHdr := headHdr
			walkOK := true
			for i := 1; i < primeHeadersNeeded; i++ {
				parentHdr := s.fetchHeaderByHash(primeSourcePeer, currentHdr.ParentHash)
				if parentHdr == nil {
					log.Warn("xdcSyncer: snap prime-anchor walk-back failed — retrying next tick with fresh peer",
						"at_block", currentHdr.Number.Uint64()-1,
						"fetched", len(primeHeaders), "needed", primeHeadersNeeded)
					// Reset so the next tick re-selects a pivot and retries, rather
					// than kicking BeaconSync with an incomplete anchor.
					s.snapBeaconStarted.Store(false)
					s.snapBeaconStartedAt.Store(0)
					s.snapPhase.Store(int32(phaseSnapSelecting))
					walkOK = false
					break
				}
				// Prepend so the slice ends ascending: [head-pivotDist, ..., head].
				primeHeaders = append([]*types.Header{parentHdr}, primeHeaders...)
				currentHdr = parentHdr
			}
			if !walkOK {
				return
			}
			if err := dl.PrimeFastSyncAnchor(primeHeaders); err != nil {
				log.Warn("xdcSyncer: snap prime anchor failed — linkup may fail", "err", err)
			} else {
				// Pre-fetch ~2*Epoch ancestor headers below the prime window so the
				// XDPoS V2 engine can resolve masternodes (header.Validators) when
				// verifyHeader fires on pre-pivot blocks during the skeleton walk-back.
				s.prefetchAncestorEpochs(primeSourcePeer, primeHeaders[0])
			}
		}
	}

	// BeaconSync(head, final): the downloader picks its own state-download
	// pivot as head.Number - fsMinFullBlocks. `final` is the trusted
	// finalized point. We pass the peer's head as `head` (so downloader
	// pivot is near peer tip, within retention) and our xdcSyncer-selected
	// pivot.Block as `final` (the trusted ancestor for QC verification).
	//
	// BeaconSync returns quickly (it kicks the skeleton sync into a
	// channel and returns); the actual state download runs in the
	// downloader's worker goroutines. We listen for the completion
	// event via the subscription created in start().
	go func() {
		if err := s.handler.downloader.BeaconSync(headHdr, pivot.Block); err != nil {
			log.Error("xdcSyncer: BeaconSync failed; falling back to FULL_SYNC",
				"err", err, "pivot", pivot.Block.Number.Uint64())
			s.snapPhase.Store(int32(phaseFullSync))
			s.snapBeaconStarted.Store(false)
			s.snapBeaconStartedAt.Store(0)
			return
		}
		log.Info("xdcSyncer: BeaconSync kicked off; awaiting completion via SyncEvent",
			"pivot", pivot.Block.Number.Uint64())
	}()
}

// snapKeepAlive issues a lightweight header request to each connected
// peer using the xdcSyncer-style fetchHeaderBatch path. Phase 5 bench
// found peers respond to that request format with 512-header batches,
// while the downloader's skeleton-sync request format times out after
// 60s. The keep-alive pings keep the eth subprotocol busy on a known-
// working request format so peers don't time out the downloader's
// own (slower / differently-formatted) requests.
//
// Refs #807 Phase 7. Fire-and-forget; we don't care about the
// response, the request itself counts as activity.
func (s *xdcSyncer) snapKeepAlive() {
	pivot := s.snapPivotBlock()
	if pivot == 0 {
		return
	}
	// Request 1 header starting at pivot from each peer. Returns
	// quickly; goroutine isolates each peer so a slow peer doesn't
	// stall the tick.
	for _, p := range s.handler.peers.peers {
		ethP := p.Peer
		go func() {
			defer func() { _ = recover() }()
			s.fetchHeaderBatch(ethP, pivot, 1)
		}()
	}
}

// snapEventLoop listens for downloader SyncEvents and transitions the
// phase machine on completion. Started from xdcSyncer.start() and runs
// for the lifetime of the syncer. Refs #807 Phase 3b.3.
func (s *xdcSyncer) snapEventLoop() {
	syncCh := make(chan downloader.SyncEvent, 16)
	syncSub := s.handler.downloader.SubscribeSyncEvents(syncCh)
	defer syncSub.Unsubscribe()

	for {
		select {
		case <-s.quitCh:
			return
		case ev := <-syncCh:
			// Handle SyncEvents for both snap and fast sync paths.
			phase := xdcSnapPhase(s.snapPhase.Load())

			// --- Snap sync events ---
			if phase == phaseSnapRunning && s.snapBeaconStarted.Load() {
				switch ev.Type {
				case downloader.SyncStarted:
					log.Info("xdcSyncer: SyncStarted observed (snap)", "mode", ev.Mode)
				case downloader.SyncCompleted:
					log.Info("xdcSyncer: snap-sync complete; transitioning to TIP_CATCH",
						"pivot", s.snapPivotBlock(),
						"latest", func() string {
							if ev.Latest == nil {
								return "-"
							}
							return ev.Latest.Number.String()
						}())
					s.snapPhase.Store(int32(phaseTipCatch))
					s.snapBeaconStarted.Store(false)
				case downloader.SyncFailed:
					log.Warn("xdcSyncer: snap-sync failed; falling back to FULL_SYNC",
						"pivot", s.snapPivotBlock(), "err", ev.Err)
					s.snapPhase.Store(int32(phaseFullSync))
					s.snapBeaconStarted.Store(false)
				}
				continue
			}

			// --- Fast sync events (refs #844 A.7) ---
			if phase == phaseFastSyncRunning && s.fastBeaconStarted.Load() {
				switch ev.Type {
				case downloader.SyncStarted:
					log.Info("xdcSyncer: SyncStarted observed (fast)", "mode", ev.Mode)
				case downloader.SyncCompleted:
					log.Info("xdcSyncer: fast-sync complete; transitioning to TIP_CATCH",
						"latest", func() string {
							if ev.Latest == nil {
								return "-"
							}
							return ev.Latest.Number.String()
						}())
					s.snapPhase.Store(int32(phaseTipCatch))
					s.fastEngagementResolved.Store(true)
					s.fastBeaconStarted.Store(false)
					// A.70 (refs #844 #859): re-apply cached V2 masternode-snapshot
					// seeds. Fast-sync block-import's UpdateMasternodesFromHeader
					// at gap blocks runs BEFORE this point and overwrites our
					// initial seeds with contract-state-derived lists whose
					// truncation order may diverge from canonical header.Validators
					// near the top-N boundary.
					s.reapplyA70Seeds()
					// A.73 (refs #844 #859): DO NOT flip XdcBulkSyncMode here.
					//
					// At SyncCompleted the BeaconSync downloader has only just
					// reached the operator pivot (e.g. 103,309,269). The chain
					// head is still at the pivot and must walk FORWARD through
					// ~1.5–2 epochs of post-pivot blocks via fullSyncOnce()
					// before it actually catches up to a peer's tip. Those
					// post-pivot blocks include the first V2 epoch-switch past
					// the pivot, where calcMasternodes' CandidatePool truncation
					// can diverge from canonical header.Validators by exactly
					// one swap at the top-N boundary (comeback rotation; see
					// verifyHeader.go:198). The bulk-sync tolerance in
					// verifyHeader (lines 223-228) is precisely the mechanism
					// that keeps the chain advancing past this rotation, and
					// that tolerance only engages when XdcBulkSyncMode=true.
					//
					// Flip XdcBulkSyncMode=false later, in maybeTransitionToAtTip
					// (line 853), once the local head actually matches a peer's
					// claimed head — i.e. when there is no more catchup work
					// and fullVerify=true is correct.
					//
					// Pre-A.73 behaviour: flipped here, blocked at first
					// post-pivot epoch-switch with "validators not legit", node
					// stalled retrying the same segment forever. Empirically
					// verified on xdcscan a72v2: imported 1018 blocks past
					// pivot, then stalled at 103,310,288 (epoch switch).
				case downloader.SyncFailed:
					if s.handler.downloader.ConfigSyncMode() == ethconfig.FastSync {
						log.Warn("xdcSyncer: fast-sync SyncFailed — staying in fast-sync (--syncmode=fast pinned, cooldown then retry)",
							"err", ev.Err)
						s.fastBeaconStarted.Store(false)
						// A.24: re-arm via phaseFullSync (NOT phaseTipCatch — maybeEngageFast
						// only enters from phaseFullSync per sync_xdc.go:395).
						s.snapPhase.Store(int32(phaseFullSync))
						// A.24: schedule a cooldown-then-rearm goroutine. 30s gives the
						// previous syncToHead's cancelWg.Wait() time to drain the state-sync
						// pipeline (processFastSyncContent, fastStateSync.run, fastStateFetcher).
						// Without this drain, the next BeaconSync gets errBusy or races
						// queue.Reset against in-flight NodeData deliveries. Refs A.22 regression.
						if s.fastRetryArmed.CompareAndSwap(false, true) {
							go func() {
								defer s.fastRetryArmed.Store(false)
								select {
								case <-time.After(30 * time.Second):
								case <-s.quitCh:
									return
								}
								log.Info("xdcSyncer: fast-sync cooldown elapsed — re-arming for next tick")
								// Do NOT call maybeSync directly. Let the 10s forceSync ticker
								// re-enter via maybeEngageFast naturally.
							}()
						}
					} else {
						log.Warn("xdcSyncer: fast-sync failed; falling back to FULL_SYNC",
							"err", ev.Err)
						s.snapPhase.Store(int32(phaseFullSync))
						s.fastEngagementResolved.Store(true)
						s.fastBeaconStarted.Store(false)
					}
				}
				continue
			}
		}
	}
}

// fullSyncOnce fetches one batch (headers + bodies) and imports it via
// blockchain.InsertChain. This was the body of the old `syncOnce`
// before Phase 3b split the entry into phase-machine dispatch.
//
// Bulk-sync rate optimisation: in production we observe individual peers
// returning headers=0 (the peer doesn't have / won't serve our requested
// block range) yet still ranking high in bestPeer() by TD. Without retry
// logic the syncer wastes ~2s per cycle × N retries until a different
// peer is picked. We now try up to xdcMaxPeerTriesPerSync candidates
// within a single syncOnce, in descending TD order, skipping peers that
// already proved empty for the same fromBlock this round.
func (s *xdcSyncer) fullSyncOnce() {
	cur := s.handler.chain.CurrentBlock()
	if cur == nil {
		return
	}
	// A.15: when --syncmode=fast is pinned, suppress bulk-import until fast-sync
	// has had its chance to engage. Without this, bulk-import races maybeEngageFast
	// on cold start and the syncer locks into FULL_SYNC before peers cross threshold.
	if s.handler.downloader.ConfigSyncMode() == ethconfig.FastSync && !s.fastEngagementResolved.Load() {
		// Throttled log to avoid 500ms spam; reuse fastWaitLastLogAt cadence.
		if now := time.Now().UnixNano(); now-s.fastWaitLastLogAt.Load() > int64(xdcFastSyncPeerWaitLogEvery) {
			s.fastWaitLastLogAt.Store(now)
			log.Info("xdcSyncer: bulk-import suppressed pending fast-sync engagement",
				"head", cur.Number.Uint64(),
				"peers", s.handler.peers.len())
		}
		return
	}
	// A.74: one-shot startup repair for placeholder bodies in the
	// [primeBase..pivot-1] anchor gap. Must run before InsertChain so
	// HookReward's signing-tx walk sees real bodies for the filter blocks.
	if !s.anchorGapRepaired.Load() {
		if peers := s.rankedPeers(); len(peers) > 0 {
			s.repairAnchorGapBodies(peers[0])
		}
	}
	localHead := cur.Number.Uint64()
	fromBlock := localHead + 1

	candidates := s.rankedPeers()
	if len(candidates) == 0 {
		return
	}

	// A.75: parallel fan-out to top xdcMaxPeerTriesPerSync candidates.
	// Worst-case round time: xdcBulkReqTimeout (4s) instead of
	// xdcMaxPeerTriesPerSync × xdcReqTimeout (32s) — collapses the
	// 1–6.5 min inter-burst stall to ≤ 4s per round.
	// A.78: fetchHeadersParallel now returns racedIDs — the set of peers that
	// received range-0 requests. Lookahead skips these to avoid re-querying
	// peers already rate-limited this round; it uses only fresh (non-raced) peers.
	res, atTipCount, tried, racedIDs := s.fetchHeadersParallel(candidates, fromBlock, cur.Hash())
	if res == nil {
		if atTipCount > 0 && atTipCount == tried {
			// Every candidate agrees our head is their head — genuinely at tip.
			// A.99.4 + A.98: FULL_SYNC phase never calls maybeTransitionToAtTip (that
			// only works from phaseTipCatch). Flip XdcBulkSyncMode=false here so
			// validators can begin BFT block production. This breaks the A.93
			// circular dependency: bulk-sync mode kept checkpointSyncNoState=true
			// during catch-up; now caught up → strict verify + real state execution.
			//
			// A.99.5 guard: Only flip when localHead is past the V2 switch block.
			// On fresh devnets, peers may briefly agree on a low head before a peer
			// with the full chain connects — flipping there would be a false at-tip.
			cfg := s.handler.chain.Config()
			v2SwitchBlock := uint64(0)
			if cfg.XDPoS != nil && cfg.XDPoS.V2 != nil && cfg.XDPoS.V2.SwitchBlock != nil {
				v2SwitchBlock = cfg.XDPoS.V2.SwitchBlock.Uint64()
			}
			if v2SwitchBlock > 0 && localHead < v2SwitchBlock {
				log.Debug("xdcSyncer: at-tip flip deferred — localHead below V2 switch block",
					"localHead", localHead, "v2SwitchBlock", v2SwitchBlock)
				return
			}
			if core.XdcBulkSyncMode.Load() {
				core.XdcBulkSyncMode.Store(false)
				log.Info("xdcSyncer: FULL_SYNC at canonical tip — flipped XdcBulkSyncMode=false",
					"head", localHead)
				// A.97.3 (refs #894 #890 #165): a snapshot-restored mainnet/apothem
				// node can reach FULL_SYNC at-tip with checkpointSyncNoState STILL set.
				// Its fast-snapshot state was incomplete (missing trie nodes), so the
				// A.93/A.94 path fired during catch-up and persisted the flag. Left
				// sticky, the node imports blocks with no executed state (mgas=0,
				// triedirty=0), so on every restart repair() finds head-state missing
				// and rewinds to the snapshot base. The snap-heal that fixes this only
				// ran from phaseTipCatch (maybeTransitionToAtTip); a FULL_SYNC node
				// never reaches it. Trigger it here too. maybeStartSnapHeal is a no-op
				// unless XDC_SNAP_HEAL=1 AND checkpointSyncNoState is set AND snap peers
				// are present; it heals the head-root trie, runs gate B
				// (ReexecuteAndValidateRange) and only then clears the flag — so it is
				// self-protecting. Gated to isSnapHealTrustedChain (mainnet/apothem/
				// known devnets incl. 5151) — gate B's hash verification makes clearing
				// safe on any of them; the clear below is now state-guarded, so the
				// naive/unverified clear that #1800 made unsafe can no longer happen.
				if cid := cfg.ChainID; cid != nil && isSnapHealTrustedChain(cid.Uint64()) {
					s.maybeStartSnapHeal()
				}
				// ClearCheckpointSyncNoState is called again here (it had been REMOVED).
				// The historical removal reason: an UNCONDITIONAL clear at at-tip detection
				// re-enabled strict state validation on devnet-5151, which walked back through
				// #1800 and re-tripped the unresolved cross-client parity divergence
				// (remote: ed87d976..., local: f23c9be3...). ClearCheckpointSyncNoState now
				// refuses to clear unless the head state is actually present in the trie DB.
				// A node in that 5151 state has EmptyRootHash at head, so the flag stays sticky
				// exactly as the removal intended — but it now logs a loud ERROR naming the
				// required repair (resync) instead of wedging the miner in silence, which is
				// what hid this for hours on netv12. Refs #165, #894.
				s.handler.chain.ClearCheckpointSyncNoState()
				// A.99.4b: force BFT engine re-init from chain head so currentRound is
				// set from head's embedded QC rather than whatever stale round the engine
				// last processed during bulk-sync catch-up.
				if xdposEngine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS); ok && xdposEngine.EngineV2 != nil {
					headHeader := s.handler.chain.GetHeaderByNumber(localHead)
					if headHeader != nil {
						if err := xdposEngine.EngineV2.ReinitBFT(s.handler.chain, headHeader); err != nil {
							log.Warn("xdcSyncer: BFT re-init after at-tip flip failed",
								"head", localHead, "err", err)
						} else {
							log.Info("xdcSyncer: BFT re-initialized from canonical head",
								"head", localHead)
						}
					}
				}
			}
			return
		}
		log.Debug("xdcSyncer: no peer served headers this round",
			"from", fromBlock, "tried", tried, "atTipPeers", atTipCount)
		return
	}
	peer := res.peer
	r := res.rng
	headers := res.headers

	// A.77+A.78: kick off lookahead header fetches for ranges 1..xdcLookaheadStreams
	// from FRESH candidates — peers not queried in the fan-out round — concurrently
	// with range-0 body fetch + InsertChain. Skipping raced peers prevents sending
	// a second request to peers that already returned count=0 this round.
	type lookaheadResult struct {
		peer    *eth.Peer
		headers []*types.Header
	}
	lookaheadChs := make([]chan *lookaheadResult, 0, xdcLookaheadStreams)
	{
		lhIdx := 0
		for _, c := range candidates {
			if lhIdx >= xdcLookaheadStreams {
				break
			}
			// Skip the range-0 winner and any peer already raced this round.
			if c.ID() == peer.ID() || racedIDs[c.ID()] {
				continue
			}
			lhFrom := fromBlock + uint64(len(headers)) + uint64(lhIdx)*xdcLookaheadRangeSize
			ch := make(chan *lookaheadResult, 1)
			go func(outCh chan *lookaheadResult, lhPeer *eth.Peer, from uint64) {
				hdrs := s.fetchHeaderBatchTimeout(lhPeer, from, xdcBatchSize, xdcBulkReqTimeout)
				outCh <- &lookaheadResult{peer: lhPeer, headers: hdrs}
			}(ch, c, lhFrom)
			lookaheadChs = append(lookaheadChs, ch)
			lhIdx++
		}
		if len(lookaheadChs) > 0 {
			log.Info("xdcSyncer: A.77 lookahead launched",
				"streams", len(lookaheadChs), "candidates", len(candidates), "raced", len(racedIDs))
		}
	}

	// Fetch bodies for range 0 (concurrent with lookahead goroutines above).
	hashes := make([]common.Hash, len(headers))
	for i, h := range headers {
		hashes[i] = h.Hash()
	}
	bodies := s.fetchBodiesBatch(peer, hashes)
	if len(bodies) == 0 {
		log.Debug("xdcSyncer: empty body batch", "headers", len(headers))
		return
	}

	blocks := s.buildBlocks(headers, bodies)
	if len(blocks) == 0 {
		log.Debug("xdcSyncer: no valid blocks to import")
		return
	}

	// Insert + execute range 0.
	n, err := s.handler.chain.InsertChain(blocks)
	if err != nil {
		log.Warn("xdcSyncer: InsertChain failed",
			"from", fromBlock, "attempted", len(blocks), "inserted", n, "err", err)
		// #952 Option B: if the advertised NEXT block (at/just above our head) is
		// persistently INVALID — a consensus-validation error, NOT a missing parent
		// (ErrUnknownAncestor) — then the network is feeding bad blocks above our
		// canonical tip and WE are at the valid tip. The at-tip XdcBulkSyncMode clear
		// (#190) requires 3 consecutive at-tip heartbeats and never accumulates them
		// under peer churn / a stale head when the chain is halted, so the miner stays
		// gated forever. Clear XdcBulkSyncMode here so the producer can seal the valid
		// successor and unwedge the chain (e.g. apothem halted at 83,088,791 by an
		// out-of-bound 420M gas limit above our 419,589,845 head — #952). NON-MAINNET
		// only: mainnet must never relax on a validation error. Guard on fromBlock being
		// at/just above head so a deep-history invalid block cannot trip it.
		if !errors.Is(err, consensus.ErrUnknownAncestor) &&
			s.handler.chain.Config().ChainID != nil &&
			s.handler.chain.Config().ChainID.Uint64() != 50 {
			if cur := s.handler.chain.CurrentBlock(); cur != nil && fromBlock <= cur.Number.Uint64()+1 {
				if core.XdcBulkSyncMode.Load() {
					log.Warn("xdcSyncer: #952 next block invalid at tip — clearing XdcBulkSyncMode so miner can seal the valid successor",
						"from", fromBlock, "head", cur.Number.Uint64(), "err", err)
					core.XdcBulkSyncMode.Store(false)
				}
			}
		}
		// #951 Defect 1c (Part B): before the slow rollback, try the fast fork-heal —
		// fetch the co-signed sibling of the batch's missing parent and adopt it so
		// bc.reorg swaps the empty-N fork tip for the co-signed canonical in one step.
		// On a heal, fall through to the success bookkeeping below (stuck-state reset,
		// XdcForkRecovering clear, HandleProposedBlock); on a miss, roll back as before.
		if !(errors.Is(err, consensus.ErrUnknownAncestor) && s.adoptCoSignedSibling(blocks, peer)) {
			s.maybeRollbackOnUnknownAncestor(fromBlock, peer, err)
			return
		}
	}
	// Successful import (or healed via co-signed-sibling adoption) — clear stuck state.
	if s.stuckFailures > 0 {
		s.stuckFrom = 0
		s.stuckFailures = 0
		s.stuckRolledBack = 0
	}
	// #951: back on the canonical chain — let the miner resume sealing.
	core.XdcForkRecovering.Store(false)
	log.Info("xdcSyncer: imported batch",
		"from", fromBlock, "count", n,
		"newHead", s.handler.chain.CurrentBlock().Number.Uint64(),
		"peerTD", r.LatestBlock) // XDPOS2 wire: TD, not block number

	// Advance engine's highestQuorumCert to the just-imported tip so the miner
	// worker's FindParentBlockToAssign returns the actual chain head and Prepare
	// never sees "parent hash and QC hash mismatch".  Mirrors canonical
	// XDPoSChain/eth/downloader/downloader.go:1543-1549 exactly.
	if engine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS); ok {
		go func(h *types.Header) {
			if err := engine.HandleProposedBlock(s.handler.chain, h); err != nil {
				log.Debug("xdcSyncer: HandleProposedBlock error",
					"block", h.Number, "hash", h.Hash(), "err", err)
			}
		}(blocks[len(blocks)-1].Header())
	}

	// A.77: collect and import lookahead ranges in order.
	// Wait up to xdcBulkReqTimeout for each range; stop at first failure or gap.
	for _, ch := range lookaheadChs {
		var lhRes *lookaheadResult
		select {
		case lhRes = <-ch:
		case <-time.After(xdcBulkReqTimeout):
			return
		case <-s.quitCh:
			return
		}
		if len(lhRes.headers) == 0 {
			log.Info("xdcSyncer: A.77 lookahead empty", "peer", lhRes.peer.ID()[:8])
			return
		}
		lhHashes := make([]common.Hash, len(lhRes.headers))
		for i, h := range lhRes.headers {
			lhHashes[i] = h.Hash()
		}
		lhBodies := s.fetchBodiesBatch(lhRes.peer, lhHashes)
		if len(lhBodies) == 0 {
			return
		}
		lhBlocks := s.buildBlocks(lhRes.headers, lhBodies)
		if len(lhBlocks) == 0 {
			return
		}
		lhFrom := lhRes.headers[0].Number.Uint64()
		lhN, lhErr := s.handler.chain.InsertChain(lhBlocks)
		if lhErr != nil {
			log.Warn("xdcSyncer: A.77 lookahead InsertChain failed",
				"from", lhFrom, "attempted", len(lhBlocks), "inserted", lhN, "err", lhErr)
			// #951 Defect 1c (Part B): same fast fork-heal as range 0 — adopt the
			// co-signed sibling before falling back to the slow rollback.
			if !(errors.Is(lhErr, consensus.ErrUnknownAncestor) && s.adoptCoSignedSibling(lhBlocks, lhRes.peer)) {
				s.maybeRollbackOnUnknownAncestor(lhFrom, lhRes.peer, lhErr)
				return
			}
		}
		log.Info("xdcSyncer: A.77 lookahead batch",
			"from", lhFrom, "count", lhN,
			"newHead", s.handler.chain.CurrentBlock().Number.Uint64())

		// Same highestQC advance for lookahead batches (canonical parity).
		if engine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS); ok {
			go func(h *types.Header) {
				if err := engine.HandleProposedBlock(s.handler.chain, h); err != nil {
					log.Debug("xdcSyncer: A.77 HandleProposedBlock error",
						"block", h.Number, "hash", h.Hash(), "err", err)
				}
			}(lhBlocks[len(lhBlocks)-1].Header())
		}
	}
}

// buildBlocks pairs headers with bodies into types.Blocks. Stops at the first
// malformed body. Used by fullSyncOnce for both range-0 and A.77 lookahead ranges.
func (s *xdcSyncer) buildBlocks(headers []*types.Header, bodies []eth.BlockBody) types.Blocks {
	pairCount := len(bodies)
	if pairCount > len(headers) {
		pairCount = len(headers)
	}
	blocks := make(types.Blocks, 0, pairCount)
	for i := 0; i < pairCount; i++ {
		txs, err := bodies[i].Transactions.Items()
		if err != nil {
			log.Debug("xdcSyncer: bad txs in body", "idx", i, "err", err)
			break
		}
		uncles, err := bodies[i].Uncles.Items()
		if err != nil {
			log.Debug("xdcSyncer: bad uncles in body", "idx", i, "err", err)
			break
		}
		body := types.Body{Transactions: txs, Uncles: uncles}
		if bodies[i].Withdrawals != nil {
			if wd, err := bodies[i].Withdrawals.Items(); err == nil {
				body.Withdrawals = wd
			}
		}
		blocks = append(blocks, types.NewBlockWithHeader(headers[i]).WithBody(body))
	}
	return blocks
}

// adoptCoSignedSibling implements the #951 Defect 1c "Part B" fast fork-heal for the
// V1 double-validation churn that otherwise prevents xdc04 from staying at-tip (and
// therefore from minting) above the first epoch.
//
// Above the epoch boundary every V1 block N exists in two forms that share parent,
// number, and Difficulty (Difficulty is in SigHash, unchanged by co-signing) but
// differ in header.Validator and therefore in block hash: the M1's just-minted EMPTY
// version and the assigned-M2's CO-SIGNED version. Legacy canonical is always the
// co-signed version, so a by-number pull of N+1 returns co-signed-N+1 whose parent is
// co-signed-N. When our local head is the fork-side EMPTY-N (pre-existing chaindata, a
// transient race, or a self-minted M1 block before Part C), that batch fails with
// consensus.ErrUnknownAncestor — co-signed-N is absent.
//
// The legacy-faithful resolution is to ADOPT the co-signed sibling, not to roll back.
// Both empty-N and co-signed-N descend from the SAME parent co-signed-(N-1), which we
// already have on disk (it is empty-N's parent too). So we fetch co-signed-N by its
// hash (= the failing batch's parent_hash) from the same peer that served the batch,
// InsertChain it — bc.reorg performs a single 1-block sibling swap from empty-N to
// co-signed-N — then re-insert the batch, which now extends cleanly. This replaces the
// slow maybeRollbackOnUnknownAncestor path (8 consecutive failures, then a SetHead
// window that re-stamps the miner cooldown and churns the head), so the head stays
// fresh, XdcBulkSyncMode flips false at-tip, and the miner ungates.
//
// V1-only and gated strictly above Epoch (V2 uses QC-chain fork choice, not the
// single-signature header.Validator). Uses only existing primitives — no wire, rawdb,
// core, or consensus change, and zero change to block bytes (we adopt the legacy
// canonical co-signed block verbatim). Returns true iff the sibling was fetched,
// inserted, and the batch re-imported successfully; any miss returns false so the
// caller falls back to maybeRollbackOnUnknownAncestor. Refs #951.
func (s *xdcSyncer) adoptCoSignedSibling(blocks types.Blocks, peer *eth.Peer) bool {
	if len(blocks) == 0 || peer == nil {
		return false
	}
	first := blocks[0]
	cfg := s.handler.chain.Config()
	if cfg == nil || cfg.XDPoS == nil || first.NumberU64() == 0 {
		return false
	}
	parentNum := first.NumberU64() - 1
	// V1 double-validation only exists above the first epoch.
	if parentNum <= cfg.XDPoS.Epoch {
		return false
	}
	// V1 only — V2 blocks resolve forks via the QC chain, not header.Validator.
	if cfg.XDPoS.V2 != nil && cfg.XDPoS.V2.SwitchBlock != nil &&
		parentNum > cfg.XDPoS.V2.SwitchBlock.Uint64() {
		return false
	}
	parentHash := first.ParentHash()
	// If we already have the batch's parent, the failure is not the empty/co-signed
	// sibling case — let the normal rollback path handle it.
	if s.handler.chain.HasBlock(parentHash, parentNum) {
		return false
	}
	// Fetch the co-signed sibling (the batch's missing parent) by hash from the same
	// peer that served the batch — a by-hash request returns exactly that block.
	hdr := s.fetchHeaderByHash(peer, parentHash)
	if hdr == nil || hdr.Number == nil || hdr.Number.Uint64() != parentNum || hdr.Hash() != parentHash {
		return false
	}
	// The co-signed sibling must link to a block we already have (co-signed-(N-1));
	// otherwise this is a deeper gap that needs the rollback path, not a 1-block swap.
	if !s.handler.chain.HasBlock(hdr.ParentHash, parentNum-1) {
		return false
	}
	bodies := s.fetchBodiesBatch(peer, []common.Hash{parentHash})
	if len(bodies) == 0 {
		return false
	}
	sib := s.buildBlocks([]*types.Header{hdr}, bodies)
	if len(sib) != 1 {
		return false
	}
	// Insert the co-signed sibling — bc.reorg swaps empty-N for co-signed-N.
	if _, err := s.handler.chain.InsertChain(sib); err != nil {
		log.Debug("xdcSyncer: adopt co-signed sibling — sibling insert failed",
			"number", parentNum, "hash", parentHash, "err", err)
		return false
	}
	// Re-insert the original batch; it now extends the adopted co-signed head.
	if _, err := s.handler.chain.InsertChain(blocks); err != nil {
		log.Warn("xdcSyncer: adopt co-signed sibling — batch re-insert failed",
			"from", first.NumberU64(), "err", err)
		return false
	}
	log.Info("xdcSyncer: adopted co-signed sibling (Defect 1c fast fork-heal)",
		"siblingNum", parentNum, "siblingHash", parentHash,
		"newHead", s.handler.chain.CurrentBlock().Number.Uint64())
	// Back on canonical — clear fork-recovery so the miner can resume sealing.
	core.XdcForkRecovering.Store(false)
	return true
}

// repairAnchorGapBodies is the A.74 startup repair for nodes synced with
// geth-a73 or earlier. PrimeFastSyncAnchor wrote empty placeholder bodies for
// the entire fast-sync anchor window. A.72's pre-fetch ended at primeBase-1,
// leaving the 64-block gap [primeBase..pivot-1] with placeholder bodies that
// cause HookReward's signing-tx walk to miss 4 filter blocks and compute an
// incorrect totalSigner count, producing a wrong stateRoot ("invalid merkle
// root") at the first post-pivot epoch-switch block.
//
// This function scans the 128 blocks just below the operator pivot for
// placeholder bodies (header.TxHash != EmptyTxsHash but stored body has 0
// txs), fetches the real bodies from the supplied peer, and writes them
// directly to rawdb — bypassing PrimeAncestorBodies's HasBody gate.
//
// Sets anchorGapRepaired=true on success or when no repair is needed.
// Leaves anchorGapRepaired=false on transient errors (no peer response) so
// the next fullSyncOnce tick retries.
//
// Refs #844 #859 A.74.
func (s *xdcSyncer) repairAnchorGapBodies(peer *eth.Peer) {
	db := s.handler.database
	// A.74.1 (refs #187): source the fast-sync pivot from BOTH the operator-pin
	// (when set) AND the rawdb snap-sync marker. The dynamic-pivot path
	// (--no-auto-pivot) never calls SetFastSyncPivot, so OperatorPivot() returns 0
	// and the old guard made this repair a no-op on exactly the nodes that need it.
	// ReadLastPivotNumber is written for the dynamic pivot too, so it is the reliable
	// source (already used at line ~1670).
	opPivotNum, _, _ := s.handler.downloader.OperatorPivot()
	if opPivotNum == 0 {
		if lp := rawdb.ReadLastPivotNumber(db); lp != nil {
			opPivotNum = *lp
		}
	}
	if opPivotNum == 0 {
		s.anchorGapRepaired.Store(true)
		return
	}
	// Scan the FULL V2 reward lookback below the pivot, INCLUSIVE of the pivot block.
	// The first post-pivot V2 epoch-switch reward block walks back 2*Epoch
	// (GetSigningTxCount, eth/hooks/engine_v2_hooks.go), so a placeholder body anywhere
	// in [pivot-2*Epoch .. pivot] undercounts totalSigner and diverges the post-state
	// root. The pivot block itself is a placeholder (the snap-pivot commit writes the
	// header but leaves an empty body), and the old [pivot-128..pivot-1] window EXCLUDED
	// it — the exact #187 wedge on apothem (block 83,049,946 wrong root; missing body at
	// pivot 83,048,446). Widen to 3*Epoch (2 epochs lookback + 1 epoch margin for
	// timeout-stretched V2 epochs) and make scanHi inclusive. Refs #187 #844 #859.
	scanWindow := uint64(128)
	if cfg := s.handler.chain.Config(); cfg != nil && cfg.XDPoS != nil && cfg.XDPoS.Epoch > 0 {
		scanWindow = 3 * cfg.XDPoS.Epoch
	}
	scanLo := uint64(0)
	if opPivotNum > scanWindow {
		scanLo = opPivotNum - scanWindow
	}
	scanHi := opPivotNum // INCLUSIVE of the pivot block (was opPivotNum-1)

	type gapBlock struct {
		num  uint64
		hash common.Hash
		hdr  *types.Header
	}
	var gaps []gapBlock
	for n := scanLo; n <= scanHi; n++ {
		canonHash := rawdb.ReadCanonicalHash(db, n)
		if canonHash == (common.Hash{}) {
			continue
		}
		hdr := rawdb.ReadHeader(db, canonHash, n)
		if hdr == nil || hdr.TxHash == types.EmptyTxsHash {
			continue
		}
		body := rawdb.ReadBody(db, canonHash, n)
		if body == nil || len(body.Transactions) == 0 {
			gaps = append(gaps, gapBlock{n, canonHash, hdr})
		}
	}
	if len(gaps) == 0 {
		log.Info("xdcSyncer: A.74 gap repair — no placeholder bodies detected",
			"scanLo", scanLo, "scanHi", scanHi, "pivot", opPivotNum)
		s.anchorGapRepaired.Store(true)
		return
	}
	log.Info("xdcSyncer: A.74 gap repair — placeholder bodies detected",
		"count", len(gaps), "first", gaps[0].num, "last", gaps[len(gaps)-1].num,
		"pivot", opPivotNum, "peer", peer.ID()[:10])

	hdrSlice := make([]*types.Header, len(gaps))
	hashSlice := make([]common.Hash, len(gaps))
	for i, g := range gaps {
		hdrSlice[i] = g.hdr
		hashSlice[i] = g.hash
	}
	// A.74.1: the widened scan can yield up to 3*Epoch hashes; XDPOS2 caps body
	// responses per request, so fetch in chunks of 192 (max body fetch). Bodies come
	// back in the same order as requested, matching hdrSlice.
	const bodyFetchBatch = 192
	bodies := make([]eth.BlockBody, 0, len(hashSlice))
	for off := 0; off < len(hashSlice); off += bodyFetchBatch {
		end := off + bodyFetchBatch
		if end > len(hashSlice) {
			end = len(hashSlice)
		}
		chunk := s.fetchBodiesBatch(peer, hashSlice[off:end])
		if len(chunk) == 0 {
			log.Warn("xdcSyncer: A.74 gap repair — peer returned no bodies for chunk, will retry next tick",
				"peer", peer.ID()[:10], "chunkFrom", off, "requested", end-off)
			return // don't mark done; retry on next fullSyncOnce tick
		}
		bodies = append(bodies, chunk...)
	}
	pairCount := len(bodies)
	if pairCount > len(hdrSlice) {
		pairCount = len(hdrSlice)
	}
	batch := db.NewBatch()
	written := 0
	for i := 0; i < pairCount; i++ {
		txs, err := bodies[i].Transactions.Items()
		if err != nil || len(txs) == 0 {
			continue
		}
		uncles, err := bodies[i].Uncles.Items()
		if err != nil {
			continue
		}
		body := &types.Body{Transactions: txs, Uncles: uncles}
		if bodies[i].Withdrawals != nil {
			if wd, err := bodies[i].Withdrawals.Items(); err == nil {
				body.Withdrawals = wd
			}
		}
		rawdb.WriteBody(batch, hdrSlice[i].Hash(), hdrSlice[i].Number.Uint64(), body)
		written++
	}
	if err := batch.Write(); err != nil {
		log.Error("xdcSyncer: A.74 gap repair — batch write failed", "err", err)
		return
	}
	log.Info("xdcSyncer: A.74 gap repair — complete",
		"placeholders_found", len(gaps), "real_bodies_written", written,
		"pivot", opPivotNum)
	s.anchorGapRepaired.Store(true)
}

// maybeRollbackOnUnknownAncestor implements geth-aligned self-healing for
// the V2 BFT competing-slot fork issue (issue #801). When our local tip
// lands on a non-canonical block produced in the same slot as canonical,
// peers offering canonical block N+1 fail to extend our chain because the
// parent_hash doesn't match. Upstream geth's downloader runs a
// findAncestor binary search to locate the common ancestor; xdcSyncer
// lacks that, so we trigger a fixed rollback after the same fromBlock
// fails consensus.ErrUnknownAncestor repeatedly.
//
// State machine:
//  1. On UnknownAncestor for fromBlock, increment a per-fromBlock counter.
//  2. When the counter hits xdcUnknownAncestorThreshold consecutive failures,
//     the local head must be on a fork — peer is consistently right, we're
//     consistently wrong.
//  3. Call BlockChain.SetHead(localHead - window) with exponentially
//     growing window, capped at xdcRollbackWindowMax cumulative.
//  4. Reset failure counter and stuckFrom so the next attempt at the new
//     fromBlock can itself accumulate failures (recursive rollback).
//  5. Reset the entire stuck-period on next successful import.
//
// Note: the initial implementation required N *distinct peers* to fail
// before triggering rollback. Empirical observation showed a single bestPeer
// can persistently serve canonical-N+1 (which we keep rejecting because our
// N is fork-side), and that distinct-peer requirement never fires. We now
// count failures regardless of peer.
func (s *xdcSyncer) maybeRollbackOnUnknownAncestor(fromBlock uint64, peer *eth.Peer, err error) {
	// Two failure-mode signatures trigger fork-recovery rollback:
	//   1. consensus.ErrUnknownAncestor — peer's block N+1 parent_hash differs
	//      from our local N hash (classic competing-slot fork acceptance).
	//   2. "not its turn" — local masternode computation disagrees with the
	//      canonical proposer for the round. Surfaces when our chain's
	//      recent history includes a fork-side block whose epoch-switch
	//      data poisons getEpochSwitchInfoWithParents on subsequent
	//      forward sync. Refs #802 task 3 follow-up + #801.
	// Both indicate "local tip is on a different chain than canonical" —
	// same fix applies: rollback + peer taint.
	if !errors.Is(err, consensus.ErrUnknownAncestor) &&
		!strings.Contains(err.Error(), "not its turn") {
		return
	}
	if fromBlock != s.stuckFrom {
		s.stuckFrom = fromBlock
		s.stuckFailures = 1
		return
	}
	s.stuckFailures++
	// #951: signal the miner to pause sealing while we're fork-side. Set as soon
	// as failures begin accumulating (before the rollback threshold) so a
	// producer doesn't keep minting on the orphaned tip and recreate the fork.
	core.XdcForkRecovering.Store(true)
	if s.stuckFailures < xdcUnknownAncestorThreshold {
		return
	}
	if s.stuckRolledBack >= xdcRollbackWindowMax {
		log.Error("xdcSyncer: stuck on fork past rollback cap — manual intervention required",
			"from", fromBlock, "rolledBack", s.stuckRolledBack,
			"cap", xdcRollbackWindowMax)
		return
	}
	// Window doubles each consecutive trigger so we escape quickly from deep forks.
	window := uint64(xdcRollbackWindowInit) * (1 << (s.stuckRolledBack / xdcRollbackWindowInit))
	if window+s.stuckRolledBack > xdcRollbackWindowMax {
		window = xdcRollbackWindowMax - s.stuckRolledBack
	}
	cur := s.handler.chain.CurrentBlock()
	if cur == nil || cur.Number.Uint64() <= window {
		return
	}
	target := cur.Number.Uint64() - window
	// Mark this peer as fork-tainted so rankedPeers() skips them for xdcForkTaintTTL.
	// This is the geth-aligned peer rotation that prevents post-rollback bulk-sync
	// from re-importing the same fork via the same peer (issue #802 task 2).
	s.taintPeer(peer)
	log.Warn("xdcSyncer: fork detected — rolling back to escape competing-slot block (refs #801)",
		"localHead", cur.Number.Uint64(), "localHash", cur.Hash(),
		"failedFrom", fromBlock, "consecutiveFailures", s.stuckFailures,
		"rollbackTo", target, "window", window, "cumulative", s.stuckRolledBack+window,
		"taintedPeer", peer.ID(),
		"trigger", err.Error())
	if err := s.handler.chain.SetHead(target); err != nil {
		log.Error("xdcSyncer: SetHead failed during rollback", "target", target, "err", err)
		return
	}
	// #951 oscillation gate: stamp the rollback time so the miner holds off sealing
	// until rollbacks stop recurring (i.e. we've caught up to a stable tip). Without
	// this, the miner re-mints its superseded in-turn block the instant it re-reaches
	// the contested height, recreating the fork (head bounced 143↔784, never past 785).
	core.XdcLastForkRollbackUnix.Store(time.Now().Unix())
	s.stuckRolledBack += window
	s.stuckFrom = 0
	s.stuckFailures = 0
}

// taintPeer marks the given peer as fork-tainted for xdcForkTaintTTL. While
// tainted, rankedPeers() skips them so bestPeer rotates to a different (hopefully
// canonical-side) source. Mirrors upstream geth's downloader.dropPeer pattern
// but as a soft taint rather than a hard disconnect — XDC peers are sparse on
// Apothem and dropping causes future re-discovery cost we'd rather avoid.
func (s *xdcSyncer) taintPeer(peer *eth.Peer) {
	s.forkTaintedMu.Lock()
	defer s.forkTaintedMu.Unlock()
	if s.forkTainted == nil {
		s.forkTainted = make(map[string]time.Time)
	}
	s.forkTainted[peer.ID()] = time.Now()
}

// isPeerTainted returns true if the peer is currently within the taint TTL.
// Also prunes any expired entries it sees during the lookup to keep the map
// from growing unbounded.
func (s *xdcSyncer) isPeerTainted(peerID string) bool {
	s.forkTaintedMu.Lock()
	defer s.forkTaintedMu.Unlock()
	if s.forkTainted == nil {
		return false
	}
	now := time.Now()
	// Inline prune: if this peer's entry has expired, remove it.
	if t, ok := s.forkTainted[peerID]; ok {
		if now.Sub(t) > xdcForkTaintTTL {
			delete(s.forkTainted, peerID)
			return false
		}
		return true
	}
	return false
}

// snapPivotBackoff is the number of blocks to step BACK from peer's
// current head when selecting a snap pivot (#807 Phase 11). Peers'
// XDPOS2 BlockHeaders responses come from a batch cache that lags
// their actual head by some blocks AND shifts as the chain moves.
// Pivoting at H-32 still landed at/above the batch top; the batch
// boundary moves between requests. Pivoting at H-1024 puts the anchor
// deep inside the served range so subsequent skeleton walks backward
// don't run off the batch top.
const snapPivotBackoff uint64 = 1024

// fetchPeerHeadHeader walks connected peers, fetches each peer's FULL
// header at their announced LatestBlockHash, then picks the LOWEST head
// among snap-capable peers — the archive is the slowest to import (more
// work per block due to gcmode=archive). Picking the lowest snap-capable
// head guarantees the pivot lands inside the archive's state retention
// window; otherwise pivot drifts to faster peers and the archive can't
// serve GetAccountRange for that root.
//
// After picking the lowest, step back snapPivotBackoff blocks to land
// safely inside the served batch range.
//
// Refs #807 Phase 8 + Phase 11 + Phase 13.
//
// Returns nil if no peer serves a header for their announced head hash
// or if the back-off header fetch fails.
func (s *xdcSyncer) fetchPeerHeadHeader() *types.Header {
	// Step 1: collect head headers from all snap-capable peers.
	type peerHead struct {
		peer *eth.Peer
		hdr  *types.Header
	}
	var heads []peerHead
	for _, ep := range s.handler.peers.peers {
		p := ep.Peer
		rng := p.BlockRange()
		if rng == nil || rng.LatestBlockHash == (common.Hash{}) {
			continue
		}
		// Phase 13: prefer snap-capable peers — those are the only ones
		// who can serve GetAccountRange. ep.snapExt is the snap-extension
		// registration; nil if peer doesn't support snap.
		if ep.snapExt == nil {
			continue
		}
		headHdr := s.fetchHeaderByHash(p, rng.LatestBlockHash)
		if headHdr == nil {
			continue
		}
		heads = append(heads, peerHead{peer: p, hdr: headHdr})
	}
	if len(heads) == 0 {
		return nil
	}
	// Step 2: pick the LOWEST head among snap-capable peers. Archive
	// trails behind faster snap-mode peers; picking the lowest puts the
	// pivot inside the archive's retention window.
	lowest := heads[0]
	for _, h := range heads[1:] {
		if h.hdr.Number.Uint64() < lowest.hdr.Number.Uint64() {
			lowest = h
		}
	}
	// Step 3: step back to land safely inside the served batch range.
	if lowest.hdr.Number.Uint64() <= snapPivotBackoff {
		return nil
	}
	pivotNum := lowest.hdr.Number.Uint64() - snapPivotBackoff
	pivotHdrs := s.fetchHeaderBatch(lowest.peer, pivotNum, 1)
	if len(pivotHdrs) == 0 || pivotHdrs[0] == nil {
		return nil
	}
	log.Info("xdcSyncer: peer head selection",
		"selected_peer", lowest.peer.ID()[:12],
		"selected_head", lowest.hdr.Number.Uint64(),
		"pivot", pivotHdrs[0].Number.Uint64(),
		"peer_count", len(heads))
	return pivotHdrs[0]
}

// fetchHeaderByHash issues a hash-based GetBlockHeaders request to a
// specific peer. Returns the header on success, nil on timeout/empty.
// Reads from peer.XDCHeaderResp (XDPOS2 bypass-dispatcher pattern,
// same as fetchHeaderBatch — XDPOS2 wire has no RequestId so the
// standard response sink can't route the response).
func (s *xdcSyncer) fetchHeaderByHash(peer *eth.Peer, hash common.Hash) *types.Header {
	select {
	case <-peer.XDCHeaderResp:
	default:
	}
	req, err := peer.RequestHeadersByHash(hash, 1, 0, false, nil)
	if err != nil {
		return nil
	}
	defer req.Close()
	select {
	case headers := <-peer.XDCHeaderResp:
		if len(headers) == 0 {
			return nil
		}
		return headers[0]
	case <-time.After(5 * time.Second):
		return nil
	case <-s.quitCh:
		return nil
	}
}

// prefetchAncestorEpochs walks back from the prime-window's lowest header
// (= operator pivot - fsMinFullBlocks) and seeds 2 × XDPoS Epoch (default
// 1800) ancestor headers into the local DB via Downloader.PrimeAncestorHeaders.
// Required by A.68.2 so the V2 engine's getEpochSwitchInfo recursion can find
// the most recent V2 epoch-switch header — header.Validators of that block
// is the canonical masternode set, the only data
// GetMasternodesFromEpochSwitchHeader needs to resolve verifyHeader's
// signature-verification step for pre-pivot blocks delivered by BeaconSync.
//
// Best-effort design: each fetch is a separate byNumber batch (XDPOS2
// 192-header cap), and on any failure we log + write what we have so far
// then return. The V2 engine has multiple fallback chains; even a partial
// pre-fetch reduces failure probability versus no pre-fetch.
//
// Refs #844 #859 A.68.2.
func (s *xdcSyncer) prefetchAncestorEpochs(peer *eth.Peer, primeBase *types.Header) {
	if peer == nil || primeBase == nil {
		return
	}
	// Determine target depth from the XDPoS config. Default to 900 (XDC
	// mainnet Epoch) if absent; pre-fetch 2 × Epoch headers below primeBase.
	epoch := uint64(900)
	if cfg := s.handler.chain.Config().XDPoS; cfg != nil && cfg.Epoch != 0 {
		epoch = cfg.Epoch
	}
	const targetEpochs = 2
	depth := targetEpochs * epoch
	primeBaseNum := primeBase.Number.Uint64()
	if primeBaseNum < 1 {
		return // nothing below genesis
	}
	hi := primeBaseNum - 1 // highest ancestor to fetch (exclusive of primeBase)
	var lo uint64
	if hi > depth {
		lo = hi - depth + 1
	} else {
		lo = 0
	}
	const batchCap = 192 // XDPOS2 MaxHeaderFetch
	log.Info("xdcSyncer: A.68.2 ancestor pre-fetch starting",
		"from", lo, "to", hi, "headers_needed", hi-lo+1, "primeBase", primeBaseNum)
	// Fetch in ascending order, batchCap headers at a time. Each batch is
	// then written immediately via PrimeAncestorHeaders so a mid-walk
	// failure still preserves the headers we did fetch.
	//
	// A.70 (refs #844 #859): also accumulate the headers into a single slice
	// (low→high contiguous) so the post-loop snapshot-seed walk can scan for
	// V2 epoch-switch headers without doing ~1800 round-trip DB reads.
	expectedParent := primeBase.ParentHash
	totalWritten := 0
	originalHi := primeBaseNum - 1
	// allHeaders accumulates every header we fetched, indexed by (number - lo)
	// so the seed walk can iterate low→high without sorting. Pre-sized to the
	// expected count; downstream code handles partial fills (gaps stay nil).
	allHeaders := make([]*types.Header, hi-lo+1)
	for batchStart := hi - batchCap + 1; ; {
		// Clamp batchStart to lo and adjust count accordingly.
		if batchStart < lo {
			batchStart = lo
		}
		want := int(hi-batchStart) + 1
		if want > batchCap {
			want = batchCap
		}
		headers := s.fetchHeaderBatch(peer, batchStart, want)
		if len(headers) == 0 {
			log.Warn("xdcSyncer: A.68.2 ancestor pre-fetch — empty response, stopping",
				"from", batchStart, "want", want, "total_written", totalWritten)
			s.seedMasternodeSnapshotsFromPrefetch(allHeaders, lo, originalHi, nil, primeBase)
			return
		}
		// XDPOS2 returns ascending order; verify and trim to expected range.
		if headers[0].Number.Uint64() != batchStart {
			log.Warn("xdcSyncer: A.68.2 ancestor pre-fetch — peer returned wrong starting block",
				"want", batchStart, "got", headers[0].Number.Uint64(), "stopping_at", totalWritten)
			s.seedMasternodeSnapshotsFromPrefetch(allHeaders, lo, originalHi, nil, primeBase)
			return
		}
		// Verify the last header's hash matches the expected parent of the
		// next-higher block (primeBase.ParentHash for the first batch).
		last := headers[len(headers)-1]
		if last.Number.Uint64() == hi && last.Hash() != expectedParent {
			log.Warn("xdcSyncer: A.68.2 ancestor pre-fetch — top header doesn't link to prime base",
				"expected_parent_hash", expectedParent.Hex(),
				"got_hash", last.Hash().Hex(), "stopping")
			s.seedMasternodeSnapshotsFromPrefetch(allHeaders, lo, originalHi, nil, primeBase)
			return
		}
		if err := s.handler.downloader.PrimeAncestorHeaders(headers); err != nil {
			log.Warn("xdcSyncer: A.68.2 ancestor pre-fetch — PrimeAncestorHeaders failed",
				"from", batchStart, "count", len(headers), "err", err)
			s.seedMasternodeSnapshotsFromPrefetch(allHeaders, lo, originalHi, nil, primeBase)
			return
		}
		// A.70: stash every header from this batch into the all-headers slice
		// for the post-loop seed walk.
		for _, h := range headers {
			n := h.Number.Uint64()
			if n >= lo && n-lo < uint64(len(allHeaders)) {
				allHeaders[n-lo] = h
			}
		}
		totalWritten += len(headers)
		// Done if we've reached lo.
		if batchStart <= lo {
			break
		}
		// Step the window further back. The new top of the next batch is
		// batchStart-1, expected to parent-link to the headers we just wrote.
		hi = batchStart - 1
		expectedParent = headers[0].ParentHash
		if hi < lo {
			break
		}
		if hi+1 < batchCap {
			batchStart = lo
		} else {
			batchStart = hi - batchCap + 1
		}
	}
	log.Info("xdcSyncer: A.68.2 ancestor pre-fetch complete",
		"target_depth", depth, "actual_written", totalWritten, "lo", lo, "hi", primeBaseNum-1)
	// A.70 (refs #844 #859): fetch up to Epoch+200 headers FORWARD of the
	// operator pivot for the snapshot-seed walk. The actual V2 epoch-switch
	// for the epoch containing the pivot may land past the pivot (V2
	// epochs are round-based; timeouts stretch the round-aligned switch
	// past its block-aligned checkpoint). Forward-fetched headers are kept
	// IN MEMORY only — they are NOT written to the DB, so BeaconSync's
	// canonical walk-forward remains the authoritative writer and any
	// re-org wouldn't conflict.
	forwardHeaders := s.fetchForwardEpochHeaders(peer, primeBase)
	// A.70: seed V2 masternode snapshots from the (backward + forward)
	// epoch-switch headers so post-pivot InsertChain doesn't fail every
	// block with "validators not legit" when XdcBulkSyncMode flips to
	// false after SyncCompleted and fullVerify=true engages.
	s.seedMasternodeSnapshotsFromPrefetch(allHeaders, lo, originalHi, forwardHeaders, primeBase)

	// A.72 (refs #844 #859): with headers in place + V2 masternode snapshots
	// seeded, the next thing the first post-pivot epoch-switch (≈ pivot+122)
	// needs to import is the *body* of every block in its 2-epoch reward
	// window — block 391's HookReward walks back through ~895 ancestor
	// blocks calling rawdb.ReadBlock() (needs body) and
	// rawdb.ReadRawReceipts() (needs receipts) to count signing transactions
	// per masternode. Without bodies + receipts the count is 0, no reward
	// AddBalance fires, and the post-state diverges from canonical
	// ("invalid merkle root" at block 391).
	//
	// Pre-fetch bodies+receipts for the same ancestor range A.68.2 already
	// pre-fetched headers for (lo..originalHi). The header-set is sized to
	// 2 × Epoch = exactly one reward window, so a successful pre-fetch here
	// is sufficient to import every epoch-switch block within the next
	// 1,800 forward blocks. Subsequent epoch-switches get their reward
	// window populated naturally by the forward block-import path.
	s.prefetchAncestorBodiesAndReceipts(peer, allHeaders, lo, originalHi)

	// A.74: pre-fetch bodies+receipts for the anchor-window gap [primeBaseNum..pivotNum-1].
	// PrimeFastSyncAnchor writes empty placeholder bodies for EVERY header in the
	// fast-sync anchor window. A.72's pre-fetch ends at originalHi = primeBaseNum-1,
	// leaving this 64-block gap covered only by placeholders. Blocks in this gap
	// hold the signing txs that HookReward's reward-window walk needs (the 4 missed
	// filter blocks at mainnet pivot ~103,309,269). With the A.74 PrimeAncestorBodies
	// overwrite fix, these real bodies replace the placeholders on first write.
	opPivotNum74, _, _ := s.handler.downloader.OperatorPivot()
	if opPivotNum74 > 0 && opPivotNum74 > primeBaseNum {
		gapLo74 := primeBaseNum
		gapHi74 := opPivotNum74 - 1
		gapCount74 := int(gapHi74 - gapLo74 + 1)
		gapHdrs74 := make([]*types.Header, gapCount74)
		const gapBatch74 = 192
		log.Info("xdcSyncer: A.74 anchor-gap header fetch",
			"gapLo", gapLo74, "gapHi", gapHi74, "count", gapCount74)
		for batchFrom74 := gapLo74; batchFrom74 <= gapHi74; {
			batchWant74 := int(gapHi74-batchFrom74) + 1
			if batchWant74 > gapBatch74 {
				batchWant74 = gapBatch74
			}
			hdrs74 := s.fetchHeaderBatch(peer, batchFrom74, batchWant74)
			if len(hdrs74) == 0 {
				log.Warn("xdcSyncer: A.74 gap header fetch empty — gap bodies may be incomplete",
					"from", batchFrom74, "gapLo", gapLo74, "gapHi", gapHi74)
				break
			}
			for _, h := range hdrs74 {
				n := h.Number.Uint64()
				if n >= gapLo74 && n <= gapHi74 {
					gapHdrs74[n-gapLo74] = h
				}
			}
			batchFrom74 += uint64(len(hdrs74))
		}
		s.prefetchAncestorBodiesAndReceipts(peer, gapHdrs74, gapLo74, gapHi74)
	}
}

// fetchForwardEpochHeaders requests up to one V2 Epoch + buffer worth of
// headers FORWARD of the operator pivot from the same peer that supplied
// the prime window. Headers are returned in-memory (low→high), NOT written
// to the DB. Used by A.70 to find the epoch-switch header that opens the
// epoch CONTAINING the pivot — which may land past the pivot when V2
// timeouts stretch the round-aligned switch past its block-aligned
// checkpoint. Best-effort: peer drop / empty response returns whatever was
// fetched so far (possibly nil).
//
// We deliberately fetch Epoch + Gap + buffer = 900 + 450 + 200 = 1550
// headers max. That guarantees BOTH the round-0 block of the epoch
// containing the pivot AND the next epoch's gap-block window are covered,
// even with timeout-stretched epochs.
//
// Refs #844 #859 A.70.
func (s *xdcSyncer) fetchForwardEpochHeaders(peer *eth.Peer, primeBase *types.Header) []*types.Header {
	if peer == nil || primeBase == nil {
		return nil
	}
	cfg := s.handler.chain.Config()
	if cfg == nil || cfg.XDPoS == nil {
		return nil
	}
	epoch := cfg.XDPoS.Epoch
	gap := cfg.XDPoS.Gap
	if epoch == 0 {
		epoch = 900
	}
	if gap == 0 {
		gap = 450
	}
	// Walk forward from pivot. pivotPeer's tip we're connecting to via
	// BeaconSync may be ~pivot+Epoch on a healthy chain, but we only
	// need up to pivot + epoch + gap + buffer. Cap at peer's advertised
	// latest if known; otherwise just request the window.
	pivot := s.handler.downloader
	opNumber, _, _ := pivot.OperatorPivot()
	if opNumber == 0 {
		// No operator pivot — fast-sync path didn't pin one, nothing to do.
		return nil
	}
	const buffer uint64 = 200
	want := epoch + gap + buffer
	const batchCap = 192 // XDPOS2 MaxHeaderFetch
	from := opNumber + 1
	end := from + want - 1
	collected := make([]*types.Header, 0, want)
	log.Info("xdcSyncer: A.70 forward-fetch starting",
		"from", from, "to", end, "want", want, "peer", peer.ID()[:10])
	for from <= end {
		batchSize := uint64(batchCap)
		if from+batchSize-1 > end {
			batchSize = end - from + 1
		}
		headers := s.fetchHeaderBatch(peer, from, int(batchSize))
		if len(headers) == 0 {
			log.Warn("xdcSyncer: A.70 forward-fetch — empty response, returning partial",
				"from", from, "collected", len(collected))
			return collected
		}
		// Sanity: ascending order starting at `from`.
		if headers[0].Number.Uint64() != from {
			log.Warn("xdcSyncer: A.70 forward-fetch — peer returned wrong starting block",
				"want", from, "got", headers[0].Number.Uint64(), "collected", len(collected))
			return collected
		}
		collected = append(collected, headers...)
		from = headers[len(headers)-1].Number.Uint64() + 1
		// Defensive cap: stop if we've already fetched what we wanted.
		if uint64(len(collected)) >= want {
			break
		}
	}
	log.Info("xdcSyncer: A.70 forward-fetch complete",
		"collected", len(collected), "pivot", opNumber,
		"first", func() uint64 {
			if len(collected) > 0 {
				return collected[0].Number.Uint64()
			}
			return 0
		}(),
		"last", func() uint64 {
			if len(collected) > 0 {
				return collected[len(collected)-1].Number.Uint64()
			}
			return 0
		}())
	return collected
}

// seedMasternodeSnapshotsFromPrefetch walks the pre-fetched ancestor headers
// (low→high, indexed by number-lo) looking for V2 epoch-switch headers. For
// each one found, it computes the block-aligned gap-block number for the
// epoch the switch OPENS, fetches the gap header from the local DB, and
// persists a V2 masternode snapshot keyed at the gap header's hash via the
// V2 engine's UpdateMasternodesWithPenalties path.
//
// Why we need this (A.70):
// After A.68.2 writes 1,800 ancestor headers, the V2 engine's recursive
// getEpochSwitchInfo walk can find the most recent V2 epoch-switch header
// for QC verification (which is what A.68.2 was designed for). But for the
// non-epoch-switch InsertChain path that runs after SyncCompleted flips
// XdcBulkSyncMode to false, verifyHeader (fullVerify=true) calls
// calcMasternodes → getSnapshot → loadSnapshot(x.db, gapHash). That lookup
// always misses on a fresh fast-sync datadir because no `storeSnapshot`
// call ever ran for this session — UpdateMasternodesFromHeader (the normal
// path) requires the gap-block's smart-contract STATE, which a state-pruned
// fast-sync node doesn't yet have at the gap block.
//
// The seed bypasses the contract-state dependency by deriving the active
// masternode set directly from the next epoch-switch header's Validators
// field (canonical 20-byte-packed address list, authored by the issuing
// epoch). Penalties come from header.Penalties of the same epoch-switch
// header. Both fields are part of consensus and signed by the previous
// epoch's masternodes — the operator pivot already trusts the chain
// extension via inCheckpointCatchup, so this is a safe trust transitive.
//
// Additionally seeds a snapshot at the operator pivot's own hash so the
// trusted-checkpoint catchup fallback in GetMasternodesWithParents
// (engine.go:1251) becomes functional. Pre-A.70 that fallback always
// returned empty because no snapshot was ever stored at the anchor hash.
//
// Best-effort: any failure (engine cast miss, missing gap header, empty
// Validators) is logged and skipped. The V2 engine has several other
// fallback chains; even a single successful seed unblocks the catchup
// epoch.
//
// Refs #844 #859 A.70.
func (s *xdcSyncer) seedMasternodeSnapshotsFromPrefetch(allHeaders []*types.Header, lo, hi uint64, forwardHeaders []*types.Header, primeBase *types.Header) {
	if (len(allHeaders) == 0 && len(forwardHeaders) == 0) || primeBase == nil {
		return
	}
	xdposEngine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS)
	if !ok {
		log.Debug("xdcSyncer: A.70 seed skipped — engine is not XDPoS")
		return
	}
	v2Engine, ok := xdposEngine.EngineV2.(*engine_v2.XDPoS_v2)
	if !ok || v2Engine == nil {
		log.Debug("xdcSyncer: A.70 seed skipped — V2 engine not initialised")
		return
	}
	cfg := s.handler.chain.Config()
	if cfg == nil || cfg.XDPoS == nil || cfg.XDPoS.V2 == nil || cfg.XDPoS.V2.SwitchBlock == nil {
		log.Debug("xdcSyncer: A.70 seed skipped — V2 config not present")
		return
	}
	epoch := cfg.XDPoS.Epoch
	gap := cfg.XDPoS.Gap
	if epoch == 0 || gap == 0 || gap >= epoch {
		log.Warn("xdcSyncer: A.70 seed skipped — invalid epoch/gap config",
			"epoch", epoch, "gap", gap)
		return
	}
	v2Switch := cfg.XDPoS.V2.SwitchBlock.Uint64()
	seeded := 0
	skippedNonSwitch := 0
	skippedMissingGap := 0
	skippedEmptyValidators := 0
	// Build a unified low→high header iterator that walks the backward
	// prefetch window FIRST (allHeaders is indexed by number-lo so iterate
	// directly), then the forward in-memory window (ascending order from
	// pivot+1).
	walk := func(yield func(*types.Header)) {
		for _, h := range allHeaders {
			if h != nil {
				yield(h)
			}
		}
		for _, h := range forwardHeaders {
			if h != nil {
				yield(h)
			}
		}
	}
	// findHeader looks up a gap-block header by number in priority order:
	// (1) chain DB, (2) backward prefetch slice (allHeaders), (3) forward
	// in-memory slice (forwardHeaders). Returns nil if not found anywhere.
	findHeader := func(targetNum uint64) *types.Header {
		if h := s.handler.chain.GetHeaderByNumber(targetNum); h != nil {
			return h
		}
		if targetNum >= lo && targetNum <= hi {
			if idx := targetNum - lo; idx < uint64(len(allHeaders)) && allHeaders[idx] != nil {
				return allHeaders[idx]
			}
		}
		for _, fh := range forwardHeaders {
			if fh != nil && fh.Number.Uint64() == targetNum {
				return fh
			}
		}
		return nil
	}
	// Walk low→high across both windows. For each V2-era epoch-switch
	// header, compute the block-aligned gap header for the epoch the
	// switch OPENS and persist a snapshot at the gap header's hash.
	var lastEpochSwitchHeader *types.Header
	walk(func(h *types.Header) {
		num := h.Number.Uint64()
		if num <= v2Switch {
			return // pre-V2; V2 engine snapshots don't apply
		}
		isSwitch, _, err := v2Engine.IsEpochSwitch(h)
		if err != nil || !isSwitch {
			skippedNonSwitch++
			return
		}
		lastEpochSwitchHeader = h
		// Compute the gap-block number for the epoch this switch OPENS.
		// getSnapshot keys on the BLOCK-ALIGNED gap (engine.go:755):
		//   gapNumber = number - number%Epoch - Gap
		// For any block inside the epoch that THIS switch opens, that
		// computation collapses to (checkpoint - Gap) where checkpoint
		// is the block-number-aligned epoch boundary <= h.Number.
		checkpoint := num - num%epoch
		if checkpoint < gap {
			return // before first gap; nothing to seed
		}
		gapNum := checkpoint - gap
		gapHeader := findHeader(gapNum)
		if gapHeader == nil {
			log.Debug("xdcSyncer: A.70 seed — gap header not in DB; skipping",
				"epochSwitch", num, "checkpoint", checkpoint, "gap", gapNum)
			skippedMissingGap++
			return
		}
		masternodes := v2Engine.GetMasternodesFromEpochSwitchHeader(s.handler.chain, h)
		if len(masternodes) == 0 {
			log.Debug("xdcSyncer: A.70 seed — epoch switch header has empty Validators; skipping",
				"epochSwitch", num)
			skippedEmptyValidators++
			return
		}
		penalties := extractAddresses(h.Penalties)
		if err := v2Engine.UpdateMasternodesWithPenalties(s.handler.chain, gapHeader, masternodes, penalties); err != nil {
			log.Warn("xdcSyncer: A.70 seed — UpdateMasternodesWithPenalties failed",
				"epochSwitch", num, "gap", gapNum, "err", err)
			return
		}
		seeded++
		// Cache the seed so it can be re-applied after SyncCompleted, in case
		// fast-sync block-import's UpdateMasternodesFromHeader overwrote it
		// with a contract-state-derived list whose truncation order diverges
		// from canonical header.Validators near the top-N boundary.
		s.a70SeedMu.Lock()
		s.a70SeedCache = append(s.a70SeedCache, a70SeedRecord{
			gapHeader:      gapHeader,
			masternodes:    append([]common.Address(nil), masternodes...),
			penalties:      append([]common.Address(nil), penalties...),
			epochSwitchNum: num,
		})
		s.a70SeedMu.Unlock()
		log.Info("xdcSyncer: A.70 seed — V2 masternode snapshot persisted",
			"epochSwitch", num, "gap", gapNum, "gapHash", gapHeader.Hash().Hex()[:10],
			"masternodes", len(masternodes), "penalties", len(penalties))
	})
	// Additional seed: persist a snapshot at the operator pivot's own hash
	// inheriting masternodes from the most-recent epoch-switch header in the
	// prefetch window. This unblocks the trusted-checkpoint catchup fallback
	// in GetMasternodesWithParents (engine.go:1251) for blocks where the
	// gap-key path doesn't fire (e.g. when getSnapshot's gapHeader lookup
	// itself misses the cache and the DB call lands on a different code
	// path).
	if lastEpochSwitchHeader != nil {
		anchorHash := primeBase.ParentHash // primeBase = pivot - 64 → its parent is pivot - 65
		// Walk forward from primeBase to find the operator pivot header in
		// the prime window (pivot - 64 .. pivot inclusive) so we anchor at
		// the right hash.
		if pivotHeader := s.findOperatorPivotHeader(primeBase); pivotHeader != nil {
			anchorHash = pivotHeader.Hash()
			masternodes := v2Engine.GetMasternodesFromEpochSwitchHeader(s.handler.chain, lastEpochSwitchHeader)
			penalties := extractAddresses(lastEpochSwitchHeader.Penalties)
			if len(masternodes) > 0 {
				if err := v2Engine.SeedSnapshotAtHash(pivotHeader.Number.Uint64(), anchorHash, masternodes, penalties); err != nil {
					log.Warn("xdcSyncer: A.70 anchor-hash seed failed", "anchor", anchorHash.Hex()[:10], "err", err)
				} else {
					s.a70SeedMu.Lock()
					s.a70SeedCache = append(s.a70SeedCache, a70SeedRecord{
						anchorHash:     anchorHash,
						anchorNumber:   pivotHeader.Number.Uint64(),
						masternodes:    append([]common.Address(nil), masternodes...),
						penalties:      append([]common.Address(nil), penalties...),
						epochSwitchNum: lastEpochSwitchHeader.Number.Uint64(),
					})
					s.a70SeedMu.Unlock()
					log.Info("xdcSyncer: A.70 seeded snapshot at operator pivot hash",
						"pivot", pivotHeader.Number.Uint64(), "anchorHash", anchorHash.Hex()[:10],
						"masternodes", len(masternodes), "penalties", len(penalties),
						"sourceEpochSwitch", lastEpochSwitchHeader.Number.Uint64())
				}
			}
		}
	}
	log.Info("xdcSyncer: A.70 masternode snapshot seed complete",
		"seeded", seeded,
		"skipped_non_switch", skippedNonSwitch,
		"skipped_missing_gap", skippedMissingGap,
		"skipped_empty_validators", skippedEmptyValidators,
		"prefetch_lo", lo, "prefetch_hi", hi,
		"forward_headers", len(forwardHeaders))
}

// reapplyA70Seeds re-persists every cached A.70 seed. Used after
// SyncCompleted, because fast-sync block-import calls
// engine.UpdateMasternodesFromHeader at every gap block it imports — that
// path reads contract state and overwrites our seeded snapshot with a
// candidates-by-stake list whose truncation order may diverge from
// canonical header.Validators near the top-N boundary (sort tie-breaking).
// Re-applying ensures the post-pivot InsertChain path sees header.Validators
// as the authoritative active set.
//
// Idempotent: each storeSnapshot call is unconditional last-write-wins put.
// Safe to call multiple times (the second call is a no-op snapshot rewrite
// with identical content).
//
// Refs #844 #859 A.70.
func (s *xdcSyncer) reapplyA70Seeds() {
	s.a70SeedMu.Lock()
	defer s.a70SeedMu.Unlock()
	if len(s.a70SeedCache) == 0 {
		return
	}
	xdposEngine, ok := s.handler.chain.Engine().(*XDPoS.XDPoS)
	if !ok {
		return
	}
	v2Engine, ok := xdposEngine.EngineV2.(*engine_v2.XDPoS_v2)
	if !ok || v2Engine == nil {
		return
	}
	gapSeeds := 0
	anchorSeeds := 0
	for _, rec := range s.a70SeedCache {
		if rec.gapHeader != nil {
			if err := v2Engine.UpdateMasternodesWithPenalties(s.handler.chain, rec.gapHeader, rec.masternodes, rec.penalties); err != nil {
				log.Warn("xdcSyncer: A.70 re-seed failed",
					"gap", rec.gapHeader.Number.Uint64(), "err", err)
				continue
			}
			gapSeeds++
		} else if rec.anchorHash != (common.Hash{}) {
			if err := v2Engine.SeedSnapshotAtHash(rec.anchorNumber, rec.anchorHash, rec.masternodes, rec.penalties); err != nil {
				log.Warn("xdcSyncer: A.70 anchor-hash re-seed failed",
					"anchor", rec.anchorHash.Hex()[:10], "err", err)
				continue
			}
			anchorSeeds++
		}
	}
	log.Info("xdcSyncer: A.70 re-seeded V2 masternode snapshots post-SyncCompleted",
		"gap_reseeds", gapSeeds, "anchor_reseeds", anchorSeeds, "total_cached", len(s.a70SeedCache))
}

// findOperatorPivotHeader locates the operator pivot header via the chain's
// trusted-checkpoint anchor (set from --fastsyncpivot{number,hash}). Returns
// nil if no anchor is active or the header isn't in the local DB yet.
func (s *xdcSyncer) findOperatorPivotHeader(primeBase *types.Header) *types.Header {
	if primeBase == nil {
		return nil
	}
	num, hash, active := s.handler.chain.GetTrustedCheckpointAnchor()
	if !active || num == 0 {
		return nil
	}
	h := s.handler.chain.GetHeaderByHash(hash)
	if h != nil && h.Number.Uint64() == num {
		return h
	}
	return nil
}

// extractAddresses unpacks a 20-byte-packed Address slice from a byte buffer.
// Returns nil for empty / malformed input. Mirrors contracts.ExtractAddressFromBytes
// without dragging the contracts import into eth/.
func extractAddresses(buf []byte) []common.Address {
	if len(buf) == 0 || len(buf)%common.AddressLength != 0 {
		return nil
	}
	out := make([]common.Address, len(buf)/common.AddressLength)
	for i := 0; i < len(out); i++ {
		copy(out[i][:], buf[i*common.AddressLength:])
	}
	return out
}

// fetchHeaderBatch requests `count` headers starting at `from` from peer.
// Reads from peer.XDCHeaderResp directly (bypasses dispatcher — XDPOS2 wire
// has no RequestId so the dispatcher's pending-map can't route responses).
func (s *xdcSyncer) fetchHeaderBatch(peer *eth.Peer, from uint64, count int) []*types.Header {
	// Drain any stale response in the channel before sending.
	select {
	case <-peer.XDCHeaderResp:
	default:
	}
	if count > xdpos2MaxHeaderFetch { // #971: legacy peers reply empty (count=0) to over-cap requests
		count = xdpos2MaxHeaderFetch
	}
	// Sink isn't used for XDPOS2 — pass nil to avoid wasted alloc.
	req, err := peer.RequestHeadersByNumber(from, count, 0, false, nil)
	if err != nil {
		log.Debug("xdcSyncer.fetchHeaderBatch: request failed", "from", from, "err", err)
		return nil
	}
	defer req.Close()

	select {
	case headers := <-peer.XDCHeaderResp:
		return headers
	case <-time.After(xdcReqTimeout):
		log.Debug("xdcSyncer.fetchHeaderBatch: timed out", "from", from)
		return nil
	case <-s.quitCh:
		return nil
	}
}

// peerHeaderResult is the winning result from fetchHeadersParallel.
type peerHeaderResult struct {
	peer    *eth.Peer
	rng     *eth.BlockRangeUpdatePacket
	headers []*types.Header
}

// fetchHeaderBatchTimeout is fetchHeaderBatch with an explicit per-call timeout.
// Used by the A.75 parallel-fan-out path so that bulk-sync rounds use
// xdcBulkReqTimeout (4s) while non-bulk callers keep xdcReqTimeout (8s).
func (s *xdcSyncer) fetchHeaderBatchTimeout(peer *eth.Peer, from uint64, count int, timeout time.Duration) []*types.Header {
	select {
	case <-peer.XDCHeaderResp:
	default:
	}
	if count > xdpos2MaxHeaderFetch { // #971: legacy peers reply empty (count=0) to over-cap requests
		count = xdpos2MaxHeaderFetch
	}
	req, err := peer.RequestHeadersByNumber(from, count, 0, false, nil)
	if err != nil {
		log.Debug("xdcSyncer.fetchHeaderBatchTimeout: request failed", "from", from, "err", err)
		return nil
	}
	defer req.Close()
	select {
	case headers := <-peer.XDCHeaderResp:
		return headers
	case <-time.After(timeout):
		log.Debug("xdcSyncer.fetchHeaderBatchTimeout: timed out", "from", from)
		return nil
	case <-s.quitCh:
		return nil
	}
}

// fetchHeadersParallel fans out header requests to the top xdcMaxPeerTriesPerSync
// candidates concurrently and returns the first non-empty result (A.75).
// atTipPeers counts candidates whose LatestBlockHash == curHash (skipped from
// the race). tried is the number of candidates evaluated (atTip + raced).
//
// Goroutines that lose the race complete within xdcBulkReqTimeout; the buffered
// out channel absorbs their sends so they never block. No goroutine leak.
// fetchHeadersParallel fans out header requests to the top xdcMaxPeerTriesPerSync
// candidates concurrently and returns the first non-empty result (A.75).
// racedIDs is the set of peer IDs that were actually sent requests this round;
// callers use it to avoid re-querying rate-limited peers in A.77 lookahead (A.78).
func (s *xdcSyncer) fetchHeadersParallel(candidates []*eth.Peer, fromBlock uint64, curHash common.Hash) (*peerHeaderResult, int, int, map[string]bool) {
	K := len(candidates)
	if K > xdcMaxPeerTriesPerSync {
		K = xdcMaxPeerTriesPerSync
	}
	raced := make([]*eth.Peer, 0, K)
	rngs := make(map[string]*eth.BlockRangeUpdatePacket, K)
	atTipCount := 0
	for i := 0; i < K; i++ {
		p := candidates[i]
		rng := p.BlockRange()
		if rng == nil {
			continue
		}
		// XDC: LatestBlock is TD, not block number. Use hash equality.
		if rng.LatestBlockHash != (common.Hash{}) && rng.LatestBlockHash == curHash {
			atTipCount++
			continue
		}
		raced = append(raced, p)
		rngs[p.ID()] = rng
	}
	// Build the raced-peer ID set for A.78 lookahead filtering.
	racedIDs := make(map[string]bool, len(raced))
	for _, p := range raced {
		racedIDs[p.ID()] = true
	}
	tried := atTipCount + len(raced)
	if len(raced) == 0 {
		return nil, atTipCount, tried, racedIDs
	}
	out := make(chan *peerHeaderResult, len(raced))
	var wg sync.WaitGroup
	for _, p := range raced {
		wg.Add(1)
		go func(p *eth.Peer) {
			defer wg.Done()
			hdrs := s.fetchHeaderBatchTimeout(p, fromBlock, xdcBatchSize, xdcBulkReqTimeout)
			if len(hdrs) == 0 {
				return
			}
			select {
			case out <- &peerHeaderResult{peer: p, rng: rngs[p.ID()], headers: hdrs}:
			default: // a winner already claimed the slot; drop silently
			}
		}(p)
	}
	// Wait for first result OR all goroutines done (all returned empty).
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	select {
	case first := <-out:
		return first, atTipCount, tried, racedIDs
	case <-allDone:
		return nil, atTipCount, tried, racedIDs
	case <-s.quitCh:
		return nil, atTipCount, tried, racedIDs
	}
}

// fetchBodiesBatch requests bodies for the given hashes from peer.
// Reads from peer.XDCBodyResp directly (bypasses dispatcher).
func (s *xdcSyncer) fetchBodiesBatch(peer *eth.Peer, hashes []common.Hash) []eth.BlockBody {
	// Drain any stale response.
	select {
	case <-peer.XDCBodyResp:
	default:
	}
	req, err := peer.RequestBodies(hashes, nil)
	if err != nil {
		log.Debug("xdcSyncer.fetchBodiesBatch: request failed", "count", len(hashes), "err", err)
		return nil
	}
	defer req.Close()

	select {
	case bodies := <-peer.XDCBodyResp:
		return bodies
	case <-time.After(xdcReqTimeout):
		log.Debug("xdcSyncer.fetchBodiesBatch: timed out", "count", len(hashes))
		return nil
	case <-s.quitCh:
		return nil
	}
}

// fetchReceiptsBatch requests transaction receipts for the given block hashes
// from peer. Used by A.72's pre-pivot ancestor body+receipt pre-fetch. Receipts
// on the XDPOS2 wire path are routed through the dispatcher (unlike bodies which
// have a dedicated XDCBodyResp channel), so we pass a request-scoped sink and
// decode the ReceiptsRLPResponse here. gasUsed / timestamps are required by the
// underlying API but on XDPOS2 the bare-hash request shape doesn't include
// them — pass empty slices to keep RequestReceipts happy.
//
// Refs #844 #859 A.72.
func (s *xdcSyncer) fetchReceiptsBatch(peer *eth.Peer, headers []*types.Header) []types.Receipts {
	if len(headers) == 0 {
		return nil
	}
	hashes := make([]common.Hash, len(headers))
	gasUsed := make([]uint64, len(headers))
	timestamps := make([]uint64, len(headers))
	for i, h := range headers {
		hashes[i] = h.Hash()
		gasUsed[i] = h.GasUsed
		timestamps[i] = h.Time
	}
	sink := make(chan *eth.Response, 1)
	req, err := peer.RequestReceipts(hashes, gasUsed, timestamps, sink)
	if err != nil {
		log.Debug("xdcSyncer.fetchReceiptsBatch: request failed", "count", len(hashes), "err", err)
		return nil
	}
	defer req.Close()

	select {
	case resp := <-sink:
		if resp == nil || resp.Res == nil {
			log.Debug("xdcSyncer.fetchReceiptsBatch: nil response", "count", len(hashes))
			return nil
		}
		raw, ok := resp.Res.(*eth.ReceiptsRLPResponse)
		if !ok || raw == nil {
			log.Debug("xdcSyncer.fetchReceiptsBatch: unexpected response type", "type", "non-ReceiptsRLPResponse")
			return nil
		}
		out := make([]types.Receipts, 0, len(*raw))
		for _, enc := range *raw {
			var rcpts types.Receipts
			if err := rlp.DecodeBytes(enc, &rcpts); err != nil {
				log.Debug("xdcSyncer.fetchReceiptsBatch: receipt list decode failed", "err", err)
				return nil
			}
			out = append(out, rcpts)
		}
		// Signal acceptance (dispatcher requires Done() to release in-flight slot).
		resp.Done <- nil
		return out
	case <-time.After(xdcReqTimeout):
		log.Debug("xdcSyncer.fetchReceiptsBatch: timed out", "count", len(hashes))
		return nil
	case <-s.quitCh:
		return nil
	}
}

// prefetchAncestorBodiesAndReceipts pre-fetches block bodies + receipts for
// the same ancestor header range A.68.2 already primed (lo..hi). Used by
// A.72 to populate the V2 reward-window so the first post-pivot epoch-switch
// block's HookReward can resolve `rawdb.ReadBlock` + `rawdb.ReadRawReceipts`
// for every block in the 2-epoch signing-tx walk. Without bodies + receipts
// the walk computes totalSigners=0 and post-state diverges from canonical.
//
// Best-effort: empty peer responses break out of the loop after writing
// whatever was successfully fetched. We rely on the existing eth/sync_xdc
// retry cadence (sync rounds) to backfill gaps on subsequent peer rotations.
//
// Refs #844 #859 A.72.
func (s *xdcSyncer) prefetchAncestorBodiesAndReceipts(peer *eth.Peer, allHeaders []*types.Header, lo, hi uint64) {
	if peer == nil || len(allHeaders) == 0 {
		return
	}
	// Production XDC peers cap GetBlockBodies responses well below the
	// XDPOS2 MaxBodyFetch advertised limit; observed responses arrive at
	// 128 bodies for a 192-hash request. Use 128 here to avoid the
	// short-response gap-skipping seen in the A.72.v1 deploy. If a peer
	// returns even fewer, the lastBlock-based start advance below handles
	// it correctly.
	const batchCap = 128

	// A.84: build a fallback peer list. When the primary peer drops (empty
	// body response), rotate to the next best peer so the pre-fetch
	// completes even if one peer becomes unavailable mid-batch.
	fallbacks := s.rankedPeers()
	triedPeers := map[string]bool{peer.ID(): true}
	activePeer := peer

	nextFallbackPeer := func() *eth.Peer {
		for _, p := range fallbacks {
			if !triedPeers[p.ID()] {
				triedPeers[p.ID()] = true
				return p
			}
		}
		return nil
	}

	totalBodies := 0
	totalReceipts := 0
	totalFailed := 0
	log.Info("xdcSyncer: A.72 ancestor body+receipt pre-fetch starting",
		"from", lo, "to", hi, "blocks_needed", hi-lo+1, "peer", peer.ID()[:10])
	// Iterate ascending in batchCap-sized chunks. Each chunk pairs headers
	// (from the already-fetched allHeaders slice) with the bodies + receipts
	// pulled from peer.
	for start := lo; start <= hi; {
		end := start + batchCap - 1
		if end > hi {
			end = hi
		}
		// Extract the header slice for this batch from allHeaders. Gaps
		// (header == nil) shouldn't happen in practice after A.68.2's
		// successful pre-fetch, but if they do we skip them silently.
		batchHeaders := make([]*types.Header, 0, end-start+1)
		batchHashes := make([]common.Hash, 0, end-start+1)
		for n := start; n <= end; n++ {
			idx := n - lo
			if idx >= uint64(len(allHeaders)) {
				break
			}
			h := allHeaders[idx]
			if h == nil {
				continue
			}
			batchHeaders = append(batchHeaders, h)
			batchHashes = append(batchHashes, h.Hash())
		}
		if len(batchHashes) == 0 {
			start = end + 1
			continue
		}
		bodies := s.fetchBodiesBatch(activePeer, batchHashes)
		if len(bodies) == 0 {
			// A.84: rotate to the next best peer before giving up.
			if next := nextFallbackPeer(); next != nil {
				log.Warn("xdcSyncer: A.72 body pre-fetch — peer dropped, rotating",
					"batch_start", start, "batch_end", end, "old_peer", activePeer.ID()[:10], "new_peer", next.ID()[:10])
				activePeer = next
				continue
			}
			log.Warn("xdcSyncer: A.72 body pre-fetch — all peers exhausted, stopping",
				"batch_start", start, "batch_end", end, "written_bodies", totalBodies)
			totalFailed += len(batchHashes)
			break
		}
		receipts := s.fetchReceiptsBatch(activePeer, batchHeaders)
		if len(receipts) == 0 {
			log.Warn("xdcSyncer: A.72 receipt pre-fetch — empty response, persisting bodies only",
				"batch_start", start, "batch_end", end, "bodies", len(bodies))
		}
		// Build paired slices for PrimeAncestorBodies. Use min(headers, bodies)
		// to handle short responses gracefully.
		pairCount := len(bodies)
		if pairCount > len(batchHeaders) {
			pairCount = len(batchHeaders)
		}
		typeBodies := make([]*types.Body, pairCount)
		for i := 0; i < pairCount; i++ {
			txs, err := bodies[i].Transactions.Items()
			if err != nil {
				log.Debug("xdcSyncer: A.72 bad txs in body", "idx", i, "err", err)
				continue
			}
			uncles, err := bodies[i].Uncles.Items()
			if err != nil {
				log.Debug("xdcSyncer: A.72 bad uncles in body", "idx", i, "err", err)
				continue
			}
			body := &types.Body{Transactions: txs, Uncles: uncles}
			if bodies[i].Withdrawals != nil {
				if wd, err := bodies[i].Withdrawals.Items(); err == nil {
					body.Withdrawals = wd
				}
			}
			typeBodies[i] = body
		}
		var typedReceipts []types.Receipts
		if len(receipts) >= pairCount {
			typedReceipts = receipts[:pairCount]
		}
		// Trim headers to pairCount so PrimeAncestorBodies validation succeeds.
		hdrSlice := batchHeaders[:pairCount]
		if err := s.handler.downloader.PrimeAncestorBodies(hdrSlice, typeBodies, typedReceipts); err != nil {
			log.Warn("xdcSyncer: A.72 PrimeAncestorBodies failed",
				"batch_start", start, "batch_end", end, "err", err)
			break
		}
		totalBodies += pairCount
		if typedReceipts != nil {
			totalReceipts += pairCount
		}
		// Advance start to one past the LAST block we actually wrote — not
		// to end+1 — so a short response from the peer doesn't silently
		// skip the unwritten tail of the batch. With pairCount=128 in a
		// 128-block batch this equals end+1; with pairCount<128 it advances
		// only by the number of bodies actually persisted, and the next
		// iteration re-fetches the unwritten tail.
		if pairCount > 0 {
			lastWritten := hdrSlice[pairCount-1].Number.Uint64()
			start = lastWritten + 1
		} else {
			// Defensive: if we somehow wrote zero bodies despite a non-empty
			// response, advance one block to avoid an infinite loop.
			start++
		}
	}
	log.Info("xdcSyncer: A.72 ancestor body+receipt pre-fetch complete",
		"bodies_written", totalBodies, "receipts_written", totalReceipts,
		"failed", totalFailed, "lo", lo, "hi", hi)
}

// bestPeer returns the connected eth peer with the highest LatestBlock.
// Retained for callers outside syncOnce; syncOnce uses rankedPeers().
func (s *xdcSyncer) bestPeer() *eth.Peer {
	cands := s.rankedPeers()
	if len(cands) == 0 {
		return nil
	}
	return cands[0]
}

// rankedPeers returns the eth peers sorted by descending LatestBlock
// (which on XDPOS2 carries the peer's total difficulty — see handshake62
// for the field-semantics note). Used by syncOnce to try multiple
// candidate peers within a single sync round when the top peer returns
// 0 headers (common on Apothem where some peers won't serve our requested
// historical range despite having a high TD claim).
func (s *xdcSyncer) rankedPeers() []*eth.Peer {
	all := s.handler.peers.all()
	cands := make([]*eth.Peer, 0, len(all))
	var skippedTainted int
	for _, ep := range all {
		p := ep.Peer
		if p.BlockRange() == nil {
			continue
		}
		// Exclude peers tainted by a recent rollback (#802 task 2). Within
		// xdcForkTaintTTL, this peer delivered the block that put us on a
		// non-canonical fork; without exclusion bestPeer keeps re-picking
		// them and post-rollback sync immediately re-imports the same fork.
		if s.isPeerTainted(p.ID()) {
			skippedTainted++
			continue
		}
		cands = append(cands, p)
	}
	if skippedTainted > 0 && len(cands) == 0 {
		// All available peers are tainted — log but still return empty.
		// Caller (syncOnce) will return; next cycle will retry and any
		// expired entries will be pruned by isPeerTainted's inline cleanup.
		log.Debug("xdcSyncer: all peers fork-tainted, no candidates available",
			"skipped", skippedTainted, "ttl", xdcForkTaintTTL)
	}
	// Insertion sort by descending LatestBlock — n is small (≤ maxPeers).
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j-1].BlockRange().LatestBlock < cands[j].BlockRange().LatestBlock; j-- {
			cands[j-1], cands[j] = cands[j], cands[j-1]
		}
	}
	return cands
}
