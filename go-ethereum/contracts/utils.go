// Copyright (c) 2018 XDPoSChain
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package contracts

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/blocksigner/contract"
	randomizeContract "github.com/ethereum/go-ethereum/contracts/randomize/contract"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

const (
	extraVanity = 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = 65 // Fixed number of extra-data suffix bytes reserved for signer seal
)

// RewardLog represents the reward info for logging
type RewardLog struct {
	Sign   uint64   `json:"sign"`
	Reward *big.Int `json:"reward"`
}

// TxSignMu serialises access to CreateTransactionSign so that concurrent
// sealing attempts cannot race on nonce allocation.
// Mirrors XDPoSChain/contracts/utils.go:61 (var TxSignMu sync.RWMutex).
var TxSignMu sync.RWMutex

// TxPoolAdder is the subset of txpool.TxPool that CreateTransactionSign needs.
// Exposing a narrow interface avoids a circular import between contracts ↔ core/txpool.
type TxPoolAdder interface {
	// PoolNonce returns the next nonce for addr, counting pending transactions.
	PoolNonce(addr common.Address) uint64
	// Add submits transactions to the pool.  sync=false for fire-and-forget.
	Add(txs []*types.Transaction, sync bool) []error
}

// StateNonceReader resolves an account's nonce in the state of the current
// canonical chain head. Implemented by *core.BlockChain via a thin adapter in
// the miner (see miner/xdpos_worker.go); kept as a narrow interface here to
// avoid a contracts -> core import cycle.
type StateNonceReader interface {
	// ConfirmedNonce returns the nonce of addr at the current chain head state
	// and true, or (0, false) if that state is unavailable.
	ConfirmedNonce(addr common.Address) (uint64, bool)
}

// signTxNonce picks the nonce for the next masternode signing tx as
// max(poolNonce, confirmedStateNonce).
//
// The pool nonce alone is not safe. When the txpool's reset() cannot open the
// head state it keeps serving its previous statedb (see the checkpointSyncNoState
// skip in core/txpool/legacypool.reset), so PoolNonce freezes at whatever height
// the pool last reset on. Every subsequent sign tx is then emitted at a nonce far
// below the confirmed chain nonce and can never be mined — the signing path jams
// permanently instead of degrading.
//
// The confirmed chain-state nonce is the authoritative floor: if the chain has
// already moved past the pool's idea of the next nonce, the chain wins. A stale
// pool then degrades to "already known / nonce too low" on a single tx rather
// than jamming forever.
//
// Direct mirror of erigon-xdc node/eth/xdc_sign_tx.go nextSignTxNonce (:193-202)
// and confirmedStateNonce (:206-218).
func signTxNonce(poolNonce uint64, stateNonce uint64, stateKnown bool) uint64 {
	if stateKnown && stateNonce > poolNonce {
		return stateNonce
	}
	return poolNonce
}

// CreateTransactionSign broadcasts the sign(uint256 blockNumber, bytes32 blockHash)
// transaction to the BlockSigners contract (0x0000…0089).  It is called by the
// block-production worker every MergeSignRange (15) blocks after a block is
// successfully inserted.
//
// Canonical reference: XDPoSChain/contracts/utils.go:64–166 (CreateTransactionSign).
// This port covers the V2-relevant signing path only; the V1 randomize-secret/
// opening logic is intentionally excluded because those transactions are no longer
// needed on TIP2019+ chains (TIP2019Block = 0, so all modern chains are post-TIP).
//
// Parity constraints (MUST match XDPoSChain):
//   - selector:  0xe341eaa4  (common.HexSignMethod)
//   - gas:       200 000
//   - value:     0
//   - to:        0x0000000000000000000000000000000000000089
//   - data:      selector ++ uint256(blockNumber) ++ bytes32(blockHash)
//   - cadence:   block.NumberU64() % common.MergeSignRange == 0
//
// stateNonces supplies the confirmed chain-head nonce used as a floor under the
// pool nonce (see signTxNonce). It may be nil, in which case the pool nonce is
// used unchanged.
func CreateTransactionSign(
	chainConfig *params.ChainConfig,
	pool TxPoolAdder,
	manager *accounts.Manager,
	block *types.Block,
	coinbase common.Address,
	stateNonces StateNonceReader,
) error {
	TxSignMu.Lock()
	defer TxSignMu.Unlock()

	if chainConfig.XDPoS == nil {
		return nil
	}

	// Resolve the signing account: prefer the node's coinbase wallet,
	// fall back to the first available wallet/account.
	// Mirrors XDPoSChain/contracts/utils.go:69-85.
	var (
		account accounts.Account
		wallet  accounts.Wallet
	)
	coinbaseAccount := accounts.Account{Address: coinbase}
	if wallets := manager.Wallets(); len(wallets) > 0 {
		if w, err := manager.Find(coinbaseAccount); err == nil && w != nil {
			wallet = w
			account = coinbaseAccount
		} else {
			wallet = wallets[0]
			if accts := wallet.Accounts(); len(accts) > 0 {
				account = accts[0]
			}
		}
	}
	if wallet == nil {
		log.Warn("contracts: no wallet available for masternode signing tx", "coinbase", coinbase)
		return errors.New("no wallet available for masternode signing tx")
	}

	// Use PoolNonce so we don't collide with in-flight pending transactions.
	// Mirrors XDPoSChain's pool.Nonce(account.Address) which also reflects
	// the pending queue on their version of the pool.
	poolNonce := pool.PoolNonce(account.Address)

	// Floor the pool nonce at the confirmed chain-head nonce. A txpool whose
	// reset() could not open the head state keeps serving a stale statedb, so
	// PoolNonce alone can freeze thousands of blocks behind the chain and every
	// sign tx after that is unmineable. Mirrors erigon-xdc's nextSignTxNonce.
	//
	// This runs under TxSignMu. The read must never block or error out the
	// signing path: an unavailable state is simply "no floor", logged at Debug.
	var (
		stateNonce uint64
		stateKnown bool
	)
	if stateNonces != nil {
		stateNonce, stateKnown = stateNonces.ConfirmedNonce(account.Address)
	}
	nonce := signTxNonce(poolNonce, stateNonce, stateKnown)
	if !stateKnown {
		log.Debug("contracts: no confirmed chain-head state for sign-tx nonce floor, using pool nonce",
			"from", account.Address, "poolNonce", poolNonce)
	} else if nonce != poolNonce {
		log.Warn("contracts: sign-tx nonce raised to confirmed chain nonce — txpool state is stale",
			"from", account.Address, "poolNonce", poolNonce, "stateNonce", stateNonce)
	}

	tx := CreateTxSign(block.Number(), block.Hash(), nonce, common.BlockSignersBinary)
	txSigned, err := wallet.SignTx(account, tx, chainConfig.ChainID)
	if err != nil {
		log.Error("contracts: failed to sign masternode signing tx", "error", err)
		return err
	}

	// Submit to the local txpool (fire-and-forget, no sync wait).
	errs := pool.Add([]*types.Transaction{txSigned}, false)
	if errs[0] != nil {
		log.Error("contracts: failed to add masternode signing tx to pool",
			"error", errs[0],
			"number", block.NumberU64(),
			"hash", block.Hash().Hex(),
			"from", account.Address,
			"nonce", nonce,
		)
		return errs[0]
	}

	log.Info("contracts: masternode signing tx submitted",
		"number", block.NumberU64(),
		"hash", block.Hash().Hex(),
		"from", account.Address,
		"nonce", nonce,
	)
	return nil
}

// CreateTxSign builds the masternode block-signing transaction sent to the
// BlockSigners contract (0x...0089) at every-15-block cadence.
//
// gasPrice is intentionally zero — this is a special XDC system transaction
// that is exempt from all gas-price validation in the txpool and EVM preCheck.
// Canonical reference: XDPoSChain/contracts/utils.go:173
//
//	tx := types.NewTransaction(nonce, blockSigner, big.NewInt(0), 200000, big.NewInt(0), inputData)
//
// The 5th argument (gasPrice) must remain big.NewInt(0). Raising it to any
// non-zero value would (a) diverge from canonical on-chain behaviour and
// (b) cost real XDC per signing tx — masternodes sign every 15 blocks at
// ~2s/block = once per 30 s, so 25 Gwei gasPrice × 200 000 gas = 5 000 Gwei
// per sign tx = ~263 000 sign txs per year at unexpected cost.
func CreateTxSign(blockNumber *big.Int, blockHash common.Hash, nonce uint64, blockSigner common.Address) *types.Transaction {
	data := common.Hex2Bytes(common.HexSignMethod)
	inputData := append(data, common.LeftPadBytes(blockNumber.Bytes(), 32)...)
	inputData = append(inputData, common.LeftPadBytes(blockHash.Bytes(), 32)...)
	// gasPrice = 0: special-tx exemption handles pool admission without payment.
	// XDPoSChain/contracts/utils.go:173 — gasPrice big.NewInt(0).
	tx := types.NewTransaction(nonce, blockSigner, big.NewInt(0), 200000, big.NewInt(0), inputData)

	return tx
}

// Send secret key into randomize smartcontract.
func BuildTxSecretRandomize(nonce uint64, randomizeAddr common.Address, epocNumber uint64, randomizeKey []byte) (*types.Transaction, error) {
	data := common.Hex2Bytes(common.HexSetSecret)
	rand.Seed(time.Now().UnixNano())
	secretNumb := rand.Intn(int(epocNumber))

	// Append randomize suffix in -1, 0, 1.
	secrets := []int64{int64(secretNumb)}
	sizeOfArray := int64(32)

	// Build extra data for tx with first position is size of array byte and second position are length of array byte.
	arrSizeOfSecrets := common.LeftPadBytes(new(big.Int).SetInt64(sizeOfArray).Bytes(), 32)
	arrLengthOfSecrets := common.LeftPadBytes(new(big.Int).SetInt64(int64(len(secrets))).Bytes(), 32)
	inputData := append(data, arrSizeOfSecrets...)
	inputData = append(inputData, arrLengthOfSecrets...)
	for _, secret := range secrets {
		encryptSecret := Encrypt(randomizeKey, new(big.Int).SetInt64(secret).String())
		inputData = append(inputData, common.LeftPadBytes([]byte(encryptSecret), int(sizeOfArray))...)
	}
	tx := types.NewTransaction(nonce, randomizeAddr, big.NewInt(0), 200000, big.NewInt(0), inputData)

	return tx, nil
}

// Send opening key into randomize smartcontract.
func BuildTxOpeningRandomize(nonce uint64, randomizeAddr common.Address, randomizeKey []byte) (*types.Transaction, error) {
	data := common.Hex2Bytes(common.HexSetOpening)
	inputData := append(data, randomizeKey...)
	tx := types.NewTransaction(nonce, randomizeAddr, big.NewInt(0), 200000, big.NewInt(0), inputData)

	return tx, nil
}

// RandStringByte generates a random string of specified length
func RandStringByte(n int) []byte {
	letterBytes := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ123456789"
	b := make([]byte, n)
	for i := range b {
		rand.Seed(time.Now().UnixNano())
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return b
}

// Encrypt encrypts the plaintext using AES
func Encrypt(key []byte, text string) string {
	plaintext := []byte(text)

	block, err := aes.NewCipher(key)
	if err != nil {
		log.Error("Fail to encrypt", "error", err)
		return ""
	}

	ciphertext := make([]byte, aes.BlockSize+len(plaintext))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(cryptoRand.Reader, iv); err != nil {
		log.Error("Fail to encrypt iv", "error", err)
		return ""
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

	return base64.URLEncoding.EncodeToString(ciphertext)
}

// Decrypt decrypts ciphertext using AES
func Decrypt(key []byte, cryptoText string) string {
	ciphertext, _ := base64.URLEncoding.DecodeString(cryptoText)

	block, err := aes.NewCipher(key)
	if err != nil {
		log.Error("Fail to decrypt", "error", err)
		return ""
	}

	if len(ciphertext) < aes.BlockSize {
		log.Error("Ciphertext too short")
		return ""
	}
	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(block, iv)
	stream.XORKeyStream(ciphertext, ciphertext)

	return string(ciphertext)
}

// GetSignersFromContract returns the list of signers from the block signer contract
func GetSignersFromContract(statedb *state.StateDB, block *types.Block) ([]common.Address, error) {
	return state.GetSigners(statedb, block), nil
}

// GetRandomizeFromContract returns random number from contract
func GetRandomizeFromContract(client bind.ContractBackend, addr common.Address) (int64, error) {
	randomize, err := randomizeContract.NewXDCRandomize(common.RandomizeSMCBinary, client)
	if err != nil {
		return 0, err
	}
	opts := new(bind.CallOpts)
	secrets, err := randomize.GetSecret(opts, addr)
	if err != nil {
		return 0, err
	}
	// Since there could be multiple secrets (array), we just use first one
	if len(secrets) == 0 {
		return 0, errors.New("no secrets found")
	}
	// Decrypt the secret to get the random number
	opening, err := randomize.GetOpening(opts, addr)
	if err != nil {
		return 0, err
	}
	if len(opening) == 0 {
		return 0, errors.New("no opening found")
	}

	// Decrypt secret with opening key
	decrypted := Decrypt(opening[:], string(secrets[0][:]))
	result, err := strconv.ParseInt(decrypted, 10, 64)
	if err != nil {
		// If can't parse, return a pseudo-random based on address
		return int64(addr.Big().Int64() % 100), nil
	}
	return result, nil
}

// GenM2FromRandomize generates M2 validator list from random numbers
func GenM2FromRandomize(candidates []int64, lenSigners int64) ([]int64, error) {
	if len(candidates) == 0 {
		return nil, errors.New("empty candidates")
	}

	// Create a copy of candidates for shuffling
	m2 := make([]int64, len(candidates))
	copy(m2, candidates)

	// Fisher-Yates shuffle using candidates as seeds
	for i := len(m2) - 1; i > 0; i-- {
		seed := candidates[i%len(candidates)]
		j := int(seed) % (i + 1)
		if j < 0 {
			j = -j
		}
		m2[i], m2[j] = m2[j], m2[i]
	}

	return m2, nil
}

// BuildValidatorFromM2 builds validator bytes from M2 list
func BuildValidatorFromM2(m2 []int64) []byte {
	var validators []byte
	for _, v := range m2 {
		validators = append(validators, byte(v))
	}
	return validators
}

// GetBlockSigners returns signers who signed a specific block
func GetBlockSigners(client bind.ContractBackend, blockHash common.Hash) ([]common.Address, error) {
	blockSigners, err := contract.NewBlockSigner(common.BlockSignersBinary, client)
	if err != nil {
		return nil, err
	}
	opts := &bind.CallOpts{}
	addrs, err := blockSigners.GetSigners(opts, blockHash)
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

// ExtractAddressFromBytes extracts addresses from a byte slice
func ExtractAddressFromBytes(penaltiesBytes []byte) []common.Address {
	var addresses []common.Address
	if len(penaltiesBytes) == 0 {
		return addresses
	}
	// Each address is 20 bytes
	addressLen := common.AddressLength
	for i := 0; i+addressLen <= len(penaltiesBytes); i += addressLen {
		addresses = append(addresses, common.BytesToAddress(penaltiesBytes[i:i+addressLen]))
	}
	return addresses
}

// CalculateRewardForSigner calculates rewards for signers
func CalculateRewardForSigner(chainReward *big.Int, signers map[common.Address]*RewardLog, totalSigner uint64) (map[common.Address]*big.Int, error) {
	rewardSigners := make(map[common.Address]*big.Int)

	if totalSigner == 0 {
		return rewardSigners, nil
	}

	rewardPerSign := new(big.Int).Div(chainReward, big.NewInt(int64(totalSigner)))

	for addr, rLog := range signers {
		reward := new(big.Int).Mul(rewardPerSign, big.NewInt(int64(rLog.Sign)))
		rewardSigners[addr] = reward
	}

	return rewardSigners, nil
}

// CalculateRewardForHolders calculates rewards for token holders (voters)
// This distributes rewards to:
// - Masternode owner: RewardMasterPercent (90%) — matches v2.6.8 common/constants.go
// - Voters: RewardVoterPercent (0%) — voter rewards disabled in v2.6.8
// - Foundation: RewardFoundationPercent (10%)
func CalculateRewardForHolders(foundationWalletAddr common.Address, statedb *state.StateDB, signer common.Address, calcReward *big.Int, blockNumber uint64) (map[common.Address]*big.Int, error) {
	rewards := make(map[common.Address]*big.Int)

	// Get candidate owner (the masternode operator)
	owner := state.GetCandidateOwner(statedb, signer)
	if owner == (common.Address{}) {
		// If no owner found, default to signer
		owner = signer
	}

	// Calculate masternode owner reward (RewardMasterPercent = 90%)
	rewardMaster := new(big.Int).Mul(calcReward, big.NewInt(int64(common.RewardMasterPercent)))
	rewardMaster.Div(rewardMaster, big.NewInt(100))
	rewards[owner] = rewardMaster

	// Get voters for this masternode
	voters := state.GetVoters(statedb, signer)

	if len(voters) > 0 {
		// Calculate total voter reward pool (50%)
		totalVoterReward := new(big.Int).Mul(calcReward, big.NewInt(int64(common.RewardVoterPercent)))
		totalVoterReward.Div(totalVoterReward, big.NewInt(100))

		// Calculate total voting capacity
		totalCap := new(big.Int)
		voterCaps := make(map[common.Address]*big.Int)

		for _, voteAddr := range voters {
			// Skip duplicates after TIP2019
			if _, ok := voterCaps[voteAddr]; ok && common.TIP2019Block.Uint64() <= blockNumber {
				continue
			}
			voterCap := state.GetVoterCap(statedb, signer, voteAddr)
			totalCap.Add(totalCap, voterCap)
			voterCaps[voteAddr] = voterCap
		}

		// Distribute to voters proportionally
		if totalCap.Cmp(big.NewInt(0)) > 0 {
			for addr, voteCap := range voterCaps {
				if voteCap.Cmp(big.NewInt(0)) > 0 {
					// reward = totalVoterReward * voteCap / totalCap
					rcap := new(big.Int).Mul(totalVoterReward, voteCap)
					rcap.Div(rcap, totalCap)
					if rewards[addr] != nil {
						rewards[addr].Add(rewards[addr], rcap)
					} else {
						rewards[addr] = rcap
					}
				}
			}
		}
	}

	// Foundation reward (10%)
	foundationReward := new(big.Int).Mul(calcReward, big.NewInt(int64(common.RewardFoundationPercent)))
	foundationReward.Div(foundationReward, big.NewInt(100))
	if foundationWalletAddr != (common.Address{}) {
		rewards[foundationWalletAddr] = foundationReward
	}

	log.Debug("Holders reward calculated", "masternode", signer.Hex(), "owner", owner.Hex(),
		"masterReward", rewardMaster, "foundationReward", foundationReward, "numVoters", len(voters))

	return rewards, nil
}

// DecodeExtraFields decodes extra data fields from header
func DecodeExtraFields(extra []byte) (common.Address, []byte, error) {
	if len(extra) < extraVanity {
		return common.Address{}, nil, errors.New("extra-data too short")
	}

	// Extract vanity
	vanity := extra[:extraVanity]

	// Extract signer
	if len(extra) < extraVanity+extraSeal {
		return common.Address{}, vanity, errors.New("missing signature")
	}

	signature := extra[len(extra)-extraSeal:]
	_ = signature // Would be used to recover signer

	return common.Address{}, vanity, nil
}

// CompareSignersLists compares two lists of signers
func CompareSignersLists(list1, list2 []common.Address) bool {
	if len(list1) != len(list2) {
		return false
	}

	for i := range list1 {
		if list1[i] != list2[i] {
			return false
		}
	}

	return true
}

// BytesEqual compares two byte slices
func BytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
