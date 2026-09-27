package util

import (
	"math/big"

	"github.com/ethereum/go-ethereum/consensus"
)

// RewardInflation calculates reward inflation based on block number and blocks per year.
// If TIPNoHalvingMNReward is active, the reward is not halved (matches v2.6.8).
//
// Takes consensus.ChainHeaderReader (not ChainReader) because we only need
// chain.Config() — every caller can pass either type since ChainReader embeds
// ChainHeaderReader. Caller is expected to pass a non-nil chain; passing nil
// silently bypasses the IsTIPNoHalvingMNReward guard and applies halving
// where canonical does not (Apothem block 31,536,000 = BlocksPerYear*2 was
// the precise reproduction case for that historical bug).
func RewardInflation(chain consensus.ChainHeaderReader, chainReward *big.Int, number uint64, blockPerYear uint64) *big.Int {
	if chain != nil && chain.Config().IsTIPNoHalvingMNReward(new(big.Int).SetUint64(number)) {
		return chainReward
	}

	if blockPerYear*2 <= number && number < blockPerYear*5 {
		chainReward.Div(chainReward, new(big.Int).SetUint64(2))
	}
	if blockPerYear*5 <= number {
		chainReward.Div(chainReward, new(big.Int).SetUint64(4))
	}

	return chainReward
}

// RewardHalving computes the reward for Masternode/Protector/Observer based on epoch total reward, supply after halving is enabled, and epoch after halving is enabled
// The sequence is a geometric sequence in order to make supply be limited
func RewardHalving(epochRewardSingle *big.Int, epochRewardTotal *big.Int, halvingSupply *big.Int, epochSinceHalving uint64) *big.Int {
	rt := new(big.Float).SetInt(epochRewardTotal)
	hs := new(big.Float).SetInt(halvingSupply)
	// zero cause Quo panic so return early
	// or epoch reward > halving supply, return early
	if halvingSupply.BitLen() == 0 || epochRewardTotal.Cmp(halvingSupply) > 0 {
		return big.NewInt(0)
	}
	quo := new(big.Float).Quo(rt, hs)
	// base = 1- reward/supply
	base := new(big.Float).Sub(big.NewFloat(1), quo)
	r := new(big.Float).SetInt(epochRewardSingle)
	result := new(big.Float).Mul(r, FloatPower(base, epochSinceHalving))
	resultInt, _ := result.Int(nil)
	return resultInt
}

// FloatPower calculates base^exp for big.Float
func FloatPower(base *big.Float, exp uint64) *big.Float {
	result := big.NewFloat(1)
	for exp > 0 {
		if exp%2 == 1 {
			result.Mul(result, base)
		}
		base.Mul(base, base)
		exp >>= 1 // same as: exp = exp / 2
	}
	return result
}
