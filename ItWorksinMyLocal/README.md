# Local XDPoS (DPoS) Network — "It Works in My Local"

A self-contained **4-node local [XDPoS](https://github.com/XinFinOrg/XDPoSChain)**
delegated-proof-of-stake network you can spin up in seconds — generate keys,
build a genesis, run 4 mining nodes + a bootnode, and let other machines join.

> **Non-technical?** Start with **[SETUP.md](SETUP.md)** (plain-language guide).
> **Want a script cheat-sheet?** See **[SCRIPTS.md](SCRIPTS.md)**.

## Contents
- [Requirements](#requirements)
- [Two ways in: start a network, or join one](#two-ways-in-start-a-network-or-join-one)
- [Choosing a client](#choosing-a-client)
- [Quick start](#quick-start)
  - [Run every client against it](#run-every-client-against-it)
- [Everyday commands](#everyday-commands)
- [Generating a genesis](#generating-a-genesis)
- [Joining from another machine](#joining-from-another-machine)
- [What's in this repo](#whats-in-this-repo)
- [Network details](#network-details)
- [Troubleshooting](#troubleshooting)
- [The puppeth fix](#the-puppeth-fix)

---

## Requirements

Works on **macOS** and **Linux / Ubuntu**. Node binaries are **downloaded
prebuilt on first run** (verified by sha256 against `binaries.json`) and cached
under `bin/<client>/<os>-<arch>/`. If no prebuilt exists for your platform,
`oldxdc`/`geth`/`erigon` fall back to a source build automatically, cloning the
source repo beside this one; `besu`/`nethermind`/`reth`/`xone` are download-only
unless you point the scripts at your own binary. Force a build with
`BIN_SOURCE=build` (or per client, `BIN_SOURCE_erigon=build`).

**No GitHub account is needed.** All eight clients — including `oldxdc`, the
default — download anonymously from the public mirror and are verified by sha256.
On a fresh Ubuntu box you need only `git`, `curl`, `python3` and `tar`.

**Two host prerequisites, if you want every client to come up.** Both fail
quietly rather than loudly, so they are worth checking before you debug anything
else:

| need | why | what you see without it |
|---|---|---|
| **~12 GB free disk** | nethermind's HealthChecks plugin enforces a free-space floor | nethermind initialises, prints its banner, then shuts down *gracefully with no error line*. The reason is one line earlier: `Not enough free disk space in '/' to safely run a node - please provide at least 11.51 GB`. Budget more than the floor: the clients themselves need ~2.5 GB of binaries plus chain data |
| **Java 21+ on `PATH`** (or `JAVA_HOME`) | besu's archive bundles a **linux/amd64** JRE, unusable on macOS; the launcher falls back to host Java | `besu -> besu --version failed (its bundled JRE is broken, or set BESU_BIN)` and besu is skipped. With a JDK 21 present it joins normally |

Everything else downloads and runs with no further setup.

The [GitHub CLI](https://cli.github.com/) (`gh`) is required **only** if you force
a source build (`BIN_SOURCE=build`), because the client *source* repos are
private. The normal download path never touches it.

### Quick start on a fresh Ubuntu box

```bash
./setup.sh --new 4 --all --chainid 20931 --name mylab
```

That is 4 `oldxdc` sealers plus one sync-only follower per modern client. Pick any
unused `--chainid`. `--name` labels every node on this machine; omit it and your
hostname is used.

> **On a brand-new clone, expect only `oldxdc` and `geth` to start.** `setup.sh`
> pre-builds just those two, so the other followers are reported as
> `binary not found` and skipped — the run still succeeds, it is simply smaller
> than the full set. To fetch the rest, either run `./join.sh --client all`
> (which downloads each client on demand), or re-run the command above once
> `bin/` is populated. The skip lines name every client that was left out and
> why, so you can tell this apart from a failure.

Each node's identity is composed for you as

```
<machine>-<client>-v<version>-<commit>-<nodetype>-<ip>
```

so a dashboard row tells you which box, which client, which build, what the node
is for, and where to reach it. Two nodes on the same released version but
different commits stay distinguishable — which matters, because a stale binary
otherwise looks identical to a current one.

Watch it come up:

```bash
./status.sh                # heights and peers per node
tail -f nodes/5-*.log      # one follower's log
```

Every follower should converge on the sealers' height **and the same genesis
hash**. Two failure shapes are worth knowing, because both look like "it hangs":

- **A client sits at block 0** — it never found a peer, or it rejected the genesis.
- **A client stalls at block 16** — block 16 is the first block carrying `0x89`
  BlockSigners `sign()` transactions, so it is where a gas or fork-activation
  disagreement first becomes visible.

Neither is normal. Please file an issue rather than working around it.

### Which clients `--all` starts

`geth`, `geth4`, `erigon`, `xone`, `besu`, `nethermind`, `reth`, as followers
beside the 4 sealers. Any client whose binary is missing or unrunnable is
**skipped and reported**, so a partial `bin/` degrades gracefully rather than
failing. Narrow it explicitly when you want to:

```bash
ALL_CLIENTS="geth erigon" ./setup.sh --new 4 --all --chainid 20931
```

Install the build toolchain once:

```bash
# Linux / Ubuntu  (Go 1.23+ required — apt's 'golang' is usually too old)
sudo apt update && sudo apt install -y build-essential git
wget https://go.dev/dl/go1.23.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin        # add to ~/.profile to persist

# macOS
xcode-select --install     # C toolchain; install Go 1.23+ from https://go.dev/dl or `brew install go`
```

Then just run `./setup.sh --new` — it clones the source (if needed) and builds.
`openssl`, `python3`, `curl`, and `bash` are also required (present by default
on both). Force a rebuild anytime with `REBUILD=1`.

**Source options** for the default `oldxdc` client (override the auto-clone with
`XDPOS_REPO=`, or point `XDPOS_DIR=` at an existing checkout):

- **Default — `XDCIndia/OLDXDC`**: already carries the `puppeth` genesis fix and
  matches the version the scripts expect.
- **Upstream** `XinFinOrg/XDPoSChain` pinned to `68b00c7e5`: `lib.sh` auto-applies
  the storage fix at build time (`patch_puppeth`); the pin keeps the wizard
  prompts in sync with `gen-genesis.sh`.

> Why: upstream `puppeth` has a genesis-storage bug (see
> [The puppeth fix](#the-puppeth-fix)) and its wizard prompts have since drifted.
> `OLDXDC` (or the pinned + auto-patched upstream) avoids both.

### Prebuilt binaries vs local build

By default the scripts **download** a prebuilt, platform-matched binary from
[`xdc.network/snapshots/bin`](https://xdc.network/snapshots/bin/) instead of
building from source — no Go/Java/.NET/Rust toolchain needed, and no multi-minute
build. Which binary to fetch (and its `sha256`, verified before use) is recorded
in [`binaries.json`](binaries.json); bump its `updated` tag / hashes to track new
uploads. Downloads land in `bin/<client>/<os>-<arch>/` (git-ignored), the same
place a source build would.

Control it with `BIN_SOURCE`:

| Setting | Effect |
|---------|--------|
| `BIN_SOURCE=download` (default) | download the prebuilt binary; fall back to a source build if none exists for this platform |
| `BIN_SOURCE=build` | always build from source (oldxdc/geth/erigon), skip the download |
| `BIN_SOURCE_<client>=build` | per-client override, e.g. `BIN_SOURCE_erigon=build` |
| `REBUILD=1` | force a fresh build/redownload, ignoring any cache |

Notes: `oldxdc` (XDC + bootnode + puppeth) is served from the xdc.network mirror
like every other client, so it needs no credentials. It keeps a `gh release`
fallback for the case where the mirror is behind, and still builds from source
with `BIN_SOURCE=build` (which does need `gh`, since the source repo is private).
`erigon` is published for `linux-amd64` and darwin (universal), so it downloads on
macOS too. `besu`/`reth`/`nethermind` are download-only here (the scripts
don't build them) — an explicit `BESU_BIN` / `RETH_BIN` / `NETHERMIND_DIST` (or a
binary on `PATH`) still takes priority over the download. The nethermind prebuilt
does not yet ship the `xdc-devnet5151` chainspec, so joining devnet 5151 with
nethermind still needs a custom `NETHERMIND_DIST` (join.sh says so if missing).

---

## Two ways in: start a network, or join one

Everything in this repo is one of these two. Pick the row you want and run the
command; both work from a clean clone with no extra setup beyond
[Requirements](#requirements).

### A. Start your own network

```bash
./setup.sh --new --all
```

Generates 4 fresh validator keys into `.env`, derives a matching genesis, wipes
old node data, and launches 4 `oldxdc` sealers **plus one follower per modern
client**. Leave it running; `Ctrl-C` stops everything.

```bash
./setup.sh --new                          # sealers only (no modern followers)
./setup.sh --new --all --chainid 747      # pick the chain id
./setup.sh --new --all --network HM       # name the chain (shown by network-info.sh)
./setup.sh                                # reuse the keys already in .env
```

On bring-up it prints the dashboard link for the network you just started:

```
Skynet stats: https://skynet.xdcindia.com/stats?net=<chainId>
```

### B. Join a network that already exists

`join.sh` attaches a **sync-only** node. Three cases:

```bash
# 1. a public XDC network — genesis and bootnodes are vendored, nothing else needed
./join.sh --network mainnet   --client geth      # chainId 50
./join.sh --network testnet   --client geth      # apothem, chainId 51
./join.sh --network devnet5151 --client geth     # the default if --network is omitted

# 2. a local net running on THIS machine (started by setup.sh/run.sh above)
./join.sh --client reth --genesis ./genesis/genesis.json --chainid <id> --ip 127.0.0.1

# 3. a net on ANOTHER machine — you need its genesis.json and its IP
./join.sh --client geth --genesis /path/to/genesis.json --ip <HOST_IP>
```

Swap `--client` for any of `oldxdc | geth | geth4 | erigon | reth | besu |
nethermind | xone`, or `--client all` to attach one of each at once. See
[Joining from another machine](#joining-from-another-machine) for ports,
`--mine`, and the other flags.

**Which one do I want?** If you are evaluating clients or testing consensus
changes, start your own (A) — you control the genesis and the validator set. Use
(B) only to follow a chain someone else is producing.

---

## Choosing a client

Seven node clients are selectable with `--client` (or `CLIENT=`), joining **devnet
5151** with `./join.sh --client <name>`. By default every client is fetched as a
**prebuilt binary** (see [Prebuilt binaries vs local build](#prebuilt-binaries-vs-local-build));

**Boot every client at once** with `--client all`:

```bash
./join.sh --client all            # all seven clients join devnet 5151 together
```

This fans out to one process per client — each in its own datadir
(`nodes/<client>-<nodetype>`, e.g. `nodes/geth-follower`) with a distinct port set,
all dialing the same
genesis + bootnodes. `Ctrl-C` stops them all. Any shared flag is forwarded
(`--genesis`, `--ip`/`--bootnodes`, `--mine`, `--ethstats`/`--no-ethstats`, …). A
client whose extra inputs aren't provided (`erigon` needs `--chain`; `nethermind`
needs a `NETHERMIND_DIST`) prints its own hint and is skipped while the rest keep
syncing.

`oldxdc/geth/erigon` build from source when a prebuilt isn't available (or with
`BIN_SOURCE=build`), while `besu/nethermind/reth` (separate Java/.NET/Rust
toolchains) are download-only unless you point `join.sh` at your own binary via
`BESU_BIN` / `RETH_BIN` / `NETHERMIND_DIST` (or `PATH`).

| Client | Source repo | Binary source | Status (devnet 5151) |
|--------|-------------|---------------|--------|
| `oldxdc` (default) | [`XDCIndia/OLDXDC`](https://github.com/XDCIndia/OLDXDC) (XDPoSChain) | download (GitHub release) or build | local run + genesis + seal ✅; **5151 join: 0 peers observed — under investigation ([#13](../../issues/13))** |
| `geth` | [`XDCIndia/go-ethereum`](https://github.com/XDCIndia/go-ethereum) (GP5) | download or build | **join/sync ✅ verified** (~400 blk/s); seal TODO; full-sync past the mid-history epoch switch needs the xdcSyncer fix |
| `erigon` | [`XDCIndia/erigon-xdc`](https://github.com/XDCIndia/erigon-xdc) | download (linux-amd64 + darwin universal) or build | syncs a built-in `--chain xdc\|xdc-apothem` **or a custom genesis file** — v3.5.0+ loads `--chain <path>` directly, so [#9](../../issues/9) no longer applies |
| `besu` | [`XDCIndia/besu-xdc`](https://github.com/XDCIndia/besu-xdc) | download or `BESU_BIN`/PATH | reads geth genesis natively; large prebuilt (**slow download**) and needs host **Java 21+** (the bundled JRE is linux/amd64) |
| `nethermind` | `XDCIndia/xdc-nethermind-private` (private — no public repo) | download or `NETHERMIND_DIST`/PATH | **custom `NETHERMIND_DIST` required for 5151** — the prebuilt archive doesn't ship `chainspec/xdc-devnet5151.json` |
| `reth` | [`XDCIndia/reth`](https://github.com/XDCIndia/reth) | download or `RETH_BIN`/PATH | reads geth genesis natively. Follows a private XDPoS net to tip with state-root parity **only from the build carrying the epoch-reward fix** — see the warning below |
| `xone` | [`XDCIndia/xOneGo`](https://github.com/XDCIndia/xOneGo) | download (mirror, GitHub-release fallback) | Go XDPoS reference client. Follows a private net to tip, and seals in exact fair share when seated as a validator |

### Binary acquisition — verified 2026-07-29, linux/amd64

From a **completely fresh clone**, `download_bins()` succeeds and the sha256 verifies
for **all seven** clients:

| Client | Result | Source actually used |
|---|:---:|---|
| `oldxdc` | ✅ | xdc.network mirror (`gh release` kept only as a fallback) |
| `geth` | ✅ | xdc.network mirror |
| `erigon` | ✅ | xdc.network mirror |
| `reth` | ✅ | xdc.network mirror |
| `besu` | ✅ | xdc.network mirror (large — expect a slow download) |
| `nethermind` | ✅ | xdc.network mirror |
| `xone` | ✅ | xdc.network mirror |

Reproduce it per client with:

```bash
CLIENT=<name> bash -c 'source ./lib.sh; download_bins'
```

This checks only that you get a verified binary — it is deliberately separate from
whether that client then *syncs*, which is the table above.

> **If a download reports `sha256 mismatch … (mirror may be stale)`** the manifest and
> the mirror have drifted. That is the failure mode to expect after a binary is
> republished without `binaries.json` being updated in the same change; the fetch is
> then correctly refused rather than trusted. Clients with a `ghRelease` entry
> (`oldxdc`, `xone`) recover automatically via `gh release download`; the others fail
> loudly. If you hit it, open an issue rather than working around the checksum — the
> check is the only thing standing between you and a silently substituted binary.

> **reth: use a build newer than 2026-07-29.** Builds before XDCIndia/reth#265
> mis-split the epoch reward when a masternode's registered owner is the foundation
> wallet, which diverges the state root at the **first reward block** and then stalls
> the node permanently — while it still looks healthy (process up, peers connected,
> RPC answering a *stale* head). The published `reth-linux-amd64` was refreshed to a
> fixed build on 2026-07-29; if you pinned an older one, replace it.

**Local network (own `run.sh` net, chainId 20118).** A follower joining your own
local net was also tested (macOS/arm64):

| Client | Local sync | Notes |
|--------|:---:|-------|
| `oldxdc` | ✅ | it *is* the local net (4 sealers); downloads a darwin tarball, no source build |
| `geth` | ✅ | full block-for-block parity incl. the v1→v2 switch |
| `geth4` | ✅ | the 1.17.4 pin — **linux-amd64 only**, so it is skipped on darwin |
| `erigon` | ✅ | full parity incl. the switch |
| `reth` | ✅ | since the EIP-1559-from-genesis fix (XDCIndia/reth#278) |
| `nethermind` | ✅ | chainspec is auto-converted from `--genesis` for any chainId. **Needs the free-disk floor below** — under it, it initialises, prints its banner and shuts down *cleanly with no error* |
| `besu` | ✅ | **needs host Java 21+**; the archive's bundled JRE is linux/amd64 and is moved aside on other platforms |
| `xone` | ✅ | since the genesis-config detection fix (XDCIndia/xOneGo#715) |

So on a stock macOS/arm64 host **all seven client types sync a local network** —
measured on a clean clone: 4 oldxdc sealers plus geth, erigon, reth, nethermind,
besu and xone all at the same height, with no source build. `geth4` is the only
entry skipped, for want of a darwin artifact.

```bash
./setup.sh --new                 # default oldxdc — runs the 4-node network
./setup.sh --client geth         # builds the modern geth client
CLIENT=geth ./gen-address.sh 2   # any script honors CLIENT=…

# setup.sh's own --client is limited to oldxdc|geth (it builds the sealer set).
# To attach any of the seven clients to a network, use join.sh:
./join.sh --client xone          # or geth|erigon|besu|nethermind|reth|oldxdc
./join.sh --client all           # one process per client, all seven at once
```

Binaries are kept separate per client under `bin/<client>/<os>-<arch>/`, so the
two never clash. The modern `geth` fork uses a different run model (no separate
bootnode, `--http.*` flags, its own genesis flow); building works today, but
launching a local net with it isn't wired yet.

## Quick start

From this folder, one command brings up a brand-new network (default `oldxdc`):

```bash
./setup.sh --new
```

This: generates 4 fresh keys into `.env` → derives the validator set from them →
builds a matching genesis → wipes old data → starts the bootnode + 4 mining
nodes. Leave the window open (it keeps printing block logs). **Stop with
`Ctrl + C`.**

Check it's producing blocks (in another terminal):

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:8545
```

The number after `0x` should climb (a block every ~2s).

Review what you built (validators, roles, balances):

```bash
./network-info.sh
```

### Run every client against it

The network above is sealed by 4 `oldxdc` nodes. To attach the other clients as
followers — all seven in one command. **Point them at your local net explicitly:**

```bash
./join.sh --client all \
  --genesis ./genesis/genesis.json \
  --ip <YOUR_IP>:<BOOTNODE_PORT> \
  --chainid <YOUR_CHAINID>
```

Or one at a time:

```bash
./join.sh --client geth --genesis ./genesis/genesis.json --ip <YOUR_IP>:<BOOTNODE_PORT> --chainid <YOUR_CHAINID>
#          oldxdc | geth | geth4 | erigon | reth | besu | nethermind | xone
```

> **Do not omit those flags here.** With no `--genesis`/`--ip`, `join.sh` falls back
> to `network.env`, which points at the **public devnet 5151** — so a bare
> `./join.sh --client all` will happily sync the public devnet instead of the
> network you just created, and it *succeeds*, so nothing looks wrong. join.sh now
> warns when it spots a local `genesis/genesis.json` and you have not asked for it,
> but the flags are the fix. `<BOOTNODE_PORT>` is `30301` unless you overrode it.

Each client gets its own datadir (`nodes/<client>-<nodetype>`) and port set, all
dialing the same genesis and bootnodes. `Ctrl-C` stops them together. A client
missing an extra input prints its own hint and is skipped while the rest keep
syncing, so one broken client never blocks the run.

Two things worth knowing before you interpret the output:

- **`setup.sh --client` only takes `oldxdc|geth`** — it builds the sealer set.
  `join.sh` is what accepts all seven, or `all`.
- **All seven clients sync a local net on macOS/arm64**, given the two host
  prerequisites in [Requirements](#requirements) (free disk and Java 21). Anything
  whose binary is missing or unrunnable is skipped **with a reason** rather than
  taking the topology down, so read the `Skipped:` block if you see fewer.

Confirm a follower is actually keeping up — compare its height against a sealer
rather than trusting its logs:

```bash
for p in 8545 <follower-rpc-port>; do
  curl -s -X POST -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
    http://localhost:$p
done
```

> `reth` answers JSON-RPC in **snake_case** (`state_root`, not `stateRoot`) and may
> return `number` as an integer. If you script a comparison, read both spellings —
> a missing field must not be mistaken for agreement.

---

## Everyday commands

| Goal | Command |
|------|---------|
| Start fresh (new keys) | `./setup.sh --new` |
| Start reusing current `.env` keys | `./setup.sh` |
| Just launch (keys + genesis already exist) | `./run.sh` |
| See the network summary | `./network-info.sh` |
| Wipe chain data (keep keys) | `./reset.sh` |
| Start over after a change | `./reset.sh && ./setup.sh` |
| Generate keys only | `./gen-address.sh 4 --env` |
| Build a genesis only | `./gen-genesis.sh [flags]` |
| Join another network | `./join.sh --genesis g.json --ip <host>` |

Every script supports `-h` / `--help`.

---

## Generating a genesis

`gen-genesis.sh` drives the (bug-fixed) `puppeth` non-interactively. All flags
are optional and default to the local setup. Addresses accept `0x`, `xdc`, or
bare-hex.

```bash
./gen-genesis.sh --chainid 20118 \
  --signers 0xAAA...,0xBBB...,0xCCC...,0xDDD...
```

| Flag | Meaning (default) |
|------|-------------------|
| `--chainid N` | chain / network id (`20118`) |
| `--signers a,b,c` | validator / masternode addresses |
| `--owner ADDR` | masternode owner |
| `--reward N` | block reward (`5000`) |
| `--v2block N` | v2 consensus switch block (`2700`) |
| `--epoch N` / `--gap N` | epoch length (`900`) / checkpoint gap (`450`) |
| `--foundation ADDR` / `--foundation-owners a,b` | foundation wallet / multisig |
| `--team-owners a,b` / `--swap ADDR` / `--prefund a,b` | governance / funding |
| `--network NAME` / `--out FILE` | puppeth net name / output path |

It also writes `network-roles.env` so `network-info.sh` can label accounts.

> **Keep your `genesis.json`.** puppeth stamps a genesis with the current time,
> so an identical one **cannot be regenerated later**. Whoever runs a network
> must save and share that exact `genesis.json` for others to join.

---

## Joining from another machine

Any node started from this repo shares the same `bootnode.key`, so joining just
needs the network's **`genesis.json`** and the host's **IP**:

```bash
# on the second machine (has bin/, bootnode.key, and the network's genesis.json):
./join.sh --genesis genesis.json --ip <HOST_IP>
```

`--ip` builds the bootnode enode for you. Other ways to point it:

```bash
./join.sh --genesis genesis/genesis.json                       # local machine
./join.sh --genesis g.json --ip 10.0.0.5:30301                 # host + custom port
./join.sh --genesis g.json --bootnodes "enode://KEY@HOST:30301" # explicit enode
```

Useful flags: `--chainid` (defaults to the genesis chainId), `--datadir`
(default `./peer`), `--port` / `--rpcport` / `--wsport`, `--name`,
`--mine ADDR` (only if that address is a validator in the genesis and its key is
in `.env`). By default it joins as a **read/sync** node and **reports to the same
ethstats dashboard as `run.sh`**; override with `--ethstats SECRET@HOST:PORT`
(node name auto-prepended) or turn it off with `--no-ethstats`.

`--client` selects the node client (all sync-only except oldxdc, which can also
`--mine`):

| Client | How it joins | Notes |
|--------|-------------|-------|
| `oldxdc` (default) | `--genesis` file | full sync + optional `--mine` (5151 peering under investigation — [#13](../../issues/13)) |
| `geth` | `--genesis` file | sync-only; **verified** syncing an XDPoSChain net |
| `besu` | `--genesis` file | sync-only; reads geth genesis natively |
| `reth` | `--genesis` file | sync-only; reads geth genesis natively (5151 peering — [#12](../../issues/12)) |
| `erigon` | `--chain NAME` | sync-only; built-in XDC chainspecs only, **not** a custom genesis ([#9](../../issues/9)) |
| `nethermind` | own chainspec | sync-only; needs a `NETHERMIND_DIST` with the target chainspec |

```bash
./join.sh --client geth   --genesis genesis.json --ip <HOST_IP>
./join.sh --client besu   --genesis genesis.json --ip <HOST_IP>
./join.sh --client erigon --chain <xdc-chain> --ip <HOST_IP>
```

See [Choosing a client](#choosing-a-client) for the full per-client status and
the latest verification matrix.

**Ports:** `join.sh` defaults to `p2p 30313 / rpc 8595 / ws 8596` (and, for
erigon, `authrpc 8651 / torrent 42079 / private-api 9095 / mcp 8653`) — all
clear of a running `run.sh` network (30301, 30303-30306, 8545-8548, 8555-8558),
so you can join on the same machine without conflicts. Override with
`--port/--rpcport/--wsport`.

**Requirements to connect:**
- The **same `genesis.json`** as the target network (a different one → the peer
  is rejected at the handshake).
- The host allows inbound **`30301/udp+tcp`** (discovery) and **`30303/tcp`** (p2p).
- Both machines on the same network (or the host's public IP is reachable).

Print the exact bootnode endpoint to hand out:

```bash
echo "enode://$(./bin/*/bootnode -nodekey ./bootnode.key -writeaddress)@$(hostname -I 2>/dev/null | awk '{print $1}' || ipconfig getifaddr en0):30301"
```

---

## What's in this repo

```
bin/<client>/<os>-<arch>/  per-client, per-platform binaries, built on first
                 run (git-ignored): oldxdc -> XDC+bootnode+puppeth, geth -> geth
setup.sh         one-command end-to-end: keys -> genesis -> reset -> run
run.sh           import keys, init, start bootnode + 4 mining nodes
gen-address.sh   generate fresh keypairs (private key + address)
gen-genesis.sh   generate genesis.json via puppeth (flag-driven)
network-info.sh  plain-language summary of the current network
join.sh          connect & sync an existing network (by genesis + IP/enode)
reset.sh         wipe node data for a clean re-init
lib.sh           shared helper: platform detection, IP, build-or-download
binaries.json    prebuilt-binary manifest (URL + per-client sha256/platforms)
genesis/         genesis.json (chainId 20118, 4 masternodes seeded)
bootnode.key     fixed bootnode private key (shared, so --ip joins work)
.env / .env.sample  the 4 masternode private keys (.env is git-ignored)
lab/             XDPoS multi-client consensus test lab (see below)
docs/lab/        lab design, contract inventory, worst-case scenario catalog
```

---

## Consensus test lab (`lab/`)

A validation-parity harness that checks whether each Ethereum client's XDPoS
port agrees with the reference (`oldxdc`) — the `oldxdc` net produces the chain
and injects worst cases (propose/resign at epoch boundaries, offline validators,
partitions), the other clients follow sync-only, and `lab/oracle.sh` compares
per-block **block hash** and the `0x88` **masternode set** across clients (a
divergence = a porting bug). It ships 10 P0 scenarios (`lab/scenarios/`) and a
propose/resign/vote CLI (`lab/masternode.sh`).

Result so far: **geth holds full parity with oldxdc through the v1→v2 switch**
(`PARITY OK 1..1054`); the erigon custom-genesis fix that unblocks it is proven
in the lab but not yet upstreamed. Design/status: [`docs/lab/DESIGN.md`](docs/lab/DESIGN.md).

---

## Network details

- **chainId / networkid:** `20118` (must **not** be `50/51/551` — those force the
  built-in mainnet/testnet/devnet genesis and won't accept a custom one)
- **consensus:** XDPoS — 2s blocks, epoch `900`, gap `450`, v2 switch block `2700`
- **nodes / ports:**

  | node | datadir | p2p | ws | rpc |
  |------|---------|-----|----|-----|
  | 1 | nodes/1 | 30303 | 8555 | 8545 |
  | 2 | nodes/2 | 30304 | 8556 | 8546 |
  | 3 | nodes/3 | 30305 | 8557 | 8547 |
  | 4 | nodes/4 | 30306 | 8558 | 8548 |

- Nodes report to the `ethstats` endpoint set in `run.sh` (`STATS_SECRET`) —
  change it if you don't want a shared dashboard.
- `.env` holds **throwaway devnet keys** — never reuse them anywhere real.
- **Gas limit — two different, deliberately different, numbers** (see
  [`docs/lab/DESIGN.md`](docs/lab/DESIGN.md)'s "Gas-limit plateau" section,
  [`netlab/TOPOLOGY.md`](netlab/TOPOLOGY.md)'s `gasLimit` field, issues #94/#96):
  genesis `gasLimit` is only ever the ratchet's *starting point* (this repo never edits it —
  `run.sh` uses whatever `genesis/genesis.json` already has); the **mint/plateau target** every
  producer is actually launched with is a separate, explicit number (`run.sh`/`run-mixed.sh` pass
  `--targetgaslimit 420000000`; `netlab/`'s topologies default their own `gasLimit` field to the
  same 420,000,000). **Every producer of every client in one net must be given the identical
  plateau target** — a producer left on its own client default (as `netlab/node.sh` briefly was,
  before #96) silently disagrees with the others, and the legacy XDPoSChain arbiter hard-errors
  instead of clamping once two producers' targets drift the gas limit apart, permanently locking
  every validator out of proposing (issue #94's netv12 wedge). Never "fix" this by making genesis
  equal the plateau, or by deriving the plateau from genesis — real XDC mainnet's own genesis is
  4,700,000 while its live plateau is 420,000,000, so either "fix" would hone a mainnet-parity net
  in the wrong direction.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `Permission denied` running a script | `chmod +x *.sh` |
| `address already in use` | a node from a previous run is still up. Find it by PORT, not by name: `ss -ltnp "sport = :8545"` (and `ss -lunp` for UDP — the bootnode's discovery port is UDP-only, so a TCP-only check misses it). Then `kill -TERM <pid>`. **Do not use `pkill -f`** — a substring match can hit unrelated processes, including your own shell, and on a shared host it will take out someone else's nodes. |
| `incompatible genesis (have … new …)` | old data vs new genesis: `./reset.sh` then start again |
| Blocks stop / stuck at an epoch (e.g. 900) | genesis validators don't match `0x88` — regenerate with `gen-genesis.sh` (see below) |
| A joining peer won't connect | its `genesis.json` must be byte-identical to the target's; check firewall on `30301`/`30303` |
| `networkid` rejected / wrong genesis loads | don't use `50/51/551`; keep `networkid == chainId` |

---

## The puppeth fix

Stock `puppeth` in this XDPoSChain build corrupts the genesis: its storage
exporter RLP-decoded each already-decoded storage word, truncating every
masternode address in the `XDCValidator` (`0x88`) `candidates[]` array (e.g.
`0xbd29…` → `0x00`). `getCandidates()` then returned garbage and the chain
**stalled at the first epoch boundary (block 900)**. The bundled `bin/*/puppeth`
stores the word verbatim, so validators are seeded correctly and the epoch
transition succeeds. Fix: `cmd/puppeth/wizard_genesis.go` in XDPoSChain.
