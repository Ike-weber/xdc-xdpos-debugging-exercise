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
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// softResponseLimit is the target maximum size of replies to data retrievals.
	softResponseLimit = 2 * 1024 * 1024

	// maxPacketSize is the devp2p message size limit commonly enforced by clients.
	// Any packet exceeding this limit must be rejected.
	maxPacketSize = 10 * 1024 * 1024

	// maxHeadersServe is the maximum number of block headers to serve. This number
	// is there to limit the number of disk lookups.
	maxHeadersServe = 1024

	// maxBodiesServe is the maximum number of block bodies to serve. This number
	// is mostly there to limit the number of disk lookups. With 24KB block sizes
	// nowadays, the practical limit will always be softResponseLimit.
	maxBodiesServe = 1024

	// maxReceiptsServe is the maximum number of block receipts to serve. This
	// number is mostly there to limit the number of disk lookups. With block
	// containing 200+ transactions nowadays, the practical limit will always
	// be softResponseLimit.
	maxReceiptsServe = 1024

	// maxNodeDataServe is the maximum number of state trie nodes returned in a
	// single GetNodeData query response (XDPOS2). Restored from upstream v1.10.13.
	maxNodeDataServe = 1024
	// maxBALsServe is the maximum number of block access lists to serve.
	maxBALsServe = 1024
)

// Handler is a callback to invoke from an outside runner after the boilerplate
// exchanges have passed.
type Handler func(peer *Peer) error

// Backend defines the data retrieval methods to serve remote requests and the
// callback methods to invoke on remote deliveries.
type Backend interface {
	// Chain retrieves the blockchain object to serve data.
	Chain() *core.BlockChain

	// TxPool retrieves the transaction pool object to serve data.
	TxPool() TxPool

	// BlobPool retrieves the blob pool object to serve cell requests.
	BlobPool() BlobPool

	// AcceptTxs retrieves whether transaction processing is enabled on the node
	// or if inbound transactions should simply be dropped.
	AcceptTxs() bool

	// RunPeer is invoked when a peer joins on the `eth` protocol. The handler
	// should do any peer maintenance work, handshakes and validations. If all
	// is passed, control should be given back to the `handler` to process the
	// inbound messages going forward.
	RunPeer(peer *Peer, handler Handler) error

	// PeerInfo retrieves all known `eth` information about a peer.
	PeerInfo(id enode.ID) interface{}

	// Handle is a callback to be invoked when a data packet is received from
	// the remote peer. Only packets not consumed by the protocol handler will
	// be forwarded to the backend.
	Handle(peer *Peer, packet Packet) error
}

// BlobPool defines the methods needed by the protocol handler to serve cell requests.
type BlobPool interface {
	// GetBlobHashes returns the blob versioned hashes for a given transaction hash.
	GetBlobHashes(hash common.Hash) []common.Hash
	// GetBlobCells retrieves cells and proofs for given versioned blob hashes filtered by the custody bitmap.
	GetBlobCells(vhashes []common.Hash, mask types.CustodyBitmap) ([][]*kzg4844.Cell, [][]*kzg4844.Proof, error)
	// GetCustody returns the custody bitmap for a given transaction hash.
	GetCustody(hash common.Hash) *types.CustodyBitmap
	// Has returns whether the blob pool contains a transaction with the given hash.
	Has(hash common.Hash) bool
}

// TxPool defines the methods needed by the protocol handler to serve transactions.
type TxPool interface {
	// Get retrieves the transaction from the local txpool with the given hash.
	Get(hash common.Hash) *types.Transaction

	// GetRLP retrieves the RLP-encoded transaction from the local txpool with
	// the given hash.
	GetRLP(hash common.Hash, version uint) []byte

	// GetMetadata returns the transaction type and transaction size with the
	// given transaction hash.
	GetMetadata(hash common.Hash) *txpool.TxMetadata
}

// MakeProtocols constructs the P2P protocol definitions for `eth`.
func MakeProtocols(backend Backend, network uint64, disc enode.Iterator) []p2p.Protocol {
	protocols := make([]p2p.Protocol, 0, len(ProtocolVersions))
	for _, version := range ProtocolVersions {
		protocols = append(protocols, p2p.Protocol{
			Name:    ProtocolName,
			Version: version,
			Length:  protocolLengths[version],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				peer := NewPeer(version, p, rw, backend.TxPool(), backend.BlobPool(), backend.Chain().Config())
				defer peer.Close()

				return backend.RunPeer(peer, func(peer *Peer) error {
					return Handle(backend, peer)
				})
			},
			NodeInfo: func() interface{} {
				return nodeInfo(backend.Chain(), network)
			},
			PeerInfo: func(id enode.ID) interface{} {
				return backend.PeerInfo(id)
			},
			DialCandidates: disc,
			Attributes:     []enr.Entry{currentENREntry(backend.Chain())},
		})
	}
	return protocols
}

// NodeInfo represents a short summary of the `eth` sub-protocol metadata
// known about the host peer.
type NodeInfo struct {
	Network uint64              `json:"network"` // Ethereum network ID (1=Mainnet, Holesky=17000)
	Genesis common.Hash         `json:"genesis"` // SHA3 hash of the host's genesis block
	Config  *params.ChainConfig `json:"config"`  // Chain configuration for the fork rules
	Head    common.Hash         `json:"head"`    // Hex hash of the host's best owned block
}

// nodeInfo retrieves some `eth` protocol metadata about the running host node.
func nodeInfo(chain *core.BlockChain, network uint64) *NodeInfo {
	head := chain.CurrentBlock()
	hash := head.Hash()

	return &NodeInfo{
		Network: network,
		Genesis: chain.Genesis().Hash(),
		Config:  chain.Config(),
		Head:    hash,
	}
}

// Handle is invoked whenever an `eth` connection is made that successfully passes
// the protocol handshake. This method will keep processing messages until the
// connection is torn down.
func Handle(backend Backend, peer *Peer) error {
	for {
		if err := handleMessage(backend, peer); err != nil {
			peer.Log().Debug("Message handling failed in `eth`", "err", err)
			return err
		}
	}
}

type msgHandler func(backend Backend, msg Decoder, peer *Peer) error
type Decoder interface {
	Decode(val interface{}) error
}

var eth69 = map[uint64]msgHandler{
	TransactionsMsg:               handleTransactions,
	NewPooledTransactionHashesMsg: handleNewPooledTransactionHashes,
	GetBlockHeadersMsg:            handleGetBlockHeaders,
	BlockHeadersMsg:               handleBlockHeaders,
	GetBlockBodiesMsg:             handleGetBlockBodies,
	BlockBodiesMsg:                handleBlockBodies,
	GetReceiptsMsg:                handleGetReceipts69,
	ReceiptsMsg:                   handleReceipts69,
	GetPooledTransactionsMsg:      handleGetPooledTransactions,
	PooledTransactionsMsg:         handlePooledTransactions,
	BlockRangeUpdateMsg:           handleBlockRangeUpdate,
}

var eth70 = map[uint64]msgHandler{
	TransactionsMsg:               handleTransactions,
	NewPooledTransactionHashesMsg: handleNewPooledTransactionHashes,
	GetBlockHeadersMsg:            handleGetBlockHeaders,
	BlockHeadersMsg:               handleBlockHeaders,
	GetBlockBodiesMsg:             handleGetBlockBodies,
	BlockBodiesMsg:                handleBlockBodies,
	GetReceiptsMsg:                handleGetReceipts70,
	ReceiptsMsg:                   handleReceipts70,
	GetPooledTransactionsMsg:      handleGetPooledTransactions,
	PooledTransactionsMsg:         handlePooledTransactions,
	BlockRangeUpdateMsg:           handleBlockRangeUpdate,
}

// xdpos2 handler map (refs #740). Mirrors eth/70's data-plane handlers and
// adds the three XDPoS V2 consensus messages. XDC↔XDC peering goes through
// this protocol so vote/timeout/sync-info gossip stays inside the fleet.
var xdpos2 = map[uint64]msgHandler{
	NewBlockHashesMsg:             handleNewBlockhashesXDC,
	NewBlockMsg:                   handleNewBlockXDC,
	TransactionsMsg:               handleTransactions,
	// A.60.3 (refs #857): msg code 0x08 is NewPooledTransactionHashesMsg on
	// modern eth/68+ but OrderTxMsg on legacy XDPoSChain (XDC DEX order-tx
	// broadcast). Routing inbound 0x08 from a legacy XDC peer through the
	// pooled-tx handler decodes the payload as `[]Hash` — but the actual
	// payload is `[]OrderTransaction`, a richer struct. The mismatch trips
	// the decoder and disconnects the peer. Route to a no-op consumer
	// instead: we don't run the XDC DEX, we just need to drain the message
	// off the wire so the peer stays connected.
	NewPooledTransactionHashesMsg: handleXDCTradingTxNoop,

	GetBlockHeadersMsg:            handleGetBlockHeaders,
	BlockHeadersMsg:               handleBlockHeaders,
	GetBlockBodiesMsg:             handleGetBlockBodies,
	BlockBodiesMsg:                handleBlockBodies,
	GetNodeDataMsg:                handleGetNodeData,
	NodeDataMsg:                   handleNodeData,
	// A.60.1 (refs #857): GetReceipts on XDPOS2 carries the eth/63 bare-list
	// wire shape `[]common.Hash`, NOT the eth/69+ {RequestId, hashes} wrapper.
	// Routing inbound 0x0f through handleGetReceipts70 trips its decoder on
	// the leading 32-byte hash trying to read a uint64 RequestId, fails with
	// "rlp: input string too long for uint64", and the remote disconnects us.
	GetReceiptsMsg:                handleGetReceiptsXDPOS2,
	// A.33: XDPOS2 ReceiptsMsg uses the bare-list wire shape (no RequestId),
	// not eth/70's RequestId+FirstBlockReceiptIndex wrapper. Route incoming
	// receipt responses through handleReceiptsXDPOS2 which decodes the bare
	// shape and dispatches via stashed legacyReceiptsReqID.
	ReceiptsMsg:                   handleReceiptsXDPOS2,
	// A.60.3 (refs #857): msg code 0x09 is GetPooledTransactionsMsg on modern
	// eth/68+ but LendingTxMsg on legacy XDPoSChain (XDC lending-protocol tx).
	// Same routing-mismatch story as 0x08 above; drain to no-op.
	GetPooledTransactionsMsg:      handleXDCTradingTxNoop,
	PooledTransactionsMsg:         handlePooledTransactions,
	BlockRangeUpdateMsg:           handleBlockRangeUpdate,
	VoteMsg:                       handleVoteMsg,
	TimeoutMsg:                    handleTimeoutMsg,
	SyncInfoMsg:                   handleSyncInfoMsg,
}

// handleNewBlockhashesXDC handles the legacy eth/63 NewBlockHashes message
// that production XDC peers gossip on XDPOS2. The earlier "accept-and-discard"
// implementation caused at-tip stall: xdcSyncer polls every 500ms and at tip
// most peers respond count=0 (they expect us to receive the block via the
// NewBlock broadcast instead). Now we decode + forward to backend.Handle for
// proper processing.
func handleNewBlockhashesXDC(backend Backend, msg Decoder, peer *Peer) error {
	ann := new(NewBlockHashesPacket)
	if err := msg.Decode(ann); err != nil {
		return fmt.Errorf("failed to decode NewBlockHashesPacket: %v", err)
	}
	return backend.Handle(peer, ann)
}

// handleNewBlockXDC handles the legacy eth/63 NewBlock message: a fully-mined
// tip block + the announcing peer's TD. Decodes and dispatches to backend
// for chain insertion. Without this, the node falls behind tip at the
// canonical block-production rate (~0.5 blocks/sec on Apothem) because the
// xdcSyncer's request path returns count=0 from peers that have already
// gossiped the block via this broadcast.
func handleNewBlockXDC(backend Backend, msg Decoder, peer *Peer) error {
	ann := new(NewBlockPacket)
	if err := msg.Decode(ann); err != nil {
		return fmt.Errorf("failed to decode NewBlockPacket: %v", err)
	}
	if ann.Block == nil {
		return fmt.Errorf("nil block in NewBlockPacket")
	}
	ann.Block.ReceivedAt = time.Now()
	ann.Block.ReceivedFrom = peer
	return backend.Handle(peer, ann)
}

// eth71 handler map (upstream eth/71 additively included for non-XDC peers).
var eth71 = map[uint64]msgHandler{
	TransactionsMsg:               handleTransactions,
	NewPooledTransactionHashesMsg: handleNewPooledTransactionHashes,
	GetBlockHeadersMsg:            handleGetBlockHeaders,
	BlockHeadersMsg:               handleBlockHeaders,
	GetBlockBodiesMsg:             handleGetBlockBodies,
	BlockBodiesMsg:                handleBlockBodies,
	GetReceiptsMsg:                handleGetReceipts70,
	ReceiptsMsg:                   handleReceipts70,
	GetPooledTransactionsMsg:      handleGetPooledTransactions,
	PooledTransactionsMsg:         handlePooledTransactions,
	BlockRangeUpdateMsg:           handleBlockRangeUpdate,
	GetBlockAccessListsMsg:        handleGetBlockAccessLists,
	BlockAccessListsMsg:           handleBlockAccessLists,
}

var eth72 = map[uint64]msgHandler{
	TransactionsMsg:               handleTransactions,
	NewPooledTransactionHashesMsg: handleNewPooledTransactionHashes72,
	GetBlockHeadersMsg:            handleGetBlockHeaders,
	BlockHeadersMsg:               handleBlockHeaders,
	GetBlockBodiesMsg:             handleGetBlockBodies,
	BlockBodiesMsg:                handleBlockBodies,
	GetReceiptsMsg:                handleGetReceipts70,
	ReceiptsMsg:                   handleReceipts70,
	GetPooledTransactionsMsg:      handleGetPooledTransactions,
	PooledTransactionsMsg:         handlePooledTransactions,
	BlockRangeUpdateMsg:           handleBlockRangeUpdate,
	GetBlockAccessListsMsg:        handleGetBlockAccessLists,
	BlockAccessListsMsg:           handleBlockAccessLists,
	GetCellsMsg:                   handleGetCells,
	CellsMsg:                      handleCells,
}

// handleMessage is invoked whenever an inbound message is received from a remote
// peer. The remote connection is torn down upon returning any error.
func handleMessage(backend Backend, peer *Peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := peer.rw.ReadMsg()
	if err != nil {
		return err
	}
	defer msg.Discard()
	if msg.Size > maxMessageSize {
		return fmt.Errorf("%w: %v > %v", errMsgTooLarge, msg.Size, maxMessageSize)
	}

	var handlers map[uint64]msgHandler
	switch peer.version {
	case ETH69:
		handlers = eth69
	case ETH70:
		handlers = eth70
	case ETH71:
		handlers = eth71
	case ETH72:
		handlers = eth72
	case XDPOS2:
		handlers = xdpos2
	default:
		return fmt.Errorf("unknown eth protocol version: %v", peer.version)
	}

	// Track the amount of time it takes to serve the request and run the handler
	if metrics.Enabled() {
		h := fmt.Sprintf("%s/%s/%d/%#02x", p2p.HandleHistName, ProtocolName, peer.Version(), msg.Code)
		defer func(start time.Time) {
			sampler := func() metrics.Sample {
				return metrics.ResettingSample(
					metrics.NewExpDecaySample(1028, 0.015),
				)
			}
			metrics.GetOrRegisterHistogramLazy(h, nil, sampler).Update(time.Since(start).Microseconds())
		}(time.Now())
	}
	if handler := handlers[msg.Code]; handler != nil {
		return handler(backend, msg, peer)
	}
	return fmt.Errorf("%w: %v", errInvalidMsgCode, msg.Code)
}
