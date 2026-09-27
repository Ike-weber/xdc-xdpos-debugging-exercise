// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package validator

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

// XDCValidatorABI is the input ABI used to generate the binding from.
const XDCValidatorABI = `[{"constant":false,"inputs":[{"name":"_candidate","type":"address"}],"name":"propose","outputs":[],"payable":true,"stateMutability":"payable","type":"function"},{"constant":true,"inputs":[{"name":"","type":"uint256"}],"name":"owners","outputs":[{"name":"","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"name":"_candidate","type":"address"},{"name":"_cap","type":"uint256"}],"name":"unvote","outputs":[],"payable":false,"stateMutability":"nonpayable","type":"function"},{"constant":true,"inputs":[],"name":"getCandidates","outputs":[{"name":"","type":"address[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"ownerCount","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"name":"_candidate","type":"address"}],"name":"getVoters","outputs":[{"name":"","type":"address[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"name":"_candidate","type":"address"},{"name":"_voter","type":"address"}],"name":"getVoterCap","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"name":"","type":"uint256"}],"name":"candidates","outputs":[{"name":"","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"name":"_blockNumber","type":"uint256"},{"name":"_index","type":"uint256"}],"name":"withdraw","outputs":[],"payable":false,"stateMutability":"nonpayable","type":"function"},{"constant":true,"inputs":[{"name":"_candidate","type":"address"}],"name":"getCandidateCap","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"name":"_candidate","type":"address"}],"name":"vote","outputs":[],"payable":true,"stateMutability":"payable","type":"function"},{"constant":true,"inputs":[],"name":"candidateCount","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"voterWithdrawDelay","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"name":"_candidate","type":"address"}],"name":"resign","outputs":[],"payable":false,"stateMutability":"nonpayable","type":"function"},{"constant":true,"inputs":[{"name":"_candidate","type":"address"}],"name":"getCandidateOwner","outputs":[{"name":"","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"maxValidatorNumber","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"candidateWithdrawDelay","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"name":"_candidate","type":"address"}],"name":"isCandidate","outputs":[{"name":"","type":"bool"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"minCandidateCap","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"getOwnerCount","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"minVoterCap","outputs":[{"name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"inputs":[{"name":"_candidates","type":"address[]"},{"name":"_caps","type":"uint256[]"},{"name":"_firstOwner","type":"address"},{"name":"_minCandidateCap","type":"uint256"},{"name":"_minVoterCap","type":"uint256"},{"name":"_maxValidatorNumber","type":"uint256"},{"name":"_candidateWithdrawDelay","type":"uint256"},{"name":"_voterWithdrawDelay","type":"uint256"}],"payable":false,"stateMutability":"nonpayable","type":"constructor"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_voter","type":"address"},{"indexed":false,"name":"_candidate","type":"address"},{"indexed":false,"name":"_cap","type":"uint256"}],"name":"Vote","type":"event"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_voter","type":"address"},{"indexed":false,"name":"_candidate","type":"address"},{"indexed":false,"name":"_cap","type":"uint256"}],"name":"Unvote","type":"event"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_owner","type":"address"},{"indexed":false,"name":"_candidate","type":"address"},{"indexed":false,"name":"_cap","type":"uint256"}],"name":"Propose","type":"event"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_owner","type":"address"},{"indexed":false,"name":"_candidate","type":"address"}],"name":"Resign","type":"event"},{"anonymous":false,"inputs":[{"indexed":false,"name":"_owner","type":"address"},{"indexed":false,"name":"_blockNumber","type":"uint256"},{"indexed":false,"name":"_cap","type":"uint256"}],"name":"Withdraw","type":"event"}]`

// XDCValidator is an auto generated Go binding around an Ethereum contract.
type XDCValidator struct {
	XDCValidatorCaller     // Read-only binding to the contract
	XDCValidatorTransactor // Write-only binding to the contract
	XDCValidatorFilterer   // Log filterer for contract events
}

// XDCValidatorCaller is an auto generated read-only Go binding around an Ethereum contract.
type XDCValidatorCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// XDCValidatorTransactor is an auto generated write-only Go binding around an Ethereum contract.
type XDCValidatorTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// XDCValidatorFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type XDCValidatorFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// XDCValidatorSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type XDCValidatorSession struct {
	Contract     *XDCValidator     // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// XDCValidatorCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type XDCValidatorCallerSession struct {
	Contract *XDCValidatorCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts       // Call options to use throughout this session
}

// XDCValidatorTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type XDCValidatorTransactorSession struct {
	Contract     *XDCValidatorTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts       // Transaction auth options to use throughout this session
}

// XDCValidatorRaw is an auto generated low-level Go binding around an Ethereum contract.
type XDCValidatorRaw struct {
	Contract *XDCValidator // Generic contract binding to access the raw methods on
}

// XDCValidatorCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type XDCValidatorCallerRaw struct {
	Contract *XDCValidatorCaller // Generic read-only contract binding to access the raw methods on
}

// XDCValidatorTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type XDCValidatorTransactorRaw struct {
	Contract *XDCValidatorTransactor // Generic write-only contract binding to access the raw methods on
}

// NewXDCValidator creates a new instance of XDCValidator, bound to a specific deployed contract.
func NewXDCValidator(address common.Address, backend bind.ContractBackend) (*XDCValidator, error) {
	contract, err := bindXDCValidator(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &XDCValidator{XDCValidatorCaller: XDCValidatorCaller{contract: contract}, XDCValidatorTransactor: XDCValidatorTransactor{contract: contract}, XDCValidatorFilterer: XDCValidatorFilterer{contract: contract}}, nil
}

// NewXDCValidatorCaller creates a new read-only instance of XDCValidator, bound to a specific deployed contract.
func NewXDCValidatorCaller(address common.Address, caller bind.ContractCaller) (*XDCValidatorCaller, error) {
	contract, err := bindXDCValidator(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &XDCValidatorCaller{contract: contract}, nil
}

// NewXDCValidatorTransactor creates a new write-only instance of XDCValidator, bound to a specific deployed contract.
func NewXDCValidatorTransactor(address common.Address, transactor bind.ContractTransactor) (*XDCValidatorTransactor, error) {
	contract, err := bindXDCValidator(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &XDCValidatorTransactor{contract: contract}, nil
}

// NewXDCValidatorFilterer creates a new log filterer instance of XDCValidator, bound to a specific deployed contract.
func NewXDCValidatorFilterer(address common.Address, filterer bind.ContractFilterer) (*XDCValidatorFilterer, error) {
	contract, err := bindXDCValidator(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &XDCValidatorFilterer{contract: contract}, nil
}

// bindXDCValidator binds a generic wrapper to an already deployed contract.
func bindXDCValidator(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := abi.JSON(strings.NewReader(XDCValidatorABI))
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_XDCValidator *XDCValidatorRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _XDCValidator.Contract.XDCValidatorCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_XDCValidator *XDCValidatorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _XDCValidator.Contract.XDCValidatorTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_XDCValidator *XDCValidatorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _XDCValidator.Contract.XDCValidatorTransactor.contract.Transact(opts, method, params...)
}

// GetCandidates is a free data retrieval call binding the contract method 0x06a49fce.
//
// Solidity: function getCandidates() view returns(address[])
func (_XDCValidator *XDCValidatorCaller) GetCandidates(opts *bind.CallOpts) ([]common.Address, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "getCandidates")

	if err != nil {
		return *new([]common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)

	return out0, err
}

// GetCandidates is a free data retrieval call binding the contract method 0x06a49fce.
//
// Solidity: function getCandidates() view returns(address[])
func (_XDCValidator *XDCValidatorSession) GetCandidates() ([]common.Address, error) {
	return _XDCValidator.Contract.GetCandidates(&_XDCValidator.CallOpts)
}

// GetCandidates is a free data retrieval call binding the contract method 0x06a49fce.
//
// Solidity: function getCandidates() view returns(address[])
func (_XDCValidator *XDCValidatorCallerSession) GetCandidates() ([]common.Address, error) {
	return _XDCValidator.Contract.GetCandidates(&_XDCValidator.CallOpts)
}

// GetCandidateCap is a free data retrieval call binding the contract method 0x58e7525f.
//
// Solidity: function getCandidateCap(address _candidate) view returns(uint256)
func (_XDCValidator *XDCValidatorCaller) GetCandidateCap(opts *bind.CallOpts, _candidate common.Address) (*big.Int, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "getCandidateCap", _candidate)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// GetCandidateCap is a free data retrieval call binding the contract method 0x58e7525f.
//
// Solidity: function getCandidateCap(address _candidate) view returns(uint256)
func (_XDCValidator *XDCValidatorSession) GetCandidateCap(_candidate common.Address) (*big.Int, error) {
	return _XDCValidator.Contract.GetCandidateCap(&_XDCValidator.CallOpts, _candidate)
}

// GetCandidateCap is a free data retrieval call binding the contract method 0x58e7525f.
//
// Solidity: function getCandidateCap(address _candidate) view returns(uint256)
func (_XDCValidator *XDCValidatorCallerSession) GetCandidateCap(_candidate common.Address) (*big.Int, error) {
	return _XDCValidator.Contract.GetCandidateCap(&_XDCValidator.CallOpts, _candidate)
}

// GetCandidateOwner is a free data retrieval call binding the contract method 0xb642facd.
//
// Solidity: function getCandidateOwner(address _candidate) view returns(address)
func (_XDCValidator *XDCValidatorCaller) GetCandidateOwner(opts *bind.CallOpts, _candidate common.Address) (common.Address, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "getCandidateOwner", _candidate)

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err
}

// GetCandidateOwner is a free data retrieval call binding the contract method 0xb642facd.
//
// Solidity: function getCandidateOwner(address _candidate) view returns(address)
func (_XDCValidator *XDCValidatorSession) GetCandidateOwner(_candidate common.Address) (common.Address, error) {
	return _XDCValidator.Contract.GetCandidateOwner(&_XDCValidator.CallOpts, _candidate)
}

// GetCandidateOwner is a free data retrieval call binding the contract method 0xb642facd.
//
// Solidity: function getCandidateOwner(address _candidate) view returns(address)
func (_XDCValidator *XDCValidatorCallerSession) GetCandidateOwner(_candidate common.Address) (common.Address, error) {
	return _XDCValidator.Contract.GetCandidateOwner(&_XDCValidator.CallOpts, _candidate)
}

// GetVoters is a free data retrieval call binding the contract method 0x2d15cc04.
//
// Solidity: function getVoters(address _candidate) view returns(address[])
func (_XDCValidator *XDCValidatorCaller) GetVoters(opts *bind.CallOpts, _candidate common.Address) ([]common.Address, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "getVoters", _candidate)

	if err != nil {
		return *new([]common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)

	return out0, err
}

// GetVoters is a free data retrieval call binding the contract method 0x2d15cc04.
//
// Solidity: function getVoters(address _candidate) view returns(address[])
func (_XDCValidator *XDCValidatorSession) GetVoters(_candidate common.Address) ([]common.Address, error) {
	return _XDCValidator.Contract.GetVoters(&_XDCValidator.CallOpts, _candidate)
}

// GetVoters is a free data retrieval call binding the contract method 0x2d15cc04.
//
// Solidity: function getVoters(address _candidate) view returns(address[])
func (_XDCValidator *XDCValidatorCallerSession) GetVoters(_candidate common.Address) ([]common.Address, error) {
	return _XDCValidator.Contract.GetVoters(&_XDCValidator.CallOpts, _candidate)
}

// GetVoterCap is a free data retrieval call binding the contract method 0x302b6872.
//
// Solidity: function getVoterCap(address _candidate, address _voter) view returns(uint256)
func (_XDCValidator *XDCValidatorCaller) GetVoterCap(opts *bind.CallOpts, _candidate common.Address, _voter common.Address) (*big.Int, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "getVoterCap", _candidate, _voter)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// GetVoterCap is a free data retrieval call binding the contract method 0x302b6872.
//
// Solidity: function getVoterCap(address _candidate, address _voter) view returns(uint256)
func (_XDCValidator *XDCValidatorSession) GetVoterCap(_candidate common.Address, _voter common.Address) (*big.Int, error) {
	return _XDCValidator.Contract.GetVoterCap(&_XDCValidator.CallOpts, _candidate, _voter)
}

// GetVoterCap is a free data retrieval call binding the contract method 0x302b6872.
//
// Solidity: function getVoterCap(address _candidate, address _voter) view returns(uint256)
func (_XDCValidator *XDCValidatorCallerSession) GetVoterCap(_candidate common.Address, _voter common.Address) (*big.Int, error) {
	return _XDCValidator.Contract.GetVoterCap(&_XDCValidator.CallOpts, _candidate, _voter)
}

// IsCandidate is a free data retrieval call binding the contract method 0xd51b9e93.
//
// Solidity: function isCandidate(address _candidate) view returns(bool)
func (_XDCValidator *XDCValidatorCaller) IsCandidate(opts *bind.CallOpts, _candidate common.Address) (bool, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "isCandidate", _candidate)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err
}

// IsCandidate is a free data retrieval call binding the contract method 0xd51b9e93.
//
// Solidity: function isCandidate(address _candidate) view returns(bool)
func (_XDCValidator *XDCValidatorSession) IsCandidate(_candidate common.Address) (bool, error) {
	return _XDCValidator.Contract.IsCandidate(&_XDCValidator.CallOpts, _candidate)
}

// IsCandidate is a free data retrieval call binding the contract method 0xd51b9e93.
//
// Solidity: function isCandidate(address _candidate) view returns(bool)
func (_XDCValidator *XDCValidatorCallerSession) IsCandidate(_candidate common.Address) (bool, error) {
	return _XDCValidator.Contract.IsCandidate(&_XDCValidator.CallOpts, _candidate)
}

// CandidateCount is a free data retrieval call binding the contract method 0xa9a981a3.
//
// Solidity: function candidateCount() view returns(uint256)
func (_XDCValidator *XDCValidatorCaller) CandidateCount(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "candidateCount")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// CandidateCount is a free data retrieval call binding the contract method 0xa9a981a3.
//
// Solidity: function candidateCount() view returns(uint256)
func (_XDCValidator *XDCValidatorSession) CandidateCount() (*big.Int, error) {
	return _XDCValidator.Contract.CandidateCount(&_XDCValidator.CallOpts)
}

// CandidateCount is a free data retrieval call binding the contract method 0xa9a981a3.
//
// Solidity: function candidateCount() view returns(uint256)
func (_XDCValidator *XDCValidatorCallerSession) CandidateCount() (*big.Int, error) {
	return _XDCValidator.Contract.CandidateCount(&_XDCValidator.CallOpts)
}

// MaxValidatorNumber is a free data retrieval call binding the contract method 0xd09f1ab4.
//
// Solidity: function maxValidatorNumber() view returns(uint256)
func (_XDCValidator *XDCValidatorCaller) MaxValidatorNumber(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "maxValidatorNumber")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// MaxValidatorNumber is a free data retrieval call binding the contract method 0xd09f1ab4.
//
// Solidity: function maxValidatorNumber() view returns(uint256)
func (_XDCValidator *XDCValidatorSession) MaxValidatorNumber() (*big.Int, error) {
	return _XDCValidator.Contract.MaxValidatorNumber(&_XDCValidator.CallOpts)
}

// MaxValidatorNumber is a free data retrieval call binding the contract method 0xd09f1ab4.
//
// Solidity: function maxValidatorNumber() view returns(uint256)
func (_XDCValidator *XDCValidatorCallerSession) MaxValidatorNumber() (*big.Int, error) {
	return _XDCValidator.Contract.MaxValidatorNumber(&_XDCValidator.CallOpts)
}

// MinCandidateCap is a free data retrieval call binding the contract method 0xd55b7dff.
//
// Solidity: function minCandidateCap() view returns(uint256)
func (_XDCValidator *XDCValidatorCaller) MinCandidateCap(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _XDCValidator.contract.Call(opts, &out, "minCandidateCap")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err
}

// MinCandidateCap is a free data retrieval call binding the contract method 0xd55b7dff.
//
// Solidity: function minCandidateCap() view returns(uint256)
func (_XDCValidator *XDCValidatorSession) MinCandidateCap() (*big.Int, error) {
	return _XDCValidator.Contract.MinCandidateCap(&_XDCValidator.CallOpts)
}

// MinCandidateCap is a free data retrieval call binding the contract method 0xd55b7dff.
//
// Solidity: function minCandidateCap() view returns(uint256)
func (_XDCValidator *XDCValidatorCallerSession) MinCandidateCap() (*big.Int, error) {
	return _XDCValidator.Contract.MinCandidateCap(&_XDCValidator.CallOpts)
}

// Vote is a paid mutator transaction binding the contract method 0x6dd7d8ea.
//
// Solidity: function vote(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorTransactor) Vote(opts *bind.TransactOpts, _candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.contract.Transact(opts, "vote", _candidate)
}

// Vote is a paid mutator transaction binding the contract method 0x6dd7d8ea.
//
// Solidity: function vote(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorSession) Vote(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Vote(&_XDCValidator.TransactOpts, _candidate)
}

// Vote is a paid mutator transaction binding the contract method 0x6dd7d8ea.
//
// Solidity: function vote(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorTransactorSession) Vote(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Vote(&_XDCValidator.TransactOpts, _candidate)
}

// Propose is a paid mutator transaction binding the contract method 0x01267951.
//
// Solidity: function propose(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorTransactor) Propose(opts *bind.TransactOpts, _candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.contract.Transact(opts, "propose", _candidate)
}

// Propose is a paid mutator transaction binding the contract method 0x01267951.
//
// Solidity: function propose(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorSession) Propose(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Propose(&_XDCValidator.TransactOpts, _candidate)
}

// Propose is a paid mutator transaction binding the contract method 0x01267951.
//
// Solidity: function propose(address _candidate) payable returns()
func (_XDCValidator *XDCValidatorTransactorSession) Propose(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Propose(&_XDCValidator.TransactOpts, _candidate)
}

// Resign is a paid mutator transaction binding the contract method 0xae6e43f5.
//
// Solidity: function resign(address _candidate) returns()
func (_XDCValidator *XDCValidatorTransactor) Resign(opts *bind.TransactOpts, _candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.contract.Transact(opts, "resign", _candidate)
}

// Resign is a paid mutator transaction binding the contract method 0xae6e43f5.
//
// Solidity: function resign(address _candidate) returns()
func (_XDCValidator *XDCValidatorSession) Resign(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Resign(&_XDCValidator.TransactOpts, _candidate)
}

// Resign is a paid mutator transaction binding the contract method 0xae6e43f5.
//
// Solidity: function resign(address _candidate) returns()
func (_XDCValidator *XDCValidatorTransactorSession) Resign(_candidate common.Address) (*types.Transaction, error) {
	return _XDCValidator.Contract.Resign(&_XDCValidator.TransactOpts, _candidate)
}

// DeployXDCValidator deploys a new instance of the XDCValidator contract.
func DeployXDCValidator(auth *bind.TransactOpts, backend bind.ContractBackend, _candidates []common.Address, _caps []*big.Int, _firstOwner common.Address, _minCandidateCap *big.Int, _minVoterCap *big.Int, _maxValidatorNumber *big.Int, _candidateWithdrawDelay *big.Int, _voterWithdrawDelay *big.Int) (common.Address, *types.Transaction, *XDCValidator, error) {
	parsed, err := abi.JSON(strings.NewReader(XDCValidatorABI))
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	// Note: In production, you would include the bytecode here
	// For now, we return an error as deployment requires bytecode
	_ = parsed
	return common.Address{}, nil, nil, errors.New("deployment requires bytecode - use pre-deployed contract")
}
