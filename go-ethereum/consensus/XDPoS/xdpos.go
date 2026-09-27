// Copyright (c) 2018 XDCchain
// Copyright 2024 The go-ethereum Authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

// Package XDPoS implements the XDC Network's delegated proof-of-stake consensus engine.
// This is a port from XDPoSChain (geth 1.8.x) to modern go-ethereum (1.17.x).
//
// XDPoS is a DPoS (Delegated Proof of Stake) consensus mechanism where masternodes
// take turns producing blocks in a round-robin fashion within epochs.
//
// Key concepts:
// - Epoch: A fixed number of blocks after which the masternode list may change
// - Masternode: Authorized nodes that can produce blocks
// - Gap: Blocks before epoch end to prepare next epoch's masternode list
// - Validator: Double validation - each block needs creator + validator signatures
//
// This implementation includes:
// - Hook system for rewards, penalties, and validator management
// - Block signer caching for transaction signing
// - Double validation support
// - Penalty system for misbehaving masternodes
package XDPoS

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/engines/engine_v2"
	"github.com/ethereum/go-ethereum/core"
	cbal "github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"golang.org/x/crypto/sha3"
)

const (
	inmemorySnapshots      = 2048      // Number of recent vote snapshots to keep in memory (v8.2: increased from 128 to cover 2+ epochs)
	inmemorySignatures     = 4096      // Number of recent block signatures to keep in memory
	blockSignersCacheLimit = 9000      // Cache limit for block signers
	snapshotMaxWalkBack    = 2_000_000 // Safety limit to prevent OOM when disk snapshots are missing
	M2ByteLength           = 4         // Length of M2 validator index bytes

	extraVanity = 32                     // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = crypto.SignatureLength // Fixed number of extra-data suffix bytes reserved for signer seal (65 bytes)
)

// Masternode represents a masternode with its address and stake
type Masternode struct {
	Address common.Address
	Stake   *big.Int
}

// XDPoS proof-of-stake-voting protocol constants.
var (
	defaultEpochLength = uint64(900) // Default number of blocks per epoch
	defaultPeriod      = uint64(2)   // Default block time in seconds
	defaultGap         = uint64(450) // Default gap blocks before epoch switch

	nonceAuthVote = hexutil.MustDecode("0xffffffffffffffff") // Magic nonce number to vote on adding a new signer
	nonceDropVote = hexutil.MustDecode("0x0000000000000000") // Magic nonce number to vote on removing a signer.

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

	diffInTurn = big.NewInt(2) // Block difficulty for in-turn signatures
	diffNoTurn = big.NewInt(1) // Block difficulty for out-of-turn signatures
)

// Checkpoint cache for sync - stores masternode lists from validated checkpoint headers
var (
	checkpointCache   = make(map[uint64][]common.Address)
	checkpointCacheMu sync.RWMutex
)

// Various error messages to mark blocks invalid.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errInvalidCheckpointBeneficiary is returned if a checkpoint/epoch transition
	// block has a beneficiary set to non-zeroes.
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")

	// errInvalidVote is returned if a nonce value is something else that the two
	// allowed constants of 0x00..0 or 0xff..f.
	errInvalidVote = errors.New("vote nonce not 0x00..0 or 0xff..f")

	// errInvalidCheckpointVote is returned if a checkpoint/epoch transition block
	// has a vote nonce set to non-zeroes.
	errInvalidCheckpointVote = errors.New("vote nonce in checkpoint block non-zero")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte suffix signature missing")

	// errExtraSigners is returned if non-checkpoint block contain signer data in
	// their extra-data fields.
	errExtraSigners = errors.New("non-checkpoint block contains extra signer list")

	// errInvalidCheckpointSigners is returned if a checkpoint block contains an
	// invalid list of signers (i.e. non divisible by 20 bytes, or not the correct ones).
	errInvalidCheckpointSigners = errors.New("invalid signer list on checkpoint block")

	// errInvalidCheckpointPenalties is returned if penalties list is invalid
	errInvalidCheckpointPenalties = errors.New("invalid penalty list on checkpoint block")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block is not either
	// of 1 or 2, or if the value does not match the turn of the signer.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errInvalidVotingChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errInvalidVotingChain = errors.New("invalid voting chain")

	// errUnauthorized is returned if a header is signed by a non-authorized entity.
	errUnauthorized = errors.New("unauthorized")

	// errFailedDoubleValidation is returned if double validation fails
	errFailedDoubleValidation = errors.New("wrong pair of creator-validator in double validation")

	// errWaitTransactions is returned if an empty block is attempted to be sealed
	// on an instant chain (0 second period).
	errWaitTransactions = errors.New("waiting for transactions")

	// ErrInvalidCheckpointValidators is returned if validators list is invalid
	ErrInvalidCheckpointValidators = errors.New("invalid validators list on checkpoint block")
)

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, string, []byte) ([]byte, error)

// sigHash returns the hash which is used as input for the proof-of-stake-voting
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeader(hasher, header)
	hasher.Sum(hash[:0])
	return hash
}

// SealHash returns the hash of a block prior to it being sealed (exported).
func SealHash(header *types.Header) (hash common.Hash) {
	return sigHash(header)
}

// encodeSigHeader encodes the header for signing, excluding the signature
func encodeSigHeader(w io.Writer, header *types.Header) {
	// V1 blocks store the proposer signature in the last 65 bytes of header.Extra.
	// This function is ONLY used for V1 code paths.
	// V2 blocks use engine_v2.sigHash which handles header.Validator.
	var extraForHash []byte
	if len(header.Extra) >= extraSeal {
		extraForHash = header.Extra[:len(header.Extra)-extraSeal]
	} else {
		extraForHash = header.Extra
	}
	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		extraForHash,
		header.MixDigest,
		header.Nonce,
	}
	// Add post-London fields if present
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	if header.WithdrawalsHash != nil {
		enc = append(enc, header.WithdrawalsHash)
	}
	if header.BlobGasUsed != nil {
		enc = append(enc, header.BlobGasUsed)
	}
	if header.ExcessBlobGas != nil {
		enc = append(enc, header.ExcessBlobGas)
	}
	if header.ParentBeaconRoot != nil {
		enc = append(enc, header.ParentBeaconRoot)
	}
	if err := rlp.Encode(w, enc); err != nil {
		panic("can't encode: " + err.Error())
	}
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.Cache[common.Hash, common.Address]) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address, nil
	}

	// V1 blocks ALWAYS store the proposer signature in the last 65 bytes of header.Extra.
	// This function is ONLY used for V1 code paths (whoIsCreator, verifySeal, Author for V1 blocks).
	// V2 blocks use engine_v2.ecrecover which handles header.Validator.
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}

	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// XDPoS is the delegated-proof-of-stake consensus engine.
type XDPoS struct {
	config *params.XDPoSConfig // Consensus engine configuration parameters
	db     ethdb.Database      // Database to store and retrieve snapshot checkpoints

	recents             *lru.Cache[common.Hash, *Snapshot]      // Snapshots for recent block to speed up reorgs
	signatures          *lru.Cache[common.Hash, common.Address] // Signatures of recent blocks to speed up mining
	validatorSignatures *lru.Cache[common.Hash, common.Address] // Validator signatures cache
	verifiedHeaders     *lru.Cache[common.Hash, bool]           // Verified headers cache
	proposals           map[common.Address]bool                 // Current list of proposals we are pushing

	signer common.Address // Ethereum address of the signing key
	signFn SignerFn       // Signer function to authorize hashes with
	lock   sync.RWMutex   // Protects the signer fields

	// Block signers cache for tracking signing transactions
	BlockSigners *lru.Cache[common.Hash, []*types.Transaction]

	// Hook functions for XDC-specific logic
	// These hooks integrate with the XDC smart contracts for rewards, penalties, etc.
	HookReward            func(chain consensus.ChainHeaderReader, stateBlock *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error)
	HookPenalty           func(chain consensus.ChainHeaderReader, blockNumberEpoc uint64) ([]common.Address, error)
	HookPenaltyTIPSigning func(chain consensus.ChainHeaderReader, header *types.Header, candidate []common.Address) ([]common.Address, error)
	HookValidator         func(header *types.Header, signers []common.Address) ([]byte, error)
	HookVerifyMNs         func(header *types.Header, signers []common.Address) error

	// EngineV2 is the XDPoS 2.0 consensus engine instance. (#94)
	// Set via SetEngineV2 when full chain config is available.
	// Used by verifyHeaderV2 for full V2 block verification.
	EngineV2 EngineV2Iface

	// ConsensusFeed exposes XDPoS consensus events over WebSocket (fix #87).
	// External callers subscribe via SubscribeNewQC / SubscribeNewTC etc.
	ConsensusFeed ConsensusFeed

	// Testing flags
	fakeDiff bool // Skip difficulty verifications (testing)

	// XDC: parentState is a copy of statedb before tx processing,
	// set by Process() and used by Finalize() for reward calculation.
	// This matches v2.6.8 where parentState is passed to Finalize.
	parentStateForReward *state.StateDB
}

// EngineV2Iface is the minimal interface for V2 engine, avoiding import cycle. (#94)
type EngineV2Iface interface {
	VerifyHeader(chain consensus.ChainReader, header *types.Header, fullVerify bool) error
	VerifyHeaderWithParents(chain consensus.ChainReader, header *types.Header, parents []*types.Header, fullVerify bool) error
	GetRoundNumber(header *types.Header) (types.Round, error) // (#117) per-round config dispatch
	UpdateMasternodesFromHeader(chain consensus.ChainReader, header *types.Header, statedb *state.StateDB) error
	UpdateMasternodes(chain consensus.ChainReader, header *types.Header, ms []common.Address) error                         // direct snapshot seed (used for trusted-checkpoint pre-seed when state isn't available)
	UpdateMasternodesWithPenalties(chain consensus.ChainReader, header *types.Header, ms, penalties []common.Address) error // direct snapshot seed recording both the active set and the penalty pool (checkpoint-sync anchors)
	Author(header *types.Header) (common.Address, error)                                                                    // (#407) V2-compatible sigHash recovery
	IsEpochSwitch(header *types.Header) (bool, uint64, error)                                                               // (#410) round-based epoch boundary
	GetMasternodesByHash(chain consensus.ChainReader, hash common.Hash) []common.Address                                    // (#402) post-penalty active masternodes
	GetPreviousPenaltyByHash(chain consensus.ChainReader, hash common.Hash, limitPenaltyEpoch int) []common.Address         // (#412) comeback seeding
	GetMasternodesFromEpochSwitchHeader(chain consensus.ChainReader, header *types.Header) []common.Address                 // V2 era epoch-switch masternode list (reads header.Validators, falls back to Extra at switch block)
	GetMasternodesV2(chain consensus.ChainReader, header *types.Header) []common.Address                                    // active masternode set for any V2-era header via getEpochSwitchInfo

	// Mining-side wrapper dispatch surface (audit IDs M-1..M-7).
	// Iter 4 (interface-only): widen EngineV2Iface so iter 5 can route the
	// wrapper's mining methods to the V2 engine when BlockConsensusVersion >= 2.
	// Signatures match consensus/XDPoS/engines/engine_v2/engine.go exactly --
	// the V2 engine is ground truth. Notable divergences from the
	// consensus.Engine interface:
	//   - Finalize returns (*types.Block, error) and takes parentState/txs/uncles/receipts
	//     (legacy v2.6.8 signature retained for reward-hook compatibility).
	//   - Seal is synchronous: returns (*types.Block, error) instead of pushing
	//     to a results channel. Iter 5 will adapt sync->async at the wrapper.
	//   - V2 exposes SignHash (not SealHash); the wrapper's SealHash dispatch
	//     in iter 5 will call SignHash on the V2 engine.
	// FinalizeAndAssemble is intentionally NOT on the interface: V2 has no
	// such method. The wrapper composes it via V2.Finalize (which already
	// returns *types.Block in the legacy v2.6.8 signature).
	// Authorize IS on the interface (added iter 5): the wrapper adapts its
	// 3-arg SignerFn to V2's 2-arg engine_v2.SignerFn via a closure that
	// (M-7) Authorize uses engine_v2.SignerFn (2-arg, no mimeType) — wrapper adapts via closure.
	// Iter 5: import engine_v2 was confirmed cycle-free (engine_v2 does NOT import the parent XDPoS pkg).
	Prepare(chain consensus.ChainReader, header *types.Header) error                                                                                                                                                 // (M-1)
	Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, parentState *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) // (M-2)
	Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error)                                                                                                                // (M-4) sync; iter 5 adapts to async
	CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int                                                                                                                          // (M-5)
	SignHash(header *types.Header) common.Hash                                                                                                                                                                       // (M-6) V2 analog of SealHash
	// (M-7) Authorize uses engine_v2.SignerFn (2-arg, no mimeType) — wrapper adapts via closure.
	Authorize(signer common.Address, signFn engine_v2.SignerFn)

	// SetHookReward is called by EngineV2.Finalize at epoch-switch blocks to distribute rewards.
	SetHookReward(fn func(chain consensus.ChainReader, state *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error))
	// SetHookPenalty wires the penalty hook used by calcMasternodes to remove
	// under-signing masternodes from the active set at epoch-switch blocks.
	SetHookPenalty(fn func(chain consensus.ChainReader, number *big.Int, parentHash common.Hash, candidates []common.Address) ([]common.Address, error))

	// ProposedBlockHandler processes a newly proposed/self-minted block through
	// the BFT self-vote path: processQC → allowedToSend → verifyVotingRule →
	// sendVote → broadcastToBftChannel. Called after InsertChain succeeds.
	// A.97-M.A Task 4 (refs #894, PR-A): widen EngineV2Iface for M4 delegation.
	// Canonical: engine_v2/utils.go ProposedBlockHandler.
	ProposedBlockHandler(chain consensus.ChainReader, header *types.Header) error

	// ReinitBFT forces a full BFT engine re-initialization from the given header.
	// Call after a bulk-sync catch-up completes (XdcBulkSyncMode→false) so that
	// currentRound is re-seeded from the head's embedded QC rather than being
	// left at whatever stale round the engine observed during bulk import. A.99.4b.
	ReinitBFT(chain consensus.ChainReader, header *types.Header) error
}

// SetEngineV2 wires the V2 consensus engine for full V2 header verification. (#94)
// Call this after creating the engine, passing an *engine_v2.XDPoS_v2 instance.
// #379 NOTE: This is a temporary wiring mechanism. Future refactor should pass
// engine_v2.XDPoS_v2 directly to New() to avoid the two-step initialization.
func (c *XDPoS) SetEngineV2(v2 EngineV2Iface) {
	c.EngineV2 = v2
}

// UpdateMasternodesV2 forwards a direct masternode-set seed to the V2 engine.
// Used by core/blockchain.go to pre-seed the V2 snapshot store when anchoring
// at a trusted V2-era checkpoint where contract state isn't available — the
// wrapper's plain UpdateMasternodes uses the legacy []Masternode signature, so
// this passthrough exposes the V2 []common.Address path under a distinct name.
func (c *XDPoS) UpdateMasternodesV2(chain consensus.ChainReader, header *types.Header, ms []common.Address) error {
	if c.EngineV2 == nil {
		return fmt.Errorf("XDPoS: EngineV2 not initialised")
	}
	return c.EngineV2.UpdateMasternodes(chain, header, ms)
}

// UpdateMasternodesV2WithPenalties seeds both the active masternode set and
// the penalty pool at a trusted checkpoint anchor. Penalties must be stored
// in a distinct snapshot field (not concatenated into NextEpochCandidates),
// because the synthetic-epoch-info path uses NextEpochCandidates directly
// as the active proposer rotation list.
func (c *XDPoS) UpdateMasternodesV2WithPenalties(chain consensus.ChainReader, header *types.Header, ms, penalties []common.Address) error {
	if c.EngineV2 == nil {
		return fmt.Errorf("XDPoS: EngineV2 not initialised")
	}
	return c.EngineV2.UpdateMasternodesWithPenalties(chain, header, ms, penalties)
}

// UpdateMasternodesFromHeader forwards to the V2 engine. Used by trusted-checkpoint
// pre-seeding when state is available.
func (c *XDPoS) UpdateMasternodesFromHeader(chain consensus.ChainReader, header *types.Header, statedb *state.StateDB) error {
	if c.EngineV2 == nil {
		return fmt.Errorf("XDPoS: EngineV2 not initialised")
	}
	return c.EngineV2.UpdateMasternodesFromHeader(chain, header, statedb)
}

// GetMasternodesFromEpochSwitchHeader forwards to the V2 engine. Used by
// trusted-checkpoint pre-seeding to extract masternodes from V2 switch headers.
func (c *XDPoS) GetMasternodesFromEpochSwitchHeader(chain consensus.ChainReader, header *types.Header) []common.Address {
	if c.EngineV2 == nil {
		return nil
	}
	return c.EngineV2.GetMasternodesFromEpochSwitchHeader(chain, header)
}

// SetParentState stores the pre-tx state copy for use in reward calculations.
// Called by the state processor before Finalize.
func (c *XDPoS) SetParentState(ps *state.StateDB) {
	c.parentStateForReward = ps
}

// New creates a XDPoS delegated-proof-of-stake consensus engine.
func New(config *params.XDPoSConfig, db ethdb.Database) *XDPoS {
	// Set any missing consensus parameters to their defaults
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = defaultEpochLength
	}
	if conf.Period == 0 {
		conf.Period = defaultPeriod
	}
	if conf.Gap == 0 {
		conf.Gap = defaultGap
	}
	// Sanity check: V2 switch block must align to epoch boundary (matches v2.6.8)
	if conf.V2 != nil && conf.V2.SwitchBlock != nil && conf.V2.SwitchBlock.Uint64()%conf.Epoch != 0 {
		panic(fmt.Sprintf("XDPoS: V2.SwitchBlock (%d) must be aligned to Epoch (%d)", conf.V2.SwitchBlock.Uint64(), conf.Epoch))
	}

	// Allocate the snapshot caches and create the engine
	recents := lru.NewCache[common.Hash, *Snapshot](inmemorySnapshots)
	signatures := lru.NewCache[common.Hash, common.Address](inmemorySignatures)
	validatorSignatures := lru.NewCache[common.Hash, common.Address](inmemorySignatures)
	verifiedHeaders := lru.NewCache[common.Hash, bool](inmemorySnapshots)
	blockSigners := lru.NewCache[common.Hash, []*types.Transaction](blockSignersCacheLimit)

	return &XDPoS{
		config:              &conf,
		db:                  db,
		recents:             recents,
		signatures:          signatures,
		validatorSignatures: validatorSignatures,
		verifiedHeaders:     verifiedHeaders,
		BlockSigners:        blockSigners,
		proposals:           make(map[common.Address]bool),
	}
}

// IsV2Block returns true if the header is a V2 block (strictly after the V2 switch block).
// The switch block itself is mined by the V1 engine and carries V1-format Extra data,
// so it must NOT be routed to V2 engine paths.
// Canonical boundary (XDPoSChain params/config.go BlockConsensusVersion): num > switchBlock.
func (c *XDPoS) IsV2Block(header *types.Header) bool {
	return c.config != nil && c.config.V2 != nil && c.config.V2.SwitchBlock != nil &&
		header.Number.Cmp(c.config.V2.SwitchBlock) > 0
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (c *XDPoS) Author(header *types.Header) (common.Address, error) {
	// C10 FIX: V2 blocks use V2-compatible sigHash (17-field RLP including Validators/Penalties)
	// V1 blocks use legacy 15-field sigHash
	if c.IsV2Block(header) && c.EngineV2 != nil {
		return c.EngineV2.Author(header)
	}
	return ecrecover(header, c.signatures)
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (c *XDPoS) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return c.verifyHeaderWithCache(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
//
// Optimisation (XDC): during bulk sync we use two fast-paths:
//   - V2 blocks (after SwitchBlock): parallel worker pool (header-independent checks)
//   - V1 blocks in bulk-sync mode: light verification that skips verifySeal (ecrecover)
func (c *XDPoS) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	if len(headers) == 0 {
		close(results)
		return abort, results
	}

	// Determine if we have a V2 switch block configured
	var switchBlock uint64
	hasV2 := c.config.V2 != nil && c.config.V2.SwitchBlock != nil
	if hasV2 {
		switchBlock = c.config.V2.SwitchBlock.Uint64()
	}

	// Couple to state-completeness too: a checkpoint-sync node (incomplete state)
	// must use the light V1 path for the same reason it skips V2 fullVerify.
	bulkSync := !xdcFullVerify()

	go func() {
		// Find first V2 header index (if any) for parallel dispatch
		firstV2 := -1
		if hasV2 {
			for i, h := range headers {
				if h.Number.Uint64() > switchBlock {
					firstV2 = i
					break
				}
			}
		}

		// --- V1 portion (sequential) ---
		v1End := len(headers)
		if firstV2 >= 0 {
			v1End = firstV2
		}
		log.Debug("VerifyHeaders dispatch", "hasV2", hasV2, "switchBlock", switchBlock, "firstV2", firstV2, "v1End", v1End, "totalHeaders", len(headers), "firstHeaderNum", headers[0].Number.Uint64(), "bulkSync", bulkSync)
		for i := 0; i < v1End; i++ {
			var err error
			if bulkSync {
				err = c.verifyHeaderLightV1(chain, headers[i], headers[:i])
			} else {
				err = c.verifyHeaderWithCache(chain, headers[i], headers[:i])
			}
			select {
			case <-abort:
				return
			case results <- err:
			}
		}

		// --- V2 portion ---
		if firstV2 < 0 {
			return // no V2 headers
		}
		v2Headers := headers[firstV2:]

		// v8.3: ALWAYS use sequential verification for V2 headers.
		// Parallel workers with parents=nil caused ErrUnknownAncestor on block N+1
		// because parent N was in the same batch but not yet in the chain DB.
		// This created a ~1 block per sync cycle trap during bulk sync.
		for i, header := range v2Headers {
			err := c.verifyHeaderWithCache(chain, header, headers[:firstV2+i])
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeaderLightV1 performs V1 header verification without the expensive
// verifySeal (ecrecover + snapshot). Safe during bulk sync because the blocks
// were already accepted by the network.
func (c *XDPoS) verifyHeaderLightV1(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if _, check := c.verifiedHeaders.Get(header.Hash()); check {
		return nil
	}

	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	// Checkpoint blocks need to enforce zero beneficiary
	checkpoint := (number % c.config.Epoch) == 0
	if checkpoint && header.Coinbase != (common.Address{}) {
		return errInvalidCheckpointBeneficiary
	}

	// Nonce checks
	if !bytes.Equal(header.Nonce[:], nonceAuthVote) && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidVote
	}
	if checkpoint && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidCheckpointVote
	}

	// Extra-data length checks
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}
	signersBytes := len(header.Extra) - extraVanity - extraSeal
	isV2Block := c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number >= c.config.V2.SwitchBlock.Uint64()
	if !checkpoint && signersBytes != 0 && !isV2Block {
		return errExtraSigners
	}
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}

	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// Cascading field checks (parent, timestamp) but WITHOUT verifySeal
	if number == 0 {
		c.verifiedHeaders.Add(header.Hash(), true)
		return nil
	}

	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		log.Warn("[DIAG-xdpos1] verifyCascadingFields ErrUnknownAncestor", "number", number, "parentNil", parent == nil, "parentsLen", len(parents))
		return consensus.ErrUnknownAncestor
	}
	if parent.Time+c.config.Period > header.Time {
		return ErrInvalidTimestamp
	}

	// #951 Defect 1c: reject empty header.Validator past the first epoch (V1 double
	// validation requires the assigned-M2 co-signature) — MAINNET (chainId 50) ONLY.
	//
	// On mainnet the canonical V1 chain always co-signs (legacy fullVerify), so an
	// empty Validator is a pre-co-sign anomaly that must be rejected, forcing the
	// xdcSyncer to re-fetch the co-signed canonical (modern lacks TD fork choice).
	//
	// On testnet/devnet the canonical chain legitimately CONTAINS empty-Validator
	// >epoch blocks and there is no co-signed sibling to re-fetch: e.g. devnet-5151
	// block 1962 has validator_len=0 and is canonical on legacy node1 AND accepted by
	// older modern (c52d2957), while its neighbours (1960/1961/1963) carry the 65-byte
	// co-signature. Rejecting it wedges import there (modern-val2 stuck at 1962). Mirror
	// legacy's relaxed testnet/devnet verification (common.IsTestnet → fullVerify=false):
	// accept empty there. Production still co-signs (miner Part C); this only gates IMPORT.
	if number > c.config.Epoch && len(header.Validator) == 0 &&
		chain.Config() != nil && chain.Config().ChainID != nil &&
		chain.Config().ChainID.Uint64() == params.XDCMainnetChainConfig.ChainID.Uint64() {
		return errNoValidatorSignature
	}

	c.verifiedHeaders.Add(header.Hash(), true)
	return nil
}

func (c *XDPoS) verifyHeaderWithCache(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// v8.2: ALWAYS run full verification. The previous optimization of skipping
	// already-verified headers via verifiedHeaders cache was INCORRECT: it skipped
	// the snapshot() call, so the snapshot recents cache never got populated for
	// cached headers. When a subsequent header needed a snapshot that walked back
	// through a cache-hit header, the snapshot wasn't in recents and the parents
	// slice didn't contain it (it was in the previous sync cycle) → ErrUnknownAncestor.
	//
	// This was the root cause of the ~900-block sync cycle limit.
	// Full verification is cheap (mostly signature recovery + snapshot lookup).
	err := c.verifyHeader(chain, header, parents)
	return err
}

// verifyHeader checks whether a header conforms to the consensus rules.
func (c *XDPoS) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// XDPoS 2.0: For V2 blocks, use V2 validation.
	// The switch block (number == switchBlock) is mined by V1 and carries V1-format Extra
	// (extra[0] == 0xd7, not the V2 marker 2).  Routing it to verifyHeaderV2 causes
	// getExtraFields/DecodeExtraFields to return ErrInvalidV2Extra — which is silent on
	// first sync (lenient path accepts it) but fatal on re-import after a SetHead rollback,
	// wedging the node at switchBlock−1 (observed: modern-fast stuck at 2699).
	// Canonical boundary (XDPoSChain params/config.go BlockConsensusVersion, line 578):
	//   V2 starts at num > switchBlock (strictly greater).  The switch block is the last V1 block.
	if c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number > c.config.V2.SwitchBlock.Uint64() {
		return c.verifyHeaderV2(chain, header, parents)
	}

	// V1 validation below (for blocks before V2 switch)

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	// Checkpoint blocks need to enforce zero beneficiary
	checkpoint := (number % c.config.Epoch) == 0
	if checkpoint && header.Coinbase != (common.Address{}) {
		return errInvalidCheckpointBeneficiary
	}

	// Nonces must be 0x00..0 or 0xff..f, zeroes enforced on checkpoints
	if !bytes.Equal(header.Nonce[:], nonceAuthVote) && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidVote
	}
	if checkpoint && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidCheckpointVote
	}

	// Check that the extra-data contains both the vanity and signature
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise
	// NOTE: Skip this check for V2 blocks - V2 consensus allows signers in non-checkpoint blocks
	signersBytes := len(header.Extra) - extraVanity - extraSeal
	isV2Block := c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number >= c.config.V2.SwitchBlock.Uint64()
	if !checkpoint && signersBytes != 0 && !isV2Block {
		return errExtraSigners
	}
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	// Ensure that the block doesn't contain any uncles which are meaningless in XDPoS
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// All basic checks passed, verify cascading fields
	return c.verifyCascadingFields(chain, header, parents)
}

// verifyHeaderV2 performs full validation for XDPoS 2.0 blocks via the V2 engine. (#94)
// If EngineV2 is set (normal operation), delegates to engine_v2.VerifyHeader for full QC/proposer checks.
// Falls back to basic sanity checks if EngineV2 is not yet initialized (startup edge case).
// xdcFullVerify reports whether headers should undergo strict (steady-state)
// consensus verification. It is false during bulk sync (XdcBulkSyncMode) AND
// while the node has incomplete state (CheckpointSyncNoStateActive). A node that
// fast-synced or snapshot-restored runs with checkpointSyncNoState=true and
// cannot recompute the canonical V2 masternode/penalty set, so strict
// verification would hard-fail legitimately-produced epoch-switch blocks
// (ErrPenaltiesNotLegit) and wedge it below tip (observed on a snapshot-restored
// mainnet fast node at block 103,852,092). Strict verification resumes only once
// the state heal (XDC_SNAP_HEAL) clears checkpointSyncNoState. Fully-synced nodes
// keep both flags false → no behaviour change.
func xdcFullVerify() bool {
	return !core.XdcBulkSyncMode.Load() && !core.CheckpointSyncNoStateActive.Load()
}

func (c *XDPoS) verifyHeaderV2(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	number := header.Number.Uint64()

	// If V2 engine is wired, use full verification (QC, proposer signature, masternode check). (#94)
	if c.EngineV2 != nil {
		if cr, ok := chain.(consensus.ChainReader); ok {
			// fullVerify gate (PR #536 commit 80b7e850): fail closed on
			// calcMasternodes error during single-block import / mining
			// (fullVerify=true), use the parents-aware fallback chain
			// (GetMasternodesWithParents → header.Validators / header.Penalties)
			// during bulk sync (fullVerify=false). Without this, the
			// bulk-sync fallback in engine_v2/verifyHeader.go is dead code,
			// because every block's HookPenalty fails on in-batch ancestors
			// that aren't yet committed to the DB.
			fullVerify := xdcFullVerify()
			return c.EngineV2.VerifyHeaderWithParents(cr, header, parents, fullVerify)
		}
		// ChainHeaderReader that doesn't implement ChainReader — fall through to basic checks
		log.Warn("[XDPoS V2] VerifyHeader: chain does not implement ChainReader, using basic checks", "number", number)
	}

	// Safety: if we're strictly past the V2 switch block, EngineV2 MUST be wired.
	// (verifyHeaderV2 is only called for number > switchBlock; this guard is belt-and-suspenders.)
	// Falling back to basic checks at this point would skip QC/proposer validation.
	if c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number > c.config.V2.SwitchBlock.Uint64() {
		return fmt.Errorf("[XDPoS V2] EngineV2 not initialized but block %d is past V2 switch block %d", number, c.config.V2.SwitchBlock.Uint64())
	}

	// Fallback: basic sanity checks (used during initial startup before V2 engine is set)
	if header.Time > uint64(time.Now().Unix())+15 { // Allow 15 seconds drift
		return consensus.ErrFutureBlock
	}
	if header.GasUsed > header.GasLimit {
		return errors.New("gas used exceeds gas limit")
	}
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	log.Debug("[XDPoS V2] verifyHeaderV2 fallback (no V2 engine)", "number", number, "hash", header.Hash().Hex()[:10])
	return nil
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers.
func (c *XDPoS) verifyCascadingFields(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}

	// Ensure that the block's timestamp isn't too close to its parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		log.Warn("v8: verifyCascadingFields parent lookup failed",
			"block", number,
			"parentHash", header.ParentHash.Hex()[:16],
			"parentNil", parent == nil,
			"parentsLen", len(parents))
		log.Warn("[DIAG-xdpos2] verifyHeaderLightV1 ErrUnknownAncestor", "number", header.Number, "parentNil", parent == nil)
		return consensus.ErrUnknownAncestor
	}
	if parent.Time+c.config.Period > header.Time {
		return ErrInvalidTimestamp
	}

	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// If the block is a checkpoint block, verify the signer list
	if number%c.config.Epoch == 0 {
		signers := snap.GetSigners()
		penPenalties := []common.Address{}

		// Get penalties if hook is configured
		if c.HookPenalty != nil || c.HookPenaltyTIPSigning != nil {
			var penErr error
			if c.HookPenaltyTIPSigning != nil {
				penPenalties, penErr = c.HookPenaltyTIPSigning(chain, header, signers)
			} else if c.HookPenalty != nil {
				penPenalties, penErr = c.HookPenalty(chain, number)
			}
			if penErr != nil {
				return penErr
			}
			for _, address := range penPenalties {
				log.Debug("Penalty Info", "address", address, "number", number)
			}
		}

		// Remove penalties from signers
		signers = removeItemFromArray(signers, penPenalties)

		// Remove penalties from previous epochs (up to LimitPenaltyEpoch)
		for i := 1; i <= LimitPenaltyEpoch; i++ {
			if number > uint64(i)*c.config.Epoch {
				signers = c.removePenaltiesFromBlock(chain, signers, number-uint64(i)*c.config.Epoch)
			}
		}

		// Verify checkpoint signers match
		// CRITICAL FIX: For V2 blocks, prefer header.Validators (xdc2.6.8 format).
		// For V1 blocks, header.Validators holds 4-byte ASCII M2 indices, NOT addresses.
		// We must use header.Extra for V1 and header.Validators for V2.
		var masternodesFromCheckpointHeader []common.Address
		// #951: must be strict `>` — the switch block itself (number == SwitchBlock) is the
		// LAST V1 block and carries header.Validators in V1 4-byte-M2-index format. Using `>=`
		// here misparses those M2 bytes as 20-byte addresses whenever validator count N%5==0,
		// then the bootstrap-from-header branch below persists a corrupted snapshot. Legacy V1
		// verification (engine_v1/engine.go:304-305) always reads the switch-block masternodes
		// from header.Extra. Matches the already-correct sibling at xdpos.go:1868 and IsV2Block.
		isV2Checkpoint := c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number > c.config.V2.SwitchBlock.Uint64()
		if isV2Checkpoint && len(header.Validators) > 0 && len(header.Validators)%common.AddressLength == 0 {
			masternodesFromCheckpointHeader = make([]common.Address, len(header.Validators)/common.AddressLength)
			for i := 0; i < len(masternodesFromCheckpointHeader); i++ {
				copy(masternodesFromCheckpointHeader[i][:], header.Validators[i*common.AddressLength:])
			}
			// Sanity check: reject all-zero addresses
			allZero := true
			for _, mn := range masternodesFromCheckpointHeader {
				if mn != (common.Address{}) {
					allZero = false
					break
				}
			}
			if allZero {
				log.Warn("verifyCascadingFields: header.Validators all-zero, falling back to Extra", "number", number)
				masternodesFromCheckpointHeader = nil
			}
		}
		if len(masternodesFromCheckpointHeader) == 0 {
			extraSuffix := len(header.Extra) - extraSeal
			if extraSuffix > extraVanity {
				masternodesFromCheckpointHeader = extractAddressFromBytes(header.Extra[extraVanity:extraSuffix])
			}
		}
		if !compareSignersLists(masternodesFromCheckpointHeader, signers) {
			// Mismatch between our computed signer set and what the checkpoint header says.
			// This can happen during initial sync when we don't yet have full penalty history
			// for previous epochs (removePenaltiesFromBlock returns the header from the chain,
			// but if those headers aren't downloaded yet, penalties are skipped).
			//
			// Resolution strategy (compatible with v2.6.8):
			//   1. If the header has a non-empty masternode list, trust it — the block signature
			//      will still prove it was accepted by the actual network.
			//   2. Bootstrap the snapshot's signer set from the header so that the next epoch
			//      starts from the correct state, eliminating cascading mismatches.
			//   3. Reject if the header has an empty masternode list (that would be invalid).
			if len(masternodesFromCheckpointHeader) > 0 {
				log.Warn("Checkpoint signer mismatch — bootstrapping snapshot from header (sync mode)",
					"number", number,
					"header_masternodes", len(masternodesFromCheckpointHeader),
					"snapshot_signers", len(signers),
					"note", "transient during initial sync; resolves as penalty history is downloaded")
				// Update the snapshot to match the header so downstream epochs are correct.
				// This mirrors what v2.6.8 does when it builds the snapshot from scratch.
				newSigners := make(map[common.Address]struct{}, len(masternodesFromCheckpointHeader))
				for _, mn := range masternodesFromCheckpointHeader {
					newSigners[mn] = struct{}{}
				}
				snap.Signers = newSigners
				// Persist the corrected snapshot so the next call uses the right set.
				if err := snap.store(c.db); err != nil {
					log.Warn("Failed to persist corrected checkpoint snapshot", "number", number, "err", err)
				}
				c.recents.Add(snap.Hash, snap)
				signers = masternodesFromCheckpointHeader
			} else {
				log.Error("Masternodes lists are different in checkpoint header and snapshot",
					"number", number,
					"masternodes_from_checkpoint_header", masternodesFromCheckpointHeader,
					"masternodes_in_snapshot", signers,
					"penList", penPenalties)
				return errInvalidCheckpointSigners
			}
		}

		// Verify MNs if hook is configured
		if c.HookVerifyMNs != nil {
			if err := c.HookVerifyMNs(header, signers); err != nil {
				return err
			}
		}
	}

	// All basic checks passed, verify the seal and return
	return c.verifySeal(chain, header, parents)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements.
func (c *XDPoS) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	creator, err := ecrecover(header, c.signatures)
	if err != nil {
		return err
	}

	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	difficulty := c.calcDifficulty(chain, parent, creator)
	log.Info("verify seal block", "number", header.Number, "hash", header.Hash(), "block difficulty", header.Difficulty, "calc difficulty", difficulty, "creator", creator, "snap signers", len(snap.Signers))

	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	// Note: During sync, we may not have checkpoint headers yet, so difficulty calculation
	// might return 0. In this case, we skip difficulty validation and rely on signature verification.
	if number > 0 && !c.fakeDiff && difficulty.Int64() != 0 {
		if header.Difficulty.Int64() != difficulty.Int64() {
			log.Warn("invalid difficulty", "number", number, "have", header.Difficulty, "want", difficulty, "creator", creator, "yourturn_err", err)
			return errInvalidDifficulty
		}
	}

	// Get masternodes for this header (include parents for sync scenarios)
	masternodes := c.GetMasternodesWithParents(chain, header, parents)

	// Check if creator is authorized
	if _, ok := snap.Signers[creator]; !ok {
		valid := false
		for _, m := range masternodes {
			if m == creator {
				valid = true
				break
			}
		}
		if !valid {
			log.Debug("Unauthorized creator found", "block number", number, "creator", creator.String())
			return errUnauthorized
		}
	}

	// Check recent signers to prevent spamming
	if len(masternodes) > 1 {
		for seen, recent := range snap.Recents {
			if recent == creator {
				if limit := uint64(2); seen > number-limit {
					if number%c.config.Epoch != 0 {
						return errUnauthorized
					}
				}
			}
		}
	}

	// #951 Defect 1c: V1 double-validation (M1/M2) — DIAGNOSTIC (non-enforcing) phase.
	// Legacy (engine_v1/engine.go:164-167,638-655) rejects, for Number>Epoch under
	// fullVerify: an empty header.Validator (ErrNoValidatorSignature) and a
	// RecoverValidator(header.Validator) != GetValidator(creator) mismatch
	// (ErrFailedDoubleValidation). Modern did neither. Before enforcing — a wrong M2
	// computation would reject the entire chain past the first epoch and wedge the node
	// — we LOG agreement/disagreement against canonical blocks to validate the M2 map.
	// fullVerify mirrors verifyHeaderV2: only at-tip (not bulk sync). Flip [DIAG-1c] to a
	// hard reject once the live chain shows zero mismatches.
	fullVerify := xdcFullVerify()
	if fullVerify && number > c.config.Epoch {
		switch {
		case len(header.Validator) == 0:
			log.Warn("[DIAG-1c] empty header.Validator past epoch (legacy→ErrNoValidatorSignature)", "number", number, "creator", creator)
		default:
			validator, vErr := c.RecoverValidator(header)
			assigned, aErr := c.GetValidator(creator, chain, header)
			switch {
			case vErr != nil:
				log.Warn("[DIAG-1c] RecoverValidator failed", "number", number, "err", vErr)
			case aErr != nil:
				log.Warn("[DIAG-1c] GetValidator(M2) failed", "number", number, "creator", creator, "err", aErr)
			case validator != assigned:
				log.Warn("[DIAG-1c] M2 MISMATCH (legacy→ErrFailedDoubleValidation)", "number", number, "creator", creator, "recovered", validator, "assignedM2", assigned)
			default:
				log.Debug("[DIAG-1c] M2 double-validation OK", "number", number, "creator", creator, "m2", assigned)
			}
		}
	}

	return nil
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *XDPoS) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (c *XDPoS) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	// V2 dispatch (M-1): for V2 blocks, route Prepare to the V2 engine.
	if c.IsV2Block(header) && c.EngineV2 != nil {
		if cr, ok := chain.(consensus.ChainReader); ok {
			return c.EngineV2.Prepare(cr, header)
		}
	}

	header.Coinbase = common.Address{}
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()

	snap, err := c.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}

	if number%c.config.Epoch != 0 {
		c.lock.RLock()
		addresses := make([]common.Address, 0, len(c.proposals))
		for address, authorize := range c.proposals {
			if snap.validVote(address, authorize) {
				addresses = append(addresses, address)
			}
		}
		if len(addresses) > 0 {
			header.Coinbase = addresses[rand.Intn(len(addresses))]
			if c.proposals[header.Coinbase] {
				copy(header.Nonce[:], nonceAuthVote)
			} else {
				copy(header.Nonce[:], nonceDropVote)
			}
		}
		c.lock.RUnlock()
	}

	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	header.Difficulty = c.calcDifficulty(chain, parent, c.signer)
	log.Debug("CalcDifficulty", "number", header.Number, "difficulty", header.Difficulty)

	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]

	masternodes := snap.GetSigners()
	if number >= c.config.Epoch && number%c.config.Epoch == 0 {
		if c.HookPenalty != nil || c.HookPenaltyTIPSigning != nil {
			var penMasternodes []common.Address
			var penErr error

			if c.HookPenaltyTIPSigning != nil {
				penMasternodes, penErr = c.HookPenaltyTIPSigning(chain, header, masternodes)
			} else if c.HookPenalty != nil {
				penMasternodes, penErr = c.HookPenalty(chain, number)
			}
			if penErr != nil {
				return penErr
			}
			if len(penMasternodes) > 0 {
				masternodes = removeItemFromArray(masternodes, penMasternodes)
				for _, address := range penMasternodes {
					log.Debug("Penalty status", "address", address, "number", number)
				}
				// Encode current-epoch penalties into header.Penalties so that
				// future epochs can look them up via removePenaltiesFromBlock.
				// This matches v2.6.8 behaviour and enables cross-client verification.
				penBytes := make([]byte, len(penMasternodes)*common.AddressLength)
				for i, addr := range penMasternodes {
					copy(penBytes[i*common.AddressLength:], addr[:])
				}
				header.Penalties = penBytes
				log.Debug("Encoded penalties to checkpoint header",
					"number", number,
					"penaltyCount", len(penMasternodes))
			}
		}

		for i := 1; i <= LimitPenaltyEpoch; i++ {
			if number > uint64(i)*c.config.Epoch {
				masternodes = c.removePenaltiesFromBlock(chain, masternodes, number-uint64(i)*c.config.Epoch)
			}
		}

		for _, masternode := range masternodes {
			header.Extra = append(header.Extra, masternode[:]...)
		}

		if c.HookValidator != nil {
			_, err := c.HookValidator(header, masternodes)
			if err != nil {
				return err
			}
		}
	}
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	header.MixDigest = common.Hash{}

	header.Time = parent.Time + c.config.Period
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}

	return nil
}

// Finalize implements consensus.Engine.
// Finalize implements consensus.Engine. The blockAccessIndex and bal parameters are
// ignored for XDC chains (Amsterdam/EIP-7928 is disabled in XDC consensus).
func (c *XDPoS) Finalize(chain consensus.ChainHeaderReader, header *types.Header, statedb vm.StateDB, body *types.Body, blockAccessIndex uint32, bal *cbal.ConstructionBlockAccessList) {
	number := header.Number.Uint64()
	rCheckpoint := c.config.RewardCheckpoint
	if rCheckpoint == 0 {
		rCheckpoint = c.config.Epoch
	}

	// Cache signing transactions for every block (needed for reward calculation at checkpoints)
	// Block bodies may not be in DB yet during batch import, so we cache here while we have them
	if body != nil && len(body.Transactions) > 0 {
		c.CacheSigner(header.Hash(), body.Transactions)
	}

	// Persist V2 masternode snapshot at gap blocks so that future epoch-switch
	// headers can resolve their validator set during sync. This MUST run before
	// the V2 dispatch below because V2 Finalize returns and skips the rest of
	// this function. Canonical (XDPoSChain) achieves the same via
	// BlockChain.UpdateM1() -> engine.UpdateMasternodes() at writeBlockWithState
	// when number%epoch == epoch-gap (core/blockchain.go:1426). Without this,
	// gap-block snapshots only get written for V1 blocks, and every V2 epoch
	// after the first one (block 4435 onward) sees a stale V1-bootstrapped
	// snapshot at the gap and fails "validators not legit".
	if c.EngineV2 != nil && c.config.V2 != nil && c.config.V2.SwitchBlock != nil &&
		number >= c.config.V2.SwitchBlock.Uint64() && number%c.config.Epoch == c.config.Epoch-c.config.Gap {
		if cr, ok := chain.(consensus.ChainReader); ok {
			// Unwrap hookedStateDB (set by tracing.NewHookedState in state_processor.go)
			// — the direct type assertion to *state.StateDB silently fails for
			// wrapped state, skipping the masternode snapshot write and causing
			// chain-halt at the next V2 epoch boundary (devnet-5151 root cause
			// #3, 2026-06-13). Use Underlying() accessor on hookedStateDB.
			var sdb *state.StateDB
			if direct, ok := statedb.(*state.StateDB); ok {
				sdb = direct
			} else if hooked, ok := statedb.(interface{ Underlying() *state.StateDB }); ok {
				sdb = hooked.Underlying()
			}
			if sdb != nil {
				if err := c.EngineV2.UpdateMasternodesFromHeader(cr, header, sdb); err != nil {
					// Do NOT panic — a devnet/test chain may have no smart contract
					// candidates at the first gap block (Finalize runs before the block
					// is committed to the DB). Log and continue; the epoch-switch will
					// carry forward the previous epoch's masternodes.
					log.Error("[Finalize] UpdateMasternodesFromHeader failed", "block", number, "err", err)
				}
			} else {
				log.Error("[Finalize] statedb is not *state.StateDB and not Underlying-unwrappable, cannot update V2 masternodes", "block", number, "type", fmt.Sprintf("%T", statedb))
			}
		} else {
			log.Error("[Finalize] chain does not implement ChainReader, cannot update V2 masternodes", "block", number)
		}
	}

	// V2 dispatch (M-2): for V2 blocks, route Finalize to the V2 engine.
	// V2 Finalize returns (*types.Block, error); the wrapper's Finalize has no
	// return, so we invoke for side effects (state-root, reward hooks, masternode
	// snapshot persistence) and ignore the assembled block. See FinalizeAndAssemble
	// for the assembled-block path.
	if c.IsV2Block(header) && c.EngineV2 != nil {
		if sdb, ok := statedb.(*state.StateDB); ok {
			if cr, ok2 := chain.(consensus.ChainReader); ok2 {
				parentState := c.parentStateForReward
				if parentState == nil {
					parentState = sdb
				}
				var txs []*types.Transaction
				var uncles []*types.Header
				if body != nil {
					txs = body.Transactions
					uncles = body.Uncles
				}
				if _, err := c.EngineV2.Finalize(cr, header, sdb, parentState, txs, uncles, nil); err != nil {
					log.Error("EngineV2.Finalize failed", "number", number, "err", err)
				}
				c.parentStateForReward = nil
				return
			}
		}
	}

	// Apply rewards at checkpoint blocks
	// C6 FIX: V2 blocks use round-based IsEpochSwitch; V1 blocks use block-number modulo
	shouldApplyReward := false
	if c.IsV2Block(header) && c.EngineV2 != nil {
		isEpochSwitch, _, err := c.EngineV2.IsEpochSwitch(header)
		if err != nil {
			panic(fmt.Sprintf("[Finalize] IsEpochSwitch failed at block %d: %v", number, err))
		}
		shouldApplyReward = isEpochSwitch
	} else {
		shouldApplyReward = number%rCheckpoint == 0
	}

	if shouldApplyReward {
		if sdb, ok := statedb.(*state.StateDB); ok {
			// Use pre-tx parentState for reading voter/owner info (matches v2.6.8)
			parentState := c.parentStateForReward
			if parentState == nil {
				parentState = sdb
			}
			if c.HookReward != nil {
				log.Info("Finalize: Applying HookReward", "block", number)
				rewards, err := c.HookReward(chain, sdb, parentState, header)
				if err != nil {
					log.Error("HookReward failed in Finalize", "number", number, "err", err)
				} else {
					log.Info("Finalize: HookReward completed", "block", number, "rewards", len(rewards))
				}
			} else {
				// Direct reward calculation when no hook is set
				_, err := c.ApplyRewards(chain, sdb, parentState, header)
				if err != nil {
					log.Error("Error applying rewards in Finalize", "number", number, "err", err)
				}
			}
			// Clear parentState after use
			c.parentStateForReward = nil
		} else {
			log.Warn("Finalize: statedb is not *state.StateDB, cannot apply rewards", "block", number, "type", fmt.Sprintf("%T", statedb))
		}
	}

	// Compute state root after reward application.
	if sdb, ok := statedb.(*state.StateDB); ok {
		header.Root = sdb.IntermediateRoot(chain.Config().IsEIP158(header.Number))
		// XDC-PARITY-PROBE: per Opus advisory for devnet-5151 #1800 divergence RCA.
		// Emit post-Finalize state-root for every block <= 1800 plus checkpoints
		// (every 50 blocks) so we can pinpoint which block first diverges from V1's
		// canonical chain. Removable after divergent block is identified + fixed.
		if number <= 1800 && (number%50 == 0 || number >= 1795 || number == 1) {
			parentHash := header.ParentHash.Hex()
			parentRoot := ""
			if parent := chain.GetHeaderByNumber(number - 1); parent != nil {
				parentRoot = parent.Root.Hex()
			}
			txCount := 0
			if body != nil {
				txCount = len(body.Transactions)
			}
			log.Info("XDC-PARITY-PROBE",
				"block", number,
				"stateRoot", header.Root.Hex(),
				"parentHash", parentHash,
				"parentRoot", parentRoot,
				"txs", txCount)
		}
	}

	header.UncleHash = types.CalcUncleHash(nil)

	// Persist V2 masternode snapshot at gap blocks (V1-path only; V2 blocks return above).
	// For V2 blocks, the snapshot persistence happens via a different mechanism — see
	// engine_v2.UpdateMasternodesFromHeader wired in eth/backend.go.
	if c.EngineV2 != nil && c.config.V2 != nil && c.config.V2.SwitchBlock != nil &&
		number >= c.config.V2.SwitchBlock.Uint64() && number%c.config.Epoch == c.config.Epoch-c.config.Gap {
		if cr, ok := chain.(consensus.ChainReader); ok {
			if sdb, ok := statedb.(*state.StateDB); ok {
				if err := c.EngineV2.UpdateMasternodesFromHeader(cr, header, sdb); err != nil {
					// Do NOT panic — log and continue (see above for reasoning).
					log.Error("[Finalize] UpdateMasternodesFromHeader failed (V1 path)", "block", number, "err", err)
				}
			}
		}
	}
}

// FinalizeAndAssemble implements consensus.Engine.
func (c *XDPoS) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, statedb *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, error) {
	// V2 dispatch (M-3): compose at the wrapper. V2 has no FinalizeAndAssemble;
	// V2.Finalize returns (*types.Block, error) -- it both finalizes and assembles
	// per the legacy v2.6.8 signature. Reward gating inside V2.Finalize uses
	// IsEpochSwitch (round-based), not number%rCheckpoint.
	if c.IsV2Block(header) && c.EngineV2 != nil {
		if cr, ok := chain.(consensus.ChainReader); ok {
			parentState := c.parentStateForReward
			if parentState == nil {
				parentState = statedb.Copy()
			}
			var txs []*types.Transaction
			var uncles []*types.Header
			if body != nil {
				txs = body.Transactions
				uncles = body.Uncles
			}
			block, err := c.EngineV2.Finalize(cr, header, statedb, parentState, txs, uncles, receipts)
			c.parentStateForReward = nil
			if err != nil {
				return nil, err
			}
			if block != nil {
				return block, nil
			}
			// Defensive fallback: if V2 returned nil block (shouldn't happen), assemble locally.
			return types.NewBlock(header, body, receipts, trie.NewStackTrie(nil)), nil
		}
	}

	number := header.Number.Uint64()
	rCheckpoint := c.config.RewardCheckpoint
	if rCheckpoint == 0 {
		rCheckpoint = c.config.Epoch
	}

	if c.HookReward != nil && number%rCheckpoint == 0 {
		// Use statedb copy as parentState for reading owner/voter info
		parentState := statedb.Copy()
		rewards, err := c.HookReward(chain, statedb, parentState, header)
		if err != nil {
			return nil, err
		}
		if len(StoreRewardFolder) > 0 {
			data, err := json.Marshal(rewards)
			if err == nil {
				err = os.WriteFile(filepath.Join(StoreRewardFolder, header.Number.String()+"."+header.Hash().Hex()), data, 0644)
			}
			if err != nil {
				log.Error("Error when save reward info", "number", header.Number, "hash", header.Hash().Hex(), "err", err)
			}
		}
	}

	header.Root = statedb.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)

	return types.NewBlock(header, body, receipts, trie.NewStackTrie(nil)), nil
}

// Seal implements consensus.Engine.
func (c *XDPoS) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	header := block.Header()

	// V2 dispatch (M-4): adapt sync->async. V2.Seal returns (*types.Block, error)
	// synchronously; the wrapper pushes to a results channel.
	if c.IsV2Block(header) && c.EngineV2 != nil {
		if cr, ok := chain.(consensus.ChainReader); ok {
			sealed, err := c.EngineV2.Seal(cr, block, stop)
			if err != nil {
				return err
			}
			if sealed != nil {
				go func() {
					select {
					case results <- sealed:
					default:
						log.Warn("V2 sealing result is not read by miner", "sealhash", c.SealHash(header))
					}
				}()
			}
			return nil
		}
	}

	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	if c.config.Period == 0 && len(block.Transactions()) == 0 && number%c.config.Epoch != 0 {
		return errWaitTransactions
	}

	c.lock.RLock()
	signer, signFn := c.signer, c.signFn
	c.lock.RUnlock()

	snap, err := c.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	masternodes := c.GetMasternodes(chain, header)

	if _, authorized := snap.Signers[signer]; !authorized {
		valid := false
		for _, m := range masternodes {
			if m == signer {
				valid = true
				break
			}
		}
		if !valid {
			return errUnauthorized
		}
	}

	if len(masternodes) > 1 {
		for seen, recent := range snap.Recents {
			if recent == signer {
				if limit := uint64(2); number < limit || seen > number-limit {
					if number%c.config.Epoch != 0 {
						// Geth 1.17 sealing path: XdcAgent calls Seal synchronously
						// every 500ms and expects it to return quickly. Blocking on
						// <-stop here deadlocks the agent (no further tick fires),
						// which leaves the chain stuck after each signer hits its
						// cooldown. Return nil immediately so the next tick can
						// retry with fresh snapshot state — by then this signer
						// may be eligible again, or the in-turn signer may have
						// taken the slot. Matches v2.6.8 pattern where the worker
						// abandoned the attempt and a new goroutine retried.
						log.Trace("Signed recently — yielding to other signers", "number", number, "limit", limit, "seen", seen, "signer", recent.String())
						return nil
					}
				}
			}
		}
	}

	select {
	case <-stop:
		return nil
	default:
	}

	sighash, err := signFn(accounts.Account{Address: signer}, accounts.MimetypeClique, sigHash(header).Bytes())
	if err != nil {
		return err
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sighash)

	// #951 Defect 1c (produce side): V1 double validation. When this signer is its
	// OWN assigned M2 for this block, also write header.Validator = sighash (the SAME
	// signature over sigHash(header), since both the M1 Extra seal and the M2 Validator
	// sign the identical digest). Mirrors XDPoSChain engine_v1/engine.go:933-939. For
	// blocks whose assigned M2 is a DIFFERENT node, that node co-signs header.Validator
	// on receipt (appendM2HeaderHook co-sign path — separate change). Past the first
	// epoch a missing header.Validator is rejected by full-verify peers (legacy
	// ErrNoValidatorSignature). header.Validator is in Header.Hash(), so this is set
	// before block.WithSeal(header) below.
	if number > c.config.Epoch {
		if m2, mErr := c.GetValidator(signer, chain, header); mErr != nil {
			log.Warn("XDPoS V1: GetValidator(M2) failed during seal", "number", number, "err", mErr)
		} else if m2 == signer {
			header.Validator = sighash
		}
	}

	delay := time.Unix(int64(header.Time), 0).Sub(time.Now())
	if header.Difficulty.Cmp(diffNoTurn) == 0 {
		wiggle := time.Duration(len(masternodes)/2+1) * wiggleTime
		delay += time.Duration(rand.Int63n(int64(wiggle)))
		log.Trace("Out-of-turn signing requested", "wiggle", common.PrettyDuration(wiggle))
	}

	log.Trace("Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))
	go func() {
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		select {
		case results <- block.WithSeal(header):
		default:
			log.Warn("Sealing result is not read by miner", "sealhash", SealHash(header))
		}
	}()

	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm.
func (c *XDPoS) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	// V2 dispatch (M-5, CC-3 boundary): parent of the FIRST V2 block has
	// Number == V2.SwitchBlock, so use >= (not >) to cover that boundary.
	if c.EngineV2 != nil && c.config.V2 != nil && c.config.V2.SwitchBlock != nil &&
		parent != nil && parent.Number.Cmp(c.config.V2.SwitchBlock) >= 0 {
		if cr, ok := chain.(consensus.ChainReader); ok {
			return c.EngineV2.CalcDifficulty(cr, time, parent)
		}
	}
	return c.calcDifficulty(chain, parent, c.signer)
}

func (c *XDPoS) calcDifficulty(chain consensus.ChainHeaderReader, parent *types.Header, signer common.Address) *big.Int {
	numMNs, preIndex, curIndex, _, err := c.YourTurn(chain, parent, signer)
	if err != nil {
		return big.NewInt(int64(numMNs + curIndex - preIndex))
	}
	return big.NewInt(int64(numMNs - Hop(numMNs, preIndex, curIndex)))
}

// SealHash returns the hash of a block prior to it being sealed.
func (c *XDPoS) SealHash(header *types.Header) common.Hash {
	// V2 dispatch (M-6): V2 exposes SignHash (its analog of SealHash).
	if c.IsV2Block(header) && c.EngineV2 != nil {
		return c.EngineV2.SignHash(header)
	}
	return SealHash(header)
}

// Close implements consensus.Engine.
func (c *XDPoS) Close() error {
	return nil
}

// APIs returns the RPC namespaces this engine wants registered on the node.
//
// Namespace MUST be "XDPoS" (PascalCase) — JSON-RPC method names are
// case-sensitive per spec, and existing tooling (block explorers, masternode
// dashboards, validator monitors) calls XDPoS_getMasternodesByNumber,
// XDPoS_networkInformation, etc., not xdpos_*. Canonical XDPoSChain
// (consensus/XDPoS/XDPoS.go:176) registers "XDPoS"; we match. Mismatched case
// is what made #814 invisible until a 5-way RPC matrix exposed it.
//
// NOTE: modern geth removed APIs() from the consensus.Engine interface, so
// this method is no longer wired by upstream. eth/backend.go's Ethereum.APIs()
// must explicitly type-assert *XDPoS.XDPoS and append the result of this call.
// See #814.
func (c *XDPoS) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{{
		Namespace: "XDPoS",
		Service:   &API{chain: chain, xdpos: c},
	}}
}

// Authorize injects a private key into the consensus engine.
func (c *XDPoS) Authorize(signer common.Address, signFn SignerFn) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.signer = signer
	c.signFn = signFn

	// V2 dispatch (M-7): adapt the wrapper's 3-arg SignerFn (which carries a mimeType)
	// to V2's 2-arg engine_v2.SignerFn. The mimeType plumbed here matches the existing
	// wrapper-side seal call (accounts.MimetypeClique at the V1 Seal path); v2.6.8 used
	// no mimeType, so the choice is arbitrary as long as the underlying signing backend
	// accepts it.
	if c.EngineV2 != nil {
		v2SignFn := func(account accounts.Account, hash []byte) ([]byte, error) {
			return signFn(account, accounts.MimetypeClique, hash)
		}
		c.EngineV2.Authorize(signer, v2SignFn)
	}
}

// Signer returns the node's own authorized signer/coinbase address and
// whether the engine is actually authorized to seal (signFn set) — i.e.
// mining is live, not merely configured via a flag. Used by ethstats to
// report ground-truth role/coinbase at login time (XDCIndia/ItWorksinMyLocal#62).
func (c *XDPoS) Signer() (common.Address, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.signer, c.signFn != nil
}

// GetPeriod returns the configured block period
func (c *XDPoS) GetPeriod() uint64 { return c.config.Period }

// GetDb returns the database
func (c *XDPoS) GetDb() ethdb.Database { return c.db }

// GetSnapshot returns the snapshot at a given header
func (c *XDPoS) GetSnapshot(chain consensus.ChainHeaderReader, header *types.Header) (*Snapshot, error) {
	number := header.Number.Uint64()
	log.Trace("take snapshot", "number", number, "hash", header.Hash())
	return c.snapshot(chain, number, header.Hash(), nil)
}

// StoreSnapshot stores the snapshot to the database
func (c *XDPoS) StoreSnapshot(snap *Snapshot) error {
	return snap.store(c.db)
}

// YourTurn checks if it's the signer's turn to mine
func (c *XDPoS) YourTurn(chain consensus.ChainHeaderReader, parent *types.Header, signer common.Address) (int, int, int, bool, error) {
	masternodes := c.GetMasternodes(chain, parent)

	snap, err := c.GetSnapshot(chain, parent)
	if err != nil {
		log.Warn("Failed when trying to commit new work", "err", err)
		return 0, -1, -1, false, err
	}
	if len(masternodes) == 0 {
		return 0, -1, -1, false, errors.New("Masternodes not found")
	}

	pre := common.Address{}
	preIndex := -1
	if parent.Number.Uint64() != 0 {
		pre, err = whoIsCreator(snap, parent)
		if err != nil {
			log.Warn("whoIsCreator failed", "parent", parent.Number, "err", err)
			return 0, 0, 0, false, err
		}
		preIndex = position(masternodes, pre)
	}

	curIndex := position(masternodes, signer)
	mnStrs := make([]string, len(masternodes))
	for i, mn := range masternodes {
		mnStrs[i] = mn.Hex()[:16]
	}
	log.Info("YourTurn debug", "parent", parent.Number, "signer", signer, "pre", pre, "preIndex", preIndex, "curIndex", curIndex, "masternodes", len(masternodes), "mnList", mnStrs)

	// Pre-switch (V1) turn assignment routes to the canonical engine_v1 position
	// rule  masternodes[number % len] == signer  (number = parent.Number+1).
	//
	// The legacy wrapper decision was (preIndex+1)%len == curIndex — "you are the
	// masternode immediately after whoever created the parent". That next-after-
	// creator rule only agrees with the canonical position schedule while blocks
	// are produced in strict round-robin with no skips. The moment any slot is
	// filled by the timeout/Hop fallback (a down or not-yet-producing node), the
	// creator sequence gains a gap and preIndex+1 stops pointing at the node the
	// canonical chain expects for that height — so a freshly-seated producer (e.g.
	// this geth onboarding onto net #62) is asked out of phase, never matches its
	// real turn, and seals zero blocks. The position rule is height-absolute and
	// therefore agrees with the canonical oldxdc validators regardless of skips.
	// This is exactly the change that made erigon mint (erigon-xdc#364, which
	// rewrote YourTurnV1 the same way). preIndex/curIndex are still returned for
	// CalcDifficulty (Hop-based in/out-of-turn difficulty).
	number := parent.Number.Uint64() + 1
	preSwitch := c.config.V2 == nil || c.config.V2.SwitchBlock == nil ||
		number <= c.config.V2.SwitchBlock.Uint64()
	if preSwitch {
		// Gap block (number % epoch == epoch-1): no one mines (engine_v1 parity).
		if c.config.Epoch > 0 && number%c.config.Epoch == c.config.Epoch-1 {
			return len(masternodes), preIndex, curIndex, false, nil
		}
		offset := number % uint64(len(masternodes))
		myTurn := masternodes[offset] == signer
		return len(masternodes), preIndex, curIndex, myTurn, nil
	}

	if (preIndex+1)%len(masternodes) == curIndex {
		return len(masternodes), preIndex, curIndex, true, nil
	}
	return len(masternodes), preIndex, curIndex, false, nil
}

// GetMasternodes returns the list of masternodes for a given header.
// For V2-era blocks, delegates to the V2 engine which resolves the active set
// via getEpochSwitchInfo (round-based epoch boundaries + penalty removal).
// V1-style block-number-modulo lookup (n - n%epoch) is wrong for V2 because the
// epoch switch block is determined by round arithmetic, not block number.
// Canonical: XDPoSChain consensus/XDPoS/XDPoS.go:333-339.
func (c *XDPoS) GetMasternodes(chain consensus.ChainHeaderReader, header *types.Header) []common.Address {
	if c.IsV2Block(header) && c.EngineV2 != nil {
		// V2 engine's GetMasternodes needs ChainReader (a superset of ChainHeaderReader).
		// In normal operation chain is always *core.BlockChain which implements ChainReader;
		// fall back to V1 path only when the narrower interface is all that's available.
		if cr, ok := chain.(consensus.ChainReader); ok {
			return c.EngineV2.GetMasternodesV2(cr, header)
		}
		// Narrow interface only (e.g. test stubs) — extract directly from epoch-switch header.
		// GetMasternodesFromEpochSwitchHeader reads only header.Validators; chain param unused.
		return c.EngineV2.GetMasternodesFromEpochSwitchHeader(nil, header)
	}
	return c.GetMasternodesWithParents(chain, header, nil)
}

// GetMasternodesWithParents extracts masternodes, also checking pending parents for checkpoint
func (c *XDPoS) GetMasternodesWithParents(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) []common.Address {
	n := header.Number.Uint64()
	e := c.config.Epoch

	if n%e == 0 {
		// This is a checkpoint block - extract and cache masternodes
		masternodes := c.GetMasternodesFromCheckpointHeader(header, n, e)
		if len(masternodes) > 0 {
			checkpointCacheMu.Lock()
			checkpointCache[n] = masternodes
			checkpointCacheMu.Unlock()
			log.Debug("Cached checkpoint masternodes", "checkpoint", n, "count", len(masternodes))
		}
		return masternodes
	}

	// Need to find checkpoint at n - (n % e)
	checkpointNum := n - (n % e)

	// First check cache (fastest)
	checkpointCacheMu.RLock()
	if cached, ok := checkpointCache[checkpointNum]; ok {
		checkpointCacheMu.RUnlock()
		return cached
	}
	checkpointCacheMu.RUnlock()

	// Then try chain
	h := chain.GetHeaderByNumber(checkpointNum)
	if h != nil {
		masternodes := c.GetMasternodesFromCheckpointHeader(h, n, e)
		// Cache for future lookups
		if len(masternodes) > 0 {
			checkpointCacheMu.Lock()
			checkpointCache[checkpointNum] = masternodes
			checkpointCacheMu.Unlock()
		}
		return masternodes
	}

	// If not in chain, check parents array (during sync)
	for _, p := range parents {
		if p.Number.Uint64() == checkpointNum {
			masternodes := c.GetMasternodesFromCheckpointHeader(p, n, e)
			if len(masternodes) > 0 {
				checkpointCacheMu.Lock()
				checkpointCache[checkpointNum] = masternodes
				checkpointCacheMu.Unlock()
				log.Debug("Cached checkpoint from parents", "checkpoint", checkpointNum, "count", len(masternodes))
			}
			return masternodes
		}
	}

	return []common.Address{}
}

// GetMasternodesFromCheckpointHeader extracts masternode list from checkpoint header
func (c *XDPoS) GetMasternodesFromCheckpointHeader(preCheckpointHeader *types.Header, n, e uint64) []common.Address {
	if preCheckpointHeader == nil {
		log.Info("Previous checkpoint's header is empty", "block number", n, "epoch", e)
		return []common.Address{}
	}
	// V2 FIX: Delegate to the appropriate engine based on block version.
	// V2 epoch switch headers store masternodes in header.Validators, not Extra.
	// Reading Extra for V2 headers interprets QC data as addresses, producing
	// garbage masternode lists (e.g. 222 "masternodes" instead of 108).
	// Canonical (XDPoSChain XDPoS.go:449) does the same delegation.
	if c.IsV2Block(preCheckpointHeader) && c.EngineV2 != nil {
		// Type assert to access GetMasternodesFromEpochSwitchHeader which is not on EngineV2Iface
		if v2Engine, ok := c.EngineV2.(interface {
			GetMasternodesFromEpochSwitchHeader(chain consensus.ChainReader, header *types.Header) []common.Address
		}); ok {
			return v2Engine.GetMasternodesFromEpochSwitchHeader(nil, preCheckpointHeader)
		}
	}
	// V1 epoch switch (block-number modulo)
	masternodes := make([]common.Address, (len(preCheckpointHeader.Extra)-extraVanity-extraSeal)/common.AddressLength)
	for i := 0; i < len(masternodes); i++ {
		copy(masternodes[i][:], preCheckpointHeader.Extra[extraVanity+i*common.AddressLength:])
	}
	return masternodes
}

// UpdateMasternodes updates the masternode list in the snapshot
func (c *XDPoS) UpdateMasternodes(chain consensus.ChainHeaderReader, header *types.Header, ms []Masternode) error {
	number := header.Number.Uint64()
	log.Trace("take snapshot", "number", number, "hash", header.Hash())

	snap, err := c.snapshot(chain, number, header.Hash(), nil)
	if err != nil {
		return err
	}

	newMasternodes := make(map[common.Address]struct{})
	for _, m := range ms {
		newMasternodes[m.Address] = struct{}{}
	}
	snap.Signers = newMasternodes

	nm := []string{}
	for _, n := range ms {
		nm = append(nm, n.Address.String())
	}
	c.recents.Add(snap.Hash, snap)
	log.Info("New set of masternodes has been updated to snapshot", "number", snap.Number, "hash", snap.Hash, "new masternodes", nm)
	return nil
}

// RecoverSigner recovers the signer from a header
func (c *XDPoS) RecoverSigner(header *types.Header) (common.Address, error) {
	return ecrecover(header, c.signatures)
}

// RecoverValidator recovers the validator from a header's Validator field
// RecoverValidator recovers the M2 validator address from the 65-byte signature in
// header.Validator (signed over sigHash(header) — the SAME digest as the M1 Extra
// seal). Byte-exact port of XDPoSChain engine_v1/engine.go RecoverValidator. Used by
// V1 double validation; do NOT confuse with RecoverSigner (the M1 creator, recovered
// from the Extra seal). Refs #951 Defect 1c.
func (c *XDPoS) RecoverValidator(header *types.Header) (common.Address, error) {
	hash := header.Hash()
	if address, known := c.validatorSignatures.Get(hash); known {
		return address, nil
	}
	if len(header.Validator) != extraSeal {
		return common.Address{}, errFailValidatorSignature
	}
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), header.Validator)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
	c.validatorSignatures.Add(hash, signer)
	return signer, nil
}

// CacheData caches signing transactions from a block, dropping a tx only when
// its matching receipt has status ReceiptStatusFailed. "No matching receipt
// found" is NOT the same condition as "found and failed" and must not collide
// on the shared zero value of ReceiptStatusFailed/uninitialized uint64 -- that
// collision previously caused every signing tx to be dropped whenever
// receipts arrived without derived fields (e.g. rawdb.ReadRawReceipts, whose
// ReceiptForStorage never carries TxHash, so the match can never succeed).
// Issue #1364.
func (c *XDPoS) CacheData(header *types.Header, txs []*types.Transaction, receipts []*types.Receipt) []*types.Transaction {
	signTxs := []*types.Transaction{}
	for _, tx := range txs {
		if isSigningTransaction(tx) {
			found := false
			var status uint64
			for _, r := range receipts {
				if r.TxHash == tx.Hash() {
					found = true
					if len(r.PostState) > 0 {
						status = types.ReceiptStatusSuccessful
					} else {
						status = r.Status
					}
					break
				}
			}
			if found && status == types.ReceiptStatusFailed {
				continue
			}
			if !found {
				log.Warn("CacheData: no matching receipt for signing tx, keeping it (presence-only, not treated as failed)",
					"hash", header.Hash().String(), "number", header.Number, "tx", tx.Hash().String())
			}
			signTxs = append(signTxs, tx)
		}
	}
	log.Debug("Save tx signers to cache", "hash", header.Hash().String(), "number", header.Number, "len(txs)", len(signTxs))
	c.BlockSigners.Add(header.Hash(), signTxs)
	return signTxs
}

// CacheSigner caches signing transactions by hash
func (c *XDPoS) CacheSigner(hash common.Hash, txs []*types.Transaction) []*types.Transaction {
	signTxs := []*types.Transaction{}
	for _, tx := range txs {
		if isSigningTransaction(tx) {
			signTxs = append(signTxs, tx)
		}
	}
	log.Debug("Save tx signers to cache", "hash", hash.String(), "len(txs)", len(signTxs))
	c.BlockSigners.Add(hash, signTxs)
	return signTxs
}

// snapshot retrieves the authorization snapshot at a given point in time.
func (c *XDPoS) snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	var (
		headers []*types.Header
		snap    *Snapshot
	)
	for snap == nil {
		if len(headers) >= snapshotMaxWalkBack {
			log.Error("snapshot walk-back exceeded safe limit without finding a checkpoint snapshot",
				"start", number+uint64(len(headers)), "current", number, "limit", snapshotMaxWalkBack)
			return nil, fmt.Errorf("snapshot walk-back exceeded %d blocks (missing disk snapshots?)", snapshotMaxWalkBack)
		}
		if s, ok := c.recents.Get(hash); ok {
			snap = s
			break
		}
		if (number+c.config.Gap)%c.config.Epoch == 0 {
			if s, err := loadSnapshot(c.config, c.signatures, c.db, hash); err == nil {
				log.Trace("Loaded voting snapshot from disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}
		// MISSING SNAPSHOT BOOTSTRAP: If we're at a gap block and no snapshot exists
		// on disk, try to rebuild it from the UPCOMING epoch checkpoint block header.
		// At gap block N where (N+Gap)%Epoch==0, the NEXT checkpoint at N+Gap contains
		// the elected masternode list for the epoch that begins there. That list is
		// exactly what the gap snapshot must hold.
		if (number+c.config.Gap)%c.config.Epoch == 0 {
			nextCheckpoint := number + c.config.Gap
			checkpointHeader := chain.GetHeaderByNumber(nextCheckpoint)
			if checkpointHeader == nil {
				// FIX #418: Next checkpoint not in DB yet. For V2 gap blocks, the CURRENT
				// header should have Validators set with the upcoming epoch's masternodes.
				// Try to bootstrap from the current header before falling through to walk-back.
				isV2Gap := c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number >= c.config.V2.SwitchBlock.Uint64()
				if isV2Gap {
					currentHeader := chain.GetHeaderByNumber(number)
					if currentHeader != nil && len(currentHeader.Validators) > 0 && len(currentHeader.Validators)%common.AddressLength == 0 {
						masternodes := make([]common.Address, len(currentHeader.Validators)/common.AddressLength)
						for i := 0; i < len(masternodes); i++ {
							copy(masternodes[i][:], currentHeader.Validators[i*common.AddressLength:])
						}
						// Sanity check: reject all-zero addresses
						allZero := true
						for _, mn := range masternodes {
							if mn != (common.Address{}) {
								allZero = false
								break
							}
						}
						if !allZero && len(masternodes) > 0 {
							log.Info("Bootstrapping missing snapshot from CURRENT gap header (next checkpoint not in DB)",
								"gapNumber", number, "expectedCheckpoint", nextCheckpoint,
								"masternodes", len(masternodes))
							snap = newSnapshot(c.config, c.signatures, number, hash, masternodes)
							if err := snap.store(c.db); err != nil {
								return nil, fmt.Errorf("snapshot bootstrap: failed to store gap snapshot at %d: %w", number, err)
							}
							c.recents.Add(snap.Hash, snap)
							break
						}
					}
				}
				log.Warn("snapshot bootstrap: next checkpoint not yet in DB, falling through to walk-back",
					"gapNumber", number, "nextCheckpoint", nextCheckpoint)
			} else {
				// CRITICAL FIX: For V2 blocks, prefer header.Validators (xdc2.6.8 format).
				// For V1 blocks, header.Validators holds 4-byte ASCII M2 indices, NOT addresses.
				// We must use header.Extra for V1 and header.Validators for V2.
				var masternodes []common.Address
				isV2Checkpoint := c.config.V2 != nil && c.config.V2.SwitchBlock != nil && nextCheckpoint > c.config.V2.SwitchBlock.Uint64()
				if isV2Checkpoint && len(checkpointHeader.Validators) > 0 && len(checkpointHeader.Validators)%common.AddressLength == 0 {
					masternodes = make([]common.Address, len(checkpointHeader.Validators)/common.AddressLength)
					for i := 0; i < len(masternodes); i++ {
						copy(masternodes[i][:], checkpointHeader.Validators[i*common.AddressLength:])
					}
					// Sanity check: reject all-zero addresses (indicates corrupt/invalid Validators field)
					allZero := true
					for _, mn := range masternodes {
						if mn != (common.Address{}) {
							allZero = false
							break
						}
					}
					if allZero {
						log.Warn("snapshot bootstrap: header.Validators contains all-zero addresses, falling back to Extra",
							"gapNumber", number, "nextCheckpoint", nextCheckpoint)
						masternodes = nil
					}
				}
				if len(masternodes) == 0 {
					extraSuffix := len(checkpointHeader.Extra) - extraSeal
					if extraSuffix > extraVanity {
						masternodes = extractAddressFromBytes(checkpointHeader.Extra[extraVanity:extraSuffix])
					}
				}
				if len(masternodes) > 0 {
					log.Info("Bootstrapping missing snapshot from NEXT epoch checkpoint",
						"gapNumber", number, "nextCheckpoint", nextCheckpoint,
						"masternodes", len(masternodes), "source", func() string {
							if len(checkpointHeader.Validators) > 0 {
								return "Validators"
							}
							return "Extra"
						}())
					snap = newSnapshot(c.config, c.signatures, number, hash, masternodes)
					if err := snap.store(c.db); err != nil {
						return nil, fmt.Errorf("snapshot bootstrap: failed to store snapshot at %d: %w", number, err)
					}
					c.recents.Add(snap.Hash, snap)
					break
				}
			}
		}
		if number == 0 {
			genesis := chain.GetHeaderByNumber(0)
			if err := c.VerifyHeader(chain, genesis); err != nil {
				return nil, err
			}
			signers := make([]common.Address, (len(genesis.Extra)-extraVanity-extraSeal)/common.AddressLength)
			for i := 0; i < len(signers); i++ {
				copy(signers[i][:], genesis.Extra[extraVanity+i*common.AddressLength:])
			}
			snap = newSnapshot(c.config, c.signatures, 0, genesis.Hash(), signers)
			if err := snap.store(c.db); err != nil {
				return nil, err
			}
			log.Trace("Stored genesis voting snapshot to disk")
			break
		}
		var header *types.Header
		if len(parents) > 0 {
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				// v8.2: parents slice doesn't match expected chain — fall through to chain DB
				// This happens when snapshot walks back past the parents slice boundary.
				// Previous code returned ErrUnknownAncestor here, but the header might
				// still be in the chain DB from a previous sync cycle.
				header = chain.GetHeader(hash, number)
				if header == nil {
					log.Warn("v8.2: snapshot walk-back failed (parents mismatch AND chain miss)",
						"walkNumber", number, "walkHash", hash.Hex()[:16],
						"parentsLen", len(parents),
						"parentTipNumber", parents[len(parents)-1].Number.Uint64(),
						"parentTipHash", parents[len(parents)-1].Hash().Hex()[:16],
						"headersCollected", len(headers))
					log.Warn("[DIAG-xdpos3] snapshot DB fallback ErrUnknownAncestor", "number", number, "hash", hash.Hex()[:16])
					return nil, consensus.ErrUnknownAncestor
				}
				// Successfully found in chain DB — clear parents so we don't re-check
				parents = nil
			}
		} else {
			header = chain.GetHeader(hash, number)
			if header == nil {
				log.Warn("v8.2: snapshot walk-back failed (header nil from chain)",
					"walkNumber", number, "walkHash", hash.Hex()[:16],
					"headersCollected", len(headers))
				log.Warn("[DIAG-xdpos4] snapshot chain.GetHeader ErrUnknownAncestor", "number", number, "hash", hash.Hex()[:16])
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}
	snap, err := snap.apply(headers)
	if err != nil {
		return nil, err
	}
	c.recents.Add(snap.Hash, snap)

	if (snap.Number+c.config.Gap)%c.config.Epoch == 0 {
		if err = snap.store(c.db); err != nil {
			return nil, err
		}
		log.Trace("Stored voting snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}

func (c *XDPoS) removePenaltiesFromBlock(chain consensus.ChainHeaderReader, masternodes []common.Address, epochNumber uint64) []common.Address {
	if epochNumber <= 0 {
		return masternodes
	}
	header := chain.GetHeaderByNumber(epochNumber)
	if header == nil {
		return masternodes
	}
	// Extract penalties stored in the checkpoint header's Penalties field.
	// v2.6.8 records penalized masternodes here so that subsequent epochs can
	// exclude them from the signing set for LimitPenaltyEpoch epochs.
	penalties := extractAddressFromBytes(header.Penalties)
	if len(penalties) == 0 {
		return masternodes
	}
	log.Debug("Removing penalties from previous epoch",
		"epochBlock", epochNumber,
		"penaltyCount", len(penalties))
	return removeItemFromArray(masternodes, penalties)
}

// Helper functions

func whoIsCreator(snap *Snapshot, header *types.Header) (common.Address, error) {
	if header.Number.Uint64() == 0 {
		return common.Address{}, errors.New("Don't take block 0")
	}
	return ecrecover(header, snap.sigcache)
}

func position(list []common.Address, x common.Address) int {
	for i, item := range list {
		if item == x {
			return i
		}
	}
	return -1
}

// Hop calculates the distance between two positions in the masternode list
func Hop(length, pre, cur int) int {
	switch {
	case pre < cur:
		return cur - (pre + 1)
	case pre > cur:
		return (length - pre) + (cur - 1)
	default:
		return length - 1
	}
}

func compareSignersLists(list1 []common.Address, list2 []common.Address) bool {
	if len(list1) == 0 && len(list2) == 0 {
		return true
	}
	if len(list1) != len(list2) {
		return false
	}
	l1 := make([]common.Address, len(list1))
	l2 := make([]common.Address, len(list2))
	copy(l1, list1)
	copy(l2, list2)
	sort.Slice(l1, func(i, j int) bool { return l1[i].String() <= l1[j].String() })
	sort.Slice(l2, func(i, j int) bool { return l2[i].String() <= l2[j].String() })
	return reflect.DeepEqual(l1, l2)
}

func removeItemFromArray(array []common.Address, toRemove []common.Address) []common.Address {
	result := make([]common.Address, 0)
	for _, item := range array {
		found := false
		for _, r := range toRemove {
			if item == r {
				found = true
				break
			}
		}
		if !found {
			result = append(result, item)
		}
	}
	return result
}

func extractAddressFromBytes(byteAddresses []byte) []common.Address {
	if len(byteAddresses)%common.AddressLength != 0 {
		return []common.Address{}
	}
	addresses := make([]common.Address, len(byteAddresses)/common.AddressLength)
	for i := 0; i < len(addresses); i++ {
		copy(addresses[i][:], byteAddresses[i*common.AddressLength:])
	}
	return addresses
}

// ExtractValidatorsFromBytes extracts validator indices from bytes
func ExtractValidatorsFromBytes(byteValidators []byte) []int64 {
	lenValidator := len(byteValidators) / M2ByteLength
	var validators []int64
	for i := 0; i < lenValidator; i++ {
		trimByte := bytes.Trim(byteValidators[i*M2ByteLength:(i+1)*M2ByteLength], "\x00")
		intNumber, err := strconv.Atoi(string(trimByte))
		if err != nil {
			log.Error("Can not convert string to integer", "error", err)
			return []int64{}
		}
		validators = append(validators, int64(intNumber))
	}
	return validators
}

func isSigningTransaction(tx *types.Transaction) bool {
	return isSigningTx(tx)
}

// CacheSigningTxsFromBlock extracts and caches signing transactions from a block.
// This is called during block body validation to ensure signing txs are available
// for reward calculation during Finalize, even when blocks haven't been written
// to the database yet (e.g., during initial sync at checkpoint blocks).
func (c *XDPoS) CacheSigningTxsFromBlock(block *types.Block) {
	hash := block.Hash()
	txs := block.Transactions()

	signTxs := make([]*types.Transaction, 0)
	for _, tx := range txs {
		if isSigningTx(tx) {
			signTxs = append(signTxs, tx)
		}
	}

	if len(signTxs) > 0 {
		log.Debug("CacheSigningTxsFromBlock: cached signing txs",
			"hash", hash.String(),
			"number", block.NumberU64(),
			"txs", len(signTxs))
	}
	c.BlockSigners.Add(hash, signTxs)
}

// GetCachedSigningTxs retrieves cached signing transactions for a block hash
func (c *XDPoS) GetCachedSigningTxs(hash common.Hash) ([]*types.Transaction, bool) {
	return c.BlockSigners.Get(hash)
}

var wiggleTime = 500 * time.Millisecond

var _ consensus.Engine = (*XDPoS)(nil)
