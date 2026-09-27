// Copyright (c) 2024 XDC Network
// Error definitions for XDPoS 2.0

package utils

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Common errors
var (
	ErrUnknownBlock       = errors.New("unknown block")
	ErrInvalidQC          = errors.New("invalid quorum certificate")
	ErrInvalidQCSignatures = errors.New("invalid QC signatures")
	ErrInvalidTC          = errors.New("invalid timeout certificate")
	ErrInvalidTCSignatures = errors.New("invalid TC signatures")
	ErrInvalidVote        = errors.New("invalid vote")
	ErrInvalidTimeout     = errors.New("invalid timeout")
	ErrNotReadyToMine     = errors.New("not ready to mine")
	ErrNotReadyToPropose  = errors.New("not ready to propose")
	ErrInvalidUncleHash   = errors.New("invalid uncle hash")
	ErrInvalidDifficulty  = errors.New("invalid difficulty")
	ErrInvalidV2Extra     = errors.New("invalid v2 extra data")
	ErrInvalidQuorumCert  = errors.New("invalid quorum certificate")
	ErrInvalidTimestamp   = errors.New("invalid timestamp")
	ErrRoundInvalid       = errors.New("invalid round")
	ErrInvalidCheckpointVote = errors.New("invalid checkpoint vote")
	ErrEmptyEpochSwitchValidators = errors.New("empty epoch switch validators")
	ErrInvalidCheckpointSigners = errors.New("invalid checkpoint signers")
	ErrValidatorsNotLegit = errors.New("validators not legit")
	ErrPenaltiesNotLegit  = errors.New("penalties not legit")
	ErrInvalidFieldInNonEpochSwitch = errors.New("invalid field in non-epoch switch")
	ErrValidatorNotWithinMasternodes = errors.New("validator not within masternodes")
	ErrCoinbaseAndValidatorMismatch = errors.New("coinbase and validator mismatch")
	ErrNotItsTurn         = errors.New("not its turn")
	ErrInvalidMixDigest   = errors.New("invalid mix digest")
	ErrNoValidatorSignatureV2 = errors.New("no validator signature v2")
)

// ErrIncomingMessageRoundTooFarFromCurrentRound is returned when a message's round
// is too far from the current round
type ErrIncomingMessageRoundTooFarFromCurrentRound struct {
	Type          string
	IncomingRound types.Round
	CurrentRound  types.Round
}

func (e *ErrIncomingMessageRoundTooFarFromCurrentRound) Error() string {
	return fmt.Sprintf("%s message round %d too far from current round %d",
		e.Type, e.IncomingRound, e.CurrentRound)
}

// ErrIncomingMessageRoundNotEqualCurrentRound is returned when a message's round
// doesn't match the current round
type ErrIncomingMessageRoundNotEqualCurrentRound struct {
	Type          string
	IncomingRound types.Round
	CurrentRound  types.Round
}

func (e *ErrIncomingMessageRoundNotEqualCurrentRound) Error() string {
	return fmt.Sprintf("%s message round %d not equal to current round %d",
		e.Type, e.IncomingRound, e.CurrentRound)
}

// ErrIncomingMessageBlockNotFound is returned when the block referenced by a message
// is not found
type ErrIncomingMessageBlockNotFound struct {
	Type                string
	IncomingBlockHash   common.Hash
	IncomingBlockNumber *big.Int
	Err                 error
}

func (e *ErrIncomingMessageBlockNotFound) Error() string {
	return fmt.Sprintf("%s message references unknown block %s (#%s): %v",
		e.Type, e.IncomingBlockHash.Hex(), e.IncomingBlockNumber, e.Err)
}

func (e *ErrIncomingMessageBlockNotFound) Unwrap() error {
	return e.Err
}

var ErrAlreadyMined = errors.New("already mined on this round")

// ErrEpochSwitchParentNotInDB is returned by getEpochSwitchInfo when the parent
// header is not in the database during bulk sync / checkpoint sync.  Callers
// (e.g. getEpochSwitchInfoWithParents / verifyQC) can use this to trigger
// checkpoint-boundary fallbacks.
var ErrEpochSwitchParentNotInDB = errors.New("epoch switch parent header not in DB")

// ErrV2SnapshotMissing is returned by getSnapshot when a V2 epoch's gap-block
// snapshot was never persisted because UpdateMasternodesFromHeader didn't run
// at gap-block import (typically because A.94 checkpoint-sync-no-state was
// active and skipped Finalize). Verifies callers (verifyHeader) can detect
// this sentinel and engage the header-anchored bulk-sync fallback even when
// fullVerify=true, since the local node cannot recompute the canonical
// masternode set without the gap-block state.
var ErrV2SnapshotMissing = errors.New("V2 snapshot missing — gap-block import skipped state")

// V1 engine errors (ported from v2.6.8)
// NOTE: Some errors below are intentionally duplicates of V2 errors above.
// They are kept for backward compatibility with V1 engine code.
var (
	ErrInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")
	ErrMissingVanity                = errors.New("missing vanity in header extra-data")
	ErrMissingSignature             = errors.New("missing signature in header extra-data")
	ErrExtraSigners                 = errors.New("non-checkpoint block contains extra signer list")
	ErrInvalidCheckpointPenalties   = errors.New("invalid penalty list on checkpoint block")
	ErrInvalidVotingChain           = errors.New("invalid voting chain")
	ErrUnauthorized                 = errors.New("unauthorized")
	ErrFailedDoubleValidation       = errors.New("failed double validation")
	ErrWaitTransactions             = errors.New("waiting for transactions")
)
