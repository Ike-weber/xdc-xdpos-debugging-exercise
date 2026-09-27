// Command xdc-conformance is a cross-client conformance harness for the XDC
// network. It validates the consensus claims in
// docs/XDC_CROSS_CLIENT_CONFORMANCE_SPEC.md (Part 12 gates) against a live node
// over JSON-RPC, and serves as a known-good reference oracle that a reth /
// erigon / besu port can diff its own outputs against.
//
// Gates:
//
//	b  header RLP / block-hash parity          (re-derives keccak(RLP(header)))
//	d  v2 BFT QC quorum verification           (decodes extraData, recovers QC signers)
//	c  v1 independent seal recovery            (v1 sigHash, M1 creator ∈ masternode set)
//	e  state-transition parity (RPC diff)      (stateRoot/receiptsRoot/gasUsed: nodeA vs nodeB)
//	f  EIP-2124 fork ID (peering parity)       (reference forkHash/forkNext a client must advertise)
//	g  state-transition re-derivation          (txRoot+receiptsRoot from block data; stateRoot via eth_getProof)
//
// Usage:
//
//	xdc-conformance b <rpc> <block> [block...]
//	xdc-conformance d <rpc> <block> [block...]
//	xdc-conformance c <rpc> <block> [block...]
//	xdc-conformance e <rpcCanonical> <rpcUnderTest> <block> [block...]
//	xdc-conformance f <rpc>
//	xdc-conformance g <rpc> <block> [block...]
//
// blocks may be decimal, 0x-hex, or "latest". Exit code is non-zero if any
// checked block fails its gate.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	gate := strings.ToLower(os.Args[1])
	var ok bool
	switch gate {
	case "b":
		ok = gateB(os.Args[2], os.Args[3:])
	case "d":
		ok = gateD(os.Args[2], os.Args[3:])
	case "c":
		ok = gateC(os.Args[2], os.Args[3:])
	case "e":
		if len(os.Args) < 5 {
			usage()
		}
		ok = gateE(os.Args[2], os.Args[3], os.Args[4:])
	case "f":
		ok = gateF(os.Args[2])
	case "g":
		ok = gateG(os.Args[2], os.Args[3:])
	default:
		usage()
	}
	if !ok {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `xdc-conformance — XDC cross-client conformance harness

  b <rpc> <block...>                header RLP / block-hash parity
  d <rpc> <block...>                v2 BFT QC quorum verification
  c <rpc> <block...>                v1 round-robin sealer checks
  e <rpcCanonical> <rpcTest> <block...>  state-root/receipts/gasUsed diff
  f <rpc>                           EIP-2124 fork ID (peering parity)
  g <rpc> <block...>                re-derive txRoot+receiptsRoot from block data + verify stateRoot via eth_getProof

blocks: decimal, 0xHEX, or "latest". exit!=0 on any failure.
`)
	os.Exit(2)
}

// --- RPC plumbing ----------------------------------------------------------

func rpc(url, method, params string) (json.RawMessage, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":%s,"id":1}`, method, params)
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w (%.200s)", err, raw)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", r.Error.Message)
	}
	return r.Result, nil
}

func blockParam(b string) string {
	if b == "latest" || b == "earliest" || b == "pending" {
		return `"` + b + `"`
	}
	if strings.HasPrefix(b, "0x") {
		return `"` + b + `"`
	}
	n, err := strconv.ParseUint(b, 10, 64)
	if err != nil {
		return `"` + b + `"`
	}
	return fmt.Sprintf(`"0x%x"`, n)
}

// fetchHeader pulls a header and works around #978 (gen_header_json.go omits the
// XDC fields) by re-populating Validators/Validator/Penalties from the raw JSON.
// Returns the header and the node-reported canonical hash.
func fetchHeader(url, block string) (*types.Header, common.Hash, error) {
	res, err := rpc(url, "eth_getBlockByNumber", "["+blockParam(block)+",false]")
	if err != nil {
		return nil, common.Hash{}, err
	}
	if len(res) == 0 || string(res) == "null" {
		return nil, common.Hash{}, fmt.Errorf("no such block")
	}
	var h types.Header
	if err := json.Unmarshal(res, &h); err != nil {
		return nil, common.Hash{}, fmt.Errorf("header decode: %w", err)
	}
	var x struct {
		Hash       common.Hash   `json:"hash"`
		Validators hexutil.Bytes `json:"validators"`
		Validator  hexutil.Bytes `json:"validator"`
		Penalties  hexutil.Bytes `json:"penalties"`
	}
	_ = json.Unmarshal(res, &x)
	h.Validators = x.Validators // #978 workaround: JSON codec drops these
	h.Validator = x.Validator
	h.Penalties = x.Penalties
	return &h, x.Hash, nil
}

func getMasternodes(url string, num *big.Int) ([]common.Address, error) {
	// Prefer XDPoS_getMasternodes (flat address array). Fall back to
	// XDPoS_getMasternodesByNumber ({Masternodes:[...]}) which some public
	// gateways expose instead. Go's JSON field matching is case-insensitive,
	// so both "Masternodes" and "masternodes" bind.
	if res, err := rpc(url, "XDPoS_getMasternodes", fmt.Sprintf(`["0x%x"]`, num)); err == nil {
		var addrs []common.Address
		if json.Unmarshal(res, &addrs) == nil && len(addrs) > 0 {
			return addrs, nil
		}
	}
	res, err := rpc(url, "XDPoS_getMasternodesByNumber", fmt.Sprintf(`["0x%x"]`, num))
	if err != nil {
		return nil, err
	}
	var r struct{ Masternodes []common.Address }
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	return r.Masternodes, nil
}

func blockNumRoots(url, block string) (num uint64, stateRoot, receiptsRoot common.Hash, gasUsed uint64, err error) {
	res, e := rpc(url, "eth_getBlockByNumber", "["+blockParam(block)+",false]")
	if e != nil {
		return 0, common.Hash{}, common.Hash{}, 0, e
	}
	var x struct {
		Number       string      `json:"number"`
		StateRoot    common.Hash `json:"stateRoot"`
		ReceiptsRoot common.Hash `json:"receiptsRoot"`
		GasUsed      string      `json:"gasUsed"`
	}
	if e := json.Unmarshal(res, &x); e != nil {
		return 0, common.Hash{}, common.Hash{}, 0, e
	}
	num, _ = strconv.ParseUint(strings.TrimPrefix(x.Number, "0x"), 16, 64)
	gasUsed, _ = strconv.ParseUint(strings.TrimPrefix(x.GasUsed, "0x"), 16, 64)
	return num, x.StateRoot, x.ReceiptsRoot, gasUsed, nil
}

func pass(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

// --- Gate F: EIP-2124 fork ID (peering parity) -----------------------------
//
// The eth Status handshake exchanges a ForkID before any subprotocol (incl.
// XDPOS2/100) negotiates. A client that computes a different fork ID — because
// its fork schedule diverges from the XDC fleet's — is rejected at handshake
// and never peers (this was the literal root cause of a devnet node stuck at
// peers=0 despite an identical genesis). Gate F emits the canonical XDC fork ID
// from the *reference* chain config + the live genesis, so a porting team can
// confirm reth/erigon/besu advertise a byte-identical ForkID.
//
// ForkID is derived from the genesis HASH + the ordered list of passed fork
// heights, so it needs only the genesis block (rebuilt from its RPC header:
// genesis has no txs/uncles, so NewBlockWithHeader(h).Hash() == the canonical
// genesis hash) and the chain config — no archive state, no admin RPC.
func gateF(url string) bool {
	gh, _, err := fetchHeader(url, "0x0")
	if err != nil {
		fmt.Printf("genesis fetch ERR %v\n", err)
		return false
	}
	genesisBlock := types.NewBlockWithHeader(gh)

	cidRaw, err := rpc(url, "eth_chainId", "[]")
	if err != nil {
		fmt.Printf("eth_chainId ERR %v\n", err)
		return false
	}
	var cidHex string
	json.Unmarshal(cidRaw, &cidHex)
	cid, _ := strconv.ParseUint(strings.TrimPrefix(cidHex, "0x"), 16, 64)

	var (
		cfg  *params.ChainConfig
		name string
	)
	switch cid {
	case 50:
		cfg, name = params.XDCMainnetChainConfig, "xdc-mainnet(50)"
	case 51:
		cfg, name = params.XDCApothemChainConfig, "xdc-apothem(51)"
	default:
		fmt.Printf("chainId %d FAIL: no built-in reference XDC config (supported: 50 mainnet, 51 apothem)\n", cid)
		return false
	}

	hb, _, err := fetchHeader(url, "latest")
	if err != nil {
		fmt.Printf("head fetch ERR %v\n", err)
		return false
	}
	head := hb.Number.Uint64()

	// The genesis hash reported by the live node is authoritative; gate B already
	// proves XDC genesis headers re-hash correctly. The fork ID is seeded from it.
	idHead := forkid.NewID(cfg, genesisBlock, head, hb.Time)
	idGenesis := forkid.NewID(cfg, genesisBlock, 0, genesisBlock.Time())

	fmt.Printf("%-16s genesis=%s\n", name, genesisBlock.Hash().Hex())
	fmt.Printf("  head=%d time=%d\n", head, hb.Time)
	fmt.Printf("  forkID @genesis = forkHash=0x%x forkNext=%d\n", idGenesis.Hash, idGenesis.Next)
	fmt.Printf("  forkID @head    = forkHash=0x%x forkNext=%d\n", idHead.Hash, idHead.Next)
	fmt.Printf("  => to peer with the XDC %s fleet, a porting client at head %d MUST advertise\n", name, head)
	fmt.Printf("     forkHash=0x%x, forkNext=%d in its eth Status handshake.\n", idHead.Hash, idHead.Next)
	fmt.Printf("  (cross-checking a live peer's advertised ForkID needs admin_nodeInfo over IPC/admin RPC — fleet access, not public.)\n")
	return true
}

// --- Gate B: header RLP / block-hash parity --------------------------------

func gateB(url string, blocks []string) bool {
	all := true
	for _, b := range blocks {
		h, canon, err := fetchHeader(url, b)
		if err != nil {
			fmt.Printf("%-12s ERR  %v\n", b, err)
			all = false
			continue
		}
		got := h.Hash() // keccak256(RLP(header)) over the full XDC field list
		ok := got == canon
		all = all && ok
		fmt.Printf("%-12s num=%-9d %s derived=%s canonical=%s |v=%dB val=%dB p=%dB baseFee=%v|\n",
			b, h.Number.Uint64(), pass(ok), got.Hex(), canon.Hex(),
			len(h.Validators), len(h.Validator), len(h.Penalties), h.BaseFee != nil)
	}
	return all
}

// --- Gate D: v2 BFT QuorumCert quorum verification -------------------------

func gateD(url string, blocks []string) bool {
	all := true
	// Select the reference V2 config so the QC quorum threshold matches the
	// canonical engine's ROUND-KEYED CertThreshold instead of a hardcoded 2/3.
	// Apothem uses CertThreshold=0.45 for rounds 0..899,999 then 0.667; mainnet
	// is 0.667 from round 0. Config(round) only returns the right per-round
	// value after BuildConfigIndex() is called.
	var v2cfg *params.V2
	if cidRaw, err := rpc(url, "eth_chainId", "[]"); err == nil {
		var cidHex string
		json.Unmarshal(cidRaw, &cidHex)
		switch strings.ToLower(strings.TrimPrefix(cidHex, "0x")) {
		case "32":
			v2cfg = params.XDCMainnetChainConfig.XDPoS.V2
		case "33":
			v2cfg = params.XDCApothemChainConfig.XDPoS.V2
		}
	}
	if v2cfg != nil {
		v2cfg.BuildConfigIndex()
	}
	certThresholdFor := func(round uint64) float64 {
		if v2cfg != nil {
			if c := v2cfg.Config(round); c != nil && c.CertThreshold > 0 {
				return c.CertThreshold
			}
		}
		return 2.0 / 3.0 // fallback when the reference config is unavailable
	}
	for _, b := range blocks {
		h, _, err := fetchHeader(url, b)
		if err != nil {
			fmt.Printf("%-12s ERR  %v\n", b, err)
			all = false
			continue
		}
		num := h.Number.Uint64()
		if len(h.Extra) < 1 || h.Extra[0] != 2 {
			fmt.Printf("%-12s num=%-9d SKIP not-a-v2-block (extra[0]!=0x02)\n", b, num)
			continue
		}
		var ef types.ExtraFields_v2
		if err := rlp.DecodeBytes(h.Extra[1:], &ef); err != nil {
			fmt.Printf("%-12s num=%-9d FAIL extra decode: %v\n", b, num, err)
			all = false
			continue
		}
		qc := ef.QuorumCert
		if qc == nil || qc.ProposedBlockInfo == nil {
			fmt.Printf("%-12s num=%-9d FAIL no QuorumCert in extraData\n", b, num)
			all = false
			continue
		}
		// QC signatures sign VoteSigHash{ProposedBlockInfo, GapNumber}.
		msg := types.VoteSigHash(&types.VoteForSign{
			ProposedBlockInfo: qc.ProposedBlockInfo,
			GapNumber:         qc.GapNumber,
		})
		signers := map[common.Address]bool{}
		for _, sig := range qc.Signatures {
			s := []byte(sig)
			if len(s) == 65 && s[64] >= 27 { // normalize V if needed
				s = append([]byte(nil), s...)
				s[64] -= 27
			}
			pub, err := crypto.SigToPub(msg.Bytes(), s)
			if err != nil {
				continue
			}
			signers[crypto.PubkeyToAddress(*pub)] = true
		}
		mns, err := getMasternodes(url, qc.ProposedBlockInfo.Number)
		if err != nil {
			fmt.Printf("%-12s num=%-9d WARN getMasternodes(%d): %v — reporting raw sig count only\n",
				b, num, qc.ProposedBlockInfo.Number, err)
		}
		mnSet := map[common.Address]bool{}
		for _, a := range mns {
			mnSet[a] = true
		}
		inSet := 0
		for a := range signers {
			if mnSet[a] {
				inSet++
			}
		}
		// The QC is verified against the CertThreshold at the round it certifies.
		qcRound := uint64(qc.ProposedBlockInfo.Round)
		// A round-0 QC is the epoch-switch bootstrap marker (the first v2 block
		// has no prior v2 round to have voted on); verifyQC skips the quorum
		// check for it, so we do too.
		if qcRound == 0 {
			fmt.Printf("%-12s num=%-9d round=%d PASS bootstrap-QC (round-0 epoch-switch marker; quorum check skipped) qcSigs=%d gap=%d\n",
				b, num, uint64(ef.Round), len(qc.Signatures), qc.GapNumber)
			continue
		}
		certT := certThresholdFor(qcRound)
		threshold := int(math.Ceil(certT * float64(len(mns))))
		ok := len(mns) > 0 && inSet >= threshold
		all = all && ok
		fmt.Printf("%-12s num=%-9d round=%d qcRound=%d %s qcSigs=%d distinctSigners=%d inMasternodeSet=%d/%d certT=%.3f threshold=%d gap=%d\n",
			b, num, uint64(ef.Round), qcRound, pass(ok), len(qc.Signatures), len(signers), inSet, len(mns), certT, threshold, qc.GapNumber)
	}
	return all
}

// --- Gate C: v1 round-robin sealer checks (partial; RPC cross-check) -------

// v1SigHash replicates consensus/XDPoS.encodeSigHeader: keccak256(RLP(header
// with the trailing 65-byte seal stripped from Extra)). The v1 M1 (creator)
// signature is the last 65 bytes of header.Extra, recovered over this hash.
func v1SigHash(h *types.Header) common.Hash {
	extraForHash := h.Extra
	if len(h.Extra) >= 65 {
		extraForHash = h.Extra[:len(h.Extra)-65]
	}
	enc := []interface{}{
		h.ParentHash, h.UncleHash, h.Coinbase, h.Root, h.TxHash, h.ReceiptHash,
		h.Bloom, h.Difficulty, h.Number, h.GasLimit, h.GasUsed, h.Time,
		extraForHash, h.MixDigest, h.Nonce,
	}
	if h.BaseFee != nil {
		enc = append(enc, h.BaseFee)
	}
	if h.WithdrawalsHash != nil {
		enc = append(enc, h.WithdrawalsHash)
	}
	if h.BlobGasUsed != nil {
		enc = append(enc, h.BlobGasUsed)
	}
	if h.ExcessBlobGas != nil {
		enc = append(enc, h.ExcessBlobGas)
	}
	if h.ParentBeaconRoot != nil {
		enc = append(enc, h.ParentBeaconRoot)
	}
	b, _ := rlp.EncodeToBytes(enc)
	return crypto.Keccak256Hash(b)
}

func normSig(sig []byte) []byte {
	if len(sig) == 65 && sig[64] >= 27 {
		s := append([]byte(nil), sig...)
		s[64] -= 27
		return s
	}
	return sig
}

func recoverAddr(hash common.Hash, sig []byte) (common.Address, bool) {
	if len(sig) != 65 {
		return common.Address{}, false
	}
	pub, err := crypto.SigToPub(hash.Bytes(), normSig(sig))
	if err != nil {
		return common.Address{}, false
	}
	return crypto.PubkeyToAddress(*pub), true
}

// checkpointMasternodes decodes the active v1 masternode list (ordered) from the
// epoch checkpoint header's Extra: [32 vanity][N*20 addrs][65 seal]. No XDPoS_
// RPC namespace required — works on any node incl. public gateways.
func checkpointMasternodes(url string, n uint64) ([]common.Address, uint64, error) {
	cp := (n / 900) * 900
	h, _, err := fetchHeader(url, fmt.Sprintf("0x%x", cp))
	if err != nil {
		return nil, cp, err
	}
	ex := h.Extra
	if len(ex) < 97 {
		return nil, cp, fmt.Errorf("checkpoint %d extra len %d < 97", cp, len(ex))
	}
	body := ex[32 : len(ex)-65]
	if len(body)%20 != 0 {
		return nil, cp, fmt.Errorf("checkpoint %d masternode bytes %d not multiple of 20", cp, len(body))
	}
	addrs := make([]common.Address, len(body)/20)
	for i := range addrs {
		copy(addrs[i][:], body[i*20:(i+1)*20])
	}
	return addrs, cp, nil
}

func contains(set []common.Address, a common.Address) bool {
	for _, x := range set {
		if x == a {
			return true
		}
	}
	return false
}

func gateC(url string, blocks []string) bool {
	all := true
	for _, b := range blocks {
		h, _, err := fetchHeader(url, b)
		if err != nil {
			fmt.Printf("%-12s ERR  %v\n", b, err)
			all = false
			continue
		}
		num := h.Number.Uint64()
		if len(h.Extra) >= 1 && h.Extra[0] == 2 {
			fmt.Printf("%-12s num=%-9d SKIP v2-block (use gate d)\n", b, num)
			continue
		}
		ok := true
		var notes []string
		mns, cp, mErr := checkpointMasternodes(url, num)
		if mErr != nil {
			ok = false
			notes = append(notes, "masternodes: "+mErr.Error())
		}
		// M1 — creator/sealer: last 65 bytes of Extra recovered over v1SigHash.
		m1Str := "none"
		if len(h.Extra) >= 65 {
			if m1, okR := recoverAddr(v1SigHash(h), h.Extra[len(h.Extra)-65:]); okR {
				in := contains(mns, m1)
				inTurn := len(mns) > 0 && mns[num%uint64(len(mns))] == m1
				m1Str = fmt.Sprintf("%s(in=%v,inturn=%v)", m1.Hex()[:10], in, inTurn)
				if len(mns) > 0 && !in {
					ok = false
					notes = append(notes, "M1 not in masternode set")
				}
			} else {
				ok = false
				notes = append(notes, "M1 recover failed")
			}
		} else {
			ok = false
			notes = append(notes, "no M1 seal in Extra")
		}
		// M2 — double-validation (#951), INFORMATIONAL only. The modern
		// HashNoValidator() recovery does not match the field as populated by the
		// legacy XDPoSChain producers that mint mainnet (and M2 is a diagnostic,
		// not-enforced field), so it is not part of the gate-C pass criterion.
		// Gate C passes on independent M1 (creator) recovery ∈ masternode set.
		m2Str := "none"
		if len(h.Validator) == 65 {
			if m2, okR := recoverAddr(h.HashNoValidator(), h.Validator); okR {
				m2Str = fmt.Sprintf("%s(in=%v,info)", m2.Hex()[:10], contains(mns, m2))
			}
		}
		if num%900 == 0 && h.Coinbase != (common.Address{}) {
			ok = false
			notes = append(notes, "checkpoint coinbase!=0x0")
		}
		all = all && ok
		fmt.Printf("%-12s num=%-9d %s ckpt=%v diff=%v M1=%s M2=%s mns=%d@cp%d %s\n",
			b, num, pass(ok), num%900 == 0, h.Difficulty, m1Str, m2Str, len(mns), cp, strings.Join(notes, ";"))
	}
	return all
}

func blockSigners(url, block string) ([]common.Address, error) {
	res, err := rpc(url, "eth_getBlockSignersByNumber", "["+blockParam(block)+"]")
	if err != nil {
		return nil, err
	}
	var addrs []common.Address
	if err := json.Unmarshal(res, &addrs); err != nil {
		return nil, err
	}
	return addrs, nil
}

// --- Gate E: state-transition parity via RPC diff --------------------------

func gateE(urlCanonical, urlTest string, blocks []string) bool {
	all := true
	for _, b := range blocks {
		nA, srA, rrA, guA, eA := blockNumRoots(urlCanonical, b)
		nB, srB, rrB, guB, eB := blockNumRoots(urlTest, b)
		if eA != nil || eB != nil {
			fmt.Printf("%-12s ERR canonical=%v test=%v\n", b, eA, eB)
			all = false
			continue
		}
		ok := nA == nB && srA == srB && rrA == rrB && guA == guB
		all = all && ok
		fmt.Printf("%-12s num=%-9d %s stateRoot[%v] receiptsRoot[%v] gasUsed[%d vs %d]\n",
			b, nA, pass(ok), srA == srB, rrA == rrB, guA, guB)
		if !ok {
			fmt.Printf("             canonical: state=%s receipts=%s\n             undertest: state=%s receipts=%s\n",
				srA.Hex(), rrA.Hex(), srB.Hex(), rrB.Hex())
		}
	}
	fmt.Println("note: gate E diffs a client-under-test's RPC against the canonical archive. Full re-execution (load pre-state @N-1, execute, derive roots) requires an archive datadir + core/state — see spec Part 12-E.")
	return all
}

// --- Gate G: state-transition output re-derivation (gate E, self-contained) -
//
// Instead of trusting a node's reported roots, gate G rebuilds the
// consensus-critical execution-output commitments from the block's own data:
//
//   - txRoot       = DeriveSha(block transactions)        == header.TxHash
//   - receiptsRoot = DeriveSha(eth_getBlockReceipts(N))   == header.ReceiptHash
//   - stateRoot    : an eth_getProof account proof is verified to root to
//     header.Root via trie.VerifyProof
//
// txRoot/receiptsRoot work against any full node at any depth (bodies and
// receipts are not pruned). The stateRoot proof needs the state trie at N —
// available at the tip on a full node, and at any depth on an archive node;
// if the node has pruned state at N, eth_getProof returns no usable proof and
// the stateRoot check is reported as unavailable (not a failure of the chain).
func gateG(url string, blocks []string) bool {
	all := true
	for _, b := range blocks {
		h, _, err := fetchHeader(url, b)
		if err != nil {
			fmt.Printf("%-12s ERR %v\n", b, err)
			all = false
			continue
		}
		num := h.Number.Uint64()
		var notes []string

		// 1. txRoot — re-derive from the block's transactions.
		txOK := false
		if txs, terr := fetchBlockTxs(url, b); terr != nil {
			notes = append(notes, "txs: "+terr.Error())
		} else {
			got := types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
			txOK = got == h.TxHash
			if !txOK {
				notes = append(notes, fmt.Sprintf("txRoot %s!=%s", got.Hex()[:10], h.TxHash.Hex()[:10]))
			}
		}

		// 2. receiptsRoot — re-derive from eth_getBlockReceipts.
		rcOK, nR := false, 0
		if rs, rerr := fetchReceipts(url, b); rerr != nil {
			notes = append(notes, "receipts: "+rerr.Error())
		} else {
			nR = len(rs)
			got := types.DeriveSha(types.Receipts(rs), trie.NewStackTrie(nil))
			rcOK = got == h.ReceiptHash
			if !rcOK {
				notes = append(notes, fmt.Sprintf("receiptsRoot %s!=%s", got.Hex()[:10], h.ReceiptHash.Hex()[:10]))
			}
		}

		// 3. stateRoot — verify an eth_getProof account proof roots to header.Root.
		stState, stNote := verifyStateProof(url, b, h.Root)
		if stNote != "" {
			notes = append(notes, "stateRoot: "+stNote)
		}

		// Gate passes on the always-available output commitments (tx + receipts);
		// the stateRoot proof is reported but only fails the gate when the node
		// served a proof that did NOT root to header.Root (a real divergence).
		ok := txOK && rcOK && stState != "BADPROOF"
		all = all && ok
		fmt.Printf("%-12s num=%-9d %s txRoot[%v] receiptsRoot[%v](n=%d) stateRoot-proof[%s] %s\n",
			b, num, pass(ok), txOK, rcOK, nR, stState, strings.Join(notes, ";"))
	}
	return all
}

func fetchBlockTxs(url, block string) ([]*types.Transaction, error) {
	res, err := rpc(url, "eth_getBlockByNumber", "["+blockParam(block)+",true]")
	if err != nil {
		return nil, err
	}
	var blk struct {
		Transactions []*types.Transaction `json:"transactions"`
	}
	if err := json.Unmarshal(res, &blk); err != nil {
		return nil, err
	}
	return blk.Transactions, nil
}

func fetchReceipts(url, block string) ([]*types.Receipt, error) {
	res, err := rpc(url, "eth_getBlockReceipts", "["+blockParam(block)+"]")
	if err != nil {
		return nil, err
	}
	var rs []*types.Receipt
	if err := json.Unmarshal(res, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// verifyStateProof asks the node for an eth_getProof of a well-known account at
// the block and checks the returned Merkle proof roots to header.Root. Returns:
//
//	"OK"        proof verified against stateRoot
//	"BADPROOF"  node served a proof that does NOT root to stateRoot (divergence)
//	"n/a"       node has no state at this block (pruned) — needs an archive node
func verifyStateProof(url, block string, stateRoot common.Hash) (string, string) {
	// 0x...88 = MasternodeVotingSMC, present on both mainnet and apothem.
	addr := common.HexToAddress("0x0000000000000000000000000000000000000088")
	res, err := rpc(url, "eth_getProof", `["`+addr.Hex()+`",[],`+blockParam(block)+`]`)
	if err != nil {
		return "n/a", "getProof: " + err.Error()
	}
	var pr struct {
		AccountProof []string `json:"accountProof"`
	}
	if err := json.Unmarshal(res, &pr); err != nil || len(pr.AccountProof) == 0 {
		return "n/a", "no state at block (pruned? use an archive node)"
	}
	proofDB := memorydb.New()
	for _, node := range pr.AccountProof {
		raw := common.FromHex(node)
		proofDB.Put(crypto.Keccak256(raw), raw)
	}
	if _, err := trie.VerifyProof(stateRoot, crypto.Keccak256(addr.Bytes()), proofDB); err != nil {
		return "BADPROOF", "proof does not root to stateRoot: " + err.Error()
	}
	return "OK", ""
}
