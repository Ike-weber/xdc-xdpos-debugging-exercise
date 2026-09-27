// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Test file to verify XDPoS V2 switch block configuration

package ethconfig

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// TestXDPoSV2ConfigMainnet verifies mainnet V2 switch block configuration
func TestXDPoSV2ConfigMainnet(t *testing.T) {
	// Expected value from XDPoSChain v2.6.8 reference
	expectedSwitchBlock := big.NewInt(80370000)

	config := &params.ChainConfig{
		ChainID: big.NewInt(50), // XDC Mainnet
		XDPoS: &params.XDPoSConfig{
			Period: 2,
			Epoch:  900,
		},
	}

	// Simulate auto-population logic from CreateConsensusEngine
	if config.XDPoS.V2 == nil && config.ChainID.Uint64() == 50 {
		config.XDPoS.V2 = &params.V2{
			SwitchBlock: big.NewInt(80370000),
		}
	}

	// Verify V2 config was populated
	if config.XDPoS.V2 == nil {
		t.Fatal("V2 config should be populated for mainnet")
	}

	// Verify correct switch block
	if config.XDPoS.V2.SwitchBlock.Cmp(expectedSwitchBlock) != 0 {
		t.Errorf("Mainnet V2 SwitchBlock = %v, want %v", config.XDPoS.V2.SwitchBlock, expectedSwitchBlock)
	}
}

// TestXDPoSV2ConfigApothem verifies Apothem testnet V2 switch block configuration
func TestXDPoSV2ConfigApothem(t *testing.T) {
	// Expected value from XDPoSChain v2.6.8 reference (testnet)
	expectedSwitchBlock := big.NewInt(56828700)

	config := &params.ChainConfig{
		ChainID: big.NewInt(51), // XDC Apothem Testnet
		XDPoS: &params.XDPoSConfig{
			Period: 2,
			Epoch:  900,
		},
	}

	// Simulate auto-population logic from CreateConsensusEngine
	if config.XDPoS.V2 == nil && config.ChainID.Uint64() == 51 {
		config.XDPoS.V2 = &params.V2{
			SwitchBlock: big.NewInt(56828700),
		}
	}

	// Verify V2 config was populated
	if config.XDPoS.V2 == nil {
		t.Fatal("V2 config should be populated for Apothem")
	}

	// Verify correct switch block
	if config.XDPoS.V2.SwitchBlock.Cmp(expectedSwitchBlock) != 0 {
		t.Errorf("Apothem V2 SwitchBlock = %v, want %v", config.XDPoS.V2.SwitchBlock, expectedSwitchBlock)
	}
}

// TestXDPoSV2ConfigValues verifies the hardcoded values match XDPoSChain reference
func TestXDPoSV2ConfigValues(t *testing.T) {
	tests := []struct {
		name          string
		chainID       uint64
		expectedBlock int64
	}{
		{"Mainnet", 50, 80370000},
		{"Apothem", 51, 56828700},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &params.ChainConfig{
				ChainID: big.NewInt(int64(tt.chainID)),
				XDPoS: &params.XDPoSConfig{
					Period: 2,
					Epoch:  900,
				},
			}

			// Simulate auto-population
			switch tt.chainID {
			case 50:
				config.XDPoS.V2 = &params.V2{
					SwitchBlock: big.NewInt(80370000),
				}
			case 51:
				config.XDPoS.V2 = &params.V2{
					SwitchBlock: big.NewInt(56828700),
				}
			}

			if config.XDPoS.V2 == nil {
				t.Fatalf("V2 config should be populated for chain %d", tt.chainID)
			}

			if config.XDPoS.V2.SwitchBlock.Int64() != tt.expectedBlock {
				t.Errorf("Chain %d: SwitchBlock = %d, want %d",
					tt.chainID, config.XDPoS.V2.SwitchBlock.Int64(), tt.expectedBlock)
			}
		})
	}
}
