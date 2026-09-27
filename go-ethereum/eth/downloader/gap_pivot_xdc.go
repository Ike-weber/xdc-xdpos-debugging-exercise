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

// gap_pivot_xdc.go implements XDPoS-specific gap-block enumeration for fast
// sync. Without this, a fast-synced node fails at the first post-pivot epoch
// crossing because it lacks masternode-set state at gap blocks
// (epochBoundary - Gap). Phase A.5 of #844.
//
// See FAST_SYNC_IMPLEMENTATION.md §3.3 + §3.6-3.7.

package downloader

import (
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// GapSnapshotFn is a callback injected at Downloader construction time by
// eth/handler.go. It fetches trie state at the given root and materialises a
// masternode snapshot via engine_v2.UpdateMasternodesFromHeader so that the
// XDPoS consensus engine can resolve masternode sets at every post-pivot
// epoch crossing. The header must already be present in the local DB.
//
// Returning a non-nil error causes the fast-sync gap loop to abort. Returning
// nil means the snapshot was written successfully (or the block is not a gap
// block and was silently skipped).
//
// For non-XDPoS chains (or when the callback is not set), the gap loop is a
// no-op. Refs FAST_SYNC_IMPLEMENTATION.md §3.7.
type GapSnapshotFn func(gapNumber uint64, gapRoot [32]byte) error

// GapFallbackFn is a callback injected by eth/handler.go (via SetGapFallbackFn)
// that materialises a masternode snapshot for a gap block directly from the
// embedded header data, WITHOUT requiring trie state. It is invoked by
// processFastSyncContent when the gap-block state download fails because
// pruned mainnet peers cannot serve that state epoch (A.97.4, refs #894).
//
// The callback receives the gap block header (already downloaded during the
// fast-sync header phase) and must extract masternodes from header.Validators
// and penalties from header.Penalties, then store the V2 snapshot via
// engine_v2.UpdateMasternodesWithPenalties.
//
// Returning (masternodes, nil) means the snapshot was seeded successfully.
// Returning (0, non-nil) means the header lacks embedded masternode data;
// the caller should hard-abort (cannot continue without masternodes).
type GapFallbackFn func(gapHeader *types.Header) (masternodes int, err error)

// computePivotGapNumbers returns the list of XDPoS gap-block numbers that are
// strictly less than pivotNumber and for which state must also be downloaded
// during fast sync. The masternode set for each post-pivot epoch is resolved
// by the consensus engine from the gap block immediately preceding that epoch
// boundary (gapBlock = epochBoundary - Gap). Without state at those gap blocks
// the first epoch crossing after the pivot will fail.
//
// Algorithm (from FAST_SYNC_IMPLEMENTATION.md §3.3):
//
//	E          = chainConfig.XDPoS.Epoch   (e.g. 900 on XDC mainnet)
//	G          = chainConfig.XDPoS.Gap     (e.g. 450 on XDC mainnet)
//	epochBase  = pivotNumber - (pivotNumber % E)   // largest epoch boundary ≤ pivot
//	baseGap    = epochBase - G  if epochBase >= G
//	             E - G          otherwise  (genesis-era: first gap is E-G)
//	gaps       = { baseGap + E*i  |  i >= 0, baseGap + E*i < pivotNumber, value > 0 }
//
// Returns nil if:
//   - cfg is nil or cfg.XDPoS is nil (non-XDPoS chain)
//   - E == 0 or G >= E (misconfiguration; logs a warning)
func computePivotGapNumbers(cfg *params.ChainConfig, pivotNumber uint64) []uint64 {
	if cfg == nil || cfg.XDPoS == nil {
		return nil
	}
	E := cfg.XDPoS.Epoch
	G := cfg.XDPoS.Gap
	if E == 0 {
		log.Warn("XDPoS fast sync: Epoch is zero; skipping gap-pivot enumeration")
		return nil
	}
	if G >= E {
		log.Warn("XDPoS fast sync: Gap >= Epoch (misconfiguration); skipping gap-pivot enumeration",
			"Gap", G, "Epoch", E)
		return nil
	}

	epochBase := pivotNumber - (pivotNumber % E)
	var baseGap uint64
	if epochBase >= G {
		baseGap = epochBase - G
	} else {
		// Genesis-era: epochBase < G, so the first gap block sits at E-G
		// (the gap preceding the very first epoch boundary at E).
		baseGap = E - G
	}

	var gaps []uint64
	for n := baseGap; n < pivotNumber; n += E {
		if n == 0 {
			// Skip block 0 (genesis): no state to fetch, no masternode SMC.
			continue
		}
		gaps = append(gaps, n)
	}
	return gaps
}
