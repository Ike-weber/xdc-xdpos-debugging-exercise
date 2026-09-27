# netlab topology schema

A **topology** is a JSON file (see `topologies/*.json`) describing a
self-contained private XDPoS network: its own chain parameters, its own
genesis (generated fresh by `fleet.sh up`, never touching `./genesis/genesis.json`),
and an ordered list of nodes. `netlab/topo.sh` parses and validates it;
`netlab/node.sh` starts/stops individual nodes from it; `netlab/fleet.sh`
brings up/tears down the whole topology.

## Top-level fields

| Field            | Type    | Required | Default | Notes |
|-------------------|---------|----------|---------|-------|
| `name`            | string  | yes      | --      | Must equal the filename stem (`topologies/legacy-4.json` -> `"legacy-4"`). Used as the `nodes/<name>/` run-state directory. |
| `description`     | string  | no       | `""`    | Free text, shown by `topo.sh print`. |
| `chainId`         | int     | yes      | --      | Passed to `gen-genesis.sh --chainid` and `join.sh --chainid`. |
| `epoch`           | int     | yes      | --      | **Must be `900`.** Legacy oldxdc hardcodes its own internal checkpoint constant (`EpocBlockRandomize=900`) independent of this genesis field; any other value freezes the chain at the first checkpoint (see `gen-genesis.sh`'s own `--epoch` warning). `topo.sh validate` hard-errors otherwise. |
| `v2block`         | int     | yes      | --      | V1->V2 BFT switch block. **Must be a multiple of `epoch`** (i.e. of 900) -- the switch only happens at an epoch/checkpoint boundary. `topo.sh validate` hard-errors otherwise. |
| `period`          | int     | no       | `1`     | Block time in seconds (post-processed into the genesis like `gen-genesis.sh --period`). |
| `reward`          | int     | no       | `5000`  | Block reward (`gen-genesis.sh --reward`). |
| `gap`             | int     | no       | `450`   | Blocks before checkpoint (`gen-genesis.sh --gap`). |
| `gasLimit`        | int     | no       | `420000000` | The **mint/plateau target** every producer in this topology is launched with (threaded to `join.sh --gas-limit`, the per-client gas-ceiling flag). **Not** the genesis header's own `gasLimit` field, which this framework never generates or edits here -- those are legitimately different numbers (ItWorksinMyLocal#94/#96): geth-xdc/erigon-xdc hard-code a 420,000,000 "XDC plateau" they hone towards at runtime, one legal `parent/1024` step per block, the same mechanism by which live XDC mainnet itself climbed from its own genesis's 4,700,000 to its current 420,000,000 plateau without ever touching the genesis file. `#94`'s netv12 wedge was never a genesis-vs-target mismatch; it was the legacy arbiter falling back to its own compiled-in 50,000,000 default (no `--targetgaslimit` passed) while geth/erigon targeted 420,000,000 -- **two different targets on one chain**. This field forces every node in a topology onto the same target; `topo.sh print`'s `gasLimit` column always shows the resolved value, identical for every node (there is no per-node override, unlike `maxpeers`). |
| `skipV1Validation`| bool    | no       | `false` | **Forbidden -- must be `false`/omitted.** `docs/lab/DESIGN.md`'s "Documented risks & forbidden knobs" section bans this genesis flag outright ("they disable the validation the lab exists to compare"); `topo.sh validate` hard-errors if it is `true`. It exists as a `gen-genesis.sh --skip-v1-validation` passthrough for non-lab uses of that generator, not as a supported netlab knob -- an earlier revision of this table described it as the fix for a follower gas-cost desync, but that "fix" is exactly the validation this lab exists to exercise (see scenario 12 / ItWorksinMyLocal#34, #46). |
| `peering`         | string  | no       | `"hub"` | `"hub"` (D5's original model): followers dial ONLY the sealers, never each other. `"mesh"` (ItWorksinMyLocal#46 T3.2): `fleet.sh up` additionally hands each follower every EARLIER-started follower's own enode as a bootnode too (still one at a time, still gated on `peers>=1`) -- so followers end up peered with each other as well, not just with the sealers. Sealer<->sealer peering is unaffected by this field either way (always through the topology's own bootnode). |
| `portBase`        | int     | yes      | --      | **Must be >= 34000** (the framework's reserved port band -- never overlaps the live net5151 stack on 9645-10245 or net5050/postgres). Each node gets a distinct 20-wide port block starting here; see "Port math" below. |
| `staggerSeconds`  | int     | no       | `5`     | `fleet.sh up` waits this long (and gates on `peers>=1`, not just the clock) between starting each follower. |
| `nodes`           | array   | yes      | --      | See "Node fields" below. Must contain at least one `sealer`. |

## Node fields

Each entry in `nodes` is expanded (see "Id expansion" below) into one or
more resolved node rows before validation.

| Field     | Type   | Required           | Notes |
|-----------|--------|---------------------|-------|
| `id`      | string | yes                 | Unique after expansion. Becomes the `nodes/<topo>/<id>/` datadir and the node's identity/ethstats-name component. |
| `client`  | string | yes                 | One of `oldxdc \| geth \| erigon \| besu \| nethermind \| reth \| xone` (the same enum `join.sh --client` accepts, minus `all`). |
| `role`    | string | yes                 | `sealer` (mines, `oldxdc` only) or `follower` (sync-only). `proposer` is accepted by the schema but **hard-rejected** by `topo.sh validate` -- reserved, pending client-side BFT-voting support (go-ethereum#1341). |
| `keyRef`  | string | yes iff `role=sealer` | Name of an **environment variable** (sourced from `.env`, e.g. `PRIVATE_KEY_1`) holding the raw hex validator private key. Topology JSON files are committed to git and must never contain a private key directly -- only a reference to where the (gitignored) key lives. |
| `maxpeers`| int    | no                  | Per-node override, threaded to `join.sh --maxpeers`. Omit it to get the **role-based default** (ItWorksinMyLocal#46 T3.1) instead of the client binary's own hardcoded default: a `sealer` defaults to `node_count + 5` (enough for a full sealer mesh plus every follower dialing in, plus slack for a manual/admin connection) so a large topology can't starve a sealer's peer slots; a `follower` defaults to a fixed `15` regardless of topology size (hub mode only ever needs the sealer count; mesh mode -- see `peering` below -- adds the other followers too), so a follower can't itself become an unbounded peer-storm sink as the topology grows. `topo.sh print`'s `maxpeers` column always shows the resolved value (explicit or role-based default), never blank. |

### Id expansion

`"id": "geth{1..3}"` expands to three node rows `geth1`, `geth2`, `geth3`,
each an identical copy of the rest of that entry (same `client`/`role`/etc),
occupying consecutive port blocks in declaration order. Lets a stress
topology (e.g. Phase 3's `13-all-clients-soak`) declare "N replicas of the
same follower client" without repeating JSON blocks. Not used by the three
shipped presets (each of their nodes needs a distinct `keyRef` or is a
one-off follower), but the parser supports it for topologies that need it.
`{A..B}` requires `A <= B`; reversed or malformed ranges are a hard error.

### Constraints (`topo.sh validate` hard-errors on any violation)

- `epoch` must be `900`.
- `v2block % epoch` must be `0`.
- `portBase` must be `>= 34000`.
- every node `id` must be unique after expansion.
- `client` must be one of the 7 known values.
- `role=sealer` requires `client=oldxdc` (only oldxdc can seal via `join.sh --mine`; the other clients' join profiles are sync-only by construction).
- `role=proposer` is rejected outright (reserved, not yet implemented).
- at least one `role=sealer` node must exist (a topology with no sealer has no chain to generate/join).
- `role=sealer` requires `keyRef`; `keyRef` must name a variable that is actually set in the environment at resolve time (checked by `topo.sh print`, which is what `fleet.sh up` runs against -- so a missing `.env` key fails fast, before any process starts).
- `client=nethermind` is rejected whenever `chainId != 5151` -- nethermind loads only its own bundled net5151 Parity-style chainspec, not an arbitrary genesis file (ItWorksinMyLocal#46 open question #2). None of the three shipped presets use nethermind for this reason.

## Port math

For a resolved (post-expansion) node at zero-based position `i` in the
`nodes` list:

```
p2p      = portBase + i*20 + 0
rpc      = portBase + i*20 + 1
ws       = portBase + i*20 + 2
authrpc  = portBase + i*20 + 3   (erigon/reth/xone; T0.2's --authrpc-port)
torrent  = portBase + i*20 + 4   (erigon only;       T0.2's --torrent-port)
mcp      = portBase + i*20 + 5   (erigon only;       T0.2's --mcp-port)
privapi  = portBase + i*20 + 6   (erigon only;       T0.2's --privapi, as 127.0.0.1:<port>)
```

Slots 7-19 of each node's block are reserved for future per-node ports
(e.g. a metrics port) without renumbering anything already assigned.

The local bootnode (one per topology, started by `fleet.sh up` from the
shared `./bootnode.key`, same derivation `run.sh` uses) gets the port
**just past the last node's block**: `portBase + N*20` (both its UDP
discovery and TCP listen port), where `N` is the resolved node count. This
keeps it inside the topology's own reserved range regardless of `portBase`,
without stealing a slot from the `{A..B}`-expandable node port formula
above.

## Run-state layout

`fleet.sh up` never touches `./genesis/genesis.json` or the existing
`./nodes/1..4` / `./nodes/<client>-<type>` conventions used by `run.sh` /
`join.sh` directly. Everything for a topology lives under its own
`nodes/<name>/`:

```
nodes/<name>/genesis.json        # generated by gen-genesis.sh --out, this topology's own chain
nodes/<name>/topo.resolved.tsv   # topo.sh print's output -- the normalized node table fleet.sh acts on
nodes/<name>/bootnode.pid        # this topology's bootnode process
nodes/<name>/bootnode.log
nodes/<name>/<nodeId>/           # this node's datadir (join.sh --datadir), keystore included
nodes/<name>/<nodeId>/node.log
nodes/<name>/<nodeId>/node.pid
nodes/<name>/<nodeId>/meta.env   # resolved client/role/ports/bootnodes for stop/status/logs
```

## The three shipped presets

| Preset             | Nodes | Purpose |
|--------------------|-------|---------|
| `legacy-4.json`    | 4 oldxdc sealers | Baseline regression / engine smoke test -- mirrors `run.sh`'s default 4-validator net. |
| `mixed-5.json`     | 4 oldxdc sealers + 1 geth follower | The ItWorksinMyLocal#34 topology declaratively: node5 must sync genesis->tip in lockstep across the V1->V2 switch. |
| `all-clients.json` | 4 oldxdc sealers + geth/erigon/besu/reth/xone followers | Full cross-client bring-up (nethermind excluded per the constraint above) -- the peer-starvation / client-matrix regression scenario. |
