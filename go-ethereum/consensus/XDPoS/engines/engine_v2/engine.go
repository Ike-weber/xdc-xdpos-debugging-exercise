// Copyright (c) 2024 XDC Network
// XDPoS 2.0 BFT Consensus Engine
// This implements the HotStuff-based BFT consensus for XDC Network

package engine_v2

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/countdown"
	"github.com/ethereum/go-ethereum/common/lru"
	xdc_sort "github.com/ethereum/go-ethereum/common/sort"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/contracts"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"golang.org/x/crypto/sha3"
	"golang.org/x/sync/singleflight"
)

const (
	// Cache sizes
	InMemorySnapshots    = 128
	InMemorySignatures   = 4096
	InMemoryEpochs       = 10
	InMemoryRound2Epochs = 100

	// Pool hygiene
	PoolHygieneRound = 10

	// Periodic job interval
	PeriodicJobPeriod = 60 // seconds
)

// SignerFn is a signer callback function
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// XDPoS_v2 is the XDPoS 2.0 BFT consensus engine
type XDPoS_v2 struct {
	chainConfig   *params.ChainConfig
	config        *params.XDPoSConfig
	db            ethdb.Database
	isInitialized bool
	whosTurn      common.Address

	// Caches
	snapshots            *lru.Cache[common.Hash, *SnapshotV2]
	signatures           *lru.Cache[common.Hash, common.Address]
	epochSwitches        *lru.Cache[common.Hash, *types.EpochSwitchInfo]
	verifiedHeaders      *lru.Cache[common.Hash, struct{}]
	round2epochBlockInfo *lru.Cache[types.Round, *types.BlockInfo]

	// Signing
	signer   common.Address
	signFn   SignerFn
	lock     sync.RWMutex
	signLock sync.RWMutex

	// Channels
	BroadcastCh  chan interface{}
	minePeriodCh chan int
	newRoundCh   chan types.Round

	// Timeout handling
	timeoutWorker *countdown.ExpCountDown
	timeoutCount  int

	// Pools
	timeoutPool *utils.Pool
	votePool    *utils.Pool

	// Round state
	currentRound          types.Round
	highestSelfMinedRound types.Round
	highestVotedRound     types.Round
	highestQuorumCert     *types.QuorumCert
	lockQuorumCert        *types.QuorumCert
	highestTimeoutCert    *types.TimeoutCert
	highestCommitBlock    *types.BlockInfo

	// Hooks
	HookReward  func(chain consensus.ChainReader, state *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error)
	HookPenalty func(chain consensus.ChainReader, number *big.Int, parentHash common.Hash, candidates []common.Address) ([]common.Address, error)
	// HookCommitBlock is called whenever a block is committed (finalized) via the 3-chain rule.
	// Wire this to blockchain.SetFinalized() so eth_getFinalizedBlock reflects real BFT finality. (#95)
	HookCommitBlock func(header *types.Header)

	votePoolCollectionTime time.Time

	// Recursion guard for getEpochSwitchInfo to prevent stack overflow
	epochSwitchInProgress map[common.Hash]struct{}

	// C12 FIX: singleflight.Group deduplicates concurrent getEpochSwitchInfo calls
	// for the same hash, eliminating false-positive recursion errors under load.
	epochSwitchSF singleflight.Group
}

// SetHookReward wires the reward hook for V2 epoch-switch blocks.
func (x *XDPoS_v2) SetHookReward(fn func(chain consensus.ChainReader, state *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error)) {
	x.HookReward = fn
}

// SetHookPenalty wires the penalty hook used by calcMasternodes at epoch-switch
// blocks to remove under-signing masternodes from the active set.
func (x *XDPoS_v2) SetHookPenalty(fn func(chain consensus.ChainReader, number *big.Int, parentHash common.Hash, candidates []common.Address) ([]common.Address, error)) {
	x.HookPenalty = fn
}

// Singleton instance and once guard for XDPoS_v2 to prevent OOM at V2 switch block
var (
	engineInstance *XDPoS_v2
	engineOnce     sync.Once
)

// New creates a new XDPoS 2.0 engine (singleton pattern to prevent memory leaks)
func New(chainConfig *params.ChainConfig, db ethdb.Database, minePeriodCh chan int, newRoundCh chan types.Round) *XDPoS_v2 {
	engineOnce.Do(func() {
		engineInstance = createEngine(chainConfig, db, minePeriodCh, newRoundCh)
	})
	return engineInstance
}

// createEngine is the actual engine creation logic (extracted from New)
func createEngine(chainConfig *params.ChainConfig, db ethdb.Database, minePeriodCh chan int, newRoundCh chan types.Round) *XDPoS_v2 {
	config := chainConfig.XDPoS

	// Get timeout config from V2 config
	timeoutPeriod := 10 // default
	expBase := 2.0
	maxExponent := 6

	if config.V2 != nil && config.V2.CurrentConfig != nil {
		timeoutPeriod = config.V2.CurrentConfig.TimeoutPeriod
		if config.V2.CurrentConfig.ExpTimeoutConfig.Base > 0 {
			expBase = config.V2.CurrentConfig.ExpTimeoutConfig.Base
		}
		if config.V2.CurrentConfig.ExpTimeoutConfig.MaxExponent > 0 {
			maxExponent = int(config.V2.CurrentConfig.ExpTimeoutConfig.MaxExponent)
		}
	}

	duration := time.Duration(timeoutPeriod) * time.Second
	timeoutTimer, err := countdown.NewExpCountDown(duration, expBase, maxExponent)
	if err != nil {
		log.Crit("create exp countdown", "err", err)
	}

	engine := &XDPoS_v2{
		chainConfig:   chainConfig,
		config:        config,
		db:            db,
		isInitialized: false,

		signatures:           lru.NewCache[common.Hash, common.Address](InMemorySignatures),
		verifiedHeaders:      lru.NewCache[common.Hash, struct{}](InMemorySnapshots),
		snapshots:            lru.NewCache[common.Hash, *SnapshotV2](InMemorySnapshots),
		epochSwitches:        lru.NewCache[common.Hash, *types.EpochSwitchInfo](InMemoryEpochs),
		round2epochBlockInfo: lru.NewCache[types.Round, *types.BlockInfo](InMemoryRound2Epochs),

		timeoutWorker: timeoutTimer,
		BroadcastCh:   make(chan interface{}),
		minePeriodCh:  minePeriodCh,
		newRoundCh:    newRoundCh,

		timeoutPool: utils.NewPool(),
		votePool:    utils.NewPool(),

		highestSelfMinedRound: types.Round(0),

		// Initialize recursion guard map
		epochSwitchInProgress: make(map[common.Hash]struct{}),
		highestTimeoutCert: &types.TimeoutCert{
			Round:      types.Round(0),
			Signatures: []types.Signature{},
		},
		highestQuorumCert: &types.QuorumCert{
			ProposedBlockInfo: &types.BlockInfo{
				Hash:   common.Hash{},
				Round:  types.Round(0),
				Number: big.NewInt(0),
			},
			Signatures: []types.Signature{},
			GapNumber:  0,
		},
		highestVotedRound:  types.Round(0),
		highestCommitBlock: nil,
	}

	// Set timeout callback
	timeoutTimer.OnTimeoutFn = engine.OnCountdownTimeout

	// Start periodic job
	engine.periodicJob()

	// Build config index for per-round config dispatch (#117)
	config.V2.BuildConfigIndex()

	return engine
}

// UpdateParams updates V2 consensus parameters for the given round.
// Dispatches config based on round, updates timeout timer and mine period.
// Matches v2.6.8 engine_v2/engine.go UpdateParams. (#117)
func (x *XDPoS_v2) UpdateParams(header *types.Header) {
	_, round, err := x.getExtraFieldsNoChain(header)
	if err != nil {
		log.Error("[UpdateParams] retrieve round failed", "block", header.Number.Uint64(), "err", err)
		return
	}
	x.config.V2.UpdateConfig(uint64(round))

	// Setup timeoutTimer with new config
	duration := time.Duration(x.config.V2.CurrentConfig.TimeoutPeriod) * time.Second
	err = x.timeoutWorker.SetParams(duration, x.config.V2.CurrentConfig.ExpTimeoutConfig.Base, int(x.config.V2.CurrentConfig.ExpTimeoutConfig.MaxExponent))
	if err != nil {
		log.Error("[UpdateParams] set params failed", "err", err)
	}
	// A.97-M.A Task 1 nil-guard: minePeriodCh is nil when engine is constructed
	// without the M2 channel allocation (e.g. tests, non-mining nodes pre-PR-A).
	// Without this guard the goroutine blocks forever on a nil channel — a goroutine
	// leak under round churn. With real channels wired (eth/backend.go), the
	// buffered send succeeds immediately when the worker drains.
	if x.minePeriodCh != nil {
		go func() {
			x.minePeriodCh <- x.config.V2.CurrentConfig.MinePeriod
		}()
	}
}

// sigHash returns the hash which is used for signing.
// Aligned with XDPoSChain v2.6.8: uses header.Extra directly (no seal stripping)
// and includes Validators/Penalties inline to ensure identical RLP encoding.
func sigHash(header *types.Header) common.Hash {
	hasher := sha3.NewLegacyKeccak256()

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
		header.Extra,
		header.MixDigest,
		header.Nonce,
		header.Validators,
		header.Penalties,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	if err := rlp.Encode(hasher, enc); err != nil {
		panic("rlp.Encode fail: " + err.Error())
	}
	var hash common.Hash
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the signer address from a signed header.
// Aligned with XDPoSChain v2.6.8: signature is always in header.Validator for V2 blocks.
func ecrecover(header *types.Header, sigcache *lru.Cache[common.Hash, common.Address]) (common.Address, error) {
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address, nil
	}

	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), header.Validator)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// SignHash returns the hash for signing
func (x *XDPoS_v2) SignHash(header *types.Header) common.Hash {
	return sigHash(header)
}

// Initial initializes V2 engine state from the current chain state
func (x *XDPoS_v2) Initial(chain consensus.ChainReader, header *types.Header) error {
	x.lock.Lock()
	defer x.lock.Unlock()
	return x.initial(chain, header)
}

// ReinitBFT forces a full re-initialization of the BFT engine state from the
// given header. Call this when the chain head has advanced significantly beyond
// what the BFT engine has seen — e.g. after a bulk-sync catch-up completes and
// XdcBulkSyncMode is flipped to false. Without this, currentRound may lag behind
// the actual chain tip by many rounds, causing the node to time-out its way
// forward instead of immediately starting BFT at the correct round. A.99.4.
func (x *XDPoS_v2) ReinitBFT(chain consensus.ChainReader, header *types.Header) error {
	x.lock.Lock()
	defer x.lock.Unlock()
	log.Info("[ReinitBFT] re-initializing BFT engine from chain head",
		"number", header.Number.Uint64(), "hash", header.Hash())
	// Clear stale BFT state so initial() re-seeds everything from scratch.
	// ProposedBlockInfo must be initialized (not nil) because initial() checks
	// x.highestQuorumCert.ProposedBlockInfo.Hash — a nil pointer dereferences.
	x.isInitialized = false
	x.highestQuorumCert = &types.QuorumCert{
		ProposedBlockInfo: &types.BlockInfo{},
	}
	return x.initial(chain, header)
}

func (x *XDPoS_v2) initial(chain consensus.ChainReader, header *types.Header) error {
	log.Warn("[initial] initializing v2 related parameters")

	if x.highestQuorumCert.ProposedBlockInfo.Hash != (common.Hash{}) {
		log.Info("[initial] Already initialized", "hash", x.highestQuorumCert.ProposedBlockInfo.Hash)
		x.isInitialized = true
		return nil
	}

	var quorumCert *types.QuorumCert
	var err error

	if header.Number.Int64() == x.config.V2.SwitchBlock.Int64() {
		// First V2 block - create initial QC
		log.Info("[initial] highest QC for consensus v2 first block")
		blockInfo := &types.BlockInfo{
			Hash:   header.Hash(),
			Round:  types.Round(0),
			Number: header.Number,
		}
		gapNumber := header.Number.Uint64()
		if gapNumber > x.config.Gap {
			gapNumber -= x.config.Gap
		} else {
			gapNumber = 0
		}
		quorumCert = &types.QuorumCert{
			ProposedBlockInfo: blockInfo,
			Signatures:        nil,
			GapNumber:         gapNumber,
		}
		x.currentRound = 1
		x.highestQuorumCert = quorumCert
	} else {
		// Get QC from header
		log.Info("[initial] highest QC from current header")
		// V1 headers (including the V1->V2 switch block) have no V2 QC.
		// Fall through to the snapshot-init block below which seeds from SwitchBlock.
		if header.Number.Cmp(x.config.V2.SwitchBlock) <= 0 {
			log.Info("[initial] header is at/before V2 switch — skipping QC extraction",
				"number", header.Number.Uint64(), "v2Switch", x.config.V2.SwitchBlock.Uint64())
		} else {
			quorumCert, _, _, err = x.getExtraFields(chain, header)
			if err != nil {
				return err
			}
			// During bulk sync, the QC's proposed block may not be in DB yet.
			// Defer processQC until the block is actually committed.
			if chain.GetHeaderByHash(quorumCert.ProposedBlockInfo.Hash) != nil {
				err = x.processQC(chain, quorumCert)
				if err != nil {
					return err
				}
			} else {
				log.Debug("[initial] Deferring processQC, proposed block not in DB yet",
					"hash", quorumCert.ProposedBlockInfo.Hash,
					"number", quorumCert.ProposedBlockInfo.Number)
			}
		}
	}

	// Initialize first v2 snapshot
	lastGapNum := uint64(0)
	if x.config.V2.SwitchBlock.Uint64() > x.config.Gap {
		lastGapNum = x.config.V2.SwitchBlock.Uint64() - x.config.Gap
	}
	lastGapHeader := chain.GetHeaderByNumber(lastGapNum)

	// CHECKPOINT SYNC FIX: When syncing from a checkpoint after the V2 switch block,
	// the gap header (SwitchBlock - Gap) may not be in the DB yet. In this case,
	// we defer snapshot initialization until the gap block is processed.
	// The snapshot will be created when the gap block arrives via normal sync.
	if lastGapHeader == nil {
		currentHead := uint64(0)
		if header != nil {
			currentHead = header.Number.Uint64()
		}
		// If we're syncing from a checkpoint at or after V2 switch, the gap header
		// will arrive later. Defer snapshot init and mark as initialized
		// so we don't block sync progress.
		if currentHead >= x.config.V2.SwitchBlock.Uint64() {
			log.Warn("[initial] V2 gap header not in DB during checkpoint sync — deferring snapshot init",
				"lastGapNum", lastGapNum,
				"v2Switch", x.config.V2.SwitchBlock.Uint64(),
				"currentHead", currentHead)
			// Mark as initialized so sync can proceed
			// Snapshot will be created when gap block is processed
			x.isInitialized = true
			// A.99.3: preserve currentRound if processQC already set it from the
			// header's embedded QC (e.g. restarting with head=5220, QC sets round=2700).
			// Only default to 1 if currentRound is still at initial zero value.
			if x.currentRound == 0 {
				x.currentRound = 1 // Fix: currentRound must be set for correct leader selection
			}
			// Initialize timeout
			minePeriod := x.config.V2.CurrentConfig.MinePeriod
			log.Warn("[initial] miner wait period", "period", minePeriod)
			// A.97-M.A Task 1 nil-guard: see UpdateParams comment above.
			if x.minePeriodCh != nil {
				go func() {
					x.minePeriodCh <- minePeriod
				}()
			}
			// Start countdown timer
			x.timeoutWorker.Reset(chain, 0, 0)
			log.Warn("[initial] finish initialisation (deferred snapshot)")
			return nil
		}
		log.Error("[initial] V2 gap header missing from chain — snapshot cannot be anchored",
			"lastGapNum", lastGapNum, "v2Switch", x.config.V2.SwitchBlock.Uint64())
		return fmt.Errorf("[initial] V2 gap header %d not in chain DB (cold snapshot?)", lastGapNum)
	}

	snap, _ := loadSnapshot(x.db, lastGapHeader.Hash())
	if snap == nil {
		checkpointHeader := chain.GetHeaderByNumber(x.config.V2.SwitchBlock.Uint64())
		// Walk back through parent chain to find the switch block
		if header != nil && header.Number.Uint64() >= x.config.V2.SwitchBlock.Uint64() {
			for h := header; h != nil && h.Number.Uint64() >= x.config.V2.SwitchBlock.Uint64(); h = chain.GetHeader(h.ParentHash, h.Number.Uint64()-1) {
				if h.Number.Uint64() == x.config.V2.SwitchBlock.Uint64() {
					checkpointHeader = h
					break
				}
			}
		}

		if checkpointHeader == nil {
			// Still not found — this is expected during very early sync.
			// Return a typed error that allows the caller to retry later.
			currentHead := uint64(0)
			if header != nil {
				currentHead = header.Number.Uint64()
			}
			// DEVNET FIX: If we're past the V2 switch block but the checkpoint header is not
			// in DB yet (e.g., during checkpoint sync), defer initialization and mark as
			// initialized so sync can proceed. Snapshot will be created when gap block is processed.
			if currentHead >= x.config.V2.SwitchBlock.Uint64() {
				log.Warn("[initial] checkpoint header not found during sync past V2 switch — deferring snapshot init",
					"checkpoint", x.config.V2.SwitchBlock.Uint64(),
					"currentHead", currentHead)
				x.isInitialized = true
				// A.99.3: preserve currentRound if processQC already set it.
				if x.currentRound == 0 {
					x.currentRound = 1
				}
				minePeriod := x.config.V2.CurrentConfig.MinePeriod
				log.Warn("[initial] miner wait period", "period", minePeriod)
				// A.97-M.A Task 1 nil-guard: see UpdateParams comment above.
				if x.minePeriodCh != nil {
					go func() {
						x.minePeriodCh <- minePeriod
					}()
				}
				x.timeoutWorker.Reset(chain, 0, 0)
				log.Warn("[initial] finish initialisation (deferred snapshot)")
				return nil
			}
			log.Warn("[initial] checkpoint header not found, deferring V2 init until chain advances",
				"checkpoint", x.config.V2.SwitchBlock.Uint64(),
				"currentHead", currentHead)
			return fmt.Errorf("checkpoint header %d not found (sync in progress)", x.config.V2.SwitchBlock.Uint64())
		}
		log.Info("[initial] init first snapshot")

		// CRITICAL FIX: Use GetMasternodesFromEpochSwitchHeader instead of getExtraFields
		// to ensure we read from header.Validators (like v2.6.8) not header.Extra.
		masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, checkpointHeader)
		if len(masternodes) == 0 {
			// Fallback to Extra-based decoding only if Validators is empty
			masternodes = decodeMasternodesFromHeaderExtra(checkpointHeader)
		}
		if len(masternodes) == 0 {
			log.Error("[initial] masternodes are empty", "v2switch", x.config.V2.SwitchBlock.Uint64())
			return fmt.Errorf("masternodes are empty v2 switch number: %d", x.config.V2.SwitchBlock.Uint64())
		}

		snap = newSnapshot(lastGapNum, lastGapHeader.Hash(), masternodes)
		x.snapshots.Add(snap.Hash, snap)
		if err := storeSnapshot(snap, x.db); err != nil {
			log.Error("[initial] Error while store snapshot", "error", err)
			return err
		}
	}

	// Initialize timeout
	minePeriod := x.config.V2.CurrentConfig.MinePeriod
	log.Warn("[initial] miner wait period", "period", minePeriod)
	// A.97-M.A Task 1 nil-guard: see UpdateParams comment above.
	if x.minePeriodCh != nil {
		go func() {
			x.minePeriodCh <- minePeriod
		}()
	}

	// Start countdown timer
	x.timeoutWorker.Reset(chain, 0, 0)
	x.isInitialized = true

	log.Warn("[initial] finish initialisation")
	return nil
}

// Author returns the signer of a block
func (x *XDPoS_v2) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, x.signatures)
}

// VerifyHeader verifies a header for V2 consensus
func (x *XDPoS_v2) VerifyHeader(chain consensus.ChainReader, header *types.Header, fullVerify bool) error {
	err := x.verifyHeader(chain, header, nil, fullVerify)
	if err != nil {
		log.Debug("[VerifyHeader] Fail to verify header", "fullVerify", fullVerify, "blockNum", header.Number, "error", err)
	}
	return err
}

// VerifyHeaderWithParents verifies a header for V2 consensus using the provided parents slice.
// Required during bulk sync so that block N+1 can resolve parent N when N is not yet in the DB.
func (x *XDPoS_v2) VerifyHeaderWithParents(chain consensus.ChainReader, header *types.Header, parents []*types.Header, fullVerify bool) error {
	err := x.verifyHeader(chain, header, parents, fullVerify)
	if err != nil {
		log.Debug("[VerifyHeaderWithParents] Fail to verify header", "fullVerify", fullVerify, "blockNum", header.Number, "error", err)
	}
	return err
}

// VerifyHeaders verifies a batch of headers
func (x *XDPoS_v2) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, fullVerifies []bool, abort <-chan struct{}, results chan<- error) {
	go func() {
		for i, header := range headers {
			err := x.verifyHeader(chain, header, headers[:i], fullVerifies[i])
			if err != nil {
				log.Warn("[VerifyHeaders] Fail to verify header", "fullVerify", fullVerifies[i], "blockNum", header.Number, "error", err)
			}
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
}

// verifyHeader performs comprehensive header verification aligned with XDPoSChain v2.6.8.
// See verifyHeader.go for the full implementation.

// VerifyUncles always returns an error since XDPoS doesn't allow uncles
func (x *XDPoS_v2) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed in XDPoS_v2")
	}
	return nil
}

// Prepare prepares a header for mining
func (x *XDPoS_v2) Prepare(chain consensus.ChainReader, header *types.Header) error {
	x.lock.RLock()
	currentRound := x.currentRound
	highestQC := x.highestQuorumCert
	x.lock.RUnlock()

	// A.99: If highestQC is still at zero (engine bootstrapped but QC never seeded
	// because initial() ran on a pre-V2-switch header during bulk sync), attempt to
	// re-bootstrap from the parent header now that the parent is in the DB.
	// This happens when: node was mining+bulk-syncing, initial() ran early with a
	// pre-switch header (skipping QC extraction), isInitialized was set true, and
	// A.98 later cleared XdcBulkSyncMode — but highestQC remained at zero.
	if highestQC.ProposedBlockInfo.Hash == (common.Hash{}) {
		parentHeader := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
		if parentHeader != nil && parentHeader.Number.Cmp(x.config.V2.SwitchBlock) > 0 {
			log.Info("[Prepare] A.99: re-bootstrapping highestQC from parent header",
				"parent", parentHeader.Number.Uint64(), "parentHash", parentHeader.Hash())
			x.lock.Lock()
			x.isInitialized = false // allow initial() to re-run fully
			if err := x.initial(chain, parentHeader); err != nil {
				log.Warn("[Prepare] A.99: re-bootstrap failed, will retry",
					"err", err)
			}
			currentRound = x.currentRound
			highestQC = x.highestQuorumCert
			x.lock.Unlock()
		}
	}

	if header.ParentHash != highestQC.ProposedBlockInfo.Hash {
		log.Warn("[Prepare] parent hash and QC hash mismatch",
			"blockNum", header.Number,
			"QCNumber", highestQC.ProposedBlockInfo.Number,
			"blockHash", header.ParentHash,
			"QCHash", highestQC.ProposedBlockInfo.Hash)
		return utils.ErrNotReadyToPropose
	}

	// Set extra fields
	extra := types.ExtraFields_v2{
		Round:      currentRound,
		QuorumCert: highestQC,
	}
	extraBytes, err := extra.EncodeToBytes()
	if err != nil {
		return err
	}
	header.Extra = extraBytes
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	log.Info("Preparing new block!", "Number", number, "Parent Hash", parent.Hash())

	x.signLock.RLock()
	signer := x.signer
	x.signLock.RUnlock()

	// Verify it's our turn
	isMyTurn, err := x.yourturn(chain, currentRound, parent, signer)
	if err != nil {
		log.Error("[Prepare] Error checking turn", "currentRound", currentRound, "error", err)
		return err
	}
	if !isMyTurn {
		return utils.ErrNotReadyToMine
	}

	// Set difficulty
	header.Difficulty = x.calcDifficulty(chain, parent, signer)

	// Handle epoch switch
	isEpochSwitch, _, err := x.IsEpochSwitch(header)
	if err != nil {
		log.Error("[Prepare] Error checking epoch switch", "error", err)
		return err
	}
	if isEpochSwitch {
		masterNodes, penalties, _, err := x.calcMasternodes(chain, header.Number, header.ParentHash, currentRound, nil)
		if err != nil {
			return err
		}
		for _, v := range masterNodes {
			header.Validators = append(header.Validators, v[:]...)
		}
		for _, v := range penalties {
			header.Penalties = append(header.Penalties, v[:]...)
		}
	}

	header.MixDigest = common.Hash{}

	// Set timestamp using the PER-ROUND V2 MinePeriod, not the static V1 Period.
	// verifyHeader.go:91 verifies against x.config.V2.Config(round).MinePeriod, so the
	// stamp must use the identical source or a peer whose effective MinePeriod for this
	// round differs (e.g. legacy devnet baked the UnitTestV2Configs: minePeriod 3 for
	// rounds 10..899) rejects the block with ErrInvalidTimestamp. With a single-bucket
	// config this equals parent.Time+Period (behaviour-neutral); with a per-round
	// schedule it stays in lockstep with verify and with legacy peers. Refs #951 / devnet
	// MinePeriod parity.
	header.Time = parent.Time + uint64(x.config.V2.Config(uint64(currentRound)).MinePeriod)
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}

	if header.Coinbase != signer {
		log.Error("[Prepare] Coinbase mismatch", "headerCoinbase", header.Coinbase, "signer", signer)
		return errors.New("coinbase mismatch with signer")
	}

	return nil
}

// Finalize finalizes a block
func (x *XDPoS_v2) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, parentState *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	isEpochSwitch, _, err := x.IsEpochSwitch(header)
	if err != nil {
		log.Error("[Finalize] IsEpochSwitch bug!", "err", err)
		return nil, err
	}

	if x.HookReward != nil && isEpochSwitch {
		log.Info("[Finalize] V2 HookReward firing", "block", header.Number.Uint64(), "hash", header.Hash().Hex())
		_, err := x.HookReward(chain, state, parentState, header)
		if err != nil {
			return nil, err
		}
	} else if isEpochSwitch {
		log.Warn("[Finalize] V2 HookReward is nil at epoch switch!", "block", header.Number.Uint64())
	}

	parentHeader := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parentHeader == nil {
		log.Warn("[DIAG] Finalize ErrUnknownAncestor", "number", header.Number, "parentHash", header.ParentHash.Hex())
		return nil, consensus.ErrUnknownAncestor
	}

	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)

	return types.NewBlock(header, &types.Body{Transactions: txs}, receipts, trie.NewStackTrie(nil)), nil
}

// Seal seals a block
func (x *XDPoS_v2) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	header := block.Header()

	number := header.Number.Uint64()
	if number == 0 {
		return nil, utils.ErrUnknownBlock
	}

	x.signLock.RLock()
	signer, signFn := x.signer, x.signFn
	x.signLock.RUnlock()

	select {
	case <-stop:
		return nil, nil
	default:
	}

	// Sign the block
	signature, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	header.Validator = signature

	// Track highest self-mined round
	var decodedExtra types.ExtraFields_v2
	if err := DecodeExtraFields(header.Extra, &decodedExtra); err != nil {
		log.Error("[Seal] Error decoding extra field", "error", err)
		return nil, err
	}
	x.highestSelfMinedRound = decodedExtra.Round

	return block.WithSeal(header), nil
}

// Authorize sets the signer
func (x *XDPoS_v2) Authorize(signer common.Address, signFn SignerFn) {
	x.signLock.Lock()
	defer x.signLock.Unlock()
	x.signer = signer
	x.signFn = signFn
}

// CalcDifficulty returns the block difficulty
func (x *XDPoS_v2) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	return x.calcDifficulty(chain, parent, x.signer)
}

func (x *XDPoS_v2) calcDifficulty(chain consensus.ChainReader, parent *types.Header, signer common.Address) *big.Int {
	// V2 uses a simple difficulty of 1
	return big.NewInt(1)
}

// YourTurn checks if it's the signer's turn
func (x *XDPoS_v2) YourTurn(chain consensus.ChainReader, parent *types.Header, signer common.Address) (bool, error) {
	x.lock.RLock()
	defer x.lock.RUnlock()

	if !x.isInitialized {
		if err := x.initial(chain, parent); err != nil {
			log.Error("[YourTurn] Error initializing", "error", err)
			return false, err
		}
	}

	// Check if enough time has passed (use per-round config, fix #117)
	waitedTime := time.Now().Unix() - int64(parent.Time)
	minePeriod := x.config.V2.Config(uint64(x.currentRound)).MinePeriod
	if waitedTime < int64(minePeriod) {
		return false, nil
	}

	round := x.currentRound
	return x.yourturn(chain, round, parent, signer)
}

func (x *XDPoS_v2) yourturn(chain consensus.ChainReader, round types.Round, parent *types.Header, signer common.Address) (bool, error) {
	return x.yourturnAligned(chain, round, parent, signer)
}

// GetSnapshot returns the snapshot for a header
func (x *XDPoS_v2) GetSnapshot(chain consensus.ChainReader, header *types.Header) (*SnapshotV2, error) {
	return x.getSnapshot(chain, header.Number.Uint64(), false, nil)
}

// getSnapshot retrieves or creates a snapshot.
// `parents` is an optional ordered slice (oldest→newest) used during bulk-sync
// header verification when ancestors are not yet committed to the DB. It enables
// the seed-from-epoch-switch fallback below to find an in-batch epoch switch
// header. Pass nil when no parents context is available (mining, public API,
// post-import calls): the fallback then walks the canonical chain via the DB only.
func (x *XDPoS_v2) getSnapshot(chain consensus.ChainReader, number uint64, forSigning bool, parents []*types.Header) (*SnapshotV2, error) {
	// Try cache first
	var gapNumber uint64
	if forSigning || number%x.config.Epoch == x.config.Epoch-x.config.Gap {
		// Caller passed a gap block number directly (timeout/vote messages,
		// or calcMasternodes called from repairSnapshot/UpdateMasternodesFromHeader).
		// v2.6.8 equivalent: isGapNumber == true.
		gapNumber = number
	} else {
		gapNumber = number - number%x.config.Epoch
		if gapNumber > x.config.Gap {
			gapNumber -= x.config.Gap
		} else {
			gapNumber = 0
		}
	}
	checkpointNumber := number - number%x.config.Epoch
	if checkpointNumber == 0 {
		checkpointNumber = x.config.Epoch
	}

	gapHeader := chain.GetHeaderByNumber(gapNumber)
	if gapHeader == nil {
		// CHECKPOINT SYNC FIX: When syncing from a checkpoint at or after the V2 switch block,
		// the gap block (switchBlock - Gap) may not be in the DB. In this case, use the V2
		// switch block header directly to create the first V2 snapshot.
		if checkpointNumber == x.config.V2.SwitchBlock.Uint64() {
			switchHeader := chain.GetHeaderByNumber(x.config.V2.SwitchBlock.Uint64())
			if switchHeader != nil {
				log.Warn("[getSnapshot] gap header not in DB during checkpoint sync — using V2 switch block for first snapshot",
					"gap", gapNumber, "checkpoint", checkpointNumber, "switchBlock", x.config.V2.SwitchBlock.Uint64())
				masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, switchHeader)
				if len(masternodes) == 0 {
					masternodes = decodeMasternodesFromHeaderExtra(switchHeader)
				}
				if len(masternodes) > 0 {
					snap := newSnapshot(gapNumber, switchHeader.Hash(), masternodes)
					x.snapshots.Add(snap.Hash, snap)
					if err := storeSnapshot(snap, x.db); err != nil {
						log.Error("[getSnapshot] Error while store snapshot", "error", err)
					}
					return snap, nil
				}
			}
		}
		return nil, fmt.Errorf("no header at gap number %d", gapNumber)
	}

	// FIX (live-mn-set): For the BFT signing path (forSigning=true: vote/TC
	// membership checks), the gap snapshot MUST reflect the validator contract at
	// the gap block, not a stale "backward seed" from an old binary. Pre-fix-#1330
	// binaries persisted backward seeds derived from the previous epoch's switch-
	// header Validators (the active set only), which excluded newly-proposed
	// candidates. Those stale entries carry version≥4 and pass the trust shortcut
	// below, causing a freshly-seated masternode to fail its own vote self-check
	// (observed devnet-5151 epoch 27: stale 6-node snapshot at gap 22050; contract
	// had 8 nodes including geth 0xa119...).
	//
	// Version-6 in the in-memory cache means this snapshot was already rebuilt from
	// contract state in this session → skip the rebuild to avoid a redundant trie
	// open on every forSigning call. After a restart the first forSigning call pays
	// one rebuild; thereafter the version-6 entry persists in cache for the session.
	// Guarded past firstV2Checkpoint: the first V2 epoch's gap block was handled by
	// V1 and has no accessible V2 contract state.
	firstV2CheckpointForSigning := x.config.V2.SwitchBlock.Uint64() +
		(x.config.Epoch - x.config.V2.SwitchBlock.Uint64()%x.config.Epoch)
	if forSigning && checkpointNumber > firstV2CheckpointForSigning {
		if cached, cacheOk := x.snapshots.Get(gapHeader.Hash()); !cacheOk || cached.Version < 6 {
			if rebuilt := x.rebuildGapSnapshotFromState(chain, gapNumber, gapHeader); rebuilt != nil {
				return rebuilt, nil
			}
			// State not available (fast-sync, pruned, or trie-journal corruption):
			// fall back to a forward seed from the NEXT epoch switch header. The
			// forward seed is persisted to DB/cache with version=6 so this path
			// runs only once per session (subsequent calls find version=6 in cache
			// and skip the outer condition). This overwrites any stale backward-seed
			// snapshot that was poisoning the slot (observed devnet-5151 epoch 27:
			// stale 6-node snapshot at gap 22050 from pre-fix-#1330 binary excluded
			// geth from its own vote).
			// Start the forward scan from gapNumber+Gap-1 so that any intermediate
			// epoch switches between gapNumber and the checkpoint that THIS gap
			// defines are skipped. Example: for gapNumber=22050 there may be an
			// intermediate epoch switch at block 22097 (due to rounds skipping
			// multiple epoch boundaries); scanning from gapNumber=22050 wrongly
			// returns 22097 (6 validators, wrong epoch), while scanning from
			// gapNumber+Gap-1=22499 correctly finds the epoch-27 switch at block
			// 22942 (7 validators including the newly-seated masternode).
			forwardFrom := gapNumber + x.config.Gap - 1
			if epochSwitchHdr := x.findEpochSwitchAfter(chain, forwardFrom, parents); epochSwitchHdr != nil &&
				epochSwitchHdr.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() {
				masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, epochSwitchHdr)
				penalties := contracts.ExtractAddressFromBytes(epochSwitchHdr.Penalties)
				if len(masternodes) > 0 {
					snap := newSnapshotWithPenalties(gapNumber, gapHeader.Hash(), masternodes, penalties)
					snap.Version = 6 // forward-seed certified; see rebuildGapSnapshotFromState
					x.snapshots.Add(snap.Hash, snap)
					if serr := storeSnapshot(snap, x.db); serr != nil {
						log.Error("[getSnapshot] live-mn-set: failed to persist forward-seed snapshot",
							"gap", gapNumber, "err", serr)
					}
					log.Warn("[getSnapshot] live-mn-set: no contract state at gap, forward-seeded from epoch switch header",
						"gap", gapNumber, "epochSwitch", epochSwitchHdr.Number.Uint64(),
						"validators", len(masternodes), "penalties", len(penalties))
					return snap, nil
				}
			}
			// Both state rebuild and forward seed failed; fall through to stale
			// cache/DB snapshot. The stale snapshot is benign for non-signing
			// callers; signing callers will see a spurious rejection until the
			// epoch switch header is available in the DB.
			log.Warn("[getSnapshot] live-mn-set: state rebuild and forward seed both failed, using stale snapshot",
				"gap", gapNumber, "checkpoint", checkpointNumber)
		}
	}

	// Check cache — validate even cached snapshots to prevent stale V1-format data
	// from being reused after a restart or repair.
	// FIX #385: Accept v2.6.8 snapshots (version 0/1/2/3). v2.6.8 does not set a Version
	// field, so it unmarshals as 0. The Version gate was added for GP5's v62 fix but
	// incorrectly rejects valid v2.6.8 snapshots. Only reject exact V1 candidate count (13)
	// which indicates truly corrupt V1-format data at V2 checkpoints.
	if snap, ok := x.snapshots.Get(gapHeader.Hash()); ok {
		if checkpointNumber == x.config.V2.SwitchBlock.Uint64() {
			log.Warn("[getSnapshot] evicting cached snapshot at V2 switch block, rebuilding", "gap", gapNumber, "hash", gapHeader.Hash(), "cachedCandidates", len(snap.NextEpochCandidates))
			x.snapshots.Remove(gapHeader.Hash())
		} else if checkpointNumber > x.config.V2.SwitchBlock.Uint64() && len(snap.NextEpochCandidates) == 13 {
			log.Warn("[getSnapshot] evicting cached snapshot with V1 candidate count (13) at V2 checkpoint", "checkpoint", checkpointNumber, "gap", gapNumber, "hash", gapHeader.Hash(), "version", snap.Version, "candidates", len(snap.NextEpochCandidates))
			x.snapshots.Remove(gapHeader.Hash())
		} else {
			return snap, nil
		}
		// Fall through to rebuild after eviction
	}

	// Try loading from DB
	// FIX #385: Accept v2.6.8 snapshots (version 0/1/2/3). v2.6.8 does not set a Version
	// field, so it unmarshals as 0. The Version gate was added for GP5's v62 fix but
	// incorrectly rejects valid v2.6.8 snapshots. Only reject exact V1 candidate count (13)
	// which indicates truly corrupt V1-format data at V2 checkpoints.
	if snap, err := loadSnapshot(x.db, gapHeader.Hash()); err == nil && snap != nil {
		if checkpointNumber == x.config.V2.SwitchBlock.Uint64() {
			// Safety: the V1->V2 transition snapshot stored in the DB may have been built
			// from the old/wrong V1 Validators field (giving only 2 masternodes). Always
			// rebuild it from the epoch switch header to ensure correctness.
			log.Warn("[getSnapshot] ignoring cached DB snapshot at V2 switch block, rebuilding", "gap", gapNumber, "hash", gapHeader.Hash(), "cachedCandidates", len(snap.NextEpochCandidates))
		} else if snap.Version >= 4 {
			// Version 4+ snapshots were created by our code (newSnapshot sets Version=4).
			// They are trusted — no V1-format heuristic needed regardless of candidate count.
			// This handles devnet / chains with V2 from genesis where 3 masternodes is valid.
			x.snapshots.Add(snap.Hash, snap)
			return snap, nil
		} else if checkpointNumber > x.config.V2.SwitchBlock.Uint64() && len(snap.NextEpochCandidates) > 0 && len(snap.NextEpochCandidates) <= 20 {
			// V1-format snapshot at V2 checkpoint: V1 masternode counts were small (<=20).
			// Any such count at a V2 checkpoint with no version stamp indicates corrupt
			// V1-format data that was persisted during a bad sync. Rebuild from contract state.
			log.Warn("[getSnapshot] rejecting snapshot with V1 candidate count at V2 checkpoint", "checkpoint", checkpointNumber, "gap", gapNumber, "hash", gapHeader.Hash(), "version", snap.Version, "candidates", len(snap.NextEpochCandidates))
			// Fall through to rebuild
		} else {
			x.snapshots.Add(snap.Hash, snap)
			return snap, nil
		}
	}

	// V2 checkpoint past the V1→V2 switch block: the snapshot is normally created
	// during gap-block import by UpdateMasternodesFromHeader (which reads candidates
	// from the validator smart contract state). During checkpoint-sync-without-state
	// the gap block is imported without state, so UpdateMasternodesFromHeader cannot
	// run and the snapshot is never persisted.
	//
	// Issue #534/#536: rather than hard-erroring, seed the snapshot from the most
	// recent V2 epoch switch header at or before `number`. That header's Validators
	// (or Extra at the V1→V2 switch) is the canonical masternode list for the
	// epoch containing `number`. When read back through calcMasternodes,
	// HookPenalty returns addresses that — by construction — are not in this
	// already-filtered list, so removeItemFromArray is a no-op and the result
	// equals the canonical pipeline.
	//
	// IMPORTANT: this seed is only correct for regular (non-gap) block input. For
	// gap-block input (forSigning=true or number%Epoch == Epoch-Gap) the snapshot
	// must contain the NEXT epoch's masternodes, not the current epoch's, so we
	// continue to hard-error in that path rather than write incorrect data.
	//
	// IMPORTANT: walk back from `number`, not from `checkpointNumber`. The most
	// recent epoch switch can sit between `checkpointNumber` and `number` when
	// timeouts push the round-aligned switch past the block-aligned checkpoint.
	//
	// The previous repairSnapshot path mis-seeded from the V1 switch header's
	// V1-format candidate list (which is what PR #536 rejected);
	// GetMasternodesFromEpochSwitchHeader is V2/V1-switch aware and reads the
	// correct field for each header type.
	if checkpointNumber > x.config.V2.SwitchBlock.Uint64() {
		// STATE-FIRST rebuild (fix #1330): on nodes holding state at the gap
		// block, the only canonical snapshot content is the candidate pool
		// read from the validator contract at the gap block — exactly what
		// UpdateMasternodesFromHeader stores during import, and what legacy
		// peers check TC/vote GapNumber membership and next-epoch seating
		// against. Header-derived seeds below are approximations for
		// state-less fast-sync nodes and must never shadow real state.
		// Guarded past the first V2 checkpoint (whose gap block is V1-era),
		// mirroring repairSnapshot's boundary handling.
		firstV2Checkpoint := x.config.V2.SwitchBlock.Uint64() +
			(x.config.Epoch - x.config.V2.SwitchBlock.Uint64()%x.config.Epoch)
		if checkpointNumber > firstV2Checkpoint {
			if snap := x.rebuildGapSnapshotFromState(chain, gapNumber, gapHeader); snap != nil {
				return snap, nil
			}
		}
		isGapInput := forSigning || number%x.config.Epoch == x.config.Epoch-x.config.Gap
		// Seed-from-epoch-switch fallback. Two search directions because the
		// `number` argument has two different meanings here:
		//
		//   - !isGapInput: `number` is a regular block. We need the masternodes
		//     of `number`'s epoch. The opening epoch-switch header sits AT OR
		//     BEFORE `number`, so walk BACKWARD. Pass `parents` so bulk-sync
		//     VerifyHeaders can find in-batch ancestors not yet committed to
		//     the DB — without this, the walk would return the PREVIOUS
		//     round-based epoch's switch (Loophole M, PR #536), seeding the
		//     wrong masternodes.
		//
		//   - isGapInput (forSigning=true on timeout/vote cert, or `number`
		//     itself is a gap block): the masternodes that signed are those
		//     of the epoch STARTING AFTER `gapNumber`. The next epoch-switch
		//     header records them in its Validators field. Walk FORWARD from
		//     `gapNumber` to find it.
		//
		// In both directions the search must locate a V2-era switch (or stay
		// inside the first V2 epoch); seeding from the V1→V2 switch's
		// V1-format Extra past the first V2 epoch is the bug PR #536 rejected.
		// Regression-fix #588.X: the !isGapInput seed path was over-eager during
		// bulk-sync VerifyHeader (parents != nil). At V2-era epoch boundaries
		// where the masternode set grew/shrunk relative to the previously-
		// seeded epoch, our pool was incomplete:
		//
		//   seeded NextEpochCandidates = prev_epoch.Validators (e.g. 10 addrs)
		//   seeded Penalties           = prev_epoch.Penalties  (e.g.  3 addrs)
		//   candidates pool = 13
		//   HookPenalty returns 3 penalties → masternodes = 10
		//   header.Validators (new epoch) = 12 → CompareSignersLists fails →
		//   ErrValidatorsNotLegit, sync stuck.
		//
		// Pre-PR-#577 the seed skipped bulk-sync entirely, calcMasternodes
		// errored, verifyHeader.go's bulk-sync fallback ran
		// (GetMasternodesWithParents → header.Validators-as-authoritative)
		// and the chain advanced. Restore that behaviour for the bulk-sync
		// VerifyHeader path while keeping the seed available for the gap-input
		// callers (timeout/vote cert verify) where no fallback exists.
		var (
			epochSwitchHeader *types.Header
			seedDir           string
		)
		switch {
		case isGapInput:
			// Timeout/vote cert verify or gap-block number passed directly.
			// No verifyHeader fallback chain here — seeding is the only way
			// to materialise the snapshot.
			epochSwitchHeader = x.findEpochSwitchAfter(chain, gapNumber, parents)
			seedDir = "forward"
		case parents == nil:
			// !isGapInput, non-bulk-sync caller (mining, public API,
			// post-import). DB has ancestors at steady-state; walk back
			// is safe.
			epochSwitchHeader = x.findEpochSwitchAtOrBefore(chain, number, nil)
			seedDir = "backward"
		default:
			// !isGapInput && parents != nil: bulk-sync VerifyHeader.
			// Defer to verifyHeader's fallback chain — DO NOT seed here.
			// The fallback uses header.Validators which is the authoritative
			// next-epoch active set, and the QC-verify earlier in the same
			// verifyHeader call already proved chain extension from the
			// previous epoch's signers.
			return nil, fmt.Errorf("%w at gap %d (checkpoint %d): UpdateMasternodesFromHeader did not run during gap-block import (forSigning=%t, isGapInput=%t, parentsNil=%t, parentsLen=%d) — bulk-sync, deferred to verifyHeader fallback", utils.ErrV2SnapshotMissing, gapNumber, checkpointNumber, forSigning, isGapInput, parents == nil, len(parents))
		}
		if epochSwitchHeader != nil &&
			(epochSwitchHeader.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() ||
				number <= x.config.V2.SwitchBlock.Uint64()+x.config.Epoch) {
			// Read both the active masternode list (Validators) and the penalty set
			// (Penalties) from the epoch switch header. Keep them in DISTINCT
			// snapshot fields:
			//   - NextEpochCandidates ← Validators (active set; consumed by the
			//     synthetic-epoch-info path for proposer/leader rotation).
			//   - Penalties           ← header.Penalties (added to the comeback
			//     pool inside calcMasternodes via SnapshotV2.CandidatePool).
			// Putting Penalties inside NextEpochCandidates breaks the leader
			// calc (`leaderIndex = round % len(masterNodes)` over an inflated
			// list), seen as "not its turn" rejects on the first block past a
			// checkpoint anchor.
			masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, epochSwitchHeader)
			penalties := contracts.ExtractAddressFromBytes(epochSwitchHeader.Penalties)
			if len(masternodes) > 0 {
				snap := newSnapshotWithPenalties(gapNumber, gapHeader.Hash(), masternodes, penalties)
				// Fix #1330: NEVER persist (or cache) a backward seed at the gap
				// hash. A backward walk lands on the CURRENT epoch's switch header,
				// whose Validators is the active set seated from the PREVIOUS gap —
				// one full epoch stale for this gap's slot, and an active set rather
				// than the candidate pool. Persisting it poisons every gap-semantics
				// consumer (verifyTC / VerifyVoteMessage GapNumber membership,
				// next-epoch seating) until the DB is rebuilt: on net5151 this made
				// the node reject all round-64xx TCs (own address absent from the
				// stale 5-entry seed at gap 6750) and stall as quorum-critical voter.
				// Return the seed ephemerally — it is still the right answer for the
				// regular-block caller that asked (leader rotation of the CURRENT
				// epoch) — and let the next gap-semantics consumer materialise the
				// slot correctly (state-first rebuild above, or forward seed).
				if seedDir == "backward" {
					log.Warn("[getSnapshot] ephemeral V2 snapshot from epoch switch header (backward seed — not persisted, fix #1330)",
						"block", number, "gap", gapNumber, "checkpoint", checkpointNumber,
						"epochSwitch", epochSwitchHeader.Number.Uint64(),
						"validators", len(masternodes), "penalties", len(penalties))
					return snap, nil
				}
				x.snapshots.Add(snap.Hash, snap)
				if err := storeSnapshot(snap, x.db); err != nil {
					log.Error("[getSnapshot] failed to persist seeded V2 snapshot", "error", err)
				}
				log.Warn("[getSnapshot] seeded V2 snapshot from epoch switch header",
					"block", number, "gap", gapNumber, "checkpoint", checkpointNumber,
					"epochSwitch", epochSwitchHeader.Number.Uint64(),
					"validators", len(masternodes), "penalties", len(penalties),
					"dir", seedDir)
				return snap, nil
			}
		}
		return nil, fmt.Errorf("%w at gap %d (checkpoint %d): UpdateMasternodesFromHeader did not run during gap-block import (forSigning=%t, isGapInput=%t, parentsNil=%t, parentsLen=%d)", utils.ErrV2SnapshotMissing, gapNumber, checkpointNumber, forSigning, isGapInput, parents == nil, len(parents))
	}

	// V1→V2 switch checkpoint: bootstrap masternodes from the switch header.
	//
	// CRITICAL: at the V1→V2 switch block, header.Validators is V1's M2-index stream
	// (4-byte tokens via BuildValidatorFromM2; engine_v1/engine.go:769). When the
	// M2 count is a multiple of 5 (devnet's 100 masternodes → 400 bytes), the length
	// passes `len%AddressLength == 0` and the loop below silently misreads M2 indices
	// as 20-byte addresses (20 garbage entries). The address list at the switch block
	// lives in header.Extra (vanity 32 + N·20 + seal 65). Always read from Extra at
	// the switch block. Past the switch, header.Validators is the canonical V2 form.
	checkpointHeader := chain.GetHeaderByNumber(checkpointNumber)
	if checkpointHeader == nil {
		return nil, fmt.Errorf("no checkpoint header at %d", checkpointNumber)
	}

	var masternodes []common.Address
	isSwitchBlock := checkpointHeader.Number.Cmp(x.config.V2.SwitchBlock) == 0
	if !isSwitchBlock && len(checkpointHeader.Validators) > 0 && len(checkpointHeader.Validators)%common.AddressLength == 0 {
		masternodes = make([]common.Address, len(checkpointHeader.Validators)/common.AddressLength)
		for i := 0; i < len(masternodes); i++ {
			copy(masternodes[i][:], checkpointHeader.Validators[i*common.AddressLength:])
		}
		log.Info("[getSnapshot] V2 checkpoint: loaded masternodes from header.Validators",
			"checkpoint", checkpointNumber, "count", len(masternodes))
	} else {
		// Switch block (or empty Validators on a later V2 block) → read addresses from header.Extra.
		masternodes = x.GetMasternodesFromEpochSwitchHeader(chain, checkpointHeader)
		log.Info("[getSnapshot] V2 switch: loaded masternodes from header.Extra (V1 format)",
			"checkpoint", checkpointNumber, "count", len(masternodes), "isSwitchBlock", isSwitchBlock)
	}

	if len(masternodes) == 0 {
		return nil, fmt.Errorf("V2 switch block %d has no masternodes in Validators or Extra", checkpointNumber)
	}

	newSnap := newSnapshot(gapNumber, gapHeader.Hash(), masternodes)
	_ = storeSnapshot(newSnap, x.db)
	x.snapshots.Add(newSnap.Hash, newSnap)

	return newSnap, nil
}

// repairSnapshot recursively rebuilds the masternode snapshot for a V2 checkpoint
// by walking back to the last known good snapshot and replaying gap-block logic.
// rebuildGapSnapshotFromState reconstructs the gap-block snapshot from the
// masternode-voting contract state at the gap block — the canonical content,
// identical to what UpdateMasternodesFromHeader persists at import time:
// full candidate pool, zero-address filtered, stake-descending xdc_sort,
// penalties via HookPenalty (fix #1330).
//
// This is the authoritative source on any node that holds state at the gap
// block (full/archive sync). Header-derived seeding (below in getSnapshot) is
// an approximation for state-less fast-sync nodes only: an epoch-switch
// header's Validators is an ACTIVE SET — it excludes proposed-but-unseated
// candidates and penalised nodes — and the backward variant is additionally
// one full epoch stale (it records the set seated from the PREVIOUS gap).
// Observed on net5151: the backward seed persisted 5 validators (from switch
// 7199, seated from gap 5850) at gap 6750 whose true contract pool was 6; the
// node then failed signature-membership for every TC and vote committing to
// GapNumber 6750 — unable to follow rounds or vote, it stalled the network
// when it was quorum-critical.
//
// Returns nil when the gap header, its state, or contract candidates are
// unavailable; callers then fall back to header-derived seeding.
func (x *XDPoS_v2) rebuildGapSnapshotFromState(chain consensus.ChainReader, gapNumber uint64, gapHeader *types.Header) *SnapshotV2 {
	stateReader, ok := chain.(interface {
		StateAt(*types.Header) (*state.StateDB, error)
	})
	if !ok {
		return nil
	}
	statedb, err := stateReader.StateAt(gapHeader)
	if err != nil || statedb == nil {
		return nil
	}
	candidates := state.GetCandidates(statedb)
	if len(candidates) == 0 {
		return nil
	}
	// Stake-descending sort with the frozen Go-1.18 quicksort (xdc_sort) —
	// same tie-ordering as canonical XDPoSChain; see repairSnapshot comment.
	type candidateStake struct {
		addr  common.Address
		stake *big.Int
	}
	cs := make([]candidateStake, 0, len(candidates))
	for _, c := range candidates {
		if c != (common.Address{}) {
			cs = append(cs, candidateStake{addr: c, stake: state.GetCandidateCap(statedb, c)})
		}
	}
	xdc_sort.Slice(cs, func(i, j int) bool {
		return cs[i].stake.Cmp(cs[j].stake) >= 0
	})
	sorted := make([]common.Address, len(cs))
	for i, c := range cs {
		sorted[i] = c.addr
	}
	// Penalties at write time, mirroring UpdateMasternodesFromHeader (P0 #894).
	var penalties []common.Address
	if x.HookPenalty != nil {
		if pen, perr := x.HookPenalty(chain, gapHeader.Number, gapHeader.ParentHash, sorted); perr == nil {
			penalties = pen
		} else {
			log.Warn("[rebuildGapSnapshotFromState] HookPenalty failed, storing snapshot without penalties",
				"gap", gapNumber, "err", perr)
		}
	}
	snap := newSnapshotWithPenalties(gapNumber, gapHeader.Hash(), sorted, penalties)
	snap.Version = 6 // state-certified: authoritative for forSigning membership checks (live-mn-set fix)
	x.snapshots.Add(snap.Hash, snap)
	if serr := storeSnapshot(snap, x.db); serr != nil {
		log.Error("[rebuildGapSnapshotFromState] failed to store snapshot", "gap", gapNumber, "err", serr)
	}
	log.Info("[getSnapshot] rebuilt gap snapshot from contract state (fix #1330)",
		"gap", gapNumber, "pool", len(sorted), "penalties", len(penalties), "version", snap.Version)
	return snap
}

func (x *XDPoS_v2) repairSnapshot(chain consensus.ChainReader, checkpointNumber uint64) ([]common.Address, error) {
	switchBlock := x.config.V2.SwitchBlock.Uint64()

	// CRITICAL FIX: First V2 epoch checkpoint — gap block was processed by V1 engine
	// with a V1 snapshot. Cannot read smart contract state (pruned or V1 format).
	// Must read masternodes from checkpoint header directly.
	firstV2Checkpoint := switchBlock + (x.config.Epoch - switchBlock%x.config.Epoch)
	if checkpointNumber <= firstV2Checkpoint {
		header := chain.GetHeaderByNumber(checkpointNumber)
		if header == nil {
			return nil, fmt.Errorf("first V2 checkpoint header not found: %d", checkpointNumber)
		}
		masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, header)
		if len(masternodes) == 0 && len(header.Validators) > 0 && len(header.Validators)%common.AddressLength == 0 {
			// Fallback: read from Validators field (V2 format)
			masternodes = make([]common.Address, len(header.Validators)/common.AddressLength)
			for i := 0; i < len(masternodes); i++ {
				copy(masternodes[i][:], header.Validators[i*common.AddressLength:])
			}
		}
		log.Info("[repairSnapshot] first V2 epoch checkpoint",
			"checkpoint", checkpointNumber, "masternodes", len(masternodes))
		return masternodes, nil
	}

	// V2 CHECKPOINT FIX: At V2 checkpoints, read the FULL candidate list from
	// the smart contract state at the gap block, exactly like
	// UpdateMasternodesFromHeader does during normal sync.
	// This applies to ALL V2 checkpoints, not just the first one.
	gapNumber := checkpointNumber - x.config.Gap
	gapHeader := chain.GetHeaderByNumber(gapNumber)
	if gapHeader == nil {
		return nil, fmt.Errorf("gap header not found for V2 checkpoint: %d", gapNumber)
	}
	// Attempt to read candidates from smart contract state at gap block.
	// v1.17.3 signature: StateAt takes *types.Header (not common.Hash root).
	// Refs #740/#742: blockchain_reader.go conflict resolution adopts upstream API.
	if stateReader, ok := chain.(interface {
		StateAt(*types.Header) (*state.StateDB, error)
	}); ok {
		if statedb, err := stateReader.StateAt(gapHeader); err == nil {
			candidates := state.GetCandidates(statedb)
			if len(candidates) > 0 {
				// Sort by stake descending to match v2.6.8 / UpdateMasternodesFromHeader behavior
				type candidateStake struct {
					addr  common.Address
					stake *big.Int
				}
				cs := make([]candidateStake, 0, len(candidates))
				for _, c := range candidates {
					if c != (common.Address{}) {
						cap := state.GetCandidateCap(statedb, c)
						cs = append(cs, candidateStake{addr: c, stake: cap})
					}
				}
				// Use the frozen Go-1.18 quicksort that canonical XDPoSChain uses
				// (common/sort.Slice). Modern Go's stdlib sort.Slice is pdqsort, which
				// produces a different ordering of ties — for devnet's many-equal-stake
				// candidate pool, that ordering difference makes the top-N truncation
				// disagree with canonical and triggers "validators not legit" at every
				// V2 epoch switch.
				xdc_sort.Slice(cs, func(i, j int) bool {
					return cs[i].stake.Cmp(cs[j].stake) >= 0
				})
				sortedCandidates := make([]common.Address, len(cs))
				for i, c := range cs {
					sortedCandidates[i] = c.addr
				}
				// Persist and cache the repaired snapshot with the FULL candidate
				// pool (NextEpochCandidates) — exactly as UpdateMasternodesFromHeader
				// stores it. calcMasternodes reads this pool and derives the active set.
				snap := newSnapshot(gapNumber, gapHeader.Hash(), sortedCandidates)
				x.snapshots.Add(snap.Hash, snap)
				if err := storeSnapshot(snap, x.db); err != nil {
					log.Error("[repairSnapshot] failed to store V2-checkpoint snapshot", "gap", gapNumber, "err", err)
				}
				// PARITY FIX: return the ACTIVE masternode set, NOT the raw candidate
				// pool. Mirror calcMasternodes (engine.go ~1586-1594): remove penalties,
				// then cap at MaxMasternodes. Returning the uncapped, penalty-inclusive
				// pool diverges from canonical — it inflates the QC 2/3 threshold and
				// excludes legitimately-active proposers, wedging strict-fullVerify
				// (snapfull) nodes at non-epoch blocks with "validator not within
				// masternodes" / "verifyQC: not enough signatures" (observed mainnet
				// block 103,795,650: repairSnapshot returned 169 vs canonical 108).
				// Fast / bulk-sync nodes mask it via the !fullVerify tolerance, so only
				// complete-state nodes hit it.
				active := sortedCandidates
				maxMasternodes := x.config.V2.Config(0).MaxMasternodes
				if cph := chain.GetHeaderByNumber(checkpointNumber); cph != nil {
					if _, r, e := x.getExtraFieldsNoChain(cph); e == nil {
						if mm := x.config.V2.Config(uint64(r)).MaxMasternodes; mm > 0 {
							maxMasternodes = mm
						}
					}
					if x.HookPenalty != nil {
						if penalties, perr := x.HookPenalty(chain, cph.Number, cph.ParentHash, sortedCandidates); perr == nil {
							active = removeItemFromArray(sortedCandidates, penalties)
						} else {
							log.Warn("[repairSnapshot] HookPenalty failed; capping full pool without penalty removal",
								"checkpoint", checkpointNumber, "err", perr)
						}
					}
				}
				if maxMasternodes > 0 && len(active) > maxMasternodes {
					active = active[:maxMasternodes]
				}
				log.Info("[repairSnapshot] V2 checkpoint: derived active masternode set",
					"checkpoint", checkpointNumber, "gap", gapNumber,
					"pool", len(sortedCandidates), "active", len(active), "maxMasternodes", maxMasternodes)
				return active, nil
			}
			log.Warn("[repairSnapshot] V2 checkpoint: smart contract returned zero candidates",
				"checkpoint", checkpointNumber, "gap", gapNumber)
		} else {
			log.Warn("[repairSnapshot] V2 checkpoint: failed to open state at gap block",
				"checkpoint", checkpointNumber, "gap", gapNumber, "err", err)
		}
	}

	// CHECKPOINT SYNC FIX: State not available (checkpoint sync without state).
	// Read validators from the checkpoint header itself instead of the contract.
	// The checkpoint header's Validators field contains the correct masternode list.
	checkpointHeader := chain.GetHeaderByNumber(checkpointNumber)
	if checkpointHeader != nil {
		log.Info("[repairSnapshot] V2 checkpoint: checking checkpoint header",
			"checkpoint", checkpointNumber, "gap", gapNumber,
			"validatorsLen", len(checkpointHeader.Validators),
			"validatorLen", len(checkpointHeader.Validator),
			"penaltiesLen", len(checkpointHeader.Penalties),
			"extraLen", len(checkpointHeader.Extra))
		if len(checkpointHeader.Validators) > 0 && len(checkpointHeader.Validators)%common.AddressLength == 0 {
			masternodes := make([]common.Address, len(checkpointHeader.Validators)/common.AddressLength)
			for i := 0; i < len(masternodes); i++ {
				copy(masternodes[i][:], checkpointHeader.Validators[i*common.AddressLength:])
			}
			log.Info("[repairSnapshot] V2 checkpoint: loaded candidates from checkpoint header (checkpoint sync mode)",
				"checkpoint", checkpointNumber, "gap", gapNumber, "candidates", len(masternodes))
			snap := newSnapshot(gapNumber, gapHeader.Hash(), masternodes)
			x.snapshots.Add(snap.Hash, snap)
			if err := storeSnapshot(snap, x.db); err != nil {
				log.Error("[repairSnapshot] failed to store V2-checkpoint snapshot from header", "gap", gapNumber, "err", err)
			}
			return masternodes, nil
		}
	} else {
		log.Warn("[repairSnapshot] V2 checkpoint: checkpoint header not found", "checkpoint", checkpointNumber)
	}

	// State not available and no checkpoint header - return clear error
	return nil, fmt.Errorf("V2 checkpoint %d: cannot read validator contract state at gap %d (state pruned or unavailable) and no checkpoint header validators", checkpointNumber, gapNumber)
}

// GetMasternodesFromEpochSwitchHeader extracts masternodes from epoch switch header.
// For V1 and V2 epoch switch blocks, masternodes are stored in header.Validators.
// Only the V1->V2 switch block stores masternodes in header.Extra.
func (x *XDPoS_v2) GetMasternodesFromEpochSwitchHeader(chain consensus.ChainReader, header *types.Header) []common.Address {
	if header == nil {
		return []common.Address{}
	}

	// MINT-BLOCKER FIX (#62): route V1 pre-switch epoch checkpoints to the V1
	// reader instead of returning empty. A V1 epoch-checkpoint header (number %
	// Epoch == 0, below the V2 switch — e.g. block 900 on a switch=1800 net)
	// carries its seated masternode set as 20-byte addresses in header.Extra
	// (vanity32 + N*20 + seal65), NOT in header.Validators. The earlier C14 code
	// hazard was reading header.Validators — which for V1 headers holds 4-byte M2
	// index tokens (garbage if cast to addresses) — so it correctly refused
	// header.Validators, but then wrongly returned an EMPTY set. Empty → a modern
	// producer reads masternode count = 0 for its V1 epoch and never seals its
	// seated turn ("Masternodes not found"). Decoding from header.Extra (the V1
	// format) yields the real seated set so the node can compute its turn and MINT.
	// Non-checkpoint V1 headers have no address list in Extra (just vanity+seal) and
	// decode to an empty slice, preserving the previous behaviour for those.
	if x.config.V2 != nil && x.config.V2.SwitchBlock != nil &&
		header.Number.Cmp(x.config.V2.SwitchBlock) < 0 {
		const extraVanity, extraSeal = 32, 65
		if len(header.Extra) < extraVanity+extraSeal {
			// Malformed / non-checkpoint V1 header — no address list to read.
			return []common.Address{}
		}
		masternodes := decodeMasternodesFromHeaderExtra(header)
		log.Debug("[GetMasternodesFromEpochSwitchHeader] V1 pre-switch header routed to V1 Extra reader",
			"header", header.Number.Uint64(), "switchBlock", x.config.V2.SwitchBlock.Uint64(),
			"masternodes", len(masternodes))
		return masternodes
	}

	// At the V1→V2 switch block, V1's HookValidator (engine_v1/engine.go:769) populates
	// header.Validators with 4-byte M2 tokens via BuildValidatorFromM2 (M2ByteLength=4) —
	// NOT 20-byte addresses. When the M2 count is a multiple of 5 (e.g. devnet's 100
	// masternodes → 400 bytes), the length passes `len%AddressLength == 0` and the loop
	// below silently misreads the M2 stream as 20 garbage addresses. For the switch
	// block, always fall through to header.Extra. Past the switch block (true V2 epoch
	// switches), header.Validators IS the canonical 20-byte-packed address list.
	isSwitchBlock := x.config.V2 != nil && x.config.V2.SwitchBlock != nil &&
		header.Number.Cmp(x.config.V2.SwitchBlock) == 0
	if !isSwitchBlock && len(header.Validators) > 0 && len(header.Validators)%common.AddressLength == 0 {
		masternodes := make([]common.Address, len(header.Validators)/common.AddressLength)
		for i := 0; i < len(masternodes); i++ {
			copy(masternodes[i][:], header.Validators[i*common.AddressLength:])
		}
		return masternodes
	}

	// Fallback to V1-format Extra ONLY if header.Validators is empty.
	// This should only happen for true V1 epoch switch blocks.
	if header.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
		masternodes := decodeMasternodesFromHeaderExtra(header)
		log.Warn("[GetMasternodesFromEpochSwitchHeader] switch block: fell back to header.Extra (V1 format)",
			"number", header.Number.Uint64(), "count", len(masternodes),
			"extraLen", len(header.Extra))
		return masternodes
	}

	return []common.Address{}
}

// GetMasternodes returns masternodes for a header
func (x *XDPoS_v2) GetMasternodes(chain consensus.ChainReader, header *types.Header) []common.Address {
	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, header, header.Hash())
	if err != nil {
		log.Error("[GetMasternodes] Error getting epoch switch info", "err", err)
		return []common.Address{}
	}
	return epochSwitchInfo.Masternodes
}

// GetMasternodesV2 satisfies EngineV2Iface and returns the active masternode set
// for any V2-era header via getEpochSwitchInfo (round-based epoch resolution).
// Called by the XDPoS wrapper's GetMasternodes dispatch for V2 blocks.
func (x *XDPoS_v2) GetMasternodesV2(chain consensus.ChainReader, header *types.Header) []common.Address {
	return x.GetMasternodes(chain, header)
}

// GetMasternodesWithParents returns masternodes for a header using the provided parents slice.
// This is required during bulk sync when parent headers are not yet committed to the DB.
func (x *XDPoS_v2) GetMasternodesWithParents(chain consensus.ChainReader, header *types.Header, parents []*types.Header) []common.Address {
	// Try the DB path first (fast path for already-synced blocks)
	epochSwitchInfo, err := x.getEpochSwitchInfo(chain, header, header.Hash())
	if err == nil {
		return epochSwitchInfo.Masternodes
	}
	// Fallback: use parents slice to resolve epoch switch during bulk sync
	log.Debug("[GetMasternodesWithParents] DB path failed, trying parents fallback", "number", header.Number.Uint64(), "err", err)
	epochSwitchInfo, err = x.getEpochSwitchInfoWithParents(chain, header, header.Hash(), parents)
	if err != nil {
		// v142 DEEP FALLBACK: If parents contain the epoch switch header directly,
		// extract masternodes from header.Validators without DB dependency.
		// This handles the case where getEpochSwitchInfoWithParents fails due to
		// V1-format Extra fields or missing gap block snapshots.
		log.Debug("[GetMasternodesWithParents] parents fallback failed, trying deep fallback", "number", header.Number.Uint64(), "err", err)
		for _, p := range parents {
			// v143 FIX: Use round-based detection for V2-era epoch switches.
			// V2 epochs are round-based, not block-number-based, so pNum%Epoch==0
			// does not work after the V2 switch. A round reset (round < prevRound)
			// indicates an epoch switch in V2.
			pNum := p.Number.Uint64()
			if pNum <= x.config.V2.SwitchBlock.Uint64() {
				// V1-era: block-number-based detection still works
				if pNum%x.config.Epoch == 0 || pNum == x.config.V2.SwitchBlock.Uint64() {
					masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, p)
					if len(masternodes) > 0 {
						log.Info("[GetMasternodesWithParents] deep fallback success from V1-era epoch switch",
							"number", header.Number.Uint64(), "epochSwitch", pNum, "masternodes", len(masternodes))
						return masternodes
					}
				}
			} else {
				// V2-era: try IsEpochSwitch first, then round-based fallback
				isEpochSwitch, _, err := x.IsEpochSwitch(p)
				if err == nil && isEpochSwitch {
					masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, p)
					if len(masternodes) > 0 {
						log.Info("[GetMasternodesWithParents] deep fallback success from V2 IsEpochSwitch",
							"number", header.Number.Uint64(), "epochSwitch", pNum, "masternodes", len(masternodes))
						return masternodes
					}
				}
				// If IsEpochSwitch fails (e.g., V1-format Extra), try round-based detection
				_, round, err := x.getExtraFieldsNoChain(p)
				if err == nil && round == 0 {
					// Round 0 indicates a new epoch in V2
					masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, p)
					if len(masternodes) > 0 {
						log.Info("[GetMasternodesWithParents] deep fallback success from V2 round-0",
							"number", header.Number.Uint64(), "epochSwitch", pNum, "masternodes", len(masternodes))
						return masternodes
					}
				}
			}
		}
		// Trusted-checkpoint catchup fallback: when none of the parent-walking
		// fallbacks can resolve the masternode set (because the pre-anchor chain
		// isn't downloaded yet), use the masternodes pre-seeded into the snapshot
		// store at the trusted-checkpoint anchor. Bounded to the same catchup
		// window as inCheckpointCatchup, so this never affects post-catchup blocks.
		if x.inCheckpointCatchup(chain, header.Number.Uint64()) {
			type ckptAware interface {
				GetTrustedCheckpointAnchor() (uint64, common.Hash, bool)
			}
			if ca, ok := chain.(ckptAware); ok {
				if _, anchorHash, _ := ca.GetTrustedCheckpointAnchor(); anchorHash != (common.Hash{}) {
					if snap, errS := loadSnapshot(x.db, anchorHash); errS == nil && snap != nil && len(snap.NextEpochCandidates) > 0 {
						log.Debug("[GetMasternodesWithParents] checkpoint-catchup fallback from pre-seeded snapshot",
							"number", header.Number.Uint64(), "masternodes", len(snap.NextEpochCandidates))
						return snap.NextEpochCandidates
					}
				}
			}
		}
		log.Error("[GetMasternodesWithParents] all fallbacks failed", "number", header.Number.Uint64(), "err", err)
		return []common.Address{}
	}
	return epochSwitchInfo.Masternodes
}

// findEpochSwitchAtOrBefore returns the most recent V2 epoch switch header at
// or before targetNumber. It searches the optional `parents` slice first
// (newest→oldest) — required during bulk-sync header verification when
// in-batch ancestors are not yet committed to the DB — then falls back to
// walking the canonical chain via the DB up to 2*Epoch blocks back. Returns
// nil if no epoch switch is found in either source.
//
// The V1→V2 switch block counts as an epoch switch (see IsEpochSwitch).
func (x *XDPoS_v2) findEpochSwitchAtOrBefore(chain consensus.ChainReader, targetNumber uint64, parents []*types.Header) *types.Header {
	switchBlock := x.config.V2.SwitchBlock.Uint64()
	if targetNumber < switchBlock {
		return nil
	}

	// Parents-first scan: parents are ordered oldest→newest, so iterate in
	// reverse to get the closest match. Skip parents past targetNumber (they
	// can't be "at or before" the target).
	for i := len(parents) - 1; i >= 0; i-- {
		p := parents[i]
		if p == nil {
			continue
		}
		pn := p.Number.Uint64()
		if pn > targetNumber {
			continue
		}
		isEpochSwitch, _, err := x.IsEpochSwitch(p)
		if err == nil && isEpochSwitch {
			return p
		}
	}

	// DB walk-back fallback. Bound by 2*Epoch to cover timeout-stretched
	// epochs (matches the bound used in getEpochSwitchInfoWithParents).
	minNum := switchBlock
	if targetNumber > 2*x.config.Epoch && targetNumber-2*x.config.Epoch > switchBlock {
		minNum = targetNumber - 2*x.config.Epoch
	}
	for n := targetNumber; n >= minNum; n-- {
		h := chain.GetHeaderByNumber(n)
		if h == nil {
			if n == 0 {
				break
			}
			continue
		}
		isEpochSwitch, _, err := x.IsEpochSwitch(h)
		if err == nil && isEpochSwitch {
			return h
		}
		if n == 0 {
			break
		}
	}
	return nil
}

// findEpochSwitchAfter returns the first V2 epoch switch header strictly after
// fromNumber. Mirrors findEpochSwitchAtOrBefore in the forward direction —
// required by the isGapInput seed path in getSnapshot, where the gap defines
// masternodes that live in the NEXT epoch switch's Validators field.
// Bounded by fromNumber + 2*Epoch (timeout-stretched epochs).
func (x *XDPoS_v2) findEpochSwitchAfter(chain consensus.ChainReader, fromNumber uint64, parents []*types.Header) *types.Header {
	switchBlock := x.config.V2.SwitchBlock.Uint64()
	if fromNumber < switchBlock {
		fromNumber = switchBlock
	}

	// Parents-first scan: parents are ordered oldest→newest. Take the FIRST
	// switch strictly above fromNumber to match the forward semantics.
	for _, p := range parents {
		if p == nil {
			continue
		}
		pn := p.Number.Uint64()
		if pn <= fromNumber {
			continue
		}
		isEpochSwitch, _, err := x.IsEpochSwitch(p)
		if err == nil && isEpochSwitch {
			return p
		}
	}

	// DB walk-forward fallback.
	maxNum := fromNumber + 2*x.config.Epoch
	for n := fromNumber + 1; n <= maxNum; n++ {
		h := chain.GetHeaderByNumber(n)
		if h == nil {
			continue
		}
		isEpochSwitch, _, err := x.IsEpochSwitch(h)
		if err == nil && isEpochSwitch {
			return h
		}
	}
	return nil
}

// IsEpochSwitch checks if a header is an epoch switch block
func (x *XDPoS_v2) IsEpochSwitch(header *types.Header) (bool, uint64, error) {
	// V1->V2 switch block is treated as an epoch switch
	if header.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
		log.Info("[IsEpochSwitch] V1->V2 switch block", "number", header.Number.Uint64())
		return true, header.Number.Uint64() / x.config.Epoch, nil
	}

	quorumCert, round, err := x.getExtraFieldsNoChain(header)
	if err != nil {
		log.Error("[IsEpochSwitch] decode error", "err", err, "number", header.Number.Uint64())
		return false, 0, err
	}

	// v147 FIX: Handle V1 headers gracefully — V1 headers are NOT epoch switches
	if quorumCert == nil && round == 0 {
		return false, 0, nil
	}

	// Defensive nil checks (#391)
	if quorumCert == nil {
		log.Error("[IsEpochSwitch] quorumCert is nil", "number", header.Number.Uint64())
		return false, 0, errors.New("quorumCert is nil")
	}
	if quorumCert.ProposedBlockInfo == nil {
		log.Error("[IsEpochSwitch] ProposedBlockInfo is nil", "number", header.Number.Uint64())
		return false, 0, errors.New("ProposedBlockInfo is nil")
	}

	parentRound := quorumCert.ProposedBlockInfo.Round
	epochStartRound := round - round%types.Round(x.config.Epoch)
	epochNum := x.config.V2.SwitchEpoch + uint64(round)/x.config.Epoch

	// If parent is the V2 switch block, this is the first V2 epoch switch
	if quorumCert.ProposedBlockInfo.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
		log.Info("[IsEpochSwitch] First V2 epoch", "number", header.Number.Uint64(), "round", round)
		return true, epochNum, nil
	}

	isEpochSwitch := parentRound < epochStartRound

	// CRITICAL FIX: Add epoch switch to round2epochBlockInfo cache like v2.6.8
	if isEpochSwitch {
		x.round2epochBlockInfo.Add(round, &types.BlockInfo{
			Hash:   header.Hash(),
			Number: header.Number,
			Round:  round,
		})
		log.Debug("[IsEpochSwitch] EPOCH SWITCH CACHED", "number", header.Number.Uint64(), "round", round, "parentRound", parentRound, "epochStartRound", epochStartRound)
	}

	// Debug logging for all epoch switch checks near the V2 switch boundary
	if header.Number.Uint64() >= 56828000 && header.Number.Uint64() <= 56831000 {
		log.Info("[IsEpochSwitch] DEBUG",
			"number", header.Number.Uint64(),
			"round", round,
			"parentRound", parentRound,
			"epochStartRound", epochStartRound,
			"epochNum", epochNum,
			"isEpochSwitch", isEpochSwitch,
			"qcBlockNum", quorumCert.ProposedBlockInfo.Number.Uint64(),
			"qcBlockRound", quorumCert.ProposedBlockInfo.Round)
	}

	return isEpochSwitch, epochNum, nil
}

// calcMasternodes calculates masternodes for a block.
// `parents` (optional, oldest→newest) is forwarded to getSnapshot to enable
// the seed-from-epoch-switch fallback during bulk-sync verification when
// in-batch ancestors are not yet committed to the DB. Pass nil when no
// parents context is available (mining, post-import calls).
func (x *XDPoS_v2) calcMasternodes(chain consensus.ChainReader, blockNum *big.Int, parentHash common.Hash, round types.Round, parents []*types.Header) ([]common.Address, []common.Address, []common.Address, error) {
	// Fix #63: use per-round config for maxMasternodes
	maxMasternodes := x.config.V2.Config(uint64(round)).MaxMasternodes

	snap, err := x.getSnapshot(chain, blockNum.Uint64(), false, parents)
	if err != nil {
		return nil, nil, nil, err
	}

	// Use NextEpochCandidates directly for proposer rotation to match V1
	// behaviour exactly (XDPoSChain mining.go:53 reads NextEpochCandidates,
	// NOT a merged pool). When Penalties are non-empty in our snapshot,
	// CandidatePool() returns a longer/reordered list that produces a
	// different leaderIndex than V1 at the same round → split-view consensus
	// failure (chain halt on devnet-5151 Phase 2, 2026-06-13). HookPenalty's
	// comeback algorithm uses a different code path with its own pool view.
	candidates := snap.NextEpochCandidates
	log.Warn("[calcMasternodes] snapshot loaded", "blockNum", blockNum.Uint64(), "round", round, "candidates", len(candidates), "active", len(snap.NextEpochCandidates), "penalties", len(snap.Penalties), "maxMasternodes", maxMasternodes, "snapNumber", snap.Number, "snapHash", snap.Hash)

	// First V2 block: no penalties can be computed from V1 history,
	// and V2 HookPenalty cannot walk into V1 blocks because IsEpochSwitch fails on them.
	// Aligned with v2.6.8: only the first block after switch is special, not the entire first epoch.
	if blockNum.Uint64() == x.config.V2.SwitchBlock.Uint64()+1 {
		log.Info("[calcMasternodes] examing first v2 block")
		if len(candidates) == 0 {
			log.Error("[calcMasternodes] empty candidates at first V2 block", "blockNum", blockNum.Uint64(), "snapNumber", snap.Number, "snapHash", snap.Hash)
			return nil, nil, nil, fmt.Errorf("empty candidates at first V2 block %d", blockNum.Uint64())
		}
		if len(candidates) > maxMasternodes {
			candidates = candidates[:maxMasternodes]
		}
		return candidates, []common.Address{}, candidates, nil
	}

	// FIX #339: When checkpoint sync without state crosses V2 switch, the snapshot at gap block
	// may have 0 candidates (from V1 era). For V2 epoch switches after the first one,
	// we cannot compute penalties without state. Return what we can and let verifyHeader
	// fall back to GetMasternodesWithParents which can extract from header.Validators.
	if len(candidates) == 0 && blockNum.Uint64() > x.config.V2.SwitchBlock.Uint64()+1 {
		log.Warn("[calcMasternodes] empty candidates at post-switch V2 block, returning error to trigger verifyHeader fallback", "blockNum", blockNum.Uint64(), "round", round, "snapNumber", snap.Number)
		return nil, nil, nil, fmt.Errorf("empty candidates at V2 block %d, snapshot %d has no candidates", blockNum.Uint64(), snap.Number)
	}

	if x.HookPenalty == nil {
		if len(candidates) > maxMasternodes {
			candidates = candidates[:maxMasternodes]
		}
		// A.70 (refs #844 #859): when the snapshot was seeded via
		// UpdateMasternodesWithPenalties (checkpoint-sync anchor or A.70
		// post-pivot pre-seed), snap.Penalties holds the penalty addresses
		// from the originating epoch-switch header. Surface those as the
		// localPenalties so verifyHeader's CompareSignersLists against
		// header.Penalties succeeds. v2.6.8-style snapshots (Penalties
		// empty) fall back to the legacy empty-list behaviour, preserving
		// canonical algorithm for fully-imported chains.
		return candidates, snap.Penalties, candidates, nil
	}

	penalties, err := x.HookPenalty(chain, blockNum, parentHash, candidates)
	if err != nil {
		return nil, nil, nil, err
	}

	masternodes := removeItemFromArray(candidates, penalties)
	if len(masternodes) > maxMasternodes {
		masternodes = masternodes[:maxMasternodes]
	}

	return masternodes, penalties, candidates, nil
}

// UpdateMasternodes updates the masternode list
func (x *XDPoS_v2) UpdateMasternodes(chain consensus.ChainReader, header *types.Header, ms []common.Address) error {
	return x.UpdateMasternodesWithPenalties(chain, header, ms, nil)
}

// UpdateMasternodesWithPenalties stores a V2 snapshot at the given gap-block
// header, recording both the active masternode list and the associated
// penalty set in distinct snapshot fields. Used by the trusted-checkpoint
// anchor pre-seed (and any other path that has both pieces of information
// up front) so HookPenalty's comeback algorithm finds pens ⊆ CandidatePool
// without inflating the active-list field that drives proposer rotation.
func (x *XDPoS_v2) UpdateMasternodesWithPenalties(chain consensus.ChainReader, header *types.Header, ms, penalties []common.Address) error {
	number := header.Number.Uint64()
	if number%x.config.Epoch != x.config.Epoch-x.config.Gap {
		return nil
	}

	snap := newSnapshotWithPenalties(number, header.Hash(), ms, penalties)
	log.Info("[UpdateMasternodes] take snapshot", "number", number, "hash", header.Hash(), "masternodes", len(ms), "penalties", len(penalties))

	if err := storeSnapshot(snap, x.db); err != nil {
		log.Error("[UpdateMasternodes] Error while store snapshot", "error", err)
		return err
	}
	x.snapshots.Add(snap.Hash, snap)

	log.Info("[UpdateMasternodes] New masternodes updated", "number", snap.Number, "hash", snap.Hash)
	return nil
}

// SeedSnapshotAtHash persists a V2 masternode snapshot keyed at an arbitrary
// hash (typically the operator pivot's header hash), bypassing the
// gap-block-alignment gate on UpdateMasternodesWithPenalties. Required by
// A.70 (refs #844 #859) so the trusted-checkpoint catchup fallback in
// GetMasternodesWithParents (engine.go:1251) can resolve masternodes from a
// pre-seeded snapshot at the anchor hash. Pre-A.70 that fallback always
// failed because no snapshot was ever stored at the anchor hash, leaving
// post-pivot blocks unable to derive the active masternode set when the
// gap-key getSnapshot path also missed.
//
// Number is the block height the seed is for (informational; stored in the
// snapshot for diagnostics). Hash is the key the snapshot will be stored
// under. Masternodes is the active set (NextEpochCandidates field).
// Penalties is the penalty pool from the same epoch-switch (Penalties
// field — stored separately so HookPenalty's comeback algorithm finds
// pens ⊆ CandidatePool without inflating the proposer-rotation list).
//
// Safe to call repeatedly: storeSnapshot is a last-write-wins put.
func (x *XDPoS_v2) SeedSnapshotAtHash(number uint64, hash common.Hash, ms, penalties []common.Address) error {
	if len(ms) == 0 {
		return errors.New("SeedSnapshotAtHash: empty masternode list")
	}
	snap := newSnapshotWithPenalties(number, hash, ms, penalties)
	if err := storeSnapshot(snap, x.db); err != nil {
		log.Error("[SeedSnapshotAtHash] Failed to store snapshot", "hash", hash, "error", err)
		return err
	}
	x.snapshots.Add(snap.Hash, snap)
	log.Info("[SeedSnapshotAtHash] V2 snapshot persisted at arbitrary hash",
		"number", number, "hash", hash, "masternodes", len(ms), "penalties", len(penalties))
	return nil
}

// UpdateMasternodesFromHeader computes and stores the V2 masternode snapshot
// for a gap block. This must be called during block import so that future
// epoch switch blocks can resolve their masternode set.
// CRITICAL FIX (#390): Reads candidates from smart contract state (like v2.6.8 UpdateM1)
// instead of deriving them from the previous snapshot. This ensures new validators
// that joined the network are included in the snapshot.
func (x *XDPoS_v2) UpdateMasternodesFromHeader(chain consensus.ChainReader, header *types.Header, statedb *state.StateDB) error {
	number := header.Number.Uint64()
	if number%x.config.Epoch != x.config.Epoch-x.config.Gap {
		return nil
	}

	// CRITICAL FIX (#390): Read candidates from smart contract state at the gap block.
	// v2.6.8 does this via blockchain.UpdateM1() which queries the validator contract.
	// GP5 was deriving candidates from the previous snapshot via calcMasternodes,
	// which misses new validators that joined between epochs.
	candidates := state.GetCandidates(statedb)
	if len(candidates) == 0 {
		log.Warn("[UpdateMasternodesFromHeader] no candidates from smart contract, falling back to snapshot", "number", number)
		// Fallback: use calcMasternodes from previous snapshot (old behavior).
		// FIX (devnet/epoch-boundary): calcMasternodes calls getSnapshot(blockNum=number)
		// which tries chain.GetHeaderByNumber(gapNumber=number). When Finalize is called
		// for the gap block itself, the block is not yet in the DB so the lookup returns nil.
		// In that case, fall back further to the parent's snapshot (carry-forward masternodes).
		round, err := x.GetRoundNumber(header)
		if err != nil {
			return err
		}
		_, _, candidates, err = x.calcMasternodes(chain, header.Number, header.ParentHash, round, nil)
		if err != nil {
			log.Warn("[UpdateMasternodesFromHeader] calcMasternodes failed (gap block not yet in DB?), using parent snapshot masternodes", "number", number, "err", err)
			// Use parent's snapshot masternodes as carry-forward for this epoch.
			parentSnap, snapErr := x.getSnapshot(chain, header.Number.Uint64()-1, false, nil)
			if snapErr != nil || parentSnap == nil {
				log.Error("[UpdateMasternodesFromHeader] parent snapshot also unavailable", "number", number, "snapErr", snapErr)
				return fmt.Errorf("[UpdateMasternodesFromHeader] no candidates and no parent snapshot at block %d: %v", number, err)
			}
			candidates = parentSnap.NextEpochCandidates
			if len(candidates) == 0 {
				log.Error("[UpdateMasternodesFromHeader] parent snapshot has empty NextEpochCandidates", "number", number)
				return fmt.Errorf("[UpdateMasternodesFromHeader] empty candidates at block %d", number)
			}
			log.Info("[UpdateMasternodesFromHeader] carrying forward parent snapshot candidates", "number", number, "candidates", len(candidates))
		}
	} else {
		// Sort candidates by stake (descending) to match v2.6.8 behavior
		type candidateStake struct {
			addr  common.Address
			stake *big.Int
		}
		cs := make([]candidateStake, 0, len(candidates))
		for _, c := range candidates {
			if c != (common.Address{}) {
				cap := state.GetCandidateCap(statedb, c)
				cs = append(cs, candidateStake{addr: c, stake: cap})
			}
		}
		// Use frozen Go-1.18 quicksort (canonical's xdc_sort) — see comment in
		// repairSnapshot above for why stdlib sort.Slice (pdqsort) diverges.
		xdc_sort.Slice(cs, func(i, j int) bool {
			return cs[i].stake.Cmp(cs[j].stake) >= 0
		})
		candidates = make([]common.Address, len(cs))
		for i, c := range cs {
			candidates[i] = c.addr
		}
		log.Info("[UpdateMasternodesFromHeader] candidates from smart contract", "number", number, "candidates", len(candidates))
		if len(candidates) > 0 {
			log.Debug("V2 snapshot candidates", "block", number, "count", len(candidates),
				"first", candidates[0].Hex(), "last", candidates[len(candidates)-1].Hex())
		}
	}

	// P0 fix #894: compute penalties at write time so the snapshot carries
	// both the full pool (NextEpochCandidates=46) and the active count (32).
	// The synthetic-epoch-info path reads ActiveMasternodesLen for the QC/TC
	// threshold when the parent is absent during bulk sync.
	// INVARIANT: NextEpochCandidates stays the full pool; Penalties+ActiveMasternodesLen
	// carries the active-set info. No mutation of candidates order/content.
	if x.HookPenalty != nil {
		penalties, penErr := x.HookPenalty(chain, header.Number, header.ParentHash, candidates)
		if penErr != nil {
			log.Warn("[UpdateMasternodesFromHeader] HookPenalty failed, storing snapshot without penalties",
				"number", number, "err", penErr)
			return x.UpdateMasternodes(chain, header, candidates)
		}
		active := removeItemFromArray(candidates, penalties)
		log.Info("[UpdateMasternodesFromHeader] penalty-aware snapshot",
			"number", number, "pool", len(candidates), "penalties", len(penalties), "active", len(active))
		// Check if there is already a snapshot with ActiveMasternodesLen>0 at this hash
		// (e.g. from a trusted-checkpoint A.70 seed) — prefer it to avoid overwrite hazard.
		if existing, lerr := loadSnapshot(x.db, header.Hash()); lerr == nil && existing != nil && existing.ActiveMasternodesLen > 0 && len(existing.Penalties) > 0 {
			log.Info("[UpdateMasternodesFromHeader] existing penalty-aware snapshot found (A.70 seed), skipping overwrite",
				"number", number, "existingActive", existing.ActiveMasternodesLen, "existingPenalties", len(existing.Penalties))
			return nil
		}
		return x.UpdateMasternodesWithPenalties(chain, header, candidates, penalties)
	}
	// HookPenalty not wired (e.g. unit tests without penalty hook): store
	// without penalties; ActiveMasternodesLen stays 0 (accessor falls back).
	return x.UpdateMasternodes(chain, header, candidates)
}

// getExtraFieldsNoChain extracts V2 extra fields from a header without requiring chain access.
// Used by IsEpochSwitch which only needs quorumCert and round, not masternodes.
func (x *XDPoS_v2) getExtraFieldsNoChain(header *types.Header) (*types.QuorumCert, types.Round, error) {
	if header == nil {
		return nil, types.Round(0), errors.New("getExtraFieldsNoChain: nil header")
	}

	// Defensive check (#391): V1 pre-switch headers don't have V2-format Extra
	// v147 FIX: Return nil gracefully instead of error, so IsEpochSwitch can handle V1 headers
	if x.config.V2 != nil && x.config.V2.SwitchBlock != nil &&
		header.Number.Cmp(x.config.V2.SwitchBlock) < 0 {
		return nil, types.Round(0), nil
	}

	var decodedExtra types.ExtraFields_v2
	if err := DecodeExtraFields(header.Extra, &decodedExtra); err != nil {
		return nil, types.Round(0), err
	}
	return decodedExtra.QuorumCert, decodedExtra.Round, nil
}
func (x *XDPoS_v2) getExtraFields(chain consensus.ChainReader, header *types.Header) (*types.QuorumCert, types.Round, []common.Address, error) {
	if header == nil {
		return nil, types.Round(0), nil, errors.New("getExtraFields: nil header")
	}
	var masternodes []common.Address

	// v149 FIX: Use GetMasternodesFromEpochSwitchHeader for ALL blocks including switch block.
	// The old code hardcoded decodeMasternodesFromHeaderExtra for the switch block, which
	// assumes V1-format Extra and produces garbage when the switch block has V2-format Extra.
	// GetMasternodesFromEpochSwitchHeader now correctly prefers header.Validators for all
	// epoch switch blocks, including the V1->V2 switch block.
	masternodes = x.GetMasternodesFromEpochSwitchHeader(chain, header)

	var decodedExtra types.ExtraFields_v2
	if err := DecodeExtraFields(header.Extra, &decodedExtra); err != nil {
		log.Error("[getExtraFields] error decoding extra", "err", err, "number", header.Number.Uint64(), "extraHex", common.Bytes2Hex(header.Extra))
		return nil, types.Round(0), masternodes, err
	}
	parentRound := types.Round(0)
	if decodedExtra.QuorumCert != nil && decodedExtra.QuorumCert.ProposedBlockInfo != nil {
		parentRound = decodedExtra.QuorumCert.ProposedBlockInfo.Round
	}

	// DEBUG: Log detailed QC info for blocks near the epoch boundary
	if header.Number.Uint64() >= 56830290 && header.Number.Uint64() <= 56830300 {
		qcInfo := "nil"
		if decodedExtra.QuorumCert != nil {
			qcInfo = fmt.Sprintf("{Num:%d Round:%d Gap:%d}",
				decodedExtra.QuorumCert.ProposedBlockInfo.Number.Uint64(),
				decodedExtra.QuorumCert.ProposedBlockInfo.Round,
				decodedExtra.QuorumCert.GapNumber)
		}
		log.Info("[getExtraFields] DEBUG QC",
			"number", header.Number.Uint64(),
			"hash", header.Hash().Hex(),
			"round", decodedExtra.Round,
			"parentRound", parentRound,
			"qc", qcInfo,
			"extraLen", len(header.Extra))
	}

	return decodedExtra.QuorumCert, decodedExtra.Round, masternodes, nil
}

// decodeMasternodesFromHeaderExtra extracts masternodes from V1 header extra
func decodeMasternodesFromHeaderExtra(header *types.Header) []common.Address {
	extraVanity := 32
	extraSeal := 65
	masternodes := make([]common.Address, (len(header.Extra)-extraVanity-extraSeal)/common.AddressLength)
	for i := 0; i < len(masternodes); i++ {
		copy(masternodes[i][:], header.Extra[extraVanity+i*common.AddressLength:])
	}
	return masternodes
}

// DecodeExtraFields decodes V2 extra fields
func DecodeExtraFields(extra []byte, decoded *types.ExtraFields_v2) error {
	if len(extra) < 1 {
		return errors.New("extra too short")
	}
	if extra[0] != 2 {
		return errors.New("not V2 extra format")
	}
	return rlp.DecodeBytes(extra[1:], decoded)
}

// getEpochSwitchInfo returns epoch switch information with singleflight deduplication.
// C12 FIX: Replaces the recursion guard with singleflight.Group so concurrent calls
// for the same hash are deduplicated, eliminating false-positive recursion errors.
// v145: Added cache validation — evict entries with empty masternodes to prevent
// permanently cached bad data during sync.
func (x *XDPoS_v2) getEpochSwitchInfo(chain consensus.ChainReader, header *types.Header, hash common.Hash) (*types.EpochSwitchInfo, error) {
	// Fast path: check cache without singleflight overhead
	if info, ok := x.epochSwitches.Get(hash); ok && info != nil {
		// v145 FIX: Validate cached entry has non-empty masternodes
		if len(info.Masternodes) > 0 {
			log.Debug("[getEpochSwitchInfo] cache hit", "hash", hash.Hex(), "epochSwitchNumber", info.EpochSwitchBlockInfo.Number.Uint64(), "epochSwitchRound", info.EpochSwitchBlockInfo.Round)
			return info, nil
		}
		// v145: Cache hit returned empty masternodes — this is invalid. Evict and recompute.
		log.Warn("[getEpochSwitchInfo] cache hit returned empty masternodes, evicting and recomputing",
			"hash", hash.Hex(), "epochSwitchNumber", info.EpochSwitchBlockInfo.Number.Uint64())
		x.epochSwitches.Remove(hash)
	}

	// Slow path: use singleflight to deduplicate concurrent calls for the same hash.
	// Only one goroutine will execute the body; others wait and receive the same result.
	result, err, _ := x.epochSwitchSF.Do(hash.Hex(), func() (interface{}, error) {
		return x.getEpochSwitchInfoInner(chain, header, hash)
	})
	if err != nil {
		return nil, err
	}
	return result.(*types.EpochSwitchInfo), nil
}

// getEpochSwitchInfoInner contains the actual logic, called exclusively via singleflight.
func (x *XDPoS_v2) getEpochSwitchInfoInner(chain consensus.ChainReader, header *types.Header, hash common.Hash) (*types.EpochSwitchInfo, error) {
	// Double-check cache after winning the singleflight race
	if info, ok := x.epochSwitches.Get(hash); ok && info != nil {
		// v145: Validate cached entry has non-empty masternodes
		if len(info.Masternodes) > 0 {
			return info, nil
		}
		// v145: Evict bad cache entry
		x.epochSwitches.Remove(hash)
	}

	h := header
	if h == nil {
		h = chain.GetHeaderByHash(hash)
		if h == nil {
			return nil, fmt.Errorf("header not found: %s", hash.Hex())
		}
	} else if h.Hash() != hash {
		return nil, fmt.Errorf("header hash mismatch: header %s, input %s", h.Hash().Hex(), hash.Hex())
	}

	// Fail-fast: V2 epoch info is not defined for V1 headers.
	// Walking past the V2 switch block via parent-hash recursion is a category error —
	// it can only terminate by failing at genesis. Surface a clear error so the caller
	// (typically GetMasternodesWithParents during bulk sync) routes to V1 instead of
	// driving the recursion all the way to genesis.
	if x.config.V2 != nil && x.config.V2.SwitchBlock != nil &&
		h.Number.Cmp(x.config.V2.SwitchBlock) < 0 {
		return nil, fmt.Errorf(
			"getEpochSwitchInfoInner: header %d (%s) is V1 (< switchBlock %d); V2 epoch info not applicable",
			h.Number.Uint64(), hash.Hex(), x.config.V2.SwitchBlock.Uint64())
	}

	// Genesis guard: V2 IsEpochSwitch returns false for genesis, so handle it explicitly
	if h.Number.Sign() == 0 {
		extraVanity, extraSeal := 32, 65
		var masternodes []common.Address
		if len(h.Extra) >= extraVanity+extraSeal {
			n := (len(h.Extra) - extraVanity - extraSeal) / common.AddressLength
			masternodes = make([]common.Address, n)
			for i := 0; i < n; i++ {
				copy(masternodes[i][:], h.Extra[extraVanity+i*common.AddressLength:])
			}
		}
		info := &types.EpochSwitchInfo{
			Masternodes:    masternodes,
			MasternodesLen: len(masternodes),
			EpochSwitchBlockInfo: &types.BlockInfo{
				Hash: hash, Number: h.Number, Round: 0,
			},
		}
		x.epochSwitches.Add(hash, info)
		return info, nil
	}

	log.Debug("[getEpochSwitchInfo] enter", "number", h.Number.Uint64(), "hash", hash.Hex())
	isEpochSwitch, _, err := x.IsEpochSwitch(h)
	if err != nil {
		return nil, err
	}

	if isEpochSwitch {
		if h.Number.Uint64() == 0 {
			// Genesis block
			extraVanity := 32
			extraSeal := 65
			masternodes := make([]common.Address, (len(h.Extra)-extraVanity-extraSeal)/common.AddressLength)
			for i := 0; i < len(masternodes); i++ {
				copy(masternodes[i][:], h.Extra[extraVanity+i*common.AddressLength:])
			}
			info := &types.EpochSwitchInfo{
				Masternodes:    masternodes,
				MasternodesLen: len(masternodes),
				EpochSwitchBlockInfo: &types.BlockInfo{
					Hash:   hash,
					Number: h.Number,
					Round:  0,
				},
			}
			x.epochSwitches.Add(hash, info)
			return info, nil
		}

		masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, h)

		// V2 epoch switch blocks (except the V1->V2 switch block) may have empty
		// header.Validators. Rebuild the snapshot on-the-fly if needed.
		if len(masternodes) == 0 && h.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() {
			repaired, err := x.repairSnapshot(chain, h.Number.Uint64())
			if err == nil {
				masternodes = repaired
			} else {
				log.Error("[getEpochSwitchInfo] snapshot repair failed", "checkpoint", h.Number.Uint64(), "err", err)
			}
		}

		// v146 FIX: Fail-fast if masternodes are still empty after all fallbacks.
		// Do NOT cache empty masternode data — this permanently poisons the cache
		// and causes "empty masternode list" errors for all blocks in this epoch.
		if len(masternodes) == 0 {
			return nil, fmt.Errorf("getEpochSwitchInfoInner: empty masternodes at epoch switch %d (hash %s) — repairSnapshot failed or header.Validators empty", h.Number.Uint64(), hash.Hex())
		}

		var quorumCert *types.QuorumCert
		var round types.Round
		if h.Number.Cmp(x.config.V2.SwitchBlock) == 0 {
			// V1->V2 switch block has no V2 extra fields
			quorumCert = nil
			round = types.Round(0)
		} else {
			var err error
			quorumCert, round, err = x.getExtraFieldsNoChain(h)
			if err != nil {
				log.Error("[getEpochSwitchInfo] get extra field no chain", "err", err, "number", h.Number.Uint64())
				return nil, err
			}
		}

		info := &types.EpochSwitchInfo{
			Masternodes:    masternodes,
			MasternodesLen: len(masternodes),
			Penalties:      extractAddressesFromBytes(h.Penalties),
			EpochSwitchBlockInfo: &types.BlockInfo{
				Hash:   hash,
				Number: h.Number,
				Round:  round,
			},
		}
		if quorumCert != nil {
			info.EpochSwitchParentBlockInfo = quorumCert.ProposedBlockInfo
		}
		log.Info("[getEpochSwitchInfo] FOUND epoch switch", "number", h.Number.Uint64(), "hash", hash.Hex(), "round", round, "masternodes", len(masternodes))
		x.epochSwitches.Add(hash, info)
		return info, nil
	}

	log.Debug("[getEpochSwitchInfo] recurse to parent", "number", h.Number.Uint64(), "parentHash", h.ParentHash.Hex())
	// v142: During bulk sync, if parent is not in DB, don't recurse infinitely.
	// Return error so getEpochSwitchInfoWithParents can handle it with parents slice.
	parentHeader := chain.GetHeaderByHash(h.ParentHash)
	if parentHeader == nil {
		// Parent not in DB - likely during bulk sync
		// v155 FIX: If this header has valid V2 extra fields, try to extract
		// masternodes directly from the header and create a synthetic epoch info.
		// This handles checkpoint-sync boundaries where the parent chain isn't available.
		if h.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() {
			// Decode V2 extra once — every source below needs it, and the header's
			// own QC carries the GapNumber that indexes the epoch's seating snapshot.
			quorumCert, round, extraErr := x.getExtraFieldsNoChain(h)
			// Source A: masternodes from header.Validators (works on networks where
			// V2 epoch switch headers carry validators inline).
			masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, h)
			fromSnapshot := false
			var snapForActiveLen *SnapshotV2
			// Source B0 (fix #1327, net5151 block 5851 bulk-sync wedge): h is
			// mid-epoch here (IsEpochSwitch returned false above), so h and its
			// parent share an epoch, and h's own QC.GapNumber indexes the snapshot
			// whose activeSet SEATED that epoch — i.e. the set that actually signed
			// QCs over h. Prefer it over the snapshot stored AT h's hash (Source B):
			// when h is a gap block, that snapshot holds the NEXT epoch's candidate
			// pool, which diverges from the currently-active set whenever the
			// candidate list changed mid-epoch. Observed on net5151: xOne proposed
			// at ~5280 (pool→5) but seated only at epoch switch 6300; the gap-block
			// 5850 snapshot said pool=5 → threshold 3.335 while the valid canonical
			// QC had 3-of-4 active signatures → block 5851 rejected, catch-up sync
			// wedged permanently (the wrong info was also cached by hash below).
			// This mirrors the trust argument of the verifyQC gap cross-check
			// (utils.go verifyQC): QC signatures commit to GapNumber, so the
			// snapshot it selects is the authoritative signer set.
			if len(masternodes) == 0 && extraErr == nil && quorumCert != nil {
				if snap, serr := x.getSnapshot(chain, quorumCert.GapNumber, true, nil); serr == nil && snap != nil && len(snap.NextEpochCandidates) > 0 {
					masternodes = snap.activeSet() // active set (post-penalty) for membership
					snapForActiveLen = snap
					fromSnapshot = true
				}
			}
			// Source B: pre-seeded snapshot at this hash (trusted-checkpoint anchor).
			// Apothem V2 headers have empty Validators, so source A returns nothing —
			// the snapshot is the only path to recover masternodes when the parent
			// chain hasn't been downloaded yet. Only correct when the pool did not
			// change within the epoch — Source B0 above is preferred when resolvable.
			//
			// P0 fix #894: when loading from snapshot, use snap.activeSet() for
			// Masternodes (membership) and snap.ActiveLen() for MasternodesLen
			// (QC/TC threshold). This corrects the false-negative at gap blocks
			// (n%900==450) during bulk sync where the snapshot holds the full
			// candidate pool (46) but only 32 are active post-penalty.
			// MasternodesLen=46 → threshold=30.682 rejects valid 22-sig QCs;
			// MasternodesLen=32 → threshold=21.344 → 22 passes.
			if len(masternodes) == 0 {
				if snap, err := loadSnapshot(x.db, hash); err == nil && snap != nil && len(snap.NextEpochCandidates) > 0 {
					masternodes = snap.activeSet() // active set (post-penalty) for membership
					snapForActiveLen = snap
					fromSnapshot = true
				}
			}
			if len(masternodes) > 0 {
				err := extraErr
				if err == nil {
					// Determine the masternodesLen (threshold) — use the snapshot's
					// active count when available (P0 fix #894).
					// PARITY FIX: cap the synthetic masternode set + threshold at MaxMasternodes.
					// A pre-seeded snapshot lacking Penalties/ActiveMasternodesLen makes
					// snap.activeSet() return the full candidate pool and ActiveLen() the
					// full-pool length, neither capped at MaxMasternodesV2 -> inflated QC 2/3
					// threshold + non-active candidates admitted, so a strict complete-state
					// node hard-fails legit blocks ("validator not within masternodes" /
					// "verifyQC: not enough signatures"; mainnet 103,796,550: 171 vs 108).
					// NextEpochCandidates is stake-sorted, so top-N matches calcMasternodes.
					maxMN := x.config.V2.Config(uint64(round)).MaxMasternodes
					if maxMN > 0 && len(masternodes) > maxMN {
						masternodes = masternodes[:maxMN]
					}
					masternodesLen := len(masternodes)
					activeLenSrc := "header"
					if snapForActiveLen != nil {
						masternodesLen = snapForActiveLen.ActiveLen()
						activeLenSrc = snapForActiveLen.activeLenSource()
					}
					// DB walk (#1327 penalty-boundary fix, extended): walk the canonical DB
					// backward from h to find the real epoch-switch block for h's epoch.
					// This fires regardless of activeLenSrc ("fallback", "field", or
					// "derived") because the gap snapshot may encode penalties from the
					// PREVIOUS epoch (e.g. xOne penalized at gap 8233 but rejoining at
					// epoch switch 8683) — giving a "field"-source ActiveMasternodesLen=4
					// when the correct count for this epoch is 5.  Gating on "fallback"
					// only missed the "field" case: the VerifyHeaders goroutine can be
					// ahead of InsertChain (race between verify-goroutine and DB commit),
					// so when block N is verified its parent is not yet in DB — it falls
					// into this synthetic path, loads the stale gap snapshot, gets
					// masternodes=4 with activeLenSrc="field", skips the walk, and
					// CACHES the wrong result.  Block N+1 then hits the cache hit → 4
					// masternodes → validator-not-within-masternodes (net5151 block 8712).
					// The walk is still bounded by 2*Epoch and breaks on the first epoch
					// switch found, so it is O(Epoch) in the worst case.
					// dbWalkEpochSwitch holds the epoch-switch header found by the walk;
					// nil if nothing found (checkpoint-sync boundary with no DB coverage).
					var dbWalkEpochSwitch *types.Header
					{
						var walkMin uint64
						limit := uint64(x.config.Epoch) * 2
						if h.Number.Uint64() > x.config.V2.SwitchBlock.Uint64()+limit {
							walkMin = h.Number.Uint64() - limit
						} else {
							walkMin = x.config.V2.SwitchBlock.Uint64()
						}
						for n := h.Number.Uint64() - 1; n > walkMin; n-- {
							esHeader := chain.GetHeaderByNumber(n)
							if esHeader == nil {
								continue
							}
							isES, _, _ := x.IsEpochSwitch(esHeader)
							if isES {
								esMN := x.GetMasternodesFromEpochSwitchHeader(chain, esHeader)
								if len(esMN) > 0 {
									masternodes = esMN
									if maxMN > 0 && len(masternodes) > maxMN {
										masternodes = masternodes[:maxMN]
									}
									masternodesLen = len(masternodes)
									activeLenSrc = "dbwalk"
									dbWalkEpochSwitch = esHeader
									log.Debug("[getEpochSwitchInfo] synthetic fallback: DB walk found epoch-switch",
										"h", h.Number.Uint64(), "epochSwitch", n, "masternodes", masternodesLen)
								}
								break
							}
						}
					}
					if maxMN > 0 && masternodesLen > maxMN {
						masternodesLen = maxMN
					}
					info := &types.EpochSwitchInfo{
						Masternodes:    masternodes,
						MasternodesLen: masternodesLen,
						Penalties:      extractAddressesFromBytes(h.Penalties),
						EpochSwitchBlockInfo: &types.BlockInfo{
							Hash:   hash,
							Number: h.Number,
							Round:  round,
						},
					}
					if quorumCert != nil && quorumCert.ProposedBlockInfo != nil {
						info.EpochSwitchParentBlockInfo = quorumCert.ProposedBlockInfo
					}
					// If the DB walk-back found the real epoch-switch block, replace the
					// synthetic EpochSwitchBlockInfo (h's own number/round) with the real
					// epoch-switch block's data.  verifyQC's gap check derives the expected
					// gap from EpochSwitchBlockInfo.Number, so using h's number (e.g. 8678)
					// instead of the real epoch-switch number (7783) produces a wrong gap
					// (7650 vs 6750) and triggers a false gap-number mismatch.
					if dbWalkEpochSwitch != nil {
						esQC, esRound, esErr := x.getExtraFieldsNoChain(dbWalkEpochSwitch)
						if esErr == nil {
							info.EpochSwitchBlockInfo = &types.BlockInfo{
								Hash:   dbWalkEpochSwitch.Hash(),
								Number: dbWalkEpochSwitch.Number,
								Round:  esRound,
							}
							if esQC != nil && esQC.ProposedBlockInfo != nil {
								info.EpochSwitchParentBlockInfo = esQC.ProposedBlockInfo
							}
						}
					}
					if fromSnapshot {
						poolLen := 0
						penaltiesLen := 0
						if snapForActiveLen != nil {
							poolLen = len(snapForActiveLen.NextEpochCandidates)
							penaltiesLen = len(snapForActiveLen.Penalties)
						}
						log.Info("[getEpochSwitchInfo] synthetic epoch info from pre-seeded snapshot (parent missing, not cached)",
							"number", h.Number.Uint64(), "hash", hash.Hex(),
							"activeLen", masternodesLen, "poolLen", poolLen,
							"penalties", penaltiesLen, "activeLenSource", activeLenSrc, "round", round)
					} else {
						log.Info("[getEpochSwitchInfo] synthetic epoch info from header (parent missing, not cached)",
							"number", h.Number.Uint64(), "hash", hash.Hex(),
							"masternodes", len(masternodes), "round", round)
					}
					// Do NOT cache synthetic epoch info computed while the parent chain is
					// absent from the DB (xdcSyncer batch-verify race).  The VerifyHeaders
					// goroutine runs ahead of InsertChain's DB-commit loop: a block verified
					// before its parent is committed would persist a stale masternode set
					// (e.g. 4 masternodes from the pre-epoch-switch gap snapshot) under its
					// hash.  The next batch then hits that cache entry instead of recursing
					// to the real epoch-switch block in the DB, failing the signer check.
					// After InsertChain commits the batch, getEpochSwitchInfo will recurse
					// correctly through the now-committed parents to the real epoch-switch
					// block and cache the authoritative result there.
					return info, nil
				}
			}
		}
		return nil, fmt.Errorf("parent header %s not in DB (bulk sync?)", h.ParentHash.Hex())
	}
	parentInfo, err := x.getEpochSwitchInfo(chain, parentHeader, h.ParentHash)
	if err != nil {
		log.Error("[getEpochSwitchInfo] recursive error", "err", err, "hash", hash.Hex(), "number", h.Number.Uint64())
		return nil, err
	}
	log.Debug("[getEpochSwitchInfo] recurse result", "number", h.Number.Uint64(), "epochSwitchNumber", parentInfo.EpochSwitchBlockInfo.Number.Uint64(), "epochSwitchRound", parentInfo.EpochSwitchBlockInfo.Round)
	x.epochSwitches.Add(hash, parentInfo)
	return parentInfo, nil
}

// getMasternodesFromSnapshot loads the epoch masternodes from the snapshot DB.
// The snapshot is stored at the gap block for the given epoch switch number.
func (x *XDPoS_v2) getMasternodesFromSnapshot(chain consensus.ChainReader, epochSwitchNum uint64) []common.Address {
	gapNumber := epochSwitchNum - x.config.Gap
	if gapNumber > epochSwitchNum {
		gapNumber = 0
	}
	gapHeader := chain.GetHeaderByNumber(gapNumber)
	if gapHeader == nil {
		return nil
	}
	if snap, err := loadSnapshot(x.db, gapHeader.Hash()); err == nil && snap != nil {
		return snap.NextEpochCandidates
	}
	return nil
}

// getEpochSwitchInfoWithParents is like getEpochSwitchInfo but can resolve the
// epoch switch header from the supplied parents slice when it is not yet in the DB.
// Required during bulk sync when a batch spans an epoch boundary.
func (x *XDPoS_v2) getEpochSwitchInfoWithParents(chain consensus.ChainReader, header *types.Header, hash common.Hash, parents []*types.Header) (*types.EpochSwitchInfo, error) {
	// Try cache + DB first (re-use existing logic)
	info, err := x.getEpochSwitchInfo(chain, header, hash)
	if err == nil {
		return info, nil
	}
	// If DB lookup failed, try to find the epoch switch header in parents.
	if len(parents) == 0 {
		return nil, err
	}
	h := header
	if h == nil {
		h = chain.GetHeaderByHash(hash)
		if h == nil {
			return nil, err
		}
	}

	// v152 FIX: Check if the target header itself is the epoch switch block.
	// During bulk sync, the header passed in may be the epoch switch block,
	// but IsEpochSwitch was failing because getExtraFieldsNoChain couldn't
	// decode V1-format Extra fields. Use round-based detection for V2.
	if h != nil && h.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() {
		_, round, err := x.getExtraFieldsNoChain(h)
		if err == nil && round == 0 {
			// Round 0 = new epoch in V2. Extract masternodes directly.
			masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, h)
			if len(masternodes) > 0 {
				_, round, _, _ := x.getExtraFields(chain, h)
				info = &types.EpochSwitchInfo{
					Masternodes:    masternodes,
					MasternodesLen: len(masternodes),
					Penalties:      extractAddressesFromBytes(h.Penalties),
					EpochSwitchBlockInfo: &types.BlockInfo{
						Hash:   h.Hash(),
						Round:  round,
						Number: h.Number,
					},
				}
				log.Info("[getEpochSwitchInfoWithParents] target header is epoch switch (round-0)",
					"number", h.Number.Uint64(), "hash", hash.Hex(), "masternodes", len(masternodes))
				x.epochSwitches.Add(hash, info)
				return info, nil
			}
		}
	}

	// Use IsEpochSwitch directly on each parent, matching v2.6.8 reference.
	// Parents are ordered oldest->newest (verifyHeader.go uses parents[len-1] as
	// the immediate parent of the target header). When the parents slice spans
	// more than one epoch we must return the epoch switch CLOSEST to the target,
	// so iterate newest->oldest and break on the first match. The previous
	// oldest->newest scan returned the farthest epoch switch, producing an
	// off-by-one-epoch gap number in verifyQC (issue #534).
	// IsEpochSwitch handles V1 headers gracefully (returns false, nil).
	var epochSwitchHeader *types.Header

	for i := len(parents) - 1; i >= 0; i-- {
		isEpochSwitch, _, err := x.IsEpochSwitch(parents[i])
		if err == nil && isEpochSwitch {
			log.Debug("[getEpochSwitchInfoWithParents] found epoch switch via IsEpochSwitch",
				"number", parents[i].Number.Uint64(), "index", i, "totalParents", len(parents))
			epochSwitchHeader = parents[i]
			break
		}
	}

	if epochSwitchHeader == nil {
		// v153 FIX: Epoch switch not in parents slice. Try round2epochBlockInfo cache.
		// During bulk sync, the epoch switch block may have been committed earlier
		// and is no longer in the parents slice. The round2epochBlockInfo cache
		// maps epoch start rounds to epoch switch block info.
		_, currentRound, err2 := x.getExtraFieldsNoChain(h)
		if err2 == nil && currentRound > 0 {
			// Calculate which epoch the target block SHOULD be in
			targetEpochStartRound := currentRound - currentRound%types.Round(x.config.Epoch)

			// Try to find the epoch switch block in cache for the CORRECT epoch
			if blockInfo, ok := x.round2epochBlockInfo.Get(targetEpochStartRound); ok && blockInfo != nil {
				// Validate: the cached block's round must match the expected epoch start round
				if blockInfo.Round == types.Round(targetEpochStartRound) {
					// Found valid epoch switch block in cache - look it up in chain DB
					esHeader := chain.GetHeaderByHash(blockInfo.Hash)
					if esHeader != nil {
						masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, esHeader)
						if len(masternodes) > 0 {
							_, round, _, _ := x.getExtraFields(chain, esHeader)
							info = &types.EpochSwitchInfo{
								Masternodes:    masternodes,
								MasternodesLen: len(masternodes),
								Penalties:      extractAddressesFromBytes(esHeader.Penalties),
								EpochSwitchBlockInfo: &types.BlockInfo{
									Hash:   esHeader.Hash(),
									Round:  round,
									Number: esHeader.Number,
								},
							}
							log.Debug("[getEpochSwitchInfoWithParents] found epoch switch via round2epochBlockInfo cache",
								"number", h.Number.Uint64(), "epochSwitch", blockInfo.Number.Uint64(), "epochStartRound", targetEpochStartRound, "round", round)
							x.epochSwitches.Add(hash, info)
							return info, nil
						}
					} else {
						// Epoch switch block not in DB yet, but we have block info
						// Try to look up by number
						esHeader = chain.GetHeaderByNumber(blockInfo.Number.Uint64())
						if esHeader != nil {
							masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, esHeader)
							if len(masternodes) > 0 {
								_, round, _, _ := x.getExtraFields(chain, esHeader)
								info = &types.EpochSwitchInfo{
									Masternodes:    masternodes,
									MasternodesLen: len(masternodes),
									Penalties:      extractAddressesFromBytes(esHeader.Penalties),
									EpochSwitchBlockInfo: &types.BlockInfo{
										Hash:   esHeader.Hash(),
										Round:  round,
										Number: esHeader.Number,
									},
								}
								log.Debug("[getEpochSwitchInfoWithParents] found epoch switch via round2epochBlockInfo cache (by number)",
									"number", h.Number.Uint64(), "epochSwitch", blockInfo.Number.Uint64(), "epochStartRound", targetEpochStartRound)
								x.epochSwitches.Add(hash, info)
								return info, nil
							}
						}
					}
				} else {
					log.Warn("[getEpochSwitchInfoWithParents] round2epochBlockInfo cache has wrong epoch",
						"number", h.Number.Uint64(), "expectedRound", targetEpochStartRound, "cachedRound", blockInfo.Round, "cachedBlock", blockInfo.Number.Uint64())
				}
			}
		}

		// v153b: Try to find epoch switch block directly in chain DB by walking back
		// from the current header's number. For V2, epoch switch blocks have round=0.
		// CRITICAL: Walk back up to 2*Epoch blocks because timeouts can make an epoch
		// span more than Epoch blocks.
		if h != nil && h.Number.Uint64() > x.config.V2.SwitchBlock.Uint64() {
			// Try to find the most recent epoch switch block in DB by checking round=0
			// Start from current block and walk back up to 2*Epoch blocks
			startNum := h.Number.Uint64()
			minNum := x.config.V2.SwitchBlock.Uint64()
			if startNum > uint64(x.config.Epoch)*2 {
				minNum = startNum - uint64(x.config.Epoch)*2
			}
			for num := startNum - 1; num > minNum && num >= x.config.V2.SwitchBlock.Uint64(); num-- {
				esHeader := chain.GetHeaderByNumber(num)
				if esHeader != nil {
					// v154 FIX: Special-case the V2 switch block.
					// The V2 switch block has V1-format Extra (not starting with 0x02),
					// so getExtraFieldsNoChain returns "not V2 extra format" error.
					// However, IsEpochSwitch hard-codes the switch block as epoch switch,
					// and GetMasternodesFromEpochSwitchHeader handles both V1/V2 formats.
					isEpochSwitch, _, _ := x.IsEpochSwitch(esHeader)
					if isEpochSwitch {
						masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, esHeader)
						if len(masternodes) > 0 {
							_, fullRound, _, _ := x.getExtraFields(chain, esHeader)
							info = &types.EpochSwitchInfo{
								Masternodes:    masternodes,
								MasternodesLen: len(masternodes),
								Penalties:      extractAddressesFromBytes(esHeader.Penalties),
								EpochSwitchBlockInfo: &types.BlockInfo{
									Hash:   esHeader.Hash(),
									Round:  fullRound,
									Number: esHeader.Number,
								},
							}
							log.Debug("[getEpochSwitchInfoWithParents] found epoch switch by DB walk-back",
								"number", h.Number.Uint64(), "epochSwitch", num, "round", fullRound)
							x.epochSwitches.Add(hash, info)
							return info, nil
						}
					}
				}
			}
		}

		// v155 CHECKPOINT BOUNDARY FALLBACK: When the target block is a
		// TrustedSyncCheckpoint and its parent chain is not in DB (because
		// pre-checkpoint headers were skipped), we cannot walk back to find the
		// epoch switch block.  In this case the checkpoint header itself may
		// be the epoch switch block (e.g. V2 switch checkpoint 56828700).  If
		// it is, extract masternodes directly from header.Validators.
		if h != nil {
			isEpochSwitch, _, errEs := x.IsEpochSwitch(h)
			if errEs == nil && isEpochSwitch {
				masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, h)
				if len(masternodes) > 0 {
					_, round, _, _ := x.getExtraFields(chain, h)
					info = &types.EpochSwitchInfo{
						Masternodes:    masternodes,
						MasternodesLen: len(masternodes),
						Penalties:      extractAddressesFromBytes(h.Penalties),
						EpochSwitchBlockInfo: &types.BlockInfo{
							Hash:   h.Hash(),
							Round:  round,
							Number: h.Number,
						},
					}
					log.Info("[getEpochSwitchInfoWithParents] checkpoint boundary fallback: target IS epoch switch",
						"number", h.Number.Uint64(), "hash", hash.Hex(), "masternodes", len(masternodes))
					x.epochSwitches.Add(hash, info)
					return info, nil
				}
			}
		}
		return nil, err
	}

	// Try to get masternodes from epoch switch header
	masternodes := x.GetMasternodesFromEpochSwitchHeader(chain, epochSwitchHeader)

	// For V2 epoch switch blocks with empty validators, try to get from gap block in parents.
	// Use the ACTUAL epoch switch number found via IsEpochSwitch, not block-number modulo.
	epochSwitchNumActual := epochSwitchHeader.Number.Uint64()
	if len(masternodes) == 0 && epochSwitchNumActual > x.config.V2.SwitchBlock.Uint64() {
		gapNumber := epochSwitchNumActual - x.config.Gap
		if gapNumber <= epochSwitchNumActual {
			// Look for gap block in parents
			for i := len(parents) - 1; i >= 0; i-- {
				if parents[i].Number.Uint64() == gapNumber {
					// Try to load snapshot for this gap block
					if snap, serr := loadSnapshot(x.db, parents[i].Hash()); serr == nil && snap != nil && len(snap.NextEpochCandidates) > 0 {
						log.Info("[getEpochSwitchInfoWithParents] loaded masternodes from gap block snapshot",
							"epochSwitch", epochSwitchNumActual, "gapNumber", gapNumber, "masternodes", len(snap.NextEpochCandidates))
						masternodes = snap.NextEpochCandidates
					} else {
						log.Warn("[getEpochSwitchInfoWithParents] gap block snapshot not available, trying repair",
							"epochSwitch", epochSwitchNumActual, "gapNumber", gapNumber, "snapErr", serr)
						repaired, rerr := x.repairSnapshot(chain, epochSwitchNumActual)
						if rerr == nil {
							masternodes = repaired
						}
					}
					break
				}
			}
		}
		// If still empty, try repair as last resort
		if len(masternodes) == 0 {
			repaired, rerr := x.repairSnapshot(chain, epochSwitchNumActual)
			if rerr == nil {
				masternodes = repaired
			}
		}
	}

	// v146 FIX: Do NOT cache empty masternode data from parents slice.
	// If masternodes are empty, return error instead of poisoning cache.
	if len(masternodes) == 0 {
		return nil, fmt.Errorf("getEpochSwitchInfoWithParents: empty masternodes at epoch switch %d (hash %s) — all fallbacks exhausted", epochSwitchHeader.Number.Uint64(), epochSwitchHeader.Hash().Hex())
	}

	_, round, _, _ := x.getExtraFields(chain, epochSwitchHeader)
	info = &types.EpochSwitchInfo{
		Masternodes:    masternodes,
		MasternodesLen: len(masternodes),
		Penalties:      extractAddressesFromBytes(epochSwitchHeader.Penalties),
		EpochSwitchBlockInfo: &types.BlockInfo{
			Hash:   epochSwitchHeader.Hash(),
			Round:  round,
			Number: epochSwitchHeader.Number,
		},
	}
	// v146 FIX: Only cache if masternodes are non-empty
	log.Debug("[getEpochSwitchInfoWithParents] caching epoch switch info",
		"number", epochSwitchHeader.Number.Uint64(),
		"hash", hash.Hex(),
		"masternodes", len(masternodes),
		"source", "parents")
	x.epochSwitches.Add(hash, info)
	return info, nil
}

// Broadcast sends a message to the BFT channel
func (x *XDPoS_v2) broadcastToBftChannel(msg interface{}) {
	go func() {
		x.BroadcastCh <- msg
	}()
}

// getSyncInfo returns current sync info
func (x *XDPoS_v2) getSyncInfo() *types.SyncInfo {
	return &types.SyncInfo{
		HighestQuorumCert:  x.highestQuorumCert,
		HighestTimeoutCert: x.highestTimeoutCert,
	}
}

// setNewRound sets a new round
func (x *XDPoS_v2) setNewRound(chain consensus.ChainReader, round types.Round) {
	log.Info("[setNewRound] new round", "round", round)
	x.currentRound = round
	x.timeoutCount = 0
	x.timeoutWorker.Reset(chain, uint64(x.currentRound), uint64(x.highestQuorumCert.ProposedBlockInfo.Round))
	x.timeoutPool.Clear()

	select {
	case x.newRoundCh <- round:
	default:
	}
}

// periodicJob runs periodic maintenance
func (x *XDPoS_v2) periodicJob() {
	go func() {
		ticker := time.NewTicker(PeriodicJobPeriod * time.Second)
		defer ticker.Stop()
		for {
			<-ticker.C
			x.hygieneVotePool()
			x.hygieneTimeoutPool()
		}
	}()
}

// allowedToSend checks if this node can send consensus messages
func (x *XDPoS_v2) allowedToSend(chain consensus.ChainReader, header *types.Header, sendType string) bool {
	x.signLock.RLock()
	signer := x.signer
	x.signLock.RUnlock()

	masternodes := x.GetMasternodes(chain, header)
	for _, mn := range masternodes {
		if signer == mn {
			log.Debug("[allowedToSend] Yes, allowed", "sendType", sendType, "signer", signer)
			return true
		}
	}
	log.Debug("[allowedToSend] Not in masternode list", "sendType", sendType, "signer", signer)
	return false
}

// GetLatestCommittedBlockInfo returns the highest committed block.
// May legitimately return nil before the 3-chain rule has committed anything
// yet (see the zero-value init in newXDPoS_v2 / ReinitBFT) — callers must
// nil-check. Read under x.lock since commitBlocks() writes highestCommitBlock
// under the same lock (via ProposedBlockHandler/VoteHandler/SyncInfoHandler).
func (x *XDPoS_v2) GetLatestCommittedBlockInfo() *types.BlockInfo {
	x.lock.RLock()
	defer x.lock.RUnlock()
	return x.highestCommitBlock
}

// FindParentBlockToAssign finds the parent block for mining
func (x *XDPoS_v2) FindParentBlockToAssign(chain consensus.ChainReader) *types.Block {
	// Read highestQuorumCert under the same lock that guards its writers
	// (ProposedBlockHandler, VoteHandler, SyncInfoHandler, Initial, ReinitBFT),
	// matching the Prepare() read pattern above.
	x.lock.RLock()
	qc := x.highestQuorumCert
	x.lock.RUnlock()

	// #1368: the miner's update loop can fire before the V2 QC state is
	// populated (e.g. right after a restart, before Initial/ProposedBlockHandler
	// has run). qc, qc.ProposedBlockInfo, and qc.ProposedBlockInfo.Number were
	// all being dereferenced unguarded here, panicking the whole process. The
	// caller (miner.commitNewWork) already tolerates a nil return by falling
	// back to the canonical head, so just skip this mint attempt instead.
	if qc == nil || qc.ProposedBlockInfo == nil || qc.ProposedBlockInfo.Number == nil {
		log.Debug("[FindParentBlockToAssign] highestQuorumCert not yet available; skipping this mint attempt")
		return nil
	}

	parent := chain.GetBlock(qc.ProposedBlockInfo.Hash, qc.ProposedBlockInfo.Number.Uint64())
	if parent == nil {
		log.Error("[FindParentBlockToAssign] Parent not found",
			"hash", qc.ProposedBlockInfo.Hash,
			"number", qc.ProposedBlockInfo.Number)
	}
	return parent
}

// Utility functions

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

// GetMasternodesByHash retrieves the masternode list for a given block hash.
// Used by external callers (RPC, hooks) for block-specific masternode queries.
// Matches v2.6.8 engines/engine_v2/engine.go.
func (x *XDPoS_v2) GetMasternodesByHash(chain consensus.ChainReader, hash common.Hash) []common.Address {
	header := chain.GetHeaderByHash(hash)
	if header == nil {
		return nil
	}
	return x.GetMasternodes(chain, header)
}

// GetPreviousPenaltyByHash returns the penalties from the epoch switch block
// that is `limit` epochs before the epoch containing `hash`.
// Matches v2.6.8 engines/engine_v2/engine.go.
//
// Fast-sync safety: during header-only (fast/snap) sync the epoch-switch block
// header may not yet be present in the DB even though its hash is known via the
// EpochSwitchInfo cache.  When GetHeaderByHash returns nil we fall back to the
// Penalties slice already embedded in EpochSwitchInfo — it was populated from
// the same header.Penalties bytes at the time the epoch info was computed (see
// getEpochSwitchInfoInner).  This is identical data, not an approximation, so
// full-sync consensus correctness is unaffected.
func (x *XDPoS_v2) GetPreviousPenaltyByHash(chain consensus.ChainReader, hash common.Hash, limit int) []common.Address {
	epochSwitchInfo, err := x.getPreviousEpochSwitchInfoByHash(chain, hash, limit)
	if err != nil {
		log.Error("[GetPreviousPenaltyByHash] getPreviousEpochSwitchInfoByHash error", "err", err)
		return []common.Address{}
	}
	header := chain.GetHeaderByHash(epochSwitchInfo.EpochSwitchBlockInfo.Hash)
	if header == nil {
		// Header not yet in DB (fast/snap sync): use the Penalties already cached
		// in EpochSwitchInfo rather than dereferencing a nil header.
		// Fixes: nil-pointer panic (SIGSEGV addr=0x258) at engine.go:GetPreviousPenaltyByHash
		// when HookPenalty's comeback mechanic fires during fast-sync header verification.
		log.Warn("[GetPreviousPenaltyByHash] epoch-switch header not in DB, using cached penalties",
			"epochSwitchHash", epochSwitchInfo.EpochSwitchBlockInfo.Hash.Hex(),
			"epochSwitchNumber", epochSwitchInfo.EpochSwitchBlockInfo.Number,
			"cachedPenalties", len(epochSwitchInfo.Penalties))
		return epochSwitchInfo.Penalties
	}
	return contracts.ExtractAddressFromBytes(header.Penalties)
}

// extractAddressesFromBytes converts a packed byte array of 20-byte addresses
// into a slice of common.Address values.
func extractAddressesFromBytes(byteAddresses []byte) []common.Address {
	if len(byteAddresses) == 0 || len(byteAddresses)%common.AddressLength != 0 {
		return nil
	}
	addrs := make([]common.Address, len(byteAddresses)/common.AddressLength)
	for i := 0; i < len(addrs); i++ {
		copy(addrs[i][:], byteAddresses[i*common.AddressLength:(i+1)*common.AddressLength])
	}
	return addrs
}

// InvalidateCaches purges every in-memory cache that may contain entries
// referencing blocks above newHead. Implements the consensus.CacheInvalidator
// interface; called by BlockChain.SetHead after a rewind.
//
// Why this is necessary (refs #802 task 3, #801): the V2 engine's caches
// (snapshots, epochSwitches, verifiedHeaders, round2epochBlockInfo,
// signatures) are keyed by block hash. After a rewind, blocks above newHead
// are removed from the chain but their hashes can still be returned by
// in-memory walks done during verifyHeader on the post-rewind canonical
// chain — specifically calcMasternodes and getEpochSwitchInfo, which both
// recurse through parents and may hit the LRU. Result: stale masternode
// set computed for the canonical block, "not its turn" rejection, sync
// stuck. Observed live on xdc04 snapfullpath-test after a 6,784-block
// rollback that exhausted the rollback budget.
//
// The LRU values for unrelated blocks (those still on the canonical chain
// below newHead) get re-fetched on next access. The cost of full purge is
// a few cache misses on subsequent header verifications — cheaper than
// the alternative of selective invalidation per-cache, which would require
// walking each entry, looking up its block number, and comparing. Purge
// is correct, simple, infrequent (only on SetHead).
func (x *XDPoS_v2) InvalidateCaches(newHead uint64) {
	x.lock.Lock()
	defer x.lock.Unlock()
	x.snapshots.Purge()
	x.signatures.Purge()
	x.epochSwitches.Purge()
	x.verifiedHeaders.Purge()
	x.round2epochBlockInfo.Purge()
	log.Warn("[XDPoS_v2] caches invalidated after chain rewind (refs #802 task 3)",
		"newHead", newHead)
}
