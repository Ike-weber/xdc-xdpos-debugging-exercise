# XDPoS Fast Sync — Implementation Reference

A spec-style description of how fast sync currently works in this repo, written
so that a Geth-fluent engineer can re-implement it on top of upstream
go-ethereum. Cross-references file paths and the existing call sites.

---

## 1. Goals and trust model

Goals:

1. Allow a fresh XDPoS node to skip executing every block from genesis and
   instead reconstruct chain state from a known-good *pivot block*.
2. Sync state for every block the consensus engine will need to read state from
   after the pivot — not just the pivot itself (XDPoS-specific "gap pivots").
3. After the pivot, fall through to normal full sync (execute every block) so
   live chain progression is unchanged.

Trust model:

- The operator supplies a `(pivotNumber, pivotHash, pivotRoot)` triple via CLI
  or env vars. This triple is the **sole trust anchor** for the pre-pivot
  history.
- Pre-pivot headers are not signature-verified. Hash-chain linkage is still
  enforced inside `HeaderChain.ValidateHeaderChain`, so the chain leading to
  the verified `pivotHash` is uniquely determined (modulo SHA3 collisions).
- Post-pivot blocks are fully verified by `core.BlockChain.InsertChain`, same
  as full sync.

Concrete consequence: a wrong pivot hash → silent compromise. A wrong
intermediate header → caught by hash-chain linkage. See
`docs/FAST_SYNC_SECURITY.md` if you want the full threat analysis (not in this
file).

---

## 2. Configuration surface

### 2.1 CLI flags — `cmd/utils/flags.go`

```go
FastSyncPivotNumberFlag = &cli.Uint64Flag{
    Name:  "fastsyncpivotnumber",
    Value: 0,
}
FastSyncPivotHashFlag = &cli.StringFlag{
    Name:  "fastsyncpivothash",
    Value: "",
}
FastSyncPivotRootFlag = &cli.StringFlag{
    Name:  "fastsyncpivotroot",
    Value: "",
}
```

### 2.2 Validation rules (`SetEthConfig`)

All three flags are an atomic unit:

- If `pivotnumber` is set, both `pivothash` and `pivotroot` must also be set
  and non-empty. Otherwise → `Fatalf`.
- `pivotnumber == 0` is rejected when explicitly set.
- Hash/root strings are parsed via `common.Hash.UnmarshalText`. Malformed hex →
  `Fatalf`. Do not use `HexToHash`: it silently zero-pads bad input.
- Setting `pivothash` or `pivotroot` without `pivotnumber` is also `Fatalf`.

### 2.3 Config plumbing — `eth/ethconfig/config.go`

```go
FastSyncPivotNumber uint64
FastSyncPivotHash   common.Hash
FastSyncPivotRoot   common.Hash
```

### 2.4 Wiring — `eth/backend.go`

```go
if config.FastSyncPivotNumber != 0 {
    eth.protocolManager.downloader.SetPivotBlock(
        config.FastSyncPivotNumber,
        config.FastSyncPivotHash,
        config.FastSyncPivotRoot,
    )
}
```

### 2.5 Env var bridging — `cicd/*/start.sh`

`FASTSYNC_PIVOT_NUMBER`, `FASTSYNC_PIVOT_HASH`, `FASTSYNC_PIVOT_ROOT`. Shell
script enforces the same all-or-nothing rule before passing flags to the
binary.

---

## 3. Downloader-level changes — `eth/downloader/downloader.go`

### 3.1 Downloader struct additions

```go
// Fixed pivot config (set once before sync starts).
pivotNumber uint64
pivotHash   common.Hash
pivotRoot   common.Hash

// XDPoS-only: state-sync targets at epoch-switch boundaries before pivot.
pivotGapNumbers []uint64
pivotGapLock    sync.RWMutex
```

### 3.2 Tunable constants

```go
maxHeadersProcess      = 2048  // restored from Geth value; was 1 during debug
fsHeaderCheckFrequency = 0     // disable spot-checking pre-pivot
// fsHeaderForceVerify    deleted
```

### 3.3 SetPivotBlock — gap pivot derivation

`SetPivotBlock(number, hash, root)`:

1. If `cfg.XDPoS == nil`, return immediately (preserves upstream behaviour for
   non-XDPoS chains).
2. Store `(number, hash, root)`.
3. Compute gap pivots. Let `E = cfg.XDPoS.Epoch`, `G = cfg.XDPoS.Gap`.

   ```
   epochBase = number - (number % E)
   baseGap   = (epochBase >= G) ? epochBase - G : E - G   // underflow guard
   pivotGapNumbers = { baseGap + E*i  | i >= 0, baseGap + E*i < number }
   ```

4. Take `pivotGapLock.Lock()` while mutating `pivotGapNumbers`.

Rationale: XDPoS engine_v2 resolves the masternode set for a given block by
reading state at the *gap* block (`epochBoundary - Gap`). Without state at
those blocks, every epoch crossing post-pivot would fail. Gap blocks always
fall on `epochBase ± k*E - G`, hence the formula.

### 3.4 syncWithPeer — pivot selection

```go
pivot := uint64(0)
if mode == FastSync {
    if d.pivotNumber != 0 {
        pivot = d.pivotNumber          // configured: pin to operator value
    } else if height <= fsMinFullBlocks {
        origin = 0                     // legacy: too short for fast sync
    } else {
        pivot = height - fsMinFullBlocks
        if pivot <= origin {
            origin = pivot - 1
        }
    }
}
d.committed = 1
if mode == FastSync && pivot != 0 {
    d.committed = 0
}
// Already past the configured pivot? Skip fast-sync state phase entirely.
if mode == FastSync && d.pivotNumber != 0 && pivot <= origin {
    d.committed = 1
}
```

Note: in the configured-pivot path we do **not** rewind `origin` if
`pivot <= origin`. Instead we mark committed and let the run behave as full
sync. This avoids re-downloading state for a block we've already executed
past.

### 3.5 processHeaders — disabled spot verification

```go
frequency := fsHeaderCheckFrequency // == 0
// Geth's "force verify within fsHeaderForceVerify of pivot" branch deleted.
if n, err := d.lightchain.InsertHeaderChain(chunk, frequency); err != nil { ... }
```

With `frequency == 0`, `HeaderChain.ValidateHeaderChain`
(`core/headerchain.go:222`) still enforces:

- Numbering is contiguous (`number[i] == number[i-1] + 1`)
- Parent linkage (`ParentHash[i] == Hash(headers[i-1])`)
- Bad-hash denylist

It skips the `engine.VerifyHeader` seal check (the `seals[]` array is left
all-false).

### 3.6 processFastSyncContent — primary + gap state sync

State variables:

```go
syncedGaps       map[uint64]bool         // gap → done
pendingGapRoots  map[uint64]common.Hash  // gap → header.Root (from downloads)
pendingGapHashes map[uint64]common.Hash  // gap → header.Hash()
```

Flow:

1. **Initial state sync.** Target root selection:
   ```go
   root := latest.Root
   if (d.pivotRoot != common.Hash{}) { root = d.pivotRoot }
   sync := d.syncState(root)
   ```
   Use the configured root when present; fall back to the peer-reported head
   root for unconfigured runs (Geth behaviour preserved).

2. **Pivot selection inside the loop:**
   ```go
   var pivot uint64
   if height > fsMinFullBlocks { pivot = height - fsMinFullBlocks }
   if d.pivotNumber != 0       { pivot = d.pivotNumber }
   ```

3. **For each results batch from `d.queue.Results(...)`:**

   a. Under `pivotGapLock.RLock()`, scan results for any header whose number
      matches an unsynced gap, and record `header.Root` and `header.Hash()`
      into `pendingGapRoots` / `pendingGapHashes`.

   b. Pivot staleness check is **gated**: only execute the
      "Pivot became stale, moving" branch when `d.pivotNumber == 0`.
      Configured pivots are pinned.

   c. `splitAroundPivot(pivot, results)` returns `(P, beforeP, afterP)`.
      `commitFastSyncData(beforeP, sync)` writes pre-pivot blocks (no
      execution).

   d. If `P != nil` and pivot changed, cancel existing `sync` and restart on
      `P.Header.Root`.

4. **When primary state sync completes (`<-sync.done`):**

   a. If `d.pivotHash != 0`, assert `P.Header.Hash() == d.pivotHash`. Mismatch
      → fatal error returned up the stack, sync restarts on a different peer.

   b. If `d.pivotNumber != 0`, iterate `pivotGapNumbers` (under
      `pivotGapLock.RLock` → copy → release):

      - Pull `root` from `pendingGapRoots[gapNum]`; missing → error.
      - `gapSync := d.syncState(root); gapSync.Wait()`.
      - Open a `state.StateDB` on the just-downloaded root.
      - Call `d.generateSnapshot(statedb, gapNum, gapHash)`.

      After all gaps complete, clear `pivotGapNumbers` under write lock.

   c. `d.commitPivotBlock(P)` — unchanged Geth behaviour: writes pivot, flips
      head to it.

5. **`importBlockResults(afterP)`** — unchanged: full execution post-pivot.

### 3.7 generateSnapshot

```go
func (d *Downloader) generateSnapshot(statedb *state.StateDB, number uint64, hash common.Hash) error {
    candidates := statedb.GetCandidates()        // reads MasternodeVotingSMC storage slot 8
    var ms []utils.Masternode
    for _, candidate := range candidates {
        v := statedb.GetCandidateCap(candidate)  // slot 5.cap
        if !candidate.IsZero() {
            ms = append(ms, utils.Masternode{Address: candidate, Stake: v})
        }
    }
    xdc_sort.Slice(ms, func(i, j int) bool {
        return ms[i].Stake.Cmp(ms[j].Stake) >= 0
    })
    addrs := make([]common.Address, 0, len(ms))
    for _, m := range ms { addrs = append(addrs, m.Address) }

    snap := engine_v2.NewSnapshot(number, hash, addrs)
    return engine_v2.StoreSnapshot(snap, d.stateDB)   // rawdb.WriteXdposV2Snapshot
}
```

Required helpers:

- `state.StateDB.GetCandidates()` and `GetCandidateCap()` —
  `core/state/statedb_utils.go:92,119`. These read storage slots of
  `MasternodeVotingSMCBinary`.
- `xdc_sort.Slice` — deterministic stable sort wrapper used to avoid
  Go runtime sort instability across versions.
- `engine_v2.NewSnapshot` / `StoreSnapshot` — JSON-encodes a `SnapshotV2`
  blob keyed by `Hash` via `rawdb.WriteXdposV2Snapshot`.

If reimplementing on Geth, you need the analogue: a deterministic, persistent
key-value store keyed by gap block hash, and a way to enumerate the
"validator set at this block" from state.

### 3.8 State sync spindown — `eth/downloader/statesync.go`

`spindownStateSync` gained a `completed bool` parameter:

```go
func (d *Downloader) spindownStateSync(active map[string]*stateReq, finished []*stateReq,
        timeout chan *stateReq, peerDrop chan *peerConnection, completed bool) {
    if completed {
        for _, req := range active   { req.timer.Stop(); req.peer.SetNodeDataIdle(int(req.nItems), time.Now()) }
        for _, req := range finished { req.peer.SetNodeDataIdle(int(req.nItems), time.Now()) }
        return
    }
    // ...existing drain loop unchanged...
}
```

Call sites:

- `case next := <-d.stateSyncStart:` → `spindown(..., false)` (drain, next sync
  will reuse peers).
- `case <-s.done:` → `spindown(..., true)` (no further requests, just mark
  peers idle and return).

Reason: when state sync completes via `s.done`, there is no consumer for the
drain loop's `<-timeout` / `<-d.stateCh` reads — the original code could block
indefinitely on shutdown. Upstream Geth has since converged on a similar fix
(see PR #2278 commit message).

---

## 4. Consensus-engine support — what must change in XDPoS

### 4.1 engine_v1.verifyCascadingFields — `fullVerify=false` short-circuit

`consensus/XDPoS/engines/engine_v1/engine.go`. When `fullVerify=false`:

- Skip `snapshot(...)` lookup.
- Skip `checkSignersOnCheckpoint`.
- Skip `getSignersFromContract` fallback.
- Still call `verifySeal(chain, header, parents, fullVerify)`.

Rationale: these helpers read state and/or call into the masternode contract
via EVM. During fast sync neither is available pre-pivot.

### 4.2 engine_v2.getEpochSwitchInfo — soft-fail on snapshot miss

`consensus/XDPoS/engines/engine_v2/epochSwitch.go`. The previous error path
when `getSnapshot` failed is downgraded:

```go
snap, err := x.getSnapshot(chain, h.Number.Uint64(), false)
if err != nil {
    log.Warn("[getEpochSwitchInfo] ...cannot get standbynodes", "err", err)
    // standbynodes stays empty; continue
} else {
    // ...compute standbynodes from snap.NextEpochCandidates
}
```

This is the only path that consults the snapshot for the **standby** list;
the **active** masternode list comes from the header's `Validators` /
`Penalties` fields directly. Empty standbynodes is a benign degradation.

### 4.3 engine_v2.verifyQC / getEpochSwitchInfo — accept `[]*Header` parents

To make `maxHeadersProcess = 2048` viable, batch header verification must
avoid one-DB-read-per-header. Signature changes:

```go
// before
func (x *XDPoS_v2) verifyQC(chain ChainReader, qc *types.QuorumCert, parentHeader *types.Header) error
func (x *XDPoS_v2) getEpochSwitchInfo(chain ChainReader, header *types.Header, hash common.Hash) (*types.EpochSwitchInfo, error)

// after
func (x *XDPoS_v2) verifyQC(chain ChainReader, qc *types.QuorumCert, parents []*types.Header) error
func (x *XDPoS_v2) getEpochSwitchInfo(chain ChainReader, headers []*types.Header, hash common.Hash) (*types.EpochSwitchInfo, error)
```

Recursive lookups for `parent.ParentHash` walk backward in the `parents`
slice first, only hitting `chain.GetHeaderByHash` when off the end. All other
call sites pass `[]*types.Header{header}`.

---

## 5. Test surface

`eth/downloader/downloader_test.go`:

- Gap-pivot derivation correctness (boundary cases around `epochBase < G`).
- `processFastSyncContent` syncs each gap before primary.
- `generateSnapshot` produces one snapshot per gap, persisted under the gap
  hash.
- `pivotHash` mismatch returns the formatted error and does not commit pivot.
- `pivotRoot=0` falls back to `latest.Root` (legacy path preserved).

`statesync` test (PR #2278): completed-spindown returns within a small
deadline and resets peer idleness counters.

---

## 6. Re-implementation checklist for vanilla Geth

If you start from `ethereum/go-ethereum` and want the same behaviour:

1. **Add three fields to `eth/ethconfig.Config`** and corresponding
   `Uint64Flag` / `StringFlag` entries in `cmd/utils/flags.go`. Wire them
   through `SetEthConfig` with the all-or-nothing validation.

2. **Add `Downloader.SetPivotBlock(number, hash, root)`**:
   - Stash `(number, hash, root)` on the downloader.
   - If your chain has a notion of "gap blocks" (Geth doesn't, but a
     PoS/BFT fork might), compute and stash gap numbers under a lock.

3. **In `Downloader.syncWithPeer`** (Geth equivalent: `synchronise`/`spawnSync`
   path):
   - When `pivotNumber != 0`, force `pivot = pivotNumber` and skip Geth's
     dynamic calculation.
   - When configured `pivot <= origin`, set `committed = 1` to bypass the
     state-sync phase.

4. **In `processFastSyncContent`** (Geth: `processSnapSyncContent` for snap
   sync, but the architecture is similar):
   - Use `pivotRoot` as the state-sync target when set.
   - Gate the "pivot became stale, moving" branch behind `pivotNumber == 0`.
   - On `<-sync.done`, verify `P.Header.Hash() == pivotHash` and fatal on
     mismatch.
   - If you have gap pivots, run a serial `syncState(root); Wait()` loop for
     each, then materialise whatever consensus-level digest you need
     (`generateSnapshot` analogue) and store it.

5. **In `processHeaders`:**
   - Set `fsHeaderCheckFrequency = 0` (or whatever value matches your trust
     model) and remove the "force-verify near pivot" branch. Geth's
     `HeaderChain.ValidateHeaderChain` will still enforce parent linkage with
     `checkFreq = 0`.

6. **In `runStateSync` / `spindownStateSync`:**
   - Pass a `completed bool` flag through from the `<-s.done` case and short-
     circuit the drain loop when set (the actual upstream Geth fix is
     equivalent — pull from there if available).

7. **Consensus engine `VerifyHeader` paths:**
   - Add a `fullVerify` or `seal bool` path that skips state-reading
     validators when called during fast sync.
   - Make recursive lookups accept a `parents []*Header` slice so batched
     verification doesn't hit the DB per header. This is the unlock for
     `maxHeadersProcess >> 1`.

8. **Tests:** mirror `downloader_test.go` and `statesync` tests.

9. **Operator tooling:** publish `(number, hash, root)` triples for your
   network from a trusted channel. Without a trustworthy distribution
   mechanism, the security guarantees of the whole approach collapse.

---

## 7. File index

Code touched, relative to repo root:

- `eth/downloader/downloader.go` — pivot fields, `SetPivotBlock`,
  `syncWithPeer` pivot path, `processFastSyncContent` gap logic,
  `generateSnapshot`, header-verification constants.
- `eth/downloader/statesync.go` — `spindownStateSync` `completed` parameter.
- `eth/downloader/downloader_test.go` — gap-pivot tests, mismatch tests.
- `eth/backend.go` — `SetPivotBlock` call.
- `eth/ethconfig/config.go` — three new fields.
- `cmd/utils/flags.go` — three flags + strict validation in `SetEthConfig`.
- `cicd/{mainnet,testnet,devnet,local,docker}/start.sh` — env var bridging.
- `consensus/XDPoS/engines/engine_v1/engine.go` — `fullVerify=false`
  short-circuit.
- `consensus/XDPoS/engines/engine_v2/epochSwitch.go` — soft-fail snapshot
  miss; accept `[]*Header` parents.
- `consensus/XDPoS/engines/engine_v2/engine.go` — `verifyQC([]*Header)`.
- `consensus/XDPoS/engines/engine_v2/snapshot.go` — `NewSnapshot`,
  `StoreSnapshot` (used by `generateSnapshot`).
- `core/state/statedb_utils.go` — `GetCandidates`, `GetCandidateCap`
  (read from masternode SMC storage).
