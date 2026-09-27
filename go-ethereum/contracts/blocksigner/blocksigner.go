// Copyright (c) 2018 XDPoSChain
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

// Package blocksigner provides XDC Network block signer contract interface
package blocksigner

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

// BlockSignerAddress is the fixed address for the XDC BlockSigner contract
var BlockSignerAddress = common.HexToAddress("0x0000000000000000000000000000000000000089")

// BlockSigner wraps the XDC block signer contract
type BlockSigner struct {
	*BlockSignerSession
	contractBackend bind.ContractBackend
}

// NewBlockSigner creates a new instance of the block signer contract
func NewBlockSigner(transactOpts *bind.TransactOpts, contractAddr common.Address, contractBackend bind.ContractBackend) (*BlockSigner, error) {
	blockSigner, err := NewBlockSignerContract(contractAddr, contractBackend)
	if err != nil {
		return nil, err
	}

	return &BlockSigner{
		&BlockSignerSession{
			Contract:     blockSigner,
			TransactOpts: *transactOpts,
		},
		contractBackend,
	}, nil
}

// DeployBlockSigner deploys a new XDC block signer contract
func DeployBlockSigner(transactOpts *bind.TransactOpts, contractBackend bind.ContractBackend, epochNumber *big.Int) (common.Address, *BlockSigner, error) {
	blockSignerAddr, _, _, err := DeployBlockSignerContract(transactOpts, contractBackend, epochNumber)
	if err != nil {
		return blockSignerAddr, nil, err
	}

	blockSigner, err := NewBlockSigner(transactOpts, blockSignerAddr, contractBackend)
	if err != nil {
		return blockSignerAddr, nil, err
	}

	return blockSignerAddr, blockSigner, nil
}
