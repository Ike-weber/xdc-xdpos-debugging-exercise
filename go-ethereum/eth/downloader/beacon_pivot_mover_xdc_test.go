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

// beacon_pivot_mover_xdc_test.go exercises the A.97.1b fix for #886/#894:
// the beacon/skeleton pivot mover (beaconsync.go:336-364) must NOT advance
// d.pivotHeader once the pivot is frozen (state download underway).
//
// Two levels of coverage:
//
//  1. Decision-logic unit tests (TestBeaconPivotMoverDecision_*): pure function
//     tests of beaconPivotMoverDecision that prove the freeze input flips the
//     output and that without the freeze the race would be live.
//
//  2. Integration / race test (TestBeaconPivotMover_ConcurrentAdvanceWhileFrozen):
//     drives a real Downloader's pivotHeader/pivotFrozenState from concurrent
//     goroutines simulating the beacon loop with an advancing skeleton head.
//     Asserts pivot does not move when frozen, and demonstrates the race by
//     temporarily simulating the unfrozen path.
//
// Refs: A.97.1b / #894 / #886.

package downloader

import (
	"math/big"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

// --- Decision-logic unit tests ---

// TestBeaconPivotMoverDecision_FreezeRefuses verifies that the freeze gate
// correctly refuses the beacon-path pivot advance on the dynamic pivot path
// (operatorPivotNumber == 0, frozen == true).
//
// This is the primary guard introduced by A.97.1b. If the gate were absent,
// a head advancing beyond pivotNumber + 2*fsMinFullBlocks - 8 would set
// advance=true and move the pivot — that is exactly the #886 race.
func TestBeaconPivotMoverDecision_FreezeRefuses(t *testing.T) {
	var (
		pivotNum    = uint64(10_000)
		headNum     = pivotNum + 2*uint64(fsMinFullBlocks) // well past stale threshold
		operatorNum = uint64(0)                            // dynamic path
	)

	// WITHOUT freeze: would advance — this proves the race is real.
	advance, refused, newNum := beaconPivotMoverDecision(headNum, pivotNum, operatorNum, false, false, false)
	if !advance {
		t.Fatalf("without freeze: expected advance=true to confirm the race exists, got advance=false")
	}
	if refused {
		t.Fatalf("without freeze: expected refused=false, got true")
	}
	expectedNew := headNum - uint64(fsMinFullBlocks)
	if newNum != expectedNew {
		t.Fatalf("without freeze: expected newNumber=%d, got %d", expectedNew, newNum)
	}

	// WITH freeze: must refuse — this is the A.97.1b invariant.
	advance, refused, _ = beaconPivotMoverDecision(headNum, pivotNum, operatorNum, true, false, false)
	if advance {
		t.Fatalf("with freeze: expected advance=false (pivot must not move), got true")
	}
	if !refused {
		t.Fatalf("with freeze: expected refused=true, got false")
	}
}

// TestBeaconPivotMoverDecision_OperatorPinnedUnaffected verifies that the
// operator-pinned path (operatorPivotNumber != 0) is byte-identical in
// behavior to the original code — the freeze flag is irrelevant.
// This is the A.96 regression invariant.
func TestBeaconPivotMoverDecision_OperatorPinnedUnaffected(t *testing.T) {
	var (
		pivotNum    = uint64(10_000)
		headNum     = pivotNum + 2*uint64(fsMinFullBlocks)
		operatorNum = uint64(9_000) // operator-pinned — non-zero
	)

	// frozen=false: should advance (original behavior).
	advance, refused, newNum := beaconPivotMoverDecision(headNum, pivotNum, operatorNum, false, false, false)
	if !advance || refused {
		t.Fatalf("operator-pinned unfrozen: expected advance=true refused=false, got advance=%v refused=%v", advance, refused)
	}
	if newNum != headNum-uint64(fsMinFullBlocks) {
		t.Fatalf("operator-pinned: wrong newNumber %d", newNum)
	}

	// frozen=true: must ALSO advance (freeze does not affect operator-pinned).
	advance, refused, newNum = beaconPivotMoverDecision(headNum, pivotNum, operatorNum, true, false, false)
	if !advance || refused {
		t.Fatalf("operator-pinned frozen: expected advance=true refused=false (freeze irrelevant), got advance=%v refused=%v", advance, refused)
	}
	if newNum != headNum-uint64(fsMinFullBlocks) {
		t.Fatalf("operator-pinned frozen: wrong newNumber %d", newNum)
	}
}

// TestBeaconPivotMoverDecision_BelowThreshold verifies that neither path
// advances the pivot when the head has not yet exceeded the staleness threshold.
func TestBeaconPivotMoverDecision_BelowThreshold(t *testing.T) {
	var (
		pivotNum    = uint64(10_000)
		headNum     = pivotNum + 2*uint64(fsMinFullBlocks) - 9 // just below threshold
		operatorNum = uint64(0)
	)

	for _, frozen := range []bool{false, true} {
		advance, refused, _ := beaconPivotMoverDecision(headNum, pivotNum, operatorNum, frozen, false, false)
		if advance || refused {
			t.Fatalf("below threshold frozen=%v: expected advance=false refused=false, got advance=%v refused=%v", frozen, advance, refused)
		}
	}
}

// TestBeaconPivotMoverDecision_ExactThresholdBoundary verifies the threshold
// is ">", not ">=": at exactly 2*fsMinFullBlocks-8 ahead the pivot does NOT
// move (head must be strictly greater). This preserves original gate semantics.
func TestBeaconPivotMoverDecision_ExactThresholdBoundary(t *testing.T) {
	var (
		pivotNum    = uint64(10_000)
		headNum     = pivotNum + 2*uint64(fsMinFullBlocks) - 8 // exactly at boundary
		operatorNum = uint64(0)
	)
	advance, _, _ := beaconPivotMoverDecision(headNum, pivotNum, operatorNum, false, false, false)
	if advance {
		t.Fatalf("exactly at boundary: expected advance=false (strictly greater required), got true")
	}

	// One above boundary must advance.
	advance, _, _ = beaconPivotMoverDecision(headNum+1, pivotNum, operatorNum, false, false, false)
	if !advance {
		t.Fatalf("one above boundary: expected advance=true, got false")
	}
}

// --- Integration / race test ---

// simulatedBeaconMoverIteration simulates one iteration of fetchHeaders' pivot
// mover section, using only the Downloader's pivot state (no live skeleton).
// It atomically reads head, checks the mover decision, and writes pivotHeader
// if the decision is "advance" — exactly as fetchHeaders does.
//
// Returns the pivot number after the iteration.
func simulatedBeaconMoverIteration(d *Downloader, headNumber uint64) uint64 {
	d.pivotLock.Lock()
	defer d.pivotLock.Unlock()
	if d.pivotHeader == nil {
		return 0
	}
	advance, refused, newNum := beaconPivotMoverDecision(
		headNumber,
		d.pivotHeader.Number.Uint64(),
		d.operatorPivotNumber,
		d.IsPivotFrozen(),
		d.committed.Load(),
		d.snapPivotFrozen(),
	)
	if refused {
		// This is the branch under test — freeze gate fires.
		return d.pivotHeader.Number.Uint64()
	}
	if advance {
		// Race: pivot moved! This is what A.97.1b prevents.
		d.pivotHeader = &types.Header{Number: new(big.Int).SetUint64(newNum)}
	}
	return d.pivotHeader.Number.Uint64()
}

// TestBeaconPivotMover_ConcurrentAdvanceWhileFrozen drives N goroutines
// each simulating the beacon loop advancing the skeleton head. It asserts
// that with the freeze active the pivot does NOT move beyond its initial
// value, and separately proves the race would be live without the freeze.
//
// This test detects the #886 race via the decision logic that beaconsync.go
// uses, not just by toggling the bool — if beaconPivotMoverDecision were
// changed to ignore frozen, the test would fail.
func TestBeaconPivotMover_ConcurrentAdvanceWhileFrozen(t *testing.T) {
	var (
		initialPivot  = uint64(50_000)
		advancingHead = initialPivot + 3*uint64(fsMinFullBlocks)
	)
	const (
		goroutines = 20
		iterations = 100
	)

	t.Run("frozen_pivot_does_not_move", func(t *testing.T) {
		d := &Downloader{}
		d.pivotHeader = &types.Header{Number: new(big.Int).SetUint64(initialPivot)}
		// Freeze pivot before concurrent movers start — mirrors the A.97.1b
		// early freeze at downloader.go spawnSync call site.
		d.FreezePivot()

		var wg sync.WaitGroup
		var observedMoves atomic.Int64

		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < iterations; j++ {
					// Each iteration simulates the skeleton head advancing.
					head := advancingHead + uint64(j)*uint64(fsMinFullBlocks)
					afterPivot := simulatedBeaconMoverIteration(d, head)
					if afterPivot != initialPivot {
						observedMoves.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		if observedMoves.Load() != 0 {
			t.Fatalf("frozen pivot MOVED %d time(s) — A.97.1b freeze gate not working", observedMoves.Load())
		}
		if d.pivotHeader.Number.Uint64() != initialPivot {
			t.Fatalf("pivot changed from %d to %d while frozen", initialPivot, d.pivotHeader.Number.Uint64())
		}
	})

	t.Run("unfrozen_pivot_does_move_proving_race_exists", func(t *testing.T) {
		// This sub-test verifies that WITHOUT the freeze the concurrent movers
		// WOULD advance the pivot — confirming the test actually detects the race.
		// If this sub-test fails (i.e., pivot does not move without freeze), the
		// test harness is broken and not actually exercising the race.
		d := &Downloader{}
		d.pivotHeader = &types.Header{Number: new(big.Int).SetUint64(initialPivot)}
		// NOTE: pivot is NOT frozen here — simulates pre-A.97.1b (the bug).

		var wg sync.WaitGroup
		var observedMoves atomic.Int64

		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < iterations; j++ {
					head := advancingHead + uint64(j)*uint64(fsMinFullBlocks)
					afterPivot := simulatedBeaconMoverIteration(d, head)
					if afterPivot != initialPivot {
						observedMoves.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		if observedMoves.Load() == 0 {
			t.Fatalf("unfrozen pivot did NOT move — test harness is not exercising the beacon mover race; test is invalid")
		}
		t.Logf("confirmed: unfrozen pivot moved %d time(s) (race is live without the freeze gate)", observedMoves.Load())
	})
}

// TestBeaconPivotMover_FreezeThenOperatorPath verifies that an operator-pinned
// Downloader (operatorPivotNumber != 0) allows pivot to advance regardless of
// whether the freeze is set — A.96 regression invariant at the integration level.
func TestBeaconPivotMover_FreezeThenOperatorPath(t *testing.T) {
	var (
		initialPivot   = uint64(50_000)
		advancingHead  = initialPivot + 3*uint64(fsMinFullBlocks)
		operatorPinned = uint64(49_000) // non-zero = operator-pinned
	)

	d := &Downloader{}
	d.pivotHeader = &types.Header{Number: new(big.Int).SetUint64(initialPivot)}
	d.operatorPivotNumber = operatorPinned
	// Freeze is set — but should be ignored on the operator-pinned path.
	d.FreezePivot()

	// One iteration with an advancing head should advance the pivot.
	afterPivot := simulatedBeaconMoverIteration(d, advancingHead)
	expectedNew := advancingHead - uint64(fsMinFullBlocks)
	if afterPivot != expectedNew {
		t.Fatalf("operator-pinned with freeze set: expected pivot advance to %d, got %d (A.96 regression)", expectedNew, afterPivot)
	}
}
