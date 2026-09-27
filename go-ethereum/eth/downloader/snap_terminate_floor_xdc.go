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

// SetSnapTerminateFloor installs a skeleton reverse-walk terminate floor for the
// experimental XDC snap-sync path (refs #807, #810).
//
// WHY THIS EXISTS
// ---------------
// Fast Sync gets its terminate floor from SetFastSyncPivot, which sets the
// operator pivot AND calls skeleton.SetTerminateFloor(pivot-1) (downloader.go).
// The clean dynamic floor (beaconsync.go A.90 path) is gated on
// d.fastSyncAnchorFn != nil, but that callback is only ever wired inside a
// logically-unreachable else-if in eth/backend.go, so d.fastSyncAnchorFn is
// always nil. The snap path (eth/sync_xdc.go snapRunningCheck) never calls
// SetFastSyncPivot either (operatorPivotNumber stays 0). The net effect: on the
// snap path NO terminate floor is ever set, so the reverse skeleton walk-back
// can only link() at genesis on a cold chain (skeleton.linked: HasHeader &&
// HasBody && HasReceipts && CurrentSnapBlock >= number, true only at block 0).
// A fresh snap node therefore reverse-walks ~104M headers to genesis (64-87 min
// on mainnet) before state download can begin — and that long walk is what lets
// the downloader's auto-advanced pivot drift beyond peers' state-history
// retention (#807 Phase 14), which is the real reason state download then fails.
//
// SetSnapTerminateFloor lets the env-gated snap path floor the skeleton at its
// state-download pivot (head-fsMinFullBlocks), mirroring exactly what
// SetFastSyncPivot already does for Fast Sync, WITHOUT touching the operator
// pivot / gap-block / subchain-prune machinery (which carries different
// semantics). It deliberately lives in a new, XDC-owned file so the
// upstream-owned eth/downloader/ files stay byte-identical (refs #807
// upstream-alignment principle).
//
// SAFETY: this changes NO default-path behavior. Its only caller is
// snapRunningCheck in eth/sync_xdc.go, which returns early unless
// XDC_EXPERIMENTAL_SNAPSYNC=1 (the experimental double-opt-in gate). Default,
// full, fast and archive sync never reach it.
func (d *Downloader) SetSnapTerminateFloor(floor uint64) {
	// floor==0 is treated as "unset"; refuse to clobber an existing floor with
	// a no-op value (SetTerminateFloor uses 0 as the sentinel for "no floor",
	// see skeleton.linked / skeleton.sync guards).
	if d.skeleton != nil && floor > 0 {
		d.skeleton.SetTerminateFloor(floor)
	}
}
