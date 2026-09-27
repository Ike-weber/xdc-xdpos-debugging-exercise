#!/usr/bin/env bash
# =============================================================================
#  XDC NODE  —  one-command installer
# -----------------------------------------------------------------------------
#  Works everywhere (Linux + macOS, amd64 + arm64). Installs everything — the
#  matching node binary, and for geth: a recent chain snapshot — and starts
#  your node. You only need `curl`.
#
#      curl -fsSL https://xdc.network/install.sh | bash
#
#  Options (pass after `bash -s --`):
#      --client geth|erigon|reth|besu|nethermind|xone   (default: geth)
#      --mode   fast|full|minimal|snap|archive|validator  (default: per-client)
#      --network mainnet|apothem|devnet|net5050     (default: mainnet)
#      --no-snapshot                                (sync from genesis; skip snapshot restore)
#      setup | prepare                              (action: prepare only, don't start)
#
#  Examples:
#      curl -fsSL https://xdc.network/install.sh | bash -s -- setup --client erigon --mode minimal
#      curl -fsSL https://xdc.network/install.sh | bash -s -- --client nethermind
#      curl -fsSL https://xdc.network/install.sh | bash -s -- setup --client besu --network apothem
#      XDC_DIR=/data/xdc curl -fsSL https://xdc.network/install.sh | bash
#
#  Re-running is safe: downloads resume, an existing node is left alone.
#
#  ONE_SH_VERSION: 1.1.0
# =============================================================================
set -euo pipefail

BASE_URL="${BASE_URL:-https://xdc.network/snapshots}"
INSTALL_VERSION="1.1.0"

# ----------------------------- ARG PARSER ------------------------------------
# Precedence: CLI flag > env var > default
# Safe under set -u with empty piped argv (no args at all).

CLIENT="${CLIENT:-geth}"
NODETYPE="${NODETYPE:-}"
NO_SNAPSHOT="${NO_SNAPSHOT:-0}"
NETWORK="${NETWORK:-mainnet}"
ACTION="${XDC_ACTION:-up}"

_VALID_CLIENTS="geth erigon reth besu nethermind xone all"
_VALID_MODES="fast full minimal snap archive validator"
_VALID_NETWORKS="mainnet apothem devnet net5050"

_validate_list(){
  local val="$1" list="$2" label="$3"
  for item in $list; do [ "$val" = "$item" ] && return 0; done
  echo "Error: unsupported $label '$val'. Valid values: $list" >&2; exit 1
}

# Parse "$@" — works with zero args (piped bare run)
while [ "${1+set}" = "set" ] && [ $# -gt 0 ]; do
  case "$1" in
    --client|-c)  CLIENT="${2:?'--client requires a value'}";  shift 2 ;;
    --mode|-m)    NODETYPE="${2:?'--mode requires a value'}";  shift 2 ;;
    --network|-n) NETWORK="${2:?'--network requires a value'}"; shift 2 ;;
    --no-snapshot|--no-snap) NO_SNAPSHOT=1; shift ;;
    setup|prepare) ACTION="setup"; shift ;;
    up|start)      ACTION="up";    shift ;;
    # ignore unknown flags (e.g. piped set-u safe)
    --)  shift; break ;;
    *)   shift ;;
  esac
done

_validate_list "$CLIENT"  "$_VALID_CLIENTS"  "--client"
_validate_list "$NETWORK" "$_VALID_NETWORKS" "--network"
[ -n "$NODETYPE" ] && _validate_list "$NODETYPE" "$_VALID_MODES" "--mode"

export CLIENT NODETYPE NETWORK NO_SNAPSHOT

# ----------------------------- INSTALL DIRECTORY ----------------------------
# geth keeps the original default so bare re-runs are undisturbed;
# other clients get a sibling dir so two clients never share state.
# CLIENT=all: use geth's default dir as the reference (actual per-client dirs
# are handled inside one.sh cmd_all which re-invokes per client).
if [ "$CLIENT" = "geth" ] || [ "$CLIENT" = "all" ]; then
  XDC_DIR="${XDC_DIR:-$HOME/xdc-node}"
else
  XDC_DIR="${XDC_DIR:-$HOME/xdc-node-$CLIENT}"
fi
export XDC_DIR

# ----------------------------- PRETTY OUTPUT ---------------------------------
if [ -t 1 ]; then B=$'\033[1m'; G=$'\033[32m'; Y=$'\033[33m'; N=$'\033[0m'; else B=""; G=""; Y=""; N=""; fi
say(){ printf '\n%s▶ %s%s\n' "$B" "$*" "$N"; }
ok(){  printf '   %s✓%s %s\n' "$G" "$N" "$*"; }
note(){ printf '   %s•%s %s\n' "$Y" "$N" "$*"; }

# ----------------------------- PREREQUISITES ---------------------------------
command -v curl >/dev/null 2>&1 || { echo "curl is required — please install curl and re-run."; exit 1; }

# zstd: only geth uses snapshot restore today; skip for other clients and fleet mode.
if [ "$CLIENT" = "geth" ]; then
  if ! command -v zstd >/dev/null 2>&1; then
    note "zstd not found — trying to install it…"
    if   command -v apt-get >/dev/null 2>&1; then (sudo apt-get update -y && sudo apt-get install -y zstd) >/dev/null 2>&1 || true
    elif command -v dnf     >/dev/null 2>&1; then sudo dnf install -y zstd >/dev/null 2>&1 || true
    elif command -v yum     >/dev/null 2>&1; then sudo yum install -y zstd >/dev/null 2>&1 || true
    elif command -v apk     >/dev/null 2>&1; then sudo apk add zstd        >/dev/null 2>&1 || true
    elif command -v brew    >/dev/null 2>&1; then brew install zstd        >/dev/null 2>&1 || true
    fi
    command -v zstd >/dev/null 2>&1 && ok "zstd installed" || note "could not auto-install zstd — if setup complains, run: apt install zstd / brew install zstd"
  fi
fi

# ----------------------------- BANNER ----------------------------------------
_mode_label="${NODETYPE:-(default)}"
say "Installing XDC ${NETWORK} node  →  client=${CLIENT}  mode=${_mode_label}  dir=${XDC_DIR}"
mkdir -p "$XDC_DIR"; cd "$XDC_DIR"

# ----------------------------- FETCH ONE.SH ----------------------------------
say "Fetching one.sh (the node manager)"
curl -fsSL -A 'Mozilla/5.0' "$BASE_URL/one.sh" -o one.sh
chmod +x one.sh
ok "one.sh ready in $XDC_DIR"

# ----------------------------- VERSION GUARD ---------------------------------
ONE_REPORTED="$(grep -m1 '^# *ONE_SH_VERSION' one.sh 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"
if [ -n "$ONE_REPORTED" ] && [ "$ONE_REPORTED" != "$INSTALL_VERSION" ]; then
  note "Version notice: install.sh is ${INSTALL_VERSION}, one.sh reports ${ONE_REPORTED}."
  note "Both scripts should be from the same release. If you see odd behaviour, re-run:"
  note "  curl -fsSL https://xdc.network/install.sh | bash"
fi

# ----------------------------- DELEGATE TO ONE.SH ----------------------------
if [ "$CLIENT" = "all" ]; then
  # Fleet mode: delegate setup + start to one.sh fleet dispatcher.
  bash one.sh --client all setup && bash one.sh --client all start
else
  case "$ACTION" in
    setup|prepare) bash one.sh setup ;;
    *)             bash one.sh setup && bash one.sh start && bash one.sh status ;;
  esac
fi

# ----------------------------- FOOTER ----------------------------------------
cat <<EOF

${B}✓ Done.${N} Your ${CLIENT} node lives in: ${B}${XDC_DIR}${N}
   status : cd ${XDC_DIR} && ./one.sh status
   stop   : cd ${XDC_DIR} && ./one.sh stop
   start  : cd ${XDC_DIR} && ./one.sh start
   logs   : cd ${XDC_DIR} && ./one.sh logs
EOF
