// Copyright 2015 The go-ethereum Authors
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

package common

import (
	"bytes"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"golang.org/x/crypto/sha3"
)

// Lengths of hashes and addresses in bytes.
const (
	// HashLength is the expected length of the hash
	HashLength = 32
	// AddressLength is the expected length of the address
	AddressLength = 20
)

// XDC-specific constants
const (
	BlockSigners                     = "xdc0000000000000000000000000000000000000089"
	MasternodeVotingSMC              = "xdc0000000000000000000000000000000000000088"
	RandomizeSMC                     = "xdc0000000000000000000000000000000000000090"
	FoudationAddr                    = "xdc0000000000000000000000000000000000000068"
	TeamAddr                         = "xdc0000000000000000000000000000000000000099"
	XDCXAddr                         = "xdc0000000000000000000000000000000000000091"
	TradingStateAddr                 = "xdc0000000000000000000000000000000000000092"
	XDCXLendingAddress               = "xdc0000000000000000000000000000000000000093"
	XDCXLendingFinalizedTradeAddress = "xdc0000000000000000000000000000000000000094"
	XDCNativeAddress                 = "xdc0000000000000000000000000000000000000001"
	LendingLockAddress               = "xdc0000000000000000000000000000000000000011"

	// Method signatures for XDPoS contracts
	VoteMethod      = "0x6dd7d8ea"
	UnvoteMethod    = "0x02aa9be2"
	ProposeMethod   = "0x01267951"
	ResignMethod    = "0xae6e43f5"
	SignMethod      = "0xe341eaa4"
	XDCXApplyMethod = "0xc6b32f34"
	XDCZApplyMethod = "0xc6b32f34"
	HexSignMethod   = "e341eaa4"
	HexSetSecret    = "34d38600"
	HexSetOpening   = "e11f5ba2"

	// Reward percentages (matches v2.6.8: MN gets 90%, voters 0%, foundation 10%)
	RewardMasterPercent     = 90
	RewardVoterPercent      = 0
	RewardFoundationPercent = 10

	// Epoch block constants
	MergeSignRange     = 15
	EpocBlockSecret    = 800
	EpocBlockOpening   = 850
	EpocBlockRandomize = 900
)

var (
	hashT    = reflect.TypeFor[Hash]()
	addressT = reflect.TypeFor[Address]()

	// MaxAddress represents the maximum possible address value.
	MaxAddress = HexToAddress("0xffffffffffffffffffffffffffffffffffffffff")

	// MaxHash represents the maximum possible hash value.
	MaxHash = HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")

	// XDC contract addresses (binary form) — moved to common/xdc_contracts.go
	// per issue #694 §E. Same package, same exported names; this is a
	// cosmetic file split with zero behavior change.

	// TIP2019 block number for XDPoS reward updates
	TIP2019Block         = big.NewInt(0)
	TIPSigning           = big.NewInt(3000000)
	TIPNoHalvingMNReward = big.NewInt(38383838)
	TIPRandomize         = big.NewInt(3464000)

	// TIPXDCX / TIPXDCXMiner / TIPXDCXReceiver: XDCX trading/lending fork blocks
	// These control whether trading/lending transactions are processed as empty (0 gas)
	// or as normal EVM transactions. Aligned with XinFinOrg/XDPoSChain v2.6.8 constants.mainnet.go.
	TIPXDCX                = big.NewInt(38383838)
	TIPXDCXLending         = big.NewInt(38383838)
	TIPXDCXCancellationFee = big.NewInt(38383838)
	TIPXDCXMinerDisable    = big.NewInt(80370000)
	TIPXDCXReceiverDisable = big.NewInt(80370900)

	// TIPUpgradeReward / TipUpgradePenalty: future upgrade forks (currently no-op)
	// Aligned with XinFinOrg/XDPoSChain v2.6.8 constants
	TIPUpgradeReward  = big.NewInt(9999999999)
	TipUpgradePenalty = big.NewInt(9999999999)

	// TIPTRC21Fee: block number where fee routing changes to owner-based
	// Before this: fee goes to coinbase; After this: fee goes to coinbase owner
	TIPTRC21Fee = big.NewInt(38383838)

	// BlackListHFNumber is the block number at which the transaction blacklist
	// enforcement activates. Matches XDPoSChain v2.6.8 constants.mainnet.go.
	BlackListHFNumber = uint64(38383838)

	// blacklist contains addresses prohibited from sending or receiving
	// transactions. Sourced from XDPoSChain v2.6.8 constants.mainnet.go.
	blacklist = map[Address]struct{}{
		HexToAddress("0x5248bfb72fd4f234e062d3e9bb76f08643004fcd"): {},
		HexToAddress("0x5ac26105b35ea8935be382863a70281ec7a985e9"): {},
		HexToAddress("0x09c4f991a41e7ca0645d7dfbfee160b55e562ea4"): {},
		HexToAddress("0xb3157bbc5b401a45d6f60b106728bb82ebaa585b"): {},
		HexToAddress("0x741277a8952128d5c2ffe0550f5001e4c8247674"): {},
		HexToAddress("0x10ba49c1caa97d74b22b3e74493032b180cebe01"): {},
		HexToAddress("0x07048d51d9e6179578a6e3b9ee28cdc183b865e4"): {},
		HexToAddress("0x4b899001d73c7b4ec404a771d37d9be13b8983de"): {},
		HexToAddress("0x85cb320a9007f26b7652c19a2a65db1da2d0016f"): {},
		HexToAddress("0x06869dbd0e3a2ea37ddef832e20fa005c6f0ca39"): {},
		HexToAddress("0x82e48bc7e2c93d89125428578fb405947764ad7c"): {},
		HexToAddress("0x1f9a78534d61732367cbb43fc6c89266af67c989"): {},
		HexToAddress("0x7c3b1fa91df55ff7af0cad9e0399384dc5c6641b"): {},
		HexToAddress("0x5888dc1ceb0ff632713486b9418e59743af0fd20"): {},
		HexToAddress("0xa512fa1c735fc3cc635624d591dd9ea1ce339ca5"): {},
		HexToAddress("0x0832517654c7b7e36b1ef45d76de70326b09e2c7"): {},
		HexToAddress("0xca14e3c4c78bafb60819a78ff6e6f0f709d2aea7"): {},
		HexToAddress("0x652ce195a23035114849f7642b0e06647d13e57a"): {},
		HexToAddress("0x29a79f00f16900999d61b6e171e44596af4fb5ae"): {},
		HexToAddress("0xf9fd1c2b0af0d91b0b6754e55639e3f8478dd04a"): {},
		HexToAddress("0xb835710c9901d5fe940ef1b99ed918902e293e35"): {},
		HexToAddress("0x04dd29ce5c253377a9a3796103ea0d9a9e514153"): {},
		HexToAddress("0x2b4b56846eaf05c1fd762b5e1ac802efd0ab871c"): {},
		HexToAddress("0x1d1f909f6600b23ce05004f5500ab98564717996"): {},
		HexToAddress("0x0dfdcebf80006dc9ab7aae8c216b51c6b6759e86"): {},
		HexToAddress("0x2b373890a28e5e46197fbc04f303bbfdd344056f"): {},
		HexToAddress("0xa8a3ef3dc5d8e36aee76f3671ec501ec31e28254"): {},
		HexToAddress("0x4f3d18136fe2b5665c29bdaf74591fc6625ef427"): {},
		HexToAddress("0x175d728b0e0f1facb5822a2e0c03bde93596e324"): {},
		HexToAddress("0xd575c2611984fcd79513b80ab94f59dc5bab4916"): {},
		HexToAddress("0x0579337873c97c4ba051310236ea847f5be41bc0"): {},
		HexToAddress("0xed12a519cc15b286920fc15fd86106b3e6a16218"): {},
		HexToAddress("0x492d26d852a0a0a2982bb40ec86fe394488c419e"): {},
		HexToAddress("0xce5c7635d02dc4e1d6b46c256cae6323be294a32"): {},
		HexToAddress("0x8b94db158b5e78a6c032c7e7c9423dec62c8b11c"): {},
		HexToAddress("0x0e7c48c085b6b0aa7ca6e4cbcc8b9a92dc270eb4"): {},
		HexToAddress("0x206e6508462033ef8425edc6c10789d241d49acb"): {},
		HexToAddress("0x7710e7b7682f26cb5a1202e1cff094fbf7777758"): {},
		HexToAddress("0xcb06f949313b46bbf53b8e6b2868a0c260ff9385"): {},
		HexToAddress("0xf884e43533f61dc2997c0e19a6eff33481920c00"): {},
		HexToAddress("0x8b635ef2e4c8fe21fc2bda027eb5f371d6aa2fc1"): {},
		HexToAddress("0x10f01a27cf9b29d02ce53497312b96037357a361"): {},
		HexToAddress("0x693dd49b0ed70f162d733cf20b6c43dc2a2b4d95"): {},
		HexToAddress("0xe0bec72d1c2a7a7fb0532cdfac44ebab9f6f41ee"): {},
		HexToAddress("0xc8793633a537938cb49cdbbffd45428f10e45b64"): {},
		HexToAddress("0x0d07a6cbbe9fa5c4f154e5623bfe47fb4d857d8e"): {},
		HexToAddress("0xd4080b289da95f70a586610c38268d8d4cf1e4c4"): {},
		HexToAddress("0x8bcfb0caf41f0aa1b548cae76dcdd02e33866a1b"): {},
		HexToAddress("0xabfef22b92366d3074676e77ea911ccaabfb64c1"): {},
		HexToAddress("0xcc4df7a32faf3efba32c9688def5ccf9fefe443d"): {},
		HexToAddress("0x7ec1e48a582475f5f2b7448a86c4ea7a26ea36f8"): {},
		HexToAddress("0xe3de67289080f63b0c2612844256a25bb99ac0ad"): {},
		HexToAddress("0x3ba623300cf9e48729039b3c9e0dee9b785d636e"): {},
		HexToAddress("0x402f2cfc9c8942f5e7a12c70c625d07a5d52fe29"): {},
		HexToAddress("0xd62358d42afbde095a4ca868581d85f9adcc3d61"): {},
		HexToAddress("0x3969f86acb733526cd61e3c6e3b4660589f32bc6"): {},
		HexToAddress("0x67615413d7cdadb2c435a946aec713a9a9794d39"): {},
		HexToAddress("0xfe685f43acc62f92ab01a8da80d76455d39d3cb3"): {},
		HexToAddress("0x3538a544021c07869c16b764424c5987409cba48"): {},
		HexToAddress("0xe187cf86c2274b1f16e8225a7da9a75aba4f1f5f"): {},
		HexToAddress("0x0000000000000000000000000000000000000011"): {},
	}
)

// CopyXDCConstants sets XDC-specific constants for the given chain ID.
// Must be called during chain initialization before processing blocks.
// Synced with XinFinOrg/XDPoSChain main branch common/constants.{mainnet,testnet,devnet}.go
func CopyXDCConstants(chainID uint64) {
	switch chainID {
	case 51: // Apothem testnet
		TIPSigning = big.NewInt(3000000)
		TIPTRC21Fee = big.NewInt(23779191)
		TIPNoHalvingMNReward = big.NewInt(23779191)
		TIPXDCXCancellationFee = big.NewInt(23779191)
		TIPXDCX = big.NewInt(23779191)
		TIPXDCXLending = big.NewInt(23779191)
		TIPXDCXMinerDisable = big.NewInt(61290000)
		TIPXDCXReceiverDisable = big.NewInt(66825000)
		BlackListHFNumber = 23779191
		// Canonical XDPoSChain v2.6.8 common/constants.testnet.go: tip2019Block=1.
		// Functionally equivalent to package default 0 for block N≥1, but set
		// explicitly so the per-chain semantics are visible. (issue #694 §B)
		TIP2019Block = big.NewInt(1)
	case 50: // XDC mainnet
		TIPSigning = big.NewInt(3000000)
		TIPTRC21Fee = big.NewInt(38383838)
		TIPNoHalvingMNReward = big.NewInt(38383838)
		TIPXDCXCancellationFee = big.NewInt(38383838)
		TIPXDCX = big.NewInt(38383838)
		TIPXDCXLending = big.NewInt(38383838)
		TIPXDCXMinerDisable = big.NewInt(80370000)
		TIPXDCXReceiverDisable = big.NewInt(80370900)
		BlackListHFNumber = 38383838
		// Canonical XDPoSChain v2.6.8 common/constants.mainnet.go: tip2019Block=1.
		TIP2019Block = big.NewInt(1)
	case 551: // Active devnet (XDPoSChain main)
		TIPSigning = big.NewInt(0)
		TIPTRC21Fee = big.NewInt(10800)
		TIPNoHalvingMNReward = big.NewInt(0)
		TIPXDCXCancellationFee = big.NewInt(0)
		TIPXDCX = big.NewInt(0)
		TIPXDCXLending = big.NewInt(0)
		TIPXDCXMinerDisable = big.NewInt(0)
		// TIPXDCXReceiverDisable must be > TIPXDCX (0) so that IsTIPXDCXReceiver
		// returns true for all blocks. If both are 0, isForked(0,N)=true &&
		// !isForked(0,N)=false yields false for every block — breaking the special
		// ApplyEmptyTransaction path for XDPoSChain system txs to 0x92.
		// Use math.MaxInt64-like sentinel: XDPoSChain devnet never reaches block 1e17.
		TIPXDCXReceiverDisable = big.NewInt(1e17)
		BlackListHFNumber = 0
		// Canonical XDPoSChain v2.6.8 common/constants.devnet.go: tip2019Block=0.
		TIP2019Block = big.NewInt(0)
	case 5151: // Local devnet (XDPoSChain constants.local.go — all TIPs active from genesis)
		// canonical XDPoSChain common/constants.local.go has every tipXxx = big.NewInt(0),
		// meaning all fork activations and special transaction paths (signing, XDCx, etc.)
		// are live from block 1. Without this case, the package-level default sets
		// TIPSigning = 3,000,000 which causes signing-tx gasUsed to diverge from the
		// canonical legacy node (0 gas vs EVM-executed gas), producing wrong state roots
		// from block 1 and triggering A.94 on every fresh sync. Refs #894 / devnet-5151.
		TIPSigning = big.NewInt(0)
		TIPTRC21Fee = big.NewInt(0)
		TIPNoHalvingMNReward = big.NewInt(0)
		TIPXDCXCancellationFee = big.NewInt(0)
		TIPXDCX = big.NewInt(0)
		TIPXDCXLending = big.NewInt(0)
		TIPXDCXMinerDisable = big.NewInt(0)
		// Same sentinel fix as chainId 551: TIPXDCXReceiverDisable must be >
		// TIPXDCX (0) so that IsTIPXDCXReceiver returns true for all blocks.
		TIPXDCXReceiverDisable = big.NewInt(1e17)
		BlackListHFNumber = 0
		TIP2019Block = big.NewInt(0)
		// XDPoSChain constants.local.go: tipRandomize=0 — active from genesis so that
		// candidate-list randomisation applies in all V1 epochs (0-2699). Without this,
		// common.TIPRandomize retains the mainnet default (3,464,000) and IsTIPRandomize
		// returns false for all V1 blocks, causing M1/M2 ordering divergence from the
		// legacy node. Refs Opus RCA Step 3.
		TIPRandomize = big.NewInt(0)
	case 5050: // net5050 (XDCIndia stress-net) — V2 switch at block 2700
		// Gasless block-signer txs from genesis: the canonical oracle runs
		// TIPSigning=0, so block-1 signer txs have gasUsed=0. Without this case the
		// default (3,000,000) keeps the gasless path OFF -> block 1 charges gas ->
		// A.94 fires and the node falls to header-only sync. Refs xOneGo#45.
		TIPSigning = big.NewInt(0)
		TIPTRC21Fee = big.NewInt(0)
		TIPNoHalvingMNReward = big.NewInt(0)
		// The canonical oracle (XDPoSChain v2.7.0-devnet) activates Istanbul from
		// genesis via TIPXDCXCancellationFee=0. Confirmed by block 10727: a 0x88
		// validator-contract tx uses EIP-1884 SLOAD pricing (800 vs 200 gas),
		// producing 1800 more gas than a pre-Istanbul execution. Without this line,
		// our SLOAD cost is 200 -> gasUsed diverges by 1800 -> A.94 fires.
		TIPXDCXCancellationFee = big.NewInt(0) // Istanbul from genesis — matches oracle
		// 0x92 XDCx special txs route to ApplyEmptyTransaction (zero gas) whenever
		// IsTIPXDCXReceiver is true. Setting ReceiverDisable to a large sentinel keeps
		// that window open for all practical block numbers. The oracle also uses zero
		// gas for 0x92 txs throughout (confirmed: gasUsed=0 on blocks 1..1800).
		TIPXDCX = big.NewInt(0)
		TIPXDCXLending = big.NewInt(0)
		TIPXDCXMinerDisable = big.NewInt(0)
		TIPXDCXReceiverDisable = big.NewInt(1e17) // sentinel: IsTIPXDCXReceiver=true always
		BlackListHFNumber = 0
		TIP2019Block = big.NewInt(0)
	case 5551: // Legacy devnet (V2 switch at block 2700)
		TIPSigning = big.NewInt(0)
		TIPTRC21Fee = big.NewInt(0)
		TIPNoHalvingMNReward = big.NewInt(0)
		// Official devnet (XDPoSChain v2.7.0) activates Istanbul from genesis
		// via TIPXDCXCancellationFee=0. Setting this to 0 makes IsIstanbul()
		// return true for all blocks, matching official behavior.
		TIPXDCXCancellationFee = big.NewInt(0)
		TIPXDCX = big.NewInt(0)
		TIPXDCXLending = big.NewInt(0)
		TIPXDCXMinerDisable = big.NewInt(0)
		// Same sentinel fix as chainId 551: see comment there.
		TIPXDCXReceiverDisable = big.NewInt(1e17)
		BlackListHFNumber = 0
		// "Everything active from genesis" pattern → TIP2019Block=0.
		TIP2019Block = big.NewInt(0)
	default: // Any unregistered XDPoS private net (e.g. net5152) — LEGACY-COMPATIBLE
		// default profile. This is the #1339 principle applied to the common-package
		// gas/economics gates: a single default XDC config for every unregistered
		// chainId, with NO per-chainId hardcoding. It mirrors the legacy XDPoSChain
		// v2.7.1 mainnet base so a modern geth node computes the SAME state root as
		// the legacy oldxdc validators on a net5151/5152-parity net:
		//
		//   - TIPSigning = 3,000,000 → sign-txs are GAS-CHARGED (the gasless path is
		//     INACTIVE below block 3,000,000). Matches oldxdc's normal-gas behaviour;
		//     an unregistered net that instead wants gasless sets tipSigningBlock in
		//     its genesis JSON, which eth/backend.go honours over this default.
		//   - Randomize/XDCx/NoHalving/TRC21/BlackList use the mainnet-base values, so
		//     a low-block private net stays on the legacy pre-fork path (M1/M2 ordering
		//     and special-tx gas match oldxdc exactly).
		//
		// These equal the package-level init defaults above; set explicitly so the
		// default profile is self-documenting and TIP2019Block matches mainnet (1).
		TIPSigning = big.NewInt(3000000)
		TIPTRC21Fee = big.NewInt(38383838)
		TIPNoHalvingMNReward = big.NewInt(38383838)
		TIPXDCXCancellationFee = big.NewInt(38383838)
		TIPXDCX = big.NewInt(38383838)
		TIPXDCXLending = big.NewInt(38383838)
		TIPXDCXMinerDisable = big.NewInt(80370000)
		TIPXDCXReceiverDisable = big.NewInt(80370900)
		TIPRandomize = big.NewInt(3464000)
		BlackListHFNumber = 38383838
		TIP2019Block = big.NewInt(1)
	case 99999: // Private dev chain generated via cmd/puppeth.
		// Without this case, package-level defaults (mainnet's 38,383,838+
		// thresholds) gate every TIP — which means V2 BFT consensus never
		// activates inside any realistic demo timeline on a private chain
		// running 2-second blocks. Mirror devnet551's "everything active
		// from genesis" pattern so the genesis-encoded V2 switch block is
		// the only knob that decides when V2 takes over. See issue #603.
		TIPSigning = big.NewInt(0)
		TIPTRC21Fee = big.NewInt(0)
		TIPNoHalvingMNReward = big.NewInt(0)
		TIPXDCXCancellationFee = big.NewInt(0)
		TIPXDCX = big.NewInt(0)
		TIPXDCXLending = big.NewInt(0)
		TIPXDCXMinerDisable = big.NewInt(0)
		TIPXDCXReceiverDisable = big.NewInt(0)
		BlackListHFNumber = 0
		// "Everything active from genesis" pattern → TIP2019Block=0.
		TIP2019Block = big.NewInt(0)
	}
}

// IsInBlacklist reports whether the given address is in the transaction
// blacklist. A nil address is never blacklisted.
func IsInBlacklist(address *Address) bool {
	if address == nil {
		return false
	}
	_, ok := blacklist[*address]
	return ok
}

// Hash represents the 32 byte Keccak256 hash of arbitrary data.
type Hash [HashLength]byte

// Vote represents a masternode vote from a voter
type Vote struct {
	Masternode Address
	Voter      Address
}

// BytesToHash sets b to hash.
// If b is larger than len(h), b will be cropped from the left.
func BytesToHash(b []byte) Hash {
	var h Hash
	h.SetBytes(b)
	return h
}

// BigToHash sets byte representation of b to hash.
// If b is larger than len(h), b will be cropped from the left.
func BigToHash(b *big.Int) Hash { return BytesToHash(b.Bytes()) }

// Uint64ToHash sets byte representation of b to hash.
// XDC: ported verbatim from XDPoSChain v2.8.0 for MintedRecord epoch keys.
func Uint64ToHash(b uint64) Hash { return BytesToHash(new(big.Int).SetUint64(b).Bytes()) }

// HexToHash sets byte representation of s to hash.
// If b is larger than len(h), b will be cropped from the left.
func HexToHash(s string) Hash { return BytesToHash(FromHex(s)) }

// ExtractAddressFromBytes splits a packed byte slice into 20-byte addresses.
// XDC: ported verbatim from XDPoSChain v2.8.0; used to decode header.Penalties
// when selecting protector/observer node tiers post-TIPUpgradeReward.
func ExtractAddressFromBytes(rawBytes []byte) []Address {
	if len(rawBytes) < AddressLength {
		return []Address{}
	}
	addresses := make([]Address, len(rawBytes)/AddressLength)
	for i := 0; i < len(addresses); i++ {
		copy(addresses[i][:], rawBytes[i*AddressLength:])
	}
	return addresses
}

// IsHexHash verifies whether a string can represent a valid hex-encoded
// Ethereum hash or not.
func IsHexHash(s string) bool {
	if has0xPrefix(s) {
		s = s[2:]
	}
	return len(s) == 2*HashLength && isHex(s)
}

// Cmp compares two hashes.
func (h Hash) Cmp(other Hash) int {
	return bytes.Compare(h[:], other[:])
}

// Bytes gets the byte representation of the underlying hash.
func (h Hash) Bytes() []byte { return h[:] }

// Big converts a hash to a big integer.
func (h Hash) Big() *big.Int { return new(big.Int).SetBytes(h[:]) }

// Hex converts a hash to a hex string.
func (h Hash) Hex() string { return hexutil.Encode(h[:]) }

// TerminalString implements log.TerminalStringer, formatting a string for console
// output during logging.
func (h Hash) TerminalString() string {
	return fmt.Sprintf("%x..%x", h[:3], h[29:])
}

// String implements the stringer interface and is used also by the logger when
// doing full logging into a file.
func (h Hash) String() string {
	return h.Hex()
}

// Format implements fmt.Formatter.
// Hash supports the %v, %s, %q, %x, %X and %d format verbs.
func (h Hash) Format(s fmt.State, c rune) {
	hexb := make([]byte, 2+len(h)*2)
	copy(hexb, "0x")
	hex.Encode(hexb[2:], h[:])

	switch c {
	case 'x', 'X':
		if !s.Flag('#') {
			hexb = hexb[2:]
		}
		if c == 'X' {
			hexb = bytes.ToUpper(hexb)
		}
		fallthrough
	case 'v', 's':
		s.Write(hexb)
	case 'q':
		q := []byte{'"'}
		s.Write(q)
		s.Write(hexb)
		s.Write(q)
	case 'd':
		fmt.Fprint(s, ([len(h)]byte)(h))
	default:
		fmt.Fprintf(s, "%%!%c(hash=%x)", c, h)
	}
}

// UnmarshalText parses a hash in hex syntax.
func (h *Hash) UnmarshalText(input []byte) error {
	return hexutil.UnmarshalFixedText("Hash", input, h[:])
}

// UnmarshalJSON parses a hash in hex syntax.
func (h *Hash) UnmarshalJSON(input []byte) error {
	return hexutil.UnmarshalFixedJSON(hashT, input, h[:])
}

// MarshalText returns the hex representation of h.
func (h Hash) MarshalText() ([]byte, error) {
	return hexutil.Bytes(h[:]).MarshalText()
}

// SetBytes sets the hash to the value of b.
// If b is larger than len(h), b will be cropped from the left.
func (h *Hash) SetBytes(b []byte) {
	if len(b) > len(h) {
		b = b[len(b)-HashLength:]
	}

	copy(h[HashLength-len(b):], b)
}

// Generate implements testing/quick.Generator.
func (h Hash) Generate(rand *rand.Rand, size int) reflect.Value {
	m := rand.Intn(len(h))
	for i := len(h) - 1; i > m; i-- {
		h[i] = byte(rand.Uint32())
	}
	return reflect.ValueOf(h)
}

// Scan implements Scanner for database/sql.
func (h *Hash) Scan(src interface{}) error {
	srcB, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("can't scan %T into Hash", src)
	}
	if len(srcB) != HashLength {
		return fmt.Errorf("can't scan []byte of len %d into Hash, want %d", len(srcB), HashLength)
	}
	copy(h[:], srcB)
	return nil
}

// Value implements valuer for database/sql.
func (h Hash) Value() (driver.Value, error) {
	return h[:], nil
}

// ImplementsGraphQLType returns true if Hash implements the specified GraphQL type.
func (Hash) ImplementsGraphQLType(name string) bool { return name == "Bytes32" }

// UnmarshalGraphQL unmarshals the provided GraphQL query data.
func (h *Hash) UnmarshalGraphQL(input interface{}) error {
	var err error
	switch input := input.(type) {
	case string:
		err = h.UnmarshalText([]byte(input))
	default:
		err = fmt.Errorf("unexpected type %T for Hash", input)
	}
	return err
}

// UnprefixedHash allows marshaling a Hash without 0x prefix.
type UnprefixedHash Hash

// UnmarshalText decodes the hash from hex. The 0x prefix is optional.
func (h *UnprefixedHash) UnmarshalText(input []byte) error {
	return hexutil.UnmarshalFixedUnprefixedText("UnprefixedHash", input, h[:])
}

// MarshalText encodes the hash as hex.
func (h UnprefixedHash) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(h[:])), nil
}

/////////// Address

// Address represents the 20 byte address of an Ethereum account.
type Address [AddressLength]byte

// BytesToAddress returns Address with value b.
// If b is larger than len(h), b will be cropped from the left.
func BytesToAddress(b []byte) Address {
	var a Address
	a.SetBytes(b)
	return a
}

// BigToAddress returns Address with byte values of b.
// If b is larger than len(h), b will be cropped from the left.
func BigToAddress(b *big.Int) Address { return BytesToAddress(b.Bytes()) }

// HexToAddress returns Address with byte values of s.
// If s is larger than len(h), s will be cropped from the left.
func HexToAddress(s string) Address { return BytesToAddress(FromHex(s)) }

// IsHexAddress verifies whether a string can represent a valid hex-encoded
// Ethereum address or not.
func IsHexAddress(s string) bool {
	if has0xPrefix(s) {
		s = s[2:]
	}
	return len(s) == 2*AddressLength && isHex(s)
}

// Cmp compares two addresses.
func (a Address) Cmp(other Address) int {
	return bytes.Compare(a[:], other[:])
}

// Bytes gets the string representation of the underlying address.
func (a Address) Bytes() []byte { return a[:] }

// Big converts an address to a big integer.
func (a Address) Big() *big.Int { return new(big.Int).SetBytes(a[:]) }

// Hex returns an EIP55-compliant hex string representation of the address.
func (a Address) Hex() string {
	return string(a.checksumHex())
}

// String implements fmt.Stringer.
func (a Address) String() string {
	return a.Hex()
}

func (a *Address) checksumHex() []byte {
	buf := a.hex()

	// compute checksum
	sha := sha3.NewLegacyKeccak256()
	sha.Write(buf[2:])
	hash := sha.Sum(nil)
	for i := 2; i < len(buf); i++ {
		hashByte := hash[(i-2)/2]
		if i%2 == 0 {
			hashByte = hashByte >> 4
		} else {
			hashByte &= 0xf
		}
		if buf[i] > '9' && hashByte > 7 {
			buf[i] -= 32
		}
	}
	return buf[:]
}

func (a Address) hex() []byte {
	var buf [len(a)*2 + 2]byte
	copy(buf[:2], "0x")
	hex.Encode(buf[2:], a[:])
	return buf[:]
}

// Format implements fmt.Formatter.
// Address supports the %v, %s, %q, %x, %X and %d format verbs.
func (a Address) Format(s fmt.State, c rune) {
	switch c {
	case 'v', 's':
		s.Write(a.checksumHex())
	case 'q':
		q := []byte{'"'}
		s.Write(q)
		s.Write(a.checksumHex())
		s.Write(q)
	case 'x', 'X':
		// %x disables the checksum.
		hex := a.hex()
		if !s.Flag('#') {
			hex = hex[2:]
		}
		if c == 'X' {
			hex = bytes.ToUpper(hex)
		}
		s.Write(hex)
	case 'd':
		fmt.Fprint(s, ([len(a)]byte)(a))
	default:
		fmt.Fprintf(s, "%%!%c(address=%x)", c, a)
	}
}

// SetBytes sets the address to the value of b.
// If b is larger than len(a), b will be cropped from the left.
func (a *Address) SetBytes(b []byte) {
	if len(b) > len(a) {
		b = b[len(b)-AddressLength:]
	}
	copy(a[AddressLength-len(b):], b)
}

// MarshalText returns the hex representation of a.
func (a Address) MarshalText() ([]byte, error) {
	return hexutil.Bytes(a[:]).MarshalText()
}

// UnmarshalText parses a hash in hex syntax.
// Supports both "0x" and "xdc" prefixes for XDC Network compatibility.
func (a *Address) UnmarshalText(input []byte) error {
	// Convert xdc prefix to 0x for hexutil compatibility
	inputStr := string(input)
	if len(inputStr) >= 3 && (inputStr[0] == 'x' || inputStr[0] == 'X') &&
		(inputStr[1] == 'd' || inputStr[1] == 'D') && (inputStr[2] == 'c' || inputStr[2] == 'C') {
		input = []byte("0x" + inputStr[3:])
	}
	return hexutil.UnmarshalFixedText("Address", input, a[:])
}

// UnmarshalJSON parses a hash in hex syntax.
// Supports both "0x" and "xdc" prefixes for XDC Network compatibility.
func (a *Address) UnmarshalJSON(input []byte) error {
	// Remove quotes and check for xdc prefix
	if len(input) >= 2 && input[0] == '"' && input[len(input)-1] == '"' {
		inputStr := string(input[1 : len(input)-1])
		if len(inputStr) >= 3 && (inputStr[0] == 'x' || inputStr[0] == 'X') &&
			(inputStr[1] == 'd' || inputStr[1] == 'D') && (inputStr[2] == 'c' || inputStr[2] == 'C') {
			input = []byte("\"0x" + inputStr[3:] + "\"")
		}
	}
	return hexutil.UnmarshalFixedJSON(addressT, input, a[:])
}

// Scan implements Scanner for database/sql.
func (a *Address) Scan(src interface{}) error {
	srcB, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("can't scan %T into Address", src)
	}
	if len(srcB) != AddressLength {
		return fmt.Errorf("can't scan []byte of len %d into Address, want %d", len(srcB), AddressLength)
	}
	copy(a[:], srcB)
	return nil
}

// Value implements valuer for database/sql.
func (a Address) Value() (driver.Value, error) {
	return a[:], nil
}

// ImplementsGraphQLType returns true if Hash implements the specified GraphQL type.
func (a Address) ImplementsGraphQLType(name string) bool { return name == "Address" }

// UnmarshalGraphQL unmarshals the provided GraphQL query data.
func (a *Address) UnmarshalGraphQL(input interface{}) error {
	var err error
	switch input := input.(type) {
	case string:
		err = a.UnmarshalText([]byte(input))
	default:
		err = fmt.Errorf("unexpected type %T for Address", input)
	}
	return err
}

// UnprefixedAddress allows marshaling an Address without 0x prefix.
type UnprefixedAddress Address

// UnmarshalText decodes the address from hex. The 0x prefix is optional.
func (a *UnprefixedAddress) UnmarshalText(input []byte) error {
	return hexutil.UnmarshalFixedUnprefixedText("UnprefixedAddress", input, a[:])
}

// MarshalText encodes the address as hex.
func (a UnprefixedAddress) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(a[:])), nil
}

// MixedcaseAddress retains the original string, which may or may not be
// correctly checksummed
type MixedcaseAddress struct {
	addr     Address
	original string
}

// NewMixedcaseAddress constructor (mainly for testing)
func NewMixedcaseAddress(addr Address) MixedcaseAddress {
	return MixedcaseAddress{addr: addr, original: addr.Hex()}
}

// NewMixedcaseAddressFromString is mainly meant for unit-testing
func NewMixedcaseAddressFromString(hexaddr string) (*MixedcaseAddress, error) {
	if !IsHexAddress(hexaddr) {
		return nil, errors.New("invalid address")
	}
	a := FromHex(hexaddr)
	return &MixedcaseAddress{addr: BytesToAddress(a), original: hexaddr}, nil
}

// UnmarshalJSON parses MixedcaseAddress
func (ma *MixedcaseAddress) UnmarshalJSON(input []byte) error {
	if err := hexutil.UnmarshalFixedJSON(addressT, input, ma.addr[:]); err != nil {
		return err
	}
	return json.Unmarshal(input, &ma.original)
}

// MarshalJSON marshals the original value
func (ma MixedcaseAddress) MarshalJSON() ([]byte, error) {
	if strings.HasPrefix(ma.original, "0x") || strings.HasPrefix(ma.original, "0X") {
		return json.Marshal(fmt.Sprintf("0x%s", ma.original[2:]))
	}
	return json.Marshal(fmt.Sprintf("0x%s", ma.original))
}

// Address returns the address
func (ma *MixedcaseAddress) Address() Address {
	return ma.addr
}

// String implements fmt.Stringer
func (ma *MixedcaseAddress) String() string {
	if ma.ValidChecksum() {
		return fmt.Sprintf("%s [chksum ok]", ma.original)
	}
	return fmt.Sprintf("%s [chksum INVALID]", ma.original)
}

// ValidChecksum returns true if the address has valid checksum
func (ma *MixedcaseAddress) ValidChecksum() bool {
	return ma.original == ma.addr.Hex()
}

// Original returns the mixed-case input string
func (ma *MixedcaseAddress) Original() string {
	return ma.original
}

// AddressEIP55 is an alias of Address with a customized json marshaller
type AddressEIP55 Address

// String returns the hex representation of the address in the manner of EIP55.
func (addr AddressEIP55) String() string {
	return Address(addr).Hex()
}

// MarshalJSON marshals the address in the manner of EIP55.
func (addr AddressEIP55) MarshalJSON() ([]byte, error) {
	return json.Marshal(addr.String())
}

type Decimal uint64

func isString(input []byte) bool {
	return len(input) >= 2 && input[0] == '"' && input[len(input)-1] == '"'
}

// UnmarshalJSON parses a hash in hex syntax.
func (d *Decimal) UnmarshalJSON(input []byte) error {
	if !isString(input) {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[uint64]()}
	}
	if i, err := strconv.ParseUint(string(input[1:len(input)-1]), 10, 64); err == nil {
		*d = Decimal(i)
		return nil
	} else {
		return err
	}
}

type PrettyBytes []byte

// TerminalString implements log.TerminalStringer, formatting a string for console
// output during logging.
func (b PrettyBytes) TerminalString() string {
	if len(b) < 7 {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%#x...%x (%dB)", b[:3], b[len(b)-3:], len(b))
}
