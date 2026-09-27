#!/bin/bash
# gen-address.sh - generate fresh XDC keypairs (random private key + address).
#
# Produces, for N accounts: the raw private key and its address, plus
# copy-paste blocks for .env (PRIVATE_KEY_n=...) and for gen-genesis.sh
# (--signers a,b,c). Uses the bundled bin/XDC to derive addresses, so it
# matches exactly what run.sh imports.
#
# Every run also writes the generated keypairs to a per-network JSON file,
# accounts/accounts-<chainid>.json, mapping each account to its address and
# private key (keyed by the chain/network id, so different networks keep
# separate account files). Override the id with --chainid, the path with
# --out. This file holds private keys -- accounts/ is gitignored.
#
# gen-genesis.sh then rewrites the same file into the full picture of the
# network -- every role (signers, owner, foundation, foundation/team owners,
# swap, prefund), each one's genesis balance, the consensus parameters and the
# total supply -- preserving the private keys written here by matching on
# address. Roles that are plain genesis addresses have no key and carry
# privateKey: null.
#
# Usage:
#   ./gen-address.sh                   # 4 keypairs (default, matches the 4-node setup)
#   ./gen-address.sh 1                 # a single keypair
#   ./gen-address.sh 4 --env           # also overwrite ./.env with the generated keys
#   ./gen-address.sh 4 --chainid 12345   # tag the accounts JSON with chain id 12345
#   ./gen-address.sh 4 --out keys.json     # write the accounts JSON to a custom path

COUNT=4
WRITE_ENV=0
CHAINID=20118
ACCOUNTS_OUT=""
ROLES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --env) WRITE_ENV=1; shift;;
    --roles) ROLES=1; shift;;
    --chainid) CHAINID=$2; shift 2;;
    --out) ACCOUNTS_OUT=$2; shift 2;;
    [0-9]*) COUNT=$1; shift;;
    -h|--help) sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
done
[ -n "$ACCOUNTS_OUT" ] || ACCOUNTS_OUT="accounts/accounts-${CHAINID}.json"

cd "$(dirname "$0")" || exit 1
source ./lib.sh
ensure_bins
XDC="$XDC_BIN"
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }

TMP=$(mktemp -d); PW="$TMP/pw"; : > "$PW"
trap 'rm -rf "$TMP"' EXIT

_n=0
newkey() {   # newkey -> sets $p (private key) and $a (address, no prefix)
  _n=$((_n+1))
  p=$(openssl rand -hex 32)
  a=$("$XDC" account import --password "$PW" --datadir "$TMP/k$_n" <(printf '%s' "$p") 2>/dev/null \
        | grep -oE 'xdc[0-9a-fA-F]{40}' | head -1 | sed 's/^xdc//')
  [ -n "$a" ] || { echo "gen-address.sh: failed to derive an address" >&2; exit 1; }
}

privs=(); addrs=()
for i in $(seq 1 "$COUNT"); do
  newkey
  privs+=("$p"); addrs+=("$a")
  printf 'Key %d\n  private : %s\n  address : 0x%s\n  xdc     : xdc%s\n\n' "$i" "$p" "$a" "$a"
done

# --roles: also generate the non-signer accounts the genesis names, so the
# network has a known private key for EVERY account in it. Without this they
# fall back to gen-genesis.sh's built-in defaults -- the public XDC addresses
# nobody here holds the keys for, which is why they show up as
# privateKey: null in the accounts JSON.
#
# ROLE_ENVS are the .env names; ROLE_LABELS are the role names recorded in the
# accounts JSON (and matched by gen-genesis.sh). Index-aligned.
ROLE_ENVS=(OWNER_PRIVATE_KEY FOUNDATION_PRIVATE_KEY \
           FOUNDATION_OWNER_1_PRIVATE_KEY FOUNDATION_OWNER_2_PRIVATE_KEY \
           TEAM_OWNER_1_PRIVATE_KEY TEAM_OWNER_2_PRIVATE_KEY \
           SWAP_PRIVATE_KEY PREFUND_PRIVATE_KEY)
ROLE_LABELS=(owner foundation foundationOwner foundationOwner teamOwner teamOwner swap prefund)
role_privs=(); role_addrs=()
if [ "$ROLES" = 1 ]; then
  echo "# ---- role accounts ----"
  for r in $(seq 0 $(( ${#ROLE_ENVS[@]} - 1 ))); do
    newkey
    role_privs+=("$p"); role_addrs+=("$a")
    printf '%-32s 0x%s\n' "${ROLE_ENVS[$r]}" "$a"
  done
  echo
fi

echo "# ---- paste into .env ----"
for i in $(seq 1 "$COUNT"); do echo "PRIVATE_KEY_$i=${privs[$((i-1))]}"; done
echo
sig=$(IFS=,; echo "${addrs[*]}")
echo "# ---- gen-genesis.sh signers ----"
echo "./gen-genesis.sh --signers $sig"

if [ "$WRITE_ENV" = "1" ]; then
  {
    for i in $(seq 1 "$COUNT"); do echo "PRIVATE_KEY_$i=${privs[$((i-1))]}"; done
    # Role keys go in the same .env so a network's accounts are recoverable
    # from one place. setup.sh reads the ADDRESSES out of the accounts JSON
    # (single source of truth); these are here so the keys can be imported.
    for r in $(seq 0 $(( ${#role_privs[@]} - 1 ))); do
      [ ${#role_privs[@]} -eq 0 ] && break
      echo "${ROLE_ENVS[$r]}=${role_privs[$r]}"
    done
  } > .env
  echo
  echo "Wrote $COUNT signer keys${role_privs:+ + ${#role_privs[@]} role keys} to ./.env"
fi

# Persist the keypairs as JSON, keyed by network id: each account's address
# (0x + xdc forms) and private key. Built with python3 so values are always
# valid JSON. Holds private keys -- keep out of version control (accounts/ is
# gitignored).
mkdir -p "$(dirname "$ACCOUNTS_OUT")"
CHAINID="$CHAINID" ACCOUNTS_OUT="$ACCOUNTS_OUT" \
PRIVS="$(IFS=,; echo "${privs[*]}")" ADDRS="$(IFS=,; echo "${addrs[*]}")" \
ROLE_PRIVS="$(IFS=,; echo "${role_privs[*]}")" ROLE_ADDRS="$(IFS=,; echo "${role_addrs[*]}")" \
ROLE_LABELS="$(IFS=,; echo "${ROLE_LABELS[*]}")" \
python3 -c '
import json, os

def lst(v):
    return [x for x in os.environ.get(v, "").split(",") if x]

accounts = []
for i, (a, p) in enumerate(zip(lst("ADDRS"), lst("PRIVS"))):
    accounts.append({"roles": ["signer"], "node": i + 1, "address": "0x" + a,
                     "xdc": "xdc" + a, "privateKey": p})
# Role accounts (only when --roles was passed). Same shape, so gen-genesis.sh
# picks the keys back up by address regardless of which block wrote them.
for label, a, p in zip(lst("ROLE_LABELS"), lst("ROLE_ADDRS"), lst("ROLE_PRIVS")):
    accounts.append({"roles": [label], "address": "0x" + a, "xdc": "xdc" + a,
                     "privateKey": p})
out = {"chainId": int(os.environ["CHAINID"]), "accounts": accounts}
with open(os.environ["ACCOUNTS_OUT"], "w") as f:
    json.dump(out, f, indent=2)
    f.write("\n")
'
# Private keys: same treatment as the other keyfiles in this repo (see
# lab/gen-lab-genesis.sh, lab/onboard-masternode.sh). gitignore keeps it out of
# commits; this keeps it off a shared host's other accounts.
chmod 600 "$ACCOUNTS_OUT" 2>/dev/null || true
echo "Wrote $(( COUNT + ${#role_privs[@]} )) accounts (chain id $CHAINID) to ./$ACCOUNTS_OUT"
