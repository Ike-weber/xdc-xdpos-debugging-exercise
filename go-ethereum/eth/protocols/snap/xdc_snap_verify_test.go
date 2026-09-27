// Copyright (c) 2024 XDC Network
// Tests for XDC snap sync header verification (fix #81).

package snap

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func makeHeader(extraLen int, nonceLen int) *types.Header {
	h := &types.Header{
		Number:     big.NewInt(1000),
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, extraLen),
	}
	if nonceLen == 8 {
		copy(h.Nonce[:], make([]byte, 8))
	}
	return h
}

func TestVerifyXDCSnapHeader_Nil(t *testing.T) {
	if err := VerifyXDCSnapHeader(nil); err == nil {
		t.Error("expected error for nil header")
	}
}

func TestVerifyXDCSnapHeader_ValidV1(t *testing.T) {
	h := makeHeader(65, 8) // 32 vanity + seal, 8-byte nonce
	if err := VerifyXDCSnapHeader(h); err != nil {
		t.Errorf("expected valid V1 header to pass, got: %v", err)
	}
}

func TestVerifyXDCSnapHeader_ValidV2(t *testing.T) {
	h := makeHeader(97, 0) // 32 vanity + version + extra
	h.Extra[32] = 0x02     // V2 version byte
	if err := VerifyXDCSnapHeader(h); err != nil {
		t.Errorf("expected valid V2 header to pass, got: %v", err)
	}
}

func TestVerifyXDCSnapHeader_ExtraTooShort(t *testing.T) {
	h := makeHeader(10, 8) // only 10 bytes extra — too short
	if err := VerifyXDCSnapHeader(h); err == nil {
		t.Error("expected error for too-short extra data")
	}
}

func TestVerifyXDCSnapHeaderBatch_AllValid(t *testing.T) {
	headers := []*types.Header{
		makeHeader(65, 8),
		makeHeader(97, 0),
	}
	if err := VerifyXDCSnapHeaderBatch(headers); err != nil {
		t.Errorf("expected batch to pass, got: %v", err)
	}
}

func TestVerifyXDCSnapHeaderBatch_OneInvalid(t *testing.T) {
	headers := []*types.Header{
		makeHeader(65, 8),
		makeHeader(10, 8), // invalid
	}
	if err := VerifyXDCSnapHeaderBatch(headers); err == nil {
		t.Error("expected batch to fail on invalid header")
	}
}

func TestIsXDCV2Block_V1(t *testing.T) {
	h := makeHeader(65, 8)
	h.Extra[32] = 0x00 // not V2
	if IsXDCV2Block(h) {
		t.Error("expected false for V1 block")
	}
}

func TestIsXDCV2Block_V2(t *testing.T) {
	h := makeHeader(97, 0)
	h.Extra[32] = 0x02 // V2
	if !IsXDCV2Block(h) {
		t.Error("expected true for V2 block")
	}
}

func TestIsXDCV2Block_ShortExtra(t *testing.T) {
	h := makeHeader(32, 0) // exactly 32 bytes — no version byte
	if IsXDCV2Block(h) {
		t.Error("expected false for header with only vanity extra")
	}
}
