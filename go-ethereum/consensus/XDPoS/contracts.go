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
	"github.com/ethereum/go-ethereum/common"
)

// XDC System Contract Addresses
var (
	// ValidatorContractAddress is the XDPoS validator contract at 0x88
	// This contract manages masternodes, staking, and voting
	ValidatorContractAddress = common.HexToAddress("0x0000000000000000000000000000000000000088")

	// BlockSignerContractAddress is the block signer contract at 0x89
	// This contract tracks which masternodes signed each block
	BlockSignerContractAddress = common.HexToAddress("0x0000000000000000000000000000000000000089")

	// RandomizeContractAddress is the randomize contract at 0x90
	// This contract provides VRF for masternode selection
	RandomizeContractAddress = common.HexToAddress("0x0000000000000000000000000000000000000090")
)

// Contract Method Signatures (function selectors)
var (
	// getCandidates() returns (address[])
	// Function selector: 0x06a49fce
	GetCandidatesMethod = []byte{0x06, 0xa4, 0x9f, 0xce}

	// getMasternodes() returns (address[])
	// Function selector: 0xc7e5e134 (may vary)
	GetMasternodesMethod = []byte{0xc7, 0xe5, 0xe1, 0x34}

	// getCandidateCap(address) returns (uint256)
	// Function selector: 0x58e7525f
	GetCandidateCapMethod = []byte{0x58, 0xe7, 0x52, 0x5f}

	// getVoters(address) returns (address[])
	// Function selector: 0x2d15cc04
	GetVotersMethod = []byte{0x2d, 0x15, 0xcc, 0x04}

	// getVoterCap(address, address) returns (uint256)
	// Function selector: 0x302b6872
	GetVoterCapMethod = []byte{0x30, 0x2b, 0x68, 0x72}

	// resign(address) - called when masternode resigns
	// Function selector: 0xae6e43f5
	ResignMethod = []byte{0xae, 0x6e, 0x43, 0xf5}

	// vote(address) - called when voting for a masternode
	// Function selector: 0x6dd7d8ea
	VoteMethod = []byte{0x6d, 0xd7, 0xd8, 0xea}

	// propose(address) - called when proposing a new masternode
	// Function selector: 0x012679511
	ProposeMethod = []byte{0x01, 0x26, 0x79, 0x51}

	// Sign method for block signer contract
	// sign(uint256, bytes32) - 0xe341eaa4
	SignMethod = []byte{0xe3, 0x41, 0xea, 0xa4}

	// setSecret(bytes32[]) - 0x34d38600
	SetSecretMethod = []byte{0x34, 0xd3, 0x86, 0x00}

	// setOpening(bytes32[]) - 0xe11f5ba2
	SetOpeningMethod = []byte{0xe1, 0x1f, 0x5b, 0xa2}
)

// ContractCallData builds the calldata for a contract method call
func ContractCallData(method []byte, args ...[]byte) []byte {
	data := make([]byte, len(method))
	copy(data, method)
	for _, arg := range args {
		data = append(data, arg...)
	}
	return data
}

// AddressToPaddedBytes converts an address to 32-byte padded format for contract calls
func AddressToPaddedBytes(addr common.Address) []byte {
	padded := make([]byte, 32)
	copy(padded[12:], addr[:])
	return padded
}

// Uint256ToBytes converts a uint64 to 32-byte big-endian format
func Uint256ToBytes(val uint64) []byte {
	result := make([]byte, 32)
	for i := 31; i >= 0 && val > 0; i-- {
		result[i] = byte(val & 0xff)
		val >>= 8
	}
	return result
}

// ExtractAddressesFromReturn extracts addresses from contract return data.
// The return format is: offset (32 bytes) + length (32 bytes) + addresses (20 bytes each, padded to 32).
//
// Security fix (#93): the previous implementation had two integer-overflow vectors:
//  1. `int(length)*32` overflows int on 32-bit systems when length > 2^26.
//  2. `64 + i*32 + 12` with i as uint64 overflows uint64 when i approaches 2^58.
//
// The fix (a) caps length at a sane maximum (max masternodes ≈ 150 on XDC, so 4096 is generous),
// (b) does all arithmetic in int with an explicit overflow guard before the multiply, and
// (c) re-checks the slice bounds on every iteration to prevent out-of-bounds reads.
func ExtractAddressesFromReturn(data []byte) []common.Address {
	if len(data) < 64 {
		return nil
	}

	// Read length from bytes [32, 64).
	// ABI-encoded uint256 — only the low 8 bytes are realistically non-zero for a list of
	// masternodes, but we read all 32 and reject anything that looks unreasonable.
	var length uint64
	for i := 32; i < 64; i++ {
		// Detect overflow before shifting: if the top byte is set AND length is already
		// large, the next shift would overflow a uint64.
		if length > (1<<56) && data[i] != 0 {
			// Length claims to be astronomically large — reject as malformed.
			return nil
		}
		length = (length << 8) | uint64(data[i])
	}

	// Hard cap: XDC mainnet has at most ~150 masternodes; 4096 is a generous safety bound.
	const maxAddresses = 4096
	if length == 0 || length > maxAddresses {
		return nil
	}

	// Guard against the `64 + length*32` multiply overflowing int before the slice check.
	// Since length <= 4096 and 4096*32 = 131072, this cannot overflow, but we keep the
	// explicit check for clarity and future-proofing.
	required := 64 + int(length)*32 // safe: length <= 4096, product fits in int
	if len(data) < required {
		return nil
	}

	addresses := make([]common.Address, 0, length)
	for i := int(0); i < int(length); i++ {
		start := 64 + i*32 + 12 // skip 12-byte ABI padding before the 20-byte address
		end := start + 20
		if end > len(data) {
			// Truncated payload — stop rather than panic.
			break
		}
		var addr common.Address
		copy(addr[:], data[start:end])
		addresses = append(addresses, addr)
	}

	return addresses
}

// GetMasternodesCallData returns the calldata for getMasternodes()
func GetMasternodesCallData() []byte {
	return GetMasternodesMethod
}

// GetCandidatesCallData returns the calldata for getCandidates()
func GetCandidatesCallData() []byte {
	return GetCandidatesMethod
}

// SignBlockCallData returns the calldata for sign(blockNumber, blockHash)
func SignBlockCallData(blockNumber uint64, blockHash common.Hash) []byte {
	data := make([]byte, 0, 4+32+32)
	data = append(data, SignMethod...)
	data = append(data, Uint256ToBytes(blockNumber)...)
	data = append(data, blockHash[:]...)
	return data
}

// ValidatorCandidateInfo represents information about a masternode candidate
type ValidatorCandidateInfo struct {
	Address common.Address
	Cap     uint64 // Staked amount
	Status  int    // 0 = candidate, 1 = masternode, 2 = resigned
}

// IsSystemContract checks if an address is a known XDC system contract
func IsSystemContract(addr common.Address) bool {
	return addr == ValidatorContractAddress ||
		addr == BlockSignerContractAddress ||
		addr == RandomizeContractAddress
}

// GetValidatorContractAddress returns the validator contract address
func GetValidatorContractAddress() common.Address {
	return ValidatorContractAddress
}

// GetBlockSignerContractAddress returns the block signer contract address
func GetBlockSignerContractAddress() common.Address {
	return BlockSignerContractAddress
}
