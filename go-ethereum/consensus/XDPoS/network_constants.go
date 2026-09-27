package XDPoS

import (
	"math"
	"math/big"
)

// NetworkType represents the XDC network type
type NetworkType int

const (
	NetworkMainnet NetworkType = iota
	NetworkApothem
)

// NetworkConstants holds all network-specific constants
type NetworkConstants struct {
	// Fork blocks
	TIP2019Block           *big.Int
	TIPSigning             *big.Int
	TIPRandomize           *big.Int
	TIPNoHalvingMNReward   *big.Int
	TIPXDCX                *big.Int
	TIPXDCXLending         *big.Int
	TIPXDCXCancellationFee *big.Int
	TIPTRC21Fee            *big.Int
	TIPIncreaseMasternodes *big.Int
	TIPBerlinBlock         *big.Int
	TIPLondonBlock         *big.Int
	TIPMergeBlock          *big.Int
	TIPShanghaiBlock       *big.Int
	BlockNumberGas50x      *big.Int
	TIPV2SwitchBlock       *big.Int
	TIPXDCXMinerDisable    *big.Int
	TIPXDCXReceiverDisable *big.Int
	TIPEIP1559Block        *big.Int
	TIPCancunBlock         *big.Int
	TIPUpgradeReward       *big.Int
	TIPUpgradePenalty      *big.Int
	TIPEpochHalving        *big.Int

	// Other constants
	BlackListHFNumber uint64
	MaxMasternodesV2  int
}

// Mainnet constants
var mainnetConstants = &NetworkConstants{
	TIP2019Block:           big.NewInt(1),
	TIPSigning:             big.NewInt(3000000),
	TIPRandomize:           big.NewInt(3464000),
	TIPNoHalvingMNReward:   big.NewInt(38383838),
	TIPXDCX:                big.NewInt(38383838),
	TIPXDCXLending:         big.NewInt(38383838),
	TIPXDCXCancellationFee: big.NewInt(38383838),
	TIPTRC21Fee:            big.NewInt(38383838),
	TIPIncreaseMasternodes: big.NewInt(5000000),
	TIPBerlinBlock:         big.NewInt(76321000), // Target 19th June 2024
	TIPLondonBlock:         big.NewInt(76321000), // Target 19th June 2024
	TIPMergeBlock:          big.NewInt(76321000), // Target 19th June 2024
	TIPShanghaiBlock:       big.NewInt(76321000), // Target 19th June 2024
	BlockNumberGas50x:      big.NewInt(80370000), // Target 2nd Oct 2024
	TIPV2SwitchBlock:       big.NewInt(80370000), // Target 2nd Oct 2024
	TIPXDCXMinerDisable:    big.NewInt(80370000), // Target 2nd Oct 2024
	TIPXDCXReceiverDisable: big.NewInt(80370900), // Target 2nd Oct 2024
	TIPEIP1559Block:        big.NewInt(98800200), // Target 28th Jan 2026 — match XDPoSChain canonical + params/config.go
	TIPCancunBlock:         big.NewInt(98802000), // Target 28th Jan 2026 — match XDPoSChain canonical + params/config.go
	TIPUpgradeReward:       big.NewInt(9999999999),
	TIPUpgradePenalty:      big.NewInt(9999999999),
	TIPEpochHalving:        big.NewInt(9999999999),
	BlackListHFNumber:      38383838,
	MaxMasternodesV2:       108,
}

// Apothem testnet constants
var apothemConstants = &NetworkConstants{
	TIP2019Block:           big.NewInt(1),
	TIPSigning:             big.NewInt(3000000),
	TIPRandomize:           big.NewInt(3464000),
	TIPNoHalvingMNReward:   big.NewInt(23779191), // hardfork no halving masternodes reward
	TIPXDCX:                big.NewInt(23779191),
	TIPXDCXLending:         big.NewInt(23779191),
	TIPXDCXCancellationFee: big.NewInt(23779191),
	TIPTRC21Fee:            big.NewInt(23779191),
	TIPIncreaseMasternodes: big.NewInt(5000000),
	TIPBerlinBlock:         big.NewInt(61290000), // Target 31st March 2024
	TIPLondonBlock:         big.NewInt(61290000), // Target 31st March 2024
	TIPMergeBlock:          big.NewInt(61290000), // Target 31st March 2024
	TIPShanghaiBlock:       big.NewInt(61290000), // Target 31st March 2024
	BlockNumberGas50x:      big.NewInt(56828700), // Target 13rd Nov 2023
	TIPV2SwitchBlock:       big.NewInt(56828700), // Target 13rd Nov 2023
	TIPXDCXMinerDisable:    big.NewInt(61290000), // Target 31st March 2024
	TIPXDCXReceiverDisable: big.NewInt(66825000), // Target 26 Aug 2024
	TIPEIP1559Block:        big.NewInt(71550000), // Target 14th Feb 2025
	TIPCancunBlock:         big.NewInt(71551800),
	TIPUpgradeReward:       big.NewInt(83600000), // XinFinOrg/XDPoSChain#2383 (v2.8.0-testnet)
	TIPUpgradePenalty:      big.NewInt(83600000), // XinFinOrg/XDPoSChain#2383 (v2.8.0-testnet)
	TIPEpochHalving:        big.NewInt(9999999999),
	BlackListHFNumber:      23779191,
	MaxMasternodesV2:       15,
}

// Legacy devnet constants (chainId 5551) — preserved for the existing devnet
// datadir (V2 switch at block 2700). The active main-branch devnet on
// XinFinOrg/XDPoSChain has migrated to chainId 551 with V2-from-genesis;
// see devnet551Constants below.
var devnetConstants = &NetworkConstants{
	TIP2019Block:         big.NewInt(0),
	TIPSigning:           big.NewInt(0),
	TIPRandomize:         big.NewInt(0),
	TIPNoHalvingMNReward: big.NewInt(0),
	TIPXDCX:              big.NewInt(0),
	TIPXDCXLending:       big.NewInt(0),
	// Align with params/config.go: Constantinople/Petersburg/Istanbul at 250000
	// (matching TIPEIP1559Block). TIPXDCXCancellationFee=0 would activate
	// Istanbul from genesis, causing EIP-1884 gas overcharge (observed: 60k
	// gas diff at block 811). Set to 250000 so devnet runs Byzantium-only
	// until the planned upgrade.
	TIPXDCXCancellationFee: big.NewInt(0),
	TIPTRC21Fee:            big.NewInt(0),
	TIPIncreaseMasternodes: big.NewInt(0),
	// Berlin/London at 250000 to match EIP-1559 activation
	TIPBerlinBlock:         big.NewInt(250000),
	TIPLondonBlock:         big.NewInt(250000),
	TIPMergeBlock:          big.NewInt(0),
	TIPShanghaiBlock:       big.NewInt(0),
	BlockNumberGas50x:      big.NewInt(0),
	TIPV2SwitchBlock:       big.NewInt(2700),
	TIPXDCXMinerDisable:    big.NewInt(0),
	TIPXDCXReceiverDisable: big.NewInt(0),
	TIPEIP1559Block:        big.NewInt(250000),
	TIPCancunBlock:         big.NewInt(250000),
	TIPUpgradeReward:       big.NewInt(5000000),
	TIPUpgradePenalty:      big.NewInt(5000000),
	TIPEpochHalving:        big.NewInt(math.MaxInt64),
	BlackListHFNumber:      0,
	MaxMasternodesV2:       108,
}

// Private dev chain constants (chainId 99999, from cmd/puppeth wizard).
// Mirrors devnetConstants (5551) shape but with V2 active from a configurable
// low switch block (default 1800 = third epoch boundary) so that demos can
// exercise the V1→V2 transition within a single session. All TIP fork blocks
// other than TIPV2SwitchBlock are 0 — there's no operational reason to gate
// EVM forks on private dev chains. Issue #603.
var privateConstants = &NetworkConstants{
	TIP2019Block:           big.NewInt(0),
	TIPSigning:             big.NewInt(0),
	TIPRandomize:           big.NewInt(0),
	TIPNoHalvingMNReward:   big.NewInt(0),
	TIPXDCX:                big.NewInt(0),
	TIPXDCXLending:         big.NewInt(0),
	TIPXDCXCancellationFee: big.NewInt(0),
	TIPTRC21Fee:            big.NewInt(0),
	TIPIncreaseMasternodes: big.NewInt(0),
	TIPBerlinBlock:         big.NewInt(0),
	TIPLondonBlock:         big.NewInt(0),
	TIPMergeBlock:          big.NewInt(0),
	TIPShanghaiBlock:       big.NewInt(0),
	BlockNumberGas50x:      big.NewInt(0),
	TIPV2SwitchBlock:       big.NewInt(1800),
	TIPXDCXMinerDisable:    big.NewInt(0),
	TIPXDCXReceiverDisable: big.NewInt(0),
	TIPEIP1559Block:        big.NewInt(0),
	TIPCancunBlock:         big.NewInt(0),
	TIPUpgradeReward:       big.NewInt(9999999999),
	TIPUpgradePenalty:      big.NewInt(9999999999),
	TIPEpochHalving:        big.NewInt(math.MaxInt64),
	BlackListHFNumber:      0,
	MaxMasternodesV2:       108,
}

// Active devnet constants (chainId 551) — mirrors XinFinOrg/XDPoSChain main:
// common/constants.devnet.go. V2 from genesis, fast-cycle fork schedule for
// upgrade rehearsal: eip1559Block=32400, cancunBlock=43200, etc.
var devnet551Constants = &NetworkConstants{
	TIP2019Block:           big.NewInt(0),
	TIPSigning:             big.NewInt(0),
	TIPRandomize:           big.NewInt(0),
	TIPNoHalvingMNReward:   big.NewInt(0),
	TIPXDCX:                big.NewInt(0),
	TIPXDCXLending:         big.NewInt(0),
	TIPXDCXCancellationFee: big.NewInt(0),
	TIPTRC21Fee:            big.NewInt(10800),
	TIPIncreaseMasternodes: big.NewInt(0),
	TIPBerlinBlock:         big.NewInt(0),
	TIPLondonBlock:         big.NewInt(0),
	TIPMergeBlock:          big.NewInt(0),
	TIPShanghaiBlock:       big.NewInt(0),
	BlockNumberGas50x:      big.NewInt(21600),
	TIPV2SwitchBlock:       big.NewInt(0),
	TIPXDCXMinerDisable:    big.NewInt(0),
	TIPXDCXReceiverDisable: big.NewInt(0),
	TIPEIP1559Block:        big.NewInt(32400),
	TIPCancunBlock:         big.NewInt(43200),
	TIPUpgradeReward:       big.NewInt(243000),
	TIPUpgradePenalty:      big.NewInt(9999999999),
	TIPEpochHalving:        big.NewInt(9999999999),
	BlackListHFNumber:      0,
	MaxMasternodesV2:       108,
}

// defaultXDCConstants applies to any unregistered XDPoS chain ID (i.e. any
// chain that is not mainnet/50, Apothem/51, devnet/551, legacy devnet/5551, or
// puppeth devchain/99999). It is LEGACY-COMPATIBLE: it mirrors the legacy
// XDPoSChain v2.7.1 mainnet base so a modern geth node computes the SAME state
// root as the legacy oldxdc validators on a net5151/5152-parity private net,
// syncing to tip with NO skipV1Validation / bypass / skip-gas-check.
//
// Key parity decisions (matching an oldxdc built on the v2.7.1 mainnet base):
//
//   - TIPSigning = 3,000,000 — GAS-CHARGED sign-txs. Legacy charges intrinsic
//     gas on BlockSigners (0x89) sign-txs; the modern gasless path only zeroes
//     GasUsed once block >= TIPSigning, so keeping TIPSigning far above any
//     reachable block leaves the gasless path INACTIVE and matches legacy's
//     normal-gas behaviour (eliminates the ~13,600 gas/block signing-tx
//     divergence and the resulting epoch-boundary state-root mismatch — the
//     xone@248-class defect). This is the deliberate difference from #1339,
//     which set TIPSigning = 0 (gasless) and diverged from the gas-charged base.
//   - TIPRandomize / TIPNoHalvingMNReward / TIPXDCX* / gas50x / BlackList all
//     use the mainnet base values (far-future). A low-block private net never
//     reaches them, so M1/M2 ordering, XDCx special-tx gas, and reward math all
//     stay on the legacy pre-fork path — matching oldxdc exactly.
//
// Deployment-staged forks (reachable on a fresh private net, mirroring the
// net5151/5152 ladder; genesis JSON supplies/overrides the actual blocks):
//
//	base EVM (Byzantium/Constantinople/Istanbul/Berlin/London)  block 0
//	V1 -> V2 BFT switch (TIPV2SwitchBlock)                       block 1800 (epoch 2)
//	EIP-1559 (TIPEIP1559Block)                                   block 2250
//	Cancun   (TIPCancunBlock)                                    block 3150
//
// Upgrade-gated TIP blocks (TIPUpgradeReward/Penalty/EpochHalving) are
// math.MaxInt64 — private nets have no planned token-economics upgrades.
// MaxMasternodesV2 = 108. Deployment identity (chainId, staged v2.switchBlock,
// tipSigningBlock) still comes from the genesis JSON. Fork values are kept in
// sync with params.XDCDefaultPrivateChainConfig.
var defaultXDCConstants = &NetworkConstants{
	TIP2019Block:           big.NewInt(1),       // legacy base: reward update active from block 1
	TIPSigning:             big.NewInt(3000000), // GAS-CHARGED (gasless path INACTIVE) — matches legacy oldxdc v2.7.1
	TIPRandomize:           big.NewInt(3464000), // mainnet base: randomize OFF on a low-block net (M1/M2 ordering parity)
	TIPNoHalvingMNReward:   big.NewInt(38383838),
	TIPXDCX:                big.NewInt(38383838),
	TIPXDCXLending:         big.NewInt(38383838),
	TIPXDCXCancellationFee: big.NewInt(38383838),
	TIPTRC21Fee:            big.NewInt(38383838),
	TIPIncreaseMasternodes: big.NewInt(5000000),
	TIPBerlinBlock:         big.NewInt(0),
	TIPLondonBlock:         big.NewInt(0),
	TIPMergeBlock:          big.NewInt(0),
	TIPShanghaiBlock:       big.NewInt(0),
	BlockNumberGas50x:      big.NewInt(80370000), // mainnet base: 50x gas schedule OFF on a low-block net
	TIPV2SwitchBlock:       big.NewInt(1800),     // V1 -> V2 BFT switch (epoch 2); genesis switchBlock overrides
	TIPXDCXMinerDisable:    big.NewInt(80370000),
	TIPXDCXReceiverDisable: big.NewInt(80370900), // mainnet base: > TIPXDCX so IsTIPXDCXReceiver=false on a low-block net
	TIPEIP1559Block:        big.NewInt(2250),
	TIPCancunBlock:         big.NewInt(3150),
	TIPUpgradeReward:       big.NewInt(math.MaxInt64),
	TIPUpgradePenalty:      big.NewInt(math.MaxInt64),
	TIPEpochHalving:        big.NewInt(math.MaxInt64),
	BlackListHFNumber:      38383838,
	MaxMasternodesV2:       108,
}

// currentConstants holds the active network constants
var currentConstants = mainnetConstants

// SetNetworkConstants sets the constants based on chain ID
func SetNetworkConstants(chainID uint64) {
	switch chainID {
	case 50: // XDC mainnet
		currentConstants = mainnetConstants
		IsTestnet = false
	case 51: // Apothem testnet
		currentConstants = apothemConstants
		IsTestnet = true
	case 551: // Active devnet (XDPoSChain main branch)
		currentConstants = devnet551Constants
		IsTestnet = true
	case 5050: // net5050 (XDCIndia stress-net, V2 switch at block 2700). Parity only;
		// the gasless gate is driven by common.TIPSigning (CopyXDCConstants). Refs xOneGo#45.
		currentConstants = devnetConstants
		IsTestnet = true
	case 5551: // Legacy devnet (V2 switch at block 2700)
		currentConstants = devnetConstants
		IsTestnet = true
	case 99999: // Private dev chain generated via cmd/puppeth (issue #603).
		currentConstants = privateConstants
		IsTestnet = true
	default: // Any other chainId: unregistered XDPoS private net — inherit the XDC default profile
		currentConstants = defaultXDCConstants
		IsTestnet = true
	}
	// Update the global var pointers to point to network-specific values
	updateGlobalConstants(currentConstants)
}

// GetCurrentConstants returns the current network constants
func GetCurrentConstants() *NetworkConstants {
	return currentConstants
}

// Helper functions for accessing current constants
func GetTIPV2SwitchBlock() *big.Int       { return currentConstants.TIPV2SwitchBlock }
func GetBlockNumberGas50x() *big.Int      { return currentConstants.BlockNumberGas50x }
func GetTIPBerlinBlock() *big.Int         { return currentConstants.TIPBerlinBlock }
func GetTIPLondonBlock() *big.Int         { return currentConstants.TIPLondonBlock }
func GetTIPMergeBlock() *big.Int          { return currentConstants.TIPMergeBlock }
func GetTIPShanghaiBlock() *big.Int       { return currentConstants.TIPShanghaiBlock }
func GetTIPXDCXMinerDisable() *big.Int    { return currentConstants.TIPXDCXMinerDisable }
func GetTIPXDCXReceiverDisable() *big.Int { return currentConstants.TIPXDCXReceiverDisable }
func GetTIPEIP1559Block() *big.Int        { return currentConstants.TIPEIP1559Block }
func GetTIPCancunBlock() *big.Int         { return currentConstants.TIPCancunBlock }
func GetTIPTRC21Fee() *big.Int            { return currentConstants.TIPTRC21Fee }
func GetTIPNoHalvingMNReward() *big.Int   { return currentConstants.TIPNoHalvingMNReward }
func GetBlackListHFNumber() uint64        { return currentConstants.BlackListHFNumber }
func GetMaxMasternodesV2() int            { return currentConstants.MaxMasternodesV2 }
