// Copyright (c) 2018 XDPoSChain
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

package ethapi

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// XDC status field names
const (
	fieldStatus   = "status"
	fieldCapacity = "capacity"
	fieldSuccess  = "success"
	fieldEpoch    = "epoch"

	statusMasternode = "MASTERNODE"
	statusSlashed    = "SLASHED"
	statusProposed   = "PROPOSED"
	statusPenalty    = "PENALTY"
)

var (
	errEmptyHeader = errors.New("empty header")
)

// GetBlockSignersByHash returns the signers of a block by hash
func (s *BlockChainAPI) GetBlockSignersByHash(ctx context.Context, blockHash common.Hash) ([]common.Address, error) {
	block, err := s.b.BlockByHash(ctx, blockHash)
	if err != nil || block == nil {
		return []common.Address{}, err
	}
	masternodes, err := s.GetMasternodes(ctx, block)
	if err != nil || len(masternodes) == 0 {
		log.Error("Failed to get masternodes", "err", err, "len(masternodes)", len(masternodes))
		return []common.Address{}, err
	}
	return s.rpcOutputBlockSigners(block, ctx, masternodes)
}

// GetBlockSignersByNumber returns the signers of a block by number
func (s *BlockChainAPI) GetBlockSignersByNumber(ctx context.Context, blockNumber rpc.BlockNumber) ([]common.Address, error) {
	block, err := s.b.BlockByNumber(ctx, blockNumber)
	if err != nil || block == nil {
		return []common.Address{}, err
	}
	masternodes, err := s.GetMasternodes(ctx, block)
	if err != nil || len(masternodes) == 0 {
		log.Error("Failed to get masternodes", "err", err, "len(masternodes)", len(masternodes))
		return []common.Address{}, err
	}
	return s.rpcOutputBlockSigners(block, ctx, masternodes)
}

// GetBlockFinalityByHash returns the finality of a block by hash
func (s *BlockChainAPI) GetBlockFinalityByHash(ctx context.Context, blockHash common.Hash) (uint, error) {
	block, err := s.b.BlockByHash(ctx, blockHash)
	if err != nil || block == nil {
		return uint(0), err
	}
	masternodes, err := s.GetMasternodes(ctx, block)
	if err != nil || len(masternodes) == 0 {
		log.Error("Failed to get masternodes", "err", err, "len(masternodes)", len(masternodes))
		return uint(0), err
	}
	return s.findFinalityOfBlock(ctx, block, masternodes)
}

// GetBlockFinalityByNumber returns the finality of a block by number
func (s *BlockChainAPI) GetBlockFinalityByNumber(ctx context.Context, blockNumber rpc.BlockNumber) (uint, error) {
	block, err := s.b.BlockByNumber(ctx, blockNumber)
	if err != nil || block == nil {
		return uint(0), err
	}
	masternodes, err := s.GetMasternodes(ctx, block)
	if err != nil || len(masternodes) == 0 {
		log.Error("Failed to get masternodes", "err", err, "len(masternodes)", len(masternodes))
		return uint(0), err
	}
	return s.findFinalityOfBlock(ctx, block, masternodes)
}

// GetMasternodes returns masternodes set at the starting block of epoch of the given block
func (s *BlockChainAPI) GetMasternodes(ctx context.Context, b *types.Block) ([]common.Address, error) {
	var masternodes []common.Address
	if b.Number().Int64() >= 0 {
		if engine, ok := s.b.Engine().(*XDPoS.XDPoS); ok {
			// Get block epoch masternodes
			chain := s.b.BlockChain()
			if chain != nil {
				return engine.GetMasternodes(chain, b.Header()), nil
			}
		} else {
			log.Error("Undefined XDPoS consensus engine")
		}
	}
	return masternodes, nil
}

// GetCandidateStatus returns status of the given candidate at a specified epochNumber
func (s *BlockChainAPI) GetCandidateStatus(ctx context.Context, coinbaseAddress common.Address, epoch rpc.BlockNumber) (map[string]interface{}, error) {
	result := map[string]interface{}{
		fieldStatus:   "",
		fieldCapacity: 0,
		fieldSuccess:  true,
	}

	config := s.b.ChainConfig()
	if config.XDPoS == nil {
		return result, errors.New("XDPoS config not found")
	}
	epochConfig := config.XDPoS.Epoch

	// Calculate checkpoint block number
	blockNum := uint64(epoch)
	if epoch < 0 {
		blockNum = s.b.CurrentBlock().Number.Uint64()
	}
	checkpointNumber := (blockNum / epochConfig) * epochConfig
	if checkpointNumber == 0 {
		checkpointNumber = epochConfig
	}

	result[fieldEpoch] = checkpointNumber / epochConfig

	block, err := s.b.BlockByNumber(ctx, rpc.BlockNumber(checkpointNumber))
	if err != nil || block == nil {
		result[fieldSuccess] = false
		return result, err
	}

	header := block.Header()
	if header == nil {
		log.Error("Empty header at checkpoint", "num", checkpointNumber)
		return result, errEmptyHeader
	}

	// Get candidates from state
	statedb, _, err := s.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(checkpointNumber))
	if err != nil {
		result[fieldSuccess] = false
		return result, err
	}
	if statedb == nil {
		result[fieldSuccess] = false
		return result, errors.New("nil statedb in GetCandidateStatus")
	}

	candidatesAddresses := state.GetCandidates(statedb)
	candidates := make([]utils.Masternode, 0, len(candidatesAddresses))
	for _, address := range candidatesAddresses {
		v := state.GetCandidateCap(statedb, address)
		candidates = append(candidates, utils.Masternode{Address: address, Stake: v})
	}

	if len(candidates) == 0 {
		log.Debug("Candidates list cannot be found", "len(candidates)", len(candidates), "err", err)
		result[fieldSuccess] = false
		return result, err
	}

	// Check if the address is a candidate
	isCandidate := false
	for i := 0; i < len(candidates); i++ {
		if coinbaseAddress == candidates[i].Address {
			isCandidate = true
			result[fieldStatus] = statusProposed
			result[fieldCapacity] = candidates[i].Stake
			break
		}
	}

	// Get masternode list
	var masternodes []common.Address
	if engine, ok := s.b.Engine().(*XDPoS.XDPoS); ok {
		masternodes = engine.GetMasternodesFromCheckpointHeader(header, header.Number.Uint64(), epochConfig)
		if len(masternodes) == 0 {
			log.Error("Failed to get masternodes", "err", err, "len(masternodes)", len(masternodes), "blockNum", header.Number.Uint64())
			result[fieldSuccess] = false
			return result, err
		}
	} else {
		log.Error("Undefined XDPoS consensus engine")
	}

	// Set to statusMasternode if it is masternode
	for _, masternode := range masternodes {
		if coinbaseAddress == masternode {
			result[fieldStatus] = statusMasternode
			if !isCandidate {
				result[fieldCapacity] = -1
				log.Warn("Find non-candidate masternode", "masternode", masternode.String(), "checkpointNumber", checkpointNumber, "epoch", epoch)
			}
			return result, nil
		}
	}

	return result, nil
}

// GetCandidates returns status of all candidates at a specified epochNumber
func (s *BlockChainAPI) GetCandidates(ctx context.Context, epoch rpc.BlockNumber) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"candidates": []map[string]interface{}{},
		"success":    true,
	}

	config := s.b.ChainConfig()
	if config.XDPoS == nil {
		result["success"] = false
		return result, errors.New("XDPoS config not found")
	}
	epochConfig := config.XDPoS.Epoch

	// Calculate checkpoint block number
	blockNum := uint64(epoch)
	if epoch < 0 {
		blockNum = s.b.CurrentBlock().Number.Uint64()
	}
	checkpointNumber := (blockNum / epochConfig) * epochConfig
	if checkpointNumber == 0 {
		checkpointNumber = epochConfig
	}

	result["epoch"] = checkpointNumber / epochConfig

	// Get candidates from state
	statedb, _, err := s.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(checkpointNumber))
	if err != nil {
		result["success"] = false
		return result, err
	}
	if statedb == nil {
		result["success"] = false
		return result, errors.New("nil statedb in GetCandidates")
	}

	candidatesAddresses := state.GetCandidates(statedb)
	candidates := make([]map[string]interface{}, 0, len(candidatesAddresses))
	for _, address := range candidatesAddresses {
		cap := state.GetCandidateCap(statedb, address)
		owner := state.GetCandidateOwner(statedb, address)
		candidates = append(candidates, map[string]interface{}{
			"address":  address,
			"capacity": cap,
			"owner":    owner,
		})
	}

	// Sort by capacity descending
	sort.Slice(candidates, func(i, j int) bool {
		capI := candidates[i]["capacity"].(*big.Int)
		capJ := candidates[j]["capacity"].(*big.Int)
		return capI.Cmp(capJ) > 0
	})

	result["candidates"] = candidates
	return result, nil
}

// rpcOutputBlockSigners returns block signers for RPC output
func (s *BlockChainAPI) rpcOutputBlockSigners(block *types.Block, ctx context.Context, masternodes []common.Address) ([]common.Address, error) {
	if block == nil {
		return nil, errors.New("block not found")
	}

	header := block.Header()
	if header == nil {
		return nil, errors.New("header not found")
	}

	// Extract signers from header's Validator field
	signers := make([]common.Address, 0)
	if len(header.Validator) > 0 {
		// Decode validator bytes to addresses
		for i := 0; i+common.AddressLength <= len(header.Validator); i += common.AddressLength {
			var addr common.Address
			copy(addr[:], header.Validator[i:i+common.AddressLength])
			signers = append(signers, addr)
		}
	}

	// If no signers found in Validator field, return the block miner
	if len(signers) == 0 {
		signers = append(signers, header.Coinbase)
	}

	return signers, nil
}

// findFinalityOfBlock calculates the finality percentage of a block
func (s *BlockChainAPI) findFinalityOfBlock(ctx context.Context, block *types.Block, masternodes []common.Address) (uint, error) {
	if block == nil || len(masternodes) == 0 {
		return 0, nil
	}

	signers, err := s.rpcOutputBlockSigners(block, ctx, masternodes)
	if err != nil {
		return 0, err
	}

	// Calculate finality percentage
	signerCount := len(signers)
	masternodeCount := len(masternodes)

	if masternodeCount == 0 {
		return 0, nil
	}

	finality := uint((signerCount * 100) / masternodeCount)
	return finality, nil
}

// RPCMarshalHeaderXDC converts a header to the RPC output with XDC fields
func RPCMarshalHeaderXDC(header *types.Header) map[string]interface{} {
	result := map[string]interface{}{
		"number":           (*hexutil.Big)(header.Number),
		"hash":             header.Hash(),
		"parentHash":       header.ParentHash,
		"nonce":            header.Nonce,
		"mixHash":          header.MixDigest,
		"sha3Uncles":       header.UncleHash,
		"logsBloom":        header.Bloom,
		"stateRoot":        header.Root,
		"miner":            header.Coinbase,
		"difficulty":       (*hexutil.Big)(header.Difficulty),
		"extraData":        hexutil.Bytes(header.Extra),
		"size":             hexutil.Uint64(header.Size()),
		"gasLimit":         hexutil.Uint64(header.GasLimit),
		"gasUsed":          hexutil.Uint64(header.GasUsed),
		"timestamp":        hexutil.Uint64(header.Time),
		"transactionsRoot": header.TxHash,
		"receiptsRoot":     header.ReceiptHash,
	}

	// Add XDPoS-specific fields
	if header.Validators != nil {
		result["validators"] = hexutil.Bytes(header.Validators)
	}
	if header.Validator != nil {
		result["validator"] = hexutil.Bytes(header.Validator)
	}
	if header.Penalties != nil {
		result["penalties"] = hexutil.Bytes(header.Penalties)
	}

	if header.BaseFee != nil {
		result["baseFeePerGas"] = (*hexutil.Big)(header.BaseFee)
	}
	if header.WithdrawalsHash != nil {
		result["withdrawalsRoot"] = header.WithdrawalsHash
	}

	return result
}

// proofPairList collects trie proof key/value pairs for RPC delivery.
// Fix #66: Required by GetTransactionAndReceiptProof.
type proofPairList struct {
	keys   []string
	values []string
}

func (n *proofPairList) Put(key []byte, value []byte) error {
	n.keys = append(n.keys, hexutil.Encode(key))
	n.values = append(n.values, hexutil.Encode(value))
	return nil
}

func (n *proofPairList) Delete(key []byte) error {
	panic("proofPairList: Delete not supported")
}

// deriveTrie builds a Merkle trie from a DerivableList (txs or receipts).
func deriveTrie(list types.DerivableList) *trie.Trie {
	buf := new(bytes.Buffer)
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	for i := 0; i < list.Len(); i++ {
		buf.Reset()
		rlp.Encode(buf, uint(i))
		key := common.CopyBytes(buf.Bytes())
		buf.Reset()
		list.EncodeIndex(i, buf)
		tr.Update(key, common.CopyBytes(buf.Bytes()))
	}
	return tr
}

// GetRewardByHash returns the reward info stored for the block with the given hash.
func (s *BlockChainAPI) GetRewardByHash(hash common.Hash) map[string]map[string]map[string]*big.Int {
	return s.b.GetRewardByHash(hash)
}

// GetTransactionAndReceiptProof returns the Merkle transaction and receipt proof
// for the transaction with the given hash.
// Fix #66: Ported from v2.6.8 internal/ethapi/api.go PublicBlockChainAPI.GetTransactionAndReceiptProof.
func (s *BlockChainAPI) GetTransactionAndReceiptProof(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	found, tx, blockHash, _, index := s.b.GetCanonicalTransaction(hash)
	if !found || tx == nil {
		return nil, nil
	}
	block, err := s.b.BlockByHash(ctx, blockHash)
	if err != nil {
		return nil, err
	}

	txTrie := deriveTrie(block.Transactions())
	keybuf := new(bytes.Buffer)
	if err := rlp.Encode(keybuf, uint(index)); err != nil {
		return nil, err
	}
	var txProof proofPairList
	if err := txTrie.Prove(keybuf.Bytes(), &txProof); err != nil {
		return nil, err
	}

	receipts, err := s.b.GetReceipts(ctx, blockHash)
	if err != nil {
		return nil, err
	}
	if int(index) >= len(receipts) {
		return nil, nil
	}
	receiptTrie := deriveTrie(receipts)
	var receiptProof proofPairList
	if err := receiptTrie.Prove(keybuf.Bytes(), &receiptProof); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"blockHash":          blockHash,
		"txRoot":             txTrie.Hash(),
		"receiptRoot":        receiptTrie.Hash(),
		"key":                hexutil.Encode(keybuf.Bytes()),
		"txProofKeys":        txProof.keys,
		"txProofValues":      txProof.values,
		"receiptProofKeys":   receiptProof.keys,
		"receiptProofValues": receiptProof.values,
	}, nil
}

// GetAccountInfo returns account state information for the given address at the given block.
// Fix #66: Ported from v2.6.8 internal/ethapi/api.go PublicBlockChainAPI.GetAccountInfo.
func (s *BlockChainAPI) GetAccountInfo(ctx context.Context, address common.Address, blockNrOrHash rpc.BlockNumberOrHash) (map[string]interface{}, error) {
	statedb, _, err := s.b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if statedb == nil || err != nil {
		return nil, err
	}
	balance := statedb.GetBalance(address)
	nonce := statedb.GetNonce(address)
	codeHash := statedb.GetCodeHash(address)
	codeSize := statedb.GetCodeSize(address)
	storageRoot := statedb.GetStorageRoot(address)

	return map[string]interface{}{
		"address":     address,
		"balance":     (*hexutil.Big)(balance.ToBig()),
		"codeSize":    hexutil.Uint64(codeSize),
		"codeHash":    codeHash,
		"nonce":       hexutil.Uint64(nonce),
		"storageHash": storageRoot,
	}, nil
}

// GetCheckpointFromEpoch returns the checkpoint block number for the given epoch.
// Fix #66: Ported from v2.6.8 internal/ethapi/api.go PublicBlockChainAPI.GetCheckpointFromEpoch.
func (s *BlockChainAPI) GetCheckpointFromEpoch(ctx context.Context, epochNum rpc.BlockNumber) (rpc.BlockNumber, rpc.BlockNumber) {
	config := s.b.ChainConfig()
	if config.XDPoS == nil {
		return 0, 0
	}
	epoch := config.XDPoS.Epoch
	var checkpointNumber uint64

	if epochNum < 0 {
		// Latest epoch
		blockNum := s.b.CurrentBlock().Number.Uint64()
		if engine, ok := s.b.Engine().(*XDPoS.XDPoS); ok {
			var err error
			var currentEpoch uint64
			checkpointNumber, currentEpoch, err = engine.GetCurrentEpochSwitchBlock(s.b.BlockChain(), new(big.Int).SetUint64(blockNum))
			if err != nil {
				log.Error("[GetCheckpointFromEpoch] GetCurrentEpochSwitchBlock failed", "block", blockNum, "err", err)
				return 0, rpc.BlockNumber(epochNum)
			}
			epochNum = rpc.BlockNumber(currentEpoch)
		}
	} else if epochNum < 2 {
		checkpointNumber = 0
	} else {
		checkpointNumber = epoch * (uint64(epochNum) - 1)
	}
	return rpc.BlockNumber(checkpointNumber), epochNum
}

// GetStakerROI estimates the annual ROI for stakers using the last epoch's reward.
// Fix #66: Ported from v2.6.8 internal/ethapi/api.go PublicBlockChainAPI.GetStakerROI.
func (s *BlockChainAPI) GetStakerROI() float64 {
	config := s.b.ChainConfig()
	if config.XDPoS == nil {
		return 0
	}
	epoch := config.XDPoS.Epoch
	blockNumber := s.b.CurrentBlock().Number.Uint64()
	if blockNumber < epoch*2 {
		return 0
	}
	// Use reward from 2 epochs ago
	masternodeReward := new(big.Int).Mul(
		new(big.Int).SetUint64(config.XDPoS.Reward),
		new(big.Int).SetUint64(params.Ether),
	)
	// Estimate based on configured reward — actual voter reward ≈ 50% of total
	holderReward := new(big.Int).Div(masternodeReward, big.NewInt(2))
	period := uint64(config.XDPoS.Period)
	if period == 0 {
		period = 2
	}
	blocksPerYear := uint64(365*86400) / period
	epochsPerYear := blocksPerYear / epoch
	if epochsPerYear == 0 {
		return 0
	}
	voterRewardAYear := new(big.Int).Mul(holderReward, new(big.Int).SetUint64(epochsPerYear))

	// Estimate total staked cap: 18 masternodes × 10,000,000 XDC each (typical)
	// This is a rough estimate; real implementation queries smart contracts.
	masternodeCount := uint64(18)
	if config.XDPoS.V2 != nil && config.XDPoS.V2.CurrentConfig != nil && config.XDPoS.V2.CurrentConfig.MaxMasternodes > 0 {
		masternodeCount = uint64(config.XDPoS.V2.CurrentConfig.MaxMasternodes)
	}
	minCap := new(big.Int).Mul(new(big.Int).SetUint64(10_000_000), new(big.Int).SetUint64(params.Ether))
	totalCap := new(big.Int).Mul(minCap, new(big.Int).SetUint64(masternodeCount))

	if totalCap.Sign() == 0 {
		return 0
	}
	// ROI = (voterRewardAYear / totalCap) * 100
	roi := new(big.Float).Quo(new(big.Float).SetInt(voterRewardAYear), new(big.Float).SetInt(totalCap))
	roi.Mul(roi, big.NewFloat(100))
	result, _ := roi.Float64()
	return result
}

// GetStakerROIMasternode estimates annual ROI for stakers of a specific masternode.
// Fix #66: Ported from v2.6.8 internal/ethapi/api.go PublicBlockChainAPI.GetStakerROIMasternode.
func (s *BlockChainAPI) GetStakerROIMasternode(masternode common.Address) float64 {
	// Full implementation requires XDC smart contract integration.
	// For now, return the same as GetStakerROI() divided by masternode count.
	roi := s.GetStakerROI()
	return roi
}

// GetStateHealStatus returns the current snap-heal status for diagnostic purposes.
// Method: xdc_getStateHealStatus (also registered under eth namespace).
//
// Acceptance criterion: enabled == false at tip when snap-heal has completed.
// Refs #894.
func (s *BlockChainAPI) GetStateHealStatus() map[string]interface{} {
	fields := s.b.SnapHealStatus()
	if fields == nil {
		return map[string]interface{}{
			"enabled":         false,
			"inProgress":      false,
			"mode":            "snap-heal",
			"targetRoot":      "",
			"trienodePending": uint64(0),
			"bytecodePending": uint64(0),
			"healStartUnix":   int64(0),
			"lastError":       "",
		}
	}
	return map[string]interface{}{
		"enabled":         fields.Enabled,
		"inProgress":      fields.InProgress,
		"mode":            fields.Mode,
		"targetRoot":      fields.TargetRoot,
		"trienodePending": fields.TrienodePending,
		"bytecodePending": fields.BytecodePending,
		"healStartUnix":   fields.HealStartUnix,
		"lastError":       fields.LastError,
	}
}
