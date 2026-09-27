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

// Contains the active peer-set of the downloader, maintaining both failures
// as well as reputation metrics to prioritize the block retrievals.

package downloader

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/msgrate"
)

const (
	maxLackingHashes = 4096 // Maximum number of entries allowed on the list or lacking items

	// A.40: per-peer blacklist thresholds for PBSS-only peers that answer []
	// to every NodeData request (MarkLacking is hash-keyed, can't catch this).
	nodeDataEmptyBlacklistThreshold = 10
	nodeDataEmptyBlacklistWindow    = 120 * time.Second

	// A.69 (refs #844 #859): peer-starvation bypass for the A.40 sticky-PBSS
	// blacklist. When the total connected peer set is at or below this
	// threshold, NodeDataIdlePeers ignores the blacklist — losing the only
	// peer (legitimately mis-classified as PBSS-only after 10 empty replies
	// during a transient stall) starves state-sync indefinitely with no path
	// to recovery. The risk of returning to a genuinely-PBSS peer is bounded:
	// the assignTasks dispatcher will see the same 0-length responses on the
	// next request and the blacklist counter rewinds back up immediately, so
	// the bypass only helps when the empties were transient (peer was at tip,
	// peer was busy, peer briefly pruned the trie window). On a fleet of >=
	// nodeDataPbssBypassPeerFloor peers the blacklist still hardens against
	// genuine PBSS peers.
	nodeDataPbssBypassPeerFloor = 2

	// A.53: maximum concurrent NodeData reservations per peer. The slot table
	// is the foundation for wire-level multiplexing of GetNodeData (multiple
	// outstanding requests per peer keyed by request-id). Until that lands,
	// the table provides intent-tracking + invariants exercised by tests.
	//
	// A.67: reduced from 16 → 1 to match legacy XDPoSChain's binary-semaphore
	// semantics (XDPoSChain/eth/downloader/peer.go:252). XDPOS2 wire carries
	// no request-id, so multiple in-flight NodeData requests per peer cannot
	// be disambiguated on response — only the FIRST response back is matched
	// to its original req via active[peer.id]; subsequent responses fall into
	// the late-delivery path which routes blobs through the global s.trieTasks
	// fallback. With 16 slots per peer × 8 peers × 384 hashes/req = 49152
	// hashes nominally in flight, the trie scheduler couldn't keep up: 67% of
	// delivered blobs arrived AFTER their originating task had been satisfied
	// by a concurrent slot's response from the same or another peer, getting
	// reclassified as `duplicate` (A.65). Cutting to 1 slot/peer matches the
	// legacy backpressure profile that consistently delivers 1.81M state
	// entries/min with 0% duplicate rate vs modern's 519K/min with 67% dup
	// rate at the same peer count. Refs A.67 / #859.
	maxParallelInflightSlots = 1
)

var (
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
)

// peerConnection represents an active peer from which hashes and blocks are retrieved.
type peerConnection struct {
	id string // Unique identifier of the peer

	rates   *msgrate.Tracker         // Tracker to hone in on the number of items retrievable per second
	lacking map[common.Hash]struct{} // Set of hashes not to request (didn't have previously)

	peer Peer

	version uint       // Eth protocol version number to switch strategies
	log     log.Logger // Contextual logger to add extra infos to peer logs
	lock    sync.RWMutex

	// State sync timing (classical Fast Sync / XDPOS2 NodeData path).
	stateStarted time.Time // Time when the last NodeData request was sent

	// A.27 / A.53-2: informational in-flight indicator for NodeData requests.
	// Originally a binary semaphore (0 = idle, 1 = busy) used by FetchNodeData
	// and NodeDataIdlePeers to enforce one-request-per-peer. A.53/2 retired
	// both gates: parallel inflight is now bounded by the per-peer slot table
	// (see inflightSlots / acquireInflightSlot). stateIdle is retained for
	// diagnostics + drop-path invariants (TestNodeDataDropWhenNoFetcher) and
	// is incremented on dispatch, cleared on every termination path. Treat as
	// "wire requests outstanding to this peer" rather than a gate.
	stateIdle atomic.Int32

	// A.40: sticky PBSS blacklist. Counter tracks consecutive empties within
	// a rolling window; nodeDataPbssLikely, once set, persists for the sync
	// session and NodeDataIdlePeers skips this peer.
	nodeDataEmpties    atomic.Int32
	nodeDataFirstEmpty atomic.Int64 // unix-nano; 0 = no active run
	nodeDataPbssLikely atomic.Bool

	// A.37: proactive peer-version + state-scheme gate. Seeded at peer
	// registration from eth.Peer.RemoteCaps() and never written again, so
	// the downloader doesn't need to plumb caps refresh on every handshake
	// cycle. Sibling to A.40's reactive PBSS blacklist; together they form
	// a "deny known-bad + blacklist suspected-bad" pair.
	remoteSchemePbss atomic.Bool // peer advertised PBSS — cannot serve NodeData
	remoteLegacy     atomic.Bool // peer omitted XDCCaps trailer OR has zero CommitPrefix

	// A.53 / A.53-2: per-peer parallel inflight slot table. Up to
	// maxParallelInflightSlots concurrent NodeData reservations are tracked
	// here. A.53 introduced the table as a "shadow" of stateIdle (intent
	// tracking only). A.53/2 promoted it to the authoritative gate:
	//   - FetchNodeData no longer CAS-checks stateIdle; the slot was already
	//     reserved by faststatesync.assignTasks before dispatch.
	//   - NodeDataIdlePeers filters on inflightCount() < maxParallelInflightSlots
	//     instead of stateIdle == 0.
	// Net effect: peers with 3 HBSS-capable connections + 16 slots each can
	// hold up to 48 outstanding NodeData requests in parallel, instead of the
	// 3 the binary semaphore allowed. The slot table also gives the
	// assignment path a hard "no over-dispatch" invariant exercised by
	// TestParallelInflightNoOverlap. Refs A.53 / A.53-2 / #857.
	inflightLock  sync.Mutex
	inflightSlots [maxParallelInflightSlots]*stateReq
}

// Peer encapsulates the methods required to synchronise with a remote full peer.
type Peer interface {
	RequestHeadersByHash(common.Hash, int, int, bool, chan *eth.Response) (*eth.Request, error)
	RequestHeadersByNumber(uint64, int, int, bool, chan *eth.Response) (*eth.Request, error)

	RequestBodies([]common.Hash, chan *eth.Response) (*eth.Request, error)
	RequestReceipts([]common.Hash, []uint64, []uint64, chan *eth.Response) (*eth.Request, error)

	// RequestNodeData sends a GetNodeData request. Used only on the XDPOS2
	// classical Fast Sync path (Phase A.2 / #844). Returns immediately on error
	// since the caller wraps in a goroutine.
	RequestNodeData([]common.Hash) error
}

// xdcCapsPeer is the optional A.37 capabilities accessor implemented by
// *eth.Peer. The downloader probes it via type assertion in newPeerConnection;
// callers that don't implement it (test fakes) default to legacy/unknown
// behavior, which is conservatively allowed in the NodeData pool (A.40
// reactive blacklist remains the safety net). Refs #844 (A.37).
type xdcCapsPeer interface {
	RemoteStateSchemeHBSS() bool
	RemoteLegacy() bool
}

// newPeerConnection creates a new downloader peer.
func newPeerConnection(id string, version uint, peer Peer, logger log.Logger) *peerConnection {
	pc := &peerConnection{
		id:      id,
		lacking: make(map[common.Hash]struct{}),
		peer:    peer,
		version: version,
		log:     logger,
	}
	// A.37: seed proactive scheme/legacy flags from the peer's advertised
	// XDCCaps trailer (read once at registration; never refreshed). Peers
	// that don't implement xdcCapsPeer (test fakes, snap-only peers) default
	// to "HBSS, not legacy" — the conservative inclusion path. Refs #844.
	if cp, ok := peer.(xdcCapsPeer); ok {
		if !cp.RemoteStateSchemeHBSS() {
			pc.remoteSchemePbss.Store(true)
		}
		if cp.RemoteLegacy() {
			pc.remoteLegacy.Store(true)
		}
	}
	return pc
}

// RemoteSchemePbss / RemoteLegacy expose the A.37 proactive flags. Refs #844.
func (p *peerConnection) RemoteSchemePbss() bool { return p.remoteSchemePbss.Load() }
func (p *peerConnection) RemoteLegacy() bool     { return p.remoteLegacy.Load() }

// Reset clears the internal state of a peer entity.
func (p *peerConnection) Reset() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lacking = make(map[common.Hash]struct{})
}

// UpdateHeaderRate updates the peer's estimated header retrieval throughput with
// the current measurement.
func (p *peerConnection) UpdateHeaderRate(delivered int, elapsed time.Duration) {
	p.rates.Update(eth.BlockHeadersMsg, elapsed, delivered)
}

// UpdateBodyRate updates the peer's estimated body retrieval throughput with the
// current measurement.
func (p *peerConnection) UpdateBodyRate(delivered int, elapsed time.Duration) {
	p.rates.Update(eth.BlockBodiesMsg, elapsed, delivered)
}

// UpdateReceiptRate updates the peer's estimated receipt retrieval throughput
// with the current measurement.
func (p *peerConnection) UpdateReceiptRate(delivered int, elapsed time.Duration) {
	p.rates.Update(eth.ReceiptsMsg, elapsed, delivered)
}

// HeaderCapacity retrieves the peer's header download allowance based on its
// previously discovered throughput.
func (p *peerConnection) HeaderCapacity(targetRTT time.Duration) int {
	cap := p.rates.Capacity(eth.BlockHeadersMsg, targetRTT)
	if cap > MaxHeaderFetch {
		cap = MaxHeaderFetch
	}
	return cap
}

// BodyCapacity retrieves the peer's body download allowance based on its
// previously discovered throughput.
func (p *peerConnection) BodyCapacity(targetRTT time.Duration) int {
	cap := p.rates.Capacity(eth.BlockBodiesMsg, targetRTT)
	if cap > MaxBlockFetch {
		cap = MaxBlockFetch
	}
	return cap
}

// ReceiptCapacity retrieves the peers receipt download allowance based on its
// previously discovered throughput.
func (p *peerConnection) ReceiptCapacity(targetRTT time.Duration) int {
	cap := p.rates.Capacity(eth.ReceiptsMsg, targetRTT)
	if cap > MaxReceiptFetch {
		cap = MaxReceiptFetch
	}
	return cap
}

// FetchNodeData sends a node state data retrieval request to the remote peer.
// Used by classical Fast Sync (XDPOS2 / NodeData path). Records start time for
// RTT tracking.
//
// A.53/2: this used to gate on stateIdle.CompareAndSwap(0, 1), which capped
// each peer at one outstanding NodeData request even though A.53 had already
// introduced a 16-slot parallel inflight table. The CAS was the upstream
// bottleneck: with 3 HBSS peers and 1 inflight each, observed throughput sat
// at ~21 nodes/min vs the ~2000+ that the slot table was designed to support.
// The gate has moved upstream to faststatesync.assignTasks, which acquires a
// slot from inflightSlots before calling this method. Here we only bump the
// informational stateIdle counter so the drop-path invariants still hold.
func (p *peerConnection) FetchNodeData(hashes []common.Hash) error {
	// A.53/2: increment the informational in-flight counter. No early return —
	// the slot-table reservation in faststatesync.assignTasks is the gate.
	p.stateIdle.Add(1)
	p.lock.Lock()
	p.stateStarted = time.Now()
	p.lock.Unlock()

	// A.39: RequestNodeData is fire-and-forget on a goroutine. If the underlying
	// p2p.Send returns an error (peer write failure, broken pipe, conn closed
	// mid-flight) or the goroutine panics, stateIdle would leak and skew
	// diagnostics until the peer is unregistered. Decrement on both failure
	// paths so the counter reflects reality. Sibling fix to A.36
	// (downloader.DeliverNodeData drop path).
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Debug("Panic in RequestNodeData, decrementing stateIdle", "peer", p.id, "panic", r)
				if p.stateIdle.Add(-1) < 0 {
					p.stateIdle.Store(0)
				}
			}
		}()
		if err := p.peer.RequestNodeData(hashes); err != nil {
			log.Debug("Failed to send NodeData request, decrementing stateIdle", "peer", p.id, "err", err)
			if p.stateIdle.Add(-1) < 0 {
				p.stateIdle.Store(0)
			}
		}
	}()
	return nil
}

// SetNodeDataIdle marks one outstanding NodeData request as terminated. The
// delivered number and delivery time are used to update the peer's RTT
// tracker. A.53/2: stateIdle is now an informational counter (number of
// outstanding wire requests, not a binary semaphore) so we decrement rather
// than zero it. The slot table (releaseInflightSlot, called from
// stateReq.finalize) is what actually re-opens dispatch capacity. Called on
// every termination path (success, timeout, peer-drop, spindown).
func (p *peerConnection) SetNodeDataIdle(delivered int, deliveryTime time.Time) {
	p.lock.RLock()
	started := p.stateStarted
	p.lock.RUnlock()
	p.rates.Update(eth.NodeDataMsg, deliveryTime.Sub(started), delivered)
	// Decrement informational counter; clamp at zero so synthetic drop-path
	// callers (DeliverNodeData with no fetcher) that may have been primed
	// without a paired FetchNodeData don't drive it negative.
	if p.stateIdle.Add(-1) < 0 {
		p.stateIdle.Store(0)
	}
}

// RecordNodeDataResponse updates the A.40 PBSS blacklist counters. A non-empty
// response resets the run; an empty one increments and, on threshold-within-
// window, sets the sticky nodeDataPbssLikely flag.
func (p *peerConnection) RecordNodeDataResponse(delivered int) {
	if delivered > 0 {
		p.nodeDataEmpties.Store(0)
		p.nodeDataFirstEmpty.Store(0)
		return
	}
	if p.nodeDataPbssLikely.Load() {
		return
	}
	now := time.Now().UnixNano()
	first := p.nodeDataFirstEmpty.Load()
	if first == 0 || time.Duration(now-first) > nodeDataEmptyBlacklistWindow {
		p.nodeDataFirstEmpty.Store(now)
		p.nodeDataEmpties.Store(1)
		return
	}
	n := p.nodeDataEmpties.Add(1)
	if int(n) >= nodeDataEmptyBlacklistThreshold && p.nodeDataPbssLikely.CompareAndSwap(false, true) {
		log.Info("Peer not serving HBSS NodeData, blacklisting for this sync", "peer", p.id, "empties", n)
	}
}

// acquireInflightSlot reserves a parallel-inflight slot for this peer and
// stores req in it. Returns the slot ticket (0..maxParallelInflightSlots-1)
// for later release and ok=true on success; ok=false means all slots are
// already occupied. A.53 invariant: no two live slots ever hold the same
// *stateReq pointer (enforced by TestParallelInflightNoOverlap).
// Refs A.53 / #857.
func (p *peerConnection) acquireInflightSlot(req *stateReq) (int, bool) {
	p.inflightLock.Lock()
	defer p.inflightLock.Unlock()
	for i := range p.inflightSlots {
		if p.inflightSlots[i] == nil {
			p.inflightSlots[i] = req
			return i, true
		}
	}
	return 0, false
}

// releaseInflightSlot frees the parallel-inflight slot identified by ticket.
// Out-of-range tickets are no-ops so the dispatcher can release defensively
// without panicking on a double-release. Refs A.53 / #857.
func (p *peerConnection) releaseInflightSlot(ticket int) {
	if ticket < 0 || ticket >= maxParallelInflightSlots {
		return
	}
	p.inflightLock.Lock()
	defer p.inflightLock.Unlock()
	p.inflightSlots[ticket] = nil
}

// inflightCount returns the number of currently occupied parallel-inflight
// slots. Used by tests to assert the slot-table invariants and by the
// dispatcher to throttle further reservations. Refs A.53 / #857.
func (p *peerConnection) inflightCount() int {
	p.inflightLock.Lock()
	defer p.inflightLock.Unlock()
	n := 0
	for _, slot := range p.inflightSlots {
		if slot != nil {
			n++
		}
	}
	return n
}

// inflightHasAlias returns true if any *other* slot (i.e. not at index
// ignoreIdx) is currently holding the same *stateReq pointer as req. Test
// support for TestParallelInflightNoOverlap. Refs A.53 / #857.
func (p *peerConnection) inflightHasAlias(ignoreIdx int, req *stateReq) bool {
	p.inflightLock.Lock()
	defer p.inflightLock.Unlock()
	for i, slot := range p.inflightSlots {
		if i == ignoreIdx {
			continue
		}
		if slot == req {
			return true
		}
	}
	return false
}

// NodeDataCapacity retrieves the peers state download allowance based on its
// previously discovered throughput.
func (p *peerConnection) NodeDataCapacity(targetRTT time.Duration) int {
	cap := p.rates.Capacity(eth.NodeDataMsg, targetRTT)
	if cap > MaxStateFetch {
		cap = MaxStateFetch
	}
	return cap
}

// MarkLacking appends a new entity to the set of items (blocks, receipts, states)
// that a peer is known not to have (i.e. have been requested before). If the
// set reaches its maximum allowed capacity, items are randomly dropped off.
func (p *peerConnection) MarkLacking(hash common.Hash) {
	p.lock.Lock()
	defer p.lock.Unlock()

	for len(p.lacking) >= maxLackingHashes {
		for drop := range p.lacking {
			delete(p.lacking, drop)
			break
		}
	}
	p.lacking[hash] = struct{}{}
}

// Lacks retrieves whether the hash of a blockchain item is on the peers lacking
// list (i.e. whether we know that the peer does not have it).
func (p *peerConnection) Lacks(hash common.Hash) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	_, ok := p.lacking[hash]
	return ok
}

// peeringEvent is sent on the peer event feed when a remote peer connects or
// disconnects.
type peeringEvent struct {
	peer *peerConnection
	join bool
}

// peerSet represents the collection of active peer participating in the chain
// download procedure.
type peerSet struct {
	peers  map[string]*peerConnection
	rates  *msgrate.Trackers // Set of rate trackers to give the sync a common beat
	events event.Feed        // Feed to publish peer lifecycle events on

	lock sync.RWMutex
}

// newPeerSet creates a new peer set top track the active download sources.
func newPeerSet() *peerSet {
	return &peerSet{
		peers: make(map[string]*peerConnection),
		rates: msgrate.NewTrackers(log.New("proto", "eth")),
	}
}

// SubscribeEvents subscribes to peer arrival and departure events.
func (ps *peerSet) SubscribeEvents(ch chan<- *peeringEvent) event.Subscription {
	return ps.events.Subscribe(ch)
}

// SubscribeNewPeers subscribes to peer arrival events only. The supplied channel
// receives newly-connected peerConnection values. Used by the classical Fast
// Sync state-sync loop to assign NodeData tasks to new peers.
func (ps *peerSet) SubscribeNewPeers(ch chan<- *peerConnection) event.Subscription {
	all := make(chan *peeringEvent, 100)
	sub := ps.events.Subscribe(all)
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case ev, ok := <-all:
				if !ok {
					return
				}
				if ev.join {
					select {
					case ch <- ev.peer:
					default:
					}
				}
			case <-sub.Err():
				return
			}
		}
	}()
	return sub
}

// SubscribePeerDrops subscribes to peer departure events only. The supplied
// channel receives peerConnection values that have just disconnected. Used by
// the classical Fast Sync state-sync loop to cancel in-flight NodeData requests.
func (ps *peerSet) SubscribePeerDrops(ch chan<- *peerConnection) event.Subscription {
	all := make(chan *peeringEvent, 100)
	sub := ps.events.Subscribe(all)
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case ev, ok := <-all:
				if !ok {
					return
				}
				if !ev.join {
					select {
					case ch <- ev.peer:
					default:
					}
				}
			case <-sub.Err():
				return
			}
		}
	}()
	return sub
}

// NodeDataIdlePeers retrieves a flat list of all the currently node-data-idle
// peers within the active peer set, sorted by their reputation.
//
// A.27 / A.53-2: idle predicate is the per-peer parallel inflight slot count,
// NOT a binary in-flight flag. The previous binary stateIdle gate (A.27)
// capped each peer at one outstanding NodeData request and was the upstream
// bottleneck that prevented the A.53 16-slot table from multiplying dispatch.
// We retain the A.40 sticky-PBSS skip and A.37 advertised-PBSS skip; only the
// per-peer concurrency clause changes. Capacity remains the sort key (best
// peers ranked first), but no longer gates eligibility.
func (ps *peerSet) NodeDataIdlePeers() ([]*peerConnection, int) {
	// A.69 (refs #844 #859): peer-starvation bypass. Snapshot the total peer
	// count under the same lock that idlePeers will take, so we don't race
	// the predicate against a concurrent Register/Unregister. When the fleet
	// is at or below nodeDataPbssBypassPeerFloor, ignore the A.40 sticky-PBSS
	// blacklist — see the constant doc for the rationale.
	ps.lock.RLock()
	totalPeers := len(ps.peers)
	ps.lock.RUnlock()
	peerStarved := totalPeers <= nodeDataPbssBypassPeerFloor

	idle := func(p *peerConnection) bool {
		// A.40: sticky-skip PBSS-only peers so the fetcher stops re-assigning.
		// A.69: when peer-starved, bypass the sticky blacklist — losing the
		// only peer to a transient empty-response run leaves state-sync with
		// nowhere to recover. Log on first bypass so operators can correlate
		// stalls with the bypass active. The proactive A.37 gate
		// (remoteSchemePbss) is NOT bypassed because it reflects an explicit
		// handshake claim from the peer rather than a heuristic from observed
		// behavior.
		if p.nodeDataPbssLikely.Load() && !peerStarved {
			return false
		}
		// A.37: proactive sibling — skip peers that announced PBSS in the
		// XDPOS2 handshake (XDCCaps.Scheme == PBSS). They can't serve NodeData
		// even on the first attempt; gating them out here avoids spending
		// A.40's empty-response budget on a guaranteed-fail peer. Note: we
		// deliberately DO NOT skip remoteLegacy peers here — legacy peers can
		// still serve headers/bodies, and many of them DO serve NodeData
		// correctly. The legacy gate fires later in faststatesync.assignTasks.
		if p.remoteSchemePbss.Load() {
			return false
		}
		// A.53/2: a peer is eligible iff it has at least one free parallel
		// inflight slot. With 16 slots per peer and 3 HBSS peers the upper
		// bound on concurrent NodeData requests is 48 (vs 3 under the old
		// binary semaphore).
		return p.inflightCount() < maxParallelInflightSlots
	}
	throughput := func(p *peerConnection) int {
		return p.rates.Capacity(eth.NodeDataMsg, time.Second)
	}
	return ps.idlePeers(idle, throughput)
}

// idlePeers retrieves a flat list of all currently idle peers, using the
// provided functions to check idleness and retrieve capacity. The resulting
// set is sorted by capacity (descending).
func (ps *peerSet) idlePeers(idleCheck func(*peerConnection) bool, capacity func(*peerConnection) int) ([]*peerConnection, int) {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var (
		total = len(ps.peers)
		idle  = make([]*peerConnection, 0, len(ps.peers))
		caps  = make([]int, 0, len(ps.peers))
	)
	for _, p := range ps.peers {
		if idleCheck(p) {
			idle = append(idle, p)
			caps = append(caps, capacity(p))
		}
	}
	sort.Sort(&peerCapacitySort{peers: idle, caps: caps})
	return idle, total
}

// Reset iterates over the current peer set, and resets each of the known peers
// to prepare for a next batch of block retrieval.
func (ps *peerSet) Reset() {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	for _, peer := range ps.peers {
		peer.Reset()
	}
}

// nodeDataMinCap is the cold-peer NodeData capacity floor (A.26 Q5). When all
// existing trackers report capacity=0 (early sync, freshly-restarted node, or
// many bad peers), MeanCapacities returns 0 for NodeDataMsg and the per-round
// capacity collapses to 1 (roundCapacity clamps to max(1, ceil(throughput))).
// At 1 hash/round per peer fast sync grinds to a halt. Seeding the new peer's
// tracker at this floor lets it actually issue useful batches; the EWMA in
// msgrate.Tracker.Update will quickly correct the estimate downward if the
// peer can't honour the request.
const nodeDataMinCap = 64

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
//
// The method also sets the starting throughput values of the new peer to the
// average of all existing peers, to give it a realistic chance of being used
// for data retrievals.
func (ps *peerSet) Register(p *peerConnection) error {
	// Register the new peer with some meaningful defaults
	ps.lock.Lock()
	if _, ok := ps.peers[p.id]; ok {
		ps.lock.Unlock()
		return errAlreadyRegistered
	}
	// A.26 Q5: enforce a cold-peer NodeData capacity floor. We can't reach
	// msgrate's private capacity map after NewTracker, but we can pre-cook the
	// caps argument we pass in. If the cluster mean is below the floor we lift
	// just the NodeData entry; other message kinds are left at the real mean.
	caps := ps.rates.MeanCapacities()
	if caps == nil {
		caps = make(map[uint64]float64)
	}
	if caps[eth.NodeDataMsg] < float64(nodeDataMinCap) {
		caps[eth.NodeDataMsg] = float64(nodeDataMinCap)
	}
	p.rates = msgrate.NewTracker(caps, ps.rates.MedianRoundTrip())
	if err := ps.rates.Track(p.id, p.rates); err != nil {
		ps.lock.Unlock()
		return err
	}
	ps.peers[p.id] = p
	ps.lock.Unlock()

	ps.events.Send(&peeringEvent{peer: p, join: true})
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) Unregister(id string) error {
	ps.lock.Lock()
	p, ok := ps.peers[id]
	if !ok {
		ps.lock.Unlock()
		return errNotRegistered
	}
	delete(ps.peers, id)
	ps.rates.Untrack(id)
	ps.lock.Unlock()

	ps.events.Send(&peeringEvent{peer: p, join: false})
	return nil
}

// Peer retrieves the registered peer with the given id.
func (ps *peerSet) Peer(id string) *peerConnection {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// Len returns if the current number of peers in the set.
func (ps *peerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// AllPeers retrieves a flat list of all the peers within the set.
func (ps *peerSet) AllPeers() []*peerConnection {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peerConnection, 0, len(ps.peers))
	for _, p := range ps.peers {
		list = append(list, p)
	}
	return list
}

// peerCapacitySort implements sort.Interface.
// It sorts peer connections by capacity (descending).
type peerCapacitySort struct {
	peers []*peerConnection
	caps  []int
}

func (ps *peerCapacitySort) Len() int {
	return len(ps.peers)
}

func (ps *peerCapacitySort) Less(i, j int) bool {
	return ps.caps[i] > ps.caps[j]
}

func (ps *peerCapacitySort) Swap(i, j int) {
	ps.peers[i], ps.peers[j] = ps.peers[j], ps.peers[i]
	ps.caps[i], ps.caps[j] = ps.caps[j], ps.caps[i]
}
