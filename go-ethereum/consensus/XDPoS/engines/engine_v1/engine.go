// Copyright (c) 2024 XDC Network
// XDPoS V1 consensus engine — extracted from xdpos.go
// Matches v2.6.8 engines/engine_v1/engine.go structure
// This file provides the V1 engine struct for clean V1/V2 dispatch.

package engine_v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/engines"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/core/state"

	"golang.org/x/crypto/sha3"
)

const snapshotMaxWalkBack = 2_000_000 // Safety limit to prevent OOM when disk snapshots are missing

var (
	errUnknownBlock                 = errors.New("unknown block")
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")
	errInvalidVote                  = errors.New("vote nonce not 0x00..0 or 0xff..f")
	errInvalidCheckpointVote        = errors.New("vote nonce in checkpoint block non-zero")
	errMissingVanity                = errors.New("extra-data 32 byte vanity prefix missing")
	errExtraSigners                 = errors.New("non-checkpoint block contains extra signer list")
	errInvalidCheckpointSigners     = errors.New("invalid signer list on checkpoint block")
	errInvalidMixDigest             = errors.New("non-zero mix digest")
	errInvalidUncleHash             = errors.New("non empty uncle hash")
	errInvalidDifficulty            = errors.New("invalid difficulty")
	errInvalidTimestamp             = errors.New("invalid timestamp")
	errInvalidVotingChain           = errors.New("invalid voting chain")
	errUnauthorized                 = errors.New("unauthorized")
	errMissingSignature             = errors.New("extra-data 65 byte suffix signature missing")
)

var (
	nonceAuthVote = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	nonceDropVote = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	uncleHash     = types.CalcUncleHash(nil)
)

// XDPoS_v1 is the V1 consensus engine (pre-HotStuff BFT).
// V1 uses a round-robin masternode selection with epoch/gap/checkpoint boundaries.
// V1 is active from genesis to V2SwitchBlock (56,828,700 on mainnet).
type XDPoS_v1 struct {
	config     *params.XDPoSConfig
	db         ethdb.Database
	recents    *lru.Cache[common.Hash, *SnapshotV1]
	signatures *lru.Cache[common.Hash, common.Address]

	signer common.Address
	signFn func(signer common.Address, mimeType string, message []byte) ([]byte, error)
	lock   sync.RWMutex
}

// SnapshotV1 is the V1 snapshot of masternode state
type SnapshotV1 struct {
	config   *params.XDPoSConfig                     `json:"-"`
	sigcache *lru.Cache[common.Hash, common.Address] `json:"-"`

	Number  uint64                     `json:"number"`
	Hash    common.Hash                `json:"hash"`
	Signers map[common.Address]struct{} `json:"signers"`
	Recents map[uint64]common.Address  `json:"recents"`
	Votes   []*clique.Vote             `json:"votes"`
	Tally   map[common.Address]clique.Tally `json:"tally"`
}

// New creates a new V1 engine
func New(config *params.XDPoSConfig, db ethdb.Database) *XDPoS_v1 {
	recents := lru.NewCache[common.Hash, *SnapshotV1](256)
	signatures := lru.NewCache[common.Hash, common.Address](4096)

	return &XDPoS_v1{
		config:     config,
		db:         db,
		recents:    recents,
		signatures: signatures,
	}
}

// Author returns the block signer
func (x *XDPoS_v1) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, x.signatures)
}

// VerifyHeader verifies a V1 block header
func (x *XDPoS_v1) VerifyHeader(chain consensus.ChainReader, header *types.Header, fullVerify bool) error {
	return x.verifyHeader(chain, header, nil, fullVerify)
}

// verifyHeader is the internal V1 header verification
func (x *XDPoS_v1) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header, fullVerify bool) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	if fullVerify {
		// Don't waste time checking blocks from the future
		if header.Time > uint64(time.Now().Unix()) {
			return consensus.ErrFutureBlock
		}
	}

	// Checkpoint blocks need to enforce zero beneficiary
	checkpoint := (number % x.config.Epoch) == 0
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
	if len(header.Extra) < engines.ExtraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < engines.ExtraVanity+engines.ExtraSeal {
		return errMissingSignature
	}

	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise
	signersBytes := len(header.Extra) - engines.ExtraVanity - engines.ExtraSeal
	if !checkpoint && signersBytes != 0 {
		return errExtraSigners
	}
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	// Ensure that the block doesn't contain any uncles which are meaningless in XDPoS_v1
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// All basic checks passed, verify cascading fields
	return x.verifyCascadingFields(chain, header, parents, fullVerify)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers.
func (x *XDPoS_v1) verifyCascadingFields(chain consensus.ChainReader, header *types.Header, parents []*types.Header, fullVerify bool) error {
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
		return consensus.ErrUnknownAncestor
	}
	if parent.Time+x.config.Period > header.Time {
		return errInvalidTimestamp
	}

	// Non-checkpoint blocks: verify seal directly
	if number%x.config.Epoch != 0 {
		return x.verifySeal(chain, header, parents, fullVerify)
	}

	// Checkpoint blocks: skip signer-list verification during fast sync
	// (fullVerify=false). The snapshot may be incomplete during initial header
	// download, so the check would produce spurious mismatches. Port of
	// XDPoSChain fdfeb9f21 — only gate the snapshot/signer check, not verifySeal.
	if fullVerify {
		// Checkpoint blocks: retrieve snapshot and verify signer list matches
		snap, err := x.snapshot(chain, number-1, header.ParentHash, parents, nil)
		if err != nil {
			return err
		}

		extraSuffix := len(header.Extra) - engines.ExtraSeal
		masternodesFromCheckpointHeader := extractAddressFromBytes(header.Extra[engines.ExtraVanity:extraSuffix])
		signers := snap.GetSigners()

		if !compareSignersLists(masternodesFromCheckpointHeader, signers) {
			// During initial sync, penalty history may be missing causing a mismatch.
			// If the checkpoint header has a valid non-empty signer list, bootstrap the
			// snapshot from it so downstream epochs start from the correct state.
			if len(masternodesFromCheckpointHeader) > 0 {
				log.Warn("Checkpoint signer mismatch — bootstrapping snapshot from header",
					"number", number,
					"header_masternodes", len(masternodesFromCheckpointHeader),
					"snapshot_signers", len(signers))
				newSigners := make(map[common.Address]struct{}, len(masternodesFromCheckpointHeader))
				for _, mn := range masternodesFromCheckpointHeader {
					newSigners[mn] = struct{}{}
				}
				snap.Signers = newSigners
				if err := snap.store(x.db); err != nil {
					log.Warn("Failed to persist corrected checkpoint snapshot", "number", number, "err", err)
				}
				x.recents.Add(snap.Hash, snap)
			} else {
				log.Error("Masternodes lists are different in checkpoint header and snapshot",
					"number", number,
					"masternodes_from_checkpoint_header", masternodesFromCheckpointHeader,
					"masternodes_in_snapshot", signers)
				return errInvalidCheckpointSigners
			}
		}
	}

	return x.verifySeal(chain, header, parents, fullVerify)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements.
func (x *XDPoS_v1) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header, fullVerify bool) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := x.snapshot(chain, number-1, header.ParentHash, parents, nil)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	creator, err := ecrecover(header, x.signatures)
	if err != nil {
		return err
	}

	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	// Verify difficulty
	difficulty := x.calcDifficulty(chain, parent, creator)
	if number > 0 && difficulty.Int64() != 0 {
		if header.Difficulty.Int64() != difficulty.Int64() {
			return errInvalidDifficulty
		}
	}

	// Get masternodes for this header
	masternodes := x.GetMasternodes(chain, header)

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

	// Check recent signers to prevent consecutive blocks
	if len(masternodes) > 1 {
		for seen, recent := range snap.Recents {
			if recent == creator {
				if limit := uint64(2); seen > number-limit {
					// Only take into account the non-epoch blocks
					if number%x.config.Epoch != 0 {
						return errUnauthorized
					}
				}
			}
		}
	}

	return nil
}

// calcDifficulty calculates the difficulty for a block based on the signer position.
func (x *XDPoS_v1) calcDifficulty(chain consensus.ChainReader, parent *types.Header, signer common.Address) *big.Int {
	masternodes := x.GetMasternodes(chain, parent)
	if len(masternodes) == 0 {
		return big.NewInt(1)
	}

	length := len(masternodes)
	preIndex := -1
	if parent != nil && parent.Number.Uint64() != 0 {
		pre, err := ecrecover(parent, x.signatures)
		if err == nil {
			preIndex = position(masternodes, pre)
		}
	}
	curIndex := position(masternodes, signer)
	if curIndex == -1 {
		return big.NewInt(1)
	}

	hop := 0
	switch {
	case preIndex < curIndex:
		hop = curIndex - (preIndex + 1)
	case preIndex > curIndex:
		hop = (length - preIndex) + (curIndex - 1)
	default:
		hop = length - 1
	}
	return big.NewInt(int64(length - hop))
}

// GetSnapshot returns the V1 snapshot for a header
func (x *XDPoS_v1) GetSnapshot(chain consensus.ChainReader, header *types.Header) (*SnapshotV1, error) {
	number := header.Number.Uint64()
	return x.snapshot(chain, number, header.Hash(), nil, header)
}

// GetMasternodes returns the masternode list for a header
func (x *XDPoS_v1) GetMasternodes(chain consensus.ChainReader, header *types.Header) []common.Address {
	n := header.Number.Uint64()
	e := x.config.Epoch
	switch {
	case n%e == 0:
		return x.GetMasternodesFromCheckpointHeader(header)
	case n%e != 0:
		h := chain.GetHeaderByNumber(n - (n % e))
		if h == nil {
			log.Warn("[GetMasternodes v1] epoch switch block header nil", "BlockNum", n)
		}
		return x.GetMasternodesFromCheckpointHeader(h)
	default:
		return []common.Address{}
	}
}

// GetMasternodesFromCheckpointHeader extracts masternode list from checkpoint header
func (x *XDPoS_v1) GetMasternodesFromCheckpointHeader(checkpointHeader *types.Header) []common.Address {
	if checkpointHeader == nil {
		return []common.Address{}
	}
	masternodes := make([]common.Address, (len(checkpointHeader.Extra)-engines.ExtraVanity-engines.ExtraSeal)/common.AddressLength)
	for i := 0; i < len(masternodes); i++ {
		copy(masternodes[i][:], checkpointHeader.Extra[engines.ExtraVanity+i*common.AddressLength:])
	}
	return masternodes
}

// GetCurrentEpochSwitchBlock returns the V1 epoch switch block
func (x *XDPoS_v1) GetCurrentEpochSwitchBlock(blockNumber uint64) (uint64, uint64, error) {
	epochNum := blockNumber / x.config.Epoch
	switchBlock := epochNum * x.config.Epoch
	return switchBlock, epochNum, nil
}

// YourTurn checks if the signer should mine the next block (V1 round-robin).
// Implements full epoch/gap/checkpoint logic matching v2.6.8.
//
// Rules:
//   - Gap blocks (number % epoch == epoch-1): no one can mine
//   - Checkpoint blocks (number % epoch == 0): use masternodes from checkpoint header
//   - Normal blocks: round-robin through current masternode list
//   - Double validation: M2 signer must be different from M1
func (x *XDPoS_v1) YourTurn(chain consensus.ChainReader, parent *types.Header, signer common.Address) (bool, error) {
	number := parent.Number.Uint64() + 1
	epoch := x.config.Epoch

	// Gap block: no mining allowed
	if number%epoch == epoch-1 {
		return false, nil
	}

	masternodes := x.GetMasternodes(chain, parent)
	if len(masternodes) == 0 {
		return false, nil
	}

	// Checkpoint block: validate against checkpoint header's masternode list
	if number%epoch == 0 {
		// Load masternodes from the parent's extra data (checkpoint block encodes masternodes)
		checkpointMasternodes := x.GetMasternodesFromCheckpointHeader(parent)
		if len(checkpointMasternodes) > 0 {
			masternodes = checkpointMasternodes
		}
	}

	offset := number % uint64(len(masternodes))
	if masternodes[offset] == signer {
		return true, nil
	}
	return false, nil
}

// IsAuthorisedAddress checks if an address is in the V1 masternode list
func (x *XDPoS_v1) IsAuthorisedAddress(chain consensus.ChainReader, header *types.Header, address common.Address) bool {
	masternodes := x.GetMasternodes(chain, header)
	for _, mn := range masternodes {
		if mn == address {
			return true
		}
	}
	return false
}

// GetPeriod returns the V1 mining period
func (x *XDPoS_v1) GetPeriod() uint64 {
	return x.config.Period
}

// Authorize injects a private key for block signing
func (x *XDPoS_v1) Authorize(signer common.Address, signFn func(signer common.Address, mimeType string, message []byte) ([]byte, error)) {
	x.lock.Lock()
	defer x.lock.Unlock()
	x.signer = signer
	x.signFn = signFn
}

// snapshot retrieves the authorization snapshot at a given point in time.
func (x *XDPoS_v1) snapshot(chain consensus.ChainReader, number uint64, hash common.Hash, parents []*types.Header, selfHeader *types.Header) (*SnapshotV1, error) {
	var (
		headers []*types.Header
		snap    *SnapshotV1
	)
	for snap == nil {
		if len(headers) >= snapshotMaxWalkBack {
			log.Error("snapshot walk-back exceeded safe limit",
				"start", number+uint64(len(headers)), "current", number, "limit", snapshotMaxWalkBack)
			return nil, errors.New("snapshot walk-back exceeded safe limit")
		}

		// If an in-memory snapshot was found, use that
		if s, ok := x.recents.Get(hash); ok && s != nil {
			snap = s
			break
		}

		// If an on-disk checkpoint snapshot can be found, use that
		if (number+x.config.Gap)%x.config.Epoch == 0 {
			if s, err := loadSnapshot(x.config, x.signatures, x.db, hash); err == nil {
				log.Trace("Loaded voting snapshot from disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}
		// MISSING SNAPSHOT BOOTSTRAP: If we're at a gap block and no snapshot exists
		// on disk, try to rebuild it from the UPCOMING epoch checkpoint block header.
		if (number+x.config.Gap)%x.config.Epoch == 0 {
			nextCheckpoint := number + x.config.Gap
			checkpointHeader := chain.GetHeaderByNumber(nextCheckpoint)
			if checkpointHeader == nil {
				log.Warn("snapshot bootstrap: next checkpoint not yet in DB, falling through to walk-back",
					"gapNumber", number, "nextCheckpoint", nextCheckpoint)
			} else {
				extraSuffix := len(checkpointHeader.Extra) - engines.ExtraSeal
				if extraSuffix > engines.ExtraVanity {
					masternodes := extractAddressFromBytes(checkpointHeader.Extra[engines.ExtraVanity:extraSuffix])
					if len(masternodes) > 0 {
						log.Info("Bootstrapping missing snapshot from NEXT epoch checkpoint",
							"gapNumber", number, "nextCheckpoint", nextCheckpoint,
							"masternodes", len(masternodes))
						snap = newSnapshot(x.config, x.signatures, number, hash, masternodes)
						if err := snap.store(x.db); err != nil {
							log.Warn("Failed to store bootstrapped snapshot", "err", err)
						}
						x.recents.Add(snap.Hash, snap)
						break
					}
				}
			}
		}

		// If we're at block zero, make a snapshot
		if number == 0 {
			genesis := chain.GetHeaderByNumber(0)
			if err := x.verifyHeader(chain, genesis, nil, true); err != nil {
				return nil, err
			}
			signers := make([]common.Address, (len(genesis.Extra)-engines.ExtraVanity-engines.ExtraSeal)/common.AddressLength)
			for i := 0; i < len(signers); i++ {
				copy(signers[i][:], genesis.Extra[engines.ExtraVanity+i*common.AddressLength:])
			}
			snap = newSnapshot(x.config, x.signatures, 0, genesis.Hash(), signers)
			if err := snap.store(x.db); err != nil {
				return nil, err
			}
			log.Trace("Stored genesis voting snapshot to disk")
			break
		}

		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else if selfHeader != nil && selfHeader.Hash() == hash {
			header = selfHeader
		} else {
			header = chain.GetHeader(hash, number)
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}

	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}
	snap, err := snap.apply(headers)
	if err != nil {
		return nil, err
	}
	x.recents.Add(snap.Hash, snap)

	// If we've generated a new checkpoint snapshot, save to disk
	if (snap.Number+x.config.Gap)%x.config.Epoch == 0 {
		if err = snap.store(x.db); err != nil {
			return nil, err
		}
		log.Trace("Stored voting snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}

// sigHash computes the hash of a header for signing (excludes the 65-byte seal suffix).
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	extra := header.Extra
	if len(extra) >= engines.ExtraSeal {
		extra = extra[:len(extra)-engines.ExtraSeal]
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
		extra,
		header.MixDigest,
		header.Nonce,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	// XDPoS fields: Validators and Penalties must be included for hash
	// compatibility with v2.6.8 V1/V2 transition blocks (#237)
	enc = append(enc, header.Validators)
	enc = append(enc, header.Penalties)
	if err := rlp.Encode(hasher, enc); err != nil {
		panic("can't encode: " + err.Error())
	}
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the signer address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.Cache[common.Hash, common.Address]) (common.Address, error) {
	hash := header.Hash()
	if address, ok := sigcache.Get(hash); ok {
		return address, nil
	}

	if len(header.Extra) < engines.ExtraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-engines.ExtraSeal:]

	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// newSnapshot creates a new snapshot with the specified startup parameters.
func newSnapshot(config *params.XDPoSConfig, sigcache *lru.Cache[common.Hash, common.Address], number uint64, hash common.Hash, signers []common.Address) *SnapshotV1 {
	snap := &SnapshotV1{
		config:   config,
		sigcache: sigcache,
		Number:   number,
		Hash:     hash,
		Signers:  make(map[common.Address]struct{}),
		Recents:  make(map[uint64]common.Address),
		Tally:    make(map[common.Address]clique.Tally),
	}
	for _, signer := range signers {
		snap.Signers[signer] = struct{}{}
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.XDPoSConfig, sigcache *lru.Cache[common.Hash, common.Address], db ethdb.Database, hash common.Hash) (*SnapshotV1, error) {
	blob, err := db.Get(append([]byte("XDPoS-"), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(SnapshotV1)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config
	snap.sigcache = sigcache
	return snap, nil
}

// store inserts the snapshot into the database.
func (s *SnapshotV1) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("XDPoS-"), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot.
func (s *SnapshotV1) copy() *SnapshotV1 {
	cpy := &SnapshotV1{
		config:   s.config,
		sigcache: s.sigcache,
		Number:   s.Number,
		Hash:     s.Hash,
		Signers:  make(map[common.Address]struct{}),
		Recents:  make(map[uint64]common.Address),
		Votes:    make([]*clique.Vote, len(s.Votes)),
		Tally:    make(map[common.Address]clique.Tally),
	}
	for signer := range s.Signers {
		cpy.Signers[signer] = struct{}{}
	}
	for block, signer := range s.Recents {
		cpy.Recents[block] = signer
	}
	for address, tally := range s.Tally {
		cpy.Tally[address] = tally
	}
	copy(cpy.Votes, s.Votes)
	return cpy
}

// validVote returns whether it makes sense to cast the specified vote.
func (s *SnapshotV1) validVote(address common.Address, authorize bool) bool {
	_, signer := s.Signers[address]
	return (signer && !authorize) || (!signer && authorize)
}

// cast adds a new vote into the tally.
func (s *SnapshotV1) cast(address common.Address, authorize bool) bool {
	if !s.validVote(address, authorize) {
		return false
	}
	if old, ok := s.Tally[address]; ok {
		old.Votes++
		s.Tally[address] = old
	} else {
		s.Tally[address] = clique.Tally{Authorize: authorize, Votes: 1}
	}
	return true
}

// uncast removes a previously cast vote from the tally.
func (s *SnapshotV1) uncast(address common.Address, authorize bool) bool {
	tally, ok := s.Tally[address]
	if !ok {
		return false
	}
	if tally.Authorize != authorize {
		return false
	}
	if tally.Votes > 1 {
		tally.Votes--
		s.Tally[address] = tally
	} else {
		delete(s.Tally, address)
	}
	return true
}

// apply creates a new authorization snapshot by applying the given headers.
func (s *SnapshotV1) apply(headers []*types.Header) (*SnapshotV1, error) {
	if len(headers) == 0 {
		return s, nil
	}
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}

	snap := s.copy()

	for _, header := range headers {
		number := header.Number.Uint64()
		if number%s.config.Epoch == 0 {
			snap.Votes = nil
			snap.Tally = make(map[common.Address]clique.Tally)
		}
		if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
			delete(snap.Recents, number-limit)
		}
		signer, err := ecrecover(header, s.sigcache)
		if err != nil {
			return nil, err
		}
		snap.Recents[number] = signer

		for i, vote := range snap.Votes {
			if vote.Signer == signer && vote.Address == header.Coinbase {
				snap.uncast(vote.Address, vote.Authorize)
				snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
				break
			}
		}

		var authorize bool
		switch {
		case bytes.Equal(header.Nonce[:], nonceAuthVote):
			authorize = true
		case bytes.Equal(header.Nonce[:], nonceDropVote):
			authorize = false
		default:
			return nil, errInvalidVote
		}
		if snap.cast(header.Coinbase, authorize) {
			snap.Votes = append(snap.Votes, &clique.Vote{
				Signer:    signer,
				Block:     number,
				Address:   header.Coinbase,
				Authorize: authorize,
			})
		}

		if tally := snap.Tally[header.Coinbase]; tally.Votes > len(snap.Signers)/2 {
			if tally.Authorize {
				snap.Signers[header.Coinbase] = struct{}{}
			} else {
				delete(snap.Signers, header.Coinbase)
				if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
					delete(snap.Recents, number-limit)
				}
				for i := 0; i < len(snap.Votes); i++ {
					if snap.Votes[i].Signer == header.Coinbase {
						snap.uncast(snap.Votes[i].Address, snap.Votes[i].Authorize)
						snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
						i--
					}
				}
			}
			for i := 0; i < len(snap.Votes); i++ {
				if snap.Votes[i].Address == header.Coinbase {
					snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
					i--
				}
			}
			delete(snap.Tally, header.Coinbase)
		}
	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()
	return snap, nil
}

// GetSigners retrieves the list of authorized signers in ascending order.
func (s *SnapshotV1) GetSigners() []common.Address {
	signers := make([]common.Address, 0, len(s.Signers))
	for signer := range s.Signers {
		signers = append(signers, signer)
	}
	for i := 0; i < len(signers); i++ {
		for j := i + 1; j < len(signers); j++ {
			if bytes.Compare(signers[i][:], signers[j][:]) > 0 {
				signers[i], signers[j] = signers[j], signers[i]
			}
		}
	}
	return signers
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

func compareSignersLists(list1 []common.Address, list2 []common.Address) bool {
	if len(list1) != len(list2) {
		return false
	}
	if len(list1) == 0 && len(list2) == 0 {
		return true
	}
	l1 := make([]common.Address, len(list1))
	l2 := make([]common.Address, len(list2))
	copy(l1, list1)
	copy(l2, list2)
	sort.Slice(l1, func(i, j int) bool { return l1[i].Hex() < l1[j].Hex() })
	sort.Slice(l2, func(i, j int) bool { return l2[i].Hex() < l2[j].Hex() })
	for i := range l1 {
		if l1[i] != l2[i] {
			return false
		}
	}
	return true
}

func position(list []common.Address, x common.Address) int {
	for i, item := range list {
		if item == x {
			return i
		}
	}
	return -1
}


// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have when created at time given the parent block's time
// and difficulty.
func (x *XDPoS_v1) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	return x.calcDifficulty(chain, parent, x.signer)
}

// Prepare initializes the consensus fields of a block header according to the
// rules of a particular engine. The changes are executed inline.
func (x *XDPoS_v1) Prepare(chain consensus.ChainReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = x.calcDifficulty(chain, parent, x.signer)
	// Ensure the timestamp has the correct delay
	header.Time = parent.Time + x.config.Period
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}
	return nil
}

// Finalize runs any post-transaction state modifications (e.g. block rewards)
// and assembles the final block.
func (x *XDPoS_v1) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, parentState *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// No block rewards in XDPoS v1, just assemble the block
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs}), nil
}

// Seal generates a new sealing request for the given input block and pushes
// the result into the given channel.
func (x *XDPoS_v1) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	header := block.Header()
	// Sign the header
	sighash, err := x.signFn(x.signer, accounts.MimetypeClique, SigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	header.Validator = sighash
	return block.WithSeal(header), nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (x *XDPoS_v1) SealHash(header *types.Header) common.Hash {
	return SigHash(header)
}

// SigHash returns the hash which is used as input for the XDPoS v1 signing.
func SigHash(header *types.Header) (hash common.Hash) {
	hash = header.HashNoValidator()
	return hash
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
func (x *XDPoS_v1) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, fullVerifies []bool, abort <-chan struct{}, results chan<- error) {
	go func() {
		for i, header := range headers {
			var err error
			if fullVerifies != nil && i < len(fullVerifies) {
				err = x.VerifyHeader(chain, header, fullVerifies[i])
			} else {
				err = x.VerifyHeader(chain, header, true)
			}
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
}

// VerifySeal checks whether the crypto seal on a header is valid according to
// the consensus rules of the given engine.
func (x *XDPoS_v1) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return x.verifySeal(chain, header, nil, true)
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of a given engine. XDPoS does not allow uncles.
func (x *XDPoS_v1) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed in XDPoS")
	}
	return nil
}

// IsEpochSwitch checks if the given header is an epoch switch block.
func (x *XDPoS_v1) IsEpochSwitch(header *types.Header) (bool, uint64, error) {
	isEpoch := header.Number.Uint64()%x.config.Epoch == 0
	return isEpoch, header.Number.Uint64() / x.config.Epoch, nil
}

// RecoverSigner recovers the Ethereum address of the signer from a block header.
func (x *XDPoS_v1) RecoverSigner(header *types.Header) (common.Address, error) {
	return ecrecover(header, x.signatures)
}

// RecoverValidator recovers the Ethereum address of the validator from a block header.
func (x *XDPoS_v1) RecoverValidator(header *types.Header) (common.Address, error) {
	// Recover validator from header.Validator signature
	if len(header.Validator) == 0 {
		return common.Address{}, errors.New("missing validator signature")
	}
	pub, err := crypto.SigToPub(header.HashNoValidator().Bytes(), header.Validator)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

