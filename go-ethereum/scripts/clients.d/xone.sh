#!/usr/bin/env bash
# =============================================================================
#  XDC xone client fragment  —  sourced by one.sh
#  Binary:   xone  (ETH2030 / go-ethereum-derived XDC execution client)
#  Modes:    snap (default) · full · archive · validator
#  Networks: apothem · mainnet (xdc-mainnet) · devnet (xdc-devnet/5551) · net5050
#            devnet5151 (chainId 5151, private — genesis fetched on first run)
#  Status:   Experimental — research client. Do NOT use for production/real funds.
#            Build: https://github.com/XDCIndia/xOneGo
#
#  xone ships BUILT-IN genesis + bootnodes for apothem / xdc-mainnet / xdc-devnet,
#  so no --bootnodes is required for peering. Override with XDC_BOOTNODES if needed.
#
#  net5050 is a custom stress-test network (chainId 5050).  xone has no built-in
#  genesis for it, so the fragment fetches the genesis JSON, runs xone init, and
#  wires all 5 static peers via a TOML config.  Full sync from genesis is
#  mandatory (#96 — peers serve no snap/fast state for net5050).
#
#  devnet (5551): xone uses --network xdc-devnet (built-in).  Bootnodes are
#  embedded; ethstats is wired to stats.xdcindia.com.  Full sync is forced.
#
#  devnet5151 (chainId 5151): private dev network with no built-in xone support.
#  Genesis is fetched from https://xdc.network/snapshots/devnet5151-genesis.json,
#  xone init is run once, and full sync from genesis is used.  No live peers
#  exist yet — static-peer list is empty pending a live node being spun up.
#  Update _DEVNET5151_PEERS when a node is live.
#
#  Node types (--mode):
#    snap       -syncmode snap                 fast initial sync (default)
#    full       -syncmode full                 full block execution from genesis
#    archive    -syncmode full -gcmode archive full node, no state pruning
#    validator  -syncmode full -mine …         XDPoS V2 block producer (needs keystore)
#
#  Validator mode requires a funded/authorised signer account:
#    XDC_ETHERBASE        signer address (0x…) for --miner.etherbase (required)
#    XDC_MINER_PASSWORD   path to keystore password file (required to unlock)
#    XDC_UNLOCK           address to unlock (defaults to XDC_ETHERBASE)
#  ONE_SH_VERSION: 1.0.2
# =============================================================================

# net5050 static peers — all 5 validators/bootnodes on the stress-test network.
_NET5050_PEERS='enode://507918630af3a6080867ec3987e9d0054bbb18c97ef38ae39f05f27a13371aab067b2c9e97099c554582b542946742d999e77a84da33f2ac07498a8f03d6c261@65.21.71.4:30550,enode://7d78c32e5dfade9319b1fb564d02fac8aa1474fd01cfa6a6dce689c4f16aa2f742d4492db09930124185f8942abe1bd6b5fa1b2b572f1cbe2641e7411299e14d@65.21.71.4:30552,enode://a4b966a579d1c7abe9f8856a612978db20483ce0eabca2c9ed9768a05d08e2114d14568931fbd3e62fa5f533bfe65d40381ed014c56a67ed8ab212d85bb9d403@65.21.71.4:30554,enode://d2c5ccdc96fe0d4574242572e98057834e7bf5ea6a01afd1e15e81c0bdda735788367581c57f6a79502664d6163c4b3bbb223163382daa1f0c2d3f77d326c709@185.180.220.183:30550,enode://5baefc523ad83eccf9021a1ccb7695496cd4bf82fe96c82557e4238869e50bb8ede9cdb2417d6bde1e04ce000a0186bf3f4f08992fea80703a2788c4342ebf34@95.217.56.168:30551'

# devnet-5551 static peers: two bootnodes at 154.38.175.218 + observer on xdc-prod.
# xone uses --network xdc-devnet which already embeds these bootnodes, so this list
# is used only when XDC_BOOTNODES is set or as a reference.
_DEVNET5551_PEERS='enode://fae09c24badef5488f2bde8c88c27317919e2eaa311bee5d3e087756c8080add923d94389cb5ac29e2abd85519535c25423136d39122459b594ea58e064d6918@154.38.175.218:30701,enode://a183045c9eb8eebe2dc3ab4197630be625a03981646b668dcf74959a0c4341cb88f00b53bf0749d1e081033df20cfcb75b71054e2a03ecfdef83151f5048e1e0@154.38.175.218:30799,enode://6a30ec5b4c8a6719ad5c84368502166784b88e976ca626f5277211792570b6b8f6726f187da35784c877d114db52484e89b376c5a8194ae060f68695703694b8@65.21.27.213:30330'

# devnet-5151 static peers.
# NO LIVE NODES EXIST YET — this list is intentionally empty.
# Add enodes here when a 5151 node is spun up and confirmed dialable.
_DEVNET5151_PEERS=''

# Map the installer's network name → xone's -network value.
# net5050 and devnet5151 have no built-in network label — return empty;
# caller uses -networkid instead.
_xone_network(){
  case "${1:-apothem}" in
    mainnet)     echo "xdc-mainnet" ;;
    devnet)      echo "xdc-devnet" ;;
    net5050)     echo "" ;;
    devnet5151)  echo "" ;;
    *)           echo "apothem" ;;
  esac
}

# Hook: no snapshot published for xone yet.
client_snapshot_name(){
  echo ""
}

# Hook: sync time estimate (no snapshot → sync from genesis).
client_sync_estimate(){
  local _net="${1:-apothem}" _mode="${2:-snap}"
  case "$_net" in
    net5050)    echo "minutes–hours (net5050 is small ~361k blocks; full sync from genesis)" ;;
    devnet)     echo "hours (devnet-5551 is at ~2M blocks and advancing; full sync from genesis)" ;;
    devnet5151) echo "minutes–hours (devnet-5151 genesis sync; no peers exist yet — ensure a live 5151 node is running)" ;;
    *)
      case "$_mode" in
        snap) echo "hours–days (snap sync from genesis; no snapshot published)" ;;
        *)    echo "days (full execution sync from genesis; no snapshot published)" ;;
      esac
      ;;
  esac
}

# _xone_net5050_init: fetch genesis + run xone init for net5050.
# Called from client_start_args (after the binary is guaranteed present).
# Idempotent: skips if chaindata already exists.
_xone_net5050_init(){
  local _datadir="${DATADIR:-${DIR:-$(pwd)}/data}"
  local _xone_bin="${DIR:-$(pwd)}/xone"
  local _genesis_json="${_datadir}/net5050-genesis.json"
  # xone (geth-family) writes chaindata under <datadir>/xone/chaindata
  local _chaindata="${_datadir}/xone/chaindata"

  [ -d "$_chaindata" ] && [ -n "$(ls -A "$_chaindata" 2>/dev/null)" ] && return 0

  step "net5050: initialising chaindata from genesis"
  mkdir -p "$_datadir"
  local _genesis_url="${BASE_URL:-https://xdc.network/snapshots}/net5050-genesis.json"
  printf '   %ssource:%s %s\n' "${D:-}" "${N:-}" "$_genesis_url"
  curl -fsSL -A 'Mozilla/5.0' --max-time 60 "$_genesis_url" -o "$_genesis_json" || {
    err "Failed to download net5050 genesis from $_genesis_url"
    err "  Place the genesis JSON at $_genesis_json and re-run."
    return 1
  }
  ok "Genesis JSON downloaded."
  # Correct flag order for geth-family: flag(s) BEFORE the subcommand.
  "$_xone_bin" -datadir "$_datadir" -override.genesis "$_genesis_json" init "$_genesis_json" >/dev/null 2>&1 || {
    err "xone init failed — check the genesis JSON and binary."
    return 1
  }
  ok "net5050 chaindata initialised."
}

# _xone_devnet5151_init: fetch genesis + run xone init for devnet-5151.
# Mirrors _xone_net5050_init.  Idempotent: skips if chaindata already exists.
_xone_devnet5151_init(){
  local _datadir="${DATADIR:-${DIR:-$(pwd)}/data}"
  local _xone_bin="${DIR:-$(pwd)}/xone"
  local _genesis_json="${_datadir}/devnet5151-genesis.json"
  # xone (geth-family) writes chaindata under <datadir>/xone/chaindata
  local _chaindata="${_datadir}/xone/chaindata"

  [ -d "$_chaindata" ] && [ -n "$(ls -A "$_chaindata" 2>/dev/null)" ] && return 0

  step "devnet5151: initialising chaindata from genesis"
  mkdir -p "$_datadir"
  local _genesis_url="${BASE_URL:-https://xdc.network/snapshots}/devnet5151-genesis.json"
  printf '   %ssource:%s %s\n' "${D:-}" "${N:-}" "$_genesis_url"
  curl -fsSL -A 'Mozilla/5.0' --max-time 60 "$_genesis_url" -o "$_genesis_json" || {
    err "Failed to download devnet5151 genesis from $_genesis_url"
    err "  Place the genesis JSON at $_genesis_json and re-run."
    return 1
  }
  ok "Genesis JSON downloaded."
  "$_xone_bin" -datadir "$_datadir" -override.genesis "$_genesis_json" init "$_genesis_json" >/dev/null 2>&1 || {
    err "xone init failed — check the genesis JSON and binary."
    return 1
  }
  ok "devnet5151 chaindata initialised."
}

# Hook: pre-flight requirements. Only validator mode has extra needs.
# net5050/devnet5151 genesis init is deferred to client_start_args (after binary download).
client_requirements(){
  if [ "${NODETYPE:-}" = "validator" ]; then
    if [ -z "${XDC_ETHERBASE:-}" ]; then
      err "xone validator mode requires a signer address."
      err "  Set XDC_ETHERBASE=0x… (and XDC_MINER_PASSWORD=/path/to/pwfile to unlock the keystore)."
      return 1
    fi
    if [ -n "${XDC_MINER_PASSWORD:-}" ] && [ ! -f "${XDC_MINER_PASSWORD}" ]; then
      err "XDC_MINER_PASSWORD points to a missing file: ${XDC_MINER_PASSWORD}"
      return 1
    fi
  fi
  return 0
}

# Hook: launch arguments. Signature: client_start_args <network> <mode>
client_start_args(){
  local _net="${1:-apothem}" _mode="${2:-snap}"
  local _datadir="${DATADIR:-$DIR/data}"
  local _xnet; _xnet=$(_xone_network "$_net")

  # Mode → sync/prune/miner flags.
  local _mode_args=""
  case "$_mode" in
    snap)      _mode_args="-syncmode snap" ;;
    full)      _mode_args="-syncmode full" ;;
    archive)   _mode_args="-syncmode full -gcmode archive" ;;
    validator)
      _mode_args="-syncmode full -mine -miner.etherbase \"${XDC_ETHERBASE}\""
      [ -n "${XDC_MINER_PASSWORD:-}" ] && _mode_args="$_mode_args -password \"${XDC_MINER_PASSWORD}\" -unlock \"${XDC_UNLOCK:-$XDC_ETHERBASE}\""
      ;;
    *)         _mode_args="-syncmode snap" ;;
  esac

  # Optional operator-supplied bootnodes (xone has sane built-ins otherwise).
  local _boot_args=""
  [ -n "${XDC_BOOTNODES:-}" ] && _boot_args="-bootnodes \"${XDC_BOOTNODES}\""

  # net5050: custom stress-test network — no built-in genesis/peers in xone.
  # Uses -networkid 5050 + -override.genesis + explicit -bootnodes + forced full sync.
  # xone v1.0.0 does not support -config <toml>; peers are passed via -bootnodes.
  # ethstats is wired so the node appears on stats.xdcindia.com.
  if [ "$_net" = "net5050" ]; then
    local _datadir_n5="${DATADIR:-$DIR/data}"
    local _genesis_json_n5="${_datadir_n5}/net5050-genesis.json"
    # Ensure chaindata is initialised (binary is present at this point).
    _xone_net5050_init || return 1
    local _ethstats_name="${HOST:-xdcnode}-net5050-${_mode:-full}-${OSID:-os}"
    local _ethstats_flag="-ethstats \"${_ethstats_name}:${STATS_SECRET:-xdc_openscan_stats_2026}@stats.xdcindia.com:443\""
    # Force full sync regardless of requested mode — no snap/fast state available (#96).
    local _net5050_sync="-syncmode full"
    case "$_mode" in
      archive) _net5050_sync="-syncmode full -gcmode archive" ;;
      validator)
        _net5050_sync="-syncmode full -mine -miner.etherbase \"${XDC_ETHERBASE}\""
        [ -n "${XDC_MINER_PASSWORD:-}" ] && _net5050_sync="$_net5050_sync -password \"${XDC_MINER_PASSWORD}\" -unlock \"${XDC_UNLOCK:-$XDC_ETHERBASE}\""
        ;;
    esac
    printf '%s' "\
-datadir \"${_datadir_n5}\" \
-override.genesis \"${_genesis_json_n5}\" \
-networkid 5050 \
-bootnodes \"${_NET5050_PEERS}\" \
-http -http.addr 127.0.0.1 -http.port \"${RPC_PORT:-8615}\" \
-http.api eth,net,web3,debug,txpool,admin \
-ws -ws.addr 127.0.0.1 -ws.port \"${WS_PORT:-8616}\" -ws.api eth,net,web3 \
-authrpc.addr 127.0.0.1 -authrpc.port \"${AUTH_PORT:-8617}\" \
-port \"${P2P_PORT:-30356}\" \
-maxpeers ${MAXPEERS:-50} \
$_net5050_sync \
$_ethstats_flag \
-verbosity ${VERBOSITY:-3}"
    return
  fi

  # devnet (5551): xone has a built-in xdc-devnet chain (-network xdc-devnet).
  # Bootnodes are embedded so no explicit -bootnodes is needed.
  # Full sync is forced — no snapshot published for devnet.
  # ethstats is wired to stats.xdcindia.com.
  if [ "$_net" = "devnet" ]; then
    local _datadir_dv="${DATADIR:-$DIR/data}"
    local _ethstats_name_dv="${HOST:-xdcnode}-devnet-full-${OSID:-os}"
    local _ethstats_flag_dv="-ethstats \"${_ethstats_name_dv}:${STATS_SECRET:-xdc_openscan_stats_2026}@stats.xdcindia.com:443\""
    # Force full sync — no snapshot published for devnet.
    local _devnet_sync="-syncmode full"
    case "$_mode" in
      archive) _devnet_sync="-syncmode full -gcmode archive" ;;
      validator)
        _devnet_sync="-syncmode full -mine -miner.etherbase \"${XDC_ETHERBASE}\""
        [ -n "${XDC_MINER_PASSWORD:-}" ] && _devnet_sync="$_devnet_sync -password \"${XDC_MINER_PASSWORD}\" -unlock \"${XDC_UNLOCK:-$XDC_ETHERBASE}\""
        ;;
    esac
    # Optional explicit bootnodes override (default: use embedded devnet-5551 bootnodes).
    local _devnet_boot_args=""
    [ -n "${XDC_BOOTNODES:-}" ] && _devnet_boot_args="-bootnodes \"${XDC_BOOTNODES}\"" || \
      _devnet_boot_args="-bootnodes \"${_DEVNET5551_PEERS}\""
    printf '%s' "\
-datadir \"${_datadir_dv}\" \
-network xdc-devnet \
-http -http.addr 127.0.0.1 -http.port \"${RPC_PORT:-8615}\" \
-http.api eth,net,web3,debug,txpool,admin \
-ws -ws.addr 127.0.0.1 -ws.port \"${WS_PORT:-8616}\" -ws.api eth,net,web3 \
-authrpc.addr 127.0.0.1 -authrpc.port \"${AUTH_PORT:-8617}\" \
-port \"${P2P_PORT:-30356}\" \
-maxpeers ${MAXPEERS:-50} \
$_devnet_boot_args \
$_devnet_sync \
$_ethstats_flag_dv \
-verbosity ${VERBOSITY:-3}"
    return
  fi

  # devnet5151 (chainId 5151): private dev network, no built-in xone support.
  # Mirrors the net5050 pattern: fetch genesis, xone init, full sync.
  # NO LIVE PEERS EXIST YET — _DEVNET5151_PEERS is empty; update when a node is live.
  # ethstats is wired to stats.xdcindia.com (consistent with net5050/devnet).
  if [ "$_net" = "devnet5151" ]; then
    local _datadir_d51="${DATADIR:-$DIR/data}"
    local _genesis_json_d51="${_datadir_d51}/devnet5151-genesis.json"
    # Ensure chaindata is initialised (binary is present at this point).
    _xone_devnet5151_init || return 1
    local _ethstats_name_d51="${HOST:-xdcnode}-devnet5151-full-${OSID:-os}"
    local _ethstats_flag_d51="-ethstats \"${_ethstats_name_d51}:${STATS_SECRET:-xdc_openscan_stats_2026}@stats.xdcindia.com:443\""
    # Force full sync — no snap/fast state available on this private net.
    local _d51_sync="-syncmode full"
    case "$_mode" in
      archive) _d51_sync="-syncmode full -gcmode archive" ;;
      validator)
        _d51_sync="-syncmode full -mine -miner.etherbase \"${XDC_ETHERBASE}\""
        [ -n "${XDC_MINER_PASSWORD:-}" ] && _d51_sync="$_d51_sync -password \"${XDC_MINER_PASSWORD}\" -unlock \"${XDC_UNLOCK:-$XDC_ETHERBASE}\""
        ;;
    esac
    # Static peers: use explicit list when provided, fall back to _DEVNET5151_PEERS.
    local _d51_boot_args=""
    local _d51_peers="${XDC_BOOTNODES:-$_DEVNET5151_PEERS}"
    [ -n "$_d51_peers" ] && _d51_boot_args="-bootnodes \"${_d51_peers}\""
    printf '%s' "\
-datadir \"${_datadir_d51}\" \
-override.genesis \"${_genesis_json_d51}\" \
-networkid 5151 \
-http -http.addr 127.0.0.1 -http.port \"${RPC_PORT:-8615}\" \
-http.api eth,net,web3,debug,txpool,admin \
-ws -ws.addr 127.0.0.1 -ws.port \"${WS_PORT:-8616}\" -ws.api eth,net,web3 \
-authrpc.addr 127.0.0.1 -authrpc.port \"${AUTH_PORT:-8617}\" \
-port \"${P2P_PORT:-30356}\" \
-maxpeers ${MAXPEERS:-50} \
$_d51_boot_args \
$_d51_sync \
$_ethstats_flag_d51 \
-verbosity ${VERBOSITY:-3}"
    return
  fi

  printf '%s' "\
-datadir \"$_datadir\" \
-network \"$_xnet\" \
-http -http.addr 127.0.0.1 -http.port \"${RPC_PORT:-8615}\" \
-http.api eth,net,web3,debug,txpool,admin \
-ws -ws.addr 127.0.0.1 -ws.port \"${WS_PORT:-8616}\" -ws.api eth,net,web3 \
-authrpc.addr 127.0.0.1 -authrpc.port \"${AUTH_PORT:-8617}\" \
-port \"${P2P_PORT:-30356}\" \
-maxpeers ${MAXPEERS:-50} \
$_boot_args \
$_mode_args \
-verbosity ${VERBOSITY:-3}"
}

# Hook: RPC port for health-check.
client_rpc_port(){ echo "${RPC_PORT:-8615}"; }

# Hook: generic SIGTERM stop.
client_stop(){ return 0; }
