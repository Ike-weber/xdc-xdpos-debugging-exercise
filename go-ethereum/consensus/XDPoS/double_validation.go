// Copyright 2026 XDC Network
//
// V1 (Clique-style PoA) double-validation (M1/M2) support — ported to byte-exact
// parity from XDPoSChain consensus/XDPoS/engines/engine_v1/{engine,utils}.go.
//
// XDPoS V1 "double validation" pairs each block creator (M1) with an assigned
// validator (M2). A block past the first epoch is only valid once BOTH the M1
// (Extra seal) and the assigned M2 (header.Validator) have signed sigHash(header).
// The M2 assignment is a deterministic permutation of the checkpoint masternode
// set, derived from the per-masternode M2 indices stored in the checkpoint
// header.Validators field, plus a per-block `moveM2` rotation once TIPRandomize
// is active. See engine_v1/utils.go getM1M2 / getM1M2FromCheckpointHeader.
//
// This file provides the verify-side primitives (GetValidator → M2 map,
// RecoverValidator → recover M2 from header.Validator). The produce side (Seal
// sets header.Validator when self==M2; the appendM2HeaderHook co-sign for blocks
// whose M2 is this node) is wired separately. Refs #951 Defect 1c.

package XDPoS

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var (
	// errFailValidatorSignature mirrors consensus.ErrFailValidatorSignature (legacy):
	// header.Validator is present but not a recoverable 65-byte signature.
	errFailValidatorSignature = errors.New("xdpos v1: invalid M2 validator signature in header.Validator")
	// errNoValidatorSignature mirrors consensus.ErrNoValidatorSignature (legacy):
	// a block past the first epoch carries an empty header.Validator under full verify.
	errNoValidatorSignature = errors.New("xdpos v1: missing M2 validator signature (header.Validator empty past epoch 1)")
)

// decodeMasternodesFromHeaderExtra extracts the 20-byte masternode addresses packed
// into a V1 checkpoint header's Extra (between the 32-byte vanity prefix and the
// 65-byte seal suffix). Mirrors XDPoSChain decodeMasternodesFromHeaderExtra.
func decodeMasternodesFromHeaderExtra(h *types.Header) []common.Address {
	if h == nil || len(h.Extra) < extraVanity+extraSeal {
		return nil
	}
	raw := h.Extra[extraVanity : len(h.Extra)-extraSeal]
	n := len(raw) / common.AddressLength
	mns := make([]common.Address, n)
	for i := 0; i < n; i++ {
		copy(mns[i][:], raw[i*common.AddressLength:])
	}
	return mns
}

// getM1M2 maps each M1 (masternode/creator) to its assigned M2 (validator).
// Byte-exact port of XDPoSChain engine_v1/utils.go getM1M2:
//
//	moveM2 = 0, OR ((currentNum % Epoch) / N) % N once TIPRandomize is active.
//	m2Index = (validators[i] % N + moveM2) % N ; m1m2[masternodes[i]] = masternodes[m2Index]
//
// `validators` are the per-masternode M2 indices decoded from the checkpoint
// header.Validators (4-byte ASCII tokens via ExtractValidatorsFromBytes).
func getM1M2(masternodes []common.Address, validators []int64, currentHeader *types.Header, config *params.ChainConfig) (map[common.Address]common.Address, uint64, error) {
	m1m2 := map[common.Address]common.Address{}
	maxMNs := len(masternodes)
	moveM2 := uint64(0)
	if len(validators) < maxMNs {
		return nil, moveM2, errors.New("xdpos v1: len(m2) is less than len(m1)")
	}
	if maxMNs > 0 {
		if config.IsTIPRandomize(currentHeader.Number) {
			moveM2 = ((currentHeader.Number.Uint64() % config.XDPoS.Epoch) / uint64(maxMNs)) % uint64(maxMNs)
		}
		for i, m1 := range masternodes {
			m2Index := uint64(validators[i] % int64(maxMNs))
			m2Index = (m2Index + moveM2) % uint64(maxMNs)
			m1m2[m1] = masternodes[m2Index]
		}
	}
	return m1m2, moveM2, nil
}

// getM1M2FromCheckpointHeader builds the M1→M2 map from a checkpoint header.
// Byte-exact port of XDPoSChain engine_v1/utils.go getM1M2FromCheckpointHeader.
func getM1M2FromCheckpointHeader(checkpointHeader *types.Header, currentHeader *types.Header, config *params.ChainConfig) (map[common.Address]common.Address, error) {
	if checkpointHeader.Number.Uint64()%EpocBlockRandomize != 0 {
		return nil, errors.New("xdpos v1: this block is not a checkpoint block")
	}
	masternodes := decodeMasternodesFromHeaderExtra(checkpointHeader)
	validators := ExtractValidatorsFromBytes(checkpointHeader.Validators)
	m1m2, _, err := getM1M2(masternodes, validators, currentHeader, config)
	if err != nil {
		return map[common.Address]common.Address{}, err
	}
	return m1m2, nil
}
