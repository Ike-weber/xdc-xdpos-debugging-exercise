// Copyright (c) 2024 XDC Network
// Epoch switch utility functions for XDPoS 2.0
// Ported from v2.6.8 engines/engine_v2/epochSwitch.go
// Fixes: https://github.com/AnilChinchawale/go-ethereum/issues/36

package engine_v2

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// getPreviousEpochSwitchInfoByHash walks backward through epoch switches.
// Returns the epoch switch info `limit` epochs before the epoch containing `hash`.
// Matches v2.6.8 engines/engine_v2/epochSwitch.go.
func (x *XDPoS_v2) getPreviousEpochSwitchInfoByHash(chain consensus.ChainReader, hash common.Hash, limit int) (*types.EpochSwitchInfo, error) {
	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, nil, hash)
	if err != nil {
		log.Error("[getPreviousEpochSwitchInfoByHash] getEpochSwitchInfo error", "err", err)
		return nil, err
	}
	for i := 0; i < limit; i++ {
		if epochSwitchInfo.EpochSwitchParentBlockInfo == nil {
			break
		}
		epochSwitchInfo, err = x.getEpochSwitchInfo(chain, nil, epochSwitchInfo.EpochSwitchParentBlockInfo.Hash)
		if err != nil {
			log.Error("[getPreviousEpochSwitchInfoByHash] recursive getEpochSwitchInfo error", "err", err, "iteration", i)
			return nil, err
		}
	}
	return epochSwitchInfo, nil
}

// GetCurrentEpochSwitchBlock returns the epoch switch block number and epoch number
// for the epoch containing the given block number.
// Matches v2.6.8 engines/engine_v2/epochSwitch.go.
func (x *XDPoS_v2) GetCurrentEpochSwitchBlock(chain consensus.ChainReader, blockNum *big.Int) (uint64, uint64, error) {
	header := chain.GetHeaderByNumber(blockNum.Uint64())
	if header == nil {
		return 0, 0, ErrNotFoundBlockByNum
	}
	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, header, header.Hash())
	if err != nil {
		log.Error("[GetCurrentEpochSwitchBlock] Fail to get epoch switch info", "Num", header.Number, "Hash", header.Hash())
		return 0, 0, err
	}

	currentCheckpointNumber := epochSwitchInfo.EpochSwitchBlockInfo.Number.Uint64()
	epochNum := x.config.V2.SwitchEpoch + uint64(epochSwitchInfo.EpochSwitchBlockInfo.Round)/x.config.Epoch
	return currentCheckpointNumber, epochNum, nil
}

// GetEpochSwitchInfoBetween returns all epoch switch infos between begin and end headers.
// Searches backward from end to begin.
// Matches v2.6.8 engines/engine_v2/epochSwitch.go.
func (x *XDPoS_v2) GetEpochSwitchInfoBetween(chain consensus.ChainReader, begin, end *types.Header) ([]*types.EpochSwitchInfo, error) {
	infos := make([]*types.EpochSwitchInfo, 0)
	iteratorHeader := end
	iteratorHash := end.Hash()
	iteratorNum := end.Number

	for iteratorNum.Cmp(begin.Number) > 0 {
		epochSwitchInfo, err := x.getEpochSwitchInfo(chain, iteratorHeader, iteratorHash)
		if err != nil {
			log.Error("[GetEpochSwitchInfoBetween] getEpochSwitchInfo error", "err", err)
			return nil, err
		}
		iteratorHeader = nil
		if epochSwitchInfo.EpochSwitchParentBlockInfo == nil {
			break
		}
		iteratorHash = epochSwitchInfo.EpochSwitchParentBlockInfo.Hash
		iteratorNum = epochSwitchInfo.EpochSwitchBlockInfo.Number
		if iteratorNum.Cmp(begin.Number) >= 0 {
			infos = append(infos, epochSwitchInfo)
		}
	}

	// Reverse to chronological order
	for i := 0; i < len(infos)/2; i++ {
		infos[i], infos[len(infos)-1-i] = infos[len(infos)-1-i], infos[i]
	}
	return infos, nil
}
