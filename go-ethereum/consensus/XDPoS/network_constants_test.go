// Copyright 2024 The go-ethereum Authors
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

package XDPoS

import (
	"math/big"
	"testing"
)

// TestSetNetworkConstants_DefaultProfile verifies that an unregistered chain ID
// resolves to the LEGACY-COMPATIBLE XDC default profile: gas-charged signing
// (TIPSigning = 3,000,000, matching legacy oldxdc v2.7.1), base EVM Berlin/London
// at 0, and the net5151/5152 staircase (V2 @1800, EIP-1559 @2250, Cancun @3150).
// It also confirms that mainnet (50) is not affected by the default arm.
func TestSetNetworkConstants_DefaultProfile(t *testing.T) {
	// Unregistered chain ID — must hit the default arm.
	SetNetworkConstants(987654)
	c := GetCurrentConstants()
	if c == nil {
		t.Fatal("GetCurrentConstants returned nil for unregistered chain")
	}
	for _, tc := range []struct {
		name string
		got  *big.Int
		want int64
	}{
		{"TIPSigning", c.TIPSigning, 3000000}, // GAS-CHARGED — legacy parity, NOT gasless 0
		{"TIPBerlinBlock", c.TIPBerlinBlock, 0},
		{"TIPLondonBlock", c.TIPLondonBlock, 0},
		{"TIPV2SwitchBlock", c.TIPV2SwitchBlock, 1800},
		{"TIPEIP1559Block", c.TIPEIP1559Block, 2250},
		{"TIPCancunBlock", c.TIPCancunBlock, 3150},
	} {
		if tc.got == nil {
			t.Errorf("default: %s is nil, want %d", tc.name, tc.want)
			continue
		}
		if tc.got.Int64() != tc.want {
			t.Errorf("default: %s = %v, want %d", tc.name, tc.got, tc.want)
		}
	}
	if c.MaxMasternodesV2 != 108 {
		t.Errorf("default: MaxMasternodesV2 = %d, want 108", c.MaxMasternodesV2)
	}

	// Mainnet (50) must restore mainnet constants, not the default profile.
	SetNetworkConstants(50)
	m := GetCurrentConstants()
	if m == nil {
		t.Fatal("GetCurrentConstants returned nil for mainnet")
	}
	// Mainnet TIPSigning is non-zero (3,000,000) — not 0.
	if m.TIPSigning == nil || m.TIPSigning.Sign() == 0 {
		t.Errorf("mainnet: TIPSigning = %v, want non-zero (3000000)", m.TIPSigning)
	}
	// Mainnet TIPV2SwitchBlock is non-zero — not 0.
	if m.TIPV2SwitchBlock == nil || m.TIPV2SwitchBlock.Sign() == 0 {
		t.Errorf("mainnet: TIPV2SwitchBlock = %v, want non-zero", m.TIPV2SwitchBlock)
	}

	// Restore to mainnet so we don't pollute other tests in this package.
	SetNetworkConstants(50)
}
