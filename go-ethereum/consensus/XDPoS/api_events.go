// Copyright (c) 2024 XDC Network
// XDPoS WebSocket subscription RPC API (fix #87).
//
// Exposes the following eth_subscribe / xdpos_subscribe topics over WebSocket:
//   - "newQC"          → fired when a Quorum Certificate is created/received
//   - "newTC"          → fired when a Timeout Certificate is created/received
//   - "roundChange"    → fired when the consensus round advances
//   - "newEpoch"       → fired at the start of a new epoch
//
// Usage (JSON-RPC WebSocket):
//
//	{"jsonrpc":"2.0","id":1,"method":"xdpos_subscribe","params":["newQC"]}

package XDPoS

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/rpc"
)

// EventAPI is the RPC namespace "xdpos" for real-time consensus event subscriptions.
// Register it alongside the existing XDPoS API.
type EventAPI struct {
	feed *ConsensusFeed
}

// NewEventAPI creates a new XDPoS event subscription API.
func NewEventAPI(feed *ConsensusFeed) *EventAPI {
	return &EventAPI{feed: feed}
}

// SubscribeNewQC subscribes to new Quorum Certificate events.
// Returns a subscription that pushes *NewQCEvent payloads to the WebSocket client.
func (api *EventAPI) SubscribeNewQC(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, errors.New("xdpos_subscribe is only available over WebSocket")
	}
	sub := notifier.CreateSubscription()

	ch := make(chan *NewQCEvent, 64)
	eventSub := api.feed.SubscribeNewQC(ch)

	go func() {
		defer eventSub.Unsubscribe()
		for {
			select {
			case ev := <-ch:
				if err := notifier.Notify(sub.ID, ev); err != nil {
					return
				}
			case <-sub.Err():
				return
			}
		}
	}()

	return sub, nil
}

// SubscribeNewTC subscribes to new Timeout Certificate events.
func (api *EventAPI) SubscribeNewTC(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, errors.New("xdpos_subscribe is only available over WebSocket")
	}
	sub := notifier.CreateSubscription()

	ch := make(chan *NewTCEvent, 64)
	eventSub := api.feed.SubscribeNewTC(ch)

	go func() {
		defer eventSub.Unsubscribe()
		for {
			select {
			case ev := <-ch:
				if err := notifier.Notify(sub.ID, ev); err != nil {
					return
				}
			case <-sub.Err():
				return
			}
		}
	}()

	return sub, nil
}

// SubscribeRoundChange subscribes to round-change events.
func (api *EventAPI) SubscribeRoundChange(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, errors.New("xdpos_subscribe is only available over WebSocket")
	}
	sub := notifier.CreateSubscription()

	ch := make(chan *RoundChangeEvent, 64)
	eventSub := api.feed.SubscribeRoundChange(ch)

	go func() {
		defer eventSub.Unsubscribe()
		for {
			select {
			case ev := <-ch:
				if err := notifier.Notify(sub.ID, ev); err != nil {
					return
				}
			case <-sub.Err():
				return
			}
		}
	}()

	return sub, nil
}

// SubscribeNewEpoch subscribes to new-epoch events.
func (api *EventAPI) SubscribeNewEpoch(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, errors.New("xdpos_subscribe is only available over WebSocket")
	}
	sub := notifier.CreateSubscription()

	ch := make(chan *NewEpochEvent, 64)
	eventSub := api.feed.SubscribeNewEpoch(ch)

	go func() {
		defer eventSub.Unsubscribe()
		for {
			select {
			case ev := <-ch:
				if err := notifier.Notify(sub.ID, ev); err != nil {
					return
				}
			case <-sub.Err():
				return
			}
		}
	}()

	return sub, nil
}
