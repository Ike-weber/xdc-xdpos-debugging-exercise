// Copyright 2021 The go-ethereum Authors
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
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/tracker"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

func handleGetBlockHeaders(backend Backend, msg Decoder, peer *Peer) error {
	// XDC XDPOS2 uses the eth/63 message shape — no RequestId wrapper on
	// data-plane requests. peer.Version() is known here, so we can pick the
	// right decode target directly (no dual-decode/seek needed).
	if peer.Version() == XDPOS2 {
		var legacyQuery GetBlockHeadersRequest
		if err := msg.Decode(&legacyQuery); err != nil {
			return err
		}
		response := ServiceGetBlockHeadersQuery(backend.Chain(), &legacyQuery, peer)
		log.Info("XDPOS2 GetBlockHeaders RECEIVED (serving)",
			"peer", peer.ID()[:8],
			"origin", legacyQuery.Origin,
			"amount", legacyQuery.Amount,
			"skip", legacyQuery.Skip,
			"reverse", legacyQuery.Reverse,
			"response_count", len(response))
		return peer.ReplyBlockHeadersRLPLegacy(response)
	}
	// Decode the complex header query
	var query GetBlockHeadersPacket
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response := ServiceGetBlockHeadersQuery(backend.Chain(), query.GetBlockHeadersRequest, peer)
	return peer.ReplyBlockHeadersRLP(query.RequestId, response)
}

// ServiceGetBlockHeadersQuery assembles the response to a header query. It is
// exposed to allow external packages to test protocol behavior.
func ServiceGetBlockHeadersQuery(chain *core.BlockChain, query *GetBlockHeadersRequest, peer *Peer) []rlp.RawValue {
	if query.Amount == 0 {
		return nil
	}
	if query.Skip == 0 {
		// The fast path: when the request is for a contiguous segment of headers.
		return serviceContiguousBlockHeaderQuery(chain, query)
	} else {
		return serviceNonContiguousBlockHeaderQuery(chain, query, peer)
	}
}

func serviceNonContiguousBlockHeaderQuery(chain *core.BlockChain, query *GetBlockHeadersRequest, peer *Peer) []rlp.RawValue {
	hashMode := query.Origin.Hash != (common.Hash{})
	first := true
	maxNonCanonical := uint64(100)

	// Gather headers until the fetch or network limits is reached
	var (
		bytes   common.StorageSize
		headers []rlp.RawValue
		unknown bool
		lookups int
	)
	for !unknown && len(headers) < int(query.Amount) && bytes < softResponseLimit &&
		len(headers) < maxHeadersServe && lookups < 2*maxHeadersServe {
		lookups++
		// Retrieve the next header satisfying the query
		var origin *types.Header
		if hashMode {
			if first {
				first = false
				origin = chain.GetHeaderByHash(query.Origin.Hash)
				if origin != nil {
					query.Origin.Number = origin.Number.Uint64()
				}
			} else {
				origin = chain.GetHeader(query.Origin.Hash, query.Origin.Number)
			}
		} else {
			origin = chain.GetHeaderByNumber(query.Origin.Number)
		}
		if origin == nil {
			break
		}
		if rlpData, err := rlp.EncodeToBytes(origin); err != nil {
			log.Crit("Unable to encode our own headers", "err", err)
		} else {
			headers = append(headers, rlp.RawValue(rlpData))
			bytes += common.StorageSize(len(rlpData))
		}
		// Advance to the next header of the query
		switch {
		case hashMode && query.Reverse:
			// Hash based traversal towards the genesis block
			ancestor := query.Skip + 1
			if ancestor == 0 {
				unknown = true
			} else {
				query.Origin.Hash, query.Origin.Number = chain.GetAncestor(query.Origin.Hash, query.Origin.Number, ancestor, &maxNonCanonical)
				unknown = (query.Origin.Hash == common.Hash{})
			}
		case hashMode && !query.Reverse:
			// Hash based traversal towards the leaf block
			var (
				current = origin.Number.Uint64()
				next    = current + query.Skip + 1
			)
			if next <= current {
				infos, _ := json.MarshalIndent(peer.Peer.Info(), "", "  ")
				peer.Log().Warn("GetBlockHeaders skip overflow attack", "current", current, "skip", query.Skip, "next", next, "attacker", infos)
				unknown = true
			} else {
				if header := chain.GetHeaderByNumber(next); header != nil {
					nextHash := header.Hash()
					expOldHash, _ := chain.GetAncestor(nextHash, next, query.Skip+1, &maxNonCanonical)
					if expOldHash == query.Origin.Hash {
						query.Origin.Hash, query.Origin.Number = nextHash, next
					} else {
						unknown = true
					}
				} else {
					unknown = true
				}
			}
		case query.Reverse:
			// Number based traversal towards the genesis block
			current := query.Origin.Number
			ancestor := current - (query.Skip + 1)
			if ancestor >= current { // check for underflow
				unknown = true
			} else {
				query.Origin.Number = ancestor
			}

		case !query.Reverse:
			current := query.Origin.Number
			next := current + query.Skip + 1
			if next <= current { // check for overflow
				unknown = true
			} else {
				query.Origin.Number = next
			}
		}
	}
	return headers
}

func serviceContiguousBlockHeaderQuery(chain *core.BlockChain, query *GetBlockHeadersRequest) []rlp.RawValue {
	count := query.Amount
	if count > maxHeadersServe {
		count = maxHeadersServe
	}
	if query.Origin.Hash == (common.Hash{}) {
		// Number mode, just return the canon chain segment. The backend
		// delivers in [N, N-1, N-2..] descending order, so we need to
		// accommodate for that.
		from := query.Origin.Number
		if !query.Reverse {
			from = from + count - 1
		}
		headers := chain.GetHeadersFrom(from, count)
		if !query.Reverse {
			for i, j := 0, len(headers)-1; i < j; i, j = i+1, j-1 {
				headers[i], headers[j] = headers[j], headers[i]
			}
		}
		return headers
	}
	// Hash mode.
	var (
		headers []rlp.RawValue
		hash    = query.Origin.Hash
		header  = chain.GetHeaderByHash(hash)
	)
	if header != nil {
		rlpData, _ := rlp.EncodeToBytes(header)
		headers = append(headers, rlpData)
	} else {
		// We don't even have the origin header
		return headers
	}
	num := header.Number.Uint64()
	if !query.Reverse {
		// Theoretically, we are tasked to deliver header by hash H, and onwards.
		// However, if H is not canon, we will be unable to deliver any descendants of
		// H.
		if canonHash := chain.GetCanonicalHash(num); canonHash != hash {
			// Not canon, we can't deliver descendants
			return headers
		}
		descendants := chain.GetHeadersFrom(num+count-1, count-1)
		for i, j := 0, len(descendants)-1; i < j; i, j = i+1, j-1 {
			descendants[i], descendants[j] = descendants[j], descendants[i]
		}
		headers = append(headers, descendants...)
		return headers
	}
	{ // Last mode: deliver ancestors of H
		for i := uint64(1); i < count; i++ {
			header = chain.GetHeaderByHash(header.ParentHash)
			if header == nil {
				break
			}
			rlpData, _ := rlp.EncodeToBytes(header)
			headers = append(headers, rlpData)
		}
		return headers
	}
}

func handleGetBlockBodies(backend Backend, msg Decoder, peer *Peer) error {
	// XDPOS2 sends bare GetBlockBodiesRequest (eth/63 wire shape) without
	// the RequestId wrapper. Mirror of the headers path in handleGetBlockHeaders.
	if peer.Version() == XDPOS2 {
		var legacyQuery GetBlockBodiesRequest
		if err := msg.Decode(&legacyQuery); err != nil {
			return err
		}
		response := ServiceGetBlockBodiesQuery(backend.Chain(), legacyQuery)
		return peer.ReplyBlockBodiesRLPLegacy(response)
	}
	// Decode the block body retrieval message
	var query GetBlockBodiesPacket
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response := ServiceGetBlockBodiesQuery(backend.Chain(), query.GetBlockBodiesRequest)
	return peer.ReplyBlockBodiesRLP(query.RequestId, response)
}

// ServiceGetBlockBodiesQuery assembles the response to a body query. It is
// exposed to allow external packages to test protocol behavior.
func ServiceGetBlockBodiesQuery(chain *core.BlockChain, query GetBlockBodiesRequest) []rlp.RawValue {
	// Gather blocks until the fetch or network limits is reached
	var (
		bytes  int
		bodies []rlp.RawValue
	)
	for lookups, hash := range query {
		if bytes >= softResponseLimit || len(bodies) >= maxBodiesServe ||
			lookups >= 2*maxBodiesServe {
			break
		}
		data := chain.GetBodyRLP(hash)
		if len(data) == 0 {
			break // If we don't have this block's body, stop serving.
		}
		bodies = append(bodies, data)
		bytes += len(data)
	}
	return bodies
}

func handleGetReceipts69(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the block receipts retrieval message
	var query GetReceiptsPacket69
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response := ServiceGetReceiptsQuery69(backend.Chain(), query.GetReceiptsRequest)
	return peer.ReplyReceiptsRLP69(query.RequestId, response)
}

func handleGetReceipts70(backend Backend, msg Decoder, peer *Peer) error {
	var query GetReceiptsPacket70
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response, lastBlockIncomplete := serviceGetReceiptsQuery70(backend.Chain(), query.GetReceiptsRequest, query.FirstBlockReceiptIndex)
	return peer.ReplyReceiptsRLP70(query.RequestId, response, lastBlockIncomplete)
}

// handleXDCTradingTxNoop drains XDPoSChain's legacy DEX/lending transaction
// broadcasts (msg codes 0x08 OrderTxMsg and 0x09 LendingTxMsg) off the wire
// without decoding their bodies. Refs #857 A.60.3.
//
// On modern eth/68+ these two codes mean NewPooledTransactionHashes and
// GetPooledTransactions; on legacy XDPoSChain they mean OrderTx (XDC DEX)
// and LendingTx (XDC lending protocol). Both inbound shapes mis-decode
// when the modern pooled-tx handler tries to read them as
// `NewPooledTransactionHashesPacket{Types, Sizes, Hashes}` or as the
// RequestId-wrapped `GetPooledTransactionsPacket`, and the peer drops us.
//
// Modern XDC has not yet ported the DEX/lending protocols — when it does,
// this handler should be replaced with proper decoders. Until then we
// silently discard so the peer stays connected and other messages flow.
func handleXDCTradingTxNoop(backend Backend, msg Decoder, peer *Peer) error {
	// Drain the message bytes off the connection without decoding into a
	// typed struct. Returning nil signals "handled successfully" so devp2p
	// keeps the peer attached.
	var raw rlp.RawValue
	_ = msg.Decode(&raw)
	return nil
}

// handleGetReceiptsXDPOS2 decodes the XDPOS2 / eth-63 bare-list wire shape
// for GetReceipts (msg code 0x0f). XDPoSChain v2.7.0 sends the request as
// a flat `[]common.Hash`, NOT the eth/69+ `{RequestId, hashes}` wrapper.
//
// A.60.1 (refs #857): without this handler, msg code 0x0f from a legacy
// XDPOS2 peer was routed through handleGetReceipts70 which tried to decode
// the leading 32-byte hash as a uint64 RequestId, failed with
// `rlp: input string too long for uint64`, and disconnected the peer. The
// symmetric outbound path (peer.RequestReceipts on XDPOS2) already uses
// the bare shape; this handler closes the loop on the inbound side.
//
// The response is sent through ReplyReceiptsRLPLegacy which omits the
// RequestId and LastBlockIncomplete fields entirely, producing the
// eth/63 wire shape XDPoSChain expects: `[ list1, list2, ... ]`.
func handleGetReceiptsXDPOS2(backend Backend, msg Decoder, peer *Peer) error {
	var query GetReceiptsRequest
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response := ServiceGetReceiptsQuery69(backend.Chain(), query)
	return peer.ReplyReceiptsRLPLegacy(response)
}

// ServiceGetReceiptsQuery69 assembles the response to a receipt query.
// It does not send the bloom filters for the receipts. It is exposed
// to allow external packages to test protocol behavior.
func ServiceGetReceiptsQuery69(chain *core.BlockChain, query GetReceiptsRequest) rlp.RawList[*ReceiptList] {
	var (
		bytes    int
		receipts rlp.RawList[*ReceiptList]
	)
	for lookups, hash := range query {
		if bytes >= softResponseLimit || receipts.Len() >= maxReceiptsServe || lookups >= 2*maxReceiptsServe {
			break
		}

		// Retrieve the requested block's receipts
		results := chain.GetReceiptsRLP(hash)
		if results == nil {
			break // Don't have this block's receipts, stop serving.
		}
		body := chain.GetBodyRLP(hash)
		if body == nil {
			break // The block body is missing, stop serving.
		}
		results, _, err := blockReceiptsToNetwork(results, body, receiptQueryParams{})
		if err != nil {
			log.Error("Error in block receipts conversion", "hash", hash, "err", err)
			break
		}
		receipts.AppendRaw(results)
		bytes += len(results)
	}
	return receipts
}

// serviceGetReceiptsQuery70 assembles the response to a receipt query.
// If the receipts exceed 10 MiB, it trims them and sets the
// lastBlockIncomplete flag. Indices smaller than firstBlockReceiptIndex
// are omitted from the first block receipt list.
func serviceGetReceiptsQuery70(chain *core.BlockChain, query GetReceiptsRequest, firstBlockReceiptIndex uint64) (rlp.RawList[*ReceiptList], bool) {
	var (
		bytes    int
		receipts rlp.RawList[*ReceiptList]
	)
	for i, hash := range query {
		if bytes >= softResponseLimit || receipts.Len() >= maxReceiptsServe {
			break
		}
		results := chain.GetReceiptsRLP(hash)
		// If we don't have this block's receipts or body, stop serving.
		if results == nil {
			break
		}
		body := chain.GetBodyRLP(hash)
		if body == nil {
			break
		}
		q := receiptQueryParams{sizeLimit: uint64(maxPacketSize - bytes)}
		if i == 0 {
			q.firstIndex = firstBlockReceiptIndex
		}
		results, incomplete, err := blockReceiptsToNetwork(results, body, q)
		if err != nil {
			log.Error("Error in block receipts conversion", "hash", hash, "err", err)
			break
		}
		if results == nil {
			// This case triggers when the first receipt of the block receipts list doesn't
			// fit. We don't append anything to the response here and consider it finished.
			break
		}
		receipts.AppendRaw(results)
		bytes += len(results)
		if incomplete {
			return receipts, true
		}
	}
	return receipts, false
}

func handleBlockHeaders(backend Backend, msg Decoder, peer *Peer) error {
	// XDPOS2 responses arrive without the RequestId wrapper — bare RLP list
	// of types.Header. The matching request's id was stashed on the peer
	// by RequestHeadersByXxx for single-flight retrieval here. The legacy
	// peer may return a batch even when we requested 1, so tracker size
	// matching can't be applied (the tracker enforces response.Size <=
	// request.Size). We still want the dispatch so the request sink is
	// fulfilled and the request is removed from the dispatcher's pending
	// map — invoke Fulfil with the requested size so the check passes,
	// then truncate headers to the requested amount before dispatch.
	if peer.Version() == XDPOS2 {
		var headers []*types.Header
		if err := msg.Decode(&headers); err != nil {
			log.Info("XDPOS2 BlockHeaders decode failed", "peer", peer.ID()[:8], "err", err)
			return err
		}
		log.Info("XDPOS2 BlockHeaders received (pre-req)", "peer", peer.ID()[:8], "count", len(headers))

		// Route 1: push to peer.XDCHeaderResp where xdcSyncer waits.
		// Non-blocking — if reader isn't ready, drop.
		select {
		case peer.XDCHeaderResp <- headers:
		default:
		}

		// Route 2 (#807 Phase 9): also bridge to the standard dispatcher
		// so upstream code paths (downloader.skeleton.Sync, etc.) that
		// register a sink channel via RequestHeadersBy* receive the
		// response. Without this bridge, the downloader's request times
		// out at 60s because XDPOS2 wire has no RequestId for the
		// dispatcher to match — we fake the match using the last
		// header-request id stashed on the peer.
		//
		// The dispatcher's tracker.Fulfil expects response.Size <=
		// request.Size; XDC peers may return more headers than
		// requested (legacy quirk), so truncate before dispatch.
		// Pop the OLDEST in-flight header request from the per-peer
		// FIFO. XDPOS2 wire has no RequestId on responses so the
		// bridge can only match by arrival order — peers serve
		// requests in roughly FIFO order. Phase 9.3 / Phase 10 also
		// handles the XDPOS2 server-side quirk where the peer anchors
		// its response at a different block than what was requested.
		req := peer.popLegacyHeaderReq()
		log.Info("XDPOS2 BlockHeaders received",
			"peer", peer.ID()[:8],
			"count", len(headers),
			"req_id", req.id,
			"req_anchor_num", req.anchorNumber,
			"req_anchor_hash", req.anchorHash.Hex()[:18],
			"req_count", req.count)
		if req.id != 0 {
			// A.9: revert A.8's Fulfil-on-empty for XDPOS2 exit paths.
			// A.8's tracker.Fulfil with Size=0 on empty responses unblocked
			// tracker accumulation but caused a worse failure: the downloader
			// fired unlimited new requests which XDPOS2 peers throttled by
			// returning empty batches continuously. Pre-A.8's let-empty-time-out
			// behavior naturally rate-limited request issuance.
			// Only the normal-dispatch Fulfil (Size=len(truncated)) is retained.

			// #807 Phase 14.1: an XDPOS2 peer can return count=0 when
			// it has no headers at the requested anchor. Let the request
			// time out at 60s to rate-limit re-issuance.
			if len(headers) == 0 {
				log.Debug("XDPOS2 bridge: empty response",
					"peer", peer.ID()[:8], "want_num", req.anchorNumber)
				return nil
			}
			// Phase 10: re-anchor. Find the index in `headers` where
			// the requested anchor sits. XDPOS2 peers may return more
			// headers than requested AND start the response at a
			// different block than the request's origin field. The
			// skeleton enforces `headers[0].Number == req.head`
			// strictly, so we slice off the leading offset.
			anchorIdx := -1
			if req.anchorHash != (common.Hash{}) {
				for i, h := range headers {
					if h.Hash() == req.anchorHash {
						anchorIdx = i
						break
					}
				}
			} else if req.anchorNumber != 0 {
				for i, h := range headers {
					if h.Number.Uint64() == req.anchorNumber {
						anchorIdx = i
						break
					}
				}
			}
			if anchorIdx < 0 {
				// Anchor not in this batch; can't safely deliver.
				// Let the dispatcher request time out and retry through
				// a different peer. (A.9: no Fulfil here — letting it
				// time out naturally rate-limits re-issuance.)
				log.Debug("XDPOS2 bridge: anchor not in response",
					"peer", peer.ID()[:8], "want_num", req.anchorNumber,
					"have_first_num", headers[0].Number.Uint64(),
					"have_last_num", headers[len(headers)-1].Number.Uint64())
				return nil
			}
			// Slice [anchorIdx : anchorIdx+count] and truncate to want.
			tail := headers[anchorIdx:]
			truncated := tail
			if req.count > 0 && len(truncated) > req.count {
				truncated = truncated[:req.count]
			}
			// Fulfil the tracker before dispatching so the pending slot
			// is freed regardless of whether dispatchResponse succeeds.
			_ = peer.tracker.Fulfil(tracker.Response{
				ID:      req.id,
				MsgCode: BlockHeadersMsg,
				Size:    len(truncated),
			})
			delivered := BlockHeadersRequest(truncated)
			res := &Response{
				id:   req.id,
				code: BlockHeadersMsg,
				Res:  &delivered,
			}
			// Best-effort: run dispatchResponse in a goroutine so a
			// hung sink doesn't block this handler. Errors mean the
			// pending request doesn't exist (already cancelled / no
			// active downloader request), which is fine.
			go func() {
				_ = peer.dispatchResponse(res, nil)
			}()
		}
		return nil
	}
	// A batch of headers arrived to one of our previous requests
	res := new(BlockHeadersPacket)
	if err := msg.Decode(res); err != nil {
		return err
	}
	tresp := tracker.Response{ID: res.RequestId, MsgCode: BlockHeadersMsg, Size: res.List.Len()}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("BlockHeaders: %w", err)
	}
	headers, err := res.List.Items()
	if err != nil {
		return fmt.Errorf("BlockHeaders: %w", err)
	}

	metadata := func() interface{} {
		hashes := make([]common.Hash, len(headers))
		for i, header := range headers {
			hashes[i] = header.Hash()
		}
		return hashes
	}
	return peer.dispatchResponse(&Response{
		id:   res.RequestId,
		code: BlockHeadersMsg,
		Res:  (*BlockHeadersRequest)(&headers),
	}, metadata)
}

func handleBlockBodies(backend Backend, msg Decoder, peer *Peer) error {
	// XDPOS2 legacy: bare list of bodies, no RequestId. The XDPOS2 wire shape
	// is `[body1, body2, ...]` — a single RLP list, no request-id wrapper.
	// Matches the in-flight request via stashed legacyBodyReqID (set in
	// peer.RequestBodies for XDPOS2). A.33: route through the dispatcher so
	// downloader fast-sync body fetcher (which waits on req.sink) receives
	// the response. The legacy xdcSyncer.fetchBodiesBatch consumer also goes
	// through the dispatcher now (it creates a local sink channel).
	if peer.Version() == XDPOS2 {
		var bodies []BlockBody
		if err := msg.Decode(&bodies); err != nil {
			log.Info("XDPOS2 BlockBodies decode failed", "peer", peer.ID()[:8], "err", err)
			return err
		}
		log.Info("XDPOS2 BlockBodies received", "peer", peer.ID()[:8], "count", len(bodies))
		// BlockBodiesResponse is []BlockBody — bodies already decoded, no
		// re-encoding needed. Just wrap and dispatch.
		reqID := peer.LastXDCBodyReqID()
		metadata := func() any { return hashBodyParts(bodies) }
		// Also keep XDCBodyResp as a non-blocking secondary channel for the
		// legacy xdcSyncer.fetchBodiesBatch consumer (until it migrates to
		// the dispatcher path).
		select {
		case peer.XDCBodyResp <- bodies:
		default:
		}
		return peer.dispatchResponse(&Response{
			id:   reqID,
			code: BlockBodiesMsg,
			Res:  (*BlockBodiesResponse)(&bodies),
		}, metadata)
	}
	// A batch of block bodies arrived to one of our previous requests
	res := new(BlockBodiesPacket)
	if err := msg.Decode(res); err != nil {
		return err
	}

	// Check against the request.
	length := res.List.Len()
	tresp := tracker.Response{ID: res.RequestId, MsgCode: BlockBodiesMsg, Size: length}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("BlockBodies: %w", err)
	}

	// Collect items and dispatch.
	items, err := res.List.Items()
	if err != nil {
		return fmt.Errorf("BlockBodies: %w", err)
	}
	metadata := func() any { return hashBodyParts(items) }
	return peer.dispatchResponse(&Response{
		id:   res.RequestId,
		code: BlockBodiesMsg,
		Res:  (*BlockBodiesResponse)(&items),
	}, metadata)
}

// BlockBodyHashes contains the lists of block body part roots for a list of block bodies.
type BlockBodyHashes struct {
	TransactionRoots []common.Hash
	WithdrawalRoots  []common.Hash
	UncleHashes      []common.Hash
}

func hashBodyParts(items []BlockBody) BlockBodyHashes {
	h := BlockBodyHashes{
		TransactionRoots: make([]common.Hash, len(items)),
		WithdrawalRoots:  make([]common.Hash, len(items)),
		UncleHashes:      make([]common.Hash, len(items)),
	}
	hasher := trie.NewStackTrie(nil)
	for i, body := range items {
		// txs
		txsList := newDerivableRawList(&body.Transactions, writeTxForHash)
		h.TransactionRoots[i] = types.DeriveSha(txsList, hasher)
		// uncles
		if body.Uncles.Len() == 0 {
			h.UncleHashes[i] = types.EmptyUncleHash
		} else {
			h.UncleHashes[i] = crypto.Keccak256Hash(body.Uncles.Bytes())
		}
		// withdrawals
		if body.Withdrawals != nil {
			wdlist := newDerivableRawList(body.Withdrawals, nil)
			h.WithdrawalRoots[i] = types.DeriveSha(wdlist, hasher)
		}
	}
	return h
}

// derivableRawList implements types.DerivableList for a serialized RLP list.
type derivableRawList struct {
	data    []byte
	offsets []uint32
	write   func([]byte, *bytes.Buffer)
}

func newDerivableRawList[T any](list *rlp.RawList[T], write func([]byte, *bytes.Buffer)) *derivableRawList {
	dl := derivableRawList{data: list.Content(), write: write}
	if dl.write == nil {
		// default transform is identity
		dl.write = func(b []byte, buf *bytes.Buffer) { buf.Write(b) }
	}
	// Assert to ensure 32-bit offsets are valid. This can never trigger
	// unless a block body component or p2p receipt list is larger than 4GB.
	if uint(len(dl.data)) > math.MaxUint32 {
		panic("list data too big for derivableRawList")
	}
	it := list.ContentIterator()
	dl.offsets = make([]uint32, list.Len())
	for i := 0; it.Next(); i++ {
		dl.offsets[i] = uint32(it.Offset())
	}
	return &dl
}

// Len returns the number of items in the list.
func (dl *derivableRawList) Len() int {
	return len(dl.offsets)
}

// EncodeIndex writes the i'th item to the buffer.
func (dl *derivableRawList) EncodeIndex(i int, buf *bytes.Buffer) {
	start := dl.offsets[i]
	end := uint32(len(dl.data))
	if i != len(dl.offsets)-1 {
		end = dl.offsets[i+1]
	}
	dl.write(dl.data[start:end], buf)
}

// writeTxForHash changes a transaction in 'network encoding' into the format used for
// the transactions MPT.
func writeTxForHash(tx []byte, buf *bytes.Buffer) {
	k, content, _, _ := rlp.Split(tx)
	if k == rlp.List {
		buf.Write(tx) // legacy tx
	} else {
		buf.Write(content) // typed tx
	}
}

func handleReceipts69(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of receipts arrived to one of our previous requests
	res := new(ReceiptsPacket69)
	if err := msg.Decode(res); err != nil {
		return err
	}

	tresp := tracker.Response{ID: res.RequestId, MsgCode: ReceiptsMsg, Size: res.List.Len()}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("Receipts: %w", err)
	}

	receiptLists, err := res.List.Items()
	if err != nil {
		return fmt.Errorf("Receipts: %w", err)
	}

	return dispatchReceipts(res.RequestId, receiptLists, peer)
}

func handleReceipts70(backend Backend, msg Decoder, peer *Peer) error {
	res := new(ReceiptsPacket70)
	if err := msg.Decode(res); err != nil {
		return err
	}

	tresp := tracker.Response{ID: res.RequestId, MsgCode: ReceiptsMsg, Size: res.List.Len()}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("Receipts: %w", err)
	}
	receiptLists, err := res.List.Items()
	if err != nil {
		return fmt.Errorf("Receipts: %w", err)
	}

	err = peer.bufferReceipts(res.RequestId, receiptLists, res.LastBlockIncomplete, backend)
	if err != nil {
		return err
	}
	if res.LastBlockIncomplete {
		// Request the remaining receipts from the same peer.
		return peer.requestPartialReceipts(res.RequestId)
	}
	if complete := peer.flushReceipts(res.RequestId); complete != nil {
		receiptLists = complete
	}

	return dispatchReceipts(res.RequestId, receiptLists, peer)
}

// A.33+A.60.4: handleReceiptsXDPOS2 decodes the bare-list wire shape
// receipts response sent by production XDPOS2 peers. No RequestId wrapper —
// the in-flight request is matched via stashed legacyReceiptsReqID, set in
// peer.RequestReceipts XDPOS2 branch.
//
// A.60.4 (refs #857): the inner per-receipt shape XDPoSChain v2.7.0 sends is
// the EIP-2718-aware "consensus" shape — `[postStateOrStatus,
// cumulativeGasUsed, bloom, [logs...]]` — exactly what `types.Receipt`'s
// own RLP encoder produces. The earlier `ReceiptList` decoder assumed the
// eth/69+ compressed shape `[txType, postStateOrStatus, gasUsed, [logs...]]`,
// which has different field count, different field order, and no Bloom.
// Decoding the legacy bytes through that path produced corrupt Receipts
// whose DeriveSha never matched the block header's ReceiptHash, so the
// downloader rejected every receipt response with `errInvalidReceipt` and
// disconnected the peer.
//
// Decoding directly into `types.Receipts` (which uses the same consensus
// RLP shape as XDPoSChain) gives byte-equivalent receipts; `DeriveSha`
// over `types.Receipts` then produces the receipt-trie root the block
// header expects.
func handleReceiptsXDPOS2(backend Backend, msg Decoder, peer *Peer) error {
	var raw []types.Receipts
	if err := msg.Decode(&raw); err != nil {
		log.Info("XDPOS2 Receipts decode failed", "peer", peer.ID()[:8], "err", err)
		return err
	}
	log.Info("XDPOS2 Receipts received", "peer", peer.ID()[:8], "count", len(raw))
	reqID := peer.legacyReceiptsReqID.Load()
	return dispatchLegacyReceipts(reqID, raw, peer)
}

// dispatchLegacyReceipts submits a legacy-shape (XDPOS2 / eth/63) receipt
// response to the dispatcher. Refs #857 A.60.4.
//
// The hash metadata is computed with `types.DeriveSha` directly over
// `types.Receipts`, which uses the consensus receiptRLP shape — matching
// what the block producer used to build header.ReceiptHash.
//
// The dispatcher's ReceiptsRLPResponse expects each block's encoded
// receipt list as a single `rlp.RawValue`. We re-encode each
// `types.Receipts` back to bytes; this is a cheap roundtrip because
// types.Receipt.EncodeRLP produces the same consensus shape we just
// decoded.
func dispatchLegacyReceipts(requestId uint64, receiptLists []types.Receipts, peer *Peer) error {
	metadata := func() interface{} {
		hasher := trie.NewStackTrie(nil)
		hashes := make([]common.Hash, len(receiptLists))
		for i := range receiptLists {
			hashes[i] = types.DeriveSha(receiptLists[i], hasher)
		}
		return hashes
	}

	enc := make(ReceiptsRLPResponse, len(receiptLists))
	for i := range receiptLists {
		buf, err := rlp.EncodeToBytes(receiptLists[i])
		if err != nil {
			return fmt.Errorf("Receipts: invalid list %d: %v", i, err)
		}
		enc[i] = buf
	}
	return peer.dispatchResponse(&Response{
		id:   requestId,
		code: ReceiptsMsg,
		Res:  &enc,
	}, metadata)
}

// dispatchReceipts submits a receipt response to the dispatcher.
func dispatchReceipts(requestId uint64, receiptLists []*ReceiptList, peer *Peer) error {
	metadata := func() interface{} {
		hasher := trie.NewStackTrie(nil)
		hashes := make([]common.Hash, len(receiptLists))
		for i := range receiptLists {
			hashes[i] = types.DeriveSha(receiptLists[i].Derivable(), hasher)
		}
		return hashes
	}

	var enc ReceiptsRLPResponse
	for i := range receiptLists {
		encReceipts, err := receiptLists[i].EncodeForStorage()
		if err != nil {
			return fmt.Errorf("Receipts: invalid list %d: %v", i, err)
		}
		enc = append(enc, encReceipts)
	}
	return peer.dispatchResponse(&Response{
		id:   requestId,
		code: ReceiptsMsg,
		Res:  &enc,
	}, metadata)
}

func handleNewPooledTransactionHashes(backend Backend, msg Decoder, peer *Peer) error {
	// New transaction announcement arrived, make sure we have
	// a valid and fresh chain to handle them
	if !backend.AcceptTxs() {
		return nil
	}
	ann := new(NewPooledTransactionHashesPacket71)
	if err := msg.Decode(ann); err != nil {
		return err
	}
	if len(ann.Hashes) != len(ann.Types) || len(ann.Hashes) != len(ann.Sizes) {
		return fmt.Errorf("NewPooledTransactionHashes: invalid len of fields in %v %v %v", len(ann.Hashes), len(ann.Types), len(ann.Sizes))
	}
	// Schedule all the unknown hashes for retrieval
	for _, hash := range ann.Hashes {
		peer.MarkTransaction(hash)
	}
	return backend.Handle(peer, ann)
}

func handleNewPooledTransactionHashes72(backend Backend, msg Decoder, peer *Peer) error {
	// New transaction announcement arrived, make sure we have
	// a valid and fresh chain to handle them
	if !backend.AcceptTxs() {
		return nil
	}
	ann := new(NewPooledTransactionHashesPacket72)
	if err := msg.Decode(ann); err != nil {
		return err
	}
	if len(ann.Hashes) != len(ann.Types) || len(ann.Hashes) != len(ann.Sizes) {
		return fmt.Errorf("NewPooledTransactionHashes: invalid len of fields in %v %v %v", len(ann.Hashes), len(ann.Types), len(ann.Sizes))
	}
	// Schedule all the unknown hashes for retrieval
	for _, hash := range ann.Hashes {
		peer.MarkTransaction(hash)
	}
	return backend.Handle(peer, ann)
}

func handleGetPooledTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the pooled transactions retrieval message
	var query GetPooledTransactionsPacket
	if err := msg.Decode(&query); err != nil {
		return err
	}
	hashes, txs := answerGetPooledTransactions(backend, query.GetPooledTransactionsRequest, peer.version)
	return peer.ReplyPooledTransactionsRLP(query.RequestId, hashes, txs)
}

func answerGetPooledTransactions(backend Backend, query GetPooledTransactionsRequest, version uint) ([]common.Hash, []rlp.RawValue) {
	// Gather transactions until the fetch or network limits is reached
	var (
		bytes  int
		hashes []common.Hash
		txs    []rlp.RawValue
	)
	for _, hash := range query {
		if bytes >= softResponseLimit {
			break
		}
		// Retrieve the requested transaction, skipping if unknown to us
		encoded := backend.TxPool().GetRLP(hash, version)
		if len(encoded) == 0 {
			continue
		}
		hashes = append(hashes, hash)
		txs = append(txs, encoded)
		bytes += len(encoded)
	}
	return hashes, txs
}

func handleTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Transactions arrived, make sure we have a valid and fresh chain to handle them
	if !backend.AcceptTxs() {
		return nil
	}
	// Transactions can be processed, parse all of them and deliver to the pool
	var txs TransactionsPacket
	if err := msg.Decode(&txs); err != nil {
		return err
	}
	if txs.Len() > maxTransactionAnnouncements {
		return fmt.Errorf("too many transactions")
	}
	return backend.Handle(peer, &txs)
}

func handlePooledTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Transactions arrived, make sure we have a valid and fresh chain to handle them
	if !backend.AcceptTxs() {
		return nil
	}

	// Check against request and decode.
	var resp PooledTransactionsPacket
	if err := msg.Decode(&resp); err != nil {
		return err
	}
	tresp := tracker.Response{
		ID:      resp.RequestId,
		MsgCode: PooledTransactionsMsg,
		Size:    resp.List.Len(),
	}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("PooledTransactions: %w", err)
	}

	return backend.Handle(peer, &resp)
}

func handleBlockRangeUpdate(backend Backend, msg Decoder, peer *Peer) error {
	var update BlockRangeUpdatePacket
	if err := msg.Decode(&update); err != nil {
		return err
	}
	if err := update.Validate(); err != nil {
		return err
	}
	// We don't do anything with these messages for now, just store them on the peer.
	peer.lastRange.Store(&update)
	return nil
}

// handleGetNodeData handles a state trie node data retrieval request on XDPOS2
// (eth/63 shape — no RequestId wrapper). Restored from upstream Geth v1.10.13;
// HBSS-only: PBSS nodes will return empty data (accepted tradeoff per #845 A.1a).
func handleGetNodeData(backend Backend, msg Decoder, peer *Peer) error {
	var query GetNodeDataPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("decode GetNodeData: %w", err)
	}
	response := answerGetNodeDataQuery(backend, query, peer)
	return peer.ReplyNodeData(response)
}

// answerGetNodeDataQuery assembles the response to a state trie node query.
// Reads from the HBSS (hash-based) storage; PBSS nodes will serve empty responses.
func answerGetNodeDataQuery(backend Backend, query GetNodeDataPacket, peer *Peer) [][]byte {
	// Gather state data until the fetch or network limits is reached
	var (
		bytes int
		nodes [][]byte
	)
	// A.25: NodeReader(EmptyRootHash) bypasses the root-presence check and yields
	// a reader that consults cleans cache → dirties cache → on-disk legacy HBSS
	// storage — exactly the lookup chain the trie itself uses. Falling back to
	// bare ReadLegacyTrieNode (the previous behavior) misses nodes still alive
	// only in memory caches — the symptom that broke pivot-root serving (the
	// root is often the most recently-touched node and lives in dirties).
	// Refs #844 A.25.
	triedb := backend.Chain().TrieDB()
	reader, rerr := triedb.NodeReader(types.EmptyRootHash)
	diskDB := triedb.Disk()
	for lookups, hash := range query {
		if bytes >= softResponseLimit || len(nodes) >= maxNodeDataServe ||
			lookups >= 2*maxNodeDataServe {
			break
		}
		// Retrieve the requested state entry — try cache+disk via the reader
		// first so in-memory-only nodes (notably the pivot root) are served.
		var entry []byte
		if rerr == nil && reader != nil {
			entry, _ = reader.Node(common.Hash{}, nil, hash)
		}
		if len(entry) == 0 {
			// Fall back to bare on-disk HBSS lookup (preserves prior behavior).
			entry = rawdb.ReadLegacyTrieNode(diskDB, hash)
		}
		if len(entry) == 0 {
			// Fall back to contract code lookup (code is stored with prefix).
			entry = backend.Chain().ContractCodeWithPrefix(hash)
		}
		if len(entry) > 0 {
			nodes = append(nodes, entry)
			bytes += len(entry)
		}
	}
	return nodes
}

// handleNodeData handles a state trie node delivery on XDPOS2
// (eth/63 shape — no RequestId wrapper). Forwards the data to the backend.
//
// A.36: NodeData intentionally skips the dispatcher's pending/tracker map.
// The XDPOS2 wire carries no RequestId on either GetNodeData or NodeData, so
// peer.RequestNodeData (peer.go) uses bare p2p.Send and never calls
// p.tracker.Track. Consequently there is no synthetic tracker entry to Fulfil
// here, and attempting to call peer.tracker.Fulfil with an unknown ID would
// surface a spurious error. Outstanding-request accounting is owned by the
// downloader's per-peer stateIdle flag (cleared by SetNodeDataIdle on every
// termination path, incl. the non-blocking drop in Downloader.DeliverNodeData).
func handleNodeData(backend Backend, msg Decoder, peer *Peer) error {
	var res NodeDataPacket
	if err := msg.Decode(&res); err != nil {
		return fmt.Errorf("decode NodeData: %w", err)
	}
	return backend.Handle(peer, &res)
}

// XDPOS2 consensus message handlers (refs #740). Bodies are forwarded as
// raw RLP — the XDPoS engine handles decoding inside consensus/XDPoS so the
// wire layer stays consensus-agnostic.

// handleVoteMsg handles XDPoS V2 vote messages over the xdpos2/100 protocol.
func handleVoteMsg(backend Backend, msg Decoder, peer *Peer) error {
	// XDC sends Vote as raw RLP, decode as RawValue to preserve bytes
	var rawVote rlp.RawValue
	if err := msg.Decode(&rawVote); err != nil {
		return fmt.Errorf("failed to decode VoteMsg: %v", err)
	}
	return backend.Handle(peer, &VotePacket{Vote: rawVote})
}

// handleTimeoutMsg handles XDPoS V2 timeout messages over the xdpos2/100 protocol.
func handleTimeoutMsg(backend Backend, msg Decoder, peer *Peer) error {
	// XDC sends Timeout as raw RLP
	var rawTimeout rlp.RawValue
	if err := msg.Decode(&rawTimeout); err != nil {
		return fmt.Errorf("failed to decode TimeoutMsg: %v", err)
	}
	return backend.Handle(peer, &TimeoutPacket{Timeout: rawTimeout})
}

// handleSyncInfoMsg handles XDPoS V2 sync-info messages over the xdpos2/100 protocol.
func handleSyncInfoMsg(backend Backend, msg Decoder, peer *Peer) error {
	// XDC sends SyncInfo as raw RLP
	var rawSyncInfo rlp.RawValue
	if err := msg.Decode(&rawSyncInfo); err != nil {
		return fmt.Errorf("failed to decode SyncInfoMsg: %v", err)
	}
	return backend.Handle(peer, &SyncInfoPacket{SyncInfo: rawSyncInfo})
}

func handleGetCells(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the cell retrieval message
	var query GetCellsRequestPacket
	if err := msg.Decode(&query); err != nil {
		return err
	}
	hashes, cells, custody := answerGetCells(backend, query.GetCellsRequest)
	return peer.ReplyCells(query.RequestId, hashes, cells, custody)
}

func answerGetCells(backend Backend, query GetCellsRequest) ([]common.Hash, [][]kzg4844.Cell, types.CustodyBitmap) {
	var (
		cellCounts int
		hashes     []common.Hash
		cells      [][]kzg4844.Cell
	)
	maxCells := softResponseLimit / 2048
	for _, hash := range query.Hashes {
		if cellCounts >= maxCells {
			break
		}
		// Look up the blob versioned hashes for this transaction
		vhashes := backend.BlobPool().GetBlobHashes(hash)
		if len(vhashes) == 0 {
			continue
		}
		blobCells, _, _ := backend.BlobPool().GetBlobCells(vhashes, query.Mask)

		// Flatten per-blob cells into a single slice. If any blob has a nil
		// entry (unavailable cell), skip the entire transaction.
		var flat []kzg4844.Cell
		skip := false
		for _, bc := range blobCells {
			if bc == nil {
				skip = true
				break
			}
			for _, c := range bc {
				if c == nil {
					skip = true
					break
				}
				flat = append(flat, *c)
			}
			if skip {
				break
			}
		}
		if skip || len(flat) == 0 {
			continue
		}
		hashes = append(hashes, hash)
		cells = append(cells, flat)
		cellCounts += len(flat)
	}
	return hashes, cells, query.Mask
}

func handleCells(backend Backend, msg Decoder, peer *Peer) error {
	var cellsResponse CellsPacket
	if err := msg.Decode(&cellsResponse); err != nil {
		return err
	}
	tresp := tracker.Response{
		ID:      cellsResponse.RequestId,
		MsgCode: CellsMsg,
		Size:    cellsResponse.CellsResponse.Cells.Len(),
	}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("Cells: %w", err)
	}
	return backend.Handle(peer, &cellsResponse.CellsResponse)
}

// handleGetBlockAccessLists serves a GetBlockAccessLists request.
func handleGetBlockAccessLists(backend Backend, msg Decoder, peer *Peer) error {
	var query GetBlockAccessListsPacket
	if err := msg.Decode(&query); err != nil {
		return err
	}
	response := serviceGetBlockAccessListsQuery(backend.Chain(), query.GetBlockAccessListsRequest)
	return peer.ReplyBlockAccessLists(query.RequestId, response)
}

// serviceGetBlockAccessListsQuery assembles the response to a BAL query.
// Unavailable BALs are returned as empty list entries.
func serviceGetBlockAccessListsQuery(chain *core.BlockChain, query GetBlockAccessListsRequest) rlp.RawList[rlp.RawValue] {
	var (
		bytes int
		bals  rlp.RawList[rlp.RawValue]
	)
	for _, hash := range query {
		if bytes >= softResponseLimit || bals.Len() >= maxBALsServe {
			break
		}
		data := chain.GetAccessListRLP(hash)
		if len(data) == 0 {
			// The signal for missing BAL is the empty string, because
			// an empty list is also a valid BAL.
			bals.AppendRaw(rlp.EmptyString)
			continue
		}
		bals.AppendRaw(data)
		bytes += len(data)
	}
	return bals
}

// handleBlockAccessLists processes an incoming BlockAccessLists response,
// validates it against the request tracker, and dispatches it to the waiting caller.
func handleBlockAccessLists(backend Backend, msg Decoder, peer *Peer) error {
	res := new(BlockAccessListPacket)
	if err := msg.Decode(res); err != nil {
		return err
	}
	tresp := tracker.Response{ID: res.RequestId, MsgCode: BlockAccessListsMsg, Size: res.List.Len()}
	if err := peer.tracker.Fulfil(tresp); err != nil {
		return fmt.Errorf("BlockAccessLists: %w", err)
	}
	bals, err := res.List.Items()
	if err != nil {
		return fmt.Errorf("BlockAccessLists: %w", err)
	}

	metadata := func() interface{} {
		hashes := make([]common.Hash, len(bals))
		for i := range bals {
			// Unavailable BALs (signaled by the empty string) are marked
			// with the zero hash
			if bytes.Equal(bals[i], rlp.EmptyString) {
				continue
			}
			hashes[i] = crypto.Keccak256Hash(bals[i])
		}
		return hashes
	}

	return peer.dispatchResponse(&Response{
		id:   res.RequestId,
		code: BlockAccessListsMsg,
		Res:  (*BlockAccessListResponse)(&bals),
	}, metadata)

}
