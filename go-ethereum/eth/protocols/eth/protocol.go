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
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/rlp"
)

// Constants to match up protocol versions and messages
const (
	ETH63  = 63  // legacy eth/63 — required for XDC mainnet mesh acceptance (refs #857 A.59)
	ETH69  = 69
	ETH70  = 70
	ETH71  = 71  // upstream eth/71 (additively included)
	ETH72  = 72  // upstream eth/72 (sparse blobpool cells; not advertised on XDC, see below)
	XDPOS2 = 100 // XDC consensus protocol (refs #740)
)

// ProtocolName is the official short name of the `eth` protocol used during
// devp2p capability negotiation.
const ProtocolName = "eth"

// is primary). XDPOS2 is offered first so XDC peers prefer it over the
// upstream ETH/* family; stock-geth peers fall back to ETH/70 or ETH/69.
//
// A.59 (refs #857): ETH63 is appended to the advertised capability list so
// XDC public mesh peers — which expect `eth/63` in the cap list before
// accepting an inbound connection — see a compatible legacy version. devp2p
// version negotiation still picks the highest common version, which is
// XDPOS2/100 for every XDC peer (they all carry it), so the runtime wire
// protocol remains XDPOS2/100 and no eth/63 message handling is exercised.
// Side-by-side experiment 2026-06-06 showed fresh XDPoSChain v2.7.0 with
// `caps=[xdpos2,eth/63]` attracted 2 peers in 7 min while fresh modern
// without ETH63 sat at 0 peers for 50+ min on the same bootnodes.
//
// ETH71 and ETH72 are deliberately NOT advertised here even though both are
// fully implemented below (upstream sparse-blobpool / eth-72 support). XDC
// has no blob transactions, so eth/72 cells are dead code on every XDC
// network, and ProtocolVersions is the empirically-tuned devp2p cap set from
// A.59 above — perturbing it (by adding versions XDC peers don't speak and
// gains nothing from) is a separate, unjustified change from defining the
// constants/handlers upstream code needs to compile.
var ProtocolVersions = []uint{XDPOS2, ETH70, ETH69, ETH63}

// protocolLengths are the number of implemented message corresponding to
// different protocol versions. ETH63 uses 17 messages (msg codes 0x00..0x10)
// per the legacy go-ethereum eth/63 protocol definition; this is advertised
// for cap negotiation only and never runs at runtime on XDC peers because
// XDPOS2/100 always wins highest-common-version selection.
// ETH71/ETH72 included additively for upstream compat (not advertised, see above).
var protocolLengths = map[uint]uint64{ETH63: 17, ETH69: 18, ETH70: 18, ETH71: 20, ETH72: 22, XDPOS2: 229}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 10 * 1024 * 1024

// This is the maximum number of transactions in a Transactions message.
const maxTransactionAnnouncements = 5000

const (
	StatusMsg                     = 0x00
	NewBlockHashesMsg             = 0x01
	TransactionsMsg               = 0x02
	GetBlockHeadersMsg            = 0x03
	BlockHeadersMsg               = 0x04
	GetBlockBodiesMsg             = 0x05
	BlockBodiesMsg                = 0x06
	NewBlockMsg                   = 0x07
	NewPooledTransactionHashesMsg = 0x08
	GetPooledTransactionsMsg      = 0x09
	PooledTransactionsMsg         = 0x0a
	// XDC-specific NodeData messages on XDPOS2 (refs #844, #845).
	// Restored from upstream Geth v1.10.13 to enable classical Fast Sync.
	// HBSS-only on the server side — PBSS state scheme cannot serve these.
	GetNodeDataMsg = 0x0d
	NodeDataMsg    = 0x0e

	GetReceiptsMsg                = 0x0f
	ReceiptsMsg                   = 0x10
	BlockRangeUpdateMsg           = 0x11
	GetBlockAccessListsMsg        = 0x12
	BlockAccessListsMsg           = 0x13
	GetCellsMsg                   = 0x14
	CellsMsg                      = 0x15

	// XDPOS2 consensus messages (refs #740). Numeric codes preserved from XDC
	// fork to keep wire compatibility with existing XDC fleet nodes.
	VoteMsg     = 0xe0
	TimeoutMsg  = 0xe1
	SyncInfoMsg = 0xe2
)

var (
	errMsgTooLarge             = errors.New("message too long")
	errInvalidMsgCode          = errors.New("invalid message code")
	errProtocolVersionMismatch = errors.New("protocol version mismatch")
	// handshake errors
	errNoStatusMsg       = errors.New("no status message")
	errNetworkIDMismatch = errors.New("network ID mismatch")
	errGenesisMismatch   = errors.New("genesis mismatch")
	errForkIDRejected    = errors.New("fork ID rejected")
	errInvalidBlockRange = errors.New("invalid block range in status")
)

// Packet represents a p2p message in the `eth` protocol.
type Packet interface {
	Name() string // Name returns a string corresponding to the message type.
	Kind() byte   // Kind returns the message type.
}

// StatusPacket is the network packet for the status message (ETH/69, ETH/70 — upstream shape).
type StatusPacket struct {
	ProtocolVersion uint32
	NetworkID       uint64
	Genesis         common.Hash
	ForkID          forkid.ID
	// initial available block range
	EarliestBlock   uint64
	LatestBlock     uint64
	LatestBlockHash common.Hash
}

// XDC state-scheme constants advertised in the A.37 handshake trailer. Wire
// values are intentionally small to keep the optional RLP tail compact.
// Refs #844 (A.37) — proactive sibling to A.40's reactive PBSS blacklist.
const (
	XDCStateSchemeUnknown byte = 0 // peer did not advertise (pre-A.37 or non-XDC build)
	XDCStateSchemeHBSS    byte = 1 // legacy hash-based scheme, can serve NodeData
	XDCStateSchemePBSS    byte = 2 // path-based scheme, cannot serve classical NodeData
)

// XDCCaps is the XDC capabilities trailer attached to StatusPacket62. It
// announces the peer's state scheme and a short build identifier so a
// fast-sync requester can avoid NodeData against incompatible-binary peers.
// Encoded as the trailing optional field of StatusPacket62 — legacy peers
// (pre-A.37) silently omit the trailer and the decoder produces a zero-value
// XDCCaps. Refs #844 (A.37 ADR).
type XDCCaps struct {
	Scheme       byte    // one of XDCStateScheme{Unknown,HBSS,PBSS}
	CommitPrefix [8]byte // first 8 bytes of the peer's git commit (build identity)
}

// StatusPacket62 is the network packet for the status message on the XDC
// XDPOS2 protocol (refs #740). XDC is pre-merge and uses this format
// without ForkID. TD is a placeholder (block number on XDC); peers must
// not rely on it for sync decisions. Matches the production geth-XDC
// fleet's on-wire shape so reset binaries can complete the handshake.
//
// A.37 fix (#844): the wire shape MUST remain exactly 5 fields. The
// previous A.37 attempt appended an `rlp:"optional"` Caps trailer, which
// is decode-tolerant on OUR side but legacy peers (pre-A.37 geth-XDC,
// e.g. the canonical a119e0b archive fleet) use a strict 5-field decoder
// that rejects extra trailing list elements with "input list has too many
// elements" and drops the connection. The Caps gate is now derived
// entirely from REACTIVE signals (A.40 PBSS blacklist + empty-response
// counters). XDCCaps remains as a typed handle so the downloader's
// dormant gating flags continue to compile, but it is never read from or
// written to the wire.
type StatusPacket62 struct {
	ProtocolVersion uint32
	NetworkID       uint64
	TD              *big.Int // placeholder: block number on XDC (pre-merge)
	Head            common.Hash
	Genesis         common.Hash
}

// NewBlockHashesPacket is the legacy eth/63/XDPOS2 block-announcement packet
// (msg id 0x01). XDC peers gossip newly-produced tip blocks via this; without
// processing it the node stalls at tip even when bulk-sync caught up. Upstream
// v1.17.3 dropped this (post-merge geth has no NewBlock); XDC overlay restores it.
type NewBlockHashesPacket []struct {
	Hash   common.Hash
	Number uint64
}

// NewBlockPacket is the legacy eth/63/XDPOS2 NewBlock packet (msg id 0x07).
// Carries a fully-mined tip block plus the announcing peer's cumulative TD.
type NewBlockPacket struct {
	Block *types.Block
	TD    *big.Int
}

// TransactionsPacket is the network packet for broadcasting new transactions.
type TransactionsPacket struct {
	rlp.RawList[*types.Transaction]
}

// GetBlockHeadersRequest represents a block header query.
type GetBlockHeadersRequest struct {
	Origin  HashOrNumber // Block from which to retrieve headers
	Amount  uint64       // Maximum number of headers to retrieve
	Skip    uint64       // Blocks to skip between consecutive headers
	Reverse bool         // Query direction (false = rising towards latest, true = falling towards genesis)
}

// GetBlockHeadersPacket represents a block header query with request ID wrapping.
type GetBlockHeadersPacket struct {
	RequestId uint64
	*GetBlockHeadersRequest
}

// HashOrNumber is a combined field for specifying an origin block.
type HashOrNumber struct {
	Hash   common.Hash // Block hash from which to retrieve headers (excludes Number)
	Number uint64      // Block hash from which to retrieve headers (excludes Hash)
}

// EncodeRLP is a specialized encoder for HashOrNumber to encode only one of the
// two contained union fields.
func (hn *HashOrNumber) EncodeRLP(w io.Writer) error {
	if hn.Hash == (common.Hash{}) {
		return rlp.Encode(w, hn.Number)
	}
	if hn.Number != 0 {
		return fmt.Errorf("both origin hash (%x) and number (%d) provided", hn.Hash, hn.Number)
	}
	return rlp.Encode(w, hn.Hash)
}

// DecodeRLP is a specialized decoder for HashOrNumber to decode the contents
// into either a block hash or a block number.
func (hn *HashOrNumber) DecodeRLP(s *rlp.Stream) error {
	_, size, err := s.Kind()
	switch {
	case err != nil:
		return err
	case size == 32:
		hn.Number = 0
		return s.Decode(&hn.Hash)
	case size <= 8:
		hn.Hash = common.Hash{}
		return s.Decode(&hn.Number)
	default:
		return fmt.Errorf("invalid input size %d for origin", size)
	}
}

// BlockHeadersRequest represents a block header response.
type BlockHeadersRequest []*types.Header

// BlockHeadersPacket represents a block header response over with request ID wrapping.
type BlockHeadersPacket struct {
	RequestId uint64
	List      rlp.RawList[*types.Header]
}

// BlockHeadersRLPResponse represents a block header response, to use when we already
// have the headers rlp encoded.
type BlockHeadersRLPResponse []rlp.RawValue

// BlockHeadersRLPPacket represents a block header response with request ID wrapping.
type BlockHeadersRLPPacket struct {
	RequestId uint64
	BlockHeadersRLPResponse
}

// GetBlockBodiesRequest represents a block body query.
type GetBlockBodiesRequest []common.Hash

// GetBlockBodiesPacket represents a block body query with request ID wrapping.
type GetBlockBodiesPacket struct {
	RequestId uint64
	GetBlockBodiesRequest
}

// BlockBodiesPacket is the network packet for block content distribution with
// request ID wrapping.
type BlockBodiesPacket struct {
	RequestId uint64
	List      rlp.RawList[BlockBody]
}

// BlockBodiesRLPResponse is used for replying to block body requests, in cases
// where we already have them RLP-encoded, and thus can avoid the decode-encode
// roundtrip.
type BlockBodiesRLPResponse []rlp.RawValue

// BlockBodiesRLPPacket is the BlockBodiesRLPResponse with request ID wrapping.
type BlockBodiesRLPPacket struct {
	RequestId uint64
	BlockBodiesRLPResponse
}

// BlockBodiesResponse is the network packet for block content distribution.
type BlockBodiesResponse []BlockBody

// BlockBody represents the data content of a single block.
type BlockBody struct {
	Transactions rlp.RawList[*types.Transaction]
	Uncles       rlp.RawList[*types.Header]
	Withdrawals  *rlp.RawList[*types.Withdrawal] `rlp:"optional"`
}

// GetReceiptsRequest represents a block receipts query.
type GetReceiptsRequest []common.Hash

// GetReceiptsPacket69 represents a block receipts query with request ID wrapping.
type GetReceiptsPacket69 struct {
	RequestId uint64
	GetReceiptsRequest
}

// GetReceiptsPacket70 represents a block receipts query with request ID and
// FirstBlockReceiptIndex wrapping.
type GetReceiptsPacket70 struct {
	RequestId              uint64
	FirstBlockReceiptIndex uint64
	GetReceiptsRequest
}

// ReceiptsResponse is the network packet for block receipts distribution.
type ReceiptsResponse []types.Receipts

// ReceiptsPacket69 is the network packet for block receipts distribution with
// request ID wrapping.
type ReceiptsPacket69 struct {
	RequestId uint64
	List      rlp.RawList[*ReceiptList]
}

type ReceiptsPacket70 struct {
	RequestId           uint64
	LastBlockIncomplete bool
	List                rlp.RawList[*ReceiptList]
}

// ReceiptsRLPResponse is used for receipts, when we already have it encoded
type ReceiptsRLPResponse []rlp.RawValue

// NewPooledTransactionHashesPacket71 represents a transaction announcement packet on protocol version
// less than or equal to 71.
type NewPooledTransactionHashesPacket71 struct {
	Types  []byte
	Sizes  []uint32
	Hashes []common.Hash
}

// NewPooledTransactionHashesPacket72 represents a transaction announcement packet on ETH/72
// with an additional custody bitmap field for cell-based blob data availability.
type NewPooledTransactionHashesPacket72 struct {
	Types  []byte
	Sizes  []uint32
	Hashes []common.Hash
	Mask   types.CustodyBitmap
}

// GetPooledTransactionsRequest represents a transaction query.
type GetPooledTransactionsRequest []common.Hash

// GetPooledTransactionsPacket represents a transaction query with request ID wrapping.
type GetPooledTransactionsPacket struct {
	RequestId uint64
	GetPooledTransactionsRequest
}

// PooledTransactionsResponse is the network packet for transaction distribution.
type PooledTransactionsResponse []*types.Transaction

// PooledTransactionsPacket is the network packet for transaction distribution
// with request ID wrapping.
type PooledTransactionsPacket struct {
	RequestId uint64
	List      rlp.RawList[*types.Transaction]
}

// PooledTransactionsRLPResponse is the network packet for transaction distribution, used
// in the cases we already have them in rlp-encoded form
type PooledTransactionsRLPResponse []rlp.RawValue

// PooledTransactionsRLPPacket is PooledTransactionsRLPResponse with request ID wrapping.
type PooledTransactionsRLPPacket struct {
	RequestId uint64
	PooledTransactionsRLPResponse
}

// BlockRangeUpdatePacket is an announcement of the node's available block range.
type BlockRangeUpdatePacket struct {
	EarliestBlock   uint64
	LatestBlock     uint64
	LatestBlockHash common.Hash
}

// GetCellsRequest represents a request for cells of blob transactions.
type GetCellsRequest struct {
	Hashes []common.Hash
	Mask   types.CustodyBitmap
}

// GetCellsRequestPacket represents a cell request with request ID wrapping.
type GetCellsRequestPacket struct {
	RequestId uint64
	GetCellsRequest
}

// CellsResponse represents a response containing cells for blob transactions.
type CellsResponse struct {
	Hashes []common.Hash
	Cells  rlp.RawList[rlp.RawList[kzg4844.Cell]]
	Mask   types.CustodyBitmap
}

// CellsPacket represents a cells response with request ID wrapping.
type CellsPacket struct {
	RequestId uint64
	CellsResponse
}

type GetBlockAccessListsRequest []common.Hash

type GetBlockAccessListsPacket struct {
	RequestId uint64
	GetBlockAccessListsRequest
}

// BlockAccessListResponse holds one raw entry per requested hash. Entries are
// kept as raw values because, per EIP-8159, the RLP empty string signals an
// unavailable BAL (an empty list is itself a valid BAL).
type BlockAccessListResponse []rlp.RawValue

type BlockAccessListPacket struct {
	RequestId uint64
	List      rlp.RawList[rlp.RawValue]
}

func (*StatusPacket) Name() string { return "Status" }
func (*StatusPacket) Kind() byte   { return StatusMsg }

func (*StatusPacket62) Name() string { return "Status" }
func (*StatusPacket62) Kind() byte   { return StatusMsg }

func (*TransactionsPacket) Name() string { return "Transactions" }
func (*TransactionsPacket) Kind() byte   { return TransactionsMsg }

func (*GetBlockHeadersRequest) Name() string { return "GetBlockHeaders" }
func (*GetBlockHeadersRequest) Kind() byte   { return GetBlockHeadersMsg }

func (*BlockHeadersRequest) Name() string { return "BlockHeaders" }
func (*BlockHeadersRequest) Kind() byte   { return BlockHeadersMsg }

func (*GetBlockBodiesRequest) Name() string { return "GetBlockBodies" }
func (*GetBlockBodiesRequest) Kind() byte   { return GetBlockBodiesMsg }

func (*BlockBodiesResponse) Name() string { return "BlockBodies" }
func (*BlockBodiesResponse) Kind() byte   { return BlockBodiesMsg }

func (*NewBlockHashesPacket) Name() string { return "NewBlockHashes" }
func (*NewBlockHashesPacket) Kind() byte   { return NewBlockHashesMsg }

func (*NewBlockPacket) Name() string { return "NewBlock" }
func (*NewBlockPacket) Kind() byte   { return NewBlockMsg }

func (*NewPooledTransactionHashesPacket71) Name() string { return "NewPooledTransactionHashes" }
func (*NewPooledTransactionHashesPacket71) Kind() byte   { return NewPooledTransactionHashesMsg }

func (*NewPooledTransactionHashesPacket72) Name() string { return "NewPooledTransactionHashes" }
func (*NewPooledTransactionHashesPacket72) Kind() byte   { return NewPooledTransactionHashesMsg }

func (*GetPooledTransactionsRequest) Name() string { return "GetPooledTransactions" }
func (*GetPooledTransactionsRequest) Kind() byte   { return GetPooledTransactionsMsg }

func (*PooledTransactionsPacket) Name() string { return "PooledTransactions" }
func (*PooledTransactionsPacket) Kind() byte   { return PooledTransactionsMsg }

func (*GetReceiptsRequest) Name() string { return "GetReceipts" }
func (*GetReceiptsRequest) Kind() byte   { return GetReceiptsMsg }

func (*ReceiptsResponse) Name() string { return "Receipts" }
func (*ReceiptsResponse) Kind() byte   { return ReceiptsMsg }

func (*ReceiptsRLPResponse) Name() string { return "Receipts" }
func (*ReceiptsRLPResponse) Kind() byte   { return ReceiptsMsg }

func (*BlockRangeUpdatePacket) Name() string { return "BlockRangeUpdate" }
func (*BlockRangeUpdatePacket) Kind() byte   { return BlockRangeUpdateMsg }

// GetNodeDataPacket is the network packet for state trie node retrieval (XDPOS2/eth-63 shape).
type GetNodeDataPacket []common.Hash

// NodeDataPacket is the network packet for state trie node delivery (XDPOS2/eth-63 shape).
type NodeDataPacket [][]byte

func (*GetNodeDataPacket) Name() string { return "GetNodeData" }
func (*GetNodeDataPacket) Kind() byte   { return GetNodeDataMsg }

func (*NodeDataPacket) Name() string { return "NodeData" }
func (*NodeDataPacket) Kind() byte   { return NodeDataMsg }

// XDPOS2 consensus packets (refs #740). Vote/Timeout/SyncInfo bodies are
// forwarded as raw RLP — decoding happens inside the XDPoS engine to keep
// wire-layer code consensus-agnostic.

type VotePacket struct {
	Vote []byte
}

func (*VotePacket) Name() string { return "Vote" }
func (*VotePacket) Kind() byte   { return VoteMsg }

type TimeoutPacket struct {
	Timeout []byte
}

func (*TimeoutPacket) Name() string { return "Timeout" }
func (*TimeoutPacket) Kind() byte   { return TimeoutMsg }

type SyncInfoPacket struct {
	SyncInfo []byte
}

func (*SyncInfoPacket) Name() string { return "SyncInfo" }
func (*SyncInfoPacket) Kind() byte   { return SyncInfoMsg }

func (*GetBlockAccessListsRequest) Name() string { return "GetBlockAccessLists" }
func (*GetBlockAccessListsRequest) Kind() byte   { return GetBlockAccessListsMsg }

func (*BlockAccessListResponse) Name() string { return "BlockAccessLists" }
func (*BlockAccessListResponse) Kind() byte   { return BlockAccessListsMsg }

func (*GetCellsRequest) Name() string { return "GetCells" }
func (*GetCellsRequest) Kind() byte   { return GetCellsMsg }

func (*CellsResponse) Name() string { return "Cells" }
func (*CellsResponse) Kind() byte   { return CellsMsg }
