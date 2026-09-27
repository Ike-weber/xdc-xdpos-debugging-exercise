// Copyright 2024 XDC Network
// A.97-M.A (refs #894) — BFT dispatch decode/dispatch round-trip tests.
//
// Tests:
//   - Vote/Timeout/SyncInfo RLP round-trip (encode → decode → dispatch)
//   - Malformed RLP returns error, does not panic
//   - Nil-bfter gate: nil Bfter dispatch returns nil (no panic)
//   - Nil-engine gate: Bfter.Engine() == nil dispatch is a no-op

package bft

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// --- helpers ---

func newBlockInfo(round types.Round, num uint64) *types.BlockInfo {
	return &types.BlockInfo{
		Round:  round,
		Number: big.NewInt(int64(num)),
		Hash:   common.HexToHash("0xdeadbeef"),
	}
}

func newVote(round types.Round) *types.Vote {
	return &types.Vote{
		ProposedBlockInfo: newBlockInfo(round, 100),
		Signature:         types.Signature(make([]byte, 65)),
		GapNumber:         50,
	}
}

func newTimeout(round types.Round) *types.Timeout {
	return &types.Timeout{
		Round:     round,
		Signature: types.Signature(make([]byte, 65)),
		GapNumber: 50,
	}
}

func newSyncInfo(round types.Round) *types.SyncInfo {
	return &types.SyncInfo{
		HighestQuorumCert: &types.QuorumCert{
			ProposedBlockInfo: newBlockInfo(round, 100),
			Signatures:        []types.Signature{make([]byte, 65)},
			GapNumber:         50,
		},
		HighestTimeoutCert: &types.TimeoutCert{
			Round:      round,
			Signatures: []types.Signature{make([]byte, 65)},
			GapNumber:  50,
		},
	}
}

// fakeEngine records calls without real verification. Satisfies EngineV2.
type fakeEngine struct {
	votesCalled     int
	timeoutsCalled  int
	syncInfosCalled int
	verifyErr       error
}

func (f *fakeEngine) VerifyVoteMessage(_ consensus.ChainReader, _ *types.Vote) (bool, error) {
	return f.verifyErr == nil, f.verifyErr
}
func (f *fakeEngine) VoteHandler(_ consensus.ChainReader, _ *types.Vote) error {
	f.votesCalled++
	return nil
}
func (f *fakeEngine) VerifyTimeoutMessage(_ consensus.ChainReader, _ *types.Timeout) (bool, error) {
	return f.verifyErr == nil, f.verifyErr
}
func (f *fakeEngine) TimeoutHandler(_ consensus.ChainReader, _ *types.Timeout) error {
	f.timeoutsCalled++
	return nil
}
func (f *fakeEngine) VerifySyncInfoMessage(_ consensus.ChainReader, _ *types.SyncInfo) (bool, error) {
	return f.verifyErr == nil, f.verifyErr
}
func (f *fakeEngine) SyncInfoHandler(_ consensus.ChainReader, _ *types.SyncInfo) error {
	f.syncInfosCalled++
	return nil
}

// fakeChain satisfies consensus.ChainReader minimally for Bfter.
type fakeChain struct{}

func (fakeChain) Config() *params.ChainConfig                     { return &params.ChainConfig{} }
func (fakeChain) CurrentHeader() *types.Header                    { return &types.Header{Number: big.NewInt(100)} }
func (fakeChain) GetHeader(_ common.Hash, _ uint64) *types.Header { return nil }
func (fakeChain) GetHeaderByNumber(_ uint64) *types.Header        { return nil }
func (fakeChain) GetHeaderByHash(_ common.Hash) *types.Header     { return nil }
func (fakeChain) GetBlock(_ common.Hash, _ uint64) *types.Block   { return nil }

// Compile-time check that fakeChain satisfies consensus.ChainReader.
var _ consensus.ChainReader = fakeChain{}

// newTestBfter builds a Bfter with fakeEngine and a no-op chainHeight.
func newTestBfter() (*Bfter, *fakeEngine) {
	eng := &fakeEngine{}
	chainHeight := func() uint64 { return 100 }
	b := New(BroadcastFns{
		Vote:     func(*types.Vote) {},
		Timeout:  func(*types.Timeout) {},
		SyncInfo: func(*types.SyncInfo) {},
	}, fakeChain{}, chainHeight)
	b.SetConsensusFns(eng)
	b.SetEpoch(900)
	return b, eng
}

// --- RLP round-trip tests ---

// TestVote_RLPRoundTrip verifies encode→decode→dispatch for a Vote.
func TestVote_RLPRoundTrip(t *testing.T) {
	b, eng := newTestBfter()
	v := newVote(5)

	raw, err := rlp.EncodeToBytes(v)
	if err != nil {
		t.Fatalf("encode vote: %v", err)
	}
	var decoded types.Vote
	if err := rlp.DecodeBytes(raw, &decoded); err != nil {
		t.Fatalf("decode vote: %v", err)
	}
	if err := b.Vote("peer1", &decoded); err != nil {
		t.Errorf("Vote dispatch error: %v", err)
	}
	if eng.votesCalled == 0 {
		t.Error("expected VoteHandler to be called, but it was not")
	}
}

// TestTimeout_RLPRoundTrip verifies encode→decode→dispatch for a Timeout.
func TestTimeout_RLPRoundTrip(t *testing.T) {
	b, eng := newTestBfter()
	to := newTimeout(6)

	raw, err := rlp.EncodeToBytes(to)
	if err != nil {
		t.Fatalf("encode timeout: %v", err)
	}
	var decoded types.Timeout
	if err := rlp.DecodeBytes(raw, &decoded); err != nil {
		t.Fatalf("decode timeout: %v", err)
	}
	if err := b.Timeout("peer1", &decoded); err != nil {
		t.Errorf("Timeout dispatch error: %v", err)
	}
	if eng.timeoutsCalled == 0 {
		t.Error("expected TimeoutHandler to be called, but it was not")
	}
}

// TestSyncInfo_RLPRoundTrip verifies encode→decode→dispatch for a SyncInfo.
func TestSyncInfo_RLPRoundTrip(t *testing.T) {
	b, eng := newTestBfter()
	si := newSyncInfo(7)

	raw, err := rlp.EncodeToBytes(si)
	if err != nil {
		t.Fatalf("encode syncInfo: %v", err)
	}
	var decoded types.SyncInfo
	if err := rlp.DecodeBytes(raw, &decoded); err != nil {
		t.Fatalf("decode syncInfo: %v", err)
	}
	if err := b.SyncInfo("peer1", &decoded); err != nil {
		t.Errorf("SyncInfo dispatch error: %v", err)
	}
	if eng.syncInfosCalled == 0 {
		t.Error("expected SyncInfoHandler to be called, but it was not")
	}
}

// TestMalformedRLP_Vote verifies malformed RLP does not panic and rlp.DecodeBytes returns an error.
func TestMalformedRLP_Vote(t *testing.T) {
	// Malformed bytes — not valid RLP for types.Vote.
	garbage := []byte{0xff, 0xfe, 0x00, 0x01, 0x02}
	var v types.Vote
	if err := rlp.DecodeBytes(garbage, &v); err == nil {
		t.Error("expected error decoding malformed vote RLP, got nil")
	}
	// If it does not panic, the test passes by reaching here.
}

// TestMalformedRLP_Timeout verifies malformed RLP for Timeout.
func TestMalformedRLP_Timeout(t *testing.T) {
	garbage := []byte{0xcc, 0x00}
	var to types.Timeout
	if err := rlp.DecodeBytes(garbage, &to); err == nil {
		t.Error("expected error decoding malformed timeout RLP, got nil")
	}
}

// TestMalformedRLP_SyncInfo verifies malformed RLP for SyncInfo.
func TestMalformedRLP_SyncInfo(t *testing.T) {
	garbage := []byte{0xaa, 0xbb, 0xcc}
	var si types.SyncInfo
	if err := rlp.DecodeBytes(garbage, &si); err == nil {
		t.Error("expected error decoding malformed syncInfo RLP, got nil")
	}
}

// TestNilBfter_NoDispatch verifies that a nil *Bfter dispatch does not panic and returns nil.
func TestNilBfter_NoDispatch(t *testing.T) {
	var b *Bfter
	v := newVote(1)
	// Methods on nil Bfter have explicit nil guards in handler.go.
	if err := b.Vote("peer", v); err != nil {
		t.Errorf("nil Bfter Vote should return nil, got %v", err)
	}
	to := newTimeout(1)
	if err := b.Timeout("peer", to); err != nil {
		t.Errorf("nil Bfter Timeout should return nil, got %v", err)
	}
	si := newSyncInfo(1)
	if err := b.SyncInfo("peer", si); err != nil {
		t.Errorf("nil Bfter SyncInfo should return nil, got %v", err)
	}
}

// TestNilEngine_NoDispatch verifies that a Bfter with no engine wired returns nil (no-op).
func TestNilEngine_NoDispatch(t *testing.T) {
	chainHeight := func() uint64 { return 100 }
	b := New(BroadcastFns{
		Vote:     func(*types.Vote) {},
		Timeout:  func(*types.Timeout) {},
		SyncInfo: func(*types.SyncInfo) {},
	}, fakeChain{}, chainHeight)
	// Engine NOT set — b.engine is nil.
	if b.Engine() != nil {
		t.Fatal("expected nil engine")
	}
	v := newVote(2)
	if err := b.Vote("peer", v); err != nil {
		t.Errorf("nil-engine Vote should return nil, got %v", err)
	}
}

// TestTableDriven_BFT exercises all three message types in a table-driven manner.
func TestTableDriven_BFT(t *testing.T) {
	type testCase struct {
		name    string
		run     func(b *Bfter, eng *fakeEngine) error
		check   func(eng *fakeEngine) bool
		wantErr bool
	}
	cases := []testCase{
		{
			name: "vote_dispatch",
			run: func(b *Bfter, _ *fakeEngine) error {
				return b.Vote("peer1", newVote(10))
			},
			check: func(eng *fakeEngine) bool { return eng.votesCalled == 1 },
		},
		{
			name: "timeout_dispatch",
			run: func(b *Bfter, _ *fakeEngine) error {
				return b.Timeout("peer1", newTimeout(11))
			},
			check: func(eng *fakeEngine) bool { return eng.timeoutsCalled == 1 },
		},
		{
			name: "syncinfo_dispatch",
			run: func(b *Bfter, _ *fakeEngine) error {
				return b.SyncInfo("peer1", newSyncInfo(12))
			},
			check: func(eng *fakeEngine) bool { return eng.syncInfosCalled == 1 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, eng := newTestBfter()
			err := tc.run(b, eng)
			if (err != nil) != tc.wantErr {
				t.Errorf("wantErr=%v got %v", tc.wantErr, err)
			}
			if !tc.check(eng) {
				t.Errorf("expected handler to be called")
			}
		})
	}
}
