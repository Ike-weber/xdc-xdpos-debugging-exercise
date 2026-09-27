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

package ethconfig

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// AutoPinPivotFromCheckpoints implements XDC A.96 (refs #892).
//
// When an operator runs `--syncmode fast` WITHOUT the
// --fastsyncpivot{number,hash,root} triplet, the modern v1.17.3 downloader
// otherwise falls back to dynamic-pivot selection. The dynamic pivot moves
// during the multi-hour state download, producing a non-deterministic
// incomplete trie → `invalid merkle root` BAD BLOCK → A.94 recovery →
// checkpointSyncNoState persisted → node not validator-ready (root cause of
// #886, papered over by the A.92/A.93/A.94/A.95 series).
//
// Legacy v2.7.0-devnet operators have always sidestepped this by pinning the
// pivot triplet explicitly. A.96 makes that the default: when the operator
// omits the flags, auto-select the highest TrustedSyncCheckpoint baked into
// the chain config that carries a real anchor (non-zero hash + non-zero root)
// and an embedded masternode set, and use it as the deterministic pivot. The
// same selection predicate already proved out for the experimental snap-sync
// path in eth/sync_xdc.go (xdcSyncer.snapSelectOnce).
//
// Behaviour contract (from #892):
//   - Operator override always wins: if cfg.FastSyncPivotNumber is already set
//     (the triplet was supplied), this is a no-op. The caller must only invoke
//     this when no operator flags were passed.
//   - Only applies to XDC chains (chainConfig.IsXDC()). Non-XDC chains are a
//     no-op so upstream-shared behaviour is untouched.
//   - Graceful degradation: if no usable TrustedSyncCheckpoint exists (zero
//     entries, or none with a masternode set), log a WARN and leave cfg
//     unchanged so the existing dynamic-pivot path still runs. Never fatal —
//     devnet / private-chain operators stay unblocked.
//   - On success, set cfg.FastSyncPivot{Number,Hash,Root} so the rest of the
//     fast-sync code path (A.6 operator-pin wiring in eth/backend.go) sees a
//     pinned pivot through the normal config fields. Nothing downstream changes.
//
// Returns true if a pivot was auto-pinned, false otherwise.
func AutoPinPivotFromCheckpoints(chainConfig *params.ChainConfig, cfg *Config) bool {
	if cfg == nil || chainConfig == nil {
		return false
	}
	// Operator override wins — if the triplet was supplied, FastSyncPivotNumber
	// is non-zero (cmd/utils/flags.go enforces all-or-nothing). Do nothing.
	if cfg.FastSyncPivotNumber != 0 {
		return false
	}
	// Only XDC chains carry TrustedSyncCheckpoints with embedded masternodes.
	if !chainConfig.IsXDC() {
		return false
	}

	best := selectAutoPivotCheckpoint(chainConfig.TrustedSyncCheckpoints)
	if best == nil {
		log.Warn("XDC fast-sync: no usable TrustedSyncCheckpoint to auto-pin pivot; "+
			"falling back to dynamic-pivot selection (refs #892)",
			"checkpoints", len(chainConfig.TrustedSyncCheckpoints))
		return false
	}

	cfg.FastSyncPivotNumber = best.Number
	cfg.FastSyncPivotHash = best.Hash
	cfg.FastSyncPivotRoot = best.Root

	log.Info("XDC fast-sync: auto-pinned pivot from TrustedSyncCheckpoints",
		"block", best.Number,
		"hash", best.Hash.Hex(),
		"root", best.Root.Hex())
	return true
}

// selectAutoPivotCheckpoint returns the highest-numbered TrustedSyncCheckpoint
// that is a usable fast-sync anchor: non-zero canonical hash, non-zero state
// root, and a non-empty embedded masternode set. The masternode set is what
// lets InsertHeadersBeforeCutoff seed the V2 snapshot without contract state
// the fresh node does not yet have; a checkpoint without it cannot anchor a
// validator-ready sync, so it is skipped. Returns nil if none qualify.
func selectAutoPivotCheckpoint(checkpoints []*params.TrustedSyncCheckpoint) *params.TrustedSyncCheckpoint {
	var best *params.TrustedSyncCheckpoint
	for _, cp := range checkpoints {
		if cp == nil {
			continue
		}
		if cp.Hash == (common.Hash{}) || cp.Root == (common.Hash{}) {
			continue
		}
		if len(cp.Masternodes) == 0 {
			continue
		}
		if best == nil || cp.Number > best.Number {
			best = cp
		}
	}
	return best
}
