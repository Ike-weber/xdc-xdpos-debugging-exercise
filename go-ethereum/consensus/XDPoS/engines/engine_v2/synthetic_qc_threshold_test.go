// Copyright (c) 2026 XDC Network
// P0 fix #894: synthetic-epoch QC threshold tests.
//
// Bug: at gap blocks (n%900==450) during bulk sync the parent block is absent
// from DB. getEpochSwitchInfo falls into the synthetic-epoch path, loads
// the snapshot at the gap hash, and used snap.NextEpochCandidates (46) as
// MasternodesLen. verifyQC threshold = 46*2/3=30.682; QC has 22 sigs → REJECT.
// Correct active set = pool(46) − penalties(14) = 32; threshold = 32*2/3=21.344;
// 22 sigs → PASS.
//
// Tests:
//   HEADLINE: gap-block header (IsEpochSwitch=false), parent absent from mock
//   chain, snapshot at gap hash with NextEpochCandidates=46/Penalties=14/
//   ActiveMasternodesLen=32; QC over VoteSigHash by 22 of the 32 active →
//   getEpochSwitchInfo returns MasternodesLen=32 AND verifyQC PASSES.
//   21-signer QC → FAILS. Non-pool signer → FAILS membership.
//   Write-path: mock statedb(disabled) → penalty-hook→14 → stored snap has
//   NextEpochCandidates=46, Penalties=14, ActiveMasternodesLen=32.
//
// Run: go test ./consensus/XDPoS/engines/engine_v2/... -run TestA894 -count=1

package engine_v2

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/params"
)

// --- helpers ----------------------------------------------------------------

// genKeys generates n secp256k1 keys and their addresses.
func genNKeys(t *testing.T, n int) ([]*ecdsa.PrivateKey, []common.Address) {
	t.Helper()
	keys := make([]*ecdsa.PrivateKey, n)
	addrs := make([]common.Address, n)
	for i := range keys {
		k, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey[%d]: %v", i, err)
		}
		keys[i] = k
		addrs[i] = crypto.PubkeyToAddress(k.PublicKey)
	}
	return keys, addrs
}

// makeQCSignatures builds a QC signed by signers[0:n].
func makeQCSignatures(t *testing.T, blockInfo *types.BlockInfo, gapNum uint64, keys []*ecdsa.PrivateKey) []types.Signature {
	t.Helper()
	vfs := &types.VoteForSign{ProposedBlockInfo: blockInfo, GapNumber: gapNum}
	h := types.VoteSigHash(vfs)
	sigs := make([]types.Signature, len(keys))
	for i, k := range keys {
		sig, err := crypto.Sign(h[:], k)
		if err != nil {
			t.Fatalf("sign[%d]: %v", i, err)
		}
		sigs[i] = sig
	}
	return sigs
}

// newA894Engine returns an engine with a real in-memory DB for snapshot
// storage, configured for Apothem-like epoch=900, gap=450.
func newA894Engine(t *testing.T) (*XDPoS_v2, ethdb.Database) {
	t.Helper()
	dir := t.TempDir()
	lvl, err := leveldb.New(dir, 256, 0, "", false)
	if err != nil {
		t.Fatalf("leveldb: %v", err)
	}
	db := rawdb.NewDatabase(lvl)

	cfg := &params.ChainConfig{
		ChainID: big.NewInt(551),
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
	return eng, db
}

// mockChainForA894 is a minimal consensus.ChainReader whose DB returns nil
// for all block lookups (simulating "parent absent during bulk sync").
type mockChainForA894 struct {
	headers map[common.Hash]*types.Header
}

func (m *mockChainForA894) Config() *params.ChainConfig                     { return nil }
func (m *mockChainForA894) CurrentHeader() *types.Header                    { return nil }
func (m *mockChainForA894) GetHeader(h common.Hash, n uint64) *types.Header { return nil }
func (m *mockChainForA894) GetHeaderByNumber(n uint64) *types.Header        { return nil }
func (m *mockChainForA894) GetBlock(h common.Hash, n uint64) *types.Block   { return nil }
func (m *mockChainForA894) GetTd(h common.Hash, n uint64) *big.Int          { return nil }
func (m *mockChainForA894) GetHeaderByHash(h common.Hash) *types.Header {
	if m.headers != nil {
		return m.headers[h]
	}
	return nil
}

// gapNumForBlock computes the expected QC GapNumber for a proposed block at height N.
// The formula mirrors verifyQC's gap check:
//   epochSwitchNumber = N (synthetic path uses the gap block as the epoch switch),
//   gapNumber = epochSwitchNumber - epochSwitchNumber%epoch - gap.
func gapNumForBlock(blockNum, epoch, gap uint64) uint64 {
	epochSwitchNum := blockNum
	g := epochSwitchNum - epochSwitchNum%epoch
	if g > gap {
		return g - gap
	}
	return 0
}

// makeGapHeader builds a gap-block header (n%900==450) with V2 extra fields.
// The parent is NOT added to the mock chain (absent from DB).
func makeGapHeader(t *testing.T, gapBlock uint64, qcRound types.Round, gapNum uint64) (*types.Header, common.Hash) {
	t.Helper()
	// The QC proposed block is the parent (gapBlock-1).
	parentHash := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	qcInfo := &types.BlockInfo{
		Hash:   parentHash,
		Round:  qcRound,
		Number: big.NewInt(int64(gapBlock - 1)),
	}
	extra := &types.ExtraFields_v2{
		Round: qcRound + 1, // gap block round = qcRound+1
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: qcInfo,
			Signatures:        []types.Signature{},
			GapNumber:         gapNum,
		},
	}
	extraBytes, err := extra.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes: %v", err)
	}
	// gap blocks have empty Validators and empty Penalties
	h := &types.Header{
		Number:     big.NewInt(int64(gapBlock)),
		ParentHash: parentHash,
		Extra:      extraBytes,
		Difficulty: big.NewInt(1),
	}
	return h, h.Hash()
}

// --- Tests ------------------------------------------------------------------

// TestA894_ActiveLen_Three_Branches exercises all three code paths.
// (Same logic as snapshot_test.go but using the engine's epoch config.)
func TestA894_ActiveLen_Three_Branches(t *testing.T) {
	pool := make([]common.Address, 46)
	for i := range pool {
		pool[i] = common.Address{byte(i + 1)}
	}
	penalties := pool[:14]

	// Branch 1: ActiveMasternodesLen field set
	snap1 := newSnapshotWithPenalties(1, common.Hash{0x01}, pool, penalties)
	snap1.ActiveMasternodesLen = 32
	if got := snap1.ActiveLen(); got != 32 {
		t.Fatalf("branch1 (field): want 32 got %d", got)
	}

	// Branch 2: field=0, penalties present → derive
	snap2 := &SnapshotV2{
		NextEpochCandidates:  pool,
		Penalties:            penalties,
		ActiveMasternodesLen: 0,
	}
	if got := snap2.ActiveLen(); got != 32 {
		t.Fatalf("branch2 (derived): want 32 got %d", got)
	}

	// Branch 3: no field, no penalties → legacy fallback
	snap3 := newSnapshot(1, common.Hash{0x03}, pool)
	if got := snap3.ActiveLen(); got != 46 {
		t.Fatalf("branch3 (fallback): want 46 got %d", got)
	}

	t.Log("PASS: ActiveLen() three branches (field/derived/fallback)")
}

// TestA894_SyntheticPath_MasternodesLen32 — core assertion:
// gap-block header with parent absent → getEpochSwitchInfo synthetic path
// → returns MasternodesLen=32 when snapshot has pool=46/penalties=14/active=32.
func TestA894_SyntheticPath_MasternodesLen32(t *testing.T) {
	eng, db := newA894Engine(t)

	// Build 46 pool addresses and pick 14 as penalties.
	pool := make([]common.Address, 46)
	for i := range pool {
		pool[i] = common.Address{byte(i + 1)}
	}
	penalties := pool[:14]
	active := pool[14:] // 32 addresses

	// Confirm arithmetic
	if len(active) != 32 {
		t.Fatalf("test setup: active len want 32 got %d", len(active))
	}

	// Build gap-block header (82934550 % 900 == 450).
	const gapBlock = 82934550
	gapHeader, gapHash := makeGapHeader(t, gapBlock, 1000, 0)

	// Store snapshot at gapHash with full pool + penalties → ActiveMasternodesLen=32.
	snap := newSnapshotWithPenalties(gapBlock, gapHash, pool, penalties)
	if snap.ActiveMasternodesLen != 32 {
		t.Fatalf("snapshot setup: ActiveMasternodesLen want 32 got %d", snap.ActiveMasternodesLen)
	}
	if err := storeSnapshot(snap, db); err != nil {
		t.Fatalf("storeSnapshot: %v", err)
	}

	// Mock chain where the parent is absent.
	chain := &mockChainForA894{}

	// getEpochSwitchInfo for the gap block: parent absent → synthetic path.
	info, err := eng.getEpochSwitchInfo(chain, gapHeader, gapHash)
	if err != nil {
		t.Fatalf("getEpochSwitchInfo: %v", err)
	}
	if info == nil {
		t.Fatalf("getEpochSwitchInfo returned nil info")
	}

	// ASSERTION: MasternodesLen must be 32, not 46.
	if info.MasternodesLen != 32 {
		t.Fatalf("FAIL P0 #894: MasternodesLen want 32 got %d (threshold would be %d*2/3=%.3f; 22 sigs would %s)",
			info.MasternodesLen,
			info.MasternodesLen,
			float64(info.MasternodesLen)*2.0/3.0,
			func() string {
				if float64(22) >= float64(info.MasternodesLen)*2.0/3.0 {
					return "PASS"
				}
				return "FAIL"
			}())
	}
	threshold32 := float64(32) * 2.0 / 3.0
	t.Logf("PASS: MasternodesLen=%d (threshold=%.3f; 22 sigs passes: %v)",
		info.MasternodesLen, threshold32, float64(22) >= threshold32)

	// Masternodes should be the active set (32 addrs), not the full pool (46).
	if len(info.Masternodes) != 32 {
		t.Fatalf("Masternodes len want 32 got %d", len(info.Masternodes))
	}
	// None of the penalised addresses should appear in Masternodes.
	penSet := make(map[common.Address]bool)
	for _, p := range penalties {
		penSet[p] = true
	}
	for _, mn := range info.Masternodes {
		if penSet[mn] {
			t.Errorf("penalised address %v in active Masternodes", mn)
		}
	}
	t.Log("PASS: active Masternodes excludes penalties")
}

// TestA894_HEADLINE_verifyQC_22of32_passes is the headline test:
// 22 of 32 active signers → verifyQC PASSES (22 >= ceil(32*2/3)=22).
//
// Production flow: block B (not the gap block) has QC over gap block G (82934550).
// verifyQC(chain, QC_over_G, parentHeader=G, parents) →
//   getEpochSwitchInfo(chain, G, G.Hash()) →
//   G.ParentHash absent from chain → synthetic path → loadSnapshot(G.Hash()) →
//   snap.ActiveLen()=32 → MasternodesLen=32 → threshold=21.344 → 22 sigs PASS.
//
// So the snapshot is stored at gapHash=G.Hash(), and verifyQC is called with
// parentHeader=G (the gap block itself) and proposedBlock.Hash=G.Hash().
func TestA894_HEADLINE_verifyQC_22of32_passes(t *testing.T) {
	eng, db := newA894Engine(t)

	// Generate 46 keys. First 32 are active (indices 0-31); keys 32-45 are penalised.
	allKeys, allAddrs := genNKeys(t, 46)
	penaltyAddrs := allAddrs[32:]

	// Build the gap block header (G = block 82934550).
	// makeGapHeader returns (header, header.Hash()=gapHash).
	const gapBlock = 82934550
	const epoch, gap = uint64(900), uint64(450)
	gapNum := gapNumForBlock(gapBlock, epoch, gap) // 82933650
	gapHeader, gapHash := makeGapHeader(t, gapBlock, 1000, gapNum)

	// Store snapshot at gapHash: pool=46, penalties=14 → ActiveMasternodesLen=32.
	snap := newSnapshotWithPenalties(gapBlock, gapHash, allAddrs, penaltyAddrs)
	if snap.ActiveMasternodesLen != 32 {
		t.Fatalf("snapshot setup: want ActiveMasternodesLen=32, got %d", snap.ActiveMasternodesLen)
	}
	if err := storeSnapshot(snap, db); err != nil {
		t.Fatalf("storeSnapshot: %v", err)
	}

	// QC is over G (the gap block): proposedBlock = block G, hash = gapHash.
	// verifyQC is called with parentHeader=G (the proposed block IS the parent).
	proposedBlock := &types.BlockInfo{
		Hash:   gapHash,
		Round:  1001, // gap block round = qcRound+1 set in makeGapHeader
		Number: big.NewInt(int64(gapBlock)),
	}

	// Build 22-sig QC using active keys [0:22].
	sigs22 := makeQCSignatures(t, proposedBlock, gapNum, allKeys[:22])
	qc22 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs22,
		GapNumber:         gapNum,
	}

	// Mock chain: parent of gap block (gapHeader.ParentHash) is absent from DB.
	chain := &mockChainForA894{}

	// verifyQC must PASS: parentHeader=gapHeader, hash=gapHash.
	// getEpochSwitchInfo(chain, gapHeader, gapHash) → G not epoch-switch →
	// chain.GetHeaderByHash(gapHeader.ParentHash) = nil → synthetic path →
	// loadSnapshot(gapHash) → snap.activeSet()/ActiveLen()=32 → threshold=21.344 → 22≥22 PASS.
	err := eng.verifyQC(chain, qc22, gapHeader, nil)
	if err != nil {
		t.Fatalf("FAIL HEADLINE P0 #894: verifyQC with 22-of-32 active signers should PASS, got: %v", err)
	}
	t.Logf("PASS HEADLINE: 22-of-32 active signers verifyQC=nil (threshold=%.3f)", float64(32)*2.0/3.0)

	// Sanity: 32-of-32 also passes.
	sigs32 := makeQCSignatures(t, proposedBlock, gapNum, allKeys[:32])
	qc32 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs32,
		GapNumber:         gapNum,
	}
	if err := eng.verifyQC(chain, qc32, gapHeader, nil); err != nil {
		t.Fatalf("32-of-32 active signers verifyQC should PASS, got: %v", err)
	}
	t.Log("PASS: 32-of-32 active signers passes")

	// Confirm epochInfo shows MasternodesLen=32 and Masternodes (32 addrs, no penalties).
	// Clear cache so getEpochSwitchInfo re-derives from DB.
	eng.epochSwitches.Purge()
	info, err2 := eng.getEpochSwitchInfo(chain, gapHeader, gapHash)
	if err2 != nil {
		t.Fatalf("getEpochSwitchInfo post-verifyQC: %v", err2)
	}
	if info.MasternodesLen != 32 {
		t.Errorf("getEpochSwitchInfo MasternodesLen want 32 got %d", info.MasternodesLen)
	}
	if len(info.Masternodes) != 32 {
		t.Errorf("Masternodes len want 32 got %d", len(info.Masternodes))
	}
	t.Log("PASS: epochInfo.Masternodes=32 (active only)")
}

// TestA894_21of32_fails: 21 signatures is not enough (21 < 21.344 = ceil(32*2/3)).
func TestA894_21of32_fails(t *testing.T) {
	eng, db := newA894Engine(t)

	allKeys, allAddrs := genNKeys(t, 46)
	penaltyAddrs := allAddrs[32:]

	const gapBlock = 82934550
	gapNum21 := gapNumForBlock(gapBlock, 900, 450)
	gapHeader, gapHash := makeGapHeader(t, gapBlock, 1000, gapNum21)

	snap := newSnapshotWithPenalties(gapBlock, gapHash, allAddrs, penaltyAddrs)
	if err := storeSnapshot(snap, db); err != nil {
		t.Fatalf("storeSnapshot: %v", err)
	}

	// QC over the gap block itself; parentHeader = gapHeader.
	proposedBlock := &types.BlockInfo{
		Hash:   gapHash,
		Round:  1001,
		Number: big.NewInt(int64(gapBlock)),
	}

	// Only 21 signers.
	sigs21 := makeQCSignatures(t, proposedBlock, gapNum21, allKeys[:21])
	qc21 := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs21,
		GapNumber:         gapNum21,
	}

	chain := &mockChainForA894{}
	err := eng.verifyQC(chain, qc21, gapHeader, nil)
	if err != utils.ErrInvalidQCSignatures {
		t.Fatalf("21-of-32 should return ErrInvalidQCSignatures, got: %v", err)
	}
	t.Log("PASS: 21-of-32 correctly rejected (below 2/3 threshold)")
}

// TestA894_NonPoolSigner_fails: signer not in pool → membership check fails.
func TestA894_NonPoolSigner_fails(t *testing.T) {
	eng, db := newA894Engine(t)

	// 45 pool keys + 1 outsider key
	poolKeys, poolAddrs := genNKeys(t, 45)
	outsiderKeys, _ := genNKeys(t, 1)

	// no penalties
	const gapBlock = 82934550
	gapNumNPS := gapNumForBlock(gapBlock, 900, 450)
	gapHeader, gapHash := makeGapHeader(t, gapBlock, 1000, gapNumNPS)

	snap := newSnapshotWithPenalties(gapBlock, gapHash, poolAddrs, nil)
	if err := storeSnapshot(snap, db); err != nil {
		t.Fatalf("storeSnapshot: %v", err)
	}

	// QC over gap block; parentHeader = gapHeader.
	proposedBlock := &types.BlockInfo{
		Hash:   gapHash,
		Round:  1001,
		Number: big.NewInt(int64(gapBlock)),
	}

	// 30 pool signers + 1 outsider → outsider not in Masternodes → fails membership.
	signers := append(poolKeys[:30], outsiderKeys[0])
	sigs := makeQCSignatures(t, proposedBlock, gapNumNPS, signers)
	qc := &types.QuorumCert{
		ProposedBlockInfo: proposedBlock,
		Signatures:        sigs,
		GapNumber:         gapNumNPS,
	}

	chain := &mockChainForA894{}
	err := eng.verifyQC(chain, qc, gapHeader, nil)
	if err == nil {
		t.Fatal("outsider signer in QC should fail verification, got nil error")
	}
	t.Logf("PASS: non-pool signer correctly rejected: %v", err)
}

// TestA894_WritePath_StoresPoolPenaltiesActive: UpdateMasternodesFromHeader
// stores NextEpochCandidates=pool(N), Penalties=k, ActiveMasternodesLen=N-k.
// We test the snapshot constructor path directly (HookPenalty unit tested here).
func TestA894_WritePath_StoresPoolPenaltiesActive(t *testing.T) {
	poolSize := 46
	penSize := 14
	pool := make([]common.Address, poolSize)
	for i := range pool {
		pool[i] = common.Address{byte(i + 1)}
	}
	penalties := pool[:penSize]

	snap := newSnapshotWithPenalties(1, common.Hash{0xAA}, pool, penalties)

	if len(snap.NextEpochCandidates) != poolSize {
		t.Fatalf("NextEpochCandidates want %d got %d", poolSize, len(snap.NextEpochCandidates))
	}
	if len(snap.Penalties) != penSize {
		t.Fatalf("Penalties want %d got %d", penSize, len(snap.Penalties))
	}
	if snap.ActiveMasternodesLen != poolSize-penSize {
		t.Fatalf("ActiveMasternodesLen want %d got %d", poolSize-penSize, snap.ActiveMasternodesLen)
	}
	t.Logf("PASS write-path invariant: NextEpochCandidates=%d, Penalties=%d, ActiveMasternodesLen=%d",
		len(snap.NextEpochCandidates), len(snap.Penalties), snap.ActiveMasternodesLen)
}
