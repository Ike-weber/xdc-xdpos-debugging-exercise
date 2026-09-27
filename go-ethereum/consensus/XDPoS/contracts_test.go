// Copyright (c) 2024 XDC Network
// Unit tests for ExtractAddressesFromReturn (security fix #93).

package XDPoS

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// buildReturnData constructs well-formed ABI-encoded address[] return data.
// Format: offset(32) + length(32) + n*(padding(12)+addr(20)) padded to 32.
func buildReturnData(addrs []common.Address) []byte {
	data := make([]byte, 64+len(addrs)*32)
	// offset = 0x20 (points to length field)
	data[31] = 0x20
	// length
	binary.BigEndian.PutUint64(data[56:64], uint64(len(addrs)))
	for i, addr := range addrs {
		start := 64 + i*32 + 12
		copy(data[start:], addr[:])
	}
	return data
}

func TestExtractAddressesFromReturn_Empty(t *testing.T) {
	result := ExtractAddressesFromReturn(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
	result = ExtractAddressesFromReturn([]byte{})
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
	result = ExtractAddressesFromReturn(make([]byte, 63))
	if result != nil {
		t.Errorf("expected nil for 63-byte input, got %v", result)
	}
}

func TestExtractAddressesFromReturn_ZeroLength(t *testing.T) {
	data := buildReturnData(nil) // length = 0
	result := ExtractAddressesFromReturn(data)
	if result != nil {
		t.Errorf("expected nil for zero-length address list, got %v", result)
	}
}

func TestExtractAddressesFromReturn_SingleAddress(t *testing.T) {
	addr := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")
	data := buildReturnData([]common.Address{addr})
	result := ExtractAddressesFromReturn(data)
	if len(result) != 1 {
		t.Fatalf("expected 1 address, got %d", len(result))
	}
	if result[0] != addr {
		t.Errorf("expected %v, got %v", addr, result[0])
	}
}

func TestExtractAddressesFromReturn_MultipleAddresses(t *testing.T) {
	addrs := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		common.HexToAddress("0x0000000000000000000000000000000000000002"),
		common.HexToAddress("0x0000000000000000000000000000000000000003"),
	}
	data := buildReturnData(addrs)
	result := ExtractAddressesFromReturn(data)
	if len(result) != len(addrs) {
		t.Fatalf("expected %d addresses, got %d", len(addrs), len(result))
	}
	for i, want := range addrs {
		if result[i] != want {
			t.Errorf("index %d: expected %v, got %v", i, want, result[i])
		}
	}
}

// TestExtractAddressesFromReturn_OverflowLength verifies that a maliciously crafted
// length field (> maxAddresses = 4096) is rejected without panicking.
func TestExtractAddressesFromReturn_OverflowLength(t *testing.T) {
	// Build 64-byte header claiming length = 0xFFFFFFFFFFFFFFFF
	data := make([]byte, 64)
	data[31] = 0x20
	for i := 32; i < 64; i++ {
		data[i] = 0xFF
	}
	result := ExtractAddressesFromReturn(data)
	if result != nil {
		t.Errorf("expected nil for overflow length, got %d addresses", len(result))
	}
}

// TestExtractAddressesFromReturn_LargeButValidLength verifies that exactly
// maxAddresses addresses are accepted.
func TestExtractAddressesFromReturn_MaxAddresses(t *testing.T) {
	const max = 4096
	addrs := make([]common.Address, max)
	for i := range addrs {
		addrs[i][19] = byte(i & 0xFF)
		addrs[i][18] = byte((i >> 8) & 0xFF)
	}
	data := buildReturnData(addrs)
	result := ExtractAddressesFromReturn(data)
	if len(result) != max {
		t.Errorf("expected %d addresses, got %d", max, len(result))
	}
}

// TestExtractAddressesFromReturn_TruncatedPayload verifies graceful handling
// when the byte slice is shorter than the declared length implies.
func TestExtractAddressesFromReturn_TruncatedPayload(t *testing.T) {
	addrs := make([]common.Address, 10)
	data := buildReturnData(addrs)
	// Truncate to fewer bytes than needed
	truncated := data[:64+3*32] // only room for 3 addresses despite claiming 10
	// The length check `len(data) < required` should catch this and return nil.
	result := ExtractAddressesFromReturn(truncated)
	if result != nil {
		t.Errorf("expected nil for truncated payload, got %d addresses", len(result))
	}
}
