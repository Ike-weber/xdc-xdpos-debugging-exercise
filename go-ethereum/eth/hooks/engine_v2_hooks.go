// Copyright (c) 2018 XDPoSChain
// Ported to go-ethereum for XDC compatibility

package hooks

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	commonmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/util"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Beneficiary enumerates the reward tier a signer belongs to under the tiered
// (TIPUpgradeReward / XDPoSChain v2.8.0) reward model. Ported verbatim from
// XDPoSChain eth/hooks/engine_v2_hooks.go.
type Beneficiary int

const (
	MasterNodeBeneficiary Beneficiary = iota
	ProtectorNodeBeneficiary
	ObserverNodeBeneficiary
)

// AttachConsensusV2Hooks attaches V2 consensus hooks to XDPoS engine.
//
// Wires HookPenalty and HookReward on adaptor.EngineV2, matching the shape of
// XDPoSChain v2.6.8 eth/hooks/engine_v2_hooks.go. HookPenalty determines which
// masternodes are penalised at each epoch switch; without it calcMasternodes
// naively truncates the candidate pool and produces wrong validator sets at any
// epoch switch where some top-N candidates are penalised (A.80 root cause).
func AttachConsensusV2Hooks(adaptor *XDPoS.XDPoS, bc *core.BlockChain, chainConfig *params.ChainConfig) {
	// Hook calculates reward for masternodes at epoch boundaries
	adaptor.HookReward = func(chain consensus.ChainHeaderReader, stateBlock *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error) {
		number := header.Number.Uint64()
		rCheckpoint := chainConfig.XDPoS.RewardCheckpoint
		if rCheckpoint == 0 {
			rCheckpoint = chainConfig.XDPoS.Epoch
		}
		foundationWalletAddr := chainConfig.XDPoS.FoudationWalletAddr
		if foundationWalletAddr == (common.Address{}) {
			log.Error("Foundation Wallet Address is empty", "error", foundationWalletAddr)
			return nil, errors.New("foundation wallet address is empty")
		}
		rewards := make(map[string]interface{})

		// Skip reward if this is the first v2 block — aligned with v2.6.8
		// Guard: V2 config may be nil for pre-v2 only networks or early sync
		if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil {
			if number == chainConfig.XDPoS.V2.SwitchBlock.Uint64()+1 {
				return rewards, nil
			}
		}

		if number > 0 && number > rCheckpoint && foundationWalletAddr != (common.Address{}) {
			start := time.Now()

			// TIERED REWARD GATE (XDPoSChain v2.8.0 / TIPUpgradeReward).
			//
			// FALSE branch (below): the EXISTING flat-reward path, byte-identical
			// to pre-Stage-C behaviour. For apothem, TIPUpgradeRewardBlock is nil
			// (disabled), so this gate is always FALSE and the live path is the
			// one already in production.
			//
			// TRUE branch (here): the tiered masternode/protector/observer path
			// ported verbatim from the reference HookReward else-branch. DEAD CODE
			// until the fork value is flipped.
			if chainConfig.IsTIPUpgradeReward(header.Number) {
				// Round drives the V2 config selection and epoch numbering.
				// Our EngineV2 has NO Config(round) method (unlike the reference
				// adaptor.EngineV2.Config); use chainConfig.XDPoS.V2.Config(round).
				round, rerr := adaptor.EngineV2.GetRoundNumber(header)
				if rerr != nil {
					log.Error("[HookReward] Fail to get round", "error", rerr)
					return nil, rerr
				}
				currentConfig := chainConfig.XDPoS.V2.Config(uint64(round))
				epochNum := chainConfig.XDPoS.V2.SwitchEpoch + uint64(round)/chainConfig.XDPoS.Epoch

				// Get per-tier signers + burned fees for the rewarded epoch.
				tierSigners, burnedInOneEpoch, terr := GetSigningTxCountTiered(adaptor, bc, header, chainConfig, parentState, currentConfig)
				if terr != nil {
					log.Error("[HookReward] Fail to get tiered signers count", "error", terr)
					return nil, terr
				}
				rewards["signers"] = tierSigners[MasterNodeBeneficiary]
				rewards["signersProtector"] = tierSigners[ProtectorNodeBeneficiary]
				rewards["signersObserver"] = tierSigners[ObserverNodeBeneficiary]

				rewardSum := new(big.Int)
				type rewardWithType struct {
					r   float64
					t   Beneficiary
					key string
				}
				for _, rwt := range []rewardWithType{
					{currentConfig.MasternodeReward, MasterNodeBeneficiary, "rewards"},
					{currentConfig.ProtectorReward, ProtectorNodeBeneficiary, "rewardsProtector"},
					{currentConfig.ObserverReward, ObserverNodeBeneficiary, "rewardsObserver"},
				} {
					// VERBATIM big.Float truncation path (consensus-load-bearing):
					// originalReward = floor(reward * Ether) computed in big.Float,
					// NOT big.Int math. Do not "simplify" to big.Int multiplication.
					originalRewardFloat := new(big.Float).Mul(new(big.Float).SetFloat64(rwt.r), new(big.Float).SetUint64(params.Ether))
					originalReward, _ := originalRewardFloat.Int(nil)
					// chainConfig.XDCBlocksPerYear() (NOT common.BlocksPerYear) to
					// match our existing path; equal for apothem (Period=2).
					chainReward := util.RewardInflation(chain, originalReward, number, chainConfig.XDCBlocksPerYear())
					rewardSigners, cerr := XDPoS.CalculateRewardForSignerFixed(chainReward, tierSigners[rwt.t])
					if cerr != nil {
						log.Error("[HookReward] Fail to calculate tiered reward", "error", cerr, "type", rwt.t)
						return nil, cerr
					}
					// Add reward for coin holders.
					rewardResults := make(map[common.Address]interface{})
					for signer, calcReward := range rewardSigners {
						holderRewards, herr := XDPoS.GetRewardBalancesRateUpgrade(chainConfig, foundationWalletAddr, parentState, signer, calcReward, header.Number)
						if herr != nil {
							log.Error("[HookReward] Fail to calculate reward for holders.", "error", herr)
							return nil, herr
						}
						if len(holderRewards) > 0 {
							for holder, reward := range holderRewards {
								// Match our existing path: uint256.MustFromBig +
								// tracing.BalanceChangeUnspecified.
								stateBlock.AddBalance(holder, uint256.MustFromBig(reward), tracing.BalanceChangeUnspecified)
								rewardSum.Add(rewardSum, reward)
							}
						}
						rewardResults[signer] = holderRewards
					}
					rewards[rwt.key] = rewardResults
				}

				// Record the total minted/burned into state db (MintedRecord
				// bookkeeping). Ported verbatim from the reference HookReward
				// else-branch.
				totalMinted := new(big.Int)
				totalBurned := new(big.Int)

				nonce := stateBlock.GetNonce(common.MintedRecordAddressBinary)
				if nonce == 0 {
					// initialize MintedRecordAddress
					stateBlock.PutMintedRecordOnsetEpoch(common.Uint64ToHash(epochNum))
					stateBlock.PutMintedRecordOnsetBlock(common.Uint64ToHash(number))
				} else {
					epochNumIter := epochNum
					for epochNumIter > 0 {
						epochNumIter--
						totalMinted = stateBlock.GetPostMinted(epochNumIter).Big()
						totalBurned = stateBlock.GetPostBurned(epochNumIter).Big()
						if totalMinted.Sign() != 0 || totalBurned.Sign() != 0 {
							// if previous epoch has non-zero total minted or burned, break
							break
						}
					}
				}
				totalMinted.Add(totalMinted, rewardSum)
				if totalMinted.Cmp(commonmath.MaxBig256) > 0 {
					totalMinted.Set(commonmath.MaxBig256)
					log.Warn("[HookReward] total minted overflow max u256")
				}
				log.Debug("[HookReward] total minted in hook", "value", totalMinted)
				stateBlock.PutPostMinted(epochNum, common.BigToHash(totalMinted))
				stateBlock.PutPostRewardBlock(epochNum, common.Uint64ToHash(number))
				// Record total burned into statedb
				totalBurned.Add(totalBurned, burnedInOneEpoch)
				if totalBurned.Cmp(commonmath.MaxBig256) > 0 {
					totalBurned.Set(commonmath.MaxBig256)
					log.Warn("[HookReward] total burned overflow max u256")
				}
				stateBlock.PutPostBurned(epochNum, common.BigToHash(totalBurned))
				// Increment nonce so that statedb does not treat it as empty account
				stateBlock.IncrementMintedRecordNonce()

				log.Debug("Time Calculated HookReward (tiered)", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
				return rewards, nil
			}

			// Get chain reward with inflation
			chainReward := new(big.Int).Mul(new(big.Int).SetUint64(chainConfig.XDPoS.Reward), new(big.Int).SetUint64(params.Ether))
			// Pass `chain` (not nil) so RewardInflation can consult
			// chain.Config().IsTIPNoHalvingMNReward and skip the halving
			// branches post-fork. Passing nil silently bypassed the guard
			// and halved reward at BlocksPerYear*2 (= Apothem block
			// 31,536,000), even though TIPNoHalvingMNReward was active
			// since block 23,779,191. Canonical XDPoSChain v2.6.8
			// eth/hooks/engine_v1_hooks.go:272 passes `chain` likewise.
			//
			// Issue #694 §H: BlocksPerYear is chain-relative (depends on
			// XDPoS.Period). chainConfig.XDCBlocksPerYear() derives the
			// correct value from Period; falls back to common.BlocksPerYear
			// when Period isn't configured. For Apothem/Mainnet (Period=2)
			// the result is 15,768,000 — same as the constant. Differs for
			// chains with non-2 Period.
			chainReward = util.RewardInflation(chain, chainReward, number, chainConfig.XDCBlocksPerYear())

			// Pick the signer-counting strategy based on V2 switch block.
			// V2 blocks count signers from BlockSigner contract (signing
			// transactions submitted by V2 masternodes). V1 / pre-V2 blocks
			// have no such transactions — instead, signers are recovered by
			// walking the prior epoch's blocks and ecrecover'ing seals.
			// Issue #599: without this branch the V1 case silently produced
			// `signers={}` and the entire per-epoch 5000-XDC issuance was
			// dropped on V1 chains.
			//
			// Edge cases:
			//   - V2 disabled (SwitchBlock high enough never to trigger):
			//     isV2 stays false → V1 branch always.
			//   - V2 from genesis (SwitchBlock = 0, devnet 551):
			//     number ≥ 0 is always true → V2 branch always.
			//   - Hybrid (mainnet 50 @ 80,370,000; Apothem 51 @ 56,828,700):
			//     branch flips at the switch block.
			isV2 := chainConfig.XDPoS.V2 != nil &&
				chainConfig.XDPoS.V2.SwitchBlock != nil &&
				number >= chainConfig.XDPoS.V2.SwitchBlock.Uint64()

			totalSigner := new(uint64)
			var signers map[common.Address]*XDPoS.RewardLog
			var err error
			if isV2 {
				signers, err = GetSigningTxCount(adaptor, bc, header, chainConfig, totalSigner)
			} else {
				signers, err = adaptor.GetRewardForCheckpoint(bc, header, rCheckpoint, totalSigner)
			}

			log.Debug("Time Get Signers", "block", header.Number.Uint64(), "mode", map[bool]string{true: "v2", false: "v1"}[isV2], "time", common.PrettyDuration(time.Since(start)))
			if err != nil {
				log.Crit("Fail to get signers for reward checkpoint", "error", err)
			}
			rewards["signers"] = signers

			rewardSigners, err := XDPoS.CalculateRewardForSigner(chainReward, signers, *totalSigner)
			if err != nil {
				log.Crit("Fail to calculate reward for signers", "error", err)
			}

			// Add reward for coin holders
			voterResults := make(map[common.Address]interface{})
			if len(signers) > 0 {
				for signer, calcReward := range rewardSigners {
					// Use parentState for reading owner/voter info (matches v2.6.8)
					holderRewards, err := XDPoS.GetRewardBalancesRate(foundationWalletAddr, parentState, signer, calcReward, number)
					if err != nil {
						log.Crit("Fail to calculate reward for holders.", "error", err)
					}
					if len(holderRewards) > 0 {
						for holder, reward := range holderRewards {
							// v2.6.8 uses AddBalance without tracing reason
							rewardU256 := uint256.MustFromBig(reward)
							log.Info("HookReward: AddBalance", "block", number, "holder", holder.Hex(), "reward", reward.String(), "rewardU256", rewardU256.String(), "prevBalance", stateBlock.GetBalance(holder).String())
							stateBlock.AddBalance(holder, rewardU256, tracing.BalanceChangeUnspecified)
							log.Info("HookReward: AddBalance done", "block", number, "holder", holder.Hex(), "newBalance", stateBlock.GetBalance(holder).String())
						}
					}
					voterResults[signer] = holderRewards
				}
			}
			rewards["rewards"] = voterResults
			log.Debug("Time Calculated HookReward", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
		}
		return rewards, nil
	}

	// Wire V2 engine hooks: HookReward and HookPenalty.
	// Both are called by EngineV2.Finalize / calcMasternodes on epoch-switch blocks.
	if adaptor.EngineV2 != nil {
		adaptor.EngineV2.SetHookReward(func(chain consensus.ChainReader, state *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error) {
			return adaptor.HookReward(chain, state, parentState, header)
		})

		// HookPenalty: identifies masternodes that failed to sign enough blocks in
		// the epoch and removes them from the active set at the next epoch switch.
		// Without this, calcMasternodes takes the first maxMasternodes candidates
		// naively, producing wrong sets whenever penalised nodes appear in the top-N.
		// Ported from XDPoSChain v2.6.8 eth/hooks/engine_v2_hooks.go:39-249.
		adaptor.EngineV2.SetHookPenalty(func(chain consensus.ChainReader, number *big.Int, currentHash common.Hash, candidates []common.Address) ([]common.Address, error) {
			start := time.Now()
			// Walk the epoch collecting per-coinbase block counts and block hashes.
			listBlockHash := []common.Hash{currentHash}
			statMiners := make(map[common.Address]int)
			parentNumber := number.Uint64() - 1
			parentHash := currentHash

			// Resolve round from the latest block header (with wait-for-disk retry).
			var round types.Round
			for timeout := 0; ; timeout++ {
				parentHeader := chain.GetHeader(parentHash, parentNumber)
				if parentHeader != nil {
					r, err := adaptor.EngineV2.GetRoundNumber(parentHeader)
					if err != nil {
						log.Error("[HookPenalty] failed to get round", "err", err)
						return nil, err
					}
					round = r
					break
				}
				log.Info("[HookPenalty] parentHeader nil, waiting for disk write", "parentNumber", parentNumber)
				time.Sleep(time.Second)
				if timeout > 30 {
					return []common.Address{}, errors.New("[HookPenalty] parentHeader still nil after 30s")
				}
			}

			// Walk epoch backwards until previous epoch switch, counting coinbase hits.
			for {
				parentHeader := chain.GetHeader(parentHash, parentNumber)
				if parentHeader == nil {
					break
				}
				isEpochSwitch, _, err := adaptor.EngineV2.IsEpochSwitch(parentHeader)
				if err != nil {
					return []common.Address{}, err
				}
				if isEpochSwitch {
					break
				}
				miner := parentHeader.Coinbase
				statMiners[miner]++
				parentNumber--
				parentHash = parentHeader.ParentHash
				listBlockHash = append(listBlockHash, parentHash)
			}

			currentConfig := chainConfig.XDPoS.V2.Config(uint64(round))
			preMasternodes := adaptor.EngineV2.GetMasternodesByHash(chain, currentHash)
			penalties := []common.Address{}

			minimumBlocks := common.MinimunMinerBlockPerEpoch
			if chainConfig.IsTIPUpgradePenalty(number) {
				minimumBlocks = currentConfig.MinimumMinerBlockPerEpoch
			}
			for miner, total := range statMiners {
				if total < minimumBlocks {
					log.Info("[HookPenalty] insufficient blocks", "addr", miner.Hex(), "total", total, "require", minimumBlocks)
					penalties = append(penalties, miner)
				}
			}
			for _, addr := range preMasternodes {
				if _, exists := statMiners[addr]; !exists {
					log.Info("[HookPenalty] no blocks mined", "addr", addr.Hex())
					penalties = append(penalties, addr)
				}
			}

			// Comeback mechanic: penalised candidates that signed recent blocks rejoin.
			// Only active after comebackHeight to avoid reading V1 blocks.
			if !chainConfig.IsTIPUpgradePenalty(number) {
				comebackHeight := (common.LimitPenaltyEpochV2+1)*chainConfig.XDPoS.Epoch + chainConfig.XDPoS.V2.SwitchBlock.Uint64()
				if number.Uint64() > comebackHeight {
					pens := adaptor.EngineV2.GetPreviousPenaltyByHash(chain, currentHash, common.LimitPenaltyEpochV2)
					penComebacks := []common.Address{}
					for _, p := range pens {
						for _, addr := range candidates {
							if p == addr {
								penComebacks = append(penComebacks, p)
								break
							}
						}
					}
					// Check each comeback candidate's recent signing activity.
					mapBlockHash := map[common.Hash]bool{}
					startRange := common.RangeReturnSigner - 1
					if startRange >= len(listBlockHash) {
						startRange = len(listBlockHash) - 1
					}
					for i := startRange; i >= 0; i-- {
						if len(penComebacks) == 0 {
							break
						}
						blockNumber := number.Uint64() - uint64(i) - 1
						bhash := listBlockHash[i]
						if blockNumber%uint64(XDPoS.MergeSignRange) == 0 {
							mapBlockHash[bhash] = true
						}
						signingTxs, ok := adaptor.GetCachedSigningTxs(bhash)
						if !ok {
							block := rawdb.ReadBlock(adaptor.GetDb(), bhash, blockNumber)
							if block != nil {
								signingTxs = adaptor.CacheSigner(bhash, block.Transactions())
							}
						}
						signer := types.MakeSigner(chainConfig, big.NewInt(int64(blockNumber)), 0)
						for _, tx := range signingTxs {
							if len(tx.Data()) < 36 {
								continue
							}
							blkHash := common.BytesToHash(tx.Data()[len(tx.Data())-32:])
							from, err := types.Sender(signer, tx)
							if err != nil {
								continue
							}
							if mapBlockHash[blkHash] {
								for j, addr := range penComebacks {
									if from == addr {
										penComebacks = append(penComebacks[:j], penComebacks[j+1:]...)
										break
									}
								}
							}
						}
					}
					for _, comeback := range penComebacks {
						already := false
						for _, p := range penalties {
							if p == comeback {
								already = true
								break
							}
						}
						if !already {
							penalties = append(penalties, comeback)
						}
					}
				}
			}

			for i, p := range penalties {
				log.Info("[HookPenalty] final penalty", "i", i, "addr", p.Hex())
			}
			log.Info("[HookPenalty] done", "block", number, "elapsed", common.PrettyDuration(time.Since(start)))
			return penalties, nil
		})
	}
}

// resolveMissedSigningTxs is the V2 cache-miss fallback used by
// GetSigningTxCount's walk-back loop whenever GetCachedSigningTxs has no
// entry for a block hash — i.e. this process never body-validated that
// block. That is always true, for every block already on disk, immediately
// after a restart (core/block_validator.go is the cache's only populating
// call site), so this path is not a rare corner case: it is the common case
// on any node that isn't a from-genesis replay.
//
// GetSigningTxCount runs when its caller's `isV2` predicate holds, which is
// `number >= V2.SwitchBlock` (inclusive — see the isV2 assignment earlier in
// this file). Note this is deliberately NOT the same boundary as
// XDPoS.IsV2Block, which is `Number.Cmp(SwitchBlock) > 0` (exclusive), so at
// exactly number == V2.SwitchBlock the two disagree and the reward window can
// still reach back into pre-switch (V1-era) blocks. CacheSigner is still the
// right filter there: it selects on the BlockSigner method signature, and the
// arbiter's own V2 hook is likewise presence-only (CacheSigningTxs, no receipt
// consultation) for every checkpoint it handles. The receipt-consulting
// variant is the arbiter's V1-only path and is always fed DERIVED receipts,
// never rawdb.ReadRawReceipts. The >= / > boundary mismatch against the
// arbiter is tracked separately; do not rely on this comment to conclude the
// V1 window is unreachable, because at the switch block it is not.
//
// This used to branch on chainConfig.IsTIPSigning(h.Number) and, on the
// false side, read rawdb.ReadRawReceipts and gate on receipt status
// (CacheData). Three things made that always wrong here:
//  1. IsTIPSigning tests the package-global common.TIPSigning, which
//     CopyXDCConstants (common/types.go) defaults to block 3,000,000 for any
//     unregistered chainId, so an unregistered chain always took the
//     receipt-consulting branch.
//  2. rawdb.ReadRawReceipts decodes ReceiptForStorage (core/rawdb/accessors_chain.go),
//     which carries no TxHash — it is a derived field, structurally absent
//     from storage.
//  3. CacheData's TxHash match against the real tx hash could therefore never
//     succeed, and the unmatched default collided with
//     types.ReceiptStatusFailed (both zero), so CacheData silently dropped
//     every signing tx as "failed" -> totalSigners=0 for the whole reward
//     window -> the reward hook credits nobody -> the post-state equals the
//     parent state -> permanent "invalid merkle root".
//
// Match the arbiter (XDPoSChain eth/hooks/engine_v2_hooks.go
// CacheSigningTxs) and erigon-xdc (consensus/xdpos/reward.go CacheSigner):
// presence/method-signature only, unconditionally, no receipt consultation.
// Issue #1364.
//
// Returns the signing txs, the total tx count in the block (for logging),
// and whether the block was found at all (false only when rawdb has no
// block for hash/number, e.g. a pruned or not-yet-written block).
func resolveMissedSigningTxs(c *XDPoS.XDPoS, hash common.Hash, number uint64) (signingTxs []*types.Transaction, totalTxsInBlock int, found bool) {
	block := rawdb.ReadBlock(c.GetDb(), hash, number)
	if block == nil {
		return nil, 0, false
	}
	totalTxsInBlock = len(block.Transactions())
	signingTxs = c.CacheSigner(hash, block.Transactions())
	return signingTxs, totalTxsInBlock, true
}

// GetSigningTxCount gets signing transaction sender count for reward calculation.
// Uses V1 fixed-window logic for V1 blocks, V2 epoch-switch walk for V2 blocks.
func GetSigningTxCount(c *XDPoS.XDPoS, chain *core.BlockChain, header *types.Header, chainConfig *params.ChainConfig, totalSigner *uint64) (map[common.Address]*XDPoS.RewardLog, error) {
	number := header.Number.Uint64()
	signers := make(map[common.Address]*XDPoS.RewardLog)
	if number == 0 {
		return signers, nil
	}

	rCheckpoint := chainConfig.XDPoS.RewardCheckpoint
	if rCheckpoint == 0 {
		rCheckpoint = chainConfig.XDPoS.Epoch
	}

	// For V1 blocks (number <= V2.SwitchBlock), use original V1 fixed-window logic
	var (
		startBlockNumber, endBlockNumber uint64
		masternodes                      []common.Address
		data                             = make(map[common.Hash][]common.Address)
		mapBlkHash                       = make(map[uint64]common.Hash)
	)

	// Check if this is a V1 block
	isV1Block := true
	if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil {
		if number > chainConfig.XDPoS.V2.SwitchBlock.Uint64() {
			isV1Block = false
		}
	}

	if isV1Block {
		// V1 logic: fixed window
		prevCheckpoint := number - (rCheckpoint * 2)
		startBlockNumber = prevCheckpoint + 1
		endBlockNumber = startBlockNumber + rCheckpoint - 1

		// V2 switch boundary guard
		var switchBlock uint64
		if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil {
			switchBlock = chainConfig.XDPoS.V2.SwitchBlock.Uint64()
		}
		if switchBlock > 0 && startBlockNumber <= switchBlock && number > switchBlock {
			startBlockNumber = switchBlock + 2
			if startBlockNumber > endBlockNumber {
				return signers, nil
			}
			log.Info("GetSigningTxCount: V2 boundary clamp applied", "originalStart", prevCheckpoint+1, "clampedStart", startBlockNumber, "switchBlock", switchBlock)
		}

		// Masternodes come from the checkpoint that *opened* the reward window —
		// prevCheckpoint = number - 2*rCheckpoint, NOT number - rCheckpoint.
		// V1 stores the *next* epoch's masternodes in each checkpoint's extraData,
		// so block 900 holds the masternodes for epoch 901..1800, not for the
		// epoch (1..900) we're rewarding at block 1800. Canonical:
		// XDPoSChain contracts/utils.go:357 — chain.GetHeader(parent, prevCheckpoint).
		// For block 1800: prevCheckpoint = 0 (genesis); for block 2700: prevCheckpoint = 900.
		checkpointHeader := chain.GetHeaderByNumber(prevCheckpoint)
		if checkpointHeader != nil {
			masternodes = c.GetMasternodesFromCheckpointHeader(checkpointHeader, prevCheckpoint, chainConfig.XDPoS.Epoch)
		}
	} else {
		// V2 logic: epoch-switch walk
		rewardEpochCount, signEpochCount := 2, 1
		switchBlockPlusOne := uint64(0)
		if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil {
			switchBlockPlusOne = chainConfig.XDPoS.V2.SwitchBlock.Uint64() + 1
		}

		h := header
		epochCount := 0
		for i := number - 1; ; i-- {
			parentHash := h.ParentHash
			h = chain.GetHeader(parentHash, i)
			if h == nil {
				return nil, fmt.Errorf("GetSigningTxCount: missing header at %d (%x)", i, parentHash)
			}

			isEpochSwitch, _, err := c.IsEpochSwitch(h)
			if err != nil {
				return nil, err
			}
			if isEpochSwitch && i != switchBlockPlusOne {
				epochCount++
				if epochCount == signEpochCount {
					endBlockNumber = h.Number.Uint64() - 1
				}
				if epochCount == rewardEpochCount {
					startBlockNumber = h.Number.Uint64() + 1
					// Dispatcher routes V1 headers to header.Extra parse and V2
					// headers to GetMasternodesFromEpochSwitchHeader. Required for
					// the first V2 reward block, whose 2-epoch walk-back lands on
					// a V1 checkpoint. Canonical: XDPoSChain XDPoS.go:449.
					masternodes = c.GetMasternodesFromCheckpointHeader(h, h.Number.Uint64(), chainConfig.XDPoS.Epoch)
					break
				}
			}
			if i == 0 {
				break
			}
		}
	}

	log.Info("GetSigningTxCount starting", "currentBlock", number, "scanFrom", startBlockNumber, "scanTo", endBlockNumber, "isV1", isV1Block)

	// Walk backwards from header collecting signing txs.
	// Start at number-1 (parent of current block, always available) and walk
	// backwards to startBlockNumber. The signing txs for block N are in block N+1,
	// so we need to scan blocks startBlockNumber..endBlockNumber+1.
	h := header
	for i := number - 1; i >= startBlockNumber; i-- {
		if h == nil {
			log.Error("GetSigningTxCount: header is nil at start of loop", "number", i)
			break
		}
		h = chain.GetHeader(h.ParentHash, i)
		if h == nil {
			log.Error("GetSigningTxCount: header not found", "number", i)
			break
		}
		if h.Hash() == (common.Hash{}) {
			log.Error("GetSigningTxCount: empty header hash", "number", i)
			break
		}
		mapBlkHash[i] = h.Hash()

		signingTxs, ok := c.GetCachedSigningTxs(h.Hash())
		cacheHit := ok
		var totalTxsInBlock int
		if !ok {
			var found bool
			signingTxs, totalTxsInBlock, found = resolveMissedSigningTxs(c, h.Hash(), i)
			if !found {
				log.Warn("[V2-SIG] block not found in rawdb", "number", i, "hash", h.Hash())
			}
		}
		// V2-SIG diagnostic: log EVERY block in V2 era so we can see whether
		// blocks like 3616, 3631 have signing txs (potentially ~108 each).
		// Limit to V2 era (after switchBlock) to keep V1 logs sane.
		if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil &&
			i > chainConfig.XDPoS.V2.SwitchBlock.Uint64() && i <= endBlockNumber+200 {
			// Also log the FIRST referenced block hash so we can see WHAT is being signed.
			var firstRef string
			for _, tx := range signingTxs {
				if len(tx.Data()) >= 36 {
					firstRef = common.BytesToHash(tx.Data()[len(tx.Data())-32:]).Hex()[:10]
					break
				}
			}
			log.Warn("[V2-SIG]", "block", i, "totalTxs", totalTxsInBlock,
				"signingTxs", len(signingTxs), "firstRef", firstRef, "cacheHit", cacheHit, "hash", h.Hash().Hex()[:10])
		}
		// Use historical signer for the block being scanned
		signer := types.MakeSigner(chainConfig, h.Number, h.Time)
		for _, tx := range signingTxs {
			if len(tx.Data()) >= 36 {
				blkHash := common.BytesToHash(tx.Data()[len(tx.Data())-32:])
				from, err := types.Sender(signer, tx)
				if err != nil {
					log.Warn("[V2-SIG-RECOVER-FAIL]", "block", i, "txHash", tx.Hash().Hex()[:10], "err", err)
					continue
				}
				data[blkHash] = append(data[blkHash], from)
			} else if len(tx.Data()) > 0 {
				log.Warn("[V2-SIG-DATA-SHORT]", "block", i, "dataLen", len(tx.Data()), "txHash", tx.Hash().Hex()[:10])
			}
		}

		if i == 0 {
			break
		}
	}

	// V2-SIG diagnostic: dump the data map size and how many of the count-loop
	// blocks (multiples of MergeSignRange in window) actually have signing-tx
	// references in the data map. If 0/55, signing txs reference blocks NOT
	// matching the filter — that's the real bug.
	if !isV1Block {
		dataMapSize := len(data)
		matchedBlocks := 0
		for i := startBlockNumber; i <= endBlockNumber; i++ {
			if i%common.MergeSignRange == 0 {
				if len(data[mapBlkHash[i]]) > 0 {
					matchedBlocks++
				}
			}
		}
		// Count total signing-tx-referenced unique block hashes
		log.Warn("[V2-SIG-SUMMARY]",
			"startBlock", startBlockNumber, "endBlock", endBlockNumber,
			"data_map_entries", dataMapSize,
			"countloop_filter_matches", matchedBlocks,
			"total_filter_blocks", (endBlockNumber-startBlockNumber)/common.MergeSignRange+1,
			"masternodes_len", len(masternodes))
		// Log each missed filter block so we can identify exactly which 4 are absent
		for i := startBlockNumber; i <= endBlockNumber; i++ {
			if i%common.MergeSignRange == 0 {
				h := mapBlkHash[i]
				addrCount := len(data[h])
				if addrCount == 0 {
					log.Warn("[V2-SIG-MISS]", "filterBlock", i, "mapHash", h.Hex()[:10], "hashKnown", h != (common.Hash{}))
				} else {
					log.Warn("[V2-SIG-HIT]", "filterBlock", i, "mapHash", h.Hex()[:10], "addrCount", addrCount)
				}
			}
		}
	}

	mnDiagPrinted := false
	for i := startBlockNumber; i <= endBlockNumber; i++ {
		if i%common.MergeSignRange == 0 || !chainConfig.IsTIP2019(big.NewInt(int64(i))) {
			addrs := data[mapBlkHash[i]]
			if len(addrs) == 0 {
				continue
			}
			// V2-MN-DIAG: on first matched block, dump all masternodes + first 5
			// addrs so we can compare. This identifies whether:
			//   (a) masternodes is empty/garbage
			//   (b) masternodes has V1 addrs that don't match V2 signers
			if !isV1Block && !mnDiagPrinted {
				mnDiagPrinted = true
				mnHexes := make([]string, 0, len(masternodes))
				for _, mn := range masternodes {
					mnHexes = append(mnHexes, mn.Hex())
				}
				addrHexes := make([]string, 0, 5)
				for k, a := range addrs {
					if k >= 5 {
						break
					}
					addrHexes = append(addrHexes, a.Hex())
				}
				log.Warn("[V2-MN-DIAG]", "block", i, "mnCount", len(masternodes),
					"addrsCount", len(addrs), "masternodes", mnHexes, "addrsSample", addrHexes)
			}
			seen := make(map[common.Address]bool)
			for _, mn := range masternodes {
				for _, addr := range addrs {
					if addr == mn && !seen[addr] {
						seen[addr] = true
						break
					}
				}
			}
			if len(addrs) > 0 && len(seen) == 0 {
				log.Warn("[V2-SIG-MISMATCH]", "block", i, "addrs", len(addrs), "masternodes", len(masternodes), "matched", 0)
			}
			for addr := range seen {
				if rl, ok := signers[addr]; ok {
					rl.Sign++
				} else {
					signers[addr] = &XDPoS.RewardLog{Sign: 1, Reward: new(big.Int)}
				}
				*totalSigner++
			}
		}
	}

	if *totalSigner == 0 && len(masternodes) > 0 {
		// A live masternode set that produced zero credited signers means the
		// reward window's signing txs were not found by this process (stale
		// signer cache after a restart, or a read-path regression) -- the
		// post-state will equal the parent state and this checkpoint block
		// will be rejected with "invalid merkle root" downstream. That is
		// never a legitimate outcome while masternodes exist, so surface it
		// here rather than only as a merkle mismatch several log lines away.
		// Issue #1364.
		log.Error("V2 reward checkpoint computed zero signers with a nonempty masternode set",
			"startBlock", startBlockNumber, "endBlock", endBlockNumber, "masternodes_len", len(masternodes))
	}
	log.Info("Calculate reward at checkpoint", "startBlock", startBlockNumber, "endBlock", endBlockNumber, "totalSigners", *totalSigner, "uniqueSigners", len(signers))
	return signers, nil
}

// GetSigningTxCountTiered is the tiered (TIPUpgradeReward / XDPoSChain v2.8.0)
// variant of GetSigningTxCount. It returns the per-tier signing-count maps plus
// the total fees burned during the rewarded epoch.
//
// Ported from XDPoSChain eth/hooks/engine_v2_hooks.go GetSigningTxCount: it does
// the same 2-epoch walk-back to find the reward window, accumulates burned fees
// for the first epoch encountered, builds the masternode tier from the reward
// checkpoint header, and (always, on this path) builds the protector/observer
// tiers from the candidate set in parentState — sorted by stake DESC, with
// penalised nodes and masternodes filtered out, taking up to MaxProtectorNodes
// then MaxObserverNodes.
//
// Adaptations vs the reference (forced by our codebase shapes):
//   - Signer recovery uses types.MakeSigner/types.Sender (ecrecover) and our
//     cache/rawdb read mechanics, exactly as our GetSigningTxCount does, rather
//     than the reference's tx.From() (our txs are not pre-populated with From).
//   - GetMasternodesFromCheckpointHeader takes (header, number, epoch) in our
//     fork; reference takes (header). We pass h.Number.Uint64() and the epoch.
//   - state.GetCandidates / state.GetCandidateCap are FREE functions in our
//     state pkg; reference calls them as parentState methods.
//   - Candidate zero-check uses `!= (common.Address{})`; our common.Address has
//     no IsZero() method (reference uses candidate.IsZero()).
//
// DEAD CODE until TIPUpgradeReward is enabled (apothem TIPUpgradeRewardBlock is
// nil = disabled), so it cannot affect live behaviour.
func GetSigningTxCountTiered(c *XDPoS.XDPoS, chain *core.BlockChain, header *types.Header, chainConfig *params.ChainConfig, parentState *state.StateDB, currentConfig *params.V2Config) (map[Beneficiary]map[common.Address]*XDPoS.RewardLog, *big.Int, error) {
	number := header.Number.Uint64()
	rewardEpochCount := 2
	signEpochCount := 1

	signers := make(map[Beneficiary]map[common.Address]*XDPoS.RewardLog)
	signers[MasterNodeBeneficiary] = make(map[common.Address]*XDPoS.RewardLog)
	signers[ProtectorNodeBeneficiary] = make(map[common.Address]*XDPoS.RewardLog)
	signers[ObserverNodeBeneficiary] = make(map[common.Address]*XDPoS.RewardLog)

	mapBlkHash := map[uint64]common.Hash{}
	burnedInOneEpoch := new(big.Int)

	// prevent overflow
	if number == 0 {
		return signers, burnedInOneEpoch, nil
	}

	data := make(map[common.Hash][]common.Address)
	epochCount := 0
	var startBlockNumber, endBlockNumber uint64

	nodesToKeep := make(map[Beneficiary][]common.Address)

	switchBlockPlusOne := uint64(0)
	if chainConfig.XDPoS.V2 != nil && chainConfig.XDPoS.V2.SwitchBlock != nil {
		switchBlockPlusOne = chainConfig.XDPoS.V2.SwitchBlock.Uint64() + 1
	}

	h := header
	for i := number - 1; ; i-- {
		parentHash := h.ParentHash
		h = chain.GetHeader(parentHash, i)
		if h == nil {
			log.Error("[GetSigningTxCountTiered] fail to get header", "number", i, "hash", parentHash)
			return nil, burnedInOneEpoch, fmt.Errorf("fail to get header in GetSigningTxCountTiered at number: %v, hash: %v", i, parentHash)
		}
		if epochCount == 0 && h.BaseFee != nil {
			// add burned for the first epoch during loop
			burnedInOneEpoch.Add(burnedInOneEpoch, new(big.Int).Mul(h.BaseFee, new(big.Int).SetUint64(h.GasUsed)))
		}
		isEpochSwitch, _, err := c.IsEpochSwitch(h)
		if err != nil {
			return nil, burnedInOneEpoch, err
		}
		if isEpochSwitch && i != switchBlockPlusOne {
			epochCount += 1
			if epochCount == signEpochCount {
				endBlockNumber = h.Number.Uint64() - 1
			}
			if epochCount == rewardEpochCount {
				startBlockNumber = h.Number.Uint64() + 1
				nodesToKeep[MasterNodeBeneficiary] = c.GetMasternodesFromCheckpointHeader(h, h.Number.Uint64(), chainConfig.XDPoS.Epoch)
				// In reward upgrade, add protector and observer nodes. This path is
				// only reached when IsTIPUpgradeReward is true (the caller gates it),
				// so we always build the tiers here.
				candidates := state.GetCandidates(parentState)
				var ms []utils.Masternode
				for _, candidate := range candidates {
					// ignore the zero address "0x0000...0000"
					if candidate != (common.Address{}) {
						v := state.GetCandidateCap(parentState, candidate)
						ms = append(ms, utils.Masternode{Address: candidate, Stake: v})
					}
				}
				slices.SortStableFunc(ms, func(a, b utils.Masternode) int {
					return b.Stake.Cmp(a.Stake)
				})
				// find penalty and filter them out (penalties ∪ masternodes)
				penalties := common.ExtractAddressFromBytes(h.Penalties)
				filterMap := make(map[common.Address]struct{})
				for _, addr := range penalties {
					filterMap[addr] = struct{}{}
				}
				for _, addr := range nodesToKeep[MasterNodeBeneficiary] {
					filterMap[addr] = struct{}{}
				}
				// find top candidates: MaxProtectorNodes, then MaxObserverNodes.
				// NOTE: our V2Config field is MaxObserverNodes (correct spelling);
				// the XDPoSChain reference has a typo `MaxObverserNodes`. Same value.
				protector := []common.Address{}
				observer := []common.Address{}
				for _, node := range ms {
					if _, ok := filterMap[node.Address]; ok {
						continue
					}
					if len(protector) < currentConfig.MaxProtectorNodes {
						protector = append(protector, node.Address)
					} else if len(observer) < currentConfig.MaxObserverNodes {
						observer = append(observer, node.Address)
					}
				}
				nodesToKeep[ProtectorNodeBeneficiary] = protector
				nodesToKeep[ObserverNodeBeneficiary] = observer
				break
			}
		}
		mapBlkHash[i] = h.Hash()
		signingTxs, ok := c.GetCachedSigningTxs(h.Hash())
		if !ok {
			block := rawdb.ReadBlock(c.GetDb(), h.Hash(), i)
			if block != nil {
				if chainConfig.IsTIPSigning(h.Number) {
					signingTxs = c.CacheSigner(h.Hash(), block.Transactions())
				} else {
					receipts := rawdb.ReadRawReceipts(c.GetDb(), h.Hash(), i)
					signingTxs = c.CacheData(h, block.Transactions(), receipts)
				}
			}
		}
		signer := types.MakeSigner(chainConfig, h.Number, h.Time)
		for _, tx := range signingTxs {
			if len(tx.Data()) < 36 {
				continue
			}
			blkHash := common.BytesToHash(tx.Data()[len(tx.Data())-32:])
			from, err := types.Sender(signer, tx)
			if err != nil {
				continue
			}
			data[blkHash] = append(data[blkHash], from)
		}
		// prevent overflow
		if i == 0 {
			return signers, burnedInOneEpoch, nil
		}
	}

	for i := startBlockNumber; i <= endBlockNumber; i++ {
		if i%common.MergeSignRange == 0 {
			addrs := data[mapBlkHash[i]]
			// Filter duplicate address.
			if len(addrs) > 0 {
				addrSigners := make(map[Beneficiary]map[common.Address]bool)
				addrSigners[MasterNodeBeneficiary] = make(map[common.Address]bool)
				addrSigners[ProtectorNodeBeneficiary] = make(map[common.Address]bool)
				addrSigners[ObserverNodeBeneficiary] = make(map[common.Address]bool)

				for _, addr := range addrs {
					for _, beneficiary := range []Beneficiary{MasterNodeBeneficiary, ProtectorNodeBeneficiary, ObserverNodeBeneficiary} {
						if _, ok := nodesToKeep[beneficiary]; ok {
							for _, kept := range nodesToKeep[beneficiary] {
								if addr == kept {
									if _, ok := addrSigners[beneficiary][addr]; !ok {
										addrSigners[beneficiary][addr] = true
									}
									break
								}
							}
						}
					}
				}

				for _, beneficiary := range []Beneficiary{MasterNodeBeneficiary, ProtectorNodeBeneficiary, ObserverNodeBeneficiary} {
					for addr := range addrSigners[beneficiary] {
						_, exist := signers[beneficiary][addr]
						if exist {
							signers[beneficiary][addr].Sign++
						} else {
							signers[beneficiary][addr] = &XDPoS.RewardLog{Sign: 1, Reward: new(big.Int)}
						}
					}
				}
			}
		}
	}

	log.Info("Calculate tiered reward at checkpoint", "startBlock", startBlockNumber, "endBlock", endBlockNumber)
	return signers, burnedInOneEpoch, nil
}
