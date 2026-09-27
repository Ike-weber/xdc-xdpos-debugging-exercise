package params

import (
	"math/big"
)

// XDCFeatures contains XDC-specific consensus features.
// These are nil for non-XDC chains, following Geth's pattern for chain-specific features.
type XDCFeatures struct {
	// V2SwitchBlock is the block at which XDPoS v2 activates.
	// This is duplicated from XDPoS.V2.SwitchBlock for feature detection purposes.
	V2SwitchBlock *big.Int `json:"v2SwitchBlock,omitempty"`

	// V2FirstEpochPenaltySkip disables penalties for the first V2 epoch.
	// This is needed because HookPenalty cannot walk backwards into V1 blocks.
	V2FirstEpochPenaltySkip *bool `json:"v2FirstEpochPenaltySkip,omitempty"`
}

// IsXDPoSV2 returns whether num is either the V2 switch block or after.
// This follows Geth's IsLondon(), IsShanghai() pattern.
func (c *ChainConfig) IsXDPoSV2(num uint64) bool {
	if c.XDCFeatures == nil || c.XDCFeatures.V2SwitchBlock == nil {
		return false
	}
	return isBlockForked(c.XDCFeatures.V2SwitchBlock, new(big.Int).SetUint64(num))
}

// IsXDPoSV2SwitchBlock returns whether num is exactly the V2 switch block.
func (c *ChainConfig) IsXDPoSV2SwitchBlock(num uint64) bool {
	if c.XDCFeatures == nil || c.XDCFeatures.V2SwitchBlock == nil {
		return false
	}
	return c.XDCFeatures.V2SwitchBlock.Uint64() == num
}

// SkipV2FirstEpochPenalty returns whether penalties should be skipped for the first V2 epoch.
// This handles the transition where HookPenalty cannot process V1 blocks.
func (c *ChainConfig) SkipV2FirstEpochPenalty(num uint64) bool {
	if c.XDCFeatures == nil || c.XDCFeatures.V2FirstEpochPenaltySkip == nil {
		return false
	}
	if !*c.XDCFeatures.V2FirstEpochPenaltySkip {
		return false
	}
	if c.XDPoS == nil {
		return false
	}

	switchBlock := c.XDCFeatures.V2SwitchBlock.Uint64()
	firstEpochEnd := switchBlock + c.XDPoS.Epoch

	// Skip penalties for blocks after switch block through end of first epoch
	return num > switchBlock && num <= firstEpochEnd
}

