// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package downloader

// A.50 — Skeleton peer-set bridge regression tests (refs #844, #857).
//
// Sprint 2 lesson: the architecture already routes XDPOS2 (version=100) peers
// into d.peers via h.downloader.RegisterPeer (eth/handler.go:324), and the
// skeleton.sync loop seeds s.idles from s.peers.AllPeers() plus subscribes to
// peer-join events. So XDPOS2 peers ARE structurally eligible for skeleton
// header download. This file locks that invariant in via tests so that a
// future refactor (adding a version filter at s.idles assignment, etc.) is
// caught at CI time, NOT in a 24h xdcscan deploy cycle.
//
// These tests must be runnable under `go test -race -count=1 ./eth/downloader/...`
// in under ~5 seconds total. They do NOT exercise the production XDPOS2 wire
// format — that responsibility lives in eth/protocols/eth/peer.go's
// newHeadersRequestData. Here we only verify the skeleton accepts the peer
// AND drives requests through its peer.peer.RequestHeadersByNumber method
// (which delegates to the version-aware encoder in production).

import (
	"encoding/json"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

// TestSkeletonAcceptsXDPOS2Peers verifies that peers registered with
// version=eth.XDPOS2 are eligible idle peers for the skeleton sync loop,
// served headers exactly like eth/69/70 peers, and that sync converges.
//
// Regression guard: if anyone adds a `if version != ETH69 && version != ETH70`
// gate inside assignTasks/idlePeers, this test fails immediately.
func TestSkeletonAcceptsXDPOS2Peers(t *testing.T) {
	// Build a small chain. requestHeaders (=512) headers is enough so the
	// skeleton has at least one full batch to fetch but the test runs fast.
	chainLen := requestHeaders*2 + 50
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < chainLen; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	// Fresh DB seeded with genesis (matches TestSkeletonSyncRetrievals setup).
	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	// Two peers: one ETH69, one XDPOS2(=100). Both serve the same chain.
	// If the skeleton's peer-eligibility filter is XDPOS2-blind, the xdpos2
	// peer will sit idle and `served` will stay at zero on it.
	ethPeer := newSkeletonTestPeer("eth69-peer", chain)
	xdposPeer := newSkeletonTestPeer("xdpos2-peer", chain)

	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection(ethPeer.id, eth.ETH69, ethPeer, log.New("id", ethPeer.id))); err != nil {
		t.Fatalf("register eth peer: %v", err)
	}
	if err := peerset.Register(newPeerConnection(xdposPeer.id, eth.XDPOS2, xdposPeer, log.New("id", xdposPeer.id))); err != nil {
		t.Fatalf("register xdpos2 peer: %v", err)
	}

	drop := func(id string) { peerset.Unregister(id) }
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller(), &fakeChainReader{})
	defer skeleton.Terminate()

	skeleton.Sync(chain[len(chain)-1], nil, true)

	// Wait for skeleton to fill subchain tail to 1 (genesis-linked).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var progress skeletonProgress
		if blob := rawdb.ReadSkeletonSyncStatus(db); len(blob) > 0 {
			if err := json.Unmarshal(blob, &progress); err == nil &&
				len(progress.Subchains) == 1 &&
				progress.Subchains[0].Tail == 1 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Invariant 1: XDPOS2 peer was eligible — it MUST have served at least one
	// header batch. With 2 peers in the pool the skeleton spreads work, so
	// either peer could end up serving everything if the other was slower; but
	// across requestHeaders*2 = 1024 header range with batch=requestHeaders,
	// the assignTasks round-robin should hit both.
	ethServed := ethPeer.served.Load()
	xdposServed := xdposPeer.served.Load()
	totalServed := ethServed + xdposServed
	if totalServed == 0 {
		t.Fatalf("no peer served any headers (sync failed entirely): eth=%d xdpos2=%d", ethServed, xdposServed)
	}
	if xdposServed == 0 {
		t.Errorf("XDPOS2 peer was registered but never asked to serve headers — skeleton peer-set bridge regression. eth=%d xdpos2=%d (total=%d)",
			ethServed, xdposServed, totalServed)
	}

	// Invariant 2: peer was NOT dropped (XDPOS2 peers must not be treated as
	// malformed merely because of their version).
	if xdposPeer.dropped.Load() != 0 {
		t.Errorf("XDPOS2 peer was dropped during skeleton sync (drops=%d)", xdposPeer.dropped.Load())
	}
}

// TestSkeletonSeedsExistingXDPOS2PeerBeforeSubscribe verifies the missed-join
// race window between newPeerSet population and skeleton.SubscribeEvents is
// covered by the AllPeers() snapshot at sync-loop entry. This is the same
// invariant A.14c established for the faststatesync loop, applied here to
// skeleton.
//
// Setup: register the XDPOS2 peer BEFORE Sync() is called. No peering event
// will fire after Sync() starts. The skeleton must still pick the peer up
// from the AllPeers() snapshot and drive requests through it.
func TestSkeletonSeedsExistingXDPOS2PeerBeforeSubscribe(t *testing.T) {
	chainLen := requestHeaders + 50
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < chainLen; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	// Single XDPOS2 peer, registered BEFORE skeleton starts.
	peer := newSkeletonTestPeer("xdpos2-only", chain)
	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection(peer.id, eth.XDPOS2, peer, log.New("id", peer.id))); err != nil {
		t.Fatalf("register xdpos2 peer: %v", err)
	}

	drop := func(id string) { peerset.Unregister(id) }
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller(), &fakeChainReader{})
	defer skeleton.Terminate()

	skeleton.Sync(chain[len(chain)-1], nil, true)

	// The peer was registered ONCE, before Sync. SubscribeEvents inside Sync
	// will not deliver a join event for it (event.Feed only sends to active
	// subscribers at Send time). The AllPeers() snapshot is the only path.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if peer.served.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if peer.served.Load() == 0 {
		t.Fatalf("XDPOS2 peer registered before Sync was never used — AllPeers() seed missing or filtered. served=0")
	}
}

// xdpos2BatchRecordingPeer is a minimal Peer impl that records the (amount,
// reverse, skip) args of every RequestHeadersByNumber call AND serves a real
// header chain like skeletonTestPeer so the skeleton can actually converge.
// Used by TestSkeletonRequestsXDPOS2BatchSize to verify the on-the-wire
// request shape is 192 for XDPOS2 cycles and 512 for pure-ETH cycles.
//
// We can't piggyback on skeletonTestPeer because that fixture panics on
// `amount != requestHeaders` for the legacy assertion and we want to assert
// the actual amount sent, not reject it.
type xdpos2BatchRecordingPeer struct {
	*skeletonTestPeer
	// maxAmount is the largest `amount` ever requested. The skeleton
	// trims `requestCount = int(req.head)` near the genesis tail, so the
	// LAST request can be much smaller than the cycle batch. We assert on
	// the MAXIMUM amount, which reflects the cycle-wide batch size every
	// non-tail request used.
	maxAmount atomic.Int64
	lastSkip  atomic.Int64
	calls     atomic.Uint64
}

func (p *xdpos2BatchRecordingPeer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool, sink chan *eth.Response) (*eth.Request, error) {
	for {
		cur := p.maxAmount.Load()
		if int64(amount) <= cur {
			break
		}
		if p.maxAmount.CompareAndSwap(cur, int64(amount)) {
			break
		}
	}
	p.lastSkip.Store(int64(skip))
	p.calls.Add(1)
	return p.skeletonTestPeer.RequestHeadersByNumber(origin, amount, skip, reverse, sink)
}

// TestSkeletonRequestsXDPOS2BatchSize verifies that when the seeded peer
// fleet contains an XDPOS2 peer, the skeleton's per-cycle batch size is
// requestHeadersXDPOS2 (=192), so executeTask issues amount=192 requests
// that XDPoSChain peers can actually serve (their MaxHeaderFetch cap is 192).
//
// Regression: before A.11 (refs #857), modern v1.17.3 hard-coded amount=512
// at executeTask and strict-equality-validated against 512. Every honest
// 192-header reply from the public XDC mainnet peer fleet was rejected,
// stalling the skeleton indefinitely.
func TestSkeletonRequestsXDPOS2BatchSize(t *testing.T) {
	chainLen := requestHeadersXDPOS2*3 + 50
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < chainLen; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	inner := newSkeletonTestPeer("xdpos2-batch", chain)
	peer := &xdpos2BatchRecordingPeer{skeletonTestPeer: inner}

	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection(inner.id, eth.XDPOS2, peer, log.New("id", inner.id))); err != nil {
		t.Fatalf("register xdpos2 peer: %v", err)
	}

	drop := func(id string) { peerset.Unregister(id) }
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller(), &fakeChainReader{})
	defer skeleton.Terminate()

	skeleton.Sync(chain[len(chain)-1], nil, true)

	// Wait for the skeleton to link to genesis OR for the test deadline. We
	// can't assert on a single call's amount — the very last request of a
	// cycle is trimmed to `req.head` near the genesis tail. We instead wait
	// until enough calls have been issued that the *maximum* amount has
	// stabilized at the cycle batch.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var progress skeletonProgress
		if blob := rawdb.ReadSkeletonSyncStatus(db); len(blob) > 0 {
			if err := json.Unmarshal(blob, &progress); err == nil &&
				len(progress.Subchains) == 1 &&
				progress.Subchains[0].Tail == 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if peer.calls.Load() == 0 {
		t.Fatalf("skeleton never issued a header request to the XDPOS2 peer")
	}
	if got := peer.maxAmount.Load(); got != int64(requestHeadersXDPOS2) {
		t.Errorf("XDPOS2 cycle issued amount=%d; want %d (requestHeadersXDPOS2) — A.11 regression",
			got, requestHeadersXDPOS2)
	}
	if got := peer.lastSkip.Load(); got != 0 {
		t.Errorf("XDPOS2 cycle issued skip=%d; want 0 (contiguous reverse walk-back)", got)
	}
}

// TestSkeletonAcceptsXDPOS2ShortBatch verifies the response validator
// accepts a 192-header reply (the XDPoSChain server-side serve cap) in an
// XDPOS2 cycle WITHOUT rejecting it as "Invalid non-genesis header count".
// This is the direct regression test for the #857 production stall.
//
// Setup: register one XDPOS2 peer. The skeleton will pick cycleBatch=192,
// request amount=192, and the peer returns 192 headers via its predefined
// chain. The validator must accept (skeleton converges, peer not dropped).
func TestSkeletonAcceptsXDPOS2ShortBatch(t *testing.T) {
	// chain length larger than one full XDPOS2 batch so the request shape is
	// exercised at least once with `len(headers) == requestHeadersXDPOS2`.
	chainLen := requestHeadersXDPOS2*2 + 10
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < chainLen; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	peer := newSkeletonTestPeer("xdpos2-short-batch", chain)
	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection(peer.id, eth.XDPOS2, peer, log.New("id", peer.id))); err != nil {
		t.Fatalf("register xdpos2 peer: %v", err)
	}

	drop := func(id string) { peerset.Unregister(id) }
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller(), &fakeChainReader{})
	defer skeleton.Terminate()

	skeleton.Sync(chain[len(chain)-1], nil, true)

	// Wait until the skeleton fully links to genesis (Tail==1).
	deadline := time.Now().Add(5 * time.Second)
	linked := false
	for time.Now().Before(deadline) {
		var progress skeletonProgress
		if blob := rawdb.ReadSkeletonSyncStatus(db); len(blob) > 0 {
			if err := json.Unmarshal(blob, &progress); err == nil &&
				len(progress.Subchains) == 1 &&
				progress.Subchains[0].Tail == 1 {
				linked = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !linked {
		t.Fatalf("XDPOS2 short-batch (192-header) reply was rejected — skeleton did not link to genesis. served=%d dropped=%d",
			peer.served.Load(), peer.dropped.Load())
	}
	if peer.dropped.Load() != 0 {
		t.Errorf("XDPOS2 peer was dropped during a healthy 192-header sync (drops=%d)", peer.dropped.Load())
	}
	if peer.served.Load() < uint64(requestHeadersXDPOS2) {
		t.Errorf("XDPOS2 peer served only %d headers; expected ≥ %d to prove at least one full batch was accepted",
			peer.served.Load(), requestHeadersXDPOS2)
	}
}

// TestSkeletonPureETHKeepsLegacyBatchSize is the symmetric regression: when
// NO XDPOS2 peer is in the seeded fleet, the skeleton must keep the legacy
// requestHeaders (=512) batch size so eth/66+ throughput is unchanged.
func TestSkeletonPureETHKeepsLegacyBatchSize(t *testing.T) {
	chainLen := requestHeaders*2 + 50
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < chainLen; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	inner := newSkeletonTestPeer("eth69-batch", chain)
	peer := &xdpos2BatchRecordingPeer{skeletonTestPeer: inner}

	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection(inner.id, eth.ETH69, peer, log.New("id", inner.id))); err != nil {
		t.Fatalf("register eth69 peer: %v", err)
	}

	drop := func(id string) { peerset.Unregister(id) }
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller(), &fakeChainReader{})
	defer skeleton.Terminate()

	skeleton.Sync(chain[len(chain)-1], nil, true)

	// Wait for the skeleton to link to genesis (or deadline) so the
	// max-amount observation reflects at least one full non-tail batch.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var progress skeletonProgress
		if blob := rawdb.ReadSkeletonSyncStatus(db); len(blob) > 0 {
			if err := json.Unmarshal(blob, &progress); err == nil &&
				len(progress.Subchains) == 1 &&
				progress.Subchains[0].Tail == 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if peer.calls.Load() == 0 {
		t.Fatalf("skeleton never issued a header request to the ETH69 peer")
	}
	if got := peer.maxAmount.Load(); got != int64(requestHeaders) {
		t.Errorf("pure-ETH cycle issued amount=%d; want %d (requestHeaders) — A.11 must NOT downshift batch size when no XDPOS2 peer is present",
			got, requestHeaders)
	}
}
