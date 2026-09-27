# Debugging exercise — XDPoS v2 consensus

**Time budget: 2–3 hours.** You are not expected to have prior XDPoS knowledge. Everything you
need to reason about the protocol is in §3 of this document.

---

## 1. The problem

You have been given two things:

| | |
|---|---|
| `ItWorksinMyLocal/` | a harness that runs a small XDC blockchain on your machine |
| `go-ethereum/` | the source of the client that will act as two of the four validators |

The consensus voting logic in the `go-ethereum` tree contains **several intentionally
introduced issues**. The network was not tested after they were introduced. It does not behave
the way the specification in §4 says it should.

Your task: **investigate the network, identify the issues, determine their root causes,
implement appropriate fixes, and demonstrate that the network behaves correctly afterward.**

### Scope

Every issue you need to find is in this one file:

```
go-ethereum/consensus/XDPoS/engines/engine_v2/vote.go
```

This is given to bound your search, not to point at the defects — you are told the file, not
the functions, the lines, or what is wrong with them. Read anything else you like for context,
but you should not need to change code outside that file.

The chain configuration (`ItWorksinMyLocal/genesis/genesis.json`) is **correct**. Do not hunt
for defects there, and do not edit it.

**Not every issue causes an obvious runtime failure.** At least one of them will not announce
itself at all.

---

## 2. How to approach it

1. **Read the XDPoS overview in §3 first.** You are not expected to arrive knowing any of it.
2. **Build the provided Geth source.** Do not replace it with an upstream or downloaded binary
   — the launcher will refuse to start against one.
3. **Set up and start the network** using the provided scripts and `RUNBOOK.md`.
4. **Observe the network before changing any code.** Look at block production, block timing,
   validator activity, voting, quorum/QC formation, and the relevant log messages.
5. **Compare what you observe against the expected behaviour** in §4.
6. **Investigate the source** to determine *why* the observed behaviour occurs.
7. **Fix the underlying problems.** Do not work around symptoms by changing unrelated network
   parameters.
8. **Rebuild and restart from a clean state** after making changes.
9. **Verify your fixes experimentally.** Show that what was previously incorrect is now correct.
10. **Perform the code audit in §6 even if the network appears healthy.**

---

## 3. What you need to know about XDPoS

XDPoS is the consensus protocol XDC uses. Version 2 — the one you are debugging — is a
**HotStuff-style BFT protocol**. What follows is the whole of it that matters here.

### Validators (masternodes)

A **validator** — XDC calls them *masternodes* — is a node permitted to create blocks and to
vote on other nodes' blocks. The set of validators is fixed and known to every node. In your
network there are **4 validators**: two running the legacy client, two running the geth you
will build.

Nodes that are not validators simply follow the chain. They never vote.

### Block production

Validators take turns. In each turn one validator is the **proposer**: it builds a block and
broadcasts it. Turns rotate, so over any long stretch every validator should produce roughly
the same number of blocks.

### Rounds

Time is divided into **rounds**. One round is one attempt to add one block. If a round
succeeds, the chain grows by one block and the round number advances. If a round fails — the
proposer is offline, or not enough validators agree — the round **times out** and the next
round begins with the next proposer. Round numbers only ever go up.

### Voting

When a validator receives a proposed block and is satisfied with it, it broadcasts a **vote**:
a signature saying "I accept this block at this round."

Two rules govern voting, and both matter here:

1. **A validator votes at most once per round.** Signing two different blocks at the same
   round is called **equivocation**. It is the one thing a BFT protocol exists to prevent,
   because it is what allows two different blocks to both look accepted.
2. **A validator only votes for a block that builds on the block it is currently locked on** —
   unless the new block arrives carrying proof of agreement from a *later* round than the one
   it is locked on, in which case it is allowed to move forward.

### Quorum and the quorum certificate (QC)

Votes are collected. Once enough votes exist for the same block they are bundled into a
**quorum certificate (QC)**: a single object proving that a quorum of validators accepted that
block.

"Enough" is `validators × certificateThreshold`, rounded up. In your network:

```
4 validators × 0.667 = 2.668  ->  3 votes needed to form a QC
```

The threshold is **just over two-thirds**, and that is not an arbitrary number. Two-thirds is
the smallest quorum at which two conflicting QCs cannot both form: any two groups that large
must overlap in at least one validator, and an honest validator will not vote for both. Below
two-thirds, that guarantee disappears.

**No QC means no progress.** If votes never reach 3, no certificate forms and the chain stops
adding blocks.

### Chain parameters

| parameter | your value | meaning |
|---|---|---|
| `period` | 1 | seconds between blocks under v1 |
| `minePeriod` | 1 | seconds between blocks under v2. Must equal `period`, or block time changes at the switch |
| `epoch` | 900 | blocks per epoch. The validator set is re-applied at each epoch boundary |
| `gap` | 450 | the block at which the next epoch's validator set is snapshotted (always `epoch / 2`) |
| `switchBlock` | 900 | the block at which the chain switches from XDPoS **v1** to **v2** |

### The v1 → v2 switch

Blocks 1–899 run the **older v1 engine**: simple round-robin, no voting, no QCs. From block
**900** the **v2 engine** takes over and everything described above — rounds, votes, quorum
certificates — begins to apply.

Two consequences, both important:

- **Nothing you are looking for can happen before block 900.** The voting code is not running
  yet. A perfectly healthy-looking first 15 minutes tells you nothing at all.
- The geth client **cannot produce v1 blocks** — by design; its v1 blocks are rejected. So for
  blocks 1–899 the two legacy nodes produce everything while the two geth nodes follow. That is
  correct and expected, not a defect. From block 900 all four should participate.

At 1 second per block, block 900 arrives roughly **15 minutes** after you start the network.
Use that time to read the source.

---

## 4. Expected behaviour

This is your specification. Measure against it.

### Before block 900 (v1)

| # | expectation | how to check |
|---|---|---|
| E1 | Block height increases by about 1 per second | `./status.sh` twice, 30s apart |
| E2 | All four nodes report the same height (±2 blocks) | `./status.sh` |
| E3 | The two geth nodes follow but produce nothing | expected — not a defect |

### After block 900 (v2)

| # | expectation | how to check |
|---|---|---|
| E4 | **The chain keeps growing.** Height must not stop at or near 900 | `./status.sh` repeatedly |
| E5 | **Block interval stays about 1 second.** It must not change at the switch | compare timestamps of two blocks at least 60 apart |
| E6 | **All four validators produce blocks.** Across 200 blocks each should produce roughly a quarter; none should produce zero | collect block miners over a range |
| E7 | **Both geth nodes vote.** Their logs should show votes being sent, not refusals to vote | `grep` the geth logs |
| E8 | **QCs form continuously.** Certificates should accompany the chain's progress, not appear once and stop | `grep` the geth logs |
| E9 | **No validator votes twice at the same round.** Two different votes from one validator at one round number is a defect even when the chain looks perfectly healthy | reason about the code; the unit tests in §6 step 7 also pin this |
| E10 | All four nodes agree on the block hash at a given height — no fork | compare `eth_getBlockByNumber` across nodes |

**E9 deserves a warning.** A network can satisfy E4 through E8 completely — growing on time,
everyone producing, certificates forming — and still be broken in a way that matters more than
any stall. **A block-producing chain is not proof that the implementation is correct.** Check
the rules, not only the symptoms.

---

## 5. Setup

Exact commands are in **`RUNBOOK.md`**. In outline:

1. Build geth from the `go-ethereum/` tree you were given.
2. `./setup.sh --new 4 --no-run` — generate four validator keys and the genesis.
3. `./configure-exercise-net.sh` — stamp the network parameters, import the keys and
   initialise the legacy nodes.
4. `GETH_BIN=<your build> ./run-exercise.sh` — start all four nodes.
5. Watch with `./status.sh`; read logs under `nodes/exercise-logs/`.

You must run **your own build**. The launcher refuses to start against a downloaded release,
because a stock binary would not contain the code you are debugging.

---

## 6. Your task

1. Start the network from a clean state and let it pass block 900.
2. Record what actually happens, against E1–E10.
3. Find the cause of each deviation in `vote.go`.
4. Fix the underlying cause.
5. Rebuild, reset, re-run from a clean state, and show the network now meets the expectations.
6. **Then audit the file again.** Once the network is healthy, re-read `vote.go` and ask what
   is still wrong that a run would never have shown you. This step is required, and it is not
   optional just because the chain is producing blocks.
7. **Run the consensus unit tests and make them pass.**

   ```bash
   cd go-ethereum
   go test ./consensus/XDPoS/engines/engine_v2/
   ```

   Not every rule this engine relies on is observable from a running network. Some are
   properties of a single decision, and those are pinned by tests instead. A failing test
   here is a real defect in the same file you have been working in — diagnose it the same
   way, and explain the protocol rule it breaks.

   Do not edit the tests. If you believe a test is wrong, say so in your submission and
   explain why.

---

## 7. What you are NOT expected to know

You are not expected to already know:

* XDPoS internals
* XDC's implementation details
* where the seeded bugs are
* which lines contain incorrect code
* the intended fixes

§3 contains the protocol information required to reason about the expected behaviour. If you
find yourself needing a fact about XDPoS that is not in §3 and not derivable from the source,
say so in your submission — that is useful feedback, not a failure.

---

## 8. What you may use

* the provided source code and documentation
* Geth/XDPoS source-code comments, and the repository's own history
* standard programming documentation and publicly available technical documentation
* normal debugging tools, shell commands and log analysis
* internet search for general technical information

Diffing files against each other, reading git history, and comparing a function against a
similar one elsewhere in the repository are all legitimate and smart. Say so if you did. What
is graded is whether you can explain the protocol rule each issue breaks.

---

## 9. What you should not do

Do not:

* replace the provided consensus implementation with an unrelated implementation
* **bypass the consensus/voting logic to make the chain appear healthy** — for example forcing
  a vote-verification path to always succeed
* **remove or disable validators** because they are causing a failure, or shrink the validator
  set so a smaller quorum carries the chain
* **change network parameters to match the broken implementation** — lowering
  `certificateThreshold`, lengthening timeouts, or moving `switchBlock` so the problem is
  never reached
* modify unrelated components without explaining why
* treat a successfully block-producing chain as proof that the implementation is correct

Each of these can produce a green network without fixing anything. All of them count against
you, and a submission that reaches a healthy chain by any of these routes scores below one
that finds a single genuine defect and explains it.

---

## 10. Your submission

### 1. Bug report

One section per issue:

* what you found — file and line
* how you reproduced it
* **evidence**: logs, `status.sh` output, block or vote observations showing the problem
* the root cause: what the code does, and what it should do
* **classification: does it cost LIVENESS (the chain stops) or SAFETY (the chain keeps running
  but a guarantee it is supposed to provide no longer holds)?**
* the concrete failure — what an operator or an attacker could actually cause, and what the
  network would look like while it happened

If an issue produced no observable symptom, say so explicitly and explain how you found it
instead.

### 2. Code changes

* a diff of your changes to `vote.go`
* why each change corrects the underlying problem rather than hiding the symptom

### 3. Verification

* evidence the issue no longer occurs, from a **clean re-run**
* relevant block, vote and QC observations
* the output of `go test ./consensus/XDPoS/engines/engine_v2/`, passing
* any other tests or commands you used

### 4. Short summary

* what you learned
* any remaining limitations, uncertainties, or observations
* how confident you are that you found everything, and what you did not check

---

## Important

A correct solution is not:

> "The blockchain is running."

You should be able to explain **why it is running correctly**, and connect each observation to
the relevant protocol behaviour and the specific source code responsible.
