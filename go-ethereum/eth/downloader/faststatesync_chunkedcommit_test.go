// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// A.55: Chunked commit + checkpoint resume. The A.42 mechanism already
// persists the fast-sync checkpoint on a 60s wall-clock ticker + at graceful
// shutdown. A.55 adds a *processed-count* trigger so a crash-restart that
// happened to fall between ticker fires (or before the first 60s elapsed)
// also has a recent checkpoint to resume from. The processed-count trigger
// fires every fastSyncCheckpointEveryN nodes — empirically tuned so the
// checkpoint write amortises (~one LDB Put per ~10s of sync at A.14c rates)
// while bounding crash-recovery work to ≤ N nodes.
//
// Refs A.55 / #857.

package downloader

import (
	"testing"
)

// TestChunkedCommitResume_TriggerPredicate is the focused unit test for the
// chunked-checkpoint trigger. The full integration test (drive a real
// fastStateSync to chunk count + crash-restart + resume) would require a
// trie-sync harness; the trigger predicate captures the load-bearing
// arithmetic and is the regression sentinel against off-by-one or
// "every iteration" mistakes.
func TestChunkedCommitResume_TriggerPredicate(t *testing.T) {
	tests := []struct {
		name      string
		prev      uint64 // processed count BEFORE this update
		next      uint64 // processed count AFTER this update
		wantFlush bool
	}{
		{"no progress", 0, 0, false},
		{"below first chunk", 0, 1000, false},
		{"at first chunk boundary", 0, fastSyncCheckpointEveryN, true},
		{"just past first chunk", 0, fastSyncCheckpointEveryN + 1, true},
		{"across first chunk in one update", 9999, 10001, true},
		{"sitting just before second chunk", fastSyncCheckpointEveryN, 2*fastSyncCheckpointEveryN - 1, false},
		{"at second chunk boundary", fastSyncCheckpointEveryN, 2 * fastSyncCheckpointEveryN, true},
		{"big jump across multiple chunks", 5, 5*fastSyncCheckpointEveryN + 7, true},
		{"already past, no new chunk crossed", fastSyncCheckpointEveryN + 5, fastSyncCheckpointEveryN + 9, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := crossedCheckpointChunk(tc.prev, tc.next)
			if got != tc.wantFlush {
				t.Fatalf("crossedCheckpointChunk(prev=%d, next=%d) = %v, want %v",
					tc.prev, tc.next, got, tc.wantFlush)
			}
		})
	}
}

// TestChunkedCommitResume_ConstantSanity guards against the constant being
// changed in a way that breaks the chunking ergonomics. fastSyncCheckpointEveryN
// should be:
//   - large enough to avoid LDB write amplification (1 Put / 10s)
//   - small enough to bound crash-recovery loss (≤ a few seconds of work)
// 10000 was chosen empirically and any change should be deliberate.
func TestChunkedCommitResume_ConstantSanity(t *testing.T) {
	if fastSyncCheckpointEveryN < 1000 {
		t.Errorf("fastSyncCheckpointEveryN=%d too small: LDB write amplification risk", fastSyncCheckpointEveryN)
	}
	if fastSyncCheckpointEveryN > 1_000_000 {
		t.Errorf("fastSyncCheckpointEveryN=%d too large: crash-recovery loss risk", fastSyncCheckpointEveryN)
	}
}
