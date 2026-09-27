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

// Package validator provides XDC Network validator contract interface
package validator

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

// XDCValidatorAddress is the fixed address for the XDC Validator contract
var XDCValidatorAddress = common.HexToAddress("0x0000000000000000000000000000000000000088")

// Validator wraps the XDC validator contract
type Validator struct {
	*XDCValidatorSession
	contractBackend bind.ContractBackend
}

// NewValidator creates a new instance of the validator contract
func NewValidator(transactOpts *bind.TransactOpts, contractAddr common.Address, contractBackend bind.ContractBackend) (*Validator, error) {
	validator, err := NewXDCValidator(contractAddr, contractBackend)
	if err != nil {
		return nil, err
	}

	return &Validator{
		&XDCValidatorSession{
			Contract:     validator,
			TransactOpts: *transactOpts,
		},
		contractBackend,
	}, nil
}

// DeployValidator deploys a new XDC validator contract
func DeployValidator(
	transactOpts *bind.TransactOpts,
	contractBackend bind.ContractBackend,
	candidates []common.Address,
	caps []*big.Int,
	firstOwner common.Address,
	minCandidateCap *big.Int,
	minVoterCap *big.Int,
	maxValidatorNumber *big.Int,
	candidateWithdrawDelay *big.Int,
	voterWithdrawDelay *big.Int,
) (common.Address, *Validator, error) {
	validatorAddr, _, _, err := DeployXDCValidator(
		transactOpts,
		contractBackend,
		candidates,
		caps,
		firstOwner,
		minCandidateCap,
		minVoterCap,
		maxValidatorNumber,
		candidateWithdrawDelay,
		voterWithdrawDelay,
	)
	if err != nil {
		return validatorAddr, nil, err
	}

	validator, err := NewValidator(transactOpts, validatorAddr, contractBackend)
	if err != nil {
		return validatorAddr, nil, err
	}

	return validatorAddr, validator, nil
}
