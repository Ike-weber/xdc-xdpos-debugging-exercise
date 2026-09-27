#!/usr/bin/env bash
# validate-canonical-parity.sh — verify a test node's block data matches a
# canonical Apothem (or XDC mainnet) reference, byte-for-byte on the consensus
# fields.
#
# Compares samples from a LOCAL geth attached over IPC against a remote
# JSON-RPC endpoint. By design the test node does not need HTTP RPC exposed —
# only IPC is required. The reference endpoint can be any standard ETH JSON-RPC
# (public Apothem RPC, a canonical v2.6.8 archive, or another node you trust).
#
# A matching `hash` field is sufficient proof of consensus parity for a block:
# the block hash is a digest of every consensus field. We also surface
# `stateRoot`, `transactionsRoot`, `receiptsRoot` independently so that when a
# hash mismatch happens, the operator immediately sees which dimension diverged.
#
# Usage
# -----
#   scripts/validate-canonical-parity.sh -i <ipc-path> [options]
#
#   -i, --ipc PATH       Test node IPC path. Mutually exclusive with --test-rpc.
#                        Exactly one of -i / --test-rpc is required.
#       --test-rpc URL   Test node JSON-RPC URL (e.g. http://127.0.0.1:8549).
#                        Preferred over IPC: no need for the geth binary, no
#                        attach subprocess overhead, ~10x faster on large
#                        sample counts. Mutually exclusive with --ipc.
#   -g, --geth PATH      geth binary used for IPC attach (default: build/bin/geth).
#                        Only consulted when --ipc is used.
#       --test-ssh CMD   SSH prefix wrapped around the test-side `geth attach`
#                        invocation, so the test IPC can live on a remote host.
#                        Only used with --ipc; ignored with --test-rpc.
#                        e.g. --test-ssh "ssh -p 12141 root@135.181.117.109"
#                        (default: empty — local execution).
#   -r, --ref URL        Reference JSON-RPC URL (default: https://erpc.apothem.network).
#                        Mutually exclusive with --ref-docker.
#       --ref-docker C   Reference is a docker container running geth/XDC; queries
#                        via `docker exec C XDC attach`. Pair with --ref-ipc.
#       --ref-ipc PATH   IPC path inside the docker container
#                        (default: /work/xdcchain/XDC.ipc).
#       --ref-bin NAME   Binary name to call inside the container (default: XDC).
#       --ref-ssh CMD    Prefix wrapped around `docker exec ...` so the
#                        container can live on a remote host, e.g.
#                        --ref-ssh "ssh -p 12141 root@95.217.56.168"
#                        (default: empty — local docker).
#   -s, --samples N      Random samples in addition to fixed milestones (default: 20).
#   -f, --from N         Lower bound for the random sample range (default: 1).
#   -t, --to N           Upper bound for the random sample range
#                        (default: min(test_head, ref_head)).
#   -d, --deep           Also compare receipts (one extra RPC call per sample).
#   -q, --quick          Skip random samples; check only the fixed milestones.
#   -j, --json           Emit a JSON summary on the last line (stdout).
#       --no-color       Disable ANSI colour in human-readable output.
#       --help           Show this help.
#
# Examples
# --------
#   # Quick check against archive on xdc02 using the default public ref RPC:
#   scripts/validate-canonical-parity.sh \
#       -i nodes-local/archive/geth.ipc -q
#
#   # Deep check (headers + receipts) of 100 random samples, JSON summary:
#   scripts/validate-canonical-parity.sh \
#       -i nodes-local/archive/geth.ipc -s 100 -d -j
#
#   # Compare against a specific reference (e.g. a trusted v2.6.8 node):
#   scripts/validate-canonical-parity.sh \
#       -i nodes-local/archive/geth.ipc \
#       -r https://my-trusted-archive:8545
#
# Exit codes
# ----------
#   0  all sampled blocks matched on every compared field
#   1  one or more mismatches
#   2  invalid arguments or unreachable endpoints
#
# Notes
# -----
#   * The IPC side returns numbers as decimals; the RPC side returns hex. Both
#     are normalised to hex without leading zeros before comparison.
#   * `totalDifficulty`, `size`, and the XDC-specific `validators`/`penalties`
#     fields are intentionally NOT compared:
#       - totalDifficulty/size are representation-only, not consensus inputs
#       - validators/penalties are present on canonical Apothem RPC responses
#         but absent on a stock geth IPC reply unless XDC ext APIs are wired up.
#     A `hash` match implies all consensus fields match (hash is a digest of
#     all of them), so the script flags any consensus-relevant divergence
#     via the hash field even when an XDC-only field is omitted on one side.

set -u

# ---- defaults ----------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GETH_BIN="${GETH_BIN:-$REPO_ROOT/build/bin/geth}"
IPC_PATH=""
TEST_RPC=""                                  # if set, use JSON-RPC for the test side
TEST_SSH="${TEST_SSH:-}"                     # optional SSH prefix for remote-host test
REF_RPC="${REF_RPC:-https://erpc.apothem.network}"
REF_DOCKER=""                                # if non-empty, use docker exec instead of HTTP RPC
REF_IPC="${REF_IPC:-/work/xdcchain/XDC.ipc}" # IPC path inside the docker container
REF_BIN="${REF_BIN:-XDC}"                    # binary name inside the docker container
REF_SSH="${REF_SSH:-}"                       # optional SSH prefix for remote-host docker
SAMPLES=20
FROM=1
TO=""
DEEP=0
QUICK=0
JSON_OUT=0
COLOR=1

# Fixed milestones worth always checking (skipped if > TO):
# - 0, 1: genesis-adjacent
# - 100, 1k, 10k, 100k, 1M: order-of-magnitude
# - 1.755M: validator contract event range (issue #12 in this repo's task list)
# - 3,000,000: TIPSigning fork (XDC-critical)
# - 71,490,150: V2 switch (Apothem) — trusted checkpoint anchor
MILESTONES=(0 1 100 1000 10000 100000 1000000 1755000 3000000 71490150)

# ---- argument parsing --------------------------------------------------------
usage() {
    sed -n '/^# validate-canonical-parity/,/^$/p' "$0" \
        | sed -E 's/^# ?//' \
        | head -60
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -i|--ipc)       IPC_PATH="$2"; shift 2 ;;
        --test-rpc)     TEST_RPC="$2"; shift 2 ;;
        -g|--geth)      GETH_BIN="$2"; shift 2 ;;
        --test-ssh)     TEST_SSH="$2"; shift 2 ;;
        -r|--ref)       REF_RPC="$2"; shift 2 ;;
        --ref-docker)   REF_DOCKER="$2"; shift 2 ;;
        --ref-ipc)      REF_IPC="$2"; shift 2 ;;
        --ref-bin)      REF_BIN="$2"; shift 2 ;;
        --ref-ssh)      REF_SSH="$2"; shift 2 ;;
        -s|--samples)   SAMPLES="$2"; shift 2 ;;
        -f|--from)      FROM="$2"; shift 2 ;;
        -t|--to)        TO="$2"; shift 2 ;;
        -d|--deep)      DEEP=1; shift ;;
        -q|--quick)     QUICK=1; shift ;;
        -j|--json)      JSON_OUT=1; shift ;;
        --no-color)     COLOR=0; shift ;;
        --help)         usage ;;
        *)              echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

if [[ -n "$IPC_PATH" && -n "$TEST_RPC" ]]; then
    echo "error: --ipc and --test-rpc are mutually exclusive" >&2
    exit 2
fi
if [[ -z "$IPC_PATH" && -z "$TEST_RPC" ]]; then
    echo "error: exactly one of -i/--ipc or --test-rpc is required" >&2
    echo "run with --help for usage" >&2
    exit 2
fi
# Path checks only apply when the test side is local IPC. When --test-rpc is
# used we trust the URL; when --test-ssh wraps IPC we can't see the remote
# path from here.
if [[ -n "$IPC_PATH" && -z "$TEST_SSH" ]]; then
    if [[ ! -S "$IPC_PATH" ]]; then
        echo "error: IPC path is not a socket: $IPC_PATH" >&2
        exit 2
    fi
    if [[ ! -x "$GETH_BIN" ]]; then
        echo "error: geth binary not executable: $GETH_BIN" >&2
        exit 2
    fi
fi
for cmd in curl jq awk; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "error: missing required tool: $cmd" >&2; exit 2; }
done

# ---- colour helpers ----------------------------------------------------------
if [[ $COLOR -eq 1 && -t 1 ]]; then
    C_OK=$'\033[32m'; C_BAD=$'\033[31m'; C_DIM=$'\033[2m'; C_BOLD=$'\033[1m'; C_OFF=$'\033[0m'
else
    C_OK=""; C_BAD=""; C_DIM=""; C_BOLD=""; C_OFF=""
fi

# ---- RPC primitives ----------------------------------------------------------
# normalise_hex: turn either "42" (dec) or "0x2a" (hex) into "0x2a"; passes
# through hashes (long 0x... strings) verbatim.
normalise_hex() {
    local v="$1"
    if [[ "$v" =~ ^0x[0-9a-fA-F]+$ ]]; then
        # already hex; strip leading zeros after 0x for short numerics (keep
        # full-length 64-char hashes intact)
        if [[ ${#v} -le 18 ]]; then
            printf '0x%x' "$v"
        else
            printf '%s' "$v"
        fi
    elif [[ "$v" =~ ^[0-9]+$ ]]; then
        printf '0x%x' "$v"
    else
        printf '%s' "$v"
    fi
}

# rpc_call <url> <method> <params-json-array>
rpc_call() {
    local url="$1" method="$2" params="$3"
    curl -s --max-time 15 -X POST -H 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"method\":\"$method\",\"params\":$params,\"id\":1}" \
        "$url"
}

# docker_attach_exec <script-fragment> → output of `<bin> attach --exec <script>`
# inside the configured docker container, against REF_IPC. If REF_SSH is set,
# the docker command is run via that SSH prefix (so the container can live on a
# different host than this script).
docker_attach_exec() {
    local script="$1"
    if [[ -n "$REF_SSH" ]]; then
        # shellcheck disable=SC2086
        $REF_SSH "docker exec $REF_DOCKER $REF_BIN attach --exec '$script' $REF_IPC" 2>/dev/null
    else
        docker exec "$REF_DOCKER" "$REF_BIN" attach --exec "$script" "$REF_IPC" 2>/dev/null
    fi
}

# test_attach_exec <script-fragment> → output of `<geth> attach --exec <script>`
# against IPC_PATH. If TEST_SSH is set, wraps the call via that SSH prefix so
# the IPC can live on a remote host.
test_attach_exec() {
    local script="$1"
    if [[ -n "$TEST_SSH" ]]; then
        # shellcheck disable=SC2086
        $TEST_SSH "$GETH_BIN attach --exec '$script' $IPC_PATH" 2>/dev/null
    else
        "$GETH_BIN" attach --exec "$script" "$IPC_PATH" 2>/dev/null
    fi
}

# test_block_json N → JSON object with the block's header fields.
# Dispatches to RPC if TEST_RPC is set, else to IPC attach.
test_block_json() {
    local n="$1"
    if [[ -n "$TEST_RPC" ]]; then
        local hexn
        hexn=$(printf '0x%x' "$n")
        rpc_call "$TEST_RPC" "eth_getBlockByNumber" "[\"$hexn\",false]" | jq -c .result
    else
        local raw
        raw=$(test_attach_exec "JSON.stringify(eth.getBlock($n, false))" \
            | sed -e 's/^"//' -e 's/"$//' -e 's/\\"/"/g' -e 's/\\n/\n/g')
        if [[ -z "$raw" || "$raw" == "null" ]]; then
            printf 'null'
        else
            printf '%s' "$raw"
        fi
    fi
}

# test_block_number → integer
test_block_number() {
    if [[ -n "$TEST_RPC" ]]; then
        local hex
        hex=$(rpc_call "$TEST_RPC" "eth_blockNumber" '[]' | jq -r .result)
        printf '%d' "$hex"
    else
        test_attach_exec "eth.blockNumber" | tr -d '\n'
    fi
}

# test_receipts_count <N> → integer
test_receipts_count() {
    local n="$1"
    if [[ -n "$TEST_RPC" ]]; then
        local hexn
        hexn=$(printf '0x%x' "$n")
        rpc_call "$TEST_RPC" "eth_getBlockReceipts" "[\"$hexn\"]" \
            | jq -r '.result | length // 0'
    else
        test_attach_exec "eth.getBlock($n, true).transactions.length" | tr -d '\n'
    fi
}

# ref_block_number → integer; dispatches to RPC or docker exec
ref_block_number() {
    if [[ -n "$REF_DOCKER" ]]; then
        docker_attach_exec "eth.blockNumber" | tr -d '\n'
    else
        local hex
        hex=$(rpc_call "$REF_RPC" "eth_blockNumber" '[]' | jq -r .result)
        printf '%d' "$hex"
    fi
}

# ref_block_json <N> → JSON object string
ref_block_json() {
    local n="$1"
    if [[ -n "$REF_DOCKER" ]]; then
        docker_attach_exec "JSON.stringify(eth.getBlock($n, false))" \
            | sed -e 's/^"//' -e 's/"$//' -e 's/\\"/"/g' -e 's/\\n/\n/g'
    else
        local hexn
        hexn=$(printf '0x%x' "$n")
        rpc_call "$REF_RPC" "eth_getBlockByNumber" "[\"$hexn\",false]" | jq -c .result
    fi
}

# ref_receipts_count <N> → integer
ref_receipts_count() {
    local n="$1"
    if [[ -n "$REF_DOCKER" ]]; then
        docker_attach_exec "eth.getBlock($n, true).transactions.length" | tr -d '\n'
    else
        local hexn
        hexn=$(printf '0x%x' "$n")
        rpc_call "$REF_RPC" "eth_getBlockReceipts" "[\"$hexn\"]" \
            | jq -r '.result | length // 0'
    fi
}

# ---- preflight ---------------------------------------------------------------
echo "${C_BOLD}validate-canonical-parity${C_OFF}"
if [[ -n "$TEST_RPC" ]]; then
    echo "  test RPC : $TEST_RPC"
else
    echo "  test IPC : $IPC_PATH"
    echo "  geth     : $GETH_BIN"
fi
if [[ -n "$REF_DOCKER" ]]; then
    echo "  reference: docker exec $REF_DOCKER $REF_BIN attach $REF_IPC"
    command -v docker >/dev/null 2>&1 || { echo "error: docker not in PATH" >&2; exit 2; }
else
    echo "  reference: $REF_RPC"
fi

TEST_HEAD=$(test_block_number)
if [[ -z "$TEST_HEAD" || "$TEST_HEAD" == "null" || "$TEST_HEAD" == "0" ]]; then
    if [[ -n "$TEST_RPC" ]]; then
        echo "error: could not read eth_blockNumber from test RPC ($TEST_RPC)" >&2
    else
        echo "error: could not read eth.blockNumber from test IPC" >&2
    fi
    exit 2
fi
REF_HEAD=$(ref_block_number 2>/dev/null)
if [[ -z "$REF_HEAD" || "$REF_HEAD" == "0" || "$REF_HEAD" == "null" ]]; then
    if [[ -n "$REF_DOCKER" ]]; then
        echo "error: could not read eth.blockNumber from reference container ($REF_DOCKER, ipc=$REF_IPC)" >&2
    else
        echo "error: could not read eth_blockNumber from reference RPC ($REF_RPC)" >&2
    fi
    exit 2
fi

# Upper bound: min(test_head, ref_head, user-supplied TO)
MAX_BLOCK=$TEST_HEAD
(( REF_HEAD < MAX_BLOCK )) && MAX_BLOCK=$REF_HEAD
if [[ -n "$TO" ]]; then
    (( TO < MAX_BLOCK )) && MAX_BLOCK=$TO
fi

echo "  test head: $TEST_HEAD"
echo "  ref head : $REF_HEAD"
echo "  bound    : compare in [$FROM, $MAX_BLOCK]"
echo ""

# ---- build sample set --------------------------------------------------------
declare -a SAMPLES_ARR=()
for m in "${MILESTONES[@]}"; do
    if (( m >= FROM && m <= MAX_BLOCK )); then
        SAMPLES_ARR+=("$m")
    fi
done

if [[ $QUICK -eq 0 ]]; then
    # uniformly distributed random samples in [FROM, MAX_BLOCK]
    local_max=$MAX_BLOCK
    local_min=$FROM
    span=$(( local_max - local_min + 1 ))
    if (( span > 0 )); then
        for _ in $(seq 1 "$SAMPLES"); do
            r=$(( RANDOM * 32768 + RANDOM ))
            n=$(( local_min + (r % span) ))
            SAMPLES_ARR+=("$n")
        done
    fi
fi

# de-dupe + sort
mapfile -t SAMPLES_ARR < <(printf '%s\n' "${SAMPLES_ARR[@]}" | sort -nu)

echo "${C_DIM}sampling ${#SAMPLES_ARR[@]} block(s)…${C_OFF}"
echo ""

# ---- comparison loop ---------------------------------------------------------
TOTAL=0
PASS=0
FAIL=0
declare -a FAILURES=()

compare_field() {
    local block="$1" field="$2" ref="$3" test="$4"
    ref=$(normalise_hex "$ref")
    test=$(normalise_hex "$test")
    if [[ "$ref" == "$test" ]]; then
        printf 'OK'
    else
        printf 'MISMATCH ref=%s test=%s' "$ref" "$test"
        FAILURES+=("block $block field $field: ref=$ref test=$test")
        return 1
    fi
}

for n in "${SAMPLES_ARR[@]}"; do
    TOTAL=$((TOTAL+1))
    test_json=$(test_block_json "$n")
    ref_json=$(ref_block_json "$n")

    if [[ "$test_json" == "null" ]]; then
        printf '  block %-10d %sNULL on test%s\n' "$n" "$C_BAD" "$C_OFF"
        FAIL=$((FAIL+1))
        FAILURES+=("block $n: test node returned null (block missing on test side)")
        continue
    fi
    if [[ "$ref_json" == "null" || -z "$ref_json" ]]; then
        printf '  block %-10d %sNULL on reference%s\n' "$n" "$C_BAD" "$C_OFF"
        FAIL=$((FAIL+1))
        FAILURES+=("block $n: reference returned null (block missing on ref side)")
        continue
    fi

    # extract fields from both
    h_test=$(echo "$test_json" | jq -r .hash)
    h_ref=$(echo "$ref_json" | jq -r .hash)
    sr_test=$(echo "$test_json" | jq -r .stateRoot)
    sr_ref=$(echo "$ref_json" | jq -r .stateRoot)
    tr_test=$(echo "$test_json" | jq -r .transactionsRoot)
    tr_ref=$(echo "$ref_json" | jq -r .transactionsRoot)
    rr_test=$(echo "$test_json" | jq -r .receiptsRoot)
    rr_ref=$(echo "$ref_json" | jq -r .receiptsRoot)

    if [[ "$h_test" == "$h_ref" ]]; then
        if [[ $DEEP -eq 1 ]]; then
            rc_test=$(test_receipts_count "$n")
            rc_ref=$(ref_receipts_count "$n")
            if [[ "$rc_test" == "$rc_ref" ]]; then
                printf '  block %-10d %sMATCH%s  hash %s  receipts=%s\n' \
                    "$n" "$C_OK" "$C_OFF" "$h_test" "$rc_test"
                PASS=$((PASS+1))
            else
                printf '  block %-10d %sRECEIPTS MISMATCH%s ref=%s test=%s\n' \
                    "$n" "$C_BAD" "$C_OFF" "$rc_ref" "$rc_test"
                FAIL=$((FAIL+1))
                FAILURES+=("block $n receipts count: ref=$rc_ref test=$rc_test")
            fi
        else
            printf '  block %-10d %sMATCH%s  hash %s\n' "$n" "$C_OK" "$C_OFF" "$h_test"
            PASS=$((PASS+1))
        fi
    else
        printf '  block %-10d %sHASH MISMATCH%s\n' "$n" "$C_BAD" "$C_OFF"
        printf '              hash         ref=%s test=%s\n' "$h_ref" "$h_test"
        # break down which sub-fields diverge
        [[ "$sr_test" != "$sr_ref" ]] && printf '              stateRoot    ref=%s test=%s\n' "$sr_ref" "$sr_test"
        [[ "$tr_test" != "$tr_ref" ]] && printf '              txRoot       ref=%s test=%s\n' "$tr_ref" "$tr_test"
        [[ "$rr_test" != "$rr_ref" ]] && printf '              receiptsRoot ref=%s test=%s\n' "$rr_ref" "$rr_test"
        FAIL=$((FAIL+1))
        FAILURES+=("block $n hash: ref=$h_ref test=$h_test")
    fi
done

# ---- summary -----------------------------------------------------------------
echo ""
if (( FAIL == 0 )); then
    echo "${C_OK}${C_BOLD}PASS${C_OFF}  $PASS/$TOTAL blocks matched"
else
    echo "${C_BAD}${C_BOLD}FAIL${C_OFF}  $PASS passed / $FAIL failed (total $TOTAL)"
    echo ""
    echo "first failures:"
    printf '  - %s\n' "${FAILURES[@]:0:5}"
fi

if [[ $JSON_OUT -eq 1 ]]; then
    echo ""
    ref_label=$REF_RPC
    [[ -n "$REF_DOCKER" ]] && ref_label="docker:${REF_DOCKER}:${REF_IPC}"
    test_label="ipc:${IPC_PATH}"
    [[ -n "$TEST_RPC" ]] && test_label="rpc:${TEST_RPC}"
    jq -nc \
        --argjson total "$TOTAL" \
        --argjson pass "$PASS" \
        --argjson fail "$FAIL" \
        --arg test "$test_label" \
        --arg ref "$ref_label" \
        --arg test_head "$TEST_HEAD" \
        --arg ref_head "$REF_HEAD" \
        '{total:$total, pass:$pass, fail:$fail, test:$test, ref:$ref, test_head:$test_head, ref_head:$ref_head}'
fi

(( FAIL == 0 )) && exit 0 || exit 1
