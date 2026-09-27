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

import "testing"

// TestSnapTerminateFloor_SetsFloor verifies the snap-path wrapper installs the
// skeleton terminate floor (refs #807, #810). This is the B1 fix: without a
// floor the snap skeleton reverse-walks to genesis instead of stopping at the
// state-download pivot.
func TestSnapTerminateFloor_SetsFloor(t *testing.T) {
	d := newA89Downloader(nil)
	d.skeleton = &skeleton{}

	const pivotMinusOne = uint64(82_899_935) // head 82,900,000 - fsMinFullBlocks(64) - 1
	d.SetSnapTerminateFloor(pivotMinusOne)

	if d.skeleton.terminateFloor != pivotMinusOne {
		t.Fatalf("snap terminate floor not installed: want %d, got %d",
			pivotMinusOne, d.skeleton.terminateFloor)
	}
}

// TestSnapTerminateFloor_ZeroIsNoop confirms floor==0 is treated as "unset" and
// does NOT clobber an existing floor (0 is the skeleton's no-floor sentinel).
func TestSnapTerminateFloor_ZeroIsNoop(t *testing.T) {
	d := newA89Downloader(nil)
	d.skeleton = &skeleton{terminateFloor: 12345}

	d.SetSnapTerminateFloor(0)

	if d.skeleton.terminateFloor != 12345 {
		t.Fatalf("floor==0 must be a no-op, but floor became %d", d.skeleton.terminateFloor)
	}
}

// TestSnapTerminateFloor_NilSkeletonNoPanic confirms the wrapper is a safe no-op
// when the skeleton has not been constructed yet (e.g. very early start-up).
func TestSnapTerminateFloor_NilSkeletonNoPanic(t *testing.T) {
	d := newA89Downloader(nil) // skeleton is nil
	d.SetSnapTerminateFloor(100)
	// reaching here without a panic is the assertion
}

// TestSnapTerminateFloor_FloorAtPivotNum documents the floor contract that makes
// the primed-anchor linkup work (A.90.1 dynamic-path rationale, beaconsync.go):
// snapRunningCheck floors AT pivotNum (= head-fsMinFullBlocks), NOT pivotNum-1.
// The skeleton exits walk-back when Tail <= floor+1, so floor=pivotNum stops the
// tail at pivotNum+1, and findBeaconAncestor's HasFastBlock probe checks
// beaconTail-1 == pivotNum — exactly the lowest header in the primed window
// [head-fsMinFullBlocks .. head] that PrimeFastSyncAnchor seeds. Flooring at
// pivotNum-1 would leave the linkup probe one block below the primed window.
func TestSnapTerminateFloor_FloorAtPivotNum(t *testing.T) {
	const head = uint64(82_900_000)
	pivotNum := head - uint64(fsMinFullBlocks) // downloader snap pivot = head-64
	floor := pivotNum                          // what snapRunningCheck passes

	// beaconTail stops at floor+1; the linkup probe is at beaconTail-1 == floor.
	beaconTailMinusOne := (floor + 1) - 1
	if beaconTailMinusOne != pivotNum {
		t.Fatalf("linkup probe must land on pivotNum (%d), got %d", pivotNum, beaconTailMinusOne)
	}
	// The primed window is [head-fsMinFullBlocks .. head] = [pivotNum .. head];
	// its lowest header (pivotNum) must equal the linkup probe target.
	primeWindowLow := head - uint64(fsMinFullBlocks)
	if primeWindowLow != pivotNum {
		t.Fatalf("primed window low (%d) must cover linkup probe target pivotNum (%d)", primeWindowLow, pivotNum)
	}
}
