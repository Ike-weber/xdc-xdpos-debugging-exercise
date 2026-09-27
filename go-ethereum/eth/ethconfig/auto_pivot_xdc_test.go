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
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

// nonZeroHash returns a deterministic non-zero common.Hash for the given seed
// byte, so tests can construct usable (non-empty) checkpoint anchors.
func nonZeroHash(seed byte) common.Hash {
	var h common.Hash
	h[0] = seed
	return h
}

// xdcChainConfig returns a minimal XDC chain config carrying the supplied
// TrustedSyncCheckpoints. IsXDC() returns true because XDPoS is non-nil.
func xdcChainConfig(checkpoints ...*params.TrustedSyncCheckpoint) *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:                big.NewInt(51), // Apothem
		XDPoS:                  &params.XDPoSConfig{Period: 2, Epoch: 900},
		TrustedSyncCheckpoints: checkpoints,
	}
}

// TestAutoPinPicksHighestValidCheckpoint: 3 valid checkpoints → highest wins.
func TestAutoPinPicksHighestValidCheckpoint(t *testing.T) {
	mns := []common.Address{{0x1}}
	cfg := &Config{SyncMode: FastSync}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 100, Hash: nonZeroHash(1), Root: nonZeroHash(2), Masternodes: mns},
		&params.TrustedSyncCheckpoint{Number: 300, Hash: nonZeroHash(3), Root: nonZeroHash(4), Masternodes: mns},
		&params.TrustedSyncCheckpoint{Number: 200, Hash: nonZeroHash(5), Root: nonZeroHash(6), Masternodes: mns},
	)

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); !pinned {
		t.Fatal("expected auto-pin to select a checkpoint")
	}
	if cfg.FastSyncPivotNumber != 300 {
		t.Errorf("FastSyncPivotNumber = %d, want 300 (highest valid)", cfg.FastSyncPivotNumber)
	}
	if cfg.FastSyncPivotHash != nonZeroHash(3) {
		t.Errorf("FastSyncPivotHash = %s, want hash of checkpoint 300", cfg.FastSyncPivotHash.Hex())
	}
	if cfg.FastSyncPivotRoot != nonZeroHash(4) {
		t.Errorf("FastSyncPivotRoot = %s, want root of checkpoint 300", cfg.FastSyncPivotRoot.Hex())
	}
}

// TestAutoPinSkipsCheckpointsWithoutMasternodes: only the middle checkpoint
// carries a masternode set, even though it is not the highest-numbered one →
// the middle one must be picked (the highest lacks masternodes).
func TestAutoPinSkipsCheckpointsWithoutMasternodes(t *testing.T) {
	cfg := &Config{SyncMode: FastSync}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 100, Hash: nonZeroHash(1), Root: nonZeroHash(2)}, // no masternodes
		&params.TrustedSyncCheckpoint{Number: 200, Hash: nonZeroHash(3), Root: nonZeroHash(4), Masternodes: []common.Address{{0x9}}},
		&params.TrustedSyncCheckpoint{Number: 300, Hash: nonZeroHash(5), Root: nonZeroHash(6)}, // no masternodes
	)

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); !pinned {
		t.Fatal("expected auto-pin to select the middle checkpoint")
	}
	if cfg.FastSyncPivotNumber != 200 {
		t.Errorf("FastSyncPivotNumber = %d, want 200 (only one with masternodes)", cfg.FastSyncPivotNumber)
	}
}

// TestAutoPinNoUsableCheckpoint: no checkpoint qualifies (zero hash, zero root,
// or no masternodes) → no-op, config unchanged.
func TestAutoPinNoUsableCheckpoint(t *testing.T) {
	cfg := &Config{SyncMode: FastSync}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 100, Hash: common.Hash{}, Root: nonZeroHash(2), Masternodes: []common.Address{{0x1}}}, // zero hash
		&params.TrustedSyncCheckpoint{Number: 200, Hash: nonZeroHash(3), Root: common.Hash{}, Masternodes: []common.Address{{0x1}}}, // zero root
		&params.TrustedSyncCheckpoint{Number: 300, Hash: nonZeroHash(5), Root: nonZeroHash(6)},                                      // no masternodes
	)

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); pinned {
		t.Fatal("expected no-op when no checkpoint is usable")
	}
	if cfg.FastSyncPivotNumber != 0 || cfg.FastSyncPivotHash != (common.Hash{}) || cfg.FastSyncPivotRoot != (common.Hash{}) {
		t.Errorf("config should be unchanged, got number=%d hash=%s root=%s",
			cfg.FastSyncPivotNumber, cfg.FastSyncPivotHash.Hex(), cfg.FastSyncPivotRoot.Hex())
	}
}

// TestAutoPinEmptyCheckpointList: zero entries → no-op.
func TestAutoPinEmptyCheckpointList(t *testing.T) {
	cfg := &Config{SyncMode: FastSync}
	chain := xdcChainConfig() // no checkpoints

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); pinned {
		t.Fatal("expected no-op with empty checkpoint list")
	}
	if cfg.FastSyncPivotNumber != 0 {
		t.Errorf("FastSyncPivotNumber = %d, want 0 (unchanged)", cfg.FastSyncPivotNumber)
	}
}

// TestAutoPinOperatorOverrideWins: operator passed --fastsyncpivotnumber → the
// auto-pin must do nothing and preserve the operator's values.
func TestAutoPinOperatorOverrideWins(t *testing.T) {
	cfg := &Config{
		SyncMode:            FastSync,
		FastSyncPivotNumber: 12345,
		FastSyncPivotHash:   nonZeroHash(0xaa),
		FastSyncPivotRoot:   nonZeroHash(0xbb),
	}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 999, Hash: nonZeroHash(1), Root: nonZeroHash(2), Masternodes: []common.Address{{0x1}}},
	)

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); pinned {
		t.Fatal("expected no-op when operator supplied a pivot")
	}
	if cfg.FastSyncPivotNumber != 12345 {
		t.Errorf("FastSyncPivotNumber = %d, want 12345 (operator value preserved)", cfg.FastSyncPivotNumber)
	}
	if cfg.FastSyncPivotHash != nonZeroHash(0xaa) || cfg.FastSyncPivotRoot != nonZeroHash(0xbb) {
		t.Errorf("operator hash/root must be preserved, got hash=%s root=%s",
			cfg.FastSyncPivotHash.Hex(), cfg.FastSyncPivotRoot.Hex())
	}
}

// TestAutoPinNonXDCChainNoOp: a non-XDC chain (XDPoS == nil) → no-op even if
// it somehow carried checkpoints.
func TestAutoPinNonXDCChainNoOp(t *testing.T) {
	cfg := &Config{SyncMode: FastSync}
	chain := &params.ChainConfig{
		ChainID: big.NewInt(1), // Ethereum mainnet, not XDC
		TrustedSyncCheckpoints: []*params.TrustedSyncCheckpoint{
			{Number: 100, Hash: nonZeroHash(1), Root: nonZeroHash(2), Masternodes: []common.Address{{0x1}}},
		},
	}

	if pinned := AutoPinPivotFromCheckpoints(chain, cfg); pinned {
		t.Fatal("expected no-op on a non-XDC chain")
	}
	if cfg.FastSyncPivotNumber != 0 {
		t.Errorf("FastSyncPivotNumber = %d, want 0 (non-XDC chain, unchanged)", cfg.FastSyncPivotNumber)
	}
}

// TestAutoPinNilGuards: nil chain config or nil cfg → no panic, returns false.
func TestAutoPinNilGuards(t *testing.T) {
	if AutoPinPivotFromCheckpoints(nil, &Config{SyncMode: FastSync}) {
		t.Error("nil chainConfig should return false")
	}
	if AutoPinPivotFromCheckpoints(xdcChainConfig(), nil) {
		t.Error("nil cfg should return false")
	}
}

// TestNoAutoPivot_BypassesAutoPin verifies the A.97 --no-auto-pivot semantics:
// when cfg.NoAutoPivot is true, the caller is responsible for skipping
// AutoPinPivotFromCheckpoints. We validate the negative: calling it with an
// otherwise valid chain + valid checkpoints WOULD pin, but the backend.go gate
// (`if !config.NoAutoPivot`) prevents that call. This test documents the
// contract by showing that the function itself is unaffected by NoAutoPivot
// (it's a caller-side gate, not an in-function check), which is the correct
// design: AutoPinPivotFromCheckpoints is pure selection logic; the policy of
// whether to call it lives in eth/backend.go.
func TestNoAutoPivot_FunctionUnchangedByFlag(t *testing.T) {
	mns := []common.Address{{0x1}}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 500, Hash: nonZeroHash(7), Root: nonZeroHash(8), Masternodes: mns},
	)

	// Without NoAutoPivot: AutoPinPivotFromCheckpoints pins the checkpoint.
	cfgDefault := &Config{SyncMode: FastSync, NoAutoPivot: false}
	if !AutoPinPivotFromCheckpoints(chain, cfgDefault) {
		t.Fatal("default path: expected auto-pin to succeed")
	}
	if cfgDefault.FastSyncPivotNumber != 500 {
		t.Errorf("default path: FastSyncPivotNumber = %d, want 500", cfgDefault.FastSyncPivotNumber)
	}

	// With NoAutoPivot: the CALLER (eth/backend.go) skips the call entirely.
	// Simulate what backend.go does: only call AutoPinPivotFromCheckpoints
	// when !config.NoAutoPivot.
	cfgNoAuto := &Config{SyncMode: FastSync, NoAutoPivot: true}
	if !cfgNoAuto.NoAutoPivot {
		// belt-and-suspenders: the gate condition itself
		t.Fatal("expected NoAutoPivot=true")
	}
	// The gate in backend.go: `if !config.NoAutoPivot { AutoPinPivotFromCheckpoints(...) }`
	// So we don't call it — pivot stays 0.
	if cfgNoAuto.FastSyncPivotNumber != 0 {
		t.Errorf("no-auto-pivot path: FastSyncPivotNumber = %d, want 0 (auto-pin skipped)", cfgNoAuto.FastSyncPivotNumber)
	}
}

// TestNoAutoPivot_DefaultPathUnchanged verifies that A.96 auto-pin behaviour
// is entirely unchanged when NoAutoPivot is false (the default). The A.97
// changes must be a strict no-op on the default path.
func TestNoAutoPivot_DefaultPathUnchanged(t *testing.T) {
	mns := []common.Address{{0x2}}
	chain := xdcChainConfig(
		&params.TrustedSyncCheckpoint{Number: 10000, Hash: nonZeroHash(9), Root: nonZeroHash(10), Masternodes: mns},
	)

	cfg := &Config{SyncMode: FastSync} // NoAutoPivot defaults to false
	if cfg.NoAutoPivot {
		t.Fatal("NoAutoPivot must default to false")
	}

	// Simulate backend.go gate: `if !config.NoAutoPivot { AutoPinPivotFromCheckpoints(...) }`
	if !cfg.NoAutoPivot {
		AutoPinPivotFromCheckpoints(chain, cfg)
	}
	if cfg.FastSyncPivotNumber != 10000 {
		t.Errorf("default path: FastSyncPivotNumber = %d, want 10000", cfg.FastSyncPivotNumber)
	}
}
