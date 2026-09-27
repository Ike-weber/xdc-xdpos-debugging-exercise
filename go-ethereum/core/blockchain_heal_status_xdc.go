// Copyright 2026 The go-ethereum Authors
// XDC A.97.2: snap-heal gate-B re-execution + status helpers.
//
// ReexecuteAndValidateRange runs the most recent N blocks read-only (no
// InsertChain / SetHead) to prove that the healed trie produces correct
// state roots before checkpointSyncNoState is cleared.
//
// SnapHealStatusFields returns a snapshot of the in-process heal status
// for the xdc_getStateHealStatus RPC.
//
// Refs #894.

package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
)

// ReexecuteAndValidateRange re-executes the most recent nBlocks canonical
// blocks in read-only mode (no InsertChain, no SetHead). For each block it:
//
//  1. Opens a state.StateDB on the parent block's root (state.New).
//  2. Calls bc.processor.Process (full TX execution, no skipping).
//  3. Calls bc.validator.ValidateState and compares computed root vs
//     block.Root.
//
// XdcBulkSyncMode MUST be false before this call so that A.94 cannot fire
// and any root mismatch surfaces as a real error. The caller (maybeStartSnapHeal)
// guarantees this ordering.
//
// Returns nil iff all nBlocks blocks produce the canonical root. Returns the
// first error encountered (with block number context).
func (bc *BlockChain) ReexecuteAndValidateRange(nBlocks uint64) error {
	cur := bc.CurrentBlock()
	if cur == nil {
		return errors.New("ReexecuteAndValidateRange: nil current block")
	}
	head := cur.Number.Uint64()

	// Block 0 is genesis — its state is set by genesis allocation, not TX
	// execution. Re-executing it always produces EmptyRootHash (no TXs),
	// which mismatches the genesis alloc root. Start from block 1.
	// Clamp: if chain is shorter than nBlocks just validate what's available.
	from := uint64(1)
	if head > nBlocks {
		from = head - nBlocks + 1
	}
	if from < 1 {
		from = 1
	}
	if from > head {
		// Chain has only genesis; nothing to validate.
		return nil
	}

	log.Debug("A.97.2: gate-B starting",
		"from", from,
		"to", head,
		"count", head-from+1)

	for num := from; num <= head; num++ {
		blk := bc.GetBlockByNumber(num)
		if blk == nil {
			return fmt.Errorf("ReexecuteAndValidateRange: block %d not found", num)
		}
		parent := bc.GetBlockByNumber(num - 1)
		if parent == nil {
			return fmt.Errorf("ReexecuteAndValidateRange: parent of block %d not found", num)
		}

		statedb, err := bc.StateAt(parent.Header())
		if err != nil {
			return fmt.Errorf("ReexecuteAndValidateRange: StateAt block %d parent: %w", num, err)
		}

		res, err := bc.processor.Process(context.Background(), blk, statedb, bc.jumpDestCache, vm.Config{}, nil)
		if err != nil {
			return fmt.Errorf("ReexecuteAndValidateRange: Process block %d: %w", num, err)
		}

		if err := bc.validator.ValidateState(blk, statedb, res, false); err != nil {
			return fmt.Errorf("ReexecuteAndValidateRange: ValidateState block %d: %w", num, err)
		}
	}

	log.Debug("A.97.2: gate-B passed",
		"from", from,
		"to", head,
		"blocks", head-from+1)
	return nil
}

// SnapHealStatusFields is a plain data carrier returned by xdc_getStateHealStatus.
// Refs #894.
type SnapHealStatusFields struct {
	Enabled    bool `json:"enabled"`    // checkpointSyncNoState (false = healed/not needed)
	InProgress bool `json:"inProgress"` // heal goroutine running
	// Mode is set by the caller (eth/api_backend), not by this package.
	Mode            string `json:"mode"`            // always "snap-heal"
	TargetRoot      string `json:"targetRoot"`      // hex root
	TrienodePending uint64 `json:"trienodePending"` // informational
	BytecodePending uint64 `json:"bytecodePending"` // informational
	HealStartUnix   int64  `json:"healStartUnix"`   // 0 if not started
	LastError       string `json:"lastError"`       // "" on success
}
