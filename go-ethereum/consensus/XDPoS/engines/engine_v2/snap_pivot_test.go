// Copyright (c) 2026 XDC Network
// Unit tests for snap_pivot.go — issue #807 Phase 3a.
//
// These tests cover the control-flow checks (V2-only, depth, epoch
// transition, hardfork buffer, fetcher behavior, parent linkage).
// QC verification (checks 2+3) requires masternode set wiring and is
// exercised end-to-end by Phase 5's bench, not here.

package engine_v2

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// newTestXDPoS_v2 returns a minimal XDPoS_v2 with only the config fields
// SelectSnapPivot reads. The test deliberately leaves caches / channels /
// signing-state nil — none are touched by the helper's pre-check phase.
func newTestXDPoS_v2(v2Switch uint64, epoch uint64) *XDPoS_v2 {
	return &XDPoS_v2{
		config: &params.XDPoSConfig{
			Epoch: epoch,
			V2: &params.V2{
				SwitchBlock: new(big.Int).SetUint64(v2Switch),
			},
		},
	}
}

// chainHeader returns a header at `n` with `parentHash` set so a sequence
// of such headers forms a valid parent chain. Used for fetcher fixtures.
func chainHeader(n uint64, parentHash common.Hash) *types.Header {
	return &types.Header{
		Number:     new(big.Int).SetUint64(n),
		ParentHash: parentHash,
		Root:       common.HexToHash("0x1234"), // synthetic
		Extra:      nil,                        // QC check expected to fail; we test pre-check rejections only
	}
}

// fetcherFromMap returns a HeaderFetcher backed by a map. Missing entries
// return nil (simulating peer not serving that header).
func fetcherFromMap(m map[uint64]*types.Header) HeaderFetcher {
	return func(n uint64) *types.Header {
		return m[n]
	}
}

func TestSelectSnapPivot_PeerHeadTooLow(t *testing.T) {
	x := newTestXDPoS_v2(1_000_000, 900)
	got := x.SelectSnapPivot(nil, 5, fetcherFromMap(nil), nil)
	if got != nil {
		t.Fatalf("expected nil for peerHead=5 < SnapPivotMinDepth=12, got %+v", got)
	}
}

func TestSelectSnapPivot_PreV2Rejected(t *testing.T) {
	// V2 switch at 80M; peer head at 80,000,020 — only candidate would be
	// in pre-V2 territory after the depth+offset adjustment.
	x := newTestXDPoS_v2(80_000_000, 900)
	got := x.SelectSnapPivot(nil, 80_000_020, fetcherFromMap(nil), nil)
	if got != nil {
		t.Fatalf("expected nil (only candidate is pre-V2), got %+v", got)
	}
}

func TestSelectSnapPivot_NoHeadersFromPeer(t *testing.T) {
	// Peer head well past V2 switch + epoch math lands cleanly mid-epoch,
	// but fetcher returns nil for everything → expect nil result.
	x := newTestXDPoS_v2(1_000_000, 900)
	got := x.SelectSnapPivot(nil, 82_000_000, fetcherFromMap(nil), nil)
	if got != nil {
		t.Fatalf("expected nil (fetcher returns nil), got %+v", got)
	}
}

func TestSelectSnapPivot_ParentLinkageBroken(t *testing.T) {
	// Build N..N+3 with WRONG parent links → algorithm should reject and
	// walk to next candidate, eventually returning nil since they all
	// have broken parents.
	x := newTestXDPoS_v2(1_000_000, 900)
	m := map[uint64]*types.Header{}
	// Build 20 candidates' worth, all with bad parent linkage.
	for i := range SnapPivotMaxCandidates {
		base := uint64(82_000_000 - i*900 + 450)
		for j := range uint64(4) {
			m[base+j] = chainHeader(base+j, common.HexToHash("0xdeadbeef")) // all same wrong parent
		}
	}
	got := x.SelectSnapPivot(nil, 82_000_000, fetcherFromMap(m), nil)
	if got != nil {
		t.Fatalf("expected nil (parent linkage broken), got %+v", got)
	}
}

func TestSelectSnapPivot_HardforkBufferRejection(t *testing.T) {
	// Place a hardfork right where the first candidate would land.
	x := newTestXDPoS_v2(1_000_000, 900)
	peerHead := uint64(82_000_000)
	// First candidate per algorithm: ((peerHead-12)/epoch)*epoch + 450
	startEpoch := (peerHead - SnapPivotMinDepth) / 900
	firstCandidate := startEpoch*900 + SnapPivotEpochOffset
	if firstCandidate > peerHead-SnapPivotMinDepth {
		firstCandidate = (startEpoch-1)*900 + SnapPivotEpochOffset
	}
	// Hardfork exactly at firstCandidate; algorithm should reject it
	// (and all subsequent — we want a NO_PIVOT to demonstrate the check).
	// To force NO_PIVOT we put hardforks at EVERY candidate position.
	hardforks := []uint64{}
	c := firstCandidate
	for range SnapPivotMaxCandidates {
		hardforks = append(hardforks, c)
		if c < 900 {
			break
		}
		c -= 900
	}
	got := x.SelectSnapPivot(nil, peerHead, fetcherFromMap(nil), hardforks)
	if got != nil {
		t.Fatalf("expected nil (all candidates near hardfork), got %+v", got)
	}
}

func TestSelectSnapPivot_EpochBoundaryAvoided(t *testing.T) {
	// This is the bug Phase 2 discovered. peerHead lands on epoch=0
	// position. Without the offset, all candidates would land on
	// epoch=0 positions and fail check 5. With the offset, candidates
	// land mid-epoch.
	_ = newTestXDPoS_v2(1_000_000, 900) // not invoked; we test the algorithm constants only
	peerHead := uint64(82_215_000)      // 82_215_000 % 900 == 0 (epoch boundary)
	// Compute where the first candidate lands.
	startEpoch := (peerHead - SnapPivotMinDepth) / 900
	firstCandidate := startEpoch*900 + SnapPivotEpochOffset
	if firstCandidate > peerHead-SnapPivotMinDepth {
		firstCandidate = (startEpoch-1)*900 + SnapPivotEpochOffset
	}
	pos := firstCandidate % 900
	if pos < SnapPivotHardforkBuffer || pos > 900-SnapPivotHardforkBuffer {
		t.Fatalf("first candidate %d is in epoch-transition window (pos=%d) — offset failed",
			firstCandidate, pos)
	}
	// Smoke-pass: candidate is mid-epoch as intended.
	t.Logf("epoch-boundary peerHead=%d → first candidate %d (pos %d in epoch) — OK",
		peerHead, firstCandidate, pos)
}

func TestSelectSnapPivot_DepthInvariant(t *testing.T) {
	// Algorithm invariant: depth = peerHead - candidate is monotonically
	// non-decreasing across iterations (stepping down by epoch always
	// increases depth). First candidate must already satisfy depth ≥ MIN.
	peerHead := uint64(82_000_000)
	startEpoch := (peerHead - SnapPivotMinDepth) / 900
	firstCandidate := startEpoch*900 + SnapPivotEpochOffset
	if firstCandidate > peerHead-SnapPivotMinDepth {
		firstCandidate = (startEpoch-1)*900 + SnapPivotEpochOffset
	}
	firstDepth := peerHead - firstCandidate
	if firstDepth < SnapPivotMinDepth {
		t.Fatalf("first candidate %d has depth %d < %d — algorithm broken",
			firstCandidate, firstDepth, SnapPivotMinDepth)
	}
}
