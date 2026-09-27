#!/bin/bash

# XDC Node Runner Script
# This script simplifies running an XDC node

set -e

# Configuration
DATADIR="${DATADIR:-$HOME/.xdc}"
NETWORK="${NETWORK:-mainnet}"
SYNCMODE="${SYNCMODE:-fast}"
HTTP_PORT="${HTTP_PORT:-8545}"
WS_PORT="${WS_PORT:-8546}"
P2P_PORT="${P2P_PORT:-30303}"

# Network-specific settings
case "$NETWORK" in
    mainnet)
        NETWORK_ID=50
        CHAIN_ID=50
        ;;
    testnet|apothem)
        NETWORK_ID=51
        CHAIN_ID=51
        ;;
    devnet)
        NETWORK_ID=551
        CHAIN_ID=551
        ;;
    *)
        echo "Unknown network: $NETWORK"
        echo "Supported networks: mainnet, testnet, apothem, devnet"
        exit 1
        ;;
esac

# Create data directory
mkdir -p "$DATADIR"

# Build node command
NODE_CMD="XDC \
    --datadir $DATADIR \
    --networkid $NETWORK_ID \
    --syncmode $SYNCMODE \
    --http \
    --http.addr 0.0.0.0 \
    --http.port $HTTP_PORT \
    --http.api eth,net,web3,xdpos \
    --http.corsdomain '*' \
    --ws \
    --ws.addr 0.0.0.0 \
    --ws.port $WS_PORT \
    --ws.api eth,net,web3 \
    --port $P2P_PORT"

# Operator-pinned Fast Sync trust anchor (refs #844 Phase A.6).
# Set FASTSYNC_PIVOT_NUMBER, FASTSYNC_PIVOT_HASH, and FASTSYNC_PIVOT_ROOT to
# pin a known-good block as the sync trust anchor. All three must be set together.
if test -n "${FASTSYNC_PIVOT_NUMBER:-}" || test -n "${FASTSYNC_PIVOT_HASH:-}" || test -n "${FASTSYNC_PIVOT_ROOT:-}"
then
    if test -z "${FASTSYNC_PIVOT_NUMBER:-}" || test -z "${FASTSYNC_PIVOT_HASH:-}" || test -z "${FASTSYNC_PIVOT_ROOT:-}"
    then
        echo "Error: FASTSYNC_PIVOT_NUMBER, FASTSYNC_PIVOT_HASH, and FASTSYNC_PIVOT_ROOT must all be set together."
        exit 1
    fi
    NODE_CMD="$NODE_CMD --fastsyncpivotnumber ${FASTSYNC_PIVOT_NUMBER} --fastsyncpivothash ${FASTSYNC_PIVOT_HASH} --fastsyncpivotroot ${FASTSYNC_PIVOT_ROOT}"
fi

# Add optional flags
if [ -n "$MASTERNODE" ]; then
    NODE_CMD="$NODE_CMD --masternode"
    
    if [ -n "$COINBASE" ]; then
        NODE_CMD="$NODE_CMD --masternode.coinbase $COINBASE"
    fi
fi

if [ -n "$VERBOSITY" ]; then
    NODE_CMD="$NODE_CMD --verbosity $VERBOSITY"
fi

if [ -n "$BOOTNODES" ]; then
    NODE_CMD="$NODE_CMD --bootnodes $BOOTNODES"
fi

# Enable XDCx if requested
if [ "$XDCX_ENABLED" = "true" ]; then
    NODE_CMD="$NODE_CMD --xdcx"
fi

# Enable XDCx Lending if requested
if [ "$LENDING_ENABLED" = "true" ]; then
    NODE_CMD="$NODE_CMD --xdcxlending"
fi

# Print configuration
echo "==================================="
echo "XDC Node Configuration"
echo "==================================="
echo "Network:    $NETWORK"
echo "Network ID: $NETWORK_ID"
echo "Data Dir:   $DATADIR"
echo "Sync Mode:  $SYNCMODE"
echo "HTTP Port:  $HTTP_PORT"
echo "WS Port:    $WS_PORT"
echo "P2P Port:   $P2P_PORT"
echo "==================================="

# Run node
echo "Starting XDC node..."
exec $NODE_CMD "$@"
