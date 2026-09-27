package types

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// TestHeaderTimeRLPCompatibility verifies that uint64 and *big.Int produce
// identical RLP encoding for all realistic timestamp values.
//
// This test documents the resolution of issue #233:
// GP5 uses uint64 for Header.Time (matching upstream geth 1.17).
// XDC v2.6.8 uses *big.Int.
// For all values < 2^64, RLP encoding is identical, so block hashes match.
// Changing Header.Time to *big.Int would touch ~200 files and make upstream
// merges prohibitively difficult. This divergence is accepted as permanent.
func TestHeaderTimeRLPCompatibility(t *testing.T) {
	testCases := []uint64{
		0,
		1,
		42,
		1000000,
		1713600000, // ~2024-04-20
		2000000000, // ~2033
		^uint64(0) >> 1, // max int64
	}

	for _, ts := range testCases {
		// Encode as uint64 (GP5 style)
		var buf64 bytes.Buffer
		if err := rlp.Encode(&buf64, ts); err != nil {
			t.Fatalf("uint64 encode failed for %d: %v", ts, err)
		}

		// Encode as *big.Int (v2.6.8 style)
		var bufBig bytes.Buffer
		if err := rlp.Encode(&bufBig, new(big.Int).SetUint64(ts)); err != nil {
			t.Fatalf("*big.Int encode failed for %d: %v", ts, err)
		}

		if !bytes.Equal(buf64.Bytes(), bufBig.Bytes()) {
			t.Errorf("RLP mismatch for timestamp %d: uint64=%x, *big.Int=%x",
				ts, buf64.Bytes(), bufBig.Bytes())
		}
	}
}

// TestHeaderHashWithTime verifies that a Header with a realistic timestamp
// encodes consistently regardless of how Time is accessed.
func TestHeaderHashWithTime(t *testing.T) {
	header := &Header{
		ParentHash:  EmptyRootHash,
		UncleHash:   EmptyUncleHash,
		Coinbase:    common.Address{},
		Root:        EmptyRootHash,
		TxHash:      EmptyTxsHash,
		ReceiptHash: EmptyReceiptsHash,
		Bloom:       Bloom{},
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(1),
		GasLimit:    8000000,
		GasUsed:     0,
		Time:        1713600000,
		Extra:       []byte("xdc test"),
		MixDigest:   common.Hash{},
		Nonce:       BlockNonce{},
	}

	// Compute hash
	hash := header.Hash()

	// Verify hash is non-zero and consistent
	if hash == (common.Hash{}) {
		t.Fatal("header hash is zero")
	}

	// Re-compute and verify determinism
	hash2 := header.Hash()
	if hash != hash2 {
		t.Fatalf("header hash not deterministic: %v vs %v", hash, hash2)
	}
}
