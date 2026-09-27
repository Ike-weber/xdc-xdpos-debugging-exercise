// Copyright 2024 The go-ethereum Authors
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

package eip1559

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// TestFeeMarketActiveXDCSplitsLondonFromEIP1559 pins the gate that decides whether
// a header must carry a BaseFee.
//
// XDPoSChain splits the London EVM upgrade from the EIP-1559 fee market, so on an
// XDC chain IsLondon and "must carry BaseFee" are NOT the same question. A private
// net commonly has London active at block 0 and no Eip1559Block at all; a producer
// that gated BaseFee on IsLondon there would stamp a BaseFee that the rest of the
// net rejects, and every block it minted would be discarded. Refs ethOne#62.
func TestFeeMarketActiveXDCSplitsLondonFromEIP1559(t *testing.T) {
	// Private XDC net: London at 0, no EIP-1559 fork at all.
	xdcNoFeeMarket := &params.ChainConfig{
		ChainID:     big.NewInt(34093),
		LondonBlock: big.NewInt(0),
		XDPoS:       &params.XDPoSConfig{Period: 2, Epoch: 900},
	}
	for _, num := range []int64{0, 1, 2249, 2250, 100000} {
		if got := FeeMarketActive(xdcNoFeeMarket, big.NewInt(num)); got {
			t.Errorf("FeeMarketActive(no Eip1559Block, block %d) = true, want false", num)
		}
		// The divergence this guards against: IsLondon is true throughout.
		if !xdcNoFeeMarket.IsLondon(big.NewInt(num)) {
			t.Fatalf("test premise broken: IsLondon(block %d) should be true", num)
		}
	}

	// Private XDC net that opted in to the staged ladder at 2250.
	xdcStaged := &params.ChainConfig{
		ChainID:      big.NewInt(5151),
		LondonBlock:  big.NewInt(450),
		Eip1559Block: big.NewInt(2250),
		XDPoS:        &params.XDPoSConfig{Period: 2, Epoch: 900},
	}
	for _, tc := range []struct {
		num  int64
		want bool
	}{
		{0, false}, {450, false}, {2249, false}, {2250, true}, {5000, true},
	} {
		if got := FeeMarketActive(xdcStaged, big.NewInt(tc.num)); got != tc.want {
			t.Errorf("FeeMarketActive(Eip1559Block=2250, block %d) = %v, want %v", tc.num, got, tc.want)
		}
	}

	// Non-XDC chain: the fee market follows London, as upstream.
	upstream := &params.ChainConfig{ChainID: big.NewInt(1), LondonBlock: big.NewInt(100)}
	if FeeMarketActive(upstream, big.NewInt(99)) {
		t.Error("FeeMarketActive(upstream, pre-London) = true, want false")
	}
	if !FeeMarketActive(upstream, big.NewInt(100)) {
		t.Error("FeeMarketActive(upstream, at London) = false, want true")
	}
}
