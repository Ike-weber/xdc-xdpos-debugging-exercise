// Copyright 2026 The go-ethereum Authors
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
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// newSigningTx builds a well-formed BlockSigner signing transaction: to =
// common.BlockSignersBinary, data = 4-byte HexSignMethod selector + 32-byte
// blockNumber + 32-byte blockHash (68 bytes total). Mirrors
// contracts.CreateTxSign / miner/signing_tx_worker_test.go's
// TestCreateTxSign_Parity fixture, reimplemented here to avoid importing the
// contracts package into consensus/XDPoS.
func newSigningTx(nonce uint64, blockNumber int64, blockHashByte byte) *types.Transaction {
	data := common.Hex2Bytes(common.HexSignMethod)
	data = append(data, common.LeftPadBytes(big.NewInt(blockNumber).Bytes(), 32)...)
	var bh common.Hash
	for i := range bh {
		bh[i] = blockHashByte
	}
	data = append(data, bh.Bytes()...)
	return types.NewTransaction(nonce, common.BlockSignersBinary, big.NewInt(0), 200000, big.NewInt(0), data)
}

// rawStoredReceipt round-trips a receipt through the SAME storage encoding
// rawdb.ReadRawReceipts uses (types.ReceiptForStorage / storedReceiptRLP),
// which carries only status, cumulative gas and logs -- never TxHash. This
// reproduces exactly what rawdb.ReadRawReceipts hands back in production: a
// receipt whose TxHash is the zero hash, regardless of which real tx it
// belongs to.
func rawStoredReceipt(status uint64) *types.Receipt {
	original := &types.Receipt{Status: status}
	blob, err := rlp.EncodeToBytes((*types.ReceiptForStorage)(original))
	if err != nil {
		panic(err)
	}
	var decoded types.ReceiptForStorage
	if err := rlp.DecodeBytes(blob, &decoded); err != nil {
		panic(err)
	}
	out := (*types.Receipt)(&decoded)
	// Sanity-anchor the fixture on the exact defect: TxHash must be zero,
	// exactly like rawdb.ReadRawReceipts returns.
	if out.TxHash != (common.Hash{}) {
		panic("rawStoredReceipt fixture: TxHash unexpectedly non-zero")
	}
	return out
}

// TestCacheData_MissingReceiptKeepsSigningTx is the "must fail without the
// fix" test for the CacheData hardening (issue #1364 item 2). It builds
// receipts EXACTLY as rawdb.ReadRawReceipts actually returns them for a real
// block: TxHash is the zero hash (ReceiptForStorage/storedReceiptRLP never
// carries it), status is ReceiptStatusSuccessful. Against those receipts, no
// tx's hash can ever match any receipt's TxHash -- "no matching receipt
// found" and "found a failed receipt" must not collide, or every signing tx
// silently vanishes (exactly the composed defect that produced
// totalSigners=0 on netv12). CacheData must keep the signing tx.
//
// Before the fix: CacheData's unmatched-receipt default (var b uint64,
// never assigned) equals types.ReceiptStatusFailed (also 0), so `if b ==
// types.ReceiptStatusFailed { continue }` drops it. This test fails on that
// code (len(kept) == 0) and passes after the fix (len(kept) == 1).
func TestCacheData_MissingReceiptKeepsSigningTx(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	header := &types.Header{Number: big.NewInt(16818)}
	tx := newSigningTx(1, 15926, 0xAB)

	// Raw receipts as ReadRawReceipts returns them: TxHash zero, status
	// successful. They correspond POSITIONALLY to some other, unrelated set
	// of txs in storage -- not to `tx` -- exactly as happens when the
	// receipt list for the actual signing tx set was never correctly
	// threaded through (or, in production, is simply unmatchable because
	// TxHash is always zero).
	receipts := []*types.Receipt{
		rawStoredReceipt(types.ReceiptStatusSuccessful),
	}

	kept := engine.CacheData(header, []*types.Transaction{tx}, receipts)

	if len(kept) != 1 {
		t.Fatalf("CacheData must keep a signing tx when no receipt matches it (missing != failed): got %d kept, want 1", len(kept))
	}
	if kept[0].Hash() != tx.Hash() {
		t.Fatalf("CacheData kept the wrong tx: got %s, want %s", kept[0].Hash(), tx.Hash())
	}
}

// TestCacheData_ActuallyFailedReceiptIsDropped is the companion regression
// test: a signing tx whose receipt DOES match by TxHash and DOES report
// ReceiptStatusFailed must still be dropped. This guards against a hardening
// fix that over-corrects into "always keep everything".
func TestCacheData_ActuallyFailedReceiptIsDropped(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	header := &types.Header{Number: big.NewInt(16818)}
	tx := newSigningTx(2, 15926, 0xCD)

	failedReceipt := &types.Receipt{
		Status: types.ReceiptStatusFailed,
		TxHash: tx.Hash(), // matches this tx exactly, unlike the raw-receipts case
	}

	kept := engine.CacheData(header, []*types.Transaction{tx}, []*types.Receipt{failedReceipt})

	if len(kept) != 0 {
		t.Fatalf("CacheData must drop a signing tx with a matched, actually-failed receipt: got %d kept, want 0", len(kept))
	}
}

// TestCacheData_MixedFoundAndMissingReceipts exercises all three cases in
// one block: a matched-successful receipt, a matched-failed receipt, and no
// receipt at all (the netv12 shape) -- asserting each signing tx is handled
// independently and correctly.
func TestCacheData_MixedFoundAndMissingReceipts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	engine := New(testConfig, db)

	header := &types.Header{Number: big.NewInt(16818)}
	txSuccess := newSigningTx(3, 15927, 0x01)
	txFailed := newSigningTx(4, 15928, 0x02)
	txNoReceipt := newSigningTx(5, 15929, 0x03)

	receipts := []*types.Receipt{
		{Status: types.ReceiptStatusSuccessful, TxHash: txSuccess.Hash()},
		{Status: types.ReceiptStatusFailed, TxHash: txFailed.Hash()},
		// txNoReceipt has nothing in this slice at all.
	}

	kept := engine.CacheData(header, []*types.Transaction{txSuccess, txFailed, txNoReceipt}, receipts)

	gotHashes := make(map[common.Hash]bool, len(kept))
	for _, tx := range kept {
		gotHashes[tx.Hash()] = true
	}
	if !gotHashes[txSuccess.Hash()] {
		t.Errorf("expected matched-successful tx to be kept")
	}
	if gotHashes[txFailed.Hash()] {
		t.Errorf("expected matched-failed tx to be dropped")
	}
	if !gotHashes[txNoReceipt.Hash()] {
		t.Errorf("expected no-matching-receipt tx to be kept (missing != failed)")
	}
	if len(kept) != 2 {
		t.Errorf("expected exactly 2 kept txs, got %d", len(kept))
	}
}
