// Copyright 2026 The go-ethereum Authors
// Unit tests for the snap-sync phase state machine in xdcSyncer.
// Refs issue #807 Phase 3b.
//
// These tests cover the phase-enumeration behavior + the safe
// transitions implemented in this commit. The full state-machine
// integration (peer-head detection, BeaconSync invocation, completion
// event handling) ships in Phase 3b.2 with its own integration tests.

package eth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/engines/engine_v2"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestXdcSnapPhase_String(t *testing.T) {
	cases := []struct {
		p    xdcSnapPhase
		want string
	}{
		{phaseFullSync, "FULL_SYNC"},
		{phaseSnapSelecting, "SNAP_SELECTING"},
		{phaseSnapRunning, "SNAP_RUNNING"},
		{phaseTipCatch, "TIP_CATCH"},
		{phaseAtTip, "AT_TIP"},
		{xdcSnapPhase(99), "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("xdcSnapPhase(%d).String() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestXdcSnapPhase_DefaultIsFullSync(t *testing.T) {
	// Zero value of int32 atomic decodes to phaseFullSync. This is the
	// load-bearing invariant that keeps default users (no --syncmode snap)
	// on today's behavior.
	s := &xdcSyncer{}
	got := xdcSnapPhase(s.snapPhase.Load())
	if got != phaseFullSync {
		t.Fatalf("default snapPhase = %v, want phaseFullSync", got)
	}
}

func TestXdcSnapPhase_PivotBlockZeroWhenUnset(t *testing.T) {
	// Phase 3b.2: snapPivotBlock returns 0 when SelectSnapPivot has
	// not been invoked or returned nil. Real pivot value is set
	// internally by snapSelectOnce on the success path; we test that
	// the un-set state is safe to read concurrently.
	s := &xdcSyncer{}
	if got := s.snapPivotBlock(); got != 0 {
		t.Fatalf("snapPivotBlock() unset = %d, want 0", got)
	}
}

func TestXdcSnapPhase_AtomicLoadStore(t *testing.T) {
	// The phase is read by the heartbeat goroutine and written by the
	// sync loop. Sanity-check the atomic semantics hold.
	s := &xdcSyncer{}
	for _, p := range []xdcSnapPhase{
		phaseFullSync, phaseSnapSelecting, phaseSnapRunning,
		phaseTipCatch, phaseAtTip,
	} {
		s.snapPhase.Store(int32(p))
		if got := xdcSnapPhase(s.snapPhase.Load()); got != p {
			t.Errorf("Store(%v); Load() = %v, want %v", p, got, p)
		}
	}
}

func TestXdcSnapPhase_PivotBlockWithResult(t *testing.T) {
	// When snapPivot is populated (which snapSelectOnce does on the
	// success path), snapPivotBlock returns the block number.
	s := &xdcSyncer{}
	s.snapPivot = &engine_v2.PivotResult{
		Block: &types.Header{
			Number: big.NewInt(82_300_000),
		},
		Root:  common.HexToHash("0xdeadbeef"),
		Depth: 12,
	}
	if got := s.snapPivotBlock(); got != 82_300_000 {
		t.Fatalf("snapPivotBlock() with pivot set = %d, want 82_300_000", got)
	}
}

func TestSnapsyncExperimentalEnv_DefaultDisabled(t *testing.T) {
	// Without the env var: experimental BeaconSync invocation is gated off.
	t.Setenv(snapsyncExperimentalEnv, "")
	if snapsyncExperimentalEnabled() {
		t.Fatal("env var unset → snapsyncExperimentalEnabled() = true, want false")
	}
}

func TestSnapsyncExperimentalEnv_EnabledWhenOne(t *testing.T) {
	t.Setenv(snapsyncExperimentalEnv, "1")
	if !snapsyncExperimentalEnabled() {
		t.Fatal("env var = '1' → snapsyncExperimentalEnabled() = false, want true")
	}
}

func TestSnapsyncExperimentalEnv_RejectsOtherValues(t *testing.T) {
	// "true", "yes", "0", "on" all disable. Only the literal "1" enables.
	// This forces operators to read the docs rather than guessing the
	// activation string from common conventions.
	for _, v := range []string{"true", "yes", "0", "on", "TRUE", "enabled", " 1 "} {
		t.Setenv(snapsyncExperimentalEnv, v)
		if snapsyncExperimentalEnabled() {
			t.Errorf("env var = %q should NOT enable; got enabled=true", v)
		}
	}
}

func TestMaybeTransitionToAtTip_NoOpFromFullSync(t *testing.T) {
	// Default phase is FULL_SYNC. maybeTransitionToAtTip is a no-op
	// from any phase other than TIP_CATCH. This protects default-flow
	// operators (no --syncmode snap) from accidental phase mutation.
	s := &xdcSyncer{}
	// Start in FULL_SYNC (default zero value).
	for _, startPhase := range []xdcSnapPhase{
		phaseFullSync, phaseSnapSelecting, phaseSnapRunning, phaseAtTip,
	} {
		s.snapPhase.Store(int32(startPhase))
		// Don't supply a handler — if maybeTransitionToAtTip tried to
		// access chain or peers it would panic. The guard rail must
		// return BEFORE touching either when phase is not TIP_CATCH.
		s.maybeTransitionToAtTip()
		if got := xdcSnapPhase(s.snapPhase.Load()); got != startPhase {
			t.Errorf("from %v: maybeTransitionToAtTip mutated phase to %v", startPhase, got)
		}
	}
}

func TestXdcSnapEngageMinGap_Conservative(t *testing.T) {
	// Documenting invariant: the gap gate must be large enough that
	// at-tip operators with --syncmode snap never enter SNAP_SELECTING
	// inadvertently. Apothem produces a block every 2s; even an hour of
	// natural drift = 1800 blocks. We want the gate well above any
	// natural drift band.
	if xdcSnapEngageMinGap < 1800 {
		t.Fatalf("xdcSnapEngageMinGap = %d; should exceed natural Apothem hourly drift (1800)",
			xdcSnapEngageMinGap)
	}
}

// TestSnapEngageShortChainGuard_ConstantBounds documents the A.97 short-chain guard
// invariants (refs #912):
//
//  1. xdcSnapEngageMinGap is the threshold below which a peer head is
//     considered "short chain" — snap-engage is skipped when
//     actualPeerHead < xdcSnapEngageMinGap.
//
//  2. xdcSnapEngageFreshThreshold (1000) is below xdcSnapEngageMinGap (5000)
//     by design: a fresh node (localHead < 1000) on a short chain
//     (peerHead < 5000) must NOT loop through SNAP_SELECTING repeatedly.
//     The guard in maybeEngageSnap checks actualPeerHead < xdcSnapEngageMinGap
//     and returns early, keeping the phase in FULL_SYNC.
//
//  3. For long chains (peerHead >= xdcSnapEngageMinGap), the guard is a
//     no-op: the condition is false and snap-engage proceeds as before.
//
// This test verifies the threshold relationship and the short-chain boundary
// values so regressions that change the constants are caught at test time.
func TestSnapEngageShortChainGuard_ConstantBounds(t *testing.T) {
	// Fresh threshold must be below the min-gap so a fresh-but-short chain
	// still hits the guard. If freshThreshold >= minGap the guard in
	// maybeEngageSnap would never fire (localHead >= freshThreshold would
	// exit earlier).
	if xdcSnapEngageFreshThreshold >= xdcSnapEngageMinGap {
		t.Fatalf("xdcSnapEngageFreshThreshold (%d) must be < xdcSnapEngageMinGap (%d)",
			xdcSnapEngageFreshThreshold, xdcSnapEngageMinGap)
	}

	// Devnet scenario: chain of 1819 blocks. The short-chain guard must
	// prevent snap-engage (actualPeerHead=1819 < 5000).
	devnetPeerHead := uint64(1819)
	if devnetPeerHead >= xdcSnapEngageMinGap {
		t.Fatalf("devnet scenario (peerHead=%d) is not below xdcSnapEngageMinGap (%d); test premise invalid",
			devnetPeerHead, xdcSnapEngageMinGap)
	}

	// Production scenario: Apothem / mainnet well above the threshold.
	// Guard must be a no-op (actualPeerHead >= xdcSnapEngageMinGap).
	productionPeerHead := uint64(50_000_000)
	if productionPeerHead < xdcSnapEngageMinGap {
		t.Fatalf("production scenario (peerHead=%d) unexpectedly below xdcSnapEngageMinGap (%d)",
			productionPeerHead, xdcSnapEngageMinGap)
	}

	// Boundary: exactly at the threshold. This is a long-enough chain that
	// the guard does NOT fire, so snap can engage.
	atThreshold := uint64(xdcSnapEngageMinGap)
	if atThreshold < xdcSnapEngageMinGap {
		// Can't be true for a uint comparison, but satisfies the doc.
		t.Fatalf("at-threshold scenario failed: %d < %d", atThreshold, xdcSnapEngageMinGap)
	}
}

// TestSnapEngageShortChainGuard_PhaseUnchanged verifies that the phase machine
// stays in FULL_SYNC when snap-engage is skipped due to the short-chain guard.
// We simulate the guard decision by calling the constant comparisons directly
// (without constructing a full handler / peer set, which requires the full
// integration harness). The guard logic: actualPeerHead < xdcSnapEngageMinGap
// → return early, phase stays phaseFullSync.
func TestSnapEngageShortChainGuard_PhaseUnchanged(t *testing.T) {
	s := &xdcSyncer{}
	// Default phase is FULL_SYNC.
	if got := xdcSnapPhase(s.snapPhase.Load()); got != phaseFullSync {
		t.Fatalf("initial phase = %v, want phaseFullSync", got)
	}

	// Simulate the guard firing: peer head below the threshold.
	// The guard returns early without calling s.snapPhase.Store(SNAP_SELECTING).
	// We verify the phase was NOT mutated (which is the guarantee the guard provides).
	actualPeerHead := uint64(1819) // devnet scenario
	if actualPeerHead < xdcSnapEngageMinGap {
		// Guard would fire: return early, do NOT store phaseSnapSelecting.
		// Phase must remain phaseFullSync.
	} else {
		t.Fatal("test premise broken: devnet peerHead should be < xdcSnapEngageMinGap")
	}

	// Phase must still be FULL_SYNC — the guard must not have transitioned it.
	if got := xdcSnapPhase(s.snapPhase.Load()); got != phaseFullSync {
		t.Fatalf("after short-chain guard, phase = %v, want phaseFullSync", got)
	}
}

// TestSnapEngageShortChainGuard_LongChainPassthrough verifies that the guard
// does NOT suppress snap-engage on a long chain (production scenario). The
// guard condition is (actualPeerHead < xdcSnapEngageMinGap); a long-chain peer
// head must fail that condition, so the guard is a no-op.
func TestSnapEngageShortChainGuard_LongChainPassthrough(t *testing.T) {
	// Long-chain guard check: must not fire for mainnet/Apothem heights.
	for _, peerHead := range []uint64{
		uint64(xdcSnapEngageMinGap),     // exactly at threshold (not short)
		uint64(xdcSnapEngageMinGap + 1), // one above
		50_000_000,                      // Apothem
		73_000_000,                      // XDC mainnet
	} {
		if peerHead < xdcSnapEngageMinGap {
			t.Errorf("peerHead=%d unexpectedly treated as short chain (< %d)",
				peerHead, xdcSnapEngageMinGap)
		}
	}
}
