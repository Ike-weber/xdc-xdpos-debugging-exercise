// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/holiman/uint256"
)

//go:generate go run github.com/fjl/gencodec -type Genesis -field-override genesisSpecMarshaling -out gen_genesis.go

var errGenesisNoConfig = errors.New("genesis has no chain configuration")

// Deprecated: use types.Account instead.
type GenesisAccount = types.Account

// Deprecated: use types.GenesisAlloc instead.
type GenesisAlloc = types.GenesisAlloc

// Genesis specifies the header fields, state of a genesis block. It also defines hard
// fork switch-over blocks through the chain configuration.
type Genesis struct {
	Config     *params.ChainConfig `json:"config"`
	Nonce      uint64              `json:"nonce"`
	Timestamp  uint64              `json:"timestamp"`
	ExtraData  []byte              `json:"extraData"`
	GasLimit   uint64              `json:"gasLimit"   gencodec:"required"`
	Difficulty *big.Int            `json:"difficulty" gencodec:"required"`
	Mixhash    common.Hash         `json:"mixHash"`
	Coinbase   common.Address      `json:"coinbase"`
	Alloc      types.GenesisAlloc  `json:"alloc"      gencodec:"required"`

	// These fields are used for consensus tests. Please don't use them
	// in actual genesis blocks.
	Number        uint64      `json:"number"`
	GasUsed       uint64      `json:"gasUsed"`
	ParentHash    common.Hash `json:"parentHash"`
	BaseFee       *big.Int    `json:"baseFeePerGas"` // EIP-1559
	ExcessBlobGas *uint64     `json:"excessBlobGas"` // EIP-4844
	BlobGasUsed   *uint64     `json:"blobGasUsed"`   // EIP-4844
	SlotNumber    *uint64     `json:"slotNumber"`    // EIP-7843
}

// copy copies the genesis.
func (g *Genesis) copy() *Genesis {
	if g != nil {
		cpy := *g
		if g.Config != nil {
			conf := *g.Config
			cpy.Config = &conf
		}
		return &cpy
	}
	return nil
}

func ReadGenesis(db ethdb.Database) (*Genesis, error) {
	var genesis Genesis
	stored := rawdb.ReadCanonicalHash(db, 0)
	if (stored == common.Hash{}) {
		return nil, fmt.Errorf("invalid genesis hash in database: %x", stored)
	}
	blob := rawdb.ReadGenesisStateSpec(db, stored)
	if blob == nil {
		return nil, errors.New("genesis state missing from db")
	}
	if len(blob) != 0 {
		if err := genesis.Alloc.UnmarshalJSON(blob); err != nil {
			return nil, fmt.Errorf("could not unmarshal genesis state json: %s", err)
		}
	}
	genesis.Config = rawdb.ReadChainConfig(db, stored)
	if genesis.Config == nil {
		return nil, errors.New("genesis config missing from db")
	}
	genesisBlock := rawdb.ReadBlock(db, stored, 0)
	if genesisBlock == nil {
		return nil, errors.New("genesis block missing from db")
	}
	genesisHeader := genesisBlock.Header()
	genesis.Nonce = genesisHeader.Nonce.Uint64()
	genesis.Timestamp = genesisHeader.Time
	genesis.ExtraData = genesisHeader.Extra
	genesis.GasLimit = genesisHeader.GasLimit
	genesis.Difficulty = genesisHeader.Difficulty
	genesis.Mixhash = genesisHeader.MixDigest
	genesis.Coinbase = genesisHeader.Coinbase
	genesis.BaseFee = genesisHeader.BaseFee
	genesis.ExcessBlobGas = genesisHeader.ExcessBlobGas
	genesis.BlobGasUsed = genesisHeader.BlobGasUsed
	genesis.SlotNumber = genesisHeader.SlotNumber

	return &genesis, nil
}

// hashAlloc computes the state root according to the genesis specification.
func hashAlloc(ga *types.GenesisAlloc, isUBT bool) (common.Hash, error) {
	// If a genesis-time verkle trie is requested, create a trie config
	// with the verkle trie enabled so that the tree can be initialized
	// as such.
	var config *triedb.Config
	if isUBT {
		config = &triedb.Config{
			PathDB:            pathdb.Defaults,
			IsUBT:             true,
			BinTrieGroupDepth: triedb.UBTDefaults.BinTrieGroupDepth,
		}
	}
	// Create an ephemeral in-memory database for computing hash,
	// all the derived states will be discarded to not pollute disk.
	emptyRoot := types.EmptyRootHash
	if isUBT {
		emptyRoot = types.EmptyBinaryHash
	}
	db := rawdb.NewMemoryDatabase()
	statedb, err := state.New(emptyRoot, state.NewDatabase(triedb.NewDatabase(db, config), nil))
	if err != nil {
		return common.Hash{}, err
	}
	for addr, account := range *ga {
		if account.Balance != nil {
			statedb.AddBalance(addr, uint256.MustFromBig(account.Balance), tracing.BalanceIncreaseGenesisBalance)
		}
		statedb.SetCode(addr, account.Code, tracing.CodeChangeGenesis)
		statedb.SetNonce(addr, account.Nonce, tracing.NonceChangeGenesis)
		for key, value := range account.Storage {
			statedb.SetState(addr, key, value)
		}
	}
	return statedb.Commit(0, false, false)
}

// flushAlloc is very similar with hash, but the main difference is all the
// generated states will be persisted into the given database.
func flushAlloc(ga *types.GenesisAlloc, triedb *triedb.Database, tracer *tracing.Hooks) (common.Hash, error) {
	emptyRoot := types.EmptyRootHash
	if triedb.IsUBT() {
		emptyRoot = types.EmptyBinaryHash
	}
	statedb, err := state.New(emptyRoot, state.NewDatabase(triedb, nil))
	if err != nil {
		return common.Hash{}, err
	}
	for addr, account := range *ga {
		if account.Balance != nil {
			// This is not actually logged via tracer because OnGenesisBlock
			// already captures the allocations.
			statedb.AddBalance(addr, uint256.MustFromBig(account.Balance), tracing.BalanceIncreaseGenesisBalance)
		}
		statedb.SetCode(addr, account.Code, tracing.CodeChangeGenesis)
		statedb.SetNonce(addr, account.Nonce, tracing.NonceChangeGenesis)
		for key, value := range account.Storage {
			statedb.SetState(addr, key, value)
		}
	}

	var root common.Hash
	if tracer != nil && tracer.OnStateUpdate != nil {
		r, update, err := statedb.CommitWithUpdate(0, false, false)
		if err != nil {
			return common.Hash{}, err
		}
		trUpdate, err := update.ToTracingUpdate()
		if err != nil {
			return common.Hash{}, err
		}
		tracer.OnStateUpdate(trUpdate)
		root = r
	} else {
		root, err = statedb.Commit(0, false, false)
		if err != nil {
			return common.Hash{}, err
		}
	}

	// Commit newly generated states into disk if it's not empty.
	if root != emptyRoot {
		if err := triedb.Commit(root, true); err != nil {
			return common.Hash{}, err
		}
	}
	return root, nil
}

func getGenesisState(db ethdb.Database, blockhash common.Hash) (alloc types.GenesisAlloc, err error) {
	blob := rawdb.ReadGenesisStateSpec(db, blockhash)
	if len(blob) != 0 {
		if err := alloc.UnmarshalJSON(blob); err != nil {
			return nil, err
		}

		return alloc, nil
	}

	// Genesis allocation is missing and there are several possibilities:
	// the node is legacy which doesn't persist the genesis allocation or
	// the persisted allocation is just lost.
	// - supported networks(mainnet, testnets), recover with defined allocations
	// - private network, can't recover
	var genesis *Genesis
	switch blockhash {
	case params.MainnetGenesisHash:
		genesis = DefaultGenesisBlock()
	case params.SepoliaGenesisHash:
		genesis = DefaultSepoliaGenesisBlock()
	case params.HoleskyGenesisHash:
		genesis = DefaultHoleskyGenesisBlock()
	case params.HoodiGenesisHash:
		genesis = DefaultHoodiGenesisBlock()
	}
	if genesis != nil {
		return genesis.Alloc, nil
	}

	return nil, nil
}

// field type overrides for gencodec
type genesisSpecMarshaling struct {
	Nonce         math.HexOrDecimal64
	Timestamp     math.HexOrDecimal64
	ExtraData     hexutil.Bytes
	GasLimit      math.HexOrDecimal64
	GasUsed       math.HexOrDecimal64
	Number        math.HexOrDecimal64
	Difficulty    *math.HexOrDecimal256
	Alloc         map[common.UnprefixedAddress]types.Account
	BaseFee       *math.HexOrDecimal256
	ExcessBlobGas *math.HexOrDecimal64
	BlobGasUsed   *math.HexOrDecimal64
}

// GenesisMismatchError is raised when trying to overwrite an existing
// genesis block with an incompatible one.
type GenesisMismatchError struct {
	Stored, New common.Hash
}

func (e *GenesisMismatchError) Error() string {
	return fmt.Sprintf("database contains incompatible genesis (have %x, new %x)", e.Stored, e.New)
}

// ChainOverrides contains the changes to chain config.
type ChainOverrides struct {
	OverrideOsaka     *uint64
	OverrideAmsterdam *uint64
	OverrideBPO1      *uint64
	OverrideBPO2      *uint64
	OverrideUBT       *uint64
}

// apply applies the chain overrides on the supplied chain config.
func (o *ChainOverrides) apply(cfg *params.ChainConfig) error {
	if o == nil || cfg == nil {
		return nil
	}
	if o.OverrideOsaka != nil {
		cfg.OsakaTime = o.OverrideOsaka
	}
	if o.OverrideAmsterdam != nil {
		cfg.AmsterdamTime = o.OverrideAmsterdam
	}
	if o.OverrideBPO1 != nil {
		cfg.BPO1Time = o.OverrideBPO1
	}
	if o.OverrideBPO2 != nil {
		cfg.BPO2Time = o.OverrideBPO2
	}
	if o.OverrideUBT != nil {
		cfg.UBTTime = o.OverrideUBT
	}
	return cfg.CheckConfigForkOrder()
}

// SetupGenesisBlock writes or updates the genesis block in db.
// The block that will be used is:
//
//	                     genesis == nil       genesis != nil
//	                  +------------------------------------------
//	db has no genesis |  main-net default  |  genesis
//	db has genesis    |  from DB           |  genesis (if compatible)
//
// The stored chain configuration will be updated if it is compatible (i.e. does not
// specify a fork block below the local head block). In case of a conflict, the
// error is a *params.ConfigCompatError and the new, unwritten config is returned.
func SetupGenesisBlock(db ethdb.Database, triedb *triedb.Database, genesis *Genesis) (*params.ChainConfig, common.Hash, *params.ConfigCompatError, error) {
	return SetupGenesisBlockWithOverride(db, triedb, genesis, nil, nil)
}

func SetupGenesisBlockWithOverride(db ethdb.Database, triedb *triedb.Database, genesis *Genesis, overrides *ChainOverrides, tracer *tracing.Hooks) (*params.ChainConfig, common.Hash, *params.ConfigCompatError, error) {
	// Copy the genesis, so we can operate on a copy.
	genesis = genesis.copy()
	// Sanitize the supplied genesis, ensuring it has the associated chain
	// config attached.
	if genesis != nil && genesis.Config == nil {
		return nil, common.Hash{}, nil, errGenesisNoConfig
	}
	// Commit the genesis if the database is empty
	ghash := rawdb.ReadCanonicalHash(db, 0)
	if (ghash == common.Hash{}) {
		if genesis == nil {
			log.Info("Writing default main-net genesis block")
			genesis = DefaultGenesisBlock()
		} else {
			log.Info("Writing custom genesis block")
		}
		if err := overrides.apply(genesis.Config); err != nil {
			return nil, common.Hash{}, nil, err
		}
		// XDC: patch missing fork blocks before committing genesis. The embedded
		// genesis JSONs omit later forks (LondonBlock etc.). Without this,
		// SetupGenesisBlockWithOverride returns genesis.Config with nil LondonBlock,
		// and bc.chainConfig gets the unpatched config. On the very first run
		// (fresh datadir) this causes MakeSigner to return EIP155Signer for any
		// block >= EIP155Block, making DynamicFeeTx (type 2) fail with
		// "transaction type not supported" when the first post-fast-sync block
		// contains an EIP-1559 transaction. Subsequent starts load from the stored
		// config which is already patched by patchMissingForkBlocks in LoadChainConfig,
		// so the bug only manifests on the single first-run session. (refs #860)
		if genesis.Config != nil && genesis.Config.ChainID != nil {
			switch genesis.Config.ChainID.Uint64() {
			case 50:
				patchMissingForkBlocks(genesis.Config, params.XDCMainnetChainConfig, "mainnet", forkPatchRegistered)
			case 51:
				patchMissingForkBlocks(genesis.Config, params.XDCApothemChainConfig, "apothem", forkPatchRegistered)
			case 5551:
				patchMissingForkBlocks(genesis.Config, params.XDCDevnetChainConfig, "devnet", forkPatchRegistered)
			}
		}

		block, err := genesis.Commit(db, triedb, tracer)
		if err != nil {
			return nil, common.Hash{}, nil, err
		}
		return genesis.Config, block.Hash(), nil, nil
	}
	// Commit the genesis if the genesis block exists in the ancient database
	// but the key-value database is empty without initializing the genesis
	// fields. This scenario can occur when the node is created from scratch
	// with an existing ancient store.
	storedCfg := rawdb.ReadChainConfig(db, ghash)
	if storedCfg == nil {
		// Ensure the stored genesis block matches with the given genesis. Private
		// networks must explicitly specify the genesis in the config file, mainnet
		// genesis will be used as default and the initialization will always fail.
		if genesis == nil {
			log.Info("Writing default main-net genesis block")
			genesis = DefaultGenesisBlock()
		} else {
			log.Info("Writing custom genesis block")
		}
		if err := overrides.apply(genesis.Config); err != nil {
			return nil, common.Hash{}, nil, err
		}
		// XDC: same patch for the ancient-store path — same root cause applies.
		if genesis.Config != nil && genesis.Config.ChainID != nil {
			switch genesis.Config.ChainID.Uint64() {
			case 50:
				patchMissingForkBlocks(genesis.Config, params.XDCMainnetChainConfig, "mainnet", forkPatchRegistered)
			case 51:
				patchMissingForkBlocks(genesis.Config, params.XDCApothemChainConfig, "apothem", forkPatchRegistered)
			case 5551:
				patchMissingForkBlocks(genesis.Config, params.XDCDevnetChainConfig, "devnet", forkPatchRegistered)
			}
		}

		if hash := genesis.ToBlock().Hash(); hash != ghash {
			return nil, common.Hash{}, nil, &GenesisMismatchError{ghash, hash}
		}
		block, err := genesis.Commit(db, triedb, tracer)
		if err != nil {
			return nil, common.Hash{}, nil, err
		}
		return genesis.Config, block.Hash(), nil, nil
	}
	// The genesis block has already been committed previously. Verify that the
	// provided genesis with chain overrides matches the existing one, and update
	// the stored chain config if necessary.
	if genesis != nil {
		if err := overrides.apply(genesis.Config); err != nil {
			return nil, common.Hash{}, nil, err
		}

		if hash := genesis.ToBlock().Hash(); hash != ghash {
			return nil, common.Hash{}, nil, &GenesisMismatchError{ghash, hash}
		}
	}
	// Check config compatibility and write the config. Compatibility errors
	// are returned to the caller unless we're already at block zero.
	head := rawdb.ReadHeadHeader(db)
	if head == nil {
		return nil, common.Hash{}, nil, errors.New("missing head header")
	}
	newCfg := genesis.chainConfigOrDefault(ghash, storedCfg)
	// XDC: Patch missing TrustedSyncCheckpoints from hardcoded defaults.
	// The embedded apothem/mainnet genesis JSON doesn't include them, and
	// chainConfigOrDefault returns g.Config directly when genesis is provided,
	// bypassing the LoadChainConfig patch.
	if len(newCfg.TrustedSyncCheckpoints) == 0 && newCfg.ChainID != nil {
		switch newCfg.ChainID.Uint64() {
		case 50:
			newCfg.TrustedSyncCheckpoints = params.XDCMainnetChainConfig.TrustedSyncCheckpoints
			log.Info("XDC: patched missing TrustedSyncCheckpoints in SetupGenesis for mainnet", "count", len(newCfg.TrustedSyncCheckpoints))
		case 51:
			newCfg.TrustedSyncCheckpoints = params.XDCApothemChainConfig.TrustedSyncCheckpoints
			log.Info("XDC: patched missing TrustedSyncCheckpoints in SetupGenesis for apothem", "count", len(newCfg.TrustedSyncCheckpoints))
		case 5551:
			newCfg.TrustedSyncCheckpoints = params.XDCDevnetChainConfig.TrustedSyncCheckpoints
			log.Info("XDC: patched missing TrustedSyncCheckpoints in SetupGenesis for devnet", "count", len(newCfg.TrustedSyncCheckpoints))
		}
	}
	// XDC: Patch missing V2 config from hardcoded defaults.
	// The genesis JSON doesn't include V2, so it will be nil. Without this,
	// the consensus engine won't detect V2 blocks and will fail at the switch block.
	if newCfg.XDPoS != nil && newCfg.XDPoS.V2 == nil && newCfg.ChainID != nil {
		switch newCfg.ChainID.Uint64() {
		case 50:
			newCfg.XDPoS.V2 = &params.V2{
				SwitchBlock:   new(big.Int).Set(params.XDCMainnetChainConfig.XDPoS.V2.SwitchBlock),
				SwitchEpoch:   params.XDCMainnetChainConfig.XDPoS.V2.SwitchEpoch,
				CurrentConfig: params.XDCMainnetChainConfig.XDPoS.V2.CurrentConfig,
				AllConfigs:    params.XDCMainnetChainConfig.XDPoS.V2.AllConfigs,
			}
			log.Info("XDC: patched missing V2 config in SetupGenesis for mainnet (chain ID 50)", "switchBlock", newCfg.XDPoS.V2.SwitchBlock)
		case 51:
			newCfg.XDPoS.V2 = &params.V2{
				SwitchBlock:   new(big.Int).Set(params.XDCApothemChainConfig.XDPoS.V2.SwitchBlock),
				SwitchEpoch:   params.XDCApothemChainConfig.XDPoS.V2.SwitchEpoch,
				CurrentConfig: params.XDCApothemChainConfig.XDPoS.V2.CurrentConfig,
				AllConfigs:    params.XDCApothemChainConfig.XDPoS.V2.AllConfigs,
			}
			log.Info("XDC: patched missing V2 config in SetupGenesis for apothem (chain ID 51)", "switchBlock", newCfg.XDPoS.V2.SwitchBlock)
		case 5551:
			newCfg.XDPoS.V2 = &params.V2{
				SwitchBlock:   new(big.Int).Set(params.XDCDevnetChainConfig.XDPoS.V2.SwitchBlock),
				SwitchEpoch:   params.XDCDevnetChainConfig.XDPoS.V2.SwitchEpoch,
				CurrentConfig: params.XDCDevnetChainConfig.XDPoS.V2.CurrentConfig,
				AllConfigs:    params.XDCDevnetChainConfig.XDPoS.V2.AllConfigs,
			}
			log.Info("XDC: patched missing V2 config in SetupGenesis for devnet (chain ID 5551)", "switchBlock", newCfg.XDPoS.V2.SwitchBlock)
		default:
			newCfg.XDPoS.V2 = &params.V2{
				SwitchBlock:   new(big.Int).Set(params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchBlock),
				SwitchEpoch:   params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchEpoch,
				CurrentConfig: params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig,
				AllConfigs:    params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs,
			}
			log.Info("XDC: patched missing V2 config in SetupGenesis for unregistered XDPoS chain", "chainId", newCfg.ChainID, "switchBlock", newCfg.XDPoS.V2.SwitchBlock)
		}
	}
	// XDC: Staged V2 override — genesis carries a V2 block with AllConfigs nil.
	// For unregistered chains, fill CurrentConfig+AllConfigs from the default
	// profile while keeping the JSON-provided SwitchBlock/SwitchEpoch intact.
	if newCfg.XDPoS != nil && newCfg.XDPoS.V2 != nil && newCfg.XDPoS.V2.AllConfigs == nil && newCfg.ChainID != nil {
		switch newCfg.ChainID.Uint64() {
		case 50, 51, 5551:
			// registered nets: V2 AllConfigs must come from their own hardcoded config.
		default:
			newCfg.XDPoS.V2.CurrentConfig = params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig
			newCfg.XDPoS.V2.AllConfigs = params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs
			log.Info("XDC: filled missing V2 AllConfigs in SetupGenesis for unregistered XDPoS chain from default", "chainId", newCfg.ChainID)
		}
	}
	// XDC: Patch missing fork blocks from hardcoded defaults.
	// The embedded genesis JSONs omit later forks; without this,
	// CheckConfigForkOrder fails and the forkID hash differs from canonical,
	// causing wire-handshake rejection by production peers.
	if newCfg.ChainID != nil {
		switch newCfg.ChainID.Uint64() {
		case 50:
			patchMissingForkBlocks(newCfg, params.XDCMainnetChainConfig, "mainnet", forkPatchRegistered)
		case 51:
			patchMissingForkBlocks(newCfg, params.XDCApothemChainConfig, "apothem", forkPatchRegistered)
		case 5551:
			patchMissingForkBlocks(newCfg, params.XDCDevnetChainConfig, "devnet", forkPatchRegistered)
		default:
			if newCfg.XDPoS != nil {
				patchMissingForkBlocks(newCfg, params.XDCDefaultPrivateChainConfig, "xdc-default", forkPatchGenesisAuthoritative)
			}
		}
	}
	if err := overrides.apply(newCfg); err != nil {
		return nil, common.Hash{}, nil, err
	}

	// Sanity-check the new configuration.
	if err := newCfg.CheckConfigForkOrder(); err != nil {
		return nil, common.Hash{}, nil, err
	}

	// TODO(rjl493456442) better to define the comparator of chain config
	// and short circuit if the chain config is not changed.
	compatErr := storedCfg.CheckCompatible(newCfg, head.Number.Uint64(), head.Time)
	if compatErr != nil && ((head.Number.Uint64() != 0 && compatErr.RewindToBlock != 0) || (head.Time != 0 && compatErr.RewindToTime != 0)) {
		return newCfg, ghash, compatErr, nil
	}
	// Don't overwrite if the old is identical to the new. It's useful
	// for the scenarios that database is opened in the read-only mode.
	storedData, _ := json.Marshal(storedCfg)
	if newData, _ := json.Marshal(newCfg); !bytes.Equal(storedData, newData) {
		rawdb.WriteChainConfig(db, ghash, newCfg)
	}
	return newCfg, ghash, nil, nil
}

// LoadChainConfig loads the stored chain config if it is already present in
// database, otherwise, return the config in the provided genesis specification.
func LoadChainConfig(db ethdb.Database, genesis *Genesis) (cfg *params.ChainConfig, ghash common.Hash, err error) {
	// Load the stored chain config from the database. It can be nil
	// in case the database is empty. Notably, we only care about the
	// chain config corresponds to the canonical chain.
	stored := rawdb.ReadCanonicalHash(db, 0)
	if stored != (common.Hash{}) {
		storedcfg := rawdb.ReadChainConfig(db, stored)
		if storedcfg != nil {
			// XDC fix: older databases may have stored the V2 config with a
			// capitalised JSON key ("V2") which Go's json.Unmarshal misses
			// when the struct tag is "v2". If V2 is nil for a known XDC
			// network, patch it from hardcoded defaults so V2 consensus
			// validation activates instead of falling back to V1.
			if storedcfg.XDPoS != nil && storedcfg.XDPoS.V2 == nil && storedcfg.ChainID != nil {
				switch storedcfg.ChainID.Uint64() {
				case 50:
					storedcfg.XDPoS.V2 = &params.V2{
						SwitchBlock:   new(big.Int).Set(params.XDCMainnetChainConfig.XDPoS.V2.SwitchBlock),
						SwitchEpoch:   params.XDCMainnetChainConfig.XDPoS.V2.SwitchEpoch,
						CurrentConfig: params.XDCMainnetChainConfig.XDPoS.V2.CurrentConfig,
						AllConfigs:    params.XDCMainnetChainConfig.XDPoS.V2.AllConfigs,
					}
					log.Info("XDC: patched missing V2 config in stored config for mainnet (chain ID 50)", "switchBlock", storedcfg.XDPoS.V2.SwitchBlock)
				case 51:
					storedcfg.XDPoS.V2 = &params.V2{
						SwitchBlock:   new(big.Int).Set(params.XDCApothemChainConfig.XDPoS.V2.SwitchBlock),
						SwitchEpoch:   params.XDCApothemChainConfig.XDPoS.V2.SwitchEpoch,
						CurrentConfig: params.XDCApothemChainConfig.XDPoS.V2.CurrentConfig,
						AllConfigs:    params.XDCApothemChainConfig.XDPoS.V2.AllConfigs,
					}
					log.Info("XDC: patched missing V2 config in stored config for apothem (chain ID 51)", "switchBlock", storedcfg.XDPoS.V2.SwitchBlock)
				case 5551:
					storedcfg.XDPoS.V2 = &params.V2{
						SwitchBlock:   new(big.Int).Set(params.XDCDevnetChainConfig.XDPoS.V2.SwitchBlock),
						SwitchEpoch:   params.XDCDevnetChainConfig.XDPoS.V2.SwitchEpoch,
						CurrentConfig: params.XDCDevnetChainConfig.XDPoS.V2.CurrentConfig,
						AllConfigs:    params.XDCDevnetChainConfig.XDPoS.V2.AllConfigs,
					}
					log.Info("XDC: patched missing V2 config in stored config for devnet (chain ID 5551)", "switchBlock", storedcfg.XDPoS.V2.SwitchBlock)
				default:
					storedcfg.XDPoS.V2 = &params.V2{
						SwitchBlock:   new(big.Int).Set(params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchBlock),
						SwitchEpoch:   params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchEpoch,
						CurrentConfig: params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig,
						AllConfigs:    params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs,
					}
					log.Info("XDC: patched missing V2 config in stored config for unregistered XDPoS chain", "chainId", storedcfg.ChainID, "switchBlock", storedcfg.XDPoS.V2.SwitchBlock)
				}
			}
			// XDC: Staged V2 override — stored config carries a V2 block with
			// AllConfigs nil. For unregistered chains, fill CurrentConfig+AllConfigs
			// from the default profile while keeping the stored SwitchBlock/SwitchEpoch.
			if storedcfg.XDPoS != nil && storedcfg.XDPoS.V2 != nil && storedcfg.XDPoS.V2.AllConfigs == nil && storedcfg.ChainID != nil {
				switch storedcfg.ChainID.Uint64() {
				case 50, 51, 5551:
					// registered nets: V2 AllConfigs must come from their own hardcoded config.
				default:
					storedcfg.XDPoS.V2.CurrentConfig = params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig
					storedcfg.XDPoS.V2.AllConfigs = params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs
					log.Info("XDC: filled missing V2 AllConfigs in stored config for unregistered XDPoS chain from default", "chainId", storedcfg.ChainID)
				}
			}
			// XDC: Patch missing TrustedSyncCheckpoints from hardcoded defaults.
			// The genesis JSON doesn't include them, so older databases will
			// have empty lists. Restoring them at LoadChainConfig means
			// operators with existing chaindata don't need to wipe to pick up
			// new checkpoint releases.
			if len(storedcfg.TrustedSyncCheckpoints) == 0 && storedcfg.ChainID != nil {
				switch storedcfg.ChainID.Uint64() {
				case 50:
					storedcfg.TrustedSyncCheckpoints = params.XDCMainnetChainConfig.TrustedSyncCheckpoints
					log.Info("XDC: patched missing TrustedSyncCheckpoints in stored config for mainnet", "count", len(storedcfg.TrustedSyncCheckpoints))
				case 51:
					storedcfg.TrustedSyncCheckpoints = params.XDCApothemChainConfig.TrustedSyncCheckpoints
					log.Info("XDC: patched missing TrustedSyncCheckpoints in stored config for apothem", "count", len(storedcfg.TrustedSyncCheckpoints))
				case 5551:
					storedcfg.TrustedSyncCheckpoints = params.XDCDevnetChainConfig.TrustedSyncCheckpoints
					log.Info("XDC: patched missing TrustedSyncCheckpoints in stored config for devnet", "count", len(storedcfg.TrustedSyncCheckpoints))
				}
			}
			// XDC: Patch missing fork blocks from hardcoded defaults so
			// CheckConfigForkOrder passes on startup for existing chaindata.
			if storedcfg.ChainID != nil {
				switch storedcfg.ChainID.Uint64() {
				case 50:
					patchMissingForkBlocks(storedcfg, params.XDCMainnetChainConfig, "mainnet", forkPatchRegistered)
				case 51:
					patchMissingForkBlocks(storedcfg, params.XDCApothemChainConfig, "apothem", forkPatchRegistered)
				case 5551:
					patchMissingForkBlocks(storedcfg, params.XDCDevnetChainConfig, "devnet", forkPatchRegistered)
				default:
					if storedcfg.XDPoS != nil {
						patchMissingForkBlocks(storedcfg, params.XDCDefaultPrivateChainConfig, "xdc-default", forkPatchGenesisAuthoritative)
					}
				}
			}
			return storedcfg, stored, nil
		}
	}
	// Load the config from the provided genesis specification
	if genesis != nil {
		// Reject invalid genesis spec without valid chain config
		if genesis.Config == nil {
			return nil, common.Hash{}, errGenesisNoConfig
		}
		// If the canonical genesis header is present, but the chain
		// config is missing(initialize the empty leveldb with an
		// external ancient chain segment), ensure the provided genesis
		// is matched.
		ghash := genesis.ToBlock().Hash()
		if stored != (common.Hash{}) && ghash != stored {
			return nil, ghash, &GenesisMismatchError{stored, ghash}
		}
		// XDC fix (refs #807): same V2 patch as the stored-cfg branch
		// above. On a fresh-datadir first run, genesis.Config is what
		// gets returned, and --apothem / --mainnet flags supply a
		// Genesis spec whose Config.XDPoS.V2 may be nil. Without this
		// patch, downstream code (engine_v2 init, snap-sync pivot
		// selection) sees a V1-only config and never wires V2.
		if genesis.Config != nil && genesis.Config.XDPoS != nil && genesis.Config.XDPoS.V2 == nil && genesis.Config.ChainID != nil {
			switch genesis.Config.ChainID.Uint64() {
			case 50:
				genesis.Config.XDPoS.V2 = &params.V2{
					SwitchBlock:   new(big.Int).Set(params.XDCMainnetChainConfig.XDPoS.V2.SwitchBlock),
					SwitchEpoch:   params.XDCMainnetChainConfig.XDPoS.V2.SwitchEpoch,
					CurrentConfig: params.XDCMainnetChainConfig.XDPoS.V2.CurrentConfig,
					AllConfigs:    params.XDCMainnetChainConfig.XDPoS.V2.AllConfigs,
				}
				log.Info("XDC: patched missing V2 config in genesis spec for mainnet (chain ID 50)", "switchBlock", genesis.Config.XDPoS.V2.SwitchBlock)
			case 51:
				genesis.Config.XDPoS.V2 = &params.V2{
					SwitchBlock:   new(big.Int).Set(params.XDCApothemChainConfig.XDPoS.V2.SwitchBlock),
					SwitchEpoch:   params.XDCApothemChainConfig.XDPoS.V2.SwitchEpoch,
					CurrentConfig: params.XDCApothemChainConfig.XDPoS.V2.CurrentConfig,
					AllConfigs:    params.XDCApothemChainConfig.XDPoS.V2.AllConfigs,
				}
				log.Info("XDC: patched missing V2 config in genesis spec for apothem (chain ID 51)", "switchBlock", genesis.Config.XDPoS.V2.SwitchBlock)
			case 5551:
				genesis.Config.XDPoS.V2 = &params.V2{
					SwitchBlock:   new(big.Int).Set(params.XDCDevnetChainConfig.XDPoS.V2.SwitchBlock),
					SwitchEpoch:   params.XDCDevnetChainConfig.XDPoS.V2.SwitchEpoch,
					CurrentConfig: params.XDCDevnetChainConfig.XDPoS.V2.CurrentConfig,
					AllConfigs:    params.XDCDevnetChainConfig.XDPoS.V2.AllConfigs,
				}
				log.Info("XDC: patched missing V2 config in genesis spec for devnet (chain ID 5551)", "switchBlock", genesis.Config.XDPoS.V2.SwitchBlock)
			default:
				genesis.Config.XDPoS.V2 = &params.V2{
					SwitchBlock:   new(big.Int).Set(params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchBlock),
					SwitchEpoch:   params.XDCDefaultPrivateChainConfig.XDPoS.V2.SwitchEpoch,
					CurrentConfig: params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig,
					AllConfigs:    params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs,
				}
				log.Info("XDC: patched missing V2 config in genesis spec for unregistered XDPoS chain", "chainId", genesis.Config.ChainID, "switchBlock", genesis.Config.XDPoS.V2.SwitchBlock)
			}
		}
		// XDC: Staged V2 override — genesis spec carries a V2 block with AllConfigs
		// nil. For unregistered chains, fill CurrentConfig+AllConfigs from the default
		// profile while keeping the JSON-provided SwitchBlock/SwitchEpoch intact.
		if genesis.Config != nil && genesis.Config.XDPoS != nil && genesis.Config.XDPoS.V2 != nil && genesis.Config.XDPoS.V2.AllConfigs == nil && genesis.Config.ChainID != nil {
			switch genesis.Config.ChainID.Uint64() {
			case 50, 51, 5551:
				// registered nets: V2 AllConfigs must come from their own hardcoded config.
			default:
				genesis.Config.XDPoS.V2.CurrentConfig = params.XDCDefaultPrivateChainConfig.XDPoS.V2.CurrentConfig
				genesis.Config.XDPoS.V2.AllConfigs = params.XDCDefaultPrivateChainConfig.XDPoS.V2.AllConfigs
				log.Info("XDC: filled missing V2 AllConfigs in genesis spec for unregistered XDPoS chain from default", "chainId", genesis.Config.ChainID)
			}
		}
		// XDC fix (refs #807): also patch TrustedSyncCheckpoints in the
		// genesis-spec branch. Required by snap-sync pivot selection
		// which uses the highest checkpoint number as the peerHead
		// approximation under XDPOS2 (no wire-level peer-head).
		if genesis.Config != nil && len(genesis.Config.TrustedSyncCheckpoints) == 0 && genesis.Config.ChainID != nil {
			switch genesis.Config.ChainID.Uint64() {
			case 50:
				genesis.Config.TrustedSyncCheckpoints = params.XDCMainnetChainConfig.TrustedSyncCheckpoints
				log.Info("XDC: patched missing TrustedSyncCheckpoints in genesis spec for mainnet", "count", len(genesis.Config.TrustedSyncCheckpoints))
			case 51:
				genesis.Config.TrustedSyncCheckpoints = params.XDCApothemChainConfig.TrustedSyncCheckpoints
				log.Info("XDC: patched missing TrustedSyncCheckpoints in genesis spec for apothem", "count", len(genesis.Config.TrustedSyncCheckpoints))
			case 5551:
				genesis.Config.TrustedSyncCheckpoints = params.XDCDevnetChainConfig.TrustedSyncCheckpoints
				log.Info("XDC: patched missing TrustedSyncCheckpoints in genesis spec for devnet", "count", len(genesis.Config.TrustedSyncCheckpoints))
			}
		}
		return genesis.Config, ghash, nil
	}
	// There is no stored chain config and no new config provided,
	// In this case the default chain config(mainnet) will be used
	return params.MainnetChainConfig, params.MainnetGenesisHash, nil
}

// forkPatchMode selects how patchMissingForkBlocks treats a nil (unset) fork
// block field on cfg.
type forkPatchMode int

const (
	// forkPatchRegistered is used for chains with a canonical, globally-agreed
	// fork schedule that lives in code (mainnet/50, Apothem/51, devnet/5551). A
	// nil fork field there is simply an omission from the embedded genesis
	// JSON, so every default value — future-dated ones included — is inherited.
	// This is exactly today's (pre-existing) behaviour and this mode changes
	// nothing about it.
	forkPatchRegistered forkPatchMode = iota

	// forkPatchGenesisAuthoritative is used for every unregistered XDPoS chain.
	// Its genesis JSON is the ONLY fork schedule that exists for that network:
	// the highest fork the genesis itself declares becomes a ceiling, and no
	// default fork above that ceiling is inherited — not even one whose default
	// is block 0. A genesis that declares nothing at all from the ladder
	// behaves like the "future-dated forks refused" half of this mode applied
	// with no ceiling (see patchMissingForkBlocks doc for both cases).
	forkPatchGenesisAuthoritative
)

// forkLadderEntry describes one fork-block field for the purposes of
// patchMissingForkBlocks' genesis-authoritative ceiling rule. dst points at
// the field on the config being patched; def is the corresponding field on
// the defaults profile. optional mirrors CheckConfigForkOrder's "optional"
// flag (DAOFork/MuirGlacier/etc. may be legitimately absent without capping
// anything that follows them).
type forkLadderEntry struct {
	name     string
	dst      **big.Int
	def      *big.Int
	optional bool
}

// forkLadder returns the block-numbered fork ladder for cfg/defaults, in
// CheckConfigForkOrder order, plus the XDC block-gated extras
// (Eip1559/Cancun/Prague/TIPUpgradeReward/TIPUpgradePenalty) appended at the
// end. It is used both to rank forks (for the genesis-authoritative ceiling)
// and to drive the actual patch.
func forkLadder(cfg, defaults *params.ChainConfig) []forkLadderEntry {
	return []forkLadderEntry{
		{"homesteadBlock", &cfg.HomesteadBlock, defaults.HomesteadBlock, false},
		{"daoForkBlock", &cfg.DAOForkBlock, defaults.DAOForkBlock, true},
		{"eip150Block", &cfg.EIP150Block, defaults.EIP150Block, false},
		{"eip155Block", &cfg.EIP155Block, defaults.EIP155Block, false},
		{"eip158Block", &cfg.EIP158Block, defaults.EIP158Block, false},
		{"byzantiumBlock", &cfg.ByzantiumBlock, defaults.ByzantiumBlock, false},
		{"constantinopleBlock", &cfg.ConstantinopleBlock, defaults.ConstantinopleBlock, false},
		{"petersburgBlock", &cfg.PetersburgBlock, defaults.PetersburgBlock, false},
		{"istanbulBlock", &cfg.IstanbulBlock, defaults.IstanbulBlock, false},
		{"muirGlacierBlock", &cfg.MuirGlacierBlock, defaults.MuirGlacierBlock, true},
		{"berlinBlock", &cfg.BerlinBlock, defaults.BerlinBlock, false},
		{"londonBlock", &cfg.LondonBlock, defaults.LondonBlock, false},
		// XDC block-gated extras — not part of upstream CheckConfigForkOrder,
		// but they follow London in every profile this codebase ships.
		{"eip1559Block", &cfg.Eip1559Block, defaults.Eip1559Block, true},
		{"cancunBlock", &cfg.CancunBlock, defaults.CancunBlock, true},
		{"pragueBlock", &cfg.PragueBlock, defaults.PragueBlock, true},
		{"tipUpgradeRewardBlock", &cfg.TIPUpgradeRewardBlock, defaults.TIPUpgradeRewardBlock, true},
		{"tipUpgradePenaltyBlock", &cfg.TIPUpgradePenaltyBlock, defaults.TIPUpgradePenaltyBlock, true},
	}
}

// patchMissingForkBlocks fills nil fork-block fields from defaults so that
// XDC chains (whose embedded genesis JSON omits later forks) compute the same
// forkID hash as canonical XDC nodes. Without this, CheckConfigForkOrder fails
// because ByzantiumBlock is set but ConstantinopleBlock/PetersburgBlock/etc are
// nil, and the wire handshake forkID hash diverges from production.
//
// mode separates the two very different situations this serves:
//
//   - forkPatchRegistered (mainnet/50, Apothem/51, devnet/5551): a canonical,
//     globally-agreed fork schedule lives in code. A genesis JSON omitting a
//     fork block is simply incomplete, so inheriting the canonical value —
//     future-dated forks included — is both correct and required for forkID
//     parity. Unchanged from before this mode existed.
//
//   - forkPatchGenesisAuthoritative (every unregistered/private XDPoS chain):
//     there is no canonical schedule. The genesis JSON *is* the agreement
//     shared with every other client and producer on the net, so inheriting
//     ANY fork it never mentions makes this node change consensus rules
//     unilaterally and diverge from the whole network — including a fork
//     whose default happens to be block 0. That is not hypothetical: on a
//     5-validator legacy XDPoSChain private net (chainId 34093) whose genesis
//     declares nothing past byzantiumBlock, the default profile injected
//     constantinople/petersburg/istanbul/berlin/londonBlock=0. Istanbul's
//     EIP-1884/2200 repricing and EIP-2929's cold/warm access repricing (which
//     on XDC chains follows IsEIP1559, itself gated by the injected London/
//     Eip1559 pair) mispriced a 5-tx BlockSigners.sign() checkpoint block by
//     +3600 gas per tx (435790 vs the canonical 417790), which the A.94
//     auto-recover path masked and which then made the node's own fee-market
//     mint gate refuse to ever produce a block. Refs ethOne#62.
//
//     The rule: compute a CEILING from the single highest-ranked fork the
//     genesis itself declares (a non-nil field, at whatever ladder position —
//     see forkLadder). Any nil field ABOVE that ceiling is left nil, no matter
//     what block its default schedules it at. A nil field AT OR BELOW the
//     ceiling is inherited only if its default is already active at block 0
//     (a future-dated default is still refused exactly as before), and the
//     inherited value is clamped up to the previous non-optional resolved
//     fork's value so an intervening fork the genesis skipped can never
//     violate CheckConfigForkOrder.
//
//     A genesis that declares NOTHING in the entire ladder gets no ceiling at
//     all (every field qualifies as "at or below"), which reduces to today's
//     documented baseline: inherit every already-active (block 0) default,
//     refuse every future-dated one. A genesis that spells out the whole
//     ladder itself (net5151-class genesis files spell out
//     londonBlock/eip1559Block/cancunBlock) is untouched either way — this
//     function only ever fills nil fields.
//
// Note this is a rule about fork *scheduling*, not a chain-ID special case —
// no chainId is hardcoded here, so it also covers any future-dated fork later
// added to the default private profile.
func patchMissingForkBlocks(cfg, defaults *params.ChainConfig, network string, mode forkPatchMode) {
	if mode == forkPatchRegistered {
		// Registered networks: unchanged pre-existing behaviour. Every nil
		// field is filled from defaults, future-dated forks included.
		patch := func(dst **big.Int, def *big.Int, name string) {
			if *dst != nil || def == nil {
				return
			}
			*dst = new(big.Int).Set(def)
			log.Info("XDC: patched missing fork block", "network", network, "fork", name, "block", *dst)
		}
		patch(&cfg.ConstantinopleBlock, defaults.ConstantinopleBlock, "ConstantinopleBlock")
		patch(&cfg.PetersburgBlock, defaults.PetersburgBlock, "PetersburgBlock")
		patch(&cfg.IstanbulBlock, defaults.IstanbulBlock, "IstanbulBlock")
		patch(&cfg.BerlinBlock, defaults.BerlinBlock, "BerlinBlock")
		patch(&cfg.LondonBlock, defaults.LondonBlock, "LondonBlock")
		patch(&cfg.Eip1559Block, defaults.Eip1559Block, "Eip1559Block")
		patch(&cfg.CancunBlock, defaults.CancunBlock, "CancunBlock")
		// XDC block-based upgrade forks (#963 / dev-upgrade). Genesis JSONs
		// shipped with devnet/apothem omit these keys, so without patching
		// they stay nil at runtime and their features never activate —
		// causing a state-root divergence from canonical at the fork block
		// (devnet 5551 block 50,000: Prague/EIP-2935 history write + tiered
		// reward/penalty bookkeeping). Refs XDCIndia/ethOne#39.
		patch(&cfg.PragueBlock, defaults.PragueBlock, "PragueBlock")
		patch(&cfg.TIPUpgradeRewardBlock, defaults.TIPUpgradeRewardBlock, "TIPUpgradeRewardBlock")
		patch(&cfg.TIPUpgradePenaltyBlock, defaults.TIPUpgradePenaltyBlock, "TIPUpgradePenaltyBlock")
		return
	}

	// forkPatchGenesisAuthoritative: the genesis JSON is the complete fork
	// schedule for this chain. See doc comment above for the rule.
	ladder := forkLadder(cfg, defaults)

	// CEILING = index of the highest-ranked fork the genesis itself declares.
	// If nothing at all is declared, there is no ceiling (every entry is "at
	// or below" — R3's documented baseline).
	ceiling := len(ladder) - 1
	for i, e := range ladder {
		if *e.dst != nil {
			ceiling = i
		}
	}

	var (
		prevResolved            *big.Int
		inherited, notInherited []string
	)
	for i, e := range ladder {
		if *e.dst != nil {
			// R1: declared is untouchable.
			if !e.optional {
				prevResolved = *e.dst
			}
			continue
		}
		if e.def == nil {
			continue
		}
		if i > ceiling {
			log.Info("XDC: fork above the genesis-declared ceiling — not inherited (genesis is authoritative)",
				"network", network, "fork", e.name, "defaultBlock", e.def, "ceiling", ladder[ceiling].name)
			notInherited = append(notInherited, e.name)
			continue
		}
		if e.def.Sign() > 0 {
			// R4: a future-dated default is still refused even below the ceiling.
			log.Info("XDC: not inheriting future-dated fork block — genesis config is authoritative for this chain",
				"network", network, "fork", e.name, "defaultBlock", e.def)
			notInherited = append(notInherited, e.name)
			continue
		}
		// R5: clamp up to the previous non-optional resolved fork so an
		// intervening fork the genesis skipped can never go backwards and
		// violate CheckConfigForkOrder.
		val := new(big.Int).Set(e.def)
		if prevResolved != nil && val.Cmp(prevResolved) < 0 {
			val = new(big.Int).Set(prevResolved)
		}
		*e.dst = val
		log.Info("XDC: patched missing fork block", "network", network, "fork", e.name, "block", *e.dst)
		inherited = append(inherited, e.name)
		if !e.optional {
			prevResolved = val
		}
	}
	if len(inherited) > 0 || len(notInherited) > 0 {
		log.Info("XDC: genesis-authoritative fork schedule resolved",
			"network", network, "chainId", cfg.ChainID, "ceiling", ladder[ceiling].name,
			"inherited", strings.Join(inherited, ","), "notInherited", strings.Join(notInherited, ","))
	}
}

// chainConfigOrDefault retrieves the attached chain configuration. If the genesis
// object is null, it returns the default chain configuration based on the given
// genesis hash, or the locally stored config if it's not a pre-defined network.
func (g *Genesis) chainConfigOrDefault(ghash common.Hash, stored *params.ChainConfig) *params.ChainConfig {
	switch {
	case g != nil:
		return g.Config
	case ghash == params.MainnetGenesisHash:
		return params.MainnetChainConfig
	case ghash == params.HoleskyGenesisHash:
		return params.HoleskyChainConfig
	case ghash == params.SepoliaGenesisHash:
		return params.SepoliaChainConfig
	case ghash == params.HoodiGenesisHash:
		return params.HoodiChainConfig
	default:
		return stored
	}
}

// IsUBT indicates whether the state is already stored in a verkle
// tree at genesis time.
func (g *Genesis) IsUBT() bool {
	return g.Config.IsUBTGenesis()
}

// ToBlock returns the genesis block according to genesis specification.
func (g *Genesis) ToBlock() *types.Block {
	root, err := hashAlloc(&g.Alloc, g.IsUBT())
	if err != nil {
		panic(err)
	}
	return g.toBlockWithRoot(root)
}

// toBlockWithRoot constructs the genesis block with the given genesis state root.
func (g *Genesis) toBlockWithRoot(root common.Hash) *types.Block {
	head := &types.Header{
		Number:     new(big.Int).SetUint64(g.Number),
		Nonce:      types.EncodeNonce(g.Nonce),
		Time:       g.Timestamp,
		ParentHash: g.ParentHash,
		Extra:      g.ExtraData,
		GasLimit:   g.GasLimit,
		GasUsed:    g.GasUsed,
		BaseFee:    g.BaseFee,
		Difficulty: g.Difficulty,
		MixDigest:  g.Mixhash,
		Coinbase:   g.Coinbase,
		Root:       root,
		// XDPoS fields - initialize to empty slices (not nil) for RLP compatibility
		Validators: []byte{},
		Validator:  []byte{},
		Penalties:  []byte{},
	}
	if g.GasLimit == 0 {
		head.GasLimit = params.GenesisGasLimit
	}
	if g.Difficulty == nil {
		if g.Config != nil && g.Config.Ethash == nil {
			head.Difficulty = big.NewInt(0)
		} else if g.Mixhash == (common.Hash{}) {
			head.Difficulty = params.GenesisDifficulty
		}
	}
	// XDC chains decouple the EIP-1559 fee market (and thus the genesis baseFee)
	// from London: baseFee is gated on Eip1559Block, not LondonBlock (canonical
	// XDPoSChain behaviour). The devnet (chainId 5551) sets London @0 but EIP1559
	// @25000, so its genesis must carry NO baseFee to match the canonical genesis
	// hash 0xb8be…. Apothem/mainnet have London >0, so IsLondon(0) is already false
	// there — this only changes the London-at-genesis (devnet) case.
	feeMarketAtGenesis := g.Config != nil && g.Config.IsLondon(common.Big0)
	if g.Config != nil && g.Config.IsXDC() {
		feeMarketAtGenesis = g.Config.IsEIP1559(common.Big0)
	}
	if feeMarketAtGenesis {
		if g.BaseFee != nil {
			head.BaseFee = g.BaseFee
		} else {
			head.BaseFee = new(big.Int).SetUint64(params.InitialBaseFee)
		}
	}
	var (
		withdrawals []*types.Withdrawal
	)
	if conf := g.Config; conf != nil {
		num := big.NewInt(int64(g.Number))
		if conf.IsShanghai(num, g.Timestamp) {
			head.WithdrawalsHash = &types.EmptyWithdrawalsHash
			withdrawals = make([]*types.Withdrawal, 0)
		}
		if conf.IsCancun(num, g.Timestamp) {
			// EIP-4788: The parentBeaconBlockRoot of the genesis block is always
			// the zero hash. This is because the genesis block does not have a parent
			// by definition.
			head.ParentBeaconRoot = new(common.Hash)
			// EIP-4844 fields
			head.ExcessBlobGas = g.ExcessBlobGas
			head.BlobGasUsed = g.BlobGasUsed
			if head.ExcessBlobGas == nil {
				head.ExcessBlobGas = new(uint64)
			}
			if head.BlobGasUsed == nil {
				head.BlobGasUsed = new(uint64)
			}
		} else {
			if g.ExcessBlobGas != nil {
				log.Warn("Invalid genesis, unexpected ExcessBlobGas set before Cancun, allowing it for testing purposes")
				head.ExcessBlobGas = g.ExcessBlobGas
			}
		}
		if conf.IsPrague(num, g.Timestamp) {
			head.RequestsHash = &types.EmptyRequestsHash
		}
		if conf.IsAmsterdam(num, g.Timestamp) {
			head.SlotNumber = g.SlotNumber
			if head.SlotNumber == nil {
				head.SlotNumber = new(uint64)
			}
			head.BlockAccessListHash = &types.EmptyBlockAccessListHash
		}
	}
	return types.NewBlock(head, &types.Body{Withdrawals: withdrawals}, nil, trie.NewStackTrie(nil))
}

// Commit writes the block and state of a genesis specification to the database.
// The block is committed as the canonical head block.
func (g *Genesis) Commit(db ethdb.Database, triedb *triedb.Database, tracer *tracing.Hooks) (*types.Block, error) {
	if g.Number != 0 {
		return nil, errors.New("can't commit genesis block with number > 0")
	}
	config := g.Config
	if config == nil {
		return nil, errors.New("invalid genesis without chain config")
	}
	if err := config.CheckConfigForkOrder(); err != nil {
		return nil, err
	}
	if config.Clique != nil && len(g.ExtraData) < 32+crypto.SignatureLength {
		return nil, errors.New("can't start clique chain without signers")
	}
	// flush the data to disk and compute the state root
	root, err := flushAlloc(&g.Alloc, triedb, tracer)
	if err != nil {
		return nil, err
	}
	block := g.toBlockWithRoot(root)

	// Marshal the genesis state specification and persist.
	blob, err := json.Marshal(g.Alloc)
	if err != nil {
		return nil, err
	}
	batch := db.NewBatch()
	rawdb.WriteGenesisStateSpec(batch, block.Hash(), blob)
	rawdb.WriteBlock(batch, block)
	rawdb.WriteReceipts(batch, block.Hash(), block.NumberU64(), nil)
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteHeadBlockHash(batch, block.Hash())
	rawdb.WriteHeadFastBlockHash(batch, block.Hash())
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	rawdb.WriteChainConfig(batch, block.Hash(), config)
	return block, batch.Write()
}

// MustCommit writes the genesis block and state to db, panicking on error.
// The block is committed as the canonical head block.
func (g *Genesis) MustCommit(db ethdb.Database, triedb *triedb.Database) *types.Block {
	block, err := g.Commit(db, triedb, nil)
	if err != nil {
		panic(err)
	}
	return block
}

// EnableUBTAtGenesis indicates whether the verkle fork should be activated
// at genesis. This is a temporary solution only for verkle devnet testing, where
// verkle fork is activated at genesis, and the configured activation date has
// already passed.
//
// In production networks (mainnet and public testnets), verkle activation always
// occurs after the genesis block, making this function irrelevant in those cases.
func EnableUBTAtGenesis(db ethdb.Database, genesis *Genesis) (bool, error) {
	if genesis != nil {
		if genesis.Config == nil {
			return false, errGenesisNoConfig
		}
		return genesis.Config.EnableUBTAtGenesis, nil
	}
	if ghash := rawdb.ReadCanonicalHash(db, 0); ghash != (common.Hash{}) {
		chainCfg := rawdb.ReadChainConfig(db, ghash)
		if chainCfg != nil {
			return chainCfg.EnableUBTAtGenesis, nil
		}
	}
	return false, nil
}

// DefaultGenesisBlock returns the Ethereum main net genesis block.
func DefaultGenesisBlock() *Genesis {
	return &Genesis{
		Config:     params.MainnetChainConfig,
		Nonce:      66,
		ExtraData:  hexutil.MustDecode("0x11bbe8db4e347b4e8c937c1c8370e4b5ed33adb3db69cbdb7a38e1e50b1b82fa"),
		GasLimit:   5000,
		Difficulty: big.NewInt(17179869184),
		Alloc:      decodePrealloc(mainnetAllocData),
	}
}

// DefaultSepoliaGenesisBlock returns the Sepolia network genesis block.
func DefaultSepoliaGenesisBlock() *Genesis {
	return &Genesis{
		Config:     params.SepoliaChainConfig,
		Nonce:      0,
		ExtraData:  []byte("Sepolia, Athens, Attica, Greece!"),
		GasLimit:   0x1c9c380,
		Difficulty: big.NewInt(0x20000),
		Timestamp:  1633267481,
		Alloc:      decodePrealloc(sepoliaAllocData),
	}
}

// DefaultHoleskyGenesisBlock returns the Holesky network genesis block.
func DefaultHoleskyGenesisBlock() *Genesis {
	return &Genesis{
		Config:     params.HoleskyChainConfig,
		Nonce:      0x1234,
		GasLimit:   0x17d7840,
		Difficulty: big.NewInt(0x01),
		Timestamp:  1695902100,
		Alloc:      decodePrealloc(holeskyAllocData),
	}
}

// DefaultHoodiGenesisBlock returns the Hoodi network genesis block.
func DefaultHoodiGenesisBlock() *Genesis {
	return &Genesis{
		Config:     params.HoodiChainConfig,
		Nonce:      0x1234,
		GasLimit:   0x2255100,
		Difficulty: big.NewInt(0x01),
		Timestamp:  1742212800,
		Alloc:      decodePrealloc(hoodiAllocData),
	}
}

// DeveloperGenesisBlock returns the 'geth --dev' genesis block.
func DeveloperGenesisBlock(gasLimit uint64, faucet *common.Address) *Genesis {
	// Override the default period to the user requested one
	config := *params.AllDevChainProtocolChanges

	// Assemble and return the genesis with the precompiles and faucet pre-funded
	genesis := &Genesis{
		Config:     &config,
		GasLimit:   gasLimit,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Difficulty: big.NewInt(0),
		Alloc: map[common.Address]types.Account{
			common.BytesToAddress([]byte{0x01}):    {Balance: big.NewInt(1)}, // ECRecover
			common.BytesToAddress([]byte{0x02}):    {Balance: big.NewInt(1)}, // SHA256
			common.BytesToAddress([]byte{0x03}):    {Balance: big.NewInt(1)}, // RIPEMD
			common.BytesToAddress([]byte{0x04}):    {Balance: big.NewInt(1)}, // Identity
			common.BytesToAddress([]byte{0x05}):    {Balance: big.NewInt(1)}, // ModExp
			common.BytesToAddress([]byte{0x06}):    {Balance: big.NewInt(1)}, // ECAdd
			common.BytesToAddress([]byte{0x07}):    {Balance: big.NewInt(1)}, // ECScalarMul
			common.BytesToAddress([]byte{0x08}):    {Balance: big.NewInt(1)}, // ECPairing
			common.BytesToAddress([]byte{0x09}):    {Balance: big.NewInt(1)}, // BLAKE2b
			common.BytesToAddress([]byte{0x0a}):    {Balance: big.NewInt(1)}, // KZGPointEval
			common.BytesToAddress([]byte{0x0b}):    {Balance: big.NewInt(1)}, // BLSG1Add
			common.BytesToAddress([]byte{0x0c}):    {Balance: big.NewInt(1)}, // BLSG1MultiExp
			common.BytesToAddress([]byte{0x0d}):    {Balance: big.NewInt(1)}, // BLSG2Add
			common.BytesToAddress([]byte{0x0e}):    {Balance: big.NewInt(1)}, // BLSG2MultiExp
			common.BytesToAddress([]byte{0x0f}):    {Balance: big.NewInt(1)}, // BLSG1Pairing
			common.BytesToAddress([]byte{0x10}):    {Balance: big.NewInt(1)}, // BLSG1MapG1
			common.BytesToAddress([]byte{0x11}):    {Balance: big.NewInt(1)}, // BLSG2MapG2
			common.BytesToAddress([]byte{0x1, 00}): {Balance: big.NewInt(1)}, // P256Verify
			// Pre-deploy system contracts
			params.BeaconRootsAddress:        {Nonce: 1, Code: params.BeaconRootsCode, Balance: common.Big0},
			params.HistoryStorageAddress:     {Nonce: 1, Code: params.HistoryStorageCode, Balance: common.Big0},
			params.WithdrawalQueueAddress:    {Nonce: 1, Code: params.WithdrawalQueueCode, Balance: common.Big0},
			params.ConsolidationQueueAddress: {Nonce: 1, Code: params.ConsolidationQueueCode, Balance: common.Big0},
			// EIP-8282 - Builder Execution Requests
			params.BuilderDepositAddress: {Nonce: 1, Code: params.BuilderDepositCode, Balance: common.Big0},
			params.BuilderExitAddress:    {Nonce: 1, Code: params.BuilderExitCode, Balance: common.Big0},
			// EIP-7997 - Deterministic deployment factory
			params.DeterministicFactoryAddress: {Nonce: 1, Code: params.DeterministicFactoryCode, Balance: common.Big0},
		},
	}
	if faucet != nil {
		genesis.Alloc[*faucet] = types.Account{Balance: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(9))}
	}
	return genesis
}

func decodePrealloc(data string) types.GenesisAlloc {
	var p []struct {
		Addr    *big.Int
		Balance *big.Int
		Misc    *struct {
			Nonce uint64
			Code  []byte
			Slots []struct {
				Key common.Hash
				Val common.Hash
			}
		} `rlp:"optional"`
	}
	if err := rlp.NewStream(strings.NewReader(data), 0).Decode(&p); err != nil {
		panic(err)
	}
	ga := make(types.GenesisAlloc, len(p))
	for _, account := range p {
		acc := types.Account{Balance: account.Balance}
		if account.Misc != nil {
			acc.Nonce = account.Misc.Nonce
			acc.Code = account.Misc.Code

			acc.Storage = make(map[common.Hash]common.Hash)
			for _, slot := range account.Misc.Slots {
				acc.Storage[slot.Key] = slot.Val
			}
		}
		ga[common.BigToAddress(account.Addr)] = acc
	}
	return ga
}
