package engine_v2

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Equivocation is a validator signing two different blocks at the same round.
// A BFT protocol's core safety guarantee is that this cannot happen: if two
// conflicting blocks can each gather a quorum, two conflicting certificates
// exist at one round and the chain can finalise a fork.
//
// XDPoS v2 enforces this with one rule:
//
//	a validator votes AT MOST ONCE per round.
//
// The engine tracks the highest round it has already voted in. A block offered
// at a round it has already voted in must be refused; a block offered at a
// later round must be accepted.
//
// These two tests pin both halves of that rule. Neither of them starts a
// network: equivocation is a property of the voting decision itself, and the
// decision is reachable directly.

// helper: an engine that has already cast its vote in `round`.
func engineHavingVotedAt(round types.Round) *XDPoS_v2 {
	return &XDPoS_v2{
		currentRound:      round,
		highestVotedRound: round,
	}
}

func blockAtRound(round types.Round, number int64, hash string) *types.BlockInfo {
	return &types.BlockInfo{
		Hash:   common.HexToHash(hash),
		Round:  round,
		Number: big.NewInt(number),
	}
}

// TestNoDoubleVoteInSameRound is the safety half of the rule.
//
// Scenario: the node has already voted in round 42. A SECOND, different block
// is then offered at round 42 — a competing proposal, which is exactly what an
// equivocating leader or a fork produces. The node must refuse.
//
// If this test fails, the node is willing to sign two different blocks at the
// same round. Nothing about the running chain will look wrong: blocks are
// produced, certificates form, every node stays in step. The guarantee is gone
// anyway.
func TestNoDoubleVoteInSameRound(t *testing.T) {
	const round = types.Round(42)

	x := engineHavingVotedAt(round)

	// The competing block, at the round we have already voted in.
	competing := blockAtRound(round, 1042, "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	qc := &types.QuorumCert{
		ProposedBlockInfo: blockAtRound(round-1, 1041, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}

	mayVote, err := x.verifyVotingRule(nil, competing, qc)
	if err != nil {
		t.Fatalf("verifyVotingRule returned an unexpected error: %v", err)
	}
	if mayVote {
		t.Fatalf("EQUIVOCATION: node already voted in round %d and was allowed to vote "+
			"again in round %d on a different block. A validator must vote at most once "+
			"per round; allowing a second vote lets two conflicting certificates form at "+
			"the same round.", round, round)
	}
}

// TestVoteAllowedInLaterRound is the liveness half of the same rule, and it is
// here on purpose: refusing every vote would satisfy the test above while
// halting the chain. The guard must reject only a REPEAT of a round already
// voted in, never a genuinely new round.
func TestVoteAllowedInLaterRound(t *testing.T) {
	const voted = types.Round(42)

	x := engineHavingVotedAt(voted)
	x.currentRound = voted + 1 // the round advanced; we have not voted in it yet

	next := blockAtRound(voted+1, 1043, "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	qc := &types.QuorumCert{
		ProposedBlockInfo: blockAtRound(voted, 1042, "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}

	mayVote, err := x.verifyVotingRule(nil, next, qc)
	if err != nil {
		t.Fatalf("verifyVotingRule returned an unexpected error: %v", err)
	}
	if !mayVote {
		t.Fatalf("node refused to vote in round %d although its last vote was in round %d. "+
			"Refusing new rounds stops the chain.", voted+1, voted)
	}
}
