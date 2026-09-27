// Copyright 2021 The go-ethereum Authors
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

package types

import (
	"bytes"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// NOTE: go:generate for rlpgen is disabled — EncodeRLP/DecodeRLP are now
// hand-written in gen_account_rlp.go to support balances > 2^256-1 (XDC).
// OLD: //go:generate go run ../../rlp/rlpgen -type StateAccount -out gen_account_rlp.go

// StateAccount is the Ethereum consensus representation of accounts.
// These objects are stored in the main account trie.
type StateAccount struct {
	Nonce    uint64
	Balance  *uint256.Int
	Root     common.Hash // merkle root of the storage trie
	CodeHash []byte

	// BigBalance holds the true balance when it exceeds MaxUint256.
	// This is needed for XDC chains where genesis accounts have 2^256-1
	// balance and rewards push the balance beyond uint256 range.
	// When BigBalance is non-nil, it is used for RLP encoding instead
	// of Balance. Balance still holds the modular (wrapped) value for
	// uint256-based operations.
	BigBalance *big.Int `rlp:"-"`
}

// NewEmptyStateAccount constructs an empty state account.
func NewEmptyStateAccount() *StateAccount {
	return &StateAccount{
		Balance:  new(uint256.Int),
		Root:     EmptyRootHash,
		CodeHash: EmptyCodeHash.Bytes(),
	}
}

// Copy returns a deep-copied state account object.
func (acct *StateAccount) Copy() *StateAccount {
	var balance *uint256.Int
	if acct.Balance != nil {
		balance = new(uint256.Int).Set(acct.Balance)
	}
	var bigBal *big.Int
	if acct.BigBalance != nil {
		bigBal = new(big.Int).Set(acct.BigBalance)
	}
	return &StateAccount{
		Nonce:      acct.Nonce,
		Balance:    balance,
		Root:       acct.Root,
		CodeHash:   common.CopyBytes(acct.CodeHash),
		BigBalance: bigBal,
	}
}

// SlimAccount is a modified version of an Account, where the root is replaced
// with a byte slice. This format can be used to represent full-consensus format
// or slim format which replaces the empty root and code hash as nil byte slice.
type SlimAccount struct {
	Nonce      uint64
	Balance    *uint256.Int
	BigBalance *big.Int `rlp:"-"` // Holds true balance when it exceeds 2^256-1 (XDC overflow) - NOT encoded in RLP
	Root       []byte   // Nil if root equals to types.EmptyRootHash
	CodeHash   []byte   // Nil if hash equals to types.EmptyCodeHash
}

// slimAccountBigBal is a helper struct for encoding SlimAccount with big.Int balance.
// Used when the balance exceeds uint256 (XDC overflow scenario).
type slimAccountBigBal struct {
	Nonce    uint64
	Balance  *big.Int
	Root     []byte
	CodeHash []byte
}

// SlimAccountRLP encodes the state account in 'slim RLP' format.
func SlimAccountRLP(account StateAccount) []byte {
	// If BigBalance is set (overflow), encode with big.Int to preserve full value
	if account.BigBalance != nil {
		slim := slimAccountBigBal{
			Nonce:   account.Nonce,
			Balance: account.BigBalance,
		}
		if account.Root != EmptyRootHash {
			slim.Root = account.Root[:]
		}
		if !bytes.Equal(account.CodeHash, EmptyCodeHash[:]) {
			slim.CodeHash = account.CodeHash
		}
		data, err := rlp.EncodeToBytes(slim)
		if err != nil {
			panic(err)
		}
		return data
	}

	slim := SlimAccount{
		Nonce:   account.Nonce,
		Balance: account.Balance,
	}
	if account.Root != EmptyRootHash {
		slim.Root = account.Root[:]
	}
	if !bytes.Equal(account.CodeHash, EmptyCodeHash[:]) {
		slim.CodeHash = account.CodeHash
	}
	data, err := rlp.EncodeToBytes(slim)
	if err != nil {
		panic(err)
	}
	return data
}

// slimAccountBigBalDecode is a helper struct for decoding slim accounts
// where the balance might exceed uint256 (XDC overflow scenario).
type slimAccountBigBalDecode struct {
	Nonce    uint64
	Balance  *big.Int
	Root     []byte
	CodeHash []byte
}

// FullAccount decodes the data on the 'slim RLP' format and returns
// the consensus format account.
func FullAccount(data []byte) (*StateAccount, error) {
	// First try decoding with big.Int balance to handle >256-bit values
	var slimBig slimAccountBigBalDecode
	if err := rlp.DecodeBytes(data, &slimBig); err != nil {
		return nil, err
	}
	var account StateAccount
	account.Nonce = slimBig.Nonce

	// Check if the balance fits in uint256
	bal, overflow := uint256.FromBig(slimBig.Balance)
	if overflow {
		// Balance exceeds uint256 — store in BigBalance
		account.Balance = bal // modular value
		account.BigBalance = slimBig.Balance
	} else {
		account.Balance = bal
	}

	// Interpret the storage root and code hash in slim format.
	if len(slimBig.Root) == 0 {
		account.Root = EmptyRootHash
	} else {
		account.Root = common.BytesToHash(slimBig.Root)
	}
	if len(slimBig.CodeHash) == 0 {
		account.CodeHash = EmptyCodeHash[:]
	} else {
		account.CodeHash = slimBig.CodeHash
	}
	return &account, nil
}

// FullAccountRLP converts data on the 'slim RLP' format into the full RLP-format.
func FullAccountRLP(data []byte) ([]byte, error) {
	account, err := FullAccount(data)
	if err != nil {
		return nil, err
	}
	return rlp.EncodeToBytes(account)
}

// ToSlimAccount converts a StateAccount to a SlimAccount, preserving BigBalance.
func (a *StateAccount) ToSlimAccount() *SlimAccount {
	slim := &SlimAccount{
		Nonce:      a.Nonce,
		Balance:    a.Balance,
		BigBalance: a.BigBalance, // Preserve overflow balance
	}
	if a.Root != EmptyRootHash {
		slim.Root = a.Root[:]
	}
	if !bytes.Equal(a.CodeHash, EmptyCodeHash[:]) {
		slim.CodeHash = a.CodeHash
	}
	return slim
}

// DecodeSlimAccount decodes slim RLP data handling >256-bit balances (XDC).
func DecodeSlimAccount(data []byte) (*SlimAccount, error) {
	full, err := FullAccount(data)
	if err != nil {
		return nil, err
	}
	return full.ToSlimAccount(), nil
}
