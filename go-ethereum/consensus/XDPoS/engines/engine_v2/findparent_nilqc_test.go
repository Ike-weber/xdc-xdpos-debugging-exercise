// Copyright (c) 2026 XDC Network
// Regression test for #1368: FindParentBlockToAssign dereferenced
// x.highestQuorumCert.ProposedBlockInfo.Number with no nil guard, panicking
// the miner goroutine (and killing the whole process) whenever the update
// loop fired before the V2 QC state was populated.

package engine_v2

import (
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

// newBareXDPoS_v2 returns an XDPoS_v2 with no QC state seeded at all — the
// zero-value lock is usable directly, and FindParentBlockToAssign must not
// require any other field to be present.
func newBareXDPoS_v2() *XDPoS_v2 {
	return &XDPoS_v2{}
}

// TestFindParentBlockToAssign_NilHighestQuorumCert covers the case where the
// engine hasn't populated highestQuorumCert at all (e.g. constructed but
// Initial()/ReinitBFT() hasn't run, or raced with ReinitBFT clearing state).
func TestFindParentBlockToAssign_NilHighestQuorumCert(t *testing.T) {
	x := newBareXDPoS_v2()
	x.highestQuorumCert = nil

	var parent *types.Block
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FindParentBlockToAssign panicked with nil highestQuorumCert: %v", r)
			}
		}()
		parent = x.FindParentBlockToAssign(nil)
	}()

	if parent != nil {
		t.Fatalf("expected nil parent for nil highestQuorumCert, got %+v", parent)
	}
}

// TestFindParentBlockToAssign_NilProposedBlockInfo covers a non-nil QC whose
// ProposedBlockInfo has not been set yet.
func TestFindParentBlockToAssign_NilProposedBlockInfo(t *testing.T) {
	x := newBareXDPoS_v2()
	x.highestQuorumCert = &types.QuorumCert{
		ProposedBlockInfo: nil,
	}

	var parent *types.Block
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FindParentBlockToAssign panicked with nil ProposedBlockInfo: %v", r)
			}
		}()
		parent = x.FindParentBlockToAssign(nil)
	}()

	if parent != nil {
		t.Fatalf("expected nil parent for nil ProposedBlockInfo, got %+v", parent)
	}
}

// TestFindParentBlockToAssign_NilProposedBlockInfoNumber is the case actually
// observed live on netv12: the QC struct and its ProposedBlockInfo both
// exist (e.g. from ReinitBFT's `&types.BlockInfo{}` placeholder), but Number
// — a *big.Int — was never set, so it's nil. The SIGSEGV addr=0x10 in the
// reported panic is math/big.(*Int).Uint64 dereferencing this nil Number.
func TestFindParentBlockToAssign_NilProposedBlockInfoNumber(t *testing.T) {
	x := newBareXDPoS_v2()
	x.highestQuorumCert = &types.QuorumCert{
		ProposedBlockInfo: &types.BlockInfo{
			// Number deliberately left nil.
		},
	}

	var parent *types.Block
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FindParentBlockToAssign panicked with nil ProposedBlockInfo.Number: %v", r)
			}
		}()
		parent = x.FindParentBlockToAssign(nil)
	}()

	if parent != nil {
		t.Fatalf("expected nil parent for nil ProposedBlockInfo.Number, got %+v", parent)
	}
}
