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

package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// TestXDCDefaultPrivateChainResolution verifies that an unregistered XDPoS chain
// (chainId 987654 — arbitrary, not mainnet/51/551/5551) whose genesis declares
// the base EVM ladder up to Byzantium is resolved GENESIS-AUTHORITATIVELY: the
// genesis's own highest declared fork (byzantiumBlock) becomes a ceiling, and
// nothing above that ceiling is injected — including the block-0 defaults the
// XDCDefaultPrivateChainConfig profile carries for Constantinople/Petersburg/
// Istanbul/Berlin/London. After resolution the config must have:
//   - Constantinople/Petersburg/Istanbul/Berlin/London/Eip1559/Cancun ALL nil —
//     the genesis never declared any of them, so neither do the other clients
//     and producers on that net, and injecting even a "block 0" default above
//     the genesis's own ceiling would silently change this node's EVM alone
//     (ethOne#62; this is the geth-follower-never-mints defect on netv12).
//   - ByzantiumBlock stays 0 (declared, untouched).
//   - XDPoS.V2 non-nil with SwitchBlock==1800 and AllConfigs non-nil
//   - CheckConfigForkOrder passes
//
// The genesis JSON for a fresh private XDC net carries the base EVM compatibility
// blocks (Homestead through Byzantium, all at 0) but omits the newer fork blocks
// and V2 — the V2 config still comes from the XDCDefaultPrivateChainConfig profile
// in code (that patch is unaffected by the genesis-authoritative fork-ceiling
// rule, which only governs EVM fork blocks). The patching path fires on the
// second SetupGenesisBlock call (re-init against a DB that already has the
// genesis committed), which is the real-world node-restart scenario that
// LoadChainConfig / SetupGenesisBlockWithOverride cover.
func TestXDCDefaultPrivateChainResolution(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)

	// Minimal genesis shaped like a real private-net genesis JSON:
	// base EVM blocks at 0, XDPoS config, no post-Byzantium fork blocks, V2 nil.
	g := &Genesis{
		Config: &params.ChainConfig{
			ChainID:        big.NewInt(987654),
			HomesteadBlock: big.NewInt(0),
			EIP150Block:    big.NewInt(0),
			EIP155Block:    big.NewInt(0),
			EIP158Block:    big.NewInt(0),
			ByzantiumBlock: big.NewInt(0),
			// Constantinople/Petersburg/Istanbul/Berlin/London/Eip1559/Cancun
			// left nil — the default profile patches them in.
			XDPoS: &params.XDPoSConfig{
				Period:              2,
				Epoch:               900,
				Reward:              5000,
				RewardCheckpoint:    900,
				Gap:                 450,
				FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000001"),
				// V2 intentionally nil — to be patched by SetupGenesisBlock
			},
		},
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x0000000000000000000000000000000000000001"): {
				Balance: big.NewInt(1e18),
			},
		},
	}

	// First call: DB is empty. SetupGenesisBlock commits the genesis and returns
	// the unpatched config — patching only fires on re-init (stored-config path).
	_, _, _, err := SetupGenesisBlock(db, tdb, g)
	if err != nil {
		t.Fatalf("first SetupGenesisBlock (init) failed: %v", err)
	}

	// Second call: DB now has the genesis committed. SetupGenesisBlock follows the
	// stored-config path and applies the default-profile patches before returning.
	cfg, _, compatErr, err := SetupGenesisBlock(db, tdb, g)
	if err != nil {
		t.Fatalf("second SetupGenesisBlock (re-init) failed: %v", err)
	}
	if compatErr != nil {
		t.Fatalf("SetupGenesisBlock returned compat error: %v", compatErr)
	}

	// ByzantiumBlock is the genesis's own declared ceiling: it stays 0, declared
	// and untouched.
	if cfg.ByzantiumBlock == nil || cfg.ByzantiumBlock.Sign() != 0 {
		t.Errorf("ByzantiumBlock: want 0 (declared, untouched), got %v", cfg.ByzantiumBlock)
	}

	// Everything ABOVE that ceiling — Constantinople/Petersburg/Istanbul/Berlin/
	// London (whose defaults in XDCDefaultPrivateChainConfig are all block 0) and
	// the future-dated Eip1559/Cancun — must stay nil. This genesis never
	// declared any of them, so neither do the other clients and producers on
	// that net; injecting even a "block 0" default here is exactly the defect
	// that left a genuinely-seated geth masternode unable to ever mint (Istanbul/
	// EIP-2929 repricing masked by A.94, then refused by the node's own
	// fee-market mint gate). Refs ethOne#62.
	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", cfg.ConstantinopleBlock},
		{"PetersburgBlock", cfg.PetersburgBlock},
		{"IstanbulBlock", cfg.IstanbulBlock},
		{"BerlinBlock", cfg.BerlinBlock},
		{"LondonBlock", cfg.LondonBlock},
		{"Eip1559Block", cfg.Eip1559Block},
		{"CancunBlock", cfg.CancunBlock},
	} {
		if tc.block != nil {
			t.Errorf("%s: want nil (above the genesis-declared ceiling, must not be inherited), got %v", tc.name, tc.block)
		}
	}

	// XDPoS.V2 must have been patched in from the default profile (switch @1800).
	if cfg.XDPoS == nil {
		t.Fatal("XDPoS config is nil after resolution")
	}
	v2 := cfg.XDPoS.V2
	if v2 == nil {
		t.Fatal("XDPoS.V2 is nil after resolution — default V2 patch did not fire")
	}
	if v2.SwitchBlock == nil || v2.SwitchBlock.Int64() != 1800 {
		t.Errorf("XDPoS.V2.SwitchBlock: want 1800, got %v", v2.SwitchBlock)
	}
	if v2.AllConfigs == nil {
		t.Fatal("XDPoS.V2.AllConfigs is nil — MainnetV2Configs not inherited")
	}

	// CheckConfigForkOrder must pass — no fork ordering violation.
	if err := cfg.CheckConfigForkOrder(); err != nil {
		t.Errorf("CheckConfigForkOrder failed: %v", err)
	}
}

// TestXDCPrivateChainGenesisDeclaredForksHonored is the counterpart to the test
// above: a private net that DOES want the staged EIP-1559/Cancun ladder declares
// it in its genesis JSON, and those values must survive resolution untouched.
// patchMissingForkBlocks only ever fills nil fields, so declaring a fork is the
// supported way to schedule one on an unregistered chain — which is what the
// net5151-class genesis files do (londonBlock/eip1559Block/cancunBlock spelled
// out explicitly). Refs ethOne#62.
func TestXDCPrivateChainGenesisDeclaredForksHonored(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)

	g := &Genesis{
		Config: &params.ChainConfig{
			// Shaped after the real net5151-class genesis JSON, which declares the
			// whole ladder explicitly.
			ChainID:             big.NewInt(987655),
			HomesteadBlock:      big.NewInt(1),
			EIP150Block:         big.NewInt(2),
			EIP155Block:         big.NewInt(3),
			EIP158Block:         big.NewInt(3),
			ByzantiumBlock:      big.NewInt(4),
			ConstantinopleBlock: big.NewInt(4),
			PetersburgBlock:     big.NewInt(4),
			IstanbulBlock:       big.NewInt(4),
			BerlinBlock:         big.NewInt(450),
			// Explicitly declared staged ladder — must be preserved verbatim.
			LondonBlock:  big.NewInt(450),
			Eip1559Block: big.NewInt(2250),
			CancunBlock:  big.NewInt(3150),
			XDPoS: &params.XDPoSConfig{
				Period:              2,
				Epoch:               900,
				Reward:              5000,
				RewardCheckpoint:    900,
				Gap:                 450,
				FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			},
		},
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x0000000000000000000000000000000000000001"): {
				Balance: big.NewInt(1e18),
			},
		},
	}

	if _, _, _, err := SetupGenesisBlock(db, tdb, g); err != nil {
		t.Fatalf("first SetupGenesisBlock (init) failed: %v", err)
	}
	cfg, _, compatErr, err := SetupGenesisBlock(db, tdb, g)
	if err != nil {
		t.Fatalf("second SetupGenesisBlock (re-init) failed: %v", err)
	}
	if compatErr != nil {
		t.Fatalf("SetupGenesisBlock returned compat error: %v", compatErr)
	}

	for _, tc := range []struct {
		name  string
		block *big.Int
		want  int64
	}{
		{"LondonBlock", cfg.LondonBlock, 450},
		{"Eip1559Block", cfg.Eip1559Block, 2250},
		{"CancunBlock", cfg.CancunBlock, 3150},
	} {
		if tc.block == nil {
			t.Errorf("%s: want %d, got nil — genesis-declared fork was dropped", tc.name, tc.want)
			continue
		}
		if tc.block.Int64() != tc.want {
			t.Errorf("%s: want %d, got %v — genesis-declared fork was overwritten", tc.name, tc.want, tc.block)
		}
	}
	if err := cfg.CheckConfigForkOrder(); err != nil {
		t.Errorf("CheckConfigForkOrder failed: %v", err)
	}
}

// TestPatchMissingForkBlocksFutureForkGate pins the two modes of
// patchMissingForkBlocks directly:
//   - forkPatchRegistered inherits the canonical schedule including
//     future-dated forks (unchanged pre-existing behaviour).
//   - forkPatchGenesisAuthoritative inherits NOTHING above the genesis's own
//     declared ceiling — including forks whose default is block 0 — and,
//     at or below that ceiling, only forks already active at block 0
//     (future-dated ones are still refused).
//
// A config that declares only ByzantiumBlock has byzantiumBlock as its
// ceiling, which is BELOW Constantinople/Berlin/London/Eip1559/Cancun in the
// ladder, so genesis-authoritative mode must inherit NONE of them — this is
// the exact shape of the netv12 defect (ethOne#62): the pre-fix code inherited
// Constantinople/Berlin/London at block 0 anyway because it never considered
// a ceiling, only a future/not-future split.
func TestPatchMissingForkBlocksFutureForkGate(t *testing.T) {
	defaults := &params.ChainConfig{
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		Eip1559Block:        big.NewInt(2250),
		CancunBlock:         big.NewInt(3150),
	}

	// Registered-network mode: everything is inherited, future-dated included.
	registered := &params.ChainConfig{ByzantiumBlock: big.NewInt(0)}
	patchMissingForkBlocks(registered, defaults, "test-registered", forkPatchRegistered)
	if registered.Eip1559Block == nil || registered.Eip1559Block.Int64() != 2250 {
		t.Errorf("registered: Eip1559Block want 2250, got %v", registered.Eip1559Block)
	}
	if registered.CancunBlock == nil || registered.CancunBlock.Int64() != 3150 {
		t.Errorf("registered: CancunBlock want 3150, got %v", registered.CancunBlock)
	}
	if registered.LondonBlock == nil || registered.LondonBlock.Int64() != 0 {
		t.Errorf("registered: LondonBlock want 0, got %v", registered.LondonBlock)
	}
	if registered.ConstantinopleBlock == nil || registered.ConstantinopleBlock.Int64() != 0 {
		t.Errorf("registered: ConstantinopleBlock want 0, got %v", registered.ConstantinopleBlock)
	}

	// Genesis-authoritative mode, byzantium-only genesis: the ceiling is
	// byzantiumBlock, which is BELOW every nil field here, so NONE of them
	// are inherited — not even Constantinople/Berlin/London whose defaults
	// are block 0. This is the corrected behaviour; the old
	// allowFutureForks=false semantics wrongly inherited these three.
	private := &params.ChainConfig{ByzantiumBlock: big.NewInt(0)}
	patchMissingForkBlocks(private, defaults, "test-private", forkPatchGenesisAuthoritative)
	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", private.ConstantinopleBlock},
		{"BerlinBlock", private.BerlinBlock},
		{"LondonBlock", private.LondonBlock},
		{"Eip1559Block", private.Eip1559Block},
		{"CancunBlock", private.CancunBlock},
	} {
		if tc.block != nil {
			t.Errorf("private (byzantium ceiling): %s want nil (above ceiling, genesis is authoritative), got %v", tc.name, tc.block)
		}
	}
	if private.ByzantiumBlock == nil || private.ByzantiumBlock.Sign() != 0 {
		t.Errorf("private: ByzantiumBlock want 0 (declared, untouched), got %v", private.ByzantiumBlock)
	}

	// Genesis-authoritative mode, genesis declares up to London (skipping
	// Constantinople/Petersburg/Istanbul/Berlin in between): the ceiling is
	// londonBlock, so those four skipped, at-or-below-ceiling fields (default
	// block 0) ARE inherited (R4), clamped via R5 so ordering can't invert,
	// while the still-future-dated Eip1559/Cancun (ABOVE londonBlock in the
	// ladder) are refused.
	privateLondon := &params.ChainConfig{
		HomesteadBlock: big.NewInt(0),
		EIP150Block:    big.NewInt(0),
		EIP155Block:    big.NewInt(0),
		EIP158Block:    big.NewInt(0),
		ByzantiumBlock: big.NewInt(0),
		LondonBlock:    big.NewInt(0),
	}
	patchMissingForkBlocks(privateLondon, defaults, "test-private-london-ceiling", forkPatchGenesisAuthoritative)
	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", privateLondon.ConstantinopleBlock},
		{"PetersburgBlock", privateLondon.PetersburgBlock},
		{"IstanbulBlock", privateLondon.IstanbulBlock},
		{"BerlinBlock", privateLondon.BerlinBlock},
	} {
		if tc.block == nil || tc.block.Sign() != 0 {
			t.Errorf("private (london ceiling): %s want 0 (at-or-below ceiling, active at genesis), got %v", tc.name, tc.block)
		}
	}
	if err := privateLondon.CheckConfigForkOrder(); err != nil {
		t.Errorf("private (london ceiling): CheckConfigForkOrder failed: %v", err)
	}
	if privateLondon.Eip1559Block != nil {
		t.Errorf("private (london ceiling): Eip1559Block want nil (above ceiling), got %v", privateLondon.Eip1559Block)
	}
	if privateLondon.CancunBlock != nil {
		t.Errorf("private (london ceiling): CancunBlock want nil (above ceiling), got %v", privateLondon.CancunBlock)
	}

	// Explicit values are never overwritten in either mode.
	explicit := &params.ChainConfig{ByzantiumBlock: big.NewInt(0), Eip1559Block: big.NewInt(99)}
	patchMissingForkBlocks(explicit, defaults, "test-explicit", forkPatchGenesisAuthoritative)
	if explicit.Eip1559Block.Int64() != 99 {
		t.Errorf("explicit: Eip1559Block want 99 (untouched), got %v", explicit.Eip1559Block)
	}
}

// netV12Genesis returns a Genesis shaped verbatim after the live netv12
// deployment (chainId 34093, 5-validator legacy XDPoSChain net, V2 switch at
// 1800): the genesis JSON declares only ChainID plus the base EVM ladder up
// to Byzantium (all at block 0) and XDPoS — nothing above Byzantium, no V2
// block. This is the exact shape that exposed the geth-follower-never-mints
// defect (ethOne#62).
func netV12Genesis() *Genesis {
	return &Genesis{
		Config: &params.ChainConfig{
			ChainID:        big.NewInt(34093),
			HomesteadBlock: big.NewInt(0),
			EIP150Block:    big.NewInt(0),
			EIP155Block:    big.NewInt(0),
			EIP158Block:    big.NewInt(0),
			ByzantiumBlock: big.NewInt(0),
			XDPoS: &params.XDPoSConfig{
				Period:              2,
				Epoch:               900,
				Reward:              5000,
				RewardCheckpoint:    900,
				Gap:                 450,
				FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000001"),
				V2: &params.V2{
					SwitchBlock: big.NewInt(1800),
					SwitchEpoch: 2,
				},
			},
		},
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x0000000000000000000000000000000000000001"): {
				Balance: big.NewInt(1e18),
			},
		},
	}
}

// TestNetV12GenesisAuthoritativeByzantiumOnly is test A1 from the ethOne#62
// fix-design verification plan: netv12's exact genesis shape, resolved twice
// through SetupGenesisBlock (init, then reopen — the reopen is the path that,
// before this fix, injected Constantinople/Petersburg/Istanbul/Berlin/London
// at block 0). All five must come out nil; ByzantiumBlock stays 0; the V2
// fill must still happen; CheckConfigForkOrder and CheckCompatible must both
// be clean.
func TestNetV12GenesisAuthoritativeByzantiumOnly(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	g := netV12Genesis()

	if _, _, _, err := SetupGenesisBlock(db, tdb, g); err != nil {
		t.Fatalf("first SetupGenesisBlock (init) failed: %v", err)
	}
	cfg, _, compatErr, err := SetupGenesisBlock(db, tdb, g)
	if err != nil {
		t.Fatalf("second SetupGenesisBlock (re-init) failed: %v", err)
	}
	if compatErr != nil {
		t.Fatalf("SetupGenesisBlock returned compat error: %v", compatErr)
	}

	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", cfg.ConstantinopleBlock},
		{"PetersburgBlock", cfg.PetersburgBlock},
		{"IstanbulBlock", cfg.IstanbulBlock},
		{"MuirGlacierBlock", cfg.MuirGlacierBlock},
		{"BerlinBlock", cfg.BerlinBlock},
		{"LondonBlock", cfg.LondonBlock},
		{"Eip1559Block", cfg.Eip1559Block},
		{"CancunBlock", cfg.CancunBlock},
		{"PragueBlock", cfg.PragueBlock},
		{"TIPUpgradeRewardBlock", cfg.TIPUpgradeRewardBlock},
		{"TIPUpgradePenaltyBlock", cfg.TIPUpgradePenaltyBlock},
	} {
		if tc.block != nil {
			t.Errorf("%s: want nil (above the byzantium ceiling, must not be inherited), got %v", tc.name, tc.block)
		}
	}
	if cfg.ByzantiumBlock == nil || cfg.ByzantiumBlock.Sign() != 0 {
		t.Errorf("ByzantiumBlock: want 0 (declared, untouched), got %v", cfg.ByzantiumBlock)
	}
	if err := cfg.CheckConfigForkOrder(); err != nil {
		t.Errorf("CheckConfigForkOrder failed: %v", err)
	}
	if cfg.XDPoS == nil || cfg.XDPoS.V2 == nil {
		t.Fatal("XDPoS.V2 is nil after resolution")
	}
	if cfg.XDPoS.V2.SwitchBlock == nil || cfg.XDPoS.V2.SwitchBlock.Int64() != 1800 {
		t.Errorf("XDPoS.V2.SwitchBlock: want 1800, got %v", cfg.XDPoS.V2.SwitchBlock)
	}
	if cfg.XDPoS.V2.AllConfigs == nil {
		t.Fatal("XDPoS.V2.AllConfigs is nil — V2 fill did not fire")
	}
}

// TestNetV12RulesAtBlock16 is test A2 from the verification plan: it pins the
// exact repricing cause on the netv12 genesis shape. IsIstanbul/IsPetersburg
// OR in the package-level common.TIPXDCXCancellationFee sentinel, so this
// calls common.CopyXDCConstants(34093) first (restored via t.Cleanup) — the
// unregistered-chainId default is 38383838, which is what makes Istanbul
// genuinely off at block 16 after the fix; a test that skipped this call
// could pass for the wrong reason depending on package test-run order.
func TestNetV12RulesAtBlock16(t *testing.T) {
	origCancellationFee := common.TIPXDCXCancellationFee
	t.Cleanup(func() { common.TIPXDCXCancellationFee = origCancellationFee })
	common.CopyXDCConstants(34093)

	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	g := netV12Genesis()
	if _, _, _, err := SetupGenesisBlock(db, tdb, g); err != nil {
		t.Fatalf("first SetupGenesisBlock (init) failed: %v", err)
	}
	cfg, _, _, err := SetupGenesisBlock(db, tdb, g)
	if err != nil {
		t.Fatalf("second SetupGenesisBlock (re-init) failed: %v", err)
	}

	rules := cfg.Rules(big.NewInt(16), false, 0)
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"IsByzantium", rules.IsByzantium, true},
		{"IsConstantinople", rules.IsConstantinople, false},
		{"IsPetersburg", rules.IsPetersburg, false},
		{"IsIstanbul", rules.IsIstanbul, false},
		{"IsBerlin", rules.IsBerlin, false},
		{"IsLondon", rules.IsLondon, false},
		{"IsEIP1559", rules.IsEIP1559, false},
		{"IsEIP2929", rules.IsEIP2929, false},
		{"IsEIP3860", rules.IsEIP3860, false},
		{"IsMerge", rules.IsMerge, false},
		{"IsShanghai", rules.IsShanghai, false},
		{"IsCancun", rules.IsCancun, false},
		{"IsPrague", rules.IsPrague, false},
	} {
		if tc.got != tc.want {
			t.Errorf("Rules(16).%s: want %v, got %v", tc.name, tc.want, tc.got)
		}
	}
}

// TestXDCPrivateChainSilentGenesisBaseline is test A5: a genesis that declares
// NOTHING from the fork ladder at all (only ChainID + XDPoS) has no ceiling,
// so genesis-authoritative mode reduces to today's documented baseline —
// every already-active (block 0) default is inherited, and the still
// future-dated Eip1559/Cancun are refused exactly as before. This is the
// "genesis declares no fork schedule at all" class from the fix design (R3).
func TestXDCPrivateChainSilentGenesisBaseline(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(987656)}
	patchMissingForkBlocks(cfg, params.XDCDefaultPrivateChainConfig, "test-silent-genesis", forkPatchGenesisAuthoritative)

	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", cfg.ConstantinopleBlock},
		{"PetersburgBlock", cfg.PetersburgBlock},
		{"IstanbulBlock", cfg.IstanbulBlock},
		{"BerlinBlock", cfg.BerlinBlock},
		{"LondonBlock", cfg.LondonBlock},
	} {
		if tc.block == nil || tc.block.Sign() != 0 {
			t.Errorf("%s: want 0 (silent genesis baseline — no ceiling, block-0 defaults inherited), got %v", tc.name, tc.block)
		}
	}
	if cfg.Eip1559Block != nil {
		t.Errorf("Eip1559Block: want nil (future-dated, still refused with no ceiling), got %v", cfg.Eip1559Block)
	}
	if cfg.CancunBlock != nil {
		t.Errorf("CancunBlock: want nil (future-dated, still refused with no ceiling), got %v", cfg.CancunBlock)
	}
}

// TestXDCPrivateChainPartialLadderKeepsForkOrder is test A6: a genesis that
// declares byzantiumBlock=4 and (as an XDC block-gated extra) cancunBlock=3150
// but leaves berlin/london nil must not end up with berlin=london=0 — that
// would violate CheckConfigForkOrder ("byzantiumBlock enabled at block 4, but
// berlinBlock enabled at block 0"). The R5 ordering clamp pins them up to the
// previous resolved non-optional fork (byzantiumBlock=4) instead.
func TestXDCPrivateChainPartialLadderKeepsForkOrder(t *testing.T) {
	cfg := &params.ChainConfig{
		ChainID:        big.NewInt(987657),
		HomesteadBlock: big.NewInt(0),
		EIP150Block:    big.NewInt(0),
		EIP155Block:    big.NewInt(0),
		EIP158Block:    big.NewInt(0),
		ByzantiumBlock: big.NewInt(4),
		CancunBlock:    big.NewInt(3150),
	}
	patchMissingForkBlocks(cfg, params.XDCDefaultPrivateChainConfig, "test-partial-ladder", forkPatchGenesisAuthoritative)

	if err := cfg.CheckConfigForkOrder(); err != nil {
		t.Fatalf("CheckConfigForkOrder failed: %v", err)
	}
	for _, tc := range []struct {
		name  string
		block *big.Int
	}{
		{"ConstantinopleBlock", cfg.ConstantinopleBlock},
		{"PetersburgBlock", cfg.PetersburgBlock},
		{"IstanbulBlock", cfg.IstanbulBlock},
		{"BerlinBlock", cfg.BerlinBlock},
		{"LondonBlock", cfg.LondonBlock},
	} {
		if tc.block == nil || tc.block.Int64() != 4 {
			t.Errorf("%s: want 4 (clamped up to the declared byzantiumBlock, never 0), got %v", tc.name, tc.block)
		}
	}
	if cfg.CancunBlock == nil || cfg.CancunBlock.Int64() != 3150 {
		t.Errorf("CancunBlock: want 3150 (declared, untouched), got %v", cfg.CancunBlock)
	}
}

// cloneBig returns a copy of x, or nil if x is nil.
func cloneBig(x *big.Int) *big.Int {
	if x == nil {
		return nil
	}
	return new(big.Int).Set(x)
}

// TestRegisteredNetsForkResolutionUnchanged is test A7: for every registered
// network (mainnet/50, Apothem/51, devnet/5551), a config that declares the
// base EVM ladder up to Byzantium but omits every later fork block (exactly
// what the embedded genesis JSONs do) must resolve those later fields to
// EXACTLY the hardcoded per-network default — future-dated forks included.
// forkPatchRegistered must behave byte-for-byte as it did before this change.
func TestRegisteredNetsForkResolutionUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		defaults *params.ChainConfig
	}{
		{"mainnet", params.XDCMainnetChainConfig},
		{"apothem", params.XDCApothemChainConfig},
		{"devnet", params.XDCDevnetChainConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &params.ChainConfig{
				ChainID:        cloneBig(tc.defaults.ChainID),
				HomesteadBlock: cloneBig(tc.defaults.HomesteadBlock),
				DAOForkBlock:   cloneBig(tc.defaults.DAOForkBlock),
				EIP150Block:    cloneBig(tc.defaults.EIP150Block),
				EIP155Block:    cloneBig(tc.defaults.EIP155Block),
				EIP158Block:    cloneBig(tc.defaults.EIP158Block),
				ByzantiumBlock: cloneBig(tc.defaults.ByzantiumBlock),
				// Constantinople..TIPUpgradePenaltyBlock intentionally left nil,
				// exactly as the embedded genesis JSON omits them today.
			}
			patchMissingForkBlocks(cfg, tc.defaults, tc.name, forkPatchRegistered)

			check := func(field string, got, want *big.Int) {
				if (got == nil) != (want == nil) {
					t.Errorf("%s: %s = %v, want %v", tc.name, field, got, want)
					return
				}
				if got != nil && got.Cmp(want) != 0 {
					t.Errorf("%s: %s = %v, want %v", tc.name, field, got, want)
				}
			}
			check("ConstantinopleBlock", cfg.ConstantinopleBlock, tc.defaults.ConstantinopleBlock)
			check("PetersburgBlock", cfg.PetersburgBlock, tc.defaults.PetersburgBlock)
			check("IstanbulBlock", cfg.IstanbulBlock, tc.defaults.IstanbulBlock)
			check("BerlinBlock", cfg.BerlinBlock, tc.defaults.BerlinBlock)
			check("LondonBlock", cfg.LondonBlock, tc.defaults.LondonBlock)
			check("Eip1559Block", cfg.Eip1559Block, tc.defaults.Eip1559Block)
			check("CancunBlock", cfg.CancunBlock, tc.defaults.CancunBlock)
			check("PragueBlock", cfg.PragueBlock, tc.defaults.PragueBlock)
			check("TIPUpgradeRewardBlock", cfg.TIPUpgradeRewardBlock, tc.defaults.TIPUpgradeRewardBlock)
			check("TIPUpgradePenaltyBlock", cfg.TIPUpgradePenaltyBlock, tc.defaults.TIPUpgradePenaltyBlock)

			if err := cfg.CheckConfigForkOrder(); err != nil {
				t.Errorf("CheckConfigForkOrder failed: %v", err)
			}
		})
	}
}
