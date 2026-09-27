// Copyright (c) 2018 XDCchain
// Copyright 2024 The go-ethereum Authors
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

package XDPoS

import "math/big"

// XDC Network constants
// NOTE: These are initialized with mainnet defaults but will be updated
// by SetNetworkConstants() when the chain ID is known.
const (
	// Reward distribution percentages
	RewardMasterPercent     = 90
	RewardVoterPercent      = 0
	RewardFoundationPercent = 10

	// Method signatures for contract calls
	HexSignMethod = "e341eaa4"
	HexSetSecret  = "34d38600"
	HexSetOpening = "e11f5ba2"

	// Epoch block offsets
	EpocBlockSecret    = 800
	EpocBlockOpening   = 850
	EpocBlockRandomize = 900

	// Masternode limits
	MaxMasternodes = 18

	// Penalty configuration
	LimitPenaltyEpoch = 4

	// Blocks per year (assuming 2 second blocks)
	BlocksPerYear = uint64(15768000)

	// Queue limits
	LimitThresholdNonceInQueue = 10

	// Gas price
	DefaultMinGasPrice = 2500

	// Block signing
	MergeSignRange    = 15
	RangeReturnSigner = 150

	// Minimum miner blocks per epoch
	MinimunMinerBlockPerEpoch = 1
)

// Fork block numbers — synced with XinFinOrg/XDPoSChain main branch
// These are pointers that will be redirected to network-specific values
// by SetNetworkConstants() based on chain ID.
var (
	TIP2019Block           = big.NewInt(1)
	TIPSigning             = big.NewInt(3000000)
	TIPRandomize           = big.NewInt(3464000)
	TIPIncreaseMasternodes = big.NewInt(5000000)

	// Hard fork blocks (from common/constants.mainnet.go)
	TIPNoHalvingMNReward   = big.NewInt(38383838)
	TIPXDCX                = big.NewInt(38383838)
	TIPXDCXLending         = big.NewInt(38383838)
	TIPXDCXCancellationFee = big.NewInt(38383838)
	TIPTRC21Fee            = big.NewInt(38383838)
	BlackListHFNumber      = uint64(38383838)

	// Berlin/London/Merge/Shanghai at block 76,321,000 (19th June 2024)
	XDCBerlinBlock   = big.NewInt(76321000)
	XDCLondonBlock   = big.NewInt(76321000)
	XDCMergeBlock    = big.NewInt(76321000)
	XDCShanghaiBlock = big.NewInt(76321000)

	// V2 switch + Gas 50x at block 80,370,000 (2nd Oct 2024)
	BlockNumberGas50x      = big.NewInt(80370000)
	TIPV2SwitchBlock       = big.NewInt(80370000)
	TIPXDCXMinerDisable    = big.NewInt(80370000)
	TIPXDCXReceiverDisable = big.NewInt(80370900)

	// Future (not yet active)
	XDCEIP1559Block   = big.NewInt(9999999999)
	XDCCancunBlock    = big.NewInt(9999999999)
	TIPUpgradeReward  = big.NewInt(9999999999)
	TIPUpgradePenalty = big.NewInt(9999999999)
	TIPEpochHalving   = big.NewInt(9999999999)
)

// MaxMasternodesV2 is a variable (not const) to support network-specific values
var MaxMasternodesV2 = 108

// Global configuration variables
var (
	IsTestnet         bool = false
	StoreRewardFolder string
	MinGasPrice       int64
)

// updateGlobalConstants updates the global var pointers to point to network-specific values
// This is called by SetNetworkConstants in network_constants.go
func updateGlobalConstants(nc *NetworkConstants) {
	TIP2019Block = nc.TIP2019Block
	// A.99.5b: TIPSigning must be updated here so that reward.go (which uses the
	// XDPoS-package TIPSigning, not common.TIPSigning) picks up the correct value
	// for the active network. Without this, chainID 5151 kept TIPSigning=3,000,000
	// (the mainnet default) causing the reward scan to use receipt-based signing-tx
	// extraction instead of CacheSigner, leading to signing-tx count differences and
	// state-root mismatch at every V1 epoch boundary (block 1800, 2700, etc). Refs #894.
	TIPSigning = nc.TIPSigning
	// Propagate TIPRandomize so that IsTIPRandomize (params/config.go) and the V1
	// masternode-list randomisation in engine_v1/utils.go use the network-specific value.
	// Without this, chainID 5151 retains the mainnet default (3,464,000) while
	// XDPoSChain constants.local.go has tipRandomize=0 — causing M1/M2 ordering
	// divergence at V1 epoch boundaries. Also propagate TIPIncreaseMasternodes for
	// completeness (XDPoSChain local constant = 0). Refs Opus RCA Step 3 / parity hygiene.
	TIPRandomize = nc.TIPRandomize
	TIPIncreaseMasternodes = nc.TIPIncreaseMasternodes
	TIPNoHalvingMNReward = nc.TIPNoHalvingMNReward
	TIPXDCX = nc.TIPXDCX
	TIPXDCXLending = nc.TIPXDCXLending
	TIPXDCXCancellationFee = nc.TIPXDCXCancellationFee
	TIPTRC21Fee = nc.TIPTRC21Fee
	BlackListHFNumber = nc.BlackListHFNumber

	XDCBerlinBlock = nc.TIPBerlinBlock
	XDCLondonBlock = nc.TIPLondonBlock
	XDCMergeBlock = nc.TIPMergeBlock
	XDCShanghaiBlock = nc.TIPShanghaiBlock

	BlockNumberGas50x = nc.BlockNumberGas50x
	TIPV2SwitchBlock = nc.TIPV2SwitchBlock
	TIPXDCXMinerDisable = nc.TIPXDCXMinerDisable
	TIPXDCXReceiverDisable = nc.TIPXDCXReceiverDisable

	XDCEIP1559Block = nc.TIPEIP1559Block
	XDCCancunBlock = nc.TIPCancunBlock
	TIPUpgradeReward = nc.TIPUpgradeReward
	TIPUpgradePenalty = nc.TIPUpgradePenalty
	TIPEpochHalving = nc.TIPEpochHalving

	MaxMasternodesV2 = nc.MaxMasternodesV2
}

// IgnoreSignerCheckBlocksMainnet is the set of XDC mainnet epoch-boundary blocks
// where the signer-in-set check must be skipped (matches XDPoSChain v2.6.8).
var IgnoreSignerCheckBlocksMainnet = map[uint64]struct{}{
	1032300:  {},
	1033200:  {},
	27307800: {},
	28270800: {},
}

// IgnoreSignerCheckBlocksApothem is the set of XDC Apothem testnet blocks
// where the signer-in-set check must be skipped.
var IgnoreSignerCheckBlocksApothem = map[uint64]struct{}{
	1032300:  {},
	1033200:  {},
	27307800: {},
	28270800: {},
}

// IsIgnoreSignerCheckBlock returns whether the given block number should skip
// the signer-in-set verification for the given chain ID.
func IsIgnoreSignerCheckBlock(chainID uint64, blockNumber uint64) bool {
	switch chainID {
	case 50: // mainnet
		_, ok := IgnoreSignerCheckBlocksMainnet[blockNumber]
		return ok
	case 51: // apothem
		_, ok := IgnoreSignerCheckBlocksApothem[blockNumber]
		return ok
	}
	return false
}
