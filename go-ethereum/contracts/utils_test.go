// Copyright (c) 2018 XDPoSChain
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package contracts

import "testing"

// TestSignTxNonceFloor covers the state-nonce floor under the sign-tx nonce.
//
// Motivation (netv12): the txpool's reset() can fail to open the head state and
// then keeps serving its previous statedb, freezing PoolNonce at an old height.
// With PoolNonce as the sole source, every subsequent signing tx is emitted at a
// nonce far below the confirmed chain nonce and can never be mined — the signing
// path jams permanently. Flooring at the confirmed chain nonce makes a stale pool
// degrade to a single "already known / nonce too low" instead.
//
// Mirrors the cases in erigon-xdc node/eth/xdc_sign_tx.go nextSignTxNonce.
func TestSignTxNonceFloor(t *testing.T) {
	for _, tt := range []struct {
		name       string
		poolNonce  uint64
		stateNonce uint64
		stateKnown bool
		want       uint64
	}{
		{
			// Healthy steady state: the pool has in-flight sign txs the chain has
			// not mined yet, so the pool leads and must win. Using the state nonce
			// here would collide with the pending tx.
			name: "pool-ahead-of-state", poolNonce: 130, stateNonce: 129, stateKnown: true, want: 130,
		},
		{
			// The netv12 failure: pool frozen on block 1799's state at nonce 113
			// while the chain has advanced to 629. The chain nonce must win, or
			// every emitted tx is unmineable.
			name: "state-ahead-of-pool-frozen-pool", poolNonce: 113, stateNonce: 629, stateKnown: true, want: 629,
		},
		{
			// No head state available (the very condition that freezes the pool):
			// there is no floor to apply, so fall back to the pool nonce rather
			// than emitting nonce 0 or failing the signing path.
			name: "state-unavailable", poolNonce: 113, stateNonce: 0, stateKnown: false, want: 113,
		},
		{
			// A stale state read must never drag the nonce backwards.
			name: "state-unavailable-but-nonzero-value-ignored", poolNonce: 42, stateNonce: 999, stateKnown: false, want: 42,
		},
		{
			name: "equal", poolNonce: 7, stateNonce: 7, stateKnown: true, want: 7,
		},
		{
			name: "both-zero-fresh-account", poolNonce: 0, stateNonce: 0, stateKnown: true, want: 0,
		},
		{
			// The measured netv12 coinbase: on-chain nonce is already 1, so a
			// fresh node whose pool starts at 0 must not re-emit nonce 0.
			name: "onboarded-coinbase-first-fresh-tx", poolNonce: 0, stateNonce: 1, stateKnown: true, want: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := signTxNonce(tt.poolNonce, tt.stateNonce, tt.stateKnown)
			if got != tt.want {
				t.Fatalf("signTxNonce(%d, %d, %v) = %d, want %d",
					tt.poolNonce, tt.stateNonce, tt.stateKnown, got, tt.want)
			}
			// Invariant: the emitted nonce is never below the confirmed chain nonce.
			if tt.stateKnown && got < tt.stateNonce {
				t.Fatalf("emitted nonce %d is below the confirmed chain nonce %d", got, tt.stateNonce)
			}
			// Invariant: the emitted nonce is never below the pool nonce either,
			// so a healthy pool's in-flight txs are never overwritten.
			if got < tt.poolNonce {
				t.Fatalf("emitted nonce %d is below the pool nonce %d", got, tt.poolNonce)
			}
		})
	}
}
