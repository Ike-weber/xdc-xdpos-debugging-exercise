// Copyright (c) 2026 XDC Network Authors
//
// This file tests the XDC special-transaction (gasPrice=0 signing txs to
// BlockSignersBinary/RandomizeSMCBinary) exemption in the legacy tx pool.
//
// Canonical reference: XDPoSChain/core/txpool/txpool.go:675,710-730,779-780

package legacypool

import (
	"math/big"
	"testing"

	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// xdcSpecialPool returns a pool with a 1 Gwei MinTip to exercise the
// special-tx exemption (ordinary gasPrice=0 txs would fail at this MinTip).
func xdcSpecialPool(t *testing.T) (*LegacyPool, func()) {
	t.Helper()

	cpy := *params.TestChainConfig
	cpy.LondonBlock = common.Big0
	cpy.BerlinBlock = common.Big0

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	bc := newTestBlockChain(&cpy, 10_000_000, statedb, new(event.Feed))

	cfg := DefaultConfig
	cfg.Journal = ""
	cfg.NoLocals = true // disable local-tx pricing exception — test special-tx path explicitly

	pool := New(cfg, bc)
	if err := pool.Init(cfg.PriceLimit, bc.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("pool init: %v", err)
	}
	<-pool.initDoneCh
	// Raise the min tip so ordinary gasPrice=0 txs are rejected.
	pool.SetGasTip(big.NewInt(1_000_000_000)) // 1 Gwei

	return pool, func() { pool.Close() }
}

// makeSpecialTx builds a signed gasPrice=0 tx to dest (BlockSigners or Randomize).
// Mirrors CreateTxSign in contracts/utils.go:56 (gasPrice = big.NewInt(0)).
func makeSpecialTx(nonce uint64, dest common.Address, privKey *ecdsa.PrivateKey) *types.Transaction {
	data := common.Hex2Bytes(common.HexSignMethod)
	blockNum := new(big.Int).SetUint64(nonce * 15)
	inputData := append(data, common.LeftPadBytes(blockNum.Bytes(), 32)...)
	inputData = append(inputData, common.LeftPadBytes(blockNum.Bytes(), 32)...)
	// gasPrice = big.NewInt(0) — canonical gasless special tx.
	// XDPoSChain/contracts/utils.go:173 and this fork's contracts/utils.go:56.
	raw := types.NewTransaction(nonce, dest, big.NewInt(0), 200000, big.NewInt(0), inputData)
	tx, err := types.SignTx(raw, types.HomesteadSigner{}, privKey)
	if err != nil {
		panic(err)
	}
	return tx
}

// makeSigningTxForBlock builds a signed BlockSigners tx that signs a specific
// targetBlock.  Used for freshness-expiry tests where the block number embedded
// in the tx data must be controlled precisely.
func makeSigningTxForBlock(nonce, targetBlock uint64, privKey *ecdsa.PrivateKey) *types.Transaction {
	data := common.Hex2Bytes(common.HexSignMethod)
	blockNumBig := new(big.Int).SetUint64(targetBlock)
	var blockHash common.Hash
	blockHash[0] = byte(targetBlock & 0xff)
	inputData := append(data, common.LeftPadBytes(blockNumBig.Bytes(), 32)...)
	inputData = append(inputData, blockHash[:]...)
	raw := types.NewTransaction(nonce, common.BlockSignersBinary, big.NewInt(0), 200000, big.NewInt(0), inputData)
	tx, err := types.SignTx(raw, types.HomesteadSigner{}, privKey)
	if err != nil {
		panic(err)
	}
	return tx
}

// xdcSpecialPoolWithEpoch returns a pool configured for XDPoS with a given
// epoch, allowing expiry tests.
func xdcSpecialPoolWithEpoch(t *testing.T, epoch uint64) (*LegacyPool, func()) {
	t.Helper()

	cpy := *params.TestChainConfig
	cpy.LondonBlock = common.Big0
	cpy.BerlinBlock = common.Big0
	cpy.XDPoS = &params.XDPoSConfig{Epoch: epoch}

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	bc := newTestBlockChain(&cpy, 10_000_000, statedb, new(event.Feed))

	cfg := DefaultConfig
	cfg.Journal = ""
	cfg.NoLocals = true

	pool := New(cfg, bc)
	if err := pool.Init(cfg.PriceLimit, bc.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("pool init: %v", err)
	}
	<-pool.initDoneCh
	pool.SetGasTip(big.NewInt(1_000_000_000))

	return pool, func() { pool.Close() }
}

// TestXDCSpecialTxLocalAdd verifies that a gasPrice=0 tx to BlockSignersBinary
// is accepted by AddLocal even when MinTip is set to 1 Gwei.
//
// XDPoSChain parity: txpool.go:710-730 — the entire ErrZeroGasPrice /
// ErrUnderMinGasPrice block is guarded by !tx.IsSpecialTransaction().
func TestXDCSpecialTxLocalAdd(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	// Tiny balance: cost = value + gasPrice*gas = 0 + 0*200000 = 0,
	// so balance=1 Wei is sufficient.
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	tx := makeSpecialTx(0, common.BlockSignersBinary, privKey)

	if !tx.IsSpecialTransaction() {
		t.Fatal("tx to BlockSignersBinary must be IsSpecialTransaction()==true")
	}

	errs := pool.Add([]*types.Transaction{tx}, true /* sync */)
	if errs[0] != nil {
		t.Fatalf("AddLocal of gasPrice=0 special tx rejected: %v", errs[0])
	}

	pending, _ := pool.Pending(txpool.PendingFilter{})
	if _, ok := pending[addr]; !ok {
		t.Fatal("special tx not found in pending after AddLocal")
	}
}

// TestXDCSpecialTxRemoteAdd verifies that a gasPrice=0 tx to BlockSignersBinary
// is accepted via the remote (gossip) path.
//
// XDPoSChain parity: txpool.go:675 — special txs from registered signers are
// exempt from ErrUnderpriced even for remote adds.
func TestXDCSpecialTxRemoteAdd(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	tx := makeSpecialTx(0, common.BlockSignersBinary, privKey)

	// addRemoteSync simulates the p2p gossip (non-local) path.
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("addRemote of gasPrice=0 special tx rejected: %v", err)
	}

	pending, _ := pool.Pending(txpool.PendingFilter{})
	if _, ok := pending[addr]; !ok {
		t.Fatal("special tx not found in pending after remote add")
	}
}

// TestXDCNonSpecialTxZeroGasPriceRejected verifies that a gasPrice=0 tx to a
// non-special address IS still rejected. Only 0x89/0x90 destinations are exempt.
//
// XDPoSChain parity: txpool.go:710 — guard is !tx.IsSpecialTransaction().
func TestXDCNonSpecialTxZeroGasPriceRejected(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1_000_000_000_000_000_000), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	// Normal tx to a non-special address with gasPrice=0 — must be rejected.
	dest := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	raw := types.NewTransaction(0, dest, big.NewInt(0), 21000, big.NewInt(0), nil)
	tx, _ := types.SignTx(raw, types.HomesteadSigner{}, privKey)

	if tx.IsSpecialTransaction() {
		t.Fatal("tx to non-special address must NOT be IsSpecialTransaction()")
	}

	errs := pool.Add([]*types.Transaction{tx}, true)
	if errs[0] == nil {
		t.Fatal("gasPrice=0 tx to non-special address should have been rejected by MinTip check")
	}
}

// TestXDCRandomizeTxLocalAdd verifies that a gasPrice=0 tx to RandomizeSMCBinary
// (0x90) is also accepted — the special-address set covers both 0x89 and 0x90.
//
// XDPoSChain/core/types/transaction.go:485:
//
//	IsSpecialTx = to == BlockSignersBinary || to == RandomizeSMCBinary
func TestXDCRandomizeTxLocalAdd(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	tx := makeSpecialTx(0, common.RandomizeSMCBinary, privKey)

	if !tx.IsSpecialTransaction() {
		t.Fatal("tx to RandomizeSMCBinary must be IsSpecialTransaction()==true")
	}

	errs := pool.Add([]*types.Transaction{tx}, true)
	if errs[0] != nil {
		t.Fatalf("AddLocal of gasPrice=0 special tx to RandomizeSMC rejected: %v", errs[0])
	}

	pending, _ := pool.Pending(txpool.PendingFilter{})
	if _, ok := pending[addr]; !ok {
		t.Fatal("RandomizeSMC special tx not found in pending")
	}
}

// TestXDCCreateTxSignGasPriceZero verifies that contracts.CreateTxSign builds
// a tx with gasPrice=0 exactly matching canonical XDPoSChain.
//
// Regression guard against commit d6b863156 which wrongly raised gasPrice to
// 25 Gwei — canonical on-chain txs use gasPrice=0 (sample blocks 82958701,
// 82958716, 82958731 all show gasPrice:0x0).
//
// XDPoSChain/contracts/utils.go:173: gasPrice = big.NewInt(0).
func TestXDCCreateTxSignGasPriceZero(t *testing.T) {
	t.Parallel()

	tx := contracts.CreateTxSign(
		big.NewInt(82_958_701),
		common.HexToHash("0xdeadbeef"),
		42,
		common.BlockSignersBinary,
	)
	if tx == nil {
		t.Fatal("CreateTxSign returned nil")
	}
	if tx.GasPrice().Sign() != 0 {
		t.Fatalf("CreateTxSign gasPrice must be 0, got %v — canonical XDPoSChain/contracts/utils.go:173 uses big.NewInt(0)", tx.GasPrice())
	}
	if tx.Value().Sign() != 0 {
		t.Fatalf("CreateTxSign value must be 0, got %v", tx.Value())
	}
	if !tx.IsSpecialTransaction() {
		t.Fatal("CreateTxSign output must satisfy IsSpecialTransaction() (dest == BlockSignersBinary)")
	}
}

// TestXDCSpecialTxIsSigner verifies that the promoteSpecialTx fast-path is
// used when IsSigner is wired and the sender is a registered masternode.
//
// XDPoSChain/core/txpool/txpool.go:779-780:
//
//	if tx.IsSpecialTransaction() && pool.IsSigner != nil && pool.IsSigner(from)
//	   && pool.pendingNonces.get(from) == tx.Nonce() { promoteSpecialTx }
func TestXDCSpecialTxIsSigner(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	// Register the sender as a known masternode signer.
	pool.IsSigner = func(address common.Address) bool {
		return address == addr
	}

	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	tx := makeSpecialTx(0, common.BlockSignersBinary, privKey)

	errs := pool.Add([]*types.Transaction{tx}, true)
	if errs[0] != nil {
		t.Fatalf("IsSigner fast-path special tx rejected: %v", errs[0])
	}

	pending, _ := pool.Pending(txpool.PendingFilter{})
	senderTxs, ok := pending[addr]
	if !ok || len(senderTxs) == 0 {
		t.Fatal("special tx not in pending after IsSigner fast-path")
	}
}

// TestXDCSpecialTxDuplicateRejected verifies that a second special tx at the
// same nonce via the IsSigner fast-path is rejected with ErrDuplicateSpecialTransaction.
//
// XDPoSChain/core/txpool/txpool.go:960-961.
func TestXDCSpecialTxDuplicateRejected(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPool(t)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.IsSigner = func(address common.Address) bool {
		return address == addr
	}

	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	// First special tx at nonce 0 — must succeed.
	tx1 := makeSpecialTx(0, common.BlockSignersBinary, privKey)
	if errs := pool.Add([]*types.Transaction{tx1}, true); errs[0] != nil {
		t.Fatalf("first special tx rejected: %v", errs[0])
	}

	// Second special tx at same nonce (nonce=0) — must be rejected.
	tx2 := makeSpecialTx(0, common.BlockSignersBinary, privKey)
	errs := pool.Add([]*types.Transaction{tx2}, true)
	if errs[0] == nil {
		t.Fatal("second special tx at same nonce should have been rejected as duplicate")
	}
}

// ─── Tests for isSigningTxExpired and expired-tx replacement (#932) ───────────

// TestXDCIsSigningTxExpired_Expired verifies that a BlockSigners signing tx
// whose target block is outside the epoch*2 window is correctly identified
// as expired.
//
// BlockSigner.sol:21: require(block.number <= _blockNumber + epochNumber*2).
func TestXDCIsSigningTxExpired_Expired(t *testing.T) {
	t.Parallel()

	// epoch = 900, so expiry window = 1800 blocks past target.
	pool, close := xdcSpecialPoolWithEpoch(t, 900)
	defer close()

	// Build a signing tx targeting block 15.
	privKey, _ := crypto.GenerateKey()
	tx := makeSigningTxForBlock(0, 15, privKey)

	// Set chain head far ahead of block 15 + epoch*2 (=1815).
	pool.currentHead.Store(&types.Header{Number: big.NewInt(2000)})

	if !pool.isSigningTxExpired(tx) {
		t.Errorf("tx targeting block 15 should be expired when head=2000, epoch=900 (expiry at 1815)")
	}
}

// TestXDCIsSigningTxExpired_Fresh verifies that a signing tx whose target block
// is within the epoch*2 window is classified as fresh.
func TestXDCIsSigningTxExpired_Fresh(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPoolWithEpoch(t, 900)
	defer close()

	privKey, _ := crypto.GenerateKey()
	tx := makeSigningTxForBlock(0, 15, privKey)

	// Head = 100: 15 + 900*2 = 1815, and 100 <= 1815 — still fresh.
	pool.currentHead.Store(&types.Header{Number: big.NewInt(100)})

	if pool.isSigningTxExpired(tx) {
		t.Errorf("tx targeting block 15 should NOT be expired when head=100, epoch=900 (expiry at 1815)")
	}
}

// TestXDCIsSigningTxExpired_AtBoundary verifies the exact boundary:
// head == targetBlock + epoch*2 is NOT expired (the contract allows <=).
func TestXDCIsSigningTxExpired_AtBoundary(t *testing.T) {
	t.Parallel()

	const epoch = uint64(900)
	pool, close := xdcSpecialPoolWithEpoch(t, epoch)
	defer close()

	privKey, _ := crypto.GenerateKey()
	const target = uint64(15)
	tx := makeSigningTxForBlock(0, target, privKey)

	// head == target + epoch*2 = 1815 — exactly at expiry, contract still accepts.
	pool.currentHead.Store(&types.Header{Number: big.NewInt(int64(target + epoch*2))})

	if pool.isSigningTxExpired(tx) {
		t.Errorf("tx should not be expired when head == target + epoch*2 (contract: <=)")
	}

	// head == target + epoch*2 + 1 — one past expiry, contract rejects.
	pool.currentHead.Store(&types.Header{Number: big.NewInt(int64(target + epoch*2 + 1))})

	if !pool.isSigningTxExpired(tx) {
		t.Errorf("tx should be expired when head == target + epoch*2 + 1")
	}
}

// TestXDCIsSigningTxExpired_NotBlockSignersTx verifies that a non-BlockSigners
// tx (e.g. to RandomizeSMC) is never classified as expired — we only gate
// the BlockSigners signing window.
func TestXDCIsSigningTxExpired_NotBlockSignersTx(t *testing.T) {
	t.Parallel()

	pool, close := xdcSpecialPoolWithEpoch(t, 900)
	defer close()

	privKey, _ := crypto.GenerateKey()
	// Build a special tx to RandomizeSMC (not BlockSigners) — should never expire.
	tx := makeSigningTxForBlock(0, 15, privKey)
	// Re-sign to RandomizeSMC address.
	data := common.Hex2Bytes(common.HexSignMethod)
	blockNumBig := big.NewInt(15)
	inputData := append(data, common.LeftPadBytes(blockNumBig.Bytes(), 32)...)
	inputData = append(inputData, make([]byte, 32)...)
	raw := types.NewTransaction(0, common.RandomizeSMCBinary, big.NewInt(0), 200000, big.NewInt(0), inputData)
	randomizeTx, _ := types.SignTx(raw, types.HomesteadSigner{}, privKey)
	_ = tx

	// Even with head far ahead, RandomizeSMC tx is never expired.
	pool.currentHead.Store(&types.Header{Number: big.NewInt(99999)})
	if pool.isSigningTxExpired(randomizeTx) {
		t.Errorf("RandomizeSMC tx should never be classified as expired")
	}
}

// TestXDCExpiredSigningTxReplacement verifies that an expired signing tx at
// a given nonce CAN be replaced by a new signing tx via the IsSigner fast-path.
//
// Fixes #932: the old code returned ErrDuplicateSpecialTransaction for any
// special-tx replacement; the new code allows replacement when the existing
// tx's target block is expired.
func TestXDCExpiredSigningTxReplacement(t *testing.T) {
	t.Parallel()

	const epoch = uint64(900)
	pool, close := xdcSpecialPoolWithEpoch(t, epoch)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.IsSigner = func(address common.Address) bool { return address == addr }
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	// Insert first signing tx targeting block 15 (nonce=0).
	tx1 := makeSigningTxForBlock(0, 15, privKey)
	if errs := pool.Add([]*types.Transaction{tx1}, true); errs[0] != nil {
		t.Fatalf("first signing tx rejected: %v", errs[0])
	}

	// Advance head past the expiry window for block 15: 15 + 900*2 + 1 = 1816.
	pool.mu.Lock()
	pool.currentHead.Store(&types.Header{Number: big.NewInt(1816), Difficulty: common.Big0, GasLimit: 10_000_000})
	pool.mu.Unlock()

	// Now insert a new signing tx at the same nonce targeting the current head.
	// The old tx is expired → replacement must succeed.
	tx2 := makeSigningTxForBlock(0, 1816, privKey)
	errs := pool.Add([]*types.Transaction{tx2}, true)
	if errs[0] != nil {
		t.Fatalf("replacement of expired signing tx rejected: %v (want nil)", errs[0])
	}

	// Verify the new tx is in pending.
	pending, _ := pool.Pending(txpool.PendingFilter{})
	senderTxs, ok := pending[addr]
	if !ok || len(senderTxs) == 0 {
		t.Fatal("replacement signing tx not in pending")
	}
	if senderTxs[0].Hash != tx2.Hash() {
		t.Errorf("pending tx is old tx (hash %s), want new tx (hash %s)",
			senderTxs[0].Hash, tx2.Hash())
	}
}

// TestXDCFreshSigningTxNotReplaced verifies that a FRESH (non-expired) signing
// tx at a nonce CANNOT be replaced with a same-gasPrice signing tx.
//
// When an incumbent special tx is at nonce N and fresh, a new special tx at
// the same nonce must be rejected.  The rejection mechanism depends on context:
//   - If submitted via the IsSigner fast-path (pendingNonces == tx.Nonce()),
//     promoteSpecialTx returns ErrDuplicateSpecialTransaction.
//   - If the fast-path nonce check fails (pendingNonces > tx.Nonce()), the
//     normal add() path rejects with ErrReplaceUnderpriced (gasPrice=0 on both).
//   - Either way: the duplicate tx must be rejected (the pool is not jammed).
func TestXDCFreshSigningTxNotReplaced(t *testing.T) {
	t.Parallel()

	const epoch = uint64(900)
	pool, close := xdcSpecialPoolWithEpoch(t, epoch)
	defer close()

	privKey, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	pool.IsSigner = func(address common.Address) bool { return address == addr }
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, new(uint256.Int).SetUint64(1), tracing.BalanceChangeUnspecified)
	pool.mu.Unlock()

	// Insert first signing tx targeting block 15.  Head = 0 (genesis) → fresh.
	tx1 := makeSigningTxForBlock(0, 15, privKey)
	if errs := pool.Add([]*types.Transaction{tx1}, true); errs[0] != nil {
		t.Fatalf("first signing tx rejected: %v", errs[0])
	}

	// Attempt to replace with another signing tx at same nonce.
	// The incumbent targets block 15; head=0, expiry=1815 → NOT expired.
	// After tx1 insertion: pendingNonces[addr]=1. The new tx has nonce=0,
	// so the IsSigner fast-path (which requires pendingNonces==nonce) is not
	// taken, and the non-expired check routes through normal add() →
	// ErrReplaceUnderpriced (both gasPrice=0 → no price bump possible).
	tx2 := makeSigningTxForBlock(0, 30, privKey)
	errs := pool.Add([]*types.Transaction{tx2}, true)
	if errs[0] == nil {
		t.Fatal("replacement of non-expired signing tx should fail")
	}
	// Must be some rejection error — either ErrDuplicateSpecialTransaction
	// (fast-path) or ErrReplaceUnderpriced (normal path).  Either is correct.
	if errs[0] == nil {
		t.Fatal("second signing tx at same nonce must be rejected")
	}
}
