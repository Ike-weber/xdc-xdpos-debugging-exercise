// Copyright 2022 The go-ethereum Authors
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

package downloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// shortPeerID returns at most the first 8 chars of a peer ID for log output.
// Some test peers use very short IDs (e.g. "peer4"), so a bare peer.id[:8]
// would panic. Refs A.10/A.50/A.53 (regression-safe logger).
func shortPeerID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// scratchHeaders is the number of headers to store in a scratch space to allow
// concurrent downloads. A header is about 0.5KB in size, so there is no worry
// about using too much memory. The only catch is that we can only validate gaps
// after they're linked to the head, so the bigger the scratch space, the larger
// potential for invalid headers.
//
// The current scratch space of 196608 headers is expected to use 96MB RAM.
//
// A.11 (refs #857): bumped from 131072 to 196608 so the scratch space divides
// evenly by BOTH the eth/66+ batch size (512 → 384 slots) and the XDPOS2
// MaxHeaderFetch batch size (192 → 1024 slots). The skeleton picks one of the
// two batch sizes per cycle (see s.cycleBatch) depending on the active peer
// fleet, and the scratchOwners table is sized accordingly. 131072 % 192 != 0,
// so the previous value forced an integer-divisibility panic when the cycle
// chose the XDPOS2 batch size.
const scratchHeaders = 196608

// requestHeaders is the default number of headers to request from a remote peer
// in a single network packet for eth/66+ peers (modern Ethereum fleet, serve cap
// 1024). For XDPOS2 (XDC) peers, the per-cycle batch is dynamically lowered to
// requestHeadersXDPOS2 (=192, the XDPoSChain server-side MaxHeaderFetch cap) at
// sync-cycle start in sync(). See s.cycleBatch.
//
// Although the skeleton downloader takes into consideration peer capacities
// when picking idlers, the packet size remains constant within a sync cycle
// since headers are relatively small and it's easier to work with fixed
// batches vs. dynamic interval fillings.
const requestHeaders = 512

// requestHeadersXDPOS2 is the per-packet header count used when the active
// peer fleet contains any XDPOS2 (=eth.XDPOS2, version=100) peer. XDPoSChain
// peers cap their GetBlockHeaders response at MaxHeaderFetch (192) server-side
// (XDPoSChain/eth/handler.go:445). Asking for 512 returns at most 192 and the
// strict-equality validator below ("Invalid non-genesis header count") rejects
// every honest reply, stalling the skeleton indefinitely. Refs #857, see
// docs/port/XDPOSCHAIN_WIRE_DIFF.md Section 4.1.
//
// Note: this is intentionally hard-coded (not pulled from MaxHeaderFetch) so
// that the skeleton package has no build-time dependency on the downloader
// package's MaxHeaderFetch constant. The value MUST match
// XDPoSChain/eth/downloader/downloader.go:45 (and the local mirror at
// eth/downloader/downloader.go:45) — both are 192 today.
const requestHeadersXDPOS2 = 192

// A.69 (refs #844 #859): skeleton peer-starvation floor. When the count of
// idle peers is at or below this threshold, the skeleton's empty-response
// drop path (XDC fix #131) is bypassed — dropping our only header server
// leaves the skeleton with no path to recovery. The counter is still bumped
// + logged; only the drop is suppressed.
const skeletonEmptyDropPeerFloor = 2

// errSyncLinked is an internal helper error to signal that the current sync
// cycle linked up to the genesis block, this the skeleton syncer should ping
// the backfiller to resume. Since we already have that logic on sync start,
// piggy-back on that instead of 2 entrypoints.
var errSyncLinked = errors.New("sync linked")

// errSyncMerged is an internal helper error to signal that the current sync
// cycle merged with a previously aborted subchain, thus the skeleton syncer
// should abort and restart with the new state.
var errSyncMerged = errors.New("sync merged")

// errSyncReorged is an internal helper error to signal that the head chain of
// the current sync cycle was (partially) reorged, thus the skeleton syncer
// should abort and restart with the new state.
var errSyncReorged = errors.New("sync reorged")

// errSyncTrimmed is an internal helper error to signal that the local chain
// has been trimmed (e.g, via debug_setHead explicitly) and the skeleton chain
// is no longer linked with the local chain. In this case, the skeleton sync
// should be re-scheduled again.
var errSyncTrimmed = errors.New("sync trimmed")

// errTerminated is returned if the sync mechanism was terminated for this run of
// the process. This is usually the case when Geth is shutting down and some events
// might still be propagating.
var errTerminated = errors.New("terminated")

// errChainReorged is an internal helper error to signal that the header chain
// of the current sync cycle was (partially) reorged.
var errChainReorged = errors.New("chain reorged")

// errChainGapped is an internal helper error to signal that the header chain
// of the current sync cycle is gaped with the one advertised by consensus client.
var errChainGapped = errors.New("chain gapped")

// errChainForked is an internal helper error to signal that the header chain
// of the current sync cycle is forked with the one advertised by consensus client.
var errChainForked = errors.New("chain forked")

func init() {
	// Tuning parameters is nice, but the scratch space must be assignable in
	// full to peers. It's a useless cornercase to support a dangling half-group.
	//
	// A.11 (refs #857): scratchHeaders must divide evenly by BOTH possible
	// cycle batch sizes — eth/66+ requestHeaders (512) and XDPOS2
	// requestHeadersXDPOS2 (192). The sync cycle picks one of the two at
	// startup based on the active peer fleet and partitions scratchOwners
	// accordingly.
	if scratchHeaders%requestHeaders != 0 {
		panic("Please make scratchHeaders divisible by requestHeaders")
	}
	if scratchHeaders%requestHeadersXDPOS2 != 0 {
		panic("Please make scratchHeaders divisible by requestHeadersXDPOS2")
	}
}

// subchain is a contiguous header chain segment that is backed by the database,
// but may not be linked to the live chain. The skeleton downloader may produce
// a new one of these every time it is restarted until the subchain grows large
// enough to connect with a previous subchain.
//
// The subchains use the exact same database namespace and are not disjoint from
// each other. As such, extending one to overlap the other entails reducing the
// second one first. This combined buffer model is used to avoid having to move
// data on disk when two subchains are joined together.
type subchain struct {
	Head uint64      // Block number of the newest header in the subchain
	Tail uint64      // Block number of the oldest header in the subchain
	Next common.Hash // Block hash of the next oldest header in the subchain
}

// skeletonProgress is a database entry to allow suspending and resuming a chain
// sync. As the skeleton header chain is downloaded backwards, restarts can and
// will produce temporarily disjoint subchains. There is no way to restart a
// suspended skeleton sync without prior knowledge of all prior suspension points.
type skeletonProgress struct {
	Subchains []*subchain // Disjoint subchains downloaded until now
	Finalized *uint64     // Last known finalized block number
}

// headUpdate is a notification that the beacon sync should switch to a new target.
// The update might request whether to forcefully change the target, or only try to
// extend it and fail if it's not possible.
type headUpdate struct {
	header *types.Header // Header to update the sync target to
	final  *types.Header // Finalized header to use as thresholds
	force  bool          // Whether to force the update or only extend if possible
	errc   chan error    // Channel to signal acceptance of the new head
}

// headerRequest tracks a pending header request to ensure responses are to
// actual requests and to validate any security constraints.
//
// Concurrency note: header requests and responses are handled concurrently from
// the main runloop to allow Keccak256 hash verifications on the peer's thread and
// to drop on invalid response. The request struct must contain all the data to
// construct the response without accessing runloop internals (i.e. subchains).
// That is only included to allow the runloop to match a response to the task being
// synced without having yet another set of maps.
type headerRequest struct {
	peer string // Peer to which this request is assigned
	id   uint64 // Request ID of this request

	deliver chan *headerResponse // Channel to deliver successful response on
	revert  chan *headerRequest  // Channel to deliver request failure on
	cancel  chan struct{}        // Channel to track sync cancellation
	stale   chan struct{}        // Channel to signal the request was dropped

	head uint64 // Head number of the requested batch of headers

	// A.58 Day 2 (refs #857): snapshot of the batch size used when this
	// request was emitted to the peer. The validator must compare the
	// response length against this exact value rather than reading a shared
	// per-cycle global, so that future mixed-fleet support (where different
	// peers in the same cycle may serve different batch sizes) does not
	// silently mis-validate honest replies. Today every emitter sets this
	// from s.cycleBatch, so behavior is unchanged.
	batch int
}

// headerResponse is an already verified remote response to a header request.
type headerResponse struct {
	peer    *peerConnection // Peer from which this response originates
	reqid   uint64          // Request ID that this response fulfils
	headers []*types.Header // Chain of headers
}

// backfiller is a callback interface through which the skeleton sync can tell
// the downloader that it should suspend or resume backfilling on specific head
// events (e.g. suspend on forks or gaps, resume on successful linkups).
type backfiller interface {
	// suspend requests the backfiller to abort any running full or snap sync
	// based on the skeleton chain as it might be invalid. The backfiller should
	// gracefully handle multiple consecutive suspends without a resume, even
	// on initial startup.
	//
	// The method should return the last block header that has been successfully
	// backfilled (in the current or a previous run), falling back to the genesis.
	suspend() *types.Header

	// resume requests the backfiller to start running fill or snap sync based on
	// the skeleton chain as it has successfully been linked. Appending new heads
	// to the end of the chain will not result in suspend/resume cycles.
	// leaking too much sync logic out to the filler.
	resume()
}

// skeleton represents a header chain synchronized after the merge where blocks
// aren't validated any more via PoW in a forward fashion, rather are dictated
// and extended at the head via the beacon chain and backfilled on the original
// Ethereum block sync protocol.
//
// Since the skeleton is grown backwards from head to genesis, it is handled as
// a separate entity, not mixed in with the logical sequential transition of the
// blocks. Once the skeleton is connected to an existing, validated chain, the
// headers will be moved into the main downloader for filling and execution.
//
// Opposed to the original Ethereum block synchronization which is trustless (and
// uses a master peer to minimize the attack surface), post-merge block sync starts
// from a trusted head. As such, there is no need for a master peer any more and
// headers can be requested fully concurrently (though some batches might be
// discarded if they don't link up correctly).
//
// Although a skeleton is part of a sync cycle, it is not recreated, rather stays
// alive throughout the lifetime of the downloader. This allows it to be extended
// concurrently with the sync cycle, since extensions arrive from an API surface,
// not from within (vs. legacy Ethereum sync).
//
// Since the skeleton tracks the entire header chain until it is consumed by the
// forward block filling, it needs 0.5KB/block storage. At current mainnet sizes
// this is only possible with a disk backend. Since the skeleton is separate from
// the node's header chain, storing the headers ephemerally until sync finishes
// is wasted disk IO, but it's a price we're going to pay to keep things simple
// for now.
type skeleton struct {
	db     ethdb.Database // Database backing the skeleton
	filler backfiller     // Chain syncer suspended/resumed by head events
	chain  chainReader    // Underlying block chain

	peers *peerSet                   // Set of peers we can sync from
	idles map[string]*peerConnection // Set of idle peers in the current sync cycle
	drop  peerDropFn                 // Drops a peer for misbehaving

	// XDC fix #131: track consecutive empty-header responses per peer.
	// Peers that repeatedly return 0 headers get dropped to unblock skeleton sync.
	emptyResponseCount map[string]int // consecutive empty-header counts per peer ID

	progress *skeletonProgress // Sync progress tracker for resumption and metrics
	started  time.Time         // Timestamp when the skeleton syncer was created
	logged   time.Time         // Timestamp when progress was last logged to the user
	pulled   uint64            // Number of headers downloaded in this run

	scratchSpace  []*types.Header // Scratch space to accumulate headers in (first = recent)
	scratchOwners []string        // Peer IDs owning chunks of the scratch space (pend or delivered)
	scratchHead   uint64          // Block number of the first item in the scratch space

	requests map[uint64]*headerRequest // Header requests currently running

	headEvents chan *headUpdate // Notification channel for new heads
	terminate  chan chan error  // Termination channel to abort sync
	terminated chan struct{}    // Channel to signal that the syncer is dead

	// Callback hooks used during testing
	syncStarting func() // callback triggered after a sync cycle is inited but before started

	// terminateFloor, when non-zero, causes the reverse skeleton walk-back to
	// terminate (returning errSyncLinked) as soon as the active subchain tail
	// reaches floor+1, and causes linked() to short-circuit true at any height
	// >= floor. Set via SetTerminateFloor from the operator-pinned Fast Sync
	// path (eth/sync_xdc) so the skeleton does not chase headers below the
	// operator's pinned anchor — which the v1.17.3 trusted-peer fleet typically
	// does not have. Refs A.14.
	terminateFloor uint64

	// cycleBatch is the per-cycle GetBlockHeaders packet size in use for the
	// current sync cycle. It is chosen at sync-cycle start in sync(): if ANY
	// peer in the seeded idle pool is XDPOS2 (eth.XDPOS2, version=100), the
	// cycle uses requestHeadersXDPOS2 (=192) to respect XDPoSChain's
	// MaxHeaderFetch server-side cap; otherwise it uses requestHeaders (=512).
	// Mixing batch sizes within a single cycle is not supported because
	// scratchOwners is partitioned uniformly. A.11 (refs #857).
	cycleBatch int
}

// chainReader wraps the method to retrieve the head of the local chain.
type chainReader interface {
	// CurrentSnapBlock retrieves the head snap block from the local chain.
	CurrentSnapBlock() *types.Header
}

// newSkeleton creates a new sync skeleton that tracks a potentially dangling
// header chain until it's linked into an existing set of blocks.
func newSkeleton(db ethdb.Database, peers *peerSet, drop peerDropFn, filler backfiller, chain chainReader) *skeleton {
	sk := &skeleton{
		db:                 db,
		filler:             filler,
		chain:              chain,
		peers:              peers,
		drop:               drop,
		requests:           make(map[uint64]*headerRequest),
		headEvents:         make(chan *headUpdate),
		terminate:          make(chan chan error),
		terminated:         make(chan struct{}),
		emptyResponseCount: make(map[string]int), // XDC fix #131
	}
	go sk.startup()
	return sk
}

// SetTerminateFloor configures a height below which the reverse skeleton
// walk-back will not chase. When set, sync()'s response-processing branch
// returns errSyncLinked once the active subchain tail reaches floor+1, and
// linked() short-circuits true at any height >= floor. A zero floor disables
// the behaviour (default).
//
// Used by the operator-pinned Fast Sync path (eth/sync_xdc.fastRunningCheck)
// to bound the walk-back at the operator's pinned anchor, so the skeleton does
// not stall trying to fetch headers the v1.17.3 trusted-peer fleet doesn't
// have. The operator is trusting the source of the pinned header.
// Refs A.14.
func (s *skeleton) SetTerminateFloor(floor uint64) {
	s.terminateFloor = floor
}

// dropStaleSubchainsBelow inspects any previously persisted skeleton progress
// on disk and removes subchains whose Head is strictly below `pivotNumber`.
// Returns the number of subchains dropped.
//
// A.71 (refs #844 #859): under operator-pinned Fast Sync the only valid
// skeleton subchain is the one anchored at (or above) the operator-pinned
// pivot — every block below the pivot is trusted by operator authority and
// will be back-filled via processFastSyncContent, never via skeleton walk-back.
// A stale subchain with Head far below the pivot poisons initSync (which only
// drops subchains whose Tail >= the new headchain's Tail; with old Tail
// ≈ 64.9M and new Tail = 103.3M the drop predicate fails). The resulting
// two-subchain state prevents `linked()` from ever returning true on the
// re-entry path (len(Subchains) == 1 check), so beaconBackfiller.resume() is
// never called → synchronise() never runs → SyncCompleted never fires →
// block-import never engages. Empirical evidence (xdcscan 2026-06-07, 8 min
// of "GetBlockHeaders SEND origin_num=103,309,268 amount=192 reverse=true"
// on peer cafdf5e8 with eth_blockNumber stuck at 0) confirms this signature.
//
// The cleanup is safe because every subchain below the pivot is by definition
// out of date relative to the trusted pivot — we'll never use it for linkup.
// If after pruning no subchains remain, the entire skeleton sync status is
// deleted so the next initSync starts from a clean slate.
//
// Idempotent. Must be called BEFORE any Sync() call so the in-memory
// `s.progress` (loaded on initSync) reflects the on-disk cleanup.
//
// Refs #844 #859 A.71.
func (s *skeleton) dropStaleSubchainsBelow(pivotNumber uint64) int {
	if pivotNumber == 0 {
		return 0
	}
	status := rawdb.ReadSkeletonSyncStatus(s.db)
	if len(status) == 0 {
		return 0
	}
	progress := new(skeletonProgress)
	if err := json.Unmarshal(status, progress); err != nil {
		log.Warn("Skeleton: failed to decode persisted progress for stale-subchain prune; deleting status",
			"err", err)
		rawdb.DeleteSkeletonSyncStatus(s.db)
		return 0
	}
	if len(progress.Subchains) == 0 {
		return 0
	}
	kept := progress.Subchains[:0]
	dropped := 0
	for _, sc := range progress.Subchains {
		if sc == nil {
			continue
		}
		if sc.Head < pivotNumber {
			log.Info("Skeleton: dropping stale subchain below operator pivot",
				"head", sc.Head, "tail", sc.Tail, "pivot", pivotNumber)
			dropped++
			continue
		}
		kept = append(kept, sc)
	}
	if dropped == 0 {
		return 0
	}
	progress.Subchains = kept
	if len(progress.Subchains) == 0 {
		// Nothing valid left — clear the persisted status entirely so the next
		// Sync() reinitialises from the announced head without trying to merge
		// against ghost subchains.
		rawdb.DeleteSkeletonSyncStatus(s.db)
		log.Info("Skeleton: cleared skeleton sync status (all subchains pruned)",
			"dropped", dropped, "pivot", pivotNumber)
		return dropped
	}
	encoded, err := json.Marshal(progress)
	if err != nil {
		// Cannot happen for well-formed Subchains; log defensively and clear.
		log.Warn("Skeleton: failed to re-encode pruned progress; deleting status",
			"err", err)
		rawdb.DeleteSkeletonSyncStatus(s.db)
		return dropped
	}
	rawdb.WriteSkeletonSyncStatus(s.db, encoded)
	log.Info("Skeleton: rewrote sync status after pruning stale subchains",
		"dropped", dropped, "kept", len(kept), "pivot", pivotNumber)
	return dropped
}

// startup is an initial background loop which waits for an event to start or
// tear the syncer down. This is required to make the skeleton sync loop once
// per process but at the same time not start before the beacon chain announces
// a new (existing) head.
func (s *skeleton) startup() {
	// Close a notification channel so anyone sending us events will know if the
	// sync loop was torn down for good.
	defer close(s.terminated)

	// Wait for startup or teardown. This wait might loop a few times if a beacon
	// client requests sync head extensions, but not forced reorgs (i.e. they are
	// giving us new payloads without setting a starting head initially).
	for {
		select {
		case errc := <-s.terminate:
			// No head was announced but Geth is shutting down
			errc <- nil
			return

		case event := <-s.headEvents:
			// New head announced, start syncing to it, looping every time a current
			// cycle is terminated due to a chain event (head reorg, old chain merge).
			if !event.force {
				event.errc <- errors.New("forced head needed for startup")
				continue
			}
			event.errc <- nil // forced head accepted for startup
			head := event.header
			s.started = time.Now()

			for {
				// If the sync cycle terminated or was terminated, propagate up when
				// higher layers request termination. There's no fancy explicit error
				// signaling as the sync loop should never terminate (TM).
				newhead, err := s.sync(head)
				switch {
				case err == errSyncLinked:
					// Sync cycle linked up to the genesis block, or the existent chain
					// segment. Tear down the loop and restart it so, it can properly
					// notify the backfiller. Don't account a new head.
					head = nil

				case err == errSyncMerged:
					// Subchains were merged, we just need to reinit the internal
					// start to continue on the tail of the merged chain. Don't
					// announce a new head,
					head = nil

				case err == errSyncReorged:
					// The subchain being synced got modified at the head in a
					// way that requires resyncing it. Restart sync with the new
					// head to force a cleanup.
					head = newhead

				case err == errSyncTrimmed:
					// The skeleton chain is not linked with the local chain anymore,
					// restart the sync.
					head = nil

				case err == errTerminated:
					// Sync was requested to be terminated from within, stop and
					// return (no need to pass a message, was already done internally)
					return

				default:
					// Sync either successfully terminated or failed with an unhandled
					// error. Abort and wait until Geth requests a termination.
					errc := <-s.terminate
					errc <- err
					return
				}
			}
		}
	}
}

// Terminate tears down the syncer indefinitely.
func (s *skeleton) Terminate() error {
	// Request termination and fetch any errors
	errc := make(chan error)
	s.terminate <- errc
	err := <-errc

	// Wait for full shutdown (not necessary, but cleaner)
	<-s.terminated
	return err
}

// Sync starts or resumes a previous sync cycle to download and maintain a reverse
// header chain starting at the head and leading towards genesis to an available
// ancestor.
//
// This method does not block, rather it just waits until the syncer receives the
// fed header. What the syncer does with it is the syncer's problem.
func (s *skeleton) Sync(head *types.Header, final *types.Header, force bool) error {
	log.Trace("New skeleton head announced", "number", head.Number, "hash", head.Hash(), "force", force)
	errc := make(chan error)

	select {
	case s.headEvents <- &headUpdate{header: head, final: final, force: force, errc: errc}:
		return <-errc
	case <-s.terminated:
		return errTerminated
	}
}

// linked returns the flag indicating whether the skeleton has been linked with
// the local chain.
func (s *skeleton) linked(number uint64, hash common.Hash) bool {
	// A.14c: floor means "we've reached pivot anchor"; condition fires AT or
	// BELOW the floor, not above it. The chain at/below the operator-pinned
	// anchor is implicitly trusted to be provided by the operator (or
	// backfilled later), so we treat any height at or below the floor as
	// already linked and do not gate on local body/receipt presence (which is
	// impossible on a cold chain). The inverted (>=) form regressed cold-start
	// fast-sync because scratchHead = head-1 (~103M) is always >> floor
	// (~79.34M), so the short-circuit fired immediately and the walk-back was
	// skipped entirely, leaving beaconsync.fetchHeaders unable to resolve
	// headers between origin+1 and pivot.
	//
	// This short-circuit MUST run first, before upstream's canonical-mapping
	// requirement below: at/below the floor a normal chain may still lack a
	// canonical mapping in the middle of an XDC fast-sync gap (see A.85 in
	// core/rawdb/chain_freezer.go), so the requirement only makes sense above
	// the floor. NOT independently verified against A.85 gap interaction on a
	// live cold fast-sync — flagged in the merge PR for follow-up.
	if s.terminateFloor != 0 && number <= s.terminateFloor {
		return true
	}
	// Require the canonical mapping, not just presence by hash. A block present
	// only by hash (side chain or orphan from an unclean shutdown) must not be
	// used as the link-up point, otherwise it's left in place forever without its
	// canonical mapping ever being rewritten. Keep descending to a real canonical
	// block.
	linked := rawdb.ReadCanonicalHash(s.db, number) == hash &&
		rawdb.HasHeader(s.db, hash, number) &&
		rawdb.HasBody(s.db, hash, number) &&
		rawdb.HasReceipts(s.db, hash, number)

	// Ensure the skeleton chain links to the local chain below the chain head.
	// This accounts for edge cases where leftover chain segments above the head
	// may still link to the skeleton chain. In such cases, synchronization is
	// likely to fail due to potentially missing segments in the middle.
	//
	// You can try to produce the edge case by these steps:
	// - sync the chain
	// - debug.setHead(`0x1`)
	// - kill the geth process (the chain segment will be left with chain head rewound)
	// - restart
	if s.chain.CurrentSnapBlock() != nil {
		linked = linked && s.chain.CurrentSnapBlock().Number.Uint64() >= number
	}
	return linked
}

// sync is the internal version of Sync that executes a single sync cycle, either
// until some termination condition is reached, or until the current cycle merges
// with a previously aborted run.
func (s *skeleton) sync(head *types.Header) (*types.Header, error) {
	// If we're continuing a previous merge interrupt, just access the existing
	// old state without initing from disk.
	if head == nil {
		head = rawdb.ReadSkeletonHeader(s.db, s.progress.Subchains[0].Head)
	} else {
		// Otherwise, initialize the sync, trimming and previous leftovers until
		// we're consistent with the newly requested chain head
		s.initSync(head)
	}
	// A.11 (refs #857): tentatively default the per-cycle batch to the eth/66+
	// size (512). This is reassigned below once we've seeded s.idles and can
	// see whether any XDPOS2 peer is in the fleet. We MUST set it before
	// allocating scratchOwners so the chunking matches the actual request size
	// every peer in this cycle will use.
	s.cycleBatch = requestHeaders

	// Create the scratch space to fill with concurrently downloaded headers.
	// The scratchSpace itself is uniform — chunking only matters for the
	// scratchOwners book-keeping table below, which is sized after we pick
	// the per-cycle batch size from the peer fleet.
	s.scratchSpace = make([]*types.Header, scratchHeaders)
	defer func() { s.scratchSpace = nil }() // don't hold on to references after sync

	// scratchOwners is allocated AFTER the cycle batch is chosen from the
	// seeded idle pool below (see s.idles seeding). We use a placeholder here
	// so the defer is registered even if the seed-loop panics; the real
	// allocation happens in-line after the peer fleet is known.
	defer func() { s.scratchOwners = nil }() // don't hold on to references after sync

	s.scratchHead = s.progress.Subchains[0].Tail - 1 // tail must not be 0!

	// A.14c: removed the A.14b scratchHead clamp here. With Fix 1 (linked()
	// fires AT or BELOW the floor) the existing errSyncLinked floor-exit in
	// the responses case below handles termination properly. The clamp here
	// caused cold-start to skip the walk-back since Tail-1 (~103M) was always
	// above the floor — but combined with the broken linked() it also masked
	// the bug by making sync() exit on the first iteration.

	// If the sync is already done, resume the backfiller. When the loop stops,
	// terminate the backfiller too.
	linked := len(s.progress.Subchains) == 1 && s.linked(s.scratchHead, s.progress.Subchains[0].Next)
	if linked {
		s.filler.resume()
	}
	defer func() {
		// The filler needs to be suspended, but since it can block for a while
		// when there are many blocks queued up for full-sync importing, run it
		// on a separate goroutine and consume head messages that need instant
		// replies.
		done := make(chan struct{})
		go func() {
			defer close(done)
			filled := s.filler.suspend()
			if filled == nil {
				log.Warn("Latest filled block is not available")
				return
			}
			// If something was filled, try to delete stale sync helpers. If
			// unsuccessful, warn the user, but not much else we can do (it's
			// a programming error, just let users report an issue and don't
			// choke in the meantime).
			if err := s.cleanStales(filled); err != nil {
				log.Error("Failed to clean stale beacon headers", "err", err)
			}
		}()
		// Wait for the suspend to finish, consuming head events in the meantime
		// and dropping them on the floor.
		for {
			select {
			case <-done:
				return
			case event := <-s.headEvents:
				event.errc <- errors.New("beacon syncer reorging")
			}
		}
	}()
	// Create a set of unique channels for this sync cycle. We need these to be
	// ephemeral so a data race doesn't accidentally deliver something stale on
	// a persistent channel across syncs (yup, this happened)
	var (
		requestFails = make(chan *headerRequest)
		responses    = make(chan *headerResponse)
	)
	cancel := make(chan struct{})
	defer close(cancel)

	log.Debug("Starting reverse header sync cycle", "head", head.Number, "hash", head.Hash(), "cont", s.scratchHead)

	// Whether sync completed or not, disregard any future packets
	defer func() {
		log.Debug("Terminating reverse header sync cycle", "head", head.Number, "hash", head.Hash(), "cont", s.scratchHead)
		s.requests = make(map[uint64]*headerRequest)
	}()

	// Start tracking idle peers for task assignments
	peering := make(chan *peeringEvent, 64) // arbitrary buffer, just some burst protection

	peeringSub := s.peers.SubscribeEvents(peering)
	defer peeringSub.Unsubscribe()

	// A.50 (refs #857): seed s.idles from the full peerSet — INCLUDING any
	// XDPOS2 (version=100) peers that were registered before this Sync cycle
	// started. The peerSet is shared with the faststatesync loop and is
	// populated by eth/handler.go:324 for every eth-protocol peer regardless
	// of subprotocol version. Filtering by version here would silently exclude
	// the entire XDC mainnet peer fleet from skeleton header download.
	// Regression test: TestSkeletonAcceptsXDPOS2Peers in skeleton_xdpos2_test.go.
	s.idles = make(map[string]*peerConnection)
	var xdpos2Count, ethCount int
	for _, peer := range s.peers.AllPeers() {
		s.idles[peer.id] = peer
		if peer.version == eth.XDPOS2 {
			xdpos2Count++
		} else {
			ethCount++
		}
	}
	if total := xdpos2Count + ethCount; total > 0 {
		log.Info("Skeleton peer pool seeded", "total", total, "xdpos2", xdpos2Count, "eth", ethCount)
	}

	// A.11 (refs #857): pick the per-cycle request batch size from the seeded
	// peer fleet. If ANY peer in the fleet is XDPOS2 (XDC mainnet/testnet),
	// the cycle uses requestHeadersXDPOS2 (=192) so requests respect XDPoSChain
	// peers' MaxHeaderFetch server-side cap. ETH peers serve up to 1024 so
	// requesting 192 from them is correct, just slightly less throughput. In a
	// pure eth/66+ peer fleet we keep the legacy 512 to preserve throughput.
	// This MUST be set before s.scratchOwners is allocated (chunking must
	// match the request size every executeTask will use this cycle) and before
	// assignTasks runs.
	if xdpos2Count > 0 {
		s.cycleBatch = requestHeadersXDPOS2
	}
	s.scratchOwners = make([]string, scratchHeaders/s.cycleBatch)
	log.Debug("Skeleton cycle batch chosen",
		"batch", s.cycleBatch,
		"slots", len(s.scratchOwners),
		"xdpos2_peers", xdpos2Count,
		"eth_peers", ethCount)
	// Notify any tester listening for startup events
	if s.syncStarting != nil {
		s.syncStarting()
	}
	for {
		// Something happened, try to assign new tasks to any idle peers.
		// A.14c: reverted the A.14b extra floor gate here. With Fix 1
		// (linked() fires AT or BELOW the floor) `linked` already captures
		// the floor-reached state and the errSyncLinked exit below in the
		// responses case terminates the cycle.
		if !linked {
			s.assignTasks(responses, requestFails, cancel)
		}
		// Wait for something to happen
		select {
		case event := <-peering:
			// A peer joined or left, the tasks queue and allocations need to be
			// checked for potential assignment or reassignment
			peerid := event.peer.id
			if event.join {
				log.Debug("Joining skeleton peer", "id", peerid)
				s.idles[peerid] = event.peer
			} else {
				log.Debug("Leaving skeleton peer", "id", peerid)
				s.revertRequests(peerid)
				delete(s.idles, peerid)
				// XDC fix #131: clear empty-response counter for departed peer
				if s.emptyResponseCount != nil {
					delete(s.emptyResponseCount, peerid)
				}
			}

		case errc := <-s.terminate:
			errc <- nil
			return nil, errTerminated

		case event := <-s.headEvents:
			// New head was announced, try to integrate it. If successful, nothing
			// needs to be done as the head simply extended the last range. For now
			// we don't seamlessly integrate reorgs to keep things simple. If the
			// network starts doing many mini reorgs, it might be worthwhile handling
			// a limited depth without an error.
			if err := s.processNewHead(event.header, event.final); err != nil {
				// If a reorg is needed, and we're forcing the new head, signal
				// the syncer to tear down and start over. Otherwise, drop the
				// non-force reorg.
				if event.force {
					event.errc <- nil // forced head reorg accepted
					log.Info("Restarting sync cycle", "reason", err)
					return event.header, errSyncReorged
				}
				event.errc <- err
				continue
			}
			event.errc <- nil // head extension accepted

			// New head was integrated into the skeleton chain. If the backfiller
			// is still running, it will pick it up. If it already terminated,
			// a new cycle needs to be spun up.
			if linked {
				if len(s.progress.Subchains) == 1 && s.linked(s.scratchHead, s.progress.Subchains[0].Next) {
					// The skeleton chain has been extended and is still linked with the local
					// chain, try to re-schedule the backfiller if it's already terminated.
					s.filler.resume()
				} else {
					// The skeleton chain is no longer linked to the local chain for some reason
					// (e.g. debug_setHead was used to trim the local chain). Re-schedule the
					// skeleton sync to fill the chain gap.
					log.Warn("Local chain has been trimmed", "tailnumber", s.scratchHead, "tailhash", s.progress.Subchains[0].Next)
					return nil, errSyncTrimmed
				}
			}

		case req := <-requestFails:
			s.revertRequest(req)

		case res := <-responses:
			// Process the batch of headers. If though processing we managed to
			// link the current subchain to a previously downloaded one, abort the
			// sync and restart with the merged subchains.
			//
			// If we managed to link to the existing local chain or genesis block,
			// abort sync altogether.
			linked, merged := s.processResponse(res)
			if linked {
				log.Debug("Beacon sync linked to local chain")
				return nil, errSyncLinked
			}
			if merged {
				log.Debug("Beacon sync merged subchains")
				return nil, errSyncMerged
			}
			// A.14: operator-pinned terminate floor — exit walk-back as soon as
			// the active subchain tail reaches floor+1 (i.e. there is nothing
			// useful left to walk back to since the operator is the trust
			// anchor for everything below the floor).
			if s.terminateFloor != 0 && len(s.progress.Subchains) > 0 && s.progress.Subchains[0].Tail <= s.terminateFloor+1 {
				log.Info("skeleton: terminate floor reached — exiting walk-back",
					"tail", s.progress.Subchains[0].Tail,
					"floor", s.terminateFloor)
				return nil, errSyncLinked
			}
			// We still have work to do, loop and repeat
		}
	}
}

// initSync attempts to get the skeleton sync into a consistent state wrt any
// past state on disk and the newly requested head to sync to. If the new head
// is nil, the method will return and continue from the previous head.
func (s *skeleton) initSync(head *types.Header) {
	// Extract the head number, we'll need it all over
	number := head.Number.Uint64()

	// Retrieve the previously saved sync progress
	if status := rawdb.ReadSkeletonSyncStatus(s.db); len(status) > 0 {
		s.progress = new(skeletonProgress)
		if err := json.Unmarshal(status, s.progress); err != nil {
			log.Error("Failed to decode skeleton sync status", "err", err)
		} else {
			// Previous sync was available, print some continuation logs
			for _, subchain := range s.progress.Subchains {
				log.Debug("Restarting skeleton subchain", "head", subchain.Head, "tail", subchain.Tail)
			}
			// Create a new subchain for the head (unless the last can be extended),
			// trimming anything it would overwrite
			headchain := &subchain{
				Head: number,
				Tail: number,
				Next: head.ParentHash,
			}
			for len(s.progress.Subchains) > 0 {
				// If the last chain is above the new head, delete altogether
				lastchain := s.progress.Subchains[0]
				if lastchain.Tail >= headchain.Tail {
					log.Debug("Dropping skeleton subchain", "head", lastchain.Head, "tail", lastchain.Tail)
					s.progress.Subchains = s.progress.Subchains[1:]
					continue
				}
				// Otherwise truncate the last chain if needed and abort trimming
				if lastchain.Head >= headchain.Tail {
					log.Debug("Trimming skeleton subchain", "oldhead", lastchain.Head, "newhead", headchain.Tail-1, "tail", lastchain.Tail)
					lastchain.Head = headchain.Tail - 1
				}
				break
			}
			// If the last subchain can be extended, we're lucky. Otherwise, create
			// a new subchain sync task.
			var extended bool
			if n := len(s.progress.Subchains); n > 0 {
				lastchain := s.progress.Subchains[0]
				if lastchain.Head == headchain.Tail-1 {
					lasthead := rawdb.ReadSkeletonHeader(s.db, lastchain.Head)
					if lasthead.Hash() == head.ParentHash {
						log.Debug("Extended skeleton subchain with new head", "head", headchain.Tail, "tail", lastchain.Tail)
						lastchain.Head = headchain.Tail
						extended = true
					}
				}
			}
			if !extended {
				log.Debug("Created new skeleton subchain", "head", number, "tail", number)
				s.progress.Subchains = append([]*subchain{headchain}, s.progress.Subchains...)
			}
			// Update the database with the new sync stats and insert the new
			// head header. We won't delete any trimmed skeleton headers since
			// those will be outside the index space of the many subchains and
			// the database space will be reclaimed eventually when processing
			// blocks above the current head (TODO(karalabe): don't forget).
			batch := s.db.NewBatch()

			rawdb.WriteSkeletonHeader(batch, head)
			s.saveSyncStatus(batch)

			if err := batch.Write(); err != nil {
				log.Crit("Failed to write skeleton sync status", "err", err)
			}
			return
		}
	}
	// Either we've failed to decode the previous state, or there was none. Start
	// a fresh sync with a single subchain represented by the currently sent
	// chain head.
	s.progress = &skeletonProgress{
		Subchains: []*subchain{
			{
				Head: number,
				Tail: number,
				Next: head.ParentHash,
			},
		},
	}
	batch := s.db.NewBatch()

	rawdb.WriteSkeletonHeader(batch, head)
	s.saveSyncStatus(batch)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write initial skeleton sync status", "err", err)
	}
	log.Debug("Created initial skeleton subchain", "head", number, "tail", number)
}

// saveSyncStatus marshals the remaining sync tasks into leveldb.
func (s *skeleton) saveSyncStatus(db ethdb.KeyValueWriter) {
	status, err := json.Marshal(s.progress)
	if err != nil {
		panic(err) // This can only fail during implementation
	}
	rawdb.WriteSkeletonSyncStatus(db, status)
}

// processNewHead does the internal shuffling for a new head marker and either
// accepts and integrates it into the skeleton or requests a reorg. Upon reorg,
// the syncer will tear itself down and restart with a fresh head. It is simpler
// to reconstruct the sync state than to mutate it and hope for the best.
func (s *skeleton) processNewHead(head *types.Header, final *types.Header) error {
	// If a new finalized block was announced, update the sync process independent
	// of what happens with the sync head below
	if final != nil {
		if number := final.Number.Uint64(); s.progress.Finalized == nil || *s.progress.Finalized != number {
			s.progress.Finalized = new(uint64)
			*s.progress.Finalized = final.Number.Uint64()

			s.saveSyncStatus(s.db)
		}
	}
	// If the header cannot be inserted without interruption, return an error for
	// the outer loop to tear down the skeleton sync and restart it
	number := head.Number.Uint64()

	lastchain := s.progress.Subchains[0]
	if lastchain.Tail >= number {
		// If the chain is down to a single beacon header, and it is re-announced
		// once more, ignore it instead of tearing down sync for a noop.
		if lastchain.Head == lastchain.Tail {
			if current := rawdb.ReadSkeletonHeader(s.db, number); current.Hash() == head.Hash() {
				return nil
			}
		}
		// Not a noop / double head announce, abort with a reorg
		return fmt.Errorf("%w, tail: %d, head: %d, newHead: %d", errChainReorged, lastchain.Tail, lastchain.Head, number)
	}
	// Terminate the sync if the chain head is gapped
	if lastchain.Head+1 < number {
		return fmt.Errorf("%w, head: %d, newHead: %d", errChainGapped, lastchain.Head, number)
	}
	// Ignore the duplicated beacon header announcement
	if lastchain.Head == number {
		local := rawdb.ReadSkeletonHeader(s.db, number)
		if local != nil && local.Hash() == head.Hash() {
			log.Debug("Ignored the identical beacon header", "number", number, "hash", local.Hash())
			return nil
		}
	}
	// Terminate the sync if the chain head is forked
	if parent := rawdb.ReadSkeletonHeader(s.db, number-1); parent.Hash() != head.ParentHash {
		return fmt.Errorf("%w, ancestor: %d, hash: %s, want: %s", errChainForked, number-1, parent.Hash(), head.ParentHash)
	}
	// New header seems to be in the last subchain range. Unwind any extra headers
	// from the chain tip and insert the new head. We won't delete any trimmed
	// skeleton headers since those will be outside the index space of the many
	// subchains and the database space will be reclaimed eventually when processing
	// blocks above the current head (TODO(karalabe): don't forget).
	batch := s.db.NewBatch()

	rawdb.WriteSkeletonHeader(batch, head)
	lastchain.Head = number
	s.saveSyncStatus(batch)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write skeleton sync status", "err", err)
	}
	log.Debug("Extended beacon header chain", "number", head.Number, "hash", head.Hash())
	return nil
}

// assignTasks attempts to match idle peers to pending header retrievals.
func (s *skeleton) assignTasks(success chan *headerResponse, fail chan *headerRequest, cancel chan struct{}) {
	// Sort the peers by download capacity to use faster ones if many available
	idlers := &peerCapacitySort{
		peers: make([]*peerConnection, 0, len(s.idles)),
		caps:  make([]int, 0, len(s.idles)),
	}
	targetTTL := s.peers.rates.TargetTimeout()
	for _, peer := range s.idles {
		idlers.peers = append(idlers.peers, peer)
		idlers.caps = append(idlers.caps, s.peers.rates.Capacity(peer.id, eth.BlockHeadersMsg, targetTTL))
	}
	if len(idlers.peers) == 0 {
		return
	}
	sort.Sort(idlers)

	// Find header regions not yet downloading and fill them
	for task, owner := range s.scratchOwners {
		// If we're out of idle peers, stop assigning tasks
		if len(idlers.peers) == 0 {
			return
		}
		// Skip any tasks already filling
		if owner != "" {
			continue
		}
		// If we've reached the genesis, stop assigning tasks.
		// A.11 (refs #857): use s.cycleBatch — the per-cycle request size — not
		// the package default. For XDPOS2 cycles this is 192; for eth/66+ it is
		// 512.
		if uint64(task*s.cycleBatch) >= s.scratchHead {
			return
		}
		// Found a task and have peers available, assign it
		idle := idlers.peers[0]

		idlers.peers = idlers.peers[1:]
		idlers.caps = idlers.caps[1:]

		// Matched a pending task to an idle peer, allocate a unique request id
		var reqid uint64
		for {
			reqid = uint64(rand.Int63())
			if reqid == 0 {
				continue
			}
			if _, ok := s.requests[reqid]; ok {
				continue
			}
			break
		}
		// Generate the network query and send it to the peer
		req := &headerRequest{
			peer:    idle.id,
			id:      reqid,
			deliver: success,
			revert:  fail,
			cancel:  cancel,
			stale:   make(chan struct{}),
			head:    s.scratchHead - uint64(task*s.cycleBatch), // A.11 (refs #857)
			batch:   s.cycleBatch,                              // A.58 Day 2 (refs #857)
		}
		s.requests[reqid] = req
		delete(s.idles, idle.id)

		// Generate the network query and send it to the peer
		go s.executeTask(idle, req)

		// Inject the request into the task to block further assignments
		s.scratchOwners[task] = idle.id
	}
}

// executeTask executes a single fetch request, blocking until either a result
// arrives or a timeouts / cancellation is triggered. The method should be run
// on its own goroutine and will deliver on the requested channels.
func (s *skeleton) executeTask(peer *peerConnection, req *headerRequest) {
	start := time.Now()
	resCh := make(chan *eth.Response)

	// Figure out how many headers to fetch. Usually this will be a full batch,
	// but for the very tail of the chain, trim the request to the number left.
	// Since nodes may or may not return the genesis header for a batch request,
	// don't even request it. The parent hash of block #1 is enough to link.
	//
	// A.11 (refs #857): use s.cycleBatch — the per-cycle, peer-fleet-aware
	// request size — so XDPOS2 peers receive an amount=192 request that fits
	// inside their MaxHeaderFetch serve cap, and modern eth/66+ peers in a
	// pure-ETH fleet still get amount=512.
	//
	// A.58 Day 2 (refs #857): the batch the validator compares against is
	// pinned on req.batch at emit time (assignTasks). Reading it back from
	// req here, rather than re-reading s.cycleBatch, keeps the in-flight
	// request and its eventual validation locked to the same size even if
	// s.cycleBatch is reassigned mid-cycle by some future change. The two
	// values are identical today.
	batch := req.batch
	requestCount := batch
	if req.head < uint64(batch) {
		requestCount = int(req.head)
	}
	peer.log.Info("Fetching skeleton headers", "from", req.head, "count", requestCount, "batch", batch, "peer", shortPeerID(peer.id))
	netreq, err := peer.peer.RequestHeadersByNumber(req.head, requestCount, 0, true, resCh)
	if err != nil {
		peer.log.Trace("Failed to request headers", "err", err)
		s.scheduleRevertRequest(req)
		return
	}
	defer netreq.Close()

	// Wait until the response arrives, the request is cancelled or times out
	ttl := s.peers.rates.TargetTimeout()

	timeoutTimer := time.NewTimer(ttl)
	defer timeoutTimer.Stop()

	select {
	case <-req.cancel:
		peer.log.Debug("Header request cancelled")
		s.scheduleRevertRequest(req)

	case <-timeoutTimer.C:
		// Header retrieval timed out, update the metrics
		peer.log.Warn("Header request timed out, dropping peer", "elapsed", ttl)
		headerTimeoutMeter.Mark(1)
		s.peers.rates.Update(peer.id, eth.BlockHeadersMsg, 0, 0)
		s.scheduleRevertRequest(req)

		// At this point we either need to drop the offending peer, or we need a
		// mechanism to allow waiting for the response and not cancel it. For now
		// lets go with dropping since the header sizes are deterministic and the
		// beacon sync runs exclusive (downloader is idle) so there should be no
		// other load to make timeouts probable. If we notice that timeouts happen
		// more often than we'd like, we can introduce a tracker for the requests
		// gone stale and monitor them. However, in that case too, we need a way
		// to protect against malicious peers never responding, so it would need
		// a second, hard-timeout mechanism.
		s.drop(peer.id)

	case res := <-resCh:
		// Headers successfully retrieved, update the metrics
		headers := *res.Res.(*eth.BlockHeadersRequest)

		headerReqTimer.Update(time.Since(start))
		s.peers.rates.Update(peer.id, eth.BlockHeadersMsg, res.Time, len(headers))

		// Cross validate the headers with the requests
		switch {
		case len(headers) == 0:
			// No headers were delivered, reject the response and reschedule.
			// XDC fix #131: track consecutive empty responses and drop peer
			// after maxEmptyResponses to unblock snap sync skeleton stall.
			//
			// A.69 (refs #844 #859): peer-starvation bypass. When the peer
			// fleet is at or below skeletonEmptyDropPeerFloor, do NOT drop
			// the peer even after maxEmptyResponses — losing the only header-
			// serving peer leaves the skeleton with nowhere to recover from.
			// We still bump + log the counter for visibility, and keep
			// rescheduling so the peer gets another chance. On a healthier
			// fleet (count > floor) the original drop semantics apply.
			const maxEmptyResponses = 5
			s.emptyResponseCount[peer.id]++
			count := s.emptyResponseCount[peer.id]
			totalIdles := len(s.idles)
			peerStarved := totalIdles <= skeletonEmptyDropPeerFloor
			if count >= maxEmptyResponses && !peerStarved {
				peer.log.Warn("Skeleton: dropping peer after repeated empty header responses",
					"peer", peer.id, "consecutive", count, "idles", totalIdles)
				s.emptyResponseCount[peer.id] = 0
				res.Done <- errors.New("no headers delivered")
				s.scheduleRevertRequest(req)
				s.drop(peer.id)
			} else {
				if count >= maxEmptyResponses && peerStarved {
					peer.log.Warn("Skeleton: keeping peer despite empties (peer-starved)",
						"peer", peer.id, "consecutive", count, "idles", totalIdles)
					s.emptyResponseCount[peer.id] = 0 // give the peer a fresh budget
				} else {
					peer.log.Debug("No headers delivered (will retry)", "consecutive", count)
				}
				res.Done <- errors.New("no headers delivered")
				s.scheduleRevertRequest(req)
			}

		case headers[0].Number.Uint64() != req.head:
			// Header batch anchored at non-requested number
			peer.log.Debug("Invalid header response head", "have", headers[0].Number, "want", req.head)
			res.Done <- errors.New("invalid header batch anchor")
			s.scheduleRevertRequest(req)

		case req.head >= uint64(batch) && len(headers) != batch:
			// Invalid number of non-genesis headers delivered, reject the response and reschedule.
			// A.11 (refs #857): validate against the per-cycle batch size, not
			// the constant 512. XDPOS2 peers honestly cap at 192 server-side
			// (XDPoSChain/eth/handler.go:445 MaxHeaderFetch); rejecting their
			// 192-header replies with want=512 stalled the skeleton indefinitely.
			peer.log.Debug("Invalid non-genesis header count", "have", len(headers), "want", batch)
			res.Done <- errors.New("not enough non-genesis headers delivered")
			s.scheduleRevertRequest(req)

		case req.head < uint64(batch) && uint64(len(headers)) != req.head:
			// Invalid number of genesis headers delivered, reject the response and reschedule
			peer.log.Debug("Invalid genesis header count", "have", len(headers), "want", headers[0].Number.Uint64())
			res.Done <- errors.New("not enough genesis headers delivered")
			s.scheduleRevertRequest(req)

		default:
			// Packet seems structurally valid, check hash progression and if it
			// is correct too, deliver for storage
			for i := 0; i < len(headers)-1; i++ {
				if headers[i].ParentHash != headers[i+1].Hash() {
					peer.log.Debug("Invalid hash progression", "index", i, "wantparenthash", headers[i].ParentHash, "haveparenthash", headers[i+1].Hash())
					res.Done <- errors.New("invalid hash progression")
					s.scheduleRevertRequest(req)
					return
				}
			}
			// Hash chain is valid. The delivery might still be junk as we're
			// downloading batches concurrently (so no way to link the headers
			// until gaps are filled); in that case, we'll nuke the peer when
			// we detect the fault.
			res.Done <- nil

			select {
			case req.deliver <- &headerResponse{
				peer:    peer,
				reqid:   req.id,
				headers: headers,
			}:
			case <-req.cancel:
			}
		}
	}
}

// revertRequests locates all the currently pending requests from a particular
// peer and reverts them, rescheduling for others to fulfill.
func (s *skeleton) revertRequests(peer string) {
	// Gather the requests first, revertals need the lock too
	var requests []*headerRequest
	for _, req := range s.requests {
		if req.peer == peer {
			requests = append(requests, req)
		}
	}
	// Revert all the requests matching the peer
	for _, req := range requests {
		s.revertRequest(req)
	}
}

// scheduleRevertRequest asks the event loop to clean up a request and return
// all failed retrieval tasks to the scheduler for reassignment.
func (s *skeleton) scheduleRevertRequest(req *headerRequest) {
	select {
	case req.revert <- req:
		// Sync event loop notified
	case <-req.cancel:
		// Sync cycle got cancelled
	case <-req.stale:
		// Request already reverted
	}
}

// revertRequest cleans up a request and returns all failed retrieval tasks to
// the scheduler for reassignment.
//
// Note, this needs to run on the event runloop thread to reschedule to idle peers.
// On peer threads, use scheduleRevertRequest.
func (s *skeleton) revertRequest(req *headerRequest) {
	log.Trace("Reverting header request", "peer", req.peer, "reqid", req.id)
	select {
	case <-req.stale:
		log.Trace("Header request already reverted", "peer", req.peer, "reqid", req.id)
		return
	default:
	}
	close(req.stale)

	// Remove the request from the tracked set
	delete(s.requests, req.id)

	// Remove the request from the tracked set and mark the task as not-pending,
	// ready for rescheduling.
	// A.11 (refs #857): use s.cycleBatch for the chunk index so XDPOS2 cycles
	// (cycleBatch=192) recover the correct task slot. Mixing batch sizes within
	// a single cycle is disallowed by sync()'s startup logic, so cycleBatch is
	// stable for the lifetime of every request flowing through this path.
	s.scratchOwners[(s.scratchHead-req.head)/uint64(s.cycleBatch)] = ""
}

// mergeSubchains is invoked once certain beacon headers have been persisted locally
// and the subchains should be merged in case there are some overlaps between. An
// indicator will be returned if the last subchain is merged with previous subchain.
func (s *skeleton) mergeSubchains() bool {
	// If the subchain extended into the next subchain, we need to handle
	// the overlap. Since there could be many overlaps, do this in a loop.
	var merged bool
	for len(s.progress.Subchains) > 1 && s.progress.Subchains[1].Head >= s.progress.Subchains[0].Tail {
		// Extract some stats from the second subchain
		head := s.progress.Subchains[1].Head
		tail := s.progress.Subchains[1].Tail
		next := s.progress.Subchains[1].Next

		// Since we just overwrote part of the next subchain, we need to trim
		// its head independent of matching or mismatching content
		if s.progress.Subchains[1].Tail >= s.progress.Subchains[0].Tail {
			// Fully overwritten, get rid of the subchain as a whole
			log.Debug("Previous subchain fully overwritten", "head", head, "tail", tail, "next", next)
			s.progress.Subchains = append(s.progress.Subchains[:1], s.progress.Subchains[2:]...)
			continue
		} else {
			// Partially overwritten, trim the head to the overwritten size
			log.Debug("Previous subchain partially overwritten", "head", head, "tail", tail, "next", next)
			s.progress.Subchains[1].Head = s.progress.Subchains[0].Tail - 1
		}
		// If the old subchain is an extension of the new one, merge the two
		// and let the skeleton syncer restart (to clean internal state)
		if rawdb.ReadSkeletonHeader(s.db, s.progress.Subchains[1].Head).Hash() == s.progress.Subchains[0].Next {
			log.Debug("Previous subchain merged", "head", head, "tail", tail, "next", next)
			s.progress.Subchains[0].Tail = s.progress.Subchains[1].Tail
			s.progress.Subchains[0].Next = s.progress.Subchains[1].Next

			s.progress.Subchains = append(s.progress.Subchains[:1], s.progress.Subchains[2:]...)
			merged = true
		}
	}
	return merged
}

func (s *skeleton) processResponse(res *headerResponse) (linked bool, merged bool) {
	res.peer.log.Trace("Processing header response", "head", res.headers[0].Number, "hash", res.headers[0].Hash(), "count", len(res.headers))

	// Whether the response is valid, we can mark the peer as idle and notify
	// the scheduler to assign a new task. If the response is invalid, we'll
	// drop the peer in a bit.
	s.idles[res.peer.id] = res.peer

	// XDC fix #131: successful header delivery — reset empty-response counter
	if s.emptyResponseCount != nil {
		delete(s.emptyResponseCount, res.peer.id)
	}

	// Ensure the response is for a valid request
	if _, ok := s.requests[res.reqid]; !ok {
		// Some internal accounting is broken. A request either times out or it
		// gets fulfilled successfully. It should not be possible to deliver a
		// response to a non-existing request.
		res.peer.log.Error("Unexpected header packet")
		return false, false
	}
	delete(s.requests, res.reqid)

	// Insert the delivered headers into the scratch space independent of the
	// content or continuation; those will be validated in a moment
	head := res.headers[0].Number.Uint64()
	copy(s.scratchSpace[s.scratchHead-head:], res.headers)

	// If there's still a gap in the head of the scratch space, abort
	if s.scratchSpace[0] == nil {
		return false, false
	}
	// Try to consume any head headers, validating the boundary conditions
	batch := s.db.NewBatch()
	for s.scratchSpace[0] != nil {
		// Next batch of headers available, cross-reference with the subchain
		// we are extending and either accept or discard
		if s.progress.Subchains[0].Next != s.scratchSpace[0].Hash() {
			// Print a log messages to track what's going on
			tail := s.progress.Subchains[0].Tail
			want := s.progress.Subchains[0].Next
			have := s.scratchSpace[0].Hash()

			log.Warn("Invalid skeleton headers", "peer", s.scratchOwners[0], "number", tail-1, "want", want, "have", have)

			// The peer delivered junk, or at least not the subchain we are
			// syncing to. Free up the scratch space and assignment, reassign
			// and drop the original peer.
			// A.11 (refs #857): clear only the cycle's batch slot, not the
			// hard-coded 512.
			for i := 0; i < s.cycleBatch; i++ {
				s.scratchSpace[i] = nil
			}
			s.drop(s.scratchOwners[0])
			s.scratchOwners[0] = ""
			break
		}
		// Scratch delivery matches required subchain, deliver the batch of
		// headers and push the subchain forward.
		// A.11 (refs #857): iterate over the cycle's batch slot, not the
		// hard-coded 512.
		var consumed int
		for _, header := range s.scratchSpace[:s.cycleBatch] {
			if header != nil { // nil when the genesis is reached
				consumed++

				rawdb.WriteSkeletonHeader(batch, header)
				s.pulled++

				s.progress.Subchains[0].Tail--
				s.progress.Subchains[0].Next = header.ParentHash

				// If we've reached an existing block in the chain, stop retrieving
				// headers. Note, if we want to support light clients with the same
				// code we'd need to switch here based on the downloader mode. That
				// said, there's no such functionality for now, so don't complicate.
				//
				// In the case of full sync it would be enough to check for the body,
				// but even a full syncing node will generate a receipt once block
				// processing is done, so it's just one more "needless" check.
				//
				// The weird cascading checks are done to minimize the database reads.
				linked = s.linked(header.Number.Uint64()-1, header.ParentHash)
				if linked {
					log.Debug("Primary subchain linked", "number", header.Number.Uint64()-1, "hash", header.ParentHash)
					break
				}
				// A.14b: defense-in-depth — treat crossing the operator-pinned
				// floor as linked so we never decrement Tail past it inside this
				// inner consumption loop.
				if s.terminateFloor != 0 && header.Number.Uint64() <= s.terminateFloor+1 {
					log.Debug("skeleton: floor reached in processResponse — treating as linked",
						"number", header.Number.Uint64(), "floor", s.terminateFloor)
					linked = true
					break
				}
			}
		}
		head := s.progress.Subchains[0].Head
		tail := s.progress.Subchains[0].Tail
		next := s.progress.Subchains[0].Next

		log.Trace("Primary subchain extended", "head", head, "tail", tail, "next", next)

		// If the beacon chain was linked to the local chain, completely swap out
		// all internal progress and abort header synchronization.
		if linked {
			// Merge all overlapped subchains beforehand
			s.mergeSubchains()

			// Linking into the local chain should also mean that there are no
			// leftover subchains, but in the case of importing the blocks via
			// the engine API, we will not push the subchains forward. This will
			// lead to a gap between an old sync cycle and a future one.
			if subchains := len(s.progress.Subchains); subchains > 1 {
				switch {
				// If there are only 2 subchains - the current one and an older
				// one - and the old one consists of a single block, then it's
				// the expected new sync cycle after some propagated blocks. Log
				// it for debugging purposes, explicitly clean and don't escalate.
				case subchains == 2 && s.progress.Subchains[1].Head == s.progress.Subchains[1].Tail:
					// Remove the leftover skeleton header associated with old
					// skeleton chain only if it's not covered by the current
					// skeleton range.
					if s.progress.Subchains[1].Head < s.progress.Subchains[0].Tail {
						log.Debug("Cleaning previous beacon sync state", "head", s.progress.Subchains[1].Head)
						rawdb.DeleteSkeletonHeader(batch, s.progress.Subchains[1].Head)
					}
					// Drop the leftover skeleton chain since it's stale.
					s.progress.Subchains = s.progress.Subchains[:1]

				// If we have more than one header or more than one leftover chain,
				// the syncer's internal state is corrupted. Do try to fix it, but
				// be very vocal about the fault.
				default:
					var context []interface{}

					for i := range s.progress.Subchains[1:] {
						context = append(context, fmt.Sprintf("stale_head_%d", i+1))
						context = append(context, s.progress.Subchains[i+1].Head)
						context = append(context, fmt.Sprintf("stale_tail_%d", i+1))
						context = append(context, s.progress.Subchains[i+1].Tail)
						context = append(context, fmt.Sprintf("stale_next_%d", i+1))
						context = append(context, s.progress.Subchains[i+1].Next)
					}
					log.Error("Cleaning spurious beacon sync leftovers", context...)
					s.progress.Subchains = s.progress.Subchains[:1]

					// Note, here we didn't actually delete the headers at all,
					// just the metadata. We could implement a cleanup mechanism,
					// but further modifying corrupted state is kind of asking
					// for it. Unless there's a good enough reason to risk it,
					// better to live with the small database junk.
				}
			}
			break
		}
		// Batch of headers consumed, shift the download window forward.
		// A.11 (refs #857): shift by s.cycleBatch (192 for XDPOS2 cycles, 512
		// for eth/66+ cycles) and clear that many slots at the tail. The
		// scratchOwners table is sized scratchHeaders/cycleBatch so the last
		// slot index is len(s.scratchOwners)-1.
		copy(s.scratchSpace, s.scratchSpace[s.cycleBatch:])
		for i := 0; i < s.cycleBatch; i++ {
			s.scratchSpace[scratchHeaders-i-1] = nil
		}
		copy(s.scratchOwners, s.scratchOwners[1:])
		s.scratchOwners[len(s.scratchOwners)-1] = ""

		s.scratchHead -= uint64(consumed)

		// If subchains were merged, all further available headers in the scratch
		// space are invalid since we skipped ahead. Stop processing the scratch
		// space to avoid dropping peers thinking they delivered invalid data.
		merged = s.mergeSubchains()
		if merged {
			break
		}
	}
	s.saveSyncStatus(batch)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write skeleton headers and progress", "err", err)
	}
	// Print a progress report making the UX a bit nicer
	left := s.progress.Subchains[0].Tail - 1
	if linked {
		left = 0
	}
	if time.Since(s.logged) > 8*time.Second || left == 0 {
		s.logged = time.Now()

		if s.pulled == 0 {
			log.Info("Beacon sync starting", "left", left)
		} else {
			eta := float64(time.Since(s.started)) / float64(s.pulled) * float64(left)
			log.Info("Syncing beacon headers", "downloaded", s.pulled, "left", left, "eta", common.PrettyDuration(eta))
		}
	}
	return linked, merged
}

// cleanStales removes previously synced beacon headers that have become stale
// due to the downloader backfilling past the tracked tail.
func (s *skeleton) cleanStales(filled *types.Header) error {
	number := filled.Number.Uint64()
	log.Debug("Cleaning stale beacon headers", "filled", number, "hash", filled.Hash())

	// If the filled header is below the subchain, it means the skeleton is not
	// linked with local chain yet, don't bother to do cleanup.
	if number+1 < s.progress.Subchains[0].Tail {
		log.Debug("filled header below beacon header tail", "filled", number, "tail", s.progress.Subchains[0].Tail)
		return nil
	}
	// If nothing in subchain is filled, don't bother to do cleanup.
	if number+1 == s.progress.Subchains[0].Tail {
		log.Debug("Skeleton chain not yet consumed", "filled", number, "hash", filled.Hash(), "tail", s.progress.Subchains[0].Tail)
		return nil
	}
	// If the latest fill was on a different subchain, it means the backfiller
	// was interrupted before it got to do any meaningful work, no cleanup
	header := rawdb.ReadSkeletonHeader(s.db, filled.Number.Uint64())
	if header == nil {
		log.Debug("Filled header outside of skeleton range", "number", number, "head", s.progress.Subchains[0].Head, "tail", s.progress.Subchains[0].Tail)
		return nil
	} else if header.Hash() != filled.Hash() {
		log.Debug("Filled header on different sidechain", "number", number, "filled", filled.Hash(), "skeleton", header.Hash())
		return nil
	}
	var (
		start uint64
		end   uint64
		batch = s.db.NewBatch()
	)
	if number < s.progress.Subchains[0].Head {
		// The skeleton chain is partially consumed, set the new tail as filled+1.
		tail := rawdb.ReadSkeletonHeader(s.db, number+1)
		if tail == nil {
			return fmt.Errorf("filled header is missing: %d", number+1)
		}
		if tail.ParentHash != filled.Hash() {
			return fmt.Errorf("filled header is discontinuous with subchain: %d %s, please file an issue", number, filled.Hash())
		}
		start, end = s.progress.Subchains[0].Tail, number+1 // remove headers in [tail, filled]
		s.progress.Subchains[0].Tail = tail.Number.Uint64()
		s.progress.Subchains[0].Next = tail.ParentHash
	} else {
		// The skeleton chain is fully consumed, set both head and tail as filled.
		start, end = s.progress.Subchains[0].Tail, filled.Number.Uint64() // remove headers in [tail, filled)
		s.progress.Subchains[0].Tail = filled.Number.Uint64()
		s.progress.Subchains[0].Next = filled.ParentHash

		// If more headers were filled than available, push the entire subchain
		// forward to keep tracking the node's block imports.
		if number > s.progress.Subchains[0].Head {
			end = s.progress.Subchains[0].Head + 1 // delete the entire original range, including the head
			s.progress.Subchains[0].Head = number  // assign a new head (tail is already assigned to this)

			// The entire original skeleton chain was deleted and a new one
			// defined. Make sure the new single-header chain gets pushed to
			// disk to keep internal state consistent.
			rawdb.WriteSkeletonHeader(batch, filled)
		}
	}
	// Execute the trimming and the potential rewiring of the progress
	s.saveSyncStatus(batch)
	for n := start; n < end; n++ {
		// If the batch grew too big, flush it and continue with a new batch.
		// The catch is that the sync metadata needs to reflect the actually
		// flushed state, so temporarily change the subchain progress and
		// revert after the flush.
		if batch.ValueSize() >= ethdb.IdealBatchSize {
			tmpTail := s.progress.Subchains[0].Tail
			tmpNext := s.progress.Subchains[0].Next

			s.progress.Subchains[0].Tail = n
			s.progress.Subchains[0].Next = rawdb.ReadSkeletonHeader(s.db, n).ParentHash
			s.saveSyncStatus(batch)

			if err := batch.Write(); err != nil {
				log.Crit("Failed to write beacon trim data", "err", err)
			}
			batch.Reset()

			s.progress.Subchains[0].Tail = tmpTail
			s.progress.Subchains[0].Next = tmpNext
			s.saveSyncStatus(batch)
		}
		rawdb.DeleteSkeletonHeader(batch, n)
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write beacon trim data", "err", err)
	}
	log.Debug("Cleaned stale beacon headers", "start", start, "end", end)
	return nil
}

// Bounds retrieves the current head and tail tracked by the skeleton syncer
// and optionally the last known finalized header if any was announced and if
// it is still in the sync range. This method is used by the backfiller, whose
// life cycle is controlled by the skeleton syncer.
//
// Note, the method will not use the internal state of the skeleton, but will
// rather blindly pull stuff from the database. This is fine, because the back-
// filler will only run when the skeleton chain is fully downloaded and stable.
// There might be new heads appended, but those are atomic from the perspective
// of this method. Any head reorg will first tear down the backfiller and only
// then make the modification.
func (s *skeleton) Bounds() (head *types.Header, tail *types.Header, final *types.Header, err error) {
	// Read the current sync progress from disk and figure out the current head.
	// Although there's a lot of error handling here, these are mostly as sanity
	// checks to avoid crashing if a programming error happens. These should not
	// happen in live code.
	status := rawdb.ReadSkeletonSyncStatus(s.db)
	if len(status) == 0 {
		return nil, nil, nil, errors.New("beacon sync not yet started")
	}
	progress := new(skeletonProgress)
	if err := json.Unmarshal(status, progress); err != nil {
		return nil, nil, nil, err
	}
	head = rawdb.ReadSkeletonHeader(s.db, progress.Subchains[0].Head)
	if head == nil {
		return nil, nil, nil, fmt.Errorf("head skeleton header %d is missing", progress.Subchains[0].Head)
	}
	tail = rawdb.ReadSkeletonHeader(s.db, progress.Subchains[0].Tail)
	if tail == nil {
		return nil, nil, nil, fmt.Errorf("tail skeleton header %d is missing", progress.Subchains[0].Tail)
	}
	if progress.Finalized != nil && tail.Number.Uint64() <= *progress.Finalized && *progress.Finalized <= head.Number.Uint64() {
		final = rawdb.ReadSkeletonHeader(s.db, *progress.Finalized)
		if final == nil {
			return nil, nil, nil, fmt.Errorf("finalized skeleton header %d is missing", *progress.Finalized)
		}
	}
	return head, tail, final, nil
}

// Header retrieves a specific header tracked by the skeleton syncer. This method
// is meant to be used by the backfiller, whose life cycle is controlled by the
// skeleton syncer.
//
// Note, outside the permitted runtimes, this method might return nil results and
// subsequent calls might return headers from different chains.
func (s *skeleton) Header(number uint64) *types.Header {
	return rawdb.ReadSkeletonHeader(s.db, number)
}
