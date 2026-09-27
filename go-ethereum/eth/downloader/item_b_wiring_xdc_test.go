package downloader

import (
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
)

type frozenPivotStubSyncer struct {
	snap.Syncer
	hdr *types.Header
}

func (s frozenPivotStubSyncer) FrozenPivot() *types.Header { return s.hdr }

// TestSnapPivotFrozenReadsSelectedSyncer pins the Item B wiring: the beacon
// pivot mover's snap-freeze gate must consult d.snapSyncer (the syncer the
// downloader drives, snap/2-capable) and NOT the exported d.SnapSyncer
// accessor, whose FrozenPivot() is hardwired nil. Refs go-ethereum#1371.
func TestSnapPivotFrozenReadsSelectedSyncer(t *testing.T) {
	tester := newTester(t, ethconfig.SnapSync)
	defer tester.terminate()
	d := tester.downloader

	if d.snapPivotFrozen() {
		t.Fatal("fresh downloader reports a frozen snap pivot")
	}
	d.snapSyncer = frozenPivotStubSyncer{Syncer: d.snapSyncer, hdr: &types.Header{}}
	if !d.snapPivotFrozen() {
		t.Fatal("gate does not observe the selected syncer's frozen pivot (reads d.SnapSyncer instead of d.snapSyncer?)")
	}
}
