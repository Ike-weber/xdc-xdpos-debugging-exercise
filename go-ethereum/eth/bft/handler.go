// Copyright 2024 XDC Network
// Patient (geth-1.17) port of XDPoSChain/eth/bft/bft_handler.go.
//
// The Bfter is the BFT message dispatcher: it accepts inbound vote/timeout/
// sync-info packets from the eth wire protocol, hands them to the V2 engine
// for verification + handling, and (separately) forwards engine-emitted
// outbound BFT messages to the broadcast layer.
//
// This file deliberately defines a NARROW EngineV2 interface local to the
// bft package, instead of importing consensus/XDPoS, to avoid an import
// cycle (consensus/XDPoS already imports core/types; eth imports
// consensus/XDPoS; eth/bft must not pull XDPoS back in via the engine).
//
// Initialization is two-step (mirrors v2.6.8): construct via New(), then
// call SetConsensusFns once the engine is wired so we can capture
// VoteHandler / TimeoutHandler / SyncInfoHandler closures.

package bft

import (
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// maxBlockDist is the maximum allowed backward distance from the chain head.
// Matches XDPoSChain/eth/bft/bft_handler.go:12.
const maxBlockDist = 7

// EngineV2 is the narrow surface of the V2 engine that the BFT dispatcher
// requires. Defined here (rather than imported from consensus/XDPoS) to
// keep the dependency one-way: eth -> eth/bft -> {core/types, consensus},
// not eth/bft -> consensus/XDPoS.
//
// Any concrete *engine_v2.XDPoS_v2 already satisfies this interface.
type EngineV2 interface {
	VerifyVoteMessage(chain consensus.ChainReader, vote *types.Vote) (bool, error)
	VoteHandler(chain consensus.ChainReader, vote *types.Vote) error

	VerifyTimeoutMessage(chain consensus.ChainReader, timeout *types.Timeout) (bool, error)
	TimeoutHandler(chain consensus.ChainReader, timeout *types.Timeout) error

	VerifySyncInfoMessage(chain consensus.ChainReader, syncInfo *types.SyncInfo) (bool, error)
	SyncInfoHandler(chain consensus.ChainReader, syncInfo *types.SyncInfo) error
}

// ChainHeightFn returns the current canonical chain head height.
type ChainHeightFn func() uint64

// Broadcast callback signatures, matching v2.6.8.
type (
	BroadcastVoteFn     func(*types.Vote)
	BroadcastTimeoutFn  func(*types.Timeout)
	BroadcastSyncInfoFn func(*types.SyncInfo)
)

// BroadcastFns bundles the three outbound broadcast callbacks the engine
// needs to reach peers via the eth handler.
type BroadcastFns struct {
	Vote     BroadcastVoteFn
	Timeout  BroadcastTimeoutFn
	SyncInfo BroadcastSyncInfoFn
}

// Bfter dispatches inbound BFT messages into the V2 engine and forwards
// engine-emitted outbound messages to the broadcast layer. Mirrors
// XDPoSChain/eth/bft/bft_handler.go:Bfter.
type Bfter struct {
	epoch uint64

	chain       consensus.ChainReader
	engine      EngineV2
	chainHeight ChainHeightFn

	broadcast   BroadcastFns
	broadcastCh chan interface{}
	quit        chan struct{}
}

// New creates a Bfter. Call SetConsensusFns afterwards to wire the engine
// reference (matches the two-step initialization in v2.6.8).
func New(broadcasts BroadcastFns, chain consensus.ChainReader, chainHeight ChainHeightFn) *Bfter {
	return &Bfter{
		broadcast:   broadcasts,
		chain:       chain,
		chainHeight: chainHeight,
		quit:        make(chan struct{}),
		broadcastCh: make(chan interface{}),
	}
}

// SetConsensusFns wires the engine reference. Must be called before the
// dispatcher accepts traffic. The engine's broadcast channel (if exposed)
// can be wired by the caller via SetBroadcastCh.
func (b *Bfter) SetConsensusFns(engine EngineV2) {
	b.engine = engine
}

// SetBroadcastCh wires the engine's outbound broadcast channel so the
// Bfter loop can forward engine-emitted messages to peers.
func (b *Bfter) SetBroadcastCh(ch chan interface{}) {
	if ch != nil {
		b.broadcastCh = ch
	}
}

// SetEpoch records the chain's epoch length, used in the timeout-distance
// guard (matches v2.6.8 Bfter.InitEpochNumber).
func (b *Bfter) SetEpoch(epoch uint64) {
	b.epoch = epoch
}

// Engine returns the wired V2 engine, or nil if not yet wired. Callers
// (e.g. eth/handler.go) use this to gate inbound dispatch.
func (b *Bfter) Engine() EngineV2 {
	return b.engine
}

// Vote handles an inbound vote message from the given peer. Mirrors
// XDPoSChain/eth/bft/bft_handler.go:Vote.
func (b *Bfter) Vote(peer string, vote *types.Vote) error {
	if b == nil || b.engine == nil || vote == nil || vote.ProposedBlockInfo == nil {
		return nil
	}
	log.Trace("Receive Vote", "hash", vote.Hash().Hex(),
		"voted block hash", vote.ProposedBlockInfo.Hash.Hex(),
		"number", vote.ProposedBlockInfo.Number,
		"round", vote.ProposedBlockInfo.Round)

	voteBlockNum := vote.ProposedBlockInfo.Number.Int64()
	if dist := voteBlockNum - int64(b.chainHeight()); dist < -maxBlockDist || dist > maxBlockDist {
		log.Debug("Discarded propagated vote, too far away",
			"peer", peer, "number", voteBlockNum,
			"hash", vote.ProposedBlockInfo.Hash, "distance", dist)
		return nil
	}

	verified, err := b.engine.VerifyVoteMessage(b.chain, vote)
	if err != nil {
		log.Error("Verify BFT Vote", "error", err)
		return err
	}
	if !verified {
		return nil
	}

	// FIX B (A.97-M.D): forward verified vote to the broadcast channel.
	// Use a goroutine-wrapped blocking send (mirrors engine_v2.broadcastToBftChannel
	// and XDPoSChain/eth/bft/bft_handler.go:97). The non-blocking select+default
	// silently dropped inbound votes when the loop consumer was momentarily busy,
	// causing "collected vote count=1" to never advance past 1/3 — QC never formed.
	go func() { b.broadcastCh <- vote }()
	if err := b.engine.VoteHandler(b.chain, vote); err != nil {
		log.Debug("handle BFT Vote", "error", err)
		return err
	}
	return nil
}

// Timeout handles an inbound timeout message from the given peer. Mirrors
// XDPoSChain/eth/bft/bft_handler.go:Timeout.
func (b *Bfter) Timeout(peer string, timeout *types.Timeout) error {
	if b == nil || b.engine == nil || timeout == nil {
		return nil
	}
	log.Debug("Receive Timeout", "timeout", timeout)

	gapNum := timeout.GapNumber
	if b.epoch > 0 {
		if dist := int64(gapNum) - int64(b.chainHeight()); dist < -int64(b.epoch)*3 || dist > int64(b.epoch)*3 {
			log.Debug("Discarded propagated timeout, too far away",
				"peer", peer, "gapNumber", gapNum, "distance", dist)
			return nil
		}
	}

	verified, err := b.engine.VerifyTimeoutMessage(b.chain, timeout)
	if err != nil {
		log.Error("Verify BFT Timeout", "timeoutRound", timeout.Round, "timeoutGapNum", gapNum, "error", err)
		return err
	}
	if !verified {
		return nil
	}

	// FIX B: goroutine-wrapped blocking send; see Vote() for reasoning.
	go func() { b.broadcastCh <- timeout }()
	if err := b.engine.TimeoutHandler(b.chain, timeout); err != nil {
		log.Debug("handle BFT Timeout", "error", err)
		return err
	}
	return nil
}

// SyncInfo handles an inbound sync-info message from the given peer.
// Mirrors XDPoSChain/eth/bft/bft_handler.go:SyncInfo.
func (b *Bfter) SyncInfo(peer string, syncInfo *types.SyncInfo) error {
	if b == nil || b.engine == nil || syncInfo == nil ||
		syncInfo.HighestQuorumCert == nil || syncInfo.HighestQuorumCert.ProposedBlockInfo == nil {
		return nil
	}
	log.Debug("Receive SyncInfo", "syncInfo", syncInfo)

	qcBlockNum := syncInfo.HighestQuorumCert.ProposedBlockInfo.Number.Int64()
	if dist := qcBlockNum - int64(b.chainHeight()); dist < -maxBlockDist || dist > maxBlockDist {
		log.Debug("Discarded propagated syncInfo, too far away",
			"peer", peer, "blockNum", qcBlockNum, "distance", dist)
		return nil
	}

	verified, err := b.engine.VerifySyncInfoMessage(b.chain, syncInfo)
	if err != nil {
		log.Error("Verify BFT SyncInfo", "error", err)
		return err
	}
	if !verified {
		return nil
	}

	// FIX B: goroutine-wrapped blocking send; see Vote() for reasoning.
	go func() { b.broadcastCh <- syncInfo }()
	if err := b.engine.SyncInfoHandler(b.chain, syncInfo); err != nil {
		log.Debug("handle BFT SyncInfo", "error", err)
		return err
	}
	return nil
}

// Start launches the engine-broadcast forwarding loop. Safe to skip if
// the engine never emits BFT messages (read-only / sync-only mode).
func (b *Bfter) Start() {
	go b.loop()
}

// Stop terminates the loop.
func (b *Bfter) Stop() {
	close(b.quit)
}

func (b *Bfter) loop() {
	log.Info("BFT Loop Start")
	for {
		select {
		case <-b.quit:
			log.Warn("BFT Loop Close")
			return
		case obj := <-b.broadcastCh:
			switch v := obj.(type) {
			case *types.Vote:
				go b.broadcast.Vote(v)
			case *types.Timeout:
				go b.broadcast.Timeout(v)
			case *types.SyncInfo:
				go b.broadcast.SyncInfo(v)
			default:
				log.Error("Unknown BFT message type received", "value", v)
			}
		}
	}
}
