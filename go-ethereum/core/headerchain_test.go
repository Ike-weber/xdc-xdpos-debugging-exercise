// Copyright 2020 The go-ethereum Authors
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

package core

import (
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

func verifyUnbrokenCanonchain(hc *HeaderChain) error {
	h := hc.CurrentHeader()
	for {
		canonHash := rawdb.ReadCanonicalHash(hc.chainDb, h.Number.Uint64())
		if exp := h.Hash(); canonHash != exp {
			return fmt.Errorf("Canon hash chain broken, block %d got %x, expected %x",
				h.Number, canonHash[:8], exp[:8])
		}
		if h.Number.Uint64() == 0 {
			break
		}
		h = hc.GetHeader(h.ParentHash, h.Number.Uint64()-1)
	}
	return nil
}

func testInsert(t *testing.T, hc *HeaderChain, chain []*types.Header, wantStatus WriteStatus, wantErr error) {
	t.Helper()

	status, err := hc.InsertHeaderChain(chain, time.Now())
	if status != wantStatus {
		t.Errorf("wrong write status from InsertHeaderChain: got %v, want %v", status, wantStatus)
	}
	// Always verify that the header chain is unbroken
	if err := verifyUnbrokenCanonchain(hc); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("unexpected error from InsertHeaderChain: %v", err)
	}
}

// This test checks status reporting of InsertHeaderChain.
func TestHeaderInsertion(t *testing.T) {
	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)
	gspec.Commit(db, triedb.NewDatabase(db, nil), nil)
	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	// chain A: G->A1->A2...A128
	genDb, chainA := makeHeaderChainWithGenesis(gspec, 128, ethash.NewFaker(), 10)
	// chain B: G->A1->B1...B128
	chainB := makeHeaderChain(gspec.Config, chainA[0], 128, ethash.NewFaker(), genDb, 10)

	// Inserting 64 headers on an empty chain, expecting
	// 1 callbacks, 1 canon-status, 0 sidestatus,
	testInsert(t, hc, chainA[:64], CanonStatTy, nil)

	// Inserting 64 identical headers, expecting
	// 0 callbacks, 0 canon-status, 0 sidestatus,
	testInsert(t, hc, chainA[:64], NonStatTy, nil)

	// Inserting the same some old, some new headers
	// 1 callbacks, 1 canon, 0 side
	testInsert(t, hc, chainA[32:96], CanonStatTy, nil)

	// Inserting headers from chain B, overtaking the canon chain blindly
	testInsert(t, hc, chainB[0:32], CanonStatTy, nil)

	// Inserting more headers on chain B, but we don't have the parent
	testInsert(t, hc, chainB[34:36], NonStatTy, consensus.ErrUnknownAncestor)

	// Inserting more headers on chain B, extend the canon chain
	testInsert(t, hc, chainB[32:97], CanonStatTy, nil)

	// Inserting more headers on chain A, taking back the canonicality
	testInsert(t, hc, chainA[90:100], CanonStatTy, nil)

	// And B becomes canon again
	testInsert(t, hc, chainB[97:107], CanonStatTy, nil)

	// And B becomes even longer
	testInsert(t, hc, chainB[107:128], CanonStatTy, nil)
}

// TestGetTrustedCheckpointAnchorActiveAtGenesis is the A.68 regression guard
// for the fresh-start fast-sync case: when the operator pivot is installed
// (via SetFastSyncTrustedAnchor) on a chain whose header head is still at
// genesis (headNum=0), the gate MUST return active=true so the XDPoS V2
// engine's `inCheckpointCatchup` check fires and BeaconSync's verifyQC path
// skips strict round-based QC verification (which would otherwise crash on
// "parent header X not in DB (bulk sync?)" because the pre-pivot epoch-
// switch ancestors haven't been backfilled yet).
//
// The pre-A.68 implementation used a symmetric ±2*Epoch window with a
// `headNum >= pt.BlockNumber - 2*Epoch` lower bound that failed for headNum
// =0 against any sufficiently high pivot. This test pins the corrected
// semantics: active stays true until head climbs PAST pt.BlockNumber+2*Epoch.
// Refs #844 A.68.
func TestGetTrustedCheckpointAnchorActiveAtGenesis(t *testing.T) {
	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)
	gspec.Commit(db, triedb.NewDatabase(db, nil), nil)
	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	// No anchor installed yet — gate returns inactive.
	if _, _, active := hc.GetTrustedCheckpointAnchor(); active {
		t.Fatalf("anchor active before installation: want false, got true")
	}
	// Install an operator pivot far above genesis (matches XDC mainnet pivot
	// at block 103,309,269 — head is still at genesis = 0 at this point).
	const pivotNum = uint64(103_309_269)
	pivotHash := common.HexToHash("0x32a2aeb22957a7526498a2c2b447762ea2708c42fa43b03ac83d2e3be3d9eea9")
	hc.SetFastSyncTrustedAnchor(pivotNum, pivotHash)
	num, hash, active := hc.GetTrustedCheckpointAnchor()
	if num != pivotNum {
		t.Fatalf("anchor number mismatch: want %d, got %d", pivotNum, num)
	}
	if hash != pivotHash {
		t.Fatalf("anchor hash mismatch: want %s, got %s", pivotHash.Hex(), hash.Hex())
	}
	if !active {
		t.Fatalf("anchor inactive with head at genesis: want active=true (catchup window open); got false — this is the A.60.5c bug A.68 fixed")
	}
}

// TestGetTrustedCheckpointAnchorInactivePastCatchupWindow guards the upper
// bound: once the header chain head has climbed past anchor + 2*Epoch the
// gate MUST return active=false so the V2 engine re-engages strict QC
// verification. This is the opposite of the regression A.68 fixed — the
// upper-bound termination of the catchup window is the real consensus-
// correctness invariant and must remain intact. Refs #844 A.68.
func TestGetTrustedCheckpointAnchorInactivePastCatchupWindow(t *testing.T) {
	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)
	gspec.Commit(db, triedb.NewDatabase(db, nil), nil)
	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	// Install anchor and simulate head having climbed well past pivot+2*Epoch.
	const (
		pivotNum = uint64(1_000_000)
		epoch    = uint64(900) // default when XDPoS config absent
	)
	pivotHash := common.HexToHash("0xdeadbeef")
	hc.SetFastSyncTrustedAnchor(pivotNum, pivotHash)
	// Fake a head WAY past pivot + 2*Epoch.
	hc.currentHeader.Store(&types.Header{Number: big.NewInt(int64(pivotNum + 2*epoch + 1))})
	if _, _, active := hc.GetTrustedCheckpointAnchor(); active {
		t.Fatalf("anchor active past catchup window: want false, got true — strict QC must re-engage")
	}
	// Head exactly at the upper boundary — still NOT active (strict check kicks in).
	hc.currentHeader.Store(&types.Header{Number: big.NewInt(int64(pivotNum + 2*epoch))})
	if _, _, active := hc.GetTrustedCheckpointAnchor(); active {
		t.Fatalf("anchor active at exact upper boundary head=pivot+2*epoch: want false, got true")
	}
	// Head one below upper boundary — active (still inside the catchup window).
	hc.currentHeader.Store(&types.Header{Number: big.NewInt(int64(pivotNum + 2*epoch - 1))})
	if _, _, active := hc.GetTrustedCheckpointAnchor(); !active {
		t.Fatalf("anchor inactive at head=pivot+2*epoch-1: want true (still catching up), got false")
	}
}
