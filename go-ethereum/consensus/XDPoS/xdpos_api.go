// Copyright 2024 XDC Network
// XDPoS dispatcher methods ported from v2.6.8
// Fixes: https://github.com/AnilChinchawale/go-ethereum/issues/38

package XDPoS

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// CoSignM2Header implements the V1 M2 double-validation co-sign (#951 Defect 1c, stage B2).
// When this node's signing key is the assigned M2 for a freshly-received V1 block whose
// header.Validator is empty, it signs sigHash(header) with the SAME signFn/digest the M1
// used for the Extra seal and fills header.Validator, returning a rebuilt block (new
// canonical hash). Byte-faithful port of XDPoSChain appendM2HeaderHook (eth/backend.go:288-318)
// + the gate from engine_v1/engine.go:164-167. Returns (block,false,nil) as a no-op when this
// node is not the assigned M2 / not a miner / block is V2 / first-epoch / already co-signed.
//
// Signing parity: c.signFn is bound (eth/backend.go) to ks.SignHash, which signs the RAW
// 32-byte digest (no mimetype re-hash), so the signature is byte-identical to the M1 Extra
// seal (xdpos.go Seal) and to what RecoverValidator recovers. Must NOT use wallet.SignData,
// which keccak-re-hashes and would yield a non-matching M2 (ErrFailedDoubleValidation).
func (c *XDPoS) CoSignM2Header(chain consensus.ChainHeaderReader, block *types.Header) (*types.Header, bool, error) {
	header := block
	// V1 only — V2 uses header.Validators/QC, not the single-sig header.Validator.
	if c.IsV2Block(header) {
		return nil, false, nil
	}
	// Number>Epoch (first-epoch blocks have no assigned M2) and empty Validator only.
	if header.Number.Uint64() <= c.config.Epoch || len(header.Validator) != 0 {
		return nil, false, nil
	}
	c.lock.RLock()
	signer, signFn := c.signer, c.signFn
	c.lock.RUnlock()
	// Non-validator node (no key authorized) → pure import, never co-sign.
	if signFn == nil || signer == (common.Address{}) {
		return nil, false, nil
	}
	// M1 creator from the Extra seal (NOT Coinbase, NOT Author which dispatches V2).
	creator, err := c.RecoverSigner(header)
	if err != nil {
		return nil, false, err
	}
	m2, err := c.GetValidator(creator, chain, header)
	if err != nil {
		return nil, false, err // snapshot not ready near epoch boundary — defer, no peer drop
	}
	if m2 != signer {
		return nil, false, nil // not our slot — the real M2 node co-signs
	}
	sighash, err := signFn(accounts.Account{Address: signer}, accounts.MimetypeClique, sigHash(header).Bytes())
	if err != nil {
		return nil, false, err
	}
	if len(sighash) != extraSeal {
		return nil, false, errFailValidatorSignature
	}
	h2 := types.CopyHeader(header)
	h2.Validator = sighash
	// Self-check: the recovered M2 over the rebuilt header must equal our signer
	// (catches any digest/encoding drift before the co-signed block is gossiped).
	if rec, rerr := c.RecoverValidator(h2); rerr != nil || rec != signer {
		return nil, false, errFailValidatorSignature
	}
	return h2, true, nil
}

// GetValidator returns the assigned M2 validator for a given creator (M1), used
// by V1 double validation. Byte-exact port of XDPoSChain engine_v1/engine.go
// GetValidator: resolve the previous checkpoint header, build the M1→M2 map via
// getM1M2FromCheckpointHeader (checkpoint masternodes + M2 indices + moveM2
// rotation), and return m[creator]. The earlier (i+1)%N round-robin was NOT
// parity-correct — the real M2 is a randomized permutation, not the next signer.
// Refs #951 Defect 1c.
func (c *XDPoS) GetValidator(creator common.Address, chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	epoch := c.config.Epoch
	no := header.Number.Uint64()
	cpNo := no
	if no%epoch != 0 {
		cpNo = no - (no % epoch)
	}
	if cpNo == 0 {
		return common.Address{}, nil
	}
	cpHeader := chain.GetHeaderByNumber(cpNo)
	if cpHeader == nil {
		if no%epoch == 0 {
			cpHeader = header
		} else {
			return common.Address{}, errors.New("xdpos v1: couldn't find checkpoint header for M2 lookup")
		}
	}
	m, err := getM1M2FromCheckpointHeader(cpHeader, header, chain.Config())
	if err != nil {
		return common.Address{}, err
	}
	return m[creator], nil
}

// IsAuthorisedAddress checks if an address is authorised to sign blocks.
func (c *XDPoS) IsAuthorisedAddress(chain consensus.ChainHeaderReader, header *types.Header, address common.Address) bool {
	masternodes := c.GetMasternodes(chain, header)
	for _, mn := range masternodes {
		if mn == address {
			return true
		}
	}
	return false
}

// IsEpochSwitch checks if the header is an epoch switch block.
//
// For V2 blocks, this MUST delegate to EngineV2.IsEpochSwitch (round-based),
// not the block-number modulo fallback. V2 epochs advance by *round*, and with
// timeout rounds the epoch switch block is not at a fixed multiple of Epoch:
// e.g., on devnet round-1800 epoch start landed on block 4435 (not 4500). The
// V1-style fallback would mis-identify block 4500 as the switch — every caller
// that walks via this function (GetSigningTxCount, etc.) would then scan the
// wrong window and produce different totalSigner / different rewards / a
// diverging state root at epoch-switch reward blocks (e.g. block 5334).
// Canonical (XDPoSChain XDPoS.go:459) does the same delegation.
func (c *XDPoS) IsEpochSwitch(header *types.Header) (bool, uint64, error) {
	number := header.Number.Uint64()
	if c.config.V2 != nil && c.config.V2.SwitchBlock != nil && number > c.config.V2.SwitchBlock.Uint64() && c.EngineV2 != nil {
		return c.EngineV2.IsEpochSwitch(header)
	}
	// V1 epoch switch (block-number modulo)
	return number%c.config.Epoch == 0, number / c.config.Epoch, nil
}

// GetCurrentEpochSwitchBlock returns the epoch switch block for the epoch containing blockNum.
func (c *XDPoS) GetCurrentEpochSwitchBlock(chain consensus.ChainHeaderReader, blockNum *big.Int) (uint64, uint64, error) {
	num := blockNum.Uint64()
	epochNum := num / c.config.Epoch
	switchBlock := epochNum * c.config.Epoch
	return switchBlock, epochNum, nil
}

// GetMasternodesByNumber returns masternodes at a specific block number.
func (c *XDPoS) GetMasternodesByNumber(chain consensus.ChainHeaderReader, blockNumber uint64) []common.Address {
	header := chain.GetHeaderByNumber(blockNumber)
	if header == nil {
		log.Error("[GetMasternodesByNumber] Header not found", "number", blockNumber)
		return nil
	}
	return c.GetMasternodes(chain, header)
}

// UpdateParams updates V2 consensus parameters based on round extracted from header.
// Dispatches to engine_v2.UpdateParams which updates CurrentConfig for the round.
// Matches v2.6.8 XDPoS.go UpdateParams. (#117)
func (c *XDPoS) UpdateParams(header *types.Header) {
	if c.EngineV2 != nil {
		if updater, ok := c.EngineV2.(interface{ UpdateParams(header *types.Header) }); ok {
			updater.UpdateParams(header)
			return
		}
	}
	log.Debug("[UpdateParams] EngineV2 not available or does not support UpdateParams", "block", header.Number)
}

// HandleProposedBlock handles a newly proposed (self-minted) block for V2 BFT.
// A.97-M.A Task 4: delegates to EngineV2.ProposedBlockHandler which drives the
// self-vote path: processQC → allowedToSend → verifyVotingRule → sendVote →
// broadcastToBftChannel. Without this delegation the proposer never emits its
// own vote, so no QC can form and the minted block is never committed.
// Mirrors XDPoSChain: worker.go → HandleProposedBlock → ProposedBlockHandler.
func (c *XDPoS) HandleProposedBlock(chain consensus.ChainHeaderReader, header *types.Header) error {
	if c.EngineV2 == nil {
		log.Debug("[HandleProposedBlock] EngineV2 not wired, skipping", "block", header.Number)
		return nil
	}
	// V1→V2 leak guard (#951): the V2 BFT self-vote path is meaningless for a V1
	// (pre-switch) block, and forwarding a V1 header into EngineV2.ProposedBlockHandler
	// → getExtraFields → GetMasternodesFromEpochSwitchHeader/DecodeExtraFields trips the
	// C14 V1-reject guard and emits spurious "V1 pre-switch header passed to V2 engine"
	// + "not V2 extra format" errors on every V1 block the miner produces or the syncer
	// imports. Legacy (XDPoSChain/consensus/XDPoS/XDPoS.go:299-306) dispatches on
	// BlockConsensusVersion and returns nil for V1 blocks, never touching EngineV2.
	if !c.IsV2Block(header) {
		return nil
	}
	// consensus.ChainHeaderReader satisfies consensus.ChainReader (ChainReader
	// embeds ChainHeaderReader in this codebase). Cast is safe here because all
	// callers pass a *core.BlockChain which implements both interfaces.
	cr, ok := chain.(consensus.ChainReader)
	if !ok {
		log.Warn("[HandleProposedBlock] chain does not implement ChainReader, skipping", "block", header.Number)
		return nil
	}
	return c.EngineV2.ProposedBlockHandler(cr, header)
}

// Initial initializes the consensus engine with the current chain state.
func (c *XDPoS) Initial(chain consensus.ChainHeaderReader, header *types.Header) error {
	log.Info("[Initial] Consensus engine initializing", "block", header.Number)
	return nil
}

// GetAuthorisedSignersFromSnapshot returns authorised signers from the snapshot.
func (c *XDPoS) GetAuthorisedSignersFromSnapshot(chain consensus.ChainHeaderReader, header *types.Header) ([]common.Address, error) {
	snap, err := c.GetSnapshot(chain, header)
	if err != nil {
		return nil, err
	}
	signers := make([]common.Address, 0, len(snap.Signers))
	for addr := range snap.Signers {
		signers = append(signers, addr)
	}
	return signers, nil
}
