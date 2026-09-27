// Copyright 2024 XDC Network
// XDC sync speed optimization knobs

package core

import "sync/atomic"

// XdcBulkSyncMode is set to true during initial block sync (when chain head is
// far behind peers). When true, signing-tx processing is skipped (ECDSA
// recovery, logs, bloom) for faster sync. Set by the downloader when syncing,
// cleared on reaching chain tip.
//
// Previously co-located with the bypass-era state-root cache; relocated here
// in #682 Tier 2 so the cache file could be deleted outright. The flag itself
// is unrelated to that cache and is still load-bearing for bulk-sync code
// paths in core/blockchain.go and consensus/XDPoS/xdpos.go.
var XdcBulkSyncMode atomic.Bool

// CheckpointSyncNoStateActive mirrors the live BlockChain.checkpointSyncNoState
// flag as a process-global so the consensus engine can read it without a
// ChainReader interface change (same access pattern as XdcBulkSyncMode above).
//
// It is load-bearing for consensus verification: a node that fast-synced /
// snapshot-restored runs with checkpointSyncNoState=true (execution skipped,
// state trie incomplete). In that state it CANNOT recompute the canonical V2
// masternode/penalty set, so XDPoS_v2.verifyHeader must NOT run in fullVerify
// mode — otherwise it hard-fails legitimately-produced epoch-switch blocks with
// ErrPenaltiesNotLegit and wedges below tip (observed on a snapshot-restored
// mainnet fast node at block 103,852,092). fullVerify is therefore gated on BOTH
// !XdcBulkSyncMode AND !CheckpointSyncNoStateActive; strict validation resumes
// only once the state heal (XDC_SNAP_HEAL) clears the flag. Fully-synced nodes
// (flag always false) are unaffected — no consensus/block-hash behaviour change.
//
// Kept in sync with the per-instance flag via BlockChain.storeCheckpointSyncNoState
// (core/blockchain.go); there is one BlockChain per process so a global is safe.
var CheckpointSyncNoStateActive atomic.Bool

// XdcForkRecovering is set true by the xdcSyncer while it is escaping a fork
// (repeated ErrUnknownAncestor → SetHead rollback) and cleared on the next
// successful InsertChain. The XDPoS V1/V2 block-production worker checks it as
// a mining gate: a producer must NOT seal while the node is rolling back off a
// fork, otherwise it re-mints on the rolled-back tip and recreates the same
// fork, deadlocking the rollback loop (issue #951, devnet-5151 xdc04 stall at
// block 602). Clearing on successful import lets the miner resume once the node
// is back on the canonical chain.
var XdcForkRecovering atomic.Bool

// XdcLastForkRollbackUnix is the wall-clock (unix seconds) of the most recent
// xdcSyncer SetHead fork-rollback. The miner uses it as a COOLDOWN gate: clearing
// XdcForkRecovering on the first successful import after a rollback is not enough
// to break the devnet-5151 oscillation, because the node is still far behind the
// network tip — it re-mints its (already-superseded) in-turn block the instant it
// re-reaches the contested height, before the syncer imports the network's
// version, recreating the fork (issue #951 oscillation: head bounces 143↔784,
// never past 785). While the node is actively catching up, rollbacks recur and
// keep refreshing this timestamp, so the cooldown holds minting off; once the node
// is genuinely caught up to a stable tip, rollbacks stop, the cooldown elapses, and
// minting resumes. A genuinely idle/paused network produces no rollbacks, so the
// cooldown never engages there (out-of-turn recovery still works). 0 = never.
var XdcLastForkRollbackUnix atomic.Int64

// Tunable constants for XDC sync optimisation.
const (
	// XdcBulkTriesInMemory is the effective TriesInMemory during bulk sync.
	// Keeping more tries in RAM avoids expensive trie GC flushes while we are
	// still catching up with the network. Increased from 1024 to 4096 to
	// reduce trie commit frequency by 4x during fast-sync.
	XdcBulkTriesInMemory = 4096

	// XdcChainHeadEventInterval controls how often the chainHeadEvent is fired
	// during bulk sync (every N canonical blocks). Firing it for every block is
	// wasteful because nobody is watching during initial sync.
	XdcChainHeadEventInterval = 512 // v91: 512 (was 256) — even less overhead during sync

	// XdcCheckpointInterval is the epoch length for XDC chains.
	// Every 900th block is a checkpoint where full execution is required.
	XdcCheckpointInterval = 900

	// XdcMaxPeersDuringSSync is the effective MaxPeers during bulk sync.
	// More peers = more parallel body delivery = higher throughput.
	XdcMaxPeersDuringSync = 200

	// XdcBodyMinBatch is the minimum body batch size per peer request.
	// Prevents tiny batches that waste round-trips.
	XdcBodyMinBatch = 256 // v91: larger minimum batch — XDC blocks are tiny, reduces round-trips
)
