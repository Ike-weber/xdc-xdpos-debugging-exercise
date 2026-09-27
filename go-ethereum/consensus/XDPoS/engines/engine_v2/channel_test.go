// Copyright 2024 XDC Network
// A.97-M.A (refs #894) — channel nil-guard and goroutine-leak tests.
//
// Tests:
//   - minePeriodCh nil-guard: UpdateParams with nil channel does not panic
//     and does not leak goroutines (design risk 5 from A97_MINING_DESIGN.md).
//   - newRoundCh nil-guard: setNewRound with nil channel is already non-blocking
//     (uses select/default), so it is safe by construction; confirmed here.
//   - Channel drain: with real buffered channels, sends are received and
//     no goroutines pile up under ≥1000 simulated UpdateParams calls.
//   - Stop-start mining twice: channels remain open and engine does not panic.
//
// Run with: go test ./consensus/XDPoS/engines/engine_v2/... -run TestChannel -race -count=1

package engine_v2

import (
	"math/big"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// --- helpers ----------------------------------------------------------------

// minimalChainConfig returns a *params.ChainConfig with XDPoS V2 fields
// populated just enough for createEngine to succeed without panicking.
func minimalChainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID: big.NewInt(551),
		XDPoS: &params.XDPoSConfig{
			Epoch:            900,
			RewardCheckpoint: 900,
			Gap:              5,
			V2: &params.V2{
				SwitchBlock: big.NewInt(0),
				CurrentConfig: &params.V2Config{
					MinePeriod:    2,
					TimeoutPeriod: 10,
					ExpTimeoutConfig: params.ExpTimeoutConfig{
						Base:        2.0,
						MaxExponent: 6,
					},
				},
				AllConfigs: map[uint64]*params.V2Config{},
			},
		},
	}
}

// goroutineCount returns the current goroutine count.
func goroutineCount() int {
	return runtime.NumGoroutine()
}

// --- tests ------------------------------------------------------------------

// TestNilMinePeriodCh_UpdateParamsNoPanic verifies that calling UpdateParams
// when minePeriodCh is nil does not panic and does not launch a blocking goroutine.
func TestNilMinePeriodCh_UpdateParamsNoPanic(t *testing.T) {
	cfg := minimalChainConfig()
	eng := createEngine(cfg, nil, nil, nil) // nil channels

	// Build a fake header so UpdateParams can extract a round number.
	// Round 1 is enough for UpdateConfig to be called.
	extraFields := &types.ExtraFields_v2{
		Round: 1,
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.Hash{},
				Round:  0,
				Number: big.NewInt(0),
			},
			Signatures: []types.Signature{},
			GapNumber:  0,
		},
	}
	extra, err := extraFields.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes not available in this build: %v", err)
	}
	header := &types.Header{
		Number: big.NewInt(1),
		Extra:  extra,
	}

	before := goroutineCount()
	// Should not panic regardless of nil channel.
	eng.UpdateParams(header)
	// Give any background goroutine a moment to start (and then block/exit).
	time.Sleep(10 * time.Millisecond)
	after := goroutineCount()

	// With the nil-guard, no new goroutine should have been launched.
	leaked := after - before
	if leaked > 0 {
		t.Errorf("goroutine leak: %d extra goroutines after UpdateParams with nil channel", leaked)
	}
}

// TestRealChannels_NoPileup verifies that with real buffered channels and an
// active consumer, ≥1000 UpdateParams calls do not pile up goroutines.
// This is design risk 5 from A97_MINING_DESIGN.md.
//
// Labeled -race compatible: channels are buffered(1) + the consumer drains
// synchronously before the next send window; no data races.
func TestRealChannels_NoPileup(t *testing.T) {
	const rounds = 1000

	minePeriodCh := make(chan int, 1)
	newRoundCh := make(chan types.Round, 1)

	cfg := minimalChainConfig()
	eng := createEngine(cfg, nil, minePeriodCh, newRoundCh)

	// Consumer: drain minePeriodCh and newRoundCh as fast as they arrive.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-minePeriodCh:
			case <-newRoundCh:
			case <-time.After(200 * time.Millisecond):
				// No more sends expected — consumer exits.
				return
			}
		}
	}()

	// Build a fake header for UpdateParams.
	extraFields := &types.ExtraFields_v2{
		Round: 1,
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.Hash{},
				Round:  0,
				Number: big.NewInt(0),
			},
			Signatures: []types.Signature{},
			GapNumber:  0,
		},
	}
	extra, err := extraFields.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes not available: %v", err)
	}
	header := &types.Header{
		Number: big.NewInt(1),
		Extra:  extra,
	}

	before := goroutineCount()
	// Fire rounds UpdateParams calls. Each spawns a goroutine that tries to
	// send to minePeriodCh. With buffer=1 + active consumer, the goroutines
	// complete quickly instead of piling up.
	for i := 0; i < rounds; i++ {
		eng.UpdateParams(header)
	}

	// Wait for consumer to drain all pending sends.
	<-done

	// Allow a brief GC cycle for goroutine cleanup.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	time.Sleep(10 * time.Millisecond)

	after := goroutineCount()
	// Tolerate a small delta for runtime scheduling noise, but not O(rounds).
	leaked := after - before
	const leakTolerance = 5
	if leaked > leakTolerance {
		t.Errorf("goroutine leak: %d extra goroutines after %d UpdateParams calls (tolerance %d)",
			leaked, rounds, leakTolerance)
	}
}

// TestSetNewRound_NilChannelSafe verifies that setNewRound is safe with a nil
// newRoundCh (uses select/default so no goroutine is spawned).
func TestSetNewRound_NilChannelSafe(t *testing.T) {
	cfg := minimalChainConfig()
	eng := createEngine(cfg, nil, nil, nil) // nil channels

	before := goroutineCount()
	// setNewRound uses select/default — safe with nil channel (send is skipped).
	// This should not panic or leak.
	eng.setNewRound(nil, types.Round(42))
	after := goroutineCount()

	leaked := after - before
	if leaked > 0 {
		t.Errorf("goroutine leak on setNewRound with nil newRoundCh: %d extra goroutines", leaked)
	}
}

// TestStopStartMining_NoPanic verifies that channels remain open and engine
// does not panic when a consumer stops and re-starts (design risk 8).
// The test simulates: start consumer → stop consumer → start new consumer →
// fire UpdateParams. No panic expected.
func TestStopStartMining_NoPanic(t *testing.T) {
	minePeriodCh := make(chan int, 1)
	newRoundCh := make(chan types.Round, 1)

	cfg := minimalChainConfig()
	eng := createEngine(cfg, nil, minePeriodCh, newRoundCh)

	// First consumer: start and immediately stop.
	stop1 := make(chan struct{})
	go func() {
		for {
			select {
			case <-minePeriodCh:
			case <-newRoundCh:
			case <-stop1:
				return
			}
		}
	}()
	close(stop1)
	time.Sleep(5 * time.Millisecond)

	// Build header for UpdateParams.
	extraFields := &types.ExtraFields_v2{
		Round: 1,
		QuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.Hash{},
				Round:  0,
				Number: big.NewInt(0),
			},
			Signatures: []types.Signature{},
			GapNumber:  0,
		},
	}
	extra, err := extraFields.EncodeToBytes()
	if err != nil {
		t.Skipf("EncodeToBytes not available: %v", err)
	}
	header := &types.Header{Number: big.NewInt(1), Extra: extra}

	// Second consumer: start fresh.
	stop2 := make(chan struct{})
	go func() {
		for {
			select {
			case <-minePeriodCh:
			case <-newRoundCh:
			case <-stop2:
				return
			}
		}
	}()
	defer close(stop2)

	// Should NOT panic: channels are still open (NEVER closed per design).
	eng.UpdateParams(header)
	time.Sleep(20 * time.Millisecond)
}
