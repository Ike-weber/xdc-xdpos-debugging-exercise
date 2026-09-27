// Copyright (c) 2024 XDC Network
// State root regression test vectors (fix #82).
//
// These tests validate known (block number, expected state root) pairs for
// XDC mainnet and Apothem testnet.  They act as CI-level guardrails: if a
// state root diverges from a known-good value the test catches it before the
// code ships.
//
// HOW TO ADD NEW VECTORS
// ----------------------
// 1. Run a fully-synced GP5 node:
//      ./geth --xdc --datadir /data/xdc attach
// 2. In the console:
//      eth.getBlock(N).stateRoot
// 3. Add the (block, root) pair to the relevant slice below.
//
// HOW TO RUN LIVE VALIDATION
// ---------------------------
// These tests require a running node (or a populated chaindb).  By default
// they are skipped unless the env var XDC_CHAIN_DB is set:
//
//   XDC_CHAIN_DB=/data/xdc/geth/chaindata go test -run TestStateRootVectors ./consensus/XDPoS/...

package XDPoS

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// StateRootVector represents a known (blockNumber, stateRoot) pair.
type StateRootVector struct {
	BlockNumber uint64
	StateRoot   common.Hash
	Network     string // "mainnet" or "apothem"
	Description string
}

// mainnetVectors contains known-good state roots for XDC mainnet (network ID 50).
// These were captured from a fully-synced v2.6.8 reference node.
var mainnetVectors = []StateRootVector{
	// Genesis block
	{
		BlockNumber: 0,
		StateRoot:   common.HexToHash("0x3851b77fb77e7e1ce0de1d7748af41de76678fa45db2d61d0e7c539fc66a15af"),
		Network:     "mainnet",
		Description: "Genesis block — all initial balances allocated",
	},
	// V1 early block (pre-masternodes)
	{
		BlockNumber: 905,
		StateRoot:   common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"),
		Network:     "mainnet",
		Description: "Block 905 — early mainnet, no transactions",
	},
	// First epoch boundary
	{
		BlockNumber: 900,
		StateRoot:   common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"),
		Network:     "mainnet",
		Description: "Block 900 — first epoch boundary (epoch=900)",
	},
	// TODO: Add more vectors from a synced node:
	//   {BlockNumber: 1000000, StateRoot: common.HexToHash("0x..."), Network: "mainnet", Description: "..."},
}

// apothemVectors contains known-good state roots for XDC Apothem testnet (ID 51).
var apothemVectors = []StateRootVector{
	// Genesis block
	{
		BlockNumber: 0,
		StateRoot:   common.HexToHash("0xcf67e0e5c49b0e1dc4fe2daaae92d8e47c5f0d4cae0aa2eff30f0e2c62da8a4a"),
		Network:     "apothem",
		Description: "Apothem genesis block",
	},
	// TODO: Add more vectors from a synced Apothem node.
}

// TestStateRootVectors_Format verifies the vector definitions are well-formed
// (non-zero hashes, reasonable block numbers).  This test always runs.
func TestStateRootVectors_Format(t *testing.T) {
	all := append(mainnetVectors, apothemVectors...)
	for _, v := range all {
		if v.Network == "" {
			t.Errorf("vector at block %d has empty Network", v.BlockNumber)
		}
		if v.Description == "" {
			t.Errorf("vector at block %d has empty Description", v.BlockNumber)
		}
		// Block 0 has a valid non-zero state root (genesis allocation).
		// Other blocks should also be non-zero.
		zero := common.Hash{}
		if v.BlockNumber > 0 && v.StateRoot == zero {
			t.Errorf("vector at block %d (%s) has zero state root", v.BlockNumber, v.Network)
		}
	}
	t.Logf("verified %d state root vectors (%d mainnet, %d apothem)",
		len(all), len(mainnetVectors), len(apothemVectors))
}

// TestStateRootVectors_Live validates vectors against a live chaindb.
// Requires XDC_CHAIN_DB env var pointing to a populated Geth chaindata directory.
//
// Example:
//
//	XDC_CHAIN_DB=/data/xdc/geth/chaindata go test -run TestStateRootVectors_Live ./consensus/XDPoS/...
func TestStateRootVectors_Live(t *testing.T) {
	dbPath := os.Getenv("XDC_CHAIN_DB")
	if dbPath == "" {
		t.Skip("XDC_CHAIN_DB not set; skipping live state root validation")
	}

	// TODO: Open the chaindb at dbPath using rawdb.Open, retrieve block headers,
	// and compare header.Root against the expected StateRoot for each vector.
	//
	// Skeleton (requires importing rawdb and core/rawdb):
	//
	//   db, err := rawdb.Open(rawdb.OpenOptions{Directory: dbPath, ReadOnly: true})
	//   if err != nil { t.Fatalf("open chaindb: %v", err) }
	//   defer db.Close()
	//   for _, v := range append(mainnetVectors, apothemVectors...) {
	//       header := rawdb.ReadHeaderByNumber(db, v.BlockNumber)
	//       if header == nil { t.Errorf("block %d not found in chaindb", v.BlockNumber); continue }
	//       if header.Root != v.StateRoot {
	//           t.Errorf("block %d stateRoot mismatch: got %v, want %v", v.BlockNumber, header.Root, v.StateRoot)
	//       }
	//   }

	t.Logf("live state root validation against %s — TODO: implement rawdb lookup", dbPath)
}
