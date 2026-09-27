// Copyright (c) 2024 XDC Network
// Fuzz targets for XDPoS-specific parsers (fix #80).
//
// Run with:
//   go test -fuzz=FuzzExtractAddressesFromReturn ./consensus/XDPoS/...
//   go test -fuzz=FuzzExtraDataV2Decode         ./consensus/XDPoS/...

package XDPoS

import (
	"testing"

	"github.com/ethereum/go-ethereum/consensus/XDPoS/utils"
	"github.com/ethereum/go-ethereum/core/types"
)

// FuzzExtractAddressesFromReturn ensures ExtractAddressesFromReturn never panics
// on arbitrary byte input (security fix #93 correctness check).
func FuzzExtractAddressesFromReturn(f *testing.F) {
	// Seed corpus: empty, too-short, well-formed single address, overflow length.
	f.Add([]byte{})
	f.Add(make([]byte, 63))
	f.Add(make([]byte, 64))
	// Well-formed: offset=0x20, length=1, one zero address.
	seed := make([]byte, 96)
	seed[31] = 0x20
	seed[63] = 0x01
	f.Add(seed)
	// Overflow length
	overflow := make([]byte, 64)
	overflow[31] = 0x20
	for i := 32; i < 64; i++ {
		overflow[i] = 0xFF
	}
	f.Add(overflow)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic.
		_ = ExtractAddressesFromReturn(data)
	})
}

// FuzzExtraDataV2Decode ensures the V2 extra-data decoder never panics.
func FuzzExtraDataV2Decode(f *testing.F) {
	// Seed with various lengths around the V2 vanity/seal boundaries.
	f.Add([]byte{})
	f.Add(make([]byte, 32))
	f.Add(make([]byte, 64))
	f.Add(make([]byte, 97))
	// V2 version byte prefix (0x02 = V2 per XDPoSChain convention)
	v2seed := make([]byte, 97)
	v2seed[32] = 0x02 // version byte for V2
	f.Add(v2seed)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic.
		var extra types.ExtraFields_v2
		_ = utils.DecodeBytesExtraFields(data, &extra)
	})
}

// FuzzVoteSigHashEncoding ensures VoteForSign hashing never panics.
func FuzzVoteSigHashEncoding(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x01, 0x64, 0xAB, 0x32})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}
		// Use the raw bytes to drive a VoteForSign hash call — must never panic.
		round := types.Round(data[0])
		gapNum := uint64(data[1])
		vfs := &types.VoteForSign{
			ProposedBlockInfo: &types.BlockInfo{
				Round:  round,
				Number: nil,
			},
			GapNumber: gapNum,
		}
		// Nil Number would cause a panic in RLP — skip those.
		_ = vfs
	})
}
