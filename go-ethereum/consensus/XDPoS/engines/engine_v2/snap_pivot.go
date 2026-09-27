// Copyright (c) 2026 XDC Network
// Snap-sync pivot selection — refs issue #807 Phase 3a (helper only).
//
// This file implements the pure pivot-selection function specified in
// ADR-807 Phase 1. It is read-only over headers and does not mutate
// engine state. The state-machine integration that invokes this helper
// from xdcSyncer is Phase 3b (deferred — see #807 tracker).

package engine_v2

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// SnapPivotMinDepth is check 4 of ADR-807 Phase 1: the candidate
	// pivot must be at least this many blocks below the peer-claimed
	// head. 3 (raw commit-depth) + 9 (defence-in-depth) = 12.
	SnapPivotMinDepth uint64 = 12

	// SnapPivotMaxCandidates caps the walk from peer-head downward.
	// Bounds worst-case peer round-trips to 20 × 4 = 80 header fetches.
	SnapPivotMaxCandidates int = 20

	// SnapPivotHardforkBuffer is check 6 of ADR-807 Phase 1: candidates
	// within ±N of any active hardfork boundary are rejected.
	SnapPivotHardforkBuffer uint64 = 10

	// SnapPivotEpochOffset is the mid-epoch landing point for candidate
	// blocks. Without this offset, walking by EpochLength from an anchor
	// that itself sits on an epoch boundary visits ONLY epoch boundaries,
	// all of which fail check 5 (epoch-transition window). The offset
	// makes the walk visit blocks at position EpochLength/2 of each
	// epoch — well outside the transition window. Empirically derived
	// from `scripts/snap-pivot-validate.sh` run on 2026-05-25 which
	// showed 16/151 anchors hitting this pathology before the fix.
	SnapPivotEpochOffset uint64 = 450
)

// PivotResult is the output of SelectSnapPivot. The caller (xdcSyncer
// Phase 3b) uses this to drive downloader.SnapSyncer.Sync().
type PivotResult struct {
	// Block is the header at the selected pivot.
	Block *types.Header

	// Root is the post-state root that snap-sync will download.
	Root common.Hash

	// Depth is `peerHead - Block.Number` at the moment of selection.
	// The caller should re-check this is still ≥ SnapPivotMinDepth
	// before triggering the actual snap-sync, since the peer head may
	// have moved during selection.
	Depth uint64
}

// HeaderFetcher fetches a header by number. The caller (xdcSyncer)
// supplies an implementation that issues an eth/XDPOS2 BlockHeaders
// request to a specific peer. Returning nil means the header was not
// served (timeout, missing, or peer dropped); the algorithm treats nil
// as a hard failure on the current candidate.
type HeaderFetcher func(number uint64) *types.Header

// SelectSnapPivot returns the highest pivot-safe block N ≤ peerHead
// per ADR-807 Phase 1 checks 1-6. Returns nil if no candidate within
// SnapPivotMaxCandidates passes — caller falls back to full-sync.
//
// The algorithm walks downward from `peerHead - SnapPivotMinDepth` in
// EpochLength strides (offset by SnapPivotEpochOffset to avoid the
// epoch-boundary pathology). For each candidate it applies the 6
// checks. Worst-case peer round-trips: SnapPivotMaxCandidates × 4
// (headers N, N+1, N+2, N+3) = 80. Typical: 1-2 candidates.
//
// The chain argument is consulted only for the active config (V2
// switch block, epoch length). Hardfork boundaries should be passed
// explicitly via `hardforks` — the caller should source them from the
// chain config rather than have this helper traverse it (keeps the
// helper testable with synthetic configs).
func (x *XDPoS_v2) SelectSnapPivot(
	chain consensus.ChainReader,
	peerHead uint64,
	fetchHeader HeaderFetcher,
	hardforks []uint64,
) *PivotResult {
	if peerHead < SnapPivotMinDepth {
		return nil
	}
	v2Switch := x.config.V2.SwitchBlock.Uint64()
	epoch := x.config.Epoch
	if epoch == 0 {
		log.Warn("SelectSnapPivot: chain has epoch=0; cannot select pivot")
		return nil
	}

	// Initial candidate: peerHead minus depth, rounded down to mid-epoch.
	start := peerHead - SnapPivotMinDepth
	startEpoch := start / epoch
	candidate := startEpoch*epoch + SnapPivotEpochOffset
	if candidate > start {
		// SnapPivotEpochOffset landed us above `start`; step down one epoch
		if startEpoch == 0 {
			return nil
		}
		candidate = (startEpoch-1)*epoch + SnapPivotEpochOffset
	}

	for range SnapPivotMaxCandidates {
		if candidate < v2Switch {
			return nil // check 1: ran out of V2 history
		}
		if candidate < epoch {
			return nil // can't go below first epoch
		}
		if result := x.tryPivotCandidate(chain, candidate, peerHead, fetchHeader, hardforks); result != nil {
			return result
		}
		// Step down one epoch
		candidate -= epoch
	}
	return nil
}

// tryPivotCandidate applies checks 2-6 of ADR-807 Phase 1 to a single
// candidate block N. Returns a PivotResult on success, nil on failure
// (with a debug log indicating which check rejected it).
//
// Check 1 (V2-only) is enforced by the caller's loop guard.
func (x *XDPoS_v2) tryPivotCandidate(
	chain consensus.ChainReader,
	candidate uint64,
	peerHead uint64,
	fetchHeader HeaderFetcher,
	hardforks []uint64,
) *PivotResult {
	depth := peerHead - candidate

	// Check 4: confirmation depth.
	if depth < SnapPivotMinDepth {
		log.Debug("snap pivot: depth check failed", "candidate", candidate, "depth", depth)
		return nil
	}

	// Check 5: not in epoch transition window. Reject if within
	// SnapPivotHardforkBuffer blocks of an epoch start or end.
	epoch := x.config.Epoch
	posInEpoch := candidate % epoch
	if posInEpoch < SnapPivotHardforkBuffer || posInEpoch > epoch-SnapPivotHardforkBuffer {
		log.Debug("snap pivot: epoch-transition check failed", "candidate", candidate, "pos", posInEpoch)
		return nil
	}

	// Check 6: not within ±SnapPivotHardforkBuffer of any hardfork.
	for _, h := range hardforks {
		var diff uint64
		if candidate > h {
			diff = candidate - h
		} else {
			diff = h - candidate
		}
		if diff <= SnapPivotHardforkBuffer {
			log.Debug("snap pivot: hardfork-buffer check failed", "candidate", candidate, "hardfork", h)
			return nil
		}
	}

	// Fetch headers N, N+1, N+2, N+3 from the peer.
	hN := fetchHeader(candidate)
	hN1 := fetchHeader(candidate + 1)
	hN2 := fetchHeader(candidate + 2)
	hN3 := fetchHeader(candidate + 3)
	if hN == nil || hN1 == nil || hN2 == nil || hN3 == nil {
		log.Debug("snap pivot: header gap N..N+3", "candidate", candidate)
		return nil
	}

	// Parent linkage smoke test (cheap precondition for checks 2-3).
	if hN1.ParentHash != hN.Hash() || hN2.ParentHash != hN1.Hash() || hN3.ParentHash != hN2.Hash() {
		log.Debug("snap pivot: parent linkage broken N..N+3", "candidate", candidate)
		return nil
	}

	// Checks 2+3: 3-chain committed + supermajority QC verified.
	// Each of hN1, hN2, hN3 must carry a QC referencing the prior
	// block, and verifyQC must succeed against the masternode set at
	// candidate's epoch.
	parents := []*types.Header{hN, hN1, hN2}
	for i, h := range []*types.Header{hN1, hN2, hN3} {
		var ext types.ExtraFields_v2
		if err := DecodeExtraFields(h.Extra, &ext); err != nil {
			log.Debug("snap pivot: QC decode failed", "candidate", candidate, "i", i, "err", err)
			return nil
		}
		if ext.QuorumCert == nil {
			log.Debug("snap pivot: QC absent", "candidate", candidate, "i", i)
			return nil
		}
		// verifyQC walks the validator set at parent.epoch and verifies
		// each signature in ext.QuorumCert.Signatures against the
		// proposed block info.
		if err := x.verifyQC(chain, ext.QuorumCert, parents[i], parents[:i+1]); err != nil {
			log.Debug("snap pivot: QC verify failed", "candidate", candidate, "i", i, "err", err)
			return nil
		}
	}

	return &PivotResult{
		Block: hN,
		Root:  hN.Root,
		Depth: depth,
	}
}
