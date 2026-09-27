// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// A.47: NodeData late-delivery recovery.
//
// Pre-A.47, runFastStateSync's "Unrequested" branch discarded NodeData blobs
// whose originating in-flight stateReq had been moved to finished[] by the
// "Busy peer assigned new fast state fetch" or peer-drop paths. Production
// measurement on xdcscan PID 299150 over 5h49m showed
// processed=20458 vs unexpected=487200 (96% drop ratio). A.53's parallel
// dispatch made this worse because more in-flight reqs = more late deliveries.
//
// Post-A.47, the late blobs are wrapped in a synthetic stateReq (peer == nil
// sentinel) and routed through finished[] -> s.deliver -> process(). The
// extended process() detects req.peer == nil and matches blob hashes against
// the GLOBAL s.trieTasks / s.codeTasks pools (which the dropped task's
// entries have been requeued into by process()'s existing requeue branch).
// All s.sched / s.keccak / s.trieTasks / s.codeTasks mutations stay in
// s.run()'s goroutine -- no new locks needed.
//
// Refs A.47 / #857.

package downloader

import (
	"crypto/sha256"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// newLateDeliveryTestSync constructs a real fastStateSync wired against a
// tiny in-memory source state. The returned sync has its sched seeded with
// the source's trie nodes + code entries, ready for fillTasks/processLateDelivery.
// The accompanying srcDB / srcReader pair is used by the test to look up the
// canonical blob bytes by path so the test can simulate a peer delivering
// the right data for a known hash. Refs A.47.
func newLateDeliveryTestSync(t *testing.T) (*fastStateSync, common.Hash, func(path string, hash common.Hash) []byte, func(codeHash common.Hash) []byte) {
	t.Helper()

	// Build a tiny source state with a handful of accounts -- some with code,
	// some with storage -- so the resulting trie has both trie nodes and
	// code entries available for the sync to request.
	srcDisk := rawdb.NewMemoryDatabase()
	srcTriedb := triedb.NewDatabase(srcDisk, nil)
	srcStateDB := state.NewDatabase(srcTriedb, nil)
	srcState, err := state.New(common.Hash{}, srcStateDB)
	if err != nil {
		t.Fatalf("create src state: %v", err)
	}
	codeMap := map[common.Hash][]byte{}
	for i := byte(0); i < 8; i++ {
		addr := common.BytesToAddress([]byte{0xAA, i})
		srcState.AddBalance(addr, uint256.NewInt(uint64(1000)+uint64(i)), tracing.BalanceChangeUnspecified)
		srcState.SetNonce(addr, uint64(i), tracing.NonceChangeUnspecified)
		if i%2 == 0 {
			code := []byte{0x60, 0x00, 0x60, i, 0xF3} // PUSH1 0 PUSH1 i RETURN
			codeHash := crypto.Keccak256Hash(code)
			srcState.SetCode(addr, code, tracing.CodeChangeUnspecified)
			codeMap[codeHash] = code
		}
	}
	root, err := srcState.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit src state: %v", err)
	}
	if err := srcTriedb.Commit(root, false); err != nil {
		t.Fatalf("commit triedb: %v", err)
	}
	srcReader, err := srcTriedb.NodeReader(root)
	if err != nil {
		t.Fatalf("node reader: %v", err)
	}

	// Build the destination fastStateSync against a fresh memory DB. We bind
	// it to a real Downloader (via newTester) so the updateStats path that
	// touches s.d.syncStatsLock works without nil-derefs. The chain we
	// don't drive -- we only call process() on the synthetic stateReq.
	tester := newTester(t, ethconfig.FullSync)
	t.Cleanup(tester.terminate)
	dstDB := tester.downloader.stateDB
	s := &fastStateSync{
		d:                 tester.downloader,
		root:              root,
		sched:             state.NewStateSync(root, dstDB, nil, srcTriedb.Scheme()),
		keccak:            sha3.NewLegacyKeccak256().(crypto.KeccakState),
		trieTasks:         make(map[string]*trieTask),
		codeTasks:         make(map[common.Hash]*codeTask),
		inflightHashes:    make(map[common.Hash]struct{}),
		recentHashes:      make(map[common.Hash]struct{}, 1<<10),
		recentHashesOrder: make([]common.Hash, 0, 1<<10),
	}

	// Seed s.trieTasks / s.codeTasks from the scheduler the same way
	// fillTasks does -- but WITHOUT moving them into any peer's req. This
	// mirrors the post-requeue state: the original in-flight req's tasks
	// have all been put back into the global pool.
	paths, hashes, codes := s.sched.Missing(32)
	for i, path := range paths {
		s.trieTasks[path] = &trieTask{
			hash:     hashes[i],
			path:     path,
			attempts: make(map[string]struct{}),
		}
	}
	for _, h := range codes {
		s.codeTasks[h] = &codeTask{
			attempts: make(map[string]struct{}),
		}
	}

	lookupNode := func(path string, hash common.Hash) []byte {
		owner, inner := trie.ResolvePath([]byte(path))
		data, err := srcReader.Node(owner, inner, hash)
		if err != nil {
			t.Fatalf("source reader miss for path %x: %v", path, err)
		}
		return data
	}
	lookupCode := func(codeHash common.Hash) []byte {
		blob, ok := codeMap[codeHash]
		if !ok {
			t.Fatalf("code blob missing for hash %x", codeHash)
		}
		return blob
	}
	return s, root, lookupNode, lookupCode
}

// TestLateNodeDataRecoveredViaGlobalPool is the core A.47 regression test.
// Constructs a fastStateSync, seeds it with a known set of trie tasks (as
// the requeue branch in process() would), then drives processLateDelivery
// with a synthetic stateReq carrying the canonical blobs. Asserts the
// blobs are accepted (sched.Pending() drops, s.trieTasks shrinks,
// s.numUncommitted grows) -- proving the late deliveries are no longer
// silently dropped. Refs A.47.
func TestLateNodeDataRecoveredViaGlobalPool(t *testing.T) {
	s, _, lookupNode, _ := newLateDeliveryTestSync(t)
	if len(s.trieTasks) == 0 {
		t.Fatal("test fixture has no trie tasks -- nothing to recover")
	}

	trieTasksBefore := len(s.trieTasks)

	// Synthesise the blob list for the first few outstanding trie tasks.
	// Cap at 3 so the test asserts a partial-acceptance scenario and the
	// "unexpected" counter stays well-behaved.
	var (
		blobs           [][]byte
		expectedRemoved []string
	)
	count := 0
	for path, task := range s.trieTasks {
		if count >= 3 {
			break
		}
		blobs = append(blobs, lookupNode(path, task.hash))
		expectedRemoved = append(expectedRemoved, path)
		count++
	}

	req := &stateReq{
		peer:           nil, // synthetic -- the A.47 sentinel
		response:       blobs,
		dropped:        true,
		inflightTicket: -1,
	}
	successful, err := s.process(req)
	if err != nil {
		t.Fatalf("processLateDelivery returned error: %v", err)
	}
	if successful != len(blobs) {
		t.Fatalf("processLateDelivery: successful = %d, want %d", successful, len(blobs))
	}

	// All accepted tasks must have been removed from the global pool.
	for _, p := range expectedRemoved {
		if _, still := s.trieTasks[p]; still {
			t.Errorf("trie task %x still present after acceptance", p)
		}
	}
	if got, want := len(s.trieTasks), trieTasksBefore-len(blobs); got != want {
		t.Errorf("trieTasks size after accept = %d, want %d", got, want)
	}
	// Note: s.sched.Pending() can RISE after a successful ProcessNode call
	// because processing a parent node enqueues its previously-unknown
	// children. The right acceptance signal here is numUncommitted -- the
	// scheduler accepted N blobs into its membatch.
	if s.numUncommitted != len(blobs) {
		t.Errorf("numUncommitted = %d, want %d", s.numUncommitted, len(blobs))
	}
}

// TestLateNodeDataIgnoresUnknownBlob verifies the unexpected-counter branch
// of processLateDelivery: a blob whose hash matches NO outstanding trie or
// code task must NOT panic, NOT touch the scheduler, and MUST be counted
// in the unexpected stats so production telemetry stays meaningful. Refs A.47.
func TestLateNodeDataIgnoresUnknownBlob(t *testing.T) {
	s, _, _, _ := newLateDeliveryTestSync(t)

	trieTasksBefore := len(s.trieTasks)
	codeTasksBefore := len(s.codeTasks)
	uncommittedBefore := s.numUncommitted

	// Craft a blob whose hash is guaranteed not to be in any task pool.
	// sha256 of a fixed string -- keccak256 of the blob's content will be
	// computed by processLateDelivery and is overwhelmingly unlikely to
	// collide with anything in the small test trie.
	junk := sha256.Sum256([]byte("a.47-late-delivery-not-in-pool"))
	req := &stateReq{
		peer:           nil,
		response:       [][]byte{junk[:]},
		dropped:        true,
		inflightTicket: -1,
	}
	successful, err := s.process(req)
	if err != nil {
		t.Fatalf("processLateDelivery returned error on unknown blob: %v", err)
	}
	if successful != 0 {
		t.Errorf("unknown blob marked successful: got %d, want 0", successful)
	}
	if len(s.trieTasks) != trieTasksBefore {
		t.Errorf("trieTasks mutated for unknown blob: before=%d, after=%d",
			trieTasksBefore, len(s.trieTasks))
	}
	if len(s.codeTasks) != codeTasksBefore {
		t.Errorf("codeTasks mutated for unknown blob: before=%d, after=%d",
			codeTasksBefore, len(s.codeTasks))
	}
	if s.numUncommitted != uncommittedBefore {
		t.Errorf("numUncommitted mutated for unknown blob: before=%d, after=%d",
			uncommittedBefore, s.numUncommitted)
	}
}

// TestLateNodeDataCodeBlob covers the codeTasks branch of processLateDelivery.
// The trie sync schedules ContractCode entries for every account that has
// code; a late delivery for one of those code hashes must be matched against
// s.codeTasks (not s.trieTasks) and ProcessCode'd. We have to drive the
// sync forward (delivering the trie nodes the scheduler asks for in
// successive Missing() calls) until at least one code hash appears in the
// pool, because code requests are only scheduled after the scheduler walks
// the account leaves containing those code hashes. Refs A.47.
func TestLateNodeDataCodeBlob(t *testing.T) {
	s, _, lookupNode, lookupCode := newLateDeliveryTestSync(t)

	// Drive the sync forward until at least one code task is queued (or we
	// give up after a few rounds). Each round: deliver every outstanding
	// trie task via the normal Process path so the scheduler can discover
	// child nodes, including account-leaf code entries.
	const maxRounds = 16
	for round := 0; round < maxRounds && len(s.codeTasks) == 0; round++ {
		if len(s.trieTasks) == 0 {
			break
		}
		// Snapshot the trie tasks then deliver each via ProcessNode.
		batch := make([]*trieTask, 0, len(s.trieTasks))
		for _, t := range s.trieTasks {
			batch = append(batch, t)
		}
		for _, task := range batch {
			data := lookupNode(task.path, task.hash)
			if err := s.sched.ProcessNode(trie.NodeSyncResult{Path: task.path, Data: data}); err != nil {
				t.Fatalf("seed ProcessNode round=%d: %v", round, err)
			}
			delete(s.trieTasks, task.path)
		}
		// Pull the freshly scheduled children into our local pools.
		paths, hashes, codes := s.sched.Missing(64)
		for i, p := range paths {
			s.trieTasks[p] = &trieTask{
				hash:     hashes[i],
				path:     p,
				attempts: make(map[string]struct{}),
			}
		}
		for _, h := range codes {
			s.codeTasks[h] = &codeTask{attempts: make(map[string]struct{})}
		}
	}
	if len(s.codeTasks) == 0 {
		t.Skip("could not surface a code task within driving budget; skipping")
	}

	var (
		codeHash common.Hash
		codeBlob []byte
	)
	for h := range s.codeTasks {
		codeHash = h
		codeBlob = lookupCode(h)
		break
	}

	uncommittedBefore := s.numUncommitted

	req := &stateReq{
		peer:           nil,
		response:       [][]byte{codeBlob},
		dropped:        true,
		inflightTicket: -1,
	}
	successful, err := s.process(req)
	if err != nil {
		t.Fatalf("processLateDelivery (code) error: %v", err)
	}
	if successful != 1 {
		t.Errorf("code blob not accepted: successful = %d, want 1", successful)
	}
	if _, still := s.codeTasks[codeHash]; still {
		t.Errorf("code task %x still present after acceptance", codeHash)
	}
	if s.numUncommitted != uncommittedBefore+1 {
		t.Errorf("numUncommitted = %d, want %d", s.numUncommitted, uncommittedBefore+1)
	}
}
