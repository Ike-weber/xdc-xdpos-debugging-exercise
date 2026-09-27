// Copyright 2024 XDC Network
// XDC P2P broadcast functions for consensus messages.
// Ports BroadcastVote/BroadcastTimeout/BroadcastSyncInfo from v2.6.8
// (XDPoSChain/eth/handler.go BroadcastVote/Timeout/SyncInfo +
// peer.go SendVote/Timeout/SyncInfo).
// A.97-M.A Task 3: BroadcastBlock — NewBlockMsg gossip for self-minted blocks.
//
// Wire format: send types.Vote/Timeout/SyncInfo directly via p.Peer.Send()
// (which calls p2p.Send → RLP-encodes the object).  XDPoSChain's receive
// handler decodes as rlp.RawValue (raw RLP bytes of the struct) so the
// wire must carry RLP(types.Vote), NOT RLP(VotePacket{Vote: []byte}).

package eth

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

// BroadcastVote propagates a V2 consensus vote to all connected peers.
// Sends types.Vote directly so wire carries RLP(types.Vote), matching
// XDPoSChain/eth/peer.go SendVote: p2p.Send(rw, VoteMsg, vote).
// Our handleVoteMsg receive path decodes as rlp.RawValue and then
// rlp.DecodeBytes back to types.Vote, which handles both directions.
func (h *handler) BroadcastVote(vote *types.Vote) {
	if vote == nil {
		return
	}
	h.peers.lock.RLock()
	peers := make([]*ethPeer, 0, len(h.peers.peers))
	for _, p := range h.peers.peers {
		peers = append(peers, p)
	}
	h.peers.lock.RUnlock()

	for _, p := range peers {
		if err := p.Peer.Send(eth.VoteMsg, vote); err != nil {
			log.Trace("[XDC-P2P] Send vote failed", "peer", p.ID(), "err", err)
			continue
		}
	}
	log.Debug("[XDC-P2P] Vote broadcast complete", "peers", len(peers), "round", vote.ProposedBlockInfo.Round)
}

// BroadcastTimeout propagates a V2 consensus timeout to all connected peers.
// Wire format: p2p.Send(TimeoutMsg, timeout) — matches XDPoSChain/eth/peer.go
// SendTimeout which calls p2p.Send(rw, TimeoutMsg, timeout) directly.
// Sending the struct directly ensures XDPoSChain peers decode with
// msg.Decode(&timeout) correctly (refs #903 wire-compat fix).
func (h *handler) BroadcastTimeout(timeout *types.Timeout) {
	if timeout == nil {
		return
	}
	h.peers.lock.RLock()
	peers := make([]*ethPeer, 0, len(h.peers.peers))
	for _, p := range h.peers.peers {
		peers = append(peers, p)
	}
	h.peers.lock.RUnlock()

	for _, p := range peers {
		if err := p.Peer.Send(eth.TimeoutMsg, timeout); err != nil {
			log.Trace("[XDC-P2P] Send timeout failed", "peer", p.ID(), "err", err)
			continue
		}
	}
	log.Debug("[XDC-P2P] Timeout broadcast complete", "peers", len(peers), "round", timeout.Round)
}

// BroadcastSyncInfo propagates V2 sync info to all connected peers.
// Wire format: p2p.Send(SyncInfoMsg, syncInfo) — matches XDPoSChain/eth/peer.go
// SendSyncInfo which calls p2p.Send(rw, SyncInfoMsg, syncInfo) directly.
func (h *handler) BroadcastSyncInfo(syncInfo *types.SyncInfo) {
	if syncInfo == nil {
		return
	}
	h.peers.lock.RLock()
	peers := make([]*ethPeer, 0, len(h.peers.peers))
	for _, p := range h.peers.peers {
		peers = append(peers, p)
	}
	h.peers.lock.RUnlock()

	for _, p := range peers {
		if err := p.Peer.Send(eth.SyncInfoMsg, syncInfo); err != nil {
			log.Trace("[XDC-P2P] Send syncInfo failed", "peer", p.ID(), "err", err)
			continue
		}
	}
	log.Debug("[XDC-P2P] SyncInfo broadcast complete", "peers", len(peers))
}

// BroadcastBlock propagates a newly self-minted block to all connected peers
// via eth.NewBlockMsg (0x07). This is the M4c gap from A.97-M.A Task 3.
//
// Mirrors XDPoSChain/eth/handler.go BroadcastBlock (propagate=true path):
// compute TD from block.Difficulty (PoA: no cumulative TD tracking; peers
// ignore TD on XDPoS V2 blocks; block.Difficulty() is safe for wire use),
// wrap in NewBlockPacket, and Send to all eth peers.
//
// Called by the PR-B xdcWorker after InsertChain succeeds so that peers see
// the new tip and can vote on it via their own BFT dispatch path.
// Not called by anything in PR-A — exported here so PR-B can call it.
func (h *handler) BroadcastBlock(block *types.Block) {
	if block == nil {
		return
	}
	h.peers.lock.RLock()
	peers := make([]*ethPeer, 0, len(h.peers.peers))
	for _, p := range h.peers.peers {
		peers = append(peers, p)
	}
	h.peers.lock.RUnlock()

	if len(peers) == 0 {
		return
	}

	// TD: use block.Difficulty() as the announced total difficulty.
	// XDPoS V2 blocks all have the same difficulty value set by engine.Prepare;
	// peers do not use TD for chain selection in V2 (QC-chain rule wins).
	// This matches XDPoSChain's approach of parent.TD + block.Difficulty.
	td := new(big.Int).Set(block.Difficulty())

	pkt := &eth.NewBlockPacket{
		Block: block,
		TD:    td,
	}
	var sent int
	for _, p := range peers {
		if err := p.Peer.Send(eth.NewBlockMsg, pkt); err != nil {
			log.Trace("[XDC-P2P] Send NewBlock failed", "peer", p.ID(), "err", err)
			continue
		}
		sent++
	}
	log.Debug("[XDC-P2P] Block broadcast complete",
		"number", block.NumberU64(), "hash", block.Hash(), "peers", sent)
}
