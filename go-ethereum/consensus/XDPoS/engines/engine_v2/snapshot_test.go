package engine_v2

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func TestGetMasterNodes(t *testing.T) {
	masterNodes := []common.Address{{0x4}, {0x3}, {0x2}, {0x1}}
	snap := newSnapshot(1, common.Hash{}, masterNodes)

	for _, address := range masterNodes {
		if _, ok := snap.GetMappedCandidates()[address]; !ok {
			t.Error("should get master node from map", address.Hex(), snap.GetMappedCandidates())
			return
		}
	}
}

func TestStoreLoadSnapshot(t *testing.T) {
	snap := newSnapshot(1, common.Hash{0x1}, nil)
	dir := t.TempDir()
	db, err := leveldb.New(dir, 256, 0, "", false)
	if err != nil {
		panic(fmt.Sprintf("can't create temporary database: %v", err))
	}
	lddb := rawdb.NewDatabase(db)

	err = storeSnapshot(snap, lddb)
	if err != nil {
		t.Error("store snapshot failed", err)
	}

	restoredSnapshot, err := loadSnapshot(lddb, snap.Hash)
	if err != nil || restoredSnapshot.Hash != snap.Hash {
		t.Error("load snapshot failed", err)
	}
}

// --- P0 fix #894: ActiveLen() accessor tests ---

// makeAddrs creates n distinct addresses starting at offset.
func makeAddrs(n, offset int) []common.Address {
	addrs := make([]common.Address, n)
	for i := range addrs {
		addrs[i] = common.Address{byte(offset + i + 1)}
	}
	return addrs
}

// TestActiveLen_FieldSet: ActiveMasternodesLen field is authoritative.
func TestActiveLen_FieldSet(t *testing.T) {
	pool := makeAddrs(46, 0)
	penalties := makeAddrs(14, 100)
	snap := newSnapshotWithPenalties(1, common.Hash{0x1}, pool, penalties)
	// newSnapshotWithPenalties computes active = pool - penalties
	if snap.ActiveMasternodesLen != 46 {
		t.Fatalf("ActiveMasternodesLen want 46 got %d", snap.ActiveMasternodesLen)
	}
	if snap.ActiveLen() != 46 {
		t.Fatalf("ActiveLen() want 46 got %d", snap.ActiveLen())
	}
	// Override field directly to test field-takes-priority branch
	snap.ActiveMasternodesLen = 32
	if snap.ActiveLen() != 32 {
		t.Fatalf("ActiveLen() with field=32 want 32 got %d", snap.ActiveLen())
	}
	if snap.activeLenSource() != "field" {
		t.Fatalf("activeLenSource want 'field' got %q", snap.activeLenSource())
	}
}

// TestActiveLen_DerivedFromPenalties: field=0, penalties present → derive.
func TestActiveLen_DerivedFromPenalties(t *testing.T) {
	pool := makeAddrs(46, 0)
	penalties := pool[:14] // first 14 are penalised
	snap := &SnapshotV2{
		Version:             4, // old snapshot without ActiveMasternodesLen
		Number:              1,
		Hash:                common.Hash{0x2},
		NextEpochCandidates: pool,
		Penalties:           penalties,
		ActiveMasternodesLen: 0, // not set (legacy)
	}
	got := snap.ActiveLen()
	if got != 32 {
		t.Fatalf("derived ActiveLen want 32 got %d", got)
	}
	if snap.activeLenSource() != "derived" {
		t.Fatalf("activeLenSource want 'derived' got %q", snap.activeLenSource())
	}
}

// TestActiveLen_LegacyFallback: no field, no penalties → return full pool len.
func TestActiveLen_LegacyFallback(t *testing.T) {
	pool := makeAddrs(46, 0)
	snap := newSnapshot(1, common.Hash{0x3}, pool)
	snap.ActiveMasternodesLen = 0
	if snap.ActiveLen() != 46 {
		t.Fatalf("legacy ActiveLen want 46 got %d", snap.ActiveLen())
	}
	if snap.activeLenSource() != "fallback" {
		t.Fatalf("activeLenSource want 'fallback' got %q", snap.activeLenSource())
	}
}

// TestSnapshotJSONRoundTrip_ActiveMasternodesLen: field survives marshal/unmarshal.
func TestSnapshotJSONRoundTrip_ActiveMasternodesLen(t *testing.T) {
	pool := makeAddrs(46, 0)
	penalties := pool[:14]
	snap := newSnapshotWithPenalties(900, common.Hash{0xAB}, pool, penalties)
	snap.ActiveMasternodesLen = 32

	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var snap2 SnapshotV2
	if err := json.Unmarshal(blob, &snap2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap2.ActiveMasternodesLen != 32 {
		t.Fatalf("round-trip: want ActiveMasternodesLen=32 got %d", snap2.ActiveMasternodesLen)
	}
}

// TestSnapshotJSONRoundTrip_LegacyBlob: blob without activeMasternodesLen → 0 → fallback.
func TestSnapshotJSONRoundTrip_LegacyBlob(t *testing.T) {
	blob := []byte(`{"version":4,"number":1,"hash":"0x0101010101010101010101010101010101010101010101010101010101010101","masterNodes":["0x0100000000000000000000000000000000000000"]}`)
	var snap SnapshotV2
	if err := json.Unmarshal(blob, &snap); err != nil {
		t.Fatalf("unmarshal legacy blob: %v", err)
	}
	if snap.ActiveMasternodesLen != 0 {
		t.Fatalf("legacy blob should have ActiveMasternodesLen=0 got %d", snap.ActiveMasternodesLen)
	}
	// ActiveLen fallback: returns len(NextEpochCandidates)
	if snap.ActiveLen() != 1 {
		t.Fatalf("legacy fallback ActiveLen want 1 got %d", snap.ActiveLen())
	}
}

// TestActiveSet_WithPenalties: activeSet returns pool minus penalties.
func TestActiveSet_WithPenalties(t *testing.T) {
	pool := makeAddrs(10, 0)
	penalties := pool[:3]
	snap := newSnapshotWithPenalties(1, common.Hash{0x4}, pool, penalties)
	active := snap.activeSet()
	if len(active) != 7 {
		t.Fatalf("activeSet want 7 got %d", len(active))
	}
	// None of the penalties should be in active
	penSet := make(map[common.Address]struct{})
	for _, p := range penalties {
		penSet[p] = struct{}{}
	}
	for _, a := range active {
		if _, ok := penSet[a]; ok {
			t.Fatalf("penalty addr %v in active set", a)
		}
	}
}

// TestActiveSet_NoPenalties: activeSet returns full pool.
func TestActiveSet_NoPenalties(t *testing.T) {
	pool := makeAddrs(46, 0)
	snap := newSnapshot(1, common.Hash{0x5}, pool)
	active := snap.activeSet()
	if len(active) != 46 {
		t.Fatalf("activeSet (no penalties) want 46 got %d", len(active))
	}
}
