#!/usr/bin/env bash
# XDC Network — Fast Sync Node bootstrap
#
# Mainnet (default):
#   curl -fsSL https://xdc.network/install.sh | bash
#
# Apothem testnet:
#   curl -fsSL https://xdc.network/install.sh | bash -s -- apothem
#
# Override defaults via env:
#   XDC_DATA_DIR / XDC_HTTP_PORT / XDC_P2P_PORT / XDC_CONTAINER / XDC_IMG
#   XDC_NETWORK (mainnet|apothem)
#   XDC_PIVOT_NUMBER / XDC_PIVOT_HASH / XDC_PIVOT_ROOT  (override baked pivot)
#   XDC_ETHSTATS_NAME / XDC_ETHSTATS_SECRET  (opt-in to stats.xdcindia.com)
#
# The container image owns every geth flag — this script just runs it.
# Pivot rolls forward with image tag updates; users get them via `docker pull`.

set -euo pipefail

# Positional arg overrides XDC_NETWORK; default mainnet.
case "${1:-}" in
  apothem|testnet) XDC_NETWORK=apothem ;;
  mainnet|"")      XDC_NETWORK="${XDC_NETWORK:-mainnet}" ;;
  *) echo "usage: install.sh [mainnet|apothem]" >&2; exit 1 ;;
esac

# Per-network defaults — separate data dir / container / RPC port per network
# so mainnet + testnet can coexist on the same host.
if [ "$XDC_NETWORK" = "apothem" ]; then
  DATA_DIR="${XDC_DATA_DIR:-$HOME/.xdc-fast-apothem}"
  HTTP_PORT="${XDC_HTTP_PORT:-8546}"
  P2P_PORT="${XDC_P2P_PORT:-30304}"
  CONTAINER="${XDC_CONTAINER:-xdc-fast-apothem}"
else
  DATA_DIR="${XDC_DATA_DIR:-$HOME/.xdc-fast}"
  HTTP_PORT="${XDC_HTTP_PORT:-8545}"
  P2P_PORT="${XDC_P2P_PORT:-30303}"
  CONTAINER="${XDC_CONTAINER:-xdc-fast}"
fi
IMG="${XDC_IMG:-anilchinchawale/gp5-xdc:fast-v1.17.3}"

# Stats publishing is opt-in. To appear on https://stats.xdcindia.com, request
# a stats secret from the XDC infra team and pass it via env:
#   XDC_ETHSTATS_SECRET=<secret> curl -fsSL https://xdc.network/install.sh | bash
XDC_ETHSTATS_SECRET="${XDC_ETHSTATS_SECRET:-}"
XDC_ETHSTATS_NAME="${XDC_ETHSTATS_NAME:-$(hostname)-xdc-fast}"

command -v docker >/dev/null || { echo "Install Docker first: https://www.docker.com/products/docker-desktop"; exit 1; }
docker info >/dev/null 2>&1 || { echo "Start Docker first."; exit 1; }

mkdir -p "$DATA_DIR"
docker pull "$IMG"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

docker run -d --name "$CONTAINER" --restart unless-stopped \
  -v "$DATA_DIR":/data \
  -p "$P2P_PORT":30303 -p "$P2P_PORT":30303/udp \
  -p "$HTTP_PORT":8545 \
  -e XDC_NETWORK="$XDC_NETWORK" \
  -e XDC_ETHSTATS_NAME="$XDC_ETHSTATS_NAME" \
  ${XDC_ETHSTATS_SECRET:+-e XDC_ETHSTATS_SECRET="$XDC_ETHSTATS_SECRET"} \
  ${XDC_PIVOT_NUMBER:+-e XDC_PIVOT_NUMBER="$XDC_PIVOT_NUMBER"} \
  ${XDC_PIVOT_HASH:+-e XDC_PIVOT_HASH="$XDC_PIVOT_HASH"} \
  ${XDC_PIVOT_ROOT:+-e XDC_PIVOT_ROOT="$XDC_PIVOT_ROOT"} \
  "$IMG" >/dev/null

cat <<EOF

✓ XDC fast-sync node running
  Container : $CONTAINER
  Network   : $XDC_NETWORK
  Data dir  : $DATA_DIR
  RPC URL   : http://localhost:$HTTP_PORT
  Image     : $IMG
  Stats     : ${XDC_ETHSTATS_SECRET:+visible at https://stats.xdcindia.com as $XDC_ETHSTATS_NAME}${XDC_ETHSTATS_SECRET:-disabled (set XDC_ETHSTATS_SECRET to publish)}

Logs   : docker logs -f $CONTAINER
Status : docker exec $CONTAINER geth --datadir=/data attach --exec 'eth.syncing'
Stop   : docker rm -f $CONTAINER
EOF
