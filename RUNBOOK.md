# RUNBOOK — building, running and observing the exercise network

Everything here is a copy-paste command. Read `CANDIDATE-BRIEF.md` first for what the network
is supposed to do.

---

## 0. Prerequisites

| | why |
|---|---|
| **Linux or macOS** (on Windows: WSL2) | the harness is bash; it does not run under PowerShell or cmd |
| **Go 1.24+** | to build geth from the source you were given |
| **python3** | the harness uses it for genesis generation and for `status.sh` |
| **curl**, **git**, standard build tools | |
| ~4 GB free RAM, ~2 GB disk | four nodes on one machine |

Check:

```bash
go version        # want 1.24 or newer
python3 --version
```

Ports used: `30401`, `30411–30412`, `30415–30416`, `8611–8612`, `8615–8616`, `8645–8646`.
Make sure nothing else is on them.

---

## 1. Build geth from the source you were given

This is the binary under test. Do not skip it and do not substitute a release build.

```bash
cd go-ethereum
make geth
```

The binary lands at `go-ethereum/build/bin/geth`. Record its absolute path:

```bash
export GETH_BIN="$(pwd)/build/bin/geth"
echo "$GETH_BIN"
"$GETH_BIN" version | head -3
```

Now run the consensus unit tests for the package you will be editing:

```bash
go test ./consensus/XDPoS/engines/engine_v2/
```

**These are part of the exercise, not just a toolchain check.** Some rules this engine relies
on cannot be observed from a running network, so they are pinned here instead. Read any
failure carefully — it names the rule it is asserting. Treat it exactly like a defect you
found in the logs: diagnose the cause in the source, fix the source, and re-run.

Do not edit the tests to make them pass.

---

## 2. Create the network (once)

```bash
cd ../ItWorksinMyLocal
./setup.sh --new 4 --no-run
```

This generates **four fresh validator keys** into `.env` and writes a genesis seeded with
them. It takes about 30 seconds.

> `--no-run` matters. Without it `setup.sh` ends by launching its own 5-node topology on
> `nodes/1-4` — the same datadirs this exercise uses — and then blocks forever. Always pass
> `--no-run` here.

All four validators are onboarded **from genesis**. There is no staking step in this exercise
— nothing has to be proposed, funded or onboarded at runtime.

Then stamp the exercise's consensus parameters:

```bash
./configure-exercise-net.sh
```

Expected output:

```
exercise parameters stamped on genesis/genesis.json:
  period=1 epoch=900 gap=450 switchBlock=900
  v2 cfg[0]: minePeriod=1 certificateThreshold=0.667
  v2 cfg[1]: minePeriod=1 certificateThreshold=0.667
  validator keys imported: 4
  v1 signer set (extraData): 4 -> 2
    0x<legacy validator 1>
    0x<legacy validator 2>
  legacy nodes 1,2 initialised on genesis/genesis.json
```

This step also imports the four validator keys and initialises the legacy nodes, so the
parameters above actually reach them.

The `v1 signer set: 4 -> 2` line is expected and correct. Blocks 1–899 run under XDPoS v1,
which is a strict rotation: only the two legacy sealers take v1 turns. The four-member set
the network uses from block 900 comes from the masternode candidate list, not from this
line — see `CANDIDATE-BRIEF.md` §3.

> Run `configure-exercise-net.sh` **every time** you re-run `setup.sh`. `setup.sh` regenerates
> the genesis from scratch, which discards the stamped values.

---

## 3. Start the network

```bash
GETH_BIN="$GETH_BIN" ./run-exercise.sh
```

Expected output:

```
geth binary : /.../go-ethereum/build/bin/geth
             built 2026-09-26 11:04
             Geth
             Version: 1.17.5-xdc.8
bootnode    : enode://...@127.0.0.1:30401
legacy1     : rpc=8611 validator=0x...  log=./nodes/exercise-logs/legacy1.log
legacy2     : rpc=8612 validator=0x...  log=./nodes/exercise-logs/legacy2.log
geth1       : http=8615 validator=0x...  log=./nodes/exercise-logs/geth1.log
geth2       : http=8616 validator=0x...  log=./nodes/exercise-logs/geth2.log

started. validator set = 4 (2 legacy + 2 geth), quorum = 3.
v1 -> v2 switch at block 900 (~15 min at 1s blocks).
```

If it refuses to start saying `GETH_BIN is not set` or that the path is in the download cache,
that is deliberate — see §1.

---

## 4. Observe

### Height and peers, all nodes

```bash
./status.sh
```

Run it twice ~30 s apart. Height should climb by roughly 30.

### Block interval (expectation E5)

```bash
blk() { curl -s -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$1\",false],\"id\":1}" \
  http://localhost:8615 | python3 -c 'import json,sys;print(int(json.load(sys.stdin)["result"]["timestamp"],16))'; }

# average seconds per block between two heights
python3 -c "print(($(blk 0x3E8) - $(blk 0x3AC)) / 60.0, 'sec/block')"   # blocks 940 -> 1000
```

### Who produced which block (expectation E6)

```bash
for n in $(seq 950 1000); do
  curl -s -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$(printf '0x%x' $n)\",false],\"id\":1}" \
    http://localhost:8615 | python3 -c 'import json,sys;print(json.load(sys.stdin)["result"]["miner"])'
done | sort | uniq -c | sort -rn
```

Healthy: four addresses, each with a roughly equal count. A validator with a count of zero is
not participating.

### Votes and certificates (expectations E7, E8)

The geth nodes run at verbosity 4, so the vote path is visible in their logs.

```bash
L=./nodes/exercise-logs/geth1.log

grep -c 'collected vote'            "$L"   # votes this node received
grep -c 'Vote threshold reached'    "$L"   # times a quorum was reached
grep -c 'Successfully created QC'   "$L"   # certificates formed
```

Refusals and warnings — these are the interesting ones. The consensus engine tags each log
line with the function that emitted it, in square brackets, so scanning what the node
*complains* about tells you where to start reading:

```bash
grep -E 'WARN|ERROR' "$L" | tail -40          # everything the node is unhappy about
grep -oE '\[[a-zA-Z]+\]' "$L" | sort | uniq -c | sort -rn | head   # which functions are noisy
grep -i 'vote' "$L" | tail -40                # the whole vote path
```

If the chain is not progressing, the reason a node gives for *not* voting is usually the
fastest way in. Widen from there to whatever function name the bracket tag points at.

### Agreement / no fork (expectation E10)

```bash
for p in 8611 8612 8615 8616; do
  printf '%s ' "$p"
  curl -s -X POST -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x3E8",false],"id":1}' \
    http://localhost:$p | python3 -c 'import json,sys;r=json.load(sys.stdin)["result"];print(r["number"], r["hash"])'
done
```

All four lines must show the same hash for block `0x3E8` (1000).

### Live tail

```bash
tail -f ./nodes/exercise-logs/geth1.log
```

---

## 5. What healthy looks like

After block 900, on a correct build:

* `./status.sh` shows all four nodes climbing together, roughly 1 block per second.
* Block miners over any 200-block window include **all four** validator addresses.
* `Successfully created QC` appears continuously in the geth logs and keeps increasing.
* `Vote threshold reached` keeps pace with block production.
* Block 1000 has the same hash on all four nodes.
* Height at 20 minutes is comfortably past 1000 and still rising.

---

## 6. Reset and re-run from clean

After changing `vote.go` you **must** rebuild and start from an empty chain. A node that
already has blocks will not replay the code path you changed.

```bash
# 1. stop everything
pkill -f 'bin/oldxdc' || true
pkill -f "$GETH_BIN"  || true
sleep 3

# 2. wipe chain data and keystores
./reset.sh

# 3. rebuild your geth
( cd ../go-ethereum && make geth )

# 4. re-import keys, re-seat the signer set, re-init the legacy nodes
./configure-exercise-net.sh

# 5. start again
GETH_BIN="$GETH_BIN" ./run-exercise.sh
```

You do **not** need to re-run `setup.sh` between runs. `configure-exercise-net.sh` re-imports
the keys from `.env` and re-initialises the nodes, so the validator set stays the same across
resets. Re-running `./setup.sh --new` would generate brand-new keys and a brand-new validator
set, which is not what you want.

The chain restarts from block 0 every time, so budget another ~15 minutes to reach the switch.

---

## 7. Troubleshooting

| symptom | cause / fix |
|---|---|
| `GETH_BIN is not set` | §1 — build the source you were given and export the path |
| `GETH_BIN points into .../bin/geth/` | that is the harness's download cache (a stock release). Build from source instead |
| `no key in ./nodes/1 -- run: ./setup.sh --new 4` | `reset.sh` removed the keystores; run `./configure-exercise-net.sh` to re-import them |
| `setup.sh` prints node logs and never returns | you omitted `--no-run`. Ctrl-C, `./reset.sh`, and re-run it as `./setup.sh --new 4 --no-run` |
| Height stuck at 0, no peers | a stale bootnode or node is still running — redo step 1 of §6, then start again |
| `python3: command not found` | install python3; the harness needs it |
| Port already in use | another run is still alive; `pkill` as in §6 |
| geth logs show only header imports and it never mines | expected **before** block 900 — geth cannot seal v1 blocks. Wait for the switch |
| Everything looks fine and you are 10 minutes in | also expected. The voting code does not run until block 900 |
