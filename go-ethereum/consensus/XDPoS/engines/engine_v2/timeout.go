// Copyright (c) 2024 XDC Network
// Timeout handling for XDPoS 2.0 BFT consensus

package engine_v2

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// VerifyTimeoutMessage verifies a timeout message from a peer
func (x *XDPoS_v2) VerifyTimeoutMessage(chain consensus.ChainReader, timeoutMsg *types.Timeout) (bool, error) {
	// Ensure engine is initialized (seeds the gap-0 snapshot on fresh devnet start).
	// This mirrors the same guard in YourTurn — VerifyTimeoutMessage can be called
	// before the mining loop has had a chance to call initial().
	if !x.isInitialized {
		x.lock.Lock()
		if !x.isInitialized { // double-checked under write lock
			head := chain.CurrentHeader()
			if head == nil {
				head = chain.GetHeaderByNumber(0)
			}
			if head != nil {
				if err := x.initial(chain, head); err != nil {
					log.Warn("[VerifyTimeoutMessage] init on first timeout failed — snapshot may be missing",
						"err", err, "head", head.Number.Uint64())
				}
			}
		}
		x.lock.Unlock()
	}

	// Check if timeout round is current
	if timeoutMsg.Round < x.currentRound {
		log.Debug("[VerifyTimeoutMessage] Timeout round too old",
			"timeoutRound", timeoutMsg.Round,
			"currentRound", x.currentRound)
		return false, nil
	}

	// Get snapshot
	snap, err := x.getSnapshot(chain, timeoutMsg.GapNumber, true, nil)
	if err != nil || snap == nil {
		log.Error("[VerifyTimeoutMessage] Failed to get snapshot",
			"gapNumber", timeoutMsg.GapNumber,
			"error", err)
		return false, err
	}

	// Use CandidatePool() (active set ∪ penalties) rather than NextEpochCandidates
	// alone so a currently-penalized masternode is still accepted for its own
	// timeout/comeback checks (matches canonical XDPoSChain).
	pool := snap.CandidatePool()
	if len(pool) == 0 {
		log.Error("[VerifyTimeoutMessage] Empty masternode list",
			"gapNumber", timeoutMsg.GapNumber)
		return false, errors.New("empty masternode list")
	}

	// Verify signature
	verified, signer, err := x.verifyMsgSignature(
		types.TimeoutSigHash(&types.TimeoutForSign{
			Round:     timeoutMsg.Round,
			GapNumber: timeoutMsg.GapNumber,
		}),
		timeoutMsg.Signature,
		pool,
	)
	if err != nil {
		log.Warn("[VerifyTimeoutMessage] Signature verification failed", "error", err)
		return false, err
	}

	timeoutMsg.SetSigner(signer)
	return verified, nil
}

// TimeoutHandler processes a timeout message
func (x *XDPoS_v2) TimeoutHandler(chain consensus.ChainReader, timeout *types.Timeout) error {
	x.lock.Lock()
	defer x.lock.Unlock()
	return x.timeoutHandler(chain, timeout)
}

func (x *XDPoS_v2) timeoutHandler(chain consensus.ChainReader, timeout *types.Timeout) error {
	// Check round
	if timeout.Round != x.currentRound {
		return &utils.ErrIncomingMessageRoundNotEqualCurrentRound{
			Type:          "timeout",
			IncomingRound: timeout.Round,
			CurrentRound:  x.currentRound,
		}
	}

	// Add to pool
	numberOfTimeouts, pooledTimeouts := x.timeoutPool.Add(timeout)
	log.Debug("[timeoutHandler] collected timeout", "count", numberOfTimeouts)

	// Get epoch info
	epochInfo, err := x.getEpochSwitchInfo(chain, chain.CurrentHeader(), chain.CurrentHeader().Hash())
	if err != nil {
		log.Error("[timeoutHandler] Failed to get epoch info", "error", err)
		return fmt.Errorf("failed to get epoch switch info: %s", err)
	}

	// Check threshold using per-round config (fix #63)
	certThreshold := x.config.V2.Config(uint64(timeout.Round)).CertThreshold
	isThresholdReached := float64(numberOfTimeouts) >= float64(epochInfo.MasternodesLen)*certThreshold

	if isThresholdReached {
		log.Info("[timeoutHandler] Timeout threshold reached",
			"count", numberOfTimeouts,
			"threshold", float64(epochInfo.MasternodesLen)*certThreshold)

		if err := x.onTimeoutPoolThresholdReached(chain, pooledTimeouts, timeout, timeout.GapNumber); err != nil {
			return err
		}
	}

	return nil
}

// onTimeoutPoolThresholdReached generates a TC when enough timeouts are collected
func (x *XDPoS_v2) onTimeoutPoolThresholdReached(chain consensus.ChainReader, pooledTimeouts map[common.Hash]utils.PoolObj, currentTimeoutMsg utils.PoolObj, gapNumber uint64) error {
	// Collect signatures
	signatures := make([]types.Signature, 0, len(pooledTimeouts))
	for _, v := range pooledTimeouts {
		signatures = append(signatures, v.(*types.Timeout).Signature)
	}

	// Generate TC
	timeoutCert := &types.TimeoutCert{
		Round:      currentTimeoutMsg.(*types.Timeout).Round,
		Signatures: signatures,
		GapNumber:  gapNumber,
	}

	// Process TC
	if err := x.processTC(chain, timeoutCert); err != nil {
		log.Error("[onTimeoutPoolThresholdReached] Failed to process TC",
			"round", timeoutCert.Round,
			"signatures", len(timeoutCert.Signatures),
			"error", err)
		return err
	}

	// Broadcast SyncInfo
	syncInfo := x.getSyncInfo()
	x.broadcastToBftChannel(syncInfo)

	log.Info("[onTimeoutPoolThresholdReached] TC processed successfully",
		"round", timeoutCert.Round,
		"signatures", len(timeoutCert.Signatures))
	return nil
}

// sendTimeout sends a timeout message
func (x *XDPoS_v2) sendTimeout(chain consensus.ChainReader) error {
	// Calculate gap number
	var gapNumber uint64
	currentBlockHeader := chain.CurrentHeader()

	isEpochSwitch, epochNum, err := x.isEpochSwitchAtRound(x.currentRound, currentBlockHeader)
	if err != nil {
		log.Error("[sendTimeout] isEpochSwitchAtRound error",
			"currentRound", x.currentRound,
			"error", err)
		return err
	}

	if isEpochSwitch {
		currentNumber := currentBlockHeader.Number.Uint64() + 1
		gapNumber = currentNumber - currentNumber%x.config.Epoch
		if gapNumber > x.config.Gap {
			gapNumber -= x.config.Gap
		} else {
			gapNumber = 0
		}
		log.Debug("[sendTimeout] epoch switch",
			"currentNumber", currentNumber,
			"gapNumber", gapNumber)
	} else {
		epochSwitchInfo, err := x.getEpochSwitchInfo(chain, currentBlockHeader, currentBlockHeader.Hash())
		if err != nil {
			log.Error("[sendTimeout] Failed to get epoch switch info",
				"currentRound", x.currentRound,
				"epochNum", epochNum,
				"error", err)
			return err
		}
		gapNumber = epochSwitchInfo.EpochSwitchBlockInfo.Number.Uint64() - epochSwitchInfo.EpochSwitchBlockInfo.Number.Uint64()%x.config.Epoch
		if gapNumber > x.config.Gap {
			gapNumber -= x.config.Gap
		} else {
			gapNumber = 0
		}
	}

	// Sign timeout
	signedHash, err := x.signSignature(types.TimeoutSigHash(&types.TimeoutForSign{
		Round:     x.currentRound,
		GapNumber: gapNumber,
	}))
	if err != nil {
		log.Error("[sendTimeout] Failed to sign", "error", err)
		return err
	}

	timeoutMsg := &types.Timeout{
		Round:     x.currentRound,
		Signature: signedHash,
		GapNumber: gapNumber,
	}
	timeoutMsg.SetSigner(x.signer)

	log.Warn("[sendTimeout] Timeout message generated",
		"round", timeoutMsg.Round,
		"gapNumber", timeoutMsg.GapNumber,
		"whosTurn", x.whosTurn)

	// Process locally
	if err := x.timeoutHandler(chain, timeoutMsg); err != nil {
		log.Error("[sendTimeout] Local handler error", "error", err)
		return err
	}

	// Broadcast
	x.broadcastToBftChannel(timeoutMsg)
	return nil
}

// OnCountdownTimeout is called when the countdown timer expires
func (x *XDPoS_v2) OnCountdownTimeout(t time.Time, chain interface{}) error {
	x.lock.Lock()
	defer x.lock.Unlock()

	chainReader := chain.(consensus.ChainReader)

	// Check if we're a masternode
	if !x.allowedToSend(chainReader, chainReader.CurrentHeader(), "timeout") {
		return nil
	}

	// Send timeout
	if err := x.sendTimeout(chainReader); err != nil {
		log.Error("[OnCountdownTimeout] Failed to send timeout",
			"time", t,
			"error", err)
		return err
	}

	x.timeoutCount++

	// Send SyncInfo periodically (use per-round config, fix #63)
	if x.timeoutCount%x.config.V2.Config(uint64(x.currentRound)).TimeoutSyncThreshold == 0 {
		log.Warn("[OnCountdownTimeout] Timeout sync threshold reached, sending SyncInfo")
		syncInfo := x.getSyncInfo()
		x.broadcastToBftChannel(syncInfo)
	}

	return nil
}

// getTCEpochInfo returns the epoch switch info that corresponds to the round
// in the given TimeoutCert. Matches v2.6.8 engines/engine_v2/timeout.go getTCEpochInfo.
// Fix #64: verifyTC must use TC's round epoch, not chain.CurrentHeader().
func (x *XDPoS_v2) getTCEpochInfo(chain consensus.ChainReader, timeoutCert *types.TimeoutCert) (*types.EpochSwitchInfo, error) {
	currentHeader := chain.CurrentHeader()
	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, currentHeader, currentHeader.Hash())
	if err != nil {
		log.Error("[getTCEpochInfo] Error getting current epoch switch info", "error", err)
		return nil, fmt.Errorf("[getTCEpochInfo] failed to get epoch switch info: %s", err)
	}

	epochRound := epochSwitchInfo.EpochSwitchBlockInfo.Round
	switchEpoch := x.config.V2.SwitchEpoch
	tempTCEpoch := switchEpoch + uint64(epochRound)/x.config.Epoch

	epochBlockInfo := &types.BlockInfo{
		Hash:   epochSwitchInfo.EpochSwitchBlockInfo.Hash,
		Round:  epochRound,
		Number: epochSwitchInfo.EpochSwitchBlockInfo.Number,
	}
	log.Info("[getTCEpochInfo] init", "epochRound", epochRound, "tcRound", timeoutCert.Round, "tempTCEpoch", tempTCEpoch)

	for epochBlockInfo.Round > timeoutCert.Round {
		tempTCEpoch--
		epochBlockInfo, err = x.GetBlockByEpochNumber(chain, tempTCEpoch)
		if err != nil {
			log.Error("[getTCEpochInfo] Error getting epoch block by epoch number", "epoch", tempTCEpoch, "error", err)
			return nil, fmt.Errorf("[getTCEpochInfo] failed to get epoch block info for epoch %d: %s", tempTCEpoch, err)
		}
		log.Debug("[getTCEpochInfo] walking back", "epochRound", epochBlockInfo.Round, "tcRound", timeoutCert.Round, "epoch", tempTCEpoch)
	}

	log.Info("[getTCEpochInfo] found", "epochRound", epochBlockInfo.Round, "tcRound", timeoutCert.Round, "epoch", tempTCEpoch)
	epochInfo, err := x.getEpochSwitchInfo(chain, nil, epochBlockInfo.Hash)
	if err != nil {
		log.Error("[getTCEpochInfo] Error getting epoch switch info by hash", "hash", epochBlockInfo.Hash, "error", err)
		return nil, fmt.Errorf("[getTCEpochInfo] failed to get epoch switch info: %s", err)
	}
	return epochInfo, nil
}

// verifyTC verifies a timeout certificate
func (x *XDPoS_v2) verifyTC(chain consensus.ChainReader, timeoutCert *types.TimeoutCert) error {
	if timeoutCert == nil || timeoutCert.Signatures == nil {
		log.Warn("[verifyTC] TC or signatures is nil")
		return utils.ErrInvalidTC
	}

	// Get snapshot
	snap, err := x.getSnapshot(chain, timeoutCert.GapNumber, true, nil)
	if err != nil {
		log.Error("[verifyTC] Failed to get snapshot",
			"gapNumber", timeoutCert.GapNumber,
			"error", err)
		return fmt.Errorf("unable to get snapshot: %s", err)
	}

	if snap == nil {
		log.Error("[verifyTC] Empty snapshot",
			"gapNumber", timeoutCert.GapNumber)
		return errors.New("empty masternode list")
	}
	// Use CandidatePool() (active set ∪ penalties) rather than NextEpochCandidates
	// alone so a currently-penalized masternode is still accepted for its own
	// timeout-certificate checks (matches canonical XDPoSChain).
	pool := snap.CandidatePool()
	if len(pool) == 0 {
		log.Error("[verifyTC] Empty snapshot",
			"gapNumber", timeoutCert.GapNumber)
		return errors.New("empty masternode list")
	}

	// A.98: Dedupe by signature bytes (matches canonical XDPoSChain timeout.go:143).
	// See issue #926 for full parity analysis.
	signedTimeoutObj := types.TimeoutSigHash(&types.TimeoutForSign{
		Round:     timeoutCert.Round,
		GapNumber: timeoutCert.GapNumber,
	})
	signatures, duplicates := UniqueSignatures(timeoutCert.Signatures)
	if len(duplicates) > 0 {
		for _, d := range duplicates {
			log.Warn("[verifyTC] Duplicate signature in TC",
				"signature", common.Bytes2Hex(d))
		}
	}
	// Fix #64: Use TC's round epoch, not current chain head epoch.
	epochInfo, err := x.getTCEpochInfo(chain, timeoutCert)
	if err != nil {
		log.Error("[verifyTC] Failed to get TC epoch info", "tcRound", timeoutCert.Round, "error", err)
		// Fallback to current header epoch if getTCEpochInfo fails (e.g. during initial sync)
		currentHeader := chain.CurrentHeader()
		epochInfo, err = x.getEpochSwitchInfo(chain, currentHeader, currentHeader.Hash())
		if err != nil {
			return fmt.Errorf("[verifyTC] unable to get epoch info: %s", err)
		}
		log.Warn("[verifyTC] Using fallback current epoch info", "tcRound", timeoutCert.Round)
	}

	// Use per-round config for cert threshold (fix #63 benefit)
	certThreshold := x.config.V2.Config(uint64(timeoutCert.Round)).CertThreshold
	// P0 fix #894 diagnostic: log masternodesLen at the threshold gate.
	log.Debug("[verifyTC] threshold gate",
		"tcRound", timeoutCert.Round,
		"masternodesLen", epochInfo.MasternodesLen,
		"threshold", float64(epochInfo.MasternodesLen)*certThreshold,
		"signatures", len(signatures))
	if float64(len(signatures)) < float64(epochInfo.MasternodesLen)*certThreshold {
		log.Warn("[verifyTC] Not enough signatures",
			"signatures", len(signatures),
			"threshold", float64(epochInfo.MasternodesLen)*certThreshold,
			"masternodesLen", epochInfo.MasternodesLen,
			"tcRound", timeoutCert.Round)
		return utils.ErrInvalidTCSignatures
	}

	// Verify signatures in parallel
	var wg sync.WaitGroup
	var mutex sync.Mutex
	var verifyError error

	wg.Add(len(signatures))
	for _, signature := range signatures {
		go func(sig types.Signature) {
			defer wg.Done()
			verified, _, err := x.verifyMsgSignature(signedTimeoutObj, sig, pool)
			if err != nil || !verified {
				mutex.Lock()
				if verifyError == nil {
					if err != nil {
						verifyError = fmt.Errorf("signature verification error: %s", err)
					} else {
						verifyError = errors.New("signature verification failed")
					}
				}
				mutex.Unlock()
			}
		}(signature)
	}
	wg.Wait()

	return verifyError
}

// processTC processes a timeout certificate
func (x *XDPoS_v2) processTC(chain consensus.ChainReader, timeoutCert *types.TimeoutCert) error {
	// Update highest TC
	if timeoutCert.Round > x.highestTimeoutCert.Round {
		x.highestTimeoutCert = timeoutCert
	}

	// Advance round if needed
	if timeoutCert.Round >= x.currentRound {
		x.setNewRound(chain, timeoutCert.Round+1)
	}

	return nil
}

// isEpochSwitchAtRound checks if a round is an epoch switch
// Aligned with v2.6.8: compares parent round with epoch start round, not just round%Epoch==0.
func (x *XDPoS_v2) isEpochSwitchAtRound(round types.Round, parentHeader *types.Header) (bool, uint64, error) {
	epochNum := x.config.V2.SwitchEpoch + uint64(round)/x.config.Epoch
	// if parent is last v1 block and this is first v2 block, this is treated as epoch switch
	if parentHeader.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
		return true, epochNum, nil
	}

	_, parentRound, err := x.getExtraFieldsNoChain(parentHeader)
	if err != nil {
		log.Error("[isEpochSwitchAtRound] decode header error", "err", err, "header", parentHeader, "extra", common.Bytes2Hex(parentHeader.Extra))
		return false, 0, err
	}
	if round <= parentRound {
		// this round is no larger than parentRound, should return false
		return false, epochNum, nil
	}

	epochStartRound := round - round%types.Round(x.config.Epoch)
	return parentRound < epochStartRound, epochNum, nil
}

// hygieneTimeoutPool cleans up old timeouts
func (x *XDPoS_v2) hygieneTimeoutPool() {
	x.lock.RLock()
	currentRound := x.currentRound
	x.lock.RUnlock()

	timeoutPoolKeys := x.timeoutPool.PoolObjKeysList()

	for _, k := range timeoutPoolKeys {
		keyedRound, err := strconv.ParseInt(strings.Split(k, ":")[0], 10, 64)
		if err != nil {
			log.Error("[hygieneTimeoutPool] Parse error", "error", err)
			continue
		}
		if keyedRound < int64(currentRound)-PoolHygieneRound {
			log.Debug("[hygieneTimeoutPool] Cleaning timeout pool",
				"round", keyedRound,
				"currentRound", currentRound)
			x.timeoutPool.ClearByPoolKey(k)
		}
	}
}

// ReceivedTimeouts returns all received timeouts
func (x *XDPoS_v2) ReceivedTimeouts() map[string]map[common.Hash]utils.PoolObj {
	return x.timeoutPool.Get()
}
