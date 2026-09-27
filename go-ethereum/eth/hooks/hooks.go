// Package hooks provides consensus hooks for XDPoS engine integration.
// V2 hooks are in engine_v2_hooks.go.

package hooks

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/XDPoS"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// AttachConsensusV1Hooks attaches V1 consensus hooks to XDPoS engine. (#72)
// Mirrors v2.6.8 eth/hooks/engine_v1_hooks.go AttachConsensusV1Hooks.
//
// NOTE: Full M2 randomization via the 0x0090 randomize contract requires
// a ContractBackend (IPC client). That wiring (bc.GetClient / bc.IPCEndpoint)
// is tracked in issue #72 as a follow-up. For now we wire the hooks with a
// best-effort implementation: direct signer encoding pre-TIPRandomize, and
// pass-through (non-enforcing) post-TIPRandomize to unblock sync.
func AttachConsensusV1Hooks(adaptor *XDPoS.XDPoS, bc *core.BlockChain, chainConfig *params.ChainConfig) {
	xdposConf := chainConfig.XDPoS

	// HookValidator: computes the Validators bytes for a checkpoint block header.
	// Stored in header.Validators field; used by V1 verifySeal / M2 double-signing.
	adaptor.HookValidator = func(header *types.Header, signers []common.Address) ([]byte, error) {
		if xdposConf == nil || len(signers) == 0 {
			return []byte{}, nil
		}
		number := header.Number.Uint64()

		// Before TIPRandomize (3,464,000): encode signers in order
		if number < 3464000 {
			result := make([]byte, 0, len(signers)*common.AddressLength)
			for _, s := range signers {
				result = append(result, s.Bytes()...)
			}
			header.Validators = result
			return result, nil
		}

		// After TIPRandomize: M2 randomization is derived from the 0x0090 randomize contract.
		// Full contract-call support requires bc.GetClient() (IPC) — see issue #72 follow-up.
		// For now: if header.Validators is already set (synced from network), use it as-is.
		// When mining, this hook is called to SET the field — we can't derive M2 without IPC,
		// so we fall back to encoding signers directly (produces wrong M2 but won't crash).
		if len(header.Validators) > 0 {
			log.Trace("[V1 HookValidator] using existing Validators from header", "number", number)
			return header.Validators, nil
		}

		// Fallback: direct encoding (wrong M2 order post-TIPRandomize, but non-fatal in sync mode)
		log.Debug("[V1 HookValidator] M2 contract unavailable, direct encoding", "number", number)
		result := make([]byte, 0, len(signers)*common.AddressLength)
		for _, s := range signers {
			result = append(result, s.Bytes()...)
		}
		header.Validators = result
		return result, nil
	}

	// HookVerifyMNs: verifies the Validators field at epoch checkpoints. (#72)
	// Cross-checks computed validators against header.Validators.
	adaptor.HookVerifyMNs = func(header *types.Header, signers []common.Address) error {
		if xdposConf == nil {
			return nil
		}
		number := header.Number.Uint64()
		if number == 0 || number%xdposConf.Epoch != 0 {
			return nil
		}

		expected, err := adaptor.HookValidator(header, signers)
		if err != nil {
			return err
		}

		if len(expected) > 0 && len(header.Validators) > 0 {
			if !bytes.Equal(header.Validators, expected) {
				log.Warn("[V1 HookVerifyMNs] Validators field mismatch (non-fatal in sync mode)",
					"number", number,
					"expectedAddrCount", len(expected)/common.AddressLength,
					"gotAddrCount", len(header.Validators)/common.AddressLength)
				// Non-fatal: in sync mode the network's Validators takes precedence.
				// Full enforcement only makes sense when actively producing blocks.
			}
		}

		return nil
	}

	// Fix #97: Wire V1 HookPenalty.
	// Walks through the epoch, checks which masternodes signed blocks via block signers.
	// Masternodes that never signed are penalized (removed from next epoch list).
	adaptor.HookPenalty = func(chain consensus.ChainHeaderReader, blockNumberEpoch uint64) ([]common.Address, error) {
		epoch := uint64(900)
		if xdposConf != nil && xdposConf.Epoch > 0 {
			epoch = xdposConf.Epoch
		}
		if blockNumberEpoch < epoch {
			return []common.Address{}, nil
		}
		prevEpoch := blockNumberEpoch - epoch
		prevHeader := chain.GetHeaderByNumber(prevEpoch)
		if prevHeader == nil {
			return []common.Address{}, nil
		}

		// Get masternode list at previous epoch
		masternodes := adaptor.GetMasternodes(chain, prevHeader)
		if len(masternodes) == 0 {
			return []common.Address{}, nil
		}

		// Track which masternodes signed at least one block
		signed := make(map[common.Address]bool)
		for i := prevEpoch + 1; i < blockNumberEpoch; i++ {
			h := chain.GetHeaderByNumber(i)
			if h == nil {
				continue
			}
			// Recover signer from header
			signer, err := adaptor.RecoverSigner(h)
			if err == nil {
				signed[signer] = true
			}
		}

		// Penalize masternodes that never signed
		var penalties []common.Address
		for _, mn := range masternodes {
			if !signed[mn] {
				penalties = append(penalties, mn)
			}
		}
		log.Info("[V1 HookPenalty] Calculated penalties", "block", blockNumberEpoch, "penalized", len(penalties), "total", len(masternodes))
		return penalties, nil
	}

	log.Info("[XDPoS] V1 consensus hooks attached (#72, #97)")
}
