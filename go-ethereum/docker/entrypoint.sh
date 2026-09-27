#!/bin/sh
# XDC fast-sync node entrypoint.
#
# Owns every default geth flag for fast-sync mainnet operation so install.sh
# only has to mount data + map ports. Operator-pinned pivot baked in; rolls
# forward with a new image tag (operators just `docker pull` to update).
#
# Env knobs (all optional, sensible defaults):
#   XDC_NETWORK            mainnet | apothem               (default: mainnet)
#   XDC_CACHE              MiB of cache                    (default: 8192)
#   XDC_MAXPEERS           inbound+outbound peer cap       (default: 50)
#   XDC_VERBOSITY          0..5 geth verbosity             (default: 3)
#   XDC_HTTP_API           comma-list of RPC namespaces    (default: eth,net,web3,debug,XDPoS)
#   XDC_ETHSTATS_NAME      stats-page node name            (default: <hostname>-xdc-fast)
#   XDC_ETHSTATS_SECRET    stats secret; if unset, ethstats reporter is OFF
#   XDC_ETHSTATS_HOST      ethstats server host:port       (default: stats.xdcindia.com:443)
#
# Any extra args passed to `docker run …<image> EXTRA` are forwarded to geth.

set -e

# Per-network baked pivots. Roll forward when image tag bumps; operators can
# also override at runtime via XDC_PIVOT_{NUMBER,HASH,ROOT}.
NETWORK="${XDC_NETWORK:-mainnet}"
case "$NETWORK" in
  mainnet)
    NET_FLAG="--xdcmainnet"
    PIVOT_NUM=79340238
    PIVOT_HASH=0x9bbe97f7e0a2010296f4a1c0930014eed4147f99fe3f67f6804e137b1552c80d
    PIVOT_ROOT=0xca73772152116516430c85466a6ae24fed7436861eff2268fab7bf1805766145
    ;;
  apothem|testnet)
    NET_FLAG="--apothem"
    # Captured from erpc.apothem.network head-256 at image build time.
    PIVOT_NUM=82689016
    PIVOT_HASH=0x9d875b47755d9a963e605e2e4510cd42a0db4386c18e56e400829a36c75c24c0
    PIVOT_ROOT=0xc66fb2f9beb580e19afee235923fb84512f80bd2d3afd9ebf22de75772686dde
    ;;
  *)
    echo "entrypoint: unknown XDC_NETWORK=$NETWORK (expected mainnet|apothem)" >&2
    exit 1
    ;;
esac

# Runtime override — operator can pass any of the three to refresh the pivot
# without rebuilding the image (e.g. after a long stale period).
PIVOT_NUM="${XDC_PIVOT_NUMBER:-$PIVOT_NUM}"
PIVOT_HASH="${XDC_PIVOT_HASH:-$PIVOT_HASH}"
PIVOT_ROOT="${XDC_PIVOT_ROOT:-$PIVOT_ROOT}"

set -- \
  --datadir=/data \
  $NET_FLAG \
  --syncmode=fast --gcmode=full --state.scheme=hash \
  --cache="${XDC_CACHE:-8192}" \
  --maxpeers="${XDC_MAXPEERS:-50}" \
  --port=30303 \
  --http --http.addr=0.0.0.0 \
  --http.api="${XDC_HTTP_API:-eth,net,web3,debug,XDPoS}" \
  --verbosity="${XDC_VERBOSITY:-3}" \
  "$@"

if [ -n "$PIVOT_NUM" ] && [ -n "$PIVOT_HASH" ] && [ -n "$PIVOT_ROOT" ]; then
  set -- \
    --fastsyncpivotnumber="$PIVOT_NUM" \
    --fastsyncpivothash="$PIVOT_HASH" \
    --fastsyncpivotroot="$PIVOT_ROOT" \
    "$@"
fi

# Ethstats reporter — opt-in via XDC_ETHSTATS_SECRET. Without it the node
# stays invisible to stats.xdcindia.com (no leaked secret in default image).
if [ -n "${XDC_ETHSTATS_SECRET:-}" ]; then
  NAME="${XDC_ETHSTATS_NAME:-$(hostname)-xdc-fast}"
  HOST="${XDC_ETHSTATS_HOST:-stats.xdcindia.com:443}"
  set -- --ethstats="${NAME}:${XDC_ETHSTATS_SECRET}@${HOST}" "$@"
fi

exec /usr/local/bin/geth "$@"
