#!/usr/bin/env bash
# local-sync-test.sh — XDC multi-mode sync harness (Apothem + Mainnet).
#
# Spins up one or more geth instances on one host to exercise every sync
# path for either network.  Every node is STANDALONE — it discovers peers
# via the public bootnodes for its network and can sync without any other
# local node running.  When local nodes ARE running their enodes are
# auto-added as static/trusted peers on top.
#
# Supported node types
# --------------------
#   fast       — fast-sync from genesis, A.89 dynamic pivot, hash-scheme
#   fastpath   — same but path-scheme
#   full       — snap (or full) sync from public network, hash-scheme
#   snapfull   — full-sync from genesis to tip, hash-scheme
#   snap       — snap sync, hash-scheme
#   archive    — full sync gcmode=archive, hash-scheme
#   fullpath   — snap (or full) sync from public network, path-scheme
#   snapfullpath — full-sync from genesis to tip, path-scheme
#   snappath   — snap sync, path-scheme
#   archivepath — full sync gcmode=archive, path-scheme
#
# Usage
# -----
#   scripts/local-sync-test.sh [--network mainnet|apothem] start [all|fast|full|...]
#   scripts/local-sync-test.sh [--network mainnet|apothem] stop
#   scripts/local-sync-test.sh [--network mainnet|apothem] status
#   scripts/local-sync-test.sh [--network mainnet|apothem] logs   [mode]
#   scripts/local-sync-test.sh [--network mainnet|apothem] attach [mode]
#   scripts/local-sync-test.sh [--network mainnet|apothem] enode  [mode]
#   scripts/local-sync-test.sh [--network mainnet|apothem] clean
#   scripts/local-sync-test.sh [--network mainnet|apothem] wipe-stateful
#   scripts/local-sync-test.sh [--network mainnet|apothem] restore <mode> <snap.tar.zst>
#
# --network can also be set via NETWORK env var (default: apothem).
# NETWORK_OFFSET separates mainnet and apothem ports on the same host:
#   apothem hash: 303xx  apothem path: 304xx
#   mainnet hash: 305xx  mainnet path: 306xx
#
# Environment overrides
# ---------------------
#   GETH_BIN            geth binary      (default: $REPO/build/bin/geth)
#   BASE_DIR            datadir root     (default: $REPO/nodes-local[-mainnet])
#   NETWORK             mainnet|apothem  (default: apothem)
#   CACHE_MB            cache budget MB  (default: 4096)
#   MAXPEERS            peer cap         (default: 50)
#   FULL_SYNCMODE       snap|full for `full`/`fullpath` bootstrap (default: snap)
#   STATE_HISTORY       path-scheme history depth (default: 90000; 0=unbounded)
#   SNAPSHOTS_DIR       base dir for `restore` tarballs (default: /mnt/data/snapshots)
#   ETHSTATS_SECRET     stats secret     (default: xdc_openscan_stats_2026)
#   ETHSTATS_HOST       stats host:port  (default: stats.xdcindia.com:443)
#   ETHSTATS_URL        full URL override — bypasses auto-build
#   HOSTNAME_OVERRIDE   override hostname in ethstats label
#   LOCATION            region tag baked into ethstats label (e.g. fsn1)
#   HTTP_ADDR           bind address     (default: 127.0.0.1)
#   HTTP_VHOSTS         vhosts           (default: localhost)
#   HTTP_CORS           CORS origins     (default: unset → omitted)
#   HTTP_API            RPC API set      (default: eth,net,web3)
#
# Operator fast-sync pivot (optional — A.89 dynamic is the default)
#   FASTSYNC_PIVOT_NUMBER / FASTSYNC_PIVOT_HASH / FASTSYNC_PIVOT_ROOT
#   All three must be set together; if unset, dynamic pivot (A.89) is used.
#
# OS support: bash 3.2+ on macOS and Linux.

set -euo pipefail

# ─── Parse leading --network flag before command dispatch ──────────────
# Allows: local-sync-test.sh --network mainnet start all
#         local-sync-test.sh start all               (apothem default)
# The NETWORK env var is also honoured.
if [[ "${1:-}" == "--network" ]]; then
    NETWORK="$2"; shift 2
fi
NETWORK="${NETWORK:-apothem}"

if [[ "$NETWORK" != "mainnet" && "$NETWORK" != "apothem" ]]; then
    echo "ERROR: --network must be 'mainnet' or 'apothem' (got: $NETWORK)" >&2
    exit 1
fi

# ─── Resolve repository root + defaults ────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

GETH_BIN="${GETH_BIN:-$REPO_ROOT/build/bin/geth}"

# Separate datadirs per network so both can run side-by-side on one host.
if [[ "$NETWORK" == "mainnet" ]]; then
    BASE_DIR="${BASE_DIR:-$REPO_ROOT/nodes-local-mainnet}"
else
    BASE_DIR="${BASE_DIR:-$REPO_ROOT/nodes-local}"
fi

SNAPSHOTS_DIR="${SNAPSHOTS_DIR:-/mnt/data/snapshots}"

ETHSTATS_SECRET="${ETHSTATS_SECRET:-xdc_openscan_stats_2026}"
ETHSTATS_HOST="${ETHSTATS_HOST:-stats.xdcindia.com:443}"
ETHSTATS_URL="${ETHSTATS_URL:-}"
HOSTNAME_OVERRIDE="${HOSTNAME_OVERRIDE:-}"
LOCATION="${LOCATION:-}"

HOSTNAME_SHORT="${HOSTNAME_OVERRIDE:-$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo unknown)}"
OS_NAME="$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')"
ARCH_NAME="$(uname -m 2>/dev/null)"
GETH_VERSION=""
GETH_COMMIT=""
if [[ -x "$GETH_BIN" ]]; then
    GETH_VERSION="$("$GETH_BIN" version 2>/dev/null | awk '/^Version:/ {print $2; exit}')"
    GETH_COMMIT="$("$GETH_BIN" version 2>/dev/null | awk '/^Git Commit:/ {print substr($3,1,8); exit}')"
fi

CACHE_MB="${CACHE_MB:-4096}"
MAXPEERS="${MAXPEERS:-50}"
FULL_SYNCMODE_DEFAULT="${FULL_SYNCMODE:-snap}"

# ─── Network-specific config ───────────────────────────────────────────

if [[ "$NETWORK" == "mainnet" ]]; then
    NETWORK_FLAG="--xdcmainnet"
    NETWORK_ID=50
    # Port base offset +200 from apothem so both networks coexist on one host.
    # apothem hash: 303xx  apothem path: 304xx
    # mainnet hash: 305xx  mainnet path: 306xx
    PORT_BASE_HASH=30500
    PORT_BASE_PATH=30600
    GENESIS="${GENESIS:-$REPO_ROOT/genesis-mainnet.json}"
    BOOTNODES="${BOOTNODES:-enode://91e59fa1b034ae35e9f4e8a99cc6621f09d74e76a6220abb6c93b29ed41a9e1fc4e5b70e2c5fc43f883cffbdcd6f4f6cbc1d23af077f28c2aecc22403355d4b1@209.126.0.250:30304,enode://91e59fa1b034ae35e9f4e8a99cc6621f09d74e76a6220abb6c93b29ed41a9e1fc4e5b70e2c5fc43f883cffbdcd6f4f6cbc1d23af077f28c2aecc22403355d4b1@209.126.4.150:30304,enode://91e59fa1b034ae35e9f4e8a99cc6621f09d74e76a6220abb6c93b29ed41a9e1fc4e5b70e2c5fc43f883cffbdcd6f4f6cbc1d23af077f28c2aecc22403355d4b1@144.126.150.58:30304,enode://91e59fa1b034ae35e9f4e8a99cc6621f09d74e76a6220abb6c93b29ed41a9e1fc4e5b70e2c5fc43f883cffbdcd6f4f6cbc1d23af077f28c2aecc22403355d4b1@162.250.190.246:30304,enode://7524db6718828c2c7663e6585a5b1e066457b8b0235034b69358b36e584fea776666d36ed4fc43d0f8bf2a5c3b2a960b5600689b6c8f0c207e5a76f8b0ca432d@157.173.120.219:30304}"
    # Production mainnet fleet peers (add real enodes when available)
    FLEET_ENODES="${FLEET_ENODES:-}"
else
    NETWORK_FLAG="--apothem"
    NETWORK_ID=51
    PORT_BASE_HASH=30300
    PORT_BASE_PATH=30400
    GENESIS="${GENESIS:-$REPO_ROOT/genesis-apothem.json}"
    BOOTNODES="${BOOTNODES:-enode://619477913e8f05fabbd81fbed6a429b5e7f162635227c110c4693857806604b971e64fa55e446bcfc46637416251bdd117cc9c104a7ab43c084f6c831be6301c@207.90.192.100:30312,enode://9a20f2554cf495945ed24be380b3f3b95ad6a732c3954500a1270ffab0e64b1631ec12f6bdd618026bcb1bd27ba36736defe264cf664ab26be0bb1b13aff1e12@38.242.205.0:30312,enode://ee1e11e3f56b015b2b391eb9c45292159713583b4adfe29d24675238f73d33e6ec0a62397847823e2bca622c91892075c517fc383c9355d43a89bb7532e834a0@157.173.120.219:30312,enode://18799318d5ca266ca7a030d05a1a3a3b20d16db41eb8950ab448cb2a8f41519a1b05b36cac508ecfc590b7701719e3012511dbfcab9d2815a8d04ba6ac5c59ab@167.224.64.218:30312,enode://32b15b2cecda49d051c23745c42208de0a29ce90d6b2c44a09e4aafc9c8f19357fd43c6a4dbfa8cfc62ef65c33f7edbc7b711b0c57555a560012ecabe641191e@46.17.98.119:30312,enode://3218092c2ac11802c9a5b0656761e7e931ed830af2bd739cb988267641bba6476d6b7c5ea263f9b69ed1a4cd17e0544f7934cf354467a0d5c4d0cf5b6f13776c@185.198.27.214:30312,enode://fb28a124dbc3058bcd19c8efa1f51e9cbbb4ebb9b1d78cd8a30636e7eaf9ebf8fe0fc33a62eb945734e17f6716f2601e315493c0a181aabf4e7498006099c7d5@38.143.58.153:30312,enode://e7ab992bde99473c34f1cd45797dc6766572ffea85b42c858368362b543963ff12e51e3537af41a4df30a1e661cf08fede44b7baf29f0f814ea73e5ba36fa771@207.90.192.35:30312,enode://455cbd6f74059ca91e1e816422159b8befe60c835f2f3602fc76df546223aaa0c705adf6a966b656bc40327413daba546f1c76e21d857c3a3dbe9b7983dd035f@66.151.42.148:30312,enode://5419ff91d324cd82ead52360c9c3dc608768bce5e4d1fff1ea4e3ddb095af375536af01fbd7af4b80adbb14f695a272610a0745428b4311dafcfca9ebae1fc53@193.109.69.104:30312,enode://5419ff91d324cd82ead52360c9c3dc608768bce5e4d1fff1ea4e3ddb095af375536af01fbd7af4b80adbb14f695a272610a0745428b4311dafcfca9ebae1fc53@152.114.194.209:30312,enode://0028b38383d8f70b9e3899b85b9e6204c7c9e28b4278f74a1b1cf250fb827cf297d611fa1c9c6d9394acfb82b334477009d4cad55d8a64c7ade15acdc97dc429@205.172.58.142:30312,enode://e7c0396ad4700e7f17b039fe349f76aeb183a385fd9ea31feda30c82248396eae7f7889fe5c077817047d68f82009e866f8ef1c461193329760782c26f7ee99b@172.98.12.15:30312,enode://7c8c73c17e5fd7b4bc566642257a39df38275adac6c26a21226da7c88e876a488f8042d0089da4fdf6891480b9a45cc60eadca5f1523a05d432543f223f4a51c@66.151.40.157:30312,enode://b69f96268005e17e67127318f32f50c50573ed336ee5678af060133c07b4c68bdf3f5d5745d23341b4239601dbc48aead88595511f2de41359fadf5defaab537@152.114.192.190:30312,enode://b54c101f414c1058c14e443e5c63bad625abc7bebbbef2d5b308c62c8fda0894267da93dd4e35f5185d394b3ea4b55a2048fde441743c2bd2b52410bc8aa0150@209.209.11.134:30312,enode://d711d2e1e27746ebfa4f6a2e1a6be1beb813612dac68dabaaf392bde0087ece80739f44b1269f4b917e50d5b18343a96a7ee816732af9001fede0e6aed69740b@104.152.208.205:30312,enode://af8e6bec3f4f5f9c870a9010dcdaa0369bf6e7157845cf30e25dd75af4bf26f3715c34c0b0411e644a53012fb525c717b29c6b9c354e7cccc7d60f943010ade9@45.155.102.83:30312,enode://200b6e3d1fa56eef12a56a89ef0f4ade366cad17cd1a80ec45d76d073e8586ade65dcecc84bde64d2a416fc1b1a50ded623223f68d332a762a91796fab217c7d@104.152.209.72:30312,enode://c68cec795fa38cd70b99c4c25cb783565de6446a30fb365afe85d86d870e7badf32370e04b06d76107f02957871f280f5acbb56acc8cf44ea914a0c019d71e12@38.102.86.183:30312,enode://7ba52c37641ca88295398a15906647a0b57c18bc7388c514cab4fd5354cbc13553af744c657b011bb620eb1452d233d3243eca7ca9daca45d8f614b9553b6e1b@212.69.87.88:30312,enode://04fc75e70667901ec7a32d6bd52f6d4ef50477ad5a49b8b4bc649ab24ce1c20b76e9724ba43cc5427749a14cb34e437a3d79f268a8a9b98224478f599f0dc93a@207.90.194.126:30312,enode://729d763db071595bacbbf33037a8e7639d8e9a97bfcfcda3afe963435d919cb95634f27375f0aadf6494dad47e506c888bf15cb5633d5f81dbb793b05b27e676@158.255.0.178:30312,enode://266dfa5fd0152c3ec2b21ac71c5ae8c263c748b417feac2d2b6b3ff8b0d64e435e7d91d079856ec7a997d3f3ead62d5bd7922ffae7937893179b36d7ae7886e9@38.102.124.102:30312,enode://c49dbc8ab18ccbbde295484b307d07f3022c418e36e40666f6b9d333604c16b0c0dd1757b5920962ecc0a4ddd2c164028e1837e5123db60a0c2d8f223a6b54aa@167.224.64.168:30312,enode://f37ea965454180d4bc4b2be95e66e9621b6d0b16be9be5e2b3c67d32e1493af0b178d4e5820f57ed76c1ad2841baf9739379246caef6f74dc2ae0fcb9141537c@5.189.191.87:30312,enode://f8c9be8bf0761c9e31374e4583f2952755f870c24f0976d64a1647a4ae2aaa9797d5dd84c0b9852e7ba6c02f1e8a35e2fb3f54ab38ae3b4ef9e62434c00dcb66@185.70.105.62:30312,enode://9724b9cff3ae4286d13b29d2e13c1db0a3ce8ed1d469b945b4f626edf42d4043375be474bf94abd9065c52a840e207a26d6c4a86de87263d1cf0f8af561d1c2a@104.152.209.185:30312}"
    FLEET_ENODES="${FLEET_ENODES:-enode://8afedf8925a39e23531b0bd3e68256a80083bc2b41ce7674ad8ebf766d933a8242ed41d81ab5245e3695fe207213fc7d5a2bd390ed32d067ae199123a61bc1bd@95.217.56.168:30405,enode://fc786acefeb8dd9709d85ef60cebcc690d12e2323ec0ca4a4bb40d2ca98c4e291951a4ceb131edf11e1918183d0b8151b059ee86742d97474287e2b326cabcb5@65.21.71.4:30406,enode://6d97d8de6524ad126e8c902eeff81702859cb73d9221d2828f22158a2375e419313d5ef01fab7d0518f275beca57cba4fd8cc206285f4f8ccb62e2b132edad8e@135.181.117.109:30406,enode://e8bd8799e30fbd1ea512f7250eab185dce26a13851630a6037c67e2d8f18ac3f4fd695e43e6fc06648deab669a120961c0391068c7e51ec1b9f88a47d268edbb@65.21.71.4:30322}"
fi

# ─── Operator fast-sync pivot (optional) ───────────────────────────────
PIVOT_FLAGS=()
_pn="${FASTSYNC_PIVOT_NUMBER:-}"
_ph="${FASTSYNC_PIVOT_HASH:-}"
_pr="${FASTSYNC_PIVOT_ROOT:-}"
if [[ -n "$_pn" || -n "$_ph" || -n "$_pr" ]]; then
    if [[ -z "$_pn" || -z "$_ph" || -z "$_pr" ]]; then
        echo "ERROR: FASTSYNC_PIVOT_{NUMBER,HASH,ROOT} must all be set together." >&2
        exit 1
    fi
    PIVOT_FLAGS=(--fastsyncpivotnumber "$_pn" --fastsyncpivothash "$_ph" --fastsyncpivotroot "$_pr")
fi

# ─── Port layout ───────────────────────────────────────────────────────
# Offset from PORT_BASE_HASH/PATH; HTTP = base+8245+offset, authrpc = base+8251+offset.
# fast/fastpath live at offset 3/103 to avoid clashing with existing modes.
ports_for() {
    local base_hash=$PORT_BASE_HASH
    local base_path=$PORT_BASE_PATH
    case "$1" in
        fast)          echo $((base_hash + 3)) ;;
        full)          echo $((base_hash + 2)) ;;
        snapfull)      echo $((base_hash + 6)) ;;
        snap)          echo $((base_hash + 4)) ;;
        archive)       echo $((base_hash + 5)) ;;
        fastpath)      echo $((base_path + 3)) ;;
        fullpath)      echo $((base_path + 2)) ;;
        snappath)      echo $((base_path + 4)) ;;
        archivepath)   echo $((base_path + 5)) ;;
        snapfullpath)  echo $((base_path + 6)) ;;
        *) echo ""; return 1 ;;
    esac
}

# ─── Mode configuration ────────────────────────────────────────────────
modes_for() {
    case "$1" in
        fast|fastpath) echo "fast full" ;;
        full|fullpath) echo "$FULL_SYNCMODE_DEFAULT full" ;;
        snapfull|snapfullpath) echo "full full" ;;
        snap|snappath) echo "snap full" ;;
        archive|archivepath) echo "full archive" ;;
        *) echo ""; return 1 ;;
    esac
}

scheme_for() {
    case "$1" in
        *path) echo "path" ;;
        *)     echo "hash" ;;
    esac
}

ALL_MODES="fast full snapfull snap archive fastpath fullpath snapfullpath snappath archivepath"
HASH_MODES="fast full snapfull snap archive"
PATH_MODES="fastpath fullpath snapfullpath snappath archivepath"

# ─── Helpers ───────────────────────────────────────────────────────────
need_binary() {
    if [[ ! -x "$GETH_BIN" ]]; then
        echo "ERROR: geth binary not found at: $GETH_BIN" >&2
        echo "Build first: (cd $REPO_ROOT && make geth)" >&2
        exit 1
    fi
}

read_enode_loopback() {
    local datadir="$1"
    local ipc="$datadir/geth.ipc"
    [[ ! -S "$ipc" ]] && echo "" && return
    local raw
    raw="$(echo 'admin.nodeInfo.enode' | "$GETH_BIN" attach "$ipc" 2>/dev/null | grep enode | head -1 | tr -d '"')"
    [[ -z "$raw" ]] && echo "" && return
    echo "$raw" | sed 's|@[0-9a-fA-F:.][0-9a-fA-F:.]*:|@127.0.0.1:|'
}

is_running() {
    local pidfile="$1"
    [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null
}

already_running() {
    local mode="$1"
    local datadir="$BASE_DIR/$mode"
    is_running "$BASE_DIR/$mode.pid" && return 0
    ps -eo cmd 2>/dev/null | grep -F " --datadir $datadir " | grep -v grep | grep -q . && return 0
    return 1
}

guard_duplicate() {
    local mode="$1"
    if already_running "$mode"; then
        local pid="?"
        [[ -f "$BASE_DIR/$mode.pid" ]] && pid="$(cat "$BASE_DIR/$mode.pid")"
        echo "  [skip] $mode already running (pid $pid)"
        return 0
    fi
    return 1
}

ethstats_instance_name() {
    local mode="$1"
    local parts=("$HOSTNAME_SHORT")
    [[ -n "$NETWORK"       ]] && parts+=("$NETWORK")
    parts+=("$mode")
    local scheme; scheme="$(scheme_for "$mode")"
    [[ -n "$scheme"        ]] && parts+=("$scheme")
    [[ -n "$OS_NAME"       ]] && parts+=("$OS_NAME")
    [[ -n "$ARCH_NAME"     ]] && parts+=("$ARCH_NAME")
    [[ -n "$GETH_VERSION"  ]] && parts+=("$GETH_VERSION")
    [[ -n "$GETH_COMMIT"   ]] && parts+=("$GETH_COMMIT")
    [[ -n "$LOCATION"      ]] && parts+=("$LOCATION")
    local joined IFS='-'
    joined="${parts[*]}"
    echo "$joined" | tr ' /' '__' | LC_ALL=C tr -cd 'A-Za-z0-9_.-'
}

ethstats_arg() {
    local mode="$1"
    if [[ -n "$ETHSTATS_URL" ]]; then echo "--ethstats" "$ETHSTATS_URL"; return; fi
    [[ -z "$ETHSTATS_SECRET" || -z "$ETHSTATS_HOST" ]] && return
    local name; name="$(ethstats_instance_name "$mode")"
    [[ -n "$name" ]] && echo "--ethstats" "${name}:${ETHSTATS_SECRET}@${ETHSTATS_HOST}"
}

ensure_genesis() {
    local datadir="$1"
    if [[ ! -d "$datadir/geth/chaindata" ]] && [[ -f "$GENESIS" ]]; then
        echo "  Initializing genesis at $datadir"
        "$GETH_BIN" init --datadir "$datadir" "$GENESIS" >/dev/null 2>&1 || true
    fi
}

perf_args() {
    local mode="$1"
    local gcmode="full"
    case "$mode" in archive|archivepath) gcmode="archive" ;; esac
    local out=( --cache "$CACHE_MB" --maxpeers "$MAXPEERS" --txlookuplimit 0 )
    if [[ "$gcmode" == "archive" ]]; then
        out+=( --cache.database 60 --cache.gc 0 --cache.snapshot 0 --cache.trie 30 )
    else
        out+=( --cache.database 50 --cache.gc 15 --cache.snapshot 15 --cache.trie 15 )
    fi
    printf '%s\n' "${out[@]}"
}

scheme_args() {
    local mode="$1"
    local scheme; scheme="$(scheme_for "$mode")"
    if [[ "$scheme" == "path" ]]; then
        echo "--state.scheme=path"
        [[ -n "${STATE_HISTORY:-}" ]] && echo "--history.state=$STATE_HISTORY"
    fi
}

http_flags_into() {
    local httpp="$1"
    HTTP_FLAGS_OUT=(
        --http
        --http.addr   "${HTTP_ADDR:-127.0.0.1}"
        --http.port   "$httpp"
        --http.api    "${HTTP_API:-eth,net,web3}"
        --http.vhosts "${HTTP_VHOSTS:-localhost}"
    )
    [[ -n "${HTTP_CORS:-}" ]] && HTTP_FLAGS_OUT+=( --http.corsdomain "$HTTP_CORS" )
}

write_node_config() {
    local mode="$1" enodes_csv="$2"
    local datadir="$BASE_DIR/$mode"
    mkdir -p "$datadir/geth"
    local conf="$datadir/geth/config.toml"
    local entries="" trusted_entries=""
    if [[ -n "$enodes_csv" ]]; then
        IFS=',' read -r -a arr <<< "$enodes_csv"
        for e in "${arr[@]}"; do
            [[ -z "$e" ]] && continue
            entries="${entries:+$entries, }\"$e\""
        done
    fi
    if [[ -n "${EXTRA_STATIC_NODES:-}" ]]; then
        IFS=',' read -r -a extra_arr <<< "$EXTRA_STATIC_NODES"
        for e in "${extra_arr[@]}"; do
            [[ -z "$e" ]] && continue
            entries="${entries:+$entries, }\"$e\""
            trusted_entries="${trusted_entries:+$trusted_entries, }\"$e\""
        done
    fi
    cat > "$conf" <<EOF
[Node.P2P]
StaticNodes = [$entries]
TrustedNodes = [$trusted_entries]
MaxPeers = 25
EOF
}

append_fleet_enodes() {
    [[ -z "${FLEET_ENODES:-}" ]] && return 0
    local my_ip; my_ip="$(curl -s -m 3 ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')"
    local fleet_filtered="" e
    IFS=',' read -r -a fleet_arr <<< "$FLEET_ENODES"
    for e in "${fleet_arr[@]}"; do
        [[ -z "$e" ]] && continue
        [[ -n "$my_ip" && "$e" == *"@$my_ip:"* ]] && { echo "  (skipping self-fleet-enode at $my_ip)" >&2; continue; }
        fleet_filtered="${fleet_filtered:+$fleet_filtered,}$e"
    done
    [[ -n "$fleet_filtered" ]] && echo "  (fleet peers: $(echo "$fleet_filtered" | tr ',' '\n' | wc -l | tr -d ' '))" >&2
    echo "$fleet_filtered"
}

wait_for_ipc() {
    local datadir="$1" max="${2:-60}"
    local i; for i in $(seq 1 "$max"); do [[ -S "$datadir/geth.ipc" ]] && return 0; sleep 1; done
    return 1
}

# ─── start_fast — fast-sync with A.89 dynamic pivot ────────────────────
start_fast() { start_fast_for "fast"; }
start_fast_for() {
    local mode="${1:-fast}"
    if guard_duplicate "$mode"; then return 0; fi
    local port; port="$(ports_for "$mode")"
    local httpp=$((8545 + port - PORT_BASE_HASH))
    local authp=$((8551 + port - PORT_BASE_HASH))
    local scheme; scheme="$(scheme_for "$mode")"
    [[ "$scheme" == "path" ]] && httpp=$((8545 + port - PORT_BASE_PATH)) && authp=$((8551 + port - PORT_BASE_PATH))
    local datadir="$BASE_DIR/$mode"
    echo "Starting $mode node (port $port, scheme=$scheme, fast-sync dynamic-pivot A.89 — $NETWORK)..."
    mkdir -p "$datadir"

    local args=(
        --datadir "$datadir"
        --networkid "$NETWORK_ID" $NETWORK_FLAG
        --port "$port"
        --syncmode fast
        --gcmode full
        --authrpc.port "$authp" --authrpc.addr 127.0.0.1
        --verbosity 3
        "${PIVOT_FLAGS[@]+"${PIVOT_FLAGS[@]}"}"
    )
    http_flags_into "$httpp"; args+=( "${HTTP_FLAGS_OUT[@]}" )
    args+=( $(scheme_args "$mode") )
    args+=( $(perf_args "$mode") )
    args+=( $(ethstats_arg "$mode") )

    nohup "$GETH_BIN" "${args[@]}" > "$BASE_DIR/$mode.log" 2>&1 &
    echo $! > "$BASE_DIR/$mode.pid"
    echo "  $mode PID: $(cat "$BASE_DIR/$mode.pid")  HTTP: http://127.0.0.1:$httpp"
}

# ─── start_full ─────────────────────────────────────────────────────────
start_full() { start_full_for "full"; }
start_full_for() {
    local mode="${1:-full}"
    if guard_duplicate "$mode"; then return 0; fi
    local port; port="$(ports_for "$mode")"
    local scheme; scheme="$(scheme_for "$mode")"
    local base; [[ "$scheme" == "path" ]] && base=$PORT_BASE_PATH || base=$PORT_BASE_HASH
    local httpp=$((8545 + port - base))
    local authp=$((8551 + port - base))
    local datadir="$BASE_DIR/$mode"
    local sm gm; read -r sm gm < <(modes_for "$mode")
    echo "Starting $mode node (port $port, scheme=$scheme, syncmode=$sm gcmode=$gm — $NETWORK)..."
    mkdir -p "$datadir"
    ensure_genesis "$datadir"

    local args=(
        --datadir "$datadir"
        --networkid "$NETWORK_ID" $NETWORK_FLAG
        --port "$port"
        --gcmode "$gm" --syncmode "$sm"
        --bootnodes "$BOOTNODES"
        --authrpc.port "$authp" --authrpc.addr 127.0.0.1
        --verbosity 4
    )
    http_flags_into "$httpp"; args+=( "${HTTP_FLAGS_OUT[@]}" )
    args+=( $(scheme_args "$mode") )
    args+=( $(perf_args "$mode") )
    args+=( $(ethstats_arg "$mode") )

    nohup "$GETH_BIN" "${args[@]}" > "$BASE_DIR/$mode.log" 2>&1 &
    echo $! > "$BASE_DIR/$mode.pid"
    echo "  $mode PID: $(cat "$BASE_DIR/$mode.pid")"

    wait_for_ipc "$datadir" 60 || true
    local enode; enode="$(read_enode_loopback "$datadir")"
    [[ -n "$enode" ]] && echo "$enode" > "$BASE_DIR/$mode.enode" && echo "  $mode enode: $enode"
}

# ─── start_snapfull ─────────────────────────────────────────────────────
start_snapfull() { start_snapfull_for "snapfull"; }
start_snapfull_for() {
    local mode="${1:-snapfull}"
    if guard_duplicate "$mode"; then return 0; fi
    local port; port="$(ports_for "$mode")"
    local scheme; scheme="$(scheme_for "$mode")"
    local base; [[ "$scheme" == "path" ]] && base=$PORT_BASE_PATH || base=$PORT_BASE_HASH
    local httpp=$((8545 + port - base))
    local authp=$((8551 + port - base))
    local datadir="$BASE_DIR/$mode"
    local source_full="full"; local source_archive="archive"
    [[ "$scheme" == "path" ]] && source_full="fullpath" && source_archive="archivepath"

    echo "Starting $mode node (port $port, scheme=$scheme, full-sync from genesis — $NETWORK)..."
    mkdir -p "$datadir"
    ensure_genesis "$datadir"

    local enodes=""
    [[ -f "$BASE_DIR/$source_full.enode"    ]] && enodes="$(cat "$BASE_DIR/$source_full.enode")"    && echo "  + $source_full as static peer"
    [[ -f "$BASE_DIR/$source_archive.enode" ]] && enodes="${enodes:+$enodes,}$(cat "$BASE_DIR/$source_archive.enode")" && echo "  + $source_archive as static peer"
    [[ -z "$enodes" ]] && echo "  (no local peers — syncing standalone via public bootnodes)"

    local fleet_enodes; fleet_enodes="$(append_fleet_enodes)"
    [[ -n "$fleet_enodes" ]] && enodes="${enodes:+$enodes,}$fleet_enodes" && export EXTRA_STATIC_NODES="${EXTRA_STATIC_NODES:+$EXTRA_STATIC_NODES,}$fleet_enodes"
    write_node_config "$mode" "$enodes"

    local args=(
        --datadir "$datadir" --config "$datadir/geth/config.toml"
        --networkid "$NETWORK_ID" $NETWORK_FLAG
        --port "$port" --gcmode full --syncmode full
        --bootnodes "$BOOTNODES"
        --authrpc.port "$authp" --authrpc.addr 127.0.0.1
        --verbosity 4
    )
    http_flags_into "$httpp"; args+=( "${HTTP_FLAGS_OUT[@]}" )
    args+=( $(scheme_args "$mode") )
    args+=( $(perf_args "$mode") )
    args+=( $(ethstats_arg "$mode") )

    nohup "$GETH_BIN" "${args[@]}" > "$BASE_DIR/$mode.log" 2>&1 &
    echo $! > "$BASE_DIR/$mode.pid"
    echo "  $mode PID: $(cat "$BASE_DIR/$mode.pid")"

    wait_for_ipc "$datadir" 30 || true
    local enode; enode="$(read_enode_loopback "$datadir")"
    [[ -n "$enode" ]] && echo "$enode" > "$BASE_DIR/$mode.enode" && echo "  $mode enode: $enode"
}

# ─── start_archive ──────────────────────────────────────────────────────
start_archive() { start_archive_for "archive"; }
start_archive_for() {
    local mode="${1:-archive}"
    if guard_duplicate "$mode"; then return 0; fi
    local port; port="$(ports_for "$mode")"
    local scheme; scheme="$(scheme_for "$mode")"
    local base; [[ "$scheme" == "path" ]] && base=$PORT_BASE_PATH || base=$PORT_BASE_HASH
    local httpp=$((8545 + port - base))
    local authp=$((8551 + port - base))
    local datadir="$BASE_DIR/$mode"
    local source_mode="full"; [[ "$scheme" == "path" ]] && source_mode="fullpath"

    echo "Starting $mode node (port $port, scheme=$scheme, archive — $NETWORK)..."
    mkdir -p "$datadir"
    ensure_genesis "$datadir"

    local enodes=""
    [[ -f "$BASE_DIR/$source_mode.enode" ]] && enodes="$(cat "$BASE_DIR/$source_mode.enode")" && echo "  + $source_mode as static peer"
    [[ -z "$enodes" ]] && echo "  (no local peers — syncing standalone)"

    local fleet_enodes; fleet_enodes="$(append_fleet_enodes)"
    [[ -n "$fleet_enodes" ]] && enodes="${enodes:+$enodes,}$fleet_enodes" && export EXTRA_STATIC_NODES="${EXTRA_STATIC_NODES:+$EXTRA_STATIC_NODES,}$fleet_enodes"
    write_node_config "$mode" "$enodes"

    local args=(
        --datadir "$datadir" --config "$datadir/geth/config.toml"
        --networkid "$NETWORK_ID" $NETWORK_FLAG
        --port "$port" --gcmode archive --syncmode full
        --bootnodes "$BOOTNODES"
        --authrpc.port "$authp" --authrpc.addr 127.0.0.1
        --verbosity 4
    )
    http_flags_into "$httpp"; args+=( "${HTTP_FLAGS_OUT[@]}" )
    args+=( $(scheme_args "$mode") )
    args+=( $(perf_args "$mode") )
    args+=( $(ethstats_arg "$mode") )

    nohup "$GETH_BIN" "${args[@]}" > "$BASE_DIR/$mode.log" 2>&1 &
    echo $! > "$BASE_DIR/$mode.pid"
    echo "  $mode PID: $(cat "$BASE_DIR/$mode.pid")"

    wait_for_ipc "$datadir" 30 || true
    local enode; enode="$(read_enode_loopback "$datadir")"
    [[ -n "$enode" ]] && echo "$enode" > "$BASE_DIR/$mode.enode" && echo "  $mode enode: $enode"
}

# ─── start_sync_node — snap/snappath ────────────────────────────────────
start_sync_node() {
    local mode="$1" port="$2" syncmode="$3" gcmode="$4"
    if guard_duplicate "$mode"; then return 0; fi
    local scheme; scheme="$(scheme_for "$mode")"
    local base; [[ "$scheme" == "path" ]] && base=$PORT_BASE_PATH || base=$PORT_BASE_HASH
    local httpp=$((8545 + port - base))
    local authp=$((8551 + port - base))
    local datadir="$BASE_DIR/$mode"
    local archive_mode="archive" snapfull_mode="snapfull" source_full="full"
    [[ "$scheme" == "path" ]] && archive_mode="archivepath" && snapfull_mode="snapfullpath" && source_full="fullpath"

    local components=() enodes=""
    [[ -f "$BASE_DIR/$archive_mode.enode"  ]] && components+=("$(cat "$BASE_DIR/$archive_mode.enode")")  && echo "  + $archive_mode as static peer"
    [[ -f "$BASE_DIR/$snapfull_mode.enode" ]] && components+=("$(cat "$BASE_DIR/$snapfull_mode.enode")") && echo "  + $snapfull_mode as static peer"
    [[ -f "$BASE_DIR/$source_full.enode"   ]] && components+=("$(cat "$BASE_DIR/$source_full.enode")")   && echo "  + $source_full as static peer"
    [[ ${#components[@]} -eq 0 ]] && echo "  (no local peers — syncing standalone)"
    local IFS=','; enodes="${components[*]}"; unset IFS

    local fleet_enodes; fleet_enodes="$(append_fleet_enodes)"
    [[ -n "$fleet_enodes" ]] && enodes="${enodes:+$enodes,}$fleet_enodes" && export EXTRA_STATIC_NODES="${EXTRA_STATIC_NODES:+$EXTRA_STATIC_NODES,}$fleet_enodes"

    echo "Starting $mode node (port $port, scheme=$scheme, syncmode=$syncmode gcmode=$gcmode — $NETWORK)..."
    mkdir -p "$datadir"
    ensure_genesis "$datadir"
    write_node_config "$mode" "$enodes"

    local args=(
        --datadir "$datadir" --config "$datadir/geth/config.toml"
        --networkid "$NETWORK_ID" $NETWORK_FLAG
        --port "$port" --gcmode "$gcmode" --syncmode "$syncmode"
        --bootnodes "$BOOTNODES"
        --authrpc.port "$authp" --authrpc.addr 127.0.0.1
        --verbosity 4
    )
    http_flags_into "$httpp"; args+=( "${HTTP_FLAGS_OUT[@]}" )
    args+=( $(scheme_args "$mode") )
    args+=( $(perf_args "$mode") )
    args+=( $(ethstats_arg "$mode") )

    nohup "$GETH_BIN" "${args[@]}" > "$BASE_DIR/$mode.log" 2>&1 &
    echo $! > "$BASE_DIR/$mode.pid"
    echo "  $mode PID: $(cat "$BASE_DIR/$mode.pid")"
}

# ─── Top-level dispatch ────────────────────────────────────────────────
do_start() {
    local target="${1:-all}"
    local use_hash=0
    [[ "${2:-}" == "--hash" ]] && use_hash=1

    need_binary
    mkdir -p "$BASE_DIR"

    echo "Network: $NETWORK | Base: $BASE_DIR"

    local m_fast="fastpath" m_full="fullpath" m_archive="archivepath" m_snapfull="snapfullpath" m_snap="snappath"
    if [[ "$use_hash" == "1" ]]; then
        m_fast="fast"; m_full="full"; m_archive="archive"; m_snapfull="snapfull"; m_snap="snap"
    fi

    case "$target" in
      all)
        start_fast_for "$m_fast"
        start_full_for "$m_full"
        start_archive_for "$m_archive"
        start_snapfull_for "$m_snapfull"
        if [[ "$use_hash" == "1" ]]; then
            start_sync_node snap $((PORT_BASE_HASH+4)) snap full
        else
            start_sync_node snappath $((PORT_BASE_PATH+4)) snap full
        fi
        ;;
      fast|fastpath)     start_fast_for "$m_fast" ;;
      full|fullpath)     start_full_for "$m_full" ;;
      archive|archivepath) start_archive_for "$m_archive" ;;
      snapfull|snapfullpath) start_snapfull_for "$m_snapfull" ;;
      snap)
        [[ "$use_hash" == "1" ]] && start_sync_node snap $((PORT_BASE_HASH+4)) snap full \
                                  || start_sync_node snappath $((PORT_BASE_PATH+4)) snap full ;;
      snappath)
        start_sync_node snappath $((PORT_BASE_PATH+4)) snap full ;;
      path)
        start_fast_for fastpath
        start_full_for fullpath
        start_archive_for archivepath
        start_snapfull_for snapfullpath
        start_sync_node snappath $((PORT_BASE_PATH+4)) snap full ;;
      hash)
        start_fast_for fast
        start_full_for full
        start_archive_for archive
        start_snapfull_for snapfull
        start_sync_node snap $((PORT_BASE_HASH+4)) snap full ;;
      *)
        echo "ERROR: unknown target '$target'" >&2; usage; exit 1 ;;
    esac

    echo "Done. Logs: $BASE_DIR/{fast,full,snapfull,snap,archive,...}.log"
}

do_stop() {
    echo "Stopping all $NETWORK nodes..."
    local mode pid pidfile pids_to_wait=()
    for mode in $ALL_MODES; do
        pidfile="$BASE_DIR/$mode.pid"
        if [[ -f "$pidfile" ]]; then
            pid="$(cat "$pidfile")"
            if kill -0 "$pid" 2>/dev/null; then
                echo "  $mode (pid $pid) → SIGTERM"
                kill "$pid" 2>/dev/null || true
                pids_to_wait+=("$pid")
            fi
            rm -f "$pidfile"
        fi
    done
    local waited=0
    for pid in "${pids_to_wait[@]}"; do
        while kill -0 "$pid" 2>/dev/null; do
            if (( waited >= 30 )); then echo "  pid $pid still alive — SIGKILL"; kill -9 "$pid" 2>/dev/null || true; break; fi
            sleep 1; waited=$(( waited + 1 ))
        done
    done
    echo "All $NETWORK nodes stopped."
}

do_status() {
    need_binary >/dev/null 2>&1 || true
    echo "Sync status ($NETWORK, base=$BASE_DIR):"
    local mode
    for mode in $ALL_MODES; do
        local pidfile="$BASE_DIR/$mode.pid"
        if [[ -f "$pidfile" ]] && is_running "$pidfile"; then
            echo "  $mode: RUNNING (pid $(cat "$pidfile"))"
        elif [[ -f "$pidfile" ]]; then
            echo "  $mode: STOPPED (stale pid)"
            rm -f "$pidfile"
        else
            echo "  $mode: NOT STARTED"
        fi
    done
}

do_logs()   { local m="${1:-fast}";   local f="$BASE_DIR/$m.log";  [[ -f "$f" ]] && tail -f "$f" || { echo "No log at $f" >&2; exit 1; }; }
do_attach() { local m="${1:-fast}";   local i="$BASE_DIR/$m/geth.ipc"; [[ -S "$i" ]] && "$GETH_BIN" attach "$i" || { echo "IPC not available at $i" >&2; exit 1; }; }
do_enode()  { local m="${1:-full}";   local e; e="$(read_enode_loopback "$BASE_DIR/$m")"; [[ -n "$e" ]] && echo "$e" || { echo "ERROR: IPC unavailable for $m" >&2; exit 1; }; }

do_clean() {
    echo "This will delete ALL $NETWORK chaindata under $BASE_DIR"
    read -r -p "Type 'yes' to confirm: " confirm
    [[ "$confirm" != "yes" ]] && echo "Cancelled." && exit 0
    do_stop
    for mode in $ALL_MODES; do
        rm -rf "$BASE_DIR/$mode"
        rm -f "$BASE_DIR/$mode.log" "$BASE_DIR/$mode.pid" "$BASE_DIR/$mode.enode"
    done
    echo "Done."
}

do_wipe_stateful() {
    do_stop
    for mode in archive snapfull snap fast; do
        rm -rf "$BASE_DIR/$mode"
        rm -f "$BASE_DIR/$mode.log" "$BASE_DIR/$mode.pid" "$BASE_DIR/$mode.enode"
    done
    echo "Wiped archive/snapfull/snap/fast chaindata. full retained."
}

do_restore() {
    local mode="${1:-}" snapshot="${2:-}"
    [[ -z "$mode" || -z "$snapshot" ]] && { echo "Usage: $0 restore <mode> <snapshot.tar.zst>" >&2; exit 1; }
    ports_for "$mode" >/dev/null 2>&1 || { echo "ERROR: unknown mode '$mode'" >&2; exit 1; }
    [[ ! -f "$snapshot" ]] && snapshot="$SNAPSHOTS_DIR/$snapshot"
    [[ ! -f "$snapshot" ]] && { echo "ERROR: snapshot not found" >&2; exit 1; }
    need_binary
    command -v zstd >/dev/null 2>&1 || { echo "ERROR: zstd not installed" >&2; exit 1; }

    local datadir="$BASE_DIR/$mode"
    echo "Restoring $mode from $snapshot..."
    already_running "$mode" && { local pid="?"; [[ -f "$BASE_DIR/$mode.pid" ]] && pid="$(cat "$BASE_DIR/$mode.pid")"; kill "$pid" 2>/dev/null || true; sleep 3; }
    rm -rf "$datadir"; mkdir -p "$datadir"
    rm -f "$BASE_DIR/$mode.log" "$BASE_DIR/$mode.enode"

    local first strip=0
    first="$(zstd -dc "$snapshot" 2>/dev/null | tar -tf - 2>/dev/null | head -1 || true)"
    case "$first" in "$mode"/*|"$mode/") strip=1 ;; esac
    (( strip > 0 )) && zstd -dc "$snapshot" | tar -xf - -C "$datadir" --strip-components=1 \
                     || zstd -dc "$snapshot" | tar -xf - -C "$datadir"

    [[ ! -d "$datadir/geth/chaindata" ]] && { echo "ERROR: extraction failed" >&2; exit 1; }
    echo "Restore complete; starting with public HTTP RPC..."
    export HTTP_ADDR="0.0.0.0" HTTP_VHOSTS="*" HTTP_CORS="*"
    case "$mode" in
        fast|fastpath)         start_fast_for     "$mode" ;;
        full|fullpath)         start_full_for     "$mode" ;;
        snapfull|snapfullpath) start_snapfull_for "$mode" ;;
        archive|archivepath)   start_archive_for  "$mode" ;;
        snap)                  start_sync_node snap     $((PORT_BASE_HASH+4)) snap full ;;
        snappath)              start_sync_node snappath $((PORT_BASE_PATH+4)) snap full ;;
        *) echo "ERROR: no launcher for '$mode'" >&2; exit 1 ;;
    esac
}

usage() {
    cat <<EOF
Usage: $0 [--network mainnet|apothem] <command> [args]

Networks: mainnet | apothem (default)
  Override via --network flag or NETWORK env var.

Commands:
  start [all|<mode>] [--hash]     Start nodes (default: all path-scheme)
                                  Modes: fast fastpath full fullpath snapfull
                                         snapfullpath snap snappath archive
                                         archivepath
                                  Groups: all path hash
                                  --hash: start hash-scheme variant
  stop                            Stop all nodes for this network
  status                          Show per-node state
  logs   [mode]                   tail -f mode's log (default: fast)
  attach [mode]                   geth attach IPC
  enode  [mode]                   Print node enode (loopback IP)
  clean                           Delete ALL chaindata (asks)
  wipe-stateful                   Wipe archive/snapfull/snap/fast (full kept)
  restore <mode> <snap.tar.zst>   Restore from snapshot + start with public RPC

Examples:
  # Fast-sync both networks (dynamic pivot — no operator flags needed)
  scripts/local-sync-test.sh --network mainnet start fast
  scripts/local-sync-test.sh --network apothem start fast

  # Full cluster (path scheme default)
  scripts/local-sync-test.sh start all

  # Legacy hash cluster
  scripts/local-sync-test.sh start all --hash

  # Mainnet archive node
  NETWORK=mainnet scripts/local-sync-test.sh start archive

Environment:
  GETH_BIN, BASE_DIR, NETWORK, CACHE_MB, MAXPEERS, FULL_SYNCMODE
  FASTSYNC_PIVOT_{NUMBER,HASH,ROOT} (optional — A.89 dynamic pivot is default)
  ETHSTATS_SECRET, ETHSTATS_HOST, ETHSTATS_URL, HOSTNAME_OVERRIDE, LOCATION
  HTTP_ADDR, HTTP_VHOSTS, HTTP_CORS, HTTP_API
  SNAPSHOTS_DIR (default /mnt/data/snapshots)
  FLEET_ENODES (override fleet static+trusted peers)
EOF
}

cmd="${1:-status}"
shift || true
case "$cmd" in
    start)  do_start  "${1:-all}" "${2:-}" ;;
    stop)   do_stop ;;
    status) do_status ;;
    logs)   do_logs   "${1:-fast}" ;;
    attach) do_attach "${1:-fast}" ;;
    enode)  do_enode  "${1:-full}" ;;
    clean)  do_clean ;;
    wipe-stateful) do_wipe_stateful ;;
    restore) do_restore "${1:-}" "${2:-}" ;;
    -h|--help|help) usage ;;
    *)      usage; exit 1 ;;
esac
