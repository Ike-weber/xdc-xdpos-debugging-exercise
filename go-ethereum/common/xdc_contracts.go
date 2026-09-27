// Copyright 2026 The XDC Network Authors
//
// XDC protocol-defined system-contract addresses, factored out of types.go
// into a dedicated file (issue #694 §E). These are the precompile-style
// addresses for XDPoS consensus, XDCx/XDCxLending, and the XDC native
// coin — protocol-fixed values that don't vary across mainnet/testnet/devnet.
//
// Note: FoudationAddrBinary preserves the historical misspelling so that
// existing consensus-touching code paths and exported APIs are unchanged.
// A typo fix would touch ~20 call sites and require a coordinated cycle;
// tracked separately.

package common

var (
	// BlockSignersBinary is the BlockSigners contract address (V1 XDPoS
	// signer tracking; emits the per-block Sign event consumed by reward
	// calculation, and was self-destructed at the TIPSigning fork).
	BlockSignersBinary = HexToAddress("0x0000000000000000000000000000000000000089")

	// MasternodeVotingSMCBinary is the masternode-voting SMC where
	// vote/unvote/propose/resign transactions are routed.
	MasternodeVotingSMCBinary = HexToAddress("0x0000000000000000000000000000000000000088")

	// RandomizeSMCBinary is the randomness-commit contract that drives
	// masternode order selection per epoch via the
	// secret/opening reveal scheme.
	RandomizeSMCBinary = HexToAddress("0x0000000000000000000000000000000000000090")

	// FoudationAddrBinary [sic — historical misspelling preserved] is the
	// XDC foundation reward-recipient address used by HookReward.
	FoudationAddrBinary = HexToAddress("0x0000000000000000000000000000000000000068")

	// TeamAddrBinary is the XDC team allocation address.
	TeamAddrBinary = HexToAddress("0x0000000000000000000000000000000000000099")

	// XDCXAddrBinary is the XDCx trading-engine contract address.
	XDCXAddrBinary = HexToAddress("0x0000000000000000000000000000000000000091")

	// TradingStateAddrBinary is the XDCx trading-state contract address.
	TradingStateAddrBinary = HexToAddress("0x0000000000000000000000000000000000000092")

	// XDCXLendingAddressBinary is the XDCx lending-engine contract address.
	XDCXLendingAddressBinary = HexToAddress("0x0000000000000000000000000000000000000093")

	// XDCXLendingFinalizedTradeAddressBinary is the XDCx
	// finalized-lending-trade contract address.
	XDCXLendingFinalizedTradeAddressBinary = HexToAddress("0x0000000000000000000000000000000000000094")

	// XDCNativeAddressBinary is the XDC native coin contract address
	// (special-cased for fee handling and balance accounting).
	XDCNativeAddressBinary = HexToAddress("0x0000000000000000000000000000000000000001")

	// LendingLockAddressBinary is the XDCx lending-lock contract address.
	LendingLockAddressBinary = HexToAddress("0x0000000000000000000000000000000000000011")

	// MintedRecordAddressBinary is the minted-record contract address.
	MintedRecordAddressBinary = HexToAddress("0x000000000000000000000000000000000000009a")
)
