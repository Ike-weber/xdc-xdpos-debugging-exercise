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
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/tracker"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// maxKnownTxs is the maximum transactions hashes to keep in the known list
	// before starting to randomly evict them.
	maxKnownTxs = 32768

	// MaxKnownNodes is the maximum state-trie node hashes to remember per peer.
	// Ported from XDPoSChain's bounded per-peer known-set pattern (A.41 / #844)
	// to avoid unbounded heap growth in the per-peer set when the connected fleet
	// is large (>256 peers). Mirrors the legacy `maxKnown*` constants in shape.
	MaxKnownNodes = 1024

	// maxQueuedTxs is the maximum number of transactions to queue up before dropping
	// older broadcasts.
	maxQueuedTxs = 4096

	// maxQueuedTxAnns is the maximum number of transaction announcements to queue up
	// before dropping older announcements.
	maxQueuedTxAnns = 4096
)

// receiptRequest tracks the state of an in-flight receipt retrieval operation.
type receiptRequest struct {
	request     []common.Hash  // block hashes corresponding to the requested receipts
	gasUsed     []uint64       // block gas used corresponding to the requested receipts
	timestamps  []uint64       // block timestamps corresponding to the requested receipts
	list        []*ReceiptList // list of partially collected receipts
	lastLogSize uint64         // log size of last receipt list
}

// Peer is a collection of relevant information we have about a `eth` peer.
// legacyHeaderReq tracks one in-flight XDPOS2 header request so the
// bridge in handleBlockHeaders can match responses (which carry no
// RequestId on the XDPOS2 wire) to their original dispatcher sinks
// AND re-anchor the response when the peer returns headers from a
// different starting block (a common XDPOS2 server-side quirk).
// Refs #807 Phase 9.3 + Phase 10.
type legacyHeaderReq struct {
	id           uint64      // dispatcher request id
	count        int         // requested header count (for response truncation)
	anchorHash   common.Hash // ByHash request: expected hash of headers[0]
	anchorNumber uint64      // ByNumber request: expected number of headers[0]
}

type Peer struct {
	*p2p.Peer // The embedded P2P package peer

	id string // Unique ID for the peer, cached

	rw        p2p.MsgReadWriter // Input/output streams for snap
	version   uint              // Protocol version negotiated
	lastRange atomic.Pointer[BlockRangeUpdatePacket]

	// XDC: XDPOS2 sends data-plane requests without a RequestId wrapper, so
	// responses can't be matched by id alone. The downloader keeps requests
	// single-flight per (peer, code), so tracking the most recently sent
	// outbound id per request type is sufficient to route the response.
	// All accesses are atomic; zero means no in-flight request.
	legacyHeaderReqID    atomic.Uint64
	legacyHeaderReqCount atomic.Uint64 // requested header count for last in-flight req (#807 Phase 9)

	// FIFO queue of pending header request (id, count) pairs for the
	// XDPOS2 → dispatcher bridge. XDPOS2 wire has no RequestId on
	// responses, so the bridge must match responses to requests in
	// arrival order. Multiple concurrent skeleton-sync fetchers from
	// the downloader push entries here; the BlockHeaders handler pops
	// the oldest. Refs #807 Phase 9.3.
	legacyHeaderQueueMu sync.Mutex
	legacyHeaderQueue   []legacyHeaderReq

	legacyBodyReqID     atomic.Uint64
	legacyReceiptsReqID atomic.Uint64

	// XDC: dedicated response channels for XDPOS2 legacy data-plane messages.
	// Bypasses the dispatcher's pending-map + tracker (which is incompatible
	// with the legacy bare-list wire format that has no RequestId). The
	// xdcSyncer's fetchHeaderBatch/fetchBodiesBatch read directly from these.
	XDCHeaderResp chan []*types.Header // BlockHeaders responses on XDPOS2
	XDCBodyResp   chan []BlockBody     // BlockBodies responses on XDPOS2

	txpool      TxPool // Transaction pool used by the broadcasters for liveness checks
	blobpool    BlobPool
	knownTxs    *knownCache        // Set of transaction hashes known to be known by this peer
	knownNodes  *knownCache        // Set of state-trie node hashes the peer has served us (A.41, refs #844)
	txBroadcast chan []common.Hash // Channel used to queue transaction propagation requests
	txAnnounce  chan []common.Hash // Channel used to queue transaction announcement requests

	// dead is a cached single-flag view of peer health, set by Close() so callers
	// polling peer-health on a hot path can avoid re-evaluating connection state
	// each cycle. Ported from XDPoSChain's `markedDeadOrLive` pattern (A.41).
	dead atomic.Bool

	// remoteCaps holds the XDC capabilities trailer advertised by the peer in
	// the XDPOS2 handshake (A.37). nil for non-XDPOS2 peers or pre-A.37 XDC
	// peers that omitted the optional trailer. The downloader reads this once
	// at peer registration to seed proactive scheme/legacy skip flags so
	// NodeData isn't dispatched to incompatible-binary peers. Refs #844 (A.37).
	remoteCaps atomic.Pointer[XDCCaps]

	tracker     *tracker.Tracker
	reqDispatch chan *request  // Dispatch channel to send requests and track then until fulfillment
	reqCancel   chan *cancel   // Dispatch channel to cancel pending requests and untrack them
	resDispatch chan *response // Dispatch channel to fulfil pending requests and untrack them

	chainConfig *params.ChainConfig // Chain configuration for fork-aware validation

	receiptBuffer     map[uint64]*receiptRequest // Previously requested receipts to buffer partial receipts
	receiptBufferLock sync.Mutex                 // Lock for protecting the receiptBuffer

	term chan struct{} // Termination channel to stop the broadcasters
}

// NewPeer creates a wrapper for a network connection and negotiated  protocol
// version.
func NewPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter, txpool TxPool, blobpool BlobPool, chainConfig *params.ChainConfig) *Peer {
	cap := p2p.Cap{Name: ProtocolName, Version: version}
	id := p.ID().String()
	peer := &Peer{
		id:            id,
		Peer:          p,
		rw:            rw,
		version:       version,
		knownTxs:      newKnownCache(maxKnownTxs),
		knownNodes:    newKnownCache(MaxKnownNodes),
		txBroadcast:   make(chan []common.Hash),
		txAnnounce:    make(chan []common.Hash),
		tracker:       tracker.New(cap, id, 5*time.Minute),
		reqDispatch:   make(chan *request),
		reqCancel:     make(chan *cancel),
		resDispatch:   make(chan *response),
		txpool:        txpool,
		blobpool:      blobpool,
		chainConfig:   chainConfig,
		receiptBuffer: make(map[uint64]*receiptRequest),
		term:          make(chan struct{}),
		XDCHeaderResp: make(chan []*types.Header, 1),
		XDCBodyResp:   make(chan []BlockBody, 1),
	}
	// Start up all the broadcasters
	go peer.broadcastTransactions()
	go peer.announceTransactions()
	go peer.dispatcher()

	return peer
}

// Close signals the broadcast goroutine to terminate. Only ever call this if
// you created the peer yourself via NewPeer. Otherwise let whoever created it
// clean it up!
func (p *Peer) Close() {
	p.dead.Store(true)
	close(p.term)
}

// MarkNode records that the peer is known to hold the given state-trie node
// hash, trimming the per-peer known-set to MaxKnownNodes. Used by the fast-sync
// state fetcher so the dispatcher can prefer peers that have previously served
// a given hash. Refs #844 (A.41).
func (p *Peer) MarkNode(hash common.Hash) {
	p.knownNodes.Add(hash)
}

// KnownNode returns whether the peer is known to have served the given state
// node hash. Intended for the NodeData dispatcher to prefer warm peers over
// re-asking peers that already lacked the hash. Refs #844 (A.41).
// TODO(A.42): wire into eth/downloader queue.go assignNodeData path once the
// dispatcher exposes a per-peer scoring hook. For now the field is populated
// but unread; this keeps the diff surgical and the consensus path untouched.
func (p *Peer) KnownNode(hash common.Hash) bool {
	return p.knownNodes.Contains(hash)
}

// MarkDead flags the peer as no longer usable. Cheap single-flag alternative
// to inspecting connection state. Refs #844 (A.41).
func (p *Peer) MarkDead() { p.dead.Store(true) }

// IsDead reports whether the peer has been marked dead (or closed).
func (p *Peer) IsDead() bool { return p.dead.Load() }

// setRemoteCaps stashes the peer's advertised XDC capabilities (A.37).
func (p *Peer) setRemoteCaps(c XDCCaps) { cp := c; p.remoteCaps.Store(&cp) }

// RemoteCaps returns the peer's XDPOS2 XDCCaps trailer, or nil if absent
// (non-XDPOS2 peer or pre-A.37 legacy peer that omitted the trailer).
func (p *Peer) RemoteCaps() *XDCCaps { return p.remoteCaps.Load() }

// RemoteStateSchemeHBSS reports whether the peer advertised HBSS. Returns
// true on unknown (pre-A.37) for conservative inclusion — A.40's reactive
// blacklist catches PBSS-only legacy peers after a few empty responses.
func (p *Peer) RemoteStateSchemeHBSS() bool {
	c := p.remoteCaps.Load()
	if c == nil {
		return true
	}
	return c.Scheme == XDCStateSchemeHBSS || c.Scheme == XDCStateSchemeUnknown
}

// RemoteLegacy reports whether the peer is "legacy" from the A.37 gate's
// perspective. After the A.37 wire-shape fix Caps is no longer exchanged
// on the wire, so remoteCaps is always nil. We return false (NOT legacy)
// as the safe default: every peer stays eligible for NodeData and A.40's
// reactive PBSS blacklist evicts the ones that can't actually serve it.
// Kept as a method (rather than deleted) so the downloader's xdcCapsPeer
// interface and tests that exercise it continue to compile cleanly.
func (p *Peer) RemoteLegacy() bool {
	c := p.remoteCaps.Load()
	if c == nil {
		return false
	}
	return c.CommitPrefix == ([8]byte{})
}

// ID retrieves the peer's unique identifier.
func (p *Peer) ID() string {
	return p.id
}

// Send writes an arbitrary message to the peer on the eth protocol stream.
// Used by V2 BFT broadcast helpers (BroadcastVote/Timeout/SyncInfo) in
// eth/handler_xdc.go that send raw RLP payloads outside the request/response
// dispatcher's flow. Refs #740.
func (p *Peer) Send(code uint64, msg interface{}) error {
	return p2p.Send(p.rw, code, msg)
}

// Version retrieves the peer's negotiated `eth` protocol version.
func (p *Peer) Version() uint {
	return p.version
}

// LastXDCHeaderReqID returns the most recent outbound GetBlockHeaders
// request ID sent to this peer over XDPOS2. Used by the legacy
// dispatch path in handleBlockHeaders to match the bare-list response
// back to the originating request (which has no RequestId on the wire).
func (p *Peer) LastXDCHeaderReqID() uint64 {
	return p.legacyHeaderReqID.Load()
}

// LastXDCBodyReqID returns the most recent outbound GetBlockBodies
// request ID sent to this peer over XDPOS2. Mirrors LastXDCHeaderReqID
// for bodies.
func (p *Peer) LastXDCBodyReqID() uint64 {
	return p.legacyBodyReqID.Load()
}

// BlockRange returns the latest announced block range.
// This will be nil for peers below protocol version eth/69.
// pushLegacyHeaderReqHash enqueues a hash-anchored header request.
// Refs #807 Phase 9.3 + Phase 10.
func (p *Peer) pushLegacyHeaderReqHash(id uint64, count int, hash common.Hash) {
	p.legacyHeaderQueueMu.Lock()
	p.legacyHeaderQueue = append(p.legacyHeaderQueue, legacyHeaderReq{id: id, count: count, anchorHash: hash})
	p.legacyHeaderQueueMu.Unlock()
	p.legacyHeaderReqID.Store(id)
	p.legacyHeaderReqCount.Store(uint64(count))
}

// pushLegacyHeaderReqNumber enqueues a number-anchored header request.
func (p *Peer) pushLegacyHeaderReqNumber(id uint64, count int, number uint64) {
	p.legacyHeaderQueueMu.Lock()
	p.legacyHeaderQueue = append(p.legacyHeaderQueue, legacyHeaderReq{id: id, count: count, anchorNumber: number})
	p.legacyHeaderQueueMu.Unlock()
	p.legacyHeaderReqID.Store(id)
	p.legacyHeaderReqCount.Store(uint64(count))
}

// popLegacyHeaderReq removes and returns the oldest pending header
// request. Returns zero req.id if queue is empty.
func (p *Peer) popLegacyHeaderReq() legacyHeaderReq {
	p.legacyHeaderQueueMu.Lock()
	defer p.legacyHeaderQueueMu.Unlock()
	if len(p.legacyHeaderQueue) == 0 {
		return legacyHeaderReq{}
	}
	req := p.legacyHeaderQueue[0]
	p.legacyHeaderQueue = p.legacyHeaderQueue[1:]
	return req
}

func (p *Peer) BlockRange() *BlockRangeUpdatePacket {
	return p.lastRange.Load()
}

// KnownTransaction returns whether peer is known to already have a transaction.
func (p *Peer) KnownTransaction(hash common.Hash) bool {
	return p.knownTxs.Contains(hash)
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
func (p *Peer) MarkTransaction(hash common.Hash) {
	// If we reached the memory allowance, drop a previously known transaction hash
	p.knownTxs.Add(hash)
}

// SendTransactions sends transactions to the peer and includes the hashes
// in its transaction hash set for future reference.
//
// This method is a helper used by the async transaction sender. Don't call it
// directly as the queueing (memory) and transmission (bandwidth) costs should
// not be managed directly.
//
// The reasons this is public is to allow packages using this protocol to write
// tests that directly send messages without having to do the async queueing.
func (p *Peer) SendTransactions(txs types.Transactions) error {
	if err := p2p.Send(p.rw, TransactionsMsg, txs); err != nil {
		return err
	}
	for _, tx := range txs {
		p.knownTxs.Add(tx.Hash())
	}
	return nil
}

// AsyncSendTransactions queues a list of transactions (by hash) to eventually
// propagate to a remote peer. The number of pending sends are capped (new ones
// will force old sends to be dropped)
func (p *Peer) AsyncSendTransactions(hashes []common.Hash) {
	select {
	case p.txBroadcast <- hashes:
		// Mark all the transactions as known, but ensure we don't overflow our limits
		p.knownTxs.Add(hashes...)
	case <-p.term:
		p.Log().Debug("Dropping transaction propagation", "count", len(hashes))
	}
}

// sendPooledTransactionHashes sends transaction hashes (tagged with their type
// and size) to the peer and includes them in its transaction hash set for future
// reference.
//
// This method is a helper used by the async transaction announcer. Don't call it
// directly as the queueing (memory) and transmission (bandwidth) costs should
// not be managed directly.
func (p *Peer) sendPooledTransactionHashes(hashes []common.Hash, types []byte, sizes []uint32, cells types.CustodyBitmap) error {
	// A.60.2 (refs #857): XDPoSChain v2.7.0 uses msg code 0x08 for OrderTxMsg
	// (the XDC DEX order-tx broadcast), not NewPooledTransactionHashes which
	// modern eth/68+ introduced. Sending NPTH to an XDPOS2 peer hands them a
	// payload they decode as `[]OrderTransaction` — garbage — and they
	// disconnect us with a generic subprotocol error. We mark the txs as
	// known so the announcer doesn't retry, but skip the wire send. This
	// early return must come first: p.version == XDPOS2 (100) would also
	// satisfy p.version >= ETH72 below, so without the early return the
	// XDPOS2 case would fall through into the upstream send path.
	// Transactions still propagate to XDPOS2 peers via the full TransactionsMsg
	// (0x02) gossip path, matching XDPoSChain's own tx-propagation behaviour.
	if p.version == XDPOS2 {
		p.knownTxs.Add(hashes...) // suppress retries; no wire send on XDPOS2
		return nil
	}
	var err error
	if p.version >= ETH72 {
		err = p2p.Send(p.rw, NewPooledTransactionHashesMsg, NewPooledTransactionHashesPacket72{Types: types, Sizes: sizes, Hashes: hashes, Mask: cells})
	} else {
		err = p2p.Send(p.rw, NewPooledTransactionHashesMsg, NewPooledTransactionHashesPacket71{Types: types, Sizes: sizes, Hashes: hashes})
	}
	if err != nil {
		return err
	}
	// Mark all the transactions as known, but ensure we don't overflow our limits
	p.knownTxs.Add(hashes...)
	return nil
}

// AsyncSendPooledTransactionHashes queues a list of transactions hashes to eventually
// announce to a remote peer.  The number of pending sends are capped (new ones
// will force old sends to be dropped)
func (p *Peer) AsyncSendPooledTransactionHashes(hashes []common.Hash) {
	select {
	case p.txAnnounce <- hashes:
		// Mark all the transactions as known, but ensure we don't overflow our limits
		p.knownTxs.Add(hashes...)
	case <-p.term:
		p.Log().Debug("Dropping transaction announcement", "count", len(hashes))
	}
}

// ReplyPooledTransactionsRLP is the response to RequestTxs.
func (p *Peer) ReplyPooledTransactionsRLP(id uint64, hashes []common.Hash, txs []rlp.RawValue) error {
	// Not packed into PooledTransactionsResponse to avoid RLP decoding
	if err := p2p.Send(p.rw, PooledTransactionsMsg, &PooledTransactionsRLPPacket{
		RequestId:                     id,
		PooledTransactionsRLPResponse: txs,
	}); err != nil {
		return err
	}
	p.knownTxs.Add(hashes...)
	return nil
}

// ReplyBlockHeadersRLP is the response to GetBlockHeaders.
func (p *Peer) ReplyBlockHeadersRLP(id uint64, headers []rlp.RawValue) error {
	return p2p.Send(p.rw, BlockHeadersMsg, &BlockHeadersRLPPacket{
		RequestId:               id,
		BlockHeadersRLPResponse: headers,
	})
}

// ReplyBlockHeadersRLPLegacy is the response to GetBlockHeaders on the
// XDC XDPOS2 protocol — bare list without the RequestId wrapper.
// Matches the eth/63 wire shape used by the canonical XDPoSChain fleet.
func (p *Peer) ReplyBlockHeadersRLPLegacy(headers []rlp.RawValue) error {
	return p2p.Send(p.rw, BlockHeadersMsg, headers)
}

// ReplyBlockBodiesRLPLegacy is the response to GetBlockBodies on XDPOS2 —
// bare list, no RequestId wrapper. Mirrors the headers legacy reply.
func (p *Peer) ReplyBlockBodiesRLPLegacy(bodies []rlp.RawValue) error {
	return p2p.Send(p.rw, BlockBodiesMsg, bodies)
}

// ReplyBlockBodiesRLP is the response to GetBlockBodies.
func (p *Peer) ReplyBlockBodiesRLP(id uint64, bodies []rlp.RawValue) error {
	// Not packed into BlockBodiesResponse to avoid RLP decoding
	return p2p.Send(p.rw, BlockBodiesMsg, &BlockBodiesRLPPacket{
		RequestId:              id,
		BlockBodiesRLPResponse: bodies,
	})
}

// RequestNodeData fetches a batch of arbitrary state trie nodes from a remote
// peer (XDPOS2 / eth-63 shape — no RequestID wrapper). Refs #844, #845.
func (p *Peer) RequestNodeData(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of state nodes", "count", len(hashes))
	pkt := GetNodeDataPacket(hashes)
	return p2p.Send(p.rw, GetNodeDataMsg, &pkt)
}

// ReplyNodeData sends a batch of state trie nodes to a remote peer
// in response to a GetNodeData query (XDPOS2 / eth-63 shape).
func (p *Peer) ReplyNodeData(data [][]byte) error {
	pkt := NodeDataPacket(data)
	return p2p.Send(p.rw, NodeDataMsg, pkt)
}

// ReplyReceiptsRLP69 is the response to GetReceipts.
func (p *Peer) ReplyReceiptsRLP69(id uint64, receipts rlp.RawList[*ReceiptList]) error {
	return p2p.Send(p.rw, ReceiptsMsg, &ReceiptsPacket69{
		RequestId: id,
		List:      receipts,
	})
}

// ReplyCells is the response to GetCells.
func (p *Peer) ReplyCells(id uint64, hashes []common.Hash, cells [][]kzg4844.Cell, mask types.CustodyBitmap) error {
	inner := make([]rlp.RawList[kzg4844.Cell], len(cells))
	for i, c := range cells {
		raw, err := rlp.EncodeToRawList(c)
		if err != nil {
			return err
		}
		inner[i] = raw
	}
	rawCells, err := rlp.EncodeToRawList(inner)
	if err != nil {
		return err
	}
	return p2p.Send(p.rw, CellsMsg, &CellsPacket{
		RequestId: id,
		CellsResponse: CellsResponse{
			Hashes: hashes,
			Cells:  rawCells,
			Mask:   mask,
		},
	})
}

// RequestPayload fetches a batch of cells from a remote node.
func (p *Peer) RequestPayload(hashes []common.Hash, cell types.CustodyBitmap) error {
	p.Log().Debug("Fetching batch of cells", "txcount", len(hashes), "cellcount", cell.OneCount())
	id := rand.Uint64()

	err := p.tracker.Track(tracker.Request{
		ID:       id,
		ReqCode:  GetCellsMsg,
		RespCode: CellsMsg,
		Size:     len(hashes),
	})
	if err != nil {
		return err
	}
	return p2p.Send(p.rw, GetCellsMsg, &GetCellsRequestPacket{
		RequestId: id,
		GetCellsRequest: GetCellsRequest{
			Hashes: hashes,
			Mask:   cell,
		},
	})
}

// ReplyReceiptsRLP70 is the response to GetReceipts.
func (p *Peer) ReplyReceiptsRLP70(id uint64, receipts rlp.RawList[*ReceiptList], lastBlockIncomplete bool) error {
	return p2p.Send(p.rw, ReceiptsMsg, &ReceiptsPacket70{
		RequestId:           id,
		List:                receipts,
		LastBlockIncomplete: lastBlockIncomplete,
	})
}

// ReplyReceiptsRLPLegacy is the response to GetReceipts on the XDC XDPOS2
// protocol — bare receipts list without the RequestId / LastBlockIncomplete
// wrapper that eth/69+ carries. Matches the eth/63 wire shape used by the
// canonical XDPoSChain fleet (XDPoSChain/eth/peer.go SendReceiptsRLP).
// A.60.1 (refs #857): without this, msg code 0x10 sent to a legacy XDPOS2
// peer trips its decoder on the leading RequestId varint, and the peer
// disconnects us with the symmetric error to A.60's GetReceipts decode bug.
func (p *Peer) ReplyReceiptsRLPLegacy(receipts rlp.RawList[*ReceiptList]) error {
	return p2p.Send(p.rw, ReceiptsMsg, receipts)
}

// ReplyBlockAccessLists is the response to GetBlockAccessLists (EIP-8159).
func (p *Peer) ReplyBlockAccessLists(id uint64, list rlp.RawList[rlp.RawValue]) error {
	return p2p.Send(p.rw, BlockAccessListsMsg, &BlockAccessListPacket{
		RequestId: id,
		List:      list,
	})
}

// RequestBALs fetches block access lists for the given block hashes (EIP-8159)
func (p *Peer) RequestBALs(hashes []common.Hash, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching block access lists", "count", len(hashes))
	id := rand.Uint64()

	req := &Request{
		id:       id,
		sink:     sink,
		code:     GetBlockAccessListsMsg,
		want:     BlockAccessListsMsg,
		numItems: len(hashes),
		data: &GetBlockAccessListsPacket{
			RequestId:                  id,
			GetBlockAccessListsRequest: hashes,
		},
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// RequestOneHeader is a wrapper around the header query functions to fetch a
// single header. It is used solely by the fetcher.
func (p *Peer) RequestOneHeader(hash common.Hash, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching single header", "hash", hash)
	id := rand.Uint64()

	req := &Request{
		id:       id,
		sink:     sink,
		code:     GetBlockHeadersMsg,
		want:     BlockHeadersMsg,
		numItems: 1,
		data: &GetBlockHeadersPacket{
			RequestId: id,
			GetBlockHeadersRequest: &GetBlockHeadersRequest{
				Origin:  HashOrNumber{Hash: hash},
				Amount:  uint64(1),
				Skip:    uint64(0),
				Reverse: false,
			},
		},
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// RequestHeadersByHash fetches a batch of blocks' headers corresponding to the
// specified header query, based on the hash of an origin block.
func (p *Peer) RequestHeadersByHash(origin common.Hash, amount int, skip int, reverse bool, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromhash", origin, "skip", skip, "reverse", reverse)
	id := rand.Uint64()

	req := &Request{
		id:       id,
		sink:     sink,
		code:     GetBlockHeadersMsg,
		want:     BlockHeadersMsg,
		numItems: amount,
		data:     newHeadersRequestData(p.version, id, HashOrNumber{Hash: origin}, uint64(amount), uint64(skip), reverse),
	}
	if p.version == XDPOS2 {
		p.Log().Info("XDPOS2 GetBlockHeaders SEND",
			"peer", p.id[:8],
			"origin_hash", origin.Hex()[:18],
			"amount", amount,
			"skip", skip,
			"reverse", reverse,
			"req_id", id)
		p.pushLegacyHeaderReqHash(id, amount, origin)
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// RequestHeadersByNumber fetches a batch of blocks' headers corresponding to the
// specified header query, based on the number of an origin block.
func (p *Peer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromnum", origin, "skip", skip, "reverse", reverse)
	id := rand.Uint64()

	req := &Request{
		id:       id,
		sink:     sink,
		code:     GetBlockHeadersMsg,
		want:     BlockHeadersMsg,
		numItems: amount,
		data:     newHeadersRequestData(p.version, id, HashOrNumber{Number: origin}, uint64(amount), uint64(skip), reverse),
	}
	if p.version == XDPOS2 {
		p.Log().Info("XDPOS2 GetBlockHeaders SEND",
			"peer", p.id[:8],
			"origin_num", origin,
			"amount", amount,
			"skip", skip,
			"reverse", reverse,
			"req_id", id)
		p.pushLegacyHeaderReqNumber(id, amount, origin)
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// newHeadersRequestData picks the right wire shape based on peer version.
// XDPOS2 (XDC pre-merge) uses the bare eth/63 GetBlockHeadersRequest; all
// other versions wrap it with RequestId per upstream eth/66+. Refs #740.
func newHeadersRequestData(version uint, id uint64, origin HashOrNumber, amount, skip uint64, reverse bool) interface{} {
	body := &GetBlockHeadersRequest{
		Origin:  origin,
		Amount:  amount,
		Skip:    skip,
		Reverse: reverse,
	}
	if version == XDPOS2 {
		return body
	}
	return &GetBlockHeadersPacket{
		RequestId:              id,
		GetBlockHeadersRequest: body,
	}
}

// RequestBodies fetches a batch of blocks' bodies corresponding to the hashes
// specified. For XDPOS2 peers, sends the bare hash list (eth/63 shape) so the
// production XDC fleet can decode it; response routing goes through
// Peer.XDCBodyResp (bypasses dispatcher).
func (p *Peer) RequestBodies(hashes []common.Hash, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching batch of block bodies", "count", len(hashes))
	id := rand.Uint64()

	var data interface{}
	if p.version == XDPOS2 {
		// Bare hash list — eth/63 wire shape used by production XDC peers.
		data = GetBlockBodiesRequest(hashes)
		p.legacyBodyReqID.Store(id)
	} else {
		data = &GetBlockBodiesPacket{
			RequestId:             id,
			GetBlockBodiesRequest: hashes,
		}
	}
	req := &Request{
		id:       id,
		sink:     sink,
		code:     GetBlockBodiesMsg,
		want:     BlockBodiesMsg,
		numItems: len(hashes),
		data:     data,
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// RequestReceipts fetches a batch of transaction receipts from a remote node.
// `gasUsed` provides the total gas used per block, used to estimate the maximum
// log byte size. `timestamps` provides the block timestamps for fork aware validation.
func (p *Peer) RequestReceipts(hashes []common.Hash, gasUsed []uint64, timestamps []uint64, sink chan *Response) (*Request, error) {
	p.Log().Debug("Fetching batch of receipts", "count", len(hashes))
	id := rand.Uint64()

	// A.33: XDPOS2 receipts use the bare-hash wire shape (eth/63 style) — no
	// RequestId wrapper, no FirstBlockReceiptIndex. Production XDC peers
	// decode `GetReceiptsRequest` directly. The response is routed back via
	// dispatcher using the stashed legacyReceiptsReqID (see handleReceipts).
	var req *Request
	if p.version == XDPOS2 {
		req = &Request{
			id:       id,
			sink:     sink,
			code:     GetReceiptsMsg,
			want:     ReceiptsMsg,
			numItems: len(hashes),
			data:     GetReceiptsRequest(hashes),
		}
		p.legacyReceiptsReqID.Store(id)
		if err := p.dispatchRequest(req); err != nil {
			return nil, err
		}
		return req, nil
	}
	if p.version > ETH69 {
		req = &Request{
			id:       id,
			sink:     sink,
			code:     GetReceiptsMsg,
			want:     ReceiptsMsg,
			numItems: len(hashes),
			data: &GetReceiptsPacket70{
				RequestId:              id,
				FirstBlockReceiptIndex: 0,
				GetReceiptsRequest:     hashes,
			},
		}
		p.receiptBufferLock.Lock()
		p.receiptBuffer[id] = &receiptRequest{
			request:    hashes,
			gasUsed:    gasUsed,
			timestamps: timestamps,
		}
		p.receiptBufferLock.Unlock()
	} else {
		req = &Request{
			id:       id,
			sink:     sink,
			code:     GetReceiptsMsg,
			want:     ReceiptsMsg,
			numItems: len(hashes),
			data: &GetReceiptsPacket69{
				RequestId:          id,
				GetReceiptsRequest: hashes,
			},
		}
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// HandlePartialReceipts re-request partial receipts
func (p *Peer) requestPartialReceipts(id uint64) error {
	p.receiptBufferLock.Lock()
	defer p.receiptBufferLock.Unlock()

	// Do not re-request for the stale request
	if _, ok := p.receiptBuffer[id]; !ok {
		return nil
	}
	lastBlock := len(p.receiptBuffer[id].list) - 1
	lastReceipt := p.receiptBuffer[id].list[lastBlock].items.Len()

	hashes := p.receiptBuffer[id].request[lastBlock:]

	req := &Request{
		id:   id,
		sink: nil,
		code: GetReceiptsMsg,
		want: ReceiptsMsg,
		data: &GetReceiptsPacket70{
			RequestId:              id,
			FirstBlockReceiptIndex: uint64(lastReceipt),
			GetReceiptsRequest:     hashes,
		},
		numItems: len(hashes),
	}
	return p.dispatchRequest(req)
}

// bufferReceipts validates a receipt packet and buffer the incomplete packet.
// If the request is completed, it appends previously collected receipts.
func (p *Peer) bufferReceipts(requestId uint64, receiptLists []*ReceiptList, lastBlockIncomplete bool, backend Backend) error {
	p.receiptBufferLock.Lock()
	defer p.receiptBufferLock.Unlock()

	buffer := p.receiptBuffer[requestId]

	// Short circuit for the canceled response
	if buffer == nil {
		return nil
	}
	// If the response is empty, the peer likely does not have the requested receipts.
	// Forward the empty response to the internal handler regardless. However, note
	// that an empty response marked as incomplete is considered invalid.
	if len(receiptLists) == 0 {
		delete(p.receiptBuffer, requestId)

		if lastBlockIncomplete {
			return errors.New("invalid empty receipt response with incomplete flag")
		}
		return nil
	}
	// Buffer the last block when the response is incomplete.
	if lastBlockIncomplete {
		lastBlock := len(receiptLists) - 1
		if len(buffer.list) > 0 {
			lastBlock += len(buffer.list) - 1
		}
		gasUsed := buffer.gasUsed[lastBlock]
		timestamp := buffer.timestamps[lastBlock]
		logSize, err := p.validateLastBlockReceipt(receiptLists, requestId, gasUsed, timestamp)
		if err != nil {
			delete(p.receiptBuffer, requestId)
			return err
		}
		// Update the buffered data and trim the packet to exclude the incomplete block.
		if len(buffer.list) > 0 {
			// If the buffer is already allocated, it means that the previous response
			// was incomplete Append the first block receipts.
			buffer.list[len(buffer.list)-1].Append(receiptLists[0])
			buffer.list = append(buffer.list, receiptLists[1:]...)
			buffer.lastLogSize = logSize
		} else {
			buffer.list = receiptLists
			buffer.lastLogSize = logSize
		}
		return nil
	}
	// Short circuit if there is nothing cached previously.
	if len(buffer.list) == 0 {
		delete(p.receiptBuffer, requestId)
		return nil
	}
	// Aggregate the cached result into the packet.
	buffer.list[len(buffer.list)-1].Append(receiptLists[0])
	buffer.list = append(buffer.list, receiptLists[1:]...)
	return nil
}

// flushReceipts retrieves the merged receipt lists from the buffer
// and removes the buffer entry. Returns nil if no buffered data exists.
func (p *Peer) flushReceipts(requestId uint64) []*ReceiptList {
	p.receiptBufferLock.Lock()
	defer p.receiptBufferLock.Unlock()

	buffer, ok := p.receiptBuffer[requestId]
	if !ok {
		return nil
	}
	delete(p.receiptBuffer, requestId)
	return buffer.list
}

// validateLastBlockReceipt validates receipts and return log size of last block receipt.
// This function is called only when the `lastBlockincomplete == true`.
//
// Note that the last receipt response (which completes receiptLists of a pending block)
// is not verified here. Those response doesn't need hueristics below since they can be
// verified by its trie root.
func (p *Peer) validateLastBlockReceipt(receiptLists []*ReceiptList, id uint64, gasUsed uint64, timestamp uint64) (uint64, error) {
	lastReceipts := receiptLists[len(receiptLists)-1]

	// If the receipt is in the middle of retrieval, use the buffered data.
	// e.g. [[receipt1], [receipt1, receipt2], incomplete = true]
	//      [[receipt3, receipt4], incomplete = true] <<--
	//      [[receipt5], [receipt1], incomplete = false]
	// This case happens only if len(receiptLists) == 1 && incomplete == true && buffered before.
	var previousTxs int
	var previousLog uint64
	if buffer, ok := p.receiptBuffer[id]; ok && len(buffer.list) > 0 && len(receiptLists) == 1 {
		previousTxs = buffer.list[len(buffer.list)-1].items.Len()
		previousLog = buffer.lastLogSize
	}

	// Verify that the total number of transactions delivered is under the limit.
	var minTxGas uint64
	if p.chainConfig != nil && p.chainConfig.AmsterdamTime != nil && *p.chainConfig.AmsterdamTime <= timestamp {
		minTxGas = 4500
	} else {
		minTxGas = 21000
	}
	if uint64(previousTxs+lastReceipts.items.Len()) > gasUsed/minTxGas {
		// should be dropped, don't clear the buffer
		return 0, fmt.Errorf("total number of tx exceeded limit")
	}
	// Count log size per receipt
	log, err := lastReceipts.LogsSize()
	if err != nil {
		return 0, err
	}
	// Verify that the overall downloaded receipt size does not exceed the block gas limit.
	if previousLog+log > gasUsed/params.LogDataGas {
		return 0, fmt.Errorf("total download receipt size exceeded the limit")
	}
	return previousLog + log, nil
}

// RequestTxs fetches a batch of transactions from a remote node.
func (p *Peer) RequestTxs(hashes []common.Hash) error {
	p.Log().Trace("Fetching batch of transactions", "count", len(hashes))
	// A.60.2 (refs #857): XDPoSChain v2.7.0 uses msg code 0x09 for LendingTxMsg
	// (XDC lending-protocol tx broadcast), NOT GetPooledTransactions. Sending
	// our RequestId-wrapped GetPooledTransactions to an XDPOS2 peer trips its
	// LendingTx decoder and the peer disconnects us. XDPoSChain has no
	// pull-by-hash mechanism for pending txs — they only propagate via full
	// TransactionsMsg gossip. No-op here keeps the peer alive; the txpool
	// will see the full tx when it arrives via the gossip path.
	if p.version == XDPOS2 {
		return nil
	}
	id := rand.Uint64()

	err := p.tracker.Track(tracker.Request{
		ID:       id,
		ReqCode:  GetPooledTransactionsMsg,
		RespCode: PooledTransactionsMsg,
		Size:     len(hashes),
	})
	if err != nil {
		return err
	}
	return p2p.Send(p.rw, GetPooledTransactionsMsg, &GetPooledTransactionsPacket{
		RequestId:                    id,
		GetPooledTransactionsRequest: hashes,
	})
}

// SendBlockRangeUpdate sends a notification about our available block range to the peer.
// XDPOS2 (XDPoSChain v2.7.0-devnet) has no handler for BlockRangeUpdateMsg (0x11);
// V1's xdpos2/100 message switch returns ErrInvalidMsgCode on receipt, closes the
// connection, and modern reads EOF — same wire-shape mismatch family as
// A.60.1/2/3 receive-side fixes for OrderTx/LendingTx/GetReceipts.
func (p *Peer) SendBlockRangeUpdate(msg BlockRangeUpdatePacket) error {
	if p.version < ETH69 || p.version == XDPOS2 {
		return nil
	}
	return p2p.Send(p.rw, BlockRangeUpdateMsg, &msg)
}

// knownCache is a cache for known hashes.
type knownCache struct {
	hashes mapset.Set[common.Hash]
	max    int
}

// newKnownCache creates a new knownCache with a max capacity.
func newKnownCache(max int) *knownCache {
	return &knownCache{
		max:    max,
		hashes: mapset.NewSet[common.Hash](),
	}
}

// Add adds a list of elements to the set.
func (k *knownCache) Add(hashes ...common.Hash) {
	for k.hashes.Cardinality() > max(0, k.max-len(hashes)) {
		k.hashes.Pop()
	}
	for _, hash := range hashes {
		k.hashes.Add(hash)
	}
}

// Contains returns whether the given item is in the set.
func (k *knownCache) Contains(hash common.Hash) bool {
	return k.hashes.Contains(hash)
}

// Cardinality returns the number of elements in the set.
func (k *knownCache) Cardinality() int {
	return k.hashes.Cardinality()
}
