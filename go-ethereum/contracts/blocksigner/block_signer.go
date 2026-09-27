// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package blocksigner

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
)

// BlockSignerABI is the input ABI used to generate the binding from.
const BlockSignerABI = `[{"constant":false,"inputs":[{"name":"_blockNumber","type":"uint256"},{"name":"_blockHash","type":"bytes32"}],"name":"sign","outputs":[],"payable":false,"stateMutability":"nonpayable","type":"function"},{"constant":true,"inputs":[{"name":"_blockHash","type":"bytes32"}],"name":"getSigners","outputs":[{"name":"","type":"address[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"epochNumber","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"inputs":[{"name":"_epochNumber","type":"uint256"}],"payable":false,"stateMutability":"nonpayable","type":"constructor"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_signer","type":"address"},{"indexed":false,"name":"_blockNumber","type":"uint256"},{"indexed":false,"name":"_blockHash","type":"bytes32"}],"name":"Sign","type":"event"}]`

// BlockSignerContract is an auto generated Go binding around an Ethereum contract.
type BlockSignerContract struct {
	BlockSignerContractCaller     // Read-only binding to the contract
	BlockSignerContractTransactor // Write-only binding to the contract
	BlockSignerContractFilterer   // Log filterer for contract events
}

// BlockSignerContractCaller is an auto generated read-only Go binding around an Ethereum contract.
type BlockSignerContractCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BlockSignerContractTransactor is an auto generated write-only Go binding around an Ethereum contract.
type BlockSignerContractTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BlockSignerContractFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type BlockSignerContractFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BlockSignerSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type BlockSignerSession struct {
	Contract     *BlockSignerContract // Generic contract binding to set the session for
	CallOpts     bind.CallOpts        // Call options to use throughout this session
	TransactOpts bind.TransactOpts    // Transaction auth options to use throughout this session
}

// BlockSignerContractCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type BlockSignerContractCallerSession struct {
	Contract *BlockSignerContractCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts              // Call options to use throughout this session
}

// BlockSignerContractTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type BlockSignerContractTransactorSession struct {
	Contract     *BlockSignerContractTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts              // Transaction auth options to use throughout this session
}

// BlockSignerContractRaw is an auto generated low-level Go binding around an Ethereum contract.
type BlockSignerContractRaw struct {
	Contract *BlockSignerContract // Generic contract binding to access the raw methods on
}

// BlockSignerContractCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type BlockSignerContractCallerRaw struct {
	Contract *BlockSignerContractCaller // Generic read-only contract binding to access the raw methods on
}

// BlockSignerContractTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type BlockSignerContractTransactorRaw struct {
	Contract *BlockSignerContractTransactor // Generic write-only contract binding to access the raw methods on
}

// NewBlockSignerContract creates a new instance of BlockSignerContract, bound to a specific deployed contract.
func NewBlockSignerContract(address common.Address, backend bind.ContractBackend) (*BlockSignerContract, error) {
	contract, err := bindBlockSignerContract(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &BlockSignerContract{BlockSignerContractCaller: BlockSignerContractCaller{contract: contract}, BlockSignerContractTransactor: BlockSignerContractTransactor{contract: contract}, BlockSignerContractFilterer: BlockSignerContractFilterer{contract: contract}}, nil
}

// NewBlockSignerContractCaller creates a new read-only instance of BlockSignerContract, bound to a specific deployed contract.
func NewBlockSignerContractCaller(address common.Address, caller bind.ContractCaller) (*BlockSignerContractCaller, error) {
	contract, err := bindBlockSignerContract(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &BlockSignerContractCaller{contract: contract}, nil
}

// NewBlockSignerContractTransactor creates a new write-only instance of BlockSignerContract, bound to a specific deployed contract.
func NewBlockSignerContractTransactor(address common.Address, transactor bind.ContractTransactor) (*BlockSignerContractTransactor, error) {
	contract, err := bindBlockSignerContract(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &BlockSignerContractTransactor{contract: contract}, nil
}

// NewBlockSignerContractFilterer creates a new log filterer instance of BlockSignerContract, bound to a specific deployed contract.
func NewBlockSignerContractFilterer(address common.Address, filterer bind.ContractFilterer) (*BlockSignerContractFilterer, error) {
	contract, err := bindBlockSignerContract(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &BlockSignerContractFilterer{contract: contract}, nil
}

// bindBlockSignerContract binds a generic wrapper to an already deployed contract.
func bindBlockSignerContract(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := abi.JSON(strings.NewReader(BlockSignerABI))
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_BlockSignerContract *BlockSignerContractRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _BlockSignerContract.Contract.BlockSignerContractCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_BlockSignerContract *BlockSignerContractRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _BlockSignerContract.Contract.BlockSignerContractTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_BlockSignerContract *BlockSignerContractRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _BlockSignerContract.Contract.BlockSignerContractTransactor.contract.Transact(opts, method, params...)
}

// GetSigners is a free data retrieval call binding the contract method 0xe7ec6aef.
//
// Solidity: function getSigners(bytes32 _blockHash) view returns(address[])
func (_BlockSignerContract *BlockSignerContractCaller) GetSigners(opts *bind.CallOpts, _blockHash [32]byte) ([]common.Address, error) {
	var out []interface{}
	err := _BlockSignerContract.contract.Call(opts, &out, "getSigners", _blockHash)

	if err != nil {
		return *new([]common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)

	return out0, err
}

// GetSigners is a free data retrieval call binding the contract method 0xe7ec6aef.
//
// Solidity: function getSigners(bytes32 _blockHash) view returns(address[])
func (_BlockSignerContract *BlockSignerSession) GetSigners(_blockHash [32]byte) ([]common.Address, error) {
	return _BlockSignerContract.Contract.GetSigners(&_BlockSignerContract.CallOpts, _blockHash)
}

// GetSigners is a free data retrieval call binding the contract method 0xe7ec6aef.
//
// Solidity: function getSigners(bytes32 _blockHash) view returns(address[])
func (_BlockSignerContract *BlockSignerContractCallerSession) GetSigners(_blockHash [32]byte) ([]common.Address, error) {
	return _BlockSignerContract.Contract.GetSigners(&_BlockSignerContract.CallOpts, _blockHash)
}

// EpochNumber is a free data retrieval call binding the contract method 0xf4145a83.
//
// Solidity: function epochNumber() view returns(uint256)
func (_BlockSignerContract *BlockSignerContractCaller) EpochNumber(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _BlockSignerContract.contract.Call(opts, &out, "epochNumber")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// EpochNumber is a free data retrieval call binding the contract method 0xf4145a83.
//
// Solidity: function epochNumber() view returns(uint256)
func (_BlockSignerContract *BlockSignerSession) EpochNumber() (*big.Int, error) {
	return _BlockSignerContract.Contract.EpochNumber(&_BlockSignerContract.CallOpts)
}

// EpochNumber is a free data retrieval call binding the contract method 0xf4145a83.
//
// Solidity: function epochNumber() view returns(uint256)
func (_BlockSignerContract *BlockSignerContractCallerSession) EpochNumber() (*big.Int, error) {
	return _BlockSignerContract.Contract.EpochNumber(&_BlockSignerContract.CallOpts)
}

// Sign is a paid mutator transaction binding the contract method 0xe341eaa4.
//
// Solidity: function sign(uint256 _blockNumber, bytes32 _blockHash) returns()
func (_BlockSignerContract *BlockSignerContractTransactor) Sign(opts *bind.TransactOpts, _blockNumber *big.Int, _blockHash [32]byte) (*types.Transaction, error) {
	return _BlockSignerContract.contract.Transact(opts, "sign", _blockNumber, _blockHash)
}

// Sign is a paid mutator transaction binding the contract method 0xe341eaa4.
//
// Solidity: function sign(uint256 _blockNumber, bytes32 _blockHash) returns()
func (_BlockSignerContract *BlockSignerSession) Sign(_blockNumber *big.Int, _blockHash [32]byte) (*types.Transaction, error) {
	return _BlockSignerContract.Contract.Sign(&_BlockSignerContract.TransactOpts, _blockNumber, _blockHash)
}

// Sign is a paid mutator transaction binding the contract method 0xe341eaa4.
//
// Solidity: function sign(uint256 _blockNumber, bytes32 _blockHash) returns()
func (_BlockSignerContract *BlockSignerContractTransactorSession) Sign(_blockNumber *big.Int, _blockHash [32]byte) (*types.Transaction, error) {
	return _BlockSignerContract.Contract.Sign(&_BlockSignerContract.TransactOpts, _blockNumber, _blockHash)
}

// DeployBlockSignerContract deploys a new instance of the BlockSignerContract.
func DeployBlockSignerContract(auth *bind.TransactOpts, backend bind.ContractBackend, _epochNumber *big.Int) (common.Address, *types.Transaction, *BlockSignerContract, error) {
	parsed, err := abi.JSON(strings.NewReader(BlockSignerABI))
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	// Note: In production, you would include the bytecode here
	// For now, we return an error as deployment requires bytecode
	_ = parsed
	return common.Address{}, nil, nil, errors.New("deployment requires bytecode - use pre-deployed contract")
}
