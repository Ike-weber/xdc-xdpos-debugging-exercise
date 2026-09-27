// Copyright (c) 2024 XDC Network
// Mining/block production functions for XDPoS 2.0
// Ported from v2.6.8 engines/engine_v2/mining.go
// Fixes: https://github.com/AnilChinchawale/go-ethereum/issues/37

package engine_v2

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// yourturn checks if the current node is the round-robin leader for block production.
// Uses highestSelfMinedRound to prevent double-mining on the same round.
// Handles epoch switch boundary (calcMasternodes vs GetMasternodes).
// Leader formula: leaderIndex = round % Epoch % len(masternodes)
// Matches v2.6.8 engines/engine_v2/mining.go exactly.
func (x *XDPoS_v2) yourturnAligned(chain consensus.ChainReader, round types.Round, parent *types.Header, signer common.Address) (bool, error) {
	// Guard: prevent double-mining on the same round
	if round <= x.highestSelfMinedRound {
		log.Warn("[yourturn] Already mined on this round",
			"Round", round, "highestSelfMinedRound", x.highestSelfMinedRound,
			"ParentHash", parent.Hash().Hex()[:16], "ParentNumber", parent.Number)
		return false, utils.ErrAlreadyMined
	}

	// Check if this round crosses an epoch boundary
	isEpochSwitch, _, err := x.isEpochSwitchAtRound(round, parent)
	if err != nil {
		log.Error("[yourturn] check epoch switch at round failed", "Error", err)
		return false, err
	}

	// Get masternode list — recalculate at epoch switch, reuse otherwise
	var masterNodes []common.Address
	if isEpochSwitch {
		masterNodes, _, _, err = x.calcMasternodes(chain, big.NewInt(0).Add(parent.Number, big.NewInt(1)), parent.Hash(), round, nil)
		if err != nil {
			log.Error("[yourturn] Cannot calcMasternodes at gap num", "err", err, "parent number", parent.Number)
			return false, err
		}
	} else {
		masterNodes = x.GetMasternodes(chain, parent)
	}

	if len(masterNodes) == 0 {
		log.Error("[yourturn] No masternodes found",
			"Hash", parent.Hash(), "CurrentRound", round, "Number", parent.Number)
		return false, errors.New("masternodes not found")
	}

	// Check if signer is in the masternode list
	curIndex := utils.Position(masterNodes, signer)
	if curIndex == -1 {
		log.Debug("[yourturn] Not in masternodes list",
			"Hash", parent.Hash().Hex()[:16], "signer", signer.Hex())
		return false, nil
	}

	// Round-robin leader selection: leaderIndex = round % Epoch % len(masternodes)
	leaderIndex := uint64(round) % x.config.Epoch % uint64(len(masterNodes))
	x.whosTurn = masterNodes[leaderIndex]

	if x.whosTurn != signer {
		log.Info("[yourturn] Not my turn",
			"curIndex", curIndex, "leaderIndex", leaderIndex,
			"whosTurn", x.whosTurn.Hex(), "myaddr", signer.Hex())
		return false, nil
	}

	log.Info("[yourturn] Yes, it is my turn",
		"ParentHash", parent.Hash().Hex()[:16], "ParentBlockNumber", parent.Number.Uint64())
	return true, nil
}
