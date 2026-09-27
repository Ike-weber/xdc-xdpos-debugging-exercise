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

package XDPoS

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Note: RewardMasterPercent, RewardVoterPercent, RewardFoundationPercent are defined in constants.go

// RewardLog stores signing count and reward for a signer
type RewardLog struct {
	Sign   uint64
	Reward *big.Int
}

// BlockReader provides access to block headers for reward calculation.
type BlockReader interface {
	consensus.ChainHeaderReader
}

// isSigningTx checks if a transaction is a block signing transaction.
// Canonical guard (XDPoSChain core/types/transaction.go:519): requires EXACTLY 68 bytes
// (4-byte method sig + 32-byte blockNumber + 32-byte blockHash). A looser >=4 check
// would count malformed txs that canonical rejects, diverging signer counts and stateRoot.
func isSigningTx(tx *types.Transaction) bool {
	if tx == nil || tx.To() == nil {
		return false
	}
	// Must be EXACTLY 68 bytes: 4 (method) + 32 (uint256 blockNumber) + 32 (bytes32 blockHash)
	if *tx.To() == common.BlockSignersBinary && len(tx.Data()) == 68 {
		methodSig := common.Bytes2Hex(tx.Data()[:4])
		if methodSig == common.HexSignMethod {
			return true
		}
	}
	return false
}

// GetRewardForCheckpoint calculates the signing rewards for the checkpoint epoch.
// Mirrors v2.6.8's contracts.GetRewardForCheckpoint logic exactly:
// - Walks backwards by parent hash
// - Uses CacheSigner/CacheData for signing tx extraction
// - Uses MergeSignRange and IsTIP2019 filtering
// - Counts signers against the masternode list from the epoch header
func (c *XDPoS) GetRewardForCheckpoint(
	chain BlockReader,
	header *types.Header,
	rCheckpoint uint64,
	totalSigner *uint64,
) (map[common.Address]*RewardLog, error) {
	number := header.Number.Uint64()

	// v2.6.8 formula
	prevCheckpoint := number - (rCheckpoint * 2)
	startBlockNumber := prevCheckpoint + 1
	endBlockNumber := startBlockNumber + rCheckpoint - 1
	signers := make(map[common.Address]*RewardLog)
	mapBlkHash := map[uint64]common.Hash{}

	// Collect signing data: blockHash -> list of senders
	data := make(map[common.Hash][]common.Address)

	// Walk backwards from current header by parent hash (matching v2.6.8)
	h := header
	for i := prevCheckpoint + (rCheckpoint * 2) - 1; i >= startBlockNumber; i-- {
		h = chain.GetHeader(h.ParentHash, i)
		if h == nil {
			log.Error("GetRewardForCheckpoint: header not found", "number", i)
			break
		}
		mapBlkHash[i] = h.Hash()

		// ALWAYS read from DB for reward calculations to ensure deterministic results.
		// The cache (CacheSigner/CacheData) may have incomplete data during sync,
		// leading to non-deterministic signing TX counts and state root mismatches.
		// Cache is designed for P2P protocol, not for consensus-critical reward computation.
		var signingTxs []*types.Transaction

		// Use type assertion to get GetBlock from chain (like v2.6.8)
		fullChain, hasGetBlock := chain.(interface {
			GetBlock(hash common.Hash, number uint64) *types.Block
		})
		// Try chain.GetBlock first, fall back to rawdb
		var block *types.Block
		if hasGetBlock {
			block = fullChain.GetBlock(h.Hash(), i)
		}
		if block == nil {
			block = rawdb.ReadBlock(c.db, h.Hash(), i)
		}
		if block == nil {
			// Last resort: read body separately
			body := rawdb.ReadBody(c.db, h.Hash(), i)
			if body != nil {
				block = types.NewBlockWithHeader(h).WithBody(*body)
			}
		}
		if block != nil {
			txs := block.Transactions()
			if i <= 5 || (i >= 898 && i <= 902) {
				log.Info("Reward scan block (DB)", "number", i, "txs", len(txs))
			}
			// Extract signing TXs directly from block, bypassing cache.
			// Use chain.Config().IsTIPSigning (mirrors canonical XDPoSChain contracts/utils.go:339)
			// instead of the stale package-global TIPSigning which CopyXDCConstants never updates.
			if chain.Config().IsTIPSigning(h.Number) {
				// Post-TIPSigning: signing txs are valid by presence only (CacheSigner path)
				for _, tx := range txs {
					if isSigningTx(tx) {
						signingTxs = append(signingTxs, tx)
					}
				}
			} else {
				receipts := rawdb.ReadRawReceipts(c.db, h.Hash(), i)
				// Pre-TIPSigning: signing txs must have a successful receipt (CacheData path).
				// Match receipts by positional index (idx); canonical uses TxHash match
				// (XDPoSChain XDPoS.go:500-528) but idx is equivalent for non-reorged blocks.
				for idx, tx := range txs {
					if isSigningTx(tx) && idx < len(receipts) && receipts[idx].Status == types.ReceiptStatusSuccessful {
						signingTxs = append(signingTxs, tx)
					}
				}
			}
			if len(signingTxs) > 0 && i <= 10 {
				log.Info("Found signing txs from DB!", "number", i, "count", len(signingTxs))
			}
		} else {
			if i <= 5 {
				log.Info("Block NOT found anywhere", "number", i)
			}
		}

		for _, tx := range signingTxs {
			if len(tx.Data()) >= 36 {
				blkHash := common.BytesToHash(tx.Data()[len(tx.Data())-32:])
				// Use historical signer for the block being scanned, not latest signer
				signer := types.MakeSigner(chain.Config(), h.Number, h.Time)
				from, err := types.Sender(signer, tx)
				if err != nil {
					continue
				}
				data[blkHash] = append(data[blkHash], from)
			}
		}
	}

	// Get masternodes from the previous checkpoint header (walk to prevCheckpoint)
	if h != nil && h.Number.Uint64() > prevCheckpoint {
		h = chain.GetHeader(h.ParentHash, prevCheckpoint)
	}
	if h == nil {
		h = chain.GetHeaderByNumber(prevCheckpoint)
	}
	if h == nil {
		log.Error("GetRewardForCheckpoint: checkpoint header not found", "number", prevCheckpoint)
		return signers, nil
	}
	masternodes := c.GetMasternodesFromCheckpointHeader(h, prevCheckpoint, c.config.Epoch)

	log.Info("Reward checkpoint scan complete",
		"checkpoint", number, "startBlock", startBlockNumber, "endBlock", endBlockNumber,
		"masternodes", len(masternodes), "signingBlocks", len(data))

	// Count signatures per signer (matching v2.6.8 logic)
	for i := startBlockNumber; i <= endBlockNumber; i++ {
		// v2.6.8: only count blocks at MergeSignRange intervals OR if pre-TIP2019.
		// Use chain.Config().IsTIP2019 to mirror canonical XDPoSChain contracts/utils.go:356
		// instead of the stale package-global TIP2019Block.
		if i%common.MergeSignRange == 0 || !chain.Config().IsTIP2019(big.NewInt(int64(i))) {
			addrs := data[mapBlkHash[i]]
			if len(addrs) > 0 {
				addrSigners := make(map[common.Address]bool)
				for _, masternode := range masternodes {
					for _, addr := range addrs {
						if addr == masternode {
							if _, ok := addrSigners[addr]; !ok {
								addrSigners[addr] = true
							}
							break
						}
					}
				}
				for addr := range addrSigners {
					_, exist := signers[addr]
					if exist {
						signers[addr].Sign++
					} else {
						signers[addr] = &RewardLog{1, new(big.Int)}
					}
					*totalSigner++
				}
			}
		}
	}

	log.Info("Calculate reward at checkpoint", "startBlock", startBlockNumber, "endBlock", endBlockNumber,
		"totalSigners", *totalSigner, "uniqueSigners", len(signers))
	return signers, nil
}

// CalculateRewardForSigner calculates the reward amount for each signer.
// Uses v2.6.8 calculation order: (chainReward / totalSigner) * sign
func CalculateRewardForSigner(
	chainReward *big.Int,
	signers map[common.Address]*RewardLog,
	totalSigner uint64,
) (map[common.Address]*big.Int, error) {
	resultSigners := make(map[common.Address]*big.Int)
	if totalSigner > 0 {
		for signer, rLog := range signers {
			calcReward := new(big.Int)
			calcReward.Div(chainReward, new(big.Int).SetUint64(totalSigner))
			calcReward.Mul(calcReward, new(big.Int).SetUint64(rLog.Sign))
			rLog.Reward = calcReward
			resultSigners[signer] = calcReward
		}
	}
	log.Info("Signers data", "totalSigner", totalSigner, "totalReward", chainReward)
	return resultSigners, nil
}

// CalculateRewardForSignerFixed gives every signer the FULL chainReward (a
// per-capita fixed reward), regardless of how many blocks it signed.
//
// This is the tiered-reward (TIPUpgradeReward / XDPoSChain v2.8.0) variant: in
// the upgrade model each beneficiary in a tier earns the same fixed amount, so
// there is no proportional division across signers as in CalculateRewardForSigner.
//
// Verbatim semantics of XDPoSChain v2.8.0 eth/hooks/engine_v2_hooks.go
// CalculateRewardForSignerFixed: deep-copy chainReward into each signer's
// reward (new(big.Int).Set) and write rLog.Reward.
//
// DEAD CODE until TIPUpgradeReward is enabled (apothem TIPUpgradeRewardBlock is
// nil = disabled), so it cannot affect live behaviour.
func CalculateRewardForSignerFixed(
	chainReward *big.Int,
	signers map[common.Address]*RewardLog,
) (map[common.Address]*big.Int, error) {
	resultSigners := make(map[common.Address]*big.Int)
	// Add reward for signers.
	for signer, rLog := range signers {
		// Add reward for signer: full chainReward, deep-copied.
		calcReward := new(big.Int).Set(chainReward)
		rLog.Reward = calcReward
		resultSigners[signer] = calcReward
	}

	log.Info("Signers data (fixed)", "percapitaReward", chainReward)
	for addr, signer := range signers {
		log.Debug("Signer reward (fixed)", "signer", addr, "sign", signer.Sign, "reward", signer.Reward)
	}

	return resultSigners, nil
}

// GetRewardBalancesRateUpgrade mirrors the canonical v2.8.0 contracts.GetRewardBalancesRate
// (XDPoSChain contracts/utils.go GetRewardBalancesRate) used on the tiered
// (TIPUpgradeReward) path.
//
// Differences vs the legacy GetRewardBalancesRate above (DO NOT MODIFY that one):
//   - NO owner-skip: the owner share (90%) is ALWAYS credited, even when the
//     owner resolves to the foundation wallet. (The #186 / #1800 owner-skip is a
//     pre-upgrade V1 quirk; the upgrade path drops it to match canonical v2.8.)
//   - Foundation gate is fork-aware: when chainConfig.IsTIPUpgradeReward(blockNumber)
//     is true AND the foundation already has a balance entry, ACCUMULATE; otherwise
//     ASSIGN. (Canonical contracts/utils.go GetRewardBalancesRate.)
//
// The voter loop is copied verbatim from the legacy GetRewardBalancesRate for
// fidelity. RewardVoterPercent is 0, so the loop is a no-op in practice, but the
// structure is preserved bit-for-bit.
//
// DEAD CODE until TIPUpgradeReward is enabled (apothem TIPUpgradeRewardBlock is
// nil = disabled).
func GetRewardBalancesRateUpgrade(chainConfig *params.ChainConfig, foundationWalletAddr common.Address, statedb *state.StateDB, masterAddr common.Address, totalReward *big.Int, blockNumber *big.Int) (map[common.Address]*big.Int, error) {
	owner := state.GetCandidateOwner(statedb, masterAddr)
	balances := make(map[common.Address]*big.Int)

	rewardMaster := new(big.Int).Mul(totalReward, new(big.Int).SetInt64(int64(RewardMasterPercent)))
	rewardMaster = new(big.Int).Div(rewardMaster, new(big.Int).SetInt64(100))
	// Canonical v2.8 upgrade path: ALWAYS credit the owner share (no #186 owner-skip).
	balances[owner] = rewardMaster

	// Get voters for masternode (verbatim from GetRewardBalancesRate; no-op while
	// RewardVoterPercent == 0, kept for fidelity).
	voters := state.GetVoters(statedb, masterAddr)
	if len(voters) > 0 {
		totalVoterReward := new(big.Int).Mul(totalReward, new(big.Int).SetUint64(uint64(RewardVoterPercent)))
		totalVoterReward = new(big.Int).Div(totalVoterReward, new(big.Int).SetUint64(100))
		totalCap := new(big.Int)
		voterCaps := make(map[common.Address]*big.Int)
		for _, voteAddr := range voters {
			if _, ok := voterCaps[voteAddr]; ok && TIP2019Block.Uint64() <= blockNumber.Uint64() {
				continue
			}
			voterCap := state.GetVoterCap(statedb, masterAddr, voteAddr)
			totalCap.Add(totalCap, voterCap)
			voterCaps[voteAddr] = voterCap
		}
		if totalCap.Cmp(new(big.Int).SetInt64(0)) > 0 {
			for addr, voteCap := range voterCaps {
				if voteCap.Cmp(new(big.Int).SetInt64(0)) > 0 {
					rcap := new(big.Int).Mul(totalVoterReward, voteCap)
					rcap = new(big.Int).Div(rcap, totalCap)
					if balances[addr] != nil {
						balances[addr].Add(balances[addr], rcap)
					} else {
						balances[addr] = rcap
					}
				}
			}
		}
	}

	foundationReward := new(big.Int).Mul(totalReward, new(big.Int).SetInt64(int64(RewardFoundationPercent)))
	foundationReward = new(big.Int).Div(foundationReward, new(big.Int).SetInt64(100))

	// Canonical v2.8 fork-aware foundation gate: accumulate only when the upgrade
	// fork is active AND the foundation already has an entry; otherwise assign.
	if chainConfig != nil && chainConfig.IsTIPUpgradeReward(blockNumber) && balances[foundationWalletAddr] != nil {
		balances[foundationWalletAddr].Add(balances[foundationWalletAddr], foundationReward)
	} else {
		balances[foundationWalletAddr] = foundationReward
	}

	return balances, nil
}

// CalculateRewardForHolders distributes the signer's reward among the masternode owner and voters.
// - Owner gets RewardMasterPercent (90%)
// - Voters share RewardVoterPercent (0% currently)
// - Foundation gets RewardFoundationPercent (10%) - included in returned map for correct ordering
// NOTE: Matches v2.6.8 behavior - does NOT fallback to signer if owner is zero address.
// If GetCandidateOwner returns zero address, rewards go to zero address (same as v2.6.8).
func CalculateRewardForHolders(
	foundationWallet common.Address,
	statedb *state.StateDB,
	signer common.Address,
	calcReward *big.Int,
	blockNumber uint64,
) map[common.Address]*big.Int {
	balances := make(map[common.Address]*big.Int)

	if calcReward == nil || calcReward.Sign() <= 0 {
		return balances
	}

	// Get the owner of this masternode (v2.6.8 does NOT fallback to signer)
	owner := state.GetCandidateOwner(statedb, signer)
	log.Info("CalculateRewardForHolders: owner lookup",
		"signer", signer.Hex(),
		"owner", owner.Hex(),
		"isZero", owner == (common.Address{}))
	// NOTE: Intentionally NOT falling back to signer if owner is zero address
	// This matches v2.6.8 behavior exactly

	// Calculate owner portion (90% of the signer's reward)
	rewardMaster := new(big.Int).Mul(calcReward, big.NewInt(RewardMasterPercent))
	rewardMaster.Div(rewardMaster, big.NewInt(100))
	balances[owner] = rewardMaster
	log.Info("CalculateRewardForHolders: owner reward",
		"owner", owner.Hex(),
		"reward", rewardMaster.String())

	// Voter rewards are 0% currently, infrastructure kept for future
	if RewardVoterPercent > 0 {
		voters := state.GetVoters(statedb, signer)
		if len(voters) > 0 {
			totalVoterReward := new(big.Int).Mul(calcReward, big.NewInt(RewardVoterPercent))
			totalVoterReward.Div(totalVoterReward, big.NewInt(100))

			totalCap := big.NewInt(0)
			voterCaps := make(map[common.Address]*big.Int)

			for _, voter := range voters {
				if _, exists := voterCaps[voter]; exists {
					continue
				}
				voterCap := state.GetVoterCap(statedb, signer, voter)
				if voterCap.Sign() > 0 {
					totalCap.Add(totalCap, voterCap)
					voterCaps[voter] = voterCap
				}
			}

			if totalCap.Sign() > 0 {
				for voter, voterCap := range voterCaps {
					reward := new(big.Int).Mul(totalVoterReward, voterCap)
					reward.Div(reward, totalCap)

					if balances[voter] != nil {
						balances[voter].Add(balances[voter], reward)
					} else {
						balances[voter] = reward
					}
				}
			}
		}
	}

	// Include foundation reward in the balances map to match v2.6.8 behavior
	// v2.6.8 calls AddBalance for foundation within the same loop as other holders
	if foundationWallet != (common.Address{}) {
		foundationReward := new(big.Int).Mul(calcReward, big.NewInt(RewardFoundationPercent))
		foundationReward.Div(foundationReward, big.NewInt(100))
		balances[foundationWallet] = foundationReward
	}

	return balances
}

// GetRewardBalancesRate calculates reward distribution for a masternode:
// 90% to owner, 0% proportional to voters, 10% to foundation
// This is the exact v2.6.8 implementation for state root compatibility
func GetRewardBalancesRate(foundationWalletAddr common.Address, statedb *state.StateDB, masterAddr common.Address, totalReward *big.Int, blockNumber uint64) (map[common.Address]*big.Int, error) {
	owner := state.GetCandidateOwner(statedb, masterAddr)
	balances := make(map[common.Address]*big.Int)

	rewardMaster := new(big.Int).Mul(totalReward, new(big.Int).SetInt64(int64(RewardMasterPercent)))
	rewardMaster = new(big.Int).Div(rewardMaster, new(big.Int).SetInt64(100))
	// XDC parity fix #1800 + #186: skip the 90% owner share whenever the
	// OWNER resolves to the foundation wallet. Canonical V1 burns this share.
	//
	// Two cases both reach this path:
	//
	//  Case A (devnet-5151 #165/#1800): the SIGNER itself is the foundation
	//  wallet. GetCandidateOwner(foundation) returns foundation, so owner ==
	//  foundationWalletAddr. Old condition `masterAddr != foundationWalletAddr`
	//  caught this — both forms now equivalent here.
	//
	//  Case B (apothem mainnet #186/#83049946): a non-foundation signer
	//  (e.g. 0xa65010026b83368Ca05DF6E8B467985D6de3eAC5) has its
	//  candidate-owner registered as the foundation wallet. The old
	//  `masterAddr != foundationWalletAddr` check let this through,
	//  producing double-credit (90% owner share + 10% foundation cut =
	//  100% to foundation) and BAD BLOCK invalid merkle root.
	//
	// Testing `owner` directly subsumes both cases.
	// Verified for #1800 via V1 archive `debug_getModifiedAccountsByNumber(1799,1800)`
	// — only 4 accounts modified, none correspond to foundation's owner-share sink.
	if owner != foundationWalletAddr {
		balances[owner] = rewardMaster
	}

	// Get voters for masternode
	voters := state.GetVoters(statedb, masterAddr)
	if len(voters) > 0 {
		totalVoterReward := new(big.Int).Mul(totalReward, new(big.Int).SetUint64(uint64(RewardVoterPercent)))
		totalVoterReward = new(big.Int).Div(totalVoterReward, new(big.Int).SetUint64(100))
		totalCap := new(big.Int)
		voterCaps := make(map[common.Address]*big.Int)
		for _, voteAddr := range voters {
			if _, ok := voterCaps[voteAddr]; ok && TIP2019Block.Uint64() <= blockNumber {
				continue
			}
			voterCap := state.GetVoterCap(statedb, masterAddr, voteAddr)
			totalCap.Add(totalCap, voterCap)
			voterCaps[voteAddr] = voterCap
		}
		if totalCap.Cmp(new(big.Int).SetInt64(0)) > 0 {
			for addr, voteCap := range voterCaps {
				if voteCap.Cmp(new(big.Int).SetInt64(0)) > 0 {
					rcap := new(big.Int).Mul(totalVoterReward, voteCap)
					rcap = new(big.Int).Div(rcap, totalCap)
					if balances[addr] != nil {
						balances[addr].Add(balances[addr], rcap)
					} else {
						balances[addr] = rcap
					}
				}
			}
		}
	}

	foundationReward := new(big.Int).Mul(totalReward, new(big.Int).SetInt64(int64(RewardFoundationPercent)))
	foundationReward = new(big.Int).Div(foundationReward, new(big.Int).SetInt64(100))

	// XDC: must accumulate, not overwrite. The foundation wallet may already
	// have a non-zero entry in `balances` from the voter-reward loop above
	// (when the foundation is itself a voter for this signer). Overwriting
	// drops that voter share. Canonical XDPoSChain v2.6.8
	// (contracts/utils.go:467-474) accumulates; we previously assigned,
	// producing a silent under-payment to the foundation whenever it voted
	// for the signer being rewarded. First observed as a state-root mismatch
	// at Apothem block 31,536,000 (epoch boundary 35,040) with ~28,375 XDC
	// accumulated under-payment over ~430 epochs.
	if balances[foundationWalletAddr] != nil {
		balances[foundationWalletAddr].Add(balances[foundationWalletAddr], foundationReward)
	} else {
		balances[foundationWalletAddr] = foundationReward
	}

	return balances, nil
}

// ApplyRewards distributes rewards at checkpoint blocks.
func (c *XDPoS) ApplyRewards(
	chain BlockReader,
	statedb *state.StateDB,
	parentState *state.StateDB,
	header *types.Header,
) (map[string]interface{}, error) {
	rewards := make(map[string]interface{})
	number := header.Number.Uint64()

	rCheckpoint := c.config.RewardCheckpoint
	if rCheckpoint == 0 {
		rCheckpoint = c.config.Epoch
	}

	foundationWallet := c.config.FoudationWalletAddr
	if foundationWallet == (common.Address{}) {
		log.Error("Foundation wallet address is empty")
		return rewards, nil
	}

	// Only calculate rewards starting from second checkpoint (block 1800 for rCheckpoint=900)
	if number <= rCheckpoint {
		log.Debug("Skipping rewards - at or before first checkpoint", "number", number)
		return rewards, nil
	}

	// Must be above rCheckpoint*2 to have a previous checkpoint
	if number < rCheckpoint*2 {
		log.Debug("Skipping rewards - before second checkpoint", "number", number)
		return rewards, nil
	}

	// Get the chain reward with inflation/halving
	chainReward := new(big.Int).Mul(
		new(big.Int).SetUint64(c.config.Reward),
		big.NewInt(1e18),
	)
	// Apply halving: /2 at BlocksPerYear*2, /4 at BlocksPerYear*5, stops after TIPNoHalving
	blocksPerYear := uint64(15768000)
	if chain.Config().IsTIPNoHalvingMNReward(header.Number) {
		// After TIP: no halving, use full reward
	} else if blocksPerYear*5 <= number {
		chainReward.Div(chainReward, new(big.Int).SetUint64(4))
	} else if blocksPerYear*2 <= number {
		chainReward.Div(chainReward, new(big.Int).SetUint64(2))
	}

	// Get signers for this checkpoint
	var totalSigner uint64
	signers, err := c.GetRewardForCheckpoint(chain, header, rCheckpoint, &totalSigner)
	if err != nil {
		log.Error("Failed to get reward checkpoint", "err", err)
		return rewards, err
	}

	if totalSigner == 0 {
		log.Warn("No signers found for reward calculation", "number", number)
		return rewards, nil
	}

	// Calculate rewards per signer
	signerRewards, _ := CalculateRewardForSigner(chainReward, signers, totalSigner)

	// Use parentState for reading voter/owner info if available
	readState := parentState
	if readState == nil {
		readState = statedb
	}

	voterResults := make(map[common.Address]interface{})
	totalDistributed := big.NewInt(0)

	if len(signerRewards) > 0 {
		for signer, signerReward := range signerRewards {
			// Use GetRewardBalancesRate which matches v2.6.8 exactly:
			// - Processes voters even when RewardVoterPercent=0
			// - No zero-owner fallback
			// - Foundation reward uses add-to-existing pattern
			holderRewards, err := GetRewardBalancesRate(foundationWallet, readState, signer, signerReward, number)
			if err != nil {
				log.Error("Failed to calculate reward for holders", "error", err)
				continue
			}
			if len(holderRewards) > 0 {
				for holder, reward := range holderRewards {
					// Must call AddBalance even for zero rewards to match v2.6.8 behavior.
					// v2.6.8 does NOT skip zero-value AddBalance calls. Skipping them
					// causes EIP-158 state trie differences (account touch vs no-touch).
					rewardU256, _ := uint256.FromBig(reward)
					statedb.AddBalance(holder, rewardU256, tracing.BalanceIncreaseRewardMineBlock)
					if reward.Sign() > 0 {
						totalDistributed.Add(totalDistributed, reward)
					}
				}
			}
			voterResults[signer] = holderRewards
		}

		log.Info("Rewards distributed",
			"block", number,
			"totalSigners", totalSigner,
			"uniqueSigners", len(signers),
			"totalDistributed", totalDistributed.String())
	}

	rewards["signers"] = signers
	rewards["rewards"] = voterResults
	rewards["totalDistributed"] = totalDistributed.String()

	return rewards, nil
}

// CreateDefaultHookReward creates the reward hook function.
func (c *XDPoS) CreateDefaultHookReward() func(chain consensus.ChainHeaderReader, stateBlock *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error) {
	return func(chain consensus.ChainHeaderReader, stateBlock *state.StateDB, parentState *state.StateDB, header *types.Header) (map[string]interface{}, error) {
		return c.ApplyRewards(chain, stateBlock, parentState, header)
	}
}
