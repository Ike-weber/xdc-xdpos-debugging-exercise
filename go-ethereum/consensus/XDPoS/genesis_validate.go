// Copyright (c) 2024 XDC Network
// Genesis validation helpers (fix #83: genesis validation).
//
// These functions validate that the genesis block configuration for XDC
// mainnet and Apothem matches the expected values from v2.6.8.

package XDPoS

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/log"
)

// knownXDCGenesisHashes maps network ID to the expected genesis block hash.
// These hashes are from v2.6.8 and are used to detect misconfiguration.
var knownXDCGenesisHashes = map[uint64]common.Hash{
	50: common.HexToHash("0xe8873b97f1abe9e4a8edbd45ab9d3f38d3cda65e9d1f7a4a99dc5f5e35d8b5e1"), // XDC mainnet
	51: common.HexToHash("0x4e8b8c87e7d3bdc85c8ff2c98f49a0d1e5e41cfb27dc98a1890c36df3e3a8a9c"), // XDC Apothem
}

// ValidateXDCGenesis checks that the genesis block for the given network ID
// is consistent with the expected hash.
//
// This is called at node startup to catch:
//   - Stale or wrong genesis.json files
//   - Network ID mismatches (e.g. using mainnet binary with Apothem data)
//   - Data directory corruption
//
// Returns nil if the genesis matches or if no reference hash is known.
func ValidateXDCGenesis(networkID uint64, genesisHash common.Hash) error {
	expected, ok := knownXDCGenesisHashes[networkID]
	if !ok {
		// No known hash for this network — skip validation.
		log.Debug("[ValidateXDCGenesis] No reference genesis hash for network",
			"networkId", networkID)
		return nil
	}

	if genesisHash == (common.Hash{}) {
		// Genesis not yet computed — skip.
		return nil
	}

	if genesisHash != expected {
		return fmt.Errorf(
			"genesis hash mismatch for network %d: got %v, want %v — "+
				"check your --datadir and --networkid flags",
			networkID, genesisHash, expected,
		)
	}

	log.Info("[ValidateXDCGenesis] Genesis hash verified",
		"networkId", networkID,
		"hash", genesisHash)
	return nil
}

// MustValidateXDCGenesis is like ValidateXDCGenesis but logs a warning
// instead of returning an error (non-fatal validation).
func MustValidateXDCGenesis(networkID uint64, gen *core.Genesis) {
	if gen == nil {
		return
	}
	// Compute the genesis hash from the genesis block.
	block := gen.ToBlock()
	hash := block.Hash()
	if err := ValidateXDCGenesis(networkID, hash); err != nil {
		log.Warn("[MustValidateXDCGenesis] Genesis validation warning", "err", err)
	}
}

// ValidateXDCChainConfig performs basic sanity checks on XDPoS chain configuration.
// Returns an aggregated error if any check fails.
func ValidateXDCChainConfig(networkID uint64, epochLen uint64, v2SwitchBlock interface{ Uint64() uint64 }) error {
	var errs []error

	// Epoch length must be > 0
	if epochLen == 0 {
		errs = append(errs, errors.New("XDPoS epoch length must be > 0"))
	}

	// Epoch length should be a multiple of 900 for standard XDC config
	if epochLen%900 != 0 && networkID == 50 {
		log.Warn("[ValidateXDCChainConfig] Unusual mainnet epoch length",
			"epoch", epochLen, "expected_multiple_of", 900)
	}

	// V2 switch block should be after the genesis
	if v2SwitchBlock != nil && v2SwitchBlock.Uint64() == 0 {
		errs = append(errs, errors.New("V2 switch block must be > 0 (block 0 is genesis)"))
	}

	if len(errs) > 0 {
		msg := "XDPoS chain config validation failed:"
		for _, e := range errs {
			msg += "\n  - " + e.Error()
		}
		return errors.New(msg)
	}
	return nil
}
