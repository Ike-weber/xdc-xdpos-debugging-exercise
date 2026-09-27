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

// pivot_freeze_xdc.go implements XDC A.97.1 (refs #894 #886): pivot freeze for
// the dynamic fast-sync path. Once the state-trie download is underway (signalled
// by the pivot block arriving in the results queue), any subsequent "move pivot to
// a newer header" attempts are refused until the next sync cycle. This prevents
// the moving-target race described in #886 where the pivot advances during the
// multi-hour NodeData download and leaves the local trie incomplete at the
// canonical pivot the chain settles on.
//
// Only active on the dynamic-pivot path (operatorPivotNumber == 0). The
// operator-pinned path (A.96 / --fastsyncpivot*) is already frozen by design
// — its staleness branch is gated on `d.operatorPivotNumber == 0`.
//
// Refs: A.97 / #894 / #886.

package downloader

import (
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// pivotFrozenState is the internal freeze state embedded in Downloader.
// Exported via FreezePivot / ResetPivotFreeze / IsPivotFrozen accessors.
// A separate atomic.Bool avoids aliasing with committed (which also transitions
// once per cycle but has a different semantic: committed = pivot already written
// to DB; frozen = pivot must not move while state is downloading).
//
// warnArmed is set to true by FreezePivot and cleared to false the first time
// refusePivotUpdate fires a WARN. This limits the deployment-stage-grepped WARN
// to exactly one occurrence per freeze epoch; subsequent refusals within the
// same epoch log at Debug level only (the beacon mover calls every 3 s for the
// entire multi-hour state download — ~1 200 identical WARNs/hour without this).
// Re-arming happens implicitly: ResetPivotFreeze clears frozen, and the next
// FreezePivot call sets both frozen and warnArmed again.
type pivotFrozenState struct {
	frozen       atomic.Bool
	frozenNumber atomic.Uint64
	frozenHash   atomic.Pointer[common.Hash]
	warnArmed    atomic.Bool // true = next refusal emits WARN; false = emit Debug
}

// FreezePivot marks the current pivot as frozen. Must be called while d.pivotLock
// is NOT held (it takes an RLock internally to read the current pivot).
//
// After this call, processFastSyncContent will refuse to advance d.pivotHeader
// to a newer block and will log a WARN for every refused update attempt until
// ResetPivotFreeze is called.
//
// No-op if already frozen or if the current pivot is nil.
func (d *Downloader) FreezePivot() {
	if d.xdcPivotFreeze.frozen.Load() {
		return // already frozen, idempotent
	}
	d.pivotLock.RLock()
	p := d.pivotHeader
	d.pivotLock.RUnlock()
	if p == nil {
		return
	}
	hash := p.Hash()
	d.xdcPivotFreeze.frozenHash.Store(&hash)
	d.xdcPivotFreeze.frozenNumber.Store(p.Number.Uint64())
	d.xdcPivotFreeze.warnArmed.Store(true) // arm once-per-epoch WARN before setting frozen
	d.xdcPivotFreeze.frozen.Store(true)
	log.Info("XDC fast-sync: pivot frozen — state download underway, pivot updates refused",
		"number", p.Number.Uint64(),
		"hash", hash.Hex()[:16])
}

// ResetPivotFreeze clears the freeze flag so the next sync cycle can select a
// fresh pivot. Called at the start of each new fast-sync round (SynchroniseXDC
// reset path) so a cycle-restart after a failed download begins unfrozen.
func (d *Downloader) ResetPivotFreeze() {
	if !d.xdcPivotFreeze.frozen.Load() {
		return // already clear
	}
	d.xdcPivotFreeze.warnArmed.Store(false) // will be re-armed by next FreezePivot
	d.xdcPivotFreeze.frozen.Store(false)
	log.Debug("XDC fast-sync: pivot freeze cleared for new sync cycle")
}

// IsPivotFrozen reports whether the pivot is currently frozen.
// Safe to call from any goroutine.
func (d *Downloader) IsPivotFrozen() bool {
	return d.xdcPivotFreeze.frozen.Load()
}

// refusePivotUpdate logs a WARN (first call per freeze epoch) or Debug
// (subsequent calls within the same epoch) when a pivot-stale advance is
// refused due to the freeze, and returns true (= the caller should skip the
// update). Returns false when the pivot is not frozen.
//
// Rate-limit rationale: the beacon mover in fetchHeaders calls this every
// fsHeaderContCheck (~3 s) for the entire multi-hour state download because
// the frozen pivot never advances past the staleness threshold. Without the
// once-per-epoch gate that produces ~1 200 identical WARNs/hour. The first
// WARN is preserved verbatim because deploy-stage acceptance checks grep for
// the exact message text.
//
// Not inlined in processFastSyncContent to keep the hot path readable.
func (d *Downloader) refusePivotUpdate(newNumber uint64, newHash common.Hash) bool {
	if !d.xdcPivotFreeze.frozen.Load() {
		return false
	}
	frozenNum := d.xdcPivotFreeze.frozenNumber.Load()
	frozenHashPtr := d.xdcPivotFreeze.frozenHash.Load()
	var frozenHashHex string
	if frozenHashPtr != nil {
		frozenHashHex = frozenHashPtr.Hex()[:16]
	}
	// CompareAndSwap true→false: exactly one goroutine wins per freeze epoch.
	if d.xdcPivotFreeze.warnArmed.CompareAndSwap(true, false) {
		// First refusal this epoch — emit the grepped WARN verbatim.
		log.Warn("XDC fast-sync: pivot update refused — pivot frozen during state download",
			"frozenAt", frozenNum,
			"frozenHash", frozenHashHex,
			"attemptedNew", newNumber,
			"attemptedHash", newHash.Hex()[:16])
	} else {
		// Subsequent refusals this epoch — suppress to Debug to avoid log spam.
		log.Debug("XDC fast-sync: pivot update refused — pivot frozen during state download",
			"frozenAt", frozenNum,
			"frozenHash", frozenHashHex,
			"attemptedNew", newNumber,
			"attemptedHash", newHash.Hex()[:16])
	}
	return true
}
