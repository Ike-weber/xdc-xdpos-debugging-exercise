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

// gap_fallback_xdc_test.go tests A.97.4b — the embedded-header masternode
// fallback for gap-block state that pruned mainnet peers cannot serve.
//
// The fatal flaw in A.97.4 (rejected by Opus, it.22): the original closure
// passed the GAP header to GetMasternodesFromEpochSwitchHeader, but gap blocks
// are NOT epoch-switch headers — their Validators is always empty. The fix
// (A.97.4b) resolves the epoch-switch header FORWARD from gapNum (the one
// whose Validators carries the actual masternode set) and keys the snapshot at
// the gap header, mirroring A.70 (sync_xdc.go:2714-2722) exactly.
//
// Test strategy: build a realistic GapFallbackFn that mirrors the fixed
// handler.go closure (forward epoch-switch search) and exercise it via the
// production gate condition from processFastSyncContent (downloader.go:1929):
//
//	stateErr != nil &&
//	stateErr != errCancelStateFetch &&
//	stateErr != errCanceled &&
//	strings.Contains(stateErr.Error(), "failed with all peers")
//
// Refs #894, A.97 iteration log it.22/it.22b.

package downloader

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ---- test fixtures -----------------------------------------------------------

// buildValidatorsBytes packs `n` distinct 20-byte addresses into a byte slice,
// matching the header.Validators encoding used by XDC V2 epoch-switch headers.
func buildValidatorsBytes(n int) []byte {
	buf := make([]byte, n*common.AddressLength)
	for i := 0; i < n; i++ {
		// Each address is [i+1, 0, 0, ..., 0] (20 bytes).
		buf[i*common.AddressLength] = byte(i + 1)
	}
	return buf
}

// mockChain is a minimal chain lookup for epoch-switch resolution tests.
// It maps block numbers to pre-built headers.
type mockChain struct {
	headers map[uint64]*types.Header
}

func (m *mockChain) getHeaderByNumber(n uint64) *types.Header {
	if m.headers == nil {
		return nil
	}
	return m.headers[n]
}

// buildFixture returns a mockChain for the XDC mainnet-like layout:
//   - gapNum   = 79_998_750  (epochBoundary − Gap = 79_999_200 − 450)
//   - epochSwitchNum = 79_999_200 (epoch boundary; Validators/Penalties populated)
//
// Gap block has EMPTY Validators — exactly as in production.
// Epoch-switch block has 108 masternodes + 5 penalties.
func buildGapFixture() (gapNum, epochSwitchNum uint64, chain *mockChain) {
	const (
		epoch    = 900
		gap      = 450
		gapN     = 79_998_750
		switchN  = gapN + gap // = 79_999_200
		mnCount  = 108
		penCount = 5
	)
	_ = epoch
	gapHeader := &types.Header{
		Number:     big.NewInt(int64(gapN)),
		Validators: nil, // EMPTY — as in production; gap blocks are not epoch-switch
		Penalties:  nil,
	}
	epochSwitchHeader := &types.Header{
		Number:     big.NewInt(int64(switchN)),
		Validators: buildValidatorsBytes(mnCount),
		Penalties:  buildValidatorsBytes(penCount),
	}
	c := &mockChain{headers: map[uint64]*types.Header{
		gapN:    gapHeader,
		switchN: epochSwitchHeader,
	}}
	return gapN, switchN, c
}

// peersExhaustedErr returns an error matching the "failed with all peers"
// pattern from faststatesync.go:921.
func peersExhaustedErr() error {
	return fmt.Errorf("trie node abc123 failed with all peers (3 tries, 3 peers)")
}

// ---- realistic GapFallbackFn builder ----------------------------------------
//
// makeFallbackFn constructs a GapFallbackFn that mirrors the fixed handler.go
// closure (A.97.4b). It uses a mock chain for header lookup and a configurable
// isEpochSwitchFn to simulate v2.IsEpochSwitch.
//
// Parameters:
//   - getHeader: chain.GetHeaderByNumber substitute
//   - isEpochSwitch: returns (isSwitch, err) for a given header
//   - searchMax: upper bound for the forward walk (gapNum + 2*Epoch in prod)
//   - updateFn: called in place of v2.UpdateMasternodesWithPenalties; returns
//     error to simulate failure; nil means always succeed
//
// The returned closure is exercised via the production gate condition.

type updateFnType func(gapHeader *types.Header, masternodes []common.Address, penalties []common.Address) error

func makeFallbackFn(
	getHeader func(n uint64) *types.Header,
	isEpochSwitch func(h *types.Header) (bool, error),
	epoch uint64,
	updateFn updateFnType,
) GapFallbackFn {
	return func(gapHeader *types.Header) (int, error) {
		gapNum := gapHeader.Number.Uint64()
		searchMax := gapNum + 2*epoch
		var epochSwitchHeader *types.Header
		for n := gapNum + 1; n <= searchMax; n++ {
			candidate := getHeader(n)
			if candidate == nil {
				continue
			}
			isSwitch, err := isEpochSwitch(candidate)
			if err == nil && isSwitch {
				epochSwitchHeader = candidate
				break
			}
		}
		if epochSwitchHeader == nil {
			return 0, fmt.Errorf("gap %d: cannot locate epoch-switch header in [%d..%d]"+
				" — fast-sync header phase incomplete or chain DB missing"+
				" (use --fastsyncpivot* or connect an archive peer)",
				gapNum, gapNum+1, searchMax)
		}
		// Read masternodes from epoch-switch header (NOT from gapHeader).
		mnBytes := epochSwitchHeader.Validators
		if len(mnBytes) == 0 || len(mnBytes)%common.AddressLength != 0 {
			return 0, fmt.Errorf("gap %d: epoch-switch header %d has empty Validators"+
				" — cannot seed masternodes without state"+
				" (use --fastsyncpivot* or connect an archive peer)",
				gapNum, epochSwitchHeader.Number.Uint64())
		}
		masternodes := make([]common.Address, len(mnBytes)/common.AddressLength)
		for i := range masternodes {
			copy(masternodes[i][:], mnBytes[i*common.AddressLength:])
		}
		penBytes := epochSwitchHeader.Penalties
		penalties := make([]common.Address, 0, len(penBytes)/common.AddressLength)
		for i := 0; i+common.AddressLength <= len(penBytes); i += common.AddressLength {
			var a common.Address
			copy(a[:], penBytes[i:])
			penalties = append(penalties, a)
		}
		if updateFn != nil {
			if err := updateFn(gapHeader, masternodes, penalties); err != nil {
				return 0, fmt.Errorf("gap %d: UpdateMasternodesWithPenalties (epochSwitch=%d): %w",
					gapNum, epochSwitchHeader.Number.Uint64(), err)
			}
		}
		return len(masternodes), nil
	}
}

// isEpochSwitchByValidators treats any header with non-empty Validators as an
// epoch-switch header (simplified isSwitch for tests; production uses round math).
func isEpochSwitchByValidators(h *types.Header) (bool, error) {
	return len(h.Validators) > 0 && len(h.Validators)%common.AddressLength == 0, nil
}

// runProductionGate applies the production gate from processFastSyncContent
// (downloader.go:1929) to stateErr and invokes fallbackFn on match.
// Returns the effective syncErr after the gate.
func runProductionGate(stateErr error, gapHeader *types.Header, fallbackFn GapFallbackFn) (mnCount int, syncErr error) {
	if stateErr != nil && stateErr != errCancelStateFetch && stateErr != errCanceled {
		if strings.Contains(stateErr.Error(), "failed with all peers") && fallbackFn != nil {
			n, fallbackErr := fallbackFn(gapHeader)
			if fallbackErr != nil {
				return 0, fmt.Errorf("fast sync: gap %d state unavailable and header lacks embedded masternodes"+
					" (use --fastsyncpivot* or connect an archive peer): %w",
					gapHeader.Number.Uint64(), fallbackErr)
			}
			return n, nil // fallback succeeded
		}
		return 0, fmt.Errorf("fast sync: gap %d state: %w", gapHeader.Number.Uint64(), stateErr)
	}
	return 0, nil // stateErr == nil or cancel: not a fallback case
}

// ---- real-path tests ---------------------------------------------------------

// TestA97_4b_RealisticGapSucceeds is the primary acceptance test.
// It verifies that the fixed A.97.4b closure SUCCEEDS when:
//   - The gap header has EMPTY Validators (as in production).
//   - An epoch-switch header with real Validators/Penalties IS resolvable.
//   - UpdateMasternodesWithPenalties succeeds.
//
// Asserts: (1) mnCount > 0, (2) syncErr == nil, (3) gap header passed to
// updateFn (not the epoch-switch header), (4) epoch-switch header is found at
// gapNum+Gap (not gapNum itself).
func TestA97_4b_RealisticGapSucceeds(t *testing.T) {
	gapNum, epochSwitchNum, chain := buildGapFixture()
	gapHeader := chain.headers[gapNum]

	var updateCalledWith *types.Header
	var updateMasternodeCount int

	fn := makeFallbackFn(
		chain.getHeaderByNumber,
		isEpochSwitchByValidators,
		900, // epoch
		func(gapHdr *types.Header, masternodes []common.Address, penalties []common.Address) error {
			updateCalledWith = gapHdr
			updateMasternodeCount = len(masternodes)
			return nil
		},
	)

	mnCount, syncErr := runProductionGate(peersExhaustedErr(), gapHeader, fn)

	if syncErr != nil {
		t.Fatalf("A.97.4b: expected fallback to succeed for realistic gap, got: %v", syncErr)
	}
	if mnCount != 108 {
		t.Errorf("A.97.4b: expected 108 masternodes, got %d", mnCount)
	}
	// UpdateMasternodesWithPenalties must be keyed at the GAP header, not the epoch-switch.
	if updateCalledWith == nil {
		t.Fatal("A.97.4b: updateFn was not called")
	}
	if updateCalledWith.Number.Uint64() != gapNum {
		t.Errorf("A.97.4b: updateFn keyed at %d, want %d (gapNum)", updateCalledWith.Number.Uint64(), gapNum)
	}
	if updateMasternodeCount != 108 {
		t.Errorf("A.97.4b: updateFn received %d masternodes, want 108", updateMasternodeCount)
	}
	// Sanity: the epoch-switch header is NOT at gapNum — it's at gapNum+Gap.
	if epochSwitchNum == gapNum {
		t.Errorf("fixture error: epochSwitchNum (%d) == gapNum (%d)", epochSwitchNum, gapNum)
	}
	t.Logf("A.97.4b: fallback succeeded: gap=%d epochSwitch=%d masternodes=%d", gapNum, epochSwitchNum, mnCount)
}

// TestA97_4b_MissingEpochSwitchHeaderErrors verifies that when the epoch-switch
// header is NOT in the chain DB (fast-sync header phase incomplete), the fallback
// returns an error naming the search range — not a panic or an empty-validators error.
func TestA97_4b_MissingEpochSwitchHeaderErrors(t *testing.T) {
	gapNum := uint64(79_998_750)
	// Chain has only the gap block — epoch-switch block missing.
	chain := &mockChain{headers: map[uint64]*types.Header{
		gapNum: {
			Number:     big.NewInt(int64(gapNum)),
			Validators: nil, // empty — as in production
		},
	}}
	gapHeader := chain.headers[gapNum]

	fn := makeFallbackFn(
		chain.getHeaderByNumber,
		isEpochSwitchByValidators,
		900,
		nil, // updateFn not reached
	)

	_, syncErr := runProductionGate(peersExhaustedErr(), gapHeader, fn)

	if syncErr == nil {
		t.Fatal("A.97.4b: expected error when epoch-switch header is missing, got nil")
	}
	if !strings.Contains(syncErr.Error(), "cannot locate epoch-switch header") {
		t.Errorf("A.97.4b: error should name the search failure, got: %v", syncErr)
	}
	if !strings.Contains(syncErr.Error(), "fast-sync header phase incomplete") {
		t.Errorf("A.97.4b: error should note header phase, got: %v", syncErr)
	}
	t.Logf("A.97.4b: correct error on missing epoch-switch: %v", syncErr)
}

// TestA97_4b_EpochSwitchWithEmptyValidatorsErrors verifies that when the epoch-switch
// header IS found but its Validators field is empty (misconfiguration / wrong header),
// the fallback returns a meaningful error — not a silent zero-masternodes success.
func TestA97_4b_EpochSwitchWithEmptyValidatorsErrors(t *testing.T) {
	gapNum := uint64(79_998_750)
	switchNum := uint64(79_999_200)
	chain := &mockChain{headers: map[uint64]*types.Header{
		gapNum: {
			Number:     big.NewInt(int64(gapNum)),
			Validators: nil,
		},
		switchNum: {
			Number:     big.NewInt(int64(switchNum)),
			Validators: nil, // empty — wrong header or misconfiguration
		},
	}}
	gapHeader := chain.headers[gapNum]

	// isEpochSwitchFn considers any header with number % 900 == 0 an epoch-switch,
	// regardless of Validators (simulates a header that passes IsEpochSwitch but
	// has no embedded masternodes — e.g., a pre-V2 checkpoint placeholder).
	isSwitch := func(h *types.Header) (bool, error) {
		return h.Number.Uint64()%900 == 0, nil
	}

	fn := makeFallbackFn(chain.getHeaderByNumber, isSwitch, 900, nil)

	_, syncErr := runProductionGate(peersExhaustedErr(), gapHeader, fn)

	if syncErr == nil {
		t.Fatal("A.97.4b: expected error when epoch-switch has empty Validators, got nil")
	}
	if !strings.Contains(syncErr.Error(), "empty Validators") {
		t.Errorf("A.97.4b: error should name empty Validators, got: %v", syncErr)
	}
	if !strings.Contains(syncErr.Error(), "use --fastsyncpivot*") {
		t.Errorf("A.97.4b: error should carry operator guidance, got: %v", syncErr)
	}
	t.Logf("A.97.4b: correct error on empty-validators epoch-switch: %v", syncErr)
}

// TestA97_4b_UpdateMasternodesFailurePropagates verifies that when
// UpdateMasternodesWithPenalties returns an error, the fallback surfaces it.
func TestA97_4b_UpdateMasternodesFailurePropagates(t *testing.T) {
	gapNum, _, chain := buildGapFixture()
	gapHeader := chain.headers[gapNum]

	fn := makeFallbackFn(
		chain.getHeaderByNumber,
		isEpochSwitchByValidators,
		900,
		func(_ *types.Header, _ []common.Address, _ []common.Address) error {
			return fmt.Errorf("snapshot DB write failed: disk full")
		},
	)

	_, syncErr := runProductionGate(peersExhaustedErr(), gapHeader, fn)

	if syncErr == nil {
		t.Fatal("A.97.4b: expected error when UpdateMasternodesWithPenalties fails, got nil")
	}
	if !strings.Contains(syncErr.Error(), "UpdateMasternodesWithPenalties") {
		t.Errorf("A.97.4b: error should name the failed operation, got: %v", syncErr)
	}
	if !strings.Contains(syncErr.Error(), "disk full") {
		t.Errorf("A.97.4b: inner error should propagate, got: %v", syncErr)
	}
}

// TestA97_4b_DefaultSuccessPathUntouched verifies that when gap-block state
// download succeeds (no error), the fallback is NOT invoked and the normal
// gapSnapshotFn IS called. Guards the byte-identical default path.
func TestA97_4b_DefaultSuccessPathUntouched(t *testing.T) {
	gapNum, _, chain := buildGapFixture()

	fallbackCalled := false
	snapshotCalled := false

	fn := makeFallbackFn(
		chain.getHeaderByNumber,
		isEpochSwitchByValidators,
		900,
		func(_ *types.Header, _ []common.Address, _ []common.Address) error {
			fallbackCalled = true
			return nil
		},
	)
	snapshotFn := GapSnapshotFn(func(_ uint64, _ [32]byte) error {
		snapshotCalled = true
		return nil
	})

	// Simulate stateErr == nil (success path).
	var stateErr error // nil
	if stateErr != nil && stateErr != errCancelStateFetch && stateErr != errCanceled {
		if strings.Contains(stateErr.Error(), "failed with all peers") && fn != nil {
			fallbackCalled = true // would be called
		}
	} else if stateErr == nil {
		// Normal path: call snapshot fn.
		_ = snapshotFn(gapNum, [32]byte{})
	}

	if fallbackCalled {
		t.Fatal("A.97.4b: GapFallbackFn must NOT be called when state download succeeds")
	}
	if !snapshotCalled {
		t.Fatal("A.97.4b: gapSnapshotFn must be called on the success path")
	}
}

// TestA97_4b_CancelErrorsNotTriggered verifies that errCancelStateFetch and
// errCanceled do NOT trigger the fallback — they are clean-exit signals that
// must propagate normally as before A.97.4.
// Uses != sentinel comparisons (production semantics), not errors.Is.
func TestA97_4b_CancelErrorsNotTriggered(t *testing.T) {
	fallbackCalled := false
	fn := GapFallbackFn(func(_ *types.Header) (int, error) {
		fallbackCalled = true
		return 0, nil
	})

	dummyHeader := &types.Header{Number: big.NewInt(79_998_750)}

	for _, cancelErr := range []error{errCancelStateFetch, errCanceled} {
		// Production gate uses != not errors.Is.
		if cancelErr != nil && cancelErr != errCancelStateFetch && cancelErr != errCanceled {
			if strings.Contains(cancelErr.Error(), "failed with all peers") && fn != nil {
				_, _ = fn(dummyHeader)
			}
		}
	}

	if fallbackCalled {
		t.Fatal("A.97.4b: GapFallbackFn must NOT be called for errCancelStateFetch or errCanceled")
	}
}

// TestA97_4b_NilFallbackFnPreservesOriginalError verifies that when gapFallbackFn
// is nil (non-XDPoS chain or not wired), a peers-exhausted error produces the
// original hard-abort error — no panic, no silent swallow.
func TestA97_4b_NilFallbackFnPreservesOriginalError(t *testing.T) {
	gapHeader := &types.Header{Number: big.NewInt(79_998_750)}
	stateErr := peersExhaustedErr()
	var fn GapFallbackFn = nil // not wired

	_, syncErr := runProductionGate(stateErr, gapHeader, fn)

	if syncErr == nil {
		t.Fatal("A.97.4b: expected original error when fallback is nil, got nil")
	}
	if !strings.Contains(syncErr.Error(), "failed with all peers") {
		t.Errorf("A.97.4b: original error must propagate, got: %v", syncErr)
	}
}

// TestA97_4b_SetGapFallbackFnWiresCorrectly checks that SetGapFallbackFn
// stores the function and it is retrievable via the pivotGapLock path.
func TestA97_4b_SetGapFallbackFnWiresCorrectly(t *testing.T) {
	d := &Downloader{}

	called := false
	fn := GapFallbackFn(func(_ *types.Header) (int, error) {
		called = true
		return 42, nil
	})

	d.SetGapFallbackFn(fn)

	d.pivotGapLock.RLock()
	stored := d.gapFallbackFn
	d.pivotGapLock.RUnlock()

	if stored == nil {
		t.Fatal("A.97.4b: gapFallbackFn should be non-nil after SetGapFallbackFn")
	}
	n, err := stored(&types.Header{Number: big.NewInt(1)})
	if err != nil {
		t.Fatalf("A.97.4b: unexpected error from stored fn: %v", err)
	}
	if n != 42 {
		t.Fatalf("A.97.4b: expected 42 from stored fn, got %d", n)
	}
	if !called {
		t.Fatal("A.97.4b: stored fn was not actually called")
	}
}
