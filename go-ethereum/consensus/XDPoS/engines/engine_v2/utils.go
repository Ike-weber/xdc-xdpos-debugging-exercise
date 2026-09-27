// Copyright (c) 2024 XDC Network
// Utility functions for XDPoS 2.0

package engine_v2

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

// signSignature signs a hash with the node's private key
func (x *XDPoS_v2) signSignature(signingHash common.Hash) (types.Signature, error) {
	x.signLock.RLock()
	signer, signFn := x.signer, x.signFn
	x.signLock.RUnlock()

	if signFn == nil {
		return nil, errors.New("signFn is nil")
	}

	signedHash, err := signFn(accounts.Account{Address: signer}, signingHash.Bytes())
	if err != nil {
		return nil, fmt.Errorf("error signing hash: %v", err)
	}
	return signedHash, nil
}

// verifyMsgSignature verifies a signature against a list of masternodes
func (x *XDPoS_v2) verifyMsgSignature(signedHashToBeVerified common.Hash, signature types.Signature, masternodes []common.Address) (bool, common.Address, error) {
	var signerAddress common.Address

	if len(masternodes) == 0 {
		return false, signerAddress, errors.New("empty masternode list")
	}

	// Recover public key
	pubkey, err := crypto.Ecrecover(signedHashToBeVerified.Bytes(), signature)
	if err != nil {
		return false, signerAddress, fmt.Errorf("ecrecover error: %v", err)
	}

	copy(signerAddress[:], crypto.Keccak256(pubkey[1:])[12:])

	// Check if signer is in masternode list
	for _, mn := range masternodes {
		if mn == signerAddress {
			return true, signerAddress, nil
		}
	}

	log.Warn("[verifyMsgSignature] Signer not in masternode list",
		"signer", signerAddress,
		"masternodes", len(masternodes))
	return false, signerAddress, nil
}

// RecoverUniqueSigners recovers unique signers from a list of signatures
func RecoverUniqueSigners(signedHash common.Hash, signatureList []types.Signature) ([]types.Signature, []types.Signature, error) {
	if signedHash == (common.Hash{}) {
		return nil, nil, errors.New("signedHash cannot be empty")
	}
	if len(signatureList) == 0 {
		return []types.Signature{}, []types.Signature{}, nil
	}

	type Message struct {
		pubkey common.Address
		sig    types.Signature
	}

	result := make(chan Message, len(signatureList))
	errCh := make(chan error, len(signatureList))
	var wg sync.WaitGroup
	wg.Add(len(signatureList))

	for _, signature := range signatureList {
		go func(sig types.Signature) {
			defer wg.Done()
			pubkey, err := crypto.Ecrecover(signedHash.Bytes(), sig)
			if err != nil {
				log.Error("[RecoverUniqueSigners] ecrecover error",
					"error", err,
					"signature", common.Bytes2Hex(sig))
				errCh <- err
				return
			}
			var signerAddress common.Address
			copy(signerAddress[:], crypto.Keccak256(pubkey[1:])[12:])
			result <- Message{pubkey: signerAddress, sig: sig}
		}(signature)
	}
	wg.Wait()
	close(result)
	close(errCh)

	if len(errCh) > 0 {
		return nil, nil, <-errCh
	}

	keys := make(map[string]struct{})
	uniqueSigners := make([]types.Signature, 0, len(result))
	duplicates := make([]types.Signature, 0)

	for r := range result {
		pubkeyHex := r.pubkey.Hex()
		if _, ok := keys[pubkeyHex]; !ok {
			keys[pubkeyHex] = struct{}{}
			uniqueSigners = append(uniqueSigners, r.sig)
		} else {
			log.Warn("[RecoverUniqueSigners] Duplicate signature found",
				"pubkey", pubkeyHex,
				"signedHash", signedHash.Hex())
			duplicates = append(duplicates, r.sig)
		}
	}

	return uniqueSigners, duplicates, nil
}

// verifyQC verifies a quorum certificate
// inCheckpointCatchup reports whether the given block number falls within the
// "catchup window" after a trusted-checkpoint anchor — i.e. the chain was started
// with --syncfromblock against a hardcoded TrustedSyncCheckpoint, the pre-anchor
// parent chain isn't downloaded yet, and round-based QC verification can't walk
// back to find the real epoch switch. While the chain is in this window, V2
// consensus checks that depend on parent-chain history (verifyQC gap math,
// commitBlocks 3-chain rule) are skipped — the trusted hardcoded hash IS the
// trust anchor, so headers chained off it are canonical by definition. Once the
// chain has ingested 2x epoch worth of blocks past the anchor, real V2 epoch
// switch headers are in the DB and strict verification resumes.
func (x *XDPoS_v2) inCheckpointCatchup(chain consensus.ChainReader, blockNum uint64) bool {
	type ckptAware interface {
		GetTrustedCheckpointAnchor() (uint64, common.Hash, bool)
	}
	ca, ok := chain.(ckptAware)
	if !ok {
		return false
	}
	anchor, _, active := ca.GetTrustedCheckpointAnchor()
	if !active || anchor == 0 {
		return false
	}
	// Symmetric 2x-epoch window: BeaconSync walks BACK from the pivot to
	// download historical headers before walking forward to state. Both
	// directions need the same QC-skip relaxation while the pre-anchor
	// epoch-switch headers haven't been ingested yet.
	//
	// A.60.5 follow-up (refs #857): the original check `blockNum >= anchor`
	// only covered the forward walk; verifyHeader was still firing on
	// every pre-anchor block (e.g. block anchor-1 with QC referencing
	// anchor-2). With BeaconSync's reverse-walk pattern from operator-
	// pinned pivot, those pre-anchor blocks dominate the verification
	// volume and the failure cancels every sync attempt.
	window := x.config.Epoch * 2
	lo := uint64(0)
	if anchor > window {
		lo = anchor - window
	}
	return blockNum >= lo && blockNum <= anchor+window
}

func (x *XDPoS_v2) verifyQC(chain consensus.ChainReader, quorumCert *types.QuorumCert, parentHeader *types.Header, parents []*types.Header) error {
	if quorumCert == nil {
		log.Warn("[verifyQC] QC is nil")
		return utils.ErrInvalidQC
	}

	// Get epoch info (use parents fallback for bulk sync across epoch boundaries)
	epochInfo, err := x.getEpochSwitchInfoWithParents(chain, parentHeader, quorumCert.ProposedBlockInfo.Hash, parents)
	if err != nil {
		log.Error("[verifyQC] Failed to get epoch info", "error", err)
		return errors.New("failed to get epoch switch info for QC verification")
	}

	// Verify signature hash
	signedVoteObj := types.VoteSigHash(&types.VoteForSign{
		ProposedBlockInfo: quorumCert.ProposedBlockInfo,
		GapNumber:         quorumCert.GapNumber,
	})

	// A.98: Dedupe by signature bytes (matches canonical XDPoSChain).
	// RecoverUniqueSigners ran Ecrecover purely to dedupe by address, then
	// verifyMsgSignature re-Ecrecovered every sig — 2× Ecrecover per sig on
	// the hot verify path. UniqueSignatures avoids the dedup-time Ecrecover.
	// Parity note: canonical XDPoSChain (utils.go:78) dedupes by bytes;
	// address-dedup (RecoverUniqueSigners) collapsed ECDSA-malleated pairs
	// that share an address, whereas byte-dedup counts them as distinct — the
	// canonical behaviour. See issue #926 for full analysis.
	signatures, duplicates := UniqueSignatures(quorumCert.Signatures)

	if len(duplicates) > 0 {
		for _, d := range duplicates {
			log.Warn("[verifyQC] Duplicate signature in QC",
				"signature", common.Bytes2Hex(d))
		}
	}

	// Check threshold using per-round config (fix #63)
	qcRound := quorumCert.ProposedBlockInfo.Round
	certThreshold := x.config.V2.Config(uint64(qcRound)).CertThreshold
	// P0 fix #894 diagnostic: log masternodesLen so the live gate can confirm
	// that the synthetic path now uses the active count (32), not the pool (46).
	log.Debug("[verifyQC] threshold gate",
		"qcRound", qcRound,
		"masternodesLen", epochInfo.MasternodesLen,
		"threshold", float64(epochInfo.MasternodesLen)*certThreshold,
		"signatures", len(signatures))
	if qcRound > 0 && (signatures == nil || float64(len(signatures)) < float64(epochInfo.MasternodesLen)*certThreshold) {
		log.Warn("[verifyQC] Not enough signatures",
			"signatures", len(signatures),
			"threshold", float64(epochInfo.MasternodesLen)*certThreshold,
			"masternodesLen", epochInfo.MasternodesLen,
			"qcRound", qcRound)
		return utils.ErrInvalidQCSignatures
	}

	// Verify each signature
	var wg sync.WaitGroup
	var mutex sync.Mutex
	var verifyError error

	wg.Add(len(signatures))
	for _, sig := range signatures {
		go func(signature types.Signature) {
			defer wg.Done()
			verified, _, err := x.verifyMsgSignature(signedVoteObj, signature, epochInfo.Masternodes)
			if err != nil {
				mutex.Lock()
				if verifyError == nil {
					log.Error("[verifyQC] Signature verification error", "error", err)
					verifyError = errors.New("QC signature verification error")
				}
				mutex.Unlock()
				return
			}
			if !verified {
				mutex.Lock()
				if verifyError == nil {
					log.Warn("[verifyQC] Signature not verified")
					verifyError = errors.New("QC signature verification failed")
				}
				mutex.Unlock()
			}
		}(sig)
	}
	wg.Wait()

	if verifyError != nil {
		return verifyError
	}

	// Verify gap number
	// The QC is signed by masternodes of the epoch containing the QC's proposed block.
	// We must calculate the gap from THAT epoch's switch block.
	// CRITICAL: Use getEpochSwitchInfo (which walks the chain and checks round-based
	// IsEpochSwitch) instead of a block-number formula. Epochs are 900 ROUNDS, not
	// 900 blocks; timeouts can make an epoch span more than 900 blocks.
	// A block-number formula (like SwitchBlock + floor((N-SwitchBlock)/Epoch)*Epoch)
	// assumes epochs are exactly Epoch blocks long, which is wrong.
	epochSwitchNumber := epochInfo.EpochSwitchBlockInfo.Number.Uint64()
	gapNumber := epochSwitchNumber - epochSwitchNumber%x.config.Epoch
	if gapNumber > x.config.Gap {
		gapNumber -= x.config.Gap
	} else {
		gapNumber = 0
	}
	log.Debug("[verifyQC] gap check", "qcBlockNum", quorumCert.ProposedBlockInfo.Number.Uint64(), "epochSwitchNumber", epochSwitchNumber, "calculatedGap", gapNumber, "qcGap", quorumCert.GapNumber, "qcBlockRound", quorumCert.ProposedBlockInfo.Round)
	if gapNumber != quorumCert.GapNumber {
		// #45: The locally-computed epoch-switch number can be WRONG when the QC's
		// proposed block ancestors are not yet in the local DB. During reverse-walk
		// BeaconSync a validator (which runs FULL QC verification) reaches verifyQC for
		// a block whose parent chain has not been imported yet, so getEpochSwitchInfo
		// cannot walk back to the real epoch-switch block and falls back to the
		// synthetic "parent missing" path (engine.go), which returns the proposed
		// block's OWN number as EpochSwitchBlockInfo.Number. When that proposed block
		// sits past a block-number*Epoch boundary relative to its real epoch switch
		// (e.g. a gap block, number%Epoch==Gap), flooring the wrong number yields a gap
		// exactly one Epoch too high — a false mismatch that halts the validator, even
		// though the chain is valid (observed net5050: proposed 91350 round 89017,
		// synthetic epochSwitchNumber=91350 -> computed 90450 vs QC's correct 89550).
		//
		// Authoritative cross-check (mirrors verifyTC): the QC's GapNumber must index a
		// snapshot whose masternode set matches the set that verified this QC's
		// signatures above. This is safe because the QC signatures already commit to
		// GapNumber (VoteForSign embeds it) and were verified against epochInfo.Masternodes,
		// so an attacker cannot substitute a different gap without the masternodes' keys;
		// a forged or genuinely-wrong gap selects a different/absent snapshot and is
		// still rejected below.
		if snap, snapErr := x.getSnapshot(chain, quorumCert.GapNumber, true, parents); snapErr == nil &&
			snap != nil && utils.CompareSignersLists(snap.activeSet(), epochInfo.Masternodes) {
			log.Warn("[verifyQC] block-number gap check disagreed, but QC gap indexes a snapshot matching the verified signer set; accepting (epoch switch not locally resolvable)",
				"computed", gapNumber,
				"qcGap", quorumCert.GapNumber,
				"epochSwitchNumber", epochSwitchNumber,
				"qcBlockNum", quorumCert.ProposedBlockInfo.Number.Uint64(),
				"qcBlockRound", quorumCert.ProposedBlockInfo.Round)
		} else {
			log.Error("[verifyQC] Gap number mismatch",
				"expected", gapNumber,
				"got", quorumCert.GapNumber,
				"snapErr", snapErr)
			return fmt.Errorf("gap number mismatch: expected %d, got %d", gapNumber, quorumCert.GapNumber)
		}
	}

	// Verify block info
	return x.VerifyBlockInfo(chain, quorumCert.ProposedBlockInfo, parentHeader)
}

// processQC processes a quorum certificate
func (x *XDPoS_v2) processQC(chain consensus.ChainReader, incomingQuorumCert *types.QuorumCert) error {
	log.Trace("[processQC] Processing", "highestQC", x.highestQuorumCert)

	// Update highest QC
	if incomingQuorumCert.ProposedBlockInfo.Round > x.highestQuorumCert.ProposedBlockInfo.Round {
		log.Debug("[processQC] Updating highest QC",
			"blockNum", incomingQuorumCert.ProposedBlockInfo.Number,
			"round", incomingQuorumCert.ProposedBlockInfo.Round,
			"hash", incomingQuorumCert.ProposedBlockInfo.Hash)
		x.highestQuorumCert = incomingQuorumCert
	}

	// Get proposed block header
	proposedBlockHeader := chain.GetHeaderByHash(incomingQuorumCert.ProposedBlockInfo.Hash)
	if proposedBlockHeader == nil {
		log.Warn("[processQC] Proposed block not yet available, skipping QC processing",
			"hash", incomingQuorumCert.ProposedBlockInfo.Hash,
			"number", incomingQuorumCert.ProposedBlockInfo.Number)
		return nil
	}

	// Update lock QC for blocks after V2 switch
	if proposedBlockHeader.Number.Cmp(x.config.V2.SwitchBlock) > 0 {
		proposedBlockQuorumCert, round, _, err := x.getExtraFields(chain, proposedBlockHeader)
		if err != nil {
			return err
		}
		if x.lockQuorumCert == nil || (proposedBlockQuorumCert != nil && proposedBlockQuorumCert.ProposedBlockInfo.Round > x.lockQuorumCert.ProposedBlockInfo.Round) {
			x.lockQuorumCert = proposedBlockQuorumCert
		}

		// Commit blocks (3-chain rule)
		_, err = x.commitBlocks(chain, proposedBlockHeader, &round, incomingQuorumCert)
		if err != nil {
			log.Error("[processQC] commitBlocks error", "round", round)
			return err
		}
	}

	// Advance round
	if incomingQuorumCert.ProposedBlockInfo.Round >= x.currentRound {
		x.setNewRound(chain, incomingQuorumCert.ProposedBlockInfo.Round+1)
	}

	log.Trace("[processQC] Complete", "highestQC", x.highestQuorumCert)
	return nil
}

// commitBlocks implements the 3-chain commit rule
func (x *XDPoS_v2) commitBlocks(chain consensus.ChainReader, proposedBlockHeader *types.Header, proposedBlockRound *types.Round, incomingQc *types.QuorumCert) (bool, error) {
	// Skip if too close to V2 switch
	if proposedBlockHeader.Number.Int64()-2 <= x.config.V2.SwitchBlock.Int64() {
		return false, nil
	}
	// Skip if within trusted-checkpoint catchup window — the parent chain isn't
	// downloaded so the 3-chain commit rule's grandparent walk would always fail.
	if x.inCheckpointCatchup(chain, proposedBlockHeader.Number.Uint64()) {
		return false, nil
	}

	// Get parent block
	parentBlock := chain.GetHeaderByHash(proposedBlockHeader.ParentHash)
	if parentBlock == nil {
		log.Error("[commitBlocks] Parent not found", "hash", proposedBlockHeader.ParentHash)
		return false, fmt.Errorf("parent not found: %s", proposedBlockHeader.ParentHash.Hex())
	}

	_, round, _, err := x.getExtraFields(chain, parentBlock)
	if err != nil {
		log.Error("[commitBlocks] Failed to decode parent extra", "hash", proposedBlockHeader.Hash())
		return false, err
	}

	// Check if parent round is continuous
	if *proposedBlockRound-1 != round {
		log.Info("[commitBlocks] Parent round not continuous",
			"proposedRound", *proposedBlockRound,
			"parentRound", round)
		return false, nil
	}

	// Get grandparent block
	grandParentBlock := chain.GetHeaderByHash(parentBlock.ParentHash)
	if grandParentBlock == nil {
		log.Error("[commitBlocks] Grandparent not found", "hash", parentBlock.ParentHash)
		return false, fmt.Errorf("grandparent not found: %s", parentBlock.ParentHash.Hex())
	}

	_, round, _, err = x.getExtraFields(chain, grandParentBlock)
	if err != nil {
		log.Error("[commitBlocks] Failed to decode grandparent extra", "hash", parentBlock.Hash())
		return false, err
	}

	// Check if grandparent round is continuous
	if *proposedBlockRound-2 != round {
		log.Info("[commitBlocks] Grandparent round not continuous",
			"proposedRound", *proposedBlockRound,
			"grandparentRound", round)
		return false, nil
	}

	// Check if already committed
	if x.highestCommitBlock != nil &&
		(x.highestCommitBlock.Round >= round || x.highestCommitBlock.Number.Cmp(grandParentBlock.Number) >= 0) {
		return false, nil
	}

	// Commit grandparent
	x.highestCommitBlock = &types.BlockInfo{
		Number: grandParentBlock.Number,
		Hash:   grandParentBlock.Hash(),
		Round:  round,
	}
	log.Info("Block committed (3-chain rule)",
		"number", x.highestCommitBlock.Number,
		"round", x.highestCommitBlock.Round,
		"hash", x.highestCommitBlock.Hash)

	// Notify blockchain of the newly finalized block (#95).
	// This wires highestCommitBlock to geth's finality tracking so that
	// eth_getFinalizedBlock / eth_getBlockByNumber("finalized") reflect real BFT finality.
	if x.HookCommitBlock != nil {
		x.HookCommitBlock(grandParentBlock)
	}

	return true, nil
}

// VerifyBlockInfo verifies block info against the chain
func (x *XDPoS_v2) VerifyBlockInfo(chain consensus.ChainReader, blockInfo *types.BlockInfo, blockHeader *types.Header) error {
	if blockHeader == nil {
		blockHeader = chain.GetHeaderByHash(blockInfo.Hash)
		if blockHeader == nil {
			log.Warn("[VerifyBlockInfo] Header not found",
				"hash", blockInfo.Hash,
				"number", blockInfo.Number)
			return fmt.Errorf("header not found: %s", blockInfo.Hash.Hex())
		}
	} else {
		if blockHeader.Hash() != blockInfo.Hash {
			log.Warn("[VerifyBlockInfo] Hash mismatch",
				"blockInfoHash", blockInfo.Hash,
				"headerHash", blockHeader.Hash())
			return errors.New("header hash mismatch")
		}
	}

	// Verify block number
	if blockHeader.Number.Cmp(blockInfo.Number) != 0 {
		log.Warn("[VerifyBlockInfo] Number mismatch",
			"blockInfoNumber", blockInfo.Number,
			"headerNumber", blockHeader.Number)
		return fmt.Errorf("block number mismatch")
	}

	// V2 switch block has round 0
	if blockInfo.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
		if blockInfo.Round != 0 {
			log.Error("[VerifyBlockInfo] Switch block round not 0",
				"round", blockInfo.Round)
			return errors.New("switch block round must be 0")
		}
		return nil
	}

	// Verify round
	_, round, _, err := x.getExtraFields(chain, blockHeader)
	if err != nil {
		log.Error("[VerifyBlockInfo] Failed to decode extra", "error", err)
		return err
	}
	if round != blockInfo.Round {
		log.Warn("[VerifyBlockInfo] Round mismatch",
			"blockInfoRound", blockInfo.Round,
			"headerRound", round)
		return fmt.Errorf("round mismatch: expected %d, got %d", blockInfo.Round, round)
	}

	return nil
}

// VerifySyncInfoMessage verifies a sync info message
func (x *XDPoS_v2) VerifySyncInfoMessage(chain consensus.ChainReader, syncInfo *types.SyncInfo) (bool, error) {
	// Check if we need to update
	if x.highestQuorumCert.ProposedBlockInfo.Round >= syncInfo.HighestQuorumCert.ProposedBlockInfo.Round &&
		x.highestTimeoutCert.Round >= syncInfo.HighestTimeoutCert.Round {
		log.Debug("[VerifySyncInfoMessage] SyncInfo not newer",
			"localQCRound", x.highestQuorumCert.ProposedBlockInfo.Round,
			"incomingQCRound", syncInfo.HighestQuorumCert.ProposedBlockInfo.Round,
			"localTCRound", x.highestTimeoutCert.Round,
			"incomingTCRound", syncInfo.HighestTimeoutCert.Round)
		return false, nil
	}

	// Verify QC
	if err := x.verifyQC(chain, syncInfo.HighestQuorumCert, nil, nil); err != nil {
		log.Warn("[VerifySyncInfoMessage] QC verification failed",
			"blockNum", syncInfo.HighestQuorumCert.ProposedBlockInfo.Number,
			"error", err)
		return false, err
	}

	// Verify TC
	if err := x.verifyTC(chain, syncInfo.HighestTimeoutCert); err != nil {
		log.Warn("[VerifySyncInfoMessage] TC verification failed",
			"round", syncInfo.HighestTimeoutCert.Round,
			"error", err)
		return false, err
	}

	return true, nil
}

// SyncInfoHandler processes a sync info message
func (x *XDPoS_v2) SyncInfoHandler(chain consensus.ChainReader, syncInfo *types.SyncInfo) error {
	x.lock.Lock()
	defer x.lock.Unlock()

	// Process QC
	if err := x.processQC(chain, syncInfo.HighestQuorumCert); err != nil {
		return err
	}

	// Process TC
	return x.processTC(chain, syncInfo.HighestTimeoutCert)
}

// ProposedBlockHandler processes a proposed block
func (x *XDPoS_v2) ProposedBlockHandler(chain consensus.ChainReader, blockHeader *types.Header) error {
	x.lock.Lock()
	defer x.lock.Unlock()

	// Get QC and round from header
	quorumCert, round, _, err := x.getExtraFields(chain, blockHeader)
	if err != nil {
		return err
	}

	// Generate block info
	blockInfo := &types.BlockInfo{
		Hash:   blockHeader.Hash(),
		Round:  round,
		Number: blockHeader.Number,
	}

	// Process QC
	if err := x.processQC(chain, quorumCert); err != nil {
		log.Error("[ProposedBlockHandler] processQC error",
			"round", quorumCert.ProposedBlockInfo.Round,
			"hash", quorumCert.ProposedBlockInfo.Hash)
		return err
	}

	// Check if we can vote
	if !x.allowedToSend(chain, blockHeader, "vote") {
		return nil
	}

	// Verify voting rule
	verified, err := x.verifyVotingRule(chain, blockInfo, quorumCert)
	if err != nil {
		return err
	}
	if verified {
		return x.sendVote(chain, blockInfo)
	}

	return nil
}

// GetRoundNumber returns the round number from a header
func (x *XDPoS_v2) GetRoundNumber(header *types.Header) (types.Round, error) {
	if header.Number.Cmp(x.config.V2.SwitchBlock) <= 0 {
		return types.Round(0), nil
	}
	var decodedExtra types.ExtraFields_v2
	if err := DecodeExtraFields(header.Extra, &decodedExtra); err != nil {
		return types.Round(0), err
	}
	return decodedExtra.Round, nil
}

// GetSignersFromSnapshot returns signers from the snapshot
func (x *XDPoS_v2) GetSignersFromSnapshot(chain consensus.ChainReader, header *types.Header) ([]common.Address, error) {
	snap, err := x.getSnapshot(chain, header.Number.Uint64(), false, nil)
	if err != nil {
		return nil, err
	}
	return snap.NextEpochCandidates, nil
}

// UniqueSignatures deduplicates signatures by their byte content.
// Returns unique signatures and any duplicates found.
// Matches v2.6.8 engines/engine_v2/utils.go conceptually.
func UniqueSignatures(signatureSlice []types.Signature) ([]types.Signature, []types.Signature) {
	seen := make(map[common.Hash]bool)
	unique := make([]types.Signature, 0, len(signatureSlice))
	duplicates := make([]types.Signature, 0)

	for _, sig := range signatureSlice {
		sigHash := common.BytesToHash(sig)
		if seen[sigHash] {
			duplicates = append(duplicates, sig)
		} else {
			seen[sigHash] = true
			unique = append(unique, sig)
		}
	}
	return unique, duplicates
}

// CalculateMissingRounds computes which consensus rounds were missed in the current epoch.
// Used by the penalty system to identify masternodes that failed to produce blocks.
// Matches v2.6.8 engines/engine_v2/utils.go.
func (x *XDPoS_v2) CalculateMissingRounds(chain consensus.ChainReader, header *types.Header) (*utils.PublicApiMissedRoundsMetadata, error) {
	epochInfo, err := x.getEpochSwitchInfo(chain, header, header.Hash())
	if err != nil {
		return nil, err
	}

	return &utils.PublicApiMissedRoundsMetadata{
		EpochRound:       epochInfo.EpochSwitchBlockInfo.Round,
		EpochBlockNumber: epochInfo.EpochSwitchBlockInfo.Number,
	}, nil
}

// GetBlockByEpochNumber returns the block info for a given epoch number.
// Uses cache-first lookup, then binary search for efficient retrieval.
// Matches v2.6.8 engines/engine_v2/utils.go.
func (x *XDPoS_v2) GetBlockByEpochNumber(chain consensus.ChainReader, targetEpochNum uint64) (*types.BlockInfo, error) {
	currentHeader := chain.CurrentHeader()
	if currentHeader == nil {
		return nil, errors.New("no current header")
	}

	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, currentHeader, currentHeader.Hash())
	if err != nil {
		return nil, err
	}
	epochNum := x.config.V2.SwitchEpoch + uint64(epochSwitchInfo.EpochSwitchBlockInfo.Round)/x.config.Epoch
	// if current epoch is this epoch, we early return the result
	if targetEpochNum == epochNum {
		return epochSwitchInfo.EpochSwitchBlockInfo, nil
	}
	if targetEpochNum > epochNum {
		return nil, errors.New("input epoch number > current epoch number")
	}
	if targetEpochNum < x.config.V2.SwitchEpoch {
		return nil, errors.New("input epoch number < v2 begin epoch number")
	}
	// the block's round should be in [estRound, estRound+Epoch-1]
	estRound := types.Round((targetEpochNum - x.config.V2.SwitchEpoch) * x.config.Epoch)
	// check the round2epochBlockInfo cache
	blockInfo := x.getBlockByEpochNumberInCache(chain, estRound)
	if blockInfo != nil {
		return blockInfo, nil
	}
	// if cache miss, we do search
	estBlockNumDiff := new(big.Int).Mul(big.NewInt(int64(x.config.Epoch)), big.NewInt(int64(epochNum-targetEpochNum)))
	estBlockNum := new(big.Int).Sub(epochSwitchInfo.EpochSwitchBlockInfo.Number, estBlockNumDiff)
	if estBlockNum.Cmp(x.config.V2.SwitchBlock) == -1 {
		estBlockNum.Set(x.config.V2.SwitchBlock)
	}
	// if the target is close, we search brute-forcily
	closeEpochNum := uint64(2)
	if closeEpochNum >= epochNum-targetEpochNum {
		estBlockHeader := chain.GetHeaderByNumber(estBlockNum.Uint64())
		if estBlockHeader == nil {
			return nil, fmt.Errorf("fail to get est block header by number: %v", estBlockNum)
		}
		epochSwitchInfos, err := x.GetEpochSwitchInfoBetween(chain, estBlockHeader, currentHeader)
		if err != nil {
			return nil, err
		}
		for _, info := range epochSwitchInfos {
			epochNum := x.config.V2.SwitchEpoch + uint64(info.EpochSwitchBlockInfo.Round)/x.config.Epoch
			if epochNum == targetEpochNum {
				return info.EpochSwitchBlockInfo, nil
			}
		}
	}
	// else, we use binary search
	info, _, err := x.binarySearchBlockByEpochNumber(chain, targetEpochNum, estBlockNum.Uint64(), epochSwitchInfo.EpochSwitchBlockInfo.Number.Uint64())
	return info, err
}

// getBlockByEpochNumberInCache checks the round2epochBlockInfo cache for a block at the estimated round.
func (x *XDPoS_v2) getBlockByEpochNumberInCache(chain consensus.ChainReader, estRound types.Round) *types.BlockInfo {
	epochSwitchInCache := make([]*types.BlockInfo, 0)
	for r := estRound; r < estRound+types.Round(x.config.Epoch); r++ {
		blockInfo, ok := x.round2epochBlockInfo.Get(r)
		if ok && blockInfo != nil {
			epochSwitchInCache = append(epochSwitchInCache, blockInfo)
		}
	}
	if len(epochSwitchInCache) == 1 {
		return epochSwitchInCache[0]
	} else if len(epochSwitchInCache) == 0 {
		return nil
	}
	// when multiple cache hits, need to find the one in main chain
	for _, blockInfo := range epochSwitchInCache {
		header := chain.GetHeaderByNumber(blockInfo.Number.Uint64())
		if header == nil {
			continue
		}
		if header.Hash() == blockInfo.Hash {
			return blockInfo
		}
	}
	return nil
}

// binarySearchBlockByEpochNumber performs binary search to find the block at a specific epoch boundary.
func (x *XDPoS_v2) binarySearchBlockByEpochNumber(chain consensus.ChainReader, targetEpochNum uint64, start, end uint64) (*types.BlockInfo, *types.Header, error) {
	for start <= end {
		mid := (start + end) / 2
		header := chain.GetHeaderByNumber(mid)
		if header == nil {
			return nil, nil, fmt.Errorf("header not found at %d", mid)
		}

		isEpochSwitch, epochNum, err := x.IsEpochSwitch(header)
		if err != nil {
			return nil, nil, err
		}

		if isEpochSwitch && epochNum == targetEpochNum {
			round, _ := x.GetRoundNumber(header)
			return &types.BlockInfo{
				Number: header.Number,
				Hash:   header.Hash(),
				Round:  round,
			}, header, nil
		}

		if epochNum < targetEpochNum {
			start = mid + 1
		} else {
			if mid == 0 {
				break
			}
			end = mid - 1
		}
	}
	return nil, nil, fmt.Errorf("epoch %d not found in range", targetEpochNum)
}
