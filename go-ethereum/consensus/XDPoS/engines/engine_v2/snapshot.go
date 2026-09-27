// Copyright (c) 2024 XDC Network
// Snapshot management for XDPoS 2.0

package engine_v2

import (
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

var ErrNotFoundBlockByNum = errors.New("not found block by number")

// SnapshotV2 represents the state of masternodes at a given block.
// Aligned with v2.6.8: JSON tag must be "masterNodes" for DB compatibility.
//
// Field semantics (P0 fix #894 / synthetic-epoch QC threshold):
//   - NextEpochCandidates: the FULL candidate pool (e.g. 46) read from the
//     validator smart contract at the gap block. Kept full so that HookPenalty's
//     comeback algorithm (which intersects prev-epoch penalties against this
//     pool) and proposer-rotation logic operate on the complete set.
//   - Penalties: penalty addresses from the same epoch, stored SEPARATELY so
//     activeSet() and ActiveLen() can derive the real active count (e.g. 32)
//     without mutating the pool. Populated by UpdateMasternodesFromHeader
//     (post-penalty compute) and checkpoint-sync seed paths.
//   - ActiveMasternodesLen: count of active masternodes after penalty filtering
//     (len(pool) - len(penalties), capped at MaxMasternodes). Used by the
//     synthetic-epoch-info path in getEpochSwitchInfo to set MasternodesLen
//     (the QC/TC threshold) correctly when the parent block is absent during
//     bulk sync. Zero on legacy snapshots — ActiveLen() falls back gracefully.
//     omitempty for DB compatibility.
type SnapshotV2 struct {
	Version              uint64           `json:"version,omitempty"` // 1=legacy, 4=current, 5=includes ActiveMasternodesLen, 6=rebuilt from contract state (live-mn-set fix)
	Number               uint64           `json:"number"`
	Hash                 common.Hash      `json:"hash"`
	NextEpochCandidates  []common.Address `json:"masterNodes"`
	Penalties            []common.Address `json:"penalties,omitempty"`
	ActiveMasternodesLen int              `json:"activeMasternodesLen,omitempty"`
}

// newSnapshot creates a new snapshot with the current version.
// masternodes is the FULL candidate pool (e.g. 46); use newSnapshotWithPenalties
// to also record the penalty set and active count.
func newSnapshot(number uint64, hash common.Hash, masternodes []common.Address) *SnapshotV2 {
	snap := &SnapshotV2{
		Version:             5, // v5: includes ActiveMasternodesLen (P0 fix #894); evicts stale v4 gap-height snapshots
		Number:              number,
		Hash:                hash,
		NextEpochCandidates: make([]common.Address, len(masternodes)),
	}
	copy(snap.NextEpochCandidates, masternodes)
	return snap
}

// ActiveLen returns the number of active masternodes (post-penalty) for this
// snapshot. Priority:
//  1. ActiveMasternodesLen field (set by UpdateMasternodesFromHeader v5+)
//  2. Derive: len(pool) - len(penalties) when Penalties is populated
//  3. Legacy fallback: len(NextEpochCandidates) (superset — may over-count)
func (s *SnapshotV2) ActiveLen() int {
	if s.ActiveMasternodesLen > 0 {
		return s.ActiveMasternodesLen
	}
	if len(s.Penalties) > 0 {
		// Derive from pool minus recorded penalties
		active := removeItemFromArray(s.NextEpochCandidates, s.Penalties)
		return len(active)
	}
	// Legacy: no penalty info — return full pool length (conservative; may be wrong)
	return len(s.NextEpochCandidates)
}

// activeLenSource returns a diagnostic string for logging — which branch of
// ActiveLen() was used.
func (s *SnapshotV2) activeLenSource() string {
	if s.ActiveMasternodesLen > 0 {
		return "field"
	}
	if len(s.Penalties) > 0 {
		return "derived"
	}
	return "fallback"
}

// activeSet returns the active masternode addresses (post-penalty) for use by
// the synthetic-epoch-info path. When penalties are recorded it returns the
// full pool minus penalties; otherwise it returns the full pool (legacy).
func (s *SnapshotV2) activeSet() []common.Address {
	if len(s.Penalties) > 0 {
		return removeItemFromArray(s.NextEpochCandidates, s.Penalties)
	}
	return s.NextEpochCandidates
}

// newSnapshotWithPenalties creates a snapshot that records the FULL candidate
// pool (masternodes arg, e.g. 46), the associated penalty set (penalties arg,
// e.g. 14), and the derived active count (e.g. 32) in ActiveMasternodesLen.
//
// INVARIANT: NextEpochCandidates = full pool (46) so HookPenalty's comeback
// algorithm and proposer-rotation operate on the complete candidate set.
// Penalties = the 14 penalised addresses. ActiveMasternodesLen = 32 (used by
// the synthetic-epoch-info path to set the correct QC/TC threshold).
func newSnapshotWithPenalties(number uint64, hash common.Hash, masternodes, penalties []common.Address) *SnapshotV2 {
	snap := newSnapshot(number, hash, masternodes)
	if len(penalties) > 0 {
		snap.Penalties = make([]common.Address, len(penalties))
		copy(snap.Penalties, penalties)
		// Compute and store active count so the synthetic-epoch path doesn't
		// need to re-derive it (P0 fix #894).
		active := removeItemFromArray(masternodes, penalties)
		snap.ActiveMasternodesLen = len(active)
	}
	return snap
}

// CandidatePool returns the full candidate pool for HookPenalty:
// active masternodes ∪ recorded penalties (deduped). For v2.6.8-produced
// snapshots (Penalties empty) this is just NextEpochCandidates, preserving
// the canonical algorithm.
func (s *SnapshotV2) CandidatePool() []common.Address {
	if len(s.Penalties) == 0 {
		return s.NextEpochCandidates
	}
	pool := make([]common.Address, len(s.NextEpochCandidates), len(s.NextEpochCandidates)+len(s.Penalties))
	copy(pool, s.NextEpochCandidates)
	seen := make(map[common.Address]struct{}, len(pool))
	for _, a := range pool {
		seen[a] = struct{}{}
	}
	for _, p := range s.Penalties {
		if _, ok := seen[p]; !ok {
			pool = append(pool, p)
			seen[p] = struct{}{}
		}
	}
	return pool
}

// loadSnapshot loads a snapshot from the database
// Aligned with v2.6.8: DB key prefix is "XDPoS-V2-" + hash[:]
func loadSnapshot(db ethdb.Database, hash common.Hash) (*SnapshotV2, error) {
	key := append([]byte("XDPoS-V2-"), hash[:]...)
	blob, err := db.Get(key)
	if err != nil {
		return nil, err
	}
	snap := new(SnapshotV2)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// storeSnapshot stores a snapshot to the database
// Aligned with v2.6.8: DB key prefix is "XDPoS-V2-" + hash[:]
func storeSnapshot(snap *SnapshotV2, db ethdb.Database) error {
	blob, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	key := append([]byte("XDPoS-V2-"), snap.Hash[:]...)
	if err := db.Put(key, blob); err != nil {
		log.Error("Failed to store snapshot", "hash", snap.Hash, "error", err)
		return err
	}
	return nil
}

// Copy creates a copy of the snapshot
func (s *SnapshotV2) Copy() *SnapshotV2 {
	return newSnapshot(s.Number, s.Hash, s.NextEpochCandidates)
}

// GetSigners returns the list of masternodes
func (s *SnapshotV2) GetSigners() []common.Address {
	return s.NextEpochCandidates
}

// GetMappedCandidates returns the candidate list as a map for O(1) lookups.
// Matches v2.6.8 engines/engine_v2/snapshot.go.
func (s *SnapshotV2) GetMappedCandidates() map[common.Address]struct{} {
	ms := make(map[common.Address]struct{})
	for _, n := range s.NextEpochCandidates {
		ms[n] = struct{}{}
	}
	return ms
}

// IsCandidates checks if an address is in the candidate list.
// Matches v2.6.8 engines/engine_v2/snapshot.go.
func (s *SnapshotV2) IsCandidates(address common.Address) bool {
	for _, n := range s.NextEpochCandidates {
		if n == address {
			return true
		}
	}
	return false
}

// IsValidForV2Switch returns whether this snapshot is valid for use at the V2 switch block.
// Legacy snapshots (version < 2) may have incorrect masternode data at the V1->V2 boundary.
func (s *SnapshotV2) IsValidForV2Switch() bool {
	return s.Version >= 2
}
