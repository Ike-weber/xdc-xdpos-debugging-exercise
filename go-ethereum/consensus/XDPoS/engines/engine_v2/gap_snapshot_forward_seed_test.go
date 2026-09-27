// Copyright (c) 2026 XDC Network
// live-mn-set fix: forSigning gap-snapshot forward-seed must scan from
// gapNumber+Gap-1, not gapNumber, so it lands on the epoch-switch header that
// seats the epoch THIS gap block defines rather than an earlier, intermediate
// epoch switch that can sit between gapNumber and gapNumber+Gap when rounds
// skip multiple epoch boundaries.
//
// Bug: on devnet-5151, a stale/poisoned Version-5 snapshot at a gap block
// excluded the local node's own coinbase (0xA119...) from its vote/TC
// membership set, because forSigning callers either trusted the stale
// snapshot or (if forced to reseed) scanned forward from gapNumber and
// stopped at the first epoch switch found — which could be an intermediate
// switch with the WRONG (smaller, pre-seating) masternode set. Example from
// production: gap 22050, intermediate switch at 22097 (6 validators), correct
// switch at 22942 (7 validators including the newly-seated node).
//
// This test reproduces that shape at a smaller scale (epoch=900, gap=450):
//   - gapNumber = 4950
//   - intermediate epoch-switch header at 5000 (6 masternodes) — sits between
//     gapNumber and gapNumber+Gap-1 (5399); must NOT be selected.
//   - correct epoch-switch header at 5400 (7 masternodes, matching forwardFrom
//     = gapNumber+Gap-1 = 5399, so the DB walk-forward starts at 5400) — must
//     be selected.
//   - a poisoned Version-5 snapshot is pre-stored in the DB at the gap
//     header's hash; the fix must overwrite it with a Version-6 entry rather
//     than trusting it.
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run TestLiveMnSet_ForwardSeed -count=1
package engine_v2

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/params"
)

// makeRoundEpochSwitchHeader builds a synthetic V2 header at blockNum whose
// own round is `round` and whose QC references a parent at `parentRound`.
// IsEpochSwitch treats this as an epoch switch whenever parentRound is in an
// earlier Epoch-quantum than round (parentRound < round-round%Epoch).
// masternodes are packed into header.Validators exactly like a real V2 epoch
// switch header (GetMasternodesFromEpochSwitchHeader reads this field
// directly, matching canonical XDPoSChain).
func makeRoundEpochSwitchHeader(t *testing.T, blockNum uint64, round, parentRound types.Round, masternodes []common.Address) *types.Header {
	t.Helper()

	validators := make([]byte, len(masternodes)*common.AddressLength)
	for i, mn := range masternodes {
		copy(validators[i*common.AddressLength:], mn[:])
	}

	extraFields := &types.ExtraFields_v2{
		Round: round,
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.HexToHash("0xdeadbeef"),
				Round:  parentRound,
				Number: big.NewInt(int64(blockNum - 1)),
			},
			Signatures: []types.Signature{},
			GapNumber:  0, // not exercised by findEpochSwitchAfter/IsEpochSwitch
		},
	}
	extra, err := extraFields.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes unavailable: %v", err)
	}

	return &types.Header{
		Number:     big.NewInt(int64(blockNum)),
		Extra:      extra,
		Validators: validators,
		Difficulty: big.NewInt(1),
	}
}

// TestLiveMnSet_ForwardSeed_SkipsIntermediateEpochSwitch is the headline
// assertion: getSnapshot(gapNumber, forSigning=true) must seed from the
// epoch-switch header at/after gapNumber+Gap-1, never from an earlier
// intermediate epoch switch that happens to sit between gapNumber and that
// point.
func TestLiveMnSet_ForwardSeed_SkipsIntermediateEpochSwitch(t *testing.T) {
	dir := t.TempDir()
	lvl, err := leveldb.New(dir, 256, 0, "", false)
	if err != nil {
		t.Fatalf("leveldb: %v", err)
	}
	db := rawdb.NewDatabase(lvl)

	cfg := &params.ChainConfig{
		ChainID: big.NewInt(5151),
		XDPoS: &params.XDPoSConfig{
			Epoch: 900,
			Gap:   450,
			V2: &params.V2{
				SwitchBlock: new(big.Int).SetUint64(0),
				CurrentConfig: &params.V2Config{
					MinePeriod:    2,
					TimeoutPeriod: 10,
					ExpTimeoutConfig: params.ExpTimeoutConfig{
						Base:        2.0,
						MaxExponent: 6,
					},
					CertThreshold: 2.0 / 3.0,
				},
				AllConfigs: map[uint64]*params.V2Config{},
			},
		},
	}
	eng := createEngine(cfg, db, nil, nil)

	const gapNumber = uint64(4950) // 4950 % 900 == 450 == Epoch-Gap: a direct gap number
	// checkpointNumber (as computed inside getSnapshot) = 4950 - 450 = 4500,
	// which is > firstV2CheckpointForSigning (900), so the forSigning fix path
	// applies.
	// forwardFrom = gapNumber + Gap - 1 = 5399 => DB walk starts scanning at 5400.

	_, wrongMNs := genNKeys(t, 6)   // intermediate/wrong epoch switch: 6 validators
	_, correctMNs := genNKeys(t, 7) // correct epoch switch: 7 validators (matches prod: incl. newly-seated node)

	// Intermediate epoch switch at 5000 — between gapNumber (4950) and
	// forwardFrom (5399). round=5000 crosses into epoch-quantum 4500
	// (5000 - 5000%900 = 4500) from parentRound=4499 < 4500.
	wrongHeader := makeRoundEpochSwitchHeader(t, 5000, 5000, 4499, wrongMNs)

	// Correct epoch switch at 5400 — at forwardFrom+1, the first block the DB
	// walk-forward scans. round=5400 crosses into epoch-quantum 5400
	// (5400 - 5400%900 = 5400) from parentRound=5399 < 5400.
	correctHeader := makeRoundEpochSwitchHeader(t, 5400, 5400, 5399, correctMNs)

	gapHeader := &types.Header{
		Number:     big.NewInt(int64(gapNumber)),
		Difficulty: big.NewInt(1),
		Extra:      []byte{0x01}, // content irrelevant; only Hash() identity matters
	}

	chain := &mockChainForGapNumber1327{
		byHash: map[common.Hash]*types.Header{
			gapHeader.Hash():     gapHeader,
			wrongHeader.Hash():   wrongHeader,
			correctHeader.Hash(): correctHeader,
		},
		byNumber: map[uint64]*types.Header{
			gapNumber: gapHeader,
			5000:      wrongHeader,
			5400:      correctHeader,
			// gapNumber-1 (the gap block's parent) deliberately absent — this
			// mock chain does not implement StateAt, so rebuildGapSnapshotFromState
			// short-circuits to nil regardless, forcing the forward-seed path.
		},
	}

	// Poison the DB with a stale Version-5 snapshot at the gap header's hash —
	// simulating a pre-fix-#1330 binary's backward-seeded snapshot that
	// excluded the newly-seated masternode.
	poisoned := newSnapshot(gapNumber, gapHeader.Hash(), wrongMNs)
	poisoned.Version = 5
	if err := storeSnapshot(poisoned, db); err != nil {
		t.Fatalf("storeSnapshot(poisoned): %v", err)
	}

	// Sanity: nothing pre-seeded in the in-memory cache, so the DB entry above
	// is what a naive caller would otherwise trust.
	if _, ok := eng.snapshots.Get(gapHeader.Hash()); ok {
		t.Fatalf("test setup error: snapshot unexpectedly already cached")
	}

	snap, err := eng.getSnapshot(chain, gapNumber, true, nil)
	if err != nil {
		t.Fatalf("getSnapshot(forSigning=true): %v", err)
	}
	if snap == nil {
		t.Fatalf("getSnapshot returned nil snapshot")
	}

	if snap.Version != 6 {
		t.Fatalf("FAIL: snapshot Version = %d, want 6 (forward-seed/state-certified) — poisoned Version-5 entry must be overwritten", snap.Version)
	}

	if len(snap.NextEpochCandidates) != len(correctMNs) {
		t.Fatalf("FAIL: seeded from wrong epoch switch — got %d masternodes, want %d (correct switch at 5400)",
			len(snap.NextEpochCandidates), len(correctMNs))
	}
	seeded := make(map[common.Address]bool, len(snap.NextEpochCandidates))
	for _, a := range snap.NextEpochCandidates {
		seeded[a] = true
	}
	for _, mn := range correctMNs {
		if !seeded[mn] {
			t.Fatalf("FAIL: correct-epoch-switch masternode %s missing from seeded snapshot", mn.Hex())
		}
	}
	for _, mn := range wrongMNs {
		if seeded[mn] {
			t.Fatalf("FAIL: intermediate/wrong-epoch-switch masternode %s leaked into seeded snapshot (scan should have started at forwardFrom+1=5400, skipping block 5000)", mn.Hex())
		}
	}
	t.Logf("PASS: getSnapshot(gap=%d, forSigning=true) seeded from the epoch switch AT/AFTER the checkpoint (block 5400, %d validators), skipping the intermediate switch at block 5000 (%d validators), and overwrote the poisoned Version-5 DB entry with Version=6",
		gapNumber, len(correctMNs), len(wrongMNs))

	// Confirm the DB entry itself was overwritten (persisted), not just the
	// in-memory cache, so a restart does not resurrect the poisoned data.
	reloaded, err := loadSnapshot(db, gapHeader.Hash())
	if err != nil {
		t.Fatalf("loadSnapshot after fix: %v", err)
	}
	if reloaded.Version != 6 {
		t.Fatalf("FAIL: DB snapshot Version = %d after fix, want 6 (persisted overwrite)", reloaded.Version)
	}
	if len(reloaded.NextEpochCandidates) != len(correctMNs) {
		t.Fatalf("FAIL: DB snapshot has %d candidates, want %d (correct epoch switch)", len(reloaded.NextEpochCandidates), len(correctMNs))
	}
}
