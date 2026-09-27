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
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// xdposConfig builds a minimal ChainConfig with XDPoS set to the given
// Epoch and Gap values. V2.SwitchBlock = 0 so the config is always V2-era.
func xdposConfig(epoch, gap uint64) *params.ChainConfig {
	return &params.ChainConfig{
		ChainID: big.NewInt(51),
		XDPoS: &params.XDPoSConfig{
			Epoch: epoch,
			Gap:   gap,
		},
	}
}

// TestComputePivotGapNumbers_MainnetParams exercises the XDC mainnet parameters
// (Epoch=900, Gap=450) with a pivot near the production block range.
func TestComputePivotGapNumbers_MainnetParams(t *testing.T) {
	// Pivot at exactly 80,000,000.
	// 80,000,000 % 900 = 800  (80,000,000 / 900 = 88,888 r 800)
	// epochBase = 80,000,000 - 800 = 79,999,200
	// baseGap   = 79,999,200 - 450 = 79,998,750
	// gaps:
	//   79,998,750 < 80,000,000 ✓
	//   79,998,750 + 900 = 79,999,650 < 80,000,000 ✓
	//   79,999,650 + 900 = 80,000,550 >= 80,000,000 → stop
	// → [79,998,750, 79,999,650]
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 80_000_000)
	want := []uint64{79_998_750, 79_999_650}
	if len(got) != len(want) {
		t.Fatalf("mainnet pivot=80_000_000: want %v, got %v", want, got)
	}
	for i, g := range want {
		if got[i] != g {
			t.Errorf("gap[%d]: want %d, got %d", i, g, got[i])
		}
	}
}

// TestComputePivotGapNumbers_PivotAtEpochBoundary checks that a pivot that
// lands exactly on an epoch boundary produces gaps below it.
func TestComputePivotGapNumbers_PivotAtEpochBoundary(t *testing.T) {
	// E=900, G=450, pivot=900 (first epoch boundary).
	// epochBase = 900 - 0 = 900
	// baseGap   = 900 - 450 = 450
	// gaps      = 450 (450 < 900 ✓), next would be 1350 (≥ 900 → stop)
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 900)
	if len(got) != 1 || got[0] != 450 {
		t.Errorf("pivot=epoch boundary 900: want [450], got %v", got)
	}
}

// TestComputePivotGapNumbers_MultipleGaps verifies that a pivot spanning
// several epochs produces all gap blocks.
func TestComputePivotGapNumbers_MultipleGaps(t *testing.T) {
	// E=900, G=450, pivot=3000.
	// epochBase = 3000 - (3000 % 900) = 3000 - 300 = 2700
	// baseGap   = 2700 - 450 = 2250
	// gaps      = 2250, (2250+900=3150 ≥ 3000 → stop)
	// → [2250]
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 3000)
	want := []uint64{2250}
	if len(got) != len(want) {
		t.Fatalf("pivot=3000: want %v, got %v", want, got)
	}
	for i, g := range want {
		if got[i] != g {
			t.Errorf("gap[%d]: want %d, got %d", i, g, got[i])
		}
	}
}

// TestComputePivotGapNumbers_SmallPivot checks behaviour when the pivot is
// larger than the first gap but still within the first epoch.
func TestComputePivotGapNumbers_SmallPivot(t *testing.T) {
	// E=900, G=450, pivot=700.
	// epochBase = 700 - 700%900 = 700 - 700 = 0  (genesis-era)
	// baseGap   = E - G = 900 - 450 = 450
	// gaps      = 450 (450 < 700 ✓), next = 1350 (≥ 700 → stop)
	// → [450]
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 700)
	if len(got) != 1 || got[0] != 450 {
		t.Errorf("pivot=700: want [450], got %v", got)
	}
}

// TestComputePivotGapNumbers_PivotBelowFirstGap verifies that a pivot number
// that is BELOW the first gap block returns an empty slice (nothing to download).
func TestComputePivotGapNumbers_PivotBelowFirstGap(t *testing.T) {
	// E=900, G=450, pivot=400.
	// epochBase = 0 (genesis-era)
	// baseGap   = 450
	// gaps:  450 >= 400 → loop body never executes → nil
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 400)
	if len(got) != 0 {
		t.Errorf("pivot=400 (below first gap): want [], got %v", got)
	}
}

// TestComputePivotGapNumbers_PivotInGenesisEpoch checks a very early pivot
// (< Epoch) with no gap block preceding it.
func TestComputePivotGapNumbers_PivotInGenesisEpoch(t *testing.T) {
	// E=900, G=450, pivot=200.
	// epochBase = 0, baseGap = 450; 450 >= 200 → no gaps.
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 200)
	if len(got) != 0 {
		t.Errorf("pivot=200 (genesis epoch): want [], got %v", got)
	}
}

// TestComputePivotGapNumbers_NilConfig confirms nil config returns nil.
func TestComputePivotGapNumbers_NilConfig(t *testing.T) {
	got := computePivotGapNumbers(nil, 80_000_000)
	if got != nil {
		t.Errorf("nil config: want nil, got %v", got)
	}
}

// TestComputePivotGapNumbers_NilXDPoS confirms a config without XDPoS returns nil.
func TestComputePivotGapNumbers_NilXDPoS(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(1)}
	got := computePivotGapNumbers(cfg, 80_000_000)
	if got != nil {
		t.Errorf("non-XDPoS config: want nil, got %v", got)
	}
}

// TestComputePivotGapNumbers_MisconfigGapGEEpoch guards against G >= E.
func TestComputePivotGapNumbers_MisconfigGapGEEpoch(t *testing.T) {
	cfg := xdposConfig(900, 900) // G == E
	got := computePivotGapNumbers(cfg, 80_000_000)
	if got != nil {
		t.Errorf("G==E: want nil, got %v", got)
	}

	cfg2 := xdposConfig(900, 1000) // G > E
	got2 := computePivotGapNumbers(cfg2, 80_000_000)
	if got2 != nil {
		t.Errorf("G>E: want nil, got %v", got2)
	}
}

// TestComputePivotGapNumbers_ZeroEpoch guards against E == 0.
func TestComputePivotGapNumbers_ZeroEpoch(t *testing.T) {
	cfg := xdposConfig(0, 0)
	got := computePivotGapNumbers(cfg, 80_000_000)
	if got != nil {
		t.Errorf("E==0: want nil, got %v", got)
	}
}

// TestComputePivotGapNumbers_PivotExactlyOneAfterGap verifies a pivot that is
// exactly gap+1 (the gap block just entered the eligible range).
func TestComputePivotGapNumbers_PivotExactlyOneAfterGap(t *testing.T) {
	// E=900, G=450: first gap = 450. pivot=451 → epochBase=0 → baseGap=450.
	// 450 < 451 → gaps = [450].
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 451)
	if len(got) != 1 || got[0] != 450 {
		t.Errorf("pivot=451: want [450], got %v", got)
	}
}

// TestComputePivotGapNumbers_PivotExactlyAtGap confirms that when pivot ==
// gapBlock, that gap is NOT included (loop predicate is n < pivotNumber).
func TestComputePivotGapNumbers_PivotExactlyAtGap(t *testing.T) {
	// E=900, G=450, pivot=450 → epochBase=0, baseGap=450.
	// n=450 < 450 is false → no gaps.
	cfg := xdposConfig(900, 450)
	got := computePivotGapNumbers(cfg, 450)
	if len(got) != 0 {
		t.Errorf("pivot==firstGap: want [], got %v", got)
	}
}
