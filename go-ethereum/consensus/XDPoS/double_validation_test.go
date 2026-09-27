// Copyright 2026 XDC Network
// Unit tests for the V1 double-validation M2 mapping (#951 Defect 1c).

package XDPoS

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// mn returns a deterministic test masternode address from a single byte.
func mn(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}

// TestGetM1M2_NoRotation verifies the M2 permutation when TIPRandomize is inactive
// (moveM2 == 0): m1m2[masternodes[i]] = masternodes[validators[i] % N].
func TestGetM1M2_NoRotation(t *testing.T) {
	saved := common.TIPRandomize
	common.TIPRandomize = big.NewInt(1_000_000_000) // far future ⇒ IsTIPRandomize=false
	defer func() { common.TIPRandomize = saved }()

	A, B, C, D := mn(1), mn(2), mn(3), mn(4)
	masternodes := []common.Address{A, B, C, D}
	validators := []int64{2, 0, 3, 1} // M2 indices per masternode
	cfg := &params.ChainConfig{XDPoS: &params.XDPoSConfig{Epoch: 900}}
	cur := &types.Header{Number: big.NewInt(905)}

	m, move, err := getM1M2(masternodes, validators, cur, cfg)
	if err != nil {
		t.Fatalf("getM1M2 error: %v", err)
	}
	if move != 0 {
		t.Fatalf("expected moveM2=0 (TIPRandomize off), got %d", move)
	}
	want := map[common.Address]common.Address{A: C, B: A, C: D, D: B}
	for m1, m2 := range want {
		if m[m1] != m2 {
			t.Errorf("m1m2[%x] = %x, want %x", m1, m[m1], m2)
		}
	}
}

// TestGetM1M2_WithRotation verifies the per-block moveM2 rotation when TIPRandomize
// is active: moveM2 = ((num % Epoch) / N) % N, then m2Index = (validators[i]%N + moveM2) % N.
func TestGetM1M2_WithRotation(t *testing.T) {
	saved := common.TIPRandomize
	common.TIPRandomize = big.NewInt(0) // active from genesis (devnet semantics)
	defer func() { common.TIPRandomize = saved }()

	A, B, C, D := mn(1), mn(2), mn(3), mn(4)
	masternodes := []common.Address{A, B, C, D}
	validators := []int64{2, 0, 3, 1}
	cfg := &params.ChainConfig{XDPoS: &params.XDPoSConfig{Epoch: 900}}
	// num=908 ⇒ num%900=8, 8/4=2, 2%4=2 ⇒ moveM2=2
	cur := &types.Header{Number: big.NewInt(908)}

	m, move, err := getM1M2(masternodes, validators, cur, cfg)
	if err != nil {
		t.Fatalf("getM1M2 error: %v", err)
	}
	if move != 2 {
		t.Fatalf("expected moveM2=2, got %d", move)
	}
	// A:(2+2)%4=0→A  B:(0+2)%4=2→C  C:(3+2)%4=1→B  D:(1+2)%4=3→D
	want := map[common.Address]common.Address{A: A, B: C, C: B, D: D}
	for m1, m2 := range want {
		if m[m1] != m2 {
			t.Errorf("m1m2[%x] = %x, want %x", m1, m[m1], m2)
		}
	}
}

// TestGetM1M2_ShortValidators verifies the guard when fewer M2 indices than masternodes.
func TestGetM1M2_ShortValidators(t *testing.T) {
	masternodes := []common.Address{mn(1), mn(2), mn(3)}
	validators := []int64{0, 1} // too few
	cfg := &params.ChainConfig{XDPoS: &params.XDPoSConfig{Epoch: 900}}
	cur := &types.Header{Number: big.NewInt(905)}
	if _, _, err := getM1M2(masternodes, validators, cur, cfg); err == nil {
		t.Fatal("expected error for len(validators) < len(masternodes), got nil")
	}
}
