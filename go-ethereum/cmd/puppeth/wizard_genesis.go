// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// crypto.SignatureLength sentinel (V1 seal = 65 bytes).
const sealLength = crypto.SignatureLength

// makeGenesis walks the operator through XDPoS genesis assembly.
//
// What this generates:
//   - chain config: chainId, fork activation blocks, XDPoS engine config (V1 + V2)
//   - extraData: 32B vanity + signers + 65B seal placeholder (V1 layout)
//   - alloc: precompile addresses, system contract addresses (with empty code,
//     see note below), funded operator accounts
//
// What this does NOT generate (out of scope for this port; see #595):
//   - System contract bytecode and pre-populated storage for the four XDC
//     system contracts (XDCValidator 0x...88, BlockSigner 0x...89, Randomize
//     0x...90, MultiSig 0x...99). The legacy XDPoSChain puppeth deployed
//     these via a SimulatedBackend then captured the resulting code+storage.
//     That code path uses APIs that have shifted vs the upstream geth 1.17
//     base, and the storage extractor in legacy puppeth has a known bug
//     (RLP-trim corrupts multi-byte slots). For chains that need the system
//     contracts pre-deployed (validator contract callable, etc.), use the
//     patched legacy puppeth standalone — see docs/MINING_WIREUP.md for the
//     reproduction recipe.
//
// V1 consensus is functional without the system contracts (sealing reads
// signers from extraData / the snapshot, not from on-chain contracts).
func (w *wizard) makeGenesis() {
	genesis := &core.Genesis{
		Timestamp:  uint64(time.Now().Unix()),
		GasLimit:   4700000,
		Difficulty: big.NewInt(524288),
		Alloc:      make(types.GenesisAlloc),
		Config: &params.ChainConfig{
			HomesteadBlock: big.NewInt(0),
			EIP150Block:    big.NewInt(0),
			EIP155Block:    big.NewInt(0),
			EIP158Block:    big.NewInt(0),
			ByzantiumBlock: big.NewInt(0),
		},
	}

	// Figure out which consensus engine to choose
	fmt.Println()
	fmt.Println("Which consensus engine to use? (default = XDPoS)")
	fmt.Println(" 1. Ethash - proof-of-work")
	fmt.Println(" 2. Clique - proof-of-authority")
	fmt.Println(" 3. XDPoS - delegated-proof-of-stake")

	choice := w.read()
	switch {
	case choice == "1":
		genesis.Config.Ethash = new(params.EthashConfig)
		genesis.ExtraData = make([]byte, 32)

	case choice == "2":
		genesis.Difficulty = big.NewInt(1)
		genesis.Config.Clique = &params.CliqueConfig{
			Period: 15,
			Epoch:  900,
		}
		fmt.Println()
		fmt.Println("How many seconds should blocks take? (default = 15)")
		genesis.Config.Clique.Period = uint64(w.readDefaultInt(15))

		fmt.Println()
		fmt.Println("Which accounts are allowed to seal? (mandatory at least one)")
		signers := w.readSignerList()
		genesis.ExtraData = make([]byte, 32+len(signers)*common.AddressLength+sealLength)
		for i, signer := range signers {
			copy(genesis.ExtraData[32+i*common.AddressLength:], signer[:])
		}

	case choice == "" || choice == "3":
		w.configureXDPoSGenesis(genesis)

	default:
		log.Crit("Invalid consensus engine choice", "choice", choice)
	}

	// Consensus all set, ask for initial funds and chain ID
	fmt.Println()
	fmt.Println("Which accounts should be pre-funded? (advisable at least one)")
	for {
		if address := w.readAddress(); address != nil {
			genesis.Alloc[*address] = types.Account{
				Balance: new(big.Int).Lsh(big.NewInt(1), 256-7), // 2^256 / 128
			}
			continue
		}
		break
	}
	// Pre-allocate a batch of precompile addresses so they don't get culled
	for i := int64(0); i < 11; i++ {
		genesis.Alloc[common.BigToAddress(big.NewInt(i))] = types.Account{Balance: big.NewInt(0)}
	}

	fmt.Println()
	fmt.Println("Specify your chain/network ID if you want an explicit one (default = random)")
	genesis.Config.ChainID = new(big.Int).SetUint64(uint64(w.readDefaultInt(rand.Intn(65536))))

	log.Info("Configured new genesis block")
	w.conf.Genesis = genesis
	w.conf.flush()
}

// configureXDPoSGenesis fills in the XDPoS-specific config and extraData.
//
// V2.SwitchBlock is constrained to a multiple of Epoch to satisfy the engine's
// startup assertion (see consensus/XDPoS/xdpos.go New). Legacy puppeth let you
// pick any value, then geth would panic at boot.
func (w *wizard) configureXDPoSGenesis(genesis *core.Genesis) {
	genesis.Difficulty = big.NewInt(1)
	genesis.Config.XDPoS = &params.XDPoSConfig{
		Period: 2,
		Epoch:  900,
		Reward: 0,
		V2: &params.V2{
			// Default V2 switch at block 1800 (third epoch boundary, two full
			// V1 epochs before V2 takes over). Mirrors mainnet's pattern of
			// having V1 history before the V2 upgrade lands. Override at the
			// prompt below; pick a high value (e.g. 999999900) to keep on V1
			// forever. Issue #603.
			SwitchBlock:   big.NewInt(1800),
			CurrentConfig: &params.V2Config{},
			AllConfigs:    make(map[uint64]*params.V2Config),
		},
	}
	fmt.Println()
	fmt.Println("How many seconds should blocks take? (default = 2)")
	genesis.Config.XDPoS.Period = uint64(w.readDefaultInt(2))
	genesis.Config.XDPoS.V2.CurrentConfig.MinePeriod = int(genesis.Config.XDPoS.Period)

	fmt.Println()
	fmt.Println("How many XDC should be rewarded to masternode per epoch? (default = 5000)")
	genesis.Config.XDPoS.Reward = uint64(w.readDefaultInt(5000))

	fmt.Println()
	fmt.Println("How many blocks per epoch? (default = 900)")
	epoch := uint64(w.readDefaultInt(900))
	genesis.Config.XDPoS.Epoch = epoch
	genesis.Config.XDPoS.RewardCheckpoint = epoch

	fmt.Println()
	fmt.Println("How many blocks before checkpoint to prepare next masternodes? (default = 450)")
	genesis.Config.XDPoS.Gap = uint64(w.readDefaultInt(450))

	fmt.Println()
	fmt.Println("What is the foundation wallet address? (default = xdc0000000000000000000000000000000000000068)")
	genesis.Config.XDPoS.FoudationWalletAddr = w.readDefaultAddress(common.FoudationAddrBinary)

	fmt.Println()
	fmt.Println("At which block should V2 BFT consensus activate? (must be multiple of epoch)")
	fmt.Println("Enter a high value (e.g. 999999900) to keep the chain on V1 forever.")
	for {
		v := w.readDefaultBigInt(genesis.Config.XDPoS.V2.SwitchBlock)
		if v == nil || new(big.Int).Mod(v, big.NewInt(int64(epoch))).Sign() == 0 {
			genesis.Config.XDPoS.V2.SwitchBlock = v
			break
		}
		log.Error("V2 switch block must be a multiple of epoch", "epoch", epoch)
	}
	genesis.Config.XDPoS.V2.CurrentConfig.SwitchRound = 0

	fmt.Println()
	fmt.Println("V2 timeout period in seconds? (default = 10)")
	genesis.Config.XDPoS.V2.CurrentConfig.TimeoutPeriod = w.readDefaultInt(10)

	fmt.Println()
	fmt.Println("V2 timeouts before sending Synchronize? (default = 3)")
	genesis.Config.XDPoS.V2.CurrentConfig.TimeoutSyncThreshold = w.readDefaultInt(3)

	fmt.Println()
	fmt.Printf("V2 QC certificate threshold (fraction, default = %f)\n", 0.667)
	genesis.Config.XDPoS.V2.CurrentConfig.CertThreshold = w.readDefaultFloat(0.667)
	genesis.Config.XDPoS.V2.CurrentConfig.MaxMasternodes = 108
	genesis.Config.XDPoS.V2.AllConfigs[0] = genesis.Config.XDPoS.V2.CurrentConfig

	fmt.Println()
	fmt.Println("Which accounts are allowed to seal (signers)? (mandatory at least one)")
	signers := w.readSignerList()
	genesis.ExtraData = make([]byte, 32+len(signers)*common.AddressLength+sealLength)
	for i, signer := range signers {
		copy(genesis.ExtraData[32+i*common.AddressLength:], signer[:])
	}

	// Note: system contracts (0x...0088, 0x...0089, 0x...0090, 0x...0099) are
	// intentionally NOT pre-deployed here. See the makeGenesis() preamble for
	// the reasoning. V1 consensus runs without them.
	log.Info("XDPoS genesis configured", "signers", len(signers), "v2Switch", genesis.Config.XDPoS.V2.SwitchBlock)
}

// readSignerList prompts for one address per line, ending on a blank line,
// requires at least one address, and returns the list sorted ascending so
// extraData is canonical.
func (w *wizard) readSignerList() []common.Address {
	var signers []common.Address
	for {
		if address := w.readAddress(); address != nil {
			signers = append(signers, *address)
			continue
		}
		if len(signers) > 0 {
			break
		}
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

// manageGenesis edits or exports an existing genesis configuration.
func (w *wizard) manageGenesis() {
	fmt.Println()
	fmt.Println(" 1. Modify existing fork rules")
	fmt.Println(" 2. Export genesis configuration")
	fmt.Println(" 3. Remove genesis configuration")

	choice := w.read()
	switch choice {
	case "1":
		fmt.Println()
		fmt.Printf("Which block should Homestead come into effect? (default = %v)\n", w.conf.Genesis.Config.HomesteadBlock)
		w.conf.Genesis.Config.HomesteadBlock = w.readDefaultBigInt(w.conf.Genesis.Config.HomesteadBlock)

		fmt.Println()
		fmt.Printf("Which block should EIP150 come into effect? (default = %v)\n", w.conf.Genesis.Config.EIP150Block)
		w.conf.Genesis.Config.EIP150Block = w.readDefaultBigInt(w.conf.Genesis.Config.EIP150Block)

		fmt.Println()
		fmt.Printf("Which block should EIP155 come into effect? (default = %v)\n", w.conf.Genesis.Config.EIP155Block)
		w.conf.Genesis.Config.EIP155Block = w.readDefaultBigInt(w.conf.Genesis.Config.EIP155Block)

		fmt.Println()
		fmt.Printf("Which block should EIP158 come into effect? (default = %v)\n", w.conf.Genesis.Config.EIP158Block)
		w.conf.Genesis.Config.EIP158Block = w.readDefaultBigInt(w.conf.Genesis.Config.EIP158Block)

		fmt.Println()
		fmt.Printf("Which block should Byzantium come into effect? (default = %v)\n", w.conf.Genesis.Config.ByzantiumBlock)
		w.conf.Genesis.Config.ByzantiumBlock = w.readDefaultBigInt(w.conf.Genesis.Config.ByzantiumBlock)

		out, _ := json.MarshalIndent(w.conf.Genesis.Config, "", "  ")
		fmt.Printf("Chain configuration updated:\n\n%s\n", out)
		w.conf.flush()

	case "2":
		fmt.Println()
		fmt.Printf("Which file to save the genesis into? (default = %s.json)\n", w.network)
		out, _ := json.MarshalIndent(w.conf.Genesis, "", "  ")
		path := w.readDefaultString(fmt.Sprintf("%s.json", w.network))
		if err := os.WriteFile(path, out, 0644); err != nil {
			log.Error("Failed to save genesis file", "err", err)
			return
		}
		log.Info("Exported genesis block", "path", path)

	case "3":
		w.conf.Genesis = nil
		w.conf.flush()
		log.Info("Genesis configuration removed")

	default:
		log.Error("That's not something I can do")
	}
}
