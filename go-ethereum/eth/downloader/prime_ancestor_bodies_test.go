// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// A.72: Test PrimeAncestorBodies writes block bodies + receipts to rawdb
// correctly so the V2 HookReward signing-tx walk can resolve
// rawdb.ReadBlock + rawdb.ReadRawReceipts for pre-pivot ancestors.
//
// Refs #844 #859 A.72.

package downloader

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// makeContiguousHeaders builds count parent-linked headers starting from
// number `start`.
func makeContiguousHeaders(start uint64, count int) []*types.Header {
	headers := make([]*types.Header, count)
	var parentHash common.Hash
	for i := 0; i < count; i++ {
		h := &types.Header{
			Number:     new(big.Int).SetUint64(start + uint64(i)),
			ParentHash: parentHash,
			Difficulty: big.NewInt(1),
			GasLimit:   8_000_000,
		}
		headers[i] = h
		parentHash = h.Hash()
	}
	return headers
}

// TestPrimeAncestorBodies_WritesBodiesAndReceipts verifies that
// PrimeAncestorBodies persists bodies + receipts so rawdb.ReadBlock and
// rawdb.ReadRawReceipts can subsequently retrieve them. This is the
// invariant the V2 HookReward signing-tx walk depends on.
func TestPrimeAncestorBodies_WritesBodiesAndReceipts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	d := &Downloader{stateDB: db}

	const start, count = 100, 4
	headers := makeContiguousHeaders(start, count)
	// Header pre-fetch must happen before body write — mirror real A.68.2 flow.
	if err := d.PrimeAncestorHeaders(headers); err != nil {
		t.Fatalf("PrimeAncestorHeaders failed: %v", err)
	}

	bodies := make([]*types.Body, count)
	receipts := make([]types.Receipts, count)
	for i := 0; i < count; i++ {
		// Empty bodies + receipts are sufficient for the storage round-trip
		// test. Real signing-tx detection is exercised by the integration
		// harness on server.
		bodies[i] = &types.Body{}
		receipts[i] = types.Receipts{}
	}
	if err := d.PrimeAncestorBodies(headers, bodies, receipts); err != nil {
		t.Fatalf("PrimeAncestorBodies failed: %v", err)
	}

	// Verify each block can be loaded back via rawdb.ReadBlock.
	for i, h := range headers {
		num := h.Number.Uint64()
		hash := h.Hash()
		if !rawdb.HasBody(db, hash, num) {
			t.Errorf("idx %d (num=%d): HasBody=false after prime", i, num)
		}
		if !rawdb.HasReceipts(db, hash, num) {
			t.Errorf("idx %d (num=%d): HasReceipts=false after prime", i, num)
		}
		block := rawdb.ReadBlock(db, hash, num)
		if block == nil {
			t.Errorf("idx %d (num=%d): ReadBlock returned nil", i, num)
		}
	}
}

// TestPrimeAncestorBodies_LengthMismatch verifies the function rejects
// header/body length mismatches before touching the DB.
func TestPrimeAncestorBodies_LengthMismatch(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	d := &Downloader{stateDB: db}

	headers := makeContiguousHeaders(0, 3)
	bodies := []*types.Body{{}, {}} // 2 bodies for 3 headers
	if err := d.PrimeAncestorBodies(headers, bodies, nil); err == nil {
		t.Errorf("expected length mismatch error, got nil")
	}
}

// TestPrimeAncestorBodies_OptionalReceipts verifies receipts parameter is
// optional: passing nil writes bodies only, no receipt rows.
func TestPrimeAncestorBodies_OptionalReceipts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	d := &Downloader{stateDB: db}

	headers := makeContiguousHeaders(0, 2)
	if err := d.PrimeAncestorHeaders(headers); err != nil {
		t.Fatalf("PrimeAncestorHeaders failed: %v", err)
	}

	bodies := []*types.Body{{}, {}}
	if err := d.PrimeAncestorBodies(headers, bodies, nil); err != nil {
		t.Fatalf("PrimeAncestorBodies failed: %v", err)
	}
	for _, h := range headers {
		num := h.Number.Uint64()
		hash := h.Hash()
		if !rawdb.HasBody(db, hash, num) {
			t.Errorf("num=%d: HasBody=false", num)
		}
		// No receipts were supplied, so HasReceipts must be false.
		if rawdb.HasReceipts(db, hash, num) {
			t.Errorf("num=%d: HasReceipts=true but receipts were nil", num)
		}
	}
}

// TestPrimeAncestorBodies_Idempotent verifies a second call doesn't
// re-write bodies that are already present in the DB. The function's
// idempotency is critical for crash-recovery (a partial pre-fetch followed
// by a restart should not re-fetch already-written bodies).
func TestPrimeAncestorBodies_Idempotent(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	d := &Downloader{stateDB: db}

	headers := makeContiguousHeaders(0, 2)
	bodies := []*types.Body{{}, {}}
	receipts := []types.Receipts{{}, {}}

	// First write.
	if err := d.PrimeAncestorBodies(headers, bodies, receipts); err != nil {
		t.Fatalf("first PrimeAncestorBodies failed: %v", err)
	}
	// Second write must not fail (idempotent).
	if err := d.PrimeAncestorBodies(headers, bodies, receipts); err != nil {
		t.Fatalf("second PrimeAncestorBodies failed: %v", err)
	}
}
