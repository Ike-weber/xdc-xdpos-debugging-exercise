// Copyright (c) 2024 XDC Network
// This file implements XDPoS 2.0 consensus types for BFT consensus.

package types

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// Round number type in XDPoS 2.0
type Round uint64

// Signature is a cryptographic signature
type Signature []byte

// DeepCopy creates a deep copy of the signature
func (s Signature) DeepCopy() Signature {
	if s == nil {
		return nil
	}
	cpy := make([]byte, len(s))
	copy(cpy, s)
	return cpy
}

// BlockInfo struct in XDPoS 2.0, used for vote message, QC, etc.
type BlockInfo struct {
	Hash   common.Hash `json:"hash"`
	Round  Round       `json:"round"`
	Number *big.Int    `json:"number"`
}

// DeepCopy creates a deep copy of BlockInfo
func (bi *BlockInfo) DeepCopy() *BlockInfo {
	if bi == nil {
		return nil
	}
	return &BlockInfo{
		Hash:   bi.Hash,
		Round:  bi.Round,
		Number: new(big.Int).Set(bi.Number),
	}
}

// Vote message in XDPoS 2.0
type Vote struct {
	signer            common.Address // field not exported
	ProposedBlockInfo *BlockInfo     `json:"proposedBlockInfo"`
	Signature         Signature      `json:"signature"`
	GapNumber         uint64         `json:"gapNumber"`
}

// DeepCopy creates a deep copy of Vote
func (v *Vote) DeepCopy() *Vote {
	if v == nil {
		return nil
	}
	return &Vote{
		signer:            v.signer,
		ProposedBlockInfo: v.ProposedBlockInfo.DeepCopy(),
		Signature:         v.Signature.DeepCopy(),
		GapNumber:         v.GapNumber,
	}
}

// Hash returns the hash of the vote
func (v *Vote) Hash() common.Hash {
	return rlpHash(v)
}

// PoolKey returns a unique key for the vote pool
func (v *Vote) PoolKey() string {
	return fmt.Sprint(v.ProposedBlockInfo.Round, ":", v.GapNumber, ":", v.ProposedBlockInfo.Number, ":", v.ProposedBlockInfo.Hash.Hex())
}

// GetSigner returns the signer address
func (v *Vote) GetSigner() common.Address {
	return v.signer
}

// SetSigner sets the signer address
func (v *Vote) SetSigner(signer common.Address) {
	v.signer = signer
}

// Timeout message in XDPoS 2.0
type Timeout struct {
	signer    common.Address
	Round     Round
	Signature Signature
	GapNumber uint64
}

// DeepCopy creates a deep copy of Timeout
func (t *Timeout) DeepCopy() *Timeout {
	if t == nil {
		return nil
	}
	return &Timeout{
		signer:    t.signer,
		Round:     t.Round,
		Signature: t.Signature.DeepCopy(),
		GapNumber: t.GapNumber,
	}
}

// Hash returns the hash of the timeout
func (t *Timeout) Hash() common.Hash {
	return rlpHash(t)
}

// PoolKey returns a unique key for the timeout pool
func (t *Timeout) PoolKey() string {
	return fmt.Sprint(t.Round, ":", t.GapNumber)
}

// GetSigner returns the signer address
func (t *Timeout) GetSigner() common.Address {
	return t.signer
}

// SetSigner sets the signer address
func (t *Timeout) SetSigner(signer common.Address) {
	t.signer = signer
}

// SyncInfo - BFT Sync Info message in XDPoS 2.0
type SyncInfo struct {
	HighestQuorumCert  *QuorumCert
	HighestTimeoutCert *TimeoutCert
}

// DeepCopy creates a deep copy of SyncInfo
func (s *SyncInfo) DeepCopy() *SyncInfo {
	if s == nil {
		return nil
	}
	return &SyncInfo{
		HighestQuorumCert:  s.HighestQuorumCert.DeepCopy(),
		HighestTimeoutCert: s.HighestTimeoutCert.DeepCopy(),
	}
}

// Hash returns the hash of SyncInfo
func (s *SyncInfo) Hash() common.Hash {
	return rlpHash(s)
}

// QuorumCert - Quorum Certificate struct in XDPoS 2.0
type QuorumCert struct {
	ProposedBlockInfo *BlockInfo  `json:"proposedBlockInfo"`
	Signatures        []Signature `json:"signatures"`
	GapNumber         uint64      `json:"gapNumber"`
}

// DeepCopy creates a deep copy of QuorumCert
func (qc *QuorumCert) DeepCopy() *QuorumCert {
	if qc == nil {
		return nil
	}
	sigsCopy := make([]Signature, len(qc.Signatures))
	for i, sig := range qc.Signatures {
		sigsCopy[i] = sig.DeepCopy()
	}
	return &QuorumCert{
		ProposedBlockInfo: qc.ProposedBlockInfo.DeepCopy(),
		Signatures:        sigsCopy,
		GapNumber:         qc.GapNumber,
	}
}

// TimeoutCert - Timeout Certificate struct in XDPoS 2.0
type TimeoutCert struct {
	Round      Round
	Signatures []Signature
	GapNumber  uint64
}

// DeepCopy creates a deep copy of TimeoutCert
func (tc *TimeoutCert) DeepCopy() *TimeoutCert {
	if tc == nil {
		return nil
	}
	sigsCopy := make([]Signature, len(tc.Signatures))
	for i, sig := range tc.Signatures {
		sigsCopy[i] = sig.DeepCopy()
	}
	return &TimeoutCert{
		Round:      tc.Round,
		Signatures: sigsCopy,
		GapNumber:  tc.GapNumber,
	}
}

// ExtraFields_v2 - The parsed extra fields in block header in XDPoS 2.0
// The version byte (consensus version) is the first byte in header's extra
// and it's only valid with value >= 2
type ExtraFields_v2 struct {
	Round      Round
	QuorumCert *QuorumCert
}

// EncodeToBytes encodes XDPoS 2.0 extra fields into bytes
func (e *ExtraFields_v2) EncodeToBytes() ([]byte, error) {
	bytes, err := rlp.EncodeToBytes(e)
	if err != nil {
		return nil, err
	}
	versionByte := []byte{2}
	return append(versionByte, bytes...), nil
}

// EpochSwitchInfo contains information about epoch switches
type EpochSwitchInfo struct {
	Penalties                  []common.Address
	Standbynodes               []common.Address
	Masternodes                []common.Address
	MasternodesLen             int
	EpochSwitchBlockInfo       *BlockInfo
	EpochSwitchParentBlockInfo *BlockInfo
}

// VoteForSign is the data structure used for vote signing
type VoteForSign struct {
	ProposedBlockInfo *BlockInfo
	GapNumber         uint64
}

// VoteSigHash returns the hash to be signed for a vote
func VoteSigHash(m *VoteForSign) common.Hash {
	return rlpHash(m)
}

// TimeoutForSign is the data structure used for timeout signing
type TimeoutForSign struct {
	Round     Round
	GapNumber uint64
}

// TimeoutSigHash returns the hash to be signed for a timeout
func TimeoutSigHash(m *TimeoutForSign) common.Hash {
	return rlpHash(m)
}

// ForensicsInfo contains QC chain info for forensics analysis
type ForensicsInfo struct {
	HashPath        []string   `json:"hashPath"`
	QuorumCert      QuorumCert `json:"quorumCert"`
	SignerAddresses []string   `json:"signerAddresses"`
}

// ForensicsContent describes the divergence found by forensics
type ForensicsContent struct {
	DivergingBlockNumber uint64         `json:"divergingBlockNumber"`
	DivergingBlockHash   string         `json:"divergingBlockHash"`
	AcrossEpoch          bool           `json:"acrossEpoch"`
	SmallerRoundInfo     *ForensicsInfo `json:"smallerRoundInfo"`
	LargerRoundInfo      *ForensicsInfo `json:"largerRoundInfo"`
}

// VoteEquivocationContent contains evidence of double-voting
type VoteEquivocationContent struct {
	SmallerRoundVote *Vote          `json:"smallerRoundVote"`
	LargerRoundVote  *Vote          `json:"largerRoundVote"`
	Signer           common.Address `json:"signer"`
}

// ForensicProof contains evidence of misbehavior
type ForensicProof struct {
	Id            string `json:"id"`
	ForensicsType string `json:"forensicsType"` // QC or VOTE
	Content       string `json:"content"`       // Json string of the forensics data
}

// ForensicsEvent is emitted when a forensics issue is detected
type ForensicsEvent struct {
	ForensicsProof *ForensicProof
}
