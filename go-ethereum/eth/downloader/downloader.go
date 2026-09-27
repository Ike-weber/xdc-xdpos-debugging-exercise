// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package downloader contains the manual full chain synchronisation.
package downloader

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
)

var (
	MaxBlockFetch   = 128 // Number of blocks to be fetched per retrieval request
	MaxHeaderFetch  = 192 // Number of block headers to be fetched per retrieval request
	MaxReceiptFetch = 256 // Number of transaction receipts to allow fetching per request
	// A.67: drop MaxStateFetch from 768 → 384 to match legacy XDPoSChain
	// (XDPoSChain/eth/downloader/downloader.go:54). The XDC mainnet population
	// runs the legacy server which caps responses at MaxStateFetch=384 anyway —
	// asking for 768 only widens the in-flight hash set and inflates the
	// duplicate ratio (since each over-budget hash gets re-dispatched on the
	// retry path and races against a peer that already had it cached). 384
	// matches the supply ceiling 1:1 so we never request more than peers
	// actually deliver. Refs A.67 / #859.
	MaxStateFetch = 384 // Amount of node state values to allow fetching per request (classical Fast Sync). Matches legacy server cap.

	maxQueuedHeaders           = 32 * 1024                        // [eth/62] Maximum number of headers to queue for import (DOS protection)
	maxHeadersProcess          = 2048                             // Number of header download results to import at once into the chain
	maxResultsProcess          = 2048                             // Number of content download results to import at once into the chain
	fullMaxForkAncestry uint64 = params.FullImmutabilityThreshold // Maximum chain reorganisation (locally redeclared so tests can reduce it)

	reorgProtHeaderDelay = 2 // Number of headers to delay delivering to cover mini reorgs

	fsHeaderCheckFrequency = 100             // Verification frequency of the downloaded headers during fast sync
	fsHeaderForceVerify    = 24              // Number of headers to verify before and after the pivot to accept it
	fsHeaderSafetyNet      = 2048            // Number of headers to discard in case a chain violation is detected
	fsHeaderContCheck      = 3 * time.Second // Time interval to check for header continuations during state download
	fsMinFullBlocks        = 64              // Number of blocks to retrieve fully even in snap sync
)

var (
	errBusy    = errors.New("busy")
	errBadPeer = errors.New("action from bad peer ignored")

	errTimeout                 = errors.New("timeout")
	errInvalidChain            = errors.New("retrieved hash chain is invalid")
	errInvalidBody             = errors.New("retrieved block body is invalid")
	errInvalidReceipt          = errors.New("retrieved receipt is invalid")
	errCancelStateFetch        = errors.New("state data download canceled (requested)")
	errCancelContentProcessing = errors.New("content processing canceled (requested)")
	errCanceled                = errors.New("syncing canceled (requested)")
	errNoPivotHeader           = errors.New("pivot header is not found")
)

// SyncMode defines the sync method of the downloader.
// Deprecated: use ethconfig.SyncMode instead
type SyncMode = ethconfig.SyncMode

const (
	// Deprecated: use ethconfig.FullSync
	FullSync = ethconfig.FullSync
	// Deprecated: use ethconfig.SnapSync
	SnapSync = ethconfig.SnapSync
)

// peerDropFn is a callback type for dropping a peer detected as malicious.
type peerDropFn func(id string)

// badBlockFn is a callback for the async beacon sync to notify the caller that
// the origin header requested to sync to, produced a chain with a bad block.
type badBlockFn func(invalid *types.Header, origin *types.Header)

// headerTask is a set of downloaded headers to queue along with their precomputed
// hashes to avoid constant rehashing.
type headerTask struct {
	headers []*types.Header
	hashes  []common.Hash
}

type Downloader struct {
	mode  atomic.Uint32 // Synchronisation mode defining the strategy used (per sync cycle), use d.getMode() to get the SyncMode
	moder *syncModer    // Sync mode management, deliver the appropriate sync mode choice for each cycle

	// Event feed for downloader events
	feed  event.FeedOf[SyncEvent]
	scope event.SubscriptionScope

	queue *queue   // Scheduler for selecting the hashes to download
	peers *peerSet // Set of active peers from which download can proceed

	stateDB ethdb.Database // Database to state sync into (and deduplicate via)

	// Statistics
	syncStatsChainOrigin uint64       // Origin block number where syncing started at
	syncStatsChainHeight uint64       // Highest block number known when syncing started
	syncStatsLock        sync.RWMutex // Lock protecting the sync stats fields

	blockchain BlockChain

	// Callbacks
	dropPeer peerDropFn // Drops a peer for misbehaving
	badBlock badBlockFn // Reports a block as rejected by the chain

	// Status
	synchronising atomic.Bool
	notified      atomic.Bool
	committed     atomic.Bool
	ancientLimit  uint64 // The maximum block number which can be regarded as ancient data.

	// The cutoff block number and hash before which chain segments (bodies
	// and receipts) are skipped during synchronization. 0 means the entire
	// chain segment is aimed for synchronization.
	chainCutoffNumber uint64
	chainCutoffHash   common.Hash

	// Channels
	headerProcCh chan *headerTask // Channel to feed the header processor new tasks

	// Skeleton sync
	skeleton *skeleton // Header skeleton to backfill the chain with (eth2 mode)

	// State sync
	pivotHeader *types.Header // Pivot block header to dynamically push the syncing state root
	pivotLock   sync.RWMutex  // Lock protecting pivot header reads from updates

	// XDC A.97.1 (refs #894 #886): pivot freeze for the dynamic fast-sync path.
	// Prevents the moving-target race where the pivot advances during the
	// multi-hour NodeData download. Frozen when the pivot block lands in the
	// result queue; reset at the start of every new fast-sync cycle.
	xdcPivotFreeze pivotFrozenState

	SnapSyncer     snap.Syncer // XDC: public accessor for eth/sync_xdc.go (TODO: make private)
	snapSyncer     snap.Syncer // snap/1 or snap/2 state syncer, selected at construction
	stateSyncStart chan *stateSync

	// Classical Fast Sync state orchestration (XDPOS2 / NodeData path, Phase A.2 of #844).
	// Kept separate from the snap-sync stateSync path to avoid deadlocks in processSnapSyncContent.
	fastStateSyncStart chan *fastStateSync
	fastStateCh        chan dataPack  // Channel receiving inbound NodeData responses for fast sync
	trackFastStateReq  chan *stateReq // Channel to register an in-flight NodeData request

	// Progress stats for classical Fast Sync state download.
	syncStatsState stateSyncStats

	// XDC gap-pivot enumeration (Phase A.5 of #844).
	// pivotGapNumbers holds the XDPoS gap block numbers between origin and pivot at
	// which additional state downloads must happen so the consensus engine can resolve
	// masternode sets at every post-pivot epoch crossing. Populated when FastSync mode
	// sets the pivot; cleared after the gap loop completes. Refs FAST_SYNC_IMPLEMENTATION.md §3.3.
	pivotGapNumbers []uint64
	pivotGapLock    sync.RWMutex

	// gapSnapshotFn is injected by eth/handler.go (via SetGapSnapshotFn). It opens
	// trie state at the given root and materialises a masternode snapshot via
	// engine_v2.UpdateMasternodesFromHeader. Nil means no-op (non-XDPoS chain or
	// snapshot generation not wired). Refs FAST_SYNC_IMPLEMENTATION.md §3.7.
	gapSnapshotFn GapSnapshotFn

	// gapFallbackFn is injected by eth/handler.go (via SetGapFallbackFn). It
	// materialises a masternode snapshot from the embedded header data when
	// the gap-block state download fails because pruned mainnet peers cannot
	// serve that state epoch. Invoked only on the peers-exhausted error path;
	// the normal gapSnapshotFn path is unchanged. Nil means no fallback wired
	// (non-XDPoS chain). A.97.4, refs #894.
	gapFallbackFn GapFallbackFn

	// fastSyncChainConfig holds the chain configuration for XDPoS gap-pivot
	// enumeration. Injected by eth/handler.go via SetFastSyncChainConfig before
	// the first fast-sync cycle. The BlockChain interface does not expose Config()
	// to avoid import cycles, so this is the workaround. Refs Phase A.5 of #844.
	fastSyncChainConfig *params.ChainConfig

	// Operator-pinned Fast Sync trust anchor (refs #844 §2). When pivotNumber != 0,
	// processFastSyncContent uses these values instead of the dynamic peer-head pivot.
	// All three must be set together via SetFastSyncPivot; never set individually.
	operatorPivotNumber uint64
	operatorPivotHash   common.Hash
	operatorPivotRoot   common.Hash

	// fastSyncAnchorFn is injected by eth/backend.go (A.89) when no operator pivot
	// is pinned. Called once after the dynamic pivot header is finalised so the XDPoS
	// V2 engine's trusted-anchor gate fires and relaxes strict QC verification within
	// 2×Epoch of the pivot. Nil means no-op (non-XDPoS chain or operator pivot set).
	fastSyncAnchorFn func(number uint64, hash common.Hash)

	// cancelPeer is the master peer id whose drop immediately cancels the sync.
	cancelPeer string

	// Cancellation and termination
	cancelCh   chan struct{}  // Channel to cancel mid-flight syncs
	cancelLock sync.RWMutex   // Lock to protect the cancel channel and peer in delivers
	cancelWg   sync.WaitGroup // Make sure all fetcher goroutines have exited.

	quitCh   chan struct{} // Quit channel to signal termination
	quitLock sync.Mutex    // Lock to prevent double closes

	// Testing hooks
	bodyFetchHook    func([]*types.Header) // Method to call upon starting a block body fetch
	receiptFetchHook func([]*types.Header) // Method to call upon starting a receipt fetch
	chainInsertHook  func([]*fetchResult)  // Method to call upon inserting a chain of blocks (possibly in multiple invocations)

	// Progress reporting metrics
	syncStartBlock uint64    // Head snap block when Geth was started
	syncStartTime  time.Time // Time instance when chain sync started
	syncLogTime    time.Time // Time instance when status was last reported
}

// BlockChain encapsulates functions required to sync a (full or snap) blockchain.
type BlockChain interface {
	// HasHeader verifies a header's presence in the local chain.
	HasHeader(common.Hash, uint64) bool

	// HasState checks if state trie is fully present in the database or not.
	HasState(root common.Hash) bool

	// GetHeaderByHash retrieves a header from the local chain.
	GetHeaderByHash(common.Hash) *types.Header

	// CurrentHeader retrieves the head header from the local chain.
	CurrentHeader() *types.Header

	// SetHead rewinds the local chain to a new head.
	SetHead(uint64) error

	// HasBlock verifies a block's presence in the local chain.
	HasBlock(common.Hash, uint64) bool

	// HasFastBlock verifies a snap block's presence in the local chain.
	HasFastBlock(common.Hash, uint64) bool

	// GetCanonicalHash returns the canonical hash for the block at the given
	// number, or the zero hash if no canonical block is present at that height.
	GetCanonicalHash(uint64) common.Hash

	// GetBlockByHash retrieves a block from the local chain.
	GetBlockByHash(common.Hash) *types.Block

	// CurrentBlock retrieves the head block from the local chain.
	CurrentBlock() *types.Header

	// CurrentSnapBlock retrieves the head snap block from the local chain.
	CurrentSnapBlock() *types.Header

	// SnapSyncStart explicitly notifies the chain that snap sync is scheduled and
	// marks chain mutations as disallowed.
	SnapSyncStart() error

	// SnapSyncComplete directly commits the head block to a certain entity.
	SnapSyncComplete(hash common.Hash, isSnapV2 bool) error

	// FastSyncCommitHead directly commits the head block to a certain entity
	// after classical fast sync (NodeData trie download) completes.
	// Unlike SnapSyncComplete, this does not require SnapSyncStart to have
	// been called first — the NodeData path writes trie nodes directly via kv.
	FastSyncCommitHead(common.Hash) error

	// InsertHeadersBeforeCutoff inserts a batch of headers before the configured
	// chain cutoff into the ancient store.
	InsertHeadersBeforeCutoff([]*types.Header) (int, error)

	// InsertHeaderChain inserts a batch of headers into the local chain, possibly
	// creating a reorg. Used by PrimeFastSyncAnchor to seed the operator-pinned
	// pivot + parent before kicking BeaconSync. Refs A.14.
	InsertHeaderChain([]*types.Header) (int, error)

	// InsertChain inserts a batch of blocks into the local chain.
	InsertChain(types.Blocks) (int, error)

	// InterruptInsert disables or enables chain insertion.
	InterruptInsert(on bool)

	// InsertReceiptChain inserts a batch of blocks along with their receipts
	// into the local chain. Blocks older than the specified `ancientLimit`
	// are stored directly in the ancient store, while newer blocks are stored
	// in the live key-value store.
	InsertReceiptChain(types.Blocks, []rlp.RawValue, uint64) (int, error)

	// Snapshots returns the blockchain snapshot tree to paused it during sync.
	Snapshots() *snapshot.Tree

	// TrieDB retrieves the low level trie database used for interacting
	// with trie nodes.
	TrieDB() *triedb.Database

	// HistoryPruningCutoff returns the configured history pruning point.
	// Block bodies along with the receipts will be skipped for synchronization.
	HistoryPruningCutoff() (uint64, common.Hash)
}

// New creates a new downloader to fetch hashes and blocks from remote peers.
func New(stateDb ethdb.Database, mode ethconfig.SyncMode, chain BlockChain, dropPeer peerDropFn, success func(), snapV2 bool) *Downloader {
	cutoffNumber, cutoffHash := chain.HistoryPruningCutoff()
	dl := &Downloader{
		stateDB:            stateDb,
		moder:              newSyncModer(mode, chain, stateDb),
		queue:              newQueue(blockCacheMaxItems, blockCacheInitialItems),
		peers:              newPeerSet(),
		blockchain:         chain,
		chainCutoffNumber:  cutoffNumber,
		chainCutoffHash:    cutoffHash,
		dropPeer:           dropPeer,
		headerProcCh:       make(chan *headerTask, 1),
		quitCh:             make(chan struct{}),
		SnapSyncer:         snap.NewV1Syncer(stateDb, chain.TrieDB().Scheme()),
		stateSyncStart:     make(chan *stateSync),
		fastStateSyncStart: make(chan *fastStateSync),
		// A.67: buffer fastStateCh so the runFastStateSync select loop doesn't
		// race against the loop()'s synchronous s.process() call. Pre-A.67 this
		// was unbuffered with a `default:` drop branch in DeliverNodeData
		// (A.36) — whenever loop() was busy processing a prior pack, ALL new
		// NodeData responses arriving in that window fell into the drop path
		// and were silently lost. Production logs at A.66 showed nodeDataDrop
		// fires every ~50ms with `count=` ranging 100-384 each — that's the
		// dominant rate-limiter, far more than any peer over-delivery.
		//
		// Buffer size 256 = 256 * ~384 = ~98K blobs of headroom, large enough
		// to absorb 1+ second of processing latency at typical fast-sync rates
		// while staying under ~600MB of buffered state data (384 * ~2KB blobs
		// * 256 packs ≈ 200MB heap). The DeliverNodeData drop-on-default branch
		// is retained as the safety net for the original A.36 case (no fetcher
		// listening at all — sync inactive / between phases). Refs A.67 / #859.
		fastStateCh:       make(chan dataPack, 256),
		trackFastStateReq: make(chan *stateReq),
		syncStartBlock:    chain.CurrentSnapBlock().Number.Uint64(),
	}
	// Select the snap/1 or snap/2 state syncer based on the feature flag.
	if snapV2 {
		dl.snapSyncer = snap.NewV2Syncer(stateDb, chain.TrieDB().Scheme())
	} else {
		dl.snapSyncer = snap.NewV1Syncer(stateDb, chain.TrieDB().Scheme())
	}
	// Create the post-merge skeleton syncer and start the process
	dl.skeleton = newSkeleton(stateDb, dl.peers, dropPeer, newBeaconBackfiller(dl, success), chain)

	go dl.stateFetcher()
	go dl.fastStateFetcher()
	return dl
}

// SetGapSnapshotFn injects the XDPoS gap-snapshot callback. Must be called
// before the first FastSync cycle starts. Calling it concurrently with an
// active sync is safe because gapSnapshotFn is only read inside the
// processFastSyncContent goroutine which runs after this call site in practice.
// Refs FAST_SYNC_IMPLEMENTATION.md §3.7, Phase A.5 of #844.
func (d *Downloader) SetGapSnapshotFn(fn GapSnapshotFn) {
	d.pivotGapLock.Lock()
	d.gapSnapshotFn = fn
	d.pivotGapLock.Unlock()
}

// SetGapFallbackFn injects the A.97.4 embedded-header masternode fallback
// callback. Invoked by processFastSyncContent when a gap-block state download
// fails with the peers-exhausted error, so the masternode snapshot for that
// gap block can be seeded directly from header.Validators / header.Penalties
// without trie state. Must be called before the first FastSync cycle.
// Refs #894, A.97.4.
func (d *Downloader) SetGapFallbackFn(fn GapFallbackFn) {
	d.pivotGapLock.Lock()
	d.gapFallbackFn = fn
	d.pivotGapLock.Unlock()
}

// SetFastSyncChainConfig injects the chain configuration used for XDPoS
// gap-pivot enumeration. The BlockChain interface does not expose Config() to
// avoid import cycles; the handler (eth/handler.go) calls this method after
// constructing the Downloader. Refs Phase A.5 of #844.
func (d *Downloader) SetFastSyncChainConfig(cfg *params.ChainConfig) {
	d.pivotGapLock.Lock()
	d.fastSyncChainConfig = cfg
	d.pivotGapLock.Unlock()
}

// SetFastSyncPivot configures the operator-pinned trust anchor for Fast Sync.
// Must be called BEFORE Synchronise() starts. All three must be non-zero.
// When set, processFastSyncContent uses these values instead of the dynamic
// peer-head-derived pivot. Refs #844, FAST_SYNC_IMPLEMENTATION.md §2 + §3.4.
func (d *Downloader) SetFastSyncPivot(number uint64, hash, root common.Hash) {
	d.operatorPivotNumber = number
	d.operatorPivotHash = hash
	d.operatorPivotRoot = root

	// A.42: reconcile any persisted fast-sync checkpoint against the operator
	// flags. The trie-sync queue itself rebuilds implicitly from disk on restart
	// (NewStateSync walks the root and skips already-present nodes), so we only
	// need to decide: keep the partial DB state, or drop the stale checkpoint
	// key. A mismatch means the operator is pointing at a different target —
	// the partial state on disk is still valid (it remains a subset of *some*
	// root) but we must not log "restored" with the wrong target identity.
	if cp := rawdb.ReadFastSyncCheckpoint(d.stateDB); cp != nil {
		if cp.Number == number && cp.Hash == hash && cp.Root == root {
			log.Info("Fast-sync: restored checkpoint", "entries", cp.Processed, "pending", cp.Pending, "pivot", number)
		} else {
			reason := fmt.Sprintf("operator pivot changed (was %d/%s, now %d/%s)",
				cp.Number, cp.Hash.Hex(), number, hash.Hex())
			log.Info("Fast-sync: checkpoint stale, discarding", "reason", reason)
			rawdb.DeleteFastSyncCheckpoint(d.stateDB)
		}
	}

	// Populate XDPoS gap pivots immediately so processFastSyncContent has
	// them ready (don't wait until pivot is dynamically derived in syncToHead).
	// Reuses computePivotGapNumbers from Phase A.5 of #844.
	d.pivotGapLock.Lock()
	if d.fastSyncChainConfig != nil {
		d.pivotGapNumbers = computePivotGapNumbers(d.fastSyncChainConfig, number)
	}
	d.pivotGapLock.Unlock()

	// A.14: tell the skeleton not to chase headers below the pinned anchor.
	// Floor at pivot-1 (= parent height) — sync()'s response branch will
	// terminate once the subchain tail reaches floor+1 = pivot, and linked()
	// will short-circuit true at or above the floor. Refs A.14.
	if d.skeleton != nil && number > 0 {
		d.skeleton.SetTerminateFloor(number - 1)
		// A.71 (refs #844 #859): drop any persisted skeleton subchain whose
		// Head is below the operator-pinned pivot. Without this, a prior boot
		// that engaged BeaconSync against a peer-tip below the pivot (e.g. a
		// misreporting peer's LatestBlockHash on cold-start with < minPeers
		// connected) leaves a stub subchain in the on-disk skeleton sync
		// status. initSync would then preserve that stub alongside the new
		// pivot-anchored subchain (its drop predicate only fires when the
		// stub's Tail >= new headchain's Tail), and len(Subchains) > 1
		// permanently blocks `s.linked()` from short-circuiting true on the
		// post-walkback re-entry path → beaconBackfiller.resume() never runs
		// → synchronise() never reaches SyncCompleted → block-import stays
		// at head=0 even after A.70 successfully seeds masternode snapshots.
		// Empirically validated on xdcscan 2026-06-07 (8+ min of repeated
		// "GetBlockHeaders SEND origin_num=pivot-1 amount=192 reverse=true"
		// with eth_blockNumber=0 and zero "Imported new chain" lines).
		if dropped := d.skeleton.dropStaleSubchainsBelow(number); dropped > 0 {
			log.Info("Fast Sync: pruned stale skeleton subchains below operator pivot",
				"dropped", dropped, "pivot", number)
		}
	}
}

// OperatorPivot returns the operator-pinned Fast Sync trust anchor as set by
// SetFastSyncPivot. Returns (0, zero-hash, zero-hash) if no operator pivot is
// configured. Used by eth/sync_xdc.fastRunningCheck to substitute the pinned
// header for the peer-advertised tip when invoking BeaconSync, so the skeleton
// walk-back terminates at a height the local peer fleet actually has. Refs A.13.
func (d *Downloader) OperatorPivot() (uint64, common.Hash, common.Hash) {
	return d.operatorPivotNumber, d.operatorPivotHash, d.operatorPivotRoot
}

// SetFastSyncAnchorFn wires a callback that is invoked once after the dynamic
// Fast Sync pivot header is finalised (A.89). The callback calls
// blockchain.SetFastSyncTrustedAnchor so the XDPoS V2 engine relaxes strict
// QC verification within 2×Epoch of the pivot — same effect as the operator
// flag path but without requiring --fastsyncpivot{number,hash,root}.
// Must be called before Synchronise() starts. No-op when operator pivot is set.
func (d *Downloader) SetFastSyncAnchorFn(fn func(number uint64, hash common.Hash)) {
	d.pivotGapLock.Lock()
	d.fastSyncAnchorFn = fn
	d.pivotGapLock.Unlock()
}

// FastSyncPivotDistance returns the number of full blocks to keep behind the
// head when computing the dynamic fast-sync pivot (A.89/A.90/A.97.3).
// Always fsMinFullBlocks (64) — the XDPoS 2×Epoch widening introduced in A.89
// was premised on the QC trusted-anchor window requiring pivot depth, but that
// premise is false: the catchup window in engine_v2/utils.go is symmetric
// [anchor−2×Epoch, anchor+2×Epoch] around the anchor itself, so an anchor at
// head−64 already covers BeaconSync's reverse-walk (which terminates at pivot−1
// per the A.90 floor). Keeping the pivot shallow ensures mainnet peers can serve
// its trie state (most prune beyond ~TriesInMemory=128; see it.17/18). Also
// satisfies #894's "dynamic pivot within 1024 of tip" acceptance criterion.
// Exported for sync_xdc.go.
func (d *Downloader) FastSyncPivotDistance() uint64 {
	return d.fastSyncPivotDistance(ethconfig.FastSync)
}

// fastSyncPivotDistance is the unexported form used within the downloader package.
// A.97.3: always fsMinFullBlocks regardless of chain config — the symmetric QC
// catchup window does not require deep pivot placement.
func (d *Downloader) fastSyncPivotDistance(mode ethconfig.SyncMode) uint64 {
	return uint64(fsMinFullBlocks)
}

// PrimeFastSyncAnchor writes a 65-header anchor window (head-fsMinFullBlocks ..
// head) into the chain and stamps the snap-block head at the lowest header (=
// head - fsMinFullBlocks). The downloader internally derives the BeaconSync
// pivot as `head - fsMinFullBlocks` (currently 64), so findBeaconAncestor's
// HasFastBlock(pivot) linkup check expects body + receipt placeholders at
// that lower bound — NOT at head-1. Earlier (A.14) revisions primed only
// {head, head-1}; that left HasFastBlock(head-64) failing with
// "pivot header is not found" the moment skeleton/BeaconSync walked back to
// the real pivot. A.16 widens the window to fsMinFullBlocks+1 headers.
//
// Headers MUST be supplied in ascending order (oldest first), parent-linked
// (headers[i].ParentHash == headers[i-1].Hash()), and contain at least
// fsMinFullBlocks+1 entries. Caller (eth/sync_xdc.fastRunningCheck) walks
// back from the operator-pinned head via fetchHeaderByHash to assemble the
// slice before invoking this.
//
// Idempotent and gated on a truly-empty chaindata (HeadFastBlockHash unset) so
// re-runs against an already-primed datadir are no-ops, keeping rollback
// non-destructive.
// Refs A.14, A.16.
func (d *Downloader) PrimeFastSyncAnchor(headers []*types.Header) error {
	minHeaders := fsMinFullBlocks + 1
	if len(headers) < minHeaders {
		return fmt.Errorf("prime anchor: need >= %d headers, got %d", minHeaders, len(headers))
	}
	for i, h := range headers {
		if h == nil {
			return fmt.Errorf("prime anchor: nil header at index %d", i)
		}
	}
	// Idempotent no-op only if the fast-block head has advanced past genesis
	// AND the headers we'd write are still actually present locally. The earlier
	// gate (A.16) only checked HeadFastBlockHash, which leaves a stale-pointer
	// trap: if a prior run primed the anchor (setting HeadFastBlockHash to the
	// snap pivot at head-fsMinFullBlocks) but the live KV store later lost the
	// adjacent window headers (compaction race, partial wipe, mid-flush abort),
	// re-runs would skip the prime even though findBeaconAncestor's HasFastBlock
	// linkup probe at tip-1 will still fail "beacon linkup unavailable locally".
	// A.29: additionally verify the tip's parent block (= head-1) is fully
	// present (header + body + receipts) — the exact triple BlockChain.
	// HasFastBlock probes. If any leg is missing, fall through and rewrite the
	// full 65-header window. Checking the strictest (HasReceipts) alone isn't
	// sufficient on its own because HasFastBlock short-circuits on HasBlock
	// (= HasHeader && HasBody) first; we mirror that order so the gate fails
	// fast on the same condition the caller would observe.
	if headFastHash := rawdb.ReadHeadFastBlockHash(d.stateDB); headFastHash != (common.Hash{}) {
		if num, ok := rawdb.ReadHeaderNumber(d.stateDB, headFastHash); ok && num > 0 {
			tipHdr := headers[len(headers)-1]
			tipParentNum := tipHdr.Number.Uint64() - 1
			tipParentHash := tipHdr.ParentHash
			fullyPresent := rawdb.HasHeader(d.stateDB, tipParentHash, tipParentNum) &&
				rawdb.HasBody(d.stateDB, tipParentHash, tipParentNum) &&
				rawdb.HasReceipts(d.stateDB, tipParentHash, tipParentNum)
			if fullyPresent {
				return nil
			}
			log.Warn("Fast Sync: re-priming anchor — prior pointer present but tip-1 not fully linked",
				"head_fast_hash", headFastHash.Hex(),
				"head_fast_num", num,
				"missing_block", tipParentNum,
				"missing_hash", tipParentHash.Hex(),
				"has_header", rawdb.HasHeader(d.stateDB, tipParentHash, tipParentNum),
				"has_body", rawdb.HasBody(d.stateDB, tipParentHash, tipParentNum),
				"has_receipts", rawdb.HasReceipts(d.stateDB, tipParentHash, tipParentNum))
		}
	}
	// Verify parent linkage across the supplied window (ascending order).
	for i := 1; i < len(headers); i++ {
		if headers[i].ParentHash != headers[i-1].Hash() {
			return fmt.Errorf("prime anchor: parent linkage mismatch at index %d (headers[%d].ParentHash=%s headers[%d].Hash=%s)",
				i, i, headers[i].ParentHash.Hex(), i-1, headers[i-1].Hash().Hex())
		}
	}
	pivotHdr := headers[0]            // lowest = head - fsMinFullBlocks (actual snap-sync pivot)
	tipHdr := headers[len(headers)-1] // highest = operator-pinned head
	// A.18: InsertHeaderChain validates parent linkage against canonical chain;
	// on a fresh chaindata (head=genesis) it rejects with "unknown ancestor"
	// since block 103M's parent isn't anywhere. Bypass by writing headers
	// directly via rawdb accessors — skip canonical validation but ensure all
	// downstream HasHeader / GetHeaderByNumber / GetHeaderByHash calls work.
	// v1.17.3 is post-merge so TD is no longer stored (block number is the
	// monotonic chain ordering).
	// A.19: findBeaconAncestor calls HasFastBlock(tail_parent, tip-1) which
	// requires HasBody + HasReceipts at block tip-1, not just at the pivot.
	// Write empty body + receipts placeholders at EVERY header in the window
	// so the linkup gate passes regardless of which block findBeaconAncestor
	// probes. Real body + receipts arrive later via fast-sync content fetch.
	for _, h := range headers {
		hash := h.Hash()
		num := h.Number.Uint64()
		rawdb.WriteHeader(d.stateDB, h)
		rawdb.WriteCanonicalHash(d.stateDB, hash, num)
		rawdb.WriteBody(d.stateDB, hash, num, &types.Body{})
		rawdb.WriteReceipts(d.stateDB, hash, num, types.Receipts{})
	}
	// Stamp the snap-block pointer at the lowest header. Some downloader /
	// Erigon paths read this to derive sync origin; pinning it here matches
	// the legacy fast-sync invariant of "snap-block head == last
	// fully-downloaded fast block".
	rawdb.WriteHeadFastBlockHash(d.stateDB, pivotHdr.Hash())
	log.Info("Fast Sync: primed anchor",
		"headers", len(headers),
		"snap_block_pivot", pivotHdr.Number.Uint64(),
		"snap_block_pivot_hash", pivotHdr.Hash().Hex(),
		"tip", tipHdr.Number.Uint64(),
		"tip_hash", tipHdr.Hash().Hex())
	return nil
}

// PrimeAncestorHeaders writes a contiguous ancestor header window below the
// operator-pinned anchor primed by PrimeFastSyncAnchor. Used by A.68.2 to seed
// enough pre-pivot history (≥ 1 V2 epoch) so the XDPoS V2 engine's
// `getEpochSwitchInfo` recursion can walk back from pre-pivot QC ancestors to
// the most recent epoch switch header — which carries header.Validators with
// the active masternode set, the only data `GetMasternodesFromEpochSwitchHeader`
// needs. Without these headers, the V2 engine fails verifyHeader for non-
// epoch-switch blocks just below the pivot with "empty masternode list" even
// when A.68.1's catchup-window gate correctly skips strict verifyQC.
//
// Headers MUST be supplied in ascending order (oldest first), parent-linked,
// and the highest header's parent must equal the lowest-numbered header of
// the PrimeFastSyncAnchor window (i.e. they sit immediately below the prime
// window). No body / receipts / canonical-hash plumbing is needed — these
// headers exist purely so `chain.GetHeaderByHash` can find them during the
// V2 engine's recursive masternode-resolution walk.
//
// Idempotent on rerun: each header write is unconditional (RocksDB last-write-
// wins), so a partial prior prime that crashed mid-window is safely
// re-primed by a subsequent call.
//
// Refs #844 #859 A.68.2.
func (d *Downloader) PrimeAncestorHeaders(headers []*types.Header) error {
	if len(headers) == 0 {
		return nil
	}
	// Sanity: ascending order + parent linkage.
	for i, h := range headers {
		if h == nil {
			return fmt.Errorf("prime ancestors: nil header at index %d", i)
		}
	}
	for i := 1; i < len(headers); i++ {
		if headers[i].Number.Uint64() != headers[i-1].Number.Uint64()+1 {
			return fmt.Errorf("prime ancestors: non-contiguous at index %d (got %d, want %d)",
				i, headers[i].Number.Uint64(), headers[i-1].Number.Uint64()+1)
		}
		if headers[i].ParentHash != headers[i-1].Hash() {
			return fmt.Errorf("prime ancestors: parent linkage mismatch at index %d (headers[%d].ParentHash=%s headers[%d].Hash=%s)",
				i, i, headers[i].ParentHash.Hex(), i-1, headers[i-1].Hash().Hex())
		}
	}
	// Skip writing headers already present (faster reruns, and avoids
	// thrashing the LDB write buffer on resumed datadirs).
	written := 0
	for _, h := range headers {
		hash := h.Hash()
		num := h.Number.Uint64()
		if rawdb.HasHeader(d.stateDB, hash, num) {
			continue
		}
		rawdb.WriteHeader(d.stateDB, h)
		rawdb.WriteCanonicalHash(d.stateDB, hash, num)
		written++
	}
	log.Info("Fast Sync: primed ancestor headers (A.68.2)",
		"supplied", len(headers),
		"written", written,
		"already_present", len(headers)-written,
		"lo", headers[0].Number.Uint64(),
		"hi", headers[len(headers)-1].Number.Uint64())
	return nil
}

// PrimeAncestorBodies writes block bodies and (optional) receipts for the
// supplied ancestor headers. Used by A.72 to seed the pre-pivot reward
// window so that the V2 HookReward's signing-tx walk
// (eth/hooks/engine_v2_hooks.go:GetSigningTxCount) can resolve the
// `rawdb.ReadBlock` + `rawdb.ReadRawReceipts` calls it needs to count
// signers and apply per-epoch reward distribution. Without these, the first
// post-pivot epoch-switch (≈ pivot + 122 for current mainnet) computes
// totalSigners=0, performs zero AddBalance calls, and the resulting
// post-state diverges from canonical at the merkle-root check.
//
// Each input element pairs a header with its body and (optional) receipts.
// Idempotent on rerun via rawdb.HasBody / rawdb.HasReceipts.
//
// Refs #844 #859 A.72.
func (d *Downloader) PrimeAncestorBodies(headers []*types.Header, bodies []*types.Body, receipts []types.Receipts) error {
	if len(headers) == 0 {
		return nil
	}
	if len(bodies) != len(headers) {
		return fmt.Errorf("prime ancestor bodies: header/body length mismatch (%d vs %d)", len(headers), len(bodies))
	}
	// Receipts are optional but if supplied must align 1:1 with headers.
	if receipts != nil && len(receipts) != len(headers) {
		return fmt.Errorf("prime ancestor bodies: header/receipt length mismatch (%d vs %d)", len(headers), len(receipts))
	}
	writtenBodies := 0
	writtenReceipts := 0
	skippedBodies := 0
	skippedReceipts := 0
	batch := d.stateDB.NewBatch()
	for i, h := range headers {
		if h == nil || bodies[i] == nil {
			continue
		}
		hash := h.Hash()
		num := h.Number.Uint64()
		// A.74: overwrite placeholder (0-tx) bodies when the canonical header
		// declares non-empty txs (TxHash != EmptyTxsHash) but the stored body
		// has 0 txs. PrimeFastSyncAnchor writes &types.Body{} for every header
		// in the anchor window; A.72's pre-fetch ends at primeBaseNum-1, leaving
		// the gap [primeBaseNum..pivot-1] covered only by those placeholders.
		shouldWriteBody := !rawdb.HasBody(d.stateDB, hash, num)
		if !shouldWriteBody && h.TxHash != types.EmptyTxsHash && len(bodies[i].Transactions) > 0 {
			if existing := rawdb.ReadBody(d.stateDB, hash, num); existing != nil && len(existing.Transactions) == 0 {
				shouldWriteBody = true
			}
		}
		if shouldWriteBody {
			rawdb.WriteBody(batch, hash, num, bodies[i])
			writtenBodies++
		} else {
			skippedBodies++
		}
		if receipts != nil {
			shouldWriteReceipts := !rawdb.HasReceipts(d.stateDB, hash, num)
			// If we overwrote a placeholder body, overwrite its placeholder receipts too.
			if !shouldWriteReceipts && shouldWriteBody {
				shouldWriteReceipts = true
			}
			if shouldWriteReceipts {
				rawdb.WriteReceipts(batch, hash, num, receipts[i])
				writtenReceipts++
			} else {
				skippedReceipts++
			}
		}
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("prime ancestor bodies: db write failed: %w", err)
	}
	log.Info("Fast Sync: primed ancestor bodies (A.72)",
		"supplied", len(headers),
		"bodies_written", writtenBodies,
		"bodies_skipped", skippedBodies,
		"receipts_written", writtenReceipts,
		"receipts_skipped", skippedReceipts,
		"lo", headers[0].Number.Uint64(),
		"hi", headers[len(headers)-1].Number.Uint64())
	return nil
}

// Progress retrieves the synchronisation boundaries, specifically the origin
// block where synchronisation started at (may have failed/suspended); the block
// or header sync is currently at; and the latest known block which the sync targets.
//
// In addition, during the state download phase of snap synchronisation the number
// of processed and the total number of known states are also returned. Otherwise
// these are zero.
func (d *Downloader) Progress() ethereum.SyncProgress {
	// Lock the current stats and return the progress
	d.syncStatsLock.RLock()
	defer d.syncStatsLock.RUnlock()

	current := uint64(0)
	mode := d.getMode()
	switch mode {
	case ethconfig.FullSync:
		current = d.blockchain.CurrentBlock().Number.Uint64()
	case ethconfig.SnapSync:
		current = d.blockchain.CurrentSnapBlock().Number.Uint64()
	case ethconfig.FastSync:
		// Fast sync (NodeData-based) commits bodies up to pivot; track via snap block.
		current = d.blockchain.CurrentSnapBlock().Number.Uint64()
	default:
		log.Error("Unknown downloader mode", "mode", mode)
	}
	progress := d.snapSyncer.Progress()

	return ethereum.SyncProgress{
		StartingBlock:       d.syncStatsChainOrigin,
		CurrentBlock:        current,
		HighestBlock:        d.syncStatsChainHeight,
		SyncedAccounts:      progress.AccountSynced,
		SyncedAccountBytes:  uint64(progress.AccountBytes),
		SyncedBytecodes:     progress.BytecodeSynced,
		SyncedBytecodeBytes: uint64(progress.BytecodeBytes),
		SyncedStorage:       progress.StorageSynced,
		SyncedStorageBytes:  uint64(progress.StorageBytes),

		// Snap/1 progress fields
		HealedTrienodes:     progress.TrienodeHealSynced,
		HealedTrienodeBytes: uint64(progress.TrienodeHealBytes),
		HealedBytecodes:     progress.BytecodeHealSynced,
		HealedBytecodeBytes: uint64(progress.BytecodeHealBytes),
		HealingTrienodes:    progress.HealingTrienodes,
		HealingBytecode:     progress.HealingBytecode,

		// Snap/2 progress fields
		SyncedAccessLists: progress.AccessListSynced,
		TotalAccessLists:  progress.AccessListTotal,
		TrieGenProgress:   progress.TrieGenPercent,
	}
}

// Synchronising returns whether the downloader is currently running a sync
// round. Used by external callers (e.g. xdcSyncer) to avoid interrupting an
// in-progress skeleton/snap sync with a fresh BeaconDevSync target.
func (d *Downloader) Synchronising() bool {
	return d.synchronising.Load()
}

// RegisterPeer injects a new download peer into the set of block source to be
// used for fetching hashes and blocks from.
func (d *Downloader) RegisterPeer(id string, version uint, peer Peer) error {
	var logger log.Logger
	if len(id) < 16 {
		// Tests use short IDs, don't choke on them
		logger = log.New("peer", id)
	} else {
		logger = log.New("peer", id[:8])
	}
	logger.Trace("Registering sync peer")
	if err := d.peers.Register(newPeerConnection(id, version, peer, logger)); err != nil {
		logger.Error("Failed to register sync peer", "err", err)
		return err
	}
	return nil
}

// UnregisterPeer remove a peer from the known list, preventing any action from
// the specified peer. An effort is also made to return any pending fetches into
// the queue.
func (d *Downloader) UnregisterPeer(id string) error {
	// Unregister the peer from the active peer set and revoke any fetch tasks
	var logger log.Logger
	if len(id) < 16 {
		// Tests use short IDs, don't choke on them
		logger = log.New("peer", id)
	} else {
		logger = log.New("peer", id[:8])
	}
	logger.Trace("Unregistering sync peer")
	if err := d.peers.Unregister(id); err != nil {
		logger.Error("Failed to unregister sync peer", "err", err)
		return err
	}
	d.queue.Revoke(id)

	return nil
}

// synchronise will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if its TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
func (d *Downloader) synchronise(beaconPing chan struct{}) (err error) {
	// The beacon header syncer is async. It will start this synchronization and
	// will continue doing other tasks. However, if synchronization needs to be
	// cancelled, the syncer needs to know if we reached the startup point (and
	// inited the cancel channel) or not yet. Make sure that we'll signal even in
	// case of a failure.
	if beaconPing != nil {
		defer func() {
			select {
			case <-beaconPing: // already notified
			default:
				close(beaconPing) // weird exit condition, notify that it's safe to cancel (the nothing)
			}
		}()
	}
	// Make sure only one goroutine is ever allowed past this point at once
	if !d.synchronising.CompareAndSwap(false, true) {
		return errBusy
	}
	defer d.synchronising.Store(false)

	// Post a user notification of the sync (only once per session)
	if d.notified.CompareAndSwap(false, true) {
		log.Info("Block synchronisation started")
	}

	// Obtain the synchronized used in this cycle
	mode := d.moder.get(true)
	defer func() {
		// The snap-sync mode is usually already disabled right after the pivot
		// commitment; this is the fallback for the cycles terminating without
		// a pivot block (e.g. a short chain fully imported from genesis).
		if err == nil && mode == ethconfig.SnapSync {
			if d.moder.disableSnap() {
				log.Info("Disabled snap-sync after the initial sync cycle")
			}
		}
	}()

	// Disable chain mutations when snap sync is selected, ensuring the
	// downloader is the sole mutator.
	if mode == ethconfig.SnapSync {
		if err := d.blockchain.SnapSyncStart(); err != nil {
			return err
		}
	}
	// A.97.1 (refs #894 #886): reset pivot freeze at the start of every sync
	// cycle so a fresh cycle begins unfrozen. The freeze is set by
	// processFastSyncContent when the pivot block arrives in the result queue;
	// it must be cleared here (not in Cancel) so that a cycle-restart after a
	// cancelled/failed download correctly unfreezes for the next attempt.
	d.ResetPivotFreeze()

	// Reset the queue, peer set and wake channels to clean any internal leftover state
	d.queue.Reset(blockCacheMaxItems, blockCacheInitialItems)
	d.peers.Reset()

	for _, ch := range []chan bool{d.queue.blockWakeCh, d.queue.receiptWakeCh} {
		select {
		case <-ch:
		default:
		}
	}
	for empty := false; !empty; {
		select {
		case <-d.headerProcCh:
		default:
			empty = true
		}
	}
	// Create cancel channel for aborting mid-flight and mark the master peer
	d.cancelLock.Lock()
	d.cancelCh = make(chan struct{})
	d.cancelLock.Unlock()

	defer d.Cancel() // No matter what, we can't leave the cancel channel open

	// Atomically set the requested sync mode
	d.mode.Store(uint32(mode))
	defer d.mode.Store(0)

	if beaconPing != nil {
		close(beaconPing)
	}
	return d.syncToHead()
}

// getMode returns the sync mode used within current cycle.
func (d *Downloader) getMode() SyncMode {
	return SyncMode(d.mode.Load())
}

// ConfigSyncMode returns the sync mode configured for the node.
// The actual running sync mode can differ from this.
func (d *Downloader) ConfigSyncMode() SyncMode {
	return d.moder.get(false)
}

// SubscribeSyncEvents creates a subscription for downloader sync events
func (d *Downloader) SubscribeSyncEvents(ch chan<- SyncEvent) event.Subscription {
	return d.scope.Track(d.feed.Subscribe(ch))
}

// syncToHead starts a block synchronization based on the hash chain from
// the specified head hash.
func (d *Downloader) syncToHead() (err error) {
	mode := d.getMode()
	d.feed.Send(SyncEvent{Type: SyncStarted, Mode: mode})
	defer func() {
		// reset on error
		if err != nil {
			d.feed.Send(SyncEvent{Type: SyncFailed, Mode: mode, Err: err})
		} else {
			latest := d.blockchain.CurrentHeader()
			d.feed.Send(SyncEvent{Type: SyncCompleted, Mode: mode, Latest: latest})
		}
	}()

	log.Debug("Backfilling with the network", "mode", mode)
	defer func(start time.Time) {
		log.Debug("Synchronisation terminated", "elapsed", common.PrettyDuration(time.Since(start)))
	}(time.Now())

	// Look up the sync boundaries: the common ancestor and the target block
	var latest, pivot, final *types.Header
	latest, _, final, err = d.skeleton.Bounds()
	if err != nil {
		return err
	}
	// A.97.3: pivot distance = fsMinFullBlocks (64) for all chains. The
	// symmetric QC catchup window [anchor−2×Epoch, anchor+2×Epoch] in
	// engine_v2/utils.go covers BeaconSync's reverse-walk (terminates at
	// pivot−1) regardless of pivot depth, so widening the pivot to 2×Epoch
	// is both unnecessary and harmful: peers prune state beyond ~128 blocks,
	// making a head−1800 pivot unservable on mainnet (refs it.17/18, #894).
	pivotDistance := d.fastSyncPivotDistance(mode)
	if latest.Number.Uint64() > pivotDistance {
		number := latest.Number.Uint64() - pivotDistance

		// Retrieve the pivot header from the skeleton chain segment but
		// fallback to local chain if it's not found in skeleton space.
		if pivot = d.skeleton.Header(number); pivot == nil {
			_, oldest, _, _ := d.skeleton.Bounds() // error is already checked
			if number < oldest.Number.Uint64() {
				count := int(oldest.Number.Uint64() - number) // it's capped by pivotDistance
				headers := d.readHeaderRange(oldest, count)
				if len(headers) == count {
					pivot = headers[len(headers)-1]
					log.Warn("Retrieved pivot header from local", "number", pivot.Number, "hash", pivot.Hash(), "latest", latest.Number, "oldest", oldest.Number)
				}
			}
		}
		// Print an error log and return directly in case the pivot header
		// is still not found. It means the skeleton chain is not linked
		// correctly with local chain.
		if pivot == nil {
			log.Error("Pivot header is not found", "number", number)
			return errNoPivotHeader
		}
	}
	// If no pivot block was returned, the head is below the min full block
	// threshold (i.e. new chain). In that case we won't really snap sync
	// anyway, but still need a valid pivot block to avoid some code hitting
	// nil panics on access.
	if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && pivot == nil {
		pivot = d.blockchain.CurrentBlock()
	}
	// Operator-pinned Fast Sync pivot override (refs #844 §3.4).
	// When the operator has set a trust anchor via --fastsyncpivotnumber/hash/root,
	// use that pivot number for header-range computation. The actual pivot header
	// is retrieved below once headers are downloaded; here we override the number
	// only if the skeleton provides a header at the pinned block.
	if mode == ethconfig.FastSync && d.operatorPivotNumber != 0 {
		if opPivot := d.skeleton.Header(d.operatorPivotNumber); opPivot != nil {
			pivot = opPivot
			log.Info("Fast Sync: using operator-pinned pivot", "number", d.operatorPivotNumber, "hash", d.operatorPivotHash)
		} else {
			// Skeleton may not have it yet; fall through to dynamic pivot and let
			// processFastSyncContent apply the pin via operatorPivotRoot.
			log.Debug("Fast Sync: operator pivot not yet in skeleton, will pin in processFastSyncContent", "number", d.operatorPivotNumber)
		}
	}
	// A.89: fire dynamic trusted-anchor callback when no operator pivot is set.
	// This wires SetFastSyncTrustedAnchor so XDPOS2 QC verification relaxes for
	// headers within 2×Epoch of the pivot — same as the operator-flag path.
	if mode == ethconfig.FastSync && d.operatorPivotNumber == 0 && pivot != nil {
		d.pivotGapLock.RLock()
		anchorFn := d.fastSyncAnchorFn
		d.pivotGapLock.RUnlock()
		if anchorFn != nil {
			anchorFn(pivot.Number.Uint64(), pivot.Hash())
			log.Info("Fast Sync: installed dynamic pivot trusted anchor",
				"number", pivot.Number.Uint64(), "hash", pivot.Hash(),
				"peerHead", latest.Number.Uint64())
		}
	}
	// If the snap syncer froze its pivot in a previous cycle, resume against
	// the frozen header instead of a fresh one.
	if mode == ethconfig.SnapSync && pivot != nil {
		if frozen := d.snapSyncer.FrozenPivot(); frozen != nil {
			if rawdb.ReadCanonicalHash(d.stateDB, frozen.Number.Uint64()) == frozen.Hash() {
				log.Info("Resuming snap sync against frozen pivot", "number", frozen.Number, "hash", frozen.Hash())
				pivot = frozen
			} else {
				log.Warn("Frozen pivot is no longer canonical", "number", frozen.Number, "hash", frozen.Hash())
			}
		}
	}
	height := latest.Number.Uint64()

	// In beacon mode, use the skeleton chain for the ancestor lookup
	origin, err := d.findBeaconAncestor()
	if err != nil {
		return err
	}
	d.syncStatsLock.Lock()
	if d.syncStatsChainHeight <= origin || d.syncStatsChainOrigin > origin {
		d.syncStatsChainOrigin = origin
	}
	d.syncStatsChainHeight = height
	d.syncStatsLock.Unlock()

	// Ensure our origin point is below any snap/fast sync pivot point
	if mode == ethconfig.SnapSync || mode == ethconfig.FastSync {
		if height <= uint64(fsMinFullBlocks) {
			origin = 0
		} else {
			pivotNumber := pivot.Number.Uint64()
			if pivotNumber <= origin {
				origin = pivotNumber - 1
			}
			// Write out the pivot into the database so a rollback beyond it
			// can be detected
			rawdb.WriteLastPivotNumber(d.stateDB, pivotNumber)
		}
	}
	d.committed.Store(true)
	if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && pivot.Number.Uint64() != 0 {
		d.committed.Store(false)
	}
	if mode == ethconfig.SnapSync || mode == ethconfig.FastSync {
		// Set the ancient data limitation. If we are running snap sync, all block
		// data older than ancientLimit will be written to the ancient store. More
		// recent data will be written to the active database and will wait for the
		// freezer to migrate.
		//
		// If the network is post-merge, use either the last announced finalized
		// block as the ancient limit, or if we haven't yet received one, the head-
		// a max fork ancestry limit. One quirky case if we've already passed the
		// finalized block, in which case the skeleton.Bounds will return nil and
		// we'll revert to head - 90K. That's fine, we're finishing sync anyway.
		//
		// For non-merged networks, if there is a checkpoint available, then calculate
		// the ancientLimit through that. Otherwise calculate the ancient limit through
		// the advertised height of the remote peer. This is mostly a fallback for
		// legacy networks, but should eventually be dropped. TODO(karalabe).
		//
		// Beacon sync, use the latest finalized block as the ancient limit
		// or a reasonable height if no finalized block is yet announced.
		if final != nil {
			d.ancientLimit = final.Number.Uint64()
		} else if height > fullMaxForkAncestry+1 {
			d.ancientLimit = height - fullMaxForkAncestry - 1
		} else {
			d.ancientLimit = 0
		}
		// Extend the ancient chain segment range if the ancient limit is even
		// below the pre-configured chain cutoff.
		if d.chainCutoffNumber != 0 && d.chainCutoffNumber > d.ancientLimit {
			d.ancientLimit = d.chainCutoffNumber
			log.Info("Extend the ancient range with configured cutoff", "cutoff", d.chainCutoffNumber)
		}
		frozen, _ := d.stateDB.Ancients() // Ignore the error here since light client can also hit here.

		// If a part of blockchain data has already been written into active store,
		// disable the ancient style insertion explicitly.
		if origin >= frozen && origin != 0 {
			d.ancientLimit = 0
			var ancient string
			if frozen == 0 {
				ancient = "null"
			} else {
				ancient = fmt.Sprintf("%d", frozen-1)
			}
			log.Info("Disabling direct-ancient mode", "origin", origin, "ancient", ancient)
		} else if d.ancientLimit > 0 {
			log.Debug("Enabling direct-ancient mode", "ancient", d.ancientLimit)
		}
		// Rewind the ancient store and blockchain if reorg happens.
		if origin+1 < frozen {
			if err := d.blockchain.SetHead(origin); err != nil {
				return err
			}
			log.Info("Truncated excess ancient chain segment", "oldhead", frozen-1, "newhead", origin)
		}
	}
	// Skip ancient chain segments if Geth is running with a configured chain cutoff.
	// These segments are not guaranteed to be available in the network.
	chainOffset := origin + 1
	if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && d.chainCutoffNumber != 0 {
		if chainOffset < d.chainCutoffNumber {
			chainOffset = d.chainCutoffNumber
			log.Info("Skip chain segment before cutoff", "origin", origin, "cutoff", d.chainCutoffNumber)
		}
	}
	// Initiate the sync using a concurrent header and content retrieval algorithm
	d.queue.Prepare(chainOffset, mode)

	// In beacon mode, headers are served by the skeleton syncer
	fetchers := []func() error{
		func() error { return d.fetchHeaders(origin + 1) },   // Headers are always retrieved
		func() error { return d.fetchBodies(chainOffset) },   // Bodies are retrieved during normal and snap sync
		func() error { return d.fetchReceipts(chainOffset) }, // Receipts are retrieved during snap sync
		func() error { return d.processHeaders(origin + 1) },
	}
	if mode == ethconfig.SnapSync {
		d.pivotLock.Lock()
		d.pivotHeader = pivot
		d.pivotLock.Unlock()

		fetchers = append(fetchers, func() error { return d.processSnapSyncContent() })
	} else if mode == ethconfig.FastSync {
		d.pivotLock.Lock()
		d.pivotHeader = pivot
		d.pivotLock.Unlock()

		// A.97.1b (refs #894 #886): freeze the pivot BEFORE spawnSync launches the
		// beacon/skeleton goroutines, not after state-download starts. The dominant
		// pivot mover (fetchHeaders / beaconsync.go:336-364) fires every
		// fsHeaderContCheck (3s) the moment spawnSync starts. Setting the freeze here
		// — while still single-threaded, pivot just assigned — guarantees it is armed
		// before the first possible fire. The operator-pinned path (operatorPivotNumber
		// != 0) is excluded: its freeze is implicit (staleness branch already gated).
		// Also excluded when already committed (we're past the pivot, no state DL).
		if d.operatorPivotNumber == 0 && !d.committed.Load() && pivot != nil {
			d.FreezePivot()
		}

		// A.32: only mark committed when we are STRICTLY past the pivot (the chain
		// head is at or beyond it). The earlier "header in chain" short-circuit
		// (A.13) was wrong: PrimeFastSyncAnchor writes the pivot header into the
		// DB at startup, but the pivot's body + receipts have not been fetched,
		// bc.currentBlock has not been advanced, and HeadBlockHash still points
		// at genesis. Marking committed=true here caused fetchHeaders to send nil
		// before scheduling the pivot for body+receipt fetch, processFastSyncContent
		// to skip the staleness branch, and commitFastSyncPivotBlock (the only
		// function that advances bc.currentBlock via FastSyncCommitHead) to never
		// fire — leaving head=0 indefinitely after state-sync drained.
		//
		// Leaving committed=false lets the normal downstream flow run: processHeaders
		// schedules the pivot for body+receipt, fetchers grab them, queue.Results()
		// delivers the pivot result, splitAroundPivot identifies it as the P slot,
		// commitFastSyncPivotBlock runs InsertReceiptChain + FastSyncCommitHead,
		// chain head advances. Refs A.13, A.30.
		if d.operatorPivotNumber != 0 && d.operatorPivotNumber <= origin {
			d.committed.Store(true)
			log.Info("Fast Sync: already past operator-pinned pivot, skipping state sync", "pivot", d.operatorPivotNumber, "origin", origin)
		}

		// XDC gap-pivot enumeration (Phase A.5 of #844).
		// Compute XDPoS gap block numbers between origin and pivot. These are
		// blocks at which state must also be downloaded so the consensus engine
		// can resolve masternode sets after the pivot. Stored now so the
		// processFastSyncContent goroutine can iterate them without re-computing.
		// chain.Config() is not part of the BlockChain interface, so we derive
		// the chain config from the genesis hash via rawdb — but the cleanest
		// approach here is to let the handler inject the chain config via the
		// same path it injects gapSnapshotFn. We store the result using the
		// handler-injected chain config if available (populated in handler.go).
		// For now, populate using the XDPoS params embedded in pivotHeader
		// (resolved via the gap formula below). The actual config is passed
		// via SetFastSyncChainConfig if the handler sets it; fall back to nil
		// which is a no-op in computePivotGapNumbers.
		if pivot != nil {
			// When operator pinned a pivot and SetFastSyncPivot already populated
			// gap numbers, don't overwrite them — they were computed from the
			// operator-authoritative number. Only recompute for dynamic pivot.
			d.pivotGapLock.Lock()
			if d.operatorPivotNumber == 0 || len(d.pivotGapNumbers) == 0 {
				d.pivotGapNumbers = computePivotGapNumbers(d.fastSyncChainConfig, pivot.Number.Uint64())
			}
			d.pivotGapLock.Unlock()
			d.pivotGapLock.RLock()
			gaps := d.pivotGapNumbers
			d.pivotGapLock.RUnlock()
			if len(gaps) > 0 {
				log.Info("Fast Sync XDC gap pivots computed",
					"pivot", pivot.Number.Uint64(),
					"gaps", len(gaps),
					"first", gaps[0],
					"last", gaps[len(gaps)-1])
			}
		}

		fetchers = append(fetchers, func() error { return d.processFastSyncContent() })
	} else if mode == ethconfig.FullSync {
		fetchers = append(fetchers, func() error { return d.processFullSyncContent() })
	}
	return d.spawnSync(fetchers)
}

// spawnSync runs d.process and all given fetcher functions to completion in
// separate goroutines, returning the first error that appears.
func (d *Downloader) spawnSync(fetchers []func() error) error {
	errc := make(chan error, len(fetchers))
	d.cancelWg.Add(len(fetchers))
	for _, fn := range fetchers {
		go func() { defer d.cancelWg.Done(); errc <- fn() }()
	}
	// Wait for the first error, then terminate the others.
	var err error
	for i := 0; i < len(fetchers); i++ {
		if i == len(fetchers)-1 {
			// Close the queue when all fetchers have exited.
			// This will cause the block processor to end when
			// it has processed the queue.
			d.queue.Close()
		}
		if got := <-errc; got != nil {
			err = got
			if got != errCanceled {
				break // receive a meaningful error, bubble it up
			}
		}
	}
	d.queue.Close()
	d.Cancel()
	return err
}

// cancel aborts all of the operations and resets the queue. However, cancel does
// not wait for the running download goroutines to finish. This method should be
// used when cancelling the downloads from inside the downloader.
func (d *Downloader) cancel() {
	// Close the current cancel channel
	d.cancelLock.Lock()
	defer d.cancelLock.Unlock()

	if d.cancelCh != nil {
		select {
		case <-d.cancelCh:
			// Channel was already closed
		default:
			close(d.cancelCh)
		}
	}
}

// Cancel aborts all of the operations and waits for all download goroutines to
// finish before returning.
func (d *Downloader) Cancel() {
	d.blockchain.InterruptInsert(true)
	d.cancel()
	d.cancelWg.Wait()
	d.blockchain.InterruptInsert(false)
}

// Terminate interrupts the downloader, canceling all pending operations.
// The downloader cannot be reused after calling Terminate.
func (d *Downloader) Terminate() {
	// Unsubscribe all subscriptions registered from downloader
	d.scope.Close()

	// Close the termination channel (make sure double close is allowed)
	d.quitLock.Lock()
	select {
	case <-d.quitCh:
	default:
		close(d.quitCh)

		// Terminate the internal beacon syncer
		d.skeleton.Terminate()
	}
	d.quitLock.Unlock()

	// Cancel any pending download requests
	d.Cancel()
}

// fetchBodies iteratively downloads the scheduled block bodies, taking any
// available peers, reserving a chunk of blocks for each, waiting for delivery
// and also periodically checking for timeouts.
func (d *Downloader) fetchBodies(from uint64) error {
	log.Debug("Downloading block bodies", "origin", from)
	err := d.concurrentFetch((*bodyQueue)(d))

	log.Debug("Block body download terminated", "err", err)
	return err
}

// fetchReceipts iteratively downloads the scheduled block receipts, taking any
// available peers, reserving a chunk of receipts for each, waiting for delivery
// and also periodically checking for timeouts.
func (d *Downloader) fetchReceipts(from uint64) error {
	log.Debug("Downloading receipts", "origin", from)
	err := d.concurrentFetch((*receiptQueue)(d))

	log.Debug("Receipt download terminated", "err", err)
	return err
}

// processHeaders takes batches of retrieved headers from an input channel and
// keeps processing and scheduling them into the header chain and downloader's
// queue until the stream ends or a failure occurs.
func (d *Downloader) processHeaders(origin uint64) error {
	var (
		mode  = d.getMode()
		timer = time.NewTimer(time.Second)
	)
	defer timer.Stop()

	for {
		select {
		case <-d.cancelCh:
			return errCanceled

		case task := <-d.headerProcCh:
			// Terminate header processing if we synced up
			if task == nil || len(task.headers) == 0 {
				// Notify everyone that headers are fully processed
				for _, ch := range []chan bool{d.queue.blockWakeCh, d.queue.receiptWakeCh} {
					select {
					case ch <- false:
					case <-d.cancelCh:
					}
				}
				return nil
			}
			// Otherwise split the chunk of headers into batches and process them
			headers, hashes, scheduled := task.headers, task.hashes, false

			for len(headers) > 0 {
				// Terminate if something failed in between processing chunks
				select {
				case <-d.cancelCh:
					return errCanceled
				default:
				}
				// Select the next chunk of headers to import
				limit := maxHeadersProcess
				if limit > len(headers) {
					limit = len(headers)
				}
				chunkHeaders := headers[:limit]
				chunkHashes := hashes[:limit]

				// Split the headers around the chain cutoff
				var cutoff int
				if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && d.chainCutoffNumber != 0 {
					cutoff = sort.Search(len(chunkHeaders), func(i int) bool {
						return chunkHeaders[i].Number.Uint64() >= d.chainCutoffNumber
					})
				}
				// Insert the header chain into the ancient store (with block bodies and
				// receipts set to nil) if they fall before the cutoff.
				if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && cutoff != 0 {
					if n, err := d.blockchain.InsertHeadersBeforeCutoff(chunkHeaders[:cutoff]); err != nil {
						log.Warn("Failed to insert ancient header chain", "number", chunkHeaders[n].Number, "hash", chunkHashes[n], "parent", chunkHeaders[n].ParentHash, "err", err)
						return fmt.Errorf("%w: %v", errInvalidChain, err)
					}
					log.Debug("Inserted headers before cutoff", "number", chunkHeaders[cutoff-1].Number, "hash", chunkHashes[cutoff-1])
				}
				// If we've reached the allowed number of pending headers, stall a bit
				for d.queue.PendingBodies() >= maxQueuedHeaders || d.queue.PendingReceipts() >= maxQueuedHeaders {
					timer.Reset(time.Second)
					select {
					case <-d.cancelCh:
						return errCanceled
					case <-timer.C:
					}
				}
				// Otherwise, schedule the headers for content retrieval (block bodies and
				// potentially receipts in snap/fast sync).
				//
				// Skip the bodies/receipts retrieval scheduling before the cutoff in snap/fast
				// sync if chain pruning is configured.
				if (mode == ethconfig.SnapSync || mode == ethconfig.FastSync) && cutoff != 0 {
					chunkHeaders = chunkHeaders[cutoff:]
					chunkHashes = chunkHashes[cutoff:]
				}
				if len(chunkHeaders) > 0 {
					scheduled = true
					if d.queue.Schedule(chunkHeaders, chunkHashes, origin+uint64(cutoff)) != len(chunkHeaders) {
						return fmt.Errorf("%w: stale headers", errBadPeer)
					}
				}
				headers = headers[limit:]
				hashes = hashes[limit:]
				origin += uint64(limit)
			}
			// Update the highest block number we know if a higher one is found.
			d.syncStatsLock.Lock()
			if d.syncStatsChainHeight < origin {
				d.syncStatsChainHeight = origin - 1
			}
			d.syncStatsLock.Unlock()

			// Signal the downloader of the availability of new tasks
			if scheduled {
				for _, ch := range []chan bool{d.queue.blockWakeCh, d.queue.receiptWakeCh} {
					select {
					case ch <- true:
					default:
					}
				}
			}
		}
	}
}

// processFullSyncContent takes fetch results from the queue and imports them into the chain.
func (d *Downloader) processFullSyncContent() error {
	for {
		results := d.queue.Results(true)
		if len(results) == 0 {
			return nil
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		if err := d.importBlockResults(results); err != nil {
			return err
		}
	}
}

func (d *Downloader) importBlockResults(results []*fetchResult) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	default:
	}
	// Retrieve a batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log.Debug("Inserting downloaded chain", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnum", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.body())
	}
	// Downloaded blocks are always regarded as trusted after the
	// transition. Because the downloaded chain is guided by the
	// consensus-layer.
	if index, err := d.blockchain.InsertChain(blocks); err != nil {
		if index < len(results) {
			log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)

			// In post-merge, notify the engine API of encountered bad chains
			if d.badBlock != nil {
				head, _, _, err := d.skeleton.Bounds()
				if err != nil {
					log.Error("Failed to retrieve beacon bounds for bad block reporting", "err", err)
				} else {
					d.badBlock(blocks[index].Header(), head)
				}
			}
		} else {
			// The InsertChain method in blockchain.go will sometimes return an out-of-bounds index,
			// when it needs to preprocess blocks to import a sidechain.
			// The importer will put together a new list of blocks to import, which is a superset
			// of the blocks delivered from the downloader, and the indexing will be off.
			log.Debug("Downloaded item processing failed on sidechain import", "index", index, "err", err)
		}
		return fmt.Errorf("%w: %v", errInvalidChain, err)
	}
	return nil
}

// processSnapSyncContent takes fetch results from the queue and writes them to the
// database. It also controls the synchronisation of state nodes of the pivot block.
func (d *Downloader) processSnapSyncContent() error {
	// Start syncing state of the reported head block. This should get us most of
	// the state of the pivot block.
	d.pivotLock.RLock()
	sync := d.syncState(d.pivotHeader)
	d.pivotLock.RUnlock()

	defer func() {
		// The `sync` object is replaced every time the pivot moves. We need to
		// defer close the very last active one, hence the lazy evaluation vs.
		// calling defer sync.Cancel() !!!
		sync.Cancel()
	}()

	closeOnErr := func(s *stateSync) {
		if err := s.Wait(); err != nil && err != errCancelStateFetch && err != errCanceled && err != snap.ErrCancelled {
			d.queue.Close() // wake up Results
		}
	}
	go closeOnErr(sync)

	// To cater for moving pivot points, track the pivot block and subsequently
	// accumulated download results separately.
	//
	// These will be nil up to the point where we reach the pivot, and will only
	// be set temporarily if the synced blocks are piling up, but the pivot is
	// still busy downloading. In that case, we need to occasionally check for
	// pivot moves, so need to unblock the loop. These fields will accumulate
	// the results in the meantime.
	//
	// Note, there's no issue with memory piling up since after 64 blocks the
	// pivot will forcefully move so these accumulators will be dropped. The
	// exception is snap/2 trie generation, where the pivot is frozen on
	// purpose and results accumulate until the generation finishes.
	var (
		oldPivot *fetchResult   // Locked in pivot block, might change eventually
		oldTail  []*fetchResult // Downloaded content after the pivot
		timer    = time.NewTimer(time.Second)
	)
	defer timer.Stop()

	for {
		// Wait for the next batch of downloaded data to be available. If we have
		// not yet reached the pivot point, wait blockingly as there's no need to
		// spin-loop check for pivot moves. If we reached the pivot but have not
		// yet processed it, check for results async, so we might notice pivot
		// moves while state syncing. If the pivot was passed fully, block again
		// as there's no more reason to check for pivot moves at all.
		results := d.queue.Results(oldPivot == nil)
		if len(results) == 0 {
			// If pivot sync is done, stop
			if d.committed.Load() {
				d.reportSnapSyncProgress(true)
				return sync.Cancel()
			}
			// If sync failed, stop
			select {
			case <-d.cancelCh:
				sync.Cancel()
				return errCanceled
			default:
			}
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		d.reportSnapSyncProgress(false)

		// If we haven't downloaded the pivot block yet, check pivot staleness
		// notifications from the header downloader
		d.pivotLock.RLock()
		pivot := d.pivotHeader
		d.pivotLock.RUnlock()

		if oldPivot == nil { // no results piling up, we can move the pivot
			if !d.committed.Load() { // not yet passed the pivot, we can move the pivot
				if pivot.Root != sync.pivot.Root { // pivot state root changed, we can move the pivot
					sync.Cancel()
					sync = d.syncState(pivot)
					go closeOnErr(sync)
				}
			}
		} else { // results already piled up, consume before handling pivot move
			results = append(append([]*fetchResult{oldPivot}, oldTail...), results...)
		}
		P, beforeP, afterP := splitAroundPivot(pivot.Number.Uint64(), results)
		if err := d.commitSnapSyncData(beforeP, sync); err != nil {
			return err
		}
		if P != nil {
			// If new pivot block found, cancel old state retrieval and restart.
			if oldPivot != P {
				// Skip the restart if the running sync already targets the
				// pivot's root (e.g, no pivot block movement yet).
				if sync.pivot.Root != P.Header.Root {
					sync.Cancel()
					sync = d.syncState(P.Header)
					go closeOnErr(sync)
				}
				oldPivot = P
			}
			// Wait for completion, occasionally checking for pivot staleness
			timer.Reset(time.Second)
			select {
			case <-sync.done:
				if sync.err != nil {
					return sync.err
				}
				if err := d.commitPivotBlock(P); err != nil {
					return err
				}
				oldPivot = nil

			case <-timer.C:
				oldTail = afterP
				continue
			}
		}
		// Fast sync done, pivot commit done, full import
		if err := d.importBlockResults(afterP); err != nil {
			return err
		}
	}
}

func splitAroundPivot(pivot uint64, results []*fetchResult) (p *fetchResult, before, after []*fetchResult) {
	if len(results) == 0 {
		return nil, nil, nil
	}
	if lastNum := results[len(results)-1].Header.Number.Uint64(); lastNum < pivot {
		// the pivot is somewhere in the future
		return nil, results, nil
	}
	// This can also be optimized, but only happens very seldom
	for _, result := range results {
		num := result.Header.Number.Uint64()
		switch {
		case num < pivot:
			before = append(before, result)
		case num == pivot:
			p = result
		default:
			after = append(after, result)
		}
	}
	return p, before, after
}

func (d *Downloader) commitSnapSyncData(results []*fetchResult, stateSync *stateSync) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	case <-stateSync.done:
		if err := stateSync.Wait(); err != nil {
			return err
		}
	default:
	}
	// Retrieve the batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log.Debug("Inserting snap-sync blocks", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnum", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	receipts := make([]rlp.RawValue, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.body())
		receipts[i] = result.Receipts
	}
	if index, err := d.blockchain.InsertReceiptChain(blocks, receipts, d.ancientLimit); err != nil {
		log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return fmt.Errorf("%w: %v", errInvalidChain, err)
	}
	return nil
}

func (d *Downloader) commitPivotBlock(result *fetchResult) error {
	block := types.NewBlockWithHeader(result.Header).WithBody(result.body())
	log.Debug("Committing snap sync pivot as new head", "number", block.Number(), "hash", block.Hash())

	// Commit the pivot block as the new head, will require full sync from here on
	if _, err := d.blockchain.InsertReceiptChain([]*types.Block{block}, []rlp.RawValue{result.Receipts}, d.ancientLimit); err != nil {
		return err
	}
	if err := d.blockchain.SnapSyncComplete(block.Hash(), d.snapSyncer.Version() == snap.SNAP2); err != nil {
		return err
	}
	d.pivotLock.Lock()
	d.committed.Store(true)
	d.pivotLock.Unlock()

	// The chain has obtained a stateful head by committing the pivot block,
	// the mission of the snap sync is regarded as accomplished and the mode
	// is flipped to full-sync.
	if d.moder.disableSnap() {
		log.Info("Disabled snap-sync after pivot commitment", "number", block.Number(), "hash", block.Hash())
	}
	return nil
}

// processFastSyncContent takes fetch results from the queue and writes them to the
// database using the classical fast-sync path (NodeData trie download, no EVM
// execution for pre-pivot blocks). It is the orchestration layer that ties header
// download, body+receipt download, and NodeData state sync into a coherent sync.
//
// Ported from upstream Geth v1.10.13, adapted for v1.17.3:
//   - State-sync arg is *fastStateSync (vs *stateSync in the snap path)
//   - Calls d.syncFastState(root) instead of d.syncState(root)
//   - Receipts are rlp.RawValue (v1.17.3 format, not types.Receipts)
//   - d.committed is atomic.Bool (not atomic.Int32)
//   - stateBloom omitted (removed in v1.14+)
//   - Uses commitFastSyncPivotBlock (distinct from snap's commitPivotBlock)
//
// Dormant until A.4 adds the FastSync SyncMode enum value.
// Refs: #844, Phase A.3.
func (d *Downloader) processFastSyncContent() error {
	// Start syncing state of the reported head block. This should get us most of
	// the state of the pivot block.
	// State-sync root: default to the dynamic pivot header's Root.
	// Override with the operator-pinned root only when an explicit pivot number
	// was provided (all-or-nothing: number+hash+root set together in A.6).
	// Port of XDPoSChain 52351b081 — use latest.Root as fallback so a bare
	// --syncmode fast (no --fastsyncpivot* flags) still state-syncs correctly.
	d.pivotLock.RLock()
	pivotRoot := d.pivotHeader.Root
	d.pivotLock.RUnlock()
	if d.operatorPivotNumber != 0 {
		pivotRoot = d.operatorPivotRoot
	}
	sync := d.syncFastState(pivotRoot)

	defer func() {
		// The `sync` object is replaced every time the pivot moves. We need to
		// defer close the very last active one, hence the lazy evaluation vs.
		// calling defer sync.Cancel() !!!
		sync.Cancel()
	}()

	closeOnErr := func(s *fastStateSync) {
		if err := s.Wait(); err != nil && err != errCancelStateFetch && err != errCanceled {
			d.queue.Close() // wake up Results
		}
	}
	go closeOnErr(sync)

	// To cater for moving pivot points, track the pivot block and subsequently
	// accumulated download results separately.
	var (
		oldPivot *fetchResult   // Locked in pivot block, might change eventually
		oldTail  []*fetchResult // Downloaded content after the pivot
	)
	for {
		// Wait for the next batch of downloaded data to be available, and if the pivot
		// block became stale, move the goalpost
		results := d.queue.Results(oldPivot == nil) // Block if we're not monitoring pivot staleness
		if len(results) == 0 {
			// If pivot sync is done, stop
			if oldPivot == nil {
				return sync.Cancel()
			}
			// If sync failed, stop
			select {
			case <-d.cancelCh:
				sync.Cancel()
				return errCanceled
			default:
			}
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		// If we haven't downloaded the pivot block yet, check pivot staleness
		// notifications from the header downloader
		d.pivotLock.RLock()
		pivot := d.pivotHeader
		d.pivotLock.RUnlock()

		if oldPivot == nil {
			// Re-check state root — use operator-pinned root if set, else dynamic pivot root.
			// When operator-pinned, never restart state sync due to pivot root change.
			targetRoot := pivot.Root
			if d.operatorPivotRoot != (common.Hash{}) {
				targetRoot = d.operatorPivotRoot
			}
			if targetRoot != sync.root {
				sync.Cancel()
				sync = d.syncFastState(targetRoot)

				go closeOnErr(sync)
			}
		} else {
			results = append(append([]*fetchResult{oldPivot}, oldTail...), results...)
		}
		// Split around the pivot block and process the two sides via fast/full sync.
		// If the pivot became stale (2*fsMinFullBlocks behind latest), move it.
		// Operator-pinned pivot: skip the staleness branch entirely — the pin is
		// authoritative and must never be moved (refs #844 §3.4).
		//
		// A.97.1 (refs #894 #886): also skip when the pivot is frozen — state
		// download is underway and advancing the pivot now would produce an
		// incomplete trie at the canonical pivot the chain settles on. The freeze
		// is set the first time oldPivot is populated (see below).
		if !d.committed.Load() && d.operatorPivotNumber == 0 && len(results) > 0 {
			latest := results[len(results)-1].Header
			// If the height is above the pivot block by 2 sets, it means the pivot
			// became stale in the network and was garbage collected; move to a new pivot.
			// Note: reorgProtHeaderDelay blocks are withheld, account for them.
			if height := latest.Number.Uint64(); height >= pivot.Number.Uint64()+2*uint64(fsMinFullBlocks)-uint64(reorgProtHeaderDelay) {
				newPivotNum := height - uint64(fsMinFullBlocks) + uint64(reorgProtHeaderDelay)
				// A.97.1: refuse the update if pivot is frozen (state download underway).
				if d.refusePivotUpdate(newPivotNum, results[len(results)-1-fsMinFullBlocks+reorgProtHeaderDelay].Header.Hash()) {
					// Pivot frozen — skip the advance. The stale-pivot consequence
					// (pivot a few minutes behind tip at completion) is handled by
					// the existing pivot→tip block import after commitFastSyncPivotBlock.
				} else {
					log.Warn("Fast-sync pivot became stale, moving", "old", pivot.Number.Uint64(), "new", newPivotNum)
					pivot = results[len(results)-1-fsMinFullBlocks+reorgProtHeaderDelay].Header // must exist as lower old pivot is uncommitted

					d.pivotLock.Lock()
					d.pivotHeader = pivot
					d.pivotLock.Unlock()

					// Write out the pivot into the database so a rollback beyond it will
					// re-enable fast sync
					rawdb.WriteLastPivotNumber(d.stateDB, pivot.Number.Uint64())
				}
			}
		}
		P, beforeP, afterP := splitAroundPivot(pivot.Number.Uint64(), results)
		if err := d.commitFastSyncData(beforeP, sync); err != nil {
			return err
		}
		if P != nil {
			// If new pivot block found, cancel old state retrieval and restart.
			// Operator-pinned pivot: use pivotRoot as the state-sync target instead
			// of the pivot block's Root field (refs #844 §3.4).
			if oldPivot != P {
				sync.Cancel()
				stateRoot := P.Header.Root
				if d.operatorPivotRoot != (common.Hash{}) {
					stateRoot = d.operatorPivotRoot
				}
				sync = d.syncFastState(stateRoot)

				go closeOnErr(sync)
				oldPivot = P

				// A.97.1b (refs #894 #886): safety net — ensure freeze is set
				// by the time state download is underway for this pivot block.
				// The primary freeze was set before spawnSync (downloader.go
				// ~1131 region) so this is idempotent in the normal path; it
				// only fires on a restart-within-cycle (e.g. pivot root changed
				// before oldPivot was assigned). Dynamic path only.
				if d.operatorPivotNumber == 0 {
					d.FreezePivot()
				}
			}
			// Wait for completion, occasionally checking for pivot staleness
			select {
			case <-sync.done:
				if sync.err != nil {
					return sync.err
				}
				// Hash verification on completion (refs #844 §3.6.4a):
				// when operator pinned a pivot hash, assert the served block
				// matches. Mismatch → return error so sync restarts on a
				// different peer.
				if d.operatorPivotHash != (common.Hash{}) {
					if P.Header.Hash() != d.operatorPivotHash {
						return fmt.Errorf("fast sync: pivot hash mismatch — operator pinned %s, peer served %s",
							d.operatorPivotHash.Hex(), P.Header.Hash().Hex())
					}
				}
				if err := d.commitFastSyncPivotBlock(P); err != nil {
					return err
				}
				oldPivot = nil

			case <-time.After(time.Second):
				oldTail = afterP
				continue
			}
		}
		// XDC gap-pivot enumeration (FAST_SYNC_IMPLEMENTATION.md §3.6-3.7).
		// After the pivot is committed and before full-sync import of afterP,
		// iterate each XDPoS gap block between origin and pivot. For each gap
		// block, download its trie state via syncFastState then materialise the
		// masternode snapshot via the injected gapSnapshotFn callback.
		// This ensures the consensus engine can resolve masternode sets at every
		// post-pivot epoch crossing (epochBoundary - Gap blocks). Phase A.5 of #844.
		//
		// A.97.4 (refs #894): when the gap-block state download fails because
		// pruned mainnet peers cannot serve that state epoch, fall back to seeding
		// the masternode snapshot directly from the embedded header data
		// (header.Validators / header.Penalties — XDC epoch-switch headers carry
		// the full masternode set). Hard-abort only when the header lacks embedded
		// data (older epoch or config mismatch).
		d.pivotGapLock.RLock()
		gaps := append([]uint64(nil), d.pivotGapNumbers...) // copy under lock
		fn := d.gapSnapshotFn
		fallbackFn := d.gapFallbackFn
		d.pivotGapLock.RUnlock()

		// A.97.6 (refs #894): gate the gap-pivot state download behind a committed
		// pivot. The loop's contract (above) is "after the pivot is committed", but
		// nothing enforced it. In the TRUE dynamic-pivot path (--no-auto-pivot, no
		// trusted anchor) origin=0, so the body+receipt backfill walks from genesis
		// up to the pivot while the pivot-state NodeData download runs concurrently.
		// The state finishes (~2h) long before the multi-hour genesis→pivot backfill
		// reaches the pivot block, so commitFastSyncPivotBlock has not yet run and
		// d.committed is still false. With no guard, the gap loop fired the moment
		// state drained and called d.syncFastState() per gap, which cancels the
		// single-runner main `sync`; errCancelStateFetch then leaked out of
		// synchronise → "Beacon backfilling failed" → SyncFailed → rewind → retry,
		// an infinite loop with head=0 (observed: 0 commits, 97 state-sync restarts,
		// rewind targets creeping 1020→9194). Skipping the gap loop until committed
		// lets the backfill run uninterrupted; once the pivot block is reached and
		// committed (head advances, committed=true), the gap loop runs in that same
		// iteration to seed masternode snapshots before the line ~1990 committed
		// return. Pre-commit iterations leave d.pivotGapNumbers intact so the work
		// is performed exactly once, post-commit.
		if d.committed.Load() && len(gaps) > 0 && fn != nil {
			log.Info("Fast Sync: starting XDC gap-pivot state download", "count", len(gaps))
			for _, gapNum := range gaps {
				// Resolve the gap header from the local DB (headers were downloaded
				// during the fast-sync header fetch phase above).
				gapHash := rawdb.ReadCanonicalHash(d.stateDB, gapNum)
				if gapHash == (common.Hash{}) {
					log.Error("Fast Sync: gap block canonical hash missing", "number", gapNum)
					return fmt.Errorf("fast sync: gap block %d canonical hash missing", gapNum)
				}
				gapHeader := rawdb.ReadHeader(d.stateDB, gapHash, gapNum)
				if gapHeader == nil {
					log.Error("Fast Sync: gap header not in chain DB", "number", gapNum, "hash", gapHash)
					return fmt.Errorf("fast sync: gap header %d missing", gapNum)
				}

				// Download trie state at the gap block's state root.
				gapSync := d.syncFastState(gapHeader.Root)
				go closeOnErr(gapSync)
				stateErr := gapSync.Wait()
				if stateErr != nil && stateErr != errCancelStateFetch && stateErr != errCanceled {
					// A.97.4: check whether this is a peers-exhausted failure
					// (all reachable peers have pruned this epoch's state). If so,
					// attempt the embedded-header masternode fallback before aborting.
					if strings.Contains(stateErr.Error(), "failed with all peers") && fallbackFn != nil {
						mnCount, fallbackErr := fallbackFn(gapHeader)
						if fallbackErr != nil {
							log.Error("A.97.4: gap header lacks embedded masternodes — cannot continue",
								"number", gapNum, "err", fallbackErr)
							return fmt.Errorf("fast sync: gap %d state unavailable and header lacks embedded masternodes"+
								" (use --fastsyncpivot* or connect an archive peer): %w", gapNum, fallbackErr)
						}
						log.Warn("A.97.4: gap state unavailable on peers — seeded masternodes from embedded header",
							"number", gapNum, "masternodes", mnCount)
						// State not downloaded — only the masternode snapshot is seeded.
						// Missing gap state (if needed beyond masternodes) is Phase-2
						// snap-heal's responsibility. Do NOT mark state as present.
						continue
					}
					log.Error("Fast Sync: gap state download failed", "number", gapNum, "err", stateErr)
					return fmt.Errorf("fast sync: gap %d state: %w", gapNum, stateErr)
				}

				// State downloaded successfully — materialise the masternode snapshot.
				if err := fn(gapNum, gapHeader.Root); err != nil {
					log.Error("Fast Sync: gap snapshot generation failed", "number", gapNum, "err", err)
					return fmt.Errorf("fast sync: gap %d snapshot: %w", gapNum, err)
				}
				log.Info("Fast Sync: gap state synced", "number", gapNum, "hash", gapHash)
			}
			// Clear so a subsequent sync cycle doesn't re-run.
			d.pivotGapLock.Lock()
			d.pivotGapNumbers = nil
			d.pivotGapLock.Unlock()
		}

		// Fast sync done, pivot commit done, full import
		if err := d.importBlockResults(afterP); err != nil {
			return err
		}
		// A.92: once the pivot is committed, return nil immediately.
		//
		// During the 1–2h NodeData state download, fetchHeaders continuously
		// moves d.pivotHeader forward (~120 blocks every ~4 minutes). After
		// commitFastSyncPivotBlock sets committed=true and oldPivot=nil, the
		// top-of-loop "targetRoot != sync.root" check sees a stale-moved pivot
		// root that differs from the committed pivot's root, and starts a SECOND
		// 83M-entry state download.  When the queue is eventually drained,
		// sync.Cancel() on that running download returns errCancelStateFetch,
		// which propagates as SyncFailed, keeping the node in an infinite retry
		// loop with currentBlock=0 forever.
		//
		// The correct behaviour: after the pivot is committed the full-sync
		// catch-up (TIP_CATCH phase in xdcSyncer) handles all remaining
		// post-pivot blocks from scratch.  Abandoning the small tail of
		// yet-unprocessed queue entries is safe — they will be re-fetched.
		// Refs: #857 A.92.
		if d.committed.Load() {
			sync.Cancel()
			return nil
		}
	}
}

// commitFastSyncData writes pre-pivot fast-sync blocks (headers + bodies +
// receipts, no EVM execution) into the database via InsertReceiptChain.
// The stateSync argument is checked for early termination so we don't write
// blocks whose state we already know we won't finish syncing.
//
// Adapted from Geth v1.10.13: uses *fastStateSync (not *stateSync), and
// receipts are rlp.RawValue (v1.17.3 wire format).
func (d *Downloader) commitFastSyncData(results []*fetchResult, stateSync *fastStateSync) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	case <-stateSync.done:
		if err := stateSync.Wait(); err != nil {
			return err
		}
	default:
	}
	// Retrieve the batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log.Debug("Inserting fast-sync blocks", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnumn", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	receipts := make([]rlp.RawValue, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.body())
		receipts[i] = result.Receipts
	}
	if index, err := d.blockchain.InsertReceiptChain(blocks, receipts, d.ancientLimit); err != nil {
		log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return fmt.Errorf("%w: %v", errInvalidChain, err)
	}
	return nil
}

// commitFastSyncPivotBlock commits the pivot block as the new chain head after
// classical fast sync (NodeData trie download) completes. Unlike the snap-sync
// pivot path, this calls FastSyncCommitHead (no SnapSyncStart prerequisite).
//
// Adapted from Geth v1.10.13 commitPivotBlock:
//   - Uses FastSyncCommitHead instead of SnapSyncComplete
//   - stateBloom.Close() omitted (bloom removed in v1.14+)
func (d *Downloader) commitFastSyncPivotBlock(result *fetchResult) error {
	block := types.NewBlockWithHeader(result.Header).WithBody(result.body())
	log.Debug("Committing fast sync pivot as new head", "number", block.Number(), "hash", block.Hash())

	// Commit the pivot block as the new head; full sync takes over from here.
	if _, err := d.blockchain.InsertReceiptChain([]*types.Block{block}, []rlp.RawValue{result.Receipts}, d.ancientLimit); err != nil {
		return err
	}
	if err := d.blockchain.FastSyncCommitHead(block.Hash()); err != nil {
		return err
	}
	d.committed.Store(true)
	// A.42: the pivot is now durably committed; the on-disk fast-sync checkpoint
	// has served its purpose. Drop it so a subsequent (different-pivot) sync
	// doesn't trip the stale-checkpoint branch.
	rawdb.DeleteFastSyncCheckpoint(d.stateDB)
	log.Info("Fast-sync: checkpoint cleared after Committed", "pivot", block.Number().Uint64())
	return nil
}

// dataPack is a data message returned by a peer for some query.
// Used by the classical Fast Sync NodeData path (Phase A.2 of #844).
type dataPack interface {
	PeerId() string
	Items() int
	Stats() string
}

// statePack is a batch of NodeData state blobs returned by a peer.
type statePack struct {
	peerID string
	states [][]byte
}

func (p *statePack) PeerId() string { return p.peerID }
func (p *statePack) Items() int     { return len(p.states) }
func (p *statePack) Stats() string  { return fmt.Sprintf("%d", len(p.states)) }

// DeliverNodeData injects a NodeData retrieval response from a remote peer into
// the fast-sync delivery channel. Called from eth/handler_eth.go when the
// ethHandler.Handle switch processes a *eth.NodeDataPacket.
//
// A.36: the send MUST NOT block. Previously this was a 2-arm select with no
// default; if no fast-state fetcher was draining d.fastStateCh (fast-sync
// inactive, state-sync loop between phases, or simply a stray response from a
// peer the downloader has already abandoned), the peer's message-reader
// goroutine would stall forever, which manifested as silent NodeData loss and
// eventual peer expiry via the 5-min tracker. We now drop on no-reader, bump a
// meter, and reset the per-peer nodeDataIdle flag so this peer becomes
// re-assignable (otherwise A.27's CompareAndSwap-based semaphore would keep it
// permanently "in-flight" from the downloader's point of view).
//
// A.67: fastStateCh is now buffered (size 256, see New()). The select still
// has the non-blocking `default:` arm to preserve A.36 semantics when the
// buffer is FULL — that's now the only condition under which we drop. With
// the buffer drain side guaranteed (fastStateFetcher always either consumes
// to its own discard arm when no sync is active, or routes to runFastStateSync
// when sync is active), a full buffer signals the trie processor is genuinely
// overwhelmed. We still SetNodeDataIdle on drop so the peer becomes
// re-assignable rather than stuck in stateIdle accounting.
func (d *Downloader) DeliverNodeData(id string, data [][]byte) error {
	select {
	case d.fastStateCh <- &statePack{id, data}:
		// A.40: track empty responses at the peer level so PBSS-only peers
		// can be blacklisted. Only count success-arm — drops are not the
		// peer's fault.
		if peer := d.peers.Peer(id); peer != nil {
			peer.RecordNodeDataResponse(len(data))
		}
		return nil
	case <-d.cancelCh:
		return errCanceled
	default:
		// Buffer full or no fetcher — drop and unstick the peer.
		nodeDataDropMeter.Mark(int64(len(data)))
		log.Warn("NodeData drop, fast-state fetcher buffer full or absent", "peer", id, "count", len(data))
		if peer := d.peers.Peer(id); peer != nil {
			// Calling SetNodeDataIdle with (delivered=0, deliveryTime=now) clears
			// the A.27 in-flight flag and zero-updates the rate tracker. Matches
			// the semantics of a timed-out request from the peer's perspective.
			peer.SetNodeDataIdle(0, time.Now())
		}
		return nil
	}
}

// DeliverSnapPacket is invoked from a peer's message handler when it transmits a
// data packet for the local node to consume.
func (d *Downloader) DeliverSnapPacket(peer *snap.Peer, packet snap.Packet) error {
	switch packet := packet.(type) {
	case *snap.AccountRangePacket:
		hashes, accounts, err := packet.Unpack()
		if err != nil {
			return err
		}
		return d.snapSyncer.OnAccounts(peer, packet.ID, hashes, accounts, packet.Proof)

	case *snap.StorageRangesPacket:
		hashset, slotset := packet.Unpack()
		return d.snapSyncer.OnStorage(peer, packet.ID, hashset, slotset, packet.Proof)

	case *snap.ByteCodesPacket:
		return d.snapSyncer.OnByteCodes(peer, packet.ID, packet.Codes)

	case *snap.TrieNodesPacket:
		return d.snapSyncer.OnTrieNodes(peer, packet.ID, packet.Nodes)

	case *snap.AccessListsPacket:
		return d.snapSyncer.OnAccessLists(peer, packet.ID, packet.AccessLists)

	default:
		return fmt.Errorf("unexpected snap packet type: %T", packet)
	}
}

// RegisterSnapPeer registers a snap peer with the active state syncer. Peers that
// negotiated a snap version below the syncer's minimum are skipped — e.g. the
// snap/2 syncer skips snap/1-only peers, which cannot answer its BAL requests.
func (d *Downloader) RegisterSnapPeer(p *snap.Peer) error {
	if p.Version() < d.snapSyncer.Version() {
		// The peer speaks an older snap version than the active syncer needs
		// (e.g. snap/1 while we sync via snap/2). We still serve it, but it
		// cannot answer our requests, so it is not registered for syncing.
		// Surface it so an operator can tell a stalled sync from a quiet one.
		snapPeerSkipMeter.Mark(1)
		log.Debug("Skipping snap peer below syncer version", "peer", p.ID(), "version", p.Version(), "required", d.snapSyncer.Version())
		return nil
	}
	return d.snapSyncer.Register(p)
}

// SnapSyncVersion returns the snap protocol version of the active state syncer.
// Peers negotiating a lower version cannot serve its requests.
func (d *Downloader) SnapSyncVersion() uint {
	return d.snapSyncer.Version()
}

// UnregisterSnapPeer removes a snap peer from the active state syncer. It mirrors
// RegisterSnapPeer's version gate: a peer below the active syncer's version was
// never registered, so there is nothing to remove.
func (d *Downloader) UnregisterSnapPeer(p *snap.Peer) error {
	if p.Version() < d.snapSyncer.Version() {
		return nil
	}
	return d.snapSyncer.Unregister(p.ID())
}

// readHeaderRange returns a list of headers, using the given last header as the base,
// and going backwards towards genesis. This method assumes that the caller already has
// placed a reasonable cap on count.
func (d *Downloader) readHeaderRange(last *types.Header, count int) []*types.Header {
	var (
		current = last
		headers []*types.Header
	)
	for {
		parent := d.blockchain.GetHeaderByHash(current.ParentHash)
		if parent == nil {
			break // The chain is not continuous, or the chain is exhausted
		}
		headers = append(headers, parent)
		if len(headers) >= count {
			break
		}
		current = parent
	}
	return headers
}

// reportSnapSyncProgress calculates various status reports and provides it to the user.
func (d *Downloader) reportSnapSyncProgress(force bool) {
	// Initialize the sync start time if it's the first time we're reporting
	if d.syncStartTime.IsZero() {
		d.syncStartTime = time.Now().Add(-time.Millisecond) // -1ms offset to avoid division by zero
	}
	// Don't report all the events, just occasionally
	if !force && time.Since(d.syncLogTime) < 8*time.Second {
		return
	}
	// Don't report anything until we have a meaningful progress
	var (
		headerBytes, _  = d.stateDB.AncientSize(rawdb.ChainFreezerHeaderTable)
		bodyBytes, _    = d.stateDB.AncientSize(rawdb.ChainFreezerBodiesTable)
		receiptBytes, _ = d.stateDB.AncientSize(rawdb.ChainFreezerReceiptTable)
	)
	syncedBytes := common.StorageSize(headerBytes + bodyBytes + receiptBytes)
	if syncedBytes == 0 {
		return
	}
	var (
		header = d.blockchain.CurrentHeader()
		block  = d.blockchain.CurrentSnapBlock()
	)
	// Prevent reporting if nothing has been synchronized yet
	if block.Number.Uint64() <= d.syncStartBlock {
		return
	}
	// Prevent reporting noise if the actual chain synchronization (headers
	// and bodies) hasn't started yet. Inserting the ancient header chain is
	// fast enough and would introduce significant bias if included in the count.
	if d.chainCutoffNumber != 0 && block.Number.Uint64() <= d.chainCutoffNumber {
		return
	}
	fetchedBlocks := block.Number.Uint64() - d.syncStartBlock
	if d.chainCutoffNumber != 0 && d.chainCutoffNumber > d.syncStartBlock {
		fetchedBlocks = block.Number.Uint64() - d.chainCutoffNumber
	}
	// Retrieve the current chain head and calculate the ETA
	latest, _, _, err := d.skeleton.Bounds()
	if err != nil {
		// We're going to cheat for non-merged networks, but that's fine
		latest = d.pivotHeader
	}
	if latest == nil {
		// This should really never happen, but add some defensive code for now.
		// TODO(karalabe): Remove it eventually if we don't see it blow.
		log.Error("Nil latest block in sync progress report")
		return
	}
	var (
		left = latest.Number.Uint64() - block.Number.Uint64()
		eta  = time.Since(d.syncStartTime) / time.Duration(fetchedBlocks) * time.Duration(left)

		progress = fmt.Sprintf("%.2f%%", float64(block.Number.Uint64())*100/float64(latest.Number.Uint64()))
		headers  = fmt.Sprintf("%v@%v", log.FormatLogfmtUint64(header.Number.Uint64()), common.StorageSize(headerBytes).TerminalString())
		bodies   = fmt.Sprintf("%v@%v", log.FormatLogfmtUint64(block.Number.Uint64()), common.StorageSize(bodyBytes).TerminalString())
		receipts = fmt.Sprintf("%v@%v", log.FormatLogfmtUint64(block.Number.Uint64()), common.StorageSize(receiptBytes).TerminalString())
	)
	log.Info("Syncing: chain download in progress", "synced", progress, "chain", syncedBytes, "headers", headers, "bodies", bodies, "receipts", receipts, "eta", common.PrettyDuration(eta))
	d.syncLogTime = time.Now()
}
