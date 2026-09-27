// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// ethHandler implements the eth.Backend interface to handle the various network
// packets that are sent as replies or broadcasts.
type ethHandler handler

func (h *ethHandler) Chain() *core.BlockChain { return h.chain }
func (h *ethHandler) TxPool() eth.TxPool      { return h.txpool }
func (h *ethHandler) BlobPool() eth.BlobPool  { return h.blobpool }

// RunPeer is invoked when a peer joins on the `eth` protocol.
func (h *ethHandler) RunPeer(peer *eth.Peer, hand eth.Handler) error {
	return (*handler)(h).runEthPeer(peer, hand)
}

// PeerInfo retrieves all known `eth` information about a peer.
func (h *ethHandler) PeerInfo(id enode.ID) interface{} {
	if p := h.peers.peer(id.String()); p != nil {
		return p.info()
	}
	return nil
}

// AcceptTxs retrieves whether transaction processing is enabled on the node
// or if inbound transactions should simply be dropped.
func (h *ethHandler) AcceptTxs() bool {
	return h.synced.Load()
}

// Handle is invoked from a peer's message handler when it receives a new remote
// message that the handler couldn't consume and serve itself.
func (h *ethHandler) Handle(peer *eth.Peer, packet eth.Packet) error {
	// Consume any broadcasts and announces, forwarding the rest to the downloader
	switch packet := packet.(type) {
	case *eth.NewPooledTransactionHashesPacket72:
		hashes, err := h.txFetcher.Notify(peer.ID(), packet.Types, packet.Sizes, packet.Hashes)
		if err != nil {
			return err
		}
		if len(hashes) != 0 {
			return h.blobFetcher.Notify(peer.ID(), hashes, packet.Mask)
		}
		return nil

	case *eth.NewPooledTransactionHashesPacket71:
		_, err := h.txFetcher.Notify(peer.ID(), packet.Types, packet.Sizes, packet.Hashes)
		return err

	case *eth.TransactionsPacket:
		txs, err := packet.Items()
		if err != nil {
			return fmt.Errorf("Transactions: %v", err)
		}
		if err := handleTransactions(peer, txs, true); err != nil {
			return fmt.Errorf("Transactions: %v", err)
		}
		return h.txFetcher.Enqueue(peer.ID(), peer.Version(), txs, false)

	case *eth.PooledTransactionsPacket:
		txs, err := packet.List.Items()
		if err != nil {
			return fmt.Errorf("PooledTransactions: %v", err)
		}
		if err := handleTransactions(peer, txs, false); err != nil {
			return fmt.Errorf("PooledTransactions: %v", err)
		}
		return h.txFetcher.Enqueue(peer.ID(), peer.Version(), txs, true)

	case *eth.CellsResponse:
		outer, err := packet.Cells.Items()
		if err != nil {
			return fmt.Errorf("Cells: %v", err)
		}
		cells := make([][]kzg4844.Cell, len(outer))
		for i := range outer {
			if outer[i].Len() > params.BlobTxMaxBlobs*kzg4844.CellsPerBlob {
				return fmt.Errorf("Cells: cells per tx exceeded the possible maximum")
			}
			if cells[i], err = outer[i].Items(); err != nil {
				return fmt.Errorf("Cells: %v", err)
			}
		}
		return h.blobFetcher.Enqueue(peer.ID(), packet.Hashes, cells, packet.Mask)

	// XDC V2 BFT consensus messages. A.97-M.A Task 2: dispatch inbound BFT
	// messages to the Bfter when it is wired (h.bfter != nil). The Bfter
	// verifies the message, forwards it to the engine, and re-broadcasts it
	// to other peers via the engine's BroadcastCh. When the Bfter is not wired
	// (non-XDPoS-V2 chains, tests) we still return nil to keep the peer alive
	// instead of dropping as "unexpected eth packet type". Mirrors the decode
	// pattern of XDPoSChain/eth/handler.go:840-892 (refs #894, PR-A).
	case *eth.VotePacket:
		if h.bfter != nil && h.bfter.Engine() != nil {
			var vote types.Vote
			if err := rlp.DecodeBytes(packet.Vote, &vote); err != nil {
				peer.Log().Debug("XDPoS2: malformed Vote RLP, ignoring", "peer", peer.ID(), "err", err)
				return nil // malformed — don't penalise peer, just drop
			}
			if err := h.bfter.Vote(peer.ID(), &vote); err != nil {
				peer.Log().Debug("XDPoS2: Vote handler error", "peer", peer.ID(), "err", err)
			}
		} else {
			peer.Log().Trace("XDPoS2: received Vote (bfter not wired, ignored)", "size", len(packet.Vote))
		}
		return nil
	case *eth.TimeoutPacket:
		if h.bfter != nil && h.bfter.Engine() != nil {
			var timeout types.Timeout
			if err := rlp.DecodeBytes(packet.Timeout, &timeout); err != nil {
				peer.Log().Debug("XDPoS2: malformed Timeout RLP, ignoring", "peer", peer.ID(), "err", err)
				return nil
			}
			if err := h.bfter.Timeout(peer.ID(), &timeout); err != nil {
				peer.Log().Debug("XDPoS2: Timeout handler error", "peer", peer.ID(), "err", err)
			}
		} else {
			peer.Log().Trace("XDPoS2: received Timeout (bfter not wired, ignored)", "size", len(packet.Timeout))
		}
		return nil
	case *eth.SyncInfoPacket:
		if h.bfter != nil && h.bfter.Engine() != nil {
			var syncInfo types.SyncInfo
			if err := rlp.DecodeBytes(packet.SyncInfo, &syncInfo); err != nil {
				peer.Log().Debug("XDPoS2: malformed SyncInfo RLP, ignoring", "peer", peer.ID(), "err", err)
				return nil
			}
			if err := h.bfter.SyncInfo(peer.ID(), &syncInfo); err != nil {
				peer.Log().Debug("XDPoS2: SyncInfo handler error", "peer", peer.ID(), "err", err)
			}
		} else {
			peer.Log().Trace("XDPoS2: received SyncInfo (bfter not wired, ignored)", "size", len(packet.SyncInfo))
		}
		return nil

	// NodeData: state trie blobs delivered by an XDPOS2 peer in response to
	// GetNodeData (classical Fast Sync, Phase A.2 of #844). Forward to the
	// downloader's state-sync orchestration loop via DeliverNodeData.
	case *eth.NodeDataPacket:
		data := [][]byte(*packet)
		return h.downloader.DeliverNodeData(peer.ID(), data)

	// XDC pre-merge block broadcasts. Upstream geth post-merge removed these
	// (the consensus client drives chain extension via the engine API). XDC
	// keeps the legacy gossip path because canonical XDPoSChain v2.x peers
	// only broadcast new tip blocks via NewBlock — without this Handle case
	// the node falls behind tip at ~0.5 blocks/sec on Apothem.
	case *eth.NewBlockHashesPacket:
		// Announcement only — fetch handled lazily by the request path
		// (xdcSyncer's next round). At tip these aren't urgent because
		// xdcForceSyncCycle=500ms picks them up quickly via header polling.
		return nil

	case *eth.NewBlockPacket:
		// Direct tip-block insertion: peer just produced this block and
		// gossiped it. If its parent is our current head, append directly.
		// Otherwise let xdcSyncer's request path catch up later.
		block := packet.Block
		if block == nil {
			return nil
		}
		cur := h.chain.CurrentBlock()
		if cur == nil {
			return nil
		}
		// Only insert if this directly extends our head, or is a same-height
		// SIBLING of our head (V2 timeout re-mint). Multi-block gaps and other
		// reorgs are deferred to xdcSyncer + V2 BFT consensus.
		if block.ParentHash() != cur.Hash() {
			// Sibling re-proposal (fix #1329, net5151 epoch-switch stall):
			// when a proposed block fails to gather a QC (e.g. a quorum-critical
			// voter missed it), every V2 leader re-mints a fresh candidate at the
			// SAME height on the highestQC parent each round. Those candidates
			// arrive here with parent == our head's parent. Dropping them as
			// "not direct extension" means this node never votes again at that
			// height — and when this node's vote is required for quorum (observed:
			// set 5→6 at block 8099, one masternode down, threshold 5-of-6), the
			// whole network deadlocks in a permanent TC loop. The old deferral
			// assumption is false: xdcSyncer only polls head+1 and never fetches
			// same-height siblings, so no other path delivers them to the BFT
			// vote handler. Accept the sibling: InsertChain fully verifies it as
			// a side-chain/reorg import, then HandleProposedBlock applies the V2
			// voting rules (round monotonicity, allowedToSend) before any vote.
			isSibling := cur.Number.Uint64() > 0 &&
				block.NumberU64() == cur.Number.Uint64() &&
				block.ParentHash() == cur.ParentHash
			if !isSibling {
				peer.Log().Trace("XDC NewBlock: not direct extension, deferring",
					"block", block.NumberU64(), "parent", block.ParentHash(),
					"head", cur.Number.Uint64(), "headHash", cur.Hash())
				return nil
			}
			peer.Log().Debug("XDC NewBlock: same-height sibling re-proposal, verifying for BFT vote",
				"block", block.NumberU64(), "hash", block.Hash(),
				"head", cur.Number.Uint64(), "headHash", cur.Hash())
		}
		engine, isXDPoS := h.chain.Engine().(*XDPoS.XDPoS)
		// Stage B2 (#951 Defect 1c): V1 M2 co-sign. If this node is the assigned M2
		// for a freshly-minted V1 block with empty header.Validator, fill it BEFORE
		// import and re-gossip the completed block. header.Validator is in
		// Header.Hash(), so co-signing before InsertChain makes import + the BFT vote
		// operate on the canonical co-signed hash. No-op on non-M2 / non-miner / V2 /
		// first-epoch / already-co-signed blocks. Mirrors XDPoSChain appendM2HeaderHook.
		didCoSign := false
		if isXDPoS {
			if cosigned, did, err := engine.CoSignM2Header(h.chain, block.Header()); err != nil {
				peer.Log().Debug("XDC NewBlock: M2 co-sign failed",
					"block", block.NumberU64(), "err", err)
			} else if did {
				block = block.WithSeal(cosigned)
				didCoSign = true
				peer.Log().Debug("XDC NewBlock: M2 co-signed",
					"block", block.NumberU64(), "hash", block.Hash())
			}
		}
		if _, err := h.chain.InsertChain(types.Blocks{block}); err != nil {
			peer.Log().Debug("XDC NewBlock: InsertChain failed",
				"block", block.NumberU64(), "hash", block.Hash(), "err", err)
			return nil // don't drop peer for transient insert failures
		}
		peer.Log().Debug("XDC NewBlock: tip extended",
			"block", block.NumberU64(), "hash", block.Hash())
		// Re-broadcast ONLY the block we co-signed so peers holding the empty-Validator
		// variant get the canonical completed block (mirrors XDPoSChain fetcher post-insert
		// broadcast). Exactly one node (the assigned M2) emits this — no gossip amplification.
		if didCoSign {
			(*handler)(h).BroadcastBlock(block)
		}
		// Trigger BFT vote for this block (mirrors XDPoSChain eth/handler.go:199-208).
		// Without this, non-proposer nodes never vote for peer-minted blocks and
		// QC never forms.  The call is async to avoid blocking the P2P handler goroutine.
		if isXDPoS {
			go func(header *types.Header) {
				if err := engine.HandleProposedBlock(h.chain, header); err != nil {
					log.Debug("XDC NewBlock: HandleProposedBlock error",
						"block", header.Number, "hash", header.Hash(), "err", err)
				}
			}(block.Header())
		}
		return nil

	// Note: NewBlockHashes (0x01) and NewBlock (0x07) — XDC pre-merge block
	// broadcasts — are handled at the protocol layer in handler_xdc.go
	// (handleNewBlockhashesXDC / handleNewBlockXDC) and never reach this
	// dispatch. No case needed here.

	default:
		return fmt.Errorf("unexpected eth packet type: %T", packet)
	}
}

// handleTransactions marks all given transactions as known to the peer
// and performs basic validations.
func handleTransactions(peer *eth.Peer, list []*types.Transaction, directBroadcast bool) error {
	seen := make(map[common.Hash]struct{}, len(list))
	for _, tx := range list {
		if tx.Type() == types.BlobTxType {
			if directBroadcast {
				return errors.New("disallowed broadcast blob transaction")
			} else {
				// If we receive any blob transactions missing sidecars, or with
				// sidecars that don't correspond to the versioned hashes reported
				// in the header, disconnect from the sending peer.
				if tx.BlobTxSidecar() == nil {
					return errors.New("received sidecar-less blob transaction")
				}
				if err := tx.BlobTxSidecar().ValidateBlobCommitmentHashes(tx.BlobHashes()); err != nil {
					return err
				}
			}
		}

		// Check for duplicates.
		hash := tx.Hash()
		if _, exists := seen[hash]; exists {
			return fmt.Errorf("multiple copies of the same hash %v", hash)
		}
		seen[hash] = struct{}{}

		// Mark as known.
		peer.MarkTransaction(hash)
	}
	return nil
}
