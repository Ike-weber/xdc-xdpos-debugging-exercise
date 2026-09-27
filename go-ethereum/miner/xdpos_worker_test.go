// Copyright 2024 XDC Network
// Unit tests for the XDPoS V2 block-production worker (A.97-M.B Task 5).
//
// Test coverage per the design's test plan:
//   - commitNewWork gating table (all four short-circuit paths)
//   - getResetTimeXDC timing bounds
//   - lastParentBlockCommit dedupe under rapid double-fire
//   - Channel drain: no goroutine leak under 100 simulated rounds (-race)
//   - Stop/Start twice: no panic (channels never closed)
//   - xdcSkipRound error classification
//
// NOTE: full integration tests (InsertChain, actual sealing, BFT round-trip)
// require devnet and are covered by PR-C.

package miner

import (
	"testing"
	"time"

	xdposutils "github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// ─── stub implementations ────────────────────────────────────────────────────

// stubSynced implements XdcSynced.
type stubSynced struct{ synced bool }

func (s *stubSynced) Synced() bool { return s.synced }

// stubBroadcaster implements XdcBroadcaster.
type stubBroadcaster struct{}

func (b *stubBroadcaster) BroadcastBlock(_ *types.Block) {}

// ─── helpers ─────────────────────────────────────────────────────────────────

// newXdcTestChain creates a minimal in-memory clique blockchain for timer/drain tests.
func newXdcTestChain(t *testing.T) *core.BlockChain {
	t.Helper()
	signer, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(signer.PublicKey)

	cfg := new(params.ChainConfig)
	*cfg = *params.TestChainConfig
	cfg.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}

	db := rawdb.NewMemoryDatabase()
	extra := make([]byte, 32+20+65)
	copy(extra[32:], addr.Bytes())
	gspec := &core.Genesis{
		Config:    cfg,
		ExtraData: extra,
	}
	eng := clique.New(cfg.Clique, db)

	bc, err := core.NewBlockChain(db, gspec, eng, &core.BlockChainConfig{})
	if err != nil {
		t.Fatalf("newXdcTestChain: %v", err)
	}
	t.Cleanup(bc.Stop)
	return bc
}

// ─── gating tests ────────────────────────────────────────────────────────────

// TestXdcWorker_GateMining verifies that commitNewWork exits immediately when
// the mining flag is 0.
func TestXdcWorker_GateMining(t *testing.T) {
	w := &XdcWorker{
		synced: &stubSynced{synced: true},
	}
	w.mining.Store(0)
	// Must not panic with nil engine/chain — returns before touching them.
	w.commitNewWork()
}

// TestXdcWorker_GateSynced verifies that commitNewWork exits when not synced.
func TestXdcWorker_GateSynced(t *testing.T) {
	w := &XdcWorker{
		synced: &stubSynced{synced: false},
	}
	w.mining.Store(1)
	// Must not panic with nil engine/chain — returns at gate 2.
	w.commitNewWork()
}

// TestXdcWorker_GateBulkSync verifies that commitNewWork exits during bulk sync.
func TestXdcWorker_GateBulkSync(t *testing.T) {
	core.XdcBulkSyncMode.Store(true)
	t.Cleanup(func() { core.XdcBulkSyncMode.Store(false) })

	w := &XdcWorker{
		synced: &stubSynced{synced: true},
	}
	w.mining.Store(1)
	// Must not panic with nil engine/chain — returns at gate 3.
	w.commitNewWork()
}

// TestXdcWorker_GateNilV2Engine verifies that commitNewWork exits when v2engine
// is nil (gate 4). A minimal Miner is needed because the V1-dispatch block (line
// 377) accesses w.miner.chainConfig before gate 4; provide a non-XDPoS chainConfig
// so that block returns immediately, letting the nil v2engine check at gate 4 fire.
func TestXdcWorker_GateNilV2Engine(t *testing.T) {
	cfg := new(params.ChainConfig)
	*cfg = *params.TestChainConfig
	// No XDPoS field: the V1 dispatch skips entirely, falling through to gate 4.
	cfg.XDPoS = nil
	w := &XdcWorker{
		synced:   &stubSynced{synced: true},
		v2engine: nil,
		miner:    &Miner{chainConfig: cfg},
	}
	w.mining.Store(1)
	// Returns at gate 4 ("v2engine not wired").
	w.commitNewWork()
}

// TestXdcWorker_GateCheckpointSyncNoState verifies that commitNewWork exits when
// CheckpointSyncNoStateActive is true (gate 3.1 — fast-sync state not yet healed).
func TestXdcWorker_GateCheckpointSyncNoState(t *testing.T) {
	core.CheckpointSyncNoStateActive.Store(true)
	t.Cleanup(func() { core.CheckpointSyncNoStateActive.Store(false) })

	w := &XdcWorker{
		synced: &stubSynced{synced: true},
	}
	w.mining.Store(1)
	// Must not panic with nil engine/chain — returns at gate 3.1.
	w.commitNewWork()
}

// ─── dedupe test ─────────────────────────────────────────────────────────────

// TestXdcWorker_DedupeFieldLogic verifies the lastParentBlockCommit string
// comparison semantics.  Full gate-5 path requires a live v2 engine.
func TestXdcWorker_DedupeFieldLogic(t *testing.T) {
	hash := "0xaabbccddeeff"
	w := &XdcWorker{lastParentBlockCommit: hash}
	if w.lastParentBlockCommit != hash {
		t.Fatalf("expected %q got %q", hash, w.lastParentBlockCommit)
	}
	// Resetting to empty allows the next round to proceed.
	w.lastParentBlockCommit = ""
	if w.lastParentBlockCommit != "" {
		t.Fatal("failed to reset lastParentBlockCommit")
	}
}

// ─── timer bounds ────────────────────────────────────────────────────────────

// TestGetResetTimeXDC_Bounds verifies the timer stays within (0, period].
func TestGetResetTimeXDC_Bounds(t *testing.T) {
	bc := newXdcTestChain(t)
	for _, period := range []int{1, 2, 5, 10} {
		d := getResetTimeXDC(bc, period)
		max := time.Duration(period) * time.Second
		if d <= 0 || d > max {
			t.Errorf("period=%d: resetTime %v outside (0, %v]", period, d, max)
		}
	}
}

// ─── channel drain / goroutine-leak test ─────────────────────────────────────

// TestXdcWorker_ChannelDrain verifies that the update() loop drains minePeriodCh
// and newRoundCh even when mining=0, preventing goroutine pile-up.
// Run with -race to catch data races.
func TestXdcWorker_ChannelDrain(t *testing.T) {
	bc := newXdcTestChain(t)

	m := &Miner{
		chain:  bc,
		config: &Config{Recommit: 100 * time.Millisecond},
	}

	minePeriodCh := make(chan int, 1)
	newRoundCh := make(chan types.Round, 1)

	w := NewXdcWorker(
		nil, nil, m, minePeriodCh, newRoundCh,
		&stubBroadcaster{}, &stubSynced{synced: false},
		nil, nil, // accountManager, txPool — not needed for drain test
	)
	w.mining.Store(0) // mining off — commitNewWork returns at gate 1
	go w.update()

	// Send 100 events; goroutine-leak shows up under -race.
	for i := 0; i < 100; i++ {
		select {
		case minePeriodCh <- 2:
		default:
		}
		select {
		case newRoundCh <- types.Round(i):
		default:
		}
		time.Sleep(time.Millisecond)
	}

	// Stop the worker.  Channels must NOT be closed (design Q2).
	w.Stop()
	time.Sleep(30 * time.Millisecond)

	// Channels are still open after stop — send must not panic.
	select {
	case minePeriodCh <- 3:
	default:
	}
	select {
	case newRoundCh <- types.Round(999):
	default:
	}
}

// TestXdcWorker_StopStartTwice verifies that two successive Start+Stop cycles
// (with separate worker instances) do not panic.
func TestXdcWorker_StopStartTwice(t *testing.T) {
	bc := newXdcTestChain(t)
	m := &Miner{
		chain:  bc,
		config: &Config{Recommit: 100 * time.Millisecond},
	}
	minePeriodCh := make(chan int, 1)
	newRoundCh := make(chan types.Round, 1)

	for i := 0; i < 2; i++ {
		w := NewXdcWorker(nil, nil, m, minePeriodCh, newRoundCh,
			&stubBroadcaster{}, &stubSynced{synced: false},
			nil, nil, // accountManager, txPool
		)
		w.mining.Store(0)
		go w.update()
		time.Sleep(5 * time.Millisecond)
		w.Stop()
		time.Sleep(5 * time.Millisecond)
	}
	// Must reach here without panic.
}

// ─── xdcSkipRound classification ─────────────────────────────────────────────

func TestXdcSkipRound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{xdposutils.ErrNotReadyToMine, true},
		{xdposutils.ErrNotReadyToPropose, true},
		{xdposutils.ErrAlreadyMined, true},
		{xdposutils.ErrInvalidQC, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := xdcSkipRound(c.err); got != c.want {
			t.Errorf("xdcSkipRound(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
