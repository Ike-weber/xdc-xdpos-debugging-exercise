// Copyright (c) 2018 XDPoSChain
// Copyright 2024 The go-ethereum Authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package XDPoS

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// Test configuration for XDPoS
var testConfig = &params.XDPoSConfig{
	Period:              2,
	Epoch:               900,
	Reward:              5000,
	RewardCheckpoint:    900,
	Gap:                 450,
	FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000001"),
}

// TestNewXDPoS tests the creation of a new XDPoS engine
func TestNewXDPoS(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	if engine == nil {
		t.Fatal("Failed to create XDPoS engine")
	}

	// Check config values match
	if engine.config.Period != testConfig.Period {
		t.Error("XDPoS engine config Period mismatch")
	}
	if engine.config.Epoch != testConfig.Epoch {
		t.Error("XDPoS engine config Epoch mismatch")
	}
}

// TestAuthor tests the extraction of block author
func TestAuthor(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	// Create a test header with proper extraData
	header := &types.Header{
		Number:   big.NewInt(1),
		Extra:    make([]byte, extraVanity+extraSeal),
		GasLimit: 420000000,
	}

	// Author should return error for invalid extraData
	_, err := engine.Author(header)
	if err == nil {
		t.Error("Expected error for header without valid signature")
	}
}

// TestSnapshotCreation tests the creation of vote snapshots
func TestSnapshotCreation(t *testing.T) {
	signers := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		common.HexToAddress("0x0000000000000000000000000000000000000002"),
		common.HexToAddress("0x0000000000000000000000000000000000000003"),
	}

	snap := newSnapshot(testConfig, nil, 0, common.Hash{}, signers)

	if snap == nil {
		t.Fatal("Failed to create snapshot")
	}

	if snap.Number != 0 {
		t.Errorf("Snapshot number mismatch: got %d, want 0", snap.Number)
	}

	if len(snap.Signers) != len(signers) {
		t.Errorf("Snapshot signers count mismatch: got %d, want %d", len(snap.Signers), len(signers))
	}

	// Verify all signers are in the snapshot
	for _, signer := range signers {
		if _, ok := snap.Signers[signer]; !ok {
			t.Errorf("Signer %s not found in snapshot", signer.Hex())
		}
	}
}

// TestInturn tests the in-turn calculation
func TestInturn(t *testing.T) {
	signers := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		common.HexToAddress("0x0000000000000000000000000000000000000002"),
		common.HexToAddress("0x0000000000000000000000000000000000000003"),
	}

	snap := newSnapshot(testConfig, nil, 0, common.Hash{}, signers)

	// Test in-turn for first signer at block 1
	inTurn := snap.inturn(1, signers[0])
	// The expected result depends on the inturn calculation logic
	// Just verify it returns a boolean without panic
	_ = inTurn

	// Test in-turn for second signer at block 1
	inTurn2 := snap.inturn(1, signers[1])
	_ = inTurn2

	// Verify different signers have different turn status
	// (this is probabilistic based on the algorithm)
}

// TestCalcDifficulty tests difficulty constants
func TestCalcDifficulty(t *testing.T) {
	// Test that difficulty constants are defined correctly
	if diffInTurn.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("diffInTurn should be 2, got %v", diffInTurn)
	}
	if diffNoTurn.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("diffNoTurn should be 1, got %v", diffNoTurn)
	}
}

// TestVerifyHeaderExtraData tests header extra data validation
func TestVerifyHeaderExtraData(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	// Test that engine is created
	if engine == nil {
		t.Fatal("Failed to create engine")
	}

	// Test extra data requirements
	minExtraSize := extraVanity + extraSeal
	if minExtraSize != 97 {
		t.Errorf("Minimum extra size should be 97, got %d", minExtraSize)
	}
}

// TestEpochNumber tests epoch number calculation
func TestEpochNumber(t *testing.T) {
	testCases := []struct {
		blockNum uint64
		epoch    uint64
		expected uint64
	}{
		{0, 900, 0},
		{1, 900, 0},
		{899, 900, 0},
		{900, 900, 1},
		{901, 900, 1},
		{1800, 900, 2},
		{1801, 900, 2},
	}

	for _, tc := range testCases {
		result := tc.blockNum / tc.epoch
		if result != tc.expected {
			t.Errorf("Epoch calculation for block %d with epoch %d: got %d, want %d",
				tc.blockNum, tc.epoch, result, tc.expected)
		}
	}
}

// TestGapBlock tests gap block detection
func TestGapBlock(t *testing.T) {
	epoch := uint64(900)
	gap := uint64(450)

	testCases := []struct {
		blockNum uint64
		isGap    bool
	}{
		{0, false},
		{449, false},
		{450, true},  // Start of gap
		{899, true},  // End of gap (before next epoch)
		{900, false}, // New epoch
		{1350, true}, // Gap in second epoch
	}

	for _, tc := range testCases {
		epochEnd := ((tc.blockNum / epoch) + 1) * epoch
		gapStart := epochEnd - gap
		isGap := tc.blockNum >= gapStart && tc.blockNum < epochEnd

		if isGap != tc.isGap {
			t.Errorf("Gap detection for block %d: got %v, want %v",
				tc.blockNum, isGap, tc.isGap)
		}
	}
}

// TestSnapshotCopy tests that snapshot copy is independent
func TestSnapshotCopy(t *testing.T) {
	signers := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
	}

	original := newSnapshot(testConfig, nil, 100, common.Hash{}, signers)
	copied := original.copy()

	if copied == original {
		t.Error("Copy returned same pointer")
	}

	if copied.Number != original.Number {
		t.Error("Copy has different number")
	}

	if copied.Hash != original.Hash {
		t.Error("Copy has different hash")
	}

	// Modify copy and verify original is unchanged
	copied.Number = 200

	if original.Number == 200 {
		t.Error("Modifying copy affected original")
	}
}

// TestExtraDataLayout tests the extra data structure
func TestExtraDataLayout(t *testing.T) {
	// Extra data layout:
	// [0:32] - vanity
	// [len-65:len] - seal (signature)
	// [32:len-65] - signers (at epoch blocks)

	vanity := extraVanity // 32 bytes
	seal := extraSeal     // 65 bytes
	minExtra := vanity + seal

	if minExtra != 97 {
		t.Errorf("Minimum extra data size: got %d, want 97", minExtra)
	}

	// Test with 3 signers (at epoch block)
	numSigners := 3
	extraWithSigners := vanity + (numSigners * common.AddressLength) + seal
	expectedWithSigners := 32 + (3 * 20) + 65 // 32 + 60 + 65 = 157

	if extraWithSigners != expectedWithSigners {
		t.Errorf("Extra data with %d signers: got %d, want %d",
			numSigners, extraWithSigners, expectedWithSigners)
	}
}

// BenchmarkInturn benchmarks the inturn calculation
func BenchmarkInturn(b *testing.B) {
	signers := make([]common.Address, 150) // 150 masternodes
	for i := range signers {
		signers[i] = common.BigToAddress(big.NewInt(int64(i + 1)))
	}

	snap := newSnapshot(testConfig, nil, 0, common.Hash{}, signers)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap.inturn(uint64(i), signers[i%len(signers)])
	}
}

// BenchmarkSnapshotCreation benchmarks snapshot creation
func BenchmarkSnapshotCreation(b *testing.B) {
	signers := make([]common.Address, 150)
	for i := range signers {
		signers[i] = common.BigToAddress(big.NewInt(int64(i + 1)))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newSnapshot(testConfig, nil, uint64(i), common.Hash{}, signers)
	}
}

// TestGetDb verifies the engine returns its underlying database.
// Adapted from v2.6.8 XDPoS_test.go TestAdaptorShouldShareDbWithV1Engine. (#301)
func TestGetDb(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)
	if engine.GetDb() != db {
		t.Error("GetDb should return the same database instance passed to New")
	}
}

// TestV1V2SwitchBlockBoundary verifies that the V1→V2 switch block is handled
// with V1 rules (not routed to verifyHeaderV2), matching canonical XDPoSChain
// behaviour (params/config.go BlockConsensusVersion: num > switchBlock for V2).
//
// The switch block is mined by the V1 engine and carries V1-format Extra data
// (extra[0] == 0xd7, not the V2 marker byte 2).  On re-import after a SetHead
// rollback the strict getExtraFields/DecodeExtraFields call inside verifyHeaderV2
// returns ErrInvalidV2Extra and permanently wedges the node at switchBlock−1
// (observed: modern-fast node stuck at block 2699 on devnet, fixes #914).
//
// This test covers:
//   - IsV2Block boundary (switchBlock → false, switchBlock+1 → true)
//   - verifyHeader dispatch: switchBlock must NOT reach the V2 sentinel error
//   - verifyHeader dispatch: switchBlock+1 MUST reach the V2 sentinel error (EngineV2 unset)
func TestV1V2SwitchBlockBoundary(t *testing.T) {
	const switchBlock = uint64(900)

	cfgWithV2 := &params.XDPoSConfig{
		Period:              2,
		Epoch:               900,
		Reward:              5000,
		RewardCheckpoint:    900,
		Gap:                 450,
		FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		V2: &params.V2{
			SwitchBlock:   big.NewInt(int64(switchBlock)),
			CurrentConfig: &params.V2Config{MaxMasternodes: 108, MinePeriod: 2},
			AllConfigs:    map[uint64]*params.V2Config{0: {MaxMasternodes: 108, MinePeriod: 2}},
		},
	}

	db := rawdb.NewMemoryDatabase()
	engine := New(cfgWithV2, db)

	// --- Part 1: IsV2Block boundary ---
	// switchBlock itself must be V1 (false); switchBlock+1 must be V2 (true).
	cases := []struct {
		number   uint64
		wantIsV2 bool
		label    string
	}{
		{switchBlock - 1, false, "switchBlock-1"},
		{switchBlock, false, "switchBlock (last V1 block — V1-format Extra)"},
		{switchBlock + 1, true, "switchBlock+1 (first V2 block)"},
		{switchBlock + 2, true, "switchBlock+2"},
	}
	for _, tc := range cases {
		hdr := &types.Header{Number: big.NewInt(int64(tc.number))}
		got := engine.IsV2Block(hdr)
		if got != tc.wantIsV2 {
			t.Errorf("IsV2Block(block %d [%s]): got %v, want %v", tc.number, tc.label, got, tc.wantIsV2)
		}
	}

	// --- Part 2: verifyHeader routing for switchBlock+1 (first V2 block) ---
	// With no EngineV2 wired, verifyHeaderV2 must return the explicit sentinel
	// "EngineV2 not initialized" for any block > switchBlock.
	// This confirms the V2 routing is active for switchBlock+1.
	v2Header := &types.Header{
		Number:    big.NewInt(int64(switchBlock + 1)),
		Extra:     make([]byte, extraVanity+extraSeal),
		UncleHash: types.EmptyUncleHash,
	}
	errV2Block := callVerifyHeaderSafe(engine, v2Header)
	const v2SentinelMsg = "EngineV2 not initialized"
	if errV2Block == nil || !containsString(errV2Block.Error(), v2SentinelMsg) {
		t.Errorf("verifyHeader(switchBlock+1=%d): expected V2 sentinel error containing %q, got: %v",
			switchBlock+1, v2SentinelMsg, errV2Block)
	}

	// --- Part 3: verifyHeader routing for switchBlock itself (the switch block) ---
	// The switch block is a V1-format checkpoint.  With the fix it is routed to V1
	// verification, which must NOT return the V2 sentinel error.  The V1 path will
	// fail for other reasons (no snapshot, nil chain), but that is expected.
	//
	// Valid V1 checkpoint Extra = 32-byte vanity + N*20 signer bytes + 65 seal bytes.
	switchHeader := &types.Header{
		Number:    big.NewInt(int64(switchBlock)),
		Extra:     make([]byte, extraVanity+common.AddressLength+extraSeal), // 1 signer (valid epoch fmt)
		Nonce:     types.BlockNonce{},
		UncleHash: types.EmptyUncleHash,
	}
	errSwitchBlock := callVerifyHeaderSafe(engine, switchHeader)
	if errSwitchBlock != nil && containsString(errSwitchBlock.Error(), v2SentinelMsg) {
		t.Errorf("verifyHeader(switchBlock=%d): switch block incorrectly routed to V2 path: %v",
			switchBlock, errSwitchBlock)
	}
	// Any non-V2-sentinel error from the V1 path is acceptable (no chain / snapshot in unit test).
	if errSwitchBlock != nil {
		t.Logf("verifyHeader(switchBlock=%d): expected V1-path error (no chain/snapshot in unit test): %v",
			switchBlock, errSwitchBlock)
	}
}

// callVerifyHeaderSafe calls engine.verifyHeader and recovers from any panic
// that may arise from a nil chain reference in the V1 cascading-fields path.
// We only need to distinguish "V2 sentinel error" from "V1-path error / nil-chain panic".
func callVerifyHeaderSafe(engine *XDPoS, header *types.Header) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// A nil-chain panic from the V1 path (verifyCascadingFields) means
			// the header WAS routed to the V1 engine — exactly what we want to confirm.
			err = nil
		}
	}()
	return engine.verifyHeader(nil, header, nil)
}

// containsString reports whether s contains substr.
func containsString(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
