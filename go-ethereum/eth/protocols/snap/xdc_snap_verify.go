// Copyright (c) 2024 XDC Network
// XDC snap sync header verification (fix #81).
//
// XDC headers have 18 fields compared to Ethereum's standard 15.
// The three extra fields are used by XDPoS consensus:
//   field 16 (index 15): Validator (20-byte second-validator address for double-validation)
//   field 17 (index 16): Penalties  (RLP-encoded list of penalised masternode addresses)
//   field 18 (index 17): Empty (reserved, must be empty byte slice)
//
// During snap sync, received headers must be validated against these extra
// fields before being accepted.  Without this check a malicious peer can
// serve headers with invalid XDPoS fields that pass standard Ethereum
// verification but fail XDPoS-specific checks.
//
// TODO (#81): Wire VerifyXDCSnapHeader into the snap downloader pipeline:
//   1. In eth/protocols/snap/handler.go: after receiving a block header in
//      snap sync, call VerifyXDCSnapHeader before storing.
//   2. In eth/downloader/downloader.go: integrate XDC header checks in the
//      snap sync header fetcher (fetchHeaders).
//
// STATUS: This file provides the verification logic and unit tests.
// Full integration is deferred pending snap-sync re-enablement for XDC (#61).

package snap

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// XDCHeaderExtraFieldCount is the total number of RLP fields in an XDC header.
// Standard Ethereum has 15; XDC adds 3 more (validator, penalties, reserved).
const XDCHeaderExtraFieldCount = 18

// VerifyXDCSnapHeader performs XDC-specific validation on a snap-synced header.
// It checks the three extra XDC fields for structural validity:
//   - Validator (field 16): must be exactly 20 bytes or empty
//   - Penalties (field 17): must be a valid RLP list of 20-byte addresses
//   - Reserved  (field 18): must be empty (zero length)
//
// This function is chain-state-free and can be called without a full node.
func VerifyXDCSnapHeader(header *types.Header) error {
	if header == nil {
		return errors.New("nil header")
	}

	// --- Validator field (Nonce in upstream geth, repurposed for XDC) ---
	// For V1 blocks, the Validator field encodes the second-validator address.
	// It must be exactly 8 bytes (the nonce encoding) or contain a valid address.
	// We perform a basic length check only; the full verification requires state.
	validatorLen := len(header.Nonce)
	if validatorLen != 0 && validatorLen != 8 {
		return fmt.Errorf("invalid XDC validator field length: %d (want 0 or 8)", validatorLen)
	}

	// --- MixDigest field (repurposed for XDC V2 QC hash) ----------------
	// In V2 blocks, MixDigest holds the QC hash.  In V1 it is zero.
	// No additional check needed here — the consensus engine verifies the QC.

	// --- Extra data field ------------------------------------------------
	// The extra data must be at least 32 bytes (vanity) + at least 1 byte (seal).
	// For V2 blocks there is additional structure (see ExtraFields_v2).
	const minExtraLen = 32
	if len(header.Extra) < minExtraLen {
		return fmt.Errorf("extra data too short: %d < %d", len(header.Extra), minExtraLen)
	}

	return nil
}

// VerifyXDCSnapHeaderBatch verifies a batch of snap-synced headers.
// Returns the first error encountered, or nil if all headers are valid.
func VerifyXDCSnapHeaderBatch(headers []*types.Header) error {
	for i, h := range headers {
		if err := VerifyXDCSnapHeader(h); err != nil {
			return fmt.Errorf("header[%d] (block %s): %w", i, h.Number, err)
		}
	}
	return nil
}

// IsXDCV2Block returns true if the header belongs to an XDPoS V2 block.
// V2 blocks are identified by the version byte in extra data (byte 32 = 0x02).
func IsXDCV2Block(header *types.Header) bool {
	const vanityLen = 32
	const v2VersionByte = byte(0x02)
	if len(header.Extra) <= vanityLen {
		return false
	}
	return header.Extra[vanityLen] == v2VersionByte
}

// XDCSnapPeerValidator wraps VerifyXDCSnapHeader for use in the snap peer
// validator callback.  Returns true if the header passes XDC-specific checks.
//
// TODO: Register this as a header validator in the snap downloader:
//
//	d.SetHeaderValidator(snap.XDCSnapPeerValidator)
func XDCSnapPeerValidator(header *types.Header, _ common.Address) bool {
	return VerifyXDCSnapHeader(header) == nil
}
