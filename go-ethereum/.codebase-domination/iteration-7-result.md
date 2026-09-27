# Iteration 7 — authorOverride hypothesis FAILED

## Hypothesis (Senior Developer · Software Architect)
v1.17.3 reset removed XDC's `authorOverride` from state_processor.go:
- xdc-network passed `header.Coinbase` directly to NewEVMBlockContext
- v1.17.3 passes nil → falls back to `engine.Author(header)`
- If Author returns a different address for V2 blocks, COINBASE opcode
  value differs, EVM execution diverges

## Fix attempted (commit ffe3353bb)
Restored authorOverride logic. Built clean. Deployed to xdc04.

## Result (Code Reviewer · Reality Checker)
**Identical merkle error:** `local: 5d4bf47a` (unchanged from before).
Same bytes computed for block 82,219,687's post-state root.

## Conclusion
COINBASE opcode value isn't the divergence source. Either:
- engine.Author(header) returns header.Coinbase for V2 blocks anyway
- The failing tx doesn't read COINBASE
- The divergence is purely in trie commit, not EVM execution (we knew this)

Reverted via commit 114930a77. Branch back to clean state at 90c95c523.

## Hypotheses ruled out so far (9 total)
H1 cache staleness · H3 walker non-determinism · H11 node reader returns wrong bytes
boundary >=hashLen · EVM execution · state diff · account RLP encoding · keccak swap
· **H9 authorOverride / COINBASE opcode**

## Remaining unaudited
- `triedb/pathdb/database.go` 190 lines diff
- `triedb/pathdb/verifier.go` 305 lines NEW
- `triedb/pathdb/history_indexer.go` 247 lines diff
- TRC21 fee routing (coinbaseOwner) — still missing but unlikely

## Parallel sync state (Senior Developer · DevOps + SRE)
- **xdc02 head: 23,779,190** — ONE BLOCK before TIPSigning fork (23,779,191)
- **xdc04 head: 10,168,053** — climbing
- Either reaching 82,219,687 will independently confirm the bug

## Next step (Sprint Prioritizer)
Watch xdc02 cross TIPSigning at 23,779,191 — pre-flight signal for whether
our binary handles the BlockSigners SelfDestruct correctly. If FAIL, we
have a P0 bug BEFORE the 82.2M divergence. If PASS, the only remaining
known divergence is at 82,219,687.
