// Copyright 2026 XDC Network
//
// Tests for the checkpointSyncNoState safety rails (#62, netv12 mint wedge):
//   - ClearCheckpointSyncNoState refuses to clear when the head state is absent,
//     because such a DB was imported without execution and must be resynced.
//   - canMaskMissingState never lets a node configured to produce blocks enter
//     no-state mode.

package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TestClearCheckpointSyncNoStateRefusesWithoutHeadState pins the FIX 3 guard.
//
// The netv12 node had checkpointSyncNoState latched and persisted while its
// newest available state was block 1799 and its head was past 9200. Clearing the
// flag there repairs nothing — it only converts a silent skip into a hard
// "missing trie node" on every block. So the clear must refuse and say so.
//
// Conversely, when the head state IS present the flag must clear and the DB key
// must be deleted, otherwise the flag is write-only (its original defect: the
// function had zero call sites in the whole tree, so the flag could never be
// cleared at all).
func TestClearCheckpointSyncNoStateRefusesWithoutHeadState(t *testing.T) {
	_, _, chain, err := newCanonical(ethash.NewFaker(), 4, true, rawdb.PathScheme)
	if err != nil {
		t.Fatalf("failed to build test chain: %v", err)
	}
	defer chain.Stop()

	realHead := chain.CurrentBlock()
	if realHead == nil {
		t.Fatal("test chain has no head")
	}
	if !chain.HasState(realHead.Root) {
		t.Fatalf("precondition failed: head state %v should be present on a full-synced test chain", realHead.Root)
	}

	// --- Case 1: head state ABSENT -> must refuse, flag stays set. ---
	chain.storeCheckpointSyncNoState(true)
	rawdb.WriteCheckpointSyncNoState(chain.db, true)

	// Point the head at a block whose state root is not in the trie DB. This is
	// exactly the netv12 shape: a real head header whose state was never computed.
	poisonedHead := types.CopyHeader(realHead)
	poisonedHead.Root = common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	poisonedHead.Number = big.NewInt(9241) // netv12's head at the time of diagnosis
	if chain.HasState(poisonedHead.Root) {
		t.Fatal("precondition failed: the poisoned root should not be present")
	}
	chain.currentBlock.Store(poisonedHead)

	chain.ClearCheckpointSyncNoState()

	if !chain.checkpointSyncNoState.Load() {
		t.Fatal("ClearCheckpointSyncNoState cleared the in-memory flag despite an absent head state; " +
			"that turns a silent skip into a hard missing-trie-node failure on every block")
	}
	if !rawdb.ReadCheckpointSyncNoState(chain.db) {
		t.Fatal("ClearCheckpointSyncNoState deleted the persisted flag despite an absent head state")
	}
	if !CheckpointSyncNoStateActive.Load() {
		t.Fatal("the global CheckpointSyncNoStateActive mirror was cleared despite an absent head state")
	}

	// --- Case 2: head state PRESENT -> must clear, and delete the DB key. ---
	chain.currentBlock.Store(realHead)

	chain.ClearCheckpointSyncNoState()

	if chain.checkpointSyncNoState.Load() {
		t.Fatal("ClearCheckpointSyncNoState did not clear the in-memory flag with the head state present")
	}
	if rawdb.ReadCheckpointSyncNoState(chain.db) {
		t.Fatal("ClearCheckpointSyncNoState did not delete the persisted flag with the head state present")
	}
	if CheckpointSyncNoStateActive.Load() {
		t.Fatal("the global CheckpointSyncNoStateActive mirror was not cleared")
	}

	// --- Case 3: idempotent. Safe to call from heartbeat/at-tip paths that fire
	// repeatedly; must not thrash the DB key or flap state validation. ---
	chain.ClearCheckpointSyncNoState()
	if chain.checkpointSyncNoState.Load() || rawdb.ReadCheckpointSyncNoState(chain.db) {
		t.Fatal("a second ClearCheckpointSyncNoState call was not a no-op")
	}
}

// TestMinerNodeNeverAutoEntersNoStateMode pins the FIX 4 exclusion.
//
// canMaskMissingState is the single predicate behind all three A.93/A.94
// auto-enable sites in blockchain.go. Masking a missing parent state imports
// every later block with EmptyRootHash, which permanently poisons the DB and
// then gates the miner off entirely (miner/xdpos_worker.go Gate 3.1) — a seated
// masternode that mints nothing and times out its slot every round, forever.
//
// The pre-existing exclusion only covered --gcmode archive, so the netv12
// `--gcmode full` masternode slipped straight through.
func TestMinerNodeNeverAutoEntersNoStateMode(t *testing.T) {
	xdcConfig := &params.ChainConfig{
		ChainID: big.NewInt(34093),
		XDPoS:   &params.XDPoSConfig{},
	}
	nonXDCConfig := &params.ChainConfig{ChainID: big.NewInt(1)}

	for _, tt := range []struct {
		name        string
		chainConfig *params.ChainConfig
		bulkSync    bool
		archive     bool
		producer    bool
		want        bool
	}{
		{
			// The only case masking is meant for: a plain follower mid-bulk-sync.
			name: "plain-follower-bulk-syncing", chainConfig: xdcConfig,
			bulkSync: true, archive: false, producer: false, want: true,
		},
		{
			// THE netv12 REGRESSION. --gcmode full (not archive) + --mine. Before
			// FIX 4 this returned true and latched the flag at block 1800.
			name: "gcmode-full-miner", chainConfig: xdcConfig,
			bulkSync: true, archive: false, producer: true, want: false,
		},
		{
			// Pre-existing archive exclusion must be preserved.
			name: "archive-follower", chainConfig: xdcConfig,
			bulkSync: true, archive: true, producer: false, want: false,
		},
		{
			name: "archive-miner", chainConfig: xdcConfig,
			bulkSync: true, archive: true, producer: true, want: false,
		},
		{
			// Not bulk-syncing: a state miss is always a real defect.
			name: "follower-not-bulk-syncing", chainConfig: xdcConfig,
			bulkSync: false, archive: false, producer: false, want: false,
		},
		{
			// Non-XDC chains never use this XDC-specific masking path.
			name: "non-xdc-chain", chainConfig: nonXDCConfig,
			bulkSync: true, archive: false, producer: false, want: false,
		},
		{
			name: "nil-chain-config", chainConfig: nil,
			bulkSync: true, archive: false, producer: false, want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prev := XdcBulkSyncMode.Load()
			XdcBulkSyncMode.Store(tt.bulkSync)
			defer XdcBulkSyncMode.Store(prev)

			bc := &BlockChain{
				chainConfig: tt.chainConfig,
				cfg: &BlockChainConfig{
					ArchiveMode:             tt.archive,
					NoStateMaskingForbidden: tt.producer,
				},
			}
			if got := bc.canMaskMissingState(); got != tt.want {
				t.Fatalf("canMaskMissingState() = %v, want %v "+
					"(bulkSync=%v archive=%v producer=%v)",
					got, tt.want, tt.bulkSync, tt.archive, tt.producer)
			}
		})
	}
}

// TestNoStateMaskingPredicateOnRealChain checks the predicate on a BlockChain
// built by NewBlockChain, i.e. with BlockChainConfig.NoStateMaskingForbidden
// actually plumbed through construction, rather than on a hand-built struct.
//
// Scope, stated honestly: this does NOT drive the makeEnv / ValidateState call
// paths themselves — reaching them requires an out-of-order import whose parent
// state is genuinely absent, which is not constructible against a well-formed
// test chain here. What makes the predicate test sufficient is that all three
// auto-enable sites in blockchain.go now call canMaskMissingState and nothing
// else: there are zero remaining inline
// `XdcBulkSyncMode.Load() && IsXDC() && !ArchiveMode` conditions in the file.
// The end-to-end behaviour is covered by devnet gate F1/F2 instead.
func TestNoStateMaskingPredicateOnRealChain(t *testing.T) {
	for _, producer := range []bool{false, true} {
		name := "follower-masks"
		if producer {
			name = "producer-refuses-to-mask"
		}
		t.Run(name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			genesis := &Genesis{
				BaseFee: big.NewInt(params.InitialBaseFee),
				Config:  params.AllEthashProtocolChanges,
			}
			opts := DefaultConfig().WithStateScheme(rawdb.PathScheme)
			opts.ArchiveMode = false
			opts.NoStateMaskingForbidden = producer

			chain, err := NewBlockChain(db, genesis, ethash.NewFaker(), opts)
			if err != nil {
				t.Fatalf("failed to create chain: %v", err)
			}
			defer chain.Stop()

			// Force the XDC masking preconditions: XDC chain + bulk-sync mode.
			chain.chainConfig = &params.ChainConfig{
				ChainID: big.NewInt(34093),
				XDPoS:   &params.XDPoSConfig{},
			}
			prev := XdcBulkSyncMode.Load()
			XdcBulkSyncMode.Store(true)
			defer XdcBulkSyncMode.Store(prev)

			chain.storeCheckpointSyncNoState(false)
			defer chain.storeCheckpointSyncNoState(false)

			// A state root that is definitely not in the trie DB — the "parent state
			// missing" condition the A.93 path exists to mask.
			missingRoot := common.HexToHash("0xfeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface")
			if chain.HasState(missingRoot) {
				t.Fatal("precondition failed: root should be missing")
			}

			// makeEnv's masking decision is canMaskMissingState; assert it directly
			// against the live chain object (real cfg, real chainConfig), then assert
			// the flag really does stay clear for a producer.
			mayMask := chain.canMaskMissingState()
			if mayMask != !producer {
				t.Fatalf("canMaskMissingState() = %v on a %s; want %v", mayMask, name, !producer)
			}
			if producer && chain.checkpointSyncNoState.Load() {
				t.Fatal("a producer latched checkpointSyncNoState")
			}
		})
	}
}
