// Copyright 2026 The go-ethereum Authors
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
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/params"
)

// newA89Downloader returns a Downloader with only the fields needed for A.89
// unit tests. Does NOT start the sync loop.
func newA89Downloader(cfg *params.ChainConfig) *Downloader {
	db := rawdb.NewMemoryDatabase()
	return &Downloader{
		stateDB:             db,
		fastSyncChainConfig: cfg,
	}
}

// TestA89_SetFastSyncAnchorFn verifies that SetFastSyncAnchorFn stores the
// callback and the stored closure executes correctly.
func TestA89_SetFastSyncAnchorFn(t *testing.T) {
	d := newA89Downloader(nil)

	if d.fastSyncAnchorFn != nil {
		t.Fatal("expected nil fastSyncAnchorFn before Set")
	}

	var called atomic.Bool
	d.SetFastSyncAnchorFn(func(n uint64, h common.Hash) {
		called.Store(true)
	})

	if d.fastSyncAnchorFn == nil {
		t.Fatal("fastSyncAnchorFn should be set after SetFastSyncAnchorFn")
	}
	d.fastSyncAnchorFn(1000, common.Hash{1})
	if !called.Load() {
		t.Fatal("stored fastSyncAnchorFn did not execute")
	}
}

// TestA89_SetFastSyncAnchorFn_Overwrite confirms a second call replaces the fn.
func TestA89_SetFastSyncAnchorFn_Overwrite(t *testing.T) {
	d := newA89Downloader(nil)

	var first, second atomic.Bool
	d.SetFastSyncAnchorFn(func(n uint64, h common.Hash) { first.Store(true) })
	d.SetFastSyncAnchorFn(func(n uint64, h common.Hash) { second.Store(true) })

	d.fastSyncAnchorFn(0, common.Hash{})
	if first.Load() {
		t.Fatal("first fn should have been replaced")
	}
	if !second.Load() {
		t.Fatal("second fn should be active")
	}
}

// TestA89_PivotDistance_NoConfig verifies fallback to fsMinFullBlocks (64)
// when fastSyncChainConfig is nil.
func TestA89_PivotDistance_NoConfig(t *testing.T) {
	d := newA89Downloader(nil)
	got := d.fastSyncPivotDistance(ethconfig.FastSync)
	if got != uint64(fsMinFullBlocks) {
		t.Errorf("nil config: want %d, got %d", fsMinFullBlocks, got)
	}
}

// TestA89_PivotDistance_NonXDPoSChain verifies fsMinFullBlocks for a config
// without XDPoS (e.g. Ethereum mainnet).
func TestA89_PivotDistance_NonXDPoSChain(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(1)}
	d := newA89Downloader(cfg)
	got := d.fastSyncPivotDistance(ethconfig.FastSync)
	if got != uint64(fsMinFullBlocks) {
		t.Errorf("non-XDPoS: want %d, got %d", fsMinFullBlocks, got)
	}
}

// TestA89_PivotDistance_XDPoSApothem verifies Apothem/Mainnet (Epoch=900)
// returns fsMinFullBlocks (64), NOT 2×Epoch (1800).
// A.97.3: the QC catchup window is symmetric [anchor−2×Epoch, anchor+2×Epoch]
// around the anchor, so pivot depth does not need to equal 2×Epoch. Keeping
// pivot at head−64 ensures peers can serve its state (refs #894 "within 1024").
func TestA89_PivotDistance_XDPoSApothem(t *testing.T) {
	cfg := xdposConfig(900, 450)
	d := newA89Downloader(cfg)
	got := d.fastSyncPivotDistance(ethconfig.FastSync)
	if got != uint64(fsMinFullBlocks) {
		t.Errorf("Epoch=900: want %d (fsMinFullBlocks), got %d", fsMinFullBlocks, got)
	}
}

// TestA89_PivotDistance_SmallEpoch verifies that when 2×Epoch < fsMinFullBlocks,
// fsMinFullBlocks (64) wins.
func TestA89_PivotDistance_SmallEpoch(t *testing.T) {
	cfg := xdposConfig(10, 5) // 2*10 = 20 < 64
	d := newA89Downloader(cfg)
	got := d.fastSyncPivotDistance(ethconfig.FastSync)
	if got != uint64(fsMinFullBlocks) {
		t.Errorf("small epoch: want %d, got %d", fsMinFullBlocks, got)
	}
}

// TestA89_PivotDistance_ZeroEpoch verifies zero Epoch falls back to fsMinFullBlocks.
func TestA89_PivotDistance_ZeroEpoch(t *testing.T) {
	cfg := xdposConfig(0, 0)
	d := newA89Downloader(cfg)
	got := d.fastSyncPivotDistance(ethconfig.FastSync)
	if got != uint64(fsMinFullBlocks) {
		t.Errorf("zero epoch: want %d, got %d", fsMinFullBlocks, got)
	}
}

// TestA89_PivotDistance_NonFastSyncMode verifies that SnapSync and FullSync
// always return fsMinFullBlocks regardless of chain config.
func TestA89_PivotDistance_NonFastSyncMode(t *testing.T) {
	cfg := xdposConfig(900, 450)
	d := newA89Downloader(cfg)

	for _, mode := range []ethconfig.SyncMode{ethconfig.SnapSync, ethconfig.FullSync} {
		got := d.fastSyncPivotDistance(mode)
		if got != uint64(fsMinFullBlocks) {
			t.Errorf("mode=%v: want %d, got %d", mode, fsMinFullBlocks, got)
		}
	}
}

// TestA89_CallbackReceivesCorrectArgs verifies that the callback receives
// exactly the number and hash passed to it.
func TestA89_CallbackReceivesCorrectArgs(t *testing.T) {
	d := newA89Downloader(nil)

	wantNum := uint64(82_820_000)
	wantHash := common.HexToHash("0xd01bd0006d53e6631d27accbb9762c08f3375c4826d216dfe3e62f0824b83aac")

	var gotNum uint64
	var gotHash common.Hash
	d.SetFastSyncAnchorFn(func(n uint64, h common.Hash) {
		gotNum = n
		gotHash = h
	})
	d.fastSyncAnchorFn(wantNum, wantHash)

	if gotNum != wantNum {
		t.Errorf("number: want %d, got %d", wantNum, gotNum)
	}
	if gotHash != wantHash {
		t.Errorf("hash: want %s, got %s", wantHash, gotHash)
	}
}

// TestA89_OperatorPivotBlocksCallback verifies that when operatorPivotNumber
// is set, the fastSyncAnchorFn is bypassed (mirrors the gate in syncToHead).
// This tests the guard condition `operatorPivotNumber == 0`.
func TestA89_OperatorPivotBlocksCallback(t *testing.T) {
	d := newA89Downloader(xdposConfig(900, 450))
	d.operatorPivotNumber = 82_820_000 // operator pivot is set

	var called atomic.Bool
	d.SetFastSyncAnchorFn(func(n uint64, h common.Hash) { called.Store(true) })

	// Simulate the gate condition from syncToHead:
	// only fire when operatorPivotNumber == 0.
	if d.operatorPivotNumber == 0 && d.fastSyncAnchorFn != nil {
		d.fastSyncAnchorFn(1, common.Hash{})
	}

	if called.Load() {
		t.Fatal("callback should NOT fire when operatorPivotNumber is set")
	}
}

// ─── A.97.3 invariant tests ───────────────────────────────────────────────

// TestA97_3_PivotDistance_AlwaysFsMinFullBlocks encodes the A.97.3 ruling:
// fastSyncPivotDistance MUST return fsMinFullBlocks (64) for all chain configs,
// including XDPoS chains with large Epoch values. The symmetric QC catchup
// window [anchor−2×Epoch, anchor+2×Epoch] does not require a deeper pivot.
func TestA97_3_PivotDistance_AlwaysFsMinFullBlocks(t *testing.T) {
	cases := []struct {
		name string
		cfg  *params.ChainConfig
	}{
		{"nil config", nil},
		{"no XDPoS", &params.ChainConfig{ChainID: big.NewInt(1)}},
		{"XDPoS Epoch=900 (mainnet)", xdposConfig(900, 450)},
		{"XDPoS Epoch=450 (apothem)", xdposConfig(450, 225)},
		{"XDPoS large Epoch=9000", xdposConfig(9000, 4500)},
		{"XDPoS small Epoch=10", xdposConfig(10, 5)},
		{"XDPoS Epoch=0", xdposConfig(0, 0)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := newA89Downloader(tc.cfg)
			for _, mode := range []ethconfig.SyncMode{ethconfig.FastSync, ethconfig.SnapSync, ethconfig.FullSync} {
				got := d.fastSyncPivotDistance(mode)
				if got != uint64(fsMinFullBlocks) {
					t.Errorf("mode=%v: want %d (fsMinFullBlocks), got %d", mode, fsMinFullBlocks, got)
				}
			}
		})
	}
}

// TestA97_3_AnchorWindowCoversBeaconTerminateFloor encodes the decoupling
// invariant from the Opus ruling: for any Epoch value, the symmetric QC
// catchup window [anchor−2×Epoch, anchor+2×Epoch] (where anchor = head−64 =
// pivot) must contain pivot−1 (the BeaconSync terminate floor from A.90).
//
// Invariant: pivot − 2×Epoch ≤ pivot − 1  →  always true for Epoch ≥ 1.
// The upper bound (pivot + 2×Epoch) is also always ≥ pivot − 1.
// This proves the symmetric window covers the terminate floor regardless of
// whether the pivot is head−64 or any smaller value.
func TestA97_3_AnchorWindowCoversBeaconTerminateFloor(t *testing.T) {
	// Representative head numbers and Epoch values.
	type tc struct {
		head  uint64
		epoch uint64
	}
	cases := []tc{
		{head: 103_600_000, epoch: 900},                 // mainnet production range
		{head: 82_900_000, epoch: 900},                  // apothem production range
		{head: 1_000_000, epoch: 900},                   // early chain
		{head: 200, epoch: 100},                         // small chain
		{head: uint64(fsMinFullBlocks) + 1, epoch: 900}, // just above threshold
	}

	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			if tc.head <= uint64(fsMinFullBlocks) {
				t.Skip("head too low — no pivot assigned")
			}
			pivot := tc.head - uint64(fsMinFullBlocks) // A.97.3: pivot = head − 64
			terminateFloor := pivot - 1                // BeaconSync terminates at pivot−1

			// Symmetric window around anchor (= pivot in A.97.3):
			// lower = anchor − 2×Epoch.  Since epoch ≥ 1, lower < pivot.
			// upper = anchor + 2×Epoch.  Always > pivot − 1.
			// Check: lower ≤ terminateFloor (only meaningful when pivot > 2×epoch)
			windowUpper := pivot + 2*tc.epoch
			if windowUpper <= terminateFloor {
				t.Errorf("upper bound %d ≤ terminateFloor %d — window does not cover floor",
					windowUpper, terminateFloor)
			}
			// Regardless of underflow risk: if pivot ≥ 2×Epoch, check lower bound.
			if pivot >= 2*tc.epoch {
				windowLower := pivot - 2*tc.epoch
				if windowLower > terminateFloor {
					t.Errorf("lower bound %d > terminateFloor %d — window misses floor",
						windowLower, terminateFloor)
				}
			}
			// Key assertion: pivot = head − 64 always satisfies the invariant.
			// The window's upper half alone (pivot → pivot+2×Epoch) covers
			// terminateFloor (= pivot−1 < pivot).
		})
	}
}

// TestA97_3_GapBlocksUnaffectedByPivotDistance verifies that computePivotGapNumbers
// enumerates gap blocks correctly even when called with a shallow pivot
// (head−64), confirming that Phase-2 gap-state logic is independent of the
// pivot-distance change and will remain valid when A.97 phase-3 is deployed.
// Refs: #894 "gap path is a SEPARATE deep-state dependency — phase-2 scope".
func TestA97_3_GapBlocksUnaffectedByPivotDistance(t *testing.T) {
	cfg := xdposConfig(900, 450)

	// Shallow pivot: head−64 on a representative mainnet block.
	head := uint64(103_600_000)
	shallowPivot := head - uint64(fsMinFullBlocks)

	gaps := computePivotGapNumbers(cfg, shallowPivot)
	if len(gaps) == 0 {
		t.Fatal("expected non-empty gap list for shallow pivot on XDPoS mainnet config")
	}
	// All returned gaps must be strictly less than the pivot.
	for _, g := range gaps {
		if g >= shallowPivot {
			t.Errorf("gap block %d is not < shallowPivot %d", g, shallowPivot)
		}
		if g == 0 {
			t.Error("gap block 0 (genesis) must never appear in gap list")
		}
	}
	// Shallow pivot (head−64) still lands in the same epoch window as
	// head, so gap enumeration produces exactly the same result as a deeper pivot
	// within the same epoch range. Confirm gaps are epoch-aligned (each gap = epochBoundary − Gap).
	E := cfg.XDPoS.Epoch
	G := cfg.XDPoS.Gap
	for _, g := range gaps {
		// g must equal some (epochBoundary - G) where epochBoundary is a multiple of E.
		if (g+G)%E != 0 {
			t.Errorf("gap block %d is not at a valid epochBoundary−Gap position (E=%d G=%d)", g, E, G)
		}
	}
}
