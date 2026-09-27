// Copyright (c) 2026 XDC Network
// Fix #1330: gap snapshots must hold the contract candidate pool at the gap
// block, not an epoch-switch header's active set.
//
// Bug (net5151 gap 6750): with the import-time write (UpdateMasternodesFromHeader)
// not having run, getSnapshot's missing-snapshot fallback seeded the gap-6750
// snapshot from epoch-switch header 7199's Validators (dir=backward) — the
// epoch-8 ACTIVE set (5, seated from gap 5850) — while the contract pool at
// 6750 held 6 candidates (a new masternode was proposed mid-epoch). The node
// then failed signature-membership for every TC and vote committing to
// GapNumber 6750: unable to follow rounds or vote, it stalled the network as
// the quorum-critical voter.
//
// Fix under test:
//  1. STATE-FIRST: when state at the gap block is available, rebuild the
//     snapshot from the contract candidate pool (stake-sorted, canonical).
//  2. Backward header seeds are returned ephemerally and NEVER persisted or
//     cached at the gap hash.
//  3. sendVote only latches highestVotedRound once the local handler accepts
//     the vote (a locally-dropped vote was never broadcast and must remain
//     retryable).
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run Test1330 -count=1

package engine_v2

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// mockChain1330 is a consensus.ChainReader with a by-number header map and an
// optional statedb served for any header (nil statedb = state unavailable,
// like a pruned fast-sync node).
type mockChain1330 struct {
	byNumber map[uint64]*types.Header
	statedb  *state.StateDB
}

func (m *mockChain1330) Config() *params.ChainConfig                     { return nil }
func (m *mockChain1330) CurrentHeader() *types.Header                    { return nil }
func (m *mockChain1330) GetBlock(h common.Hash, n uint64) *types.Block   { return nil }
func (m *mockChain1330) GetTd(h common.Hash, n uint64) *big.Int          { return nil }
func (m *mockChain1330) GetHeader(h common.Hash, n uint64) *types.Header { return m.byNumber[n] }
func (m *mockChain1330) GetHeaderByHash(h common.Hash) *types.Header {
	for _, hdr := range m.byNumber {
		if hdr.Hash() == h {
			return hdr
		}
	}
	return nil
}
func (m *mockChain1330) GetHeaderByNumber(n uint64) *types.Header { return m.byNumber[n] }

// mockChain1330WithState additionally exposes StateAt, enabling the
// state-first rebuild (full/archive node).
type mockChain1330WithState struct{ mockChain1330 }

func (m *mockChain1330WithState) StateAt(h *types.Header) (*state.StateDB, error) {
	if m.statedb == nil {
		return nil, errMissingState1330
	}
	return m.statedb, nil
}

var errMissingState1330 = errStub1330("state unavailable")

type errStub1330 string

func (e errStub1330) Error() string { return string(e) }

// seedCandidateState writes `addrs` into the masternode-voting contract
// storage layout (slot 8 = candidates array, validatorsState mapping slot 1
// for caps) with strictly descending stakes, so the stake-sorted pool equals
// the insertion order.
func seedCandidateState(t *testing.T, addrs []common.Address) *state.StateDB {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	slot8 := common.BigToHash(big.NewInt(8))
	statedb.SetState(common.MasternodeVotingSMCBinary, slot8, common.BigToHash(big.NewInt(int64(len(addrs)))))
	for i, a := range addrs {
		elemKey := state.GetLocDynamicArrAtElement(slot8, uint64(i), 1)
		statedb.SetState(common.MasternodeVotingSMCBinary, elemKey, common.BytesToHash(a.Bytes()))
		// cap = validatorsState[a].cap at mapping slot 1, field offset 1
		loc := state.GetLocMappingAtKey(common.BytesToHash(a.Bytes()), 1)
		capLoc := new(big.Int).Add(loc, big.NewInt(1))
		stake := new(big.Int).Mul(big.NewInt(int64(len(addrs)-i)), big.NewInt(10_000_000))
		statedb.SetState(common.MasternodeVotingSMCBinary, common.BigToHash(capLoc), common.BigToHash(stake))
	}
	return statedb
}

// makeMidEpochSwitchHeader builds a V2 epoch-switch header at `num` with the
// given round (round%Epoch==0 boundary crossed vs parent round-1) and packed
// Validators.
func makeMidEpochSwitchHeader(t *testing.T, num uint64, round types.Round, validators []common.Address) *types.Header {
	t.Helper()
	packed := make([]byte, len(validators)*common.AddressLength)
	for i, v := range validators {
		copy(packed[i*common.AddressLength:], v[:])
	}
	extra := &types.ExtraFields_v2{
		Round: round,
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.HexToHash("0xabcd"),
				Round:  round - 1,
				Number: big.NewInt(int64(num - 1)),
			},
			Signatures: []types.Signature{},
			GapNumber:  0,
		},
	}
	extraBytes, err := extra.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes: %v", err)
	}
	return &types.Header{
		Number:     big.NewInt(int64(num)),
		Extra:      extraBytes,
		Validators: packed,
		Difficulty: big.NewInt(1),
	}
}

// Test1330_StateFirst_PoolFromContract: with state available at the gap block,
// getSnapshot must return the CONTRACT candidate pool (6, incl. the
// proposed-but-unseated candidate), stake-sorted, and persist it — regardless
// of what any epoch-switch header says.
func Test1330_StateFirst_PoolFromContract(t *testing.T) {
	eng, db := newA894Engine(t)

	// 5 active-style candidates + 1 proposed-but-unseated (lowest stake, last).
	pool := []common.Address{
		common.HexToAddress("0x1111111111111111111111111111111111111111"),
		common.HexToAddress("0x2222222222222222222222222222222222222222"),
		common.HexToAddress("0x3333333333333333333333333333333333333333"),
		common.HexToAddress("0x4444444444444444444444444444444444444444"),
		common.HexToAddress("0x5555555555555555555555555555555555555555"),
		common.HexToAddress("0xA119821F0963EcEF888b4de575174999b1689C7D"), // unseated
	}
	statedb := seedCandidateState(t, pool)

	const gap = uint64(6750)
	gapHeader := &types.Header{Number: big.NewInt(int64(gap)), Difficulty: big.NewInt(1), Extra: []byte{0x02}}
	chain := &mockChain1330WithState{mockChain1330{
		byNumber: map[uint64]*types.Header{gap: gapHeader},
		statedb:  statedb,
	}}

	snap, err := eng.getSnapshot(chain, gap, true, nil)
	if err != nil {
		t.Fatalf("getSnapshot(state-first): %v", err)
	}
	if len(snap.NextEpochCandidates) != len(pool) {
		t.Fatalf("FAIL #1330: pool size want %d (contract state) got %d", len(pool), len(snap.NextEpochCandidates))
	}
	for i, a := range pool {
		if snap.NextEpochCandidates[i] != a {
			t.Fatalf("pool[%d]: want %s (stake-desc order) got %s", i, a.Hex(), snap.NextEpochCandidates[i].Hex())
		}
	}
	// Must include the proposed-but-unseated candidate — the exact address
	// class the backward seed dropped on net5151.
	if snap.NextEpochCandidates[len(pool)-1] != pool[len(pool)-1] {
		t.Fatalf("unseated candidate missing from pool")
	}
	// Persisted.
	if stored, lerr := loadSnapshot(db, gapHeader.Hash()); lerr != nil || stored == nil || len(stored.NextEpochCandidates) != len(pool) {
		t.Fatalf("state-first snapshot not persisted correctly: err=%v", lerr)
	}
	t.Logf("PASS: state-first gap snapshot = contract pool (%d, stake-sorted, persisted)", len(pool))
}

// Test1330_BackwardSeed_NotPersisted reproduces the net5151 poisoning sequence:
//  1. regular-block query (no state available) → backward seed from the
//     CURRENT epoch's switch header (5 validators) — returned ephemerally,
//     with NOTHING persisted or cached at the gap hash;
//  2. a later gap-semantics query WITH state available must then return the
//     true 6-candidate contract pool (pre-fix it read back the persisted
//     5-entry seed and rejected the 6th masternode's signatures forever).
func Test1330_BackwardSeed_NotPersisted(t *testing.T) {
	eng, db := newA894Engine(t)

	activeSet := []common.Address{
		common.HexToAddress("0x1111111111111111111111111111111111111111"),
		common.HexToAddress("0x2222222222222222222222222222222222222222"),
		common.HexToAddress("0x3333333333333333333333333333333333333333"),
		common.HexToAddress("0x4444444444444444444444444444444444444444"),
		common.HexToAddress("0x5555555555555555555555555555555555555555"),
	}
	fullPool := append(append([]common.Address{}, activeSet...),
		common.HexToAddress("0xA119821F0963EcEF888b4de575174999b1689C7D"))

	const gap = uint64(6750)
	gapHeader := &types.Header{Number: big.NewInt(int64(gap)), Difficulty: big.NewInt(1), Extra: []byte{0x02}}
	// Epoch-switch header 7199 (round 5400) carrying the STALE active set —
	// what the backward walk from block 7530 finds.
	switchHeader := makeMidEpochSwitchHeader(t, 7199, 5400, activeSet)

	chainNoState := &mockChain1330{
		byNumber: map[uint64]*types.Header{gap: gapHeader, 7199: switchHeader},
	}

	// Step 1: regular-block query at 7530 (mid-epoch, forSigning=false,
	// parents=nil) → backward seed path.
	snap, err := eng.getSnapshot(chainNoState, 7530, false, nil)
	if err != nil {
		t.Fatalf("getSnapshot(backward seed): %v", err)
	}
	if len(snap.NextEpochCandidates) != len(activeSet) {
		t.Fatalf("backward seed should still answer the caller with the current active set (5), got %d", len(snap.NextEpochCandidates))
	}
	// CORE ASSERTION: nothing persisted or cached at the gap hash.
	if stored, lerr := loadSnapshot(db, gapHeader.Hash()); lerr == nil && stored != nil {
		t.Fatalf("FAIL #1330: backward seed was PERSISTED at gap hash (candidates=%d) — poisons TC/vote membership", len(stored.NextEpochCandidates))
	}
	if _, ok := eng.snapshots.Get(gapHeader.Hash()); ok {
		t.Fatalf("FAIL #1330: backward seed was CACHED at gap hash")
	}
	t.Log("PASS: backward seed returned ephemerally, gap slot left unpopulated")

	// Step 2: gap-semantics query with state now available must produce the
	// true contract pool (6) — the recovery pre-fix never got.
	statedb := seedCandidateState(t, fullPool)
	chainWithState := &mockChain1330WithState{mockChain1330{
		byNumber: map[uint64]*types.Header{gap: gapHeader, 7199: switchHeader},
		statedb:  statedb,
	}}
	snap2, err := eng.getSnapshot(chainWithState, gap, true, nil)
	if err != nil {
		t.Fatalf("getSnapshot(gap, state): %v", err)
	}
	if len(snap2.NextEpochCandidates) != len(fullPool) {
		t.Fatalf("FAIL #1330: gap snapshot want %d (contract pool) got %d", len(fullPool), len(snap2.NextEpochCandidates))
	}
	found := false
	for _, a := range snap2.NextEpochCandidates {
		if a == fullPool[len(fullPool)-1] {
			found = true
		}
	}
	if !found {
		t.Fatalf("FAIL #1330: proposed-but-unseated candidate absent from gap pool")
	}
	t.Logf("PASS: gap slot materialised from contract state (%d candidates incl. unseated)", len(fullPool))
}

// Test1330_SendVote_NoLatchOnLocalDrop: a vote dropped by the LOCAL handler
// (never broadcast) must not latch highestVotedRound; once the cause is
// cleared, the node can still vote the same round.
func Test1330_SendVote_NoLatchOnLocalDrop(t *testing.T) {
	eng, _ := newA894Engine(t)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer := crypto.PubkeyToAddress(key.PublicKey)
	eng.Authorize(signer, func(_ accounts.Account, data []byte) ([]byte, error) {
		return crypto.Sign(data, key)
	})

	// Epoch info for the voted block: switch at 7199 → gapNumber 5850.
	const votedRound = types.Round(6302)
	blockHash := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000034d2")
	eng.epochSwitches.Add(blockHash, &types.EpochSwitchInfo{
		Masternodes:    make([]common.Address, 6),
		MasternodesLen: 6,
		EpochSwitchBlockInfo: &types.BlockInfo{
			Hash:   blockHash,
			Round:  votedRound,
			Number: big.NewInt(7199),
		},
	})
	blockInfo := &types.BlockInfo{Hash: blockHash, Round: votedRound, Number: big.NewInt(8099)}

	// Gap header + POISONED snapshot (signer absent) at gap 5850.
	gapHeader := &types.Header{Number: big.NewInt(5850), Difficulty: big.NewInt(1), Extra: []byte{0x02}}
	chain := &mockChain1330{byNumber: map[uint64]*types.Header{5850: gapHeader}}
	otherAddrs := []common.Address{{0x01}, {0x02}, {0x03}, {0x04}, {0x05}}
	eng.snapshots.Add(gapHeader.Hash(), newSnapshot(5850, gapHeader.Hash(), otherAddrs))

	eng.currentRound = votedRound
	eng.isInitialized = true // skip lazy init in VerifyVoteMessage

	if err := eng.sendVote(chain, blockInfo); err == nil {
		t.Fatalf("sendVote should fail while the local snapshot excludes the signer")
	}
	if eng.highestVotedRound != 0 {
		t.Fatalf("FAIL #1330: highestVotedRound latched to %d on a locally-dropped (never-broadcast) vote", eng.highestVotedRound)
	}
	t.Log("PASS: locally-dropped vote did not latch highestVotedRound")

	// Clear the cause: snapshot now includes the signer → same-round retry must succeed.
	eng.snapshots.Add(gapHeader.Hash(), newSnapshot(5850, gapHeader.Hash(), append(otherAddrs, signer)))
	if err := eng.sendVote(chain, blockInfo); err != nil {
		t.Fatalf("sendVote retry should succeed once the snapshot includes the signer: %v", err)
	}
	if eng.highestVotedRound != votedRound {
		t.Fatalf("highestVotedRound want %d after successful vote, got %d", votedRound, eng.highestVotedRound)
	}
	t.Log("PASS: successful vote latches highestVotedRound (equivocation guard intact)")
}
