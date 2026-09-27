// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// A.53: Per-peer parallel NodeData inflight slot accounting. The slot table
// is the foundation for true wire-level multiplexing (multiple outstanding
// GetNodeData requests per peer keyed by request-id); the binary stateIdle
// flag inherited from A.27 is preserved for protocol compatibility but
// shadowed by the slot reservation API. Tests verify the invariants the
// slot table is responsible for upholding even under concurrent acquire/release.
//
// Refs A.53 / #857.

package downloader

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/msgrate"
)

// noopFetchPeer is a stub Peer that satisfies the downloader.Peer interface
// with no-op (and rapidly-returning) wire calls. Used by the A.53/2 dispatcher
// gate tests below — we only need FetchNodeData to traverse its accounting
// path without making real network calls.
type noopFetchPeer struct{}

func (noopFetchPeer) RequestHeadersByHash(common.Hash, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (noopFetchPeer) RequestHeadersByNumber(uint64, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (noopFetchPeer) RequestBodies([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (noopFetchPeer) RequestReceipts([]common.Hash, []uint64, []uint64, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (noopFetchPeer) RequestNodeData([]common.Hash) error { return nil }

// newDispatchTestPeerConn returns a peerConnection wired with a no-op Peer and
// a real msgrate.Tracker so FetchNodeData / SetNodeDataIdle can update RTT
// without panicking on a nil tracker.
func newDispatchTestPeerConn(id string) *peerConnection {
	pc := newPeerConnection(id, 67, noopFetchPeer{}, log.New("id", id))
	pc.rates = msgrate.NewTracker(nil, time.Second)
	return pc
}

// newTestPeerConn returns a peerConnection suitable for slot-table tests. The
// underlying Peer iface stays nil — we only exercise inflight accounting, not
// wire I/O.
func newTestPeerConn(id string) *peerConnection {
	return newPeerConnection(id, 67, nil, log.New("id", id))
}

// TestParallelInflightSlotsCapacity verifies the per-peer slot table accepts
// exactly maxParallelInflightSlots concurrent reservations and rejects the
// (N+1)th until one is released. This is the core A.53 invariant: the slot
// table provides parallel-dispatch headroom without unbounded queue growth.
func TestParallelInflightSlotsCapacity(t *testing.T) {
	p := newTestPeerConn("p1")
	tickets := make([]int, 0, maxParallelInflightSlots)

	// Acquire all slots.
	for i := 0; i < maxParallelInflightSlots; i++ {
		req := &stateReq{}
		tok, ok := p.acquireInflightSlot(req)
		if !ok {
			t.Fatalf("slot %d: acquireInflightSlot returned !ok with %d/%d in use",
				i, p.inflightCount(), maxParallelInflightSlots)
		}
		tickets = append(tickets, tok)
	}
	if got := p.inflightCount(); got != maxParallelInflightSlots {
		t.Fatalf("inflightCount = %d, want %d", got, maxParallelInflightSlots)
	}

	// (N+1)th must be rejected.
	req := &stateReq{}
	if _, ok := p.acquireInflightSlot(req); ok {
		t.Fatalf("over-capacity acquire succeeded (slot table not enforcing limit)")
	}

	// Release one and confirm a new acquire succeeds.
	p.releaseInflightSlot(tickets[0])
	if got := p.inflightCount(); got != maxParallelInflightSlots-1 {
		t.Fatalf("after release, inflightCount = %d, want %d", got, maxParallelInflightSlots-1)
	}
	if _, ok := p.acquireInflightSlot(&stateReq{}); !ok {
		t.Fatalf("acquire after release failed")
	}
}

// TestParallelInflightNoOverlap is the core A.53 regression test: under
// concurrent acquire/release on the same peerConnection, no two live slots
// ever hold the same *stateReq pointer. Sentinel for the bug class where a
// CAS race could double-assign a request slot and corrupt deliver dispatch.
//
// Run with -race to catch the data-race variant of the same bug.
func TestParallelInflightNoOverlap(t *testing.T) {
	const (
		goroutines = 32
		iterations = 2000
	)
	p := newTestPeerConn("p-race")

	// Each goroutine repeatedly acquires a slot with a fresh *stateReq pointer,
	// checks no other live slot holds that same pointer, then releases.
	var (
		wg     sync.WaitGroup
		errors atomic.Int64
	)
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				req := &stateReq{}
				tok, ok := p.acquireInflightSlot(req)
				if !ok {
					// Table full — that's fine, retry by skipping.
					continue
				}
				// Snapshot all live slots and assert none aliases our req
				// (other than the one we just placed).
				if p.inflightHasAlias(tok, req) {
					errors.Add(1)
				}
				p.releaseInflightSlot(tok)
			}
		}(g)
	}
	wg.Wait()
	if errors.Load() != 0 {
		t.Fatalf("detected %d slot aliasing events under concurrent dispatch", errors.Load())
	}
	if got := p.inflightCount(); got != 0 {
		t.Fatalf("after all releases, inflightCount = %d, want 0", got)
	}
}

// TestParallelInflightReleaseInvalidTicket verifies that releasing a bogus
// ticket is a no-op (defensive: the dispatcher must never panic on a
// double-release path or out-of-range ticket).
func TestParallelInflightReleaseInvalidTicket(t *testing.T) {
	p := newTestPeerConn("p-bogus")
	// Out-of-range high.
	p.releaseInflightSlot(maxParallelInflightSlots + 5)
	// Negative.
	p.releaseInflightSlot(-1)
	// Empty slot (never acquired).
	p.releaseInflightSlot(0)

	if got := p.inflightCount(); got != 0 {
		t.Fatalf("invalid releases mutated inflightCount = %d", got)
	}
	// And a fresh acquire still works.
	if _, ok := p.acquireInflightSlot(&stateReq{}); !ok {
		t.Fatalf("acquire after bogus releases failed")
	}
}

// TestParallelInflightSlotIndependentOfStateIdle confirms the A.53 slot
// table is additive to the stateIdle counter: stateIdle reflects
// outstanding wire requests informationally, while inflight slots track
// the parallel-dispatch reservations. Both must coexist without one
// clobbering the other. A.53/2 keeps this invariant: the slot table is
// now the authoritative gate, but stateIdle remains useful for diagnostics
// and drop-path invariants.
func TestParallelInflightSlotIndependentOfStateIdle(t *testing.T) {
	p := newTestPeerConn("p-coexist")

	// Acquire a slot. stateIdle should remain 0 (slot table is intent;
	// stateIdle is the wire-layer gate).
	tok, ok := p.acquireInflightSlot(&stateReq{})
	if !ok {
		t.Fatalf("initial acquire failed")
	}
	if p.stateIdle.Load() != 0 {
		t.Fatalf("acquireInflightSlot mutated stateIdle (got %d, want 0)", p.stateIdle.Load())
	}

	// Independently flip stateIdle as the wire layer would.
	p.stateIdle.Store(1)
	if got := p.inflightCount(); got != 1 {
		t.Fatalf("stateIdle mutation altered inflight count (got %d, want 1)", got)
	}
	p.stateIdle.Store(0)

	p.releaseInflightSlot(tok)
	if got := p.inflightCount(); got != 0 {
		t.Fatalf("after release, inflightCount = %d, want 0", got)
	}
}

// TestFetchNodeDataParallelDispatch verifies the A.53/2 fix: FetchNodeData no
// longer binary-gates on stateIdle, so the dispatcher can drive up to
// maxParallelInflightSlots concurrent wire requests per peer. Pre-fix, the
// 2nd through Nth call would return errAlreadyFetching and the slot table
// would be useless. Post-fix, all calls succeed and the slot-count gating
// happens upstream in faststatesync.assignTasks.
func TestFetchNodeDataParallelDispatch(t *testing.T) {
	p := newDispatchTestPeerConn("p-parallel")

	// Issue maxParallelInflightSlots concurrent FetchNodeData calls. None
	// should be rejected — the slot table is intent already reserved by the
	// caller in production; here we just exercise the wire-call side.
	hashes := []common.Hash{{0x01}}
	for i := 0; i < maxParallelInflightSlots; i++ {
		if err := p.FetchNodeData(hashes); err != nil {
			t.Fatalf("FetchNodeData #%d returned error: %v (expected nil)", i, err)
		}
	}

	// The informational stateIdle counter should reflect outstanding wire
	// requests. Allow either the full N (if the RequestNodeData goroutines
	// haven't run yet) or 0 (if they all completed and decremented). What we
	// MUST NOT see is the pre-fix behavior of "first call sets to 1, all
	// subsequent return errAlreadyFetching" — which we already validated
	// above by every call returning nil.
	//
	// We don't sleep here because the goroutines may race with us; just
	// confirm stateIdle is bounded by the number of dispatches and that it
	// will eventually settle at 0 (drain via SetNodeDataIdle in production).
	if got := p.stateIdle.Load(); got < 0 || int(got) > maxParallelInflightSlots {
		t.Fatalf("stateIdle = %d, want in [0, %d]", got, maxParallelInflightSlots)
	}
}

// TestFetchNodeDataParallelRace stresses FetchNodeData under concurrent
// dispatch. With -race, this also catches any data-race regression in the
// counter / slot-table interaction. Pre-fix, only one of the goroutines
// would succeed; post-fix, all maxParallelInflightSlots succeed and the
// table never observes aliasing.
func TestFetchNodeDataParallelRace(t *testing.T) {
	p := newDispatchTestPeerConn("p-race-fetch")

	const goroutines = maxParallelInflightSlots
	var (
		wg       sync.WaitGroup
		failures atomic.Int64
		hashes   = []common.Hash{{0xab}}
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := p.FetchNodeData(hashes); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("FetchNodeData returned error on %d/%d concurrent calls (pre-A.53/2 regression)",
			failures.Load(), goroutines)
	}
}

// TestNodeDataIdlePeersSlotGated verifies the A.53/2 NodeDataIdlePeers
// predicate: a peer is eligible iff its parallel inflight slot table has
// at least one free slot, not iff its binary stateIdle is 0. Pre-fix, a
// peer with even one slot occupied (because stateIdle was set) would be
// filtered out. Post-fix, only a fully-saturated peer is filtered.
//
// A.67: maxParallelInflightSlots dropped from 16 → 1 to match legacy
// XDPoSChain's binary-semaphore behaviour. Under A.67, "partial" and "full"
// collapse — a peer with one slot used IS at capacity. The test guards the
// "partial eligibility" assertion behind a slot-count check so future tuning
// of maxParallelInflightSlots re-exercises the partial case automatically.
func TestNodeDataIdlePeersSlotGated(t *testing.T) {
	ps := newPeerSet()

	pFree := newDispatchTestPeerConn("p-free")
	pPartial := newDispatchTestPeerConn("p-partial")
	pFull := newDispatchTestPeerConn("p-full")

	if err := ps.Register(pFree); err != nil {
		t.Fatalf("Register pFree: %v", err)
	}
	if err := ps.Register(pPartial); err != nil {
		t.Fatalf("Register pPartial: %v", err)
	}
	if err := ps.Register(pFull); err != nil {
		t.Fatalf("Register pFull: %v", err)
	}

	// pPartial: occupy one slot. With maxParallelInflightSlots > 1 it stays
	// eligible (partial). With == 1 it becomes equivalent to pFull.
	if _, ok := pPartial.acquireInflightSlot(&stateReq{}); !ok {
		t.Fatal("could not occupy pPartial slot")
	}
	// pFull: occupy all slots. Should be excluded.
	for i := 0; i < maxParallelInflightSlots; i++ {
		if _, ok := pFull.acquireInflightSlot(&stateReq{}); !ok {
			t.Fatalf("could not occupy pFull slot %d", i)
		}
	}

	idle, total := ps.NodeDataIdlePeers()
	if total != 3 {
		t.Fatalf("total peers = %d, want 3", total)
	}

	idleIDs := make(map[string]bool, len(idle))
	for _, p := range idle {
		idleIDs[p.id] = true
	}
	if !idleIDs["p-free"] {
		t.Errorf("pFree (0 slots used) excluded from idle set")
	}
	if maxParallelInflightSlots > 1 {
		if !idleIDs["p-partial"] {
			t.Errorf("pPartial (1/%d slots used) excluded from idle set — A.53/2 regression: slot-table is gate, not stateIdle",
				maxParallelInflightSlots)
		}
	} else {
		// A.67 path: partial == full.
		if idleIDs["p-partial"] {
			t.Errorf("pPartial included in idle set with maxParallelInflightSlots=1 — slot gate not enforced")
		}
	}
	if idleIDs["p-full"] {
		t.Errorf("pFull (%d/%d slots used) included in idle set — slot gate not enforced",
			maxParallelInflightSlots, maxParallelInflightSlots)
	}
}

// TestNodeDataIdlePeersA69PeerStarvationBypass verifies the A.69 peer-
// starvation bypass for the A.40 sticky-PBSS blacklist. When the fleet is
// at or below nodeDataPbssBypassPeerFloor (default 2), a peer marked as
// nodeDataPbssLikely is still returned by NodeDataIdlePeers — losing the
// only NodeData source to a transient empty-response run would otherwise
// strand state-sync indefinitely.
//
// The proactive A.37 gate (remoteSchemePbss) is intentionally NOT bypassed
// because it reflects an explicit handshake claim; the test asserts that
// invariant too.
//
// Refs A.69 / #844 / #859.
func TestNodeDataIdlePeersA69PeerStarvationBypass(t *testing.T) {
	t.Run("starved_bypasses_A40_blacklist", func(t *testing.T) {
		ps := newPeerSet()
		// Two peers — at floor (default 2); blacklist bypass should fire.
		pBlacklisted := newDispatchTestPeerConn("p-blacklisted")
		pBlacklisted.nodeDataPbssLikely.Store(true)
		pNormal := newDispatchTestPeerConn("p-normal")
		if err := ps.Register(pBlacklisted); err != nil {
			t.Fatalf("Register pBlacklisted: %v", err)
		}
		if err := ps.Register(pNormal); err != nil {
			t.Fatalf("Register pNormal: %v", err)
		}

		idle, total := ps.NodeDataIdlePeers()
		if total != 2 {
			t.Fatalf("total peers = %d, want 2", total)
		}
		ids := make(map[string]bool, len(idle))
		for _, p := range idle {
			ids[p.id] = true
		}
		if !ids["p-blacklisted"] {
			t.Errorf("p-blacklisted excluded at peer-starvation floor — A.69 bypass not active")
		}
		if !ids["p-normal"] {
			t.Errorf("p-normal excluded — unrelated regression in slot/idle gating")
		}
	})

	t.Run("healthy_fleet_still_blacklists", func(t *testing.T) {
		ps := newPeerSet()
		// Three peers — above floor; sticky-PBSS still skips the marked peer.
		pBlacklisted := newDispatchTestPeerConn("p-blacklisted")
		pBlacklisted.nodeDataPbssLikely.Store(true)
		pA := newDispatchTestPeerConn("p-a")
		pB := newDispatchTestPeerConn("p-b")
		for _, p := range []*peerConnection{pBlacklisted, pA, pB} {
			if err := ps.Register(p); err != nil {
				t.Fatalf("Register %s: %v", p.id, err)
			}
		}

		idle, total := ps.NodeDataIdlePeers()
		if total != 3 {
			t.Fatalf("total peers = %d, want 3", total)
		}
		ids := make(map[string]bool, len(idle))
		for _, p := range idle {
			ids[p.id] = true
		}
		if ids["p-blacklisted"] {
			t.Errorf("p-blacklisted included on healthy fleet — A.40 sticky-PBSS hardening regressed")
		}
		if !ids["p-a"] || !ids["p-b"] {
			t.Errorf("non-blacklisted peers wrongly excluded; got %v", ids)
		}
	})

	t.Run("proactive_pbss_gate_not_bypassed", func(t *testing.T) {
		ps := newPeerSet()
		// One peer, proactively PBSS-tagged via handshake. Bypass should NOT
		// re-include it — A.37 reflects an explicit "I can't serve" claim.
		pPbss := newDispatchTestPeerConn("p-pbss")
		pPbss.remoteSchemePbss.Store(true)
		if err := ps.Register(pPbss); err != nil {
			t.Fatalf("Register pPbss: %v", err)
		}
		idle, total := ps.NodeDataIdlePeers()
		if total != 1 {
			t.Fatalf("total peers = %d, want 1", total)
		}
		if len(idle) != 0 {
			t.Errorf("proactive A.37 PBSS gate bypassed at peer-starvation; got %d idle peers, want 0", len(idle))
		}
	})
}
