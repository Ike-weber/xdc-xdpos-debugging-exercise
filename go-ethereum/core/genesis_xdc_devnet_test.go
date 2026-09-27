// Copyright 2024 The go-ethereum Authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

// TestXDCDevnetGenesisHash asserts that the embedded XDC devnet (chain ID 5551)
// genesis reproduces the canonical XDPoSChain dev-upgrade genesis hash exactly.
// This is the HARD GATE for the devnet wiring.
func TestXDCDevnetGenesisHash(t *testing.T) {
	const want = "0xb8be003946a9c2688e9f1e255a5567c3d144293ccde2ffb38452a5840081b402"

	g := DefaultXDCDevnetGenesisBlock()
	got := g.ToBlock().Hash()
	if got != common.HexToHash(want) {
		h := g.ToBlock().Header()
		t.Logf("genesis header: time=0x%x gasLimit=0x%x diff=%v nonce=%d coinbase=%s extraLen=%d allocLen=%d root=%s",
			h.Time, h.GasLimit, h.Difficulty, g.Nonce, h.Coinbase.Hex(), len(h.Extra), len(g.Alloc), h.Root.Hex())
		t.Fatalf("XDC devnet genesis hash mismatch:\n have %s\n want %s", got.Hex(), want)
	}

	if g.Config == nil || g.Config.ChainID == nil || g.Config.ChainID.Uint64() != 5551 {
		t.Fatalf("XDC devnet genesis chainId mismatch: have %v want 5551", g.Config.ChainID)
	}

	if params.XDCDevnetGenesisHash != common.HexToHash(want) {
		t.Fatalf("params.XDCDevnetGenesisHash mismatch:\n have %s\n want %s",
			params.XDCDevnetGenesisHash.Hex(), want)
	}

	// Regression guard for the genesis baseFee gate: the running node commits with
	// the hardcoded XDCDevnetChainConfig (after SetupGenesisBlock config-selection),
	// which has London @0. Genesis must NOT carry a baseFee (EIP1559 @25000), else
	// the hash diverges from canonical (was 0x8720f24a before the fix).
	g2 := DefaultXDCDevnetGenesisBlock()
	g2.Config = params.XDCDevnetChainConfig
	if got2 := g2.ToBlock().Hash(); got2 != common.HexToHash(want) {
		t.Fatalf("XDC devnet genesis hash mismatch via XDCDevnetChainConfig (genesis baseFee gate?):\n have %s\n want %s", got2.Hex(), want)
	}
}
