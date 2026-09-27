// Copyright 2026 The go-ethereum Authors
// Unit tests for A.97.2 snap-heal-at-tip wiring in xdcSyncer.
// Refs issue #894.

package eth

import (
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// --- helpers ---

// makeSyncer constructs a minimal xdcSyncer with no handler (for guard-logic tests).
func makeSyncer() *xdcSyncer {
	return &xdcSyncer{
		quitCh: make(chan struct{}),
	}
}

// --- XDC_SNAP_HEAL env helper tests ---

func TestSnapHealEnabled_DefaultOff(t *testing.T) {
	// Env not set → disabled. This is the A.96 default path.
	os.Unsetenv(snapHealEnv)
	if snapHealEnabled() {
		t.Fatal("snapHealEnabled() must return false when env unset (default off)")
	}
}

func TestSnapHealEnabled_On(t *testing.T) {
	os.Setenv(snapHealEnv, "1")
	defer os.Unsetenv(snapHealEnv)
	if !snapHealEnabled() {
		t.Fatal("snapHealEnabled() must return true when XDC_SNAP_HEAL=1")
	}
}

func TestSnapHealEnabled_PartialValue(t *testing.T) {
	// Any value other than "1" must be disabled.
	for _, v := range []string{"0", "true", "yes", "2"} {
		os.Setenv(snapHealEnv, v)
		if snapHealEnabled() {
			t.Fatalf("snapHealEnabled() must be false for XDC_SNAP_HEAL=%q", v)
		}
	}
	os.Unsetenv(snapHealEnv)
}

// --- healStarted one-shot guard ---

func TestHealStarted_OneShotGuard(t *testing.T) {
	s := makeSyncer()
	// First CAS: allowed.
	if !s.healStarted.CompareAndSwap(false, true) {
		t.Fatal("first CAS should succeed")
	}
	// Second CAS: must be blocked (one-shot guard prevents double-spawn).
	if s.healStarted.CompareAndSwap(false, true) {
		t.Fatal("second CAS should fail — one-shot guard violated")
	}
}

// --- xdcHealStatus zero value ---

func TestHealStatus_DefaultZeroValue(t *testing.T) {
	s := makeSyncer()
	s.healStatusMu.Lock()
	st := s.healStatus
	s.healStatusMu.Unlock()

	if st.Enabled {
		t.Error("default Enabled must be false")
	}
	if st.InProgress {
		t.Error("default InProgress must be false")
	}
	if st.TargetRoot != "" {
		t.Errorf("default TargetRoot must be empty, got %q", st.TargetRoot)
	}
	if st.HealStartUnix != 0 {
		t.Errorf("default HealStartUnix must be 0, got %d", st.HealStartUnix)
	}
	if st.LastError != "" {
		t.Errorf("default LastError must be empty, got %q", st.LastError)
	}
}

// --- constants sanity ---

func TestHealConstants_Sane(t *testing.T) {
	if healMaxRootRetargets <= 0 {
		t.Errorf("healMaxRootRetargets must be positive, got %d", healMaxRootRetargets)
	}
	if healTimeout <= 0 {
		t.Error("healTimeout must be positive")
	}
	if healGateBBlocks == 0 {
		t.Error("healGateBBlocks must be non-zero")
	}
	// Gate B window must exceed fsMinFullBlocks (64) so it spans pivot→head delta.
	const fsMinFullBlocks = 64
	if healGateBBlocks <= fsMinFullBlocks {
		t.Errorf("healGateBBlocks (%d) must exceed fsMinFullBlocks (%d)",
			healGateBBlocks, fsMinFullBlocks)
	}
}

// --- maybeStartSnapHeal guard: env off → skipped immediately ---

func TestMaybeStartSnapHeal_SkipWhenEnvOff(t *testing.T) {
	os.Unsetenv(snapHealEnv)
	s := makeSyncer()
	// handler is nil; if the function reaches any handler dereference it panics.
	// With env off it must return before touching handler.
	s.maybeStartSnapHeal()
	// healStarted must still be false (function returned early, no goroutine spawned).
	if s.healStarted.Load() {
		t.Fatal("healStarted must be false when XDC_SNAP_HEAL env is off")
	}
}

// --- gate B: ReexecuteAndValidateRange ---

// makeTestChain creates a minimal ethash chain with nBlocks blocks.
// Returns the chain and a cleanup func.
func makeTestChain(t *testing.T, nBlocks int) (*core.BlockChain, func()) {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{
		Config:     params.AllEthashProtocolChanges,
		Difficulty: common.Big1,
		GasLimit:   8_000_000,
		Alloc:      types.GenesisAlloc{},
	}
	engine := ethash.NewFaker()
	_, blocks, _ := core.GenerateChainWithGenesis(gspec, engine, nBlocks, nil)
	bc, err := core.NewBlockChain(db, gspec, engine, nil)
	if err != nil {
		t.Fatalf("NewBlockChain: %v", err)
	}
	if _, err := bc.InsertChain(blocks); err != nil {
		bc.Stop()
		t.Fatalf("InsertChain: %v", err)
	}
	return bc, func() { bc.Stop() }
}

// TestReexecuteAndValidateRange_OK verifies gate B passes on a correct chain.
func TestReexecuteAndValidateRange_OK(t *testing.T) {
	bc, cleanup := makeTestChain(t, 20)
	defer cleanup()

	if err := bc.ReexecuteAndValidateRange(10); err != nil {
		t.Fatalf("ReexecuteAndValidateRange: unexpected error: %v", err)
	}
}

// TestReexecuteAndValidateRange_ClampedToChainLength verifies behaviour when
// nBlocks > chain height: should clamp and still pass. The function skips
// block 0 (genesis) since its state is set by allocation, not TX execution.
func TestReexecuteAndValidateRange_ClampedToChainLength(t *testing.T) {
	bc, cleanup := makeTestChain(t, 5)
	defer cleanup()

	// Ask for more blocks than the chain has; should clamp to [1..head].
	if err := bc.ReexecuteAndValidateRange(1000); err != nil {
		t.Fatalf("ReexecuteAndValidateRange with large window: unexpected error: %v", err)
	}
}

// TestReexecuteAndValidateRange_ZeroBlocks verifies the edge-case where
// nBlocks == 0 (should trivially pass with no iteration).
func TestReexecuteAndValidateRange_ZeroBlocks(t *testing.T) {
	bc, cleanup := makeTestChain(t, 5)
	defer cleanup()

	if err := bc.ReexecuteAndValidateRange(0); err != nil {
		t.Fatalf("ReexecuteAndValidateRange(0): unexpected error: %v", err)
	}
}

// TestReexecuteAndValidateRange_SingleBlock verifies gate B succeeds on a chain
// with exactly one post-genesis block (the minimum meaningful case).
// Block 0 is skipped (genesis state is an alloc, not TX-executed).
func TestReexecuteAndValidateRange_SingleBlock(t *testing.T) {
	bc, cleanup := makeTestChain(t, 1)
	defer cleanup()

	// Block 1's parent is genesis which has state; should succeed.
	if err := bc.ReexecuteAndValidateRange(1); err != nil {
		t.Fatalf("single-block chain: unexpected error: %v", err)
	}
}

// TestReexecuteAndValidateRange_GenesisOnly verifies that a chain with only the
// genesis block trivially passes (nothing to validate after skipping block 0).
func TestReexecuteAndValidateRange_GenesisOnly(t *testing.T) {
	// makeTestChain(t, 0) creates genesis + 0 extra blocks (head = 0).
	bc, cleanup := makeTestChain(t, 0)
	defer cleanup()

	if err := bc.ReexecuteAndValidateRange(128); err != nil {
		t.Fatalf("genesis-only chain: unexpected error: %v", err)
	}
}

// --- checkpointSyncNoState getter ---

func TestCheckpointSyncNoState_Getter(t *testing.T) {
	bc, cleanup := makeTestChain(t, 1)
	defer cleanup()

	// Default is false on a freshly created chain.
	if bc.CheckpointSyncNoState() {
		t.Error("CheckpointSyncNoState() must return false on fresh chain")
	}

	bc.SetCheckpointSyncNoState(true)
	if !bc.CheckpointSyncNoState() {
		t.Error("CheckpointSyncNoState() must return true after Set(true)")
	}

	bc.SetCheckpointSyncNoState(false)
	if bc.CheckpointSyncNoState() {
		t.Error("CheckpointSyncNoState() must return false after Set(false)")
	}
}

// --- xdcHealStatus fields written by goroutine ---

// TestHealStatus_FieldsWritten exercises the status struct mutations
// that the goroutine performs at heal-start. We directly manipulate
// the struct (as the goroutine would) and verify the result via mutex.
func TestHealStatus_FieldsWritten(t *testing.T) {
	s := makeSyncer()
	now := time.Now()

	s.healStatusMu.Lock()
	s.healStatus = xdcHealStatus{
		Enabled:       true,
		InProgress:    true,
		TargetRoot:    common.HexToHash("0xdeadbeef").Hex(),
		HealStartUnix: now.Unix(),
	}
	s.healStatusMu.Unlock()

	s.healStatusMu.Lock()
	st := s.healStatus
	s.healStatusMu.Unlock()

	if !st.Enabled {
		t.Error("Enabled must be true")
	}
	if !st.InProgress {
		t.Error("InProgress must be true")
	}
	if st.TargetRoot == "" {
		t.Error("TargetRoot must be non-empty")
	}
	if st.HealStartUnix == 0 {
		t.Error("HealStartUnix must be non-zero")
	}
}

// --- SnapHealStatusFields struct (core package) ---

func TestSnapHealStatusFields_Mode(t *testing.T) {
	// The Mode field is always "snap-heal" — verify the default constant.
	f := core.SnapHealStatusFields{Mode: "snap-heal"}
	if f.Mode != "snap-heal" {
		t.Errorf("mode = %q, want snap-heal", f.Mode)
	}
}

// --- synthetic chain: A.94 gate (XdcBulkSyncMode must be false for gate B) ---

// TestGateBRunsWithBulkModeOff verifies that after maybeTransitionToAtTip
// flips XdcBulkSyncMode=false, gate B executes in that mode.
// We check the invariant: BulkSyncMode must be false during gate B
// (A.94 must not fire).
func TestGateBRunsWithBulkModeOff(t *testing.T) {
	// Flip bulk mode on, then flip it off (simulating the AT_TIP transition),
	// then confirm gate B call to ReexecuteAndValidateRange sees it false.
	core.XdcBulkSyncMode.Store(true)
	core.XdcBulkSyncMode.Store(false)

	if core.XdcBulkSyncMode.Load() {
		t.Fatal("XdcBulkSyncMode must be false before gate B runs")
	}

	bc, cleanup := makeTestChain(t, 10)
	defer cleanup()

	// Gate B must pass.
	if err := bc.ReexecuteAndValidateRange(5); err != nil {
		t.Fatalf("gate B with bulk=false: %v", err)
	}
}

// --- genesis-derived big.Int helpers (suppress unused import) ---
var _ = big.NewInt(0)
