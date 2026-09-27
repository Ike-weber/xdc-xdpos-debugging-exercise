// Copyright (c) 2024 XDC Network
// XDPoS consensus event bus (fix #87).
//
// This file provides a typed event feed for XDPoS consensus state changes.
// External packages (e.g. eth/filters, internal/ethapi) can subscribe to
// these feeds to expose them over WebSocket via eth_subscribe / xdpos_subscribe.

package XDPoS

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// ---- Event types -------------------------------------------------------

// NewQCEvent is fired when the node generates or receives a valid Quorum Certificate.
type NewQCEvent struct {
	QC *types.QuorumCert
}

// NewTCEvent is fired when the node generates or receives a valid Timeout Certificate.
type NewTCEvent struct {
	TC *types.TimeoutCert
}

// RoundChangeEvent is fired when the current consensus round advances.
type RoundChangeEvent struct {
	Round types.Round
}

// NewEpochEvent is fired at the start of a new epoch.
type NewEpochEvent struct {
	EpochNum   uint64
	Masternodes []common.Address
	Header     *types.Header
}

// ---- Event feeds --------------------------------------------------------

// ConsensusFeed holds all XDPoS consensus event feeds.
// It is embedded in the XDPoS engine and shared with subscribers.
type ConsensusFeed struct {
	NewQCFeed      event.Feed // sends *NewQCEvent
	NewTCFeed      event.Feed // sends *NewTCEvent
	RoundChangeFeed event.Feed // sends *RoundChangeEvent
	NewEpochFeed   event.Feed // sends *NewEpochEvent
}

// SubscribeNewQC returns a subscription that receives new QC events.
func (f *ConsensusFeed) SubscribeNewQC(ch chan<- *NewQCEvent) event.Subscription {
	return f.NewQCFeed.Subscribe(ch)
}

// SubscribeNewTC returns a subscription that receives new TC events.
func (f *ConsensusFeed) SubscribeNewTC(ch chan<- *NewTCEvent) event.Subscription {
	return f.NewTCFeed.Subscribe(ch)
}

// SubscribeRoundChange returns a subscription that receives round-change events.
func (f *ConsensusFeed) SubscribeRoundChange(ch chan<- *RoundChangeEvent) event.Subscription {
	return f.RoundChangeFeed.Subscribe(ch)
}

// SubscribeNewEpoch returns a subscription that receives new-epoch events.
func (f *ConsensusFeed) SubscribeNewEpoch(ch chan<- *NewEpochEvent) event.Subscription {
	return f.NewEpochFeed.Subscribe(ch)
}

// ---- Emit helpers -------------------------------------------------------

// EmitNewQC publishes a new QC to all subscribers.
func (f *ConsensusFeed) EmitNewQC(qc *types.QuorumCert) {
	f.NewQCFeed.Send(&NewQCEvent{QC: qc})
}

// EmitNewTC publishes a new TC to all subscribers.
func (f *ConsensusFeed) EmitNewTC(tc *types.TimeoutCert) {
	f.NewTCFeed.Send(&NewTCEvent{TC: tc})
}

// EmitRoundChange publishes a round-change event.
func (f *ConsensusFeed) EmitRoundChange(round types.Round) {
	f.RoundChangeFeed.Send(&RoundChangeEvent{Round: round})
}

// EmitNewEpoch publishes a new-epoch event.
func (f *ConsensusFeed) EmitNewEpoch(epochNum uint64, masternodes []common.Address, header *types.Header) {
	f.NewEpochFeed.Send(&NewEpochEvent{
		EpochNum:    epochNum,
		Masternodes: masternodes,
		Header:      header,
	})
}
