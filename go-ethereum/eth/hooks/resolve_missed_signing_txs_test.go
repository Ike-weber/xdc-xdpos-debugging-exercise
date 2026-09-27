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

package hooks

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// newV2SigningTx builds a well-formed BlockSigner signing transaction: to =
// common.BlockSignersBinary, data = 4-byte HexSignMethod selector + 32-byte
// blockNumber + 32-byte blockHash (68 bytes total, matching
// contracts.CreateTxSign / consensus.isSigningTx's exact-68-byte guard).
func newV2SigningTx(nonce uint64, blockNumber int64, blockHashByte byte) *types.Transaction {
	data := common.Hex2Bytes(common.HexSignMethod)
	data = append(data, common.LeftPadBytes(big.NewInt(blockNumber).Bytes(), 32)...)
	var bh common.Hash
	for i := range bh {
		bh[i] = blockHashByte
	}
	data = append(data, bh.Bytes()...)
	return types.NewTransaction(nonce, common.BlockSignersBinary, big.NewInt(0), 200000, big.NewInt(0), data)
}

// rawStoredReceiptFor round-trips a receipt through the exact storage
// encoding rawdb.ReadRawReceipts uses (types.ReceiptForStorage), which never
// carries TxHash -- it is a derived field, structurally absent from storage.
// This reproduces precisely what a real geth datadir hands back for any
// receipt: TxHash is always the zero hash, regardless of which tx it
// belongs to.
func rawStoredReceiptFor(status uint64) *types.Receipt {
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
	if out.TxHash != (common.Hash{}) {
		panic("rawStoredReceiptFor fixture: TxHash unexpectedly non-zero")
	}
	return out
}

// TestResolveMissedSigningTxs_KeepsAllSigningTxsDespiteRawReceipts is the
// "must fail without the fix" test for issue #1364. It writes a block to
// rawdb containing real BlockSigner signing txs, PLUS raw-shaped receipts
// for that same block hash/number exactly as rawdb.ReadRawReceipts actually
// returns them in production (TxHash zero, status Successful) -- and asserts
// that resolveMissedSigningTxs (the V2 cache-miss fallback used by
// GetSigningTxCount, eth/hooks/engine_v2_hooks.go) keeps every one of them.
//
// Before the fix, this exact path (for a chain where IsTIPSigning is not
// registered -- true for any chainId CopyXDCConstants doesn't special-case)
// called CacheData(header, txs, rawdb.ReadRawReceipts(...)). Because those
// receipts' TxHash is always zero, CacheData's TxHash match against the
// real tx hash never succeeded, and the unmatched default collided with
// types.ReceiptStatusFailed (both zero) -- so every signing tx was silently
// dropped. Measured live on netv12: 369 signing txs present, 0 kept.
//
// Verified (see PR description) by reverting resolveMissedSigningTxs's body
// to the pre-fix IsTIPSigning+CacheData logic in a scratch copy: this test
// fails (0 kept) on that code and passes (3 kept) on the fixed code.
func TestResolveMissedSigningTxs_KeepsAllSigningTxsDespiteRawReceipts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	config := &params.XDPoSConfig{Period: 2, Epoch: 900, Reward: 5000, RewardCheckpoint: 900, Gap: 450}
	engine := XDPoS.New(config, db)

	signingTxs := []*types.Transaction{
		newV2SigningTx(1, 15026, 0x01),
		newV2SigningTx(2, 15041, 0x02),
		newV2SigningTx(3, 15056, 0x03),
	}
	// A non-signing tx must be left out entirely, matched or not.
	otherTx := types.NewTransaction(4, common.HexToAddress("0x00000000000000000000000000000000001234"), big.NewInt(1), 21000, big.NewInt(1), nil)

	header := &types.Header{Number: big.NewInt(15057), Difficulty: big.NewInt(1)}
	body := types.Body{Transactions: append(append([]*types.Transaction{}, signingTxs...), otherTx)}
	block := types.NewBlockWithHeader(header).WithBody(body)
	rawdb.WriteBlock(db, block)

	// Raw receipts for this exact block hash/number, shaped exactly like
	// production: TxHash zero, status successful. These can never TxHash-match
	// any of the real txs above.
	rawReceipts := types.Receipts{
		rawStoredReceiptFor(types.ReceiptStatusSuccessful),
		rawStoredReceiptFor(types.ReceiptStatusSuccessful),
		rawStoredReceiptFor(types.ReceiptStatusSuccessful),
		rawStoredReceiptFor(types.ReceiptStatusSuccessful),
	}
	rawdb.WriteReceipts(db, block.Hash(), block.NumberU64(), rawReceipts)

	// Sanity: confirm the fixture actually reproduces the documented shape
	// before asserting anything about the code under test.
	readBack := rawdb.ReadRawReceipts(db, block.Hash(), block.NumberU64())
	if len(readBack) != len(rawReceipts) {
		t.Fatalf("fixture setup: ReadRawReceipts returned %d receipts, want %d", len(readBack), len(rawReceipts))
	}
	for i, r := range readBack {
		if r.TxHash != (common.Hash{}) {
			t.Fatalf("fixture setup: receipt %d has non-zero TxHash %s; rawdb.ReadRawReceipts should never populate it", i, r.TxHash)
		}
	}

	kept, totalTxsInBlock, found := resolveMissedSigningTxs(engine, block.Hash(), block.NumberU64())

	if !found {
		t.Fatalf("resolveMissedSigningTxs: block not found")
	}
	if totalTxsInBlock != len(signingTxs)+1 {
		t.Fatalf("totalTxsInBlock: got %d, want %d", totalTxsInBlock, len(signingTxs)+1)
	}
	if len(kept) != len(signingTxs) {
		t.Fatalf("resolveMissedSigningTxs must keep every signing tx regardless of unmatchable raw receipts: got %d kept, want %d", len(kept), len(signingTxs))
	}
	gotHashes := make(map[common.Hash]bool, len(kept))
	for _, tx := range kept {
		gotHashes[tx.Hash()] = true
	}
	for _, want := range signingTxs {
		if !gotHashes[want.Hash()] {
			t.Errorf("expected signing tx %s to be kept, was dropped", want.Hash())
		}
	}
	if gotHashes[otherTx.Hash()] {
		t.Errorf("non-signing tx %s must never be kept", otherTx.Hash())
	}
}

// TestResolveMissedSigningTxs_BlockNotFound confirms the `found` return
// value distinguishes "block genuinely absent from rawdb" from "block
// present with zero signing txs" -- the two were conflated in the original
// inline miss-path code (both looked like "no signing txs").
func TestResolveMissedSigningTxs_BlockNotFound(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	config := &params.XDPoSConfig{Period: 2, Epoch: 900, Reward: 5000, RewardCheckpoint: 900, Gap: 450}
	engine := XDPoS.New(config, db)

	kept, totalTxsInBlock, found := resolveMissedSigningTxs(engine, common.HexToHash("0xdeadbeef"), 999)
	if found {
		t.Fatalf("expected found=false for a block absent from rawdb")
	}
	if kept != nil || totalTxsInBlock != 0 {
		t.Fatalf("expected zero-value results for a not-found block, got kept=%v totalTxsInBlock=%d", kept, totalTxsInBlock)
	}
}
