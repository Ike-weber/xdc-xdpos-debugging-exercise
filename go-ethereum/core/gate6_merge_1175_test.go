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

// This file is the required deliverable of the go-ethereum#1175 upstream
// v1.17.5 merge policy's Gate 6: consensus-sensitive assertions guarding the
// EIP-2028 suppression (state_transition.go / state_processor.go resolution)
// and the gasless sign-tx path (R3 in the merge policy risk list).

package core

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// signingTestKey returns a fresh throwaway key/address pair for the golden
// vector test below; it does not need to match any fixture used elsewhere.
func signingTestKey(t *testing.T) (*ecdsa.PrivateKey, common.Address) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key, crypto.PubkeyToAddress(key.PublicKey)
}

// TestXDCIntrinsicGasSuppressesEIP2028 directly exercises the EIP-2028
// suppression added to IntrinsicGas's new (from, to, value, rules) signature
// by the state_transition.go / validation.go merge resolutions: XDC chains
// must charge 68 gas per non-zero calldata byte (pre-EIP2028), not 16
// (EIP-2028), even though IsIstanbul is otherwise true for these chains.
func TestXDCIntrinsicGasSuppressesEIP2028(t *testing.T) {
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05} // 5 non-zero bytes, no zero bytes

	base := params.Rules{IsHomestead: true, IsIstanbul: true}

	// Modern (non-XDC) rules: EIP-2028 active, 16 gas/non-zero byte.
	modernGas, err := IntrinsicGas(data, nil, nil, from, &to, nil, base)
	if err != nil {
		t.Fatalf("IntrinsicGas (modern): %v", err)
	}
	wantModern := params.TxGas + uint64(len(data))*params.TxDataNonZeroGasEIP2028
	if modernGas != wantModern {
		t.Fatalf("modern intrinsic gas = %d, want %d (16/byte)", modernGas, wantModern)
	}

	// XDC rules: same base rules but with IsIstanbul forced false, exactly as
	// core/state_transition.go execute() and core/txpool/validation.go do via
	// their intrRules copy — must yield 68 gas/non-zero byte.
	xdcRules := base
	xdcRules.IsIstanbul = false
	xdcGas, err := IntrinsicGas(data, nil, nil, from, &to, nil, xdcRules)
	if err != nil {
		t.Fatalf("IntrinsicGas (XDC): %v", err)
	}
	wantXDC := params.TxGas + uint64(len(data))*params.TxDataNonZeroGasFrontier
	if wantXDC != params.TxGas+uint64(len(data))*68 {
		t.Fatalf("sanity: TxDataNonZeroGasFrontier is not 68, got %d", params.TxDataNonZeroGasFrontier)
	}
	if xdcGas != wantXDC {
		t.Fatalf("XDC intrinsic gas = %d, want %d (68/byte, EIP-2028 suppressed)", xdcGas, wantXDC)
	}
	if xdcGas == modernGas {
		t.Fatalf("XDC and modern intrinsic gas must differ for calldata-bearing txs (got %d for both)", xdcGas)
	}
}

// TestXDCRulesFieldsOnXDCChain asserts the Rules() output on an XDPoS-flagged
// chain config has the invariants relied on elsewhere in this merge: IsXDC
// true, IsUBT/IsBogota/IsEIP4762 false (XDC never activates UBT/Verkle/
// Bogota), and IsEIP3860 tracks IsEIP1559 (not IsShanghai, per #716).
func TestXDCRulesFieldsOnXDCChain(t *testing.T) {
	cfg := *params.TestChainConfig
	cfg.XDPoS = &params.XDPoSConfig{}
	num := big.NewInt(1)

	rules := cfg.Rules(num, false, 0)
	if !rules.IsXDC {
		t.Error("Rules.IsXDC must be true for an XDPoS-configured chain")
	}
	if rules.IsUBT {
		t.Error("Rules.IsUBT must be false — XDC never activates UBT")
	}
	if rules.IsBogota {
		t.Error("Rules.IsBogota must be false at block 1 with no BogotaTime set")
	}
	if rules.IsEIP4762 {
		t.Error("Rules.IsEIP4762 must be false — gated on IsVerkle, which XDC never activates")
	}
	if rules.IsEIP3860 != rules.IsEIP1559 {
		t.Errorf("Rules.IsEIP3860 (%v) must track Rules.IsEIP1559 (%v) on XDC chains, not IsShanghai (refs #716)", rules.IsEIP3860, rules.IsEIP1559)
	}
}

// TestApplySigningTransactionZeroGasReceipt is the golden-vector regression
// test for the #1364 failure chain: a signing tx (0x89 BlockSigners) at/after
// TIPSigning must produce a zero-gas receipt via ApplySigningTransaction. If
// any of the three conflicts on this path regress (state_transition.go's
// preCheck IsSpecialTx guard, validation.go's gasPrice=0 exemption, or
// state_processor.go's ApplySigningTransaction dispatch), a sign tx either
// fails validation or is executed through the EVM with non-zero gas, and
// after enough dropped sign txs totalSigners=0 causes a permanent
// "invalid merkle root" at the next epoch checkpoint.
func TestApplySigningTransactionZeroGasReceipt(t *testing.T) {
	db := state.NewDatabaseForTesting()
	statedb, err := state.New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	key, from := signingTestKey(t)
	statedb.SetNonce(from, 0, 0)

	cfg := *params.TestChainConfig // ByzantiumBlock = 0

	blockNumber := new(big.Int).Set(common.TIPSigning) // at-or-after TIPSigning
	if !cfg.IsTIPSigning(blockNumber) {
		t.Fatalf("test precondition failed: block %v is not past common.TIPSigning=%v", blockNumber, common.TIPSigning)
	}

	raw := types.NewTransaction(0, common.BlockSignersBinary, big.NewInt(0), 200000, big.NewInt(0), nil)
	tx, err := types.SignTx(raw, types.HomesteadSigner{}, key)
	if err != nil {
		t.Fatalf("sign tx: %v", err)
	}

	receipt, err := ApplySigningTransaction(&cfg, statedb, blockNumber, common.Hash{0x42}, tx, 0)
	if err != nil {
		t.Fatalf("ApplySigningTransaction: %v", err)
	}
	if receipt.GasUsed != 0 {
		t.Fatalf("signing tx receipt GasUsed = %d, want 0 (gasless, per #1364)", receipt.GasUsed)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("signing tx receipt Status = %d, want successful", receipt.Status)
	}
	if got := statedb.GetNonce(from); got != 1 {
		t.Fatalf("sender nonce after signing tx = %d, want 1", got)
	}
}
