// Copyright 2024 XDC Network
// A.97-M.A (refs #894) — HandleProposedBlock delegation tests.
//
// Tests:
//   - HandleProposedBlock delegates to EngineV2.ProposedBlockHandler
//   - HandleProposedBlock returns nil when EngineV2 is nil (safe no-op)
//   - HandleProposedBlock returns nil when chain is not ChainReader
//   - HandleProposedBlock returns the engine's error when engine errors

package XDPoS

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	engine_v2 "github.com/ethereum/go-ethereum/consensus/XDPoS/engines/engine_v2"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// --- stub chain reader ---

// headerOnlyChainReader satisfies consensus.ChainReader with stub values.
type headerOnlyChainReader struct{}

func (headerOnlyChainReader) Config() *params.ChainConfig { return &params.ChainConfig{} }
func (headerOnlyChainReader) CurrentHeader() *types.Header {
	return &types.Header{Number: big.NewInt(0)}
}
func (headerOnlyChainReader) GetHeader(_ common.Hash, _ uint64) *types.Header { return nil }
func (headerOnlyChainReader) GetHeaderByNumber(_ uint64) *types.Header        { return nil }
func (headerOnlyChainReader) GetHeaderByHash(_ common.Hash) *types.Header     { return nil }
func (headerOnlyChainReader) GetBlock(_ common.Hash, _ uint64) *types.Block   { return nil }

// Compile-time check.
var _ consensus.ChainReader = headerOnlyChainReader{}

// headerOnlyHeaderReader satisfies consensus.ChainHeaderReader but NOT ChainReader.
// Used to test the "chain not ChainReader" guard.
type headerOnlyHeaderReader struct{}

func (headerOnlyHeaderReader) Config() *params.ChainConfig { return &params.ChainConfig{} }
func (headerOnlyHeaderReader) CurrentHeader() *types.Header {
	return &types.Header{Number: big.NewInt(0)}
}
func (headerOnlyHeaderReader) GetHeader(_ common.Hash, _ uint64) *types.Header { return nil }
func (headerOnlyHeaderReader) GetHeaderByNumber(_ uint64) *types.Header        { return nil }
func (headerOnlyHeaderReader) GetHeaderByHash(_ common.Hash) *types.Header     { return nil }

// Compile-time check.
var _ consensus.ChainHeaderReader = headerOnlyHeaderReader{}

// --- stub EngineV2Iface ---

// fakeEngineV2 is a minimal EngineV2Iface that records ProposedBlockHandler calls.
type fakeEngineV2 struct {
	calledWith *types.Header
	returnErr  error
}

// Ensure fakeEngineV2 satisfies EngineV2Iface at compile time.
var _ EngineV2Iface = (*fakeEngineV2)(nil)

func (f *fakeEngineV2) VerifyHeader(_ consensus.ChainReader, _ *types.Header, _ bool) error {
	return nil
}
func (f *fakeEngineV2) VerifyHeaderWithParents(_ consensus.ChainReader, _ *types.Header, _ []*types.Header, _ bool) error {
	return nil
}
func (f *fakeEngineV2) GetRoundNumber(_ *types.Header) (types.Round, error) { return 0, nil }
func (f *fakeEngineV2) UpdateMasternodesFromHeader(_ consensus.ChainReader, _ *types.Header, _ *state.StateDB) error {
	return nil
}
func (f *fakeEngineV2) UpdateMasternodes(_ consensus.ChainReader, _ *types.Header, _ []common.Address) error {
	return nil
}
func (f *fakeEngineV2) UpdateMasternodesWithPenalties(_ consensus.ChainReader, _ *types.Header, _, _ []common.Address) error {
	return nil
}
func (f *fakeEngineV2) Author(_ *types.Header) (common.Address, error) { return common.Address{}, nil }
func (f *fakeEngineV2) IsEpochSwitch(_ *types.Header) (bool, uint64, error) {
	return false, 0, nil
}
func (f *fakeEngineV2) GetMasternodesByHash(_ consensus.ChainReader, _ common.Hash) []common.Address {
	return nil
}
func (f *fakeEngineV2) GetPreviousPenaltyByHash(_ consensus.ChainReader, _ common.Hash, _ int) []common.Address {
	return nil
}
func (f *fakeEngineV2) GetMasternodesFromEpochSwitchHeader(_ consensus.ChainReader, _ *types.Header) []common.Address {
	return nil
}
func (f *fakeEngineV2) GetMasternodesV2(_ consensus.ChainReader, _ *types.Header) []common.Address {
	return nil
}
func (f *fakeEngineV2) Prepare(_ consensus.ChainReader, _ *types.Header) error { return nil }
func (f *fakeEngineV2) Finalize(_ consensus.ChainReader, _ *types.Header, _ *state.StateDB, _ *state.StateDB, _ []*types.Transaction, _ []*types.Header, _ []*types.Receipt) (*types.Block, error) {
	return nil, nil
}
func (f *fakeEngineV2) Seal(_ consensus.ChainReader, _ *types.Block, _ <-chan struct{}) (*types.Block, error) {
	return nil, nil
}
func (f *fakeEngineV2) CalcDifficulty(_ consensus.ChainReader, _ uint64, _ *types.Header) *big.Int {
	return big.NewInt(1)
}
func (f *fakeEngineV2) SignHash(_ *types.Header) common.Hash             { return common.Hash{} }
func (f *fakeEngineV2) Authorize(_ common.Address, _ engine_v2.SignerFn) {}
func (f *fakeEngineV2) SetHookReward(_ func(consensus.ChainReader, *state.StateDB, *state.StateDB, *types.Header) (map[string]interface{}, error)) {
}
func (f *fakeEngineV2) SetHookPenalty(_ func(consensus.ChainReader, *big.Int, common.Hash, []common.Address) ([]common.Address, error)) {
}
func (f *fakeEngineV2) ProposedBlockHandler(chain consensus.ChainReader, header *types.Header) error {
	f.calledWith = header
	return f.returnErr
}
func (f *fakeEngineV2) ReinitBFT(_ consensus.ChainReader, _ *types.Header) error { return nil }

// buildMinimalXDPoS creates an XDPoS wrapper with a fakeEngineV2 wired and a
// V2 switch block of 0, so any header with Number > 0 is treated as a V2 block
// (HandleProposedBlock's #951 IsV2Block guard only delegates for V2 blocks).
func buildMinimalXDPoS() (*XDPoS, *fakeEngineV2) {
	fake := &fakeEngineV2{}
	x := &XDPoS{}
	x.EngineV2 = fake
	x.config = &params.XDPoSConfig{V2: &params.V2{SwitchBlock: big.NewInt(0)}}
	return x, fake
}

// --- tests ---

// TestHandleProposedBlock_DelegatesToEngineV2 verifies that HandleProposedBlock
// calls EngineV2.ProposedBlockHandler with the correct header.
func TestHandleProposedBlock_DelegatesToEngineV2(t *testing.T) {
	x, fake := buildMinimalXDPoS()
	hdr := &types.Header{Number: big.NewInt(42)}
	chain := headerOnlyChainReader{}

	if err := x.HandleProposedBlock(chain, hdr); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if fake.calledWith != hdr {
		t.Errorf("expected ProposedBlockHandler to be called with header %p, got %p", hdr, fake.calledWith)
	}
}

// TestHandleProposedBlock_ReturnsEngineError verifies that the engine's error
// is propagated back to the caller.
func TestHandleProposedBlock_ReturnsEngineError(t *testing.T) {
	x, fake := buildMinimalXDPoS()
	sentinelErr := errors.New("engine error")
	fake.returnErr = sentinelErr
	hdr := &types.Header{Number: big.NewInt(43)}

	if err := x.HandleProposedBlock(headerOnlyChainReader{}, hdr); !errors.Is(err, sentinelErr) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestHandleProposedBlock_NilEngineV2_ReturnsNil verifies that when EngineV2
// is nil the method returns nil (graceful no-op, not a panic).
func TestHandleProposedBlock_NilEngineV2_ReturnsNil(t *testing.T) {
	x := &XDPoS{} // EngineV2 is nil
	hdr := &types.Header{Number: big.NewInt(0)}
	if err := x.HandleProposedBlock(headerOnlyChainReader{}, hdr); err != nil {
		t.Errorf("nil EngineV2: expected nil return, got %v", err)
	}
}

// TestHandleProposedBlock_V1Block_NotDelegated verifies the #951 V1→V2 leak guard:
// a V1 (pre-switch) header must NOT be forwarded to EngineV2.ProposedBlockHandler.
// With SwitchBlock=10, a header at number 5 is V1 and should be a silent no-op.
func TestHandleProposedBlock_V1Block_NotDelegated(t *testing.T) {
	fake := &fakeEngineV2{}
	x := &XDPoS{}
	x.EngineV2 = fake
	x.config = &params.XDPoSConfig{V2: &params.V2{SwitchBlock: big.NewInt(10)}}
	hdr := &types.Header{Number: big.NewInt(5)} // < SwitchBlock ⇒ V1

	if err := x.HandleProposedBlock(headerOnlyChainReader{}, hdr); err != nil {
		t.Errorf("V1 block: expected nil return, got %v", err)
	}
	if fake.calledWith != nil {
		t.Errorf("V1 block must NOT be delegated to EngineV2.ProposedBlockHandler, but it was (called with %v)", fake.calledWith)
	}
}

// TestHandleProposedBlock_NonChainReader_ReturnsNil verifies that when the
// chain argument does not satisfy consensus.ChainReader the method returns nil
// (does not panic or type-assert incorrectly).
func TestHandleProposedBlock_NonChainReader_ReturnsNil(t *testing.T) {
	x, fake := buildMinimalXDPoS()
	hdr := &types.Header{Number: big.NewInt(1)}
	// headerOnlyHeaderReader only satisfies ChainHeaderReader, not ChainReader.
	if err := x.HandleProposedBlock(headerOnlyHeaderReader{}, hdr); err != nil {
		t.Errorf("non-ChainReader chain: expected nil, got %v", err)
	}
	// Engine should NOT have been called.
	if fake.calledWith != nil {
		t.Error("expected ProposedBlockHandler not to be called when chain is not ChainReader")
	}
}
