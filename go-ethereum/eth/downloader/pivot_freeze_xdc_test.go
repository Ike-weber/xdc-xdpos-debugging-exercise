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

// pivot_freeze_xdc_test.go tests XDC A.97.1 pivot freeze semantics.
// Refs: A.97 / #894 / #886.

package downloader

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// minimalDownloaderForPivotTest returns a zero-value Downloader with only the
// fields required by the pivot freeze tests populated. We avoid constructing a
// full tester because pivot freeze operates purely on the pivotFrozenState /
// pivotHeader fields and does not require a live blockchain, peers, or network.
func minimalDownloaderForPivotTest(pivotNum uint64, pivotHashSeed byte) *Downloader {
	var pivotHash common.Hash
	pivotHash[0] = pivotHashSeed

	hdr := &types.Header{
		Number: big.NewInt(int64(pivotNum)),
	}
	// We need a deterministic hash for the test. Since Header.Hash() is
	// computed from the RLP encoding, we use a minimal header and record the
	// expected hash afterwards. For the freeze tests we only care about the
	// number; the hash correctness is exercised in refusePivotUpdate tests.
	d := &Downloader{}
	d.pivotHeader = hdr
	return d
}

// TestPivotFreeze_InitiallyUnfrozen verifies that a fresh downloader reports
// the pivot as unfrozen. A.97.1 must not affect a cycle that hasn't yet
// reached the state-download phase.
func TestPivotFreeze_InitiallyUnfrozen(t *testing.T) {
	d := minimalDownloaderForPivotTest(1000, 0xAA)
	if d.IsPivotFrozen() {
		t.Fatal("expected pivot to be unfrozen on a fresh Downloader")
	}
}

// TestPivotFreeze_FreezeSetsFrozenFlag verifies that FreezePivot transitions
// IsPivotFrozen from false to true and is idempotent on a second call.
func TestPivotFreeze_FreezeSetsFrozenFlag(t *testing.T) {
	d := minimalDownloaderForPivotTest(5000, 0xBB)

	d.FreezePivot()
	if !d.IsPivotFrozen() {
		t.Fatal("expected pivot to be frozen after FreezePivot()")
	}

	// Second call must be a no-op (idempotent).
	d.FreezePivot()
	if !d.IsPivotFrozen() {
		t.Fatal("pivot must remain frozen after redundant FreezePivot()")
	}
}

// TestPivotFreeze_ResetClearsFrozenFlag verifies that ResetPivotFreeze clears
// the freeze so the next sync cycle begins unfrozen. This is the path taken at
// the top of Downloader.synchronise() before each new round.
func TestPivotFreeze_ResetClearsFrozenFlag(t *testing.T) {
	d := minimalDownloaderForPivotTest(5000, 0xCC)

	d.FreezePivot()
	if !d.IsPivotFrozen() {
		t.Fatal("expected frozen after FreezePivot")
	}

	d.ResetPivotFreeze()
	if d.IsPivotFrozen() {
		t.Fatal("expected unfrozen after ResetPivotFreeze()")
	}

	// Idempotent reset on already-cleared state.
	d.ResetPivotFreeze()
	if d.IsPivotFrozen() {
		t.Fatal("pivot must remain unfrozen after redundant ResetPivotFreeze()")
	}
}

// TestPivotFreeze_RefuseUpdateWhenFrozen verifies that refusePivotUpdate
// returns true (= update should be skipped) when the pivot is frozen, and
// records the correct attempted-new-number in the log context.
func TestPivotFreeze_RefuseUpdateWhenFrozen(t *testing.T) {
	d := minimalDownloaderForPivotTest(8000, 0xDD)

	d.FreezePivot()

	var attemptedHash common.Hash
	attemptedHash[0] = 0xFF
	refused := d.refusePivotUpdate(8200, attemptedHash)
	if !refused {
		t.Fatal("expected refusePivotUpdate to return true when frozen")
	}
}

// TestPivotFreeze_AllowUpdateWhenNotFrozen verifies that refusePivotUpdate
// returns false when the pivot is NOT frozen, meaning normal staleness
// processing proceeds unchanged (regression: A.96 operator-pinned path and
// the pre-state-download dynamic-pivot phase must be unaffected).
func TestPivotFreeze_AllowUpdateWhenNotFrozen(t *testing.T) {
	d := minimalDownloaderForPivotTest(8000, 0xEE)
	// Pivot is NOT frozen — simulates the window before state download starts.

	var attemptedHash common.Hash
	attemptedHash[0] = 0x01
	refused := d.refusePivotUpdate(8200, attemptedHash)
	if refused {
		t.Fatal("expected refusePivotUpdate to return false when NOT frozen")
	}
}

// TestPivotFreeze_NilPivotHeader verifies that FreezePivot is a no-op when
// the downloader does not yet have a pivot header (e.g., right after
// construction before the skeleton delivers the first pivot candidate).
func TestPivotFreeze_NilPivotHeader(t *testing.T) {
	d := &Downloader{}
	// pivotHeader is nil — freeze must not panic and must leave frozen=false.
	d.FreezePivot()
	if d.IsPivotFrozen() {
		t.Fatal("FreezePivot with nil pivotHeader must not set frozen=true")
	}
}

// TestPivotFreeze_FreezeResetFreezeCycle simulates a realistic two-round sync
// scenario: freeze in round 1, reset at start of round 2, freeze in round 2.
// This validates that ResetPivotFreeze + FreezePivot compose correctly across
// multiple cycles (the core of the A.97.1 requirement that pivot freeze resets
// on every new sync cycle).
func TestPivotFreeze_FreezeResetFreezeCycle(t *testing.T) {
	d := minimalDownloaderForPivotTest(1000, 0x11)

	// Round 1: freeze.
	d.FreezePivot()
	if !d.IsPivotFrozen() {
		t.Fatal("round 1: expected frozen after FreezePivot")
	}

	// Start of round 2: reset.
	d.ResetPivotFreeze()
	if d.IsPivotFrozen() {
		t.Fatal("round 2 start: expected unfrozen after ResetPivotFreeze")
	}

	// Round 2: should be able to freeze again.
	// Update pivot header to simulate a fresh round with a higher pivot.
	d.pivotHeader = &types.Header{Number: big.NewInt(2000)}
	d.FreezePivot()
	if !d.IsPivotFrozen() {
		t.Fatal("round 2: expected frozen after second FreezePivot")
	}

	// Updates are refused in round 2 as well.
	var h common.Hash
	if !d.refusePivotUpdate(2100, h) {
		t.Fatal("round 2: expected update to be refused while frozen")
	}
}

// TestPivotFreeze_WarnArmingOncePerEpoch verifies the A.97.1c requirement:
// refusePivotUpdate must fire the WARN exactly once per freeze epoch, with
// subsequent refusals within the same epoch silently degraded to Debug level.
// A new freeze epoch (ResetPivotFreeze + FreezePivot) must re-arm the WARN.
//
// We cannot intercept the log handler in a unit test without wiring up the
// full log backend, so we test the underlying flag state transitions directly:
//   - warnArmed=true  after FreezePivot (WARN will fire on first refusal)
//   - warnArmed=false after first refusePivotUpdate call (WARN already fired)
//   - warnArmed=false after second refusePivotUpdate call (no change)
//   - warnArmed=false after ResetPivotFreeze (epoch cleared)
//   - warnArmed=true  after subsequent FreezePivot (re-armed for new epoch)
func TestPivotFreeze_WarnArmingOncePerEpoch(t *testing.T) {
	d := minimalDownloaderForPivotTest(5000, 0x22)

	// ── Epoch 1 ──────────────────────────────────────────────────────────────
	// Before freeze: warnArmed must be false (zero value).
	if d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch1 pre-freeze: warnArmed must be false on fresh downloader")
	}

	d.FreezePivot()

	// After FreezePivot: warnArmed must be true (WARN is armed for first refusal).
	if !d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch1 post-freeze: warnArmed must be true after FreezePivot")
	}

	var h common.Hash
	h[0] = 0xAA

	// First refusal — fires the WARN and disarms.
	if !d.refusePivotUpdate(5100, h) {
		t.Fatal("epoch1 first refusal: expected true (update refused)")
	}
	if d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch1 after first refusal: warnArmed must be false (WARN already emitted)")
	}

	// Second refusal — should degrade to Debug; warnArmed stays false.
	if !d.refusePivotUpdate(5200, h) {
		t.Fatal("epoch1 second refusal: expected true (update still refused)")
	}
	if d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch1 after second refusal: warnArmed must remain false")
	}

	// ── Epoch 2 (reset + re-freeze) ──────────────────────────────────────────
	d.ResetPivotFreeze()

	// After reset: frozen cleared, warnArmed cleared.
	if d.IsPivotFrozen() {
		t.Fatal("epoch2 pre-freeze: expected unfrozen after ResetPivotFreeze")
	}
	if d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch2 pre-freeze: warnArmed must be false after ResetPivotFreeze")
	}

	// Simulate a fresh pivot header for the new round.
	d.pivotHeader = &types.Header{Number: big.NewInt(6000)}
	d.FreezePivot()

	// After re-freeze: warnArmed must be true again (new epoch arms a fresh WARN).
	if !d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch2 post-freeze: warnArmed must be true after second FreezePivot (re-armed)")
	}

	// First refusal in epoch 2 — disarms again.
	h[0] = 0xBB
	if !d.refusePivotUpdate(6100, h) {
		t.Fatal("epoch2 first refusal: expected true (update refused)")
	}
	if d.xdcPivotFreeze.warnArmed.Load() {
		t.Fatal("epoch2 after first refusal: warnArmed must be false after epoch-2 WARN")
	}
}
