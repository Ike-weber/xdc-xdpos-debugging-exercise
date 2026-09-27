// Copyright 2020 The go-ethereum Authors
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

package eth

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// Tests that the custom union field encoder and decoder works correctly.
func TestGetBlockHeadersDataEncodeDecode(t *testing.T) {
	// Create a "random" hash for testing
	var hash common.Hash
	for i := range hash {
		hash[i] = byte(i)
	}
	// Assemble some table driven tests
	tests := []struct {
		packet *GetBlockHeadersRequest
		fail   bool
	}{
		// Providing the origin as either a hash or a number should both work
		{fail: false, packet: &GetBlockHeadersRequest{Origin: HashOrNumber{Number: 314}}},
		{fail: false, packet: &GetBlockHeadersRequest{Origin: HashOrNumber{Hash: hash}}},

		// Providing arbitrary query field should also work
		{fail: false, packet: &GetBlockHeadersRequest{Origin: HashOrNumber{Number: 314}, Amount: 314, Skip: 1, Reverse: true}},
		{fail: false, packet: &GetBlockHeadersRequest{Origin: HashOrNumber{Hash: hash}, Amount: 314, Skip: 1, Reverse: true}},

		// Providing both the origin hash and origin number must fail
		{fail: true, packet: &GetBlockHeadersRequest{Origin: HashOrNumber{Hash: hash, Number: 314}}},
	}
	// Iterate over each of the tests and try to encode and then decode
	for i, tt := range tests {
		bytes, err := rlp.EncodeToBytes(tt.packet)
		if err != nil && !tt.fail {
			t.Fatalf("test %d: failed to encode packet: %v", i, err)
		} else if err == nil && tt.fail {
			t.Fatalf("test %d: encode should have failed", i)
		}
		if !tt.fail {
			packet := new(GetBlockHeadersRequest)
			if err := rlp.DecodeBytes(bytes, packet); err != nil {
				t.Fatalf("test %d: failed to decode packet: %v", i, err)
			}
			if packet.Origin.Hash != tt.packet.Origin.Hash || packet.Origin.Number != tt.packet.Origin.Number || packet.Amount != tt.packet.Amount ||
				packet.Skip != tt.packet.Skip || packet.Reverse != tt.packet.Reverse {
				t.Fatalf("test %d: encode decode mismatch: have %+v, want %+v", i, packet, tt.packet)
			}
		}
	}
}

// TestEmptyMessages tests encoding of empty messages.
func TestEmptyMessages(t *testing.T) {
	// All empty messages encodes to the same format
	want := common.FromHex("c4820457c0")

	for i, msg := range []any{
		// Headers
		GetBlockHeadersPacket{1111, nil},
		// Bodies
		GetBlockBodiesPacket{1111, nil},
		BlockBodiesRLPPacket{1111, nil},
		// Receipts
		GetReceiptsPacket69{1111, nil},
		// Transactions
		GetPooledTransactionsPacket{1111, nil},
		PooledTransactionsRLPPacket{1111, nil},
		// Headers
		BlockHeadersPacket{1111, encodeRL([]*types.Header{})},
		// Bodies
		GetBlockBodiesPacket{1111, GetBlockBodiesRequest([]common.Hash{})},
		BlockBodiesPacket{1111, encodeRL([]BlockBody{})},
		BlockBodiesRLPPacket{1111, BlockBodiesRLPResponse([]rlp.RawValue{})},
		// Receipts
		GetReceiptsPacket69{1111, GetReceiptsRequest([]common.Hash{})},
		ReceiptsPacket69{1111, encodeRL([]*ReceiptList{})},
		// Transactions
		GetPooledTransactionsPacket{1111, GetPooledTransactionsRequest([]common.Hash{})},
		PooledTransactionsPacket{1111, encodeRL([]*types.Transaction{})},
		PooledTransactionsRLPPacket{1111, PooledTransactionsRLPResponse([]rlp.RawValue{})},
	} {
		have, err := rlp.EncodeToBytes(msg)
		if err != nil {
			t.Errorf("test %d, type %T, error: %v", i, msg, err)
		} else if !bytes.Equal(have, want) {
			t.Errorf("test %d, type %T, have\n\t%x\nwant\n\t%x", i, msg, have, want)
		}
	}
}

// TestMessages tests the encoding of all messages.
func TestMessages(t *testing.T) {
	// Some basic structs used during testing
	var (
		header       *types.Header
		blockBody    BlockBody
		blockBodyRlp rlp.RawValue
		txs          []*types.Transaction
		txRlps       []rlp.RawValue
		hashes       []common.Hash
		receipts     []*types.Receipt

		err error
	)
	header = &types.Header{
		Difficulty: big.NewInt(2222),
		Number:     big.NewInt(3333),
		GasLimit:   4444,
		GasUsed:    5555,
		Time:       6666,
		Extra:      []byte{0x77, 0x88},
	}
	// Init the transactions, taken from a different test
	{
		for _, hexrlp := range []string{
			"f867088504a817c8088302e2489435353535353535353535353535353535353535358202008025a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c12a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c10",
			"f867098504a817c809830334509435353535353535353535353535353535353535358202d98025a052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afba052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afb",
		} {
			var tx *types.Transaction
			rlpdata := common.FromHex(hexrlp)
			if err := rlp.DecodeBytes(rlpdata, &tx); err != nil {
				t.Fatal(err)
			}
			txs = append(txs, tx)
			txRlps = append(txRlps, rlpdata)
		}
	}
	// init the block body data, both object and rlp form
	blockBody = BlockBody{
		Transactions: encodeRL(txs),
		Uncles:       encodeRL([]*types.Header{header}),
	}
	blockBodyRlp, err = rlp.EncodeToBytes(blockBody)
	if err != nil {
		t.Fatal(err)
	}

	hashes = []common.Hash{
		common.HexToHash("deadc0de"),
		common.HexToHash("feedbeef"),
	}
	// init the receipts
	{
		receipts = []*types.Receipt{
			{
				Status:            types.ReceiptStatusFailed,
				CumulativeGasUsed: 333,
				Logs: []*types.Log{
					{
						Address: common.BytesToAddress([]byte{0x11}),
						Topics:  []common.Hash{common.HexToHash("dead"), common.HexToHash("beef")},
						Data:    []byte{0x01, 0x00, 0xff},
					},
				},
			},
			{
				Status:            types.ReceiptStatusSuccessful,
				CumulativeGasUsed: 444,
				Logs: []*types.Log{
					{
						Address: common.BytesToAddress([]byte{0x22}),
						Topics:  []common.Hash{common.HexToHash("05668"), common.HexToHash("9773")},
						Data:    []byte{0x02, 0x0f, 0x0f, 0x0f, 0x06, 0x08},
					},
				},
			},
		}
		miniDeriveFields(receipts[0], 0)
		miniDeriveFields(receipts[1], 1)
	}

	for i, tc := range []struct {
		message interface{}
		want    []byte
	}{
		{
			GetBlockHeadersPacket{1111, &GetBlockHeadersRequest{HashOrNumber{hashes[0], 0}, 5, 5, false}},
			common.FromHex("e8820457e4a000000000000000000000000000000000000000000000000000000000deadc0de050580"),
		},
		{
			GetBlockHeadersPacket{1111, &GetBlockHeadersRequest{HashOrNumber{common.Hash{}, 9999}, 5, 5, false}},
			common.FromHex("ca820457c682270f050580"),
		},
		{
			BlockHeadersPacket{1111, encodeRL([]*types.Header{header})},
			common.FromHex("f90202820457f901fcf901f9a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000940000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008208ae820d0582115c8215b3821a0a827788a00000000000000000000000000000000000000000000000000000000000000000880000000000000000"),
		},
		{
			GetBlockBodiesPacket{1111, GetBlockBodiesRequest(hashes)},
			common.FromHex("f847820457f842a000000000000000000000000000000000000000000000000000000000deadc0dea000000000000000000000000000000000000000000000000000000000feedbeef"),
		},
		{
			BlockBodiesPacket{1111, encodeRL([]BlockBody{blockBody})},
			common.FromHex("f902dc820457f902d6f902d3f8d2f867088504a817c8088302e2489435353535353535353535353535353535353535358202008025a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c12a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c10f867098504a817c809830334509435353535353535353535353535353535353535358202d98025a052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afba052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afbf901fcf901f9a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000940000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008208ae820d0582115c8215b3821a0a827788a00000000000000000000000000000000000000000000000000000000000000000880000000000000000"),
		},
		{ // Identical to non-rlp-shortcut version
			BlockBodiesRLPPacket{1111, BlockBodiesRLPResponse([]rlp.RawValue{blockBodyRlp})},
			common.FromHex("f902dc820457f902d6f902d3f8d2f867088504a817c8088302e2489435353535353535353535353535353535353535358202008025a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c12a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c10f867098504a817c809830334509435353535353535353535353535353535353535358202d98025a052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afba052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afbf901fcf901f9a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000940000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008208ae820d0582115c8215b3821a0a827788a00000000000000000000000000000000000000000000000000000000000000000880000000000000000"),
		},
		{
			GetReceiptsPacket69{1111, GetReceiptsRequest(hashes)},
			common.FromHex("f847820457f842a000000000000000000000000000000000000000000000000000000000deadc0dea000000000000000000000000000000000000000000000000000000000feedbeef"),
		},
		{
			ReceiptsPacket69{1111, encodeRL([]*ReceiptList{NewReceiptList(receipts)})},
			common.FromHex("f8da820457f8d5f8d3f866808082014df85ff85d940000000000000000000000000000000000000011f842a0000000000000000000000000000000000000000000000000000000000000deada0000000000000000000000000000000000000000000000000000000000000beef830100fff86901018201bcf862f860940000000000000000000000000000000000000022f842a00000000000000000000000000000000000000000000000000000000000005668a0000000000000000000000000000000000000000000000000000000000000977386020f0f0f0608"),
		},
		{
			GetPooledTransactionsPacket{1111, GetPooledTransactionsRequest(hashes)},
			common.FromHex("f847820457f842a000000000000000000000000000000000000000000000000000000000deadc0dea000000000000000000000000000000000000000000000000000000000feedbeef"),
		},
		{
			PooledTransactionsPacket{1111, encodeRL(txs)},
			common.FromHex("f8d7820457f8d2f867088504a817c8088302e2489435353535353535353535353535353535353535358202008025a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c12a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c10f867098504a817c809830334509435353535353535353535353535353535353535358202d98025a052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afba052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afb"),
		},
		{
			PooledTransactionsRLPPacket{1111, PooledTransactionsRLPResponse(txRlps)},
			common.FromHex("f8d7820457f8d2f867088504a817c8088302e2489435353535353535353535353535353535353535358202008025a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c12a064b1702d9298fee62dfeccc57d322a463ad55ca201256d01f62b45b2e1c21c10f867098504a817c809830334509435353535353535353535353535353535353535358202d98025a052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afba052f8f61201b2b11a78d6e866abc9c3db2ae8631fa656bfe5cb53668255367afb"),
		},
	} {
		if have, _ := rlp.EncodeToBytes(tc.message); !bytes.Equal(have, tc.want) {
			t.Errorf("test %d, type %T, have\n\t%x\nwant\n\t%x", i, tc.message, have, tc.want)
		}
	}
}

// legacyStatusPacket62 is the exact 5-field shape used by pre-A.37 geth-XDC
// peers (canonical a119e0b archive fleet). The A.37 regression was that
// outbound 6-field encodes from our handler tripped legacy peers' strict
// 5-field decoder ("input list has too many elements"). This local mirror
// lets us assert the cross-decode goes both ways without importing a
// historical wire shape.
type legacyStatusPacket62 struct {
	ProtocolVersion uint32
	NetworkID       uint64
	TD              *big.Int
	Head            common.Hash
	Genesis         common.Hash
}

// TestStatusPacket62LegacyWireCompat pins the on-wire shape of
// StatusPacket62 to exactly 5 fields. If somebody re-adds an `rlp:"optional"`
// trailer here, both directions of the cross-decode break against the
// production XDC fleet — see commit history A.37 regression notes. Refs #844.
func TestStatusPacket62LegacyWireCompat(t *testing.T) {
	ours := &StatusPacket62{
		ProtocolVersion: XDPOS2,
		NetworkID:       50,
		TD:              big.NewInt(123),
		Head:            common.HexToHash("0xaa"),
		Genesis:         common.HexToHash("0xbb"),
	}
	enc, err := rlp.EncodeToBytes(ours)
	if err != nil {
		t.Fatalf("encode our StatusPacket62: %v", err)
	}
	// A.37 fix invariant: our outbound bytes MUST decode cleanly into a
	// strict 5-field legacy struct. If this fails, legacy peers will reject
	// our handshake with "input list has too many elements" and silently
	// drop the connection.
	var legacy legacyStatusPacket62
	if err := rlp.DecodeBytes(enc, &legacy); err != nil {
		t.Fatalf("LEGACY peer cannot decode our StatusPacket62 (regresses A.37): %v", err)
	}
	if legacy.ProtocolVersion != ours.ProtocolVersion || legacy.NetworkID != ours.NetworkID ||
		legacy.TD.Cmp(ours.TD) != 0 || legacy.Head != ours.Head || legacy.Genesis != ours.Genesis {
		t.Fatalf("legacy decode field mismatch: %+v vs %+v", legacy, ours)
	}
	// Reverse direction: a legacy peer's 5-field encode must also decode
	// cleanly into our struct.
	theirs := &legacyStatusPacket62{
		ProtocolVersion: XDPOS2,
		NetworkID:       50,
		TD:              big.NewInt(456),
		Head:            common.HexToHash("0xcc"),
		Genesis:         common.HexToHash("0xbb"),
	}
	enc2, err := rlp.EncodeToBytes(theirs)
	if err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	var got StatusPacket62
	if err := rlp.DecodeBytes(enc2, &got); err != nil {
		t.Fatalf("we cannot decode legacy peer's 5-field StatusPacket62: %v", err)
	}
	if got.TD.Cmp(theirs.TD) != 0 || got.Head != theirs.Head {
		t.Fatalf("our decode field mismatch: %+v vs %+v", got, theirs)
	}
}
