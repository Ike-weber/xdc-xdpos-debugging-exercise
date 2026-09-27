// Copyright 2026 The go-ethereum Authors
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

package eip1559

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// xdcApothemConfig returns a minimal Apothem-shaped chain config: XDPoS set,
// EIP-1559 fork at the canonical Apothem block.
func xdcApothemConfig() *params.ChainConfig {
	apothem := big.NewInt(71_550_000)
	return &params.ChainConfig{
		ChainID:      big.NewInt(51),
		LondonBlock:  apothem,
		Eip1559Block: apothem,
		XDPoS:        &params.XDPoSConfig{},
	}
}

// twelvePointFiveBillion is the canonical Apothem pre-fork base fee.
func twelvePointFiveBillion() *big.Int { return big.NewInt(12_500_000_000) }

// TestVerifyEIP1559Header_XDC_PreForkParentNilBaseFee covers the bug from #541:
// checkpoint sync delivers parent at fork-1 with BaseFee=nil; the fork header
// must be accepted as long as it carries a positive BaseFee.
func TestVerifyEIP1559Header_XDC_PreForkParentNilBaseFee(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_549_999),
		GasLimit: 420_000_000,
		GasUsed:  100,
		BaseFee:  nil, // sync-state nil
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_000),
		GasLimit: 420_000_000,
		GasUsed:  100,
		BaseFee:  twelvePointFiveBillion(),
	}
	if err := VerifyEIP1559Header(cfg, parent, header); err != nil {
		t.Fatalf("expected fork header to be accepted with nil pre-fork parent baseFee, got: %v", err)
	}
}

// TestVerifyEIP1559Header_XDC_SkipPathRejectsZeroBaseFee guards the L2 hardening:
// the skip must still reject a non-positive header.BaseFee.
func TestVerifyEIP1559Header_XDC_SkipPathRejectsZeroBaseFee(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_549_999),
		GasLimit: 420_000_000,
		BaseFee:  nil,
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_000),
		GasLimit: 420_000_000,
		BaseFee:  big.NewInt(0),
	}
	if err := VerifyEIP1559Header(cfg, parent, header); err == nil {
		t.Fatal("expected error for zero baseFee at fork boundary, got nil")
	}
}

// TestVerifyEIP1559Header_XDC_SkipDoesNotFireForNonXDCChain confirms the skip
// is gated on config.XDPoS — a non-XDC chain keeps standard validation.
func TestVerifyEIP1559Header_XDC_SkipDoesNotFireForNonXDCChain(t *testing.T) {
	cfg := xdcApothemConfig()
	cfg.XDPoS = nil
	parent := &types.Header{
		Number:   big.NewInt(71_549_999),
		GasLimit: 420_000_000,
		BaseFee:  nil,
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_000),
		GasLimit: 420_000_000,
		BaseFee:  twelvePointFiveBillion(),
	}
	// With XDPoS=nil and parent.BaseFee=nil, CalcBaseFee returns InitialBaseFee
	// (1B), which won't match 12.5B — header should be rejected.
	if err := VerifyEIP1559Header(cfg, parent, header); err == nil {
		t.Fatal("expected non-XDC chain to enforce baseFee match, got nil")
	}
}

// TestVerifyEIP1559Header_XDC_PostForkConstantBaseFee covers the XDPoSChain
// v2.6.8 invariant: post-fork XDC blocks all carry the same constant base fee
// (XDCBaseFee = 12.5 gwei), regardless of parent gas usage. A block at fork+2
// with parent.GasUsed well below the Ethereum target must still validate as
// long as header.BaseFee == 12.5 gwei. This is the regression test for the
// bug surfaced by issue #570 — block 71,550,002 was rejected because the
// previous code ran the standard Ethereum dynamic formula and computed a
// ~12.5% decrease, while the canonical Apothem chain keeps the fee fixed.
func TestVerifyEIP1559Header_XDC_PostForkConstantBaseFee(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_550_001),
		GasLimit: 420_000_000,
		GasUsed:  104_948, // matches the failing case from the live Apothem node
		BaseFee:  twelvePointFiveBillion(),
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_002),
		GasLimit: 420_000_000,
		BaseFee:  twelvePointFiveBillion(),
	}
	if err := VerifyEIP1559Header(cfg, parent, header); err != nil {
		t.Fatalf("XDC post-fork block with constant baseFee must validate, got: %v", err)
	}
}

// TestVerifyEIP1559Header_XDC_PostForkRejectsDynamicAdjustment confirms that a
// block carrying the *dynamic*-formula base fee (what the old broken code
// computed) is rejected. This is the inverse of the regression test above:
// it must not silently accept a non-canonical base fee just because the math
// would have produced it under the standard Ethereum formula.
func TestVerifyEIP1559Header_XDC_PostForkRejectsDynamicAdjustment(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_550_001),
		GasLimit: 420_000_000,
		GasUsed:  104_948,
		BaseFee:  twelvePointFiveBillion(),
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_002),
		GasLimit: 420_000_000,
		BaseFee:  big.NewInt(10_938_280_864), // what the old broken formula produced
	}
	if err := VerifyEIP1559Header(cfg, parent, header); err == nil {
		t.Fatal("XDC block with dynamic-adjusted baseFee must be rejected, got nil")
	}
}

// TestVerifyEIP1559Header_XDC_CanonicalForkBoundary covers the happy path:
// a fully-synced node crosses the fork with parent baseFee already populated.
func TestVerifyEIP1559Header_XDC_CanonicalForkBoundary(t *testing.T) {
	cfg := xdcApothemConfig()
	bf := twelvePointFiveBillion()
	parent := &types.Header{
		Number:   big.NewInt(71_549_999),
		GasLimit: 420_000_000,
		BaseFee:  bf,
	}
	header := &types.Header{
		Number:   big.NewInt(71_550_000),
		GasLimit: 420_000_000,
		BaseFee:  bf,
	}
	if err := VerifyEIP1559Header(cfg, parent, header); err != nil {
		t.Fatalf("canonical fork-boundary transition should validate, got: %v", err)
	}
}

// TestCalcBaseFee_XDC_PreForkParentNilBaseFee covers L3: pre-fork parent with
// nil BaseFee must return a non-nil value so callers never crash. Under the
// XDPoSChain v2.6.8 constant base-fee model, this is XDCBaseFee (12.5 gwei).
func TestCalcBaseFee_XDC_PreForkParentNilBaseFee(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_549_999),
		GasLimit: 420_000_000,
		BaseFee:  nil,
	}
	got := CalcBaseFee(cfg, parent)
	if got == nil {
		t.Fatal("CalcBaseFee returned nil for pre-fork parent with nil BaseFee — would panic in callers")
	}
	if got.Uint64() != params.XDCBaseFee {
		t.Fatalf("expected XDCBaseFee = %d, got %s", params.XDCBaseFee, got.String())
	}
}

// TestCalcBaseFee_XDC_PostForkParentNilBaseFee covers L1: post-fork parent
// with nil BaseFee must not panic.
func TestCalcBaseFee_XDC_PostForkParentNilBaseFee(t *testing.T) {
	cfg := xdcApothemConfig()
	parent := &types.Header{
		Number:   big.NewInt(71_550_500),
		GasLimit: 420_000_000,
		GasUsed:  210_000_000,
		BaseFee:  nil,
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CalcBaseFee panicked on post-fork nil parent BaseFee: %v", r)
		}
	}()
	got := CalcBaseFee(cfg, parent)
	if got == nil {
		t.Fatal("CalcBaseFee returned nil for post-fork parent with nil BaseFee")
	}
}
