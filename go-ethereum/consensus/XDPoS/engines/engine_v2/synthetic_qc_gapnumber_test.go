// Copyright (c) 2026 XDC Network
// Fix #1327: bulk-sync synthetic-epoch QC verification must use the HISTORICAL
// active masternode set (indexed by the header's own QC.GapNumber), not the
// next-epoch candidate pool stored at the header's hash.
//
// Bug (net5151 block 5851): a 5th masternode (xOne) was PROPOSED at ~block 5280
// (candidate pool → 5) but only SEATED at epoch switch 6300 (round 4501). The QC
// over gap block 5850 (round 4050) was correctly signed by 3 of the 4 ACTIVE
// masternodes (threshold ceil(4*2/3)=2.668). During bulk catch-up the parent of
// 5850 is absent from DB, so getEpochSwitchInfo's synthetic fallback fired and
// loaded the snapshot AT 5850's hash — the NEXT epoch's pool (5) — yielding
// MasternodesLen=5 → threshold 3.335 → valid canonical block 5851 rejected, and
// the wrong info was cached by hash, wedging catch-up permanently.
//
// Fix: Source B0 — prefer the snapshot indexed by the header's own QC.GapNumber
// (the seating snapshot of the CURRENT epoch, whose activeSet signed the QCs),
// falling back to the legacy snapshot-at-hash (Source B, P0 #894) only when the
// gap header cannot be resolved (true checkpoint-sync boundary).
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run TestGapNumber1327 -count=1

package engine_v2

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// mockChainForGapNumber1327 is a consensus.ChainReader that resolves headers by
// number and by hash from explicit maps — parent-of-target deliberately absent.
type mockChainForGapNumber1327 struct {
	byHash   map[common.Hash]*types.Header
	byNumber map[uint64]*types.Header
}

func (m *mockChainForGapNumber1327) Config() *params.ChainConfig                   { return nil }
func (m *mockChainForGapNumber1327) CurrentHeader() *types.Header                  { return nil }
func (m *mockChainForGapNumber1327) GetBlock(h common.Hash, n uint64) *types.Block { return nil }
func (m *mockChainForGapNumber1327) GetTd(h common.Hash, n uint64) *big.Int        { return nil }
func (m *mockChainForGapNumber1327) GetHeader(h common.Hash, n uint64) *types.Header {
	return m.byHash[h]
}
func (m *mockChainForGapNumber1327) GetHeaderByHash(h common.Hash) *types.Header {
	return m.byHash[h]
}
func (m *mockChainForGapNumber1327) GetHeaderByNumber(n uint64) *types.Header {
	return m.byNumber[n]
}

// TestGapNumber1327_SyntheticPath_UsesHistoricalActiveSet reproduces the
// net5151 block-5851 wedge and asserts the fix:
//   - snapshot at h's hash: NEXT epoch pool = 5 candidates (xOne proposed,
//     not seated) — the WRONG set for QCs at h's round;
//   - snapshot at h's QC.GapNumber (gap 4950): the 4 ACTIVE masternodes —
//     the set that actually signed;
//   - parent of h absent (bulk-sync batch verification).
//
// getEpochSwitchInfo must return MasternodesLen=4 (pre-fix: 5), and a 3-of-4
// QC over h must verify (pre-fix: "invalid QC signatures", threshold 3.335).
func TestGapNumber1327_SyntheticPath_UsesHistoricalActiveSet(t *testing.T) {
	eng, db := newA894Engine(t)

	// 4 active masternodes with real keys (they sign the QC) + 1 proposed-only
	// candidate (no signature — mirrors xOne pre-seating).
	activeKeys, activeAddrs := genNKeys(t, 4)
	_, proposedAddr := genNKeys(t, 1)
	nextPool := append(append([]common.Address{}, activeAddrs...), proposedAddr[0])

	// net5151 geometry: epoch=900 gap=450 (newA894Engine), h = gap block 5850,
	// round 4050 (mid-epoch: epoch switch was 5400/round 3600), parent round 4049,
	// QC.GapNumber = 4950 (floor(5850/900)*900 - 450).
	const gapBlock = uint64(5850)
	const seatingGap = uint64(4950)
	hdr, hdrHash := makeGapHeader(t, gapBlock, 4049, seatingGap)

	// Snapshot AT h's hash: next-epoch pool of 5, no penalties (legacy Source B
	// would return activeSet=5 → the bug).
	snapAtHash := newSnapshot(gapBlock, hdrHash, nextPool)
	if err := storeSnapshot(snapAtHash, db); err != nil {
		t.Fatalf("storeSnapshot(atHash): %v", err)
	}

	// Seating snapshot at gap 4950: the 4 active masternodes. Keyed by the gap
	// header's hash, resolved via GetHeaderByNumber(4950).
	gapHeader4950 := &types.Header{
		Number:     big.NewInt(int64(seatingGap)),
		Difficulty: big.NewInt(1),
		Extra:      []byte{0x01}, // content irrelevant; only Hash() identity matters
	}
	snapSeating := newSnapshot(seatingGap, gapHeader4950.Hash(), activeAddrs)
	if err := storeSnapshot(snapSeating, db); err != nil {
		t.Fatalf("storeSnapshot(seating): %v", err)
	}

	chain := &mockChainForGapNumber1327{
		byHash:   map[common.Hash]*types.Header{gapHeader4950.Hash(): gapHeader4950},
		byNumber: map[uint64]*types.Header{seatingGap: gapHeader4950},
		// h's parent (5849) deliberately absent → synthetic fallback fires.
	}

	// Core assertion: synthetic epoch info must carry the HISTORICAL active set.
	info, err := eng.getEpochSwitchInfo(chain, hdr, hdrHash)
	if err != nil {
		t.Fatalf("getEpochSwitchInfo: %v", err)
	}
	if info.MasternodesLen != 4 {
		t.Fatalf("FAIL #1327: MasternodesLen want 4 (historical active set) got %d (threshold %.3f would reject a valid 3-sig QC)",
			info.MasternodesLen, float64(info.MasternodesLen)*2.0/3.0)
	}
	for _, mn := range info.Masternodes {
		if mn == proposedAddr[0] {
			t.Fatalf("FAIL #1327: proposed-but-unseated candidate %s in active Masternodes", mn.Hex())
		}
	}
	t.Logf("PASS: synthetic epoch info uses GapNumber-indexed seating snapshot (MasternodesLen=%d)", info.MasternodesLen)

	// End-to-end: 3-of-4 QC over h must verify (threshold 4*2/3=2.668).
	eng.epochSwitches.Purge()
	proposedBlock := &types.BlockInfo{
		Hash:   hdrHash,
		Round:  4050,
		Number: big.NewInt(int64(gapBlock)),
	}
	sigs3 := makeQCSignatures(t, proposedBlock, seatingGap, activeKeys[:3])
	qc3 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs3,
		GapNumber:         seatingGap,
	}
	if err := eng.verifyQC(chain, qc3, hdr, nil); err != nil {
		t.Fatalf("FAIL #1327: verifyQC with 3-of-4 historical active signers should PASS, got: %v", err)
	}
	t.Log("PASS: 3-of-4 QC over the gap block verifies against the historical active set")

	// Sanity: 2-of-4 stays below threshold (2 < 2.668) — the fix must not
	// weaken the threshold, only correct the set it is computed from.
	eng.epochSwitches.Purge()
	sigs2 := makeQCSignatures(t, proposedBlock, seatingGap, activeKeys[:2])
	qc2 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs2,
		GapNumber:         seatingGap,
	}
	if err := eng.verifyQC(chain, qc2, hdr, nil); err == nil {
		t.Fatal("FAIL: 2-of-4 QC should be rejected (below 2/3 threshold)")
	}
	t.Log("PASS: 2-of-4 QC correctly rejected")
}

// TestPenaltyGap_SyntheticFallback_DBWalkback reproduces the net5151 block-8678
// xdcSyncer wedge and asserts the DB walk-back fix:
//
//   - gap snapshot at GapNumber=6750 has 5 candidates, no Penalties
//     (activeLenSource="fallback" — HookPenalty ran AFTER the gap block was
//     imported, at the epoch-switch block 7783).
//   - epoch-switch block 7783 IS in the canonical DB (committed in a prior batch).
//   - parent of h (block 8677) is NOT in DB (h is in the current xdcSyncer batch),
//     triggering the synthetic fallback in getEpochSwitchInfoInner.
//
// Pre-fix: Source B0 returns activeLen=5 from gap snapshot → threshold 3.335
//
//	rejects the valid 3-of-4 canonical QC; wrong info is cached at
//	block8678.Hash, wedging every subsequent xdcSyncer batch permanently.
//
// Post-fix: when activeLenSource="fallback" the DB walk-back finds epoch-switch
//
//	block 7783 with 4 header.Validators, overrides masternodesLen=4, and
//	the 3-of-4 QC passes threshold ceil(4×2/3)=2.668.
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run TestPenaltyGap -count=1
func TestPenaltyGap_SyntheticFallback_DBWalkback(t *testing.T) {
	eng, db := newA894Engine(t)

	// 4 active masternodes (sign the QC) + 1 candidate penalized at epoch-switch
	// block 7783 (present in gap-snapshot pool but excluded from Validators).
	activeKeys, activeAddrs := genNKeys(t, 4)
	_, penalizedAddrs := genNKeys(t, 1)
	pool5 := append(append([]common.Address{}, activeAddrs...), penalizedAddrs[0])

	// net5151 geometry: epoch=900 gap=450.
	// Epoch round [6300, 7200): epoch-switch block 7783 (round=6300, 4 masternodes).
	// Gap block derivation: checkpointByBlockNum = 7783 - (7783%900) = 7200;
	//                       gapNumber = 7200 - 450 = 6750.
	const (
		gapBlock         = uint64(6750)
		epochSwitchBlock = uint64(7783)
		epochSwitchRound = types.Round(6300) // 6300 % 900 == 0 → epoch switch
		hBlock           = uint64(8678)
		hQCRound         = types.Round(7194) // round of h's parent (block 8677)
	)

	// Gap snapshot: 5 candidates, NO Penalties → activeLenSource="fallback" → ActiveLen=5.
	// This is the buggy pre-fix state: the penalty was applied at epoch-switch 7783,
	// not at gap-block import time, so the gap snapshot carries no penalty data.
	gapHeader := &types.Header{
		Number:     big.NewInt(int64(gapBlock)),
		Difficulty: big.NewInt(1),
		Extra:      []byte{0x01},
	}
	snapGap := newSnapshot(gapBlock, gapHeader.Hash(), pool5)
	if err := storeSnapshot(snapGap, db); err != nil {
		t.Fatalf("storeSnapshot(gap): %v", err)
	}

	// Epoch-switch header at block 7783 with 4 masternodes in Validators
	// (post-penalty: penalizedAddrs[0] excluded). The DB walk-back fix uses this.
	esSwitchHeader := makeMidEpochSwitchHeader(t, epochSwitchBlock, epochSwitchRound, activeAddrs)

	// Test block h at 8678, mid-epoch. Its parent (8677) is absent from chain
	// (bulk-sync batch scenario). QC.GapNumber = gapBlock = 6750.
	hdr, hdrHash := makeGapHeader(t, hBlock, hQCRound, gapBlock)

	chain := &mockChainForGapNumber1327{
		byHash: map[common.Hash]*types.Header{
			gapHeader.Hash(): gapHeader,
			// h.ParentHash (block 8677) deliberately absent → synthetic fallback.
		},
		byNumber: map[uint64]*types.Header{
			gapBlock:         gapHeader,      // needed by getSnapshot (Source B0)
			epochSwitchBlock: esSwitchHeader, // found by the DB walk-back fix
		},
	}

	// Core assertion: DB walk-back must find epoch-switch 7783 and return
	// masternodesLen=4 (not 5 from the gap snapshot's inflated pool).
	info, err := eng.getEpochSwitchInfo(chain, hdr, hdrHash)
	if err != nil {
		t.Fatalf("getEpochSwitchInfo: %v", err)
	}
	if info.MasternodesLen != 4 {
		t.Fatalf("FAIL penalty-gap: MasternodesLen want 4 (DB walk-back to epoch-switch) got %d"+
			" (threshold %.3f would reject the valid 3-sig canonical QC)",
			info.MasternodesLen, float64(info.MasternodesLen)*2.0/3.0)
	}
	for _, mn := range info.Masternodes {
		if mn == penalizedAddrs[0] {
			t.Fatalf("FAIL penalty-gap: penalized address %s present in active Masternodes", mn.Hex())
		}
	}
	t.Logf("PASS: DB walk-back found epoch-switch at %d, MasternodesLen=%d", epochSwitchBlock, info.MasternodesLen)

	// End-to-end: 3-of-4 QC over h must verify (threshold ceil(4×2/3)=2.668).
	eng.epochSwitches.Purge()
	proposedBlock := &types.BlockInfo{
		Hash:   hdrHash,
		Round:  hQCRound + 1,
		Number: big.NewInt(int64(hBlock)),
	}
	sigs3 := makeQCSignatures(t, proposedBlock, gapBlock, activeKeys[:3])
	qc3 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs3,
		GapNumber:         gapBlock,
	}
	if err := eng.verifyQC(chain, qc3, hdr, nil); err != nil {
		t.Fatalf("FAIL penalty-gap: 3-of-4 QC should PASS with fix, got: %v", err)
	}
	t.Log("PASS: 3-of-4 QC verifies against the post-penalty active set (masternodesLen=4)")

	// Sanity: 2-of-4 must still fail (below threshold 2.668).
	eng.epochSwitches.Purge()
	sigs2 := makeQCSignatures(t, proposedBlock, gapBlock, activeKeys[:2])
	qc2 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs2,
		GapNumber:         gapBlock,
	}
	if err := eng.verifyQC(chain, qc2, hdr, nil); err == nil {
		t.Fatal("FAIL penalty-gap: 2-of-4 QC should be rejected (below 2/3 threshold)")
	}
	t.Log("PASS: 2-of-4 QC correctly rejected under post-penalty threshold")
}

// TestPenaltyGap_FieldActiveLen_DBWalkback tests the iteration-2 fix:
// the DB walk-back now fires even when the gap snapshot has an EXPLICIT
// ActiveMasternodesLen ("field" activeLenSource, not "fallback").
//
// Scenario (mirrors net5151 blocks 8683-8712):
//   - At gap block 7650 (pre-penalty-clearing): pool=5 (xOne in pool but penalized)
//     → snapshot has Penalties=[xOne], ActiveMasternodesLen=4, activeLenSource="field".
//   - At epoch-switch block 8683 (round 7200): penalty clears, xOne rejoins.
//     header.Validators encodes all 5 masternodes.
//   - Block h at 8712 (round 7226): parent (8711) absent from DB (batch-sync race).
//
// Iteration-1 bug: gate was `activeLenSrc == "fallback"` — "field" skipped the walk,
// cached masternodes=4 at hash8712, poisoning the next batch (block 8713 got 4-set
// cache hit even after parents were committed to DB).
//
// Fix 1 (this commit): remove the gate → walk fires for "field" source → finds
// epoch-switch at 8683 → masternodes=5 (correct).
// Fix 2 (this commit): do NOT cache the synthetic result → next batch re-computes
// fresh via DB recursion with parents in DB → no stale cache entry.
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run TestPenaltyGap_FieldActiveLen -count=1
func TestPenaltyGap_FieldActiveLen_DBWalkback(t *testing.T) {
	eng, db := newA894Engine(t)

	// 4 active masternodes + xOne (penalized at gap, re-joins at epoch switch 8683)
	activeKeys, activeAddrs := genNKeys(t, 4)
	_, penalizedAddrs := genNKeys(t, 1)
	pool5 := append(append([]common.Address{}, activeAddrs...), penalizedAddrs[0])

	// net5151 geometry: epoch=900 gap=450.
	// Epoch at round 7200 (block 8683): xOne re-joins.
	// GapNumber formula (mirrors vote.go): epochSwitch - epochSwitch%epoch - gap
	//   = 8683 - 8683%900 - 450 = 8683 - 583 - 450 = 7650.
	const (
		gapBlock         = uint64(7650)
		epochSwitchBlock = uint64(8683)
		epochSwitchRound = types.Round(7200) // 7200%900==0: epoch boundary
		hBlock           = uint64(8712)      // mid-epoch in post-7200 epoch
		hQCRound         = types.Round(7225) // h.Round = 7226, QC certifies 7225
	)

	// Gap snapshot at 7650: pool=5 (xOne in pool), Penalties=[xOne]
	// → activeLenSource="field", ActiveMasternodesLen=4.
	// This is the pre-fix stale state: penalty clears only at epoch-switch 8683,
	// but the gap snapshot was written at block 7650 before the switch.
	gapHeader := &types.Header{
		Number:     big.NewInt(int64(gapBlock)),
		Difficulty: big.NewInt(1),
		Extra:      []byte{0x01},
	}
	snapGap := newSnapshotWithPenalties(gapBlock, gapHeader.Hash(), pool5, penalizedAddrs)
	if snapGap.activeLenSource() != "field" {
		t.Fatalf("prerequisite: gap snapshot must have activeLenSource=field, got %q", snapGap.activeLenSource())
	}
	if snapGap.ActiveLen() != 4 {
		t.Fatalf("prerequisite: gap snapshot ActiveLen want 4 got %d", snapGap.ActiveLen())
	}
	if err := storeSnapshot(snapGap, db); err != nil {
		t.Fatalf("storeSnapshot(gap): %v", err)
	}

	// Epoch-switch header at 8683: penalty cleared, xOne rejoins.
	// header.Validators encodes all 5 masternodes — the DB walk uses this.
	allFive := append(append([]common.Address{}, activeAddrs...), penalizedAddrs[0])
	esSwitchHeader := makeMidEpochSwitchHeader(t, epochSwitchBlock, epochSwitchRound, allFive)

	// Block h at 8712, mid-epoch. Parent (8711) is absent → synthetic fallback.
	hdr, hdrHash := makeGapHeader(t, hBlock, hQCRound, gapBlock)

	chain := &mockChainForGapNumber1327{
		byHash: map[common.Hash]*types.Header{
			gapHeader.Hash(): gapHeader,
			// hdr.ParentHash (block 8711) deliberately absent → parent-nil path.
		},
		byNumber: map[uint64]*types.Header{
			gapBlock:         gapHeader,
			epochSwitchBlock: esSwitchHeader, // DB walk must find this
		},
	}

	// Assert 1: DB walk fires for "field" source and returns 5 masternodes (xOne re-joined).
	info, err := eng.getEpochSwitchInfo(chain, hdr, hdrHash)
	if err != nil {
		t.Fatalf("getEpochSwitchInfo: %v", err)
	}
	if info.MasternodesLen != 5 {
		t.Fatalf("FAIL field-gate: MasternodesLen want 5 (DB walk to post-penalty epoch-switch) got %d"+
			" (pre-fix: 'field' source skipped the walk, returned stale 4-set from gap snapshot)",
			info.MasternodesLen)
	}
	found := false
	for _, mn := range info.Masternodes {
		if mn == penalizedAddrs[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("FAIL field-gate: xOne (%s) not in Masternodes after penalty cleared", penalizedAddrs[0].Hex())
	}
	t.Logf("PASS: DB walk fired for field-source, returned %d masternodes including xOne", info.MasternodesLen)

	// Assert 2: synthetic result must NOT be cached (no-caching fix).
	// Pre-fix: the stale 4-masternode result was cached at hdrHash; subsequent blocks
	// in the next batch would recurse to that cache hit and also get 4 masternodes.
	if _, cached := eng.epochSwitches.Get(hdrHash); cached {
		t.Fatal("FAIL no-cache: synthetic epoch info must NOT be cached (stale entry poisons next batch)")
	}
	t.Log("PASS: synthetic result not cached (next batch re-computes via DB recursion)")

	// Assert 3: 4-of-5 QC (activeKeys signs) passes threshold ceil(5×2/3)=3.333→≥4.
	eng.epochSwitches.Purge()
	proposedBlock := &types.BlockInfo{
		Hash:   hdrHash,
		Round:  hQCRound + 1,
		Number: big.NewInt(int64(hBlock)),
	}
	sigs4 := makeQCSignatures(t, proposedBlock, gapBlock, activeKeys) // 4 of the 5 masternodes
	qc4 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs4,
		GapNumber:         gapBlock,
	}
	if err := eng.verifyQC(chain, qc4, hdr, nil); err != nil {
		t.Fatalf("FAIL field-gate: 4-of-5 QC should pass with fix, got: %v", err)
	}
	t.Log("PASS: 4-of-5 QC verifies against 5-masternode post-penalty set")

	// Assert 4: 3-of-5 must fail (3 < ceil(5×2/3)=3.333).
	eng.epochSwitches.Purge()
	sigs3 := makeQCSignatures(t, proposedBlock, gapBlock, activeKeys[:3])
	qc3 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs3,
		GapNumber:         gapBlock,
	}
	if err := eng.verifyQC(chain, qc3, hdr, nil); err == nil {
		t.Fatal("FAIL field-gate: 3-of-5 QC should be rejected (below threshold)")
	}
	t.Log("PASS: 3-of-5 QC correctly rejected under 5-masternode threshold")
}
