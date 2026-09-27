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
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
)

// xdcLocalScheme is set at process start by the eth package once the chain
// (whose concrete TrieDB type lives outside this package) is constructed.
// Returns one of rawdb.HashScheme / rawdb.PathScheme / "" (unknown). Kept as
// a free variable rather than an interface assertion to avoid pulling core
// or triedb into eth/protocols/eth's import graph. Refs A.37.
var xdcLocalScheme atomic.Pointer[string]

// SetLocalStateScheme registers the local node's state scheme so the XDPOS2
// handshake can advertise it in the A.37 XDCCaps trailer. Called once at
// node startup from eth.NewHandler. Idempotent; concurrent-safe via atomic.
// Refs #844 (A.37).
func SetLocalStateScheme(scheme string) {
	s := scheme
	xdcLocalScheme.Store(&s)
}

// localXDCCaps builds the XDC capabilities trailer for outbound handshakes.
// Best-effort: any failure to read scheme or commit returns the zero-value
// (Unknown / all-zero prefix), which encodes as a benign trailer on the wire
// and is treated by remote A.37 peers exactly like a legacy peer. Refs A.37.
func localXDCCaps() XDCCaps {
	var caps XDCCaps
	if sp := xdcLocalScheme.Load(); sp != nil {
		switch *sp {
		case rawdb.HashScheme:
			caps.Scheme = XDCStateSchemeHBSS
		case rawdb.PathScheme:
			caps.Scheme = XDCStateSchemePBSS
		}
	}
	if vcs, ok := version.VCS(); ok && len(vcs.Commit) >= 16 {
		if raw, err := hex.DecodeString(vcs.Commit[:16]); err == nil && len(raw) == 8 {
			copy(caps.CommitPrefix[:], raw)
		}
	}
	return caps
}

const (
	// handshakeTimeout is the maximum allowed time for the `eth` handshake to
	// complete before dropping the connection.= as malicious.
	handshakeTimeout = 5 * time.Second
)

// estimateXDTDP returns the best available total-difficulty estimate for
// XDC. XDC is pre-merge and does not use TD meaningfully; the only proxy
// is the current block number. Callers must not rely on TD for consensus.
func estimateXDTDP(latest *types.Header) *big.Int {
	return new(big.Int).SetUint64(latest.Number.Uint64())
}

// Handshake executes the eth protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis blocks. XDPOS2 uses the XDC
// pre-merge wire shape (StatusPacket62, no ForkID). Other versions use the
// upstream StatusPacket shape.
func (p *Peer) Handshake(networkID uint64, chain forkid.Blockchain, rangeMsg BlockRangeUpdatePacket) error {
	if p.version == XDPOS2 {
		return p.handshake62(networkID, chain)
	}
	var (
		genesis    = chain.Genesis()
		latest     = chain.CurrentHeader()
		forkID     = forkid.NewID(chain.Config(), genesis, latest.Number.Uint64(), latest.Time)
		forkFilter = forkid.NewFilter(chain)
	)

	errc := make(chan error, 2)
	go func() {
		pkt := &StatusPacket{
			ProtocolVersion: uint32(p.version),
			NetworkID:       networkID,
			Genesis:         genesis.Hash(),
			ForkID:          forkID,
			EarliestBlock:   rangeMsg.EarliestBlock,
			LatestBlock:     rangeMsg.LatestBlock,
			LatestBlockHash: rangeMsg.LatestBlockHash,
		}
		errc <- p2p.Send(p.rw, StatusMsg, pkt)
	}()
	var status StatusPacket // safe to read after two values have been received from errc
	go func() {
		errc <- p.readStatus(networkID, &status, genesis.Hash(), forkFilter)
	}()

	return waitForHandshake(errc, p)
}

// handshake62 performs the XDC pre-merge handshake (XDPOS2). No ForkID —
// XDC's chain config evolution does not advertise via ForkID; peers
// negotiate forks via consensus rules instead.
func (p *Peer) handshake62(networkID uint64, chain forkid.Blockchain) error {
	var (
		genesis = chain.Genesis()
		latest  = chain.CurrentHeader()
	)
	td := estimateXDTDP(latest)
	// A.37 fix: localXDCCaps() is intentionally NOT sent on the wire.
	// See StatusPacket62 doc comment for why (legacy 5-field decoders
	// reject extra trailing elements). The caps helper is retained for
	// future post-handshake out-of-band signaling but is no-op today.
	_ = localXDCCaps
	errc := make(chan error, 2)
	go func() {
		pkt := &StatusPacket62{
			ProtocolVersion: uint32(p.version),
			NetworkID:       networkID,
			TD:              td,
			Head:            latest.Hash(),
			Genesis:         genesis.Hash(),
		}
		errc <- p2p.Send(p.rw, StatusMsg, pkt)
	}()
	var status StatusPacket62
	go func() {
		errc <- p.readStatus62(networkID, &status, genesis.Hash())
	}()
	return waitForHandshake(errc, p)
}

func (p *Peer) readStatus62(networkID uint64, status *StatusPacket62, genesis common.Hash) error {
	if err := p.readStatusMsg(status); err != nil {
		return err
	}
	if status.NetworkID != networkID {
		return fmt.Errorf("%w: %d (!= %d)", errNetworkIDMismatch, status.NetworkID, networkID)
	}
	if uint(status.ProtocolVersion) != p.version {
		return fmt.Errorf("%w: %d (!= %d)", errProtocolVersionMismatch, status.ProtocolVersion, p.version)
	}
	if status.Genesis != genesis {
		return fmt.Errorf("%w: %x (!= %x)", errGenesisMismatch, status.Genesis, genesis)
	}
	// XDC handshake62 (XDPOS2) does NOT carry the peer's head block number
	// on the wire — only a head HASH (status.Head) and cumulative total
	// difficulty (status.TD). Real-world observation on canonical Apothem
	// (rpc.apothem.network @ block 82,266,800): peers report TD ≈
	// 338,603,500 — i.e. ~4 difficulty units per block. So status.TD.Uint64()
	// is NOT the peer's block number; it's the actual TD.
	//
	// We stash TD in the LatestBlock slot of BlockRangeUpdatePacket as a
	// peer-ranking metric: highest-TD peer = most caught-up peer. This is
	// correct for bestPeer() selection but DO NOT use this value as a real
	// block number elsewhere (e.g. don't subtract it from local head to
	// compute "blocks behind"; don't add fixed block-count windows to it).
	// To learn a peer's actual head block number, fetch the header by
	// status.Head hash via GetBlockHeaders. See issue #800 follow-up for
	// the proper field-semantics fix.
	var tdAsRank uint64
	if status.TD != nil {
		tdAsRank = status.TD.Uint64()
	}
	initRange := &BlockRangeUpdatePacket{
		EarliestBlock:   0, // pre-merge XDC peers serve from genesis
		LatestBlock:     tdAsRank,
		LatestBlockHash: status.Head,
	}
	// Skip Validate(): legacy XDC peers may send TD=0 on fresh chains;
	// Validate would reject that as zero-latest-hash. The downloader
	// tolerates a zero hash and just doesn't request from this peer
	// until the next BlockRangeUpdate.
	if status.Head != (common.Hash{}) && tdAsRank > 0 {
		p.lastRange.Store(initRange)
	}
	// A.37 fix: no Caps field on the wire (see StatusPacket62 doc). The
	// downloader's remoteSchemePbss / remoteLegacy flags stay false; the
	// NodeData gate is driven entirely by A.40's reactive PBSS blacklist
	// and the empty-response counters.
	// No ForkID check for XDPOS2.
	return nil
}

func (p *Peer) readStatus(networkID uint64, status *StatusPacket, genesis common.Hash, forkFilter forkid.Filter) error {
	if err := p.readStatusMsg(status); err != nil {
		return err
	}
	if status.NetworkID != networkID {
		return fmt.Errorf("%w: %d (!= %d)", errNetworkIDMismatch, status.NetworkID, networkID)
	}
	if uint(status.ProtocolVersion) != p.version {
		return fmt.Errorf("%w: %d (!= %d)", errProtocolVersionMismatch, status.ProtocolVersion, p.version)
	}
	if status.Genesis != genesis {
		return fmt.Errorf("%w: %x (!= %x)", errGenesisMismatch, status.Genesis, genesis)
	}
	if err := forkFilter(status.ForkID); err != nil {
		return fmt.Errorf("%w: %v", errForkIDRejected, err)
	}
	// Handle initial block range.
	initRange := &BlockRangeUpdatePacket{
		EarliestBlock:   status.EarliestBlock,
		LatestBlock:     status.LatestBlock,
		LatestBlockHash: status.LatestBlockHash,
	}
	if err := initRange.Validate(); err != nil {
		return fmt.Errorf("%w: %v", errInvalidBlockRange, err)
	}
	p.lastRange.Store(initRange)
	return nil
}

// readStatusMsg reads the first message on the connection.
func (p *Peer) readStatusMsg(dst any) error {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != StatusMsg {
		return fmt.Errorf("%w: first msg has code %x (!= %x)", errNoStatusMsg, msg.Code, StatusMsg)
	}
	if msg.Size > maxMessageSize {
		return fmt.Errorf("%w: %v > %v", errMsgTooLarge, msg.Size, maxMessageSize)
	}
	if err := msg.Decode(dst); err != nil {
		return err
	}
	return nil
}

func waitForHandshake(errc <-chan error, p *Peer) error {
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for range 2 {
		select {
		case err := <-errc:
			if err != nil {
				markError(p, err)
				return err
			}
		case <-timeout.C:
			markError(p, p2p.DiscReadTimeout)
			return p2p.DiscReadTimeout
		}
	}
	return nil
}

// markError registers the error with the corresponding metric.
func markError(p *Peer, err error) {
	if !metrics.Enabled() {
		return
	}
	m := meters.get(p.Inbound())
	switch {
	case errors.Is(err, errNetworkIDMismatch):
		m.networkIDMismatch.Mark(1)
	case errors.Is(err, errProtocolVersionMismatch):
		m.protocolVersionMismatch.Mark(1)
	case errors.Is(err, errGenesisMismatch):
		m.genesisMismatch.Mark(1)
	case errors.Is(err, errForkIDRejected):
		m.forkidRejected.Mark(1)
	case errors.Is(err, p2p.DiscReadTimeout):
		m.timeoutError.Mark(1)
	default:
		m.peerError.Mark(1)
	}
}

// Validate checks basic validity of a block range announcement.
func (p *BlockRangeUpdatePacket) Validate() error {
	if p.EarliestBlock > p.LatestBlock {
		return errors.New("earliest > latest")
	}
	if p.LatestBlockHash == (common.Hash{}) {
		return errors.New("zero latest hash")
	}
	return nil
}
