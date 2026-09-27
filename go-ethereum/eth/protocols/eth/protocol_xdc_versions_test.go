// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// This file is the required deliverable of the go-ethereum#1175 upstream
// v1.17.5 merge policy's Gate 3: a construction-based proof that the
// ProtocolVersions/XDPOS2 devp2p cap set and message dispatch survived the
// merge unperturbed. See the merge resolution policy for
// eth/protocols/eth/protocol.go and eth/protocols/eth/handler.go.
//
// Every assertion here is by construction against the actual package-level
// vars (ProtocolVersions, protocolLengths, the msg code consts, and the
// xdpos2/eth69/eth70/eth71/eth72 handler maps) — nothing is assumed.

package eth

import (
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p"
)

// TestXDCProtocolVersionsConstruction asserts requirement 1 and 2 of Gate 3:
// XDPOS2 and ETH63 are present (XDPOS2 primary/first), and ETH71/ETH72 are
// deliberately absent from the advertised devp2p capability list even though
// both are fully implemented (see protocol.go's ProtocolVersions comment).
func TestXDCProtocolVersionsConstruction(t *testing.T) {
	if len(ProtocolVersions) == 0 {
		t.Fatal("ProtocolVersions is empty")
	}
	if ProtocolVersions[0] != XDPOS2 {
		t.Fatalf("XDPOS2 must be the primary (first) advertised version, got ProtocolVersions[0]=%d", ProtocolVersions[0])
	}
	var haveXDPOS2, haveETH63, haveETH71, haveETH72 bool
	for _, v := range ProtocolVersions {
		switch v {
		case XDPOS2:
			haveXDPOS2 = true
		case ETH63:
			haveETH63 = true
		case ETH71:
			haveETH71 = true
		case ETH72:
			haveETH72 = true
		}
	}
	if !haveXDPOS2 {
		t.Error("ProtocolVersions must contain XDPOS2 (100)")
	}
	if !haveETH63 {
		t.Error("ProtocolVersions must contain ETH63 (63) — refs #857 A.59, required for XDC mainnet mesh acceptance")
	}
	if haveETH71 {
		t.Error("ProtocolVersions must NOT contain ETH71 — it is implemented but deliberately not advertised on XDC")
	}
	if haveETH72 {
		t.Error("ProtocolVersions must NOT contain ETH72 — it is implemented (sparse blobpool) but deliberately not advertised on XDC (no blob txs)")
	}
}

// TestXDCProtocolLengthsConstruction asserts requirement 3: protocolLengths
// has an entry for every advertised version, with the two XDC-critical
// lengths pinned to their exact values.
func TestXDCProtocolLengthsConstruction(t *testing.T) {
	for _, v := range ProtocolVersions {
		if _, ok := protocolLengths[v]; !ok {
			t.Errorf("protocolLengths missing entry for advertised version %d", v)
		}
	}
	if got := protocolLengths[ETH63]; got != 17 {
		t.Errorf("protocolLengths[ETH63] = %d, want 17", got)
	}
	if got := protocolLengths[XDPOS2]; got != 229 {
		t.Errorf("protocolLengths[XDPOS2] = %d, want 229", got)
	}
}

// TestXDCMsgCodesConstruction asserts requirement 4: the XDC consensus and
// legacy NodeData message codes have their fixed wire values, and do not
// collide with upstream's new eth-72 Cells codes.
func TestXDCMsgCodesConstruction(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"VoteMsg", VoteMsg, 0xe0},
		{"TimeoutMsg", TimeoutMsg, 0xe1},
		{"SyncInfoMsg", SyncInfoMsg, 0xe2},
		{"GetNodeDataMsg", GetNodeDataMsg, 0x0d},
		{"NodeDataMsg", NodeDataMsg, 0x0e},
		{"GetCellsMsg", GetCellsMsg, 0x14},
		{"CellsMsg", CellsMsg, 0x15},
	}
	seen := map[uint64]string{}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
		if prev, ok := seen[c.got]; ok {
			t.Errorf("msg code collision: %s and %s both use %#x", c.name, prev, c.got)
		}
		seen[c.got] = c.name
	}
}

// funcName returns a stable identity string for a msgHandler function value,
// used to compare "is this the same function" without relying on Go's
// unspecified func-value equality.
func funcName(f msgHandler) string {
	if f == nil {
		return "<nil>"
	}
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// TestXDPOS2HandlerRegistration asserts requirement 5: every XDC consensus
// and legacy-shape message code is registered in the xdpos2 handler map, and
// the msg codes that are semantically overloaded between modern eth/68+ and
// legacy XDPoSChain (0x08, 0x09, GetReceipts/Receipts) route to the XDC-aware
// handlers, not the modern eth/69+ ones.
func TestXDPOS2HandlerRegistration(t *testing.T) {
	mustHave := []uint64{
		VoteMsg, TimeoutMsg, SyncInfoMsg,
		GetNodeDataMsg, NodeDataMsg,
		NewBlockHashesMsg, NewBlockMsg,
	}
	for _, code := range mustHave {
		if xdpos2[code] == nil {
			t.Errorf("xdpos2[%#x] is nil — expected a registered handler", code)
		}
	}

	// A.60.3: 0x08/0x09 are OrderTxMsg/LendingTxMsg on legacy XDPoSChain, not
	// NewPooledTransactionHashes/GetPooledTransactions. They must route to the
	// no-op drain, not to the modern pooled-tx handlers.
	if got, want := funcName(xdpos2[NewPooledTransactionHashesMsg]), funcName(handleXDCTradingTxNoop); got != want {
		t.Errorf("xdpos2[NewPooledTransactionHashesMsg] = %s, want the A.60.3 no-op %s", got, want)
	}
	if got, want := funcName(xdpos2[GetPooledTransactionsMsg]), funcName(handleXDCTradingTxNoop); got != want {
		t.Errorf("xdpos2[GetPooledTransactionsMsg] = %s, want the A.60.3 no-op %s", got, want)
	}
	if bad := funcName(handleNewPooledTransactionHashes); funcName(xdpos2[NewPooledTransactionHashesMsg]) == bad {
		t.Errorf("xdpos2[NewPooledTransactionHashesMsg] must not be the modern pooled-tx-hashes handler")
	}

	// A.60.1/A.33: GetReceipts/Receipts on XDPOS2 use the eth/63 bare-list wire
	// shape, not the eth/69+ RequestId-wrapped shape.
	if got, want := funcName(xdpos2[GetReceiptsMsg]), funcName(handleGetReceiptsXDPOS2); got != want {
		t.Errorf("xdpos2[GetReceiptsMsg] = %s, want the XDPOS2 bare-list variant %s", got, want)
	}
	if got, want := funcName(xdpos2[ReceiptsMsg]), funcName(handleReceiptsXDPOS2); got != want {
		t.Errorf("xdpos2[ReceiptsMsg] = %s, want the XDPOS2 bare-list variant %s", got, want)
	}
	if bad69 := funcName(handleGetReceipts69); funcName(xdpos2[GetReceiptsMsg]) == bad69 {
		t.Error("xdpos2[GetReceiptsMsg] must not be the eth/69 RequestId-wrapped handler")
	}
}

// TestOtherVersionMapsUnperturbed is a light sanity check that the
// eth69/eth70/eth71/eth72 handler maps introduced/kept by the merge are all
// present and distinct from xdpos2, i.e. the union resolution in
// eth/protocols/eth/handler.go did not accidentally collapse them.
func TestOtherVersionMapsUnperturbed(t *testing.T) {
	maps := map[string]map[uint64]msgHandler{
		"eth69":  eth69,
		"eth70":  eth70,
		"eth71":  eth71,
		"eth72":  eth72,
		"xdpos2": xdpos2,
	}
	for name, m := range maps {
		if len(m) == 0 {
			t.Errorf("%s handler map is empty", name)
		}
	}
	// eth71/eth72 must use the modern pooled-tx-hashes handlers, confirming
	// they were additively merged rather than overwritten by the xdpos2 side.
	if funcName(eth71[NewPooledTransactionHashesMsg]) != funcName(handleNewPooledTransactionHashes) {
		t.Error("eth71[NewPooledTransactionHashesMsg] should be the modern handler")
	}
	if funcName(eth72[GetCellsMsg]) != funcName(handleGetCells) {
		t.Error("eth72[GetCellsMsg] should be handleGetCells (upstream eth-72 cells support)")
	}
}

// TestStatusPacket62FieldCount asserts requirement 7: the #844 five-field
// wire invariant for the legacy eth/63-shaped Status packet. A sixth field
// (an accidental optional trailer) silently breaks wire compatibility with
// the live XDC mesh.
func TestStatusPacket62FieldCount(t *testing.T) {
	if n := reflect.TypeOf(StatusPacket62{}).NumField(); n != 5 {
		t.Fatalf("StatusPacket62 has %d fields, want exactly 5 (refs #844)", n)
	}
}

// TestHandleMessageDispatchesXDPOS2ToXdpos2Map asserts requirement 6 by
// driving an actual peer at version XDPOS2 through Handle() with a real
// devp2p message pipe, proving end-to-end that the version switch in
// handleMessage resolves XDPOS2 to the xdpos2 map (not eth69/70/71/72 or the
// "unknown protocol version" error path).
//
// BlockRangeUpdateMsg is chosen because its handler (handleBlockRangeUpdate)
// only decodes, validates, and stores the packet on the peer — no Backend
// interaction — keeping the test backend trivial while still exercising the
// real dispatch switch in eth/protocols/eth/handler.go.
func TestHandleMessageDispatchesXDPOS2ToXdpos2Map(t *testing.T) {
	backend := newTestBackend(1)
	defer backend.close()

	peer, errc := newTestPeer("xdpos2-dispatch-peer", XDPOS2, backend)
	defer peer.close()

	want := &BlockRangeUpdatePacket{
		EarliestBlock:   0,
		LatestBlock:     1,
		LatestBlockHash: backend.chain.CurrentBlock().Hash(),
	}
	if err := p2p.Send(peer.app, BlockRangeUpdateMsg, want); err != nil {
		t.Fatalf("failed to send BlockRangeUpdate: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if got := peer.BlockRange(); got != nil {
			if got.EarliestBlock != want.EarliestBlock || got.LatestBlock != want.LatestBlock || got.LatestBlockHash != want.LatestBlockHash {
				t.Fatalf("BlockRange mismatch: got %+v, want %+v", got, want)
			}
			return
		}
		select {
		case err := <-errc:
			t.Fatalf("peer handler exited early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for BlockRangeUpdateMsg dispatch via xdpos2 map")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
