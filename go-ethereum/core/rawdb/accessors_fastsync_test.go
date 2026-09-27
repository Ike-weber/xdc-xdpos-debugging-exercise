// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Refs A.42 of #844: tests the on-disk fast-sync checkpoint accessors that
// let an XDPOS2 classical fast-sync survive a crash-restart against the same
// operator-pinned pivot instead of starting state download from scratch.

package rawdb

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestFastSyncCheckpoint exercises the round-trip of the A.42 fast-sync
// checkpoint key (write, read, delete). It also asserts that a fresh DB
// returns nil instead of a zero-valued struct, which the SetFastSyncPivot
// reconciliation path relies on for the "no checkpoint to restore" branch.
func TestFastSyncCheckpoint(t *testing.T) {
	db := NewMemoryDatabase()

	if cp := ReadFastSyncCheckpoint(db); cp != nil {
		t.Fatalf("expected nil on fresh DB, got %+v", cp)
	}

	want := &FastSyncCheckpoint{
		Number:    1234567,
		Hash:      common.BytesToHash([]byte("hash-bytes")),
		Root:      common.BytesToHash([]byte("root-bytes")),
		Pending:   42,
		Processed: 9876543,
	}
	WriteFastSyncCheckpoint(db, want)

	got := ReadFastSyncCheckpoint(db)
	if got == nil {
		t.Fatal("expected non-nil checkpoint after write")
	}
	if *got != *want {
		t.Fatalf("checkpoint mismatch: got %+v, want %+v", *got, *want)
	}

	DeleteFastSyncCheckpoint(db)
	if cp := ReadFastSyncCheckpoint(db); cp != nil {
		t.Fatalf("expected nil after delete, got %+v", cp)
	}
}
